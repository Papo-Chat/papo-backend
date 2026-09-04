package webrtc

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"papo/internal/config"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// testManager monta um Manager mínimo (signaler no-op) para testes que
// precisam de cfg + signaler sem criar PeerConnections.
func testManager(t *testing.T, cfg *config.Config) *Manager {
	t.Helper()
	if cfg == nil {
		cfg = &config.Config{
			VoiceVideoCodec:   "vp8",
			VoiceVideoSlots:   6,
			VoiceAudioSlots:   4,
			VoiceMaxRoomPeers: 25,
		}
	}
	return &Manager{
		cfg: cfg,
		signaler: Signaler{
			SendToUser:       func(userID string, event any) {},
			BroadcastToUsers: func(allowed map[string]bool, event any) {},
			VoiceAudience:    func(channelID string) map[string]bool { return map[string]bool{} },
		},
		rooms:     make(map[string]*Room),
		limiters:  make(map[string]*userLimiters),
		userRooms: make(map[string]map[string]struct{}),
		ssrcOwner: make(map[uint32]ssrcOwner),
	}
}

func TestSSNTranslator(t *testing.T) {
	tr := &ssnTranslator{}
	ownerA := &Peer{}
	ownerB := &Peer{}

	// slot novo (ownerA): mantém o SSN do publisher (offset 0)
	tr.translate(&rtp.Packet{Header: rtp.Header{SequenceNumber: 100}}, ownerA)
	if got := 100; got != 100 {
		t.Fatalf("primeiro pacote: SSN = %d, esperado 100", got)
	}

	// mesmo owner: sequência continua
	pkt := &rtp.Packet{Header: rtp.Header{SequenceNumber: 101}}
	tr.translate(pkt, ownerA)
	if pkt.Header.SequenceNumber != 101 {
		t.Fatalf("mesmo owner: SSN = %d, esperado 101", pkt.Header.SequenceNumber)
	}

	// troca de owner (ownerB): continua do último SSN do slot (101 → 102)
	pkt = &rtp.Packet{Header: rtp.Header{SequenceNumber: 50}}
	tr.translate(pkt, ownerB)
	if pkt.Header.SequenceNumber != 102 {
		t.Fatalf("troca de owner: SSN = %d, esperado 102", pkt.Header.SequenceNumber)
	}

	// mesmo owner (ownerB): continua (51 → 103)
	pkt = &rtp.Packet{Header: rtp.Header{SequenceNumber: 51}}
	tr.translate(pkt, ownerB)
	if pkt.Header.SequenceNumber != 103 {
		t.Fatalf("mesmo owner (B): SSN = %d, esperado 103", pkt.Header.SequenceNumber)
	}
}

func TestParseMidRoles(t *testing.T) {
	sdp := `v=0
o=- 1 2 IN IP4 127.0.0.1
s=-
t=0 0
a=group:BUNDLE 0 1 2
m=audio 9 UDP/TLS/RTP/SAVPF 111
c=IN IP4 127.0.0.1
a=mid:0
m=video 9 UDP/TLS/RTP/SAVPF 96
c=IN IP4 127.0.0.1
a=mid:1
a=rtpmap:96 vp8/90000
m=video 9 UDP/TLS/RTP/SAVPF 96
c=IN IP4 127.0.0.1
a=mid:2
a=rtpmap:96 vp8/90000`

	roles := parseMidRoles(sdp)
	if roles["0"] != roleAudio {
		t.Errorf("mid 0 = %d, esperado roleAudio", roles["0"])
	}
	if roles["1"] != roleCamera {
		t.Errorf("mid 1 = %d, esperado roleCamera (1ª vídeo)", roles["1"])
	}
	if roles["2"] != roleScreen {
		t.Errorf("mid 2 = %d, esperado roleScreen (2ª vídeo)", roles["2"])
	}
	if len(roles) != 3 {
		t.Errorf("esperava 3 mids, obtive %d", len(roles))
	}
}

func TestAudioLevelToDBFS(t *testing.T) {
	// RFC 6464: o valor 0-127 já é o dBFS negado (0 = mais alto, 127 = silêncio).
	if got := audioLevelToDBFS(0); got != 0 {
		t.Errorf("level=0: dBFS = %f, esperado 0 (mais alto)", got)
	}
	if got := audioLevelToDBFS(127); got != -127 {
		t.Errorf("level=127: dBFS = %f, esperado -127 (silêncio)", got)
	}
	if got := audioLevelToDBFS(1); got != -1 {
		t.Errorf("level=1: dBFS = %f, esperado -1", got)
	}
	if got := audioLevelToDBFS(64); got != -64 {
		t.Errorf("level=64: dBFS = %f, esperado -64", got)
	}
}

