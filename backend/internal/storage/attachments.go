package storage

import (
	"context"
	"fmt"
	"time"

	"papo/internal/models"
)

// attachmentColumns inclui mime_type e size_bytes da tabela media (join):
// o attachment só guarda a referência content-addressable do blob. As
// colunas moderation_* são o estado da moderação assíncrona de imagens
// (migrations/008_moderation.sql).
const attachmentColumns = "a.id, a.original_file_name, m.mime_type, a.media_sha_hash, a.messages_id, m.size_bytes, a.created_by, a.created_at, a.moderation_status, a.moderation_attempts, a.moderation_checked_at, a.moderation_updated_at, a.moderation_model_version, a.moderation_sfw_score, a.moderation_nudity_score, a.moderation_gore_score"
const attachmentFrom = "FROM attachments a JOIN media m ON m.sha_hash = a.media_sha_hash"

func scanAttachment(row rowScanner) (models.Attachments, error) {
	var attachment models.Attachments
	err := row.Scan(
		&attachment.ID,
		&attachment.OriginalFileName,
		&attachment.MimeType,
		&attachment.MediaShaHash,
		&attachment.MessagesID,
		&attachment.SizeBytes,
		&attachment.CreatedBy,
		&attachment.CreatedAt,
		&attachment.ModerationStatus,
		&attachment.ModerationAttempts,
		&attachment.ModerationCheckedAt,
		&attachment.ModerationUpdatedAt,
		&attachment.ModerationModelVersion,
		&attachment.ModerationSFWScore,
		&attachment.ModerationNudityScore,
		&attachment.ModerationGoreScore,
	)
	if err != nil {
		return models.Attachments{}, err
	}

	return attachment, nil
}

// CreateAttachment insere um novo attachment (upload ainda não vinculado a
// uma mensagem) e retorna o registro criado. MediaShaHash referencia o blob
// já gravado na tabela media.
func CreateAttachment(ctx context.Context, a models.Attachments) (models.Attachments, error) {
	// O SELECT lê o resultado do RETURNING (não a tabela attachments): a
	// query principal e o CTE de dados compartilham o mesmo snapshot, então
	// a linha inserida ainda não seria visível na tabela.
	row := GetDB().QueryRowContext(ctx,
		`WITH inserted AS (
			INSERT INTO attachments (original_file_name, media_sha_hash, created_by) VALUES ($1, $2, $3)
			RETURNING id, original_file_name, media_sha_hash, messages_id, created_by, created_at,
			          moderation_status, moderation_attempts, moderation_checked_at, moderation_updated_at,
			          moderation_model_version, moderation_sfw_score, moderation_nudity_score, moderation_gore_score
		 )
		 SELECT i.id, i.original_file_name, m.mime_type, i.media_sha_hash, i.messages_id, m.size_bytes, i.created_by, i.created_at,
		        i.moderation_status, i.moderation_attempts, i.moderation_checked_at, i.moderation_updated_at,
		        i.moderation_model_version, i.moderation_sfw_score, i.moderation_nudity_score, i.moderation_gore_score
		 FROM inserted i JOIN media m ON m.sha_hash = i.media_sha_hash`,
		a.OriginalFileName, a.MediaShaHash, a.CreatedBy,
	)

	attachment, err := scanAttachment(row)
	if err != nil {
		return models.Attachments{}, mapStorageError(err)
	}

	return attachment, nil
}

// GetAttachmentByID busca um attachment pelo id.
func GetAttachmentByID(ctx context.Context, id string) (models.Attachments, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+attachmentColumns+" "+attachmentFrom+" WHERE a.id = $1",
		id,
	)

	attachment, err := scanAttachment(row)
	if err != nil {
		return models.Attachments{}, mapStorageError(err)
	}

	return attachment, nil
}

const thumbnailColumns = "t.id, t.attachment_id, t.kind, m.mime_type, t.media_sha_hash, t.width, t.height, t.created_at"
const thumbnailFrom = "FROM attachment_thumbnails t JOIN media m ON m.sha_hash = t.media_sha_hash"

