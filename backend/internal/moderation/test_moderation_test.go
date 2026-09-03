package moderation

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/storage"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// migrationsDir é o caminho relativo ao diretório deste pacote.
const migrationsDir = "../../../migrations"

// defaultDatabaseURL corresponde aos padrões do infra/docker-compose.yml.
const defaultDatabaseURL = "postgres://papo:papo123@localhost:5432/papo"

// dbAvailable indica se o banco foi preparado (testes puros rodam sem ele;
// testes de banco são pulados).
var dbAvailable bool

func TestMain(m *testing.M) {
	os.Exit(runModerationTests(m))
}

// runModerationTests prepara um banco temporário com as migrations do projeto
// (mesmo padrão dos testes de storage). Sem banco disponível, os testes de
// banco são pulados e os testes puros (policy, fila, client) seguem.
func runModerationTests(m *testing.M) int {
	baseURL := testDatabaseURL()

	baseDB, err := sql.Open("pgx", baseURL)
	if err != nil {
		fmt.Printf("moderação: sem banco para os testes (%v); testes de banco serão pulados\n", err)
		return m.Run()
	}
	defer baseDB.Close()

	if err := pingDB(baseDB); err != nil {
		fmt.Printf("moderação: não foi possível conectar ao PostgreSQL (%v); testes de banco serão pulados. Inicie o PostgreSQL (infra/docker-compose.yml) ou defina TEST_DATABASE_URL/DATABASE_URL.\n", err)
		return m.Run()
	}

	removeOldTempDatabases(baseDB)

	tempDBName, err := createTempDatabase(baseDB)
	if err != nil {
		fmt.Printf("moderação: falha ao criar banco temporário (%v); testes de banco serão pulados\n", err)
		return m.Run()
	}
	defer dropTempDatabase(baseDB, tempDBName)

	tempURL, err := withDatabase(baseURL, tempDBName)
	if err != nil {
		fmt.Printf("testes de moderação FALHARAM na preparação: %v\n", err)
		return 1
	}

	tempDB, err := sql.Open("pgx", tempURL)
	if err != nil {
		fmt.Printf("testes de moderação FALHARAM na preparação: %v\n", err)
		return 1
	}
	defer tempDB.Close()

	if err := pingDB(tempDB); err != nil {
		fmt.Printf("testes de moderação FALHARAM na preparação: %v\n", err)
		return 1
	}

	if err := applyMigrations(tempDB); err != nil {
		fmt.Printf("testes de moderação FALHARAM na preparação: %v\n", err)
		return 1
	}

	if err := storage.InitDB(tempURL); err != nil {
		fmt.Printf("testes de moderação FALHARAM na preparação: %v\n", err)
		return 1
	}
	defer storage.CloseDB()

	dbAvailable = true
	return m.Run()
}