func TestTopKLocked(t *testing.T) {
	m := testManager(t, &config.Config{VoiceAudioSlots: 2})
	r := &Room{
		m:          m,
		channelID:  "ch1",
		peers:      make(map[string]*Peer),
		scores:     make(map[string]float64),
		tickerStop: make(chan struct{}),
	}

	r.mu.Lock()
	r.scores["a"] = -10
	r.scores["b"] = -20
	r.scores["c"] = -30
	r.scores["d"] = -20 // empate com b; tiebreak por id (b < d)
	topK := r.topKLocked("")
	r.mu.Unlock()

	if len(topK) != 2 || topK[0] != "a" || topK[1] != "b" {
		t.Fatalf("topK = %v, esperado [a b]", topK)
	}

	r.mu.Lock()
	topK = r.topKLocked("a") // exclui o próprio subscriber
	r.mu.Unlock()
	if len(topK) != 2 || topK[0] != "b" || topK[1] != "d" {
		t.Fatalf("topK(exclui a) = %v, esperado [b d]", topK)
	}
}

const sdpVP8 = `v=0
o=- 1 2 IN IP4 127.0.0.1
s=-
t=0 0
a=group:BUNDLE 0 1
m=audio 9 UDP/TLS/RTP/SAVPF 111
c=IN IP4 127.0.0.1
a=mid:0
a=rtpmap:111 opus/48000/2
m=video 9 UDP/TLS/RTP/SAVPF 96
c=IN IP4 127.0.0.1
a=mid:1
a=rtpmap:96 vp8/90000`

const sdpVP9 = `v=0
o=- 1 2 IN IP4 127.0.0.1
s=-
t=0 0
a=group:BUNDLE 0 1
m=audio 9 UDP/TLS/RTP/SAVPF 111
c=IN IP4 127.0.0.1
a=mid:0
a=rtpmap:111 opus/48000/2
m=video 9 UDP/TLS/RTP/SAVPF 97
c=IN IP4 127.0.0.1
a=mid:1
a=rtpmap:97 vp9/90000`

const sdpAudioOnly = `v=0
o=- 1 2 IN IP4 127.0.0.1
s=-
t=0 0
m=audio 9 UDP/TLS/RTP/SAVPF 111
c=IN IP4 127.0.0.1
a=mid:0
a=rtpmap:111 opus/48000/2`

func TestValidateSDP(t *testing.T) {
	cfg := testManager(t, nil).cfg

	// vazio
	if err := validateSDP("", cfg, true); !errors.Is(err, ErrVoiceInvalidSDP) {
		t.Errorf("SDP vazio: esperado ErrVoiceInvalidSDP, obtive %v", err)
	}
	// inválido
	if err := validateSDP("não é uma sdp", cfg, true); !errors.Is(err, ErrVoiceInvalidSDP) {
		t.Errorf("SDP inválida: esperado ErrVoiceInvalidSDP, obtive %v", err)
	}
	// válida com codec da sala (vp8)
	if err := validateSDP(sdpVP8, cfg, true); err != nil {
		t.Errorf("SDP vp8: esperado nil, obtive %v", err)
	}
	// válida sem m-line de vídeo (peer só com áudio)
	if err := validateSDP(sdpAudioOnly, cfg, true); err != nil {
		t.Errorf("SDP só áudio: esperado nil, obtive %v", err)
	}
	// vídeo com codec diferente da sala (vp9) → unsupported
	if err := validateSDP(sdpVP9, cfg, true); !errors.Is(err, ErrVoiceCodecUnsupported) {
		t.Errorf("SDP vp9: esperado ErrVoiceCodecUnsupported, obtive %v", err)
	}
	// answer (checkCodec=false) não valida codec
	if err := validateSDP(sdpVP9, cfg, false); err != nil {
		t.Errorf("answer vp9: esperado nil, obtive %v", err)
	}
}

