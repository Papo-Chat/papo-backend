package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// claimInterval é o tempo durante o qual um job reservado fica com
// next_attempt_at no futuro (já não é "due") enquanto é processado. Deve ser
// maior que o tempo máximo de processamento de um lote (PUSH_BATCH_SIZE *
// PUSH_REQUEST_TIMEOUT). Em caso de crash do worker, o job só é re-reservado
// após esse período (at-least-once).
const claimInterval = "15 minutes"

// ClaimDueJobs reserva (FOR UPDATE SKIP LOCKED) os jobs vencidos da outbox, em
// ordem de prioridade (mais antigos primeiro). A reserva é atômica: dentro da
// transação os jobs são marcados como reservados (next_attempt_at movido para
// o futuro, claimWindow), impedindo que outros workers os re-reservem durante
// o processamento.
func (s *Store) ClaimDueJobs(ctx context.Context, limit int) ([]PushJob, error) {
	if limit <= 0 {
		limit = s.cfg.PushBatchSize
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	rows, err := tx.QueryContext(ctx,
		"SELECT "+pushOutboxColumns+" "+
			"FROM push_outbox "+
			"WHERE next_attempt_at <= NOW() "+
			"ORDER BY next_attempt_at ASC, id ASC "+
			"FOR UPDATE SKIP LOCKED "+
			"LIMIT $1",
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
		var deliveredRaw sql.NullString
		if err := rows.Scan(
			&job.ID,
			&job.UserID,
			&job.Payload,
			&job.NotificationID,
			&job.Attempts,
			&job.NextAttemptAt,
			&job.CreatedAt,
			&lastError,
			&deliveredRaw,
		); err != nil {
			return nil, fmt.Errorf("falha ao ler job da outbox: %w", err)
		}
		job.LastError = lastError.String
		job.DeliveredTokens = []string{}
		if deliveredRaw.Valid {
			if err := json.Unmarshal([]byte(deliveredRaw.String), &job.DeliveredTokens); err != nil {
				job.DeliveredTokens = []string{}
			}
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao reservar jobs da outbox: %w", err)
	}

	if len(jobs) > 0 {
		ids := make([]any, len(jobs))
		for i, j := range jobs {
			ids[i] = j.ID
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE push_outbox SET next_attempt_at = NOW() + $1::interval WHERE id = ANY($2)",
			claimInterval, ids,
		); err != nil {
			return nil, fmt.Errorf("falha ao marcar os jobs como reservados: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("falha ao commitar a reserva dos jobs: %w", err)
	}
	return jobs, nil
}

// GetPushJob retorna um job da outbox pelo id.
func (s *Store) GetPushJob(ctx context.Context, jobID string) (PushJob, error) {
	var job PushJob
	var lastError sql.NullString
	var deliveredRaw sql.NullString
	if err := s.db.QueryRowContext(ctx,
		"SELECT "+pushOutboxColumns+" FROM push_outbox WHERE id = $1",
		jobID,
	).Scan(
		&job.ID,
		&job.UserID,
		&job.Payload,
		&job.NotificationID,
		&job.Attempts,
		&job.NextAttemptAt,
		&job.CreatedAt,
		&lastError,
		&deliveredRaw,
	); err != nil {
		return PushJob{}, fmt.Errorf("falha ao ler job da outbox: %w", err)
	}
	job.LastError = lastError.String
	job.DeliveredTokens = []string{}
	if deliveredRaw.Valid {
		if err := json.Unmarshal([]byte(deliveredRaw.String), &job.DeliveredTokens); err != nil {
			job.DeliveredTokens = []string{}
		}
	}
	return job, nil
}

// CompletePushJob remove o job da outbox (entrega concluída ou descartado).
func (s *Store) CompletePushJob(ctx context.Context, jobID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM push_outbox WHERE id = $1", jobID)
	if err != nil {
		return fmt.Errorf("falha ao remover o job da outbox: %w", err)
	}
	return nil
}

// ReschedulePushJob reagenda o job com o próximo intervalo (retry), incrementando
// o contador de tentativas.
func (s *Store) ReschedulePushJob(ctx context.Context, jobID string, attempts int, nextAttemptAt time.Time, lastError string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE push_outbox SET attempts = $1, next_attempt_at = $2, last_error = $3 WHERE id = $4",
		attempts, nextAttemptAt, lastError, jobID,
	)
	if err != nil {
		return fmt.Errorf("falha ao reagendar o job da outbox: %w", err)
	}
	return nil
}

// MergeDeliveredTokens adiciona tokens entregues ao job, sem duplicatas, na
// coluna delivered_tokens. É atômico (uma única UPDATE): o job é exclusivo do
// worker que o processou (reserva via FOR UPDATE SKIP LOCKED move
// next_attempt_at para o futuro), então não há disputa entre workers.
// Serve para que, em um retry, o worker não reenvie para dispositivos que já
// receberam (evita notificação duplicada).
func (s *Store) MergeDeliveredTokens(ctx context.Context, jobID string, tokens []string) error {
	if len(tokens) == 0 {
		return nil
	}
	tokensJSON, err := json.Marshal(tokens)
	if err != nil {
		return fmt.Errorf("falha ao serializar delivered_tokens: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		"UPDATE push_outbox "+
			"SET delivered_tokens = ("+
			"  SELECT COALESCE(jsonb_agg(DISTINCT t), '[]'::jsonb) "+
			"  FROM jsonb_array_elements(COALESCE(delivered_tokens, '[]'::jsonb) || $1::jsonb) AS t"+
			") WHERE id = $2",
		tokensJSON, jobID,
	)
	if err != nil {
		return fmt.Errorf("falha ao atualizar delivered_tokens do job: %w", err)
	}
	return nil
}