func testDatabaseURL() string {
	for _, key := range []string{"TEST_DATABASE_URL", "DATABASE_URL"} {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return defaultDatabaseURL
}

func pingDB(db *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return db.PingContext(ctx)
}

func createTempDatabase(db *sql.DB) (string, error) {
	name := "papo_test_" + randHex(6)
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

func removeOldTempDatabases(db *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, `SELECT datname FROM pg_database WHERE datname LIKE 'papo\_test\_%'`)
	if err != nil {
		fmt.Printf("falha ao listar bancos temporários antigos: %v", err)
		return
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			fmt.Printf("falha ao listar bancos temporários antigos: %v", err)
			return
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		fmt.Printf("falha ao listar bancos temporários antigos: %v", err)
		return
	}
	for _, name := range names {
		if _, err := db.ExecContext(ctx, `DROP DATABASE "`+name+`"`); err != nil {
			fmt.Printf("aviso: falha ao remover banco temporário antigo %s: %v\n", name, err)
		}
	}
}

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

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func requireDB(t *testing.T) {
	t.Helper()
	if !dbAvailable {
		t.Skip("sem banco disponível para os testes")
	}
}

// almostEqual compara scores com tolerância: as colunas de score são REAL
// (float32) e valores como 0.85 não sobrevivem ao round-trip exato.
func almostEqual(a, b float64) bool {
	const epsilon = 1e-6
	return a-b < epsilon && b-a < epsilon
}

// --- Policy ---

func TestPolicyEvaluate(t *testing.T) {
	base := func(nudityMode, goreMode string) Policy {
		return Policy{NudityMode: nudityMode, GoreMode: goreMode, NudityThreshold: 0.8, GoreThreshold: 0.8}
	}

	cases := []struct {
		name       string
		policy     Policy
		result     Result
		wantStatus Status
		wantReason string
	}{
		{"ambas off, scores altos", base("off", "off"), Result{Nudity: 0.99, Gore: 0.99}, StatusClean, ""},
		{"flag abaixo do threshold", base("flag", "off"), Result{Nudity: 0.79}, StatusClean, ""},
		{"flag exatamente no threshold", base("flag", "off"), Result{Nudity: 0.8}, StatusSensitive, "nsfw"},
		{"flag acima do threshold", base("flag", "off"), Result{Nudity: 0.99}, StatusSensitive, "nsfw"},
		{"blur acima do threshold", base("blur", "off"), Result{Nudity: 0.99}, StatusSensitive, "nsfw"},
		{"block acima do threshold", base("block", "off"), Result{Nudity: 0.99}, StatusBlocked, "nsfw"},
		{"block abaixo do threshold", base("block", "off"), Result{Nudity: 0.79}, StatusClean, ""},
		{"gore block", base("off", "block"), Result{Gore: 0.9}, StatusBlocked, "nsfl"},
		{"gore flag", base("off", "flag"), Result{Gore: 0.9}, StatusSensitive, "nsfl"},
		{"block tem prioridade sobre flag", base("flag", "block"), Result{Nudity: 0.9, Gore: 0.9}, StatusBlocked, "nsfl"},
		{"ambos block: nudity primeiro", base("block", "block"), Result{Nudity: 0.9, Gore: 0.9}, StatusBlocked, "nsfw"},
		{"gore abaixo do threshold, nudity off", base("off", "flag"), Result{Gore: 0.5}, StatusClean, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, reason := tc.policy.Evaluate(tc.result)
			if status != tc.wantStatus || reason != tc.wantReason {
				t.Fatalf("Evaluate(%+v) = (%s, %q); esperado (%s, %q)", tc.result, status, reason, tc.wantStatus, tc.wantReason)
			}
		})
	}
}

func TestNewPolicyNormalizesInvalidMode(t *testing.T) {
	cfg := &config.Config{
		ModerationNudityMode:      "banana",
		ModerationGoreMode:        "off",
		ModerationNudityThreshold: 0.8,
		ModerationGoreThreshold:   0.8,
	}
	p := NewPolicy(cfg)
	if p.NudityMode != ModeOff {
		t.Fatalf("modo inválido deveria normalizar para %q, obtive %q", ModeOff, p.NudityMode)
	}
}

// --- Fila ---

func newBareService(t *testing.T, queueSize int) *Service {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := &config.Config{ModerationQueueSize: queueSize, ModerationConcurrency: 1}
	return New(cfg, ctx)
}

func TestEnqueue(t *testing.T) {
	t.Run("id vazio é no-op", func(t *testing.T) {
		s := newBareService(t, 4)
		s.enqueue("")
		if len(s.queue) != 0 || len(s.inflight) != 0 {
			t.Fatalf("enqueue(\"\") deveria ser no-op (queue=%d inflight=%d)", len(s.queue), len(s.inflight))
		}
	})

	t.Run("deduplica in-flight", func(t *testing.T) {
		s := newBareService(t, 4)
		s.enqueue("a")
		s.enqueue("a")
		if len(s.queue) != 1 {
			t.Fatalf("esperava 1 job na fila (dedup), obtive %d", len(s.queue))
		}
	})

	t.Run("fila cheia descarta e libera o inflight", func(t *testing.T) {
		s := newBareService(t, 1)
		s.enqueue("a")
		s.enqueue("b") // não cabe: permanece pending, sai do inflight
		if len(s.queue) != 1 {
			t.Fatalf("esperava 1 job na fila, obtive %d", len(s.queue))
		}
		s.mu.Lock()
		_, inA := s.inflight["a"]
		_, inB := s.inflight["b"]
		s.mu.Unlock()
		if !inA || inB {
			t.Fatalf("inflight deveria conter apenas 'a' (a=%v b=%v)", inA, inB)
		}
	})

	t.Run("descartado pode ser reenfileirado", func(t *testing.T) {
		s := newBareService(t, 1)
		s.enqueue("a")
		s.enqueue("b") // descartado (fila cheia)
		<-s.queue      // consome 'a'
		s.enqueue("b") // agora cabe
		if len(s.queue) != 1 {
			t.Fatalf("esperava 1 job na fila após reenfileirar, obtive %d", len(s.queue))
		}
	})
}

// --- Client (socket Unix falso) ---

