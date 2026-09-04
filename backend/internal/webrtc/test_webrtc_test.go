package webrtc

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"papo/internal/config"

	"github.com/pion/rtp"
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
	if got := audioLevelToDBFS(0); got != -120 {
		t.Errorf("level=0: dBFS = %f, esperado -120", got)
	}
	if got := audioLevelToDBFS(127); math.Abs(got) > 1e-9 {
		t.Errorf("level=127: dBFS = %f, esperado 0", got)
	}
	// level=1 → 20*log10(1/127) ≈ -42.08
	want := 20 * math.Log10(1.0/127.0)
	if got := audioLevelToDBFS(1); math.Abs(got-want) > 1e-9 {
		t.Errorf("level=1: dBFS = %f, esperado %f", got, want)
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
