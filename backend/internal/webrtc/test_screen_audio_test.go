package webrtc

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"papo/internal/config"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// O áudio da tela é a SEGUNDA publicação de áudio do peer: o microfone vem
// primeiro e continua publicando; a segunda só é aceita durante a transmissão.
func TestParseMidRolesScreenAudio(t *testing.T) {
	micOnly := "v=0\n" +
		"o=- 1 2 IN IP4 127.0.0.1\n" +
		"s=-\n" +
		"t=0 0\n" +
		"m=audio 9 UDP/TLS/RTP/SAVPF 111\n" +
		"c=IN IP4 127.0.0.1\n" +
		"a=mid:0\n" +
		"a=sendrecv\n" +
		"a=rtpmap:111 opus/48000/2\n"

	audioLine := func(mid, direction string) string {
		return "m=audio 9 UDP/TLS/RTP/SAVPF 111\n" +
			"c=IN IP4 127.0.0.1\n" +
			"a=mid:" + mid + "\n" +
			"a=" + direction + "\n" +
			"a=rtpmap:111 opus/48000/2\n"
	}

	roles, _, err := parseMidRoles(micOnly, nil, false, false)
	if err != nil {
		t.Fatalf("offer só com mic: %v", err)
	}
	if roles["0"] != roleAudio {
		t.Fatalf("mid 0 = %v, esperado roleAudio", roles["0"])
	}

	withScreenAudio := micOnly + audioLine("5", "sendonly")

	// Sem transmissão ligada, um segundo áudio continua proibido.
	if _, _, err := parseMidRoles(withScreenAudio, roles, false, false); !errors.Is(err, ErrVoiceInvalidSDP) {
		t.Fatalf("segundo áudio sem screen: esperado ErrVoiceInvalidSDP, obtive %v", err)
	}

	sharing, active, err := parseMidRoles(withScreenAudio, roles, false, true)
	if err != nil {
		t.Fatalf("segundo áudio com screen: %v", err)
	}
	if sharing["0"] != roleAudio || active["0"] != roleAudio {
		t.Fatalf("mic perdeu o papel: roles=%#v active=%#v", sharing, active)
	}
	if sharing["5"] != roleScreenAudio || active["5"] != roleScreenAudio {
		t.Fatalf("mid 5 deveria ser screen audio ativo: roles=%#v active=%#v", sharing, active)
	}

	// Um terceiro áudio publicando é sempre rejeitado.
	if _, _, err := parseMidRoles(withScreenAudio+audioLine("6", "sendonly"), sharing, false, true); !errors.Is(err, ErrVoiceInvalidSDP) {
		t.Fatalf("terceiro áudio: esperado ErrVoiceInvalidSDP, obtive %v", err)
	}

	// Transmissão desligada: o MID mantém o papel, mas deixa de estar ativo.
	stopped, active, err := parseMidRoles(withScreenAudio, sharing, false, false)
	if err != nil {
		t.Fatalf("offer após parar a transmissão: %v", err)
	}
	if stopped["5"] != roleScreenAudio {
		t.Fatalf("mid 5 perdeu o papel: %#v", stopped)
	}
	if _, ok := active["5"]; ok {
		t.Fatalf("screen audio continuou ativo sem transmissão: %#v", active)
	}
	if active["0"] != roleAudio {
		t.Fatalf("mic deixou de estar ativo: %#v", active)
	}

	// Os slots recvonly do cliente (K de mic + S de tela) não ganham papel.
	withSlots := micOnly + audioLine("1", "recvonly") + audioLine("2", "recvonly")
	slots, active, err := parseMidRoles(withSlots, nil, false, true)
	if err != nil {
		t.Fatalf("offer com slots recvonly: %v", err)
	}
	if hasRole(slots, roleScreenAudio) || hasRole(active, roleScreenAudio) {
		t.Fatalf("slot recvonly foi confundido com screen audio: %#v", slots)
	}

	// Mic e áudio da tela chegando no mesmo offer (reconexão já transmitindo).
	both, active, err := parseMidRoles(withScreenAudio, nil, false, true)
	if err != nil {
		t.Fatalf("mic + screen audio no primeiro offer: %v", err)
	}
	if both["0"] != roleAudio || both["5"] != roleScreenAudio || active["5"] != roleScreenAudio {
		t.Fatalf("classificação no primeiro offer: roles=%#v active=%#v", both, active)
	}
}