// startFakeWorkerServer sobe um worker falso num socket Unix: lê uma
// requisição NDJSON por conexão e responde com respond(req) (com \n).
func startFakeWorkerServer(t *testing.T, respond func(req map[string]any) []byte) string {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "worker.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("falha ao escutar no socket Unix: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
		_ = os.Remove(socketPath)
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				var req map[string]any
				if err := json.Unmarshal(line, &req); err != nil {
					return
				}
				if _, err := c.Write(respond(req)); err != nil {
					return
				}
			}(conn)
		}
	}()
	return socketPath
}

func TestClientHealth(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		socketPath := startFakeWorkerServer(t, func(req map[string]any) []byte {
			return []byte(`{"status":"ok"}
`)
		})
		c := NewClient(socketPath, 5*time.Second)
		if err := c.Health(context.Background()); err != nil {
			t.Fatalf("Health deveria retornar nil, obtive %v", err)
		}
	})

	t.Run("status não ok", func(t *testing.T) {
		socketPath := startFakeWorkerServer(t, func(req map[string]any) []byte {
			return []byte(`{"status":"degraded"}
`)
		})
		c := NewClient(socketPath, 5*time.Second)
		if err := c.Health(context.Background()); err == nil {
			t.Fatal("Health deveria retornar erro para status não ok")
		}
	})

	t.Run("socket inexistente", func(t *testing.T) {
		c := NewClient(filepath.Join(t.TempDir(), "inexistente.sock"), 5*time.Second)
		if err := c.Health(context.Background()); err == nil {
			t.Fatal("esperava erro de conexão")
		}
	})
}

func TestClientClassify(t *testing.T) {
	t.Run("parsea os scores", func(t *testing.T) {
		socketPath := startFakeWorkerServer(t, func(req map[string]any) []byte {
			if req["path"] != "/media/x.jpg" || req["mime"] != "image/jpeg" {
				t.Errorf("requisição chegou com campos errados: %v", req)
			}
			return []byte(`{"request_id":"r1","sfw":0.5,"nudity":0.3,"gore":0.2,"model":"m1"}
`)
		})
		c := NewClient(socketPath, 5*time.Second)
		res, err := c.Classify(context.Background(), "/media/x.jpg", "image/jpeg")
		if err != nil {
			t.Fatalf("Classify retornou erro: %v", err)
		}
		if res.SFW != 0.5 || res.Nudity != 0.3 || res.Gore != 0.2 || res.Model != "m1" {
			t.Fatalf("resultado inesperado: %+v", res)
		}
	})

	t.Run("erro do worker", func(t *testing.T) {
		socketPath := startFakeWorkerServer(t, func(req map[string]any) []byte {
			return []byte(`{"request_id":"r1","error":"arquivo não encontrado"}
`)
		})
		c := NewClient(socketPath, 5*time.Second)
		_, err := c.Classify(context.Background(), "/x", "image/jpeg")
		if err == nil || !strings.Contains(err.Error(), "arquivo não encontrado") {
			t.Fatalf("esperava erro contendo a mensagem do worker, obtive %v", err)
		}
	})

	t.Run("scores ausentes", func(t *testing.T) {
		socketPath := startFakeWorkerServer(t, func(req map[string]any) []byte {
			return []byte(`{"request_id":"r1","sfw":0.5}
`)
		})
		c := NewClient(socketPath, 5*time.Second)
		if _, err := c.Classify(context.Background(), "/x", "image/jpeg"); err == nil {
			t.Fatal("esperava erro para resposta malformada (scores ausentes)")
		}
	})

	t.Run("json malformado", func(t *testing.T) {
		socketPath := startFakeWorkerServer(t, func(req map[string]any) []byte {
			return []byte(`isso não é json
`)
		})
		c := NewClient(socketPath, 5*time.Second)
		if _, err := c.Classify(context.Background(), "/x", "image/jpeg"); err == nil {
			t.Fatal("esperava erro para resposta com JSON inválido")
		}
	})

	t.Run("resposta excede o limite", func(t *testing.T) {
		socketPath := startFakeWorkerServer(t, func(req map[string]any) []byte {
			// Linha sem \n maior que maxResponseBytes: a leitura deve falhar
			// com limite de memória (sem buffering ilimitado).
			return bytes.Repeat([]byte("x"), maxResponseBytes+1024)
		})
		c := NewClient(socketPath, 5*time.Second)
		if _, err := c.Classify(context.Background(), "/x", "image/jpeg"); err == nil {
			t.Fatal("esperava erro para resposta excedendo o limite")
		}
	})
}

// --- Concorrência ---

