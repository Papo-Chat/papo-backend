// Package store acessa as tabelas push_outbox e push_devices do PostgreSQL
// do papo-push. O worker consome a outbox, resolve os dispositivos do usuário
// e finaliza os jobs (sucesso: DELETE; retry: UPDATE; token inválido: desativa).
package store

import (
	"context"
	"database/sql"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"papo/push/internal/config"
)

// Store é a conexão com o PostgreSQL do papo-push.
type Store struct {
	db  *sql.DB
	cfg *config.Config
}

// New cria um Store conectando ao PostgreSQL.
func New(cfg *config.Config) (*Store, error) {
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxIdleConns(10)
	db.SetMaxOpenConns(20)
	db.SetConnMaxLifetime(1 * time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, err
	}
	return &Store{db: db, cfg: cfg}, nil
}

// Ping verifica a conexão (usado pelo healthz).
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// Close fecha a conexão.
func (s *Store) Close() error {
	return s.db.Close()
}

// PushJob é o job de entrega push na outbox (push_outbox).
type PushJob struct {
	ID             string
	UserID         string
	Payload        string
	NotificationID *string
	Attempts       int
	NextAttemptAt  time.Time
	CreatedAt      time.Time
	LastError       string
}

// PushDevice é o dispositivo registrado (push_devices).
type PushDevice struct {
	ID         string
	UserID     string
	Token      string
	Platform   string
	Provider   string
	DeviceName *string
	Enabled    bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
	LastSeenAt time.Time
}

// pushOutboxColumns são as colunas da push_outbox.
const pushOutboxColumns = "id, user_id, payload, notification_id, attempts, next_attempt_at, created_at, last_error"

// pushDevicesColumns são as colunas da push_devices.
const pushDevicesColumns = "id, user_id, token, platform, provider, device_name, enabled, created_at, updated_at, last_seen_at"

// pushDevicesColumnsNoID são as colunas da push_devices sem o id.
const pushDevicesColumnsNoID = "user_id, token, platform, provider, device_name, enabled, created_at, updated_at, last_seen_at"
