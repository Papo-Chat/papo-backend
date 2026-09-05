package websocket

import (
	"time"

	"papo/internal/models"
)

// EventType identifica o tipo de um evento WebSocket.
type EventType string

const (
	EventTypeMessage           EventType = "message"
	EventTypeMessageEdit       EventType = "message_edit"
	EventTypeMessageDelete     EventType = "message_delete"
	EventTypeMessagePin        EventType = "message_pin"
	EventTypeChannelCreate     EventType = "channel_create"
	EventTypeChannelUpdate     EventType = "channel_update"
	EventTypeChannelDelete     EventType = "channel_delete"
	EventTypeTyping            EventType = "typing"
	EventTypeAvatarUpdate      EventType = "avatar_update"
	EventTypePresenceUpdate    EventType = "presence_update"
	EventTypePresenceSync      EventType = "presence_sync"
	EventTypeHeartbeat         EventType = "heartbeat"
	EventTypeHeartbeatAck      EventType = "heartbeat_ack"
	EventTypeError             EventType = "error"
	EventTypeNewPreview        EventType = "new_preview"
	EventTypeRemovePreview     EventType = "remove_preview"
	EventTypeLinkPreviewUpdate EventType = "link_preview_update"
	EventTypeUserJoin          EventType = "user_join"
	EventTypeReactUpdate       EventType = "react_update"
	EventTypeNewNotification   EventType = "new_notification"
	EventTypeRoleAdd           EventType = "role_add"
	EventTypeRoleRemove        EventType = "role_remove"
	// Eventos de voz (canais type "voice" + SFU). Inbound: client → server.
	// Outbound: voice_joined, voice_answer, voice_ice_candidate,
	// voice_state_update, voice_leave, active_speaker_update.
	EventTypeVoiceJoin         EventType = "voice_join"
	EventTypeVoiceLeave        EventType = "voice_leave"
	EventTypeVoiceOffer        EventType = "voice_offer"
	EventTypeVoiceAnswer       EventType = "voice_answer"
	EventTypeVoiceICECandidate EventType = "voice_ice_candidate"
	EventTypeTrackSubscribe    EventType = "track_subscribe"
	EventTypeTrackUnsubscribe  EventType = "track_unsubscribe"
	EventTypeVoiceMute         EventType = "voice_mute"
	EventTypeVoiceCamera       EventType = "voice_camera"
	EventTypeScreenShareStart  EventType = "screen_share_start"
	EventTypeScreenShareStop   EventType = "screen_share_stop"
	// EventTypeAttachmentModerationUpdate é o evento de mudança de estado da
	// moderação assíncrona de um attachment de imagem (sensitive; o blocked
	// chega como message_delete, pois a mensagem inteira é excluída).
	EventTypeAttachmentModerationUpdate EventType = "attachment_moderation_update"
)

// IsInbound indica se o tipo de evento é aceito no sentido cliente ->
// servidor, conforme o contrato da API.
func (t EventType) IsInbound() bool {
	switch t {
	case EventTypeTyping, EventTypeHeartbeat,
		EventTypeVoiceJoin, EventTypeVoiceLeave, EventTypeVoiceOffer,
		EventTypeVoiceAnswer, EventTypeVoiceICECandidate,
		EventTypeTrackSubscribe, EventTypeTrackUnsubscribe,
		EventTypeVoiceMute, EventTypeVoiceCamera,
		EventTypeScreenShareStart, EventTypeScreenShareStop:
		return true
	default:
		return false
	}
}

// Eventos inbound (cliente -> servidor)

// TypingInbound é o evento de digitação enviado pelo cliente.
type TypingInbound struct {
	Type      EventType `json:"type"`
	ChannelID string    `json:"channel_id"`
}

// HeartbeatInbound é o evento de keepalive enviado pelo cliente.
type HeartbeatInbound struct {
	Type EventType `json:"type"`
}