func TestNewClampsConcurrency(t *testing.T) {
	// O worker Python atende sequencialmente: MODERATION_CONCURRENCY>1 seria
	// mentira (só enfileiraria no socket). O serviço fixa em 1.
	cfg := &config.Config{ModerationQueueSize: 8, ModerationConcurrency: 4}
	s := New(cfg, context.Background())
	if s.concurrency != 1 {
		t.Fatalf("esperava concorrência 1 (worker Python sequencial), obtive %d", s.concurrency)
	}
}

// --- Worker: fluxo completo com classificador falso ---

// fakeClassifier alimenta o process() com um resultado de inferência
// sintético (sem worker Python real e sem imagem sensível em disco).
type fakeClassifier struct {
	result Result
	err    error
	calls  int
}

func (f *fakeClassifier) Classify(ctx context.Context, path, mime string) (Result, error) {
	f.calls++
	return f.result, f.err
}

func testConfig(t *testing.T, nudityMode, goreMode string, nudityThreshold, goreThreshold float64) *config.Config {
	t.Helper()
	return &config.Config{
		ModerationEnabled:         true,
		ModerationWorkerCommand:   "python3",
		ModerationWorkerPath:      "moderation_worker/worker.py",
		ModerationSocketPath:      filepath.Join(t.TempDir(), "moderation.sock"),
		ModerationModelsDir:       filepath.Join(t.TempDir(), "models"),
		ModerationQueueSize:       256,
		ModerationConcurrency:     1,
		ModerationTimeout:         5 * time.Second,
		ModerationNudityMode:      nudityMode,
		ModerationGoreMode:        goreMode,
		ModerationNudityThreshold: nudityThreshold,
		ModerationGoreThreshold:   goreThreshold,
	}
}

// newTestService monta o serviço com o classificador falso e o supervisor
// marcado como pronto (sem iniciar o worker Python de verdade).
func newTestService(t *testing.T, cfg *config.Config, classifier Classifier) *Service {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := New(cfg, ctx)
	s.client = classifier
	s.sup.setState(StateReady)
	return s
}

// imageMessageFixture cria a cadeia completa: usuário → canal → linha de
// media → attachment (pending) → mensagem com o attachment.
type imageMessageFixture struct {
	user       models.User
	channel    models.Channel
	attachment models.Attachments
	message    models.Message
}

