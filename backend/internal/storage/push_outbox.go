package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PushJob é o job de entrega push na outbox (push_outbox).
type PushJob struct {
	ID            string    `json:"id"`
	UserID        string    `json:"user_id"`
	Payload       string    `json:"payload"`
	NotificationID *string  `json:"notification_id"`
	Attempts      int       `json:"attempts"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	CreatedAt     time.Time `json:"created_at"`
	LastError      string    `json:"last_error"`
}

// CreatePushJobInTx grava um job de entrega push na outbox dentro da
// transação tx (mesma transação da notificação, quando houver).
// payload é o JSON já serializado (PushJobPayload).
func CreatePushJobInTx(tx *sql.Tx, ctx context.Context, userID string, notificationID *string, payload string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO push_outbox (user_id, notification_id, payload)
		 VALUES ($1, $2, $3)`,
		userID, notificationID, payload,
	)
	if err != nil {
		return fmt.Errorf("falha ao criar o job de push: %w", err)
	}

	return nil
}

// CreatePushJob grava um job de entrega push na outbox fora de transação
// (usado para eventos efêmeros, sem row de notificação).
func CreatePushJob(ctx context.Context, userID string, notificationID *string, payload string) error {
	_, err := GetDB().ExecContext(ctx,
		`INSERT INTO push_outbox (user_id, notification_id, payload)
		 VALUES ($1, $2, $3)`,
		userID, notificationID, payload,
	)
	if err != nil {
		return fmt.Errorf("falha ao criar o job de push: %w", err)
	}

	return nil
}

// ClaimDueJobs reserva (FOR UPDATE SKIP LOCKED) os jobs vencidos da outbox,
// em ordem de prioridade (mais antigos primeiro). Os jobs permanecem na
// tabela até serem finalizados (sucesso: DELETE; retry: UPDATE) — uma morte
// abrupta do worker não perde jobs.
func ClaimDueJobs(ctx context.Context, limit int) ([]PushJob, error) {
	if limit <= 0 {
		limit = 50
	}

	rows, err := GetDB().QueryContext(ctx,
		`SELECT id, user_id, payload, notification_id, attempts, next_attempt_at, created_at, last_error
		 FROM push_outbox
		 WHERE next_attempt_at <= NOW()
		 FOR UPDATE SKIP LOCKED
		 ORDER BY next_attempt_at ASC, id ASC
		 LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao reservar jobs da outbox: %w", err)
	}
	defer rows.Close()

	jobs := make([]PushJob, 0, limit)
	for rows.Next() {
		var job PushJob
		var lastError sql.NullString
		if err := rows.Scan(
			&job.ID,
			&job.UserID,
			&job.Payload,
			&job.NotificationID,
			&job.Attempts,
			&job.NextAttemptAt,
			&job.CreatedAt,
			&lastError,
		); err != nil {
			return nil, fmt.Errorf("falha ao ler job da outbox: %w", err)
		}
		job.LastError = lastError.String
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao reservar jobs da outbox: %w", err)
	}

	return jobs, nil
}

// CompletePushJob remove o job da outbox (entrega concluída).
func CompletePushJob(ctx context.Context, jobID string) error {
	_, err := GetDB().ExecContext(ctx,
		"DELETE FROM push_outbox WHERE id = $1",
		jobID,
	)
	if err != nil {
		return fmt.Errorf("falha ao remover o job da outbox: %w", err)
	}

	return nil
}

// ReschedulePushJob reagenda o job com o próximo intervalo (retry).
func ReschedulePushJob(ctx context.Context, jobID string, attempts int, nextAttemptAt time.Time, lastError string) error {
	_, err := GetDB().ExecContext(ctx,
		`UPDATE push_outbox
		 SET attempts = $1, next_attempt_at = $2, last_error = $3
		 WHERE id = $4`,
		attempts, nextAttemptAt, lastError, jobID,
	)
	if err != nil {
		return fmt.Errorf("falha ao reagendar o job da outbox: %w", err)
	}

	return nil
}
