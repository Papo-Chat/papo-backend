package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CreatePasswordResetToken invalida links ainda ativos do usuário e persiste
// apenas o SHA-256 do novo token.
func CreatePasswordResetToken(ctx context.Context, userID, createdBy, tokenHash string, expiresAt time.Time) error {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("falha ao iniciar reset de senha: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		"UPDATE password_reset_tokens SET used_at = now() WHERE user_id = $1 AND used_at IS NULL",
		userID,
	); err != nil {
		return fmt.Errorf("falha ao invalidar resets anteriores: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		"INSERT INTO password_reset_tokens (token_hash, user_id, created_by, expires_at) VALUES ($1, $2, $3, $4)",
		tokenHash, userID, createdBy, expiresAt,
	); err != nil {
		return fmt.Errorf("falha ao criar reset de senha: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("falha ao confirmar reset de senha: %w", err)
	}
	return nil
}

// ConsumePasswordResetToken usa o token uma única vez, troca a senha e revoga
// todas as sessões do usuário dentro da mesma transação.
func ConsumePasswordResetToken(ctx context.Context, tokenHash, passwordHash string) (string, error) {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("falha ao consumir reset de senha: %w", err)
	}
	defer tx.Rollback()

	var userID string
	err = tx.QueryRowContext(ctx,
		"UPDATE password_reset_tokens SET used_at = now() WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now() RETURNING user_id",
		tokenHash,
	).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("falha ao validar reset de senha: %w", err)
	}

	result, err := tx.ExecContext(ctx,
		"UPDATE users SET password_hash = $2, reset_password = FALSE, connection_violation = FALSE WHERE id = $1",
		userID, passwordHash,
	)
	if err != nil {
		return "", fmt.Errorf("falha ao atualizar senha: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return "", ErrNotFound
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE user_connections SET replaced_at = now() WHERE user_id = $1 AND replaced_at IS NULL",
		userID,
	); err != nil {
		return "", fmt.Errorf("falha ao revogar sessões: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE password_reset_tokens SET used_at = COALESCE(used_at, now()) WHERE user_id = $1",
		userID,
	); err != nil {
		return "", fmt.Errorf("falha ao invalidar resets restantes: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("falha ao confirmar troca de senha: %w", err)
	}
	return userID, nil
}
