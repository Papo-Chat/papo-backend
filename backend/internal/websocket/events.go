package websocket

import (
	"time"

	"papo/internal/models"
)

// EventType identifica o tipo de um evento WebSocket.
type EventType string

const (
	EventTypeMessage        EventType = "message"
	EventTypeMessageEdit    EventType = "message_edit"
	EventTypeMessageDelete  EventType = "message_delete"
	EventTypeChannelCreate  EventType = "channel_create"
	EventTypeChannelUpdate  EventType = "channel_update"
	EventTypeChannelDelete  EventType = "channel_delete"
	EventTypeTyping         EventType = "typing"
	EventTypeAvatarUpdate   EventType = "avatar_update"
	EventTypePresenceUpdate EventType = "presence_update"
	EventTypePresenceSync   EventType = "presence_sync"
	EventTypeHeartbeat      EventType = "heartbeat"
	EventTypeHeartbeatAck   EventType = "heartbeat_ack"
	EventTypeError          EventType = "error"
	EventTypeNewPreview     EventType = "new_preview"
	EventTypeRemovePreview  EventType = "remove_preview"
	EventTypeUserJoin       EventType = "user_join"
)

// IsInbound indica se o tipo de evento é aceito no sentido cliente ->
// servidor, conforme o contrato da API.
func (t EventType) IsInbound() bool {
	switch t {
	case EventTypeTyping, EventTypeHeartbeat:
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

// Eventos outbound (servidor -> cliente)

// MessageOutbound é o evento de nova mensagem distribuído aos clientes.
// Attachments omite o campo quando a mensagem não tem attachments.
type MessageOutbound struct {
	Type        EventType                  `json:"type"`
	ID          string                     `json:"id"`
	ChannelID   string                     `json:"channel_id"`
	AuthorID    string                     `json:"author_id"`
	Content     string                     `json:"content"`
	CreatedAt   time.Time                  `json:"created_at"`
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

// ChannelCreateOutbound é o evento de criação de canal distribuído aos clientes.
type ChannelCreateOutbound struct {
	Type        EventType `json:"type"`
	ChannelID   string    `json:"channel_id"`
	Name        string    `json:"name"`
	ChannelType string    `json:"channel_type"`
	Position    int       `json:"position"`
}

// ChannelUpdateOutbound é o evento de atualização de canal distribuído aos clientes.
type ChannelUpdateOutbound struct {
	Type      EventType `json:"type"`
	ChannelID string    `json:"channel_id"`
	Name      string    `json:"name"`
	Position  int       `json:"position"`
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