func newImageMessageFixture(t *testing.T, mime string) *imageMessageFixture {
	t.Helper()
	requireDB(t)
	ctx := context.Background()

	user, _, err := storage.CreateUser(ctx, "mod_"+randHex(8), "hash_"+randHex(8), "127.0.0.1")
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	channel, err := storage.CreateChannel(ctx, "canal_"+randHex(8), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	hash := randHex(32)
	if _, _, err := storage.InsertMediaIfAbsent(ctx, hash, mime, 1024); err != nil {
		t.Fatalf("falha ao criar registro de mídia: %v", err)
	}

	attachment, err := storage.CreateAttachment(ctx, models.Attachments{
		OriginalFileName: "foto.bin",
		MediaShaHash:     hash,
		CreatedBy:        &user.ID,
	})
	if err != nil {
		t.Fatalf("falha ao criar attachment: %v", err)
	}

	message, err := storage.CreateMessage(ctx, channel.ID, user.ID, "olá", "", []string{attachment.ID})
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	// Relê o attachment já vinculado à mensagem (messages_id preenchido).
	attachment, err = storage.GetAttachmentByID(ctx, attachment.ID)
	if err != nil {
		t.Fatalf("falha ao reler o attachment: %v", err)
	}
	if attachment.ModerationStatus != string(StatusPending) {
		t.Fatalf("attachment deveria nascer pending, obtive %s", attachment.ModerationStatus)
	}
	if attachment.MessagesID == nil || *attachment.MessagesID != message.ID {
		t.Fatal("attachment deveria estar vinculado à mensagem")
	}

	return &imageMessageFixture{user: user, channel: channel, attachment: attachment, message: message}
}

func TestProcessBlockedDeletesMessageAndAudits(t *testing.T) {
	f := newImageMessageFixture(t, "image/jpeg")
	fake := &fakeClassifier{result: Result{SFW: 0.01, Nudity: 0.99, Gore: 0.01, Model: "fake-v1"}}
	s := newTestService(t, testConfig(t, ModeBlock, ModeOff, 0.8, 0.8), fake)

	s.process(Job{AttachmentID: f.attachment.ID})

	ctx := context.Background()

	// A mensagem é excluída (o attachment sai via ON DELETE CASCADE).
	if _, err := storage.GetMessageByID(ctx, f.message.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("esperava a mensagem excluída (obtive %v)", err)
	}
	if _, err := storage.GetAttachmentByID(ctx, f.attachment.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("esperava o attachment excluído (obtive %v)", err)
	}

	// Log de auditoria explícito da ação de sistema.
	var actorID sql.NullString
	var actorUsername string
	var targetUserID sql.NullString
	var metadata []byte
	err := storage.GetDB().QueryRowContext(ctx,
		"SELECT actor_id, actor_username, target_user_id, metadata FROM audit_logs WHERE action = $1 AND entity_id = $2",
		services.ActionMessageModerationBlocked, f.message.ID,
	).Scan(&actorID, &actorUsername, &targetUserID, &metadata)
	if err != nil {
		t.Fatalf("esperava log de auditoria da mensagem blocked: %v", err)
	}
	if actorID.Valid {
		t.Fatalf("ação de sistema deveria ter actor_id NULL, obtive %q", actorID.String)
	}
	if actorUsername != "sistema" {
		t.Fatalf("esperava actor_username %q, obtive %q", "sistema", actorUsername)
	}
	if !targetUserID.Valid || targetUserID.String != f.user.ID {
		t.Fatalf("esperava target_user_id = %s, obtive %v", f.user.ID, targetUserID)
	}

	var meta map[string]any
	if err := json.Unmarshal(metadata, &meta); err != nil {
		t.Fatalf("falha ao interpretar o metadata da auditoria: %v", err)
	}
	if meta["reason"] != "nsfw" || meta["attachment_id"] != f.attachment.ID || meta["model_version"] != "fake-v1" {
		t.Fatalf("metadata inesperado: %v", meta)
	}
	if meta["nudity_score"] != 0.99 {
		t.Fatalf("esperava nudity_score 0.99 no metadata, obtive %v", meta["nudity_score"])
	}

	if fake.calls != 1 {
		t.Fatalf("esperava 1 chamada ao classificador, obtive %d", fake.calls)
	}
}

func TestProcessSensitiveKeepsMessage(t *testing.T) {
	f := newImageMessageFixture(t, "image/jpeg")
	fake := &fakeClassifier{result: Result{SFW: 0.1, Nudity: 0.85, Gore: 0.05, Model: "fake-v1"}}
	s := newTestService(t, testConfig(t, ModeFlag, ModeOff, 0.8, 0.8), fake)

	s.process(Job{AttachmentID: f.attachment.ID})

	ctx := context.Background()
	attachment, err := storage.GetAttachmentByID(ctx, f.attachment.ID)
	if err != nil {
		t.Fatalf("o attachment deveria continuar existindo: %v", err)
	}
	if attachment.ModerationStatus != string(StatusSensitive) {
		t.Fatalf("esperava status sensitive, obtive %s", attachment.ModerationStatus)
	}
	if attachment.ModerationNudityScore == nil || !almostEqual(*attachment.ModerationNudityScore, 0.85) {
		t.Fatalf("esperava score de nudez 0.85, obtive %v", *attachment.ModerationNudityScore)
	}
	if attachment.ModerationModelVersion == nil || *attachment.ModerationModelVersion != "fake-v1" {
		t.Fatalf("esperava model_version fake-v1, obtive %v", attachment.ModerationModelVersion)
	}
	if attachment.ModerationCheckedAt == nil {
		t.Fatal("esperava moderation_checked_at preenchido")
	}
	if _, err := storage.GetMessageByID(ctx, f.message.ID); err != nil {
		t.Fatalf("sensitive não deve excluir a mensagem: %v", err)
	}

	// Sensitive não gera auditoria (apenas blocked).
	var count int
	if err := storage.GetDB().QueryRowContext(ctx,
		"SELECT count(*) FROM audit_logs WHERE action = $1 AND entity_id = $2",
		services.ActionMessageModerationBlocked, f.message.ID,
	).Scan(&count); err != nil {
		t.Fatalf("falha ao consultar a auditoria: %v", err)
	}
	if count != 0 {
		t.Fatalf("esperava 0 logs de auditoria para sensitive, obtive %d", count)
	}
}

func TestProcessClean(t *testing.T) {
	f := newImageMessageFixture(t, "image/png")
	fake := &fakeClassifier{result: Result{SFW: 0.97, Nudity: 0.02, Gore: 0.01, Model: "fake-v1"}}
	s := newTestService(t, testConfig(t, ModeBlock, ModeBlock, 0.8, 0.8), fake)

	s.process(Job{AttachmentID: f.attachment.ID})

	ctx := context.Background()
	attachment, err := storage.GetAttachmentByID(ctx, f.attachment.ID)
	if err != nil {
		t.Fatalf("o attachment deveria continuar existindo: %v", err)
	}
	if attachment.ModerationStatus != string(StatusClean) {
		t.Fatalf("esperava status clean, obtive %s", attachment.ModerationStatus)
	}
	if attachment.ModerationSFWScore == nil || !almostEqual(*attachment.ModerationSFWScore, 0.97) {
		t.Fatalf("esperava score sfw 0.97, obtive %v", *attachment.ModerationSFWScore)
	}
	if _, err := storage.GetMessageByID(ctx, f.message.ID); err != nil {
		t.Fatalf("clean não deve excluir a mensagem: %v", err)
	}
}

func TestProcessNonImageSkipsClassifier(t *testing.T) {
	f := newImageMessageFixture(t, "application/pdf")
	fake := &fakeClassifier{result: Result{Nudity: 0.99}}
	s := newTestService(t, testConfig(t, ModeBlock, ModeBlock, 0.8, 0.8), fake)

	s.process(Job{AttachmentID: f.attachment.ID})

	ctx := context.Background()
	attachment, err := storage.GetAttachmentByID(ctx, f.attachment.ID)
	if err != nil {
		t.Fatalf("o attachment deveria continuar existindo: %v", err)
	}
	if attachment.ModerationStatus != string(StatusClean) {
		t.Fatalf("não-imagem deveria ficar clean, obtive %s", attachment.ModerationStatus)
	}
	if fake.calls != 0 {
		t.Fatalf("não-imagem não deve chamar o classificador (chamadas=%d)", fake.calls)
	}
	if attachment.ModerationModelVersion != nil {
		t.Fatalf("não-imagem não deve registrar model_version, obtive %v", *attachment.ModerationModelVersion)
	}
}

func TestProcessNotFoundIsNoop(t *testing.T) {
	requireDB(t)
	fake := &fakeClassifier{}
	s := newTestService(t, testConfig(t, ModeBlock, ModeOff, 0.8, 0.8), fake)

	// UUID bem-formado mas inexistente (exerce o caminho ErrNotFound).
	s.process(Job{AttachmentID: "00000000-0000-0000-0000-000000000000"})

	if fake.calls != 0 {
		t.Fatalf("attachment inexistente não deve chamar o classificador (chamadas=%d)", fake.calls)
	}
}

func TestProcessInferenceFailureRetriesThenFails(t *testing.T) {
	f := newImageMessageFixture(t, "image/jpeg")
	fake := &fakeClassifier{err: errors.New("worker fora do ar")}
	s := newTestService(t, testConfig(t, ModeBlock, ModeOff, 0.8, 0.8), fake)

	ctx := context.Background()
	statusOf := func() (string, int) {
		a, err := storage.GetAttachmentByID(ctx, f.attachment.ID)
		if err != nil {
			t.Fatalf("falha ao ler o attachment: %v", err)
		}
		return a.ModerationStatus, a.ModerationAttempts
	}

	s.process(Job{AttachmentID: f.attachment.ID})
	if status, attempts := statusOf(); status != string(StatusPending) || attempts != 1 {
		t.Fatalf("após a 1ª falha esperava pending/1, obtive %s/%d", status, attempts)
	}

	s.process(Job{AttachmentID: f.attachment.ID})
	if status, attempts := statusOf(); status != string(StatusPending) || attempts != 2 {
		t.Fatalf("após a 2ª falha esperava pending/2, obtive %s/%d", status, attempts)
	}

	s.process(Job{AttachmentID: f.attachment.ID})
	if status, attempts := statusOf(); status != string(StatusFailed) || attempts != 3 {
		t.Fatalf("após a 3ª falha esperava failed/3, obtive %s/%d", status, attempts)
	}

	// A mensagem continua intacta (falha de inferência não exclui nada).
	if _, err := storage.GetMessageByID(ctx, f.message.ID); err != nil {
		t.Fatalf("falha de inferência não deve excluir a mensagem: %v", err)
	}
}

// --- Claim / reconciler ---

func TestClaimAttachmentForModeration(t *testing.T) {
	f := newImageMessageFixture(t, "image/jpeg")
	ctx := context.Background()
	id := f.attachment.ID

	if claimed, err := storage.ClaimAttachmentForModeration(ctx, id, staleProcessing); err != nil || !claimed {
		t.Fatalf("pending deveria ser claimado (claimed=%v err=%v)", claimed, err)
	}

	// Em processing fresco (não stale): claim falha.
	if claimed, err := storage.ClaimAttachmentForModeration(ctx, id, staleProcessing); err != nil || claimed {
		t.Fatalf("processing fresco não deveria ser claimado (claimed=%v err=%v)", claimed, err)
	}

	// Simula órfão de crash: processing com idade acima do limite.
	if _, err := storage.GetDB().ExecContext(ctx,
		"UPDATE attachments SET moderation_updated_at = now() - interval '10 minutes' WHERE id = $1", id); err != nil {
		t.Fatalf("falha ao envelhecer o attachment: %v", err)
	}
	if claimed, err := storage.ClaimAttachmentForModeration(ctx, id, staleProcessing); err != nil || !claimed {
		t.Fatalf("processing stale deveria ser claimado (claimed=%v err=%v)", claimed, err)
	}
}

func TestListModerationPending(t *testing.T) {
	f := newImageMessageFixture(t, "image/jpeg")
	ctx := context.Background()

	// Cria mais attachments pendentes na mesma cadeia de mídia.
	extra := func(suffix string) string {
		t.Helper()
		a, err := storage.CreateAttachment(ctx, models.Attachments{
			OriginalFileName: "extra_" + suffix + ".bin",
			MediaShaHash:     f.attachment.MediaShaHash,
			CreatedBy:        &f.user.ID,
		})
		if err != nil {
			t.Fatalf("falha ao criar attachment extra: %v", err)
		}
		return a.ID
	}
	cleanID := extra("clean")
	staleID := extra("stale")
	freshID := extra("fresh")

	setStatus := func(id, status string) {
		t.Helper()
		if _, err := storage.GetDB().ExecContext(ctx,
			"UPDATE attachments SET moderation_status = $1, moderation_updated_at = now() WHERE id = $2", status, id); err != nil {
			t.Fatalf("falha ao ajustar o status de %s: %v", id, err)
		}
	}
	setStatus(cleanID, string(StatusClean))
	setStatus(staleID, string(StatusProcessing))
	if _, err := storage.GetDB().ExecContext(ctx,
		"UPDATE attachments SET moderation_updated_at = now() - interval '10 minutes' WHERE id = $1", staleID); err != nil {
		t.Fatalf("falha ao envelhecer o attachment stale: %v", err)
	}
	setStatus(freshID, string(StatusProcessing))

	ids, err := storage.ListModerationPending(ctx, staleProcessing, reconcileBatchSize)
	if err != nil {
		t.Fatalf("falha ao listar os pendentes: %v", err)
	}

	contains := func(id string) bool {
		for _, got := range ids {
			if got == id {
				return true
			}
		}
		return false
	}
	if !contains(f.attachment.ID) {
		t.Fatalf("esperava o attachment pending na lista: %v", ids)
	}
	if !contains(staleID) {
		t.Fatalf("esperava o processing stale na lista: %v", ids)
	}
	if contains(cleanID) {
		t.Fatalf("clean não deveria estar na lista: %v", ids)
	}
	if contains(freshID) {
		t.Fatalf("processing fresco não deveria estar na lista: %v", ids)
	}
}

// --- Supervisor / lifecycle (worker fake via sh, sem Python real) ---

// fakeEnsureModels stub do bootstrap de modelos (sem download).
func fakeEnsureModels(ctx context.Context, modelsDir string) (map[string]string, error) {
	return map[string]string{"safety-xs-v1": "/fake/model.onnx"}, nil
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func countLines(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "\n")
}

// readPIDFile lê o pid gravado pelo worker fake (aguardando a escrita
// completa).
func readPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0
}