// Eventos inbound de voz (cliente -> servidor). O despachamento e a permissão
// (connect_voice no join; membership nos demais) ficam em voice.go.

// VoiceJoinInbound pede a entrada do usuário na sala de voz do canal.
type VoiceJoinInbound struct {
	Type      EventType `json:"type"`
	ChannelID string    `json:"channel_id"`
}

// VoiceLeaveInbound pede a saída explícita do usuário da sala de voz.
type VoiceLeaveInbound struct {
	Type      EventType `json:"type"`
	ChannelID string    `json:"channel_id"`
}

// VoiceOfferInbound é a oferta SDP do cliente (join, screen share, mais slots).
type VoiceOfferInbound struct {
	Type      EventType `json:"type"`
	ChannelID string    `json:"channel_id"`
	SDP       string    `json:"sdp"`
}

// VoiceAnswerInbound é a resposta SDP do cliente a uma renegociação do servidor.
type VoiceAnswerInbound struct {
	Type      EventType `json:"type"`
	ChannelID string    `json:"channel_id"`
	SDP       string    `json:"sdp"`
}

// VoiceICECandidateInbound é um candidate trickle de ICE do cliente.
type VoiceICECandidateInbound struct {
	Type          EventType `json:"type"`
	ChannelID     string    `json:"channel_id"`
	Candidate     string    `json:"candidate"`
	SDPMid        *string   `json:"sdp_mid"`
	SDPMLineIndex *int      `json:"sdp_mline_index"`
}

// TrackSubscribeInbound pede o forward da track de vídeo/screen do publisher.
type TrackSubscribeInbound struct {
	Type        EventType `json:"type"`
	ChannelID   string    `json:"channel_id"`
	PublisherID string    `json:"publisher_id"`
	Kind        string    `json:"kind"`
}

// TrackUnsubscribeInbound para o forward da track de vídeo/screen do publisher.
type TrackUnsubscribeInbound struct {
	Type        EventType `json:"type"`
	ChannelID   string    `json:"channel_id"`
	PublisherID string    `json:"publisher_id"`
	Kind        string    `json:"kind"`
}

// VoiceMuteInbound atualiza o estado do mic do usuário.
type VoiceMuteInbound struct {
	Type      EventType `json:"type"`
	ChannelID string    `json:"channel_id"`
	Muted     bool      `json:"muted"`
}

// VoiceCameraInbound atualiza o estado da câmera do usuário.
type VoiceCameraInbound struct {
	Type      EventType `json:"type"`
	ChannelID string    `json:"channel_id"`
	On        bool      `json:"on"`
}

// ScreenShareStartInbound marca o início do screen share (a track chega na
// renegociação seguinte).
type ScreenShareStartInbound struct {
	Type      EventType `json:"type"`
	ChannelID string    `json:"channel_id"`
}

// ScreenShareStopInbound marca o fim do screen share.
type ScreenShareStopInbound struct {
	Type      EventType `json:"type"`
	ChannelID string    `json:"channel_id"`
}

// Eventos outbound (servidor -> cliente)

// MessageOutbound é o evento de nova mensagem distribuído aos clientes.
// Attachments omite o campo quando a mensagem não tem attachments.
// ReplyTo é a mensagem referenciada (null quando a mensagem não é uma
// resposta); pode apontar para uma mensagem já excluída (apontador pendente).
type MessageOutbound struct {
	Type        EventType                  `json:"type"`
	ID          string                     `json:"id"`
	ChannelID   string                     `json:"channel_id"`
	AuthorID    string                     `json:"author_id"`
	Content     string                     `json:"content"`
	CreatedAt   time.Time                  `json:"created_at"`
	ReplyTo     *string                    `json:"reply_to"`
	Attachments []models.MessageAttachment `json:"attachments,omitempty"`
}

