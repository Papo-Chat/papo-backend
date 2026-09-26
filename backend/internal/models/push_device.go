package models

import "time"

// PushDevice representa a tabela push_devices: um dispositivo registrado para
// notificação push (FCM), associado ao usuário autenticado que o registrou.
type PushDevice struct {
	ID         string    `json:"id"`
	UserID     string    `json:"user_id"`
	Token      string    `json:"token"`
	Platform   string    `json:"platform"`
	Provider   string    `json:"provider"`
	DeviceName *string   `json:"device_name"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

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