// writeWorkerScript grava um worker fake (shell com shebang) em dir e retorna
// o caminho. O supervisor executa <comando> <path> <flags>; usamos o próprio
// script como comando (path vazio) para o shell interpretar o corpo.
func writeWorkerScript(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "worker.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// newLifecycleSupervisor monta um supervisor com worker fake (script shell) e
// bootstrap de modelos stubado.
func newLifecycleSupervisor(t *testing.T, body string, readyTimeout time.Duration) (*Supervisor, *config.Config, context.CancelFunc) {
	t.Helper()
	dir := t.TempDir()
	script := writeWorkerScript(t, dir, body)
	cfg := &config.Config{
		ModerationEnabled:         true,
		ModerationWorkerCommand:   script,
		ModerationWorkerPath:      "",
		ModerationSocketPath:      filepath.Join(dir, "moderation.sock"),
		ModerationModelsDir:       filepath.Join(dir, "models"),
		ModerationQueueSize:       4,
		ModerationConcurrency:     1,
		ModerationTimeout:         2 * time.Second,
		ModerationNudityMode:      ModeOff,
		ModerationGoreMode:        ModeOff,
		ModerationNudityThreshold: 0.8,
		ModerationGoreThreshold:   0.8,
	}
	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sup := NewSupervisor(cfg)
	sup.ensureModels = fakeEnsureModels
	sup.readyTimeout = readyTimeout
	return sup, cfg, cancel
}

// TestServiceStopTerminatesSupervisor é a regressão do deadlock: Service.Stop()
// deve sinalizar o supervisor ANTES do wg.Wait() e encerrar o worker vivo.
func TestServiceStopTerminatesSupervisor(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "worker.pid")
	script := writeWorkerScript(t, dir, fmt.Sprintf("echo $$ > %s\nsleep 300", pidFile))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := &config.Config{
		ModerationWorkerCommand: script,
		ModerationWorkerPath:    "",
		ModerationSocketPath:    filepath.Join(dir, "moderation.sock"),
		ModerationModelsDir:     filepath.Join(dir, "models"),
	}
	s := New(cfg, ctx)
	s.sup.ensureModels = fakeEnsureModels
	s.sup.readyTimeout = 30 * time.Second

	// Mesma goroutine que Service.Start() inicia para o supervisor.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.sup.Run(ctx)
	}()

	pid := readPIDFile(t, pidFile, 5*time.Second)
	if pid == 0 {
		t.Fatal("worker fake não iniciou (pid file ausente)")
	}
	if !processAlive(pid) {
		t.Fatal("worker fake deveria estar vivo")
	}

	// Stop() deve retornar (antes da correção: deadlock no wg.Wait()).
	stopped := make(chan struct{})
	go func() {
		s.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(15 * time.Second):
		t.Fatal("deadlock: Service.Stop() não retornou")
	}

	if processAlive(pid) {
		t.Fatal("o worker deveria ter sido encerrado pelo Stop()")
	}
}

