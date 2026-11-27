// Package worker consome a push_outbox: reserva jobs vencidos, resolve os
// dispositivos do usuário, delega a entrega (FCM direto ou relay) e finaliza os
// jobs (sucesso: DELETE; retry: UPDATE com backoff; limite: descarta). Com
// PUSH_PROVIDERS, cada dispositivo é entregue pelo provider em que foi
// registrado.
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

// jobStore é o que o worker usa da outbox e dos dispositivos (*store.Store; os
// testes usam um fake).
type jobStore interface {
	ClaimDueJobs(ctx context.Context, limit int) ([]store.PushJob, error)
	ListEnabledPushDevices(ctx context.Context, userID string) ([]store.PushDevice, error)
	CompletePushJob(ctx context.Context, jobID string) error
	ReschedulePushJob(ctx context.Context, jobID string, attempts int, nextAttemptAt time.Time, lastError string) error
	MergeDeliveredTokens(ctx context.Context, jobID string, tokens []string) error
	DisablePushDeviceByToken(ctx context.Context, token string) (int64, error)
}

// Worker processa jobs da outbox.
type Worker struct {
	cfg   *config.Config
	store jobStore
	// deliverers mapeia o nome do provider à sua entrega. No modo legado
	// (cfg.LegacyProviders) tem só config.LegacyProviderName e recebe todos os
	// dispositivos.
	deliverers map[string]delivery.Delivery
}

// New cria um Worker.
func New(cfg *config.Config, s jobStore, deliverers map[string]delivery.Delivery) *Worker {
	return &Worker{
		cfg:        cfg,
		store:      s,
		deliverers: deliverers,
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

	// Agrupa os alvos por provider. Dispositivos sem provider configurado são
	// ignorados (não é falha nem motivo de retry): o servidor não consegue
	// enviar a eles.
	groups, skipped := w.groupTargets(devices, delivered)
	if skipped > 0 {
		slog.Info("dispositivos ignorados: provider de push não configurado", "job", job.ID, "devices", skipped)
	}

	// Todos os dispositivos já entregues ou ignorados: completo.
	if len(groups) == 0 {
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

	res, err := w.sendGroups(ctx, job.ID, message, groups)
	return w.rescheduleOrDiscard(ctx, job, res, err)
}

// providerGroup são os alvos de um provider.
type providerGroup struct {
	provider string
	targets  []delivery.Target
}

// groupTargets separa os dispositivos ainda não entregues por provider, na
// ordem em que cada provider aparece. skipped conta os dispositivos sem
// provider configurado. No modo legado todos vão ao provider único.
func (w *Worker) groupTargets(devices []store.PushDevice, delivered map[string]struct{}) (groups []providerGroup, skipped int) {
	index := make(map[string]int)
	for _, d := range devices {
		if _, ok := delivered[d.Token]; ok {
			continue
		}
		provider := d.Provider
		if w.cfg.LegacyProviders() {
			provider = config.LegacyProviderName
		}
		if _, ok := w.deliverers[provider]; !ok {
			skipped++
			continue
		}
		i, ok := index[provider]
		if !ok {
			i = len(groups)
			index[provider] = i
			groups = append(groups, providerGroup{provider: provider})
		}
		groups[i].targets = append(groups[i].targets, delivery.Target{Token: d.Token, Platform: d.Platform})
	}
	return groups, skipped
}

// sendGroups envia cada grupo pelo seu provider e junta os resultados. Um
// erro em um grupo não impede o envio dos demais; os erros são unidos.
func (w *Worker) sendGroups(ctx context.Context, jobID string, message delivery.Message, groups []providerGroup) (delivery.Result, error) {
	results := make([]delivery.Result, 0, len(groups))
	var errs []error
	for _, g := range groups {
		res, err := w.deliverers[g.provider].Send(ctx, message, g.targets)
		if err != nil {
			slog.Error("falha na entrega push", "job", jobID, "provider", g.provider, "err", err)
			errs = append(errs, fmt.Errorf("provider %s: %w", g.provider, err))
		}
		results = append(results, res)
	}
	return delivery.MergeResults(results...), errors.Join(errs...)
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
