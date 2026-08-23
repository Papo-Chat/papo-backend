package test_storage

import (
	"context"
	"crypto/rand"
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

	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// migrationsDir é o caminho relativo ao diretório deste pacote (backend/internal/storage/test_storage).
const migrationsDir = "../../../../migrations"

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

	if err := storage.InitDB(tempURL); err != nil {
		fmt.Printf("testes de storage FALHARAM na preparação: %v\n", err)
		return 1
	}

	code := m.Run()

	storage.CloseDB()
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
	user, _, err := storage.CreateUser(testCtx(), "user_"+randHex(8), "hash_"+randHex(8), "123.123.123.123")
	if err != nil {
		t.Fatalf("falha ao criar usuário de apoio: %v", err)
	}
	return user
}

func newTestServer(t *testing.T, ownerID *string) models.Server {
	t.Helper()
	server, err := storage.CreateServer(testCtx(), "server_"+randHex(8), ownerID)
	if err != nil {
		t.Fatalf("falha ao criar servidor de apoio: %v", err)
	}
	return server
}

func newTestChannel(t *testing.T, serverID string) models.Channel {
	t.Helper()
	channel, err := storage.CreateChannel(testCtx(), serverID, "channel_"+randHex(8), "text")
	if err != nil {
		t.Fatalf("falha ao criar canal de apoio: %v", err)
	}
	return channel
}

func newTestRole(t *testing.T, serverID string) models.Role {
	t.Helper()
	role, err := storage.CreateRole(testCtx(), serverID, "role_"+randHex(8), strPtr("#123456"), models.RolePermissions{ManageRoles: true})
	if err != nil {
		t.Fatalf("falha ao criar role de apoio: %v", err)
	}
	return role
}

// --- users ---

func TestCreateUser(t *testing.T) {
	username := "user_" + randHex(8)
	user, settings, err := storage.CreateUser(testCtx(), username, "hash_abc", "123.123.123.123")
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
	if _, _, err := storage.CreateUser(testCtx(), username, "hash_1", "123.123.123.123"); err != nil {
		t.Fatalf("falha ao criar primeiro usuário: %v", err)
	}

	_, _, err := storage.CreateUser(testCtx(), username, "hash_2", "123.123.123.123")
	if !errors.Is(err, storage.ErrUniqueViolation) {
		t.Errorf("esperava ErrUniqueViolation, obtive %v", err)
	}
}

func TestGetUserByID(t *testing.T) {
	user := newTestUser(t)

	got, err := storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if got.ID != user.ID || got.Username != user.Username {
		t.Errorf("usuário retornado não confere: got %+v, want ID=%s username=%s", got, user.ID, user.Username)
	}
	if got.PasswordHash != "" {
		t.Errorf("GetUserByID não deve retornar password_hash, obtive %q", got.PasswordHash)
	}

	if _, err := storage.GetUserByID(testCtx(), randUUID()); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestGetUserByUsername(t *testing.T) {
	hash := "hash_" + randHex(8)
	user, _, err := storage.CreateUser(testCtx(), "user_"+randHex(8), hash, "123.123.123.123")
	if err != nil {
		t.Fatalf("falha ao criar usuário de apoio: %v", err)
	}

	got, err := storage.GetUserByUsername(testCtx(), user.Username)
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

	if _, err := storage.GetUserByUsername(testCtx(), "user_"+randHex(8)); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para username inexistente, obtive %v", err)
	}
}