// O mute vale para o microfone mesmo durante a transmissão: o áudio da tela
// tem faixa própria e não depende mais de encaminhar a faixa do mic.
func TestMutedScreenSharerMicIsNotRouted(t *testing.T) {
	p := &Peer{
		userID:        "streamer",
		muted:         true,
		screenSharing: true,
		screenAudio:   true,
		activeMidRole: map[string]trackRole{},
	}

	muted, _, _ := p.audioRoutingSnapshot()
	if !muted {
		t.Fatal("peer mutado e transmitindo deveria continuar mutado no roteamento de mic")
	}

	state := p.state()
	if !state.ScreenAudio || !state.ScreenSharing || !state.Muted {
		t.Fatalf("estado publicado = %#v", state)
	}
}

// Overflow na fila de um forwarder de áudio de tela não pode colocá-lo em
// espera de keyframe (isso só existe para vídeo) — senão o áudio nunca volta.
func TestScreenAudioForwarderDoesNotWaitForKeyframe(t *testing.T) {
	opus := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	local, err := webrtc.NewTrackLocalStaticRTP(opus, "papo-screen-audio-0", "papo-screen-audio")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP: %v", err)
	}

	owner := &Peer{userID: "streamer", muted: true}
	fan := newFanout(&webrtc.TrackRemote{})
	s := &slot{local: local}
	f := newForwarder(s, owner, "screen_audio", fan)
	if f.waitingKeyframe {
		t.Fatal("forwarder de screen_audio nasceu esperando keyframe")
	}

	f.resyncNeeded.Store(true)
	f.start()
	f.ch <- &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 7}, Payload: []byte{1}}

	deadline := time.Now().Add(2 * time.Second)
	for f.resyncNeeded.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	f.finish()
	// Aguarda a goroutine do forwarder sair antes de ler seus campos.
	time.Sleep(20 * time.Millisecond)

	if f.resyncNeeded.Load() {
		t.Fatal("forwarder não consumiu o pacote")
	}
	if f.waitingKeyframe {
		t.Fatal("overflow colocou o áudio da tela em espera de keyframe")
	}
	if f.wasMuted {
		t.Fatal("mute do microfone foi aplicado ao áudio da tela")
	}
}

// Rotas: o snapshot voice_audio_routes diferencia microfone e áudio da tela e
// é reenviado quando o slot de áudio da tela é liberado.
func TestScreenAudioRoutesKindAndRelease(t *testing.T) {
	events := make(chan any, 4)
	m := testManager(t, &config.Config{VoiceAudioSlots: 1, VoiceScreenAudioSlots: 1})
	m.signaler.SendToClient = func(clientID string, event any) {
		events <- event
	}

	room := &Room{m: m, channelID: "voice-channel", peers: map[string]*Peer{}}
	pub := &Peer{userID: "streamer"}
	room.peers["streamer"] = pub
	sub := &Peer{
		m:                m,
		room:             room,
		userID:           "viewer",
		signalingClient:  "viewer-client",
		pendingVideoSubs: map[string]pendingVideoSubscription{},
	}

	opus := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	micLocal, err := webrtc.NewTrackLocalStaticRTP(opus, "papo-audio-0", "papo-audio")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP: %v", err)
	}
	screenLocal, err := webrtc.NewTrackLocalStaticRTP(opus, "papo-screen-audio-0", "papo-screen-audio")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP: %v", err)
	}

	sub.audioSlots = []*slot{{peer: sub, local: micLocal, owner: pub, kind: "audio"}}
	sub.screenAudioSlots = []*slot{{peer: sub, local: screenLocal, owner: pub, kind: "screen_audio"}}

	want := []VoiceAudioRoute{
		{TrackID: "papo-audio-0", UserID: "streamer", Kind: "audio"},
		{TrackID: "papo-screen-audio-0", UserID: "streamer", Kind: "screen_audio"},
	}
	if got := sub.audioRoutesLocked(); !sameAudioRoutes(got, want) {
		t.Fatalf("routes = %#v, esperado %#v", got, want)
	}

	// track_unsubscribe kind=screen_audio libera só o slot de áudio da tela.
	sub.releaseSlot("streamer", "screen_audio")

	select {
	case raw := <-events:
		event, ok := raw.(VoiceAudioRoutes)
		if !ok {
			t.Fatalf("evento = %T, esperado VoiceAudioRoutes", raw)
		}
		if !sameAudioRoutes(event.Routes, want[:1]) {
			t.Fatalf("routes após unsubscribe = %#v, esperado só o microfone", event.Routes)
		}
	case <-time.After(time.Second):
		t.Fatal("voice_audio_routes não foi enviado ao liberar o áudio da tela")
	}

	// Liberar vídeo/screen não mexe nas rotas de áudio: nenhum evento.
	sub.releaseSlot("streamer", "screen")
	select {
	case raw := <-events:
		t.Fatalf("evento inesperado ao liberar screen: %#v", raw)
	case <-time.After(100 * time.Millisecond):
	}
}

