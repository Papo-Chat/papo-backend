package storage

import (
	"context"
	"fmt"

	"papo/internal/models"
)

const attachmentColumns = "id, original_file_name, mime_type, file_path, sha_hash, messages_id, size_bytes, created_by, created_at"

func scanAttachment(row rowScanner) (models.Attachments, error) {
	var attachment models.Attachments
	err := row.Scan(
		&attachment.ID,
		&attachment.OriginalFileName,
		&attachment.MimeType,
		&attachment.FilePath,
		&attachment.ShaHash,
		&attachment.MessagesID,
		&attachment.SizeBytes,
		&attachment.CreatedBy,
		&attachment.CreatedAt,
	)
	if err != nil {
		return models.Attachments{}, err
	}

	return attachment, nil
}

// CreateAttachment insere um novo attachment (upload ainda não vinculado a
// uma mensagem) e retorna o registro criado.
func CreateAttachment(ctx context.Context, a models.Attachments) (models.Attachments, error) {
	row := GetDB().QueryRowContext(ctx,
		"INSERT INTO attachments (original_file_name, mime_type, file_path, sha_hash, size_bytes, created_by) VALUES ($1, $2, $3, $4, $5, $6) RETURNING "+attachmentColumns,
		a.OriginalFileName, a.MimeType, a.FilePath, a.ShaHash, a.SizeBytes, a.CreatedBy,
	)

	attachment, err := scanAttachment(row)
	if err != nil {
		return models.Attachments{}, mapStorageError(err)
	}

	return attachment, nil
}

// ExistsAttachmentByHash indica se já existe um attachment com o sha_hash
// informado. É o sinal de deduplicação do content-addressable storage: se
// existir, o blob do conteúdo já foi gravado em disco e não precisa ser
// reescrito.
func ExistsAttachmentByHash(ctx context.Context, shaHash string) (bool, error) {
	var exists bool
	err := GetDB().QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM attachments WHERE sha_hash = $1)",
		shaHash,
	).Scan(&exists)
	if err != nil {
		return false, mapStorageError(err)
	}

	return exists, nil
}

// GetAttachmentByID busca um attachment pelo id.
func GetAttachmentByID(ctx context.Context, id string) (models.Attachments, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+attachmentColumns+" FROM attachments WHERE id = $1",
		id,
	)

	attachment, err := scanAttachment(row)
	if err != nil {
		return models.Attachments{}, mapStorageError(err)
	}

	return attachment, nil
}

// ListAttachmentsByMessage lista os attachments de uma mensagem ordenados por
// data de criação.
func ListAttachmentsByMessage(ctx context.Context, messageID string) ([]models.Attachments, error) {
	rows, err := GetDB().QueryContext(ctx,
		"SELECT "+attachmentColumns+" FROM attachments WHERE messages_id = $1 ORDER BY created_at, id",
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