func TestListUsers(t *testing.T) {
	userA := newTestUser(t)
	userB := newTestUser(t)

	users, err := storage.ListUsers(testCtx(), nil, "", 100)
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

	updated, err := storage.UpdateUser(testCtx(), user.ID, models.User{
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

	if _, err := storage.UpdateUser(testCtx(), randUUID(), models.User{}); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestUpdateUserAvatar(t *testing.T) {
	user := newTestUser(t)
	avatar := []byte{0x89, 0x50, 0x4e, 0x47}

	if err := storage.UpdateUserAvatar(testCtx(), avatar, "PNG", user.ID); err != nil {
		t.Fatalf("UpdateUserAvatar retornou erro: %v", err)
	}

	got, err := storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if string(got.AvatarBlob) != string(avatar) {
		t.Errorf("esperava avatar_blob %v, obtive %v", avatar, got.AvatarBlob)
	}
	if got.AvatarFormat != "PNG" {
		t.Errorf("esperava avatar_format %q, obtive %q", "PNG", got.AvatarFormat)
	}
	if got.Username != user.Username {
		t.Errorf("username deveria permanecer %q, obtive %q", user.Username, got.Username)
	}

	if err := storage.UpdateUserAvatar(testCtx(), nil, "", user.ID); err != nil {
		t.Fatalf("UpdateUserAvatar(remove) retornou erro: %v", err)
	}
	got, err = storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if len(got.AvatarBlob) != 0 {
		t.Errorf("esperava avatar_blob vazio, obtive %v", got.AvatarBlob)
	}
	if got.AvatarFormat != "" {
		t.Errorf("esperava avatar_format vazio, obtive %q", got.AvatarFormat)
	}

	if err := storage.UpdateUserAvatar(testCtx(), avatar, "PNG", randUUID()); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestUpdateUserLastIP(t *testing.T) {
	user := newTestUser(t)
	ip := "2001:db8::1"

	if err := storage.UpdateUserLastIP(testCtx(), user.ID, ip); err != nil {
		t.Fatalf("UpdateUserLastIP retornou erro: %v", err)
	}

	got, err := storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if got.LastIP == nil || *got.LastIP != ip {
		t.Errorf("esperava last_ip %q, obtive %v", ip, got.LastIP)
	}

	if err := storage.UpdateUserLastIP(testCtx(), randUUID(), ip); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestSetUserBanned(t *testing.T) {
	user := newTestUser(t)

	if _, err := storage.SetUserBanned(testCtx(), user.ID, true); err != nil {
		t.Fatalf("SetUserBanned(true) retornou erro: %v", err)
	}
	got, err := storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if !got.Banned {
		t.Error("esperava banned = true")
	}

	if _, err := storage.SetUserBanned(testCtx(), user.ID, false); err != nil {
		t.Fatalf("SetUserBanned(false) retornou erro: %v", err)
	}
	got, err = storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if got.Banned {
		t.Error("esperava banned = false")
	}

	if _, err := storage.SetUserBanned(testCtx(), randUUID(), true); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestSetUserResetPassword(t *testing.T) {
	user := newTestUser(t)

	if err := storage.SetUserResetPassword(testCtx(), user.ID); err != nil {
		t.Fatalf("SetUserResetPassword retornou erro: %v", err)
	}
	got, err := storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if !got.ResetPassword {
		t.Error("esperava reset_password = true")
	}

	if err := storage.SetUserResetPassword(testCtx(), randUUID()); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestUpdateUserPassword(t *testing.T) {
	user := newTestUser(t)
	newHash := "hash_" + randHex(8)

	// marca o usuário para reset e confirma que a troca de senha reinicia a flag
	if err := storage.SetUserResetPassword(testCtx(), user.ID); err != nil {
		t.Fatalf("SetUserResetPassword retornou erro: %v", err)
	}

	if err := storage.UpdateUserPassword(testCtx(), user.ID, newHash); err != nil {
		t.Fatalf("UpdateUserPassword retornou erro: %v", err)
	}
	got, err := storage.GetUserByUsername(testCtx(), user.Username)
	if err != nil {
		t.Fatalf("GetUserByUsername retornou erro: %v", err)
	}
	if got.PasswordHash != newHash {
		t.Errorf("esperava password_hash %q, obtive %q", newHash, got.PasswordHash)
	}
	if got.ResetPassword {
		t.Error("esperava reset_password = false após trocar a senha")
	}

	if err := storage.UpdateUserPassword(testCtx(), randUUID(), newHash); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

// --- servers ---

func TestCreateServer(t *testing.T) {
	owner := newTestUser(t)
	name := "server_" + randHex(8)

	server, err := storage.CreateServer(testCtx(), name, &owner.ID)
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
	serverNoOwner, err := storage.CreateServer(testCtx(), "server_"+randHex(8), nil)
	if err != nil {
		t.Fatalf("CreateServer sem owner retornou erro: %v", err)
	}
	if serverNoOwner.OwnerID != nil {
		t.Errorf("esperava owner_id nil, obtive %v", serverNoOwner.OwnerID)
	}
}

func TestCreateServerWithIcon(t *testing.T) {
	owner := newTestUser(t)
	name := "server_" + randHex(8)
	icon := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

	server, err := storage.CreateServerWithIcon(testCtx(), name, icon, "PNG", true, &owner.ID, nil)
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
	if string(server.IconBlob) != string(icon) {
		t.Errorf("esperava icon_blob %x, obtive %x", icon, server.IconBlob)
	}
	if server.IconFormat != "PNG" {
		t.Errorf("esperava icon_format %q, obtive %q", "PNG", server.IconFormat)
	}
	if server.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}
}

func TestUserOwnsAnyServer(t *testing.T) {
	owner := newTestUser(t)
	newTestServer(t, &owner.ID)

	owns, err := storage.UserOwnsAnyServer(testCtx(), owner.ID)
	if err != nil {
		t.Fatalf("UserOwnsAnyServer retornou erro: %v", err)
	}
	if !owns {
		t.Error("esperava owns = true para o dono de um servidor")
	}

	// usuário sem servidor não é dono de nenhum
	other := newTestUser(t)
	owns, err = storage.UserOwnsAnyServer(testCtx(), other.ID)
	if err != nil {
		t.Fatalf("UserOwnsAnyServer retornou erro: %v", err)
	}
	if owns {
		t.Error("esperava owns = false para usuário sem servidor")
	}

	// id inexistente não é dono de nenhum servidor
	owns, err = storage.UserOwnsAnyServer(testCtx(), randUUID())
	if err != nil {
		t.Fatalf("UserOwnsAnyServer retornou erro para id inexistente: %v", err)
	}
	if owns {
		t.Error("esperava owns = false para id inexistente")
	}
}

func TestGetServerByID(t *testing.T) {
	server := newTestServer(t, nil)

	got, err := storage.GetServerByID(testCtx(), server.ID)
	if err != nil {
		t.Fatalf("GetServerByID retornou erro: %v", err)
	}
	if got.ID != server.ID || got.Name != server.Name {
		t.Errorf("servidor retornado não confere: got %+v, want ID=%s name=%s", got, server.ID, server.Name)
	}

	if _, err := storage.GetServerByID(testCtx(), randUUID()); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestListServers(t *testing.T) {
	s1 := newTestServer(t, nil)
	s2 := newTestServer(t, nil)

	servers, err := storage.ListServers(testCtx())
	if err != nil {
		t.Fatalf("ListServers retornou erro: %v", err)
	}

	ids := make(map[string]bool, len(servers))
	for _, s := range servers {
		ids[s.ID] = true
	}
	if !ids[s1.ID] || !ids[s2.ID] {
		t.Errorf("ListServers não retornou os servidores criados: got %v", ids)
	}
}

func TestUpdateServer(t *testing.T) {
	owner := newTestUser(t)
	server := newTestServer(t, &owner.ID)
	newName := "server_" + randHex(8)
	icon := []byte{0xff, 0xd8, 0xff}

	updated, err := storage.UpdateServer(testCtx(), server.ID, models.Server{
		Name:         newName,
		IconBlob:     icon,
		IconFormat:   "JPEG",
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
	if string(updated.IconBlob) != string(icon) {
		t.Errorf("esperava icon_blob %v, obtive %v", icon, updated.IconBlob)
	}
	if updated.IconFormat != "JPEG" {
		t.Errorf("esperava icon_format %q, obtive %q", "JPEG", updated.IconFormat)
	}
	if updated.OwnerID == nil || *updated.OwnerID != owner.ID {
		t.Errorf("owner_id deveria permanecer %s, obtive %v", owner.ID, updated.OwnerID)
	}

	if _, err := storage.UpdateServer(testCtx(), randUUID(), models.Server{}, nil); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestListServerSummaries(t *testing.T) {
	owner := newTestUser(t)
	server := newTestServer(t, &owner.ID)
	newTestChannel(t, server.ID)
	newTestRole(t, server.ID)

	summaries, err := storage.ListServerSummaries(testCtx())
	if err != nil {
		t.Fatalf("ListServerSummaries retornou erro: %v", err)
	}

	var found *models.ServerSummary
	for i := range summaries {
		if summaries[i].ID == server.ID {
			found = &summaries[i]
		}
	}
	if found == nil {
		t.Fatal("servidor criado não aparece em ListServerSummaries")
	}

	if found.Name != server.Name {
		t.Errorf("esperava name %q, obtive %q", server.Name, found.Name)
	}
	if found.OwnerID == nil || *found.OwnerID != owner.ID {
		t.Errorf("esperava owner_id %s, obtive %v", owner.ID, found.OwnerID)
	}
	if found.OwnerUsername == nil || *found.OwnerUsername != owner.Username {
		t.Errorf("esperava owner_username %q, obtive %v", owner.Username, found.OwnerUsername)
	}
	if found.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}

	// as contagens devem refletir o estado atual do banco
	channels, err := storage.ListChannelsByServer(testCtx(), server.ID)
	if err != nil {
		t.Fatalf("ListChannelsByServer retornou erro: %v", err)
	}
	if found.ChannelCount != len(channels) {
		t.Errorf("esperava channel_count %d, obtive %d", len(channels), found.ChannelCount)
	}

	roles, err := storage.ListRolesByServer(testCtx(), server.ID)
	if err != nil {
		t.Fatalf("ListRolesByServer retornou erro: %v", err)
	}
	if found.RoleCount != len(roles) {
		t.Errorf("esperava role_count %d, obtive %d", len(roles), found.RoleCount)
	}

	// por enquanto todos os usuários pertencem ao mesmo servidor:
	// member_count é o total de usuários
	users, err := storage.ListUsers(testCtx(), nil, "", 100)
	if err != nil {
		t.Fatalf("ListUsers retornou erro: %v", err)
	}
	if found.MemberCount != len(users) {
		t.Errorf("esperava member_count %d, obtive %d", len(users), found.MemberCount)
	}
}

func TestListServerSummariesWithoutOwner(t *testing.T) {
	server := newTestServer(t, nil)

	summaries, err := storage.ListServerSummaries(testCtx())
	if err != nil {
		t.Fatalf("ListServerSummaries retornou erro: %v", err)
	}

	var found *models.ServerSummary
	for i := range summaries {
		if summaries[i].ID == server.ID {
			found = &summaries[i]
		}
	}
	if found == nil {
		t.Fatal("servidor criado não aparece em ListServerSummaries")
	}
	if found.OwnerID != nil {
		t.Errorf("esperava owner_id nil, obtive %v", *found.OwnerID)
	}
	if found.OwnerUsername != nil {
		t.Errorf("esperava owner_username nil, obtive %v", *found.OwnerUsername)
	}
}

func TestGetServerSummary(t *testing.T) {
	owner := newTestUser(t)
	server := newTestServer(t, &owner.ID)
	newTestChannel(t, server.ID)
	newTestRole(t, server.ID)

	summary, err := storage.GetServerSummary(testCtx(), server.ID)
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
	channels, err := storage.ListChannelsByServer(testCtx(), server.ID)
	if err != nil {
		t.Fatalf("ListChannelsByServer retornou erro: %v", err)
	}
	if summary.ChannelCount != len(channels) {
		t.Errorf("esperava channel_count %d, obtive %d", len(channels), summary.ChannelCount)
	}

	roles, err := storage.ListRolesByServer(testCtx(), server.ID)
	if err != nil {
		t.Fatalf("ListRolesByServer retornou erro: %v", err)
	}
	if summary.RoleCount != len(roles) {
		t.Errorf("esperava role_count %d, obtive %d", len(roles), summary.RoleCount)
	}

	// por enquanto todos os usuários pertencem ao mesmo servidor:
	// member_count é o total de usuários
	users, err := storage.ListUsers(testCtx(), nil, "", 100)
	if err != nil {
		t.Fatalf("ListUsers retornou erro: %v", err)
	}
	if summary.MemberCount != len(users) {
		t.Errorf("esperava member_count %d, obtive %d", len(users), summary.MemberCount)
	}

	if _, err := storage.GetServerSummary(testCtx(), randUUID()); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

// --- channels ---

func TestCreateChannel(t *testing.T) {
	server := newTestServer(t, nil)
	name := "channel_" + randHex(8)

	channel, err := storage.CreateChannel(testCtx(), server.ID, name, "text")
	if err != nil {
		t.Fatalf("CreateChannel retornou erro: %v", err)
	}
	if channel.ID == "" {
		t.Error("esperava channel.ID preenchido")
	}
	if channel.ServerID != server.ID {
		t.Errorf("esperava server_id %s, obtive %s", server.ID, channel.ServerID)
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

	category, err := storage.CreateChannel(testCtx(), server.ID, "channel_"+randHex(8), "category")
	if err != nil {
		t.Fatalf("CreateChannel(category) retornou erro: %v", err)
	}
	if category.Type != "category" {
		t.Errorf("esperava type %q, obtive %q", "category", category.Type)
	}

	if _, err := storage.CreateChannel(testCtx(), server.ID, "channel_"+randHex(8), "voice"); err == nil {
		t.Error("esperava erro para type inválido, obtive nil")
	}
}

func TestCreateChannelDuplicateName(t *testing.T) {
	server := newTestServer(t, nil)
	name := "channel_" + randHex(8)

	if _, err := storage.CreateChannel(testCtx(), server.ID, name, "text"); err != nil {
		t.Fatalf("falha ao criar primeiro canal: %v", err)
	}
	if _, err := storage.CreateChannel(testCtx(), server.ID, name, "text"); !errors.Is(err, storage.ErrUniqueViolation) {
		t.Errorf("esperava ErrUniqueViolation, obtive %v", err)
	}
}

func TestGetChannelByID(t *testing.T) {
	server := newTestServer(t, nil)
	channel := newTestChannel(t, server.ID)

	got, err := storage.GetChannelByID(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if got.ID != channel.ID || got.ServerID != server.ID || got.Name != channel.Name || got.Type != channel.Type {
		t.Errorf("canal retornado não confere: got %+v, want ID=%s name=%s", got, channel.ID, channel.Name)
	}

	if _, err := storage.GetChannelByID(testCtx(), randUUID()); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestListChannelsByServer(t *testing.T) {
	server := newTestServer(t, nil)
	otherServer := newTestServer(t, nil)
	c1 := newTestChannel(t, server.ID)
	c2 := newTestChannel(t, server.ID)
	newTestChannel(t, otherServer.ID)

	channels, err := storage.ListChannelsByServer(testCtx(), server.ID)
	if err != nil {
		t.Fatalf("ListChannelsByServer retornou erro: %v", err)
	}

	ids := make(map[string]bool, len(channels))
	for _, c := range channels {
		ids[c.ID] = true
		if c.ServerID != server.ID {
			t.Errorf("canal %s pertence a outro servidor (%s)", c.ID, c.ServerID)
		}
	}
	if !ids[c1.ID] || !ids[c2.ID] {
		t.Errorf("ListChannelsByServer não retornou os canais criados: got %v", ids)
	}
}

func TestUpdateChannel(t *testing.T) {
	server := newTestServer(t, nil)
	channel := newTestChannel(t, server.ID)
	role := newTestRole(t, server.ID)
	perm := models.ChannelPermission{ReadChannel: true, SendMessages: true}
	if _, err := storage.UpdateChannelPermissions(testCtx(), channel.ID, role.ID, perm); err != nil {
		t.Fatalf("falha ao configurar permissões do canal: %v", err)
	}

	newName := "channel_" + randHex(8)
	updated, err := storage.UpdateChannel(testCtx(), channel.ID, newName)
	if err != nil {
		t.Fatalf("UpdateChannel retornou erro: %v", err)
	}
	if updated.ID != channel.ID {
		t.Errorf("esperava id %s, obtive %s", channel.ID, updated.ID)
	}
	if updated.ServerID != server.ID {
		t.Errorf("esperava server_id %s, obtive %s", server.ID, updated.ServerID)
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
	stored, err := storage.GetChannelByID(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Name != newName {
		t.Errorf("esperava name persistido %q, obtive %q", newName, stored.Name)
	}
}

func TestUpdateChannelDuplicateName(t *testing.T) {
	server := newTestServer(t, nil)
	otherServer := newTestServer(t, nil)
	c1 := newTestChannel(t, server.ID)
	c2 := newTestChannel(t, server.ID)
	c3 := newTestChannel(t, otherServer.ID)

	// a constraint UNIQUE de channels.name é global: o mesmo nome em outro
	// servidor também é conflito
	for _, tc := range []struct {
		name  string
		id    string
		taken string
	}{
		{"mesmo servidor", c1.ID, c2.Name},
		{"outro servidor", c1.ID, c3.Name},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := storage.UpdateChannel(testCtx(), tc.id, tc.taken); !errors.Is(err, storage.ErrUniqueViolation) {
				t.Errorf("esperava ErrUniqueViolation, obtive %v", err)
			}
		})
	}

	// o rename recusado não deve alterar o nome original
	stored, err := storage.GetChannelByID(testCtx(), c1.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Name != c1.Name {
		t.Errorf("esperava name original %q, obtive %q", c1.Name, stored.Name)
	}
}

func TestUpdateChannelNotFound(t *testing.T) {
	if _, err := storage.UpdateChannel(testCtx(), randUUID(), "channel_"+randHex(8)); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestUpdateChannelPermissions(t *testing.T) {
	server := newTestServer(t, nil)
	channel := newTestChannel(t, server.ID)
	role := newTestRole(t, server.ID)

	perm := models.ChannelPermission{ReadChannel: true, SendMessages: true}
	updated, err := storage.UpdateChannelPermissions(testCtx(), channel.ID, role.ID, perm)
	if err != nil {
		t.Fatalf("UpdateChannelPermissions retornou erro: %v", err)
	}
	if got := updated.Permissions[role.ID]; got != perm {
		t.Errorf("esperava permissions[%s] = %+v, obtive %+v", role.ID, perm, got)
	}

	// uma nova atualização substitui as permissões da mesma role
	perm2 := models.ChannelPermission{ReadChannel: true}
	updated, err = storage.UpdateChannelPermissions(testCtx(), channel.ID, role.ID, perm2)
	if err != nil {
		t.Fatalf("UpdateChannelPermissions (segunda) retornou erro: %v", err)
	}
	if got := updated.Permissions[role.ID]; got != perm2 {
		t.Errorf("esperava permissions[%s] = %+v, obtive %+v", role.ID, perm2, got)
	}

	if _, err := storage.UpdateChannelPermissions(testCtx(), randUUID(), role.ID, perm); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para canal inexistente, obtive %v", err)
	}
}

func TestDeleteChannel(t *testing.T) {
	server := newTestServer(t, nil)
	channel := newTestChannel(t, server.ID)

	if err := storage.DeleteChannel(testCtx(), channel.ID); err != nil {
		t.Fatalf("DeleteChannel retornou erro: %v", err)
	}
	if _, err := storage.GetChannelByID(testCtx(), channel.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound após exclusão, obtive %v", err)
	}
	if err := storage.DeleteChannel(testCtx(), channel.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound ao excluir novamente, obtive %v", err)
	}
}

// --- ChangeChannelPosition (tarefa 8.4) ---

func TestChangeChannelPositionMoveDown(t *testing.T) {
	server := newTestServer(t, nil)
	c1 := newTestChannel(t, server.ID)
	c2 := newTestChannel(t, server.ID)
	c3 := newTestChannel(t, server.ID)

	updated, err := storage.ChangeChannelPosition(testCtx(), c1.ID, 1, 3)
	if err != nil {
		t.Fatalf("ChangeChannelPosition retornou erro: %v", err)
	}
	if updated.ID != c1.ID || updated.Position != 3 {
		t.Errorf("esperava canal %s na posição 3, obtive %s na posição %d", c1.ID, updated.ID, updated.Position)
	}

	channels, err := storage.ListChannelsByServer(testCtx(), server.ID)
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
	server := newTestServer(t, nil)
	c1 := newTestChannel(t, server.ID)
	c2 := newTestChannel(t, server.ID)
	c3 := newTestChannel(t, server.ID)

	updated, err := storage.ChangeChannelPosition(testCtx(), c3.ID, 3, 1)
	if err != nil {
		t.Fatalf("ChangeChannelPosition retornou erro: %v", err)
	}
	if updated.ID != c3.ID || updated.Position != 1 {
		t.Errorf("esperava canal %s na posição 1, obtive %s na posição %d", c3.ID, updated.ID, updated.Position)
	}

	channels, err := storage.ListChannelsByServer(testCtx(), server.ID)
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
	server := newTestServer(t, nil)
	c1 := newTestChannel(t, server.ID)
	c2 := newTestChannel(t, server.ID)

	updated, err := storage.ChangeChannelPosition(testCtx(), c2.ID, 2, 2)
	if err != nil {
		t.Fatalf("ChangeChannelPosition retornou erro: %v", err)
	}
	if updated.Position != 2 {
		t.Errorf("esperava posição 2, obtive %d", updated.Position)
	}

	channels, err := storage.ListChannelsByServer(testCtx(), server.ID)
	if err != nil {
		t.Fatalf("ListChannelsByServer retornou erro: %v", err)
	}
	if channels[0].ID != c1.ID || channels[1].ID != c2.ID {
		t.Errorf("ordem alterada pela operação inofensiva: %+v", channels)
	}
}

func TestChangeChannelPositionConflict(t *testing.T) {
	server := newTestServer(t, nil)
	c1 := newTestChannel(t, server.ID)
	c2 := newTestChannel(t, server.ID)
	c3 := newTestChannel(t, server.ID)

	if _, err := storage.ChangeChannelPosition(testCtx(), c1.ID, 2, 3); !errors.Is(err, storage.ErrPositionConflict) {
		t.Fatalf("esperava ErrPositionConflict, obtive %v", err)
	}

	channels, err := storage.ListChannelsByServer(testCtx(), server.ID)
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
	server := newTestServer(t, nil)
	c1 := newTestChannel(t, server.ID)

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
			if _, err := storage.ChangeChannelPosition(testCtx(), c1.ID, tc.old, tc.new); !errors.Is(err, storage.ErrInvalidPosition) {
				t.Errorf("esperava ErrInvalidPosition, obtive %v", err)
			}
		})
	}

	stored, err := storage.GetChannelByID(testCtx(), c1.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Position != 1 {
		t.Errorf("esperava posição 1 inalterada, obtive %d", stored.Position)
	}
}

func TestChangeChannelPositionNotFound(t *testing.T) {
	if _, err := storage.ChangeChannelPosition(testCtx(), randUUID(), 1, 1); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestListChannelSummaries(t *testing.T) {
	author := newTestUser(t)
	server := newTestServer(t, nil)
	otherServer := newTestServer(t, nil)
	c1 := newTestChannel(t, server.ID)
	c2 := newTestChannel(t, server.ID)
	newTestChannel(t, otherServer.ID)
	role := newTestRole(t, server.ID)
	perm := models.ChannelPermission{ReadChannel: true, SendMessages: true}

	if _, err := storage.UpdateChannelPermissions(testCtx(), c1.ID, role.ID, perm); err != nil {
		t.Fatalf("falha ao configurar permissões do canal: %v", err)
	}
	// garante timestamps distintos para a última mensagem
	time.Sleep(10 * time.Millisecond)
	if _, err := storage.CreateMessage(testCtx(), c1.ID, author.ID, "primeira", nil); err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	m2, err := storage.CreateMessage(testCtx(), c1.ID, author.ID, "segunda", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	// sem filtro: retorna todos os canais criados
	summaries, err := storage.ListChannelSummaries(testCtx(), nil)
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
	if got.ServerID != server.ID {
		t.Errorf("esperava server_id %s, obtive %s", server.ID, got.ServerID)
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

	// filtro por servidor: apenas os canais do servidor informado
	summaries, err = storage.ListChannelSummaries(testCtx(), &server.ID)
	if err != nil {
		t.Fatalf("ListChannelSummaries(filtro) retornou erro: %v", err)
	}
	for _, s := range summaries {
		if s.ServerID != server.ID {
			t.Errorf("canal %s pertence a outro servidor (%s)", s.ID, s.ServerID)
		}
	}
}

func TestGetChannelSummary(t *testing.T) {
	author := newTestUser(t)
	server := newTestServer(t, nil)
	channel := newTestChannel(t, server.ID)
	role := newTestRole(t, server.ID)
	perm := models.ChannelPermission{SendMessages: true, DeleteMessages: true}

	if _, err := storage.UpdateChannelPermissions(testCtx(), channel.ID, role.ID, perm); err != nil {
		t.Fatalf("falha ao configurar permissões do canal: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	message, err := storage.CreateMessage(testCtx(), channel.ID, author.ID, "mensagem única", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	summary, err := storage.GetChannelSummary(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelSummary retornou erro: %v", err)
	}

	if summary.ID != channel.ID {
		t.Errorf("esperava id %s, obtive %s", channel.ID, summary.ID)
	}
	if summary.ServerID != server.ID {
		t.Errorf("esperava server_id %s, obtive %s", server.ID, summary.ServerID)
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
	emptyChannel := newTestChannel(t, server.ID)
	emptySummary, err := storage.GetChannelSummary(testCtx(), emptyChannel.ID)
	if err != nil {
		t.Fatalf("GetChannelSummary (sem mensagens) retornou erro: %v", err)
	}
	if emptySummary.LastMessage != nil {
		t.Errorf("esperava last_message nil, obtive %+v", emptySummary.LastMessage)
	}
	if emptySummary.Permissions == nil || len(emptySummary.Permissions) != 0 {
		t.Errorf("esperava permissions vazio, obtive %v", emptySummary.Permissions)
	}

	if _, err := storage.GetChannelSummary(testCtx(), randUUID()); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

// --- messages ---

// --- roles ---

func TestCreateRole(t *testing.T) {
	server := newTestServer(t, nil)
	name := "role_" + randHex(8)
	perms := models.RolePermissions{ManageChannels: true, BanMembers: true}

	role, err := storage.CreateRole(testCtx(), server.ID, name, strPtr("#ff0000"), perms)
	if err != nil {
		t.Fatalf("CreateRole retornou erro: %v", err)
	}
	if role.ID == "" {
		t.Error("esperava role.ID preenchido")
	}
	if role.ServerID != server.ID {
		t.Errorf("esperava server_id %s, obtive %s", server.ID, role.ServerID)
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
	roleNoColor, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(8), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("CreateRole sem color retornou erro: %v", err)
	}
	if roleNoColor.Color != nil {
		t.Errorf("esperava color nil, obtive %v", roleNoColor.Color)
	}
}

func TestCreateRoleDuplicateName(t *testing.T) {
	server := newTestServer(t, nil)
	name := "role_" + randHex(8)

	if _, err := storage.CreateRole(testCtx(), server.ID, name, nil, models.RolePermissions{}); err != nil {
		t.Fatalf("falha ao criar primeira role: %v", err)
	}
	if _, err := storage.CreateRole(testCtx(), server.ID, name, nil, models.RolePermissions{}); !errors.Is(err, storage.ErrUniqueViolation) {
		t.Errorf("esperava ErrUniqueViolation, obtive %v", err)
	}

	// o mesmo nome em outro servidor é permitido
	otherServer := newTestServer(t, nil)
	if _, err := storage.CreateRole(testCtx(), otherServer.ID, name, nil, models.RolePermissions{}); err != nil {
		t.Errorf("mesmo nome em outro servidor deveria ser permitido, obtive %v", err)
	}
}

// listRoleNames é usado por ListChannelSummaries/GetChannelSummary para
// expandir as permissões dos canais com o nome de cada role.
// TestListRoleNames verifica a expansão de nomes de roles nas permissões dos
// canais, exercitando listRoleNames (não exportado) via GetChannelSummary e
// ListChannelSummaries.
func TestListRoleNames(t *testing.T) {
	server := newTestServer(t, nil)
	otherServer := newTestServer(t, nil)
	r1 := newTestRole(t, server.ID)
	r2 := newTestRole(t, server.ID)
	r3 := newTestRole(t, otherServer.ID)
	channel := newTestChannel(t, server.ID)
	perm1 := models.ChannelPermission{ReadChannel: true}
	perm2 := models.ChannelPermission{SendMessages: true, DeleteMessages: true}

	if _, err := storage.UpdateChannelPermissions(testCtx(), channel.ID, r1.ID, perm1); err != nil {
		t.Fatalf("falha ao configurar permissões da role r1: %v", err)
	}
	if _, err := storage.UpdateChannelPermissions(testCtx(), channel.ID, r2.ID, perm2); err != nil {
		t.Fatalf("falha ao configurar permissões da role r2: %v", err)
	}
	// role de outro servidor também é resolvida pelo nome (mapa global)
	if _, err := storage.UpdateChannelPermissions(testCtx(), channel.ID, r3.ID, perm1); err != nil {
		t.Fatalf("falha ao configurar permissões da role r3: %v", err)
	}

	summary, err := storage.GetChannelSummary(testCtx(), channel.ID)
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
		wantName, err := storage.GetRoleByID(testCtx(), role)
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
	summaries, err := storage.ListChannelSummaries(testCtx(), &server.ID)
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
	server := newTestServer(t, nil)
	role := newTestRole(t, server.ID)

	got, err := storage.GetRoleByID(testCtx(), role.ID)
	if err != nil {
		t.Fatalf("GetRoleByID retornou erro: %v", err)
	}
	if got.ID != role.ID || got.ServerID != role.ServerID || got.Name != role.Name {
		t.Errorf("role retornado não confere: got %+v, want ID=%s name=%s", got, role.ID, role.Name)
	}

	if _, err := storage.GetRoleByID(testCtx(), randUUID()); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestListRolesByServer(t *testing.T) {
	server := newTestServer(t, nil)
	otherServer := newTestServer(t, nil)
	r1 := newTestRole(t, server.ID)
	r2 := newTestRole(t, server.ID)
	newTestRole(t, otherServer.ID)

	roles, err := storage.ListRolesByServer(testCtx(), server.ID)
	if err != nil {
		t.Fatalf("ListRolesByServer retornou erro: %v", err)
	}

	ids := make(map[string]bool, len(roles))
	for _, r := range roles {
		ids[r.ID] = true
		if r.ServerID != server.ID {
			t.Errorf("role %s pertence a outro servidor (%s)", r.ID, r.ServerID)
		}
	}
	if !ids[r1.ID] || !ids[r2.ID] {
		t.Errorf("ListRolesByServer não retornou as roles criadas: got %v", ids)
	}
}

func TestUpdateRole(t *testing.T) {
	server := newTestServer(t, nil)
	role := newTestRole(t, server.ID)
	newName := "role_" + randHex(8)
	newPerms := models.RolePermissions{ManageServer: true, PinMessage: true}

	updated, err := storage.UpdateRole(testCtx(), role.ID, models.Role{
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

	if _, err := storage.UpdateRole(testCtx(), randUUID(), models.Role{}); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestDeleteRole(t *testing.T) {
	server := newTestServer(t, nil)
	user := newTestUser(t)
	role := newTestRole(t, server.ID)
	channel := newTestChannel(t, server.ID)

	// a role tem permissão no canal e é atribuída ao usuário
	if _, err := storage.UpdateChannelPermissions(testCtx(), channel.ID, role.ID, models.ChannelPermission{ReadChannel: true}); err != nil {
		t.Fatalf("falha ao configurar permissões do canal: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), user.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}

	if err := storage.DeleteRole(testCtx(), role.ID); err != nil {
		t.Fatalf("DeleteRole retornou erro: %v", err)
	}
	if _, err := storage.GetRoleByID(testCtx(), role.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound após exclusão, obtive %v", err)
	}

	// a entrada da role é removida das permissões dos canais do servidor
	updatedChannel, err := storage.GetChannelByID(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if _, exists := updatedChannel.Permissions[role.ID]; exists {
		t.Error("esperava a role removida das permissões do canal")
	}

	// cascade: a atribuição em user_roles é removida
	roles, err := storage.GetRolesByUser(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetRolesByUser retornou erro: %v", err)
	}
	if len(roles) != 0 {
		t.Errorf("esperava user_roles esvaziado após DeleteRole, obtive %v", roles)
	}

	if err := storage.DeleteRole(testCtx(), randUUID()); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestAssignUserRole(t *testing.T) {
	user := newTestUser(t)
	server := newTestServer(t, nil)
	role := newTestRole(t, server.ID)

	userRole, err := storage.AssignUserRole(testCtx(), user.ID, role.ID)
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
	if _, err := storage.AssignUserRole(testCtx(), user.ID, role.ID); !errors.Is(err, storage.ErrUniqueViolation) {
		t.Errorf("esperava ErrUniqueViolation para atribuição duplicada, obtive %v", err)
	}
}

func TestRemoveUserRole(t *testing.T) {
	user := newTestUser(t)
	server := newTestServer(t, nil)
	role := newTestRole(t, server.ID)
	if _, err := storage.AssignUserRole(testCtx(), user.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role de apoio: %v", err)
	}

	if err := storage.RemoveUserRole(testCtx(), user.ID, role.ID); err != nil {
		t.Fatalf("RemoveUserRole retornou erro: %v", err)
	}
	userIDs, err := storage.GetUsersByRole(testCtx(), role.ID)
	if err != nil {
		t.Fatalf("GetUsersByRole retornou erro: %v", err)
	}
	if len(userIDs) != 0 {
		t.Errorf("esperava role sem usuários após remoção, obtive %v", userIDs)
	}
	if err := storage.RemoveUserRole(testCtx(), user.ID, role.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound ao remover novamente, obtive %v", err)
	}
}

func TestGetRolesByUser(t *testing.T) {
	user := newTestUser(t)
	server := newTestServer(t, nil)
	r1 := newTestRole(t, server.ID)
	r2 := newTestRole(t, server.ID)
	newTestRole(t, server.ID)

	if _, err := storage.AssignUserRole(testCtx(), user.ID, r1.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), user.ID, r2.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}

	roles, err := storage.GetRolesByUser(testCtx(), user.ID)
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
	empty, err := storage.GetRolesByUser(testCtx(), newTestUser(t).ID)
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
	server := newTestServer(t, nil)
	role := newTestRole(t, server.ID)

	if _, err := storage.AssignUserRole(testCtx(), u1.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), u2.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}

	userIDs, err := storage.GetUsersByRole(testCtx(), role.ID)
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
	server := newTestServer(t, nil)
	name := "emoji_" + randHex(8)
	blob := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a}

	emoji, err := storage.CreateEmoji(testCtx(), server.ID, name, "PNG", blob, &creator.ID)
	if err != nil {
		t.Fatalf("CreateEmoji retornou erro: %v", err)
	}
	if emoji.ID == "" {
		t.Error("esperava emoji.ID preenchido")
	}
	if emoji.ServerID != server.ID {
		t.Errorf("esperava server_id %s, obtive %s", server.ID, emoji.ServerID)
	}
	if emoji.Name != name {
		t.Errorf("esperava name %q, obtive %q", name, emoji.Name)
	}
	if emoji.Format != "PNG" {
		t.Errorf("esperava format %q, obtive %q", "PNG", emoji.Format)
	}
	if string(emoji.ImageBlob) != string(blob) {
		t.Errorf("esperava image_blob %v, obtive %v", blob, emoji.ImageBlob)
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
	server := newTestServer(t, nil)
	name := "emoji_" + randHex(8)

	if _, err := storage.CreateEmoji(testCtx(), server.ID, name, "PNG", []byte{1}, &creator.ID); err != nil {
		t.Fatalf("falha ao criar primeiro emoji: %v", err)
	}
	if _, err := storage.CreateEmoji(testCtx(), server.ID, name, "PNG", []byte{2}, &creator.ID); !errors.Is(err, storage.ErrUniqueViolation) {
		t.Errorf("esperava ErrUniqueViolation, obtive %v", err)
	}
}

func TestGetEmojiByID(t *testing.T) {
	creator := newTestUser(t)
	server := newTestServer(t, nil)
	created, err := storage.CreateEmoji(testCtx(), server.ID, "emoji_"+randHex(8), "GIF", []byte{0x47, 0x49, 0x46}, &creator.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji de apoio: %v", err)
	}

	got, err := storage.GetEmojiByID(testCtx(), created.ID)
	if err != nil {
		t.Fatalf("GetEmojiByID retornou erro: %v", err)
	}
	if got.ID != created.ID || got.ServerID != created.ServerID || got.Name != created.Name || got.Format != created.Format {
		t.Errorf("emoji retornado não confere: got %+v, want ID=%s name=%s", got, created.ID, created.Name)
	}

	if _, err := storage.GetEmojiByID(testCtx(), randUUID()); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestListEmojisByServer(t *testing.T) {
	creator := newTestUser(t)
	server := newTestServer(t, nil)
	otherServer := newTestServer(t, nil)
	e1, err := storage.CreateEmoji(testCtx(), server.ID, "emoji_"+randHex(8), "PNG", []byte{1}, &creator.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}
	e2, err := storage.CreateEmoji(testCtx(), server.ID, "emoji_"+randHex(8), "PNG", []byte{2}, &creator.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}
	if _, err := storage.CreateEmoji(testCtx(), otherServer.ID, "emoji_"+randHex(8), "PNG", []byte{3}, &creator.ID); err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}

	emojis, err := storage.ListEmojisByServer(testCtx(), server.ID)
	if err != nil {
		t.Fatalf("ListEmojisByServer retornou erro: %v", err)
	}

	ids := make(map[string]bool, len(emojis))
	for _, e := range emojis {
		ids[e.ID] = true
		if e.ServerID != server.ID {
			t.Errorf("emoji %s pertence a outro servidor (%s)", e.ID, e.ServerID)
		}
	}
	if !ids[e1.ID] || !ids[e2.ID] {
		t.Errorf("ListEmojisByServer não retornou os emojis criados: got %v", ids)
	}
}

func TestListEmojis(t *testing.T) {
	creator := newTestUser(t)
	server := newTestServer(t, nil)
	otherServer := newTestServer(t, nil)

	e1, err := storage.CreateEmoji(testCtx(), server.ID, "emoji_"+randHex(8), "PNG", []byte{1}, &creator.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	e2, err := storage.CreateEmoji(testCtx(), server.ID, "emoji_"+randHex(8), "PNG", []byte{2}, &creator.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	e3, err := storage.CreateEmoji(testCtx(), server.ID, "emoji_"+randHex(8), "PNG", []byte{3}, &creator.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}
	other, err := storage.CreateEmoji(testCtx(), otherServer.ID, "emoji_"+randHex(8), "PNG", []byte{4}, &creator.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}

	// sem filtros: todos os emojis, em ordem (created_at, id)
	all, err := storage.ListEmojis(testCtx(), nil, nil, "", 0)
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

	// filtro por servidor
	serverEmojis, err := storage.ListEmojis(testCtx(), &server.ID, nil, "", 0)
	if err != nil {
		t.Fatalf("ListEmojis com filtro retornou erro: %v", err)
	}
	if len(serverEmojis) != 3 {
		t.Fatalf("esperava 3 emojis do servidor, obtive %d", len(serverEmojis))
	}
	for _, e := range serverEmojis {
		if e.ServerID != server.ID {
			t.Errorf("emoji %s pertence a outro servidor (%s)", e.ID, e.ServerID)
		}
	}

	// limit
	limited, err := storage.ListEmojis(testCtx(), nil, nil, "", 2)
	if err != nil {
		t.Fatalf("ListEmojis com limit retornou erro: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("esperava 2 emojis com limit, obtive %d", len(limited))
	}

	// since: apenas emojis criados após e2
	since := e2.CreatedAt
	after, err := storage.ListEmojis(testCtx(), nil, &since, "", 0)
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
	if _, err := storage.GetDB().ExecContext(testCtx(),
		"UPDATE emojis SET created_at = $1 WHERE id = $2", e2.CreatedAt, e3.ID); err != nil {
		t.Fatalf("falha ao igualar created_at: %v", err)
	}
	// e2 e e3 agora têm o mesmo timestamp; o cursor (created_at, id) deve
	// posicionar pela ordem dos ids
	boundary, next := e2, e3
	if e3.ID < e2.ID {
		boundary, next = e3, e2
	}

	afterBoundary, err := storage.ListEmojis(testCtx(), nil, &since, boundary.ID, 0)
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
	ignored, err := storage.ListEmojis(testCtx(), nil, nil, boundary.ID, 0)
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

func TestCountEmojisByServer(t *testing.T) {
	creator := newTestUser(t)
	server := newTestServer(t, nil)
	otherServer := newTestServer(t, nil)

	for i := 0; i < 3; i++ {
		if _, err := storage.CreateEmoji(testCtx(), server.ID, "emoji_"+randHex(8), "PNG", []byte{1}, &creator.ID); err != nil {
			t.Fatalf("falha ao criar emoji: %v", err)
		}
	}
	if _, err := storage.CreateEmoji(testCtx(), otherServer.ID, "emoji_"+randHex(8), "PNG", []byte{2}, &creator.ID); err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}

	count, err := storage.CountEmojisByServer(testCtx(), server.ID)
	if err != nil {
		t.Fatalf("CountEmojisByServer retornou erro: %v", err)
	}
	if count != 3 {
		t.Errorf("esperava 3 emojis, obtive %d", count)
	}

	otherCount, err := storage.CountEmojisByServer(testCtx(), otherServer.ID)
	if err != nil {
		t.Fatalf("CountEmojisByServer retornou erro: %v", err)
	}
	if otherCount != 1 {
		t.Errorf("esperava 1 emoji, obtive %d", otherCount)
	}

	empty, err := storage.CountEmojisByServer(testCtx(), randUUID())
	if err != nil {
		t.Fatalf("CountEmojisByServer retornou erro: %v", err)
	}
	if empty != 0 {
		t.Errorf("esperava 0 emojis para servidor inexistente, obtive %d", empty)
	}
}

func TestDeleteEmoji(t *testing.T) {
	creator := newTestUser(t)
	server := newTestServer(t, nil)
	emoji, err := storage.CreateEmoji(testCtx(), server.ID, "emoji_"+randHex(8), "PNG", []byte{1}, &creator.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}

	if err := storage.DeleteEmoji(testCtx(), emoji.ID); err != nil {
		t.Fatalf("DeleteEmoji retornou erro: %v", err)
	}
	if _, err := storage.GetEmojiByID(testCtx(), emoji.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound após a exclusão, obtive %v", err)
	}
	if err := storage.DeleteEmoji(testCtx(), emoji.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound na segunda exclusão, obtive %v", err)
	}
	if err := storage.DeleteEmoji(testCtx(), randUUID()); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

// --- user_settings ---

func TestGetUserSettings(t *testing.T) {
	user := newTestUser(t)

	settings, err := storage.GetUserSettings(testCtx(), user.ID)
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

	if _, err := storage.GetUserSettings(testCtx(), randUUID()); !errors.Is(err, storage.ErrNotFound) {
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
	updated, err := storage.UpsertUserSettings(testCtx(), user.ID, config)
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
	stored, err := storage.GetUserSettings(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserSettings retornou erro: %v", err)
	}
	if stored.Config != config {
		t.Errorf("config persistido não confere:\n got  %+v\n want %+v", stored.Config, config)
	}

	// upsert para usuário sem settings cria o registro
	newUser := newTestUser(t)
	created, err := storage.UpsertUserSettings(testCtx(), newUser.ID, config)
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
	if _, err := storage.GetDB().ExecContext(testCtx(), "DELETE FROM servers"); err != nil {
		t.Fatalf("falha ao limpar a tabela servers: %v", err)
	}
}

func TestGetServerWithPasswordHashAny(t *testing.T) {
	removeAllServersTest(t)

	// bootstrap: sem servidor no banco
	_, err := storage.GetServerWithPasswordHash(testCtx())
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("esperava ErrNotFound sem servidor, obtive %v", err)
	}

	// servidor não público: retorna o password_hash
	password := "server_pw_" + randHex(4)
	hash, err := utils.HashPassword(password)
	if err != nil {
		t.Fatalf("falha ao gerar hash da senha do servidor: %v", err)
	}
	if _, err := storage.CreateServerWithIcon(testCtx(), "server_"+randHex(8), nil, "", false, nil, &hash); err != nil {
		t.Fatalf("falha ao criar servidor não público: %v", err)
	}

	server, err := storage.GetServerWithPasswordHash(testCtx())
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
	if _, err := storage.CreateServerWithIcon(testCtx(), "server_"+randHex(8), nil, "", true, nil, nil); err != nil {
		t.Fatalf("falha ao criar servidor público: %v", err)
	}

	server, err = storage.GetServerWithPasswordHash(testCtx())
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
	server := newTestServer(t, nil)
	channel := newTestChannel(t, server.ID)

	message, err := storage.CreateMessage(testCtx(), channel.ID, author.ID, "olá mundo", nil)
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
	server := newTestServer(t, nil)
	channel := newTestChannel(t, server.ID)

	message, err := storage.CreateMessage(testCtx(), channel.ID, author.ID, "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	loaded, err := storage.GetMessageByID(testCtx(), message.ID)
	if err != nil {
		t.Fatalf("GetMessageByID retornou erro: %v", err)
	}
	if loaded.Content != nil {
		t.Errorf("esperava content NULL para content vazio, obtive %q", *loaded.Content)
	}
}

func TestCreateMessageWithAttachments(t *testing.T) {
	author := newTestUser(t)
	server := newTestServer(t, nil)
	channel := newTestChannel(t, server.ID)

	attachmentIDs := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		attachment, err := storage.CreateAttachment(testCtx(), models.Attachments{
			OriginalFileName: "arquivo_" + randHex(4) + ".txt",
			MimeType:         "text/plain",
			FilePath:         "attachments/" + randHex(8),
			ShaHash:          randHex(32),
			SizeBytes:        10,
			CreatedBy:        &author.ID,
		})
		if err != nil {
			t.Fatalf("falha ao criar attachment de apoio: %v", err)
		}
		attachmentIDs = append(attachmentIDs, attachment.ID)
	}

	message, err := storage.CreateMessage(testCtx(), channel.ID, author.ID, "com arquivos", attachmentIDs)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	for _, attachmentID := range attachmentIDs {
		attachment, err := storage.GetAttachmentByID(testCtx(), attachmentID)
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
	server := newTestServer(t, nil)
	channel := newTestChannel(t, server.ID)

	message, err := storage.CreateMessage(testCtx(), channel.ID, author.ID, "conteúdo", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}

	loaded, err := storage.GetMessageByID(testCtx(), message.ID)
	if err != nil {
		t.Fatalf("GetMessageByID retornou erro: %v", err)
	}
	if loaded.ID != message.ID {
		t.Errorf("esperava id %s, obtive %s", message.ID, loaded.ID)
	}

	if _, err := storage.GetMessageByID(testCtx(), randUUID()); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestListMessagesByChannel(t *testing.T) {
	author := newTestUser(t)
	server := newTestServer(t, nil)
	channel := newTestChannel(t, server.ID)

	first, err := storage.CreateMessage(testCtx(), channel.ID, author.ID, "primeira", nil)
	if err != nil {
		t.Fatalf("falha ao criar primeira mensagem: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	second, err := storage.CreateMessage(testCtx(), channel.ID, author.ID, "segunda", nil)
	if err != nil {
		t.Fatalf("falha ao criar segunda mensagem: %v", err)
	}

	// canal vizinho com mensagem que não pode vazar
	otherChannel := newTestChannel(t, server.ID)
	if _, err := storage.CreateMessage(testCtx(), otherChannel.ID, author.ID, "de outro canal", nil); err != nil {
		t.Fatalf("falha ao criar mensagem de outro canal: %v", err)
	}

	messages, err := storage.ListMessagesByChannel(testCtx(), channel.ID, nil, "", nil)
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
	if limit, err := storage.ListMessagesByChannel(testCtx(), channel.ID, nil, "", intPtr(1)); err != nil {
		t.Fatalf("ListMessagesByChannel com limit retornou erro: %v", err)
	} else if len(limit) != 1 || limit[0].ID != second.ID {
		t.Errorf("esperava apenas a mensagem mais recente com limit 1, obtive %d", len(limit))
	}

	// since: apenas mensagens criadas após o timestamp
	since := first.CreatedAt
	sinceMessages, err := storage.ListMessagesByChannel(testCtx(), channel.ID, timePtr(since), "", nil)
	if err != nil {
		t.Fatalf("ListMessagesByChannel com since retornou erro: %v", err)
	}
	if len(sinceMessages) != 1 || sinceMessages[0].ID != second.ID {
		t.Errorf("esperava apenas a mensagem após o since, obtive %d", len(sinceMessages))
	}
}

func TestListMessagesWithAttachmentsByChannel(t *testing.T) {
	author := newTestUser(t)
	server := newTestServer(t, nil)
	channel := newTestChannel(t, server.ID)

	// mensagem sem attachment
	plain, err := storage.CreateMessage(testCtx(), channel.ID, author.ID, "sem attachment", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem sem attachment: %v", err)
	}

	// mensagem com dois attachments
	attachmentIDs := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		attachment, err := storage.CreateAttachment(testCtx(), models.Attachments{
			OriginalFileName: "arquivo_" + randHex(4) + ".txt",
			MimeType:         "text/plain",
			FilePath:         "attachments/" + randHex(8),
			ShaHash:          randHex(32),
			SizeBytes:        10,
			CreatedBy:        &author.ID,
		})
		if err != nil {
			t.Fatalf("falha ao criar attachment de apoio: %v", err)
		}
		attachmentIDs = append(attachmentIDs, attachment.ID)
	}
	withAttachments, err := storage.CreateMessage(testCtx(), channel.ID, author.ID, "com attachments", attachmentIDs)
	if err != nil {
		t.Fatalf("falha ao criar mensagem com attachments: %v", err)
	}

	messages, err := storage.ListMessagesWithAttachmentsByChannel(testCtx(), channel.ID, nil, "", 100)
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
	limited, err := storage.ListMessagesWithAttachmentsByChannel(testCtx(), channel.ID, nil, "", 1)
	if err != nil {
		t.Fatalf("ListMessagesWithAttachmentsByChannel com limit 1 retornou erro: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("esperava limit+1 mensagens para has_more, obtive %d", len(limited))
	}

	// since: apenas a mensagem criada após o timestamp
	since := plain.CreatedAt
	sinceMessages, err := storage.ListMessagesWithAttachmentsByChannel(testCtx(), channel.ID, timePtr(since), "", 100)
	if err != nil {
		t.Fatalf("ListMessagesWithAttachmentsByChannel com since retornou erro: %v", err)
	}
	if len(sinceMessages) != 1 || sinceMessages[0].ID != withAttachments.ID {
		t.Errorf("esperava apenas a mensagem após o since, obtive %d", len(sinceMessages))
	}
}

func TestUpdateMessage(t *testing.T) {
	author := newTestUser(t)
	server := newTestServer(t, nil)
	channel := newTestChannel(t, server.ID)

	message, err := storage.CreateMessage(testCtx(), channel.ID, author.ID, "original", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}

	updated, err := storage.UpdateMessage(testCtx(), message.ID, models.Message{Content: strPtr("editado")})
	if err != nil {
		t.Fatalf("UpdateMessage retornou erro: %v", err)
	}
	if updated.Content == nil || *updated.Content != "editado" {
		t.Errorf("esperava content %q, obtive %v", "editado", updated.Content)
	}
	if updated.EditedAt == nil {
		t.Error("esperava edited_at preenchido após a edição")
	}

	if _, err := storage.UpdateMessage(testCtx(), randUUID(), models.Message{Content: strPtr("x")}); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestDeleteMessage(t *testing.T) {
	author := newTestUser(t)
	server := newTestServer(t, nil)
	channel := newTestChannel(t, server.ID)

	attachment, err := storage.CreateAttachment(testCtx(), models.Attachments{
		OriginalFileName: "arquivo.txt",
		MimeType:         "text/plain",
		FilePath:         "attachments/" + randHex(8),
		ShaHash:          randHex(32),
		SizeBytes:        10,
		CreatedBy:        &author.ID,
	})
	if err != nil {
		t.Fatalf("falha ao criar attachment de apoio: %v", err)
	}
	message, err := storage.CreateMessage(testCtx(), channel.ID, author.ID, "vai ser apagada", []string{attachment.ID})
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}

	if err := storage.DeleteMessage(testCtx(), message.ID); err != nil {
		t.Fatalf("DeleteMessage retornou erro: %v", err)
	}
	if _, err := storage.GetMessageByID(testCtx(), message.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound após a exclusão, obtive %v", err)
	}
	// attachments são removidos em cascata pela foreign key
	if _, err := storage.GetAttachmentByID(testCtx(), attachment.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava attachment removido em cascata, obtive %v", err)
	}

	if err := storage.DeleteMessage(testCtx(), randUUID()); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

// --- attachments ---

func TestCreateAttachment(t *testing.T) {
	author := newTestUser(t)

	attachment, err := storage.CreateAttachment(testCtx(), models.Attachments{
		OriginalFileName: "arquivo.txt",
		MimeType:         "text/plain",
		FilePath:         "attachments/" + randHex(8),
		ShaHash:          randHex(32),
		SizeBytes:        10,
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
	if attachment.MimeType != "text/plain" {
		t.Errorf("esperava mime_type %q, obtive %q", "text/plain", attachment.MimeType)
	}
	if attachment.ShaHash == "" {
		t.Error("esperava sha_hash preenchido")
	}
	if attachment.SizeBytes != 10 {
		t.Errorf("esperava size_bytes 10, obtive %d", attachment.SizeBytes)
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

func TestExistsAttachmentByHash(t *testing.T) {
	author := newTestUser(t)
	hash := randHex(32)

	exists, err := storage.ExistsAttachmentByHash(testCtx(), hash)
	if err != nil {
		t.Fatalf("ExistsAttachmentByHash retornou erro: %v", err)
	}
	if exists {
		t.Error("esperava false para hash inexistente")
	}

	if _, err := storage.CreateAttachment(testCtx(), models.Attachments{
		OriginalFileName: "arquivo.txt",
		MimeType:         "text/plain",
		FilePath:         "attachments/" + randHex(8),
		ShaHash:          hash,
		SizeBytes:        10,
		CreatedBy:        &author.ID,
	}); err != nil {
		t.Fatalf("falha ao criar attachment de apoio: %v", err)
	}

	exists, err = storage.ExistsAttachmentByHash(testCtx(), hash)
	if err != nil {
		t.Fatalf("ExistsAttachmentByHash retornou erro: %v", err)
	}
	if !exists {
		t.Error("esperava true para hash já existente")
	}
}

func TestGetAttachmentByID(t *testing.T) {
	author := newTestUser(t)

	attachment, err := storage.CreateAttachment(testCtx(), models.Attachments{
		OriginalFileName: "arquivo.txt",
		MimeType:         "text/plain",
		FilePath:         "attachments/" + randHex(8),
		ShaHash:          randHex(32),
		SizeBytes:        10,
		CreatedBy:        &author.ID,
	})
	if err != nil {
		t.Fatalf("falha ao criar attachment de apoio: %v", err)
	}

	loaded, err := storage.GetAttachmentByID(testCtx(), attachment.ID)
	if err != nil {
		t.Fatalf("GetAttachmentByID retornou erro: %v", err)
	}
	if loaded.ID != attachment.ID {
		t.Errorf("esperava id %s, obtive %s", attachment.ID, loaded.ID)
	}

	if _, err := storage.GetAttachmentByID(testCtx(), randUUID()); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

func TestListAttachmentsByMessage(t *testing.T) {
	author := newTestUser(t)
	server := newTestServer(t, nil)
	channel := newTestChannel(t, server.ID)

	attachmentIDs := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		attachment, err := storage.CreateAttachment(testCtx(), models.Attachments{
			OriginalFileName: "arquivo_" + randHex(4) + ".txt",
			MimeType:         "text/plain",
			FilePath:         "attachments/" + randHex(8),
			ShaHash:          randHex(32),
			SizeBytes:        10,
			CreatedBy:        &author.ID,
		})
		if err != nil {
			t.Fatalf("falha ao criar attachment de apoio: %v", err)
		}
		attachmentIDs = append(attachmentIDs, attachment.ID)
	}
	message, err := storage.CreateMessage(testCtx(), channel.ID, author.ID, "com attachments", attachmentIDs)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}

	attachments, err := storage.ListAttachmentsByMessage(testCtx(), message.ID)
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
	emptyMessage, err := storage.CreateMessage(testCtx(), channel.ID, author.ID, "sem attachments", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}
	empty, err := storage.ListAttachmentsByMessage(testCtx(), emptyMessage.ID)
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
	server := newTestServer(t, &owner.ID)
	channel := newTestChannel(t, server.ID)
	restricted := newTestChannel(t, server.ID)
	role := newTestRole(t, server.ID)
	if _, err := storage.UpdateChannelPermissions(testCtx(), restricted.ID, role.ID, models.ChannelPermission{ReadChannel: true}); err != nil {
		t.Fatalf("falha ao definir permissão no canal restrito: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), reader.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role ao leitor: %v", err)
	}

	m1, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "zebra borboleta", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem 1: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	m2, err := storage.CreateMessage(testCtx(), channel.ID, reader.ID, "borboleta vagalume", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem 2: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	m3, err := storage.CreateMessage(testCtx(), channel.ID, stranger.ID, "vagalume", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem 3: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	attachment, err := storage.CreateAttachment(testCtx(), models.Attachments{
		OriginalFileName: "peixe.txt",
		MimeType:         "text/plain",
		FilePath:         "attachments/test/peixe.txt",
		ShaHash:          randHex(32),
		SizeBytes:        10,
		CreatedBy:        &owner.ID,
	})
	if err != nil {
		t.Fatalf("falha ao criar attachment de apoio: %v", err)
	}
	m4, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "peixe", []string{attachment.ID})
	if err != nil {
		t.Fatalf("falha ao criar mensagem 4 com attachment: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	m5, err := storage.CreateMessage(testCtx(), restricted.ID, owner.ID, "zebra secreta", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem 5 no canal restrito: %v", err)
	}

	base := func(userID string) storage.SearchParams {
		return storage.SearchParams{UserID: userID, Limit: 100}
	}

	t.Run("texto e score", func(t *testing.T) {
		params := base(owner.ID)
		params.Text = "zebra"
		results, err := storage.SearchMessages(testCtx(), params)
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
		results, err = storage.SearchMessages(testCtx(), params)
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
		results, err := storage.SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages com autor retornou erro: %v", err)
		}
		assertSearchSet(t, results, m3.ID)
	})

	t.Run("intervalo de datas", func(t *testing.T) {
		params := base(owner.ID)
		params.DateStart = &m1.CreatedAt
		params.DateEndExclusive = &m5.CreatedAt
		results, err := storage.SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages com datas retornou erro: %v", err)
		}
		assertSearchSet(t, results, m1.ID, m2.ID, m3.ID, m4.ID)

		// limite superior exclusivo: nada antes de m1
		params = base(owner.ID)
		params.DateStart = nil
		params.DateEndExclusive = &m1.CreatedAt
		results, err = storage.SearchMessages(testCtx(), params)
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
		results, err := storage.SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages com contains_attachment true retornou erro: %v", err)
		}
		assertSearchSet(t, results, m4.ID)

		withoutAttachment := false
		params = base(owner.ID)
		params.ContainsAttachment = &withoutAttachment
		results, err = storage.SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages com contains_attachment false retornou erro: %v", err)
		}
		assertSearchSet(t, results, m1.ID, m2.ID, m3.ID, m5.ID)
	})

	t.Run("filtro por server_id", func(t *testing.T) {
		params := base(owner.ID)
		params.AuthorID = owner.ID
		params.ServerID = server.ID
		results, err := storage.SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages com server_id retornou erro: %v", err)
		}
		assertSearchSet(t, results, m1.ID, m4.ID, m5.ID)

		params = base(owner.ID)
		params.AuthorID = owner.ID
		params.ServerID = randUUID()
		results, err = storage.SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages com server_id inexistente retornou erro: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("esperava 0 resultados, obtive %d", len(results))
		}
	})

	t.Run("todos os filtros combinados", func(t *testing.T) {
		withAttachment := true
		params := storage.SearchParams{
			UserID:             owner.ID,
			Text:               "peixe",
			AuthorID:           owner.ID,
			DateStart:          &m1.CreatedAt,
			DateEndExclusive:   &m5.CreatedAt,
			ContainsAttachment: &withAttachment,
			ServerID:           server.ID,
			Limit:              100,
		}
		results, err := storage.SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages com todos os filtros retornou erro: %v", err)
		}
		assertSearchSet(t, results, m4.ID)
	})

	t.Run("ordem", func(t *testing.T) {
		params := base(owner.ID)
		params.AuthorID = owner.ID
		params.OrderAsc = true
		results, err := storage.SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages com order asc retornou erro: %v", err)
		}
		assertSearchOrder(t, results, m1.ID, m4.ID, m5.ID)

		params = base(owner.ID)
		params.AuthorID = owner.ID
		params.OrderAsc = false
		results, err = storage.SearchMessages(testCtx(), params)
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
		page1, err := storage.SearchMessages(testCtx(), params)
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
		page2, err := storage.SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages da segunda página retornou erro: %v", err)
		}
		assertSearchOrder(t, page2, m1.ID)

		params = base(owner.ID)
		params.AuthorID = owner.ID
		params.Limit = 2
		params.Since = &page2[0].CreatedAt
		params.LastID = page2[0].ID
		page3, err := storage.SearchMessages(testCtx(), params)
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
		results, err := storage.SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages do leitor retornou erro: %v", err)
		}
		assertSearchSet(t, results, m1.ID, m5.ID)

		// stranger: sem roles, não vê o canal restrito
		params = base(stranger.ID)
		params.Text = "vagalume"
		results, err = storage.SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages do estranho retornou erro: %v", err)
		}
		assertSearchSet(t, results, m2.ID, m3.ID)

		params = base(stranger.ID)
		params.Text = "secreta"
		results, err = storage.SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages do estranho no restrito retornou erro: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("esperava 0 resultados para o estranho, obtive %d", len(results))
		}

		// owner: vê tudo
		params = base(owner.ID)
		params.Text = "secreta"
		results, err = storage.SearchMessages(testCtx(), params)
		if err != nil {
			t.Fatalf("SearchMessages do dono retornou erro: %v", err)
		}
		assertSearchSet(t, results, m5.ID)
	})

	t.Run("limit acima do máximo", func(t *testing.T) {
		for i := 0; i < 105; i++ {
			if _, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "clamp "+randHex(2), nil); err != nil {
				t.Fatalf("falha ao criar mensagem de clamp %d: %v", i, err)
			}
		}

		params := base(owner.ID)
		params.Text = "clamp"
		params.Limit = 500
		results, err := storage.SearchMessages(testCtx(), params)
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
	limited, err := storage.ListUsers(testCtx(), nil, "", 2)
	if err != nil {
		t.Fatalf("ListUsers com limit retornou erro: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("esperava 2 usuários com limit, obtive %d", len(limited))
	}

	// since: only users created after userB (other tests' users were created before that)
	since := userB.CreatedAt
	after, err := storage.ListUsers(testCtx(), &since, "", 0)
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
	if _, err := storage.GetDB().ExecContext(testCtx(),
		"UPDATE users SET created_at = $1 WHERE id = $2", userB.CreatedAt, userC.ID); err != nil {
		t.Fatalf("failed to equalize created_at: %v", err)
	}
	// userB and userC now have the same timestamp; the (created_at, id) cursor
	// must be positioned by id order
	boundary, next := userB, userC
	if userC.ID < userB.ID {
		boundary, next = userC, userB
	}

	afterBoundary, err := storage.ListUsers(testCtx(), &since, boundary.ID, 0)
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
	ignored, err := storage.ListUsers(testCtx(), nil, boundary.ID, 0)
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

func TestCountChannelsByServer(t *testing.T) {
	server := newTestServer(t, nil)
	otherServer := newTestServer(t, nil)

	for i := 0; i < 3; i++ {
		newTestChannel(t, server.ID)
	}
	newTestChannel(t, otherServer.ID)

	count, err := storage.CountChannelsByServer(testCtx(), server.ID)
	if err != nil {
		t.Fatalf("CountChannelsByServer returned error: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 channels, got %d", count)
	}

	otherCount, err := storage.CountChannelsByServer(testCtx(), otherServer.ID)
	if err != nil {
		t.Fatalf("CountChannelsByServer returned error: %v", err)
	}
	if otherCount != 1 {
		t.Errorf("expected 1 channel, got %d", otherCount)
	}

	empty, err := storage.CountChannelsByServer(testCtx(), randUUID())
	if err != nil {
		t.Fatalf("CountChannelsByServer returned error: %v", err)
	}
	if empty != 0 {
		t.Errorf("expected 0 channels for a nonexistent server, got %d", empty)
	}
}