// TestSupervisorRestartsDyingWorker: worker que morre no startup deve ser
// detectado imediatamente (e não após o readyTimeout) e reiniciado.
func TestSupervisorRestartsDyingWorker(t *testing.T) {
	dir := t.TempDir()
	counterFile := filepath.Join(dir, "starts")
	sup, _, _ := newLifecycleSupervisor(t, "echo x >> "+counterFile, 10*time.Second)

	go sup.Run(context.Background())
	t.Cleanup(sup.Stop)

	// O worker morre em cada startup; com backoff 1s/2s, 3 startups cabem em
	// poucos segundos (antes da correção o 2º só acontecia após ~120s).
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if countLines(counterFile) >= 3 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("worker que morre no startup não foi reiniciado com rapidez (starts=%d)", countLines(counterFile))
}

// TestSupervisorKillsWorkerOnReadinessTimeout: worker vivo que nunca fica
// ready deve ser encerrado no timeout de readiness (e não aguardado para
// sempre) e o supervisor deve reiniciar.
func TestSupervisorKillsWorkerOnReadinessTimeout(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "worker.pid")
	counterFile := filepath.Join(dir, "starts")
	workerScript := fmt.Sprintf("echo x >> %s; echo $$ > %s; sleep 300", counterFile, pidFile)

	sup, _, _ := newLifecycleSupervisor(t, workerScript, 2*time.Second)
	go sup.Run(context.Background())
	t.Cleanup(sup.Stop)

	firstPID := readPIDFile(t, pidFile, 5*time.Second)
	if firstPID == 0 {
		t.Fatal("worker fake não iniciou")
	}

	// Timeout (2s) + backoff (1s): o 2º startup e a morte do 1º cabem em 15s.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if countLines(counterFile) >= 2 && !processAlive(firstPID) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("worker preso em readiness não foi encerrado (starts=%d, vivo=%v)",
		countLines(counterFile), processAlive(firstPID))
}

// TestSupervisorRetriesModelBootstrap: falha no download do modelo não
// impede o serviço — o supervisor retry com backoff até o bootstrap passar e
// o worker iniciar.
func TestSupervisorRetriesModelBootstrap(t *testing.T) {
	dir := t.TempDir()
	counterFile := filepath.Join(dir, "starts")
	sup, _, _ := newLifecycleSupervisor(t, "echo x >> "+counterFile, 10*time.Second)

	fails := 0
	sup.ensureModels = func(ctx context.Context, modelsDir string) (map[string]string, error) {
		if fails < 2 {
			fails++
			return nil, errors.New("download indisponível")
		}
		return fakeEnsureModels(ctx, modelsDir)
	}

	go sup.Run(context.Background())
	t.Cleanup(sup.Stop)

	// 2 falhas (backoff 1s + 2s) e o worker sobe na 3ª tentativa.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if countLines(counterFile) >= 1 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("worker não iniciou após o bootstrap do modelo ser recuperado")
}