func TestValidateSDPTooManyMlines(t *testing.T) {
	cfg := &config.Config{VoiceVideoCodec: "vp8", VoiceVideoSlots: 1, VoiceAudioSlots: 1}
	// teto = 3 + 1 + 1 = 5 m-lines; 6 m-lines excede
	var sb strings.Builder
	sb.WriteString("v=0\no=- 1 2 IN IP4 127.0.0.1\ns=-\nt=0 0\n")
	for i := 0; i < 6; i++ {
		sb.WriteString(fmt.Sprintf("m=audio 9 UDP/TLS/RTP/SAVPF 111\nc=IN IP4 127.0.0.1\na=mid:%d\n", i))
	}
	if err := validateSDP(sb.String(), cfg, true); !errors.Is(err, ErrVoiceInvalidSDP) {
		t.Errorf("6 m-lines: esperado ErrVoiceInvalidSDP, obtive %v", err)
	}
}

func TestValidateCandidate(t *testing.T) {
	// válido (IP privado — LAN é o caso principal, D15)
	if err := validateCandidate("candidate:1 1 UDP 2122260223 192.168.1.100 50000 typ host"); err != nil {
		t.Errorf("candidate válido: esperado nil, obtive %v", err)
	}
	// IP público
	if err := validateCandidate("candidate:2 1 UDP 1694498815 203.0.113.7 50000 typ srflx raddr 0.0.0.0 rport 0"); err != nil {
		t.Errorf("candidate público: esperado nil, obtive %v", err)
	}
	// loopback é rejeitado
	if err := validateCandidate("candidate:1 1 UDP 2122260223 127.0.0.1 50000 typ host"); !errors.Is(err, ErrVoiceInvalidSDP) {
		t.Errorf("loopback: esperado ErrVoiceInvalidSDP, obtive %v", err)
	}
	// vazio
	if err := validateCandidate(""); !errors.Is(err, ErrVoiceInvalidSDP) {
		t.Errorf("vazio: esperado ErrVoiceInvalidSDP, obtive %v", err)
	}
	// campos insuficientes
	if err := validateCandidate("candidate:1 1 UDP 2122260223"); !errors.Is(err, ErrVoiceInvalidSDP) {
		t.Errorf("poucos campos: esperado ErrVoiceInvalidSDP, obtive %v", err)
	}
}

// connectPCs faz a negociação offer/answer + troca de candidatos ICE entre duas
// PCs (loopback) e aguarda ambas conectadas.
func connectPCs(t *testing.T, a, b *webrtc.PeerConnection) {
	t.Helper()
	a.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = b.AddICECandidate(c.ToJSON())
		}
	})
	b.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = a.AddICECandidate(c.ToJSON())
		}
	})

	offer, err := a.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := a.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription(offer): %v", err)
	}
	if err := b.SetRemoteDescription(offer); err != nil {
		t.Fatalf("SetRemoteDescription(offer): %v", err)
	}
	answer, err := b.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("CreateAnswer: %v", err)
	}
	if err := b.SetLocalDescription(answer); err != nil {
		t.Fatalf("SetLocalDescription(answer): %v", err)
	}
	if err := a.SetRemoteDescription(answer); err != nil {
		t.Fatalf("SetRemoteDescription(answer): %v", err)
	}
	waitForConnected(t, a)
	waitForConnected(t, b)
}

func waitForConnected(t *testing.T, pc *webrtc.PeerConnection) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if pc.ConnectionState() == webrtc.PeerConnectionStateConnected {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("PC não conectou a tempo (estado: %s)", pc.ConnectionState())
}

