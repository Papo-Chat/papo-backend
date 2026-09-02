package storage

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/utils"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// migrationsDir é o caminho relativo ao diretório deste pacote (backend/internal/storage/test_storage).
const migrationsDir = "../../../migrations"

// defaultDatabaseURL corresponde aos padrões do infra/docker-compose.yml.
const defaultDatabaseURL = "postgres://papo:papo123@localhost:5432/papo"

func TestMain(m *testing.M) {
	os.Exit(runStorageTests(m))
}

// runStorageTests prepara um banco temporário com as migrations do projeto,
// inicializa o storage contra ele, executa os testes e remove o banco ao final.
func runStorageTests(m *testing.M) int {
	baseURL := testDatabaseURL()

	baseDB, err := sql.Open("pgx", baseURL)
	if err != nil {
		fmt.Printf("testes de storage ignorados: falha ao abrir conexão: %v\n", err)
		return 0
	}
	defer baseDB.Close()

	if err := ping(baseDB); err != nil {
		fmt.Printf("testes de storage ignorados: não foi possível conectar ao PostgreSQL (%v). Inicie o PostgreSQL (infra/docker-compose.yml) ou defina TEST_DATABASE_URL/DATABASE_URL.\n", err)
		return 0
	}

	removeOldTempDatabases(baseDB)

	tempDBName, err := createTempDatabase(baseDB)
	if err != nil {
		fmt.Printf("testes de storage ignorados: falha ao criar banco temporário: %v\n", err)
		return 0
	}
	defer dropTempDatabase(baseDB, tempDBName)

	tempURL, err := withDatabase(baseURL, tempDBName)
	if err != nil {
		fmt.Printf("testes de storage ignorados: %v\n", err)
		return 0
	}

	tempDB, err := sql.Open("pgx", tempURL)
	if err != nil {
		fmt.Printf("testes de storage ignorados: %v\n", err)
		return 0
	}
	defer tempDB.Close()

	if err := ping(tempDB); err != nil {
		fmt.Printf("testes de storage ignorados: falha ao conectar no banco temporário: %v\n", err)
		return 0
	}

	if err := applyMigrations(tempDB); err != nil {
		fmt.Printf("testes de storage FALHARAM na preparação: %v\n", err)
		return 1
	}

	if err := InitDB(tempURL); err != nil {
		fmt.Printf("testes de storage FALHARAM na preparação: %v\n", err)
		return 1
	}

	code := m.Run()

	CloseDB()
	return code
}

// testDatabaseURL resolve a DSN base: TEST_DATABASE_URL > DATABASE_URL > padrão do docker-compose.
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

// createTempDatabase cria um banco isolado para os testes, evitando poluir os dados de desenvolvimento.
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

