package storage

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"papo/internal/models"
)

// emojiColumns inclui o mime_type da tabela media (join) para o service
// resolver o formato da imagem.
const emojiColumns = "e.id, e.server_id, e.name, e.image_media, m.mime_type, e.created_by, e.created_at"
const emojiFrom = "FROM emojis e JOIN media m ON m.sha_hash = e.image_media"

func scanEmoji(row rowScanner) (models.Emoji, error) {
	var emoji models.Emoji
	err := row.Scan(
		&emoji.ID,
		&emoji.ServerID,
		&emoji.Name,
		&emoji.ImageMedia,
		&emoji.MimeType,
		&emoji.CreatedBy,
		&emoji.CreatedAt,
	)
	if err != nil {
		return models.Emoji{}, err
	}

	return emoji, nil
}

// CreateEmoji cria um novo emoji e retorna o registro criado.
// imageMedia é a referência do blob da imagem na tabela media.
// Nomes duplicados retornam ErrUniqueViolation.
func CreateEmoji(ctx context.Context, serverID, name, imageMedia string, createdBy *string) (models.Emoji, error) {
	// O SELECT lê o resultado do RETURNING (não a tabela emojis): a query
	// principal e o CTE de dados compartilham o mesmo snapshot, então a
	// linha inserida ainda não seria visível na tabela.
	row := GetDB().QueryRowContext(ctx,
		`WITH inserted AS (
			INSERT INTO emojis (server_id, name, image_media, created_by) VALUES ($1, $2, $3, $4)
			RETURNING id, server_id, name, image_media, created_by, created_at
		 )
		 SELECT i.id, i.server_id, i.name, i.image_media, m.mime_type, i.created_by, i.created_at
		 FROM inserted i JOIN media m ON m.sha_hash = i.image_media`,
		serverID, name, imageMedia, createdBy,
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
		"SELECT "+emojiColumns+" "+emojiFrom+" WHERE e.id = $1",
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
	query := "SELECT " + emojiColumns + " " + emojiFrom
	args := []any{}
	where := ""
	if serverID != nil && *serverID != "" {
		where = "e.server_id = $" + strconv.Itoa(len(args)+1)
		args = append(args, *serverID)
	}
	if since != nil {
		var cond string
		if lastID != "" {
			cond = "(e.created_at > $" + strconv.Itoa(len(args)+1) +
				" OR (e.created_at = $" + strconv.Itoa(len(args)+1) +
				" AND e.id > $" + strconv.Itoa(len(args)+2) + "))"
			args = append(args, *since, lastID)
		} else {
			cond = "e.created_at > $" + strconv.Itoa(len(args)+1)
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
	query += " ORDER BY e.created_at, e.id"
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
