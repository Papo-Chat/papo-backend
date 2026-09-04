package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

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

// FindExistingMediaHashes retorna o subconjunto de hashes que já existem na
// tabela media (verificação em lote para o GC de mídia).
func FindExistingMediaHashes(ctx context.Context, hashes []string) (map[string]bool, error) {
	existing := make(map[string]bool)
	const chunkSize = 500
	for start := 0; start < len(hashes); start += chunkSize {
		end := start + chunkSize
		if end > len(hashes) {
			end = len(hashes)
		}

		rows, err := GetDB().QueryContext(ctx,
			"SELECT sha_hash FROM media WHERE sha_hash = ANY($1)",
			hashes[start:end],
		)
		if err != nil {
			return nil, fmt.Errorf("falha ao verificar hashes de mídia: %w", err)
		}
		for rows.Next() {
			var hash string
			if err := rows.Scan(&hash); err != nil {
				rows.Close()
				return nil, fmt.Errorf("falha ao ler hash de mídia: %w", err)
			}
			existing[hash] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("falha ao verificar hashes de mídia: %w", err)
		}
		rows.Close()
	}

	return existing, nil
}

// ListMediaHashesBefore lista os sha_hash criados antes do cutoff (candidatos
// do GC de mídia: rows que podem ter perdido o arquivo ou a referência).
func ListMediaHashesBefore(ctx context.Context, cutoff time.Time) ([]string, error) {
	rows, err := GetDB().QueryContext(ctx,
		"SELECT sha_hash FROM media WHERE created_at < $1",
		cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar mídia antiga: %w", err)
	}
	defer rows.Close()

	hashes := make([]string, 0)
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, fmt.Errorf("falha ao ler hash de mídia: %w", err)
		}
		hashes = append(hashes, hash)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar mídia antiga: %w", err)
	}

	return hashes, nil
}

// MediaIsReferenced indica se o sha_hash é referenciado por qualquer tabela
// (users, servers, emojis, attachments, attachment_thumbnails, link_previews).
func MediaIsReferenced(ctx context.Context, shaHash string) (bool, error) {
	var referenced bool
	err := GetDB().QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE avatar_media = $1 OR banner_media = $1)
		   OR EXISTS (SELECT 1 FROM servers WHERE icon_media = $1)
		   OR EXISTS (SELECT 1 FROM emojis WHERE image_media = $1)
		   OR EXISTS (SELECT 1 FROM attachments WHERE media_sha_hash = $1)
		   OR EXISTS (SELECT 1 FROM attachment_thumbnails WHERE media_sha_hash = $1)
		   OR EXISTS (SELECT 1 FROM link_previews WHERE image_media = $1)`,
		shaHash,
	).Scan(&referenced)
	if err != nil {
		return false, fmt.Errorf("falha ao verificar referências da mídia: %w", err)
	}

	return referenced, nil
}

// DeleteMediaByHash remove o registro de mídia (GC: row sem referência, com
// ou sem arquivo no disco).
func DeleteMediaByHash(ctx context.Context, shaHash string) error {
	if _, err := GetDB().ExecContext(ctx, "DELETE FROM media WHERE sha_hash = $1", shaHash); err != nil {
		return fmt.Errorf("falha ao remover registro de mídia: %w", err)
	}
	return nil
}
