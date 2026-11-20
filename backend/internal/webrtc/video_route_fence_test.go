package webrtc

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"papo/internal/config"

	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

func TestVideoRouteFenceReuse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		optIn bool
	}{
		{"negotiated", true},
		{"legacy", false},
	} {
		t.Run(tc.name, func(t *testing.T) { testVideoRouteFenceReuse(t, tc.optIn) })
	}
}

func testVideoRouteFenceReuse(t *testing.T, optIn bool) {
	t.Helper()
	const extID = 14
	m := testManager(t, &config.Config{VoiceVideoCodec: "vp8", VoiceVideoSlots: 2})
	var err error
	m.api, err = newSharedAPI(m.cfg, m)
	if err != nil {
		t.Fatal(err)
	}
	var answer VoiceAnswer
	var snapshots []VoiceVideoRoutes
	var beforeSnapshot func()
	m.signaler.SendToClient = func(_ string, event any) {
		if response, ok := event.(VoiceAnswer); ok {
			answer = response
		}
		if routes, ok := event.(VoiceVideoRoutes); ok {
			if beforeSnapshot != nil {
				beforeSnapshot()
			}
			snapshots = append(snapshots, routes)
		}
	}
	room := &Room{m: m, channelID: "voice-channel", peers: make(map[string]*Peer)}
	sub := newPeer(m, room, "sub", "sub-client")
	defer sub.close()
	a := cameraPublisher(t, m, room, "a")
	defer a.close()
	b := cameraPublisher(t, m, room, "b")
	defer b.close()
	if err := sub.subscribeVideo(a, "video"); err != nil {
		t.Fatal(err)
	}
	if err := sub.handleOffer("sub-client", videoRouteOffer(t, optIn), false, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(answer.SDP, videoRouteExtensionURI) != optIn {
		t.Fatal("answer não respeitou o opt-in no offer")
	}
	if len(snapshots) != 1 || len(snapshots[0].Routes) != 1 {
		t.Fatalf("subscrição pendente: %#v", snapshots)
	}
	s := sub.videoSlots[0]
	if sub.videoSlots[1].routeExtID != 0 {
		t.Fatal("o segundo MID não ofereceu a extensão")
	}
	first := snapshots[0].Routes[0]
	old := s.fwd
	if optIn && (first.RTPRouteID == 0 || first.RTPRouteExtID != extID) {
		t.Fatalf("atribuição inicial não marcada: %#v", first)
	}

	publisher := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 41}}
	// Mesmo ID no publisher: a marca de saída não pode usar nem alterar esse slice.
	for _, id := range []uint8{extID, 3} {
		if err := publisher.SetExtension(id, []byte{0xff}); err != nil {
			t.Fatal(err)
		}
	}
	delayed := cloneRTPPacket(publisher)
	if s.translator.forward(delayed, old) != forwardWrite {
		t.Fatal("o pacote antigo deveria passar pelo tradutor")
	}

	write := func(f *forwarder, pkt *rtp.Packet) {
		t.Helper()
		if err := f.writeRTP(pkt); err != nil {
			t.Fatal(err)
		}
	}
	sub.releaseAllFrom(a)
	// Force o write novo antes do snapshot e o write já traduzido do
	// forwarder antigo depois, sem depender do scheduler.
	early := cloneRTPPacket(publisher)
	beforeSnapshot = func() {
		if s.translator.forward(early, s.fwd) != forwardWrite {
			t.Fatal("o pacote novo deveria passar pelo tradutor")
		}
		write(s.fwd, early)
	}
	if err := sub.assignVideoSlot(b, "video"); err != nil {
		t.Fatal(err)
	}
	beforeSnapshot = nil
	write(old, delayed)
	if len(snapshots) != 3 || len(snapshots[1].Routes) != 0 || len(snapshots[2].Routes) != 1 {
		t.Fatalf("liberação e reutilização: %#v", snapshots)
	}
	reused := snapshots[2].Routes[0]
	if reused.TrackID != first.TrackID || reused.UserID != "b" || reused.FirstSeq == nil || *reused.FirstSeq != 42 {
		t.Fatalf("rota reutilizada inválida: %#v", reused)
	}
	if !optIn {
		encoded, err := json.Marshal(reused)
		if err != nil || strings.Contains(string(encoded), "rtp_route_") ||
			len(early.GetExtension(extID)) != 1 || len(delayed.GetExtensionIDs()) != 2 {
			t.Fatalf("cliente legado não deve mudar: %s; err=%v", encoded, err)
		}
		if updates := sub.updateVideoRouteExtensions(map[*webrtc.RTPSender]uint8{s.sender: extID}); len(updates) != 0 || s.routeExtID != 0 {
			t.Fatal("um slot legado não pode começar a marcar durante a chamada")
		}
		return
	}
	assertMark := func(pkt *rtp.Packet, id uint64) {
		t.Helper()
		raw := pkt.GetExtension(extID)
		if pkt.ExtensionProfile != rtp.ExtensionProfileOneByte || len(pkt.GetExtensionIDs()) != 1 ||
			len(raw) != 8 || binary.BigEndian.Uint64(raw) != id {
			t.Fatalf("marca RTP = %x (IDs=%v), esperado %d", raw, pkt.GetExtensionIDs(), id)
		}
	}
	if reused.RTPRouteID <= first.RTPRouteID || reused.RTPRouteExtID != extID {
		t.Fatalf("ID não renovado: %#v", reused)
	}
	encoded, err := json.Marshal(reused)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	var wireID uint64
	var wireExtID uint8
	if json.Unmarshal(wire["rtp_route_id"], &wireID) != nil || wireID != reused.RTPRouteID ||
		json.Unmarshal(wire["rtp_route_ext_id"], &wireExtID) != nil || wireExtID != extID {
		t.Fatalf("par numérico inválido: %s", encoded)
	}
	assertMark(early, reused.RTPRouteID)
	assertMark(delayed, first.RTPRouteID)
	if raw := publisher.GetExtension(extID); len(raw) != 1 || raw[0] != 0xff || len(publisher.GetExtensionIDs()) != 2 {
		t.Fatal("extensões compartilhadas do publisher foram modificadas")
	}

	// Troca da TrackRemote mantendo publisher, slot e mídia.
	track := &webrtc.TrackRemote{}
	b.mu.Lock()
	b.videoTrack = track
	b.fanouts["video"] = startedFanout(track)
	b.mu.Unlock()
	sub.rebindVideoSlot(b, "video", track)
	if len(snapshots) != 4 || len(snapshots[3].Routes) != 1 || snapshots[3].Routes[0].RTPRouteID <= reused.RTPRouteID {
		t.Fatalf("troca de track não renovou o ID: %#v", snapshots)
	}
	rebound := cloneRTPPacket(publisher)
	write(s.fwd, rebound)
	assertMark(rebound, snapshots[3].Routes[0].RTPRouteID)

	// Remover a extensão em um offer posterior nunca volta a repassar
	// bytes não confiáveis enquanto o subscriber ainda tem a rota marcada.
	updates := sub.updateVideoRouteExtensions(nil)
	if len(updates) != 1 || updates[0].(VoiceVideoRoutes).Routes[0].RTPRouteID != 0 {
		t.Fatalf("a rota sem extensão ainda anuncia a marca: %#v", updates)
	}
	if err := s.fwd.writeRTP(cloneRTPPacket(publisher)); !errors.Is(err, errVideoRouteUnavailable) {
		t.Fatalf("downgrade deixou encaminhar o pacote sem marca: %v", err)
	}
	updates = sub.updateVideoRouteExtensions(map[*webrtc.RTPSender]uint8{s.sender: extID})
	if len(updates) != 1 || updates[0].(VoiceVideoRoutes).Routes[0].RTPRouteID <= snapshots[3].Routes[0].RTPRouteID {
		t.Fatalf("a restauração não renovou a marca: %#v", updates)
	}
}

func videoRouteOffer(t *testing.T, optIn bool) string {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	for range 2 {
		if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
			webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			t.Fatal(err)
		}
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !optIn {
		return offer.SDP
	}
	var desc sdp.SessionDescription
	if err := desc.Unmarshal([]byte(offer.SDP)); err != nil {
		t.Fatal(err)
	}
	desc.MediaDescriptions[0].WithValueAttribute("extmap", "14/recvonly "+videoRouteExtensionURI)
	out, err := desc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
