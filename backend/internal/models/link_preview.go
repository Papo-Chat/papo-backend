package models

import "time"

// LinkPreview representa a tabela link_previews (cache por URL normalizada).
// Kind: 'og' (OpenGraph) ou 'oembed' (provedor allowlistado).
type LinkPreview struct {
	ID             string    `db:"id" json:"id"`
	URL            string    `db:"url" json:"url"`
	Kind           string    `db:"kind" json:"kind"`
	Title          *string   `db:"title" json:"title"`
	Description    *string   `db:"description" json:"description"`
	ProviderName   *string   `db:"provider_name" json:"provider_name"`
	EmbedURL       *string   `db:"embed_url" json:"embed_url"`
	ImageFilePath  *string   `db:"image_file_path" json:"-"`
	ImageMimeType  *string   `db:"image_mime_type" json:"image_mime_type"`
	ImageSizeBytes *int64    `db:"image_size_bytes" json:"image_size_bytes"`
	FetchedAt      time.Time `db:"fetched_at" json:"fetched_at"`
}

// AttachmentThumbnail representa a tabela attachment_thumbnails.
// Kind: 'preview' (512px / 128px GIF).
type AttachmentThumbnail struct {
	ID           string    `db:"id" json:"id"`
	AttachmentID string    `db:"attachment_id" json:"attachment_id"`
	Kind         string    `db:"kind" json:"kind"`
	MimeType     string    `db:"mime_type" json:"mime_type"`
	FilePath     string    `db:"file_path" json:"-"`
	SizeBytes    int64     `db:"size_bytes" json:"size_bytes"`
	Width        int       `db:"width" json:"width"`
	Height       int       `db:"height" json:"height"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}
