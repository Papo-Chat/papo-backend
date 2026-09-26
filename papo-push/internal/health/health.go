// Package health expõe o endpoint local /healthz (somente em 127.0.0.1) usado
// pelo supervisor do backend para verificar a liveness do papo-push. Ele
// informa o status básico e o estado da conexão com o banco.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"papo/push/internal/config"
)

// healthResponse é a resposta do /healthz.
type healthResponse struct {
	Status   string `json:"status"`
	Database string `json:"database"`
	Delivery string `json:"delivery"`
}

// Health é o servidor do endpoint de health.
type Health struct {
	cfg      *config.Config
	dbPing   func(ctx context.Context) error
	delivery string
}

// New cria um Health. deliveryMode é "fcm" ou "relay".
func New(cfg *config.Config, dbPing func(ctx context.Context) error, deliveryMode string) *Health {
	return &Health{
		cfg:      cfg,
		dbPing:   dbPing,
		delivery: deliveryMode,
	}
}

// Start escuta em 127.0.0.1:cfg.PushHealthPort e serve /healthz.
func (h *Health) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.handle)

	laddr := fmt.Sprintf("127.0.0.1:%d", h.cfg.PushHealthPort)
	server := &http.Server{
		Addr:    laddr,
		Handler: mux,
	}
	return server.ListenAndServe()
}

// handle responde /healthz. O status é sempre "ok" (o processo está de pé);
// database indica o estado da conexão (o supervisor só verifica o 200).
func (h *Health) handle(w http.ResponseWriter, r *http.Request) {
	dbStatus := "ok"
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.dbPing(ctx); err != nil {
		dbStatus = "error"
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(healthResponse{
		Status:   "ok",
		Database: dbStatus,
		Delivery: h.delivery,
	})
}
