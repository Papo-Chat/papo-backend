package storage

import (
	"context"
	"database/sql"
	"errors"

	"papo/internal/models"
)

const mediaColumns = "sha_hash, mime_type, size_bytes, created_at"

func scanMedia(row rowScanner) (models.Media, error) {
	var media models.Media
	err := row.Scan(
		&media.ShaHash,
		&media.MimeType,
		&media.SizeBytes,
		&media.CreatedAt,
	)
	if err != nil {
		return models.Media{}, err
	}

	return media, nil
}

// InsertMediaIfAbsent insere o registro de mídia pelo sha_hash (deduplicação
// content-addressable). Quando o hash já existe, retorna o registro existente
// sem alterar nada. O segundo retorno indica se a inserção foi feita por esta
// chamada (false = hash já existia).
func InsertMediaIfAbsent(ctx context.Context, shaHash, mimeType string, sizeBytes int64) (models.Media, bool, error) {
	row := GetDB().QueryRowContext(ctx,
		"INSERT INTO media (sha_hash, mime_type, size_bytes) VALUES ($1, $2, $3) ON CONFLICT (sha_hash) DO NOTHING RETURNING "+mediaColumns,
		shaHash, mimeType, sizeBytes,
	)

	media, err := scanMedia(row)
	if errors.Is(err, sql.ErrNoRows) {
		existing, getErr := GetMediaByHash(ctx, shaHash)
		if getErr != nil {
			return models.Media{}, false, getErr
		}
		return existing, false, nil
	}
	if err != nil {
		return models.Media{}, false, mapStorageError(err)
	}

	return media, true, nil
}

// GetMediaByHash busca o registro de mídia pelo sha_hash.
// Retorna ErrNotFound quando não existe.
func GetMediaByHash(ctx context.Context, shaHash string) (models.Media, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+mediaColumns+" FROM media WHERE sha_hash = $1",
		shaHash,
	)

	media, err := scanMedia(row)
	if err != nil {
		return models.Media{}, mapStorageError(err)
	}

	return media, nil
}
