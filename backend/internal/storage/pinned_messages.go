package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"papo/internal/models"
)

// ErrPinnedLimitReached indica que o canal atingiu o limite de mensagens
// pinadas (100).
var ErrPinnedLimitReached = errors.New("limite de mensagens pinadas atingido")

// maxPinnedPerChannel é o número máximo de mensagens que podem ser fixadas em
// um canal (README: máximo 100).
const maxPinnedPerChannel = 100

// pinnedLockKeyPrefix é o prefixo da chave do advisory lock que serializa as
// fixações por canal, evitando estourar o limite em escrita concorrente.
const pinnedLockKeyPrefix = "papo:pinned:"

const pinnedMessageColumns = "channel_id, message_id, pinned_by, pinned_at"

func scanPinnedMessage(row rowScanner) (models.PinnedMessage, error) {
	var pinned models.PinnedMessage
	err := row.Scan(
		&pinned.ChannelID,
		&pinned.MessageID,
		&pinned.PinnedBy,
		&pinned.PinnedAt,
	)
	if err != nil {
		return models.PinnedMessage{}, err
	}
	return pinned, nil
}

// PinMessage fixa uma mensagem em um canal. A operação é idempotente: se a
// mensagem já estiver pinada, o registro existente é retornado (created=false)
// sem alterar pinned_by/pinned_at. O limite de 100 pin por canal é aplicado
// apenas na criação de um novo pin; a escrita é serializada por um advisory
// lock por canal para evitar estourar o limite em escrita concorrente.
// Retorna ErrPinnedLimitReached quando o canal já tem 100 mensagens pinadas e
// a mensagem ainda não está pinada.
func PinMessage(ctx context.Context, channelID, messageID, pinnedBy string) (models.PinnedMessage, bool, error) {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return models.PinnedMessage{}, false, fmt.Errorf("falha ao fixar mensagem: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		"SELECT pg_advisory_xact_lock(hashtext($1))",
		pinnedLockKeyPrefix+channelID,
	); err != nil {
		return models.PinnedMessage{}, false, fmt.Errorf("falha ao fixar mensagem: %w", err)
	}

	// Idempotência: se já estiver pinada, retorna o registro existente.
	existing, err := scanPinnedMessage(tx.QueryRowContext(ctx,
		"SELECT "+pinnedMessageColumns+" FROM pinned_messages WHERE channel_id = $1 AND message_id = $2",
		channelID, messageID,
	))
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return models.PinnedMessage{}, false, mapStorageError(err)
	}

	var count int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM pinned_messages WHERE channel_id = $1",
		channelID,
	).Scan(&count); err != nil {
		return models.PinnedMessage{}, false, fmt.Errorf("falha ao fixar mensagem: %w", err)
	}
	if count >= maxPinnedPerChannel {
		return models.PinnedMessage{}, false, ErrPinnedLimitReached
	}

	pinned, err := scanPinnedMessage(tx.QueryRowContext(ctx,
		"INSERT INTO pinned_messages (channel_id, message_id, pinned_by) VALUES ($1, $2, $3) RETURNING "+pinnedMessageColumns,
		channelID, messageID, pinnedBy,
	))
	if err != nil {
		return models.PinnedMessage{}, false, mapStorageError(err)
	}

	if err := tx.Commit(); err != nil {
		return models.PinnedMessage{}, false, fmt.Errorf("falha ao fixar mensagem: %w", err)
	}

	return pinned, true, nil
}

// UnpinMessage remove a fixação de uma mensagem em um canal
// (DELETE /channels/:channel_id/messages/:message_id/pin).
// Retorna (false, nil) quando a mensagem não estava pinada.
func UnpinMessage(ctx context.Context, channelID, messageID string) (bool, error) {
	result, err := GetDB().ExecContext(ctx,
		"DELETE FROM pinned_messages WHERE channel_id = $1 AND message_id = $2",
		channelID, messageID,
	)
	if err != nil {
		return false, fmt.Errorf("falha ao remover fixação da mensagem: %w", err)
	}

	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("falha ao remover fixação da mensagem: %w", err)
	}

	return n > 0, nil
}

// ListPinnedMessagesWithAttachmentsByChannel lista as mensagens pinadas de um
// canal com a informação mínima dos seus attachments, na ordem em que foram
// fixadas (pinned_at crescente; o desempate por id torna a ordem estável).
// Mensagens sem attachments aparecem com Attachments vazia.
func ListPinnedMessagesWithAttachmentsByChannel(ctx context.Context, channelID string) ([]models.MessageWithAttachment, error) {
	query := "SELECT p.id, p.channel_id, p.author_id, p.content, p.created_at, p.edited_at, p.reply_to, " +
		"a.id, m.mime_type, a.original_file_name, m.size_bytes, a.created_at " +
		"FROM (SELECT msg.id, msg.channel_id, msg.author_id, msg.content, msg.created_at, msg.edited_at, msg.reply_to, pm.pinned_at " +
		"FROM messages msg " +
		"JOIN pinned_messages pm ON pm.message_id = msg.id AND pm.channel_id = msg.channel_id " +
		"WHERE msg.channel_id = $1) p " +
		"LEFT JOIN attachments a ON a.messages_id = p.id " +
		"LEFT JOIN media m ON m.sha_hash = a.media_sha_hash " +
		"ORDER BY p.pinned_at, p.id, a.created_at, a.id"

	rows, err := GetDB().QueryContext(ctx, query, channelID)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar mensagens pinadas: %w", err)
	}
	defer rows.Close()

	messages := make([]models.MessageWithAttachment, 0)
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
			&message.ReplyTo,
			&attachmentID,
			&mimeType,
			&fileName,
			&sizeBytes,
			&attachmentAt,
		)
		if err != nil {
			return nil, fmt.Errorf("falha ao ler mensagem pinada: %w", err)
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
		return nil, fmt.Errorf("falha ao listar mensagens pinadas: %w", err)
	}
	if hasCurrent {
		messages = append(messages, current)
	}

	return messages, nil
}