// ---------------------------------------------------------------------------
// Ponta a ponta: Manager real + PeerConnections reais (loopback).
// ---------------------------------------------------------------------------

const (
	micMarker    byte = 0xA1
	screenMarker byte = 0xB2
)

// voiceTestClient é um cliente mínimo do protocolo de voz: uma PeerConnection
// que oferece o microfone (sendrecv) + os slots de recepção, e registra o
// primeiro byte do payload de cada pacote recebido por track id.
type voiceTestClient struct {
	t        *testing.T
	m        *Manager
	channel  string
	userID   string
	clientID string
	pc       *webrtc.PeerConnection
	mic      *webrtc.TrackLocalStaticRTP

	events chan any

	mu         sync.Mutex
	hasAnswer  bool
	pendingICE []webrtc.ICECandidateInit
	received   map[string]map[byte]int
}

// newVoiceTestClient cria o cliente; recvAudio/recvVideo são as m-lines
// recvonly oferecidas além da m-line do microfone.
func newVoiceTestClient(
	t *testing.T,
	m *Manager,
	channel, userID string,
	recvAudio, recvVideo int,
) *voiceTestClient {
	t.Helper()

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection(%s): %v", userID, err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	c := &voiceTestClient{
		t:        t,
		m:        m,
		channel:  channel,
		userID:   userID,
		clientID: userID + "-client",
		pc:       pc,
		events:   make(chan any, 64),
		received: make(map[string]map[byte]int),
	}

	opus := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	c.mic, err = webrtc.NewTrackLocalStaticRTP(opus, "mic-"+userID, "mic")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP: %v", err)
	}
	// Microfone: sendrecv (como no cliente real, recebe o primeiro slot).
	if _, err := pc.AddTrack(c.mic); err != nil {
		t.Fatalf("AddTrack(mic): %v", err)
	}
	for i := 0; i < recvAudio; i++ {
		if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			t.Fatalf("AddTransceiverFromKind(audio): %v", err)
		}
	}
	for i := 0; i < recvVideo; i++ {
		if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			t.Fatalf("AddTransceiverFromKind(video): %v", err)
		}
	}

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		id := track.ID()
		go func() {
			for {
				pkt, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				if len(pkt.Payload) == 0 {
					continue
				}
				c.mu.Lock()
				if c.received[id] == nil {
					c.received[id] = make(map[byte]int)
				}
				c.received[id][pkt.Payload[0]]++
				c.mu.Unlock()
			}
		}()
	})

	return c
}

// deliver recebe os eventos unicast do servidor (chamado por goroutines do
// SFU: nunca falha o teste daqui).
func (c *voiceTestClient) deliver(event any) {
	if ice, ok := event.(VoiceICECandidate); ok {
		init := webrtc.ICECandidateInit{Candidate: ice.Candidate, SDPMid: ice.SDPMid}
		if ice.SDPMLineIndex != nil {
			idx := uint16(*ice.SDPMLineIndex)
			init.SDPMLineIndex = &idx
		}
		c.mu.Lock()
		ready := c.hasAnswer
		if !ready {
			c.pendingICE = append(c.pendingICE, init)
		}
		c.mu.Unlock()
		if ready {
			_ = c.pc.AddICECandidate(init)
		}
		return
	}

	select {
	case c.events <- event:
	default:
	}
}

// waitEvent espera um evento unicast que satisfaça match (descarta os demais).
func (c *voiceTestClient) waitEvent(what string, match func(any) bool) any {
	c.t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case event := <-c.events:
			if voiceErr, ok := event.(VoiceError); ok {
				c.t.Fatalf("%s: servidor respondeu erro %q (%s) esperando %s",
					c.userID, voiceErr.Code, voiceErr.Message, what)
			}
			if match(event) {
				return event
			}
		case <-timeout:
			c.t.Fatalf("%s: timeout esperando %s", c.userID, what)
			return nil
		}
	}
}

