package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"papo/internal/models"
)

const directConversationLockPrefix = "papo:dm:"

func normalizeDirectPair(a, b string) (string, string) {
	if a < b {
		return a, b
	}
	return b, a
}

// CreateOrShowDirectConversation cria o backing channel de uma DM ou reabre
// uma conversa já existente para o usuário que a solicitou.
func CreateOrShowDirectConversation(ctx context.Context, userID, targetID string) (string, bool, error) {
	low, high := normalizeDirectPair(userID, targetID)
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("falha ao abrir transação da DM: %w", err)
	}
	defer tx.Rollback()

	lockKey := directConversationLockPrefix + low + ":" + high
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", lockKey); err != nil {
		return "", false, fmt.Errorf("falha ao bloquear criação da DM: %w", err)
	}

	var channelID string
	err = tx.QueryRowContext(ctx,
		"SELECT channel_id FROM direct_conversations WHERE user_low_id = $1 AND user_high_id = $2",
		low, high,
	).Scan(&channelID)
	if err == nil {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO direct_conversation_state (channel_id, user_id, hidden_at)
			 VALUES ($1, $2, NULL)
			 ON CONFLICT (channel_id, user_id) DO UPDATE SET hidden_at = NULL`,
			channelID, userID,
		); err != nil {
			return "", false, fmt.Errorf("falha ao reabrir DM: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return "", false, fmt.Errorf("falha ao confirmar reabertura da DM: %w", err)
		}
		return channelID, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("falha ao consultar DM existente: %w", err)
	}

	if err := tx.QueryRowContext(ctx,
		`INSERT INTO channels (name, permissions, position, type)
		 VALUES ('dm:' || gen_random_uuid()::text, '{}'::jsonb, 0, 'dm')
		 RETURNING id`,
	).Scan(&channelID); err != nil {
		return "", false, fmt.Errorf("falha ao criar backing channel da DM: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO direct_conversations (channel_id, user_low_id, user_high_id)
		 VALUES ($1, $2, $3)`,
		channelID, low, high,
	); err != nil {
		return "", false, fmt.Errorf("falha ao criar DM: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO direct_conversation_state (channel_id, user_id, hidden_at)
		 VALUES
		   ($1, $2, CASE WHEN $2::uuid = $4::uuid THEN NULL ELSE NOW() END),
		   ($1, $3, CASE WHEN $3::uuid = $4::uuid THEN NULL ELSE NOW() END)`,
		channelID, low, high, userID,
	); err != nil {
		return "", false, fmt.Errorf("falha ao criar estado da DM: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("falha ao confirmar criação da DM: %w", err)
	}
	return channelID, true, nil
}

func directConversationSelect(where string) string {
	return `SELECT
		dc.channel_id, dc.created_at,
		u.id, u.username, u.nickname, u.banned, u.status, u.status_message, u.typing, u.status_updated_at, u.created_at,
		lm.id, lm.content, lm.author_id, au.username, lm.created_at,
		ucs.last_read_message_id, ucs.last_read_at,
		(
			SELECT COUNT(*)
			FROM messages um
			WHERE um.channel_id = dc.channel_id
			  AND um.author_id <> $1
			  AND (ucs.last_read_at IS NULL OR um.created_at > ucs.last_read_at)
		) AS unread_count
	FROM direct_conversations dc
	JOIN users u ON u.id = CASE WHEN dc.user_low_id = $1 THEN dc.user_high_id ELSE dc.user_low_id END
	LEFT JOIN user_channel_state ucs ON ucs.channel_id = dc.channel_id AND ucs.user_id = $1
	LEFT JOIN LATERAL (
		SELECT m.id, m.content, m.author_id, m.created_at
		FROM messages m
		WHERE m.channel_id = dc.channel_id
		ORDER BY m.created_at DESC, m.id DESC
		LIMIT 1
	) lm ON true
	LEFT JOIN users au ON au.id = lm.author_id
	` + where
}

func scanDirectConversation(row rowScanner) (models.DirectConversation, error) {
	var dm models.DirectConversation
	var lastMessageID, lastMessageContent, lastMessageAuthorID, lastMessageAuthorUsername *string
	var lastMessageCreatedAt *time.Time
	var unread int64

	err := row.Scan(
		&dm.ID, &dm.CreatedAt,
		&dm.User.ID, &dm.User.Username, &dm.User.Nickname, &dm.User.Banned,
		&dm.User.Status, &dm.User.StatusMessage, &dm.User.Typing, &dm.User.StatusUpdatedAt, &dm.User.CreatedAt,
		&lastMessageID, &lastMessageContent, &lastMessageAuthorID, &lastMessageAuthorUsername, &lastMessageCreatedAt,
		&dm.LastReadMessage, &dm.LastReadAt, &unread,
	)
	if err != nil {
		return models.DirectConversation{}, err
	}
	dm.UnreadCount = int(unread)
	dm.User.Roles = []models.RoleSummary{}
	if lastMessageID != nil && lastMessageCreatedAt != nil {
		dm.LastMessage = &models.ChannelLastMessage{
			ID:             *lastMessageID,
			Content:        lastMessageContent,
			AuthorID:       lastMessageAuthorID,
			AuthorUsername: lastMessageAuthorUsername,
			CreatedAt:      *lastMessageCreatedAt,
		}
	}
	return dm, nil
}

func ListDirectConversations(ctx context.Context, userID string) ([]models.DirectConversation, error) {
	query := directConversationSelect(`
	JOIN direct_conversation_state dcs ON dcs.channel_id = dc.channel_id AND dcs.user_id = $1
	WHERE (dc.user_low_id = $1 OR dc.user_high_id = $1)
	  AND dcs.hidden_at IS NULL
	  AND NOT EXISTS (
		SELECT 1 FROM user_blocks b
		WHERE (b.user_id = dc.user_low_id AND b.blocked_user_id = dc.user_high_id)
		   OR (b.user_id = dc.user_high_id AND b.blocked_user_id = dc.user_low_id)
	  )
	ORDER BY COALESCE(lm.created_at, dc.created_at) DESC, dc.channel_id DESC`)

	rows, err := GetDB().QueryContext(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar DMs: %w", err)
	}
	defer rows.Close()

	out := make([]models.DirectConversation, 0)
	for rows.Next() {
		dm, err := scanDirectConversation(rows)
		if err != nil {
			return nil, fmt.Errorf("falha ao ler DM: %w", err)
		}
		out = append(out, dm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar DMs: %w", err)
	}
	return out, nil
}

func GetDirectConversation(ctx context.Context, channelID, userID string) (models.DirectConversation, error) {
	row := GetDB().QueryRowContext(ctx,
		directConversationSelect(`WHERE dc.channel_id = $2 AND (dc.user_low_id = $1 OR dc.user_high_id = $1)`),
		userID, channelID,
	)
	dm, err := scanDirectConversation(row)
	if err != nil {
		return models.DirectConversation{}, mapStorageError(err)
	}
	return dm, nil
}

// DirectConversationAccess retorna membership e se existe bloqueio em qualquer
// direção entre os dois participantes.
func DirectConversationAccess(ctx context.Context, channelID, userID string) (bool, bool, error) {
	var member, blocked bool
	err := GetDB().QueryRowContext(ctx, `
		SELECT
			($2::uuid = dc.user_low_id OR $2::uuid = dc.user_high_id) AS member,
			EXISTS (
				SELECT 1 FROM user_blocks b
				WHERE (b.user_id = dc.user_low_id AND b.blocked_user_id = dc.user_high_id)
				   OR (b.user_id = dc.user_high_id AND b.blocked_user_id = dc.user_low_id)
			) AS blocked
		FROM direct_conversations dc
		WHERE dc.channel_id = $1
	`, channelID, userID).Scan(&member, &blocked)
	if err != nil {
		return false, false, mapStorageError(err)
	}
	return member, blocked, nil
}

func GetDirectConversationMembers(ctx context.Context, channelID string) ([]string, error) {
	var low, high string
	err := GetDB().QueryRowContext(ctx,
		"SELECT user_low_id, user_high_id FROM direct_conversations WHERE channel_id = $1",
		channelID,
	).Scan(&low, &high)
	if err != nil {
		return nil, mapStorageError(err)
	}
	return []string{low, high}, nil
}

func DirectConversationReaders(ctx context.Context, channelID string, candidates []string) (map[string]bool, error) {
	allowed := make(map[string]bool, len(candidates))
	members, err := GetDirectConversationMembers(ctx, channelID)
	if err != nil {
		return allowed, err
	}
	blocked, err := UsersBlocked(ctx, members[0], members[1])
	if err != nil {
		return allowed, err
	}
	if blocked {
		return allowed, nil
	}
	memberSet := map[string]bool{members[0]: true, members[1]: true}
	for _, id := range candidates {
		if memberSet[id] {
			allowed[id] = true
		}
	}
	return allowed, nil
}

func HideDirectConversation(ctx context.Context, channelID, userID string) error {
	result, err := GetDB().ExecContext(ctx, `
		UPDATE direct_conversation_state dcs
		SET hidden_at = NOW()
		WHERE dcs.channel_id = $1 AND dcs.user_id = $2
		  AND EXISTS (
			SELECT 1 FROM direct_conversations dc
			WHERE dc.channel_id = dcs.channel_id
			  AND (dc.user_low_id = $2 OR dc.user_high_id = $2)
		  )
	`, channelID, userID)
	if err != nil {
		return fmt.Errorf("falha ao ocultar DM: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ShowDirectConversationForMembers faz uma DM fechada reaparecer quando uma
// nova mensagem é enviada.
func ShowDirectConversationForMembers(ctx context.Context, channelID string) error {
	_, err := GetDB().ExecContext(ctx, `
		UPDATE direct_conversation_state
		SET hidden_at = NULL
		WHERE channel_id = $1
	`, channelID)
	if err != nil {
		return fmt.Errorf("falha ao reabrir DM após mensagem: %w", err)
	}
	return nil
}
