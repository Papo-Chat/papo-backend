package websocket

import (
	"time"

	"papo/internal/models"
)

// EventType identifica o tipo de um evento WebSocket.
type EventType string

const (
	EventTypeMessage         EventType = "message"
	EventTypeMessageEdit     EventType = "message_edit"
	EventTypeMessageDelete   EventType = "message_delete"
	EventTypeMessagePin      EventType = "message_pin"
	EventTypeChannelCreate   EventType = "channel_create"
	EventTypeChannelUpdate   EventType = "channel_update"
	EventTypeChannelDelete   EventType = "channel_delete"
	EventTypeTyping          EventType = "typing"
	EventTypeAvatarUpdate    EventType = "avatar_update"
	EventTypePresenceUpdate  EventType = "presence_update"
	EventTypePresenceSync    EventType = "presence_sync"
	EventTypeHeartbeat       EventType = "heartbeat"
	EventTypeHeartbeatAck    EventType = "heartbeat_ack"
	EventTypeError           EventType = "error"
	EventTypeNewPreview      EventType = "new_preview"
	EventTypeRemovePreview   EventType = "remove_preview"
	EventTypeUserJoin        EventType = "user_join"
	EventTypeReactUpdate     EventType = "react_update"
	EventTypeNewNotification EventType = "new_notification"
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
