package storage

import (
	"context"
	"fmt"

	"papo/internal/models"
)

func UsersBlocked(ctx context.Context, a, b string) (bool, error) {
	var blocked bool
	err := GetDB().QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM user_blocks
			WHERE (user_id = $1 AND blocked_user_id = $2)
			   OR (user_id = $2 AND blocked_user_id = $1)
		)
	`, a, b).Scan(&blocked)
	if err != nil {
		return false, fmt.Errorf("falha ao verificar bloqueio entre usuários: %w", err)
	}
	return blocked, nil
}

// CreateUserBlock é idempotente. Ao bloquear, qualquer DM existente entre os
// usuários é escondida para ambos, sem apagar histórico.
func CreateUserBlock(ctx context.Context, userID, blockedUserID string) error {
	low, high := normalizeDirectPair(userID, blockedUserID)
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("falha ao abrir transação de bloqueio: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_blocks (user_id, blocked_user_id)
		VALUES ($1, $2)
		ON CONFLICT (user_id, blocked_user_id) DO NOTHING
	`, userID, blockedUserID); err != nil {
		return fmt.Errorf("falha ao bloquear usuário: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE direct_conversation_state
		SET hidden_at = NOW()
		WHERE channel_id IN (
			SELECT channel_id FROM direct_conversations
			WHERE user_low_id = $1 AND user_high_id = $2
		)
		  AND user_id IN ($1, $2)
	`, low, high); err != nil {
		return fmt.Errorf("falha ao ocultar DM durante bloqueio: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("falha ao confirmar bloqueio: %w", err)
	}
	return nil
}

func DeleteUserBlock(ctx context.Context, userID, blockedUserID string) error {
	_, err := GetDB().ExecContext(ctx,
		"DELETE FROM user_blocks WHERE user_id = $1 AND blocked_user_id = $2",
		userID, blockedUserID,
	)
	if err != nil {
		return fmt.Errorf("falha ao desbloquear usuário: %w", err)
	}
	return nil
}

func ListBlockedUsers(ctx context.Context, userID string) ([]models.UserSummary, error) {
	rows, err := GetDB().QueryContext(ctx, `
		SELECT `+userSummaryColumns+`
		FROM user_blocks b
		JOIN users u ON u.id = b.blocked_user_id
		WHERE b.user_id = $1
		ORDER BY b.created_at DESC, u.id
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar usuários bloqueados: %w", err)
	}
	defer rows.Close()

	users := make([]models.UserSummary, 0)
	for rows.Next() {
		user, err := scanUserSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("falha ao ler usuário bloqueado: %w", err)
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar usuários bloqueados: %w", err)
	}
	return users, nil
}
