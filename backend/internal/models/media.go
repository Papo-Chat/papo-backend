package models

import "time"

// Media representa a tabela media: o registro content-addressable de um
// blob gravado em disco uma única vez (media/<2hex>/<2hex>/<sha256>).
// Todas as mídias do sistema (avatar, ícone, emoji, attachment, thumbnail e
// imagem de link preview) referenciam o blob pelo sha_hash.
type Media struct {
	ShaHash   string    `db:"sha_hash" json:"-"`
	MimeType  string    `db:"mime_type" json:"-"`
	SizeBytes int64     `db:"size_bytes" json:"-"`
	CreatedAt time.Time `db:"created_at" json:"-"`
}
