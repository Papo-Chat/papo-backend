// Package worker consome a push_outbox: reserva jobs vencidos, resolve os
// dispositivos do usuário, delega a entrega (FCM direto ou relay) e finaliza os
// jobs (sucesso: DELETE; retry: UPDATE com backoff; limite: descarta).
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"papo/push/internal/config"
	"papo/push/internal/delivery"
	"papo/push/internal/models"
	"papo/push/internal/store"
)

// Worker processa jobs da outbox.
type Worker struct {
	cfg      *config.Config
	store    *store.Store
	delivery delivery.Delivery
}

// New cria um Worker.
func New(cfg *config.Config, s *store.Store, d delivery.Delivery) *Worker {
	return &Worker{
		cfg:      cfg,
		store:    s,
		delivery: d,
	}
}

// Run inicia os workers em goroutines (PUSH_WORKERS). Retorna quando o ctx é
// cancelado (graceful shutdown).
func (w *Worker) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for i := 0; i < w.cfg.PushWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.runLoop(ctx)
		}()
	}
	wg.Wait()
	return nil
}

// runLoop é o loop de um worker: reservar lote → processar cada job → repetir.
func (w *Worker) runLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		jobs, err := w.store.ClaimDueJobs(ctx, w.cfg.PushBatchSize)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			slog.Error("falha ao reservar jobs da outbox", "err", err)
			continue
		}

		for _, job := range jobs {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return
			}
			if err := w.processJob(ctx, job); err != nil {
				slog.Error("falha ao processar job da outbox", "job", job.ID, "err", err)
			}
		}

		if len(jobs) == 0 {
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				return
			}
		}
	}
}

// processJob processa um job: resolve os dispositivos, envia e finaliza
// (delete ou reschedule).
func (w *Worker) processJob(ctx context.Context, job store.PushJob) error {
	// Resolva os dispositivos ativos do usuário.
	devices, err := w.store.ListEnabledPushDevices(ctx, job.UserID)
	if err != nil {
		return err
	}
	// Sem dispositivos ativos: nada para entregar → complete.
	if len(devices) == 0 {
		return w.store.CompletePushJob(ctx, job.ID)
	}

	// Exclui dispositivos já entregues em tentativas anteriores: evita reenviar
	// (e duplicar) a notificação em retry.
	delivered := make(map[string]struct{}, len(job.DeliveredTokens))
	for _, token := range job.DeliveredTokens {
		delivered[token] = struct{}{}
	}

	targets := make([]delivery.Target, 0, len(devices))
	for _, d := range devices {
		if _, ok := delivered[d.Token]; ok {
			continue
		}
		targets = append(targets, delivery.Target{Token: d.Token, Platform: d.Platform})
	}

	// Todos os dispositivos já entregues (ou nenhum não-entregue): completo.
	if len(targets) == 0 {
		return w.store.CompletePushJob(ctx, job.ID)
	}

	// Construa a mensagem a partir do payload.
	var payload models.PushJobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		// Payload inválido → permanente (não há como recuperar).
		return w.rescheduleOrDiscard(ctx, job, delivery.Result{},
			fmt.Errorf("falha ao decodificar o payload do job %s: %w", job.ID, err))
	}
	message := buildMessage(payload, w.cfg.PushPreview)

	res, err := w.delivery.Send(ctx, message, targets)
	if err != nil {
		slog.Error("falha na entrega push", "job", job.ID, "err", err)
	}
	return w.rescheduleOrDiscard(ctx, job, res, err)
}

// rescheduleOrDiscard decide o destino do job: complete (delete), reschedule
// (retry) ou discard (limite de tentativas atingido).
func (w *Worker) rescheduleOrDiscard(ctx context.Context, job store.PushJob, res delivery.Result, err error) error {
	// Persiste os tokens entregues em delivered_tokens: em um retry, o worker
	// não reenvia para dispositivos que já receberam (evita notificação duplicada).
	sent := res.SentTokens()
	if len(sent) > 0 {
		if err2 := w.store.MergeDeliveredTokens(ctx, job.ID, sent); err2 != nil {
			slog.Error("falha ao persistir tokens entregues", "job", job.ID, "err", err2)
		}
	}

	// Desative tokens inválidos (o token permanece desativado para auditoria).
	if invalid := res.InvalidTokens(); len(invalid) > 0 {
		for _, token := range invalid {
			if _, err2 := w.store.DisablePushDeviceByToken(ctx, token); err2 != nil {
				slog.Error("falha ao desativar token inválido", "job", job.ID, "token", maskToken(token), "err", err2)
			}
		}
	}

	// Se a entrega falhou ou houver retryable/permanent, o job não está pronto.
	needsRetry := err != nil || res.HasRetryable() // || res.HasPermanent() não precisamos dar retry num erro permanente
	if !needsRetry {
		// Todos os dispositivos válidos receberam (ou não havia nenhum).
		return w.store.CompletePushJob(ctx, job.ID)
	}

	newAttempts := job.Attempts + 1
	if newAttempts >= w.cfg.PushMaxAttempts {
		slog.Info("limite de tentativas atingido, descartando job", "job", job.ID, "attempts", newAttempts)
		return w.store.CompletePushJob(ctx, job.ID)
	}

	delay := pushBackoffDelay(newAttempts)
	nextAt := time.Now().Add(delay)
	return w.store.ReschedulePushJob(ctx, job.ID, newAttempts, nextAt, buildLastError(res, err))
}

// pushBackoffDelay retorna o delay do backoff para o número de tentativas
// (5s, 30s, 2m, 10m, 30m, 1h, 1h).
func pushBackoffDelay(attempts int) time.Duration {
	var backoff = []time.Duration{
		5 * time.Second,
		30 * time.Second,
		2 * time.Minute,
		10 * time.Minute,
		30 * time.Minute,
		time.Hour,
		time.Hour,
	}
	idx := attempts - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(backoff) {
		idx = len(backoff) - 1
	}
	return backoff[idx]
}

// buildMessage constrói a mensagem de push (título e corpo) a partir do payload
// e da configuração PUSH_PREVIEW (full/generic/hidden).
func buildMessage(payload models.PushJobPayload, preview string) delivery.Message {
	title := payload.Author
	body := ""
	switch preview {
	case "generic":
		body = payload.Author + " enviou uma mensagem"
	case "hidden":
		body = "Nova mensagem"
	default: // full
		body = payload.Author + ": " + payload.Preview
	}
	data := map[string]string{
		"type":       payload.Type,
		"message_id": payload.MessageID,
		"channel_id": payload.ChannelID,
	}
	if payload.NotificationID != nil {
		data["notification_id"] = *payload.NotificationID
	}
	return delivery.Message{
		Title: title,
		Body:  body,
		Data:  data,
	}
}

// buildLastError constrói o resumo do erro para a coluna last_error.
func buildLastError(res delivery.Result, err error) string {
	var parts []string
	if err != nil {
		parts = append(parts, err.Error())
	}
	if res.HasRetryable() {
		parts = append(parts, "retryable_error")
	}
	if res.HasPermanent() {
		parts = append(parts, "permanent_error")
	}
	if len(res.InvalidTokens()) > 0 {
		parts = append(parts, fmt.Sprintf("invalid_tokens=%d", len(res.InvalidTokens())))
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, "; ")
}

// maskToken mascara o token para não expor o valor completo em logs.
func maskToken(token string) string {
	if len(token) <= 8 {
		return "*"
	}
	return token[:4] + "..." + token[len(token)-4:]
}