// negotiate faz um ciclo voice_offer → voice_answer (candidatos do cliente vão
// dentro do SDP; os do servidor chegam por voice_ice_candidate).
func (c *voiceTestClient) negotiate() string {
	c.t.Helper()

	offer, err := c.pc.CreateOffer(nil)
	if err != nil {
		c.t.Fatalf("%s: CreateOffer: %v", c.userID, err)
	}
	gathered := webrtc.GatheringCompletePromise(c.pc)
	if err := c.pc.SetLocalDescription(offer); err != nil {
		c.t.Fatalf("%s: SetLocalDescription: %v", c.userID, err)
	}
	select {
	case <-gathered:
	case <-time.After(10 * time.Second):
		c.t.Fatalf("%s: ICE gathering não terminou", c.userID)
	}

	if err := c.m.ClientOffer(c.channel, c.userID, c.clientID, c.pc.LocalDescription().SDP); err != nil {
		c.t.Fatalf("%s: ClientOffer: %v", c.userID, err)
	}

	answer := c.waitEvent("voice_answer", func(event any) bool {
		_, ok := event.(VoiceAnswer)
		return ok
	}).(VoiceAnswer)

	if err := c.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answer.SDP,
	}); err != nil {
		c.t.Fatalf("%s: SetRemoteDescription(answer): %v", c.userID, err)
	}

	c.mu.Lock()
	c.hasAnswer = true
	pending := c.pendingICE
	c.pendingICE = nil
	c.mu.Unlock()
	for _, init := range pending {
		_ = c.pc.AddICECandidate(init)
	}

	return answer.SDP
}

// count retorna quantos pacotes com o marker chegaram em tracks cujo id começa
// com prefix.
func (c *voiceTestClient) count(prefix string, marker byte) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for id, markers := range c.received {
		if strings.HasPrefix(id, prefix) {
			total += markers[marker]
		}
	}
	return total
}

func (c *voiceTestClient) waitCount(what, prefix string, marker byte, min int) {
	c.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c.count(prefix, marker) >= min {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatalf("%s: %s (recebidos %d, esperado ≥ %d)", c.userID, what, c.count(prefix, marker), min)
}

func (c *voiceTestClient) waitRoutes(what string, match func([]VoiceAudioRoute) bool) {
	c.t.Helper()
	c.waitEvent(what, func(event any) bool {
		routes, ok := event.(VoiceAudioRoutes)
		return ok && match(routes.Routes)
	})
}

func hasScreenAudioRoute(routes []VoiceAudioRoute, userID string) bool {
	for _, r := range routes {
		if r.Kind == "screen_audio" && r.UserID == userID && r.TrackID == "papo-screen-audio-0" {
			return true
		}
	}
	return false
}

// audioMsidsInOrder devolve o track id (msid) de cada m-line de áudio do SDP,
// na ordem ("" quando a m-line não envia nada).
func audioMsidsInOrder(sdpStr string) []string {
	var (
		out     []string
		inAudio bool
	)
	for _, line := range strings.Split(strings.ReplaceAll(sdpStr, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "m=audio"):
			inAudio = true
			out = append(out, "")
		case strings.HasPrefix(line, "m="):
			inAudio = false
		case inAudio && strings.HasPrefix(line, "a=msid:"):
			fields := strings.Fields(strings.TrimPrefix(line, "a=msid:"))
			if len(fields) == 2 {
				out[len(out)-1] = fields[1]
			}
		}
	}
	return out
}

// voiceTestHarness é um Manager real (API pion compartilhada) com o signaler
// ligado a clientes de teste.
type voiceTestHarness struct {
	t          *testing.T
	m          *Manager
	channel    string
	broadcasts chan any

	mu      sync.Mutex
	clients map[string]*voiceTestClient
}

