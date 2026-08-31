package storage

import (
	"context"
	"fmt"
	"time"

	"papo/internal/models"
)

const messageColumns = "id, channel_id, author_id, content, created_at, edited_at, reply_to"

func scanMessage(row rowScanner) (models.Message, error) {
	var message models.Message
	err := row.Scan(
		&message.ID,
		&message.ChannelID,
		&message.AuthorID,
		&message.Content,
		&message.CreatedAt,
		&message.EditedAt,
		&message.ReplyTo,
	)
	if err != nil {
		return models.Message{}, err
	}

	return message, nil
}

// CreateMessage cria uma nova mensagem e retorna o registro criado.
// Content vazio é gravado como NULL (a mensagem pode ter apenas
// attachments). ReplyTo vazio é gravado como NULL (sem referência); a
// validação de existência/mesmo canal da mensagem referenciada é feita na
// camada de serviço.
// Se attachmentIDs não for vazio, os attachments (já inseridos na etapa
// anterior do fluxo de criação de mensagem) são vinculados à mensagem na
// mesma transação do INSERT.
func CreateMessage(ctx context.Context, channelID, authorID, content, replyTo string, attachmentIDs []string) (models.Message, error) {
	const insertMessage = "INSERT INTO messages (channel_id, author_id, content, reply_to) VALUES ($1, $2, $3, $4) RETURNING " + messageColumns

	// content vazio vira NULL na coluna (nullable)
	var contentArg any
	if content != "" {
		contentArg = content
	}

	// replyTo vazio vira NULL (sem referência)
	var replyToArg any
	if replyTo != "" {
		replyToArg = replyTo
	}

	if len(attachmentIDs) == 0 {
		message, err := scanMessage(GetDB().QueryRowContext(ctx, insertMessage, channelID, authorID, contentArg, replyToArg))
		if err != nil {
			return models.Message{}, mapStorageError(err)
		}
		return message, nil
	}

	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return models.Message{}, fmt.Errorf("falha ao criar mensagem: %w", err)
	}
	defer tx.Rollback()

	message, err := scanMessage(tx.QueryRowContext(ctx, insertMessage, channelID, authorID, contentArg, replyToArg))
	if err != nil {
		return models.Message{}, mapStorageError(err)
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE attachments SET messages_id = $1 WHERE id = ANY($2)",
		message.ID, attachmentIDs,
	); err != nil {
		return models.Message{}, fmt.Errorf("falha ao vincular attachments à mensagem: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return models.Message{}, fmt.Errorf("falha ao criar mensagem: %w", err)
	}

	return message, nil
}

// GetMessageByID busca uma mensagem pelo id.
func GetMessageByID(ctx context.Context, id string) (models.Message, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+messageColumns+" FROM messages WHERE id = $1",
		id,
	)

	message, err := scanMessage(row)
	if err != nil {
		return models.Message{}, mapStorageError(err)
	}

	return message, nil
}

// ListMessagesByChannel lista as mensagens de um canal ordenadas por data de criação.
// Se since for fornecido, retorna apenas mensagens criadas após esse timestamp;
// se lastID for fornecido junto, o cursor é o par (created_at, id) e o filtro
// inclui mensagens do mesmo timestamp com id menor que lastID (evita pular
// mensagens com timestamp igual).
// limit usado para paginação via Cursor
func ListMessagesByChannel(ctx context.Context, channelID string, since *time.Time, lastID string, limit *int) ([]models.Message, error) {
	query := "SELECT " + messageColumns + " FROM messages WHERE channel_id = $1"
	args := []any{channelID}

	//Limite máximo de mensagens é 100
	var lim = 100
	if limit != nil && *limit > 0 && *limit <= 100 {
		lim = *limit
	}

	if since != nil {
		if lastID != "" {
			query += " AND (created_at > $2 OR (created_at = $2 AND id < $3))"
			args = append(args, *since, lastID)
			query += " ORDER BY created_at DESC, id DESC LIMIT $4"
		} else {
			query += " AND created_at > $2"
			args = append(args, *since)
			query += " ORDER BY created_at DESC, id DESC LIMIT $3"
		}
	} else {
		query += " ORDER BY created_at DESC, id DESC LIMIT $2"
	}
	args = append(args, lim)

	rows, err := GetDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar mensagens: %w", err)
	}
	defer rows.Close()

	messages := make([]models.Message, 0)
	for rows.Next() {
		message, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("falha ao ler mensagem: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar mensagens: %w", err)
	}

	return messages, nil
}

