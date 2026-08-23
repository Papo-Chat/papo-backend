package models

import "time"

// Emoji representa a tabela emojis.
type Emoji struct {
	ID        string    `db:"id" json:"id"`
	ServerID  string    `db:"server_id" json:"server_id"`
	Name      string    `db:"name" json:"name"`
	Format    string    `db:"format" json:"format"`
	ImageBlob []byte    `db:"image_blob" json:"image_blob"`
	CreatedBy *string   `db:"created_by" json:"created_by"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

// EmojiList é a resposta paginada de GET /emojis.
type EmojiList struct {
	Emojis  []Emoji `json:"emojis"`
	HasMore bool    `json:"has_more"`
}