// TestFanoutDistributesFullStreamToAllSubscribers valida a correção do fan-out
// (1 publisher + 2 subscribers). Antes da correção, cada subscriber tinha um
// forwarder chamando ReadRTP() na MESMA TrackRemote — como ReadRTP consome o
// pacote, os 2 forwarders COMPETIAM e cada subscriber recebia apenas uma fração
// do stream. Com o fanout (reader único + cópias), os dois subscribers recebem
// o stream COMPLETO e idêntico (mesma sequência de SSN).
func TestFanoutDistributesFullStreamToAllSubscribers(t *testing.T) {
	pubClient, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection(pubClient): %v", err)
	}
	defer pubClient.Close()
	pubServer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection(pubServer): %v", err)
	}
	defer pubServer.Close()

	trackCh := make(chan *webrtc.TrackRemote, 1)
	pubServer.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		trackCh <- track
	})

	opus := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	audioTrack, err := webrtc.NewTrackLocalStaticRTP(opus, "papo", "pub-audio")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP: %v", err)
	}
	if _, err := pubClient.AddTrack(audioTrack); err != nil {
		t.Fatalf("AddTrack: %v", err)
	}

	connectPCs(t, pubClient, pubServer)

	const n = 20
	// Publisher envia o stream em background (com pequeno espaçamento para o PC
	// entregar). O 1o pacote é o que dispara OnTrack no receiver. O SSRC/PT são
	// sobrescritos pelo pion na escrita (track já bound) — só o SSN importa.
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for i := 0; i < n; i++ {
			pkt := &rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: uint16(i + 1)},
				Payload: []byte{byte(i)},
			}
			_ = audioTrack.WriteRTP(pkt)
			time.Sleep(5 * time.Millisecond)
		}
	}()

	// Aguarda OnTrack (disparado pelo 1o pacote) e monta o fanout + 2 subs.
	select {
	case pubTrack := <-trackCh:
		// fanout: reader único da track do publisher.
		fanout := newFanout(pubTrack)
		fanout.start()
		defer fanout.destroy()

		// 2 subscribers (forwarders). Não iniciamos o run() — lemos direto do
		// canal para isolar a distribuição do fanout (sem WriteRTP/PC de sub).
		fwd1 := newForwarder(&slot{}, nil, fanout)
		fwd2 := newForwarder(&slot{}, nil, fanout)
		fanout.subscribe(fwd1)
		fanout.subscribe(fwd2)

		// Cada subscriber deve receber os 20 pacotes, na ordem, com o mesmo SSN.
		// (Antes da correção, os 2 forwarders competiam na ReadRTP e cada um
		// recebia apenas uma fração do stream — aqui ambos recebem o stream
		// completo e idêntico.)
		readAll := func(name string, ch chan *rtp.Packet) {
			t.Helper()
			for i := 0; i < n; i++ {
				select {
				case p := <-ch:
					if want := uint16(i + 1); p.Header.SequenceNumber != want {
						t.Fatalf("%s: pacote %d SSN = %d, esperado %d", name, i, p.Header.SequenceNumber, want)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("%s: não recebeu o pacote %d a tempo (fan-out dividiu o stream?)", name, i)
				}
			}
		}
		readAll("sub1", fwd1.ch)
		readAll("sub2", fwd2.ch)
		<-sent

	case <-time.After(10 * time.Second):
		t.Fatalf("não recebeu a TrackRemote do publisher a tempo")
	}
}

// TestRenegWorkerSurfacesErrors valida que erros de operações de signaling
// processadas pelo worker de renegociação (offer/answer/candidate) NÃO são
// descartados: o worker envia um evento `error` à conexão dona do signaling
// (SendToClient) para que o cliente possa reagir (tentar de novo, mostrar
// mensagem). Antes da correção, o erro era ignorado (só havia um comentário).
func TestRenegWorkerSurfacesErrors(t *testing.T) {
	cfg := &config.Config{
		VoiceVideoCodec:   "vp8",
		VoiceVideoSlots:   6,
		VoiceAudioSlots:   4,
		VoiceMaxRoomPeers: 25,
	}
	clientEvents := make(chan any, 16)
	userEvents := make(chan any, 16)
	m := &Manager{
		cfg: cfg,
		signaler: Signaler{
			SendToUser:       func(userID string, event any) { userEvents <- event },
			SendToClient:     func(clientID string, event any) { clientEvents <- event },
			BroadcastToUsers: func(allowed map[string]bool, event any) {},
			VoiceAudience:    func(channelID string) map[string]bool { return map[string]bool{} },
		},
		rooms:     make(map[string]*Room),
		limiters:  make(map[string]*userLimiters),
		userRooms: make(map[string]map[string]struct{}),
		ssrcOwner: make(map[uint32]ssrcOwner),
	}
	room := &Room{m: m, channelID: "test-channel"}
	peer := newPeer(m, room, "user-1")
	defer peer.close()

	// A conexão que originou o signaling (o offer definiu o signalingClient).
	peer.setSignalingClient("conn-1")

	// Simula uma operação de signaling que falha no worker (ex.: SDP inválido
	// detectado só no processamento, ou AddICECandidate rejeitado).
	if err := peer.enqueue(func() error { return ErrVoiceInvalidSDP }); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	select {
	case ev := <-clientEvents:
		ve, ok := ev.(VoiceError)
		if !ok {
			t.Fatalf("esperado VoiceError, obtive %T", ev)
		}
		if ve.Code != CodeVoiceInvalidSDP {
			t.Fatalf("code = %q, esperado %q", ve.Code, CodeVoiceInvalidSDP)
		}
		if ve.Message != ErrVoiceInvalidSDP.Error() {
			t.Fatalf("message = %q, esperado %q", ve.Message, ErrVoiceInvalidSDP.Error())
		}
	case ev := <-userEvents:
		t.Fatalf("erro foi enviado via SendToUser (todas as conexões) em vez de SendToClient: %T", ev)
	case <-time.After(2 * time.Second):
		t.Fatalf("erro da renegociação não foi enviado ao cliente (descartado?)")
	}
}

