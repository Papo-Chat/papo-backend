package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"papo/push/internal/config"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// migrationsDir é o caminho relativo ao diretório deste pacote
// (papo-push/internal/store). Três níveis acima = raiz do projeto.
const migrationsDir = "../../../migrations"

// testStore é o store usado pelos testes, apontando para o banco temporário.
var testStore *Store

// defaultDatabaseURL corresponde aos padrões do infra/docker-compose.yml.
const defaultDatabaseURL = "postgres://papo:papo123@localhost:5432/papo"

func TestMain(m *testing.M) {
	os.Exit(runStoreTests(m))
}

// runStoreTests prepara um banco temporário com as migrations do projeto,
// inicializa o store contra ele, executa os testes e remove o banco ao final.
func runStoreTests(m *testing.M) int {
	baseURL := testDatabaseURL()

	baseDB, err := sql.Open("pgx", baseURL)
	if err != nil {
		fmt.Printf("testes de store ignorados: falha ao abrir conexão: %v\n", err)
		return 0
	}
	defer baseDB.Close()

	if err := ping(baseDB); err != nil {
		fmt.Printf("testes de store ignorados: não foi possível conectar ao PostgreSQL (%v). Inicie o PostgreSQL ou defina TEST_DATABASE_URL/DATABASE_URL.\n", err)
		return 0
	}

	removeOldTempDatabases(baseDB)

	tempDBName, err := createTempDatabase(baseDB)
	if err != nil {
		fmt.Printf("testes de store ignorados: falha ao criar banco temporário: %v\n", err)
		return 0
	}
	defer dropTempDatabase(baseDB, tempDBName)

	tempURL, err := withDatabase(baseURL, tempDBName)
	if err != nil {
		fmt.Printf("testes de store ignorados: %v\n", err)
		return 0
	}

	tempDB, err := sql.Open("pgx", tempURL)
	if err != nil {
		fmt.Printf("testes de store ignorados: %v\n", err)
		return 0
	}
	defer tempDB.Close()

	if err := ping(tempDB); err != nil {
		fmt.Printf("testes de store ignorados: falha ao conectar no banco temporário: %v\n", err)
		return 0
	}

	if err := applyMigrations(tempDB); err != nil {
		fmt.Printf("testes de store FALHARAM na preparação: %v\n", err)
		return 1
	}

	testStore = &Store{
		db:  tempDB,
		cfg: &config.Config{DatabaseURL: tempURL},
	}

	code := m.Run()

	_ = testStore.Close()
	return code
}

// testDatabaseURL resolve a DSN base: TEST_DATABASE_URL > DATABASE_URL > padrão.
func testDatabaseURL() string {
	for _, key := range []string{"TEST_DATABASE_URL", "DATABASE_URL"} {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return defaultDatabaseURL
}

func ping(db *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return db.PingContext(ctx)
}

// createTempDatabase cria um banco isolado para os testes.
func createTempDatabase(db *sql.DB) (string, error) {
	name := "papo_push_test_" + randHex(6)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		return "", err
	}
	return name, nil
}

func dropTempDatabase(db *sql.DB, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `DROP DATABASE "`+name+`"`); err != nil {
		fmt.Printf("aviso: falha ao remover banco temporário %s: %v\n", name, err)
	}
}

// removeOldTempDatabases limpa bancos deixados por execuções anteriores interrompidas.
func removeOldTempDatabases(db *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, `SELECT datname FROM pg_database WHERE datname LIKE 'papo\_push\_test\_%'`)
	if err != nil {
		fmt.Printf("falha ao remover banco temporário antigo: %v\n", err)
		return
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			fmt.Printf("falha ao remover banco temporário antigo: %v\n", err)
			return
		}
		names = append(names, name)
	}

	if err := rows.Err(); err != nil {
		fmt.Printf("falha ao remover banco temporário antigo: %v\n", err)
		return
	}

	for _, name := range names {
		if _, err := db.ExecContext(ctx, `DROP DATABASE "`+name+`"`); err != nil {
			fmt.Printf("falha ao remover banco temporário antigo %s: %v\n", name, err)
		}
	}
}