// removeOldTempDatabases limpa bancos deixados por execuções anteriores interrompidas.
func removeOldTempDatabases(db *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, `SELECT datname FROM pg_database WHERE datname LIKE 'papo\_test\_%'`)
	if err != nil {
		fmt.Printf("falha ao remover banco temporário antigo: %v", err)
		return
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			fmt.Printf("falha ao remover banco temporário antigo: %v", err)
			return
		}
		names = append(names, name)
	}

	if err := rows.Err(); err != nil {
		fmt.Printf("falha ao remover banco temporário antigo: %v", err)
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

// --- helpers de teste ---

func testCtx() context.Context {
	return context.Background()
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// randUUID gera um UUID v4 válido para consultas de "registro inexistente".
func randUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func strPtr(s string) *string        { return &s }
func intPtr(i int) *int              { return &i }
func timePtr(t time.Time) *time.Time { return &t }

func newTestUser(t *testing.T) models.User {
	t.Helper()
	user, _, err := CreateUser(testCtx(), "user_"+randHex(8), "hash_"+randHex(8), "123.123.123.123")
	if err != nil {
		t.Fatalf("falha ao criar usuário de apoio: %v", err)
	}
	return user
}

// newTestMedia insere um registro de mídia (tabela media) com o conteúdo
// informado e retorna o sha256 (hex) — referência usada por avatar, ícone,
// emoji, attachment, thumbnail e link preview.
func newTestMedia(t *testing.T, content []byte) string {
	t.Helper()
	return newTestMediaWithMime(t, content, "image/png")
}

// newTestMediaWithMime é o mesmo de newTestMedia, com o MIME type informado
// (o mime_type da tabela media é o que os joins de attachment/thumbnail
// expõem como mime_type).
func newTestMediaWithMime(t *testing.T, content []byte, mimeType string) string {
	t.Helper()
	cfg := config.LoadConfig()

	mac := hmac.New(sha256.New, []byte(cfg.HMACSecret))
	mac.Write(content)

	hash := hex.EncodeToString(mac.Sum(nil))

	if _, _, err := InsertMediaIfAbsent(testCtx(), hash, mimeType, int64(len(content))); err != nil {
		t.Fatalf("falha ao inserir mídia de apoio: %v", err)
	}
	return hash
}

// wipeAppTables remove todo o estado de servidores/canais/roles/emojis
// (ordem segura para as FKs). Usuários, user_settings e media não são
// removidos. Necessário porque o banco de teste é compartilhado entre os
// testes do pacote e agora existe apenas um servidor (singleton).
func wipeAppTables(t *testing.T) {
	t.Helper()
	for _, table := range []string{
		"user_roles",
		"roles",
		"attachment_thumbnails",
		"attachments",
		"messages",
		"user_channel_state",
		"channels",
		"emojis",
		"servers",
	} {
		if _, err := GetDB().ExecContext(testCtx(), "DELETE FROM "+table); err != nil {
			t.Fatalf("falha ao limpar tabela %s: %v", table, err)
		}
	}
}

func newTestServer(t *testing.T, ownerID *string) models.Server {
	t.Helper()
	wipeAppTables(t)
	server, err := CreateServer(testCtx(), "server_"+randHex(8), ownerID)
	if err != nil {
		t.Fatalf("falha ao criar servidor de apoio: %v", err)
	}
	return server
}

func newTestChannel(t *testing.T) models.Channel {
	t.Helper()
	channel, err := CreateChannel(testCtx(), "channel_"+randHex(8), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal de apoio: %v", err)
	}
	return channel
}

func newTestRole(t *testing.T) models.Role {
	t.Helper()
	role, err := CreateRole(testCtx(), "role_"+randHex(8), strPtr("#123456"), models.RolePermissions{ManageRoles: true})
	if err != nil {
		t.Fatalf("falha ao criar role de apoio: %v", err)
	}
	return role
}

// --- users ---

func TestCreateUser(t *testing.T) {
	username := "user_" + randHex(8)
	user, settings, err := CreateUser(testCtx(), username, "hash_abc", "123.123.123.123")
	if err != nil {
		t.Fatalf("CreateUser retornou erro: %v", err)
	}

	if user.ID == "" {
		t.Error("esperava user.ID preenchido")
	}
	if user.Username != username {
		t.Errorf("esperava username %q, obtive %q", username, user.Username)
	}
	if user.PasswordHash != "" {
		t.Errorf("CreateUser não deve retornar password_hash, obtive %q", user.PasswordHash)
	}
	if user.Banned {
		t.Error("esperava banned = false")
	}
	if user.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}

	if settings.UserID != user.ID {
		t.Errorf("esperava settings.user_id = %s, obtive %s", user.ID, settings.UserID)
	}
	if settings.Version != models.CurrentVersion {
		t.Errorf("esperava settings.version = %d, obtive %d", models.CurrentVersion, settings.Version)
	}
	if settings.Config != (models.UserConfig{}) {
		t.Errorf("esperava settings.config vazio, obtive %+v", settings.Config)
	}
	if settings.UpdatedAt.IsZero() {
		t.Error("esperava settings.updated_at preenchido")
	}
}

func TestCreateUserDuplicateUsername(t *testing.T) {
	username := "user_" + randHex(8)
	if _, _, err := CreateUser(testCtx(), username, "hash_1", "123.123.123.123"); err != nil {
		t.Fatalf("falha ao criar primeiro usuário: %v", err)
	}

	_, _, err := CreateUser(testCtx(), username, "hash_2", "123.123.123.123")
	if !errors.Is(err, ErrUniqueViolation) {
		t.Errorf("esperava ErrUniqueViolation, obtive %v", err)
	}
}

func TestGetUserByID(t *testing.T) {
	user := newTestUser(t)

	got, err := GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if got.ID != user.ID || got.Username != user.Username {
		t.Errorf("usuário retornado não confere: got %+v, want ID=%s username=%s", got, user.ID, user.Username)
	}
	if got.PasswordHash != "" {
		t.Errorf("GetUserByID não deve retornar password_hash, obtive %q", got.PasswordHash)
	}

	if _, err := GetUserByID(testCtx(), randUUID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestGetUsersByIDs(t *testing.T) {
	u1 := newTestUser(t)
	u2 := newTestUser(t)

	users, err := GetUsersByIDs(testCtx(), []string{u1.ID, u2.ID, randUUID()})
	if err != nil {
		t.Fatalf("GetUsersByIDs retornou erro: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("esperava 2 usuários (id inexistente deve ser pulado), obtive %d", len(users))
	}
	set := make(map[string]models.User, len(users))
	for _, u := range users {
		set[u.ID] = u
		if u.PasswordHash != "" {
			t.Errorf("GetUsersByIDs não deve retornar password_hash, obtive %q", u.PasswordHash)
		}
	}
	for _, want := range []models.User{u1, u2} {
		got, ok := set[want.ID]
		if !ok {
			t.Errorf("usuário %s não foi retornado", want.ID)
			continue
		}
		if got.Username != want.Username {
			t.Errorf("esperava username %q, obtive %q", want.Username, got.Username)
		}
	}
}

func TestGetUsersByIDSEmpty(t *testing.T) {
	users, err := GetUsersByIDs(testCtx(), nil)
	if err != nil {
		t.Fatalf("GetUsersByIDs retornou erro: %v", err)
	}
	if len(users) != 0 {
		t.Errorf("esperava lista vazia, obtive %d usuários", len(users))
	}
}

func TestGetUserByUsername(t *testing.T) {
	hash := "hash_" + randHex(8)
	user, _, err := CreateUser(testCtx(), "user_"+randHex(8), hash, "123.123.123.123")
	if err != nil {
		t.Fatalf("falha ao criar usuário de apoio: %v", err)
	}

	got, err := GetUserByUsername(testCtx(), user.Username)
	if err != nil {
		t.Fatalf("GetUserByUsername retornou erro: %v", err)
	}
	if got.ID != user.ID {
		t.Errorf("esperava id %s, obtive %s", user.ID, got.ID)
	}
	// GetUserByUsername é a única função autorizada a retornar o hash do banco
	if got.PasswordHash != hash {
		t.Errorf("esperava password_hash %q, obtive %q", hash, got.PasswordHash)
	}

	if _, err := GetUserByUsername(testCtx(), "user_"+randHex(8)); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para username inexistente, obtive %v", err)
	}
}

func TestListUsers(t *testing.T) {
	userA := newTestUser(t)
	userB := newTestUser(t)

	users, err := ListUsers(testCtx(), nil, "", 100)
	if err != nil {
		t.Fatalf("ListUsers retornou erro: %v", err)
	}
	if len(users) < 2 {
		t.Fatalf("esperava pelo menos 2 usuários, obtive %d", len(users))
	}

	byID := make(map[string]models.UserSummary, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}

	for _, want := range []models.User{userA, userB} {
		got, ok := byID[want.ID]
		if !ok {
			t.Errorf("usuário %s (%s) não aparece na listagem", want.ID, want.Username)
			continue
		}
		if got.Username != want.Username {
			t.Errorf("esperava username %q, obtive %q", want.Username, got.Username)
		}
		if got.Nickname != nil {
			t.Errorf("esperava nickname nil, obtive %q", *got.Nickname)
		}
		if got.StatusMessage != nil {
			t.Errorf("esperava status_message nil, obtive %q", *got.StatusMessage)
		}
		if got.StatusUpdatedAt != nil {
			t.Errorf("esperava status_updated_at nil, obtive %v", *got.StatusUpdatedAt)
		}
		if got.CreatedAt.IsZero() {
			t.Error("esperava created_at preenchido")
		}
	}
}

func TestUpdateUser(t *testing.T) {
	user := newTestUser(t)
	nickname := "nick_" + randHex(4)
	status := "status_" + randHex(4)
	avatar := []byte{0x89, 0x50, 0x4e, 0x47}
	updatedAt := time.Now().UTC().Truncate(time.Millisecond)

	updated, err := UpdateUser(testCtx(), user.ID, models.User{
		Nickname:        strPtr(nickname),
		AvatarBlob:      avatar,
		AvatarFormat:    "PNG",
		StatusMessage:   strPtr(status),
		StatusUpdatedAt: timePtr(updatedAt),
	})
	if err != nil {
		t.Fatalf("UpdateUser retornou erro: %v", err)
	}

	if updated.ID != user.ID {
		t.Errorf("esperava id %s, obtive %s", user.ID, updated.ID)
	}
	if updated.Nickname == nil || *updated.Nickname != nickname {
		t.Errorf("esperava nickname %q, obtive %v", nickname, updated.Nickname)
	}
	if updated.StatusMessage == nil || *updated.StatusMessage != status {
		t.Errorf("esperava status_message %q, obtive %v", status, updated.StatusMessage)
	}
	if updated.StatusUpdatedAt == nil || !updated.StatusUpdatedAt.Equal(updatedAt) {
		t.Errorf("esperava status_updated_at %v, obtive %v", updatedAt, updated.StatusUpdatedAt)
	}
	if updated.Username != user.Username {
		t.Errorf("username deveria permanecer %q, obtive %q", user.Username, updated.Username)
	}

	if _, err := UpdateUser(testCtx(), randUUID(), models.User{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestUpdateUserAvatar(t *testing.T) {
	user := newTestUser(t)
	hash := newTestMedia(t, []byte{0x89, 0x50, 0x4e, 0x47})

	if err := UpdateUserAvatar(testCtx(), &hash, user.ID); err != nil {
		t.Fatalf("UpdateUserAvatar retornou erro: %v", err)
	}

	got, err := GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if got.AvatarMedia == nil || *got.AvatarMedia != hash {
		t.Errorf("esperava avatar_media %s, obtive %v", hash, got.AvatarMedia)
	}
	if got.Username != user.Username {
		t.Errorf("username deveria permanecer %q, obtive %q", user.Username, got.Username)
	}

	if err := UpdateUserAvatar(testCtx(), nil, user.ID); err != nil {
		t.Fatalf("UpdateUserAvatar(remove) retornou erro: %v", err)
	}
	got, err = GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if got.AvatarMedia != nil {
		t.Errorf("esperava avatar_media nil, obtive %v", got.AvatarMedia)
	}

	if err := UpdateUserAvatar(testCtx(), &hash, randUUID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestUpdateUserBanner(t *testing.T) {
	user := newTestUser(t)
	hash := newTestMedia(t, []byte{0x89, 0x50, 0x4e, 0x47})

	if err := UpdateUserBanner(testCtx(), &hash, user.ID); err != nil {
		t.Fatalf("UpdateUserBanner retornou erro: %v", err)
	}

	got, err := GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if got.BannerMedia == nil || *got.BannerMedia != hash {
		t.Errorf("esperava banner_media %s, obtive %v", hash, got.BannerMedia)
	}

	if err := UpdateUserBanner(testCtx(), nil, user.ID); err != nil {
		t.Fatalf("UpdateUserBanner(remove) retornou erro: %v", err)
	}
	got, err = GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if got.BannerMedia != nil {
		t.Errorf("esperava banner_media nil, obtive %v", got.BannerMedia)
	}

	if err := UpdateUserBanner(testCtx(), &hash, randUUID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestUpdateUserStatus(t *testing.T) {
	user := newTestUser(t)

	if user.Status != nil {
		t.Fatalf("usuário novo deveria nascer sem status persistido, obtive %v", user.Status)
	}

	away := "away"
	if err := UpdateUserStatus(testCtx(), user.ID, &away); err != nil {
		t.Fatalf("UpdateUserStatus(away) retornou erro: %v", err)
	}
	got, err := GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if got.Status == nil || *got.Status != "away" {
		t.Errorf("esperava status %q, obtive %v", "away", got.Status)
	}
	// status_message não é alterada por UpdateUserStatus
	if got.StatusMessage != nil {
		t.Errorf("status_message deveria permanecer nil, obtive %v", got.StatusMessage)
	}

	busy := "busy"
	if err := UpdateUserStatus(testCtx(), user.ID, &busy); err != nil {
		t.Fatalf("UpdateUserStatus(busy) retornou erro: %v", err)
	}
	got, err = GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if got.Status == nil || *got.Status != "busy" {
		t.Errorf("esperava status %q, obtive %v", "busy", got.Status)
	}

	if err := UpdateUserStatus(testCtx(), user.ID, nil); err != nil {
		t.Fatalf("UpdateUserStatus(nil) retornou erro: %v", err)
	}
	got, err = GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if got.Status != nil {
		t.Errorf("esperava status nil após remoção, obtive %v", got.Status)
	}

	if err := UpdateUserStatus(testCtx(), randUUID(), &away); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

// --- media ---

func TestInsertMediaIfAbsent(t *testing.T) {
	content := []byte("conteúdo de mídia de teste")
	cfg := config.LoadConfig()

	mac := hmac.New(sha256.New, []byte(cfg.HMACSecret))
	mac.Write(content)

	hash := hex.EncodeToString(mac.Sum(nil))

	media, created, err := InsertMediaIfAbsent(testCtx(), hash, "image/png", int64(len(content)))
	if err != nil {
		t.Fatalf("InsertMediaIfAbsent retornou erro: %v", err)
	}
	if !created {
		t.Error("a primeira inserção deveria retornar created=true")
	}
	if media.ShaHash != hash {
		t.Errorf("esperava sha_hash %s, obtive %s", hash, media.ShaHash)
	}
	if media.MimeType != "image/png" {
		t.Errorf("esperava mime_type %q, obtive %q", "image/png", media.MimeType)
	}
	if media.SizeBytes != int64(len(content)) {
		t.Errorf("esperava size_bytes %d, obtive %d", len(content), media.SizeBytes)
	}
	if media.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}

	// mesmo hash: deduplicação, sem nova linha
	again, created, err := InsertMediaIfAbsent(testCtx(), hash, "image/png", int64(len(content)))
	if err != nil {
		t.Fatalf("InsertMediaIfAbsent (repetida) retornou erro: %v", err)
	}
	if created {
		t.Error("a inserção repetida deveria retornar created=false")
	}
	if again.CreatedAt != media.CreatedAt {
		t.Errorf("a inserção repetida deveria retornar o registro original: got %v, want %v", again.CreatedAt, media.CreatedAt)
	}

	// conteúdo diferente: hash diferente, linha nova
	other := []byte("outro conteúdo")
	otherMac := hmac.New(sha256.New, []byte(cfg.HMACSecret))
	otherMac.Write(other)

	otherHash := hex.EncodeToString(otherMac.Sum(nil))

	if otherHash == hash {
		t.Fatal("hashes de conteúdos diferentes deveriam diferir")
	}
	if _, created, err := InsertMediaIfAbsent(testCtx(), otherHash, "image/gif", int64(len(other))); err != nil || !created {
		t.Errorf("conteúdo diferente deveria criar nova linha (created=%v, err=%v)", created, err)
	}
}

func TestGetMediaByHash(t *testing.T) {
	hash := newTestMedia(t, []byte("mídia de busca"))

	media, err := GetMediaByHash(testCtx(), hash)
	if err != nil {
		t.Fatalf("GetMediaByHash retornou erro: %v", err)
	}
	if media.ShaHash != hash || media.MimeType != "image/png" || media.SizeBytes != int64(len("mídia de busca")) {
		t.Errorf("registro inesperado: %+v", media)
	}

	if _, err := GetMediaByHash(testCtx(), strings.Repeat("0", 64)); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para hash inexistente, obtive %v", err)
	}
}

func TestUpdateUserLastIP(t *testing.T) {
	user := newTestUser(t)
	ip := "2001:db8::1"

	if err := UpdateUserLastIP(testCtx(), user.ID, ip); err != nil {
		t.Fatalf("UpdateUserLastIP retornou erro: %v", err)
	}

	got, err := GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if got.LastIP == nil || *got.LastIP != ip {
		t.Errorf("esperava last_ip %q, obtive %v", ip, got.LastIP)
	}

	if err := UpdateUserLastIP(testCtx(), randUUID(), ip); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestSetUserBanned(t *testing.T) {
	user := newTestUser(t)

	if _, err := SetUserBanned(testCtx(), user.ID, true); err != nil {
		t.Fatalf("SetUserBanned(true) retornou erro: %v", err)
	}
	got, err := GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if !got.Banned {
		t.Error("esperava banned = true")
	}

	if _, err := SetUserBanned(testCtx(), user.ID, false); err != nil {
		t.Fatalf("SetUserBanned(false) retornou erro: %v", err)
	}
	got, err = GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if got.Banned {
		t.Error("esperava banned = false")
	}

	if _, err := SetUserBanned(testCtx(), randUUID(), true); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestSetUserResetPassword(t *testing.T) {
	user := newTestUser(t)

	if err := SetUserResetPassword(testCtx(), user.ID); err != nil {
		t.Fatalf("SetUserResetPassword retornou erro: %v", err)
	}
	got, err := GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if !got.ResetPassword {
		t.Error("esperava reset_password = true")
	}

	if err := SetUserResetPassword(testCtx(), randUUID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestUpdateUserPassword(t *testing.T) {
	user := newTestUser(t)
	newHash := "hash_" + randHex(8)

	// marca o usuário para reset e confirma que a troca de senha reinicia a flag
	if err := SetUserResetPassword(testCtx(), user.ID); err != nil {
		t.Fatalf("SetUserResetPassword retornou erro: %v", err)
	}

	if err := UpdateUserPassword(testCtx(), user.ID, newHash); err != nil {
		t.Fatalf("UpdateUserPassword retornou erro: %v", err)
	}
	got, err := GetUserByUsername(testCtx(), user.Username)
	if err != nil {
		t.Fatalf("GetUserByUsername retornou erro: %v", err)
	}
	if got.PasswordHash != newHash {
		t.Errorf("esperava password_hash %q, obtive %q", newHash, got.PasswordHash)
	}
	if got.ResetPassword {
		t.Error("esperava reset_password = false após trocar a senha")
	}

	if err := UpdateUserPassword(testCtx(), randUUID(), newHash); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

// --- servers ---

func TestCreateServer(t *testing.T) {
	wipeAppTables(t)
	owner := newTestUser(t)
	name := "server_" + randHex(8)

	server, err := CreateServer(testCtx(), name, &owner.ID)
	if err != nil {
		t.Fatalf("CreateServer retornou erro: %v", err)
	}
	if server.ID == "" {
		t.Error("esperava server.ID preenchido")
	}
	if server.Name != name {
		t.Errorf("esperava name %q, obtive %q", name, server.Name)
	}
	if server.OwnerID == nil || *server.OwnerID != owner.ID {
		t.Errorf("esperava owner_id %s, obtive %v", owner.ID, server.OwnerID)
	}
	if server.IconFormat != "" {
		t.Errorf("esperava icon_format vazio, obtive %q", server.IconFormat)
	}
	if server.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}

	// owner_id é opcional
	wipeAppTables(t)
	serverNoOwner, err := CreateServer(testCtx(), "server_"+randHex(8), nil)
	if err != nil {
		t.Fatalf("CreateServer sem owner retornou erro: %v", err)
	}
	if serverNoOwner.OwnerID != nil {
		t.Errorf("esperava owner_id nil, obtive %v", serverNoOwner.OwnerID)
	}
}

func TestCreateServerWithIcon(t *testing.T) {
	wipeAppTables(t)
	owner := newTestUser(t)
	name := "server_" + randHex(8)
	iconHash := newTestMedia(t, []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a})

	server, err := CreateServerWithIcon(testCtx(), name, &iconHash, true, &owner.ID, nil)
	if err != nil {
		t.Fatalf("CreateServerWithIcon retornou erro: %v", err)
	}
	if server.ID == "" {
		t.Error("esperava server.ID preenchido")
	}
	if server.Name != name {
		t.Errorf("esperava name %q, obtive %q", name, server.Name)
	}
	if server.OwnerID == nil || *server.OwnerID != owner.ID {
		t.Errorf("esperava owner_id %s, obtive %v", owner.ID, server.OwnerID)
	}
	if server.IconMedia == nil || *server.IconMedia != iconHash {
		t.Errorf("esperava icon_media %s, obtive %v", iconHash, server.IconMedia)
	}
	if server.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}

	// sem ícone: icon_media nil
	wipeAppTables(t)
	noIcon, err := CreateServerWithIcon(testCtx(), "server_"+randHex(8), nil, true, &owner.ID, nil)
	if err != nil {
		t.Fatalf("CreateServerWithIcon sem ícone retornou erro: %v", err)
	}
	if noIcon.IconMedia != nil {
		t.Errorf("esperava icon_media nil, obtive %v", noIcon.IconMedia)
	}
}

func TestUserOwnsAnyServer(t *testing.T) {
	owner := newTestUser(t)
	newTestServer(t, &owner.ID)

	owns, err := UserOwnsAnyServer(testCtx(), owner.ID)
	if err != nil {
		t.Fatalf("UserOwnsAnyServer retornou erro: %v", err)
	}
	if !owns {
		t.Error("esperava owns = true para o dono de um servidor")
	}

	// usuário sem servidor não é dono de nenhum
	other := newTestUser(t)
	owns, err = UserOwnsAnyServer(testCtx(), other.ID)
	if err != nil {
		t.Fatalf("UserOwnsAnyServer retornou erro: %v", err)
	}
	if owns {
		t.Error("esperava owns = false para usuário sem servidor")
	}

	// id inexistente não é dono de nenhum servidor
	owns, err = UserOwnsAnyServer(testCtx(), randUUID())
	if err != nil {
		t.Fatalf("UserOwnsAnyServer retornou erro para id inexistente: %v", err)
	}
	if owns {
		t.Error("esperava owns = false para id inexistente")
	}
}

func TestUpdateServer(t *testing.T) {
	owner := newTestUser(t)
	server := newTestServer(t, &owner.ID)
	newName := "server_" + randHex(8)
	iconHash := newTestMedia(t, []byte{0xff, 0xd8, 0xff})

	updated, err := UpdateServer(testCtx(), server.ID, models.Server{
		Name:         newName,
		IconMedia:    &iconHash,
		PublicServer: true,
	}, nil)
	if err != nil {
		t.Fatalf("UpdateServer retornou erro: %v", err)
	}
	if updated.ID != server.ID {
		t.Errorf("esperava id %s, obtive %s", server.ID, updated.ID)
	}
	if updated.Name != newName {
		t.Errorf("esperava name %q, obtive %q", newName, updated.Name)
	}
	if updated.IconMedia == nil || *updated.IconMedia != iconHash {
		t.Errorf("esperava icon_media %s, obtive %v", iconHash, updated.IconMedia)
	}
	if updated.OwnerID == nil || *updated.OwnerID != owner.ID {
		t.Errorf("owner_id deveria permanecer %s, obtive %v", owner.ID, updated.OwnerID)
	}

	if _, err := UpdateServer(testCtx(), randUUID(), models.Server{}, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestGetServerSummary(t *testing.T) {
	owner := newTestUser(t)
	server := newTestServer(t, &owner.ID)
	newTestChannel(t)
	newTestRole(t)

	summary, err := GetServerSummary(testCtx())
	if err != nil {
		t.Fatalf("GetServerSummary retornou erro: %v", err)
	}

	if summary.ID != server.ID {
		t.Errorf("esperava id %s, obtive %s", server.ID, summary.ID)
	}
	if summary.Name != server.Name {
		t.Errorf("esperava name %q, obtive %q", server.Name, summary.Name)
	}
	if summary.OwnerID == nil || *summary.OwnerID != owner.ID {
		t.Errorf("esperava owner_id %s, obtive %v", owner.ID, summary.OwnerID)
	}
	if summary.OwnerUsername == nil || *summary.OwnerUsername != owner.Username {
		t.Errorf("esperava owner_username %q, obtive %v", owner.Username, summary.OwnerUsername)
	}
	if summary.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}

	// as contagens devem refletir o estado atual do banco
	channels, err := ListChannels(testCtx())
	if err != nil {
		t.Fatalf("ListChannelsByServer retornou erro: %v", err)
	}
	if summary.ChannelCount != len(channels) {
		t.Errorf("esperava channel_count %d, obtive %d", len(channels), summary.ChannelCount)
	}

	roles, err := ListRoles(testCtx())
	if err != nil {
		t.Fatalf("ListRolesByServer retornou erro: %v", err)
	}
	if summary.RoleCount != len(roles) {
		t.Errorf("esperava role_count %d, obtive %d", len(roles), summary.RoleCount)
	}

	// por enquanto todos os usuários pertencem ao mesmo servidor:
	// member_count é o total de usuários
	users, err := ListUsers(testCtx(), nil, "", 100)
	if err != nil {
		t.Fatalf("ListUsers retornou erro: %v", err)
	}
	if summary.MemberCount != len(users) {
		t.Errorf("esperava member_count %d, obtive %d", len(users), summary.MemberCount)
	}

	wipeAppTables(t)
	if _, err := GetServerSummary(testCtx()); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound sem servidor criado, obtive %v", err)
	}
}

// --- channels ---

func TestCreateChannel(t *testing.T) {
	_ = newTestServer(t, nil)
	name := "channel_" + randHex(8)

	channel, err := CreateChannel(testCtx(), name, "text", "")
	if err != nil {
		t.Fatalf("CreateChannel retornou erro: %v", err)
	}
	if channel.ID == "" {
		t.Error("esperava channel.ID preenchido")
	}

	if channel.Name != name {
		t.Errorf("esperava name %q, obtive %q", name, channel.Name)
	}
	if channel.Type != "text" {
		t.Errorf("esperava type %q, obtive %q", "text", channel.Type)
	}
	if channel.Permissions == nil || len(channel.Permissions) != 0 {
		t.Errorf("esperava permissions vazio, obtive %v", channel.Permissions)
	}
	if channel.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}

	category, err := CreateChannel(testCtx(), "channel_"+randHex(8), "category", "")
	if err != nil {
		t.Fatalf("CreateChannel(category) retornou erro: %v", err)
	}
	if category.Type != "category" {
		t.Errorf("esperava type %q, obtive %q", "category", category.Type)
	}

	if _, err := CreateChannel(testCtx(), "channel_"+randHex(8), "voice", ""); err == nil {
		t.Error("esperava erro para type inválido, obtive nil")
	}
}

func TestCreateChannelDuplicateName(t *testing.T) {
	_ = newTestServer(t, nil)
	name := "channel_" + randHex(8)

	if _, err := CreateChannel(testCtx(), name, "text", ""); err != nil {
		t.Fatalf("falha ao criar primeiro canal: %v", err)
	}
	if _, err := CreateChannel(testCtx(), name, "text", ""); !errors.Is(err, ErrUniqueViolation) {
		t.Errorf("esperava ErrUniqueViolation, obtive %v", err)
	}
}

func TestCreateChannelTopic(t *testing.T) {
	_ = newTestServer(t, nil)

	topic := "tópico de canal"
	channel, err := CreateChannel(testCtx(), "channel_"+randHex(8), "text", topic)
	if err != nil {
		t.Fatalf("CreateChannel com topic retornou erro: %v", err)
	}
	if channel.Topic == nil || *channel.Topic != topic {
		t.Errorf("esperava topic %q, obtive %v", topic, channel.Topic)
	}

	// topic vazio é gravado como NULL
	noTopic, err := CreateChannel(testCtx(), "channel_"+randHex(8), "text", "")
	if err != nil {
		t.Fatalf("CreateChannel sem topic retornou erro: %v", err)
	}
	if noTopic.Topic != nil {
		t.Errorf("esperava topic nil, obtive %v", noTopic.Topic)
	}
}

func TestChannelsTopicCheckConstraint(t *testing.T) {
	_ = newTestServer(t, nil)

	// o banco rejeita canal category com topic (CHECK constraint)
	_, err := GetDB().ExecContext(testCtx(),
		`INSERT INTO channels (name, type, position, topic)
		 VALUES ($1, 'category', 1, 'topic')`,
		"check_"+randHex(8))
	if err == nil {
		t.Error("esperava erro do banco para canal category com topic, obtive nil")
	}
}

func TestGetChannelByID(t *testing.T) {
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)

	got, err := GetChannelByID(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if got.ID != channel.ID || got.Name != channel.Name || got.Type != channel.Type {
		t.Errorf("canal retornado não confere: got %+v, want ID=%s name=%s", got, channel.ID, channel.Name)
	}

	if _, err := GetChannelByID(testCtx(), randUUID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestListChannels(t *testing.T) {
	newTestServer(t, nil)
	c1 := newTestChannel(t)
	c2 := newTestChannel(t)

	channels, err := ListChannels(testCtx())
	if err != nil {
		t.Fatalf("ListChannels retornou erro: %v", err)
	}

	ids := make(map[string]bool, len(channels))
	for _, c := range channels {
		ids[c.ID] = true
	}
	if !ids[c1.ID] || !ids[c2.ID] {
		t.Errorf("ListChannels não retornou os canais criados: got %v", ids)
	}
}

func TestUpdateChannel(t *testing.T) {
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)
	role := newTestRole(t)
	perm := models.ChannelPermission{ReadChannel: true, SendMessages: true}
	if _, err := UpdateChannelPermissions(testCtx(), channel.ID, role.ID, perm); err != nil {
		t.Fatalf("falha ao configurar permissões do canal: %v", err)
	}

	newName := "channel_" + randHex(8)
	updated, err := UpdateChannel(testCtx(), channel.ID, newName, nil)
	if err != nil {
		t.Fatalf("UpdateChannel retornou erro: %v", err)
	}
	if updated.ID != channel.ID {
		t.Errorf("esperava id %s, obtive %s", channel.ID, updated.ID)
	}

	if updated.Name != newName {
		t.Errorf("esperava name %q, obtive %q", newName, updated.Name)
	}
	if updated.Type != channel.Type {
		t.Errorf("esperava type %q, obtive %q", channel.Type, updated.Type)
	}
	if updated.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}
	if got := updated.Permissions[role.ID]; got != perm {
		t.Errorf("esperava permissões preservadas %+v, obtive %+v", perm, got)
	}

	// o novo nome deve estar persistido
	stored, err := GetChannelByID(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Name != newName {
		t.Errorf("esperava name persistido %q, obtive %q", newName, stored.Name)
	}
}

func TestUpdateChannelDuplicateName(t *testing.T) {
	newTestServer(t, nil)
	c1 := newTestChannel(t)
	c2 := newTestChannel(t)

	// a constraint UNIQUE de channels.name é global
	if _, err := UpdateChannel(testCtx(), c1.ID, c2.Name, nil); !errors.Is(err, ErrUniqueViolation) {
		t.Errorf("esperava ErrUniqueViolation, obtive %v", err)
	}

	// o rename recusado não deve alterar o nome original
	stored, err := GetChannelByID(testCtx(), c1.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Name != c1.Name {
		t.Errorf("esperava name original %q, obtive %q", c1.Name, stored.Name)
	}
}

func TestUpdateChannelNotFound(t *testing.T) {
	if _, err := UpdateChannel(testCtx(), randUUID(), "channel_"+randHex(8), nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestUpdateChannelPermissions(t *testing.T) {
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)
	role := newTestRole(t)

	perm := models.ChannelPermission{ReadChannel: true, SendMessages: true}
	updated, err := UpdateChannelPermissions(testCtx(), channel.ID, role.ID, perm)
	if err != nil {
		t.Fatalf("UpdateChannelPermissions retornou erro: %v", err)
	}
	if got := updated.Permissions[role.ID]; got != perm {
		t.Errorf("esperava permissions[%s] = %+v, obtive %+v", role.ID, perm, got)
	}

	// uma nova atualização substitui as permissões da mesma role
	perm2 := models.ChannelPermission{ReadChannel: true}
	updated, err = UpdateChannelPermissions(testCtx(), channel.ID, role.ID, perm2)
	if err != nil {
		t.Fatalf("UpdateChannelPermissions (segunda) retornou erro: %v", err)
	}
	if got := updated.Permissions[role.ID]; got != perm2 {
		t.Errorf("esperava permissions[%s] = %+v, obtive %+v", role.ID, perm2, got)
	}

	if _, err := UpdateChannelPermissions(testCtx(), randUUID(), role.ID, perm); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para canal inexistente, obtive %v", err)
	}
}

func TestDeleteChannel(t *testing.T) {
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)

	if err := DeleteChannel(testCtx(), channel.ID); err != nil {
		t.Fatalf("DeleteChannel retornou erro: %v", err)
	}
	if _, err := GetChannelByID(testCtx(), channel.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound após exclusão, obtive %v", err)
	}
	if err := DeleteChannel(testCtx(), channel.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound ao excluir novamente, obtive %v", err)
	}
}

// --- ChangeChannelPosition (tarefa 8.4) ---

func TestChangeChannelPositionMoveDown(t *testing.T) {
	_ = newTestServer(t, nil)
	c1 := newTestChannel(t)
	c2 := newTestChannel(t)
	c3 := newTestChannel(t)

	updated, err := ChangeChannelPosition(testCtx(), c1.ID, 1, 3)
	if err != nil {
		t.Fatalf("ChangeChannelPosition retornou erro: %v", err)
	}
	if updated.ID != c1.ID || updated.Position != 3 {
		t.Errorf("esperava canal %s na posição 3, obtive %s na posição %d", c1.ID, updated.ID, updated.Position)
	}

	channels, err := ListChannels(testCtx())
	if err != nil {
		t.Fatalf("ListChannelsByServer retornou erro: %v", err)
	}
	expected := []string{c2.ID, c3.ID, c1.ID}
	for i, want := range expected {
		if channels[i].ID != want {
			t.Errorf("posição %d: esperava canal %s, obtive %s", i+1, want, channels[i].ID)
		}
		if channels[i].Position != i+1 {
			t.Errorf("posição %d: esperava position %d, obtive %d", i+1, i+1, channels[i].Position)
		}
	}
}

func TestChangeChannelPositionMoveUp(t *testing.T) {
	_ = newTestServer(t, nil)
	c1 := newTestChannel(t)
	c2 := newTestChannel(t)
	c3 := newTestChannel(t)

	updated, err := ChangeChannelPosition(testCtx(), c3.ID, 3, 1)
	if err != nil {
		t.Fatalf("ChangeChannelPosition retornou erro: %v", err)
	}
	if updated.ID != c3.ID || updated.Position != 1 {
		t.Errorf("esperava canal %s na posição 1, obtive %s na posição %d", c3.ID, updated.ID, updated.Position)
	}

	channels, err := ListChannels(testCtx())
	if err != nil {
		t.Fatalf("ListChannelsByServer retornou erro: %v", err)
	}
	expected := []string{c3.ID, c1.ID, c2.ID}
	for i, want := range expected {
		if channels[i].ID != want {
			t.Errorf("posição %d: esperava canal %s, obtive %s", i+1, want, channels[i].ID)
		}
		if channels[i].Position != i+1 {
			t.Errorf("posição %d: esperava position %d, obtive %d", i+1, i+1, channels[i].Position)
		}
	}
}

func TestChangeChannelPositionSamePosition(t *testing.T) {
	_ = newTestServer(t, nil)
	c1 := newTestChannel(t)
	c2 := newTestChannel(t)

	updated, err := ChangeChannelPosition(testCtx(), c2.ID, 2, 2)
	if err != nil {
		t.Fatalf("ChangeChannelPosition retornou erro: %v", err)
	}
	if updated.Position != 2 {
		t.Errorf("esperava posição 2, obtive %d", updated.Position)
	}

	channels, err := ListChannels(testCtx())
	if err != nil {
		t.Fatalf("ListChannelsByServer retornou erro: %v", err)
	}
	if channels[0].ID != c1.ID || channels[1].ID != c2.ID {
		t.Errorf("ordem alterada pela operação inofensiva: %+v", channels)
	}
}

func TestChangeChannelPositionConflict(t *testing.T) {
	_ = newTestServer(t, nil)
	c1 := newTestChannel(t)
	c2 := newTestChannel(t)
	c3 := newTestChannel(t)

	if _, err := ChangeChannelPosition(testCtx(), c1.ID, 2, 3); !errors.Is(err, ErrPositionConflict) {
		t.Fatalf("esperava ErrPositionConflict, obtive %v", err)
	}

	channels, err := ListChannels(testCtx())
	if err != nil {
		t.Fatalf("ListChannelsByServer retornou erro: %v", err)
	}
	expected := []string{c1.ID, c2.ID, c3.ID}
	for i, want := range expected {
		if channels[i].ID != want || channels[i].Position != i+1 {
			t.Errorf("ordem alterada pelo conflito: %+v", channels)
		}
	}
}

func TestChangeChannelPositionInvalidNewPosition(t *testing.T) {
	_ = newTestServer(t, nil)
	c1 := newTestChannel(t)

	for _, tc := range []struct {
		name string
		old  int
		new  int
	}{
		{"acima do último", 1, 2},
		{"zero", 1, 0},
		{"negativo", 1, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ChangeChannelPosition(testCtx(), c1.ID, tc.old, tc.new); !errors.Is(err, ErrInvalidPosition) {
				t.Errorf("esperava ErrInvalidPosition, obtive %v", err)
			}
		})
	}

	stored, err := GetChannelByID(testCtx(), c1.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Position != 1 {
		t.Errorf("esperava posição 1 inalterada, obtive %d", stored.Position)
	}
}

func TestChangeChannelPositionNotFound(t *testing.T) {
	if _, err := ChangeChannelPosition(testCtx(), randUUID(), 1, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestListChannelSummaries(t *testing.T) {
	author := newTestUser(t)
	newTestServer(t, nil)
	c1 := newTestChannel(t)
	c2 := newTestChannel(t)
	role := newTestRole(t)
	perm := models.ChannelPermission{ReadChannel: true, SendMessages: true}

	if _, err := UpdateChannelPermissions(testCtx(), c1.ID, role.ID, perm); err != nil {
		t.Fatalf("falha ao configurar permissões do canal: %v", err)
	}
	// garante timestamps distintos para a última mensagem
	time.Sleep(10 * time.Millisecond)
	if _, err := CreateMessage(testCtx(), c1.ID, author.ID, "primeira", "", nil); err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	m2, err := CreateMessage(testCtx(), c1.ID, author.ID, "segunda", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	// retorna todos os canais criados
	summaries, err := ListChannelSummaries(testCtx())
	if err != nil {
		t.Fatalf("ListChannelSummaries retornou erro: %v", err)
	}

	byID := make(map[string]models.ChannelSummary, len(summaries))
	for i := range summaries {
		byID[summaries[i].ID] = summaries[i]
	}

	// canal com mensagem e permissões expandidas
	got, ok := byID[c1.ID]
	if !ok {
		t.Fatal("canal c1 não aparece em ListChannelSummaries")
	}

	if got.Name != c1.Name {
		t.Errorf("esperava name %q, obtive %q", c1.Name, got.Name)
	}
	if got.Type != c1.Type {
		t.Errorf("esperava type %q, obtive %q", c1.Type, got.Type)
	}
	if !got.CreatedAt.Equal(c1.CreatedAt) {
		t.Errorf("esperava created_at %v, obtive %v", c1.CreatedAt, got.CreatedAt)
	}

	// permissões expandidas: role com nome e permissões configuradas
	if len(got.Permissions) != 1 {
		t.Fatalf("esperava 1 permissão, obtive %d", len(got.Permissions))
	}
	entry := got.Permissions[0]
	if entry.RoleID != role.ID {
		t.Errorf("esperava role_id %s, obtive %s", role.ID, entry.RoleID)
	}
	if entry.RoleName != role.Name {
		t.Errorf("esperava role_name %q, obtive %q", role.Name, entry.RoleName)
	}
	if entry.Permissions != perm {
		t.Errorf("esperava permissions %+v, obtive %+v", perm, entry.Permissions)
	}

	// última mensagem: a mais recente do canal
	if got.LastMessage == nil {
		t.Fatal("esperava last_message preenchida, obtive nil")
	}
	if got.LastMessage.ID != m2.ID {
		t.Errorf("esperava last_message.id %s, obtive %s", m2.ID, got.LastMessage.ID)
	}
	if got.LastMessage.Content == nil {
		t.Errorf("esperava last_message.content")
	}

	if *got.LastMessage.Content != "segunda" {
		t.Errorf("esperava last_message.content %q, obtive %q", "segunda", *got.LastMessage.Content)
	}
	if got.LastMessage.AuthorID == nil || *got.LastMessage.AuthorID != author.ID {
		t.Errorf("esperava author_id %s, obtive %v", author.ID, got.LastMessage.AuthorID)
	}
	if got.LastMessage.AuthorUsername == nil || *got.LastMessage.AuthorUsername != author.Username {
		t.Errorf("esperava author_username %q, obtive %v", author.Username, got.LastMessage.AuthorUsername)
	}

	// canal sem mensagens: last_message nil
	got2, ok := byID[c2.ID]
	if !ok {
		t.Fatal("canal c2 não aparece em ListChannelSummaries")
	}
	if got2.LastMessage != nil {
		t.Errorf("esperava last_message nil, obtive %+v", got2.LastMessage)
	}
	if got2.Permissions == nil || len(got2.Permissions) != 0 {
		t.Errorf("esperava permissions vazio, obtive %v", got2.Permissions)
	}

}

func TestGetChannelSummary(t *testing.T) {
	author := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)
	role := newTestRole(t)
	perm := models.ChannelPermission{SendMessages: true, DeleteMessages: true}

	if _, err := UpdateChannelPermissions(testCtx(), channel.ID, role.ID, perm); err != nil {
		t.Fatalf("falha ao configurar permissões do canal: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	message, err := CreateMessage(testCtx(), channel.ID, author.ID, "mensagem única", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	summary, err := GetChannelSummary(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelSummary retornou erro: %v", err)
	}

	if summary.ID != channel.ID {
		t.Errorf("esperava id %s, obtive %s", channel.ID, summary.ID)
	}

	if summary.Name != channel.Name {
		t.Errorf("esperava name %q, obtive %q", channel.Name, summary.Name)
	}
	if summary.Type != channel.Type {
		t.Errorf("esperava type %q, obtive %q", channel.Type, summary.Type)
	}
	if !summary.CreatedAt.Equal(channel.CreatedAt) {
		t.Errorf("esperava created_at %v, obtive %v", channel.CreatedAt, summary.CreatedAt)
	}

	if len(summary.Permissions) != 1 {
		t.Fatalf("esperava 1 permissão, obtive %d", len(summary.Permissions))
	}
	if summary.Permissions[0].RoleID != role.ID {
		t.Errorf("esperava role_id %s, obtive %s", role.ID, summary.Permissions[0].RoleID)
	}
	if summary.Permissions[0].RoleName != role.Name {
		t.Errorf("esperava role_name %q, obtive %q", role.Name, summary.Permissions[0].RoleName)
	}
	if summary.Permissions[0].Permissions != perm {
		t.Errorf("esperava permissions %+v, obtive %+v", perm, summary.Permissions[0].Permissions)
	}

	if summary.LastMessage == nil {
		t.Fatal("esperava last_message preenchida, obtive nil")
	}
	if summary.LastMessage.ID != message.ID {
		t.Errorf("esperava last_message.id %s, obtive %s", message.ID, summary.LastMessage.ID)
	}
	if summary.LastMessage.Content == nil {
		t.Errorf("esperava last_message.content")
	}
	if *summary.LastMessage.Content != "mensagem única" {
		t.Errorf("esperava last_message.content %q, obtive %q", "mensagem única", *summary.LastMessage.Content)
	}
	if summary.LastMessage.AuthorID == nil || *summary.LastMessage.AuthorID != author.ID {
		t.Errorf("esperava author_id %s, obtive %v", author.ID, summary.LastMessage.AuthorID)
	}
	if summary.LastMessage.AuthorUsername == nil || *summary.LastMessage.AuthorUsername != author.Username {
		t.Errorf("esperava author_username %q, obtive %v", author.Username, summary.LastMessage.AuthorUsername)
	}

	// canal sem mensagens: last_message nil
	emptyChannel := newTestChannel(t)
	emptySummary, err := GetChannelSummary(testCtx(), emptyChannel.ID)
	if err != nil {
		t.Fatalf("GetChannelSummary (sem mensagens) retornou erro: %v", err)
	}
	if emptySummary.LastMessage != nil {
		t.Errorf("esperava last_message nil, obtive %+v", emptySummary.LastMessage)
	}
	if emptySummary.Permissions == nil || len(emptySummary.Permissions) != 0 {
		t.Errorf("esperava permissions vazio, obtive %v", emptySummary.Permissions)
	}

	if _, err := GetChannelSummary(testCtx(), randUUID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

// --- messages ---

// --- roles ---

func TestCreateRole(t *testing.T) {
	_ = newTestServer(t, nil)
	name := "role_" + randHex(8)
	perms := models.RolePermissions{ManageChannels: true, BanMembers: true}

	role, err := CreateRole(testCtx(), name, strPtr("#ff0000"), perms)
	if err != nil {
		t.Fatalf("CreateRole retornou erro: %v", err)
	}
	if role.ID == "" {
		t.Error("esperava role.ID preenchido")
	}

	if role.Name != name {
		t.Errorf("esperava name %q, obtive %q", name, role.Name)
	}
	if role.Color == nil || *role.Color != "#ff0000" {
		t.Errorf("esperava color %q, obtive %v", "#ff0000", role.Color)
	}
	if role.Permissions != perms {
		t.Errorf("esperava permissions %+v, obtive %+v", perms, role.Permissions)
	}
	if role.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}

	// color é opcional
	roleNoColor, err := CreateRole(testCtx(), "role_"+randHex(8), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("CreateRole sem color retornou erro: %v", err)
	}
	if roleNoColor.Color != nil {
		t.Errorf("esperava color nil, obtive %v", roleNoColor.Color)
	}
}

func TestCreateRoleDuplicateName(t *testing.T) {
	_ = newTestServer(t, nil)
	name := "role_" + randHex(8)

	if _, err := CreateRole(testCtx(), name, nil, models.RolePermissions{}); err != nil {
		t.Fatalf("falha ao criar primeira role: %v", err)
	}
	if _, err := CreateRole(testCtx(), name, nil, models.RolePermissions{}); !errors.Is(err, ErrUniqueViolation) {
		t.Errorf("esperava ErrUniqueViolation, obtive %v", err)
	}
}

// listRoleNames é usado por ListChannelSummaries/GetChannelSummary para
// expandir as permissões dos canais com o nome de cada role.
// TestListRoleNames verifica a expansão de nomes de roles nas permissões dos
// canais, exercitando listRoleNames (não exportado) via GetChannelSummary e
// ListChannelSummaries.
func TestListRoleNames(t *testing.T) {
	newTestServer(t, nil)
	r1 := newTestRole(t)
	r2 := newTestRole(t)
	r3 := newTestRole(t)
	channel := newTestChannel(t)
	perm1 := models.ChannelPermission{ReadChannel: true}
	perm2 := models.ChannelPermission{SendMessages: true, DeleteMessages: true}

	if _, err := UpdateChannelPermissions(testCtx(), channel.ID, r1.ID, perm1); err != nil {
		t.Fatalf("falha ao configurar permissões da role r1: %v", err)
	}
	if _, err := UpdateChannelPermissions(testCtx(), channel.ID, r2.ID, perm2); err != nil {
		t.Fatalf("falha ao configurar permissões da role r2: %v", err)
	}
	// a terceira role também é resolvida pelo nome (mapa global)
	if _, err := UpdateChannelPermissions(testCtx(), channel.ID, r3.ID, perm1); err != nil {
		t.Fatalf("falha ao configurar permissões da role r3: %v", err)
	}

	summary, err := GetChannelSummary(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelSummary retornou erro: %v", err)
	}
	if len(summary.Permissions) != 3 {
		t.Fatalf("esperava 3 permissões, obtive %d", len(summary.Permissions))
	}

	byRole := make(map[string]models.ChannelPermissionEntry, len(summary.Permissions))
	for _, entry := range summary.Permissions {
		byRole[entry.RoleID] = entry
	}
	for role, wantPerm := range map[string]models.ChannelPermission{
		r1.ID: perm1,
		r2.ID: perm2,
		r3.ID: perm1,
	} {
		entry, ok := byRole[role]
		if !ok {
			t.Errorf("permissão da role %s não aparece no summary", role)
			continue
		}
		wantName, err := GetRoleByID(testCtx(), role)
		if err != nil {
			t.Fatalf("falha ao buscar role de apoio: %v", err)
		}
		if entry.RoleName != wantName.Name {
			t.Errorf("esperava role_name %q, obtive %q", wantName.Name, entry.RoleName)
		}
		if entry.Permissions != wantPerm {
			t.Errorf("esperava permissions %+v, obtive %+v", wantPerm, entry.Permissions)
		}
	}

	// ListChannelSummaries também expande os nomes
	summaries, err := ListChannelSummaries(testCtx())
	if err != nil {
		t.Fatalf("ListChannelSummaries retornou erro: %v", err)
	}
	var found *models.ChannelSummary
	for i := range summaries {
		if summaries[i].ID == channel.ID {
			found = &summaries[i]
		}
	}
	if found == nil {
		t.Fatal("canal não aparece em ListChannelSummaries")
	}
	if len(found.Permissions) != 3 {
		t.Errorf("esperava 3 permissões na listagem, obtive %d", len(found.Permissions))
	}
}

func TestGetRoleByID(t *testing.T) {
	_ = newTestServer(t, nil)
	role := newTestRole(t)

	got, err := GetRoleByID(testCtx(), role.ID)
	if err != nil {
		t.Fatalf("GetRoleByID retornou erro: %v", err)
	}
	if got.ID != role.ID || got.Name != role.Name {
		t.Errorf("role retornado não confere: got %+v, want ID=%s name=%s", got, role.ID, role.Name)
	}

	if _, err := GetRoleByID(testCtx(), randUUID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestListRoles(t *testing.T) {
	newTestServer(t, nil)
	r1 := newTestRole(t)
	r2 := newTestRole(t)
	newTestRole(t)

	roles, err := ListRoles(testCtx())
	if err != nil {
		t.Fatalf("ListRoles retornou erro: %v", err)
	}

	ids := make(map[string]bool, len(roles))
	for _, r := range roles {
		ids[r.ID] = true
	}
	if !ids[r1.ID] || !ids[r2.ID] {
		t.Errorf("ListRoles não retornou as roles criadas: got %v", ids)
	}
}

func TestUpdateRole(t *testing.T) {
	_ = newTestServer(t, nil)
	role := newTestRole(t)
	newName := "role_" + randHex(8)
	newPerms := models.RolePermissions{ManageServer: true, PinMessage: true}

	updated, err := UpdateRole(testCtx(), role.ID, models.Role{
		Name:        newName,
		Color:       strPtr("#00ff00"),
		Permissions: newPerms,
	})
	if err != nil {
		t.Fatalf("UpdateRole retornou erro: %v", err)
	}
	if updated.ID != role.ID {
		t.Errorf("esperava id %s, obtive %s", role.ID, updated.ID)
	}
	if updated.Name != newName {
		t.Errorf("esperava name %q, obtive %q", newName, updated.Name)
	}
	if updated.Color == nil || *updated.Color != "#00ff00" {
		t.Errorf("esperava color %q, obtive %v", "#00ff00", updated.Color)
	}
	if updated.Permissions != newPerms {
		t.Errorf("esperava permissions %+v, obtive %+v", newPerms, updated.Permissions)
	}

	if _, err := UpdateRole(testCtx(), randUUID(), models.Role{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestDeleteRole(t *testing.T) {
	_ = newTestServer(t, nil)
	user := newTestUser(t)
	role := newTestRole(t)
	channel := newTestChannel(t)

	// a role tem permissão no canal e é atribuída ao usuário
	if _, err := UpdateChannelPermissions(testCtx(), channel.ID, role.ID, models.ChannelPermission{ReadChannel: true}); err != nil {
		t.Fatalf("falha ao configurar permissões do canal: %v", err)
	}
	if _, err := AssignUserRole(testCtx(), user.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}

	if err := DeleteRole(testCtx(), role.ID); err != nil {
		t.Fatalf("DeleteRole retornou erro: %v", err)
	}
	if _, err := GetRoleByID(testCtx(), role.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound após exclusão, obtive %v", err)
	}

	// a entrada da role é removida das permissões dos canais do servidor
	updatedChannel, err := GetChannelByID(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if _, exists := updatedChannel.Permissions[role.ID]; exists {
		t.Error("esperava a role removida das permissões do canal")
	}

	// cascade: a atribuição em user_roles é removida
	roles, err := GetRolesByUser(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetRolesByUser retornou erro: %v", err)
	}
	if len(roles) != 0 {
		t.Errorf("esperava user_roles esvaziado após DeleteRole, obtive %v", roles)
	}

	if err := DeleteRole(testCtx(), randUUID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestAssignUserRole(t *testing.T) {
	user := newTestUser(t)
	_ = newTestServer(t, nil)
	role := newTestRole(t)

	userRole, err := AssignUserRole(testCtx(), user.ID, role.ID)
	if err != nil {
		t.Fatalf("AssignUserRole retornou erro: %v", err)
	}
	if userRole.UserID != user.ID {
		t.Errorf("esperava user_id %s, obtive %s", user.ID, userRole.UserID)
	}
	if userRole.RoleID != role.ID {
		t.Errorf("esperava role_id %s, obtive %s", role.ID, userRole.RoleID)
	}
	if userRole.AssignedAt.IsZero() {
		t.Error("esperava assigned_at preenchido")
	}

	// atribuir a mesma role duas vezes viola a PK (user_id, role_id)
	if _, err := AssignUserRole(testCtx(), user.ID, role.ID); !errors.Is(err, ErrUniqueViolation) {
		t.Errorf("esperava ErrUniqueViolation para atribuição duplicada, obtive %v", err)
	}
}

func TestRemoveUserRole(t *testing.T) {
	user := newTestUser(t)
	_ = newTestServer(t, nil)
	role := newTestRole(t)
	if _, err := AssignUserRole(testCtx(), user.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role de apoio: %v", err)
	}

	if err := RemoveUserRole(testCtx(), user.ID, role.ID); err != nil {
		t.Fatalf("RemoveUserRole retornou erro: %v", err)
	}
	userIDs, err := GetUsersByRole(testCtx(), role.ID)
	if err != nil {
		t.Fatalf("GetUsersByRole retornou erro: %v", err)
	}
	if len(userIDs) != 0 {
		t.Errorf("esperava role sem usuários após remoção, obtive %v", userIDs)
	}
	if err := RemoveUserRole(testCtx(), user.ID, role.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound ao remover novamente, obtive %v", err)
	}
}

func TestGetRolesByUser(t *testing.T) {
	user := newTestUser(t)
	_ = newTestServer(t, nil)
	r1 := newTestRole(t)
	r2 := newTestRole(t)
	newTestRole(t)

	if _, err := AssignUserRole(testCtx(), user.ID, r1.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}
	if _, err := AssignUserRole(testCtx(), user.ID, r2.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}

	roles, err := GetRolesByUser(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetRolesByUser retornou erro: %v", err)
	}
	if len(roles) != 2 {
		t.Fatalf("esperava 2 roles, obtive %d", len(roles))
	}
	ids := make(map[string]bool, len(roles))
	for _, r := range roles {
		ids[r.ID] = true
	}
	if !ids[r1.ID] || !ids[r2.ID] {
		t.Errorf("GetRolesByUser não retornou as roles atribuídas: got %v", ids)
	}

	// usuário sem roles retorna lista vazia
	empty, err := GetRolesByUser(testCtx(), newTestUser(t).ID)
	if err != nil {
		t.Fatalf("GetRolesByUser retornou erro: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("esperava lista vazia, obtive %v", empty)
	}
}

func TestGetUsersByRole(t *testing.T) {
	u1 := newTestUser(t)
	u2 := newTestUser(t)
	_ = newTestServer(t, nil)
	role := newTestRole(t)

	if _, err := AssignUserRole(testCtx(), u1.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}
	if _, err := AssignUserRole(testCtx(), u2.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}

	userIDs, err := GetUsersByRole(testCtx(), role.ID)
	if err != nil {
		t.Fatalf("GetUsersByRole retornou erro: %v", err)
	}
	if len(userIDs) != 2 {
		t.Fatalf("esperava 2 usuários, obtive %d", len(userIDs))
	}
	set := make(map[string]bool, len(userIDs))
	for _, id := range userIDs {
		set[id] = true
	}
	if !set[u1.ID] || !set[u2.ID] {
		t.Errorf("GetUsersByRole não retornou os usuários atribuídos: got %v", set)
	}
}

// --- emojis ---

func TestCreateEmoji(t *testing.T) {
	creator := newTestUser(t)
	_ = newTestServer(t, nil)
	name := "emoji_" + randHex(8)
	imageHash := newTestMedia(t, []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a})

	emoji, err := CreateEmoji(testCtx(), name, imageHash, &creator.ID)
	if err != nil {
		t.Fatalf("CreateEmoji retornou erro: %v", err)
	}
	if emoji.ID == "" {
		t.Error("esperava emoji.ID preenchido")
	}

	if emoji.Name != name {
		t.Errorf("esperava name %q, obtive %q", name, emoji.Name)
	}
	if emoji.ImageMedia != imageHash {
		t.Errorf("esperava image_media %s, obtive %s", imageHash, emoji.ImageMedia)
	}
	if emoji.MimeType != "image/png" {
		t.Errorf("esperava mime_type %q (join media), obtive %q", "image/png", emoji.MimeType)
	}
	if emoji.CreatedBy == nil || *emoji.CreatedBy != creator.ID {
		t.Errorf("esperava created_by %s, obtive %v", creator.ID, emoji.CreatedBy)
	}
	if emoji.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}
}

func TestCreateEmojiDuplicateName(t *testing.T) {
	creator := newTestUser(t)
	_ = newTestServer(t, nil)
	name := "emoji_" + randHex(8)

	firstHash := newTestMedia(t, []byte{1})
	secondHash := newTestMedia(t, []byte{2})
	if firstHash == secondHash {
		t.Fatal("mídias de conteúdos diferentes deveriam ter hashes distintos")
	}
	if _, err := CreateEmoji(testCtx(), name, firstHash, &creator.ID); err != nil {
		t.Fatalf("falha ao criar primeiro emoji: %v", err)
	}
	if _, err := CreateEmoji(testCtx(), name, secondHash, &creator.ID); !errors.Is(err, ErrUniqueViolation) {
		t.Errorf("esperava ErrUniqueViolation, obtive %v", err)
	}
}

func TestGetEmojiByID(t *testing.T) {
	creator := newTestUser(t)
	_ = newTestServer(t, nil)
	imageHash := newTestMedia(t, []byte{0x47, 0x49, 0x46})
	created, err := CreateEmoji(testCtx(), "emoji_"+randHex(8), imageHash, &creator.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji de apoio: %v", err)
	}

	got, err := GetEmojiByID(testCtx(), created.ID)
	if err != nil {
		t.Fatalf("GetEmojiByID retornou erro: %v", err)
	}
	if got.ID != created.ID || got.Name != created.Name || got.ImageMedia != created.ImageMedia {
		t.Errorf("emoji retornado não confere: got %+v, want ID=%s name=%s", got, created.ID, created.Name)
	}

	if _, err := GetEmojiByID(testCtx(), randUUID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestListEmojis(t *testing.T) {
	creator := newTestUser(t)
	newTestServer(t, nil)

	h1 := newTestMedia(t, []byte{1})
	h2 := newTestMedia(t, []byte{2})
	h3 := newTestMedia(t, []byte{3})
	h4 := newTestMedia(t, []byte{4})
	e1, err := CreateEmoji(testCtx(), "emoji_"+randHex(8), h1, &creator.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	e2, err := CreateEmoji(testCtx(), "emoji_"+randHex(8), h2, &creator.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	e3, err := CreateEmoji(testCtx(), "emoji_"+randHex(8), h3, &creator.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}
	other, err := CreateEmoji(testCtx(), "emoji_"+randHex(8), h4, &creator.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}

	// sem filtros: todos os emojis, em ordem (created_at, id)
	all, err := ListEmojis(testCtx(), nil, "", 0)
	if err != nil {
		t.Fatalf("ListEmojis retornou erro: %v", err)
	}
	pos := make(map[string]int, len(all))
	for i, e := range all {
		if _, ok := pos[e.ID]; ok {
			t.Errorf("emoji %s duplicado na listagem", e.ID)
		}
		pos[e.ID] = i
	}
	for _, want := range []string{e1.ID, e2.ID, e3.ID, other.ID} {
		if _, ok := pos[want]; !ok {
			t.Errorf("ListEmojis não retornou o emoji %s: got %v", want, pos)
		}
	}
	if pos[e1.ID] > pos[e2.ID] || pos[e2.ID] > pos[e3.ID] {
		t.Errorf("ordem inesperada (esperava created_at ascendente): %+v", pos)
	}

	// limit
	limited, err := ListEmojis(testCtx(), nil, "", 2)
	if err != nil {
		t.Fatalf("ListEmojis com limit retornou erro: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("esperava 2 emojis com limit, obtive %d", len(limited))
	}

	// since: apenas emojis criados após e2
	since := e2.CreatedAt
	after, err := ListEmojis(testCtx(), &since, "", 0)
	if err != nil {
		t.Fatalf("ListEmojis com since retornou erro: %v", err)
	}
	afterIDs := make(map[string]bool, len(after))
	for _, e := range after {
		afterIDs[e.ID] = true
	}
	if afterIDs[e1.ID] || afterIDs[e2.ID] {
		t.Errorf("since deveria excluir e1/e2: got %v", afterIDs)
	}
	if !afterIDs[e3.ID] || !afterIDs[other.ID] {
		t.Errorf("since deveria incluir e3 e other: got %v", afterIDs)
	}

	// since + last_id: cursor composto para emojis com created_at igual
	if _, err := GetDB().ExecContext(testCtx(),
		"UPDATE emojis SET created_at = $1 WHERE id = $2", e2.CreatedAt, e3.ID); err != nil {
		t.Fatalf("falha ao igualar created_at: %v", err)
	}
	// e2 e e3 agora têm o mesmo timestamp; o cursor (created_at, id) deve
	// posicionar pela ordem dos ids
	boundary, next := e2, e3
	if e3.ID < e2.ID {
		boundary, next = e3, e2
	}

	afterBoundary, err := ListEmojis(testCtx(), &since, boundary.ID, 0)
	if err != nil {
		t.Fatalf("ListEmojis com cursor composto retornou erro: %v", err)
	}
	abIDs := make(map[string]bool, len(afterBoundary))
	for _, e := range afterBoundary {
		abIDs[e.ID] = true
	}
	if !abIDs[next.ID] {
		t.Errorf("cursor (since, last_id) deveria incluir o emoji %s do mesmo timestamp, got %v", next.ID, abIDs)
	}
	if abIDs[boundary.ID] {
		t.Errorf("cursor (since, last_id) deveria excluir o emoji do cursor, got %v", abIDs)
	}

	// last_id sem since é ignorado: o resultado deve ser igual ao da
	// listagem sem filtros (independente de emojis deixados por outros testes)
	ignored, err := ListEmojis(testCtx(), nil, boundary.ID, 0)
	if err != nil {
		t.Fatalf("ListEmojis com last_id e sem since retornou erro: %v", err)
	}
	ignoredIDs := make(map[string]bool, len(ignored))
	for _, e := range ignored {
		ignoredIDs[e.ID] = true
	}
	if len(ignored) != len(all) {
		t.Errorf("esperava %d emojis (last_id sem since é ignorado), obtive %d", len(all), len(ignored))
	}
	for id := range pos {
		if !ignoredIDs[id] {
			t.Errorf("last_id sem since deveria ser ignorado, mas o emoji %s não foi retornado", id)
		}
	}
}

func TestCountEmojis(t *testing.T) {
	creator := newTestUser(t)
	newTestServer(t, nil)

	h1 := newTestMedia(t, []byte{1})
	for i := 0; i < 3; i++ {
		if _, err := CreateEmoji(testCtx(), "emoji_"+randHex(8), h1, &creator.ID); err != nil {
			t.Fatalf("falha ao criar emoji: %v", err)
		}
	}

	count, err := CountEmojis(testCtx())
	if err != nil {
		t.Fatalf("CountEmojis retornou erro: %v", err)
	}
	if count != 3 {
		t.Errorf("esperava 3 emojis, obtive %d", count)
	}

	wipeAppTables(t)
	empty, err := CountEmojis(testCtx())
	if err != nil {
		t.Fatalf("CountEmojis retornou erro: %v", err)
	}
	if empty != 0 {
		t.Errorf("esperava 0 emojis sem registros, obtive %d", empty)
	}
}

func TestDeleteEmoji(t *testing.T) {
	creator := newTestUser(t)
	_ = newTestServer(t, nil)
	imageHash := newTestMedia(t, []byte{1})
	emoji, err := CreateEmoji(testCtx(), "emoji_"+randHex(8), imageHash, &creator.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}

	if err := DeleteEmoji(testCtx(), emoji.ID); err != nil {
		t.Fatalf("DeleteEmoji retornou erro: %v", err)
	}
	if _, err := GetEmojiByID(testCtx(), emoji.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound após a exclusão, obtive %v", err)
	}
	if err := DeleteEmoji(testCtx(), emoji.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound na segunda exclusão, obtive %v", err)
	}
	if err := DeleteEmoji(testCtx(), randUUID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

// --- user_settings ---

func TestGetUserSettings(t *testing.T) {
	user := newTestUser(t)

	settings, err := GetUserSettings(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserSettings retornou erro: %v", err)
	}
	if settings.UserID != user.ID {
		t.Errorf("esperava user_id %s, obtive %s", user.ID, settings.UserID)
	}
	if settings.Version != models.CurrentVersion {
		t.Errorf("esperava version %d, obtive %d", models.CurrentVersion, settings.Version)
	}
	if settings.Config != (models.UserConfig{}) {
		t.Errorf("esperava config vazio, obtive %+v", settings.Config)
	}
	if settings.UpdatedAt.IsZero() {
		t.Error("esperava updated_at preenchido")
	}

	if _, err := GetUserSettings(testCtx(), randUUID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para usuário inexistente, obtive %v", err)
	}
}

func TestUpsertUserSettings(t *testing.T) {
	user := newTestUser(t)
	config := models.UserConfig{
		Theme: "dark",
		Notifications: models.Notifications{
			Enabled:        true,
			MessagePreview: true,
			Sound:          false,
			Mentions:       true,
		},
		Display: models.Display{
			FontSize:       "medium",
			MessageDensity: "compact",
			ShowTimestamps: true,
			ShowAvatars:    false,
		},
	}

	// inserção: usuário já tem settings criadas pelo CreateUser, o upsert atualiza o config
	updated, err := UpsertUserSettings(testCtx(), user.ID, config)
	if err != nil {
		t.Fatalf("UpsertUserSettings retornou erro: %v", err)
	}
	if updated.UserID != user.ID {
		t.Errorf("esperava user_id %s, obtive %s", user.ID, updated.UserID)
	}
	if updated.Config != config {
		t.Errorf("config não confere:\n got  %+v\n want %+v", updated.Config, config)
	}
	if updated.UpdatedAt.IsZero() {
		t.Error("esperava updated_at preenchido")
	}

	// persistência: o config atualizado deve ser lido de volta
	stored, err := GetUserSettings(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserSettings retornou erro: %v", err)
	}
	if stored.Config != config {
		t.Errorf("config persistido não confere:\n got  %+v\n want %+v", stored.Config, config)
	}

	// upsert para usuário sem settings cria o registro
	newUser := newTestUser(t)
	created, err := UpsertUserSettings(testCtx(), newUser.ID, config)
	if err != nil {
		t.Fatalf("UpsertUserSettings (inserção) retornou erro: %v", err)
	}
	if created.UserID != newUser.ID {
		t.Errorf("esperava user_id %s, obtive %s", newUser.ID, created.UserID)
	}
	if created.Config != config {
		t.Errorf("config criado não confere:\n got  %+v\n want %+v", created.Config, config)
	}
}

// removeAllServersTest limpa a tabela servers (as dependências são removidas
// em cascata) para isolar os testes que dependem do estado do servidor do
// backend (1 backend = 1 servidor).
func removeAllServersTest(t *testing.T) {
	t.Helper()
	if _, err := GetDB().ExecContext(testCtx(), "DELETE FROM servers"); err != nil {
		t.Fatalf("falha ao limpar a tabela servers: %v", err)
	}
}

func TestGetServerWithPasswordHashAny(t *testing.T) {
	removeAllServersTest(t)

	// bootstrap: sem servidor no banco
	_, err := GetServerWithPasswordHash(testCtx())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("esperava ErrNotFound sem servidor, obtive %v", err)
	}

	// servidor não público: retorna o password_hash
	password := "server_pw_" + randHex(4)
	hash, err := utils.HashPassword(password)
	if err != nil {
		t.Fatalf("falha ao gerar hash da senha do servidor: %v", err)
	}
	if _, err := CreateServerWithIcon(testCtx(), "server_"+randHex(8), nil, false, nil, &hash); err != nil {
		t.Fatalf("falha ao criar servidor não público: %v", err)
	}

	server, err := GetServerWithPasswordHash(testCtx())
	if err != nil {
		t.Fatalf("GetServerWithPasswordHashAny retornou erro: %v", err)
	}
	if server.PublicServer {
		t.Error("esperava public_server = false")
	}
	if server.PasswordHash == nil || *server.PasswordHash != hash {
		t.Errorf("esperava password_hash %q, obtive %v", hash, server.PasswordHash)
	}

	// servidor público: password_hash é nil
	removeAllServersTest(t)
	if _, err := CreateServerWithIcon(testCtx(), "server_"+randHex(8), nil, true, nil, nil); err != nil {
		t.Fatalf("falha ao criar servidor público: %v", err)
	}

	server, err = GetServerWithPasswordHash(testCtx())
	if err != nil {
		t.Fatalf("GetServerWithPasswordHashAny retornou erro: %v", err)
	}
	if !server.PublicServer {
		t.Error("esperava public_server = true")
	}
	if server.PasswordHash != nil {
		t.Errorf("esperava password_hash nil para servidor público, obtive %q", *server.PasswordHash)
	}
}

// --- messages ---

func TestCreateMessage(t *testing.T) {
	author := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)

	message, err := CreateMessage(testCtx(), channel.ID, author.ID, "olá mundo", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}
	if message.ID == "" {
		t.Error("esperava id preenchido")
	}
	if message.ChannelID != channel.ID {
		t.Errorf("esperava channel_id %s, obtive %s", channel.ID, message.ChannelID)
	}
	if message.AuthorID == nil || *message.AuthorID != author.ID {
		t.Errorf("esperava author_id %s, obtive %v", author.ID, message.AuthorID)
	}
	if message.Content == nil || *message.Content != "olá mundo" {
		t.Errorf("esperava content %q, obtive %v", "olá mundo", message.Content)
	}
	if message.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}
	if message.EditedAt != nil {
		t.Errorf("esperava edited_at nil, obtive %v", message.EditedAt)
	}
}

func TestCreateMessageEmptyContent(t *testing.T) {
	author := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)

	message, err := CreateMessage(testCtx(), channel.ID, author.ID, "", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	loaded, err := GetMessageByID(testCtx(), message.ID)
	if err != nil {
		t.Fatalf("GetMessageByID retornou erro: %v", err)
	}
	if loaded.Content != nil {
		t.Errorf("esperava content NULL para content vazio, obtive %q", *loaded.Content)
	}
}

func TestCreateMessageReplyTo(t *testing.T) {
	author := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)

	target, err := CreateMessage(testCtx(), channel.ID, author.ID, "mensagem alvo", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem alvo: %v", err)
	}

	// reply_to vazio é gravado como NULL
	if target.ReplyTo != nil {
		t.Errorf("esperava reply_to nil, obtive %v", target.ReplyTo)
	}

	reply, err := CreateMessage(testCtx(), channel.ID, author.ID, "resposta", target.ID, nil)
	if err != nil {
		t.Fatalf("CreateMessage com reply_to retornou erro: %v", err)
	}
	if reply.ReplyTo == nil || *reply.ReplyTo != target.ID {
		t.Errorf("esperava reply_to %s, obtive %v", target.ID, reply.ReplyTo)
	}

	loaded, err := GetMessageByID(testCtx(), reply.ID)
	if err != nil {
		t.Fatalf("GetMessageByID retornou erro: %v", err)
	}
	if loaded.ReplyTo == nil || *loaded.ReplyTo != target.ID {
		t.Errorf("esperava reply_to %s no banco, obtive %v", target.ID, loaded.ReplyTo)
	}
}

func TestCreateMessageWithAttachments(t *testing.T) {
	author := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)

	attachmentIDs := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		attachment, err := CreateAttachment(testCtx(), models.Attachments{
			OriginalFileName: "arquivo_" + randHex(4) + ".txt",
			MediaShaHash:     newTestMedia(t, []byte("1234567890")),
			CreatedBy:        &author.ID,
		})
		if err != nil {
			t.Fatalf("falha ao criar attachment de apoio: %v", err)
		}
		attachmentIDs = append(attachmentIDs, attachment.ID)
	}

	message, err := CreateMessage(testCtx(), channel.ID, author.ID, "com arquivos", "", attachmentIDs)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	for _, attachmentID := range attachmentIDs {
		attachment, err := GetAttachmentByID(testCtx(), attachmentID)
		if err != nil {
			t.Fatalf("GetAttachmentByID retornou erro: %v", err)
		}
		if attachment.MessagesID == nil || *attachment.MessagesID != message.ID {
			t.Errorf("esperava messages_id %s, obtive %v", message.ID, attachment.MessagesID)
		}
	}
}

func TestGetMessageByID(t *testing.T) {
	author := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)

	message, err := CreateMessage(testCtx(), channel.ID, author.ID, "conteúdo", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}

	loaded, err := GetMessageByID(testCtx(), message.ID)
	if err != nil {
		t.Fatalf("GetMessageByID retornou erro: %v", err)
	}
	if loaded.ID != message.ID {
		t.Errorf("esperava id %s, obtive %s", message.ID, loaded.ID)
	}

	if _, err := GetMessageByID(testCtx(), randUUID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestListMessagesByChannel(t *testing.T) {
	author := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)

	first, err := CreateMessage(testCtx(), channel.ID, author.ID, "primeira", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar primeira mensagem: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	second, err := CreateMessage(testCtx(), channel.ID, author.ID, "segunda", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar segunda mensagem: %v", err)
	}

	// canal vizinho com mensagem que não pode vazar
	otherChannel := newTestChannel(t)
	if _, err := CreateMessage(testCtx(), otherChannel.ID, author.ID, "de outro canal", "", nil); err != nil {
		t.Fatalf("falha ao criar mensagem de outro canal: %v", err)
	}

	messages, err := ListMessagesByChannel(testCtx(), channel.ID, nil, "", nil)
	if err != nil {
		t.Fatalf("ListMessagesByChannel retornou erro: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("esperava 2 mensagens, obtive %d", len(messages))
	}
	if messages[0].ID != second.ID {
		t.Errorf("esperava a mensagem mais recente primeiro, obtive %s", messages[0].ID)
	}
	if messages[1].ID != first.ID {
		t.Errorf("esperava a mensagem mais antiga por último, obtive %s", messages[1].ID)
	}

	// limit
	if limit, err := ListMessagesByChannel(testCtx(), channel.ID, nil, "", intPtr(1)); err != nil {
		t.Fatalf("ListMessagesByChannel com limit retornou erro: %v", err)
	} else if len(limit) != 1 || limit[0].ID != second.ID {
		t.Errorf("esperava apenas a mensagem mais recente com limit 1, obtive %d", len(limit))
	}

	// since: apenas mensagens criadas após o timestamp
	since := first.CreatedAt
	sinceMessages, err := ListMessagesByChannel(testCtx(), channel.ID, timePtr(since), "", nil)
	if err != nil {
		t.Fatalf("ListMessagesByChannel com since retornou erro: %v", err)
	}
	if len(sinceMessages) != 1 || sinceMessages[0].ID != second.ID {
		t.Errorf("esperava apenas a mensagem após o since, obtive %d", len(sinceMessages))
	}
}

func TestListMessagesWithAttachmentsByChannel(t *testing.T) {
	author := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)

	// mensagem sem attachment
	plain, err := CreateMessage(testCtx(), channel.ID, author.ID, "sem attachment", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem sem attachment: %v", err)
	}

	// mensagem com dois attachments
	attachmentIDs := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		attachment, err := CreateAttachment(testCtx(), models.Attachments{
			OriginalFileName: "arquivo_" + randHex(4) + ".txt",
			MediaShaHash:     newTestMedia(t, []byte("1234567890")),
			CreatedBy:        &author.ID,
		})
		if err != nil {
			t.Fatalf("falha ao criar attachment de apoio: %v", err)
		}
		attachmentIDs = append(attachmentIDs, attachment.ID)
	}
	withAttachments, err := CreateMessage(testCtx(), channel.ID, author.ID, "com attachments", "", attachmentIDs)
	if err != nil {
		t.Fatalf("falha ao criar mensagem com attachments: %v", err)
	}

	messages, err := ListMessagesWithAttachmentsByChannel(testCtx(), channel.ID, nil, "", 100)
	if err != nil {
		t.Fatalf("ListMessagesWithAttachmentsByChannel retornou erro: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("esperava 2 mensagens, obtive %d", len(messages))
	}

	// a mais recente é a que tem attachments
	if messages[0].ID != withAttachments.ID {
		t.Fatalf("esperava a mensagem com attachments primeiro, obtive %s", messages[0].ID)
	}
	if len(messages[0].Attachments) != 2 {
		t.Fatalf("esperava 2 attachments, obtive %d", len(messages[0].Attachments))
	}
	for i, attachmentID := range attachmentIDs {
		if messages[0].Attachments[i].ID != attachmentID {
			t.Errorf("esperava attachment %s na posição %d, obtive %s", attachmentID, i, messages[0].Attachments[i].ID)
		}
	}
	if messages[1].ID != plain.ID {
		t.Errorf("esperava a mensagem sem attachment por último, obtive %s", messages[1].ID)
	}
	if len(messages[1].Attachments) != 0 {
		t.Errorf("esperava 0 attachments na mensagem simples, obtive %d", len(messages[1].Attachments))
	}

	// limit 1: busca limit+1, retorna 2 (o chamador usa para has_more)
	limited, err := ListMessagesWithAttachmentsByChannel(testCtx(), channel.ID, nil, "", 1)
	if err != nil {
		t.Fatalf("ListMessagesWithAttachmentsByChannel com limit 1 retornou erro: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("esperava limit+1 mensagens para has_more, obtive %d", len(limited))
	}

	// since: apenas a mensagem criada após o timestamp
	since := plain.CreatedAt
	sinceMessages, err := ListMessagesWithAttachmentsByChannel(testCtx(), channel.ID, timePtr(since), "", 100)
	if err != nil {
		t.Fatalf("ListMessagesWithAttachmentsByChannel com since retornou erro: %v", err)
	}
	if len(sinceMessages) != 1 || sinceMessages[0].ID != withAttachments.ID {
		t.Errorf("esperava apenas a mensagem após o since, obtive %d", len(sinceMessages))
	}
}

func TestUpdateMessage(t *testing.T) {
	author := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)

	message, err := CreateMessage(testCtx(), channel.ID, author.ID, "original", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}

	updated, err := UpdateMessage(testCtx(), message.ID, models.Message{Content: strPtr("editado")})
	if err != nil {
		t.Fatalf("UpdateMessage retornou erro: %v", err)
	}
	if updated.Content == nil || *updated.Content != "editado" {
		t.Errorf("esperava content %q, obtive %v", "editado", updated.Content)
	}
	if updated.EditedAt == nil {
		t.Error("esperava edited_at preenchido após a edição")
	}

	if _, err := UpdateMessage(testCtx(), randUUID(), models.Message{Content: strPtr("x")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestDeleteMessage(t *testing.T) {
	author := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)

	attachment, err := CreateAttachment(testCtx(), models.Attachments{
		OriginalFileName: "arquivo.txt",
		MediaShaHash:     newTestMedia(t, []byte("1234567890")),
		CreatedBy:        &author.ID,
	})
	if err != nil {
		t.Fatalf("falha ao criar attachment de apoio: %v", err)
	}
	message, err := CreateMessage(testCtx(), channel.ID, author.ID, "vai ser apagada", "", []string{attachment.ID})
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}

	if err := DeleteMessage(testCtx(), message.ID); err != nil {
		t.Fatalf("DeleteMessage retornou erro: %v", err)
	}
	if _, err := GetMessageByID(testCtx(), message.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound após a exclusão, obtive %v", err)
	}
	// attachments são removidos em cascata pela foreign key
	if _, err := GetAttachmentByID(testCtx(), attachment.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava attachment removido em cascata, obtive %v", err)
	}

	if err := DeleteMessage(testCtx(), randUUID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

// --- attachments ---

func TestCreateAttachment(t *testing.T) {
	author := newTestUser(t)
	// conteúdo exclusivo: a tabela media é append-only e deduplica por hash,
	// então reusar um conteúdo inserido por outro teste herdaria o mime dele
	content := []byte("conteúdo exclusivo do TestCreateAttachment")
	hash := newTestMediaWithMime(t, content, "text/plain")

	attachment, err := CreateAttachment(testCtx(), models.Attachments{
		OriginalFileName: "arquivo.txt",
		MediaShaHash:     hash,
		CreatedBy:        &author.ID,
	})
	if err != nil {
		t.Fatalf("CreateAttachment retornou erro: %v", err)
	}
	if attachment.ID == "" {
		t.Error("esperava id preenchido")
	}
	if attachment.OriginalFileName != "arquivo.txt" {
		t.Errorf("esperava original_file_name %q, obtive %q", "arquivo.txt", attachment.OriginalFileName)
	}
	if attachment.MediaShaHash != hash {
		t.Errorf("esperava media_sha_hash %s, obtive %s", hash, attachment.MediaShaHash)
	}
	// mime_type e size_bytes vêm do join com a tabela media
	if attachment.MimeType != "text/plain" {
		t.Errorf("esperava mime_type %q (join media), obtive %q", "text/plain", attachment.MimeType)
	}
	if attachment.SizeBytes != int64(len(content)) {
		t.Errorf("esperava size_bytes %d (join media), obtive %d", len(content), attachment.SizeBytes)
	}
	if attachment.CreatedBy == nil || *attachment.CreatedBy != author.ID {
		t.Errorf("esperava created_by %s, obtive %v", author.ID, attachment.CreatedBy)
	}
	if attachment.MessagesID != nil {
		t.Errorf("esperava messages_id nil para upload não vinculado, obtive %v", attachment.MessagesID)
	}
	if attachment.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}
}

// TestCreateAttachmentForeignMedia garante que o media_sha_hash do
// attachment referencia a tabela media (foreign key): hash inexistente é
// rejeitado.
func TestCreateAttachmentForeignMedia(t *testing.T) {
	author := newTestUser(t)

	if _, err := CreateAttachment(testCtx(), models.Attachments{
		OriginalFileName: "arquivo.txt",
		MediaShaHash:     strings.Repeat("0", 64),
		CreatedBy:        &author.ID,
	}); err == nil {
		t.Error("esperava violação de foreign key para media_sha_hash inexistente")
	}
}

func TestGetAttachmentByID(t *testing.T) {
	author := newTestUser(t)

	attachment, err := CreateAttachment(testCtx(), models.Attachments{
		OriginalFileName: "arquivo.txt",
		MediaShaHash:     newTestMedia(t, []byte("1234567890")),
		CreatedBy:        &author.ID,
	})
	if err != nil {
		t.Fatalf("falha ao criar attachment de apoio: %v", err)
	}

	loaded, err := GetAttachmentByID(testCtx(), attachment.ID)
	if err != nil {
		t.Fatalf("GetAttachmentByID retornou erro: %v", err)
	}
	if loaded.ID != attachment.ID {
		t.Errorf("esperava id %s, obtive %s", attachment.ID, loaded.ID)
	}

	if _, err := GetAttachmentByID(testCtx(), randUUID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestListAttachmentsByMessage(t *testing.T) {
	author := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)

	attachmentIDs := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		attachment, err := CreateAttachment(testCtx(), models.Attachments{
			OriginalFileName: "arquivo_" + randHex(4) + ".txt",
			MediaShaHash:     newTestMedia(t, []byte("1234567890")),
			CreatedBy:        &author.ID,
		})
		if err != nil {
			t.Fatalf("falha ao criar attachment de apoio: %v", err)
		}
		attachmentIDs = append(attachmentIDs, attachment.ID)
	}
	message, err := CreateMessage(testCtx(), channel.ID, author.ID, "com attachments", "", attachmentIDs)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}

	attachments, err := ListAttachmentsByMessage(testCtx(), message.ID)
	if err != nil {
		t.Fatalf("ListAttachmentsByMessage retornou erro: %v", err)
	}
	if len(attachments) != 2 {
		t.Fatalf("esperava 2 attachments, obtive %d", len(attachments))
	}
	for i, attachmentID := range attachmentIDs {
		if attachments[i].ID != attachmentID {
			t.Errorf("esperava attachment %s na posição %d, obtive %s", attachmentID, i, attachments[i].ID)
		}
	}

	// mensagem sem attachments: fatia vazia não nula
	emptyMessage, err := CreateMessage(testCtx(), channel.ID, author.ID, "sem attachments", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}
	empty, err := ListAttachmentsByMessage(testCtx(), emptyMessage.ID)
	if err != nil {
		t.Fatalf("ListAttachmentsByMessage retornou erro: %v", err)
	}
	if empty == nil {
		t.Fatal("esperava fatia vazia não nula, obtive nil")
	}
	if len(empty) != 0 {
		t.Errorf("esperava 0 attachments, obtive %d", len(empty))
	}
}

// --- search ---

// searchResultIDs retorna os ids dos resultados da busca ordenados (comparação por conjunto).
func searchResultIDs(results []models.SearchResult) []string {
	ids := make([]string, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	return ids
}

// assertSearchSet verifica que os resultados da busca formam exatamente o conjunto de ids esperado (ordem irrelevante).
func assertSearchSet(t *testing.T, results []models.SearchResult, want ...string) {
	t.Helper()
	got := searchResultIDs(results)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if len(got) != len(wantSorted) {
		t.Errorf("esperava %d resultados %v, obtive %d %v", len(wantSorted), wantSorted, len(got), got)
		return
	}
	for i := range got {
		if got[i] != wantSorted[i] {
			t.Errorf("esperava resultados %v, obtive %v", wantSorted, got)
			return
		}
	}
}

// assertSearchOrder verifica que os resultados da busca vêm exatamente na ordem esperada.
func assertSearchOrder(t *testing.T, results []models.SearchResult, want ...string) {
	t.Helper()
	got := make([]string, 0, len(results))
	for _, r := range results {
		got = append(got, r.ID)
	}
	if len(got) != len(want) {
		t.Errorf("esperava %d resultados %v, obtive %d %v", len(want), want, len(got), got)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("esperava ordem %v, obtive %v", want, got)
			return
		}
	}
}

func TestSearchMessages(t *testing.T) {
	// a busca é global (sem filtro de canal): limpa servidores de outros
	// testes para que canais/mensagens restantes não vazem nos resultados
	removeAllServersTest(t)
	owner := newTestUser(t)
	reader := newTestUser(t)
	stranger := newTestUser(t)
	_ = newTestServer(t, &owner.ID)
	channel := newTestChannel(t)
	restricted := newTestChannel(t)
	role := newTestRole(t)
	if _, err := UpdateChannelPermissions(testCtx(), restricted.ID, role.ID, models.ChannelPermission{ReadChannel: true}); err != nil {
		t.Fatalf("falha ao definir permissão no canal restrito: %v", err)
	}
	if _, err := AssignUserRole(testCtx(), reader.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role ao leitor: %v", err)
	}

	m1, err := CreateMessage(testCtx(), channel.ID, owner.ID, "zebra borboleta", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem 1: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	m2, err := CreateMessage(testCtx(), channel.ID, reader.ID, "borboleta vagalume", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem 2: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	m3, err := CreateMessage(testCtx(), channel.ID, stranger.ID, "vagalume", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem 3: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	attachment, err := CreateAttachment(testCtx(), models.Attachments{
		OriginalFileName: "peixe.txt",
		MediaShaHash:     newTestMediaWithMime(t, []byte("1234567890"), "text/plain"),
		CreatedBy:        &owner.ID,
	})
	if err != nil {
		t.Fatalf("falha ao criar attachment de apoio: %v", err)
	}
	m4, err := CreateMessage(testCtx(), channel.ID, owner.ID, "peixe", "", []string{attachment.ID})
	if err != nil {
		t.Fatalf("falha ao criar mensagem 4 com attachment: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	m5, err := CreateMessage(testCtx(), restricted.ID, owner.ID, "zebra secreta", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem 5 no canal restrito: %v", err)
	}

	base := func(userID string) SearchParams {
		return SearchParams{UserID: userID, Limit: 100}
	}

	t.Run("texto e score", func(t *testing.T) {
		params := base(owner.ID)
		params.Text = "zebra"
		results, err := SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages com texto retornou erro: %v", err)
		}
		assertSearchSet(t, results, m1.ID, m5.ID)
		for _, r := range results {
			if r.Type != "message" {
				t.Errorf("esperava type message, obtive %q", r.Type)
			}
			if r.Score == nil {
				t.Error("esperava score preenchido em busca textual")
			}
		}

		// sem texto: score nil
		params = base(owner.ID)
		params.AuthorID = reader.ID
		results, err = SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages sem texto retornou erro: %v", err)
		}
		assertSearchSet(t, results, m2.ID)
		if results[0].Score != nil {
			t.Error("esperava score nil sem busca textual")
		}
	})

	t.Run("apenas autor", func(t *testing.T) {
		params := base(owner.ID)
		params.AuthorID = stranger.ID
		results, err := SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages com autor retornou erro: %v", err)
		}
		assertSearchSet(t, results, m3.ID)
	})

	t.Run("intervalo de datas", func(t *testing.T) {
		params := base(owner.ID)
		params.DateStart = &m1.CreatedAt
		params.DateEndExclusive = &m5.CreatedAt
		results, err := SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages com datas retornou erro: %v", err)
		}
		assertSearchSet(t, results, m1.ID, m2.ID, m3.ID, m4.ID)

		// limite superior exclusivo: nada antes de m1
		params = base(owner.ID)
		params.DateStart = nil
		params.DateEndExclusive = &m1.CreatedAt
		results, err = SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages com date_end no passado retornou erro: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("esperava 0 resultados, obtive %d", len(results))
		}
	})

	t.Run("contains_attachment", func(t *testing.T) {
		withAttachment := true
		params := base(owner.ID)
		params.ContainsAttachment = &withAttachment
		results, err := SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages com contains_attachment true retornou erro: %v", err)
		}
		assertSearchSet(t, results, m4.ID)

		withoutAttachment := false
		params = base(owner.ID)
		params.ContainsAttachment = &withoutAttachment
		results, err = SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages com contains_attachment false retornou erro: %v", err)
		}
		assertSearchSet(t, results, m1.ID, m2.ID, m3.ID, m5.ID)
	})

	t.Run("todos os filtros combinados", func(t *testing.T) {
		withAttachment := true
		params := SearchParams{
			UserID:             owner.ID,
			Text:               "peixe",
			AuthorID:           owner.ID,
			DateStart:          &m1.CreatedAt,
			DateEndExclusive:   &m5.CreatedAt,
			ContainsAttachment: &withAttachment,
			Limit:              100,
		}
		results, err := SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages com todos os filtros retornou erro: %v", err)
		}
		assertSearchSet(t, results, m4.ID)
	})

	t.Run("ordem", func(t *testing.T) {
		params := base(owner.ID)
		params.AuthorID = owner.ID
		params.OrderAsc = true
		results, err := SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages com order asc retornou erro: %v", err)
		}
		assertSearchOrder(t, results, m1.ID, m4.ID, m5.ID)

		params = base(owner.ID)
		params.AuthorID = owner.ID
		params.OrderAsc = false
		results, err = SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages com order desc retornou erro: %v", err)
		}
		assertSearchOrder(t, results, m5.ID, m4.ID, m1.ID)
	})

	t.Run("paginação por cursor", func(t *testing.T) {
		// o storage retorna limit+1: a linha extra é a sonda de has_more
		// (o serviço é quem corta para limit)
		params := base(owner.ID)
		params.AuthorID = owner.ID
		params.Limit = 2
		params.OrderAsc = false
		page1, err := SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages da primeira página retornou erro: %v", err)
		}
		assertSearchOrder(t, page1, m5.ID, m4.ID, m1.ID)

		// cursor no último resultado da página real (m4)
		params = base(owner.ID)
		params.AuthorID = owner.ID
		params.Limit = 2
		params.Since = &page1[1].CreatedAt
		params.LastID = page1[1].ID
		page2, err := SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages da segunda página retornou erro: %v", err)
		}
		assertSearchOrder(t, page2, m1.ID)

		params = base(owner.ID)
		params.AuthorID = owner.ID
		params.Limit = 2
		params.Since = &page2[0].CreatedAt
		params.LastID = page2[0].ID
		page3, err := SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages da terceira página retornou erro: %v", err)
		}
		if len(page3) != 0 {
			t.Errorf("esperava 0 resultados na terceira página, obtive %d", len(page3))
		}
	})

	t.Run("autorização", func(t *testing.T) {
		// reader: canal aberto + restrito (read_channel via role)
		params := base(reader.ID)
		params.Text = "zebra"
		results, err := SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages do leitor retornou erro: %v", err)
		}
		assertSearchSet(t, results, m1.ID, m5.ID)

		// stranger: sem roles, não vê o canal restrito
		params = base(stranger.ID)
		params.Text = "vagalume"
		results, err = SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages do estranho retornou erro: %v", err)
		}
		assertSearchSet(t, results, m2.ID, m3.ID)

		params = base(stranger.ID)
		params.Text = "secreta"
		results, err = SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages do estranho no restrito retornou erro: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("esperava 0 resultados para o estranho, obtive %d", len(results))
		}

		// owner: vê tudo
		params = base(owner.ID)
		params.Text = "secreta"
		results, err = SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages do dono retornou erro: %v", err)
		}
		assertSearchSet(t, results, m5.ID)
	})

	t.Run("limit acima do máximo", func(t *testing.T) {
		for i := 0; i < 105; i++ {
			if _, err := CreateMessage(testCtx(), channel.ID, owner.ID, "clamp "+randHex(2), "", nil); err != nil {
				t.Fatalf("falha ao criar mensagem de clamp %d: %v", i, err)
			}
		}

		params := base(owner.ID)
		params.Text = "clamp"
		params.Limit = 500
		results, err := SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages com limit 500 retornou erro: %v", err)
		}
		// limit é limitado a 100; a busca retorna limit+1 para detectar has_more
		if len(results) != 101 {
			t.Errorf("esperava 101 resultados com limit limitado a 100, obtive %d", len(results))
		}
	})
}

func TestListUsersKeyset(t *testing.T) {
	userA := newTestUser(t)
	time.Sleep(10 * time.Millisecond)
	userB := newTestUser(t)
	time.Sleep(10 * time.Millisecond)
	userC := newTestUser(t)

	// limit
	limited, err := ListUsers(testCtx(), nil, "", 2)
	if err != nil {
		t.Fatalf("ListUsers com limit retornou erro: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("esperava 2 usuários com limit, obtive %d", len(limited))
	}

	// since: only users created after userB (other tests' users were created before that)
	since := userB.CreatedAt
	after, err := ListUsers(testCtx(), &since, "", 0)
	if err != nil {
		t.Fatalf("ListUsers com since retornou erro: %v", err)
	}
	afterIDs := make(map[string]bool, len(after))
	for _, u := range after {
		afterIDs[u.ID] = true
	}
	if afterIDs[userA.ID] || afterIDs[userB.ID] {
		t.Errorf("since should exclude userA/userB: got %v", afterIDs)
	}
	if !afterIDs[userC.ID] {
		t.Errorf("since should include userC: got %v", afterIDs)
	}

	// since + lastID: composite cursor for users with the same created_at
	if _, err := GetDB().ExecContext(testCtx(),
		"UPDATE users SET created_at = $1 WHERE id = $2", userB.CreatedAt, userC.ID); err != nil {
		t.Fatalf("failed to equalize created_at: %v", err)
	}
	// userB and userC now have the same timestamp; the (created_at, id) cursor
	// must be positioned by id order
	boundary, next := userB, userC
	if userC.ID < userB.ID {
		boundary, next = userC, userB
	}

	afterBoundary, err := ListUsers(testCtx(), &since, boundary.ID, 0)
	if err != nil {
		t.Fatalf("ListUsers with composite cursor returned error: %v", err)
	}
	abIDs := make(map[string]bool, len(afterBoundary))
	for _, u := range afterBoundary {
		abIDs[u.ID] = true
	}
	if !abIDs[next.ID] {
		t.Errorf("cursor (since, last_id) should include user %s with the same timestamp, got %v", next.ID, abIDs)
	}
	if abIDs[boundary.ID] {
		t.Errorf("cursor (since, last_id) should exclude the cursor's user, got %v", abIDs)
	}

	// last_id without since is ignored: the result must be the same as the unfiltered list
	ignored, err := ListUsers(testCtx(), nil, boundary.ID, 0)
	if err != nil {
		t.Fatalf("ListUsers with last_id and without since returned error: %v", err)
	}
	ignoredIDs := make(map[string]bool, len(ignored))
	for _, u := range ignored {
		ignoredIDs[u.ID] = true
	}
	for _, want := range []models.User{userA, userB, userC} {
		if !ignoredIDs[want.ID] {
			t.Errorf("last_id without since should be ignored, but user %s was not returned", want.ID)
		}
	}
}

func TestCountChannels(t *testing.T) {
	newTestServer(t, nil)

	for i := 0; i < 3; i++ {
		newTestChannel(t)
	}

	count, err := CountChannels(testCtx())
	if err != nil {
		t.Fatalf("CountChannels returned error: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 channels, got %d", count)
	}

	wipeAppTables(t)
	empty, err := CountChannels(testCtx())
	if err != nil {
		t.Fatalf("CountChannels returned error: %v", err)
	}
	if empty != 0 {
		t.Errorf("expected 0 channels with no records, got %d", empty)
	}
}

// --- attachment thumbnails ---

func newTestAttachment(t *testing.T) models.Attachments {
	t.Helper()
	author := newTestUser(t)
	attachment, err := CreateAttachment(testCtx(), models.Attachments{
		OriginalFileName: "arquivo.txt",
		MediaShaHash:     newTestMediaWithMime(t, []byte("1234567890"), "text/plain"),
		CreatedBy:        &author.ID,
	})
	if err != nil {
		t.Fatalf("falha ao criar attachment de apoio: %v", err)
	}
	return attachment
}

func TestCreateAttachmentThumbnail(t *testing.T) {
	attachment := newTestAttachment(t)
	thumbHash := newTestMediaWithMime(t, []byte("blob da thumbnail"), "image/webp")

	err := CreateAttachmentThumbnail(testCtx(), models.AttachmentThumbnail{
		AttachmentID: attachment.ID,
		Kind:         "preview",
		MediaShaHash: thumbHash,
		Width:        64,
		Height:       32,
	})
	if err != nil {
		t.Fatalf("CreateAttachmentThumbnail retornou erro: %v", err)
	}

	thumb, err := GetThumbnailByAttachmentID(testCtx(), attachment.ID, "preview")
	if err != nil {
		t.Fatalf("GetThumbnailByAttachmentID retornou erro: %v", err)
	}
	if thumb.AttachmentID != attachment.ID || thumb.Kind != "preview" {
		t.Errorf("campos attachment_id/kind incorretos: %+v", thumb)
	}
	if thumb.MediaShaHash != thumbHash {
		t.Errorf("esperava media_sha_hash %s, obtive %s", thumbHash, thumb.MediaShaHash)
	}
	if thumb.MimeType != "image/webp" {
		t.Errorf("esperava mime_type %q (join media), obtive %q", "image/webp", thumb.MimeType)
	}
	if thumb.Width != 64 || thumb.Height != 32 {
		t.Errorf("campos dimensao incorretos: %+v", thumb)
	}
	if thumb.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}

	// ON CONFLICT DO NOTHING: a primeira thumbnail permanece
	otherHash := newTestMediaWithMime(t, []byte("outra thumbnail"), "image/png")
	err = CreateAttachmentThumbnail(testCtx(), models.AttachmentThumbnail{
		AttachmentID: attachment.ID,
		Kind:         "preview",
		MediaShaHash: otherHash,
		Width:        1,
		Height:       1,
	})
	if err != nil {
		t.Fatalf("CreateAttachmentThumbnail duplicada retornou erro: %v", err)
	}
	thumb2, err := GetThumbnailByAttachmentID(testCtx(), attachment.ID, "preview")
	if err != nil {
		t.Fatalf("GetThumbnailByAttachmentID retornou erro: %v", err)
	}
	if thumb2.ID != thumb.ID || thumb2.MediaShaHash != thumbHash {
		t.Errorf("ON CONFLICT DO NOTHING deveria manter a primeira, obtive %+v", thumb2)
	}

	// FK: attachment inexistente
	if err := CreateAttachmentThumbnail(testCtx(), models.AttachmentThumbnail{
		AttachmentID: randUUID(),
		Kind:         "preview",
	}); err == nil {
		t.Error("esperava erro de FK para attachment inexistente")
	}

	// inexistente
	if _, err := GetThumbnailByAttachmentID(testCtx(), randUUID(), "preview"); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para thumbnail inexistente, obtive %v", err)
	}
}

func TestListThumbnailsByAttachmentIDs(t *testing.T) {
	att1 := newTestAttachment(t)
	att2 := newTestAttachment(t)
	att3 := newTestAttachment(t) // sem thumbnail

	for i, att := range []models.Attachments{att1, att2} {
		thumbHash := newTestMediaWithMime(t, []byte("thumb"+string(rune('a'+i))), "image/webp")
		if err := CreateAttachmentThumbnail(testCtx(), models.AttachmentThumbnail{
			AttachmentID: att.ID,
			Kind:         "preview",
			MediaShaHash: thumbHash,
			Width:        64,
			Height:       32,
		}); err != nil {
			t.Fatalf("falha ao criar thumbnail de apoio: %v", err)
		}
	}

	thumbs, err := ListThumbnailsByAttachmentIDs(testCtx(), []string{att1.ID, att2.ID, att3.ID})
	if err != nil {
		t.Fatalf("ListThumbnailsByAttachmentIDs retornou erro: %v", err)
	}
	if len(thumbs) != 2 {
		t.Fatalf("esperava 2 thumbnails (attachment sem thumbnail nao aparece), obtive %d", len(thumbs))
	}
	if thumbs[att1.ID].ID == "" || thumbs[att2.ID].ID == "" {
		t.Error("mapa deveria ser indexado por attachment_id")
	}
	if _, ok := thumbs[att3.ID]; ok {
		t.Error("attachment sem thumbnail nao deveria aparecer no mapa")
	}

	empty, err := ListThumbnailsByAttachmentIDs(testCtx(), nil)
	if err != nil {
		t.Fatalf("ListThumbnailsByAttachmentIDs com lista vazia retornou erro: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("lista vazia deveria retornar mapa vazio, obtive %d", len(empty))
	}
}

func TestAttachmentThumbnailCascadeOnMessageDelete(t *testing.T) {
	author := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)
	attachment := newTestAttachment(t)

	thumbHash := newTestMediaWithMime(t, []byte("blob da thumbnail"), "image/webp")
	if err := CreateAttachmentThumbnail(testCtx(), models.AttachmentThumbnail{
		AttachmentID: attachment.ID,
		Kind:         "preview",
		MediaShaHash: thumbHash,
		Width:        64,
		Height:       32,
	}); err != nil {
		t.Fatalf("falha ao criar thumbnail de apoio: %v", err)
	}

	message, err := CreateMessage(testCtx(), channel.ID, author.ID, "com anexo", "", []string{attachment.ID})
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}
	if err := DeleteMessage(testCtx(), message.ID); err != nil {
		t.Fatalf("DeleteMessage retornou erro: %v", err)
	}

	if _, err := GetThumbnailByAttachmentID(testCtx(), attachment.ID, "preview"); !errors.Is(err, ErrNotFound) {
		t.Errorf("thumbnail deveria ter sido removida em cascata, obtive %v", err)
	}
}

// --- link previews ---

func newTestPreview(t *testing.T, url string) models.LinkPreview {
	t.Helper()
	title := "título"
	preview, err := UpsertPreview(testCtx(), models.LinkPreview{
		URL:   url,
		Kind:  "og",
		Title: &title,
	})
	if err != nil {
		t.Fatalf("falha ao criar preview de apoio: %v", err)
	}
	return preview
}

func TestUpsertPreview(t *testing.T) {
	p1, err := UpsertPreview(testCtx(), models.LinkPreview{
		URL:   "https://upsert.example.com/pagina",
		Kind:  "og",
		Title: strPtr("A"),
	})
	if err != nil {
		t.Fatalf("UpsertPreview retornou erro: %v", err)
	}
	if p1.ID == "" || p1.FetchedAt.IsZero() {
		t.Errorf("esperava id e fetched_at preenchidos: %+v", p1)
	}

	p2, err := UpsertPreview(testCtx(), models.LinkPreview{
		URL:   "https://upsert.example.com/pagina",
		Kind:  "og",
		Title: strPtr("B"),
	})
	if err != nil {
		t.Fatalf("UpsertPreview (update) retornou erro: %v", err)
	}
	if p2.ID != p1.ID {
		t.Errorf("upsert da mesma URL deveria manter o mesmo id, obtive %s (esperado %s)", p2.ID, p1.ID)
	}
	if p2.Title == nil || *p2.Title != "B" {
		t.Errorf("upsert deveria atualizar o title, obtive %v", p2.Title)
	}

	byURL, err := GetPreviewByURL(testCtx(), "https://upsert.example.com/pagina")
	if err != nil {
		t.Fatalf("GetPreviewByURL retornou erro: %v", err)
	}
	if byURL.Title == nil || *byURL.Title != "B" {
		t.Errorf("GetPreviewByURL deveria retornar o title atualizado, obtive %v", byURL.Title)
	}

	byID, err := GetPreviewByID(testCtx(), p1.ID)
	if err != nil {
		t.Fatalf("GetPreviewByID retornou erro: %v", err)
	}
	if byID.URL != "https://upsert.example.com/pagina" {
		t.Errorf("GetPreviewByID retornou URL incorreta: %q", byID.URL)
	}

	if _, err := GetPreviewByURL(testCtx(), "https://nao-existe.example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para URL inexistente, obtive %v", err)
	}
	if _, err := GetPreviewByID(testCtx(), randUUID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestAddMessagePreviews(t *testing.T) {
	author := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)
	message, err := CreateMessage(testCtx(), channel.ID, author.ID, "msg", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}
	p1 := newTestPreview(t, "https://add.example.com/1")
	p2 := newTestPreview(t, "https://add.example.com/2")

	if err := AddMessagePreviews(testCtx(), message.ID, []string{p1.ID, p2.ID}); err != nil {
		t.Fatalf("AddMessagePreviews retornou erro: %v", err)
	}
	linked, err := ListPreviewsByMessageIDs(testCtx(), []string{message.ID})
	if err != nil {
		t.Fatalf("ListPreviewsByMessageIDs retornou erro: %v", err)
	}
	if len(linked[message.ID]) != 2 {
		t.Fatalf("esperava 2 previews vinculados, obtive %d", len(linked[message.ID]))
	}

	// duplicados são ignorados
	if err := AddMessagePreviews(testCtx(), message.ID, []string{p1.ID, p2.ID}); err != nil {
		t.Fatalf("AddMessagePreviews duplicada retornou erro: %v", err)
	}
	linked, err = ListPreviewsByMessageIDs(testCtx(), []string{message.ID})
	if err != nil {
		t.Fatalf("ListPreviewsByMessageIDs retornou erro: %v", err)
	}
	if len(linked[message.ID]) != 2 {
		t.Errorf("duplicados deveriam ser ignorados, obtive %d", len(linked[message.ID]))
	}

	// lista vazia é no-op
	if err := AddMessagePreviews(testCtx(), message.ID, nil); err != nil {
		t.Errorf("AddMessagePreviews com lista vazia deveria ser no-op, obtive %v", err)
	}

	// FK: mensagem inexistente
	if err := AddMessagePreviews(testCtx(), randUUID(), []string{p1.ID}); err == nil {
		t.Error("esperava erro de FK para mensagem inexistente")
	}
	// FK: preview inexistente
	if err := AddMessagePreviews(testCtx(), message.ID, []string{randUUID()}); err == nil {
		t.Error("esperava erro de FK para preview inexistente")
	}
}

func TestReplaceMessagePreviews(t *testing.T) {
	author := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)
	message, err := CreateMessage(testCtx(), channel.ID, author.ID, "msg", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}
	p1 := newTestPreview(t, "https://replace.example.com/1")
	p2 := newTestPreview(t, "https://replace.example.com/2")
	p3 := newTestPreview(t, "https://replace.example.com/3")

	if err := AddMessagePreviews(testCtx(), message.ID, []string{p1.ID}); err != nil {
		t.Fatalf("AddMessagePreviews retornou erro: %v", err)
	}
	if err := ReplaceMessagePreviews(testCtx(), message.ID, []string{p2.ID, p3.ID}); err != nil {
		t.Fatalf("ReplaceMessagePreviews retornou erro: %v", err)
	}
	linked, err := ListPreviewsByMessageIDs(testCtx(), []string{message.ID})
	if err != nil {
		t.Fatalf("ListPreviewsByMessageIDs retornou erro: %v", err)
	}
	ids := make(map[string]bool, len(linked[message.ID]))
	for _, p := range linked[message.ID] {
		ids[p.ID] = true
	}
	if len(ids) != 2 || !ids[p2.ID] || !ids[p3.ID] || ids[p1.ID] {
		t.Errorf("replace deveria trocar p1 por p2+p3, obtive %v", ids)
	}

	if err := ReplaceMessagePreviews(testCtx(), message.ID, nil); err != nil {
		t.Fatalf("ReplaceMessagePreviews (limpeza) retornou erro: %v", err)
	}
	linked, err = ListPreviewsByMessageIDs(testCtx(), []string{message.ID})
	if err != nil {
		t.Fatalf("ListPreviewsByMessageIDs retornou erro: %v", err)
	}
	if len(linked[message.ID]) != 0 {
		t.Errorf("lista vazia deveria limpar os vinculos, obtive %d", len(linked[message.ID]))
	}
}

func TestListPreviewsByMessageIDs(t *testing.T) {
	author := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)

	m1, err := CreateMessage(testCtx(), channel.ID, author.ID, "m1", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}
	m2, err := CreateMessage(testCtx(), channel.ID, author.ID, "m2", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}
	m3, err := CreateMessage(testCtx(), channel.ID, author.ID, "m3", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}

	p1 := newTestPreview(t, "https://list.example.com/1")
	p2 := newTestPreview(t, "https://list.example.com/2")
	p3 := newTestPreview(t, "https://list.example.com/3")

	if err := AddMessagePreviews(testCtx(), m1.ID, []string{p1.ID, p2.ID}); err != nil {
		t.Fatalf("AddMessagePreviews retornou erro: %v", err)
	}
	if err := AddMessagePreviews(testCtx(), m2.ID, []string{p3.ID}); err != nil {
		t.Fatalf("AddMessagePreviews retornou erro: %v", err)
	}

	linked, err := ListPreviewsByMessageIDs(testCtx(), []string{m1.ID, m2.ID, m3.ID})
	if err != nil {
		t.Fatalf("ListPreviewsByMessageIDs retornou erro: %v", err)
	}
	if len(linked[m1.ID]) != 2 {
		t.Errorf("m1 deveria ter 2 previews, obtive %d", len(linked[m1.ID]))
	}
	if len(linked[m2.ID]) != 1 || linked[m2.ID][0].ID != p3.ID {
		t.Errorf("m2 deveria ter o preview p3, obtive %v", linked[m2.ID])
	}
	if _, ok := linked[m3.ID]; ok {
		t.Error("mensagem sem preview nao deveria aparecer no mapa")
	}

	empty, err := ListPreviewsByMessageIDs(testCtx(), nil)
	if err != nil {
		t.Fatalf("ListPreviewsByMessageIDs com lista vazia retornou erro: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("lista vazia deveria retornar mapa vazio, obtive %d", len(empty))
	}
}

func TestGetChannelIDByPreviewID(t *testing.T) {
	author := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)
	message, err := CreateMessage(testCtx(), channel.ID, author.ID, "msg", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}

	p1 := newTestPreview(t, "https://channel.example.com/1")
	if err := AddMessagePreviews(testCtx(), message.ID, []string{p1.ID}); err != nil {
		t.Fatalf("AddMessagePreviews retornou erro: %v", err)
	}

	channelID, err := GetChannelIDByPreviewID(testCtx(), p1.ID)
	if err != nil {
		t.Fatalf("GetChannelIDByPreviewID retornou erro: %v", err)
	}
	if channelID != channel.ID {
		t.Errorf("esperava channel %s, obtive %s", channel.ID, channelID)
	}

	unlinked := newTestPreview(t, "https://channel.example.com/2")
	if _, err := GetChannelIDByPreviewID(testCtx(), unlinked.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("preview sem vinculo deveria retornar ErrNotFound, obtive %v", err)
	}
}

// --- user_channel_state ---

func TestTouchLastReadMessage(t *testing.T) {
	reader := newTestUser(t)
	author := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)

	m1, err := CreateMessage(testCtx(), channel.ID, author.ID, "primeira", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}
	m2, err := CreateMessage(testCtx(), channel.ID, author.ID, "segunda", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}
	m3, err := CreateMessage(testCtx(), channel.ID, author.ID, "terceira", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}

	// primeiro touch cria a linha com a mensagem mais nova
	if err := TouchLastReadMessage(testCtx(), reader.ID, channel.ID, m3); err != nil {
		t.Fatalf("TouchLastReadMessage retornou erro: %v", err)
	}
	state, err := GetLastReadMessage(testCtx(), reader.ID, channel.ID)
	if err != nil {
		t.Fatalf("GetLastReadMessage retornou erro: %v", err)
	}
	if state.LastReadMessageID != m3.ID {
		t.Errorf("esperava last_read_message_id %s, obtive %s", m3.ID, state.LastReadMessageID)
	}
	if state.LastReadAt.IsZero() {
		t.Error("esperava last_read_at preenchido")
	}

	// mensagem mais antiga não regride o último read
	if err := TouchLastReadMessage(testCtx(), reader.ID, channel.ID, m1); err != nil {
		t.Fatalf("TouchLastReadMessage (mais antiga) retornou erro: %v", err)
	}
	state, err = GetLastReadMessage(testCtx(), reader.ID, channel.ID)
	if err != nil {
		t.Fatalf("GetLastReadMessage retornou erro: %v", err)
	}
	if state.LastReadMessageID != m3.ID {
		t.Errorf("último read não deveria regridir para %s, obtive %s", m1.ID, state.LastReadMessageID)
	}

	// mesma mensagem de novo: sem mudança
	if err := TouchLastReadMessage(testCtx(), reader.ID, channel.ID, m3); err != nil {
		t.Fatalf("TouchLastReadMessage (repetida) retornou erro: %v", err)
	}
	state, err = GetLastReadMessage(testCtx(), reader.ID, channel.ID)
	if err != nil {
		t.Fatalf("GetLastReadMessage retornou erro: %v", err)
	}
	if state.LastReadMessageID != m3.ID {
		t.Errorf("esperava last_read_message_id %s, obtive %s", m3.ID, state.LastReadMessageID)
	}

	// mensagem armazenada excluída: o último read avança
	if err := DeleteMessage(testCtx(), m3.ID); err != nil {
		t.Fatalf("DeleteMessage retornou erro: %v", err)
	}
	if err := TouchLastReadMessage(testCtx(), reader.ID, channel.ID, m2); err != nil {
		t.Fatalf("TouchLastReadMessage (armazenada excluída) retornou erro: %v", err)
	}
	state, err = GetLastReadMessage(testCtx(), reader.ID, channel.ID)
	if err != nil {
		t.Fatalf("GetLastReadMessage retornou erro: %v", err)
	}
	if state.LastReadMessageID != m2.ID {
		t.Errorf("esperava last_read_message_id %s após exclusão da armazenada, obtive %s", m2.ID, state.LastReadMessageID)
	}
}

func TestGetLastReadMessage(t *testing.T) {
	reader := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)

	if _, err := GetLastReadMessage(testCtx(), reader.ID, channel.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("esperava ErrNotFound para estado inexistente, obtive %v", err)
	}
}

// --- pinned messages ---

// countPinnedMessages conta os pins de um canal direto na tabela (não há
// getter de listagem de pins).
func countPinnedMessages(t *testing.T, channelID string) int {
	t.Helper()
	var count int
	if err := GetDB().QueryRowContext(testCtx(),
		"SELECT COUNT(*) FROM pinned_messages WHERE channel_id = $1", channelID,
	).Scan(&count); err != nil {
		t.Fatalf("falha ao contar pins: %v", err)
	}
	return count
}

func TestPinMessage(t *testing.T) {
	owner := newTestUser(t)
	_ = newTestServer(t, strPtr(owner.ID))
	channel := newTestChannel(t)
	message, err := CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	pinned, created, err := PinMessage(testCtx(), channel.ID, message.ID, owner.ID)
	if err != nil {
		t.Fatalf("PinMessage retornou erro: %v", err)
	}
	if !created {
		t.Error("esperava created=true na primeira fixação")
	}
	if pinned.ChannelID != channel.ID || pinned.MessageID != message.ID {
		t.Errorf("pin não confere: %+v", pinned)
	}
	if pinned.PinnedBy == nil || *pinned.PinnedBy != owner.ID {
		t.Errorf("esperava pinned_by %s, obtive %v", owner.ID, pinned.PinnedBy)
	}
	if count := countPinnedMessages(t, channel.ID); count != 1 {
		t.Errorf("esperava 1 pin no banco, obtive %d", count)
	}
}

func TestPinMessageIdempotent(t *testing.T) {
	owner := newTestUser(t)
	_ = newTestServer(t, strPtr(owner.ID))
	channel := newTestChannel(t)
	message, err := CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	first, created, err := PinMessage(testCtx(), channel.ID, message.ID, owner.ID)
	if err != nil || !created {
		t.Fatalf("primeira fixação falhou: created=%v err=%v", created, err)
	}

	second, created, err := PinMessage(testCtx(), channel.ID, message.ID, owner.ID)
	if err != nil {
		t.Fatalf("segunda fixação retornou erro: %v", err)
	}
	if created {
		t.Error("esperava created=false na segunda fixação")
	}
	if second.PinnedBy == nil || *second.PinnedBy != owner.ID {
		t.Errorf("esperava pinned_by %s, obtive %v", owner.ID, second.PinnedBy)
	}
	if !second.PinnedAt.Equal(first.PinnedAt) {
		t.Errorf("esperava pinned_at inalterado: %v != %v", second.PinnedAt, first.PinnedAt)
	}
	if count := countPinnedMessages(t, channel.ID); count != 1 {
		t.Errorf("esperava 1 pin no banco, obtive %d", count)
	}
}

func TestPinMessageLimit(t *testing.T) {
	owner := newTestUser(t)
	_ = newTestServer(t, strPtr(owner.ID))
	channel := newTestChannel(t)

	for i := 0; i < maxPinnedPerChannel; i++ {
		message, err := CreateMessage(testCtx(), channel.ID, owner.ID, fmt.Sprintf("msg %d", i), "", nil)
		if err != nil {
			t.Fatalf("CreateMessage[%d] retornou erro: %v", i, err)
		}
		if _, created, err := PinMessage(testCtx(), channel.ID, message.ID, owner.ID); err != nil || !created {
			t.Fatalf("PinMessage[%d] falhou: created=%v err=%v", i, created, err)
		}
	}
	if count := countPinnedMessages(t, channel.ID); count != maxPinnedPerChannel {
		t.Fatalf("esperava %d pins, obtive %d", maxPinnedPerChannel, count)
	}

	overflow, err := CreateMessage(testCtx(), channel.ID, owner.ID, "estouro", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage (estouro) retornou erro: %v", err)
	}
	if _, _, err := PinMessage(testCtx(), channel.ID, overflow.ID, owner.ID); !errors.Is(err, ErrPinnedLimitReached) {
		t.Errorf("esperava ErrPinnedLimitReached, obtive %v", err)
	}
}

func TestPinMessageCascadeOnMessageDelete(t *testing.T) {
	owner := newTestUser(t)
	_ = newTestServer(t, strPtr(owner.ID))
	channel := newTestChannel(t)
	message, err := CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}
	if _, _, err := PinMessage(testCtx(), channel.ID, message.ID, owner.ID); err != nil {
		t.Fatalf("PinMessage retornou erro: %v", err)
	}
	if count := countPinnedMessages(t, channel.ID); count != 1 {
		t.Fatalf("esperava 1 pin antes da exclusão, obtive %d", count)
	}

	if err := DeleteMessage(testCtx(), message.ID); err != nil {
		t.Fatalf("DeleteMessage retornou erro: %v", err)
	}
	if count := countPinnedMessages(t, channel.ID); count != 0 {
		t.Errorf("esperava 0 pins após excluir a mensagem, obtive %d", count)
	}
}

func TestPinMessageCascadeOnChannelDelete(t *testing.T) {
	owner := newTestUser(t)
	_ = newTestServer(t, strPtr(owner.ID))
	channel := newTestChannel(t)
	message, err := CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}
	if _, _, err := PinMessage(testCtx(), channel.ID, message.ID, owner.ID); err != nil {
		t.Fatalf("PinMessage retornou erro: %v", err)
	}

	if err := DeleteChannel(testCtx(), channel.ID); err != nil {
		t.Fatalf("DeleteChannel retornou erro: %v", err)
	}
	if count := countPinnedMessages(t, channel.ID); count != 0 {
		t.Errorf("esperava 0 pins após excluir o canal, obtive %d", count)
	}
}
