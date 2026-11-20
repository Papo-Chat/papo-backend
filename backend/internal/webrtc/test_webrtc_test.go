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
	"github.com/pion/rtp/codecs"
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


func TestSFUIceServersExcludeTURN(t *testing.T) {
	m := testManager(t, &config.Config{
		STUNURLs: []string{"stun:stun.l.google.com:19302"},
		TURNURLs: []string{"turn:turn.example.com:3478?transport=udp"},
	})

	servers := m.iceServers()
	if len(servers) != 1 {
		t.Fatalf("iceServers retornou %d servidores, esperado 1", len(servers))
	}
	if len(servers[0].URLs) != 1 || servers[0].URLs[0] != "stun:stun.l.google.com:19302" {
		t.Fatalf("iceServers do SFU = %#v, esperado apenas STUN", servers)
	}
}

func TestVideoKeyframeDetection(t *testing.T) {
	vp8Track := &webrtc.TrackRemote{}
	// TrackRemote.Codec não é trivial de montar sem receiver; valida os
	// parsers auxiliares diretamente onde possível e o VP8 frame-tag abaixo
	// pela mesma regra usada em isVideoKeyframe.
	var vp8 codecs.VP8Packet
	payload, err := vp8.Unmarshal([]byte{0x10, 0x00})
	if err != nil {
		t.Fatalf("vp8 unmarshal: %v", err)
	}
	if vp8.S != 1 || vp8.PID != 0 || len(payload) == 0 || payload[0]&0x01 != 0 {
		t.Fatal("VP8 keyframe inicial não foi reconhecido")
	}
	_ = vp8Track

	if !isH264RandomAccessPacket([]byte{0x65, 0x01}) {
		t.Fatal("H264 IDR deveria ser random access")
	}
	if isH264RandomAccessPacket([]byte{0x41, 0x01}) {
		t.Fatal("H264 P-frame não deveria ser random access")
	}
	if !isH264RandomAccessPacket([]byte{0x7c, 0x85, 0x01}) {
		t.Fatal("H264 FU-A IDR start deveria ser random access")
	}
}

func TestSSNTranslator(t *testing.T) {
	tr := &ssnTranslator{}
	fwdA := &forwarder{}
	fwdB := &forwarder{}

	forward := func(fwd *forwarder, seq uint16) (uint16, forwardResult) {
		pkt := &rtp.Packet{Header: rtp.Header{SequenceNumber: seq}}
		result := tr.forward(pkt, fwd)
		return pkt.Header.SequenceNumber, result
	}

	// slot novo (A): mantém o SSN do publisher (offset 0)
	if _, continues := tr.begin(fwdA); continues {
		t.Fatal("slot novo não deveria continuar sequência anterior")
	}
	if got, _ := forward(fwdA, 100); got != 100 {
		t.Fatalf("primeiro pacote: SSN = %d, esperado 100", got)
	}
	if got, _ := forward(fwdA, 101); got != 101 {
		t.Fatalf("mesmo forwarder: SSN = %d, esperado 101", got)
	}

	// troca de ocupante (B): continua do último SSN do slot (101 → 102)
	firstSeq, continues := tr.begin(fwdB)
	if !continues || firstSeq != 102 {
		t.Fatalf("begin(B) = (%d, %v), esperado (102, true)", firstSeq, continues)
	}
	if got, _ := forward(fwdB, 50); got != 102 {
		t.Fatalf("troca de ocupante: SSN = %d, esperado 102", got)
	}
	if got, _ := forward(fwdB, 51); got != 103 {
		t.Fatalf("mesmo forwarder (B): SSN = %d, esperado 103", got)
	}

	// mesmo forwarder, nova fronteira RTP: continua do último SSN do slot
	tr.restart(fwdB)
	if got, _ := forward(fwdB, 500); got != 104 {
		t.Fatalf("restart: SSN = %d, esperado 104", got)
	}
}

// TestReplacedForwarderCannotWrite garante que o forwarder antigo de um slot
// não escreve mais depois da troca de ocupante. Antes, finish() só avisava a
// goroutine, e um pacote que ela já tinha em mãos era escrito depois do
// primeiro pacote do novo publisher, com SSN maior: o cliente via um pedaço
// do vídeo antigo dentro do novo, sem como separar.
func TestReplacedForwarderCannotWrite(t *testing.T) {
	tr := &ssnTranslator{}
	old := &forwarder{}
	next := &forwarder{}

	tr.begin(old)
	tr.forward(&rtp.Packet{Header: rtp.Header{SequenceNumber: 10}}, old)

	firstSeq, _ := tr.begin(next)

	late := &rtp.Packet{Header: rtp.Header{SequenceNumber: 11}}
	if tr.forward(late, old) != forwardStop {
		t.Fatal("forwarder substituído conseguiu escrever no slot")
	}
	tr.restart(old) // não pode recalcular o offset do forwarder novo

	pkt := &rtp.Packet{Header: rtp.Header{SequenceNumber: 9000}}
	if tr.forward(pkt, next) != forwardWrite || pkt.Header.SequenceNumber != firstSeq {
		t.Fatalf("primeiro pacote do novo ocupante: SSN = %d, esperado firstSeq %d",
			pkt.Header.SequenceNumber, firstSeq)
	}

	tr.end(next)
	if tr.forward(&rtp.Packet{}, next) != forwardStop {
		t.Fatal("slot liberado ainda aceitou pacote")
	}
}

