package storage

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"papo/internal/models"
)

const emojiColumns = "id, server_id, name, format, image_blob, created_by, created_at"

func scanEmoji(row rowScanner) (models.Emoji, error) {
	var emoji models.Emoji
	err := row.Scan(
		&emoji.ID,
		&emoji.ServerID,
		&emoji.Name,
		&emoji.Format,
		&emoji.ImageBlob,
		&emoji.CreatedBy,
		&emoji.CreatedAt,
	)
	if err != nil {
		return models.Emoji{}, err
	}

	return emoji, nil
}

// CreateEmoji cria um novo emoji e retorna o registro criado.
// Nomes duplicados retornam ErrUniqueViolation.
func CreateEmoji(ctx context.Context, serverID, name, format string, imageBlob []byte, createdBy *string) (models.Emoji, error) {
	row := GetDB().QueryRowContext(ctx,
		"INSERT INTO emojis (server_id, name, format, image_blob, created_by) VALUES ($1, $2, $3, $4, $5) RETURNING "+emojiColumns,
		serverID, name, format, imageBlob, createdBy,
	)

	emoji, err := scanEmoji(row)
	if err != nil {
		return models.Emoji{}, mapStorageError(err)
	}

	return emoji, nil
}

// GetEmojiByID busca um emoji pelo id.
func GetEmojiByID(ctx context.Context, id string) (models.Emoji, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+emojiColumns+" FROM emojis WHERE id = $1",
		id,
	)

	emoji, err := scanEmoji(row)
	if err != nil {
		return models.Emoji{}, mapStorageError(err)
	}

	return emoji, nil
}

// ListEmojis lista os emojis ordenados por data de criação.
// serverID é opcional (filtro por servidor).
// Se since for fornecido, retorna apenas emojis criados após esse timestamp;
// se lastID for fornecido junto, o cursor é o par (created_at, id) e o filtro
// inclui emojis criados no mesmo timestamp com id maior que lastID (evita
// pular emojis com timestamp igual).
// Se limit for > 0, retorna no máximo limit emojis.
func ListEmojis(ctx context.Context, serverID *string, since *time.Time, lastID string, limit int) ([]models.Emoji, error) {
	query := "SELECT " + emojiColumns + " FROM emojis"
	args := []any{}
	where := ""
	if serverID != nil && *serverID != "" {
		where = "server_id = $" + strconv.Itoa(len(args)+1)
		args = append(args, *serverID)
	}
	if since != nil {
		var cond string
		if lastID != "" {
			cond = "(created_at > $" + strconv.Itoa(len(args)+1) +
				" OR (created_at = $" + strconv.Itoa(len(args)+1) +
				" AND id > $" + strconv.Itoa(len(args)+2) + "))"
			args = append(args, *since, lastID)
		} else {
			cond = "created_at > $" + strconv.Itoa(len(args)+1)
			args = append(args, *since)
		}
		if where != "" {
			where += " AND "
		}
		where += cond
	}
	if where != "" {
		query += " WHERE " + where
	}
	query += " ORDER BY created_at, id"
	if limit > 0 {
		query += " LIMIT $" + strconv.Itoa(len(args)+1)
		args = append(args, limit)
	}

	rows, err := GetDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar emojis: %w", err)
	}
	defer rows.Close()

	emojis := make([]models.Emoji, 0)
	for rows.Next() {
		emoji, err := scanEmoji(rows)
		if err != nil {
			return nil, fmt.Errorf("falha ao ler emoji: %w", err)
		}
		emojis = append(emojis, emoji)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar emojis: %w", err)
	}

	return emojis, nil
}

// ListEmojisByServer lista os emojis de um servidor ordenados por data de criação.
func ListEmojisByServer(ctx context.Context, serverID string) ([]models.Emoji, error) {
	return ListEmojis(ctx, &serverID, nil, "", 0)
}

// CountEmojisByServer conta os emojis de um servidor.
func CountEmojisByServer(ctx context.Context, serverID string) (int, error) {
	var count int
	err := GetDB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM emojis WHERE server_id = $1",
		serverID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("falha ao contar emojis: %w", err)
	}

	return count, nil
}

// DeleteEmoji exclui um emoji pelo id.
func DeleteEmoji(ctx context.Context, id string) error {
	result, err := GetDB().ExecContext(ctx,
		"DELETE FROM emojis WHERE id = $1",
		id,
	)
	if err != nil {
		return fmt.Errorf("falha ao excluir emoji: %w", err)
	}

	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	return nil
}