func scanAttachmentThumbnail(row rowScanner) (models.AttachmentThumbnail, error) {
	var thumbnail models.AttachmentThumbnail
	err := row.Scan(
		&thumbnail.ID,
		&thumbnail.AttachmentID,
		&thumbnail.Kind,
		&thumbnail.MimeType,
		&thumbnail.MediaShaHash,
		&thumbnail.Width,
		&thumbnail.Height,
		&thumbnail.CreatedAt,
	)
	if err != nil {
		return models.AttachmentThumbnail{}, err
	}

	return thumbnail, nil
}

// CreateAttachmentThumbnail insere a thumbnail de um attachment.
// ON CONFLICT (attachment_id, kind) DO NOTHING: se já existir (upload
// duplicado do mesmo conteúdo), mantém a primeira.
func CreateAttachmentThumbnail(ctx context.Context, t models.AttachmentThumbnail) error {
	_, err := GetDB().ExecContext(ctx,
		"INSERT INTO attachment_thumbnails (attachment_id, kind, media_sha_hash, width, height) VALUES ($1, $2, $3, $4, $5) ON CONFLICT (attachment_id, kind) DO NOTHING",
		t.AttachmentID, t.Kind, t.MediaShaHash, t.Width, t.Height,
	)
	if err != nil {
		return mapStorageError(err)
	}

	return nil
}

// GetThumbnailByAttachmentID busca a thumbnail de um attachment pelo kind.
// Retorna ErrNotFound quando não existe.
func GetThumbnailByAttachmentID(ctx context.Context, attachmentID, kind string) (models.AttachmentThumbnail, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+thumbnailColumns+" "+thumbnailFrom+" WHERE t.attachment_id = $1 AND t.kind = $2",
		attachmentID, kind,
	)

	thumbnail, err := scanAttachmentThumbnail(row)
	if err != nil {
		return models.AttachmentThumbnail{}, mapStorageError(err)
	}

	return thumbnail, nil
}

// ListThumbnailsByAttachmentIDs busca as thumbnails de vários attachments em
// uma única query (evita N+1 na listagem de mensagens). O mapa é indexado por
// attachment_id; attachments sem thumbnail não aparecem.
func ListThumbnailsByAttachmentIDs(ctx context.Context, attachmentIDs []string) (map[string]models.AttachmentThumbnail, error) {
	thumbnails := make(map[string]models.AttachmentThumbnail, len(attachmentIDs))
	if len(attachmentIDs) == 0 {
		return thumbnails, nil
	}

	rows, err := GetDB().QueryContext(ctx,
		"SELECT "+thumbnailColumns+" "+thumbnailFrom+" WHERE t.attachment_id = ANY($1)",
		attachmentIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar thumbnails: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		thumbnail, err := scanAttachmentThumbnail(rows)
		if err != nil {
			return nil, fmt.Errorf("falha ao ler thumbnail: %w", err)
		}
		thumbnails[thumbnail.AttachmentID] = thumbnail
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar thumbnails: %w", err)
	}

	return thumbnails, nil
}

// DeleteOrphanAttachments remove attachments não vinculados a mensagem
// (messages_id NULL, órfãos de uma gravação incompleta) criados antes do
// cutoff. As thumbnails caem via ON DELETE CASCADE. Retorna o número de
// registros removidos.
func DeleteOrphanAttachments(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := GetDB().ExecContext(ctx,
		"DELETE FROM attachments WHERE messages_id IS NULL AND created_at < $1",
		cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("falha ao remover attachments órfãos: %w", err)
	}
	return res.RowsAffected()
}