// TestTranslatorDropsPacketsFromBeforeBoundary garante que um pacote que
// chega atrasado de antes da fronteira (o pion não reordena) é descartado em
// vez de receber um SSN anterior ao firstSeq. Sem isso, o pacote do frame
// anterior ao keyframe do novo ocupante saía com firstSeq-1, o mesmo SSN do
// último pacote do ocupante antigo.
func TestTranslatorDropsPacketsFromBeforeBoundary(t *testing.T) {
	tr := &ssnTranslator{}
	old := &forwarder{}
	next := &forwarder{}

	tr.begin(old)
	tr.forward(&rtp.Packet{Header: rtp.Header{SequenceNumber: 10}}, old)
	firstSeq, _ := tr.begin(next)

	keyframe := &rtp.Packet{Header: rtp.Header{SequenceNumber: 500}}
	if tr.forward(keyframe, next) != forwardWrite || keyframe.Header.SequenceNumber != firstSeq {
		t.Fatalf("keyframe: SSN = %d, esperado %d", keyframe.Header.SequenceNumber, firstSeq)
	}

	lateBeforeKeyframe := &rtp.Packet{Header: rtp.Header{SequenceNumber: 499}}
	if tr.forward(lateBeforeKeyframe, next) != forwardSkip {
		t.Fatalf("pacote de antes da fronteira foi escrito com SSN %d", lateBeforeKeyframe.Header.SequenceNumber)
	}

	after := &rtp.Packet{Header: rtp.Header{SequenceNumber: 501}}
	if tr.forward(after, next) != forwardWrite || after.Header.SequenceNumber != firstSeq+1 {
		t.Fatalf("pacote seguinte: SSN = %d, esperado %d", after.Header.SequenceNumber, firstSeq+1)
	}
}

// TestTranslatorFirstSeqFollowsHighestSSN garante que, com pacotes fora de
// ordem, o firstSeq do próximo ocupante continua depois do MAIOR SSN já
// escrito. Antes, lastSSN era o último escrito: um pacote atrasado o fazia
// voltar, e o próximo ocupante repetia SSNs já enviados pelo anterior.
func TestTranslatorFirstSeqFollowsHighestSSN(t *testing.T) {
	tr := &ssnTranslator{}
	old := &forwarder{}

	tr.begin(old)
	for _, seq := range []uint16{100, 102, 101} {
		tr.forward(&rtp.Packet{Header: rtp.Header{SequenceNumber: seq}}, old)
	}

	if firstSeq, _ := tr.begin(&forwarder{}); firstSeq != 103 {
		t.Fatalf("firstSeq = %d, esperado 103 (depois do maior SSN escrito)", firstSeq)
	}
}

func TestParseMidRoles(t *testing.T) {
	cameraSDP := `v=0
o=- 1 2 IN IP4 127.0.0.1
s=-
t=0 0
a=group:BUNDLE 0 1
m=audio 9 UDP/TLS/RTP/SAVPF 111
c=IN IP4 127.0.0.1
a=mid:0
a=sendrecv
a=rtpmap:111 opus/48000/2
m=video 9 UDP/TLS/RTP/SAVPF 96
c=IN IP4 127.0.0.1
a=mid:1
a=sendonly
a=rtpmap:96 vp8/90000`

	roles, active, err := parseMidRoles(
		cameraSDP,
		nil,
		true,  // cameraOn
		false, // screenOn
	)
	if err != nil {
		t.Fatalf("primeiro offer: %v", err)
	}

	if roles["0"] != roleAudio {
		t.Fatalf("mid 0 = %v, esperado audio", roles["0"])
	}
	if roles["1"] != roleCamera {
		t.Fatalf("mid 1 = %v, esperado camera", roles["1"])
	}
	if active["1"] != roleCamera {
		t.Fatalf("camera não ficou ativa")
	}

	// Depois o usuário inicia screen share.
	screenSDP := `v=0
o=- 1 3 IN IP4 127.0.0.1
s=-
t=0 0
a=group:BUNDLE 0 1 2
m=audio 9 UDP/TLS/RTP/SAVPF 111
c=IN IP4 127.0.0.1
a=mid:0
a=sendrecv
a=rtpmap:111 opus/48000/2
m=video 9 UDP/TLS/RTP/SAVPF 96
c=IN IP4 127.0.0.1
a=mid:1
a=sendonly
a=rtpmap:96 vp8/90000
m=video 9 UDP/TLS/RTP/SAVPF 96
c=IN IP4 127.0.0.1
a=mid:2
a=sendonly
a=rtpmap:96 vp8/90000`

	roles, active, err = parseMidRoles(
		screenSDP,
		roles, // identidade da câmera já conhecida
		true,  // cameraOn
		true,  // screenOn
	)
	if err != nil {
		t.Fatalf("offer com screen: %v", err)
	}

	if active["0"] != roleAudio {
		t.Fatalf("audio conhecido deixou de ficar ativo: %#v", active)
	}

	if roles["1"] != roleCamera {
		t.Fatalf("mid 1 perdeu roleCamera: %#v", roles)
	}
	if roles["2"] != roleScreen {
		t.Fatalf("mid 2 = %v, esperado screen", roles["2"])
	}
	if active["2"] != roleScreen {
		t.Fatalf("screen não ficou ativa")
	}
}

