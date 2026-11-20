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
	EventTypeVoiceAudioRoutes    = "voice_audio_routes"
	EventTypeVoiceVideoRoutes    = "voice_video_routes"
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

// VoiceError é o evento de erro de voz enviado ao cliente. O shape é o do
// ErrorOutbound do websocket (type/message/code), com dois campos opcionais
// para o cliente saber a que call o erro pertence:
//
//   - ChannelID: o canal da sala (erros assíncronos e voice-room-closed);
//   - PeerID: o peer que originou o erro (falhas do worker de renegociação).
//
// Os dois são omitidos nos erros que respondem diretamente a um evento do
// cliente, porque esses chegam em ordem, logo depois do evento que os causou.
type VoiceError struct {
	Type      string `json:"type"`
	Message   string `json:"message"`
	Code      string `json:"code,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	PeerID    string `json:"peer_id,omitempty"`
}

// VoiceJoined é o estado inicial enviado em unicast ao late joiner
// (membros da sala + active speakers atuais).
//
// PeerID identifica este peer (esta entrada na call). Todo evento que só
// interessa a ele (answer, ICE, rotas de áudio e vídeo, erros do worker)
// carrega o mesmo PeerID, para o cliente descartar o que ainda chegar de uma
// call anterior pela mesma conexão.
//
// VideoCodec é o codec de vídeo da sala (VOICE_VIDEO_CODEC: vp8, vp9 ou
// h264). O SFU não transcodifica: todo publisher envia e todo subscriber
// decodifica esse codec. Sabendo dele antes do primeiro voice_offer, o
// cliente prepara o encoder e o decoder certos (inclusive por hardware) em
// vez de descobrir pelo answer ou por um voice-codec-unsupported.
type VoiceJoined struct {
	Type           string              `json:"type"`
	ChannelID      string              `json:"channel_id"`
	PeerID         string              `json:"peer_id"`
	VideoCodec     string              `json:"video_codec"`
	Members        []models.VoiceState `json:"members"`
	ActiveSpeakers []string            `json:"active_speakers"`
}

// VoiceAnswer é a resposta SDP a um voice_offer do cliente (unicast).
type VoiceAnswer struct {
	Type      string `json:"type"`
	ChannelID string `json:"channel_id"`
	PeerID    string `json:"peer_id"`
	SDP       string `json:"sdp"`
}

// VoiceOffer é a oferta SDP de uma renegociação iniciada pelo servidor
// (ex.: novo slot de vídeo sem slot livre), respondida pelo cliente com
// voice_answer.
//
// O servidor não inicia renegociação hoje; o evento e o PeerID ficam
// reservados para quando iniciar.
type VoiceOffer struct {
	Type      string `json:"type"`
	ChannelID string `json:"channel_id"`
	PeerID    string `json:"peer_id"`
	SDP       string `json:"sdp"`
}

// VoiceICECandidate é um candidate trickle de ICE (unicast). SDPMid e
// SDPMLineIndex apontam para a m-line do candidate (null quando o browser
// não informa).
type VoiceICECandidate struct {
	Type          string  `json:"type"`
	ChannelID     string  `json:"channel_id"`
	PeerID        string  `json:"peer_id"`
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
//
// PeerID é o peer que saiu. A remoção pode acontecer fora da ordem do socket
// (a PeerConnection falhou e o pion remove o peer na goroutine dele): o
// usuário fica livre para entrar de novo antes de o voice_leave ser enviado,
// e o voice_leave do peer antigo chega depois do voice_joined do novo. Pelo
// PeerID o cliente sabe que a saída não é da call atual.
type VoiceLeave struct {
	Type      string `json:"type"`
	ChannelID string `json:"channel_id"`
	UserID    string `json:"user_id"`
	PeerID    string `json:"peer_id"`
}

// ActiveSpeakerUpdate é o top-K de active speakers da sala, distribuído aos
// leitores do canal de voz (apenas para a UI destacar quem está falando).
type ActiveSpeakerUpdate struct {
	Type      string   `json:"type"`
	ChannelID string   `json:"channel_id"`
	UserIDs   []string `json:"user_ids"`
}

// VoiceAudioRoute associa a track de um slot de áudio do SFU ao publisher
// atualmente encaminhado naquele slot. TrackID é o ID da TrackLocalStaticRTP
// ("papo-audio-N"), exposto no browser como RTCTrackEvent.track.id.
type VoiceAudioRoute struct {
	TrackID string `json:"track_id"`
	UserID  string `json:"user_id"`
}

// VoiceAudioRoutes é um snapshot completo, específico do subscriber, das
// rotas de áudio atualmente recebidas por sua PeerConnection. É enviado em
// unicast apenas para a conexão dona do signaling sempre que a associação de
// slots muda. Um snapshot vazio remove todas as associações anteriores.
//
// Revision cresce a cada snapshot do peer (um contador para áudio e vídeo).
// Os snapshots são montados sob o lock do peer mas enviados depois de
// soltá-lo, então dois deles podem chegar fora de ordem: o cliente ignora um
// voice_audio_routes com revision menor que a do último voice_audio_routes
// que aplicou, e o mesmo para voice_video_routes. A comparação é por tipo de
// evento: um voice_video_routes com revision menor que a do último
// voice_audio_routes é válido.
type VoiceAudioRoutes struct {
	Type      string            `json:"type"`
	ChannelID string            `json:"channel_id"`
	PeerID    string            `json:"peer_id"`
	Revision  uint64            `json:"revision"`
	Routes    []VoiceAudioRoute `json:"routes"`
}

// VoiceVideoRoute associa um slot de vídeo do SFU ao publisher e ao tipo de
// mídia (câmera ou tela) que ele encaminha agora. TrackID é o ID da
// TrackLocalStaticRTP ("papo-video-N"), o mesmo msid que o cliente vê.
//
// FirstSeq é o SSN do primeiro pacote deste ocupante no slot. Os slots são
// reutilizados entre publishers com a sequência contínua, então pacotes do
// ocupante anterior ainda podem chegar (rede, jitter buffer) depois da troca.
// Todos eles têm SSN anterior a FirstSeq, e todo pacote do ocupante atual tem
// SSN igual ou posterior. A comparação é circular (16 bits) e só vale perto
// da troca: o cliente aplica o filtro aos pacotes que recebe até ver o
// primeiro com SSN >= FirstSeq, ou compara sequence numbers estendidos (como
// o jitter buffer faz), nunca como regra permanente. Ausente quando o slot
// nunca carregou outro ocupante.
type VoiceVideoRoute struct {
	TrackID  string  `json:"track_id"`
	UserID   string  `json:"user_id"`
	Kind     string  `json:"kind"` // "video" (câmera) | "screen"
	FirstSeq *uint16 `json:"first_seq,omitempty"`
}

// VoiceVideoRoutes é o snapshot completo das rotas de vídeo recebidas pela
// PeerConnection do subscriber, no mesmo formato de VoiceAudioRoutes. Antes
// dele o cliente precisava adivinhar em que slot o SFU tinha colocado cada
// track_subscribe (o primeiro livre), e errava quando um subscribe falhava ou
// um slot era liberado sem ele saber. Um snapshot vazio remove todas as rotas.
type VoiceVideoRoutes struct {
	Type      string            `json:"type"`
	ChannelID string            `json:"channel_id"`
	PeerID    string            `json:"peer_id"`
	Revision  uint64            `json:"revision"`
	Routes    []VoiceVideoRoute `json:"routes"`
}
