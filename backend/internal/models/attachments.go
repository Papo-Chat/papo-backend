package models

import "time"

// Upload representa a tabela attachments.
// MediaShaHash referencia o blob do arquivo na tabela media; MimeType e
// SizeBytes vêm da tabela media (join) e FilePath é resolvido do hash pelo
// service (caminho do blob em disco).
type Attachments struct {
	ID               string    `db:"id" json:"id"`
	OriginalFileName string    `db:"original_file_name" json:"original_file_name"`
	MediaShaHash     string    `db:"media_sha_hash" json:"-"`
	MimeType         string    `json:"mime_type"`
	FilePath         string    `json:"-"`
	MessagesID       *string   `db:"messages_id" json:"-"`
	SizeBytes        int64     `json:"size_bytes"`
	CreatedBy        *string   `db:"created_by" json:"-"`
	CreatedAt        time.Time `db:"created_at" json:"created_at"`

	// Moderação de imagens (nudez/gore), assíncrona. ModerationStatus:
	// pending/processing/clean/sensitive/blocked/failed (ver
	// migrations/008_moderation.sql). Exposto nas respostas de mensagens via
	// MessageAttachment.ModerationStatus.
	ModerationStatus       string     `db:"moderation_status" json:"-"`
	ModerationAttempts     int        `db:"moderation_attempts" json:"-"`
	ModerationCheckedAt    *time.Time `db:"moderation_checked_at" json:"-"`
	ModerationUpdatedAt    *time.Time `db:"moderation_updated_at" json:"-"`
	ModerationModelVersion *string    `db:"moderation_model_version" json:"-"`
	ModerationSFWScore     *float64   `db:"moderation_sfw_score" json:"-"`
	ModerationNudityScore  *float64   `db:"moderation_nudity_score" json:"-"`
	ModerationGoreScore    *float64   `db:"moderation_gore_score" json:"-"`
}
