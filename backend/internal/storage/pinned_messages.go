package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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