// freeUDPPort pega uma porta UDP livre (bind em 0 → o SO escolhe) e a devolve.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	l, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	port := l.LocalAddr().(*net.UDPAddr).Port
	l.Close()
	return port
}

// TestNewSharedAPIICEUDPMux valida o wiring do ICEUDPMux (VOICE_ICE_UDP_PORT):
//   - porta válida → mux criado (todas as PCs compartilham a porta);
//   - porta em uso → erro no boot (falha clara, não ICE flaky em runtime);
//   - porta 0 → mux desligado (portas efêmeras, comportamento legado).
func TestNewSharedAPIICEUDPMux(t *testing.T) {
	t.Run("porta válida cria mux", func(t *testing.T) {
		port := freeUDPPort(t)
		m := testManager(t, &config.Config{VoiceVideoCodec: "vp8", VoiceICEUDPPort: port})
		api, err := newSharedAPI(m.cfg, m)
		if err != nil {
			t.Fatalf("newSharedAPI: %v", err)
		}
		if api.iceMux == nil {
			t.Fatal("ICEUDPMux não foi criado com porta válida")
		}
		if err := api.iceMux.Close(); err != nil {
			t.Fatalf("fechar mux: %v", err)
		}
	})

	t.Run("porta em uso retorna erro", func(t *testing.T) {
		// Ocupa a porta em todas as interfaces IPv4 (0.0.0.0) para simular
		// conflito (ex.: outra instância do backend).
		l, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: 0})
		if err != nil {
			t.Fatalf("ListenUDP: %v", err)
		}
		defer l.Close()
		port := l.LocalAddr().(*net.UDPAddr).Port

		m := testManager(t, &config.Config{VoiceVideoCodec: "vp8", VoiceICEUDPPort: port})
		if _, err := newSharedAPI(m.cfg, m); err == nil {
			t.Fatal("esperado erro de porta em uso, obtive nil")
		}
	})

	t.Run("porta 0 desliga mux", func(t *testing.T) {
		m := testManager(t, &config.Config{VoiceVideoCodec: "vp8", VoiceICEUDPPort: 0})
		api, err := newSharedAPI(m.cfg, m)
		if err != nil {
			t.Fatalf("newSharedAPI: %v", err)
		}
		if api.iceMux != nil {
			t.Fatal("ICEUDPMux deveria estar desligado com porta 0")
		}
	})
}

// TestPeerConnectionWithICEUDPMux valida que o mux NÃO quebra a conectividade:
// uma PC "servidor" (API compartilhada, usa o ICEUDPMux na porta única) conecta
// de fato (ICE+DTLS) com uma PC "cliente" (portas efêmeras, com track de áudio)
// — o mesmo cenário de produção (SFU com porta fixa ↔ browser com microfone).
func TestPeerConnectionWithICEUDPMux(t *testing.T) {
	port := freeUDPPort(t)
	m := testManager(t, &config.Config{VoiceVideoCodec: "vp8", VoiceICEUDPPort: port})
	api, err := newSharedAPI(m.cfg, m)
	if err != nil {
		t.Fatalf("newSharedAPI: %v", err)
	}
	defer api.iceMux.Close()

	// Cliente offre (com track de áudio — o offer só tem m-line com um transceiver).
	opus := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	audioTrack, err := webrtc.NewTrackLocalStaticRTP(opus, "papo", "test-audio")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP: %v", err)
	}
	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection(cliente): %v", err)
	}
	defer clientPC.Close()
	if _, err := clientPC.AddTrack(audioTrack); err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	serverPC, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection(servidor): %v", err)
	}
	defer serverPC.Close()

	connectPCs(t, clientPC, serverPC)
}