func TestParseMidRolesRejectsAmbiguousVideo(t *testing.T) {
	sdpStr := "v=0\n" +
		"o=- 1 2 IN IP4 127.0.0.1\n" +
		"s=-\n" +
		"t=0 0\n" +
		"m=video 9 UDP/TLS/RTP/SAVPF 96\n" +
		"c=IN IP4 127.0.0.1\n" +
		"a=mid:1\n" +
		"a=sendonly\n" +
		"a=rtpmap:96 VP8/90000\n"

	_, _, err := parseMidRoles(
		sdpStr,
		nil,
		true, // cameraOn
		true, // screenOn
	)

	if !errors.Is(err, ErrVoiceInvalidSDP) {
		t.Fatalf("esperado ErrVoiceInvalidSDP, recebido %v", err)
	}
}

func TestParseMidRolesScreenAsFirstVideo(t *testing.T) {
	sdpStr := `v=0
o=- 1 2 IN IP4 127.0.0.1
s=-
t=0 0
m=audio 9 UDP/TLS/RTP/SAVPF 111
c=IN IP4 127.0.0.1
a=mid:0
a=sendrecv
a=rtpmap:111 opus/48000/2
m=video 9 UDP/TLS/RTP/SAVPF 96
c=IN IP4 127.0.0.1
a=mid:3
a=sendonly
a=rtpmap:96 vp8/90000`

	roles, active, err := parseMidRoles(
		sdpStr,
		nil,
		false, // câmera desligada
		true,  // screen explicitamente ligado
	)
	if err != nil {
		t.Fatal(err)
	}

	if roles["3"] != roleScreen {
		t.Fatalf("mid 3 = %v, esperado screen", roles["3"])
	}

	if active["3"] != roleScreen {
		t.Fatalf("screen não ficou ativa")
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

func TestStableSelectLockedInitialSelection(t *testing.T) {
	m := testManager(t, &config.Config{VoiceAudioSlots: 2})

	r := &Room{
		m:                 m,
		channelID:         "ch1",
		peers:             make(map[string]*Peer),
		scores:            make(map[string]float64),
		lastAudioActivity: make(map[string]time.Time),
		tickerStop:        make(chan struct{}),
	}

	candidates := []scoredCandidate{
		{id: "a", score: -10},
		{id: "b", score: -20},
		{id: "c", score: -30},
		{id: "d", score: -20},
	}

	r.mu.Lock()
	selected := r.stableSelectLocked(
		candidates,
		nil,
		time.Now(),
		2,
	)
	r.mu.Unlock()

	if len(selected) != 2 ||
		selected[0] != "a" ||
		selected[1] != "b" {
		t.Fatalf("selected = %v, esperado [a b]", selected)
	}
}

func TestStableSelectLockedHysteresis(t *testing.T) {
	r := &Room{
		lastAudioActivity: make(map[string]time.Time),
	}

	now := time.Now()

	candidates := []scoredCandidate{
		{id: "a", score: -20},
		{id: "b", score: -40},
		{id: "c", score: -36}, // só 4 dB acima de b
	}

	current := []string{"a", "b"}

	r.lastAudioActivity["a"] = now
	r.lastAudioActivity["b"] = now
	r.lastAudioActivity["c"] = now

	selected := r.stableSelectLocked(
		candidates,
		current,
		now,
		2,
	)

	// c é mais alto que b, mas não supera a margem de 6 dB.
	if len(selected) != 2 ||
		selected[0] != "a" ||
		selected[1] != "b" {
		t.Fatalf("selected = %v, esperado [a b]", selected)
	}
}

func TestStableSelectLockedSwitchesAboveMargin(t *testing.T) {
	r := &Room{
		lastAudioActivity: make(map[string]time.Time),
	}

	now := time.Now()

	candidates := []scoredCandidate{
		{id: "a", score: -20},
		{id: "b", score: -40},
		{id: "c", score: -33}, // 7 dB acima de b
	}

	current := []string{"a", "b"}

	r.lastAudioActivity["a"] = now
	r.lastAudioActivity["b"] = now
	r.lastAudioActivity["c"] = now

	selected := r.stableSelectLocked(
		candidates,
		current,
		now,
		2,
	)

	if len(selected) != 2 ||
		selected[0] != "a" ||
		selected[1] != "c" {
		t.Fatalf("selected = %v, esperado [a c]", selected)
	}
}

func TestStableSelectLockedInactiveChallengerDoesNotReplace(t *testing.T) {
	r := &Room{
		lastAudioActivity: make(map[string]time.Time),
	}

	now := time.Now()

	candidates := []scoredCandidate{
		{id: "a", score: -20},
		{id: "b", score: -40},
		{id: "c", score: -10},
	}

	current := []string{"a", "b"}

	r.lastAudioActivity["a"] = now
	r.lastAudioActivity["b"] = now
	r.lastAudioActivity["c"] = now.Add(-time.Second)

	selected := r.stableSelectLocked(
		candidates,
		current,
		now,
		2,
	)

	if len(selected) != 2 ||
		selected[0] != "a" ||
		selected[1] != "b" {
		t.Fatalf("selected = %v, esperado [a b]", selected)
	}
}

func TestActiveSpeakerCandidatesDoesNotPromoteStaleNonIncumbent(t *testing.T) {
	now := time.Now()

	r := &Room{
		scores: map[string]float64{
			"a": -20,
			"c": -10,
		},
		lastTopK: []string{"a"},
		lastAudioActivity: map[string]time.Time{
			"a": now,
			"c": now.Add(-time.Second),
		},
	}

	views := map[string]audioPeerView{
		"a": {track: &webrtc.TrackRemote{}},
		"c": {track: &webrtc.TrackRemote{}},
	}

	got := r.activeSpeakerCandidatesLocked(views, now)

	if len(got) != 1 || got[0].id != "a" {
		t.Fatalf("candidates = %#v, esperado somente a", got)
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

func TestValidateSDPSizeBudget(t *testing.T) {
	cfg := testManager(t, nil).cfg

	buildPadded := func(target int) string {
		var sb strings.Builder
		sb.WriteString(sdpAudioOnly)
		for sb.Len() < target {
			sb.WriteString("\na=x-padding:0123456789abcdef0123456789abcdef")
		}
		return sb.String()
	}

	// Chrome offers with the configured receive slots can exceed the old
	// 64 KiB guard while still fitting comfortably inside the 128 KiB WS frame.
	withinBudget := buildPadded(80 * 1024)
	if len(withinBudget) <= 64*1024 || len(withinBudget) > maxSDPSize {
		t.Fatalf("fixture size = %d, expected between 64 KiB and maxSDPSize", len(withinBudget))
	}
	if err := validateSDP(withinBudget, cfg, false); err != nil {
		t.Fatalf("SDP de ~80 KiB deveria ser aceita, obtive %v", err)
	}

	overBudget := buildPadded(maxSDPSize + 1024)
	if err := validateSDP(overBudget, cfg, false); !errors.Is(err, ErrVoiceInvalidSDP) {
		t.Fatalf("SDP acima de maxSDPSize: esperado ErrVoiceInvalidSDP, obtive %v", err)
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
	if err := validateCandidate("candidate:3 1 UDP 2122260223 8f3c9a2d-1234-5678-abcd-0123456789ab.local 50000 typ host"); err != nil {
		t.Errorf("candidate mDNS .local: esperado nil, obtive %v", err)
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

func TestParseMidRolesIgnoresRecvonlySlots(t *testing.T) {
	sdpStr := `v=0
o=- 1 2 IN IP4 127.0.0.1
s=-
t=0 0
a=group:BUNDLE 0 1 2
m=audio 9 UDP/TLS/RTP/SAVPF 111
c=IN IP4 127.0.0.1
a=mid:0
a=sendrecv
a=rtpmap:111 opus/48000/2
m=video 9 UDP/TLS/RTP/SAVPF 96
c=IN IP4 127.0.0.1
a=mid:1
a=sendonly
a=rtpmap:96 vp8/90000
m=video 9 UDP/TLS/RTP/SAVPF 96
c=IN IP4 127.0.0.1
a=mid:2
a=recvonly
a=rtpmap:96 vp8/90000`

	roles, active, err := parseMidRoles(
		sdpStr,
		nil,
		true,  // cameraOn
		false, // screenOn
	)
	if err != nil {
		t.Fatal(err)
	}

	if roles["0"] != roleAudio {
		t.Fatalf("mid 0 = %v, esperado roleAudio", roles["0"])
	}

	if roles["1"] != roleCamera {
		t.Fatalf("mid 1 = %v, esperado roleCamera", roles["1"])
	}

	if _, ok := roles["2"]; ok {
		t.Fatalf("recvonly recebeu role: %#v", roles)
	}

	if active["0"] != roleAudio {
		t.Fatalf("audio não ficou ativo: %#v", active)
	}

	if active["1"] != roleCamera {
		t.Fatalf("camera não ficou ativa: %#v", active)
	}

	if _, ok := active["2"]; ok {
		t.Fatalf("recvonly ficou ativo: %#v", active)
	}

	if hasRole(roles, roleScreen) {
		t.Fatalf("slot recvonly foi confundido com screen: %#v", roles)
	}
}

func TestForwarderRunUnsubscribesOnExit(t *testing.T) {
	f := newFanout(&webrtc.TrackRemote{})
	fwd := newForwarder(&slot{}, nil, "audio", f)
	f.subscribe(fwd)
	close(fwd.ch)

	done := make(chan struct{})
	go func() {
		fwd.run()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("forwarder não encerrou")
	}

	f.mu.Lock()
	_, exists := f.subs[fwd]
	f.mu.Unlock()
	if exists {
		t.Fatal("forwarder encerrado permaneceu em fanout.subs")
	}
}

func TestPeerIsOwnedByJoiningClient(t *testing.T) {
	cfg := &config.Config{
		VoiceVideoCodec:         "vp8",
		VoiceVideoSlots:         2,
		VoiceAudioSlots:         2,
		VoiceMaxRoomPeers:       10,
		VoiceMaxRoomsPerUser:    1,
		VoiceSignalRateLimit:    100,
		VoiceSignalRateBurst:    100,
		VoiceSubscribeRateLimit: 100,
		VoiceSubscribeRateBurst: 100,
		VoiceRoomCleanupGrace:   time.Hour,
	}
	m := testManager(t, cfg)
	defer m.Shutdown()

	if err := m.Join("ch", "u", "owner"); err != nil {
		t.Fatalf("Join: %v", err)
	}
	if err := m.Leave("ch", "u", "other-tab"); !errors.Is(err, ErrVoiceNotFound) {
		t.Fatalf("outra aba conseguiu controlar peer: %v", err)
	}
	if !m.getRoom("ch").hasPeer("u") {
		t.Fatal("peer foi removido pela aba errada")
	}

	m.ClientOffline("u", "other-tab")
	if !m.getRoom("ch").hasPeer("u") {
		t.Fatal("disconnect da aba errada removeu peer")
	}

	m.ClientOffline("u", "owner")
	if m.getRoom("ch").hasPeer("u") {
		t.Fatal("disconnect da aba dona não removeu peer")
	}
	if got := m.userRoomCount("u"); got != 0 {
		t.Fatalf("userRooms stale: %d", got)
	}
}

func TestValidateSDPIgnoresRecvonlyCodec(t *testing.T) {
	cfg := &config.Config{VoiceVideoCodec: "vp8", VoiceVideoSlots: 6, VoiceAudioSlots: 4}
	sdpStr := "v=0\no=- 1 2 IN IP4 127.0.0.1\ns=-\nt=0 0\n" +
		"m=video 9 UDP/TLS/RTP/SAVPF 97\nc=IN IP4 127.0.0.1\na=mid:0\na=sendonly\na=rtpmap:97 h264/90000\n" +
		"m=video 9 UDP/TLS/RTP/SAVPF 96\nc=IN IP4 127.0.0.1\na=mid:1\na=recvonly\na=rtpmap:96 vp8/90000\n"
	if err := validateSDP(sdpStr, cfg, true); !errors.Is(err, ErrVoiceCodecUnsupported) {
		t.Fatalf("recvonly VP8 mascarou publisher incompatível: %v", err)
	}
}

func TestIncomingAudioStartsFanoutBeforeTopK(t *testing.T) {
	pubClient, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pubClient.Close()
	pubServer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pubServer.Close()

	trackCh := make(chan *webrtc.TrackRemote, 1)
	pubServer.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		trackCh <- track
	})

	opus := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	local, err := webrtc.NewTrackLocalStaticRTP(opus, "papo", "bootstrap-audio")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pubClient.AddTrack(local); err != nil {
		t.Fatal(err)
	}
	connectPCs(t, pubClient, pubServer)

	go func() {
		_ = local.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 1}, Payload: []byte{1}})
	}()

	select {
	case remote := <-trackCh:
		m := testManager(t, nil)

		r := &Room{
			m:          m,
			channelID:  "ch",
			peers:      make(map[string]*Peer),
			scores:     make(map[string]float64),
			tickerStop: make(chan struct{}),
		}

		p := newPeer(m, r, "pub", "client-1")
		defer p.close()

		// Simula o estado após um offer válido contendo áudio ativo.
		p.mu.Lock()
		p.midRole["0"] = roleAudio
		p.activeMidRole["0"] = roleAudio
		p.mu.Unlock()

		p.onIncomingTrack(remote, "0")

		p.mu.Lock()
		f := p.fanouts["audio"]
		p.mu.Unlock()

		if f == nil || f.track != remote {
			t.Fatal("track de áudio não iniciou fanout/reader antes do active-speaker")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnTrack não disparou")
	}
}

func TestSameAudioSetRebindsChangedTrack(t *testing.T) {
	m := testManager(t, nil)
	r := &Room{m: m, channelID: "ch", peers: make(map[string]*Peer), scores: make(map[string]float64), tickerStop: make(chan struct{})}
	pub := newPeer(m, r, "pub", "pub-client")
	sub := newPeer(m, r, "sub", "sub-client")
	defer sub.close()
	defer pub.close()

	track1 := &webrtc.TrackRemote{}
	track2 := &webrtc.TrackRemote{}
	f1 := newFanout(track1)
	f2 := newFanout(track2)

	pub.mu.Lock()
	pub.fanouts["audio"] = f1
	pub.mu.Unlock()

	s := &slot{peer: sub}
	sub.audioSlots = []*slot{s}
	sub.setAudioSet([]string{"pub"}, []*Peer{pub}, []*webrtc.TrackRemote{track1})
	if s.src != track1 {
		t.Fatal("primeira track não foi vinculada")
	}

	pub.mu.Lock()
	pub.fanouts["audio"] = f2
	pub.mu.Unlock()
	f1.destroy()

	// userID/top-K é idêntico; apenas TrackRemote mudou.
	sub.setAudioSet([]string{"pub"}, []*Peer{pub}, []*webrtc.TrackRemote{track2})
	if s.src != track2 {
		t.Fatal("top-K igual impediu rebind da nova TrackRemote")
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
		fwd1 := newForwarder(&slot{}, nil, "audio", fanout)
		fwd2 := newForwarder(&slot{}, nil, "audio", fanout)
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

	peer := newPeer(m, room, "user-1", "conn-1")
	defer peer.close()
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


func TestReleaseAllFromPublishesAudioRoutes(t *testing.T) {
	events := make(chan any, 1)
	m := testManager(t, &config.Config{VoiceAudioSlots: 1})
	m.signaler.SendToClient = func(clientID string, event any) {
		if clientID != "subscriber-client" {
			t.Fatalf("clientID = %q, esperado subscriber-client", clientID)
		}
		events <- event
	}

	room := &Room{m: m, channelID: "voice-channel"}
	pub := &Peer{userID: "publisher-user"}
	sub := &Peer{
		m:               m,
		room:            room,
		userID:          "subscriber-user",
		signalingClient: "subscriber-client",
	}

	opus := webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	}
	local, err := webrtc.NewTrackLocalStaticRTP(opus, "papo-audio-0", "papo-audio")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP: %v", err)
	}

	sub.audioSlots = []*slot{{
		peer:  sub,
		local: local,
		owner: pub,
		kind:  "audio",
	}}

	routes := sub.audioRoutesLocked()
	if len(routes) != 1 || routes[0].TrackID != "papo-audio-0" || routes[0].UserID != "publisher-user" {
		t.Fatalf("routes iniciais = %#v", routes)
	}

	sub.releaseAllFrom(pub)

	select {
	case raw := <-events:
		event, ok := raw.(VoiceAudioRoutes)
		if !ok {
			t.Fatalf("evento = %T, esperado VoiceAudioRoutes", raw)
		}
		if event.Type != EventTypeVoiceAudioRoutes || event.ChannelID != "voice-channel" {
			t.Fatalf("evento inválido: %#v", event)
		}
		if len(event.Routes) != 0 {
			t.Fatalf("routes após release = %#v, esperado vazio", event.Routes)
		}
	case <-time.After(time.Second):
		t.Fatal("voice_audio_routes não foi enviado")
	}
}


func TestSetMutedEvictsActiveSpeakerImmediately(t *testing.T) {
	events := make(chan any, 4)
	m := testManager(t, &config.Config{VoiceAudioSlots: 2})
	m.signaler.BroadcastToUsers = func(_ map[string]bool, event any) {
		events <- event
	}

	peer := &Peer{userID: "speaker"}
	r := &Room{
		m:                 m,
		channelID:         "voice-channel",
		peers:             map[string]*Peer{"speaker": peer},
		scores:            map[string]float64{"speaker": -10, "other": -20},
		lastTopK:          []string{"speaker", "other"},
		lastAudioActivity: map[string]time.Time{"speaker": time.Now(), "other": time.Now()},
	}

	r.setMuted("speaker", true)

	if _, ok := r.scores["speaker"]; ok {
		t.Fatal("score do usuário mutado não foi removido")
	}
	if _, ok := r.lastAudioActivity["speaker"]; ok {
		t.Fatal("atividade do usuário mutado não foi removida")
	}
	if len(r.lastTopK) != 1 || r.lastTopK[0] != "other" {
		t.Fatalf("lastTopK = %v, esperado [other]", r.lastTopK)
	}

	var (
		gotState  bool
		gotActive bool
	)
	for i := 0; i < 2; i++ {
		select {
		case raw := <-events:
			switch event := raw.(type) {
			case VoiceStateUpdate:
				gotState = event.UserID == "speaker" && event.Muted
			case ActiveSpeakerUpdate:
				gotActive = len(event.UserIDs) == 1 && event.UserIDs[0] == "other"
			}
		case <-time.After(time.Second):
			t.Fatal("eventos de mute/active-speaker não foram enviados")
		}
	}

	if !gotState {
		t.Fatal("voice_state_update de mute não foi enviado")
	}
	if !gotActive {
		t.Fatal("active_speaker_update sem o usuário mutado não foi enviado")
	}
}

// --- peer_id e envios depois do close ---

// TestPeerIDIdentifiesEachJoin garante que cada voice_join aceito cria um peer
// com um peer_id novo, devolvido no voice_joined. Sair e entrar de novo pela
// mesma conexão precisa gerar outro id; é ele que permite ao cliente
// descartar eventos que ainda chegam da call anterior.
func TestPeerIDIdentifiesEachJoin(t *testing.T) {
	cfg := &config.Config{
		VoiceVideoCodec:         "vp8",
		VoiceVideoSlots:         2,
		VoiceAudioSlots:         2,
		VoiceMaxRoomPeers:       10,
		VoiceMaxRoomsPerUser:    1,
		VoiceSignalRateLimit:    100,
		VoiceSignalRateBurst:    100,
		VoiceSubscribeRateLimit: 100,
		VoiceSubscribeRateBurst: 100,
		VoiceRoomCleanupGrace:   time.Hour,
	}
	joined := make(chan VoiceJoined, 2)
	m := testManager(t, cfg)
	m.signaler.SendToClient = func(clientID string, event any) {
		if event, ok := event.(VoiceJoined); ok {
			joined <- event
		}
	}
	defer m.Shutdown()

	if err := m.Join("ch", "u", "conn"); err != nil {
		t.Fatalf("primeiro Join: %v", err)
	}
	first := <-joined
	if first.PeerID == "" {
		t.Fatal("voice_joined sem peer_id")
	}
	if got := m.getRoom("ch").peer("u").id; got != first.PeerID {
		t.Fatalf("peer_id do voice_joined = %q, esperado o do peer (%q)", first.PeerID, got)
	}

	if err := m.Leave("ch", "u", "conn"); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	if err := m.Join("ch", "u", "conn"); err != nil {
		t.Fatalf("segundo Join: %v", err)
	}
	second := <-joined
	if second.PeerID == "" || second.PeerID == first.PeerID {
		t.Fatalf("rejoin reutilizou o peer_id: %q e %q", first.PeerID, second.PeerID)
	}
}

// TestAsyncErrorCarriesChannelAndPeer garante que um erro do worker de
// renegociação leva o canal e o peer_id. Ele chega fora de ordem em relação
// aos eventos do cliente, então sem esses campos o cliente não sabe a que
// call o erro pertence.
func TestAsyncErrorCarriesChannelAndPeer(t *testing.T) {
	events := make(chan any, 1)
	m := testManager(t, nil)
	m.signaler.SendToClient = func(clientID string, event any) { events <- event }

	room := &Room{m: m, channelID: "voice-channel"}
	peer := newPeer(m, room, "user", "conn")
	defer peer.close()

	if err := peer.enqueue(func() error { return ErrVoiceInvalidSDP }); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	select {
	case raw := <-events:
		event, ok := raw.(VoiceError)
		if !ok {
			t.Fatalf("evento = %T, esperado VoiceError", raw)
		}
		if event.ChannelID != "voice-channel" || event.PeerID != peer.id {
			t.Fatalf("erro sem identificação da call: %#v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("erro do worker não foi enviado")
	}
}

// TestClosedPeerSendsNothing garante que um peer fechado não envia mais nada
// à conexão. Antes, releaseAllFrom (outro membro saindo) mandava
// voice_audio_routes mesmo para um subscriber que já tinha saído; se o
// cliente já tivesse entrado de novo pela mesma conexão, esse snapshot antigo
// era aplicado na call nova.
func TestClosedPeerSendsNothing(t *testing.T) {
	events := make(chan any, 4)
	m := testManager(t, &config.Config{VoiceAudioSlots: 1})
	m.signaler.SendToClient = func(clientID string, event any) { events <- event }

	room := &Room{m: m, channelID: "voice-channel"}
	pub := &Peer{userID: "publisher-user"}
	sub := newPeer(m, room, "subscriber-user", "subscriber-client")
	sub.audioSlots = []*slot{{peer: sub, local: mustAudioTrack(t, "papo-audio-0"), owner: pub, kind: "audio"}}

	sub.close()

	sub.releaseAllFrom(pub)
	sub.sendError(ErrVoiceInvalidSDP)
	sub.sendToOwner(VoiceAnswer{Type: EventTypeVoiceAnswer})

	select {
	case event := <-events:
		t.Fatalf("peer fechado enviou %#v", event)
	case <-time.After(100 * time.Millisecond):
	}
}

func mustAudioTrack(t *testing.T, id string) *webrtc.TrackLocalStaticRTP {
	t.Helper()
	opus := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	track, err := webrtc.NewTrackLocalStaticRTP(opus, id, "papo-audio")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP: %v", err)
	}
	return track
}

// --- voice_video_routes ---

// TestVideoRoutesFollowSlotAssignments garante que cada mudança de slot de
// vídeo gera um voice_video_routes com o publisher real de cada slot. Antes,
// o cliente adivinhava o slot (o primeiro livre) e, quando um slot era
// reutilizado, não tinha como saber de quem era o vídeo nem onde começava o
// novo ocupante. O snapshot traz o first_seq da troca e revisions crescentes.
func TestVideoRoutesFollowSlotAssignments(t *testing.T) {
	events := make(chan any, 8)
	m := testManager(t, nil)
	m.signaler.SendToClient = func(clientID string, event any) { events <- event }

	room := &Room{m: m, channelID: "voice-channel", peers: make(map[string]*Peer)}
	sub := newPeer(m, room, "sub", "sub-client")
	defer sub.close()
	sub.videoSlots = []*slot{
		{peer: sub, local: mustVideoTrack(t, "papo-video-0")},
		{peer: sub, local: mustVideoTrack(t, "papo-video-1")},
	}

	pubA := cameraPublisher(t, m, room, "user-a")
	pubC := cameraPublisher(t, m, room, "user-c")
	defer pubA.close()
	defer pubC.close()

	next := func() VoiceVideoRoutes {
		t.Helper()
		for {
			select {
			case raw := <-events:
				if event, ok := raw.(VoiceVideoRoutes); ok {
					return event
				}
			case <-time.After(time.Second):
				t.Fatal("voice_video_routes não foi enviado")
			}
		}
	}

	if err := sub.assignVideoSlot(pubA, "video"); err != nil {
		t.Fatalf("assign A: %v", err)
	}
	first := next()
	if first.PeerID != sub.id || len(first.Routes) != 1 {
		t.Fatalf("snapshot após A = %#v", first)
	}
	if r := first.Routes[0]; r.TrackID != "papo-video-0" || r.UserID != "user-a" || r.Kind != "video" || r.FirstSeq != nil {
		t.Fatalf("rota de A = %#v (slot novo não tem first_seq)", r)
	}

	// A envia alguns pacotes pelo slot 0 e depois sai; C ocupa o mesmo slot.
	slot0 := sub.videoSlots[0]
	for seq := uint16(40); seq <= 42; seq++ {
		slot0.translator.forward(&rtp.Packet{Header: rtp.Header{SequenceNumber: seq}}, slot0.fwd)
	}
	sub.releaseAllFrom(pubA)
	if released := next(); len(released.Routes) != 0 || released.Revision <= first.Revision {
		t.Fatalf("snapshot após saída de A = %#v", released)
	}

	if err := sub.assignVideoSlot(pubC, "video"); err != nil {
		t.Fatalf("assign C: %v", err)
	}
	reused := next()
	if len(reused.Routes) != 1 {
		t.Fatalf("snapshot após C = %#v", reused)
	}
	r := reused.Routes[0]
	if r.TrackID != "papo-video-0" || r.UserID != "user-c" {
		t.Fatalf("slot reutilizado = %#v, esperado papo-video-0 com user-c", r)
	}
	if r.FirstSeq == nil || *r.FirstSeq != 43 {
		t.Fatalf("first_seq do slot reutilizado = %v, esperado 43", r.FirstSeq)
	}
}

// cameraPublisher monta um peer publicando câmera (track remota vazia, sem
// PeerConnection), o suficiente para assignVideoSlot.
func cameraPublisher(t *testing.T, m *Manager, room *Room, userID string) *Peer {
	t.Helper()
	track := &webrtc.TrackRemote{}

	pub := newPeer(m, room, userID, userID+"-client")
	pub.mu.Lock()
	pub.videoTrack = track
	pub.videoMID = "1"
	pub.activeMidRole["1"] = roleCamera
	pub.fanouts["video"] = startedFanout(track)
	pub.mu.Unlock()
	return pub
}

// startedFanout cria um fanout já marcado como iniciado, sem readLoop. Os
// testes usam TrackRemote vazia, sem receiver; o readLoop real chamaria
// ReadRTP nela e derrubaria o processo de teste (nil pointer no pion) sempre
// que a goroutine rodasse antes do fim do teste. Os testes que usam isto só
// precisam das trocas de slot, não dos pacotes do publisher.
func startedFanout(track *webrtc.TrackRemote) *fanout {
	f := newFanout(track)
	f.startOnce.Do(func() {})
	return f
}

func mustVideoTrack(t *testing.T, id string) *webrtc.TrackLocalStaticRTP {
	t.Helper()
	vp8 := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}
	track, err := webrtc.NewTrackLocalStaticRTP(vp8, id, "papo-video")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP: %v", err)
	}
	return track
}