// ListMessagesWithAttachmentsByChannel lista as mensagens de um canal com a
// informação mínima dos seus attachments, ordenadas por data de criação em
// ordem decrescente.
// Se since for fornecido, retorna apenas mensagens criadas após esse
// timestamp; se lastID for fornecido junto, o cursor é o par (created_at, id)
// e o filtro inclui mensagens do mesmo timestamp com id menor que lastID
// (evita pular mensagens com timestamp igual).
// limit é o número máximo de mensagens (padrão e máximo 100); para permitir
// que o chamador determine has_more, é buscada uma mensagem a mais que o
// limite. O LIMIT é aplicado antes do join com attachments, então o limite
// conta mensagens (e não linhas de attachments).
func ListMessagesWithAttachmentsByChannel(ctx context.Context, channelID string, since *time.Time, lastID string, limit int) ([]models.MessageWithAttachment, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	fetch := limit + 1

	query := "SELECT p.id, p.channel_id, p.author_id, p.content, p.created_at, p.edited_at, " +
		"a.id, m.mime_type, a.original_file_name, m.size_bytes, a.created_at " +
		"FROM (SELECT " + messageColumns + " FROM messages WHERE channel_id = $1"
	args := []any{channelID}

	if since != nil {
		if lastID != "" {
			query += " AND (created_at > $2 OR (created_at = $2 AND id < $3))"
			args = append(args, *since, lastID)
			query += " ORDER BY created_at DESC, id DESC LIMIT $4"
		} else {
			query += " AND created_at > $2"
			args = append(args, *since)
			query += " ORDER BY created_at DESC, id DESC LIMIT $3"
		}
	} else {
		query += " ORDER BY created_at DESC, id DESC LIMIT $2"
	}
	args = append(args, fetch)

	query += ") p LEFT JOIN attachments a ON a.messages_id = p.id " +
		"LEFT JOIN media m ON m.sha_hash = a.media_sha_hash " +
		"ORDER BY p.created_at DESC, p.id DESC, a.created_at, a.id"

	rows, err := GetDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar mensagens: %w", err)
	}
	defer rows.Close()

	messages := make([]models.MessageWithAttachment, 0, fetch)
	var current models.MessageWithAttachment
	var hasCurrent bool
	for rows.Next() {
		var (
			message      models.Message
			attachmentID *string
			mimeType     *string
			fileName     *string
			sizeBytes    *int64
			attachmentAt *time.Time
		)
		err := rows.Scan(
			&message.ID,
			&message.ChannelID,
			&message.AuthorID,
			&message.Content,
			&message.CreatedAt,
			&message.EditedAt,
			&attachmentID,
			&mimeType,
			&fileName,
			&sizeBytes,
			&attachmentAt,
		)
		if err != nil {
			return nil, fmt.Errorf("falha ao ler mensagem: %w", err)
		}

		if !hasCurrent || current.Message.ID != message.ID {
			if hasCurrent {
				messages = append(messages, current)
			}
			current = models.MessageWithAttachment{
				Message:     message,
				Attachments: make([]models.MessageAttachment, 0),
			}
			hasCurrent = true
		}

		if attachmentID != nil {
			current.Attachments = append(current.Attachments, models.MessageAttachment{
				ID:               *attachmentID,
				MimeType:         *mimeType,
				OriginalFileName: *fileName,
				SizeBytes:        *sizeBytes,
				CreatedAt:        *attachmentAt,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar mensagens: %w", err)
	}
	if hasCurrent {
		messages = append(messages, current)
	}

	return messages, nil
}

// UpdateMessage atualiza o conteúdo de uma mensagem e define edited_at
// com o tempo atual do banco. Retorna o registro atualizado.
func UpdateMessage(ctx context.Context, id string, message models.Message) (models.Message, error) {
	row := GetDB().QueryRowContext(ctx,
		`UPDATE messages
		 SET content = $2, edited_at = NOW()
		 WHERE id = $1
		 RETURNING `+messageColumns,
		id, message.Content,
	)

	updated, err := scanMessage(row)
	if err != nil {
		return models.Message{}, mapStorageError(err)
	}

	return updated, nil
}

// DeleteMessage exclui uma mensagem pelo id.
func DeleteMessage(ctx context.Context, id string) error {
	result, err := GetDB().ExecContext(ctx,
		"DELETE FROM messages WHERE id = $1",
		id,
	)
	if err != nil {
		return fmt.Errorf("falha ao excluir mensagem: %w", err)
	}

	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	return nil
}