// MessageEditOutbound é o evento de edição de mensagem distribuído aos clientes.
type MessageEditOutbound struct {
	Type      EventType `json:"type"`
	ID        string    `json:"id"`
	ChannelID string    `json:"channel_id"`
	Content   string    `json:"content"`
	EditedAt  time.Time `json:"edited_at"`
}

// MessageDeleteOutbound é o evento de exclusão de mensagem distribuído aos clientes.
type MessageDeleteOutbound struct {
	Type      EventType `json:"type"`
	ID        string    `json:"id"`
	ChannelID string    `json:"channel_id"`
}

// AttachmentModerationUpdateOutbound é o evento de mudança de estado da
// moderação assíncrona de um attachment de imagem (nudez/gore), distribuído
// aos leitores do canal após a inferência. Status: sensitive (clean não
// gera evento; blocked exclui a mensagem e gera message_delete).
type AttachmentModerationUpdateOutbound struct {
	Type         EventType `json:"type"`
	ChannelID    string    `json:"channel_id"`
	MessageID    string    `json:"message_id"`
	AttachmentID string    `json:"attachment_id"`
	Status       string    `json:"status"`
}

// NewPreviewOutbound é o evento de link preview vinculado a uma mensagem,
// distribuído após o processamento em background (crawl) da mensagem. Um
// evento por preview.
type NewPreviewOutbound struct {
	Type      EventType `json:"type"`
	MessageID string    `json:"message_id"`
	PreviewID string    `json:"preview_id"`
}

// RemovePreviewOutbound é o evento de link preview removido de uma mensagem
// (edição que substitui/limpa os previews), distribuído após o processamento
// em background. Um evento por preview.
type RemovePreviewOutbound struct {
	Type      EventType `json:"type"`
	MessageID string    `json:"message_id"`
	PreviewID string    `json:"preview_id"`
}

// LinkPreviewObject é o objeto de link preview distribuído no evento
// link_preview_update: mesma forma da resposta de GET /link-previews/:id
// (campos públicos + image_data, base64 da thumbnail, null quando não há
// imagem).
type LinkPreviewObject struct {
	models.LinkPreview
	ImageData *string `json:"image_data"`
}

// LinkPreviewUpdateOutbound é o evento de link preview atualizado (refetch
// com cache expirou e a row foi reescrita), distribuído aos leitores do canal
// com o objeto completo — o cliente atualiza o cache sem refetch. Um evento
// por mensagem vinculada ao preview.
type LinkPreviewUpdateOutbound struct {
	Type      EventType         `json:"type"`
	ChannelID string            `json:"channel_id"`
	MessageID string            `json:"message_id"`
	Preview   LinkPreviewObject `json:"preview"`
}

// ReactUpdateOutbound é o evento de atualização de reação em uma mensagem
// (reação adicionada ou removida), distribuído aos clientes que leem o canal.
// EmojiID é o id do emoji custom do banco (null para emoji unicode) e
// Unicode é o emoji unicode (null para emoji custom). Count é o número de
// usuários que reagiram com aquele emoji após a operação (0 quando a última
// reação foi removida).
type ReactUpdateOutbound struct {
	Type      EventType `json:"type"`
	MessageID string    `json:"message_id"`
	EmojiID   *string   `json:"emoji_id"`
	Unicode   *string   `json:"unicode"`
	Count     int       `json:"count"`
}

// MessagePinOutbound é o evento de fixação de mensagem (pin adicionado ou
// removido), distribuído aos clientes que leem o canal. IsPinned é true
// quando a mensagem passou a estar pinada e false quando a fixação foi
// removida.
type MessagePinOutbound struct {
	Type      EventType `json:"type"`
	MessageID string    `json:"message_id"`
	IsPinned  bool      `json:"is_pinned"`
}

// ChannelCreateOutbound é o evento de criação de canal distribuído aos clientes.
// Topic é null para canais category (ou sem tópico).
type ChannelCreateOutbound struct {
	Type        EventType `json:"type"`
	ChannelID   string    `json:"channel_id"`
	Name        string    `json:"name"`
	ChannelType string    `json:"channel_type"`
	Position    int       `json:"position"`
	Topic       *string   `json:"topic"`
}

