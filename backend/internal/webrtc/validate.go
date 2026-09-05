package webrtc

import (
	"net"
	"strings"

	"papo/internal/config"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

const (
	// maxSDPSize é o teto de tamanho de uma SDP no WS (64 KB; o teto físico
	// do WS é 128 KB — websocket/client.go).
	maxSDPSize = 64 * 1024
	// maxCandidateSize é o teto de tamanho de um candidate trickle (~1 KB).
	maxCandidateSize = 1024
)

// validateSDP valida a SDP de um evento voice_offer/voice_answer (seção 5.9):
// tamanho, parse, teto de m-lines e — apenas em offers — a presença do codec
// da sala na m-line de vídeo (D6: sem interseção → voice-codec-unsupported).
func validateSDP(sdpStr string, cfg *config.Config, checkCodec bool) error {
	if len(sdpStr) == 0 || len(sdpStr) > maxSDPSize {
		return ErrVoiceInvalidSDP
	}

	var desc sdp.SessionDescription
	if err := desc.UnmarshalString(normalizeSDP(sdpStr)); err != nil {
		return ErrVoiceInvalidSDP
	}

	// Teto de m-lines: as m-lines do peer (áudio + vídeo + screen, no máx. 3)
	// + os slots pré-alocados (N vídeo + K áudio). Mais que isso é injeção de
	// m-lines extras.
	maxMlines := 3 + cfg.VoiceVideoSlots + cfg.VoiceAudioSlots
	if len(desc.MediaDescriptions) > maxMlines {
		return ErrVoiceInvalidSDP
	}

	if checkCodec {
		return checkVideoCodec(&desc, cfg.VoiceVideoCodec)
	}
	return nil
}

func mediaPublishes(media *sdp.MediaDescription) bool {
	for _, a := range media.Attributes {
		switch a.Key {
		case "sendonly", "sendrecv":
			return true
		case "recvonly", "inactive":
			return false
		}
	}
	// RFC: ausência de direction equivale a sendrecv.
	return true
}

// checkVideoCodec verifica se a m-line de vídeo do offer contém o codec da
// sala (D6). O codec é identificado pelo encoding-name do rtpmap (ex.: "vp8"),
// não pelo payload type. Sem m-line de vídeo (peer só com áudio) é válido.
func checkVideoCodec(desc *sdp.SessionDescription, roomCodec string) error {
	hasVideo := false
	for _, media := range desc.MediaDescriptions {
		if media.MediaName.Media != "video" || !mediaPublishes(media) {
			continue
		}
		hasVideo = true
		if videoMLineHasCodec(media, roomCodec) {
			return nil
		}
	}
	if hasVideo {
		return ErrVoiceCodecUnsupported
	}
	return nil
}

// videoMLineHasCodec verifica se a m-line tem um rtpmap com o codec (o
// encoding-name, ex.: "96 vp8/90000" → "vp8").
func videoMLineHasCodec(media *sdp.MediaDescription, codec string) bool {
	for _, a := range media.Attributes {
		if a.Key != "rtpmap" {
			continue
		}
		parts := strings.Fields(a.Value)
		if len(parts) < 2 {
			continue
		}
		encoding := strings.SplitN(parts[1], "/", 2)[0]
		if strings.EqualFold(encoding, codec) {
			return true
		}
	}
	return false
}

func validICEHostname(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

// validateCandidate valida um candidate trickle de ICE (seção 5.9): tamanho,
// formato e rejeição de addresses loopback/unspecified. IPs privados são
// permitidos (D15 — self-hosted/LAN é o caso principal).
func validateCandidate(raw string) error {
	if raw == "" || len(raw) > maxCandidateSize {
		return ErrVoiceInvalidSDP
	}

	// Formato do candidate do browser (sem o prefixo "candidate:") — RFC 5245:
	// foundation component protocol priority connection-address port typ [rest]
	// → o IP é o campo 4 (0-based); o campo 5 é a porta.
	value := strings.TrimPrefix(raw, "candidate:")
	fields := strings.Fields(value)
	if len(fields) < 7 {
		return ErrVoiceInvalidSDP
	}

	addr := fields[4]
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		host := strings.TrimSuffix(strings.ToLower(addr), ".")
		if !strings.HasSuffix(host, ".local") || !validICEHostname(host) {
			return ErrVoiceInvalidSDP
		}
		return nil
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return ErrVoiceInvalidSDP
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return ErrVoiceInvalidSDP
	}

	return nil
}

// toICECandidateInit converte o candidate do browser para o
// ICECandidateInit do pion (o pion faz o parse do campo candidate).
func toICECandidateInit(raw, sdpMid string, sdpMLineIndex int) webrtc.ICECandidateInit {
	init := webrtc.ICECandidateInit{Candidate: raw}
	if sdpMid != "" {
		init.SDPMid = &sdpMid
	}
	idx := uint16(sdpMLineIndex)
	init.SDPMLineIndex = &idx
	return init
}

// normalizeSDP garante o newline final que o lexer do pion exige (tolera
// SDPs sem o \n final — o browser costuma enviar, mas não é garantido).
func normalizeSDP(sdpStr string) string {
	if !strings.HasSuffix(sdpStr, "\n") {
		sdpStr += "\n"
	}
	return sdpStr
}

// parseMidRoles mapeia o mid de cada m-line do offer para o papel da track
// (audio / primeira vídeo = câmera / segunda vídeo = screen share). Seção 5.6:
// determinístico, sem depender de label.
func parseMidRoles(sdpStr string) map[string]trackRole {
	roles := make(map[string]trackRole)
	var desc sdp.SessionDescription
	if err := desc.UnmarshalString(normalizeSDP(sdpStr)); err != nil {
		return roles
	}
	videoCount := 0
	for _, media := range desc.MediaDescriptions {
		if !mediaPublishes(media) {
			continue
		}
		mid := ""
		for _, a := range media.Attributes {
			if a.Key == "mid" {
				mid = a.Value
				break
			}
		}
		if mid == "" {
			continue
		}
		switch media.MediaName.Media {
		case "audio":
			roles[mid] = roleAudio
		case "video":
			if videoCount == 0 {
				roles[mid] = roleCamera
			} else {
				roles[mid] = roleScreen
			}
			videoCount++
		}
	}
	return roles
}
