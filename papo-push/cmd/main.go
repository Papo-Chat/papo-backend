// Command papo-push é o worker de entrega de push do Papo. Ele consome a
// push_outbox (PostgreSQL), resolve os dispositivos do usuário e envia o push
// via FCM direto (GOOGLE_APPLICATION_CREDENTIALS) ou relay remoto
// (FCM_RELAY_URL + FCM_RELAY_TOKEN). É o único processo responsável pela
// entrega push; o papo-backend só grava jobs na outbox e supervisiona este
// processo.
package main

import (
	"context"
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

	// Delivery: FCM direto quando FCM_RELAY_URL está vazio, senão relay.
	var deliverer delivery.Delivery
	var deliveryMode string
	if cfg.FCMRelayURL == "" {
		d, err := delivery.NewFCM(cfg)
		if err != nil {
			slog.Error("falha ao inicializar a entrega via FCM", "err", err)
			os.Exit(1)
		}
		deliverer = d
		deliveryMode = "fcm"
	} else {
		d, err := delivery.NewRelay(cfg)
		if err != nil {
			slog.Error("falha ao inicializar a entrega via relay", "err", err)
			os.Exit(1)
		}
		deliverer = d
		deliveryMode = "relay"
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
	w := worker.New(cfg, db, deliverer)
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