func newVoiceTestHarness(t *testing.T) *voiceTestHarness {
	t.Helper()

	cfg := &config.Config{
		VoiceVideoCodec:         "vp8",
		VoiceVideoSlots:         1,
		VoiceAudioSlots:         2,
		VoiceScreenAudioSlots:   1,
		VoiceMaxRoomPeers:       25,
		VoiceMaxRoomsPerUser:    1,
		VoiceSignalRateLimit:    1000,
		VoiceSignalRateBurst:    1000,
		VoiceSubscribeRateLimit: 1000,
		VoiceSubscribeRateBurst: 1000,
	}
	m := testManager(t, cfg)
	api, err := newSharedAPI(cfg, m)
	if err != nil {
		t.Fatalf("newSharedAPI: %v", err)
	}
	m.api = api
	t.Cleanup(m.Shutdown)

	h := &voiceTestHarness{
		t:          t,
		m:          m,
		channel:    "voice-channel",
		broadcasts: make(chan any, 256),
		clients:    make(map[string]*voiceTestClient),
	}
	m.signaler.SendToClient = func(clientID string, event any) {
		h.mu.Lock()
		c := h.clients[clientID]
		h.mu.Unlock()
		if c != nil {
			c.deliver(event)
		}
	}
	m.signaler.BroadcastToUsers = func(_ map[string]bool, event any) {
		select {
		case h.broadcasts <- event:
		default:
		}
	}
	return h
}

// join entra na sala com um cliente que oferece o microfone + recvAudio/
// recvVideo m-lines de recepção.
func (h *voiceTestHarness) join(userID string, recvAudio, recvVideo int) *voiceTestClient {
	h.t.Helper()
	c := newVoiceTestClient(h.t, h.m, h.channel, userID, recvAudio, recvVideo)
	h.mu.Lock()
	h.clients[c.clientID] = c
	h.mu.Unlock()
	if err := h.m.Join(h.channel, userID, c.clientID); err != nil {
		h.t.Fatalf("Join(%s): %v", userID, err)
	}
	return c
}

// joinCurrent entra com um cliente atualizado: K slots de microfone (um deles
// na m-line do próprio mic) + S de áudio de tela + N de vídeo.
func (h *voiceTestHarness) joinCurrent(userID string) *voiceTestClient {
	h.t.Helper()
	cfg := h.m.cfg
	return h.join(userID, cfg.VoiceAudioSlots-1+cfg.VoiceScreenAudioSlots, cfg.VoiceVideoSlots)
}

func (h *voiceTestHarness) waitScreenAudioState(userID string, want bool) {
	h.t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case event := <-h.broadcasts:
			state, ok := event.(VoiceStateUpdate)
			if ok && state.UserID == userID && state.ScreenAudio == want {
				return
			}
		case <-timeout:
			h.t.Fatalf("timeout esperando voice_state_update screen_audio=%v de %s", want, userID)
		}
	}
}

// speak desmuta o cliente e escreve continuamente no microfone dele (e na
// faixa devolvida por screen(), quando houver) até o fim do teste.
func (h *voiceTestHarness) speak(c *voiceTestClient, screen func() *webrtc.TrackLocalStaticRTP) {
	h.t.Helper()
	if err := h.m.SetMuted(h.channel, c.userID, c.clientID, false); err != nil {
		h.t.Fatalf("SetMuted(false): %v", err)
	}
	stop := make(chan struct{})
	h.t.Cleanup(func() { close(stop) })
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		var seq uint16
		var ts uint32
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			seq++
			ts += 480
			_ = c.mic.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts},
				Payload: []byte{micMarker},
			})
			if screen == nil {
				continue
			}
			if track := screen(); track != nil {
				_ = track.WriteRTP(&rtp.Packet{
					Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts},
					Payload: []byte{screenMarker},
				})
			}
		}
	}()
}

