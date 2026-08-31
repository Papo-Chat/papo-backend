package models

import "time"

// Emoji representa a tabela emojis.
// ImageMedia referencia o blob da imagem na tabela media; ImageBlob e Format
// são resolvidos do disco/pela tabela media pelo service para as respostas
// da API.
type Emoji struct {
	ID         string    `db:"id" json:"id"`
	Name       string    `db:"name" json:"name"`
	ImageMedia string    `db:"image_media" json:"-"`
	MimeType   string    `db:"mime_type" json:"-"`
	ImageBlob  []byte    `json:"image_blob"`
	Format     string    `json:"format"`
	CreatedBy  *string   `db:"created_by" json:"created_by"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

// EmojiList é a resposta paginada de GET /emojis.
type EmojiList struct {
	Emojis  []Emoji `json:"emojis"`
	HasMore bool    `json:"has_more"`
}
