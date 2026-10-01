package webrtc

import (
	"encoding/binary"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
)

// Buffers curtos absorvem jitter de scheduler sem transformar congestionamento
// em centenas de ms de latência. Vídeo recebe mais espaço porque um frame pode
// ocupar dezenas de pacotes RTP em 1080p/30.
const (
	audioForwarderBuffer = 64
	videoForwarderBuffer = 128
)

// slot é um slot de envio pré-alocado de um subscriber (D5): uma
// TrackLocalStaticRTP fixa, bound a um RTPSender na renegociação de join.
// Trocar o ocupante (publisher) = iniciar/parar o forwarder — SEM
// renegociação de SDP (o SSRC do slot é estável; o SSN é traduzido por slot).
type slot struct {
	peer   *Peer
	sender *webrtc.RTPSender
	local  *webrtc.TrackLocalStaticRTP

	// ocupação — guardados por peer.mu
	owner *Peer
	kind  string // "video" | "screen" | "audio"
	src   *webrtc.TrackRemote
	fwd   *forwarder

	translator ssnTranslator
}

func (t *ssnTranslator) resetSource() {
	t.mu.Lock()
	t.owner = nil
	t.mu.Unlock()
}

// assignWithFanout faz o slot forwardar a track do publisher via o fanout
// dela (o caller segura o peer.mu do SUBSCRIBER; o fanout já foi resolvido no
// publisher sem segurar este lock — ver Peer.fanoutFor). Se já havia
// forwarder, ele é terminado (o slot muda de ocupante/track).
func (s *slot) assignWithFanout(
	owner *Peer,
	kind string,
	track *webrtc.TrackRemote,
	fanout *fanout,
) {
	if s.fwd != nil {
		s.fwd.finish()
		s.fwd = nil
	}

	// Toda nova atribuição representa uma nova fronteira RTP.
	// O próximo pacote deve continuar a sequência do slot.
	s.translator.resetSource()

	s.owner = owner
	s.kind = kind
	s.src = track
	s.fwd = newForwarder(s, owner, kind, fanout)

	// Para vídeo/screen, o fanout é criado de forma lazy. Inscreva e inicie o
	// forwarder ANTES de liberar o reader da TrackRemote; assim o keyframe
	// inicial não pode ser consumido pelo SFU sem nenhum subscriber conectado.
	fanout.subscribe(s.fwd)
	s.fwd.start()
	fanout.start()
}

// release para de forwardar (slot fica vazio — sem pacotes, D5). O
// translator preserva o lastSSN para o próximo ocupante continuar o SSN de
// forma monótona (o caller segura peer.mu).
func (s *slot) release() {
	s.owner = nil
	if s.fwd != nil {
		s.fwd.finish()
		s.fwd = nil
	}
}

// startRTCPFeedback consome feedback RTCP do receiver deste slot. Como o SFU
// reescreve o stream para uma TrackLocal própria, PLI/NACK recebidos aqui não
// chegam automaticamente ao publisher. Para vídeo/screen, convertemos feedback
// de perda em PLI para a track de origem atual, permitindo recuperação de
// keyframe em vez de deixar o subscriber preto/congelado.
func (s *slot) startRTCPFeedback() {
	if s == nil || s.sender == nil || s.peer == nil {
		return
	}

	go func() {
		for {
			packets, _, err := s.sender.ReadRTCP()
			if err != nil {
				return
			}

			needsKeyframe := false
			for _, packet := range packets {
				switch packet.(type) {
				case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
					needsKeyframe = true
				case *rtcp.TransportLayerNack:
					// RegisterDefaultInterceptors já instala o NACK responder
					// no stream de saída e retransmite do cache usando o SSN do
					// slot. Transformar todo NACK em PLI criava keyframe storms.
				}
			}
			if !needsKeyframe {
				continue
			}

			s.peer.mu.Lock()
			owner := s.owner
			src := s.src
			kind := s.kind
			s.peer.mu.Unlock()

			if owner == nil || src == nil || (kind != "video" && kind != "screen") {
				continue
			}

			owner.requestKeyframe(src)
		}
	}()
}

// forwarder é a goroutine de UM subscriber (slot): recebe os pacotes do
// fanout da track do publisher, traduz o SSN por slot e escreve na track local
// do slot. Uma por par (publisher → slot) ativo. NUNCA chama ReadRTP na track
// do publisher — isso é feito uma única vez pelo fanout (ver fanout).
type forwarder struct {
	slot   *slot
	owner  *Peer
	kind   string
	fanout *fanout

	ch   chan *rtp.Packet
	done chan struct{}
	once sync.Once

	wasMuted          bool
	waitingKeyframe   bool
	lastKeyframePLIAt time.Time
	resyncNeeded      atomic.Bool
}