// withDatabase substitui o nome do banco na DSN.
func withDatabase(rawURL, name string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

// applyMigrations aplica a seção Up de cada migration do projeto, na ordem dos arquivos.
func applyMigrations(db *sql.DB) error {
	files, err := filepath.Glob(filepath.Join(migrationsDir, "*.sql"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("nenhum arquivo de migration encontrado em %s", migrationsDir)
	}
	sort.Strings(files)

	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if _, err := db.Exec(gooseUpSection(string(content))); err != nil {
			return fmt.Errorf("falha ao aplicar migration %s: %w", file, err)
		}
	}
	return nil
}

// gooseUpSection retorna apenas a seção Up do arquivo goose.
func gooseUpSection(sqlText string) string {
	if idx := strings.Index(sqlText, "-- +goose Down"); idx >= 0 {
		sqlText = sqlText[:idx]
	}
	return sqlText
}

// randHex gera uma sequência hexadecimal aleatória.
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// createTestUser insere um usuário mínimo e retorna o ID.
func createTestUser(t *testing.T) string {
	ctx := context.Background()
	username := "user_" + randHex(6)
	var userID string
	query := `
		INSERT INTO users (username, password_hash, banned)
		VALUES ($1, 'test-password', FALSE)
		RETURNING id
	`
	if err := testStore.db.QueryRowContext(ctx, query, username).Scan(&userID); err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	return userID
}

// createTestDevice insere um dispositivo ativo para o usuário.
func createTestDevice(t *testing.T, userID, token string) {
	ctx := context.Background()
	query := `
		INSERT INTO push_devices (user_id, token, platform, provider, enabled)
		VALUES ($1, $2, 'android', 'fcm', TRUE)
	`
	if _, err := testStore.db.ExecContext(ctx, query, userID, token); err != nil {
		t.Fatalf("falha ao criar dispositivo: %v", err)
	}
}

// createTestJob insere um job na outbox (attempts e next_attempt_at ajustáveis).
func createTestJob(t *testing.T, userID, payload string, attempts int, nextAttemptAt time.Time) string {
	ctx := context.Background()
	var jobID string
	query := `
		INSERT INTO push_outbox (user_id, payload, attempts, next_attempt_at)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`
	if err := testStore.db.QueryRowContext(ctx, query, userID, payload, attempts, nextAttemptAt).Scan(&jobID); err != nil {
		t.Fatalf("falha ao criar job: %v", err)
	}
	return jobID
}

// TestClaimDueJobs verifica que jobs vencidos são reservados e que next_attempt_at
// é movido para o futuro (claim).
func TestClaimDueJobs(t *testing.T) {
	ctx := context.Background()
	userID := createTestUser(t)

	payload := `{"type":"message","message_id":"m1","channel_id":"c1","author":"João","preview":"oi"}`
	now := time.Now()

	// Job vencido (next_attempt_at no passado).
	jobDue := createTestJob(t, userID, payload, 0, now.Add(-time.Minute))
	// Job futuro (next_attempt_at no futuro) — não deve ser reservado.
	// payloadFuturo: JSON válido e distinto do do job vencido.
	payloadFuturo := `{"type":"message","message_id":"m2","channel_id":"c1","author":"João","preview":"oi-future"}`
	jobFuture := createTestJob(t, userID, payloadFuturo, 0, now.Add(time.Hour))

	jobs, err := testStore.ClaimDueJobs(ctx, 10)
	if err != nil {
		t.Fatalf("falha em ClaimDueJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("esperava 1 job reservado, obtive %d", len(jobs))
	}
	if jobs[0].ID != jobDue {
		t.Fatalf("esperava reservar %s, obtive %s", jobDue, jobs[0].ID)
	}

	// Verifica que next_attempt_at foi movido para o futuro.
	var nextAt time.Time
	if err := testStore.db.QueryRowContext(ctx,
		"SELECT next_attempt_at FROM push_outbox WHERE id = $1", jobDue).Scan(&nextAt); err != nil {
		t.Fatalf("falha ao consultar next_attempt_at: %v", err)
	}
	if nextAt.Before(now.Add(5 * time.Minute)) {
		t.Fatalf("esperava next_attempt_at no futuro, obtive %v", nextAt)
	}

	// O job futuro não deve ter sido reservado (next_attempt_at continua no futuro original).
	var futureAt time.Time
	if err := testStore.db.QueryRowContext(ctx,
		"SELECT next_attempt_at FROM push_outbox WHERE id = $1", jobFuture).Scan(&futureAt); err != nil {
		t.Fatalf("falha ao consultar next_attempt_at do job futuro: %v", err)
	}
	if !futureAt.After(now.Add(30 * time.Minute)) {
		t.Fatalf("esperava job futuro ainda no futuro, obtive %v", futureAt)
	}
}

// TestClaimDueJobsNoJobs verifica que a ausência de jobs vencidos não falha.
func TestClaimDueJobsNoJobs(t *testing.T) {
	ctx := context.Background()
	jobs, err := testStore.ClaimDueJobs(ctx, 10)
	if err != nil {
		t.Fatalf("falha em ClaimDueJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("esperava 0 jobs, obtive %d", len(jobs))
	}
}

// TestCompletePushJob verifica que o job é removido.
func TestCompletePushJob(t *testing.T) {
	ctx := context.Background()
	userID := createTestUser(t)
	payload := `{"type":"message"}`
	jobID := createTestJob(t, userID, payload, 0, time.Now().Add(-time.Minute))

	if err := testStore.CompletePushJob(ctx, jobID); err != nil {
		t.Fatalf("falha em CompletePushJob: %v", err)
	}
	var count int
	if err := testStore.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM push_outbox WHERE id = $1", jobID).Scan(&count); err != nil {
		t.Fatalf("falha ao consultar: %v", err)
	}
	if count != 0 {
		t.Fatalf("esperava job removido, obtive %d linha(s)", count)
	}
}

// TestReschedulePushJob verifica que o job é reagendado com attempts e next_attempt_at.
func TestReschedulePushJob(t *testing.T) {
	ctx := context.Background()
	userID := createTestUser(t)
	payload := `{"type":"message"}`
	jobID := createTestJob(t, userID, payload, 0, time.Now().Add(-time.Minute))
	nextAt := time.Now().Add(30 * time.Second)

	if err := testStore.ReschedulePushJob(ctx, jobID, 2, nextAt, "retryable_error"); err != nil {
		t.Fatalf("falha em ReschedulePushJob: %v", err)
	}

	var attempts int
	var lastError string
	var actualNext time.Time
	if err := testStore.db.QueryRowContext(ctx,
		"SELECT attempts, last_error, next_attempt_at FROM push_outbox WHERE id = $1", jobID).Scan(&attempts, &lastError, &actualNext); err != nil {
		t.Fatalf("falha ao consultar: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("esperava attempts=2, obtive %d", attempts)
	}
	if lastError != "retryable_error" {
		t.Fatalf("esperava last_error='retryable_error', obtive %q", lastError)
	}
	if actualNext.Before(nextAt.Add(-time.Second)) || actualNext.After(nextAt.Add(time.Second)) {
		t.Fatalf("esperava next_attempt_at=%v, obtive %v", nextAt, actualNext)
	}
}

// TestListEnabledPushDevices verifica que apenas dispositivos ativos são listados.
func TestListEnabledPushDevices(t *testing.T) {
	ctx := context.Background()
	userID := createTestUser(t)

	createTestDevice(t, userID, "tok-1")
	createTestDevice(t, userID, "tok-2")
	// Desativa um dispositivo.
	if _, err := testStore.db.ExecContext(ctx,
		"UPDATE push_devices SET enabled = FALSE WHERE token = 'tok-2'"); err != nil {
		t.Fatalf("falha ao desativar dispositivo: %v", err)
	}

	devices, err := testStore.ListEnabledPushDevices(ctx, userID)
	if err != nil {
		t.Fatalf("falha em ListEnabledPushDevices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("esperava 1 dispositivo ativo, obtive %d", len(devices))
	}
	if devices[0].Token != "tok-1" {
		t.Fatalf("esperava tok-1, obtive %s", devices[0].Token)
	}
}

// TestDisablePushDeviceByToken verifica que o token é desativado.
func TestDisablePushDeviceByToken(t *testing.T) {
	ctx := context.Background()
	userID := createTestUser(t)
	createTestDevice(t, userID, "tok-x")

	affected, err := testStore.DisablePushDeviceByToken(ctx, "tok-x")
	if err != nil {
		t.Fatalf("falha em DisablePushDeviceByToken: %v", err)
	}
	if affected != 1 {
		t.Fatalf("esperava 1 linha afetada, obtive %d", affected)
	}

	var enabled bool
	if err := testStore.db.QueryRowContext(ctx,
		"SELECT enabled FROM push_devices WHERE token = $1", "tok-x").Scan(&enabled); err != nil {
		t.Fatalf("falha ao consultar: %v", err)
	}
	if enabled {
		t.Fatalf("esperava dispositivo desativado, obtive enabled=true")
	}
}

// TestStorePingAndClose verifica basicamente o ping e o close.
func TestStorePingAndClose(t *testing.T) {
	ctx := context.Background()
	if err := testStore.Ping(ctx); err != nil {
		t.Fatalf("falha no ping: %v", err)
	}
}
