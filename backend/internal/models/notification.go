package models

import "time"

// Tipos de configuração de notificação por canal (channel_user_settings).
// Ausência de row equivale a NotificationTypeOnlyMentions (padrão).
const (
	NotificationTypeOff          = "off"
	NotificationTypeOnlyMentions = "only_mentions"
	NotificationTypeAll          = "all"
)

// ChannelUserSetting representa a tabela channel_user_settings: a
// configuração de notificação de um usuário em um canal.
type ChannelUserSetting struct {
	UserID               string    `db:"user_id" json:"user_id"`
	ChannelID            string    `db:"channel_id" json:"channel_id"`
	NotificationSettings string    `db:"notification_settings" json:"notification_settings"`
	UpdatedAt            time.Time `db:"updated_at" json:"updated_at"`
}

// Notification representa a tabela notifications: uma notificação
// persistida de uma mensagem para um usuário (read = false até o usuário
// marcá-la como lida).
type Notification struct {
	ID        string    `db:"id" json:"id"`
	UserID    string    `db:"user_id" json:"user_id"`
	MessageID string    `db:"message_id" json:"message_id"`
	Read      bool      `db:"read" json:"read"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

// ChannelNotificationCandidate é um usuário com a configuração de
// notificação efetiva em um canal (ausência de row = only_mentions), como
// usado pela rotina de disparo de notificações.
type ChannelNotificationCandidate struct {
	UserID               string
	NotificationSettings string
}

// NotificationSummary é a notificação exposta na listagem (GET
// /users/:user_id/notifications), com o preview do conteúdo da mensagem
// truncado a 512 caracteres.
type NotificationSummary struct {
	ID             string    `json:"id"`
	MessageID      string    `json:"message_id"`
	ChannelID      string    `json:"channel_id"`
	AuthorID       *string   `json:"author_id"`
	MessageContent string    `json:"message_content"`
	Read           bool      `json:"read"`
	CreatedAt      time.Time `json:"created_at"`
}

// NotificationList é a resposta de GET /users/:user_id/notifications
// (mais recentes primeiro; Notifications é [] quando não há notificações).
type NotificationList struct {
	Notifications []NotificationSummary `json:"notifications"`
	HasMore       bool                  `json:"has_more"`
}