// TestScreenAudioEndToEnd cobre o cenário do bug no nível do SFU: A transmite
// com áudio de tela em faixa própria e B assiste.
//   - o áudio da tela nunca viaja no slot de microfone, e só chega a quem
//     assinou screen_audio;
//   - mutar o microfone de A corta a voz, não o áudio da transmissão;
//   - unsubscribe e o fim da transmissão liberam o slot e atualizam as rotas.
func TestScreenAudioEndToEnd(t *testing.T) {
	h := newVoiceTestHarness(t)
	m, channel := h.m, h.channel
	waitScreenAudioState := h.waitScreenAudioState

	a := h.joinCurrent("user-a") // transmite
	b := h.joinCurrent("user-b") // assiste

	a.negotiate()
	answerB := b.negotiate()
	waitForConnected(t, a.pc)
	waitForConnected(t, b.pc)

	// Os K primeiros transceivers de áudio do cliente são de microfone e os S
	// seguintes de áudio de tela (contrato de ordem dos slots).
	wantMsids := []string{"papo-audio-0", "papo-audio-1", "papo-screen-audio-0"}
	if got := audioMsidsInOrder(answerB); strings.Join(got, ",") != strings.Join(wantMsids, ",") {
		t.Fatalf("ordem dos slots de áudio no answer = %v, esperado %v", got, wantMsids)
	}

	// A fala (mic desmutado) e escreve continuamente no microfone e, quando
	// houver, na faixa de áudio da tela.
	var screenTrack atomic.Pointer[webrtc.TrackLocalStaticRTP]
	h.speak(a, screenTrack.Load)

	b.waitCount("voz de A não chegou a B", "papo-audio-", micMarker, 5)

	// Antes de A transmitir, não há o que assinar.
	if err := m.Subscribe(channel, b.userID, b.clientID, a.userID, "screen_audio"); !errors.Is(err, ErrVoiceNotFound) {
		t.Fatalf("subscribe sem transmissão: esperado ErrVoiceNotFound, obtive %v", err)
	}

	// A começa a transmitir com áudio: segunda m-line de áudio, sendonly.
	if err := m.StartScreenShare(channel, a.userID, a.clientID); err != nil {
		t.Fatalf("StartScreenShare: %v", err)
	}
	opus := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	screen, err := webrtc.NewTrackLocalStaticRTP(opus, "screen-audio-a", "screen")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP(screen): %v", err)
	}
	screenTransceiver, err := a.pc.AddTransceiverFromTrack(screen, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendonly,
	})
	if err != nil {
		t.Fatalf("AddTransceiverFromTrack(screen): %v", err)
	}
	a.negotiate()
	screenTrack.Store(screen)

	waitScreenAudioState(a.userID, true)

	// Sem assinatura, o áudio da tela não chega a B em nenhuma faixa.
	time.Sleep(400 * time.Millisecond)
	if n := b.count("", screenMarker); n != 0 {
		t.Fatalf("B recebeu %d pacotes de áudio da tela sem assinar", n)
	}

	// Ninguém assina o próprio áudio de tela.
	if err := m.Subscribe(channel, a.userID, a.clientID, a.userID, "screen_audio"); !errors.Is(err, ErrVoiceNotFound) {
		t.Fatalf("auto-subscribe: esperado ErrVoiceNotFound, obtive %v", err)
	}

	if err := m.Subscribe(channel, b.userID, b.clientID, a.userID, "screen_audio"); err != nil {
		t.Fatalf("Subscribe(screen_audio): %v", err)
	}
	b.waitRoutes("rota screen_audio de A", func(routes []VoiceAudioRoute) bool {
		return hasScreenAudioRoute(routes, a.userID)
	})
	b.waitCount("áudio da tela não chegou após assinar", "papo-screen-audio-0", screenMarker, 5)

	// Mutar o microfone corta a voz de A, não o áudio da transmissão.
	if err := m.SetMuted(channel, a.userID, a.clientID, true); err != nil {
		t.Fatalf("SetMuted(true): %v", err)
	}
	time.Sleep(300 * time.Millisecond) // escoa o que já estava em trânsito
	micBefore := b.count("papo-audio-", micMarker)
	screenBefore := b.count("papo-screen-audio-0", screenMarker)
	b.waitCount("áudio da tela parou quando A mutou o microfone",
		"papo-screen-audio-0", screenMarker, screenBefore+10)
	if micAfter := b.count("papo-audio-", micMarker); micAfter != micBefore {
		t.Fatalf("voz de A continuou chegando com o mic mutado (%d → %d)", micBefore, micAfter)
	}

	// As faixas nunca se misturam.
	if n := b.count("papo-audio-", screenMarker); n != 0 {
		t.Fatalf("slot de microfone carregou %d pacotes de áudio da tela", n)
	}
	if n := b.count("papo-screen-audio-", micMarker); n != 0 {
		t.Fatalf("slot de áudio da tela carregou %d pacotes de microfone", n)
	}

	// track_unsubscribe: a rota some e os pacotes param.
	if err := m.Unsubscribe(channel, b.userID, b.clientID, a.userID, "screen_audio"); err != nil {
		t.Fatalf("Unsubscribe(screen_audio): %v", err)
	}
	b.waitRoutes("rotas sem screen_audio após unsubscribe", func(routes []VoiceAudioRoute) bool {
		return !hasScreenAudioRoute(routes, a.userID)
	})
	time.Sleep(300 * time.Millisecond)
	stopped := b.count("papo-screen-audio-0", screenMarker)
	time.Sleep(300 * time.Millisecond)
	if after := b.count("papo-screen-audio-0", screenMarker); after != stopped {
		t.Fatalf("áudio da tela continuou após unsubscribe (%d → %d)", stopped, after)
	}

	// Assina de novo e A encerra a transmissão: o slot é liberado pelo servidor.
	if err := m.Subscribe(channel, b.userID, b.clientID, a.userID, "screen_audio"); err != nil {
		t.Fatalf("Subscribe(screen_audio) de novo: %v", err)
	}
	b.waitRoutes("rota screen_audio de A (segunda vez)", func(routes []VoiceAudioRoute) bool {
		return hasScreenAudioRoute(routes, a.userID)
	})

	if err := m.StopScreenShare(channel, a.userID, a.clientID); err != nil {
		t.Fatalf("StopScreenShare: %v", err)
	}
	if err := a.pc.RemoveTrack(screenTransceiver.Sender()); err != nil {
		t.Fatalf("RemoveTrack(screen): %v", err)
	}
	a.negotiate()

	waitScreenAudioState(a.userID, false)
	b.waitRoutes("rotas sem screen_audio após o fim da transmissão", func(routes []VoiceAudioRoute) bool {
		return !hasScreenAudioRoute(routes, a.userID)
	})
	if err := m.Subscribe(channel, b.userID, b.clientID, a.userID, "screen_audio"); !errors.Is(err, ErrVoiceNotFound) {
		t.Fatalf("subscribe após o fim: esperado ErrVoiceNotFound, obtive %v", err)
	}

	// Uma segunda transmissão (em uma m-line nova) volta a funcionar: a m-line
	// antiga mantém o papel mas está inativa, e a nova é classificada.
	screenTrack.Store(nil)
	if err := m.StartScreenShare(channel, a.userID, a.clientID); err != nil {
		t.Fatalf("StartScreenShare (segunda vez): %v", err)
	}
	screen2, err := webrtc.NewTrackLocalStaticRTP(opus, "screen-audio-a-2", "screen")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP(screen2): %v", err)
	}
	if _, err := a.pc.AddTransceiverFromTrack(screen2, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendonly,
	}); err != nil {
		t.Fatalf("AddTransceiverFromTrack(screen2): %v", err)
	}
	a.negotiate()
	screenTrack.Store(screen2)

	waitScreenAudioState(a.userID, true)
	if err := m.Subscribe(channel, b.userID, b.clientID, a.userID, "screen_audio"); err != nil {
		t.Fatalf("Subscribe(screen_audio) na segunda transmissão: %v", err)
	}
	restarted := b.count("papo-screen-audio-0", screenMarker)
	b.waitCount("áudio da segunda transmissão não chegou",
		"papo-screen-audio-0", screenMarker, restarted+5)
}

