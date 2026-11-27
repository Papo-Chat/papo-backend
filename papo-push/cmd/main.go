// Command papo-push é o worker de entrega de push do Papo. Ele consome a
// push_outbox (PostgreSQL), resolve os dispositivos do usuário e envia o push
// via FCM direto (GOOGLE_APPLICATION_CREDENTIALS) ou relay remoto
// (FCM_RELAY_URL + FCM_RELAY_TOKEN); com PUSH_PROVIDERS, cada provider tem a
// sua credencial ou relay (PUSH_PROVIDER_<NOME>_*). É o único processo responsável pela
// entrega push; o papo-backend só grava jobs na outbox e supervisiona este
// processo.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"papo/push/internal/config"
	"papo/push/internal/delivery"
	"papo/push/internal/health"
	"papo/push/internal/store"
	"papo/push/internal/worker"
)

func main() {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		slog.Error("falha ao carregar a configuração do papo-push", "err", err)
		os.Exit(1)
	}

	// Conexão com o PostgreSQL (mesmo banco do backend).
	db, err := store.New(cfg)
	if err != nil {
		slog.Error("falha ao conectar ao PostgreSQL", "err", err)
		os.Exit(1)
	}
	defer func() {
		if err := db.Close(); err != nil {
			slog.Error("falha ao fechar a conexão com o PostgreSQL", "err", err)
		}
	}()

	// Delivery: um por provider. Sem PUSH_PROVIDERS há só o provider legado
	// "fcm" (FCM direto ou relay, conforme FCM_RELAY_URL).
	deliverers, deliveryMode, err := buildDeliverers(cfg)
	if err != nil {
		slog.Error("falha ao inicializar a entrega push", "err", err)
		os.Exit(1)
	}

	// Health endpoint (127.0.0.1:PUSH_HEALTH_PORT) — usado pelo supervisor.
	healthServer := health.New(cfg, db.Ping, deliveryMode)
	go func() {
		slog.Info("healthz ouvindo em 127.0.0.1", "port", cfg.PushHealthPort, "delivery", deliveryMode)
		if err := healthServer.Start(); err != nil {
			slog.Error("falha no endpoint de health", "err", err)
		}
	}()

	// Worker: consome a outbox até o shutdown.
	w := worker.New(cfg, db, deliverers)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("papo-push iniciado",
		"workers", cfg.PushWorkers,
		"batch_size", cfg.PushBatchSize,
		"max_attempts", cfg.PushMaxAttempts,
		"delivery", deliveryMode,
	)
	if err := w.Run(ctx); err != nil {
		slog.Error("falha no worker de push", "err", err)
	}
	slog.Info("papo-push encerrado")
}

// buildDeliverers cria a entrega de cada provider configurado e devolve o modo
// para o health: "fcm" ou "relay" no modo legado; "providers" com PUSH_PROVIDERS.
func buildDeliverers(cfg *config.Config) (map[string]delivery.Delivery, string, error) {
	providers := cfg.ResolvedProviders()
	deliverers := make(map[string]delivery.Delivery, len(providers))
	for _, p := range providers {
		var d delivery.Delivery
		var err error
		if p.IsRelay() {
			d, err = delivery.NewRelay(p.RelayURL, p.RelayToken, cfg.PushRequestTimeout)
		} else {
			d, err = delivery.NewFCM(p.Credentials)
		}
		if err != nil {
			return nil, "", fmt.Errorf("provider %s: %w", p.Name, err)
		}
		deliverers[p.Name] = d
		slog.Info("provider de push configurado", "provider", p.Name, "relay", p.IsRelay())
	}
	if !cfg.LegacyProviders() {
		return deliverers, "providers", nil
	}
	if providers[0].IsRelay() {
		return deliverers, "relay", nil
	}
	return deliverers, "fcm", nil
}
