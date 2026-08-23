package models

import "time"

// Upload representa a tabela attachments.
type Attachments struct {
	ID               string    `db:"id" json:"id"`
	OriginalFileName string    `db:"original_file_name" json:"original_file_name"`
	MimeType         string    `db:"mime_type" json:"mime_type"`
	FilePath         string    `db:"file_path" json:"file_path"`
	ShaHash          string    `db:"sha_hash" json:"-"`
	MessagesID       *string   `db:"messages_id" json:"-"`
	SizeBytes        int64     `db:"size_bytes" json:"size_bytes"`
	CreatedBy        *string   `db:"created_by" json:"-"`
	CreatedAt        time.Time `db:"created_at" json:"created_at"`
}