// TestLegacyClientWithoutScreenAudioSlots: um cliente que só oferece as m-lines
// de microfone (sem os slots de áudio de tela e sem vídeo, como o cliente de
// terminal) continua entrando, ouvindo as vozes e renegociando normalmente.
func TestLegacyClientWithoutScreenAudioSlots(t *testing.T) {
	h := newVoiceTestHarness(t)

	speaker := h.joinCurrent("user-speaker")
	// Só os K slots de microfone: 1 na m-line do mic + (K-1) recvonly.
	legacy := h.join("user-legacy", h.m.cfg.VoiceAudioSlots-1, 0)

	speaker.negotiate()
	answer := legacy.negotiate()
	waitForConnected(t, speaker.pc)
	waitForConnected(t, legacy.pc)

	want := []string{"papo-audio-0", "papo-audio-1"}
	if got := audioMsidsInOrder(answer); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("slots de áudio do cliente antigo = %v, esperado %v", got, want)
	}

	h.speak(speaker, nil)
	legacy.waitCount("voz não chegou ao cliente antigo", "papo-audio-", micMarker, 5)

	// Os slots de áudio de tela que sobraram sem m-line não podem atrapalhar
	// uma renegociação posterior do mesmo cliente.
	legacy.negotiate()
	before := legacy.count("papo-audio-", micMarker)
	legacy.waitCount("voz parou após a renegociação do cliente antigo",
		"papo-audio-", micMarker, before+10)
}
