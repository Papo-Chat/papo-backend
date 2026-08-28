package storage

import (
	"context"
	"fmt"

	"papo/internal/models"
)

// TouchLastReadMessage atualiza o último read do usuário no canal
// (user_channel_state) para a mensagem informada. A atualização só avança
// quando a mensagem informada é mais nova que a última lida (comparação por
// (created_at, id), a mesma ordem da listagem de mensagens); quando a
// mensagem armazenada foi excluída, o valor avança (COALESCE para epoch).
// A linha é criada quando não existe.
func TouchLastReadMessage(ctx context.Context, userID, channelID string, message models.Message) error {
	_, err := GetDB().ExecContext(ctx,
		`INSERT INTO user_channel_state (user_id, channel_id, last_read_message_id, last_read_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (user_id, channel_id) DO UPDATE
		 SET last_read_message_id = EXCLUDED.last_read_message_id,
		     last_read_at = EXCLUDED.last_read_at
		 WHERE COALESCE(
		         (SELECT (m.created_at, m.id) FROM messages m WHERE m.id = user_channel_state.last_read_message_id),
		         ('epoch'::timestamptz, '00000000-0000-0000-0000-000000000000'::uuid)
 		     ) < ($4::timestamptz, $3::uuid)`,
		userID, channelID, message.ID, message.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("falha ao atualizar o último read do usuário no canal: %w", err)
	}

	return nil
}

// GetLastReadMessage busca o último read do usuário no canal.
// Retorna ErrNotFound quando não existe.
func GetLastReadMessage(ctx context.Context, userID, channelID string) (models.UserChannelState, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT user_id, channel_id, last_read_message_id, last_read_at FROM user_channel_state WHERE user_id = $1 AND channel_id = $2",
		userID, channelID,
	)

	var state models.UserChannelState
	err := row.Scan(
		&state.UserID,
		&state.ChannelID,
		&state.LastReadMessageID,
		&state.LastReadAt,
	)
	if err != nil {
		return models.UserChannelState{}, mapStorageError(err)
	}

	return state, nil
}