func newForwarder(
	slot *slot,
	owner *Peer,
	kind string,
	fanout *fanout,
) *forwarder {
	bufferSize := audioForwarderBuffer
	if kind == "video" || kind == "screen" {
		bufferSize = videoForwarderBuffer
	}

	return &forwarder{
		slot:            slot,
		owner:           owner,
		kind:            kind,
		fanout:          fanout,
		ch:              make(chan *rtp.Packet, bufferSize),
		done:            make(chan struct{}),
		waitingKeyframe: kind == "video" || kind == "screen",
	}
}

func (f *forwarder) start() { go f.run() }

// finish encerra o forwarder e o remove do fanout (idempotente). Chamado quando
// o slot muda de ocupante, é liberado, ou quando o fanout é destruído.
func (f *forwarder) finish() {
	f.once.Do(func() {
		close(f.done)
		f.fanout.unsubscribe(f)
	})
}

// run consome os pacotes do fanout, traduz o SSN e escreve no slot. Termina
// quando encerrado (done) ou quando o WriteRTP falha (PC fechada).
func (f *forwarder) run() {
	defer f.finish()

	for {
		select {
		case <-f.done:
			return

		case pkt, ok := <-f.ch:
			if !ok {
				return
			}

			if f.resyncNeeded.Swap(false) && f.kind != "audio" {
				// Houve overflow local antes do envio. Como o translator esconde
				// o gap de SSN, continuar o frame produziria corrupção visual no
				// decoder. Descartamos até um novo keyframe.
				f.waitingKeyframe = true
				f.slot.translator.resetSource()
				f.lastKeyframePLIAt = time.Time{}
			}

			if f.kind == "audio" {
				if !f.owner.shouldForwardAudio() {
					f.wasMuted = true
					continue
				}

				if f.wasMuted {
					// O SSN da fonte avançou enquanto os pacotes eram
					// descartados. Recalcula o offset para continuar
					// exatamente após o último SSN enviado pelo slot.
					f.slot.translator.resetSource()
					f.wasMuted = false
				}
			} else if f.waitingKeyframe {
				if !isVideoKeyframe(f.fanout.track, pkt) {
					now := time.Now()
					if f.lastKeyframePLIAt.IsZero() || now.Sub(f.lastKeyframePLIAt) >= 500*time.Millisecond {
						f.lastKeyframePLIAt = now
						f.owner.requestKeyframe(f.fanout.track)
					}
					continue
				}

				// O primeiro pacote encaminhado para este subscriber deve iniciar
				// um frame decodificável. Isso evita tela cinza quando ele entra
				// no meio de um GOP ou perde o keyframe inicial.
				f.waitingKeyframe = false
				f.slot.translator.resetSource()
			}

			f.slot.translator.translate(pkt, f.owner)

			if err := f.slot.local.WriteRTP(pkt); err != nil {
				return
			}
		}
	}
}

// fanout é o ÚNICO reader de uma TrackRemote do publisher: lê cada pacote uma
// única vez e distribui cópias para os forwarders ativos (1 por subscriber).
//
// Sem o fanout, cada subscriber criava um forwarder chamando ReadRTP() na mesma
// track — e como ReadRTP consome o pacote, 2+ subscribers COMPETIAM pelos
// pacotes (cada um recebia uma fração do stream, com perdas aleatórias). O
// fanout centraliza a leitura e replica para N subscribers.
type fanout struct {
	track *webrtc.TrackRemote

	mu        sync.RWMutex
	subs      map[*forwarder]struct{}
	done      chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func newFanout(track *webrtc.TrackRemote) *fanout {
	return &fanout{
		track: track,
		subs:  make(map[*forwarder]struct{}),
		done:  make(chan struct{}),
	}
}

// start inicia o reader único da track uma única vez. Para áudio ele começa
// imediatamente; para vídeo/screen apenas depois que o primeiro forwarder já
// foi inscrito, evitando perder o keyframe inicial.
func (f *fanout) start() {
	f.startOnce.Do(func() {
		go f.readLoop()
	})
}

// subscribe adiciona um forwarder (chamado com o fanout.mu solto — adquire
// internamente).
func (f *fanout) subscribe(fwd *forwarder) {
	f.mu.Lock()
	select {
	case <-f.done:
		f.mu.Unlock()
		return
	default:
	}
	f.subs[fwd] = struct{}{}
	f.mu.Unlock()
}

// unsubscribe remove um forwarder (idempotente).
func (f *fanout) unsubscribe(fwd *forwarder) {
	f.mu.Lock()
	delete(f.subs, fwd)
	f.mu.Unlock()
}

// destroy encerra o reader e todos os forwarders (idempotente). Chamado quando
// a track é substituída (renegociação) ou o publisher sai/fecha.
func (f *fanout) destroy() {
	f.closeOnce.Do(func() {
		close(f.done)
		f.mu.Lock()
		subs := make([]*forwarder, 0, len(f.subs))
		for s := range f.subs {
			subs = append(subs, s)
		}
		f.subs = make(map[*forwarder]struct{})
		f.mu.Unlock()
		for _, s := range subs {
			s.finish()
		}
	})
}

// readLoop é o reader único da track: lê cada pacote uma vez e distribui aos
// forwarders. Termina quando a track encerra (publisher saiu, PC fechou ou
// receiver resetado na renegociação).
func (f *fanout) readLoop() {
	defer f.destroy()
	for {
		select {
		case <-f.done:
			return
		default:
		}
		pkt, _, err := f.track.ReadRTP()
		if err != nil {
			return
		}
		f.distribute(pkt)
	}
}

// distribute replica o pacote aos forwarders ativos sem alocar uma slice de
// subscribers a cada pacote. Em overflow descartamos o pacote MAIS ANTIGO e
// mantemos o mais novo, evitando aumentar a latência. Para vídeo/screen,
// qualquer overflow também força resync por keyframe para nunca entregar ao
// decoder um frame parcialmente perdido.
func (f *fanout) distribute(pkt *rtp.Packet) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	subCount := len(f.subs)

	for s := range f.subs {
		p := pkt
		if subCount > 1 {
			p = cloneRTPPacket(pkt)
		}

		select {
		case s.ch <- p:
			continue
		default:
		}

		// Mantém a fila próxima do tempo real: remove o mais antigo.
		select {
		case <-s.ch:
		default:
		}

		if s.kind == "video" || s.kind == "screen" {
			s.resyncNeeded.Store(true)
		}

		// Best effort: o consumidor pode ter alterado a fila entre os selects.
		select {
		case s.ch <- p:
		default:
		}
	}
}

