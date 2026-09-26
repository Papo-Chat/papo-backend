// Package models contém os tipos de payload do papo-push (dupla do
// backend/internal/models para o módulo próprio).
package models

// PushJobPayload é o payload gravado em push_outbox.payload (JSONB).
// Contém apenas o mínimo necessário para o papo-push construir a mensagem
// de push (título, corpo e IDs para o app abrir a conversa).
type PushJobPayload struct {
	Type           string  `json:"type"`
	NotificationID *string `json:"notification_id,omitempty"`
	MessageID      string  `json:"message_id"`
	ChannelID      string  `json:"channel_id"`
	Author         string  `json:"author"`
	Preview        string  `json:"preview"`
}