// ListAttachmentsByMessage lista os attachments de uma mensagem ordenados por
// data de criação.
func ListAttachmentsByMessage(ctx context.Context, messageID string) ([]models.Attachments, error) {
	rows, err := GetDB().QueryContext(ctx,
		"SELECT "+attachmentColumns+" "+attachmentFrom+" WHERE a.messages_id = $1 ORDER BY a.created_at, a.id",
		messageID,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar attachments: %w", err)
	}
	defer rows.Close()

	attachments := make([]models.Attachments, 0)
	for rows.Next() {
		attachment, err := scanAttachment(rows)
		if err != nil {
			return nil, fmt.Errorf("falha ao ler attachment: %w", err)
		}
		attachments = append(attachments, attachment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar attachments: %w", err)
	}

	return attachments, nil
}

// ClaimAttachmentForModeration marca um attachment como 'processing' de
// forma atômica (transição condicional: evita processamento duplicado quando
// o reconciler e o enqueue correm em paralelo). Aceita 'pending' e
// 'processing' órfãos com mais de staleAfter (crash em pleno
// processamento). Retorna true quando a transição aconteceu.
func ClaimAttachmentForModeration(ctx context.Context, id string, staleAfter time.Duration) (bool, error) {
	res, err := GetDB().ExecContext(ctx,
		`UPDATE attachments
		 SET moderation_status = 'processing', moderation_updated_at = now()
		 WHERE id = $1
		   AND (moderation_status = 'pending'
		        OR (moderation_status = 'processing' AND moderation_updated_at < now() - $2::interval))`,
		id, fmt.Sprintf("%d seconds", int(staleAfter.Seconds())),
	)
	if err != nil {
		return false, fmt.Errorf("falha ao marcar attachment para moderação: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("falha ao marcar attachment para moderação: %w", err)
	}

	return affected == 1, nil
}

// FinishAttachmentModeration grava o resultado final da moderação
// (clean/sensitive/blocked) com os scores e a versão do modelo usado.
func FinishAttachmentModeration(ctx context.Context, id, status string, modelVersion *string, sfw, nudity, gore *float64) error {
	_, err := GetDB().ExecContext(ctx,
		`UPDATE attachments
		 SET moderation_status = $2,
		     moderation_checked_at = now(),
		     moderation_updated_at = now(),
		     moderation_model_version = $3,
		     moderation_sfw_score = $4,
		     moderation_nudity_score = $5,
		     moderation_gore_score = $6
		 WHERE id = $1`,
		id, status, modelVersion, sfw, nudity, gore,
	)
	if err != nil {
		return fmt.Errorf("falha ao gravar resultado da moderação: %w", err)
	}

	return nil
}

// FailAttachmentModeration incrementa as tentativas de moderação e grava o
// novo estado: 'pending' (nova tentativa, o reconciler recoloca) ou 'failed'
// (tentativas esgotadas).
func FailAttachmentModeration(ctx context.Context, id, status string) error {
	_, err := GetDB().ExecContext(ctx,
		"UPDATE attachments SET moderation_attempts = moderation_attempts + 1, moderation_status = $2, moderation_updated_at = now() WHERE id = $1",
		id, status,
	)
	if err != nil {
		return fmt.Errorf("falha ao registrar falha de moderação: %w", err)
	}

	return nil
}

// ListModerationPending lista os ids de attachments pendentes de moderação:
// 'pending' (fila normal) e 'processing' órfãos com mais de staleAfter
// (crash do processo em pleno processamento), na ordem de criação.
func ListModerationPending(ctx context.Context, staleAfter time.Duration, limit int) ([]string, error) {
	if limit <= 0 {
		return []string{}, nil
	}

	rows, err := GetDB().QueryContext(ctx,
		`SELECT a.id FROM attachments a
		 WHERE a.moderation_status = 'pending'
		    OR (a.moderation_status = 'processing' AND a.moderation_updated_at < now() - $2::interval)
		 ORDER BY a.created_at
		 LIMIT $1`,
		limit, fmt.Sprintf("%d seconds", int(staleAfter.Seconds())),
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar pendentes de moderação: %w", err)
	}
	defer rows.Close()

	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("falha ao ler pendente de moderação: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar pendentes de moderação: %w", err)
	}

	return ids, nil
}