// ChannelUpdateOutbound é o evento de atualização de canal distribuído aos clientes.
// Topic é null para canais category (ou sem tópico).
type ChannelUpdateOutbound struct {
	Type      EventType `json:"type"`
	ChannelID string    `json:"channel_id"`
	Name      string    `json:"name"`
	Position  int       `json:"position"`
	Topic     *string   `json:"topic"`
}

// ChannelDeleteOutbound é o evento de exclusão de canal distribuído aos clientes.
type ChannelDeleteOutbound struct {
	Type      EventType `json:"type"`
	ChannelID string    `json:"channel_id"`
}

// TypingOutbound é o evento de digitação distribuído aos clientes.
type TypingOutbound struct {
	Type      EventType `json:"type"`
	ChannelID string    `json:"channel_id"`
	UserID    string    `json:"user_id"`
	IsTyping  bool      `json:"is_typing"`
}

// AvatarUpdateOutbound é o evento de atualização de avatar distribuído aos
// clientes.
type AvatarUpdateOutbound struct {
	Type   EventType `json:"type"`
	UserID string    `json:"user_id"`
}

// PresenceUpdateOutbound é o evento de presença/status distribuído aos clientes.
// Status: online/offline (efêmero) ou away/busy (persistido pelo usuário).
type PresenceUpdateOutbound struct {
	Type          EventType `json:"type"`
	UserID        string    `json:"user_id"`
	Status        string    `json:"status"`                   //online/offline/away/busy
	StatusMessage *string   `json:"status_message,omitempty"` //mensagem pessoal
	Nickname      *string   `json:"nickname,omitempty"`       //apelido do usuário
}

// UserJoinOutbound é o evento de novo usuário distribuído aos clientes
// conectados após o registro (POST /auth/register).
type UserJoinOutbound struct {
	Type   EventType `json:"type"`
	UserID string    `json:"user_id"`
}

// RoleAddOutbound é o evento de atribuição de role a um usuário
// (POST /users/:user_id/roles), distribuído aos clientes conectados.
type RoleAddOutbound struct {
	Type   EventType `json:"type"`
	UserID string    `json:"user_id"`
	RoleID string    `json:"role_id"`
}

// RoleRemoveOutbound é o evento de remoção de role de um usuário
// (DELETE /users/:user_id/roles/:role_id), distribuído aos clientes conectados.
type RoleRemoveOutbound struct {
	Type   EventType `json:"type"`
	UserID string    `json:"user_id"`
	RoleID string    `json:"role_id"`
}

// PresenceSyncOutbound é a lista de membros online enviada ao cliente no
// estabelecimento de cada conexão (unicast).
type PresenceSyncOutbound struct {
	Type    EventType        `json:"type"`
	Members []PresenceMember `json:"members"`
}

// HeartbeatAckOutbound é o ack de keepalive enviado ao cliente.
type HeartbeatAckOutbound struct {
	Type EventType `json:"type"`
}

// ErrorOutbound é o evento de erro enviado ao cliente.
// Code omite o campo quando não há código legível por máquina.
type ErrorOutbound struct {
	Type    EventType `json:"type"`
	Message string    `json:"message"`
	Code    *string   `json:"code,omitempty"`
}

// NewNotificationOutbound é o evento de nova notificação enviado em unicast
// ao usuário notificado. ID é o id da row de notificação (ou um UUID efêmero
// quando a configuração 'all' gera evento sem row); UserID é o autor da
// mensagem; MessageContent é o conteúdo truncado a 512 caracteres.
type NewNotificationOutbound struct {
	Type           EventType `json:"type"`
	ID             string    `json:"id"`
	UserID         string    `json:"user_id"`
	MessageID      string    `json:"message_id"`
	MessageContent string    `json:"message_content"`
}
