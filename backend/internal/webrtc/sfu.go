package webrtc

import (
	"sync"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// forwarderBuffer é o tamanho do buffer de pacotes por forwarder (slot).
// Absorve bursts; quando estoura (subscriber lento), o pacote é descartado
// (drop-on-overflow) para não dar backpressure ao reader único do fanout.
const forwarderBuffer = 64

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

// assignWithFanout faz o slot forwardar a track do publisher via o fanout
// dela (o caller segura o peer.mu do SUBSCRIBER; o fanout já foi resolvido no
// publisher sem segurar este lock — ver Peer.fanoutFor). Se já havia
// forwarder, ele é terminado (o slot muda de ocupante/track).
func (s *slot) assignWithFanout(owner *Peer, kind string, track *webrtc.TrackRemote, fanout *fanout) {
	if s.fwd != nil {
		s.fwd.finish()
		s.fwd = nil
	}
	s.owner = owner
	s.kind = kind
	s.src = track
	s.fwd = newForwarder(s, owner, kind, fanout)
	fanout.subscribe(s.fwd)
	s.fwd.start()
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
}

func newForwarder(
	slot *slot,
	owner *Peer,
	kind string,
	fanout *fanout,
) *forwarder {
	return &forwarder{
		slot:   slot,
		owner:  owner,
		kind:   kind,
		fanout: fanout,
		ch:     make(chan *rtp.Packet, forwarderBuffer),
		done:   make(chan struct{}),
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

			if f.kind == "audio" && f.owner.isMuted() {
				continue
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

	mu   sync.Mutex
	subs map[*forwarder]struct{}
	done chan struct{}
	once sync.Once
}

func newFanout(track *webrtc.TrackRemote) *fanout {
	return &fanout{
		track: track,
		subs:  make(map[*forwarder]struct{}),
		done:  make(chan struct{}),
	}
}

// start inicia o reader único da track.
func (f *fanout) start() { go f.readLoop() }

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
	f.once.Do(func() {
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

// distribute replica o pacote aos forwarders ativos. Com 2+ subscribers cada
// um recebe uma CÓPIA (o translator altera o SSN in-place); com 1 subscriber
// recebe o pacote original (sem alocação). Subscriber lento → drop.
func (f *fanout) distribute(pkt *rtp.Packet) {
	f.mu.Lock()
	subs := make([]*forwarder, 0, len(f.subs))
	for s := range f.subs {
		subs = append(subs, s)
	}
	f.mu.Unlock()

	for _, s := range subs {
		var p *rtp.Packet
		if len(subs) > 1 {
			p = cloneRTPPacket(pkt)
		} else {
			p = pkt
		}
		select {
		case s.ch <- p:
		default:
			// subscriber lento: descarta (sem backpressure no reader único).
		}
	}
}

// cloneRTPPacket faz uma cópia suficiente para o fan-out: copia o header (o
// translator só altera SequenceNumber, um valor) e o payload (compartilhado
// seria corrompido por WriteRTP concorrente). CSRC/Extensions são lidos
// somente, então podem ser compartilhados.
func cloneRTPPacket(pkt *rtp.Packet) *rtp.Packet {
	c := *pkt
	c.Payload = append([]byte(nil), pkt.Payload...)
	return &c
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
