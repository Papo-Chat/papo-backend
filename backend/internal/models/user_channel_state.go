package models

import "time"

// UserChannelState representa a tabela user_channel_state.
type UserChannelState struct {
	UserID            string    `db:"user_id" json:"user_id"`
	ChannelID         string    `db:"channel_id" json:"channel_id"`
	LastReadMessageID string    `db:"last_read_message_id" json:"last_read_message_id"`
	LastReadAt        time.Time `db:"last_read_at" json:"last_read_at"`
}
