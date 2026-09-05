package models

import "time"

// LinkPreview representa a tabela link_previews (cache por URL normalizada).
// Kind: 'og' (OpenGraph) ou 'oembed' (provedor allowlistado).
// ImageMedia referencia o blob da thumbnail na tabela media (nil quando não
// há imagem); ImageMimeType e ImageSizeBytes vêm da tabela media (join) e
// ImageFilePath é resolvido do hash pelo service.
type LinkPreview struct {
	ID             string    `db:"id" json:"id"`
	URL            string    `db:"url" json:"url"`
	Kind           string    `db:"kind" json:"kind"`
	Title          *string   `db:"title" json:"title"`
	Description    *string   `db:"description" json:"description"`
	ProviderName   *string   `db:"provider_name" json:"provider_name"`
	EmbedURL       *string   `db:"embed_url" json:"embed_url"`
	ImageMedia     *string   `db:"image_media" json:"-"`
	ImageMimeType  *string   `json:"image_mime_type"`
	ImageSizeBytes *int64    `json:"image_size_bytes"`
	ImageFilePath  *string   `json:"-"`
	FetchedAt      time.Time `db:"fetched_at" json:"fetched_at"`
}

// PreviewMessageRef identifica uma mensagem vinculada a um link preview (e o
// canal da mensagem) — alvo do evento link_preview_update.
type PreviewMessageRef struct {
	MessageID string
	ChannelID string
}

// AttachmentThumbnail representa a tabela attachment_thumbnails.
// Kind: 'preview' (512px / 128px GIF).
// MediaShaHash referencia o blob da thumbnail na tabela media; MimeType vem
// da tabela media (join) e FilePath é resolvido do hash pelo service.
type AttachmentThumbnail struct {
	ID           string    `db:"id" json:"id"`
	AttachmentID string    `db:"attachment_id" json:"attachment_id"`
	Kind         string    `db:"kind" json:"kind"`
	MediaShaHash string    `db:"media_sha_hash" json:"-"`
	MimeType     string    `json:"mime_type"`
	FilePath     string    `json:"-"`
	Width        int       `db:"width" json:"width"`
	Height       int       `db:"height" json:"height"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}