// cloneRTPPacket precisa copiar apenas a struct/header, porque cada forwarder
// altera SequenceNumber. O payload é imutável neste SFU e TrackLocalStaticRTP
// copia a struct antes de ajustar SSRC/PT, portanto compartilhar []byte evita
// uma alocação grande por pacote por subscriber.
func cloneRTPPacket(pkt *rtp.Packet) *rtp.Packet {
	c := *pkt
	return &c
}

func isVideoKeyframe(track *webrtc.TrackRemote, pkt *rtp.Packet) bool {
	if track == nil || pkt == nil || len(pkt.Payload) == 0 {
		return false
	}

	switch strings.ToLower(track.Codec().MimeType) {
	case strings.ToLower(webrtc.MimeTypeVP8):
		var p codecs.VP8Packet
		payload, err := p.Unmarshal(pkt.Payload)
		if err != nil || p.S != 1 || p.PID != 0 || len(payload) == 0 {
			return false
		}
		// RFC 6386: bit 0 do primeiro byte do frame tag é 0 em keyframe.
		return payload[0]&0x01 == 0

	case strings.ToLower(webrtc.MimeTypeVP9):
		var p codecs.VP9Packet
		if _, err := p.Unmarshal(pkt.Payload); err != nil {
			return false
		}
		return p.B && !p.P

	case strings.ToLower(webrtc.MimeTypeH264):
		return isH264RandomAccessPacket(pkt.Payload)

	default:
		// Codec desconhecido: não bloqueie indefinidamente um stream válido.
		return true
	}
}

func isH264RandomAccessPacket(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}

	const (
		naluTypeMask = 0x1f
		idrType      = 5
		spsType      = 7
		ppsType      = 8
		stapAType    = 24
		fuAType      = 28
	)

	switch typ := payload[0] & naluTypeMask; typ {
	case idrType, spsType, ppsType:
		return true

	case fuAType:
		if len(payload) < 2 {
			return false
		}
		start := payload[1]&0x80 != 0
		originalType := payload[1] & naluTypeMask
		return start && originalType == idrType

	case stapAType:
		for off := 1; off+2 <= len(payload); {
			n := int(binary.BigEndian.Uint16(payload[off : off+2]))
			off += 2
			if n <= 0 || off+n > len(payload) {
				return false
			}
			typ := payload[off] & naluTypeMask
			if typ == idrType || typ == spsType || typ == ppsType {
				return true
			}
			off += n
		}
	}

	return false
}

// ssnTranslator traduz o sequence number por slot (prática padrão de SFU):
// o SSN do publisher é remapeado para continuar a sequência do slot, de modo
// que o subscriber veja um stream contínuo mesmo quando o ocupante do slot
// muda. O SSRC é sobrescrito pelo sender (não traduzimos).
type ssnTranslator struct {
	mu         sync.Mutex
	lastSSN    uint16
	hasWritten bool
	owner      *Peer
	offset     uint16
}

func (t *ssnTranslator) translate(pkt *rtp.Packet, owner *Peer) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.owner != owner {
		t.owner = owner
		if !t.hasWritten {
			// Slot novo: mantém o SSN do publisher (offset 0).
			t.offset = 0
		} else {
			// Slot já estava enviando: continua do último SSN do slot.
			t.offset = (t.lastSSN + 1) - pkt.Header.SequenceNumber
		}
	}
	pkt.Header.SequenceNumber += t.offset
	t.lastSSN = pkt.Header.SequenceNumber
	t.hasWritten = true
}
