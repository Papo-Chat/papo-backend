package models

import "time"

// DirectConversation é a visão de uma DM para um usuário específico.
// ID é também o channel_id usado pelo pipeline de mensagens existente.
type DirectConversation struct {
	ID              string              `json:"id"`
	User            UserSummary         `json:"user"`
	CreatedAt       time.Time           `json:"created_at"`
	LastMessage     *ChannelLastMessage `json:"last_message"`
	LastReadMessage *string             `json:"last_read_message"`
	LastReadAt      *time.Time          `json:"last_read_at"`
	UnreadCount     int                 `json:"unread_count"`
}

type DirectConversationList struct {
	DMs []DirectConversation `json:"dms"`
}
