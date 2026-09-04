package webrtc

import (
	"context"
	"sync"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
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

// assign faz o slot forwardar a track do publisher (o caller segura peer.mu).
// Se já havia forwarder, ele é parado (o slot muda de ocupante/track).
func (s *slot) assign(owner *Peer, kind string, track *webrtc.TrackRemote) {
	s.owner = owner
	s.kind = kind
	s.src = track
	if s.fwd != nil {
		s.fwd.stop()
	}
	s.fwd = newForwarder(s, owner, track)
	s.fwd.start()
}

// release para de forwardar (slot fica vazio — sem pacotes, D5). O
// translator preserva o lastSSN para o próximo ocupante continuar o SSN de
// forma monótona (o caller segura peer.mu).
func (s *slot) release() {
	s.owner = nil
	if s.fwd != nil {
		s.fwd.stop()
		s.fwd = nil
	}
}

// forwarder é a goroutine que lê a track do publisher e escreve no slot com
// tradução de SSN por slot. Uma por par (publisher → slot) ativo.
type forwarder struct {
	slot   *slot
	owner  *Peer
	track  *webrtc.TrackRemote
	ctx    context.Context
	cancel context.CancelFunc
}

func newForwarder(slot *slot, owner *Peer, track *webrtc.TrackRemote) *forwarder {
	ctx, cancel := context.WithCancel(context.Background())
	return &forwarder{slot: slot, owner: owner, track: track, ctx: ctx, cancel: cancel}
}

func (f *forwarder) start() { go f.run() }
func (f *forwarder) stop()  { f.cancel() }

// run lê pacotes da track do publisher, traduz o SSN e escreve no slot.
// Termina quando a track encerra (publisher saiu/PC fechou) ou quando o slot
// muda de ocupante (cancel).
func (f *forwarder) run() {
	for {
		pkt, _, err := f.track.ReadRTP()
		if err != nil {
			return
		}
		select {
		case <-f.ctx.Done():
			return
		default:
		}
		f.slot.translator.translate(pkt, f.owner)
		if err := f.slot.local.WriteRTP(pkt); err != nil {
			return
		}
	}
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
