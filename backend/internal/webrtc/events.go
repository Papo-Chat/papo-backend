package webrtc

import "papo/internal/models"

// Tipos de eventos outbound de voz (contrato da seção 7 do plano). Os
// valores são idênticos aos EventType correspondentes do pacote websocket;
// ficam neste pacote para evitar ciclo de import (websocket importa webrtc
// para despachar os eventos inbound; webrtc não importa websocket).
const (
	EventTypeVoiceJoined         = "voice_joined"
	EventTypeVoiceAnswer         = "voice_answer"
	EventTypeVoiceICECandidate   = "voice_ice_candidate"
	EventTypeVoiceStateUpdate    = "voice_state_update"
	EventTypeVoiceLeave          = "voice_leave"
	EventTypeActiveSpeakerUpdate = "active_speaker_update"
)

// Códigos de erro dos eventos de voz (evento `error` com `code`).
const (
	CodeVoiceNotFound         = "voice-not-found"
	CodeVoiceForbidden        = "voice-forbidden"
	CodeVoiceRoomFull         = "voice-room-full"
	CodeVoiceAlreadyInRoom    = "voice-already-in-room"
	CodeVoiceCodecUnsupported = "voice-codec-unsupported"
	CodeVoiceInvalidSDP       = "voice-invalid-sdp"
	CodeVoiceRateLimited      = "voice-rate-limited"
	CodeVoiceRoomClosed       = "voice-room-closed"
)

// VoiceError é o evento de erro de voz enviado ao cliente. O shape é
// idêntico ao ErrorOutbound do websocket (type/message/code).
type VoiceError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

// VoiceJoined é o estado inicial enviado em unicast ao late joiner
// (membros da sala + active speakers atuais).
type VoiceJoined struct {
	Type           string              `json:"type"`
	ChannelID      string              `json:"channel_id"`
	Members        []models.VoiceState `json:"members"`
	ActiveSpeakers []string            `json:"active_speakers"`
}

// VoiceAnswer é a resposta SDP a um voice_offer do cliente (unicast).
type VoiceAnswer struct {
	Type      string `json:"type"`
	ChannelID string `json:"channel_id"`
	SDP       string `json:"sdp"`
}

// VoiceOffer é a oferta SDP de uma renegociação iniciada pelo servidor
// (ex.: novo slot de vídeo sem slot livre), respondida pelo cliente com
// voice_answer.
type VoiceOffer struct {
	Type      string `json:"type"`
	ChannelID string `json:"channel_id"`
	SDP       string `json:"sdp"`
}

// VoiceICECandidate é um candidate trickle de ICE (unicast). SDPMid e
// SDPMLineIndex apontam para a m-line do candidate (null quando o browser
// não informa).
type VoiceICECandidate struct {
	Type          string  `json:"type"`
	ChannelID     string  `json:"channel_id"`
	Candidate     string  `json:"candidate"`
	SDPMid        *string `json:"sdp_mid,omitempty"`
	SDPMLineIndex *int    `json:"sdp_mline_index,omitempty"`
}

// VoiceStateUpdate é a mudança de estado de um usuário na call (mic/câmera/
// screen share), distribuída aos leitores do canal de voz (inclui quem está
// fora da call).
type VoiceStateUpdate struct {
	Type          string `json:"type"`
	ChannelID     string `json:"channel_id"`
	UserID        string `json:"user_id"`
	Muted         bool   `json:"muted"`
	CameraOn      bool   `json:"camera_on"`
	ScreenSharing bool   `json:"screen_sharing"`
}

// VoiceLeave é a saída de um usuário da call (explícita, WS caiu, sessão
// revogada ou sala destruída), distribuída aos leitores do canal de voz.
type VoiceLeave struct {
	Type      string `json:"type"`
	ChannelID string `json:"channel_id"`
	UserID    string `json:"user_id"`
}

// ActiveSpeakerUpdate é o top-K de active speakers da sala, distribuído aos
// leitores do canal de voz (apenas para a UI destacar quem está falando).
type ActiveSpeakerUpdate struct {
	Type      string   `json:"type"`
	ChannelID string   `json:"channel_id"`
	UserIDs   []string `json:"user_ids"`
}
