package services

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// migrationsDir é o caminho relativo ao diretório deste pacote (backend/internal/services/test_services).
const migrationsDir = "../../../migrations"

// defaultDatabaseURL corresponde aos padrões do infra/docker-compose.yml.
const defaultDatabaseURL = "postgres://papo:papo123@localhost:5432/papo"

func TestMain(m *testing.M) {
	os.Exit(runServicesTests(m))
}

// runServicesTests prepara um banco temporário com as migrations do projeto,
// inicializa o storage contra ele, executa os testes e remove o banco ao final.
func runServicesTests(m *testing.M) int {
	baseURL := testDatabaseURL()

	baseDB, err := sql.Open("pgx", baseURL)
	if err != nil {
		fmt.Printf("testes de services ignorados: falha ao abrir conexão: %v\n", err)
		return 0
	}
	defer baseDB.Close()

	if err := ping(baseDB); err != nil {
		fmt.Printf("testes de services ignorados: não foi possível conectar ao PostgreSQL (%v). Inicie o PostgreSQL (infra/docker-compose.yml) ou defina TEST_DATABASE_URL/DATABASE_URL.\n", err)
		return 0
	}

	removeOldTempDatabases(baseDB)

	tempDBName, err := createTempDatabase(baseDB)
	if err != nil {
		fmt.Printf("testes de services ignorados: falha ao criar banco temporário: %v\n", err)
		return 0
	}
	defer dropTempDatabase(baseDB, tempDBName)

	tempURL, err := withDatabase(baseURL, tempDBName)
	if err != nil {
		fmt.Printf("testes de services ignorados: %v\n", err)
		return 0
	}

	tempDB, err := sql.Open("pgx", tempURL)

	if err != nil {
		fmt.Printf("testes de services ignorados: %v\n", err)
		return 0
	}
	defer tempDB.Close()

	if err := ping(tempDB); err != nil {
		fmt.Printf("testes de services ignorados: falha ao conectar no banco temporário: %v\n", err)
	}

	if err := applyMigrations(tempDB); err != nil {
		fmt.Printf("testes de services FALHARAM na preparação: %v\n", err)
		return 1
	}

	if err := storage.InitDB(tempURL); err != nil {
		fmt.Printf("testes de services FALHARAM na preparação: %v\n", err)
		return 1
	}

	// remove blobs de execuções anteriores (os uploads de mídia gravam em
	// media/ relativo ao diretório deste pacote de testes)
	if err := os.RemoveAll("media"); err != nil {
		fmt.Printf("aviso: falha ao limpar pasta de mídia de testes: %v\n", err)
	}

	code := m.Run()

	storage.CloseDB()
	return code
}

// exclui o servidor e todo o estado escopado ao servidor nos testes para
// manter a regra de negócio de 1 servidor por backend (a ordem respeita as
// foreign keys entre as tabelas)
func cleanServers(ctx context.Context) error {
	for _, query := range []string{
		"DELETE FROM user_roles",
		"DELETE FROM roles",
		"DELETE FROM attachment_thumbnails",
		"DELETE FROM attachments",
		"DELETE FROM message_reactions",
		"DELETE FROM messages",
		"DELETE FROM user_channel_state",
		"DELETE FROM channels",
		"DELETE FROM emojis",
		"DELETE FROM servers",
	} {
		if _, err := storage.GetDB().ExecContext(ctx, query); err != nil {
			return err
		}
	}

	return nil
}

// newTestMediaHash grava o conteúdo no content-addressable storage (tabela
// media + blob em disco) e retorna o sha256 — referência usada por avatar,
// ícone, emoji, attachment, thumbnail e link preview.
func newTestMediaHash(t *testing.T, content []byte) string {
	t.Helper()
	hash, _, err := StoreMediaFromBytes(testCtx(), content, "image/png")
	if err != nil {
		t.Fatalf("falha ao gravar mídia de apoio: %v", err)
	}
	return hash
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

// testActorOnce/sharedActorID mantêm um único usuário compartilhado usado como
// ator nos testes. As funções de service agora recebem o actorID explicitamente
// (em produção vem do JWT); nos testes usamos este usuário estável. Os testes
// toleram usuários extras (a paginação de ListUsers anda pela fronteira e as
// contagens de membros são dinâmicas).
var (
	testActorOnce sync.Once
	sharedActorID string
)

func testActorID() string {
	testActorOnce.Do(func() {
		user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
		if err != nil {
			panic(fmt.Errorf("falha ao criar usuário ator de teste: %w", err))
		}
		sharedActorID = user.ID
	})
	return sharedActorID
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func newRandomUsername() string {
	return "usr" + randHex(4)
}

func newRandomPassword() string {
	return "pw" + randHex(4)
}

func newRandomIP() string {
	return fmt.Sprintf("10.0.%d.%d", randInt(1, 254), randInt(1, 254))
}

func randInt(min, max int) int {
	if min > max {
		min, max = max, min
	}

	n, err := rand.Int(rand.Reader, big.NewInt(int64(max-min+1)))
	if err != nil {
		panic(err)
	}

	return int(n.Int64()) + min
}

// randUUID gera um UUID v4 válido para simular um ID inexistente.
func randUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func timePtr(v time.Time) *time.Time {
	return &v
}

// --- Register ---

func TestRegister(t *testing.T) {
	username := newRandomUsername()
	password := newRandomPassword()
	ip := newRandomIP()

	user, err := Register(testCtx(), username, password, ip)
	if err != nil {
		t.Fatalf("Register retornou erro: %v", err)
	}

	if user.ID == "" {
		t.Error("esperava user.ID preenchido")
	}
	if user.Username != username {
		t.Errorf("esperava username %q, obtive %q", username, user.Username)
	}
	// Register não deve retornar o hash do banco
	if user.PasswordHash != "" {
		t.Errorf("Register não deve retornar password_hash, obtive %q", user.PasswordHash)
	}
	if user.Banned {
		t.Error("esperava banned = false")
	}
	if user.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}
	if user.LastIP == nil || *user.LastIP != ip {
		t.Errorf("esperava last_ip %q, obtive %v", ip, user.LastIP)
	}

	// o hash persistido deve validar a senha original
	stored, err := storage.GetUserByUsername(testCtx(), username)
	if err != nil {
		t.Fatalf("GetUserByUsername retornou erro: %v", err)
	}
	if err := utils.CheckPassword(password, stored.PasswordHash); err != nil {
		t.Errorf("CheckPassword falhou para o hash persistido: %v", err)
	}
}

func getMaxLenFields() (int, int) {
	cfg := config.LoadConfig()

	return cfg.MaxPasswordLength, cfg.MaxUsernameLength
}

func TestRegisterInvalidInput(t *testing.T) {
	//Lê a configuração do tamanho máximo dos campos
	MaxPasswordLength, MaxUsernameLength := getMaxLenFields()

	cases := []struct {
		name     string
		username string
		password string
	}{

		{"username vazio", "", "senha123"},
		{"password vazio", "user123", ""},
		{"username acima do limite", strings.Repeat("a", MaxUsernameLength+1), "senha123"},
		{"password acima do limite", "user123", strings.Repeat("a", MaxPasswordLength+1)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Register(testCtx(), tc.username, tc.password, newRandomIP())
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("esperava ErrInvalidInput, obtive %v", err)
			}
		})
	}
}

func TestRegisterBoundaryLengths(t *testing.T) {
	//Lê a configuração do tamanho máximo dos campos
	MaxPasswordLength, MaxUsernameLength := getMaxLenFields()

	username := strings.Repeat("a", MaxUsernameLength)
	password := strings.Repeat("b", MaxPasswordLength)

	user, err := Register(testCtx(), username, password, newRandomIP())
	if err != nil {
		t.Fatalf("Register com tamanhos no limite retornou erro: %v", err)
	}
	if user.Username != username {
		t.Errorf("esperava username %q, obtive %q", username, user.Username)
	}
}

func TestRegisterUsernameTaken(t *testing.T) {
	username := newRandomUsername()
	ip := newRandomIP()

	if _, err := Register(testCtx(), username, newRandomPassword(), ip); err != nil {
		t.Fatalf("falha ao criar primeiro usuário: %v", err)
	}

	_, err := Register(testCtx(), username, newRandomPassword(), newRandomIP())
	if !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("esperava ErrUsernameTaken, obtive %v", err)
	}
}

func TestRegisterBannedIP(t *testing.T) {
	ip := newRandomIP()
	bannedUser, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), ip)
	if err != nil {
		t.Fatalf("falha ao criar usuário para banir: %v", err)
	}

	if _, err := storage.SetUserBanned(testCtx(), bannedUser.ID, true); err != nil {
		t.Fatalf("falha ao banir usuário: %v", err)
	}

	_, err = Register(testCtx(), newRandomUsername(), newRandomPassword(), ip)
	if !errors.Is(err, ErrBannedIP) {
		t.Errorf("esperava ErrBannedIP, obtive %v", err)
	}
}

// --- Login ---

func TestLogin(t *testing.T) {
	username := newRandomUsername()
	password := newRandomPassword()
	ip := newRandomIP()

	if _, err := Register(testCtx(), username, password, ip); err != nil {
		t.Fatalf("falha ao criar usuário para login: %v", err)
	}

	user, err := Login(testCtx(), username, password, ip)
	if err != nil {
		t.Fatalf("Login retornou erro: %v", err)
	}
	if user.ID == "" {
		t.Error("esperava user.ID preenchido")
	}
	if user.Username != username {
		t.Errorf("esperava username %q, obtive %q", username, user.Username)
	}
	// Login não deve retornar o hash do banco
	if user.PasswordHash != "" {
		t.Errorf("Login não deve retornar password_hash, obtive %q", user.PasswordHash)
	}
	if user.Banned {
		t.Error("esperava banned = false")
	}
	if user.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}

	// o login deve atualizar o last_ip do usuário
	stored, err := storage.GetUserByUsername(testCtx(), username)
	if err != nil {
		t.Fatalf("GetUserByUsername retornou erro: %v", err)
	}
	if err := utils.CheckPassword(password, stored.PasswordHash); err != nil {
		t.Errorf("CheckPassword falhou para o hash persistido: %v", err)
	}
}

func TestLoginInvalidInput(t *testing.T) {
	MaxPasswordLength, MaxUsernameLength := getMaxLenFields()

	cases := []struct {
		name     string
		username string
		password string
	}{
		{"username vazio", "", "senha123"},
		{"password vazio", "user123", ""},
		{"username acima do limite", strings.Repeat("a", MaxUsernameLength+1), "senha123"},
		{"password acima do limite", "user123", strings.Repeat("a", MaxPasswordLength+1)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Login(testCtx(), tc.username, tc.password, newRandomIP())
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("esperava ErrInvalidInput, obtive %v", err)
			}
		})
	}
}

func TestLoginBoundaryLengths(t *testing.T) {
	MaxPasswordLength, MaxUsernameLength := getMaxLenFields()

	username := strings.Repeat("a", MaxUsernameLength-2) + randHex(1)
	password := strings.Repeat("b", MaxPasswordLength)
	ip := newRandomIP()

	if _, err := Register(testCtx(), username, password, ip); err != nil {
		t.Fatalf("falha ao criar usuário com tamanhos no limite: %v", err)
	}

	if _, err := Login(testCtx(), username, password, ip); err != nil {
		t.Errorf("Login com tamanhos no limite retornou erro: %v", err)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	username := newRandomUsername()
	ip := newRandomIP()

	if _, err := Register(testCtx(), username, newRandomPassword(), ip); err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	_, err := Login(testCtx(), username, newRandomPassword(), ip)
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("esperava ErrInvalidCredentials, obtive %v", err)
	}
}

func TestLoginNonexistentUser(t *testing.T) {
	ip := newRandomIP()
	_, err := Login(testCtx(), newRandomUsername(), newRandomPassword(), ip)
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("esperava ErrInvalidCredentials, obtive %v", err)
	}
}

func TestLoginBannedUser(t *testing.T) {
	username := newRandomUsername()
	password := newRandomPassword()
	ip := newRandomIP()

	user, err := Register(testCtx(), username, password, ip)
	if err != nil {
		t.Fatalf("falha ao criar usuário para banir: %v", err)
	}

	if _, err := storage.SetUserBanned(testCtx(), user.ID, true); err != nil {
		t.Fatalf("falha ao banir usuário: %v", err)
	}

	_, err = Login(testCtx(), username, password, ip)
	if !errors.Is(err, ErrBannedIP) {
		t.Errorf("esperava ErrBannedIP, obtive %v", err)
	}
}

func TestLoginBannedIP(t *testing.T) {
	ip := newRandomIP()
	bannedUser, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), ip)
	if err != nil {
		t.Fatalf("falha ao criar usuário para banir: %v", err)
	}

	if _, err := storage.SetUserBanned(testCtx(), bannedUser.ID, true); err != nil {
		t.Fatalf("falha ao banir usuário: %v", err)
	}

	_, err = Login(testCtx(), newRandomUsername(), newRandomPassword(), ip)
	if !errors.Is(err, ErrBannedIP) {
		t.Errorf("esperava ErrBannedIP, obtive %v", err)
	}
}

// --- Whoami ---

func TestWhoami(t *testing.T) {
	username := newRandomUsername()
	password := newRandomPassword()
	ip := newRandomIP()

	created, err := Register(testCtx(), username, password, ip)
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	user, settings, err := Whoami(testCtx(), created.ID)
	if err != nil {
		t.Fatalf("Whoami retornou erro: %v", err)
	}

	if user.ID != created.ID {
		t.Errorf("esperava id %s, obtive %s", created.ID, user.ID)
	}
	if user.Username != username {
		t.Errorf("esperava username %q, obtive %q", username, user.Username)
	}
	// Whoami não deve retornar o hash do banco
	if user.PasswordHash != "" {
		t.Errorf("Whoami não deve retornar password_hash, obtive %q", user.PasswordHash)
	}
	if user.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}

	if settings.UserID != created.ID {
		t.Errorf("esperava settings.user_id %s, obtive %s", created.ID, settings.UserID)
	}
	if settings.Version != models.CurrentVersion {
		t.Errorf("esperava settings.version %d, obtive %d", models.CurrentVersion, settings.Version)
	}
	if settings.Config != (models.UserConfig{}) {
		t.Errorf("esperava settings.config vazio, obtive %+v", settings.Config)
	}
	if settings.UpdatedAt.IsZero() {
		t.Error("esperava settings.updated_at preenchido")
	}
}

func TestWhoamiReflectsProfileUpdate(t *testing.T) {
	ip := newRandomIP()
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), ip)
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	nickname := "nick_" + randHex(4)
	status := "disponível"
	updatedAt := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := storage.UpdateUser(testCtx(), user.ID, models.User{
		Nickname:        &nickname,
		StatusMessage:   &status,
		StatusUpdatedAt: &updatedAt,
	}); err != nil {
		t.Fatalf("falha ao atualizar perfil: %v", err)
	}

	got, _, err := Whoami(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("Whoami retornou erro: %v", err)
	}
	if got.Nickname == nil || *got.Nickname != nickname {
		t.Errorf("esperava nickname %q, obtive %v", nickname, got.Nickname)
	}
	if got.StatusMessage == nil || *got.StatusMessage != status {
		t.Errorf("esperava status_message %q, obtive %v", status, got.StatusMessage)
	}
	if got.StatusUpdatedAt == nil || !got.StatusUpdatedAt.Equal(updatedAt) {
		t.Errorf("esperava status_updated_at %v, obtive %v", updatedAt, got.StatusUpdatedAt)
	}
}

func TestWhoamiReflectsSettingsUpdate(t *testing.T) {
	ip := newRandomIP()
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), ip)
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

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
	if _, err := storage.UpsertUserSettings(testCtx(), user.ID, config); err != nil {
		t.Fatalf("falha ao atualizar settings: %v", err)
	}

	_, settings, err := Whoami(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("Whoami retornou erro: %v", err)
	}
	if settings.Config != config {
		t.Errorf("config não confere:\n got  %+v\n want %+v", settings.Config, config)
	}
}

func TestWhoamiEmptyUserID(t *testing.T) {
	_, _, err := Whoami(testCtx(), "")
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para userID vazio, obtive %v", err)
	}
}

func TestWhoamiNonexistentUser(t *testing.T) {
	_, _, err := Whoami(testCtx(), randUUID())
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para id inexistente, obtive %v", err)
	}
}

func TestWhoamiIncludesRoles(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	color := "#FF0000"
	role, err := storage.CreateRole(testCtx(), newRandomRoleName(), &color, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), user.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}

	got, _, err := Whoami(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("Whoami retornou erro: %v", err)
	}
	if len(got.Roles) != 1 {
		t.Fatalf("esperava 1 role, obtive %d", len(got.Roles))
	}
	if got.Roles[0].ID != role.ID || got.Roles[0].Name != role.Name {
		t.Errorf("role não confere: got %+v", got.Roles[0])
	}
	if got.Roles[0].Color == nil || *got.Roles[0].Color != color {
		t.Errorf("esperava cor %q, obtive %v", color, got.Roles[0].Color)
	}
}

func TestWhoamiNoRoles(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	got, _, err := Whoami(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("Whoami retornou erro: %v", err)
	}
	if got.Roles == nil || len(got.Roles) != 0 {
		t.Errorf("esperava roles vazia (não nil), obtive %v", got.Roles)
	}
}

// --- Profile ---

func TestProfile(t *testing.T) {
	username := newRandomUsername()
	password := newRandomPassword()
	ip := newRandomIP()

	created, err := Register(testCtx(), username, password, ip)
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	user, err := Profile(testCtx(), created.ID)
	if err != nil {
		t.Fatalf("Profile retornou erro: %v", err)
	}

	if user.ID != created.ID {
		t.Errorf("esperava id %s, obtive %s", created.ID, user.ID)
	}
	if user.Username != username {
		t.Errorf("esperava username %q, obtive %q", username, user.Username)
	}
	// Profile não deve retornar o hash do banco
	if user.PasswordHash != "" {
		t.Errorf("Profile não deve retornar password_hash, obtive %q", user.PasswordHash)
	}
	if user.Banned {
		t.Error("esperava banned = false")
	}
	if user.LastIP == nil || *user.LastIP != ip {
		t.Errorf("esperava last_ip %q, obtive %v", ip, user.LastIP)
	}
	if user.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}
}

func TestProfileReflectsUpdate(t *testing.T) {
	ip := newRandomIP()
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), ip)
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	nickname := "nick_" + randHex(4)
	status := "disponível"
	updatedAt := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := storage.UpdateUser(testCtx(), user.ID, models.User{
		Nickname:        &nickname,
		StatusMessage:   &status,
		StatusUpdatedAt: &updatedAt,
	}); err != nil {
		t.Fatalf("falha ao atualizar perfil: %v", err)
	}

	got, err := Profile(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("Profile retornou erro: %v", err)
	}
	if got.Nickname == nil || *got.Nickname != nickname {
		t.Errorf("esperava nickname %q, obtive %v", nickname, got.Nickname)
	}
	if got.StatusMessage == nil || *got.StatusMessage != status {
		t.Errorf("esperava status_message %q, obtive %v", status, got.StatusMessage)
	}
	if got.StatusUpdatedAt == nil || !got.StatusUpdatedAt.Equal(updatedAt) {
		t.Errorf("esperava status_updated_at %v, obtive %v", updatedAt, got.StatusUpdatedAt)
	}
}

func TestProfileEmptyUserID(t *testing.T) {
	_, err := Profile(testCtx(), "")
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para userID vazio, obtive %v", err)
	}
}

func TestProfileNonexistentUser(t *testing.T) {
	_, err := Profile(testCtx(), randUUID())
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para id inexistente, obtive %v", err)
	}
}

func TestProfileIncludesRoles(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	color := "#00FF00"
	role, err := storage.CreateRole(testCtx(), newRandomRoleName(), &color, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), user.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}

	got, err := Profile(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("Profile retornou erro: %v", err)
	}
	if len(got.Roles) != 1 {
		t.Fatalf("esperava 1 role, obtive %d", len(got.Roles))
	}
	if got.Roles[0].ID != role.ID || got.Roles[0].Name != role.Name {
		t.Errorf("role não confere: got %+v", got.Roles[0])
	}
	if got.Roles[0].Color == nil || *got.Roles[0].Color != color {
		t.Errorf("esperava cor %q, obtive %v", color, got.Roles[0].Color)
	}

	// usuário sem roles retorna roles vazia (não nil)
	other, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	gotOther, err := Profile(testCtx(), other.ID)
	if err != nil {
		t.Fatalf("Profile retornou erro: %v", err)
	}
	if gotOther.Roles == nil || len(gotOther.Roles) != 0 {
		t.Errorf("esperava roles vazia (não nil), obtive %v", gotOther.Roles)
	}
}

// --- ProfilesBatch ---

func TestProfilesBatch(t *testing.T) {
	u1, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	u2, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	// a ordem da requisição é preservada e ids inexistentes são pulados
	profiles, err := ProfilesBatch(testCtx(), []string{u2.ID, randUUID(), u1.ID})
	if err != nil {
		t.Fatalf("ProfilesBatch retornou erro: %v", err)
	}
	if len(profiles) != 2 {
		t.Fatalf("esperava 2 perfis, obtive %d", len(profiles))
	}
	if profiles[0].ID != u2.ID || profiles[0].Username != u2.Username {
		t.Errorf("primeiro perfil não confere (ordem da requisição): got %+v", profiles[0])
	}
	if profiles[1].ID != u1.ID || profiles[1].Username != u1.Username {
		t.Errorf("segundo perfil não confere (ordem da requisição): got %+v", profiles[1])
	}
	for _, p := range profiles {
		if p.PasswordHash != "" {
			t.Errorf("ProfilesBatch não deve retornar password_hash, obtive %q", p.PasswordHash)
		}
	}
}

func TestProfilesBatchIncludesRoles(t *testing.T) {
	u1, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	u2, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	color := "#0000FF"
	role, err := storage.CreateRole(testCtx(), newRandomRoleName(), &color, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), u1.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}

	profiles, err := ProfilesBatch(testCtx(), []string{u1.ID, u2.ID})
	if err != nil {
		t.Fatalf("ProfilesBatch retornou erro: %v", err)
	}
	if len(profiles) != 2 {
		t.Fatalf("esperava 2 perfis, obtive %d", len(profiles))
	}
	if len(profiles[0].Roles) != 1 || profiles[0].Roles[0].ID != role.ID {
		t.Errorf("primeiro perfil deveria ter a role atribuída: got %+v", profiles[0].Roles)
	}
	if profiles[0].Roles[0].Color == nil || *profiles[0].Roles[0].Color != color {
		t.Errorf("esperava cor %q, obtive %v", color, profiles[0].Roles[0].Color)
	}
	if profiles[1].Roles == nil || len(profiles[1].Roles) != 0 {
		t.Errorf("segundo perfil deveria ter roles vazia (não nil), obtive %v", profiles[1].Roles)
	}
}

func TestProfilesBatchDeduplicates(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	profiles, err := ProfilesBatch(testCtx(), []string{user.ID, user.ID, user.ID})
	if err != nil {
		t.Fatalf("ProfilesBatch retornou erro: %v", err)
	}
	if len(profiles) != 1 {
		t.Fatalf("esperava 1 perfil (ids duplicados), obtive %d", len(profiles))
	}
	if profiles[0].ID != user.ID {
		t.Errorf("esperava id %s, obtive %s", user.ID, profiles[0].ID)
	}
}

func TestProfilesBatchEmptyIDs(t *testing.T) {
	if _, err := ProfilesBatch(testCtx(), nil); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para ids nil, obtive %v", err)
	}
	if _, err := ProfilesBatch(testCtx(), []string{}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para ids vazios, obtive %v", err)
	}
}

func TestProfilesBatchTooManyIDs(t *testing.T) {
	ids := make([]string, 0, profileBatchLimit+1)
	for i := 0; i < profileBatchLimit+1; i++ {
		ids = append(ids, randUUID())
	}
	if _, err := ProfilesBatch(testCtx(), ids); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para %d ids, obtive %v", len(ids), err)
	}
}

func TestProfilesBatchMaxIDSAccepted(t *testing.T) {
	ids := make([]string, 0, profileBatchLimit)
	for i := 0; i < profileBatchLimit; i++ {
		ids = append(ids, randUUID())
	}
	profiles, err := ProfilesBatch(testCtx(), ids)
	if err != nil {
		t.Fatalf("ProfilesBatch com %d ids retornou erro: %v", profileBatchLimit, err)
	}
	if len(profiles) != 0 {
		t.Errorf("esperava 0 perfis para ids inexistentes, obtive %d", len(profiles))
	}
}

func TestProfilesBatchResolvesAvatar(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	avatar := base64.StdEncoding.EncodeToString(pngAvatarBytes(64, 64))
	if err := UpdateAvatar(testCtx(), user.ID, avatar, "PNG"); err != nil {
		t.Fatalf("UpdateAvatar retornou erro: %v", err)
	}

	profiles, err := ProfilesBatch(testCtx(), []string{user.ID})
	if err != nil {
		t.Fatalf("ProfilesBatch retornou erro: %v", err)
	}
	if len(profiles) != 1 {
		t.Fatalf("esperava 1 perfil, obtive %d", len(profiles))
	}
	if !bytes.Equal(profiles[0].AvatarBlob, pngAvatarBytes(64, 64)) {
		t.Error("avatar_blob não confere")
	}
	if profiles[0].AvatarFormat != "PNG" {
		t.Errorf("esperava avatar_format PNG, obtive %q", profiles[0].AvatarFormat)
	}
}

// --- UpdateUser ---

func TestUpdateUser(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	nickname := "nick_" + randHex(4)
	status := "disponível"
	description := "sobre mim"

	if err := UpdateUser(testCtx(), user.ID, nickname, status, description); err != nil {
		t.Fatalf("UpdateUser retornou erro: %v", err)
	}

	stored, err := storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if stored.Nickname == nil || *stored.Nickname != nickname {
		t.Errorf("esperava nickname %q, obtive %v", nickname, stored.Nickname)
	}
	if stored.StatusMessage == nil || *stored.StatusMessage != status {
		t.Errorf("esperava status_message %q, obtive %v", status, stored.StatusMessage)
	}
	if stored.Description == nil || *stored.Description != description {
		t.Errorf("esperava description %q, obtive %v", description, stored.Description)
	}
	if stored.StatusUpdatedAt == nil {
		t.Error("esperava status_updated_at preenchido")
	}

	// uma segunda atualização substitui os valores anteriores
	updatedNickname := "nick_" + randHex(4)
	updatedStatus := "ausente"
	updatedDescription := "sobre mim v2"
	if err := UpdateUser(testCtx(), user.ID, updatedNickname, updatedStatus, updatedDescription); err != nil {
		t.Fatalf("UpdateUser (segunda atualização) retornou erro: %v", err)
	}
	stored, err = storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if stored.Nickname == nil || *stored.Nickname != updatedNickname {
		t.Errorf("esperava nickname %q, obtive %v", updatedNickname, stored.Nickname)
	}
	if stored.StatusMessage == nil || *stored.StatusMessage != updatedStatus {
		t.Errorf("esperava status_message %q, obtive %v", updatedStatus, stored.StatusMessage)
	}
	if stored.Description == nil || *stored.Description != updatedDescription {
		t.Errorf("esperava description %q, obtive %v", updatedDescription, stored.Description)
	}

	// description vazia limpa o valor
	if err := UpdateUser(testCtx(), user.ID, updatedNickname, updatedStatus, ""); err != nil {
		t.Fatalf("UpdateUser (description vazia) retornou erro: %v", err)
	}
	stored, err = storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if stored.Description == nil || *stored.Description != "" {
		t.Errorf("esperava description vazia, obtive %v", stored.Description)
	}
}

func TestUpdateUserEmptyUserID(t *testing.T) {
	err := UpdateUser(testCtx(), "", "nick", "status", "desc")
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para userID vazio, obtive %v", err)
	}
}

func TestUpdateUserNonexistentUser(t *testing.T) {
	err := UpdateUser(testCtx(), randUUID(), "nick", "status", "desc")
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para id inexistente, obtive %v", err)
	}
}

// --- UpdateAvatar ---

// pngAvatarBytes gera um PNG válido de verdade, nas dimensões dadas,
// pra testar validação de tamanho/dimensão.
func pngAvatarBytes(width, height int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, width, height))

	// opcional: preenche com uma cor sólida, só pra não ser um bitmap vazio
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err) // em teste, panic é aceitável — indica bug no próprio helper
	}
	return buf.Bytes()
}

func jpegAvatarBytes(width, height int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil { // nil = usa opções default
		panic(err)
	}
	return buf.Bytes()
}

func gifAvatarBytes(width, height int) []byte {
	palette := []color.Color{color.RGBA{R: 255, A: 255}, color.Black}
	img := image.NewPaletted(image.Rect(0, 0, width, height), palette)

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetColorIndex(x, y, 0) // índice 0 da paleta acima
		}
	}

	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// webpAvatarBytes gera um WebP válido nas dimensões dadas (output do
// GenerateThumbnail — sem depender de encoder externo).
func webpAvatarBytes(width, height int) []byte {
	webp, _, _, _, err := utils.GenerateThumbnail(pngAvatarBytes(width, height), 512, 0)
	if err != nil {
		panic(err)
	}
	return webp
}

func TestUpdateAvatar(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	cases := []struct {
		name   string
		format string
		expect string
		avatar []byte
	}{
		{"PNG", "PNG", "PNG", pngAvatarBytes(100, 100)},
		{"JPEG", "JPEG", "JPEG", jpegAvatarBytes(100, 100)},
		{"JPG (alias de JPEG)", "JPG", "JPEG", jpegAvatarBytes(100, 100)},
		{"GIF", "GIF", "GIF", gifAvatarBytes(100, 100)},
		{"WEBP", "WEBP", "WEBP", webpAvatarBytes(100, 100)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			avatar := base64.StdEncoding.EncodeToString(tc.avatar)
			if err := UpdateAvatar(testCtx(), user.ID, avatar, tc.format); err != nil {
				t.Fatalf("UpdateAvatar retornou erro: %v", err)
			}

			stored, err := storage.GetUserByID(testCtx(), user.ID)
			if err != nil {
				t.Fatalf("GetUserByID retornou erro: %v", err)
			}
			// o banco guarda apenas a referência content-addressable
			if stored.AvatarMedia == nil {
				t.Fatal("esperava avatar_media definido no banco")
			}
			if _, err := os.Stat(mediaBlobPath(*stored.AvatarMedia)); err != nil {
				t.Errorf("blob do avatar não encontrado no storage: %v", err)
			}

			// o service resolve blob e formato a partir da referência
			profile, err := Profile(testCtx(), user.ID)
			if err != nil {
				t.Fatalf("Profile retornou erro: %v", err)
			}
			if profile.AvatarFormat != tc.expect {
				t.Errorf("esperava avatar_format %q, obtive %q", tc.expect, profile.AvatarFormat)
			}
			if !bytes.Equal(profile.AvatarBlob, tc.avatar) {
				t.Errorf("avatar_blob não confere:\n got  %x\n want %x", profile.AvatarBlob, tc.avatar)
			}
		})
	}
}

func TestUpdateAvatarRemovesWhenEmpty(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	// define um avatar inicialmente
	avatar := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	if err := UpdateAvatar(testCtx(), user.ID, avatar, "PNG"); err != nil {
		t.Fatalf("falha ao definir avatar inicial: %v", err)
	}

	// avatar e formato vazios devem remover o avatar
	if err := UpdateAvatar(testCtx(), user.ID, "", ""); err != nil {
		t.Fatalf("UpdateAvatar (remoção) retornou erro: %v", err)
	}

	stored, err := storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if stored.AvatarMedia != nil {
		t.Errorf("esperava avatar_media nulo, obtive %q", *stored.AvatarMedia)
	}

	profile, err := Profile(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("Profile retornou erro: %v", err)
	}
	if len(profile.AvatarBlob) != 0 {
		t.Errorf("esperava avatar_blob vazio, obtive %x", profile.AvatarBlob)
	}
	if profile.AvatarFormat != "" {
		t.Errorf("esperava avatar_format vazio, obtive %q", profile.AvatarFormat)
	}
}

func TestUpdateAvatarInvalidInput(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	cases := []struct {
		name         string
		avatar       string
		avatarFormat string
	}{
		{"base64 inválido", "!!!nao-e-base64!!!", "PNG"},
		{"formato não aceito", base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100)), "BMP"},
		{"formato vazio com avatar", base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100)), ""},
		{"conteúdo não corresponde ao formato", base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100)), "GIF"},
		{"avatar vazio com formato", "", "PNG"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := UpdateAvatar(testCtx(), user.ID, tc.avatar, tc.avatarFormat)
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("esperava ErrInvalidInput, obtive %v", err)
			}
		})
	}
}

func TestUpdateAvatarExceedsMaxSize(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	// PNG válido com tamanho acima do limite de 2MB
	oversized := make([]byte, 2<<20+1)
	copy(oversized, pngAvatarBytes(100, 100))
	avatar := base64.StdEncoding.EncodeToString(oversized)

	err = UpdateAvatar(testCtx(), user.ID, avatar, "PNG")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para avatar acima de 2MB, obtive %v", err)
	}
}

func TestUpdateAvatarEmptyUserID(t *testing.T) {
	avatar := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	err := UpdateAvatar(testCtx(), "", avatar, "PNG")
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para userID vazio, obtive %v", err)
	}
}

func TestUpdateAvatarNonexistentUser(t *testing.T) {
	avatar := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	err := UpdateAvatar(testCtx(), randUUID(), avatar, "PNG")
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para id inexistente, obtive %v", err)
	}
}

// --- UpdateBanner ---

func TestUpdateBanner(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	cases := []struct {
		name   string
		format string
		banner []byte
	}{
		{"PNG", "PNG", pngAvatarBytes(100, 100)},
		{"JPEG", "JPEG", jpegAvatarBytes(100, 100)},
		{"GIF", "GIF", gifAvatarBytes(100, 100)},
		{"WEBP", "WEBP", webpAvatarBytes(100, 100)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			banner := base64.StdEncoding.EncodeToString(tc.banner)
			if err := UpdateBanner(testCtx(), user.ID, banner, tc.format); err != nil {
				t.Fatalf("UpdateBanner retornou erro: %v", err)
			}

			stored, err := storage.GetUserByID(testCtx(), user.ID)
			if err != nil {
				t.Fatalf("GetUserByID retornou erro: %v", err)
			}
			// o banco guarda apenas a referência content-addressable
			if stored.BannerMedia == nil {
				t.Fatal("esperava banner_media definido no banco")
			}
			if _, err := os.Stat(mediaBlobPath(*stored.BannerMedia)); err != nil {
				t.Errorf("blob do banner não encontrado no storage: %v", err)
			}

			// o profile retorna apenas a referência (sem blob)
			profile, err := Profile(testCtx(), user.ID)
			if err != nil {
				t.Fatalf("Profile retornou erro: %v", err)
			}
			if profile.BannerMedia == nil || *profile.BannerMedia != *stored.BannerMedia {
				t.Errorf("esperava banner_media %v no profile, obtive %v", *stored.BannerMedia, profile.BannerMedia)
			}
		})
	}
}

func TestUpdateBannerAllows1024px(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	// 1024px é aceito para banner (limite do avatar é 512px)
	banner := base64.StdEncoding.EncodeToString(pngAvatarBytes(1024, 100))
	if err := UpdateBanner(testCtx(), user.ID, banner, "PNG"); err != nil {
		t.Errorf("esperava banner de 1024px aceito, obtive %v", err)
	}

	// 1025px é rejeitado
	banner = base64.StdEncoding.EncodeToString(pngAvatarBytes(2049, 100))
	if err := UpdateBanner(testCtx(), user.ID, banner, "PNG"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para banner de 2049px, obtive %v", err)
	}
}

func TestUpdateBannerRemovesWhenEmpty(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	// define um banner inicialmente
	banner := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	if err := UpdateBanner(testCtx(), user.ID, banner, "PNG"); err != nil {
		t.Fatalf("falha ao definir banner inicial: %v", err)
	}

	// banner e formato vazios devem remover o banner
	if err := UpdateBanner(testCtx(), user.ID, "", ""); err != nil {
		t.Fatalf("UpdateBanner (remoção) retornou erro: %v", err)
	}

	stored, err := storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if stored.BannerMedia != nil {
		t.Errorf("esperava banner_media nulo, obtive %q", *stored.BannerMedia)
	}
}

func TestUpdateBannerInvalidInput(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	cases := []struct {
		name         string
		banner       string
		bannerFormat string
	}{
		{"base64 inválido", "!!!nao-e-base64!!!", "PNG"},
		{"formato não aceito", base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100)), "BMP"},
		{"formato vazio com banner", base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100)), ""},
		{"conteúdo não corresponde ao formato", base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100)), "GIF"},
		{"banner vazio com formato", "", "PNG"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := UpdateBanner(testCtx(), user.ID, tc.banner, tc.bannerFormat)
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("esperava ErrInvalidInput, obtive %v", err)
			}
		})
	}
}

func TestUpdateBannerExceedsMaxSize(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	// PNG válido com tamanho acima do limite de 2MB
	oversized := make([]byte, 2<<20+1)
	copy(oversized, pngAvatarBytes(100, 100))
	banner := base64.StdEncoding.EncodeToString(oversized)

	err = UpdateBanner(testCtx(), user.ID, banner, "PNG")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para banner acima de 2MB, obtive %v", err)
	}
}

func TestUpdateBannerEmptyUserID(t *testing.T) {
	banner := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	err := UpdateBanner(testCtx(), "", banner, "PNG")
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para userID vazio, obtive %v", err)
	}
}

func TestUpdateBannerNonexistentUser(t *testing.T) {
	banner := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	err := UpdateBanner(testCtx(), randUUID(), banner, "PNG")
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para id inexistente, obtive %v", err)
	}
}

func TestProfileResolvesAvatar(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	// sem avatar: Profile não resolve blob
	profile, err := Profile(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("Profile retornou erro: %v", err)
	}
	if profile.AvatarBlob != nil || profile.AvatarFormat != "" {
		t.Errorf("esperava avatar vazio, obtive format=%q blob=%x", profile.AvatarFormat, profile.AvatarBlob)
	}

	// com avatar: Profile resolve blob e formato a partir do storage
	avatar := base64.StdEncoding.EncodeToString(pngAvatarBytes(64, 64))
	if err := UpdateAvatar(testCtx(), user.ID, avatar, "PNG"); err != nil {
		t.Fatalf("UpdateAvatar retornou erro: %v", err)
	}
	profile, err = Profile(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("Profile retornou erro: %v", err)
	}
	if !bytes.Equal(profile.AvatarBlob, pngAvatarBytes(64, 64)) {
		t.Error("avatar_blob não confere")
	}
	if profile.AvatarFormat != "PNG" {
		t.Errorf("esperava avatar_format PNG, obtive %q", profile.AvatarFormat)
	}
}

// --- UpdateStatus ---

func TestUpdateStatus(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	t.Run("persiste away", func(t *testing.T) {
		status := "away"
		if err := UpdateStatus(testCtx(), user.ID, &status); err != nil {
			t.Fatalf("UpdateStatus retornou erro: %v", err)
		}
		stored, err := storage.GetUserByID(testCtx(), user.ID)
		if err != nil {
			t.Fatalf("GetUserByID retornou erro: %v", err)
		}
		if stored.Status == nil || *stored.Status != "away" {
			t.Errorf("esperava status away, obtive %v", stored.Status)
		}
	})

	t.Run("persiste busy", func(t *testing.T) {
		status := "busy"
		if err := UpdateStatus(testCtx(), user.ID, &status); err != nil {
			t.Fatalf("UpdateStatus retornou erro: %v", err)
		}
		stored, err := storage.GetUserByID(testCtx(), user.ID)
		if err != nil {
			t.Fatalf("GetUserByID retornou erro: %v", err)
		}
		if stored.Status == nil || *stored.Status != "busy" {
			t.Errorf("esperava status busy, obtive %v", stored.Status)
		}
	})

	t.Run("nil limpa o status", func(t *testing.T) {
		if err := UpdateStatus(testCtx(), user.ID, nil); err != nil {
			t.Fatalf("UpdateStatus retornou erro: %v", err)
		}
		stored, err := storage.GetUserByID(testCtx(), user.ID)
		if err != nil {
			t.Fatalf("GetUserByID retornou erro: %v", err)
		}
		if stored.Status != nil {
			t.Errorf("esperava status nulo, obtive %q", *stored.Status)
		}
	})

	t.Run("valor inválido é rejeitado", func(t *testing.T) {
		for _, invalid := range []string{"online", "idle", "AWAY", ""} {
			status := invalid
			if err := UpdateStatus(testCtx(), user.ID, &status); !errors.Is(err, ErrInvalidInput) {
				t.Errorf("esperava ErrInvalidInput para %q, obtive %v", invalid, err)
			}
		}
	})

	t.Run("userID vazio vira ErrUserNotFound", func(t *testing.T) {
		if err := UpdateStatus(testCtx(), "", nil); !errors.Is(err, ErrUserNotFound) {
			t.Errorf("esperava ErrUserNotFound, obtive %v", err)
		}
	})

	t.Run("usuário inexistente vira ErrUserNotFound", func(t *testing.T) {
		status := "away"
		if err := UpdateStatus(testCtx(), randUUID(), &status); !errors.Is(err, ErrUserNotFound) {
			t.Errorf("esperava ErrUserNotFound, obtive %v", err)
		}
	})
}

// --- Mídia (content-addressable) ---

func TestStoreMediaFromBytes(t *testing.T) {
	cfg := config.LoadConfig()

	mac := hmac.New(sha256.New, []byte(cfg.HMACSecret))

	content := []byte("conteúdo de teste da mídia")
	mac.Write(content)

	expectedHash := hex.EncodeToString(mac.Sum(nil))

	hash, media, err := StoreMediaFromBytes(testCtx(), content, "text/plain")
	if err != nil {
		t.Fatalf("StoreMediaFromBytes retornou erro: %v", err)
	}
	if hash != expectedHash {
		t.Errorf("esperava hash %s, obtive %s", expectedHash, hash)
	}
	if media.ShaHash != expectedHash || media.MimeType != "text/plain" || media.SizeBytes != int64(len(content)) {
		t.Errorf("media não confere: %+v", media)
	}
	if _, err := os.Stat(mediaBlobPath(hash)); err != nil {
		t.Errorf("arquivo da mídia não encontrado em disco: %v", err)
	}

	// deduplicação: mesmo conteúdo não gera nova linha
	_, media2, err := StoreMediaFromBytes(testCtx(), content, "text/plain")
	if err != nil {
		t.Fatalf("StoreMediaFromBytes (dedup) retornou erro: %v", err)
	}
	if media2.ShaHash != expectedHash {
		t.Errorf("esperava o mesmo hash na dedup, obtive %s", media2.ShaHash)
	}

	// conteúdo diferente gera hash diferente
	other, _, err := StoreMediaFromBytes(testCtx(), []byte("outro conteúdo"), "text/plain")
	if err != nil {
		t.Fatalf("StoreMediaFromBytes (outro) retornou erro: %v", err)
	}
	if other == expectedHash {
		t.Error("conteúdos diferentes deveriam gerar hashes diferentes")
	}
}

// TestStoreMediaFromBytesConcurrent garante que gravações concorrentes do
// mesmo conteúdo produzem uma única row e um único arquivo completo (escrita
// atômica: sem blob parcial visível e sem temporário .media-* residual).
func TestStoreMediaFromBytesConcurrent(t *testing.T) {
	withTempMediaDir(t)

	cfg := config.LoadConfig()
	content := []byte("conteúdo concorrente da mídia")
	mac := hmac.New(sha256.New, []byte(cfg.HMACSecret))
	mac.Write(content)
	expectedHash := hex.EncodeToString(mac.Sum(nil))

	const workers = 8
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := StoreMediaFromBytes(testCtx(), content, "text/plain"); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("StoreMediaFromBytes concorrente retornou erro: %v", err)
	}

	var count int
	if err := storage.GetDB().QueryRowContext(testCtx(),
		"SELECT count(*) FROM media WHERE sha_hash = $1", expectedHash).Scan(&count); err != nil {
		t.Fatalf("falha ao contar rows de mídia: %v", err)
	}
	if count != 1 {
		t.Errorf("esperava 1 row para o conteúdo, obtive %d", count)
	}

	got, err := MediaContent(expectedHash)
	if err != nil {
		t.Fatalf("MediaContent retornou erro: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("conteúdo do blob não confere:\n got  %q\n want %q", got, content)
	}

	// Nenhum temporário .media-* residual na pasta de mídia.
	if err := filepath.WalkDir(mediaBaseDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if strings.HasPrefix(d.Name(), ".media-") {
			t.Errorf("arquivo temporário residual encontrado: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatalf("falha ao varrer a pasta de mídia: %v", err)
	}
}

func TestMediaContent(t *testing.T) {
	content := []byte("conteúdo lido de volta")
	hash, _, err := StoreMediaFromBytes(testCtx(), content, "text/plain")
	if err != nil {
		t.Fatalf("StoreMediaFromBytes retornou erro: %v", err)
	}

	got, err := MediaContent(hash)
	if err != nil {
		t.Fatalf("MediaContent retornou erro: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("conteúdo não confere:\n got  %q\n want %q", got, content)
	}

	if _, err := MediaContent(strings.Repeat("0", 64)); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para hash inexistente, obtive %v", err)
	}
}

// --- UpdateSettings ---

func TestUpdateSettings(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

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

	settings, err := UpdateSettings(testCtx(), user.ID, config)
	if err != nil {
		t.Fatalf("UpdateSettings retornou erro: %v", err)
	}
	if settings.UserID != user.ID {
		t.Errorf("esperava user_id %s, obtive %s", user.ID, settings.UserID)
	}
	if settings.Version != models.CurrentVersion {
		t.Errorf("esperava version %d, obtive %d", models.CurrentVersion, settings.Version)
	}
	if settings.Config != config {
		t.Errorf("config não confere:\n got  %+v\n want %+v", settings.Config, config)
	}
	if settings.UpdatedAt.IsZero() {
		t.Error("esperava updated_at preenchido")
	}

	// o config deve ter sido persistido
	stored, err := storage.GetUserSettings(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserSettings retornou erro: %v", err)
	}
	if stored.Config != config {
		t.Errorf("config persistido não confere:\n got  %+v\n want %+v", stored.Config, config)
	}

	// uma segunda atualização substitui o config anterior
	updated := config
	updated.Theme = "light"
	updated.Display.ShowAvatars = true

	again, err := UpdateSettings(testCtx(), user.ID, updated)
	if err != nil {
		t.Fatalf("UpdateSettings (segunda atualização) retornou erro: %v", err)
	}
	if again.Config != updated {
		t.Errorf("config atualizado não confere:\n got  %+v\n want %+v", again.Config, updated)
	}
	stored, err = storage.GetUserSettings(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserSettings retornou erro: %v", err)
	}
	if stored.Config != updated {
		t.Errorf("config persistido após atualização não confere:\n got  %+v\n want %+v", stored.Config, updated)
	}
}

func TestUpdateSettingsInvalidConfig(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	valid := models.UserConfig{
		Theme: "dark",
		Display: models.Display{
			FontSize:       "medium",
			MessageDensity: "normal",
		},
	}

	cases := []struct {
		name   string
		config models.UserConfig
	}{
		{"theme inválido", func() models.UserConfig { c := valid; c.Theme = "blue"; return c }()},
		{"fontSize inválido", func() models.UserConfig { c := valid; c.Display.FontSize = "large"; return c }()},
		{"messageDensity inválido", func() models.UserConfig { c := valid; c.Display.MessageDensity = "wide"; return c }()},
		{"theme e fontSize inválidos", func() models.UserConfig { c := valid; c.Theme = ""; c.Display.FontSize = "xlarge"; return c }()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := UpdateSettings(testCtx(), user.ID, tc.config)
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("esperava ErrInvalidInput, obtive %v", err)
			}
		})
	}

	// configurações inválidas não devem alterar o config persistido
	if _, err := UpdateSettings(testCtx(), user.ID, valid); err != nil {
		t.Fatalf("falha ao salvar config válido: %v", err)
	}
	_, err = UpdateSettings(testCtx(), user.ID, cases[0].config)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("esperava ErrInvalidInput, obtive %v", err)
	}
	stored, err := storage.GetUserSettings(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserSettings retornou erro: %v", err)
	}
	if stored.Config != valid {
		t.Errorf("config persistido mudou após tentativa inválida:\n got  %+v\n want %+v", stored.Config, valid)
	}
}

func TestUpdateSettingsEmptyUserID(t *testing.T) {
	config := models.UserConfig{
		Theme: "dark",
		Display: models.Display{
			FontSize:       "medium",
			MessageDensity: "normal",
		},
	}
	_, err := UpdateSettings(testCtx(), "", config)
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para userID vazio, obtive %v", err)
	}
}

func TestUpdateSettingsNonexistentUser(t *testing.T) {
	config := models.UserConfig{
		Theme: "dark",
		Display: models.Display{
			FontSize:       "medium",
			MessageDensity: "normal",
		},
	}
	_, err := UpdateSettings(testCtx(), randUUID(), config)
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para id inexistente, obtive %v", err)
	}
}

// --- BanUser ---

func TestBanUser(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	if err := BanUser(testCtx(), testActorID(), user.ID, true); err != nil {
		t.Fatalf("BanUser(true) retornou erro: %v", err)
	}
	stored, err := storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if !stored.Banned {
		t.Error("esperava banned = true")
	}

	if err := BanUser(testCtx(), testActorID(), user.ID, false); err != nil {
		t.Fatalf("BanUser(false) retornou erro: %v", err)
	}
	stored, err = storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if stored.Banned {
		t.Error("esperava banned = false após desbanir")
	}
}

// O dono do servidor NÃO PODE ser banível
func TestBanServerOwner(t *testing.T) {
	cleanServers(testCtx())

	owner, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário dono: %v", err)
	}

	name := newRandomServerName()
	icon := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))

	_, err = CreateServerWithIcon(testCtx(), name, icon, "png", true, nil, &owner.ID)

	if err != nil {
		t.Fatalf("CreateServerWithIcon retornou erro: %v", err)
	}
	err = BanUser(testCtx(), testActorID(), owner.ID, true)

	if !errors.Is(err, ErrServerOwner) {
		t.Errorf("esperava ErrServerOwner para tentar banir dono do servidor, obtive %v", err)
	}

}

func TestBanUserEmptyUserID(t *testing.T) {
	if err := BanUser(testCtx(), testActorID(), "", true); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para id vazio, obtive %v", err)
	}
}

func TestBanUserNonexistentUser(t *testing.T) {
	if err := BanUser(testCtx(), testActorID(), randUUID(), true); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para id inexistente, obtive %v", err)
	}
}

// --- ResetUserPassword ---

func TestResetUserPassword(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	if err := ResetUserPassword(testCtx(), testActorID(), user.ID); err != nil {
		t.Fatalf("ResetUserPassword retornou erro: %v", err)
	}
	stored, err := storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if !stored.ResetPassword {
		t.Error("esperava reset_password = true")
	}
}

func TestResetUserPasswordEmptyUserID(t *testing.T) {
	if err := ResetUserPassword(testCtx(), testActorID(), ""); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para id vazio, obtive %v", err)
	}
}

func TestResetUserPasswordNonexistentUser(t *testing.T) {
	if err := ResetUserPassword(testCtx(), testActorID(), randUUID()); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para id inexistente, obtive %v", err)
	}
}

// --- ListUsers ---

func TestListUsers(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	users, err := ListUsers(testCtx(), nil, "")
	if err != nil {
		t.Fatalf("ListUsers retornou erro: %v", err)
	}

	byID := make(map[string]models.UserSummary, len(users.Users))
	for _, u := range users.Users {
		byID[u.ID] = u
	}
	got, ok := byID[user.ID]
	if !ok {
		t.Fatalf("usuário %s não aparece na listagem", user.ID)
	}
	if got.Username != user.Username {
		t.Errorf("esperava username %q, obtive %q", user.Username, got.Username)
	}
	if got.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}
}

func TestListUsersIncludesRoles(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	color := "#123456"
	role, err := storage.CreateRole(testCtx(), newRandomRoleName(), &color, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), user.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}

	users, err := ListUsers(testCtx(), nil, "")
	if err != nil {
		t.Fatalf("ListUsers retornou erro: %v", err)
	}
	byID := make(map[string]models.UserSummary, len(users.Users))
	for _, u := range users.Users {
		byID[u.ID] = u
	}
	got, ok := byID[user.ID]
	if !ok {
		t.Fatalf("usuário %s não aparece na listagem", user.ID)
	}
	if len(got.Roles) != 1 || got.Roles[0].ID != role.ID || got.Roles[0].Name != role.Name {
		t.Errorf("listagem não expôs a role atribuída: got %+v", got.Roles)
	}
	if got.Roles[0].Color == nil || *got.Roles[0].Color != color {
		t.Errorf("esperava cor %q, obtive %v", color, got.Roles[0].Color)
	}
}

// --- ChangePassword ---

func TestChangePassword(t *testing.T) {
	password := newRandomPassword()
	user, err := Register(testCtx(), newRandomUsername(), password, newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	if err := ResetUserPassword(testCtx(), testActorID(), user.ID); err != nil {
		t.Fatalf("ResetUserPassword retornou erro: %v", err)
	}

	newPassword := newRandomPassword()
	if err := ChangePassword(testCtx(), user.ID, newPassword); err != nil {
		t.Fatalf("ChangePassword retornou erro: %v", err)
	}

	stored, err := storage.GetUserByUsername(testCtx(), user.Username)
	if err != nil {
		t.Fatalf("GetUserByUsername retornou erro: %v", err)
	}
	if err := utils.CheckPassword(newPassword, stored.PasswordHash); err != nil {
		t.Errorf("CheckPassword falhou para o novo hash persistido: %v", err)
	}
	if err := utils.CheckPassword(password, stored.PasswordHash); err == nil {
		t.Error("a senha antiga deveria deixar de validar após a troca")
	}
	if stored.ResetPassword {
		t.Error("esperava reset_password = false após trocar a senha")
	}
}

func TestChangePasswordBoundaryLengths(t *testing.T) {
	MaxPasswordLength, _ := getMaxLenFields()

	password := newRandomPassword()
	user, err := Register(testCtx(), newRandomUsername(), password, newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	err = storage.SetUserResetPassword(testCtx(), user.ID)

	if err != nil {
		t.Fatalf("SetUserResetPassword retornou erro: %v", err)
	}

	boundaryPassword := strings.Repeat("b", MaxPasswordLength)

	err = storage.SetUserResetPassword(testCtx(), user.ID)

	if err != nil {
		t.Fatalf("SetUserResetPassword retornou erro: %v", err)
	}

	if err := ChangePassword(testCtx(), user.ID, boundaryPassword); err != nil {
		t.Fatalf("ChangePassword com senha no limite retornou erro: %v", err)
	}

	stored, err := storage.GetUserByUsername(testCtx(), user.Username)
	if err != nil {
		t.Fatalf("GetUserByUsername retornou erro: %v", err)
	}
	if err := utils.CheckPassword(boundaryPassword, stored.PasswordHash); err != nil {
		t.Errorf("CheckPassword falhou para o hash persistido: %v", err)
	}
}

func TestChangePasswordInvalidInput(t *testing.T) {
	MaxPasswordLength, _ := getMaxLenFields()

	password := newRandomPassword()
	user, err := Register(testCtx(), newRandomUsername(), password, newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	err = storage.SetUserResetPassword(testCtx(), user.ID)

	if err != nil {
		t.Fatalf("SetUserResetPassword retornou erro: %v", err)
	}

	if err := ChangePassword(testCtx(), user.ID, ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para senha vazia, obtive %v", err)
	}

	err = storage.SetUserResetPassword(testCtx(), user.ID)

	if err != nil {
		t.Fatalf("SetUserResetPassword retornou erro: %v", err)
	}

	longPassword := strings.Repeat("a", MaxPasswordLength+1)
	if err := ChangePassword(testCtx(), user.ID, longPassword); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para senha acima do limite, obtive %v", err)
	}

	stored, err := storage.GetUserByUsername(testCtx(), user.Username)
	if err != nil {
		t.Fatalf("GetUserByUsername retornou erro: %v", err)
	}
	if err := utils.CheckPassword(password, stored.PasswordHash); err != nil {
		t.Error("a senha original deveria continuar válida após tentativas inválidas")
	}
}
func TestChangePasswordNoReset(t *testing.T) {

	password := newRandomPassword()
	user, err := Register(testCtx(), newRandomUsername(), password, newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	if err := ChangePassword(testCtx(), user.ID, newRandomPassword()); !errors.Is(err, ErrUserNotReset) {
		t.Errorf("esperava ErrUserNotReset para não reset_password, obtive %v", err)
	}
}

func TestChangePasswordEmptyUserID(t *testing.T) {
	if err := ChangePassword(testCtx(), "", newRandomPassword()); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para id vazio, obtive %v", err)
	}
}

func TestChangePasswordNonexistentUser(t *testing.T) {
	if err := ChangePassword(testCtx(), randUUID(), newRandomPassword()); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para id inexistente, obtive %v", err)
	}
}

// --- CreateServer ---

// newRandomServerName gera um nome de servidor único (a constraint UNIQUE de
// servers.name é global).
func newRandomServerName() string {
	return "srv" + randHex(4)
}

func TestCreateServer(t *testing.T) {
	cleanServers(testCtx())
	owner, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário dono: %v", err)
	}

	name := newRandomServerName()
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
	if server.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}

	// o servidor deve ter sido persistido
	stored, err := storage.GetServer(testCtx())
	if err != nil {
		t.Fatalf("GetServer retornou erro: %v", err)
	}
	if stored.Name != name {
		t.Errorf("esperava name %q, obtive %q", name, stored.Name)
	}
	if stored.OwnerID == nil || *stored.OwnerID != owner.ID {
		t.Errorf("esperava owner_id %s, obtive %v", owner.ID, stored.OwnerID)
	}
}

func TestCreateServerWithoutOwner(t *testing.T) {
	cleanServers(testCtx())
	server, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("CreateServer retornou erro: %v", err)
	}

	if server.OwnerID != nil {
		t.Errorf("esperava owner_id nulo, obtive %v", *server.OwnerID)
	}

	stored, err := storage.GetServer(testCtx())
	if err != nil {
		t.Fatalf("GetServer retornou erro: %v", err)
	}
	if stored.OwnerID != nil {
		t.Errorf("esperava owner_id nulo no banco, obtive %v", *stored.OwnerID)
	}
}

func TestCreateServerEmptyName(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), "", nil)
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para nome vazio, obtive %v", err)
	}
}

func TestCreateServerWithIcon(t *testing.T) {
	owner, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário dono: %v", err)
	}

	name := newRandomServerName()
	icon := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))

	server, err := CreateServerWithIcon(testCtx(), name, icon, "png", true, nil, &owner.ID)
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
	// o ícone é resolvido (blob + formato) a partir da referência media
	if server.IconMedia == nil {
		t.Fatal("esperava icon_media definida no servidor retornado")
	}
	summary, err := GetServer(testCtx())
	if err != nil {
		t.Fatalf("GetServer retornou erro: %v", err)
	}
	// o formato deve ser normalizado para maiúsculas
	if summary.IconFormat != "PNG" {
		t.Errorf("esperava icon_format %q, obtive %q", "PNG", summary.IconFormat)
	}
	if !bytes.Equal(summary.IconBlob, pngAvatarBytes(100, 100)) {
		t.Errorf("icon_blob não confere:\n got  %x\n want %x", summary.IconBlob, pngAvatarBytes(100, 100))
	}
	if server.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}

	// o servidor deve ter sido persistido (referência content-addressable)
	stored, err := storage.GetServer(testCtx())
	if err != nil {
		t.Fatalf("GetServer retornou erro: %v", err)
	}
	if stored.Name != name {
		t.Errorf("esperava name %q, obtive %q", name, stored.Name)
	}
	if stored.IconMedia == nil || *stored.IconMedia != *server.IconMedia {
		t.Errorf("esperava icon_media %v no banco, obtive %v", *server.IconMedia, stored.IconMedia)
	}
	blob, err := os.ReadFile(mediaBlobPath(*stored.IconMedia))
	if err != nil {
		t.Fatalf("blob do ícone não encontrado no storage: %v", err)
	}
	if !bytes.Equal(blob, pngAvatarBytes(100, 100)) {
		t.Errorf("icon_blob persistido não confere: %x", blob)
	}
}

// TestCreateServerWithIconAlreadyExistsNoOrphan garante que uma criação
// repetida com ícone distinto retorna ErrServerAlreadyCreated SEM gravar o
// ícone na mídia (sem blob órfão — o DoS de disco via POST /server).
func TestCreateServerWithIconAlreadyExistsNoOrphan(t *testing.T) {
	withTempMediaDir(t)
	cleanServers(testCtx())
	owner, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário dono: %v", err)
	}

	icon1 := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	if _, err := CreateServerWithIcon(testCtx(), newRandomServerName(), icon1, "png", true, nil, &owner.ID); err != nil {
		t.Fatalf("CreateServerWithIcon (1ª) retornou erro: %v", err)
	}

	// Ícone distinto: o servidor já existe → ErrServerAlreadyCreated antes da
	// gravação em mídia.
	icon2Bytes := pngAvatarBytes(120, 120)
	icon2 := base64.StdEncoding.EncodeToString(icon2Bytes)
	_, err = CreateServerWithIcon(testCtx(), newRandomServerName(), icon2, "png", true, nil, &owner.ID)
	if !errors.Is(err, ErrServerAlreadyCreated) {
		t.Fatalf("esperava ErrServerAlreadyCreated, obtive %v", err)
	}

	// O 2º ícone não pode ter sido gravado (nem row, nem arquivo).
	cfg := config.LoadConfig()
	mac := hmac.New(sha256.New, []byte(cfg.HMACSecret))
	mac.Write(icon2Bytes)
	hash2 := hex.EncodeToString(mac.Sum(nil))
	if mediaRowExists(t, hash2) {
		t.Errorf("ícone de criação repetida não deveria ter row na tabela media")
	}
	if _, statErr := os.Stat(mediaBlobPath(hash2)); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("ícone de criação repetida não deveria ter arquivo em disco (stat err = %v)", statErr)
	}
}

func TestCreateServerWithIconAllFormats(t *testing.T) {
	cases := []struct {
		name   string
		format string
		icon   []byte
	}{
		{"PNG", "PNG", pngAvatarBytes(100, 100)},
		{"JPEG", "JPEG", jpegAvatarBytes(100, 100)},
		{"GIF", "GIF", gifAvatarBytes(100, 100)},
		{"WEBP", "WEBP", webpAvatarBytes(100, 100)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cleanServers(testCtx())
			icon := base64.StdEncoding.EncodeToString(tc.icon)
			_, err := CreateServerWithIcon(testCtx(), newRandomServerName(), icon, tc.format, true, nil, nil)
			if err != nil {
				t.Fatalf("CreateServerWithIcon retornou erro: %v", err)
			}

			stored, err := storage.GetServer(testCtx())
			if err != nil {
				t.Fatalf("GetServer retornou erro: %v", err)
			}
			if stored.IconMedia == nil {
				t.Fatal("esperava icon_media definida no banco")
			}
			summary, err := GetServer(testCtx())
			if err != nil {
				t.Fatalf("GetServer retornou erro: %v", err)
			}
			if summary.IconFormat != tc.format {
				t.Errorf("esperava icon_format %q, obtive %q", tc.format, summary.IconFormat)
			}
			if !bytes.Equal(summary.IconBlob, tc.icon) {
				t.Errorf("icon_blob não confere:\n got  %x\n want %x", summary.IconBlob, tc.icon)
			}
		})
	}
}

func TestCreateServerWithIconInvalidInput(t *testing.T) {
	validIcon := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))

	cases := []struct {
		name       string
		serverName string
		icon       string
		iconFormat string
	}{
		{"nome vazio", "", validIcon, "PNG"},
		{"nome acima de 32 caracteres", strings.Repeat("a", 33), validIcon, "PNG"},
		{"base64 inválido", newRandomServerName(), "!!!nao-e-base64!!!", "PNG"},
		{"formato não aceito", newRandomServerName(), validIcon, "BMP"},
		{"formato vazio com ícone", newRandomServerName(), validIcon, ""},
		{"conteúdo não corresponde ao formato", newRandomServerName(), validIcon, "GIF"},
		{"ícone vazio com formato", newRandomServerName(), "", "PNG"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cleanServers(testCtx())
			_, err := CreateServerWithIcon(testCtx(), tc.serverName, tc.icon, tc.iconFormat, true, nil, nil)
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("esperava ErrInvalidInput, obtive %v", err)
			}
		})
	}
}

func TestCreateServerWithIconBoundaryNameLength(t *testing.T) {
	cleanServers(testCtx())
	// 32 caracteres multibyte (64 bytes) estão dentro do limite
	name := strings.Repeat("ç", 32)
	_, err := CreateServerWithIcon(testCtx(), name, "", "", true, nil, nil)
	if err != nil {
		t.Fatalf("CreateServerWithIcon com nome de 32 caracteres retornou erro: %v", err)
	}

	stored, err := storage.GetServer(testCtx())
	if err != nil {
		t.Fatalf("GetServer retornou erro: %v", err)
	}
	if stored.Name != name {
		t.Errorf("esperava name %q, obtive %q", name, stored.Name)
	}
}

func TestCreateServerWithIconExceedsMaxSize(t *testing.T) {
	cleanServers(testCtx())
	// PNG válido com tamanho acima do limite de 2MB
	oversized := make([]byte, 2<<20+1)
	copy(oversized, pngAvatarBytes(100, 100))
	icon := base64.StdEncoding.EncodeToString(oversized)

	_, err := CreateServerWithIcon(testCtx(), newRandomServerName(), icon, "PNG", true, nil, nil)
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para ícone acima de 2MB, obtive %v", err)
	}
}

func TestCreateServerWithIconBoundarySize(t *testing.T) {
	cleanServers(testCtx())
	// PNG válido com exatamente 2MB deve ser aceito
	exact := make([]byte, 2<<20)
	copy(exact, pngAvatarBytes(100, 100))
	icon := base64.StdEncoding.EncodeToString(exact)

	_, err := CreateServerWithIcon(testCtx(), newRandomServerName(), icon, "PNG", true, nil, nil)
	if err != nil {
		t.Fatalf("CreateServerWithIcon com ícone de exatamente 2MB retornou erro: %v", err)
	}

	stored, err := storage.GetServer(testCtx())
	if err != nil {
		t.Fatalf("GetServer retornou erro: %v", err)
	}
	if stored.IconMedia == nil {
		t.Fatal("esperava icon_media definida no banco")
	}
	blob, err := os.ReadFile(mediaBlobPath(*stored.IconMedia))
	if err != nil {
		t.Fatalf("blob do ícone não encontrado no storage: %v", err)
	}
	if len(blob) != 2<<20 {
		t.Errorf("esperava icon_blob com %d bytes, obtive %d", 2<<20, len(blob))
	}
}

// --- GetServer ---

func TestGetServer(t *testing.T) {
	cleanServers(testCtx())
	owner, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário dono: %v", err)
	}

	server, err := CreateServer(testCtx(), newRandomServerName(), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	// um canal para verificar o channel_count da visão ServerSummary
	if _, err := storage.CreateChannel(testCtx(), "ch_"+randHex(4), "text", ""); err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	// member_count é o total de usuários (storage)
	users, err := ListUsers(testCtx(), nil, "")
	if err != nil {
		t.Fatalf("ListUsers retornou erro: %v", err)
	}
	wantMembers := len(users.Users)

	summary, err := GetServer(testCtx())
	if err != nil {
		t.Fatalf("GetServer retornou erro: %v", err)
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
	if summary.ChannelCount != 1 {
		t.Errorf("esperava channel_count 1, obtive %d", summary.ChannelCount)
	}
	if summary.MemberCount != wantMembers {
		t.Errorf("esperava member_count %d, obtive %d", wantMembers, summary.MemberCount)
	}
	if summary.RoleCount != 0 {
		t.Errorf("esperava role_count 0, obtive %d", summary.RoleCount)
	}
	if len(summary.IconBlob) != 0 {
		t.Errorf("esperava icon_blob vazio, obtive %x", summary.IconBlob)
	}
	if summary.IconFormat != "" {
		t.Errorf("esperava icon_format vazio, obtive %q", summary.IconFormat)
	}
	if summary.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}
}

func TestGetServerNoServer(t *testing.T) {
	cleanServers(testCtx())
	_, err := GetServer(testCtx())
	if !errors.Is(err, ErrServerNotFound) {
		t.Errorf("esperava ErrServerNotFound sem servidor criado, obtive %v", err)
	}
}

// --- UpdateServer ---

func TestUpdateServer(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	newName := newRandomServerName()
	icon := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))

	if err := UpdateServer(testCtx(), testActorID(), newName, icon, "png", nil, nil); err != nil {
		t.Fatalf("UpdateServer retornou erro: %v", err)
	}

	stored, err := storage.GetServer(testCtx())
	if err != nil {
		t.Fatalf("GetServer retornou erro: %v", err)
	}
	if stored.Name != newName {
		t.Errorf("esperava name %q, obtive %q", newName, stored.Name)
	}
	if stored.IconMedia == nil {
		t.Fatal("esperava icon_media definida no banco")
	}
	// o ícone é resolvido (blob + formato) a partir da referência media
	summary, err := GetServer(testCtx())
	if err != nil {
		t.Fatalf("GetServer retornou erro: %v", err)
	}
	// o formato deve ser normalizado para maiúsculas
	if summary.IconFormat != "PNG" {
		t.Errorf("esperava icon_format %q, obtive %q", "PNG", summary.IconFormat)
	}
	if !bytes.Equal(summary.IconBlob, pngAvatarBytes(100, 100)) {
		t.Errorf("icon_blob não confere:\n got  %x\n want %x", summary.IconBlob, pngAvatarBytes(100, 100))
	}
}

func TestUpdateServerAllFormats(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	cases := []struct {
		name   string
		format string
		icon   []byte
	}{
		{"PNG", "PNG", pngAvatarBytes(100, 100)},
		{"JPEG", "JPEG", jpegAvatarBytes(100, 100)},
		{"GIF", "GIF", gifAvatarBytes(100, 100)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			icon := base64.StdEncoding.EncodeToString(tc.icon)
			if err := UpdateServer(testCtx(), testActorID(), newRandomServerName(), icon, tc.format, nil, nil); err != nil {
				t.Fatalf("UpdateServer retornou erro: %v", err)
			}

			stored, err := storage.GetServer(testCtx())
			if err != nil {
				t.Fatalf("GetServer retornou erro: %v", err)
			}
			if stored.IconMedia == nil {
				t.Fatal("esperava icon_media definida no banco")
			}
			summary, err := GetServer(testCtx())
			if err != nil {
				t.Fatalf("GetServer retornou erro: %v", err)
			}
			if summary.IconFormat != tc.format {
				t.Errorf("esperava icon_format %q, obtive %q", tc.format, summary.IconFormat)
			}
			if !bytes.Equal(summary.IconBlob, tc.icon) {
				t.Errorf("icon_blob não confere:\n got  %x\n want %x", summary.IconBlob, tc.icon)
			}
		})
	}
}

func TestUpdateServerRemovesIconWhenEmpty(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	// define um ícone inicialmente
	icon := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	if err := UpdateServer(testCtx(), testActorID(), newRandomServerName(), icon, "PNG", nil, nil); err != nil {
		t.Fatalf("falha ao definir ícone inicial: %v", err)
	}

	// ícone e formato vazios devem remover o ícone
	if err := UpdateServer(testCtx(), testActorID(), newRandomServerName(), "", "", nil, nil); err != nil {
		t.Fatalf("UpdateServer (remoção) retornou erro: %v", err)
	}

	stored, err := storage.GetServer(testCtx())
	if err != nil {
		t.Fatalf("GetServer retornou erro: %v", err)
	}
	if stored.IconMedia != nil {
		t.Errorf("esperava icon_media nula, obtive %q", *stored.IconMedia)
	}
	summary, err := GetServer(testCtx())
	if err != nil {
		t.Fatalf("GetServer retornou erro: %v", err)
	}
	if len(summary.IconBlob) != 0 {
		t.Errorf("esperava icon_blob vazio, obtive %x", summary.IconBlob)
	}
	if summary.IconFormat != "" {
		t.Errorf("esperava icon_format vazio, obtive %q", summary.IconFormat)
	}
}

func TestUpdateServerInvalidInput(t *testing.T) {
	cleanServers(testCtx())
	server, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	validIcon := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))

	cases := []struct {
		name       string
		serverName string
		icon       string
		iconFormat string
	}{
		{"base64 inválido", newRandomServerName(), "!!!nao-e-base64!!!", "PNG"},
		{"formato não aceito", newRandomServerName(), validIcon, "BMP"},
		{"formato vazio com ícone", newRandomServerName(), validIcon, ""},
		{"conteúdo não corresponde ao formato", newRandomServerName(), validIcon, "GIF"},
		{"ícone vazio com formato", newRandomServerName(), "", "PNG"},
		{"nome vazio", "", validIcon, "PNG"},
		{"nome acima de 32 caracteres", strings.Repeat("a", 33), validIcon, "PNG"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := UpdateServer(testCtx(), testActorID(), tc.serverName, tc.icon, tc.iconFormat, nil, nil)
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("esperava ErrInvalidInput, obtive %v", err)
			}
		})
	}

	// tentativas inválidas não devem alterar o servidor
	stored, err := storage.GetServer(testCtx())
	if err != nil {
		t.Fatalf("GetServer retornou erro: %v", err)
	}
	if stored.Name != server.Name {
		t.Errorf("name mudou após tentativa inválida: esperado %q, obtive %q", server.Name, stored.Name)
	}
	if len(stored.IconBlob) != 0 {
		t.Errorf("esperava icon_blob vazio, obtive %x", stored.IconBlob)
	}
	if stored.IconFormat != "" {
		t.Errorf("esperava icon_format vazio, obtive %q", stored.IconFormat)
	}
}

func TestUpdateServerBoundaryNameLength(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	// 32 caracteres multibyte (64 bytes) estão dentro do limite
	name := strings.Repeat("ç", 32)
	if err := UpdateServer(testCtx(), testActorID(), name, "", "", nil, nil); err != nil {
		t.Fatalf("UpdateServer com nome de 32 caracteres retornou erro: %v", err)
	}

	stored, err := storage.GetServer(testCtx())
	if err != nil {
		t.Fatalf("GetServer retornou erro: %v", err)
	}
	if stored.Name != name {
		t.Errorf("esperava name %q, obtive %q", name, stored.Name)
	}
}

func TestUpdateServerExceedsMaxSize(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	// PNG válido com tamanho acima do limite de 2MB
	oversized := make([]byte, 2<<20+1)
	copy(oversized, pngAvatarBytes(100, 100))
	icon := base64.StdEncoding.EncodeToString(oversized)

	err = UpdateServer(testCtx(), testActorID(), newRandomServerName(), icon, "PNG", nil, nil)
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para ícone acima de 2MB, obtive %v", err)
	}
}

func TestUpdateServerBoundarySize(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	// PNG válido com exatamente 2MB deve ser aceito
	exact := make([]byte, 2<<20)
	copy(exact, pngAvatarBytes(100, 100))
	icon := base64.StdEncoding.EncodeToString(exact)

	if err := UpdateServer(testCtx(), testActorID(), newRandomServerName(), icon, "PNG", nil, nil); err != nil {
		t.Fatalf("UpdateServer com ícone de exatamente 2MB retornou erro: %v", err)
	}

	stored, err := storage.GetServer(testCtx())
	if err != nil {
		t.Fatalf("GetServer retornou erro: %v", err)
	}
	if stored.IconMedia == nil {
		t.Fatal("esperava icon_media definida no banco")
	}
	blob, err := os.ReadFile(mediaBlobPath(*stored.IconMedia))
	if err != nil {
		t.Fatalf("blob do ícone não encontrado no storage: %v", err)
	}
	if len(blob) != 2<<20 {
		t.Errorf("esperava icon_blob com %d bytes, obtive %d", 2<<20, len(blob))
	}
}

func TestUpdateServerNoServer(t *testing.T) {
	cleanServers(testCtx())
	icon := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	err := UpdateServer(testCtx(), testActorID(), newRandomServerName(), icon, "PNG", nil, nil)
	if !errors.Is(err, ErrServerNotFound) {
		t.Errorf("esperava ErrServerNotFound sem servidor criado, obtive %v", err)
	}
}

// --- ListChannels ---

// newRandomChannelName gera um nome de canal único (a constraint UNIQUE de
// channels.name é global).
func newRandomChannelName() string {
	return "ch" + randHex(4)
}

// newBoundaryChannelName gera um nome de canal único de exatamente 32
// caracteres (runes) com conteúdo multibyte, para testar o limite de tamanho.
func newBoundaryChannelName() string {
	return randHex(4) + strings.Repeat("ç", 24)
}

// newRandomRoleName gera um nome de role único (constraint UNIQUE (name)).
func newRandomRoleName() string {
	return "role" + randHex(4)
}

func TestListChannels(t *testing.T) {
	cleanServers(testCtx())
	owner, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário dono: %v", err)
	}

	_, err = CreateServer(testCtx(), newRandomServerName(), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	channelA, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal A: %v", err)
	}
	channelB, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal B: %v", err)
	}

	channels, err := ListChannels(testCtx(), owner.ID)
	if err != nil {
		t.Fatalf("ListChannels retornou erro: %v", err)
	}

	byID := make(map[string]models.ChannelSummary, len(channels))
	for _, c := range channels {
		byID[c.ID] = c
	}

	for _, want := range []models.ChannelSummary{channelA, channelB} {
		got, ok := byID[want.ID]
		if !ok {
			t.Fatalf("canal %q não encontrado na listagem", want.Name)
		}
		if got.Name != want.Name {
			t.Errorf("esperava name %q, obtive %q", want.Name, got.Name)
		}
		if got.Type != "text" {
			t.Errorf("esperava type %q, obtive %q", "text", got.Type)
		}
		if len(got.Permissions) != 0 {
			t.Errorf("esperava permissões vazias, obtive %+v", got.Permissions)
		}
		if got.LastMessage != nil {
			t.Errorf("esperava last_message nula, obtive %+v", got.LastMessage)
		}
		if got.CreatedAt.IsZero() {
			t.Error("esperava created_at preenchido")
		}
	}
}

func TestListChannelsLastMessage(t *testing.T) {
	cleanServers(testCtx())
	owner, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário dono: %v", err)
	}
	author, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário autor: %v", err)
	}

	_, err = CreateServer(testCtx(), newRandomServerName(), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	first, err := storage.CreateMessage(testCtx(), channel.ID, author.ID, "primeira mensagem", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar primeira mensagem: %v", err)
	}

	channels, err := ListChannels(testCtx(), author.ID)
	if err != nil {
		t.Fatalf("ListChannels retornou erro: %v", err)
	}
	if len(channels) != 1 {
		t.Fatalf("esperava 1 canal, obtive %d", len(channels))
	}

	last := channels[0].LastMessage
	if last == nil {
		t.Fatal("esperava last_message preenchida")
	}
	if last.ID != first.ID {
		t.Errorf("esperava last_message.id %s, obtive %s", first.ID, last.ID)
	}
	if last.Content == nil {
		t.Errorf("esperava content")
	}
	if *last.Content != "primeira mensagem" {
		t.Errorf("esperava content %q, obtive %q", "primeira mensagem", *last.Content)
	}
	if last.AuthorID == nil || *last.AuthorID != author.ID {
		t.Errorf("esperava author_id %s, obtive %v", author.ID, last.AuthorID)
	}
	if last.AuthorUsername == nil || *last.AuthorUsername != author.Username {
		t.Errorf("esperava author_username %q, obtive %v", author.Username, last.AuthorUsername)
	}
	if !last.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("esperava created_at %v, obtive %v", first.CreatedAt, last.CreatedAt)
	}

	// A segunda mensagem deve substituir a primeira como last_message.
	// O intervalo garante timestamps distintos (o desempate da ordenação por id é aleatório).
	time.Sleep(20 * time.Millisecond)
	second, err := storage.CreateMessage(testCtx(), channel.ID, author.ID, "segunda mensagem", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar segunda mensagem: %v", err)
	}

	channels, err = ListChannels(testCtx(), author.ID)
	if err != nil {
		t.Fatalf("ListChannels retornou erro: %v", err)
	}
	if len(channels) != 1 {
		t.Fatalf("esperava 1 canal, obtive %d", len(channels))
	}
	last = channels[0].LastMessage
	if last == nil {
		t.Fatal("esperava last_message preenchida")
	}
	if last.ID != second.ID {
		t.Errorf("esperava last_message.id %s, obtive %s", second.ID, last.ID)
	}
	if last.Content == nil {
		t.Errorf("esperava content")
	}
	if *last.Content != "segunda mensagem" {
		t.Errorf("esperava content %q, obtive %q", "segunda mensagem", *last.Content)
	}
}

func TestListChannelsPermissionsExpanded(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	roleA, err := storage.CreateRole(testCtx(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role A: %v", err)
	}
	roleB, err := storage.CreateRole(testCtx(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role B: %v", err)
	}

	permA := models.ChannelPermission{ReadChannel: true, SendMessages: true, DeleteMessages: false}
	permB := models.ChannelPermission{ReadChannel: false, SendMessages: false, DeleteMessages: true}
	if _, err := UpdateChannelPermissions(testCtx(), testActorID(), channel.ID, roleA.ID, permA); err != nil {
		t.Fatalf("falha ao atualizar permissões da role A: %v", err)
	}
	if _, err := UpdateChannelPermissions(testCtx(), testActorID(), channel.ID, roleB.ID, permB); err != nil {
		t.Fatalf("falha ao atualizar permissões da role B: %v", err)
	}

	channels, err := ListChannels(testCtx(), testActorID())
	if err != nil {
		t.Fatalf("ListChannels retornou erro: %v", err)
	}
	if len(channels) != 1 {
		t.Fatalf("esperava 1 canal, obtive %d", len(channels))
	}

	permissions := channels[0].Permissions
	if len(permissions) != 2 {
		t.Fatalf("esperava 2 permissões, obtive %d", len(permissions))
	}
	// as entradas devem estar ordenadas por role_id
	if permissions[0].RoleID > permissions[1].RoleID {
		t.Error("esperava permissões ordenadas por role_id")
	}

	byRoleID := make(map[string]models.ChannelPermissionEntry, len(permissions))
	for _, p := range permissions {
		byRoleID[p.RoleID] = p
	}

	if got := byRoleID[roleA.ID]; got.RoleName != roleA.Name || got.Permissions != permA {
		t.Errorf("permissões da role A não conferem: got %+v", got)
	}
	if got := byRoleID[roleB.ID]; got.RoleName != roleB.Name || got.Permissions != permB {
		t.Errorf("permissões da role B não conferem: got %+v", got)
	}
}

// --- CreateChannel ---

func TestCreateChannel(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	name := newRandomChannelName()
	summary, err := CreateChannel(testCtx(), testActorID(), name, "text", "")
	if err != nil {
		t.Fatalf("CreateChannel retornou erro: %v", err)
	}

	if summary.ID == "" {
		t.Error("esperava id preenchido")
	}
	if summary.Name != name {
		t.Errorf("esperava name %q, obtive %q", name, summary.Name)
	}
	if summary.Type != "text" {
		t.Errorf("esperava type %q, obtive %q", "text", summary.Type)
	}
	if len(summary.Permissions) != 0 {
		t.Errorf("esperava permissões vazias, obtive %+v", summary.Permissions)
	}
	if summary.LastMessage != nil {
		t.Errorf("esperava last_message nula, obtive %+v", summary.LastMessage)
	}
	if summary.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}

	// o canal deve ter sido persistido
	stored, err := storage.GetChannelByID(testCtx(), summary.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}

	if stored.Name != name {
		t.Errorf("esperava name %q, obtive %q", name, stored.Name)
	}
	if stored.Type != "text" {
		t.Errorf("esperava type %q, obtive %q", "text", stored.Type)
	}
	if len(stored.Permissions) != 0 {
		t.Errorf("esperava permissões vazias no banco, obtive %+v", stored.Permissions)
	}
}

func TestCreateChannelInvalidInput(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	cases := []struct {
		name    string
		channel string
	}{
		{"nome vazio", ""},
		{"nome acima de 32 caracteres", strings.Repeat("a", 33)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateChannel(testCtx(), testActorID(), tc.channel, "", "")
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("esperava ErrInvalidInput, obtive %v", err)
			}
		})
	}
}

func TestCreateChannelBoundaryNameLength(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	// 32 caracteres (runes) com conteúdo multibyte estão dentro do limite
	name := newBoundaryChannelName()
	summary, err := CreateChannel(testCtx(), testActorID(), name, "text", "")
	if err != nil {
		t.Fatalf("CreateChannel com nome de 32 caracteres retornou erro: %v", err)
	}
	if summary.Name != name {
		t.Errorf("esperava name %q, obtive %q", name, summary.Name)
	}
}

func TestCreateChannelNameTaken(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	name := newRandomChannelName()
	if _, err := CreateChannel(testCtx(), testActorID(), name, "text", ""); err != nil {
		t.Fatalf("falha ao criar primeiro canal: %v", err)
	}

	_, err = CreateChannel(testCtx(), testActorID(), name, "text", "")
	if !errors.Is(err, ErrChannelNameTaken) {
		t.Errorf("esperava ErrChannelNameTaken, obtive %v", err)
	}

	// a tentativa duplicada não deve criar um segundo registro
	channels, err := storage.ListChannels(testCtx())
	if err != nil {
		t.Fatalf("ListChannels retornou erro: %v", err)
	}
	count := 0
	for _, c := range channels {
		if c.Name == name {
			count++
		}
	}
	if count != 1 {
		t.Errorf("esperava 1 canal com o nome %q, obtive %d", name, count)
	}
}

func TestCreateChannelType(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	// type ausente usa o padrão "text"
	defaultSummary, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "", "")
	if err != nil {
		t.Fatalf("CreateChannel sem type retornou erro: %v", err)
	}
	if defaultSummary.Type != "text" {
		t.Errorf("esperava type %q quando ausente, obtive %q", "text", defaultSummary.Type)
	}

	// type "text" explícito
	textSummary, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("CreateChannel com type text retornou erro: %v", err)
	}
	if textSummary.Type != "text" {
		t.Errorf("esperava type %q, obtive %q", "text", textSummary.Type)
	}

	// type "category" é gravado no banco
	categorySummary, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "category", "")
	if err != nil {
		t.Fatalf("CreateChannel com type category retornou erro: %v", err)
	}
	if categorySummary.Type != "category" {
		t.Errorf("esperava type %q, obtive %q", "category", categorySummary.Type)
	}
	stored, err := storage.GetChannelByID(testCtx(), categorySummary.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Type != "category" {
		t.Errorf("esperava type %q no banco, obtive %q", "category", stored.Type)
	}

	// type inválido é rejeitado ("voice" é válido desde a migration 009)
	for _, invalidType := range []string{"audio", "TEXT"} {
		if _, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), invalidType, ""); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("esperava ErrInvalidInput para type %q, obtive %v", invalidType, err)
		}
	}
}

func TestCreateChannelTopic(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	topic := "tópico do canal"
	summary, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", topic)
	if err != nil {
		t.Fatalf("CreateChannel com topic retornou erro: %v", err)
	}
	if summary.Topic == nil || *summary.Topic != topic {
		t.Errorf("esperava topic %q, obtive %v", topic, summary.Topic)
	}

	stored, err := storage.GetChannelByID(testCtx(), summary.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Topic == nil || *stored.Topic != topic {
		t.Errorf("esperava topic %q no banco, obtive %v", topic, stored.Topic)
	}
}

func TestCreateChannelTopicTooLong(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	if _, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", strings.Repeat("a", 513)); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para topic acima de 512 caracteres, obtive %v", err)
	}
}

func TestCreateChannelTopicBoundary(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	// 512 caracteres (runes) com conteúdo multibyte estão dentro do limite
	topic := randHex(4) + strings.Repeat("ç", 504)
	summary, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", topic)
	if err != nil {
		t.Fatalf("CreateChannel com topic de 512 caracteres retornou erro: %v", err)
	}
	if summary.Topic == nil || *summary.Topic != topic {
		t.Errorf("esperava topic %q, obtive %v", topic, summary.Topic)
	}
}

func TestCreateChannelCategoryWithTopic(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	if _, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "category", "topic de categoria"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para canal category com topic, obtive %v", err)
	}
}

func TestUpdateChannelTopic(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	topic := "novo tópico"
	summary, err := UpdateChannel(testCtx(), testActorID(), channel.ID, channel.Name, &topic)
	if err != nil {
		t.Fatalf("UpdateChannel com topic retornou erro: %v", err)
	}
	if summary.Topic == nil || *summary.Topic != topic {
		t.Errorf("esperava topic %q, obtive %v", topic, summary.Topic)
	}

	stored, err := storage.GetChannelByID(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Topic == nil || *stored.Topic != topic {
		t.Errorf("esperava topic %q no banco, obtive %v", topic, stored.Topic)
	}
}

func TestUpdateChannelClearTopic(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "tópico inicial")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	empty := ""
	summary, err := UpdateChannel(testCtx(), testActorID(), channel.ID, channel.Name, &empty)
	if err != nil {
		t.Fatalf("UpdateChannel limpando topic retornou erro: %v", err)
	}
	if summary.Topic != nil {
		t.Errorf("esperava topic nil após limpar, obtive %v", summary.Topic)
	}
}

func TestUpdateChannelTopicNilUnchanged(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "tópico mantido")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	summary, err := UpdateChannel(testCtx(), testActorID(), channel.ID, newRandomChannelName(), nil)
	if err != nil {
		t.Fatalf("UpdateChannel com topic nil retornou erro: %v", err)
	}
	if summary.Topic == nil || *summary.Topic != "tópico mantido" {
		t.Errorf("esperava topic %q inalterado, obtive %v", "tópico mantido", summary.Topic)
	}
}

func TestUpdateChannelTopicTooLong(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	tooLong := strings.Repeat("a", 513)
	if _, err := UpdateChannel(testCtx(), testActorID(), channel.ID, channel.Name, &tooLong); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para topic acima de 512 caracteres, obtive %v", err)
	}
}

func TestUpdateChannelCategoryWithTopic(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "category", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	topic := "topic de categoria"
	if _, err := UpdateChannel(testCtx(), testActorID(), channel.ID, channel.Name, &topic); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para canal category com topic, obtive %v", err)
	}
}

// --- UpdateChannel ---

func TestUpdateChannel(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	permission := models.ChannelPermission{ReadChannel: true, SendMessages: true}
	if _, err := UpdateChannelPermissions(testCtx(), testActorID(), channel.ID, role.ID, permission); err != nil {
		t.Fatalf("falha ao atualizar permissões: %v", err)
	}

	newName := newRandomChannelName()
	summary, err := UpdateChannel(testCtx(), testActorID(), channel.ID, newName, nil)
	if err != nil {
		t.Fatalf("UpdateChannel retornou erro: %v", err)
	}

	if summary.ID != channel.ID {
		t.Errorf("esperava id %s, obtive %s", channel.ID, summary.ID)
	}
	if summary.Name != newName {
		t.Errorf("esperava name %q, obtive %q", newName, summary.Name)
	}
	if summary.Type != "text" {
		t.Errorf("esperava type %q, obtive %q", "text", summary.Type)
	}
	if summary.LastMessage != nil {
		t.Errorf("esperava last_message nula, obtive %+v", summary.LastMessage)
	}
	if summary.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}

	// as permissões expandidas devem acompanhar o rename
	if len(summary.Permissions) != 1 {
		t.Fatalf("esperava 1 permissão, obtive %d", len(summary.Permissions))
	}
	if summary.Permissions[0].RoleID != role.ID {
		t.Errorf("esperava role_id %s, obtive %s", role.ID, summary.Permissions[0].RoleID)
	}
	if summary.Permissions[0].RoleName != role.Name {
		t.Errorf("esperava role_name %q, obtive %q", role.Name, summary.Permissions[0].RoleName)
	}
	if summary.Permissions[0].Permissions != permission {
		t.Errorf("esperava permissões %v, obtive %v", permission, summary.Permissions[0].Permissions)
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

func TestUpdateChannelEmptyID(t *testing.T) {
	_, err := UpdateChannel(testCtx(), testActorID(), "", newRandomChannelName(), nil)
	if !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("esperava ErrChannelNotFound para id vazio, obtive %v", err)
	}
}

func TestUpdateChannelEmptyName(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	_, err = UpdateChannel(testCtx(), testActorID(), channel.ID, "", nil)
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para nome vazio, obtive %v", err)
	}
}

func TestUpdateChannelNameTooLong(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	_, err = UpdateChannel(testCtx(), testActorID(), channel.ID, strings.Repeat("a", 33), nil)
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para nome acima de 32 caracteres, obtive %v", err)
	}
}

func TestUpdateChannelBoundaryNameLength(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	// 32 caracteres (runes) com conteúdo multibyte estão dentro do limite
	name := newBoundaryChannelName()
	summary, err := UpdateChannel(testCtx(), testActorID(), channel.ID, name, nil)
	if err != nil {
		t.Fatalf("UpdateChannel com nome de 32 caracteres retornou erro: %v", err)
	}
	if summary.Name != name {
		t.Errorf("esperava name %q, obtive %q", name, summary.Name)
	}
}

func TestUpdateChannelNonexistent(t *testing.T) {
	_, err := UpdateChannel(testCtx(), testActorID(), randUUID(), newRandomChannelName(), nil)
	if !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("esperava ErrChannelNotFound para id inexistente, obtive %v", err)
	}
}

func TestUpdateChannelNameTaken(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar primeiro canal: %v", err)
	}
	takenName := newRandomChannelName()
	if _, err := CreateChannel(testCtx(), testActorID(), takenName, "text", ""); err != nil {
		t.Fatalf("falha ao criar segundo canal: %v", err)
	}

	_, err = UpdateChannel(testCtx(), testActorID(), channel.ID, takenName, nil)
	if !errors.Is(err, ErrChannelNameTaken) {
		t.Errorf("esperava ErrChannelNameTaken, obtive %v", err)
	}

	// o rename recusado não deve alterar o nome original
	stored, err := storage.GetChannelByID(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Name != channel.Name {
		t.Errorf("esperava name original %q, obtive %q", channel.Name, stored.Name)
	}
}

// --- DeleteChannel ---

func TestDeleteChannel(t *testing.T) {
	cleanServers(testCtx())
	owner, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário dono: %v", err)
	}
	author, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário autor: %v", err)
	}

	_, err = CreateServer(testCtx(), newRandomServerName(), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	if _, err := storage.CreateMessage(testCtx(), channel.ID, author.ID, "mensagem do canal", "", nil); err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	if err := DeleteChannel(testCtx(), testActorID(), channel.ID); err != nil {
		t.Fatalf("DeleteChannel retornou erro: %v", err)
	}

	if _, err := storage.GetChannelByID(testCtx(), channel.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava canal removido, obtive %v", err)
	}

	// as mensagens do canal devem ser removidas em cascata (schema)
	messages, err := storage.ListMessagesByChannel(testCtx(), channel.ID, nil, "", nil)
	if err != nil {
		t.Fatalf("ListMessagesByChannel retornou erro: %v", err)
	}
	if len(messages) != 0 {
		t.Errorf("esperava mensagens removidas em cascata, obtive %d", len(messages))
	}
}

func TestDeleteChannelEmptyID(t *testing.T) {
	err := DeleteChannel(testCtx(), testActorID(), "")
	if !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("esperava ErrChannelNotFound para id vazio, obtive %v", err)
	}
}

func TestDeleteChannelNonexistent(t *testing.T) {
	err := DeleteChannel(testCtx(), testActorID(), randUUID())
	if !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("esperava ErrChannelNotFound para id inexistente, obtive %v", err)
	}
}

// --- ChangeChannelPosition (tarefa 8.4) ---

func TestChangeChannelPosition(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	c1, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar primeiro canal: %v", err)
	}
	c2, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar segundo canal: %v", err)
	}
	c3, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar terceiro canal: %v", err)
	}

	summary, err := ChangeChannelPosition(testCtx(), testActorID(), c1.ID, 1, 3)
	if err != nil {
		t.Fatalf("ChangeChannelPosition retornou erro: %v", err)
	}
	if summary.ID != c1.ID || summary.Position != 3 {
		t.Errorf("esperava canal %s na posição 3, obtive %s na posição %d", c1.ID, summary.ID, summary.Position)
	}

	channels, err := storage.ListChannels(testCtx())
	if err != nil {
		t.Fatalf("ListChannels retornou erro: %v", err)
	}
	expected := []string{c2.ID, c3.ID, c1.ID}
	for i, want := range expected {
		if channels[i].ID != want {
			t.Errorf("posição %d: esperava canal %s, obtive %s", i+1, want, channels[i].ID)
		}
	}
}

func TestChangeChannelPositionMoveUp(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	_, err = CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar primeiro canal: %v", err)
	}
	_, err = CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar segundo canal: %v", err)
	}
	c3, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar terceiro canal: %v", err)
	}

	summary, err := ChangeChannelPosition(testCtx(), testActorID(), c3.ID, 3, 1)
	if err != nil {
		t.Fatalf("ChangeChannelPosition retornou erro: %v", err)
	}
	if summary.ID != c3.ID || summary.Position != 1 {
		t.Errorf("esperava canal %s na posição 1, obtive %s na posição %d", c3.ID, summary.ID, summary.Position)
	}
}

func TestChangeChannelPositionSamePosition(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	_, err = CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar primeiro canal: %v", err)
	}
	c2, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar segundo canal: %v", err)
	}

	summary, err := ChangeChannelPosition(testCtx(), testActorID(), c2.ID, 2, 2)
	if err != nil {
		t.Fatalf("ChangeChannelPosition retornou erro: %v", err)
	}
	if summary.ID != c2.ID || summary.Position != 2 {
		t.Errorf("esperava canal %s na posição 2, obtive %s na posição %d", c2.ID, summary.ID, summary.Position)
	}
}

func TestChangeChannelPositionInvalidInput(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	c1, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	for _, tc := range []struct {
		name string
		old  int
		new  int
	}{
		{"old_position zero", 0, 1},
		{"old_position negativo", -1, 1},
		{"new_position zero", 1, 0},
		{"new_position negativa", 1, -1},
		{"new_position acima do último", 1, 2},
		{"new_position muito acima", 1, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ChangeChannelPosition(testCtx(), testActorID(), c1.ID, tc.old, tc.new); !errors.Is(err, ErrInvalidInput) {
				t.Errorf("esperava ErrInvalidInput, obtive %v", err)
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
	if _, err := ChangeChannelPosition(testCtx(), testActorID(), "", 1, 1); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("esperava ErrChannelNotFound para id vazio, obtive %v", err)
	}
	if _, err := ChangeChannelPosition(testCtx(), testActorID(), randUUID(), 1, 1); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("esperava ErrChannelNotFound para id inexistente, obtive %v", err)
	}
}

func TestChangeChannelPositionConflict(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	c1, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar primeiro canal: %v", err)
	}
	c2, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar segundo canal: %v", err)
	}
	c3, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar terceiro canal: %v", err)
	}

	if _, err := ChangeChannelPosition(testCtx(), testActorID(), c1.ID, 2, 3); !errors.Is(err, ErrChannelPositionConflict) {
		t.Fatalf("esperava ErrChannelPositionConflict, obtive %v", err)
	}

	channels, err := storage.ListChannels(testCtx())
	if err != nil {
		t.Fatalf("ListChannels retornou erro: %v", err)
	}
	expected := []string{c1.ID, c2.ID, c3.ID}
	for i, want := range expected {
		if channels[i].ID != want {
			t.Errorf("ordem alterada pelo conflito: %+v", channels)
		}
	}
}

// --- GetChannelPermissions ---

func TestGetChannelPermissions(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	roleA, err := storage.CreateRole(testCtx(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role A: %v", err)
	}
	roleB, err := storage.CreateRole(testCtx(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role B: %v", err)
	}

	permA := models.ChannelPermission{ReadChannel: true, SendMessages: true, DeleteMessages: false}
	permB := models.ChannelPermission{ReadChannel: false, SendMessages: false, DeleteMessages: true}
	if _, err := UpdateChannelPermissions(testCtx(), testActorID(), channel.ID, roleA.ID, permA); err != nil {
		t.Fatalf("falha ao atualizar permissões da role A: %v", err)
	}
	if _, err := UpdateChannelPermissions(testCtx(), testActorID(), channel.ID, roleB.ID, permB); err != nil {
		t.Fatalf("falha ao atualizar permissões da role B: %v", err)
	}

	permissions, err := GetChannelPermissions(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelPermissions retornou erro: %v", err)
	}
	if len(permissions) != 2 {
		t.Fatalf("esperava 2 permissões, obtive %d", len(permissions))
	}
	// as entradas devem estar ordenadas por role_id
	if permissions[0].RoleID > permissions[1].RoleID {
		t.Error("esperava permissões ordenadas por role_id")
	}

	byRoleID := make(map[string]models.ChannelPermissionEntry, len(permissions))
	for _, p := range permissions {
		byRoleID[p.RoleID] = p
	}

	if got := byRoleID[roleA.ID]; got.RoleName != roleA.Name || got.Permissions != permA {
		t.Errorf("permissões da role A não conferem: got %+v", got)
	}
	if got := byRoleID[roleB.ID]; got.RoleName != roleB.Name || got.Permissions != permB {
		t.Errorf("permissões da role B não conferem: got %+v", got)
	}
}

func TestGetChannelPermissionsEmpty(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	permissions, err := GetChannelPermissions(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelPermissions retornou erro: %v", err)
	}
	if len(permissions) != 0 {
		t.Errorf("esperava permissões vazias, obtive %+v", permissions)
	}
}

func TestGetChannelPermissionsEmptyID(t *testing.T) {
	_, err := GetChannelPermissions(testCtx(), "")
	if !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("esperava ErrChannelNotFound para id vazio, obtive %v", err)
	}
}

func TestGetChannelPermissionsNonexistent(t *testing.T) {
	_, err := GetChannelPermissions(testCtx(), randUUID())
	if !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("esperava ErrChannelNotFound para id inexistente, obtive %v", err)
	}
}

// --- UpdateChannelPermissions ---

func TestUpdateChannelPermissions(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	permission := models.ChannelPermission{ReadChannel: true, SendMessages: false, DeleteMessages: true}

	got, err := UpdateChannelPermissions(testCtx(), testActorID(), channel.ID, role.ID, permission)
	if err != nil {
		t.Fatalf("UpdateChannelPermissions retornou erro: %v", err)
	}
	if got != permission {
		t.Errorf("esperava %v, obtive %v", permission, got)
	}

	stored, err := storage.GetChannelByID(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Permissions[role.ID] != permission {
		t.Errorf("permissão persistida não confere: got %v", stored.Permissions[role.ID])
	}
}

func TestUpdateChannelPermissionsReplacesExisting(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	first := models.ChannelPermission{ReadChannel: true, SendMessages: true, DeleteMessages: true}
	second := models.ChannelPermission{ReadChannel: true, SendMessages: false, DeleteMessages: false}

	if _, err := UpdateChannelPermissions(testCtx(), testActorID(), channel.ID, role.ID, first); err != nil {
		t.Fatalf("falha ao atualizar permissões (primeira): %v", err)
	}
	if _, err := UpdateChannelPermissions(testCtx(), testActorID(), channel.ID, role.ID, second); err != nil {
		t.Fatalf("falha ao atualizar permissões (segunda): %v", err)
	}

	stored, err := storage.GetChannelByID(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Permissions[role.ID] != second {
		t.Errorf("esperava permissão substituída %v, obtive %v", second, stored.Permissions[role.ID])
	}
}

func TestUpdateChannelPermissionsMultipleRoles(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	roleA, err := storage.CreateRole(testCtx(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role A: %v", err)
	}
	roleB, err := storage.CreateRole(testCtx(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role B: %v", err)
	}

	permA := models.ChannelPermission{ReadChannel: true, SendMessages: true, DeleteMessages: false}
	permB := models.ChannelPermission{ReadChannel: true, SendMessages: false, DeleteMessages: true}

	if _, err := UpdateChannelPermissions(testCtx(), testActorID(), channel.ID, roleA.ID, permA); err != nil {
		t.Fatalf("falha ao atualizar permissões da role A: %v", err)
	}
	if _, err := UpdateChannelPermissions(testCtx(), testActorID(), channel.ID, roleB.ID, permB); err != nil {
		t.Fatalf("falha ao atualizar permissões da role B: %v", err)
	}

	stored, err := storage.GetChannelByID(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if len(stored.Permissions) != 2 {
		t.Fatalf("esperava 2 permissões, obtive %d", len(stored.Permissions))
	}
	if stored.Permissions[roleA.ID] != permA {
		t.Errorf("permissão da role A não confere: got %v", stored.Permissions[roleA.ID])
	}
	if stored.Permissions[roleB.ID] != permB {
		t.Errorf("permissão da role B não confere: got %v", stored.Permissions[roleB.ID])
	}
}

func TestUpdateChannelPermissionsNonexistentRole(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	_, err = UpdateChannelPermissions(testCtx(), testActorID(), channel.ID, randUUID(), models.ChannelPermission{ReadChannel: true})
	if !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("esperava ErrRoleNotFound para role inexistente, obtive %v", err)
	}
}

func TestUpdateChannelPermissionsEmptyChannelID(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	_, err = UpdateChannelPermissions(testCtx(), testActorID(), "", role.ID, models.ChannelPermission{})
	if !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("esperava ErrChannelNotFound para channel_id vazio, obtive %v", err)
	}
}

func TestUpdateChannelPermissionsEmptyRoleID(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	_, err = UpdateChannelPermissions(testCtx(), testActorID(), channel.ID, "", models.ChannelPermission{})
	if !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("esperava ErrRoleNotFound para role_id vazio, obtive %v", err)
	}
}

func TestUpdateChannelPermissionsNonexistentChannel(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	_, err = UpdateChannelPermissions(testCtx(), testActorID(), randUUID(), role.ID, models.ChannelPermission{})
	if !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("esperava ErrChannelNotFound para canal inexistente, obtive %v", err)
	}
}

func strPtr(s string) *string {
	return &s
}

// --- ListRoles ---
func TestListRoles(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	permsA := models.RolePermissions{ManageRoles: true, BanMembers: true}
	roleA, err := CreateRole(testCtx(), testActorID(), newRandomRoleName(), strPtr("#FF0000"), permsA)
	if err != nil {
		t.Fatalf("falha ao criar role A: %v", err)
	}
	roleB, err := CreateRole(testCtx(), testActorID(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role B: %v", err)
	}

	roles, err := ListRoles(testCtx())
	if err != nil {
		t.Fatalf("ListRoles retornou erro: %v", err)
	}
	if len(roles) != 2 {
		t.Fatalf("esperava 2 roles, obtive %d", len(roles))
	}

	byID := make(map[string]models.Role, len(roles))
	for _, r := range roles {
		byID[r.ID] = r
	}

	gotA, ok := byID[roleA.ID]
	if !ok {
		t.Fatalf("role A ausente da listagem: %+v", roles)
	}

	if gotA.Color == nil || *gotA.Color != "#FF0000" {
		t.Errorf("esperava color #FF0000, obtive %v", gotA.Color)
	}
	if gotA.Permissions != permsA {
		t.Errorf("esperava permissões %v, obtive %v", permsA, gotA.Permissions)
	}

	gotB, ok := byID[roleB.ID]
	if !ok {
		t.Fatalf("role B ausente da listagem: %+v", roles)
	}
	if gotB.Color != nil {
		t.Errorf("esperava color nula, obtive %v", *gotB.Color)
	}
}

// --- CreateRole ---

func TestCreateRole(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	name := newRandomRoleName()
	color := "#FF0000"
	perms := models.RolePermissions{ManageRoles: true, BanMembers: true, EveryoneMessage: true}
	role, err := CreateRole(testCtx(), testActorID(), name, &color, perms)
	if err != nil {
		t.Fatalf("CreateRole retornou erro: %v", err)
	}

	if role.ID == "" {
		t.Error("esperava id preenchido")
	}

	if role.Name != name {
		t.Errorf("esperava name %q, obtive %q", name, role.Name)
	}
	if role.Color == nil || *role.Color != color {
		t.Errorf("esperava color %q, obtive %v", color, role.Color)
	}
	if role.Permissions != perms {
		t.Errorf("esperava permissões %v, obtive %v", perms, role.Permissions)
	}
	if role.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}

	// a role deve ter sido persistida
	stored, err := storage.GetRoleByID(testCtx(), role.ID)
	if err != nil {
		t.Fatalf("GetRoleByID retornou erro: %v", err)
	}
	if stored.Name != name {
		t.Errorf("esperava name %q, obtive %q", name, stored.Name)
	}
	if stored.Color == nil || *stored.Color != color {
		t.Errorf("esperava color %q no banco, obtive %v", color, stored.Color)
	}
	if stored.Permissions != perms {
		t.Errorf("esperava permissões %v no banco, obtive %v", perms, stored.Permissions)
	}
}

func TestCreateRoleWithoutColor(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	role, err := CreateRole(testCtx(), testActorID(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("CreateRole sem color retornou erro: %v", err)
	}
	if role.Color != nil {
		t.Errorf("esperava color nula, obtive %v", *role.Color)
	}

	stored, err := storage.GetRoleByID(testCtx(), role.ID)
	if err != nil {
		t.Fatalf("GetRoleByID retornou erro: %v", err)
	}
	if stored.Color != nil {
		t.Errorf("esperava color nula no banco, obtive %v", *stored.Color)
	}
}

func TestCreateRoleInvalidInput(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	cases := []struct {
		name     string
		roleName string
		color    *string
	}{
		{"nome vazio", "", nil},
		{"nome acima de 32 caracteres", strings.Repeat("a", 33), nil},
		{"cor sem prefixo #", newRandomRoleName(), strPtr("FF0000")},
		{"cor com 3 dígitos", newRandomRoleName(), strPtr("#FFF")},
		{"cor com 7 dígitos", newRandomRoleName(), strPtr("#1234567")},
		{"cor com caracteres inválidos", newRandomRoleName(), strPtr("#GGGGGG")},
		{"cor em formato nomeado", newRandomRoleName(), strPtr("red")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateRole(testCtx(), testActorID(), tc.roleName, tc.color, models.RolePermissions{})
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("esperava ErrInvalidInput, obtive %v", err)
			}
		})
	}
}

func TestCreateRoleBoundaryNameLength(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	// 32 caracteres multibyte (64 bytes) estão dentro do limite
	name := strings.Repeat("ç", 32)
	role, err := CreateRole(testCtx(), testActorID(), name, nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("CreateRole com nome de 32 caracteres retornou erro: %v", err)
	}
	if role.Name != name {
		t.Errorf("esperava name %q, obtive %q", name, role.Name)
	}
}

// --- UpdateRole ---

func TestUpdateRole(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := CreateRole(testCtx(), testActorID(), newRandomRoleName(), strPtr("#FF0000"), models.RolePermissions{ManageRoles: true})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	newName := newRandomRoleName()
	newColor := "#00FF00"
	newPerms := models.RolePermissions{BanMembers: true, EveryoneMessage: true}
	updated, err := UpdateRole(testCtx(), testActorID(), role.ID, newName, &newColor, newPerms)
	if err != nil {
		t.Fatalf("UpdateRole retornou erro: %v", err)
	}

	if updated.ID != role.ID {
		t.Errorf("esperava id %s, obtive %s", role.ID, updated.ID)
	}

	if updated.Name != newName {
		t.Errorf("esperava name %q, obtive %q", newName, updated.Name)
	}
	if updated.Color == nil || *updated.Color != newColor {
		t.Errorf("esperava color %q, obtive %v", newColor, updated.Color)
	}
	if updated.Permissions != newPerms {
		t.Errorf("esperava permissões %v, obtive %v", newPerms, updated.Permissions)
	}
	if !updated.CreatedAt.Equal(role.CreatedAt) {
		t.Errorf("esperava created_at %s inalterado, obtive %s", role.CreatedAt, updated.CreatedAt)
	}

	// os novos valores devem estar persistidos
	stored, err := storage.GetRoleByID(testCtx(), role.ID)
	if err != nil {
		t.Fatalf("GetRoleByID retornou erro: %v", err)
	}
	if stored.Name != newName {
		t.Errorf("esperava name persistido %q, obtive %q", newName, stored.Name)
	}
	if stored.Color == nil || *stored.Color != newColor {
		t.Errorf("esperava color persistido %q, obtive %v", newColor, stored.Color)
	}
	if stored.Permissions != newPerms {
		t.Errorf("esperava permissões persistidas %v, obtive %v", newPerms, stored.Permissions)
	}
}

func TestUpdateRoleClearsColor(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := CreateRole(testCtx(), testActorID(), newRandomRoleName(), strPtr("#FF0000"), models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	if _, err := UpdateRole(testCtx(), testActorID(), role.ID, newRandomRoleName(), nil, models.RolePermissions{}); err != nil {
		t.Fatalf("UpdateRole com color nula retornou erro: %v", err)
	}

	stored, err := storage.GetRoleByID(testCtx(), role.ID)
	if err != nil {
		t.Fatalf("GetRoleByID retornou erro: %v", err)
	}
	if stored.Color != nil {
		t.Errorf("esperava color removida, obtive %v", *stored.Color)
	}
}

func TestUpdateRoleEmptyID(t *testing.T) {
	_, err := UpdateRole(testCtx(), testActorID(), "", newRandomRoleName(), nil, models.RolePermissions{})
	if !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("esperava ErrRoleNotFound para id vazio, obtive %v", err)
	}
}

func TestUpdateRoleInvalidInput(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := CreateRole(testCtx(), testActorID(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	cases := []struct {
		name     string
		roleName string
		color    *string
	}{
		{"nome vazio", "", nil},
		{"nome acima de 32 caracteres", strings.Repeat("a", 33), nil},
		{"cor sem prefixo #", newRandomRoleName(), strPtr("FF0000")},
		{"cor com 3 dígitos", newRandomRoleName(), strPtr("#FFF")},
		{"cor com caracteres inválidos", newRandomRoleName(), strPtr("#GGGGGG")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := UpdateRole(testCtx(), testActorID(), role.ID, tc.roleName, tc.color, models.RolePermissions{})
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("esperava ErrInvalidInput, obtive %v", err)
			}
		})
	}
}

func TestUpdateRoleNonexistent(t *testing.T) {
	_, err := UpdateRole(testCtx(), testActorID(), randUUID(), newRandomRoleName(), nil, models.RolePermissions{})
	if !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("esperava ErrRoleNotFound para id inexistente, obtive %v", err)
	}
}

func TestUpdateRoleNameTaken(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := CreateRole(testCtx(), testActorID(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar primeira role: %v", err)
	}
	takenName := newRandomRoleName()
	if _, err := CreateRole(testCtx(), testActorID(), takenName, nil, models.RolePermissions{}); err != nil {
		t.Fatalf("falha ao criar segunda role: %v", err)
	}

	_, err = UpdateRole(testCtx(), testActorID(), role.ID, takenName, nil, models.RolePermissions{})
	if !errors.Is(err, ErrRoleNameTaken) {
		t.Errorf("esperava ErrRoleNameTaken, obtive %v", err)
	}

	// o rename recusado não deve alterar o nome original
	stored, err := storage.GetRoleByID(testCtx(), role.ID)
	if err != nil {
		t.Fatalf("GetRoleByID retornou erro: %v", err)
	}
	if stored.Name != role.Name {
		t.Errorf("esperava name original %q, obtive %q", role.Name, stored.Name)
	}
}

// --- DeleteRole ---

func TestDeleteRole(t *testing.T) {
	cleanServers(testCtx())
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	_, err = CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	role, err := CreateRole(testCtx(), testActorID(), newRandomRoleName(), nil, models.RolePermissions{ManageRoles: true})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	if _, err := AssignUserRole(testCtx(), testActorID(), user.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role ao usuário: %v", err)
	}
	permission := models.ChannelPermission{ReadChannel: true, SendMessages: true}
	if _, err := UpdateChannelPermissions(testCtx(), testActorID(), channel.ID, role.ID, permission); err != nil {
		t.Fatalf("falha ao atualizar permissões: %v", err)
	}

	if err := DeleteRole(testCtx(), testActorID(), role.ID); err != nil {
		t.Fatalf("DeleteRole retornou erro: %v", err)
	}

	if _, err := storage.GetRoleByID(testCtx(), role.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava role removida, obtive %v", err)
	}

	// as atribuições dos usuários devem ser removidas em cascata (schema)
	if _, err := storage.GetUserRole(testCtx(), user.ID, role.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava atribuição removida em cascata, obtive %v", err)
	}

	// a entrada da role nas permissões dos canais do servidor deve ser removida
	stored, err := storage.GetChannelByID(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if _, ok := stored.Permissions[role.ID]; ok {
		t.Errorf("esperava permissão da role removida do canal, obtive %v", stored.Permissions)
	}
}

func TestDeleteRoleEmptyID(t *testing.T) {
	err := DeleteRole(testCtx(), testActorID(), "")
	if !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("esperava ErrRoleNotFound para id vazio, obtive %v", err)
	}
}

func TestDeleteRoleNonexistent(t *testing.T) {
	err := DeleteRole(testCtx(), testActorID(), randUUID())
	if !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("esperava ErrRoleNotFound para id inexistente, obtive %v", err)
	}
}

// --- AssignUserRole ---

func TestAssignUserRole(t *testing.T) {
	cleanServers(testCtx())
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	_, err = CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := CreateRole(testCtx(), testActorID(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	userRole, err := AssignUserRole(testCtx(), testActorID(), user.ID, role.ID)
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

	// a atribuição deve ter sido persistida
	stored, err := storage.GetUserRole(testCtx(), user.ID, role.ID)
	if err != nil {
		t.Fatalf("GetUserRole retornou erro: %v", err)
	}
	if stored.AssignedAt != userRole.AssignedAt {
		t.Errorf("esperava assigned_at %s no banco, obtive %s", userRole.AssignedAt, stored.AssignedAt)
	}

	userRoles, err := storage.GetRolesByUser(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetRolesByUser retornou erro: %v", err)
	}
	if len(userRoles) != 1 || userRoles[0].ID != role.ID {
		t.Errorf("esperava apenas a role %s no usuário, obtive %+v", role.ID, userRoles)
	}
}

func TestAssignUserRoleAlreadyAssigned(t *testing.T) {
	cleanServers(testCtx())
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	_, err = CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := CreateRole(testCtx(), testActorID(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	first, err := AssignUserRole(testCtx(), testActorID(), user.ID, role.ID)
	if err != nil {
		t.Fatalf("primeira AssignUserRole retornou erro: %v", err)
	}

	// atribuir a mesma role novamente é idempotente
	second, err := AssignUserRole(testCtx(), testActorID(), user.ID, role.ID)
	if err != nil {
		t.Fatalf("segunda AssignUserRole retornou erro: %v", err)
	}
	if second.UserID != first.UserID || second.RoleID != first.RoleID {
		t.Errorf("esperava a mesma atribuição %v, obtive %v", first, second)
	}
	if !second.AssignedAt.Equal(first.AssignedAt) {
		t.Errorf("esperava assigned_at %s preservado, obtive %s", first.AssignedAt, second.AssignedAt)
	}

	// a atribuição duplicada não deve criar um segundo registro
	roles, err := storage.GetRolesByUser(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetRolesByUser retornou erro: %v", err)
	}
	if len(roles) != 1 {
		t.Errorf("esperava 1 role atribuída, obtive %d", len(roles))
	}
}

func TestAssignUserRoleEmptyUserID(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := CreateRole(testCtx(), testActorID(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	_, err = AssignUserRole(testCtx(), testActorID(), "", role.ID)
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para user_id vazio, obtive %v", err)
	}
}

func TestAssignUserRoleEmptyRoleID(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	_, err = AssignUserRole(testCtx(), testActorID(), user.ID, "")
	if !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("esperava ErrRoleNotFound para role_id vazio, obtive %v", err)
	}
}

func TestAssignUserRoleNonexistentUser(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := CreateRole(testCtx(), testActorID(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	_, err = AssignUserRole(testCtx(), testActorID(), randUUID(), role.ID)
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para usuário inexistente, obtive %v", err)
	}
}

func TestAssignUserRoleNonexistentRole(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	_, err = AssignUserRole(testCtx(), testActorID(), user.ID, randUUID())
	if !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("esperava ErrRoleNotFound para role inexistente, obtive %v", err)
	}
}

// --- RemoveUserRole ---

func TestRemoveUserRole(t *testing.T) {
	cleanServers(testCtx())
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	_, err = CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := CreateRole(testCtx(), testActorID(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	if _, err := AssignUserRole(testCtx(), testActorID(), user.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role ao usuário: %v", err)
	}

	if err := RemoveUserRole(testCtx(), testActorID(), user.ID, role.ID); err != nil {
		t.Fatalf("RemoveUserRole retornou erro: %v", err)
	}

	if _, err := storage.GetUserRole(testCtx(), user.ID, role.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava atribuição removida, obtive %v", err)
	}
}

func TestRemoveUserRoleEmptyUserID(t *testing.T) {
	cleanServers(testCtx())
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := CreateRole(testCtx(), testActorID(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	err = RemoveUserRole(testCtx(), testActorID(), "", role.ID)
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para user_id vazio, obtive %v", err)
	}
}

func TestRemoveUserRoleEmptyRoleID(t *testing.T) {
	cleanServers(testCtx())
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	err = RemoveUserRole(testCtx(), testActorID(), user.ID, "")
	if !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("esperava ErrRoleNotFound para role_id vazio, obtive %v", err)
	}
}

func TestRemoveUserRoleNonexistentUser(t *testing.T) {
	_, err := CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := CreateRole(testCtx(), testActorID(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	err = RemoveUserRole(testCtx(), testActorID(), randUUID(), role.ID)
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("esperava ErrUserNotFound para usuário inexistente, obtive %v", err)
	}
}

func TestRemoveUserRoleNonexistentRole(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	err = RemoveUserRole(testCtx(), testActorID(), user.ID, randUUID())
	if !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("esperava ErrRoleNotFound para role inexistente, obtive %v", err)
	}
}

func TestRemoveUserRoleNotAssigned(t *testing.T) {
	cleanServers(testCtx())
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	_, err = CreateServer(testCtx(), newRandomServerName(), nil)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := CreateRole(testCtx(), testActorID(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	err = RemoveUserRole(testCtx(), testActorID(), user.ID, role.ID)
	if !errors.Is(err, ErrUserRoleNotFound) {
		t.Errorf("esperava ErrUserRoleNotFound para role não atribuída, obtive %v", err)
	}
}

// --- LoginServer / RequireServerAccess ---

// removeAllServersTest limpa a tabela servers (as dependências são removidas
// em cascata) para isolar os testes que dependem do estado do servidor do
// backend (1 backend = 1 servidor).
func removeAllServersTest(t *testing.T) {
	t.Helper()
	if _, err := storage.GetDB().ExecContext(testCtx(), "DELETE FROM servers"); err != nil {
		t.Fatalf("falha ao limpar a tabela servers: %v", err)
	}
}

// createNonPublicServerTest cria o servidor do backend como não público, com a
// senha informada (hash bcrypt).
func createNonPublicServerTest(t *testing.T, password string) {
	t.Helper()
	hash, err := utils.HashPassword(password)
	if err != nil {
		t.Fatalf("falha ao gerar hash da senha do servidor: %v", err)
	}
	if _, err := storage.CreateServerWithIcon(testCtx(), "server_"+randHex(8), nil, false, nil, &hash); err != nil {
		t.Fatalf("falha ao criar servidor não público: %v", err)
	}
}

// createPublicServerTest cria o servidor do backend como público.
func createPublicServerTest(t *testing.T) {
	t.Helper()
	if _, err := storage.CreateServerWithIcon(testCtx(), "server_"+randHex(8), nil, true, nil, nil); err != nil {
		t.Fatalf("falha ao criar servidor público: %v", err)
	}
}

func TestLoginServerInvalidInput(t *testing.T) {
	MaxPasswordLength, _ := getMaxLenFields()

	cases := []struct {
		name     string
		password string
	}{
		{"senha vazia", ""},
		{"senha acima do limite", strings.Repeat("a", MaxPasswordLength+1)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := LoginServer(testCtx(), tc.password, newRandomIP())
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("esperava ErrInvalidInput, obtive %v", err)
			}
		})
	}
}

func TestLoginServerBannedIP(t *testing.T) {
	ip := newRandomIP()
	bannedUser, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), ip)
	if err != nil {
		t.Fatalf("falha ao criar usuário para banir: %v", err)
	}
	if _, err := storage.SetUserBanned(testCtx(), bannedUser.ID, true); err != nil {
		t.Fatalf("falha ao banir usuário: %v", err)
	}

	err = LoginServer(testCtx(), "qualquer_senha", ip)
	if !errors.Is(err, ErrBannedIP) {
		t.Errorf("esperava ErrBannedIP, obtive %v", err)
	}
}

func TestLoginServerNotFound(t *testing.T) {
	removeAllServersTest(t)

	err := LoginServer(testCtx(), "qualquer_senha", newRandomIP())
	if !errors.Is(err, ErrServerNotFound) {
		t.Errorf("esperava ErrServerNotFound, obtive %v", err)
	}
}

func TestLoginServerSuccessNonPublic(t *testing.T) {
	password := "server_pw_" + randHex(4)
	removeAllServersTest(t)
	createNonPublicServerTest(t, password)

	if err := LoginServer(testCtx(), password, newRandomIP()); err != nil {
		t.Errorf("LoginServer com a senha correta retornou erro: %v", err)
	}

	err := LoginServer(testCtx(), "senha_incorreta_"+randHex(4), newRandomIP())
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("esperava ErrInvalidCredentials para senha incorreta, obtive %v", err)
	}
}

func TestLoginServerSuccessPublic(t *testing.T) {
	removeAllServersTest(t)
	createPublicServerTest(t)

	if err := LoginServer(testCtx(), "qualquer_senha", newRandomIP()); err != nil {
		t.Errorf("LoginServer em servidor público retornou erro: %v", err)
	}
}

func TestRequireServerAccess(t *testing.T) {
	cfg := config.LoadConfig()

	t.Run("bootstrap sem servidor", func(t *testing.T) {
		removeAllServersTest(t)
		if err := RequireServerAccess(testCtx(), ""); err != nil {
			t.Errorf("esperava sem erro no bootstrap, obtive %v", err)
		}
	})

	t.Run("servidor público", func(t *testing.T) {
		removeAllServersTest(t)
		createPublicServerTest(t)
		if err := RequireServerAccess(testCtx(), ""); err != nil {
			t.Errorf("esperava sem erro em servidor público, obtive %v", err)
		}
	})

	t.Run("servidor não público sem cookie", func(t *testing.T) {
		removeAllServersTest(t)
		createNonPublicServerTest(t, "server_pw_"+randHex(4))
		if err := RequireServerAccess(testCtx(), ""); !errors.Is(err, ErrServerAccessRequired) {
			t.Errorf("esperava ErrServerAccessRequired, obtive %v", err)
		}
	})

	t.Run("servidor não público com cookie inválido", func(t *testing.T) {
		removeAllServersTest(t)
		createNonPublicServerTest(t, "server_pw_"+randHex(4))
		if err := RequireServerAccess(testCtx(), "token-invalido"); !errors.Is(err, ErrServerAccessRequired) {
			t.Errorf("esperava ErrServerAccessRequired, obtive %v", err)
		}
	})

	t.Run("servidor não público com token de sessão", func(t *testing.T) {
		removeAllServersTest(t)
		createNonPublicServerTest(t, "server_pw_"+randHex(4))
		sessionToken, err := utils.GenerateToken("usuario-teste", cfg.JWTSecret)
		if err != nil {
			t.Fatalf("falha ao gerar token de sessão: %v", err)
		}
		if err := RequireServerAccess(testCtx(), sessionToken); !errors.Is(err, ErrServerAccessRequired) {
			t.Errorf("token de sessão não deveria conceder acesso, obtive %v", err)
		}
	})

	t.Run("servidor não público com token temporário válido", func(t *testing.T) {
		removeAllServersTest(t)
		createNonPublicServerTest(t, "server_pw_"+randHex(4))
		tempToken, err := utils.GenerateTempToken(cfg.JWTSecret)
		if err != nil {
			t.Fatalf("falha ao gerar token temporário: %v", err)
		}
		if err := RequireServerAccess(testCtx(), tempToken); err != nil {
			t.Errorf("esperava sem erro com token temporário válido, obtive %v", err)
		}
	})
}

// --- ListMessages ---

// newTestMessageUser registra um usuário de apoio para os testes de mensagem.
func newTestMessageUser(t *testing.T) models.User {
	t.Helper()
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário de apoio: %v", err)
	}
	return user
}

// newTestMessageChannel cria servidor e canal de apoio para os testes de mensagem.
func newTestMessageChannel(t *testing.T, ownerID *string) models.ChannelSummary {
	t.Helper()
	_, err := CreateServer(testCtx(), newRandomServerName(), ownerID)
	if err != nil {
		t.Fatalf("falha ao criar servidor de apoio: %v", err)
	}
	channel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal de apoio: %v", err)
	}
	return channel
}

// grantChannelPermission atribui uma role com a permissão de canal informada
// ao usuário e a vincula ao canal.
func grantChannelPermission(t *testing.T, channel models.ChannelSummary, user models.User, permission models.ChannelPermission) {
	t.Helper()
	role, err := storage.CreateRole(testCtx(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role de apoio: %v", err)
	}
	if _, err := UpdateChannelPermissions(testCtx(), testActorID(), channel.ID, role.ID, permission); err != nil {
		t.Fatalf("falha ao vincular permissão ao canal: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), user.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role ao usuário: %v", err)
	}
}

func TestListMessages(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	reader := newTestMessageUser(t)
	stranger := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	grantChannelPermission(t, channel, reader, models.ChannelPermission{ReadChannel: true})

	if _, err := CreateMessage(testCtx(), channel.ID, owner.ID, "primeira mensagem", "", nil); err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := CreateMessage(testCtx(), channel.ID, owner.ID, "segunda mensagem", "", nil); err != nil {
		t.Fatalf("falha ao criar segunda mensagem de apoio: %v", err)
	}

	// dono do servidor: sempre pode ler
	list, err := ListMessages(testCtx(), channel.ID, owner.ID, nil, "")
	if err != nil {
		t.Fatalf("ListMessages do dono retornou erro: %v", err)
	}
	if list.ChannelID != channel.ID {
		t.Errorf("esperava channel_id %s, obtive %s", channel.ID, list.ChannelID)
	}
	if len(list.Messages) != 2 {
		t.Fatalf("esperava 2 mensagens, obtive %d", len(list.Messages))
	}
	if list.HasMore {
		t.Error("esperava has_more false")
	}

	// usuário com read_channel: pode ler
	if _, err := ListMessages(testCtx(), channel.ID, reader.ID, nil, ""); err != nil {
		t.Errorf("ListMessages do leitor retornou erro: %v", err)
	}

	// usuário sem permissão no canal do servidor: negado
	if _, err := ListMessages(testCtx(), channel.ID, stranger.ID, nil, ""); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("esperava ErrPermissionDenied para usuário sem permissão, obtive %v", err)
	}

	// canal inexistente
	if _, err := ListMessages(testCtx(), randUUID(), owner.ID, nil, ""); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("esperava ErrChannelNotFound para canal inexistente, obtive %v", err)
	}

	// channel_id vazio
	if _, err := ListMessages(testCtx(), "", owner.ID, nil, ""); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("esperava ErrChannelNotFound para channel_id vazio, obtive %v", err)
	}
}

func TestListMessagesSince(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)

	first, err := CreateMessage(testCtx(), channel.ID, owner.ID, "primeira", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar primeira mensagem: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := CreateMessage(testCtx(), channel.ID, owner.ID, "segunda", "", nil); err != nil {
		t.Fatalf("falha ao criar segunda mensagem: %v", err)
	}

	// since após a primeira mensagem: só a segunda
	since := first.CreatedAt
	list, err := ListMessages(testCtx(), channel.ID, owner.ID, timePtr(since), "")
	if err != nil {
		t.Fatalf("ListMessages com since retornou erro: %v", err)
	}
	if len(list.Messages) != 1 {
		t.Fatalf("esperava 1 mensagem após o since, obtive %d", len(list.Messages))
	}
}

func TestListMessagesHasMore(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)

	for i := 0; i < 101; i++ {
		if _, err := CreateMessage(testCtx(), channel.ID, owner.ID, "mensagem "+randHex(2), "", nil); err != nil {
			t.Fatalf("falha ao criar mensagem de apoio %d: %v", i, err)
		}
	}

	list, err := ListMessages(testCtx(), channel.ID, owner.ID, nil, "")
	if err != nil {
		t.Fatalf("ListMessages retornou erro: %v", err)
	}
	if !list.HasMore {
		t.Error("esperava has_more true com 101 mensagens")
	}
	if len(list.Messages) != 100 {
		t.Errorf("esperava 100 mensagens, obtive %d", len(list.Messages))
	}
}

// --- CreateMessage ---

func TestCreateMessage(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	writer := newTestMessageUser(t)
	stranger := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	grantChannelPermission(t, channel, writer, models.ChannelPermission{SendMessages: true})

	// dono do servidor: sempre pode enviar
	message, err := CreateMessage(testCtx(), channel.ID, owner.ID, "olá mundo", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage do dono retornou erro: %v", err)
	}
	if message.ID == "" {
		t.Error("esperava id preenchido")
	}
	if message.ChannelID != channel.ID {
		t.Errorf("esperava channel_id %s, obtive %s", channel.ID, message.ChannelID)
	}
	if message.AuthorID == nil || *message.AuthorID != owner.ID {
		t.Errorf("esperava author_id %s, obtive %v", owner.ID, message.AuthorID)
	}
	if message.Content == nil || *message.Content != "olá mundo" {
		t.Errorf("esperava content %q, obtive %v", "olá mundo", message.Content)
	}
	if message.EditedAt != nil {
		t.Errorf("esperava edited_at nil, obtive %v", message.EditedAt)
	}
	if len(message.Attachments) != 0 {
		t.Errorf("esperava 0 attachments, obtive %d", len(message.Attachments))
	}

	// usuário com send_messages: pode enviar
	if _, err := CreateMessage(testCtx(), channel.ID, writer.ID, "mensagem do writer", "", nil); err != nil {
		t.Errorf("CreateMessage do writer retornou erro: %v", err)
	}

	// usuário sem permissão no canal: negado
	if _, err := CreateMessage(testCtx(), channel.ID, stranger.ID, "mensagem do estranho", "", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("esperava ErrPermissionDenied para usuário sem send_messages, obtive %v", err)
	}
}

func TestCreateMessageInvalidInput(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)

	// channel_id vazio
	if _, err := CreateMessage(testCtx(), "", owner.ID, "conteúdo", "", nil); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("esperava ErrChannelNotFound para channel_id vazio, obtive %v", err)
	}

	// canal inexistente
	if _, err := CreateMessage(testCtx(), randUUID(), owner.ID, "conteúdo", "", nil); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("esperava ErrChannelNotFound para canal inexistente, obtive %v", err)
	}

	// author_id vazio
	if _, err := CreateMessage(testCtx(), channel.ID, "", "conteúdo", "", nil); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para author_id vazio, obtive %v", err)
	}

	// content acima do limite (8192 caracteres)
	if _, err := CreateMessage(testCtx(), channel.ID, owner.ID, strings.Repeat("a", 8193), "", nil); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para content acima do limite, obtive %v", err)
	}

	// content vazio sem attachments
	if _, err := CreateMessage(testCtx(), channel.ID, owner.ID, "", "", nil); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para content vazio sem attachments, obtive %v", err)
	}

	// nome de attachment vazio
	if _, err := CreateMessage(testCtx(), channel.ID, owner.ID, "", "", []AttachmentInput{{OriginalFileName: "", Content: strings.NewReader("x")}}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para nome de attachment vazio, obtive %v", err)
	}

	// nome de attachment acima do limite (128 caracteres)
	if _, err := CreateMessage(testCtx(), channel.ID, owner.ID, "", "", []AttachmentInput{{OriginalFileName: strings.Repeat("a", 129) + ".txt", Content: strings.NewReader("x")}}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para nome de attachment acima do limite, obtive %v", err)
	}
}

func TestCreateMessageBoundary(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)

	// content exatamente no limite (8192 caracteres): aceito
	if _, err := CreateMessage(testCtx(), channel.ID, owner.ID, strings.Repeat("a", 8192), "", nil); err != nil {
		t.Errorf("CreateMessage com content no limite retornou erro: %v", err)
	}

	// nome de attachment exatamente no limite (128 caracteres): aceito
	name := strings.Repeat("a", 127) + "b"
	if _, err := CreateMessage(testCtx(), channel.ID, owner.ID, "", "", []AttachmentInput{{OriginalFileName: name, Content: strings.NewReader("conteúdo")}}); err != nil {
		t.Errorf("CreateMessage com nome de attachment no limite retornou erro: %v", err)
	}
}

func TestCreateMessageReplyTo(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)

	target, err := CreateMessage(testCtx(), channel.ID, owner.ID, "mensagem alvo", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem alvo: %v", err)
	}

	reply, err := CreateMessage(testCtx(), channel.ID, owner.ID, "resposta", target.ID, nil)
	if err != nil {
		t.Fatalf("CreateMessage com reply_to retornou erro: %v", err)
	}
	if reply.ReplyTo == nil || *reply.ReplyTo != target.ID {
		t.Errorf("esperava reply_to %s, obtive %v", target.ID, reply.ReplyTo)
	}

	stored, err := storage.GetMessageByID(testCtx(), reply.ID)
	if err != nil {
		t.Fatalf("GetMessageByID retornou erro: %v", err)
	}
	if stored.ReplyTo == nil || *stored.ReplyTo != target.ID {
		t.Errorf("esperava reply_to %s no banco, obtive %v", target.ID, stored.ReplyTo)
	}
}

func TestCreateMessageReplyToNotFound(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)

	if _, err := CreateMessage(testCtx(), channel.ID, owner.ID, "resposta", randUUID(), nil); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("esperava ErrMessageNotFound para reply_to inexistente, obtive %v", err)
	}
}

func TestCreateMessageReplyToDifferentChannel(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	_, err := CreateServer(testCtx(), newRandomServerName(), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channelA, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal A: %v", err)
	}
	channelB, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal B: %v", err)
	}

	target, err := CreateMessage(testCtx(), channelA.ID, owner.ID, "mensagem no canal A", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem no canal A: %v", err)
	}

	if _, err := CreateMessage(testCtx(), channelB.ID, owner.ID, "resposta no canal B", target.ID, nil); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para reply_to de outro canal, obtive %v", err)
	}
}

func TestCreateMessageReplyToDangling(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)

	target, err := CreateMessage(testCtx(), channel.ID, owner.ID, "mensagem a ser apagada", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem alvo: %v", err)
	}
	reply, err := CreateMessage(testCtx(), channel.ID, owner.ID, "resposta", target.ID, nil)
	if err != nil {
		t.Fatalf("falha ao criar resposta: %v", err)
	}

	// apagar a mensagem referenciada não remove a resposta nem limpa o reply_to
	if _, err := DeleteMessage(testCtx(), target.ID, owner.ID); err != nil {
		t.Fatalf("falha ao apagar mensagem alvo: %v", err)
	}

	stored, err := storage.GetMessageByID(testCtx(), reply.ID)
	if err != nil {
		t.Fatalf("GetMessageByID retornou erro: %v", err)
	}
	if stored.ReplyTo == nil || *stored.ReplyTo != target.ID {
		t.Errorf("esperava reply_to pendente %s, obtive %v", target.ID, stored.ReplyTo)
	}
}

func TestCreateMessageMaxAttachments(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)

	inputs := func(n int) []AttachmentInput {
		list := make([]AttachmentInput, 0, n)
		for i := 0; i < n; i++ {
			list = append(list, AttachmentInput{
				OriginalFileName: fmt.Sprintf("arquivo-%d.txt", i),
				Content:          strings.NewReader(fmt.Sprintf("conteúdo %d", i)),
			})
		}
		return list
	}

	// exatamente no limite (10 attachments): aceito
	message, err := CreateMessage(testCtx(), channel.ID, owner.ID, "no limite", "", inputs(10))
	if err != nil {
		t.Fatalf("CreateMessage com 10 attachments retornou erro: %v", err)
	}
	if len(message.Attachments) != 10 {
		t.Errorf("esperava 10 attachments, obtive %d", len(message.Attachments))
	}

	// acima do limite (11 attachments): ErrTooManyAttachments
	if _, err := CreateMessage(testCtx(), channel.ID, owner.ID, "acima do limite", "", inputs(11)); !errors.Is(err, ErrTooManyAttachments) {
		t.Errorf("esperava ErrTooManyAttachments para 11 attachments, obtive %v", err)
	}
}

func TestCreateMessageWithAttachments(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	writer := newTestMessageUser(t)
	noAttachment := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	grantChannelPermission(t, channel, writer, models.ChannelPermission{SendMessages: true})
	grantChannelPermission(t, channel, noAttachment, models.ChannelPermission{SendMessages: true})

	// dá send_attachment apenas para o writer
	role, err := storage.CreateRole(testCtx(), newRandomRoleName(), nil, models.RolePermissions{SendAttachment: true})
	if err != nil {
		t.Fatalf("falha ao criar role de apoio: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), writer.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role ao usuário: %v", err)
	}

	// dono: pode enviar com attachments
	message, err := CreateMessage(testCtx(), channel.ID, owner.ID, "com arquivo", "", []AttachmentInput{
		{OriginalFileName: "documento.txt", Content: strings.NewReader("conteúdo do documento")},
	})
	if err != nil {
		t.Fatalf("CreateMessage do dono com attachment retornou erro: %v", err)
	}
	if len(message.Attachments) != 1 {
		t.Fatalf("esperava 1 attachment, obtive %d", len(message.Attachments))
	}
	attachment := message.Attachments[0]
	if attachment.OriginalFileName != "documento.txt" {
		t.Errorf("esperava original_file_name %q, obtive %q", "documento.txt", attachment.OriginalFileName)
	}
	if attachment.MimeType != "text/plain; charset=utf-8" {
		t.Errorf("esperava mime_type %q, obtive %q", "text/plain; charset=utf-8", attachment.MimeType)
	}
	if attachment.SizeBytes != int64(len("conteúdo do documento")) {
		t.Errorf("esperava size_bytes %d, obtive %d", len("conteúdo do documento"), attachment.SizeBytes)
	}

	// o attachment foi vinculado à mensagem no banco
	stored, err := storage.ListAttachmentsByMessage(testCtx(), message.ID)
	if err != nil {
		t.Fatalf("ListAttachmentsByMessage retornou erro: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("esperava 1 attachment vinculado, obtive %d", len(stored))
	}
	if stored[0].MessagesID == nil || *stored[0].MessagesID != message.ID {
		t.Errorf("esperava messages_id %s, obtive %v", message.ID, stored[0].MessagesID)
	}

	// o blob foi gravado no content-addressable storage (caminho derivado do hash)
	hash := stored[0].MediaShaHash
	if _, err := os.Stat(filepath.Join("media", hash[:2], hash[2:4], hash)); err != nil {
		t.Errorf("blob do attachment não encontrado em disco: %v", err)
	}

	// writer com send_messages + send_attachment: pode enviar
	if _, err := CreateMessage(testCtx(), channel.ID, writer.ID, "com arquivo do writer", "", []AttachmentInput{
		{OriginalFileName: "outro.txt", Content: strings.NewReader("outro conteúdo")},
	}); err != nil {
		t.Errorf("CreateMessage do writer com attachment retornou erro: %v", err)
	}

	// noAttachment com send_messages mas sem send_attachment: negado
	if _, err := CreateMessage(testCtx(), channel.ID, noAttachment.ID, "com arquivo negado", "", []AttachmentInput{
		{OriginalFileName: "negado.txt", Content: strings.NewReader("não deve passar")},
	}); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("esperava ErrPermissionDenied para usuário sem send_attachment, obtive %v", err)
	}
}

func TestCreateMessageAttachmentSanitization(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)

	// nome com componentes de caminho é sanitizado para o último segmento
	message, err := CreateMessage(testCtx(), channel.ID, owner.ID, "sanitizado", "", []AttachmentInput{
		{OriginalFileName: "campos/sub/pasta/arquivo.txt", Content: strings.NewReader("conteúdo")},
	})
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}
	if len(message.Attachments) != 1 {
		t.Fatalf("esperava 1 attachment, obtive %d", len(message.Attachments))
	}
	if message.Attachments[0].OriginalFileName != "arquivo.txt" {
		t.Errorf("esperava nome sanitizado %q, obtive %q", "arquivo.txt", message.Attachments[0].OriginalFileName)
	}
}

func TestCreateMessageAttachmentDeduplication(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)

	content := "conteúdo duplicado"
	input := func() AttachmentInput {
		return AttachmentInput{OriginalFileName: "dup.txt", Content: strings.NewReader(content)}
	}

	first, err := CreateMessage(testCtx(), channel.ID, owner.ID, "primeira", "", []AttachmentInput{input()})
	if err != nil {
		t.Fatalf("CreateMessage da primeira mensagem retornou erro: %v", err)
	}
	second, err := CreateMessage(testCtx(), channel.ID, owner.ID, "segunda", "", []AttachmentInput{input()})
	if err != nil {
		t.Fatalf("CreateMessage da segunda mensagem retornou erro: %v", err)
	}

	if len(first.Attachments) != 1 || len(second.Attachments) != 1 {
		t.Fatalf("esperava 1 attachment em cada mensagem, obtive %d e %d", len(first.Attachments), len(second.Attachments))
	}
	// mesmo conteúdo: mesmo hash e mesmo caminho de blob
	if first.Attachments[0].ID == second.Attachments[0].ID {
		t.Fatal("esperava ids de attachment distintos")
	}
	firstStored, err := storage.GetAttachmentByID(testCtx(), first.Attachments[0].ID)
	if err != nil {
		t.Fatalf("GetAttachmentByID da primeira retornou erro: %v", err)
	}
	secondStored, err := storage.GetAttachmentByID(testCtx(), second.Attachments[0].ID)
	if err != nil {
		t.Fatalf("GetAttachmentByID da segunda retornou erro: %v", err)
	}
	if firstStored.MediaShaHash != secondStored.MediaShaHash {
		t.Errorf("esperava o mesmo media_sha_hash para o mesmo conteúdo")
	}
	// o blob é único em disco (caminho derivado do hash)
	hash := firstStored.MediaShaHash
	wantPath := filepath.Join("media", hash[:2], hash[2:4], hash)
	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("blob do attachment não encontrado em disco (%s): %v", wantPath, err)
	}
}

// --- EditMessage ---

func TestEditMessage(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	other := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)

	message, err := CreateMessage(testCtx(), channel.ID, owner.ID, "original", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}

	// autor: pode editar
	edited, err := EditMessage(testCtx(), message.ID, owner.ID, "editado")
	if err != nil {
		t.Fatalf("EditMessage do autor retornou erro: %v", err)
	}
	if edited.Content == nil || *edited.Content != "editado" {
		t.Errorf("esperava content %q, obtive %v", "editado", edited.Content)
	}
	if edited.EditedAt == nil {
		t.Error("esperava edited_at preenchido após a edição")
	}

	// não autor: negado
	if _, err := EditMessage(testCtx(), message.ID, other.ID, "invadido"); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("esperava ErrPermissionDenied para não autor, obtive %v", err)
	}
}

func TestEditMessageClearsContent(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)

	message, err := CreateMessage(testCtx(), channel.ID, owner.ID, "vai ser limpo", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}

	// content vazio limpa o texto da mensagem (NULL)
	edited, err := EditMessage(testCtx(), message.ID, owner.ID, "")
	if err != nil {
		t.Fatalf("EditMessage com content vazio retornou erro: %v", err)
	}
	if edited.Content != nil {
		t.Errorf("esperava content NULL após limpar, obtive %q", *edited.Content)
	}
}

func TestEditMessageInvalidInput(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := CreateMessage(testCtx(), channel.ID, owner.ID, "conteúdo", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}

	// message_id vazio
	if _, err := EditMessage(testCtx(), "", owner.ID, "x"); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("esperava ErrMessageNotFound para message_id vazio, obtive %v", err)
	}

	// mensagem inexistente
	if _, err := EditMessage(testCtx(), randUUID(), owner.ID, "x"); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("esperava ErrMessageNotFound para mensagem inexistente, obtive %v", err)
	}

	// author_id vazio
	if _, err := EditMessage(testCtx(), message.ID, "", "x"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para author_id vazio, obtive %v", err)
	}

	// content acima do limite
	if _, err := EditMessage(testCtx(), message.ID, owner.ID, strings.Repeat("a", 8193)); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para content acima do limite, obtive %v", err)
	}
}

// --- DeleteMessage ---

func TestDeleteMessage(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)

	message, err := CreateMessage(testCtx(), channel.ID, owner.ID, "vai ser apagada", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}

	// autor: pode excluir
	if _, err := DeleteMessage(testCtx(), message.ID, owner.ID); err != nil {
		t.Fatalf("DeleteMessage do autor retornou erro: %v", err)
	}
	if _, err := storage.GetMessageByID(testCtx(), message.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound após a exclusão, obtive %v", err)
	}
}

func TestDeleteMessageByModerator(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	moderator := newTestMessageUser(t)
	stranger := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	grantChannelPermission(t, channel, moderator, models.ChannelPermission{DeleteMessages: true})

	message, err := CreateMessage(testCtx(), channel.ID, owner.ID, "mensagem do dono", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}

	// usuário com delete_messages: pode excluir mensagem de outro autor
	if _, err := DeleteMessage(testCtx(), message.ID, moderator.ID); err != nil {
		t.Fatalf("DeleteMessage do moderador retornou erro: %v", err)
	}
	if _, err := storage.GetMessageByID(testCtx(), message.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound após a exclusão, obtive %v", err)
	}

	// usuário sem permissão: negado
	other, err := CreateMessage(testCtx(), channel.ID, owner.ID, "outra mensagem", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}
	if _, err := DeleteMessage(testCtx(), other.ID, stranger.ID); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("esperava ErrPermissionDenied para usuário sem delete_messages, obtive %v", err)
	}
}

func TestDeleteMessageInvalidInput(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := CreateMessage(testCtx(), channel.ID, owner.ID, "conteúdo", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}

	// message_id vazio
	if _, err := DeleteMessage(testCtx(), "", owner.ID); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("esperava ErrMessageNotFound para message_id vazio, obtive %v", err)
	}

	// mensagem inexistente
	if _, err := DeleteMessage(testCtx(), randUUID(), owner.ID); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("esperava ErrMessageNotFound para mensagem inexistente, obtive %v", err)
	}

	// author_id vazio
	if _, err := DeleteMessage(testCtx(), message.ID, ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para author_id vazio, obtive %v", err)
	}
}

// --- UploadAttachment ---

func TestUploadAttachment(t *testing.T) {
	cfg := config.LoadConfig()
	user := newTestMessageUser(t)

	attachment, err := UploadAttachment(testCtx(), "documento.txt", strings.NewReader("conteúdo do documento"), user.ID)
	if err != nil {
		t.Fatalf("UploadAttachment retornou erro: %v", err)
	}
	if attachment.ID == "" {
		t.Error("esperava id preenchido")
	}
	if attachment.OriginalFileName != "documento.txt" {
		t.Errorf("esperava original_file_name %q, obtive %q", "documento.txt", attachment.OriginalFileName)
	}
	if attachment.MimeType != "text/plain; charset=utf-8" {
		t.Errorf("esperava mime_type %q, obtive %q", "text/plain; charset=utf-8", attachment.MimeType)
	}
	if attachment.SizeBytes != int64(len("conteúdo do documento")) {
		t.Errorf("esperava size_bytes %d, obtive %d", len("conteúdo do documento"), attachment.SizeBytes)
	}
	if attachment.CreatedBy == nil || *attachment.CreatedBy != user.ID {
		t.Errorf("esperava created_by %s, obtive %v", user.ID, attachment.CreatedBy)
	}
	if attachment.MessagesID != nil {
		t.Errorf("esperava messages_id nil para upload avulso, obtive %v", attachment.MessagesID)
	}
	// o media_sha_hash é o sha256 do conteúdo

	mac := hmac.New(sha256.New, []byte(cfg.HMACSecret))
	mac.Write([]byte("conteúdo do documento"))
	expectedHash := hex.EncodeToString(mac.Sum(nil))

	if attachment.MediaShaHash != expectedHash {
		t.Errorf("esperava media_sha_hash %q, obtive %q",
			expectedHash,
			attachment.MediaShaHash,
		)
	}

	// o blob foi gravado no content-addressable storage
	if _, err := os.Stat(attachment.FilePath); err != nil {
		t.Errorf("blob do attachment não encontrado em disco: %v", err)
	}
}

func TestUploadAttachmentMimeDetection(t *testing.T) {
	user := newTestMessageUser(t)

	cases := []struct {
		name     string
		content  []byte
		expected string
	}{
		{"png", []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00}, "image/png"},
		{"gif", []byte{'G', 'I', 'F', '8', '9', 'a', 0x00, 0x00}, "image/gif"},
		{"texto", []byte("apenas texto puro"), "text/plain; charset=utf-8"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attachment, err := UploadAttachment(testCtx(), "arquivo_"+randHex(4), bytes.NewReader(tc.content), user.ID)
			if err != nil {
				t.Fatalf("UploadAttachment retornou erro: %v", err)
			}
			if attachment.MimeType != tc.expected {
				t.Errorf("esperava mime_type %q, obtive %q", tc.expected, attachment.MimeType)
			}
		})
	}
}

func TestUploadAttachmentInvalidInput(t *testing.T) {
	user := newTestMessageUser(t)

	// nome vazio
	if _, err := UploadAttachment(testCtx(), "", strings.NewReader("x"), user.ID); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para nome vazio, obtive %v", err)
	}

	// nome apenas com separadores de caminho (sanitização resultou vazio)
	if _, err := UploadAttachment(testCtx(), "///", strings.NewReader("x"), user.ID); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para nome com apenas separadores, obtive %v", err)
	}

	// nome acima do limite (128 caracteres)
	if _, err := UploadAttachment(testCtx(), strings.Repeat("a", 129), strings.NewReader("x"), user.ID); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para nome acima do limite, obtive %v", err)
	}
}

func TestUploadAttachmentTooLarge(t *testing.T) {
	user := newTestMessageUser(t)

	// conteúdo acima de 100MB: recusado
	reader := &largeReader{total: 100*1024*1024 + 1}
	if _, err := UploadAttachment(testCtx(), "grande.bin", reader, user.ID); !errors.Is(err, ErrAttachmentTooLarge) {
		t.Errorf("esperava ErrAttachmentTooLarge para arquivo acima de 100MB, obtive %v", err)
	}
}

// largeReader fornece um número total de bytes em blocos de 1MiB, sem
// alocar o conteúdo em memória.
type largeReader struct {
	total  int64
	served int64
	buf    []byte
}

func (r *largeReader) Read(p []byte) (int, error) {
	if r.served >= r.total {
		return 0, io.EOF
	}
	if r.buf == nil {
		r.buf = make([]byte, 1024*1024)
	}
	remaining := r.total - r.served
	chunk := int64(len(r.buf))
	if chunk > remaining {
		chunk = remaining
	}
	if chunk > int64(len(p)) {
		chunk = int64(len(p))
	}
	copy(p, r.buf[:chunk])
	r.served += chunk
	return int(chunk), nil
}

// TestDownloadAttachment garante que o download do attachment aplica a regra
// de read_channel: em canal aberto (sem roles com permissões definidas)
// qualquer usuário pode baixar; em canal fechado apenas o dono do servidor e
// quem tem read_channel; e que os erros de entrada são mapeados.
func TestDownloadAttachment(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	reader := newTestMessageUser(t)
	stranger := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)

	message, err := CreateMessage(testCtx(), channel.ID, owner.ID, "com anexo", "", []AttachmentInput{
		{OriginalFileName: "documento.txt", Content: strings.NewReader("conteúdo do documento")},
	})
	if err != nil {
		t.Fatalf("CreateMessage com attachment retornou erro: %v", err)
	}
	if len(message.Attachments) != 1 {
		t.Fatalf("esperava 1 attachment, obtive %d", len(message.Attachments))
	}
	attachment := message.Attachments[0]

	// canal aberto (nenhuma role com permissões definidas): qualquer usuário baixa
	downloaded, err := DownloadAttachment(testCtx(), attachment.ID, stranger.ID)
	if err != nil {
		t.Fatalf("DownloadAttachment em canal aberto retornou erro: %v", err)
	}
	if downloaded.ID != attachment.ID {
		t.Errorf("esperava attachment %s, obtive %s", attachment.ID, downloaded.ID)
	}
	if downloaded.OriginalFileName != "documento.txt" {
		t.Errorf("esperava original_file_name %q, obtive %q", "documento.txt", downloaded.OriginalFileName)
	}
	if downloaded.MimeType != "text/plain; charset=utf-8" {
		t.Errorf("esperava mime_type %q, obtive %q", "text/plain; charset=utf-8", downloaded.MimeType)
	}
	if downloaded.SizeBytes != int64(len("conteúdo do documento")) {
		t.Errorf("esperava size_bytes %d, obtive %d", len("conteúdo do documento"), downloaded.SizeBytes)
	}
	if downloaded.MessagesID == nil || *downloaded.MessagesID != message.ID {
		t.Errorf("esperava messages_id %s, obtive %v", message.ID, downloaded.MessagesID)
	}
	if _, err := os.Stat(downloaded.FilePath); err != nil {
		t.Errorf("blob do attachment não encontrado em disco: %v", err)
	}

	// fecha o canal: apenas read_channel pode baixar
	grantChannelPermission(t, channel, reader, models.ChannelPermission{ReadChannel: true})

	// dono do servidor: sempre pode baixar
	if _, err := DownloadAttachment(testCtx(), attachment.ID, owner.ID); err != nil {
		t.Errorf("DownloadAttachment do dono retornou erro: %v", err)
	}

	// usuário com read_channel: pode baixar
	if _, err := DownloadAttachment(testCtx(), attachment.ID, reader.ID); err != nil {
		t.Errorf("DownloadAttachment do leitor retornou erro: %v", err)
	}

	// usuário sem permissão em canal fechado: negado
	if _, err := DownloadAttachment(testCtx(), attachment.ID, stranger.ID); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("esperava ErrPermissionDenied para usuário sem permissão, obtive %v", err)
	}

	// attachment inexistente
	if _, err := DownloadAttachment(testCtx(), randUUID(), owner.ID); !errors.Is(err, ErrAttachmentNotFound) {
		t.Errorf("esperava ErrAttachmentNotFound para attachment inexistente, obtive %v", err)
	}

	// file_id vazio
	if _, err := DownloadAttachment(testCtx(), "", owner.ID); !errors.Is(err, ErrAttachmentNotFound) {
		t.Errorf("esperava ErrAttachmentNotFound para file_id vazio, obtive %v", err)
	}

	// user_id vazio
	if _, err := DownloadAttachment(testCtx(), attachment.ID, ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para user_id vazio, obtive %v", err)
	}
}

// TestDownloadAttachmentOrphan garante que um attachment não vinculado a uma
// mensagem (messages_id NULL, upload avulso) não é exposto pela API de
// download e retorna ErrAttachmentNotFound.
func TestDownloadAttachmentOrphan(t *testing.T) {
	user := newTestMessageUser(t)

	attachment, err := UploadAttachment(testCtx(), "orfa.txt", strings.NewReader("conteúdo órfão"), user.ID)
	if err != nil {
		t.Fatalf("UploadAttachment retornou erro: %v", err)
	}
	if attachment.MessagesID != nil {
		t.Fatalf("esperava messages_id nil para upload avulso, obtive %v", attachment.MessagesID)
	}

	if _, err := DownloadAttachment(testCtx(), attachment.ID, user.ID); !errors.Is(err, ErrAttachmentNotFound) {
		t.Errorf("esperava ErrAttachmentNotFound para attachment órfão, obtive %v", err)
	}
}

// --- services (attachments) fim ---

// --- Emojis (tarefa 7.4) ---

func TestListEmojisPagination(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	_ = newTestMessageChannel(t, &owner.ID)

	const total = 26
	for i := 0; i < total; i++ {
		if _, err := storage.CreateEmoji(testCtx(), "emoji_"+randHex(8), newTestMediaHash(t, []byte{1}), &owner.ID); err != nil {
			t.Fatalf("falha ao criar emoji %d: %v", i, err)
		}
	}

	// primeira página: 25 emojis (limite) e has_more
	list, err := ListEmojis(testCtx(), nil, "")
	if err != nil {
		t.Fatalf("ListEmojis retornou erro: %v", err)
	}
	if len(list.Emojis) != 25 {
		t.Fatalf("esperava 25 emojis na primeira página, obtive %d", len(list.Emojis))
	}
	if !list.HasMore {
		t.Error("esperava has_more true na primeira página")
	}
	// segunda página via cursor (created_at, id): retorna o emoji restante
	last := list.Emojis[24]
	page2, err := ListEmojis(testCtx(), &last.CreatedAt, last.ID)
	if err != nil {
		t.Fatalf("ListEmojis da segunda página retornou erro: %v", err)
	}
	if len(page2.Emojis) != 1 {
		t.Fatalf("esperava 1 emoji na segunda página, obtive %d", len(page2.Emojis))
	}
	if page2.Emojis[0].ID == last.ID {
		t.Error("segunda página não deve repetir o último emoji da primeira")
	}
	if page2.HasMore {
		t.Error("esperava has_more false na segunda página")
	}
}

func TestCreateEmoji(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	_ = newTestMessageChannel(t, &owner.ID)
	png := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))

	emoji, err := CreateEmoji(testCtx(), "emoji_"+randHex(8), "png", png, owner.ID)
	if err != nil {
		t.Fatalf("CreateEmoji retornou erro: %v", err)
	}

	if emoji.Format != "PNG" {
		t.Errorf("esperava format normalizado para PNG, obtive %q", emoji.Format)
	}
	if emoji.CreatedBy == nil || *emoji.CreatedBy != owner.ID {
		t.Errorf("esperava created_by %s, obtive %v", owner.ID, emoji.CreatedBy)
	}
	if string(emoji.ImageBlob) != string(pngAvatarBytes(100, 100)) {
		t.Error("image_blob retornado não corresponde ao conteúdo enviado")
	}

	webp := base64.StdEncoding.EncodeToString(webpAvatarBytes(100, 100))
	emojiWebP, err := CreateEmoji(testCtx(), "emoji_"+randHex(8), "webp", webp, owner.ID)
	if err != nil {
		t.Fatalf("CreateEmoji (webp) retornou erro: %v", err)
	}
	if emojiWebP.Format != "WEBP" {
		t.Errorf("esperava format normalizado para WEBP, obtive %q", emojiWebP.Format)
	}
	if string(emojiWebP.ImageBlob) != string(webpAvatarBytes(100, 100)) {
		t.Error("image_blob (webp) retornado não corresponde ao conteúdo enviado")
	}

	jpg := base64.StdEncoding.EncodeToString(jpegAvatarBytes(100, 100))
	emojiJPG, err := CreateEmoji(testCtx(), "emoji_"+randHex(8), "jpg", jpg, owner.ID)
	if err != nil {
		t.Fatalf("CreateEmoji (jpg) retornou erro: %v", err)
	}
	if emojiJPG.Format != "JPEG" {
		t.Errorf("esperava format normalizado para JPEG, obtive %q", emojiJPG.Format)
	}
	if string(emojiJPG.ImageBlob) != string(jpegAvatarBytes(100, 100)) {
		t.Error("image_blob (jpg) retornado não corresponde ao conteúdo enviado")
	}
}

func TestCreateEmojiInvalidInput(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	_ = newTestMessageChannel(t, &owner.ID)
	png := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	gif := base64.StdEncoding.EncodeToString(gifAvatarBytes(100, 100))
	name := "emoji_" + randHex(8)

	cases := []struct {
		desc   string
		name   string
		format string
		blob   string
		userID string
	}{
		{"name vazio", "", "PNG", png, owner.ID},
		{"format vazio", name, "", png, owner.ID},
		{"image_blob vazio", name, "PNG", "", owner.ID},
		{"created_by vazio", name, "PNG", png, ""},
		{"name acima de 32 caracteres", strings.Repeat("a", 33), "PNG", png, owner.ID},
		{"format inválido", name, "SVG", png, owner.ID},
		{"base64 inválido", name, "PNG", "!!!", owner.ID},
		{"conteúdo não corresponde ao formato", name, "PNG", gif, owner.ID},
		{"emoji acima de 256kb", name, "PNG", base64.StdEncoding.EncodeToString(append(pngAvatarBytes(100, 100), make([]byte, 257*1024)...)), owner.ID},
	}
	for _, tc := range cases {
		if _, err := CreateEmoji(testCtx(), tc.name, tc.format, tc.blob, tc.userID); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: esperava ErrInvalidInput, obtive %v", tc.desc, err)
		}
	}
}

func TestCreateEmojiNameTaken(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	_ = newTestMessageChannel(t, &owner.ID)
	png := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	name := "emoji_" + randHex(8)

	if _, err := CreateEmoji(testCtx(), name, "PNG", png, owner.ID); err != nil {
		t.Fatalf("falha ao criar primeiro emoji: %v", err)
	}
	if _, err := CreateEmoji(testCtx(), name, "PNG", png, owner.ID); !errors.Is(err, ErrEmojiNameTaken) {
		t.Errorf("esperava ErrEmojiNameTaken, obtive %v", err)
	}
}

func TestCreateEmojiLimitReached(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	_ = newTestMessageChannel(t, &owner.ID)

	// preenche o servidor até o limite (500)
	for i := 0; i < 500; i++ {
		if _, err := storage.CreateEmoji(testCtx(), "emoji_"+randHex(8), newTestMediaHash(t, []byte{1}), &owner.ID); err != nil {
			t.Fatalf("falha ao criar emoji %d: %v", i, err)
		}
	}

	png := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	if _, err := CreateEmoji(testCtx(), "emoji_"+randHex(8), "PNG", png, owner.ID); !errors.Is(err, ErrEmojiLimitReached) {
		t.Errorf("esperava ErrEmojiLimitReached, obtive %v", err)
	}
}

func TestDeleteEmoji(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	author := newTestMessageUser(t)
	stranger := newTestMessageUser(t)
	_ = newTestMessageChannel(t, &owner.ID)

	emoji, err := storage.CreateEmoji(testCtx(), "emoji_"+randHex(8), newTestMediaHash(t, []byte{1}), &author.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}

	// usuário sem permissão: negado
	if err := DeleteEmoji(testCtx(), emoji.ID, stranger.ID); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("esperava ErrPermissionDenied para usuário sem permissão, obtive %v", err)
	}

	// autor: pode excluir
	if err := DeleteEmoji(testCtx(), emoji.ID, author.ID); err != nil {
		t.Fatalf("DeleteEmoji do autor retornou erro: %v", err)
	}
	if _, err := storage.GetEmojiByID(testCtx(), emoji.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound após a exclusão, obtive %v", err)
	}
}

func TestDeleteEmojiByServerOwner(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	author := newTestMessageUser(t)
	_ = newTestMessageChannel(t, &owner.ID)

	emoji, err := storage.CreateEmoji(testCtx(), "emoji_"+randHex(8), newTestMediaHash(t, []byte{1}), &author.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}

	if err := DeleteEmoji(testCtx(), emoji.ID, owner.ID); err != nil {
		t.Fatalf("DeleteEmoji do dono retornou erro: %v", err)
	}
	if _, err := storage.GetEmojiByID(testCtx(), emoji.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound após a exclusão, obtive %v", err)
	}
}

func TestDeleteEmojiByManageServerRole(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	author := newTestMessageUser(t)
	mod := newTestMessageUser(t)
	_ = newTestMessageChannel(t, &owner.ID)

	role, err := storage.CreateRole(testCtx(), newRandomRoleName(), nil, models.RolePermissions{ManageServer: true})
	if err != nil {
		t.Fatalf("falha ao criar role de apoio: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), mod.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role ao usuário: %v", err)
	}

	emoji, err := storage.CreateEmoji(testCtx(), "emoji_"+randHex(8), newTestMediaHash(t, []byte{1}), &author.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}

	if err := DeleteEmoji(testCtx(), emoji.ID, mod.ID); err != nil {
		t.Fatalf("DeleteEmoji do usuário com manage_server retornou erro: %v", err)
	}
	if _, err := storage.GetEmojiByID(testCtx(), emoji.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound após a exclusão, obtive %v", err)
	}
}

func TestDeleteEmojiInvalidInput(t *testing.T) {
	owner := newTestMessageUser(t)

	if err := DeleteEmoji(testCtx(), "", owner.ID); !errors.Is(err, ErrEmojiNotFound) {
		t.Errorf("esperava ErrEmojiNotFound para emoji_id vazio, obtive %v", err)
	}
	if err := DeleteEmoji(testCtx(), randUUID(), ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput para user_id vazio, obtive %v", err)
	}
}

func TestDeleteEmojiNotFound(t *testing.T) {
	owner := newTestMessageUser(t)

	if err := DeleteEmoji(testCtx(), randUUID(), owner.ID); !errors.Is(err, ErrEmojiNotFound) {
		t.Errorf("esperava ErrEmojiNotFound para emoji inexistente, obtive %v", err)
	}
}

// --- SearchMessages ---

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
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	reader := newTestMessageUser(t)
	stranger := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	restricted, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal restrito de apoio: %v", err)
	}
	grantChannelPermission(t, restricted, reader, models.ChannelPermission{ReadChannel: true})

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
	m4, err := CreateMessage(testCtx(), channel.ID, owner.ID, "peixe", "", []AttachmentInput{
		{OriginalFileName: "peixe.txt", Content: strings.NewReader("conteúdo do peixe")},
	})
	if err != nil {
		t.Fatalf("falha ao criar mensagem 4 com attachment: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	m5, err := CreateMessage(testCtx(), restricted.ID, owner.ID, "zebra secreta", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem 5 no canal restrito: %v", err)
	}

	dateStart := m1.CreatedAt.UTC().Format("2006-01-02")
	dateEnd := m5.CreatedAt.UTC().Format("2006-01-02")
	yesterday := m1.CreatedAt.UTC().AddDate(0, 0, -1).Format("2006-01-02")
	withAttachment := true
	withoutAttachment := false

	t.Run("apenas texto", func(t *testing.T) {
		res, err := SearchMessages(testCtx(), models.SearchRequest{Text: "zebra"}, nil, "", owner.ID)
		if err != nil {
			t.Fatalf("SearchMessages com texto retornou erro: %v", err)
		}
		assertSearchSet(t, res.Results, m1.ID, m5.ID)
		for _, r := range res.Results {
			if r.Type != "message" {
				t.Errorf("esperava type message, obtive %q", r.Type)
			}
			if r.Score == nil {
				t.Error("esperava score preenchido em busca textual")
			}

		}
		byID := map[string]models.SearchResult{}
		for _, r := range res.Results {
			byID[r.ID] = r
		}
		if byID[m1.ID].ChannelName != channel.Name {
			t.Errorf("esperava channel_name %q, obtive %q", channel.Name, byID[m1.ID].ChannelName)
		}
		if byID[m1.ID].AuthorUsername == nil || *byID[m1.ID].AuthorUsername != owner.Username {
			t.Errorf("esperava author_username %q, obtive %v", owner.Username, byID[m1.ID].AuthorUsername)
		}

		res, err = SearchMessages(testCtx(), models.SearchRequest{Text: "borboleta"}, nil, "", owner.ID)
		if err != nil {
			t.Fatalf("SearchMessages com borboleta retornou erro: %v", err)
		}
		assertSearchSet(t, res.Results, m1.ID, m2.ID)
	})

	t.Run("apenas autor", func(t *testing.T) {
		res, err := SearchMessages(testCtx(), models.SearchRequest{Author: reader.ID}, nil, "", owner.ID)
		if err != nil {
			t.Fatalf("SearchMessages com autor retornou erro: %v", err)
		}
		assertSearchSet(t, res.Results, m2.ID)
		if res.Results[0].AuthorID == nil || *res.Results[0].AuthorID != reader.ID {
			t.Errorf("esperava author_id %s, obtive %v", reader.ID, res.Results[0].AuthorID)
		}
		if res.Results[0].Score != nil {
			t.Error("esperava score nil sem busca textual")
		}
	})

	t.Run("apenas intervalo de datas", func(t *testing.T) {
		res, err := SearchMessages(testCtx(), models.SearchRequest{DateStart: dateStart, DateEnd: dateEnd}, nil, "", owner.ID)
		if err != nil {
			t.Fatalf("SearchMessages com datas retornou erro: %v", err)
		}
		assertSearchSet(t, res.Results, m1.ID, m2.ID, m3.ID, m4.ID, m5.ID)

		res, err = SearchMessages(testCtx(), models.SearchRequest{DateEnd: yesterday}, nil, "", owner.ID)
		if err != nil {
			t.Fatalf("SearchMessages com date_end no passado retornou erro: %v", err)
		}
		if len(res.Results) != 0 {
			t.Errorf("esperava 0 resultados, obtive %d", len(res.Results))
		}
	})

	t.Run("apenas contains_attachment", func(t *testing.T) {
		res, err := SearchMessages(testCtx(), models.SearchRequest{ContainsAttachment: &withAttachment}, nil, "", owner.ID)
		if err != nil {
			t.Fatalf("SearchMessages com contains_attachment true retornou erro: %v", err)
		}
		assertSearchSet(t, res.Results, m4.ID)

		res, err = SearchMessages(testCtx(), models.SearchRequest{ContainsAttachment: &withoutAttachment}, nil, "", owner.ID)
		if err != nil {
			t.Fatalf("SearchMessages com contains_attachment false retornou erro: %v", err)
		}
		assertSearchSet(t, res.Results, m1.ID, m2.ID, m3.ID, m5.ID)
	})

	t.Run("texto + autor", func(t *testing.T) {
		res, err := SearchMessages(testCtx(), models.SearchRequest{Text: "borboleta", Author: owner.ID}, nil, "", owner.ID)
		if err != nil {
			t.Fatalf("SearchMessages com texto + autor retornou erro: %v", err)
		}
		assertSearchSet(t, res.Results, m1.ID)
	})

	t.Run("texto + intervalo de datas", func(t *testing.T) {
		res, err := SearchMessages(testCtx(), models.SearchRequest{Text: "zebra", DateStart: dateStart, DateEnd: dateEnd}, nil, "", owner.ID)
		if err != nil {
			t.Fatalf("SearchMessages com texto + datas retornou erro: %v", err)
		}
		assertSearchSet(t, res.Results, m1.ID, m5.ID)

		res, err = SearchMessages(testCtx(), models.SearchRequest{Text: "zebra", DateEnd: yesterday}, nil, "", owner.ID)
		if err != nil {
			t.Fatalf("SearchMessages com texto + date_end no passado retornou erro: %v", err)
		}
		if len(res.Results) != 0 {
			t.Errorf("esperava 0 resultados, obtive %d", len(res.Results))
		}
	})

	t.Run("autor + contains_attachment", func(t *testing.T) {
		res, err := SearchMessages(testCtx(), models.SearchRequest{Author: owner.ID, ContainsAttachment: &withAttachment}, nil, "", owner.ID)
		if err != nil {
			t.Fatalf("SearchMessages com autor + contains_attachment retornou erro: %v", err)
		}
		assertSearchSet(t, res.Results, m4.ID)
	})

	t.Run("todos os filtros combinados", func(t *testing.T) {
		res, err := SearchMessages(testCtx(), models.SearchRequest{
			Text:               "peixe",
			Author:             owner.ID,
			DateStart:          dateStart,
			DateEnd:            dateEnd,
			ContainsAttachment: &withAttachment,
		}, nil, "", owner.ID)
		if err != nil {
			t.Fatalf("SearchMessages com todos os filtros retornou erro: %v", err)
		}
		assertSearchSet(t, res.Results, m4.ID)
	})

	t.Run("ordem", func(t *testing.T) {
		res, err := SearchMessages(testCtx(), models.SearchRequest{Author: owner.ID, Order: "asc"}, nil, "", owner.ID)
		if err != nil {
			t.Fatalf("SearchMessages com order asc retornou erro: %v", err)
		}
		assertSearchOrder(t, res.Results, m1.ID, m4.ID, m5.ID)

		res, err = SearchMessages(testCtx(), models.SearchRequest{Author: owner.ID, Order: "desc"}, nil, "", owner.ID)
		if err != nil {
			t.Fatalf("SearchMessages com order desc retornou erro: %v", err)
		}
		assertSearchOrder(t, res.Results, m5.ID, m4.ID, m1.ID)

		// padrão (order ausente): desc
		res, err = SearchMessages(testCtx(), models.SearchRequest{Author: owner.ID}, nil, "", owner.ID)
		if err != nil {
			t.Fatalf("SearchMessages sem order retornou erro: %v", err)
		}
		assertSearchOrder(t, res.Results, m5.ID, m4.ID, m1.ID)
	})

	t.Run("autorização", func(t *testing.T) {
		// reader: lê o canal aberto e o restrito (read_channel via role)
		res, err := SearchMessages(testCtx(), models.SearchRequest{Text: "zebra"}, nil, "", reader.ID)
		if err != nil {
			t.Fatalf("SearchMessages do reader retornou erro: %v", err)
		}
		assertSearchSet(t, res.Results, m1.ID, m5.ID)

		// stranger: sem roles, não vê o canal restrito
		res, err = SearchMessages(testCtx(), models.SearchRequest{Text: "vagalume"}, nil, "", stranger.ID)
		if err != nil {
			t.Fatalf("SearchMessages do stranger retornou erro: %v", err)
		}
		assertSearchSet(t, res.Results, m2.ID, m3.ID)

		res, err = SearchMessages(testCtx(), models.SearchRequest{Text: "secreta"}, nil, "", stranger.ID)
		if err != nil {
			t.Fatalf("SearchMessages do stranger no restrito retornou erro: %v", err)
		}
		if len(res.Results) != 0 {
			t.Errorf("esperava 0 resultados para o stranger, obtive %d", len(res.Results))
		}

		// owner: vê tudo
		res, err = SearchMessages(testCtx(), models.SearchRequest{Text: "secreta"}, nil, "", owner.ID)
		if err != nil {
			t.Fatalf("SearchMessages do owner retornou erro: %v", err)
		}
		assertSearchSet(t, res.Results, m5.ID)
	})

	t.Run("paginação", func(t *testing.T) {
		for i := 0; i < 101; i++ {
			if _, err := CreateMessage(testCtx(), channel.ID, owner.ID, "pagina "+randHex(2), "", nil); err != nil {
				t.Fatalf("falha ao criar mensagem de paginação %d: %v", i, err)
			}
		}

		page1, err := SearchMessages(testCtx(), models.SearchRequest{Text: "pagina"}, nil, "", owner.ID)
		if err != nil {
			t.Fatalf("SearchMessages da primeira página retornou erro: %v", err)
		}
		if !page1.HasMore {
			t.Error("esperava has_more true na primeira página")
		}
		if len(page1.Results) != 100 {
			t.Fatalf("esperava 100 resultados, obtive %d", len(page1.Results))
		}

		last := page1.Results[len(page1.Results)-1]
		page2, err := SearchMessages(testCtx(), models.SearchRequest{Text: "pagina"}, timePtr(last.CreatedAt), last.ID, owner.ID)
		if err != nil {
			t.Fatalf("SearchMessages da segunda página retornou erro: %v", err)
		}
		if page2.HasMore {
			t.Error("esperava has_more false na segunda página")
		}
		if len(page2.Results) != 1 {
			t.Fatalf("esperava 1 resultado na segunda página, obtive %d", len(page2.Results))
		}

		// sem duplicatas entre as páginas
		seen := map[string]bool{}
		for _, r := range page1.Results {
			seen[r.ID] = true
		}
		for _, r := range page2.Results {
			if seen[r.ID] {
				t.Errorf("resultado %s duplicado entre as páginas", r.ID)
			}
		}
	})

	t.Run("validação", func(t *testing.T) {
		expectInvalid := func(req models.SearchRequest, since *time.Time, lastID string) {
			if _, err := SearchMessages(testCtx(), req, since, lastID, owner.ID); !errors.Is(err, ErrInvalidInput) {
				t.Errorf("esperava ErrInvalidInput, obtive %v", err)
			}
		}

		expectInvalid(models.SearchRequest{}, nil, "")                                               // nenhum filtro
		expectInvalid(models.SearchRequest{Text: "   "}, nil, "")                                    // só espaços
		expectInvalid(models.SearchRequest{Text: "palavra", Order: "up"}, nil, "")                   // order inválido
		expectInvalid(models.SearchRequest{DateStart: "31/12/2026"}, nil, "")                        // data malformada
		expectInvalid(models.SearchRequest{DateStart: "2026-12-31", DateEnd: "2026-01-01"}, nil, "") // start depois de end
		expectInvalid(models.SearchRequest{Text: "palavra"}, timePtr(time.Now()), "")                // since sem last_id
		expectInvalid(models.SearchRequest{Text: "palavra"}, nil, randUUID())                        // last_id sem since

		if _, err := SearchMessages(testCtx(), models.SearchRequest{Text: "palavra"}, nil, "", ""); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("esperava ErrInvalidInput para user vazio, obtive %v", err)
		}
	})
}

func TestUpdateUserBoundaryLengths(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	// exactly 32 runes for nickname, 64 runes for status and 512 runes for
	// description (multibyte) are accepted
	nickname := "n" + strings.Repeat("ç", 31)
	status := "s" + strings.Repeat("ç", 63)
	description := "d" + strings.Repeat("ç", 511)
	if err := UpdateUser(testCtx(), user.ID, nickname, status, description); err != nil {
		t.Fatalf("UpdateUser with 32-rune nickname, 64-rune status and 512-rune description returned error: %v", err)
	}
	stored, err := storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID returned error: %v", err)
	}
	if stored.Nickname == nil || *stored.Nickname != nickname {
		t.Errorf("expected nickname %q, got %v", nickname, stored.Nickname)
	}
	if stored.StatusMessage == nil || *stored.StatusMessage != status {
		t.Errorf("expected status_message %q, got %v", status, stored.StatusMessage)
	}
	if stored.StatusUpdatedAt == nil {
		t.Error("expected status_updated_at to be set")
	}

	// 33 runes for nickname is rejected
	longNickname := "n" + strings.Repeat("ç", 32)
	if err := UpdateUser(testCtx(), user.ID, longNickname, "ok", "ok"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for 33-rune nickname, got %v", err)
	}

	// 65 runes for status is rejected
	longStatus := "s" + strings.Repeat("ç", 64)
	if err := UpdateUser(testCtx(), user.ID, "ok", longStatus, "ok"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for 65-rune status, got %v", err)
	}

	// 513 runes for description is rejected
	longDescription := "d" + strings.Repeat("ç", 512)
	if err := UpdateUser(testCtx(), user.ID, "ok", "ok", longDescription); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for 513-rune description, got %v", err)
	}

	// rejections must not persist anything
	stored, err = storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID returned error: %v", err)
	}
	if stored.Nickname == nil || *stored.Nickname != nickname {
		t.Errorf("expected nickname %q after rejections, got %v", nickname, stored.Nickname)
	}
	if stored.StatusMessage == nil || *stored.StatusMessage != status {
		t.Errorf("expected status_message %q after rejections, got %v", status, stored.StatusMessage)
	}
	if stored.Description == nil || *stored.Description != description {
		t.Errorf("expected description %q after rejections, got %v", description, stored.Description)
	}
}

func TestListUsersPagination(t *testing.T) {
	// frontier: the last user in (created_at, id) order, including users
	// left behind by other tests
	var frontier models.UserSummary
	var since *time.Time
	lastID := ""
	for {
		page, err := ListUsers(testCtx(), since, lastID)
		if err != nil {
			t.Fatalf("ListUsers returned error: %v", err)
		}
		if len(page.Users) == 0 {
			t.Fatal("ListUsers returned an empty page while walking the frontier")
		}
		frontier = page.Users[len(page.Users)-1]
		if !page.HasMore {
			break
		}
		since = &frontier.CreatedAt
		lastID = frontier.ID
	}

	// 10ms to guarantee the new users have a created_at after the frontier
	time.Sleep(10 * time.Millisecond)
	const newTotal = 101
	for i := 0; i < newTotal; i++ {
		if _, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP()); err != nil {
			t.Fatalf("failed to create user %d: %v", i, err)
		}
	}

	// first page from the frontier: 100 users (the limit) and has_more
	page1, err := ListUsers(testCtx(), &frontier.CreatedAt, frontier.ID)
	if err != nil {
		t.Fatalf("ListUsers returned error: %v", err)
	}
	if len(page1.Users) != 100 {
		t.Fatalf("expected 100 users on the first page, got %d", len(page1.Users))
	}
	if !page1.HasMore {
		t.Error("expected has_more true on the first page")
	}

	// second page via the (created_at, id) cursor: the remaining user
	last := page1.Users[len(page1.Users)-1]
	page2, err := ListUsers(testCtx(), &last.CreatedAt, last.ID)
	if err != nil {
		t.Fatalf("ListUsers for the second page returned error: %v", err)
	}
	if len(page2.Users) != 1 {
		t.Fatalf("expected 1 user on the second page, got %d", len(page2.Users))
	}
	if page2.Users[0].ID == last.ID {
		t.Error("second page must not repeat the last user from the first page")
	}
	if page2.HasMore {
		t.Error("expected has_more false on the second page")
	}

	// no duplicates between pages
	seen := make(map[string]bool, len(page1.Users)+len(page2.Users))
	for _, u := range page1.Users {
		seen[u.ID] = true
	}
	if seen[page2.Users[0].ID] {
		t.Error("user duplicated between pages")
	}
}

func TestListUsersSince(t *testing.T) {
	userA := newTestMessageUser(t)
	time.Sleep(10 * time.Millisecond)
	userB := newTestMessageUser(t)

	since := userA.CreatedAt
	list, err := ListUsers(testCtx(), &since, "")
	if err != nil {
		t.Fatalf("ListUsers with since returned error: %v", err)
	}

	byID := make(map[string]models.UserSummary, len(list.Users))
	for _, u := range list.Users {
		byID[u.ID] = u
	}
	if _, ok := byID[userA.ID]; ok {
		t.Error("since should exclude the user with the same timestamp")
	}
	got, ok := byID[userB.ID]
	if !ok {
		t.Fatal("user created after since does not appear in the list")
	}
	if got.Username != userB.Username {
		t.Errorf("expected username %q, got %q", userB.Username, got.Username)
	}
}

// lastUserSummary returns the last user in (created_at, id) order, walking
// all pages of ListUsers (including users left behind by other tests).
func lastUserSummary(t *testing.T) models.UserSummary {
	t.Helper()
	var frontier models.UserSummary
	var since *time.Time
	lastID := ""
	for {
		page, err := ListUsers(testCtx(), since, lastID)
		if err != nil {
			t.Fatalf("ListUsers returned error: %v", err)
		}
		if len(page.Users) == 0 {
			t.Fatal("ListUsers returned an empty page")
		}
		frontier = page.Users[len(page.Users)-1]
		if !page.HasMore {
			break
		}
		since = &frontier.CreatedAt
		lastID = frontier.ID
	}
	return frontier
}

func TestCreateChannelLimitReached(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	_ = newTestMessageChannel(t, &owner.ID)

	// fill the server up to the limit (500)
	for i := 0; i < 499; i++ {
		if _, err := storage.CreateChannel(testCtx(), "ch_"+randHex(8), "text", ""); err != nil {
			t.Fatalf("failed to create channel %d: %v", i, err)
		}
	}

	if _, err := CreateChannel(testCtx(), testActorID(), "ch_"+randHex(8), "text", ""); !errors.Is(err, ErrChannelLimitReached) {
		t.Errorf("expected ErrChannelLimitReached, got %v", err)
	}

	count, err := storage.CountChannels(testCtx())
	if err != nil {
		t.Fatalf("CountChannels returned error: %v", err)
	}
	if count != 500 {
		t.Errorf("expected 500 channels, got %d", count)
	}
}

func TestParseRobotsGroups(t *testing.T) {
	t.Run("crawl-delay entre UAs cria grupos separados", func(t *testing.T) {
		body := "User-agent: *\nCrawl-delay: 10\nUser-agent: PapoBot\nDisallow: /\n"
		rules := parseRobots([]byte(body))
		if len(rules.groups) != 2 {
			t.Fatalf("grupos esperados 2, obtidos %d", len(rules.groups))
		}
		if len(rules.groups[0].agents) != 1 || rules.groups[0].agents[0] != "*" {
			t.Errorf("agentes do grupo 0: %v", rules.groups[0].agents)
		}
		if len(rules.groups[1].agents) != 1 || rules.groups[1].agents[0] != "PapoBot" {
			t.Errorf("agentes do grupo 1: %v", rules.groups[1].agents)
		}
		if len(rules.groups[1].disallow) != 1 || rules.groups[1].disallow[0] != "/" {
			t.Errorf("disallow do grupo 1: %v", rules.groups[1].disallow)
		}
	})
	t.Run("UAs consecutivos sem diretiva = mesmo grupo", func(t *testing.T) {
		body := "User-agent: Googlebot\nUser-agent: PapoBot\nDisallow: /\n"
		rules := parseRobots([]byte(body))
		if len(rules.groups) != 1 {
			t.Fatalf("grupo esperado 1, obtidos %d", len(rules.groups))
		}
		if len(rules.groups[0].agents) != 2 {
			t.Errorf("agentes esperados 2, obtidos %v", rules.groups[0].agents)
		}
	})
	t.Run("diretiva nao rastreada apos regra fecha o grupo", func(t *testing.T) {
		body := "User-agent: *\nDisallow: /\nSitemap: https://exemplo.com/sitemap.xml\nUser-agent: PapoBot\nAllow: /\n"
		rules := parseRobots([]byte(body))
		if len(rules.groups) != 2 {
			t.Fatalf("grupos esperados 2, obtidos %d", len(rules.groups))
		}
	})
}

func TestRobotsAllowed(t *testing.T) {
	ua := "PapoBot/1.0 (+link preview)"
	t.Run("sem grupo aplicavel = permitido", func(t *testing.T) {
		rules := parseRobots([]byte("User-agent: Googlebot\nDisallow: /\n"))
		if !robotsAllowed(rules, ua, "/qualquer") {
			t.Error("deveria ser permitido (sem grupo para o UA)")
		}
	})
	t.Run("UA mais especifico vence", func(t *testing.T) {
		rules := parseRobots([]byte("User-agent: *\nDisallow: /\nUser-agent: PapoBot\nAllow: /\n"))
		if !robotsAllowed(rules, ua, "/x") {
			t.Error("grupo PapoBot (token mais longo) deveria vencer = permitido")
		}
	})
	t.Run("regra mais especifica vence", func(t *testing.T) {
		rules := parseRobots([]byte("User-agent: *\nDisallow: /\nAllow: /public\n"))
		if !robotsAllowed(rules, ua, "/public/arquivo") {
			t.Error("Allow mais longa deveria vencer = permitido")
		}
		if robotsAllowed(rules, ua, "/privado") {
			t.Error("Disallow deveria vencer = negado")
		}
	})
	t.Run("empate = Allow", func(t *testing.T) {
		rules := parseRobots([]byte("User-agent: *\nDisallow: /a\nAllow: /a\n"))
		if !robotsAllowed(rules, ua, "/a") {
			t.Error("empate deveria ser permitido")
		}
	})
	t.Run("wildcard e ancora", func(t *testing.T) {
		rules := parseRobots([]byte("User-agent: *\nDisallow: /privado/*.pdf$\n"))
		if robotsAllowed(rules, ua, "/privado/doc.pdf") {
			t.Error("/privado/doc.pdf deveria ser negado")
		}
		if !robotsAllowed(rules, ua, "/privado/doc.pdf?download=1") {
			t.Error("regra ancorada nao deve casar com query = permitido")
		}
	})
	t.Run("disallow vazio permite tudo", func(t *testing.T) {
		rules := parseRobots([]byte("User-agent: *\nDisallow:\n"))
		if !robotsAllowed(rules, ua, "/x") {
			t.Error("Disallow vazio deveria permitir tudo")
		}
	})
}

func TestRobotsEntryTTL(t *testing.T) {
	normal := 3600 * time.Second
	t.Run("fail-closed usa TTL curto", func(t *testing.T) {
		entry := robotsEntry{allowedAll: false, rules: nil}
		if got := robotsEntryTTL(entry, normal); got != robotsFailTTL {
			t.Errorf("TTL fail-closed = %v, esperado %v", got, robotsFailTTL)
		}
	})
	t.Run("sucesso com regras usa TTL normal", func(t *testing.T) {
		entry := robotsEntry{rules: &robotsRules{}}
		if got := robotsEntryTTL(entry, normal); got != normal {
			t.Errorf("TTL sucesso = %v, esperado %v", got, normal)
		}
	})
	t.Run("404 (allowedAll) usa TTL normal", func(t *testing.T) {
		entry := robotsEntry{allowedAll: true}
		if got := robotsEntryTTL(entry, normal); got != normal {
			t.Errorf("TTL allowedAll = %v, esperado %v", got, normal)
		}
	})
}

// TestRobotsAllowedCacheHit exercita o RobotsAllowed real nos caminhos de
// cache (sem rede): fail-closed fresco → negado; sucesso fresco → permitido.
func TestRobotsAllowedCacheHit(t *testing.T) {
	origin := "https://cache-hit.invalid"
	u, _ := url.Parse(origin + "/algo")
	ctx := context.Background()

	robotsCacheMu.Lock()
	robotsCache[origin] = robotsEntry{allowedAll: false, rules: nil, fetchedAt: time.Now()}
	robotsCacheMu.Unlock()
	if RobotsAllowed(ctx, u) {
		t.Error("fail-closed fresco deveria ser negado")
	}

	robotsCacheMu.Lock()
	robotsCache[origin] = robotsEntry{allowedAll: true, rules: nil, fetchedAt: time.Now()}
	robotsCacheMu.Unlock()
	if !RobotsAllowed(ctx, u) {
		t.Error("sucesso fresco deveria ser permitido")
	}

	robotsCacheMu.Lock()
	delete(robotsCache, origin)
	robotsCacheMu.Unlock()
}

func TestEnsureAttachmentThumbnailEnabledFlag(t *testing.T) {
	ctx := testCtx()

	makePNG := func(t *testing.T) []byte {
		t.Helper()
		img := image.NewNRGBA(image.Rect(0, 0, 64, 32))
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatalf("falha ao codificar PNG: %v", err)
		}
		return buf.Bytes()
	}

	runCase := func(t *testing.T, enabled string) string {
		content := makePNG(t)
		hash := newTestMediaHash(t, content)
		attachment, err := storage.CreateAttachment(ctx, models.Attachments{
			OriginalFileName: "teste.png",
			MediaShaHash:     hash,
		})
		if err != nil {
			t.Fatalf("CreateAttachment: %v", err)
		}
		t.Setenv("THUMBNAIL_ENABLED", enabled)
		ensureAttachmentThumbnail(ctx, attachment.ID, mediaBlobPath(hash), "image/png")
		return attachment.ID
	}

	t.Run("desabilitado nao gera thumbnail", func(t *testing.T) {
		attachmentID := runCase(t, "false")
		if _, err := storage.GetThumbnailByAttachmentID(ctx, attachmentID, thumbnailKind); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("thumbnail nao deveria existir, err = %v", err)
		}
	})

	t.Run("habilitado gera thumbnail webp", func(t *testing.T) {
		attachmentID := runCase(t, "true")
		thumb, err := storage.GetThumbnailByAttachmentID(ctx, attachmentID, thumbnailKind)
		if err != nil {
			t.Fatalf("GetThumbnailByAttachmentID: %v", err)
		}
		if thumb.MimeType != "image/webp" {
			t.Errorf("mime esperado image/webp, obtido %s", thumb.MimeType)
		}
		// o blob da thumbnail vai para o content-addressable storage
		if _, err := os.Stat(mediaBlobPath(thumb.MediaShaHash)); err != nil {
			t.Errorf("blob da thumbnail deveria existir no storage: %v", err)
		}
	})
}

func TestDownloadPreviewImageDisabled(t *testing.T) {
	cfg := config.LoadConfig()
	cfg.ThumbnailEnabled = false
	if sha, err := downloadPreviewImage(testCtx(), cfg, "https://exemplo.com/imagem.png"); err == nil || sha != "" {
		t.Error("THUMBNAIL_ENABLED=false deveria retornar erro com hash vazio (sem fetch)")
	}
}

func TestExtractPreviewURLs(t *testing.T) {
	cases := []struct {
		content string
		max     int
		want    []string
	}{
		{"veja https://a.com/x e https://b.com/y", 2, []string{"https://a.com/x", "https://b.com/y"}},
		{"https://a.com/x https://b.com/y https://c.com/z", 2, []string{"https://a.com/x", "https://b.com/y"}},
		{"https://a.com/x e https://a.com/x de novo", 2, []string{"https://a.com/x"}},
		{"https://a.com/x.,", 2, []string{"https://a.com/x"}},
		{"(https://a.com/b(c).)", 2, []string{"https://a.com/b(c)"}},
		{"(https://a.com/b(c))", 2, []string{"https://a.com/b(c)"}},
		{"sem url aqui", 2, nil},
		{"ftp://a.com/x", 2, nil},
		{"https://a.com/x", 0, []string{"https://a.com/x"}},
	}
	for _, tc := range cases {
		got := extractPreviewURLs(tc.content, tc.max)
		if len(got) != len(tc.want) {
			t.Errorf("extractPreviewURLs(%q, %d) = %v, esperado %v", tc.content, tc.max, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("extractPreviewURLs(%q, %d)[%d] = %q, esperado %q", tc.content, tc.max, i, got[i], tc.want[i])
			}
		}
	}
}

func TestStripTrailingPunctuation(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://x.com/a.", "https://x.com/a"},
		{"https://x.com/a!!", "https://x.com/a"},
		{"https://x.com/a(b).", "https://x.com/a(b)"},
		{"https://x.com/a(b)", "https://x.com/a(b)"},
		{"https://x.com/a(b))", "https://x.com/a(b)"},
	}
	for _, tc := range cases {
		if got := stripTrailingPunctuation(tc.in); got != tc.want {
			t.Errorf("stripTrailingPunctuation(%q) = %q, esperado %q", tc.in, got, tc.want)
		}
	}
}

func TestParseOpenGraph(t *testing.T) {
	page, err := url.Parse("https://exemplo.com/noticias/post-1?ref=x")
	if err != nil {
		t.Fatalf("url.Parse retornou erro: %v", err)
	}

	t.Run("og completo", func(t *testing.T) {
		body := []byte(`<html><head>
			<title>Título da página</title>
			<meta property="og:title" content="OG Title">
			<meta property="og:description" content="OG Desc">
			<meta property="og:image" content="https://cdn.exemplo.com/img.png">
		</head><body></body></html>`)
		title, desc, img := parseOpenGraph(body, page)
		if title != "OG Title" {
			t.Errorf("title = %q, esperado %q", title, "OG Title")
		}
		if desc != "OG Desc" {
			t.Errorf("description = %q, esperado %q", desc, "OG Desc")
		}
		if img != "https://cdn.exemplo.com/img.png" {
			t.Errorf("image = %q, esperado URL absoluta do og:image", img)
		}
	})

	t.Run("fallback twitter e pagina", func(t *testing.T) {
		body := []byte(`<html><head>
			<title>Página Title</title>
			<meta name="twitter:title" content="TW Title">
			<meta name="description" content="Página Desc">
			<meta name="twitter:image" content="https://cdn.exemplo.com/tw.png">
		</head><body></body></html>`)
		title, desc, img := parseOpenGraph(body, page)
		if title != "TW Title" {
			t.Errorf("title = %q, esperado fallback twitter", title)
		}
		if desc != "Página Desc" {
			t.Errorf("description = %q, esperado meta name=description", desc)
		}
		if img != "https://cdn.exemplo.com/tw.png" {
			t.Errorf("image = %q, esperado twitter:image", img)
		}
	})

	t.Run("title da pagina quando nao ha meta", func(t *testing.T) {
		body := []byte(`<html><head><title>Só o Title</title></head><body></body></html>`)
		title, desc, img := parseOpenGraph(body, page)
		if title != "Só o Title" {
			t.Errorf("title = %q, esperado <title> da página", title)
		}
		if desc != "" || img != "" {
			t.Errorf("desc/img deveriam ser vazios, obtive %q/%q", desc, img)
		}
	})

	t.Run("imagem relativa resolvida contra a pagina", func(t *testing.T) {
		body := []byte(`<html><head>
			<meta property="og:image" content="/midias/img.png">
		</head><body></body></html>`)
		_, _, img := parseOpenGraph(body, page)
		if img != "https://exemplo.com/midias/img.png" {
			t.Errorf("image = %q, esperado resolvida contra a URL da página", img)
		}
	})

	t.Run("corpo vazio", func(t *testing.T) {
		title, desc, img := parseOpenGraph(nil, page)
		if title != "" || desc != "" || img != "" {
			t.Errorf("corpo vazio deveria retornar tudo vazio, obtive %q/%q/%q", title, desc, img)
		}
	})
}

func TestYoutubeEmbedURL(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ", "https://www.youtube.com/embed/dQw4w9WgXcQ"},
		{"https://m.youtube.com/watch?v=dQw4w9WgXcQ", "https://www.youtube.com/embed/dQw4w9WgXcQ"},
		{"https://youtu.be/dQw4w9WgXcQ", "https://www.youtube.com/embed/dQw4w9WgXcQ"},
		{"https://youtube.com/shorts/dQw4w9WgXcQ", "https://www.youtube.com/embed/dQw4w9WgXcQ"},
		{"https://youtube.com/embed/dQw4w9WgXcQ", "https://www.youtube.com/embed/dQw4w9WgXcQ"},
		{"https://youtube.com/watch", ""},
		{"https://youtube.com/watch?v=curto", ""},
		{"https://youtu.be/dQw4w9WgXcQ/extra", ""},
		{"https://vimeo.com/123", ""},
		{"https://youtube.com/", ""},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatalf("url.Parse(%q) retornou erro: %v", tc.raw, err)
		}
		if got := youtubeEmbedURL(u); got != tc.want {
			t.Errorf("youtubeEmbedURL(%q) = %q, esperado %q", tc.raw, got, tc.want)
		}
	}
}

func TestOembedProviderHost(t *testing.T) {
	cases := []struct{ host, want string }{
		{"youtube.com", "youtube.com"},
		{"m.youtube.com", "youtube.com"},
		{"www.youtube.com", "youtube.com"},
		{"x.com", "x.com"},
		{"www.x.com", "x.com"},
		{"notyoutube.com", ""},
		{"myx.com", ""},
		{"example.com", ""},
	}
	for _, tc := range cases {
		if got := oembedProviderHost(tc.host); got != tc.want {
			t.Errorf("oembedProviderHost(%q) = %q, esperado %q", tc.host, got, tc.want)
		}
	}
}

func TestNormalizeHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"WWW.Example.COM", "example.com"},
		{"m.youtube.com", "youtube.com"},
		{"www.m.youtube.com", "youtube.com"},
		{"x.com", "x.com"},
	}
	for _, tc := range cases {
		if got := normalizeHost(tc.in); got != tc.want {
			t.Errorf("normalizeHost(%q) = %q, esperado %q", tc.in, got, tc.want)
		}
	}
}

func TestRobotsTargetPath(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"https://exemplo.com/a/b?c=1", "/a/b?c=1"},
		{"https://exemplo.com", "/"},
		{"https://exemplo.com/", "/"},
		{"https://exemplo.com/x", "/x"},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatalf("url.Parse(%q) retornou erro: %v", tc.raw, err)
		}
		if got := robotsTargetPath(u); got != tc.want {
			t.Errorf("robotsTargetPath(%q) = %q, esperado %q", tc.raw, got, tc.want)
		}
	}
}

func TestTruncateRune(t *testing.T) {
	if got := truncateRune("abcdef", 3); got != "abc" {
		t.Errorf("truncateRune longo = %q, esperado %q", got, "abc")
	}
	if got := truncateRune("abc", 5); got != "abc" {
		t.Errorf("truncateRune curto = %q, esperado inalterado", got)
	}
	if got := truncateRune("olá ☺ mundo", 4); got != "olá " {
		t.Errorf("truncateRune UTF-8 = %q, esperado 4 runes %q", got, "olá ")
	}
	if got := truncateRune("abc", 0); got != "abc" {
		t.Errorf("truncateRune n<=0 = %q, esperado inalterado", got)
	}
}

func TestNullableTextAndFirstNonEmpty(t *testing.T) {
	if nullableText("") != nil {
		t.Error("nullableText(\"\") deveria ser nil")
	}
	if s := nullableText("x"); s == nil || *s != "x" {
		t.Errorf("nullableText(\"x\") = %v, esperado ponteiro para x", s)
	}
	if got := firstNonEmpty("", "a", "b"); got != "a" {
		t.Errorf("firstNonEmpty = %q, esperado %q", got, "a")
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("firstNonEmpty tudo vazio = %q, esperado vazio", got)
	}
}

func TestGetOrCreatePreviewCacheHit(t *testing.T) {
	ctx := testCtx()
	user := newTestMessageUser(t)

	title := "Título em cache"
	seed, err := storage.UpsertPreview(ctx, models.LinkPreview{
		URL:   "https://cache-hit.example.com/pagina",
		Kind:  "og",
		Title: &title,
	})
	if err != nil {
		t.Fatalf("UpsertPreview retornou erro: %v", err)
	}

	got, refetched, err := GetOrCreatePreview(ctx, user.ID, "https://cache-hit.example.com/pagina")
	if err != nil {
		t.Fatalf("GetOrCreatePreview com cache hit retornou erro (deveria evitar rede): %v", err)
	}
	if refetched {
		t.Errorf("cache hit dentro do TTL não deve marcar refetch, obtive refetched=true")
	}
	if got.ID != seed.ID {
		t.Errorf("esperava o mesmo id do preview em cache, obtive %s (esperado %s)", got.ID, seed.ID)
	}
	if got.Title == nil || *got.Title != title {
		t.Errorf("esperava title %q do cache, obtive %v", title, got.Title)
	}
}

func TestGetLinkPreview(t *testing.T) {
	ctx := testCtx()
	cleanServers(ctx)
	owner := newTestMessageUser(t)
	reader := newTestMessageUser(t)
	outsider := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)

	imgBytes := pngAvatarBytes(16, 16)
	imgHash := newTestMediaHash(t, imgBytes)

	preview, err := storage.UpsertPreview(ctx, models.LinkPreview{
		URL:        "https://preview-image.example.com/pagina",
		Kind:       "og",
		ImageMedia: &imgHash,
	})
	if err != nil {
		t.Fatalf("UpsertPreview retornou erro: %v", err)
	}

	message, err := storage.CreateMessage(ctx, channel.ID, owner.ID, "mensagem com preview", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}
	if err := storage.AddMessagePreviews(ctx, message.ID, []string{preview.ID}); err != nil {
		t.Fatalf("falha ao vincular preview à mensagem: %v", err)
	}

	t.Run("dono acessa", func(t *testing.T) {
		got, err := GetLinkPreview(ctx, preview.ID, owner.ID)
		if err != nil {
			t.Fatalf("GetLinkPreview para o dono retornou erro: %v", err)
		}
		if got.ID != preview.ID {
			t.Errorf("esperava preview %s, obtive %s", preview.ID, got.ID)
		}
	})

	t.Run("preview sem imagem é acessível (retorna sem imagem)", func(t *testing.T) {
		noImg, err := storage.UpsertPreview(ctx, models.LinkPreview{
			URL:  "https://preview-image.example.com/sem-imagem",
			Kind: "og",
		})
		if err != nil {
			t.Fatalf("UpsertPreview retornou erro: %v", err)
		}
		if err := storage.AddMessagePreviews(ctx, message.ID, []string{noImg.ID}); err != nil {
			t.Fatalf("falha ao vincular preview: %v", err)
		}
		got, err := GetLinkPreview(ctx, noImg.ID, owner.ID)
		if err != nil {
			t.Fatalf("preview sem imagem deveria ser acessível, obtive %v", err)
		}
		if got.ID != noImg.ID || got.ImageFilePath != nil {
			t.Errorf("esperava preview %s sem imagem, obtive id=%s img=%v", noImg.ID, got.ID, got.ImageFilePath)
		}
	})

	t.Run("preview sem vinculo vira 404", func(t *testing.T) {
		unlinked, err := storage.UpsertPreview(ctx, models.LinkPreview{
			URL:        "https://preview-image.example.com/sem-vinculo",
			Kind:       "og",
			ImageMedia: &imgHash,
		})
		if err != nil {
			t.Fatalf("UpsertPreview retornou erro: %v", err)
		}
		if _, err := GetLinkPreview(ctx, unlinked.ID, owner.ID); !errors.Is(err, ErrPreviewNotFound) {
			t.Errorf("esperava ErrPreviewNotFound para preview sem vinculo, obtive %v", err)
		}
	})

	t.Run("preview inexistente vira 404", func(t *testing.T) {
		if _, err := GetLinkPreview(ctx, randUUID(), owner.ID); !errors.Is(err, ErrPreviewNotFound) {
			t.Errorf("esperava ErrPreviewNotFound, obtive %v", err)
		}
	})

	t.Run("ids vazios viram 404", func(t *testing.T) {
		if _, err := GetLinkPreview(ctx, "", owner.ID); !errors.Is(err, ErrPreviewNotFound) {
			t.Errorf("previewID vazio: esperava ErrPreviewNotFound, obtive %v", err)
		}
		if _, err := GetLinkPreview(ctx, preview.ID, ""); !errors.Is(err, ErrPreviewNotFound) {
			t.Errorf("userID vazio: esperava ErrPreviewNotFound, obtive %v", err)
		}
	})

	// Fecha o canal: o reader ganha a role, o outsider fica de fora.
	grantChannelPermission(t, channel, reader, models.ChannelPermission{ReadChannel: true})

	t.Run("leitor com role acessa", func(t *testing.T) {
		if _, err := GetLinkPreview(ctx, preview.ID, reader.ID); err != nil {
			t.Errorf("leitor com role deveria acessar, obtive %v", err)
		}
	})

	t.Run("canal fechado vira 404 para o de fora", func(t *testing.T) {
		if _, err := GetLinkPreview(ctx, preview.ID, outsider.ID); !errors.Is(err, ErrPreviewNotFound) {
			t.Errorf("esperava ErrPreviewNotFound (404, nao vaza existencia), obtive %v", err)
		}
	})
}

func TestCreateMessageLinksCachedPreview(t *testing.T) {
	ctx := testCtx()
	cleanServers(ctx)
	author := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &author.ID)

	title := "Preview da mensagem"
	seed, err := storage.UpsertPreview(ctx, models.LinkPreview{
		URL:   "https://msg-preview.example.com/a",
		Kind:  "og",
		Title: &title,
	})
	if err != nil {
		t.Fatalf("UpsertPreview retornou erro: %v", err)
	}

	content := "veja https://msg-preview.example.com/a."
	msg, err := CreateMessage(ctx, channel.ID, author.ID, content, "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}
	// previews são processados em background: a criação não retorna previews
	if len(msg.Previews) != 0 {
		t.Errorf("criação não deve retornar previews, obtive %v", msg.Previews)
	}

	// simula o processamento em background (goroutine do handler)
	added, updates := ProcessMessagePreviews(ctx, msg.ID, author.ID, content)
	if len(added) != 1 {
		t.Fatalf("esperava 1 preview processado, obtive %d", len(added))
	}
	if len(updates) != 0 {
		t.Errorf("preview em cache (sem refetch) não deve gerar updates, obtive %v", updates)
	}
	if added[0].ID != seed.ID {
		t.Errorf("esperava preview %s, obtive %s", seed.ID, added[0].ID)
	}

	linked, err := storage.ListPreviewsByMessageIDs(ctx, []string{msg.ID})
	if err != nil {
		t.Fatalf("ListPreviewsByMessageIDs retornou erro: %v", err)
	}
	if len(linked[msg.ID]) != 1 || linked[msg.ID][0].ID != seed.ID {
		t.Errorf("preview nao ficou vinculado a mensagem no banco: %v", linked)
	}
}

func TestEditMessageReplacesPreviews(t *testing.T) {
	ctx := testCtx()
	cleanServers(ctx)
	author := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &author.ID)

	titleA := "A"
	titleB := "B"
	seedA, err := storage.UpsertPreview(ctx, models.LinkPreview{
		URL: "https://edit-preview.example.com/a", Kind: "og", Title: &titleA,
	})
	if err != nil {
		t.Fatalf("UpsertPreview A retornou erro: %v", err)
	}
	seedB, err := storage.UpsertPreview(ctx, models.LinkPreview{
		URL: "https://edit-preview.example.com/b", Kind: "og", Title: &titleB,
	})
	if err != nil {
		t.Fatalf("UpsertPreview B retornou erro: %v", err)
	}

	contentA := "https://edit-preview.example.com/a"
	msg, err := CreateMessage(ctx, channel.ID, author.ID, contentA, "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}
	// previews são processados em background: a criação não retorna previews
	if len(msg.Previews) != 0 {
		t.Fatalf("criação não deve retornar previews, obtive %v", msg.Previews)
	}
	added, updates := ProcessMessagePreviews(ctx, msg.ID, author.ID, contentA)
	if len(added) != 1 || added[0].ID != seedA.ID {
		t.Fatalf("esperava preview A processado na criação, obtive %v", added)
	}
	if len(updates) != 0 {
		t.Fatalf("preview em cache (sem refetch) não deve gerar updates, obtive %v", updates)
	}

	contentB := "agora https://edit-preview.example.com/b"
	edited, err := EditMessage(ctx, msg.ID, author.ID, contentB)
	if err != nil {
		t.Fatalf("EditMessage retornou erro: %v", err)
	}
	if len(edited.Previews) != 0 {
		t.Errorf("edição não deve retornar previews, obtive %v", edited.Previews)
	}
	added, removed, updates := ProcessEditedMessagePreviews(ctx, msg.ID, author.ID, contentB)
	if len(added) != 1 || added[0].ID != seedB.ID {
		t.Errorf("esperava B adicionado após edição, obtive %v", added)
	}
	if len(removed) != 1 || removed[0].ID != seedA.ID {
		t.Errorf("esperava A removido após edição, obtive %v", removed)
	}
	if len(updates) != 0 {
		t.Errorf("previews em cache (sem refetch) não devem gerar updates, obtive %v", updates)
	}
	linked, err := storage.ListPreviewsByMessageIDs(ctx, []string{msg.ID})
	if err != nil {
		t.Fatalf("ListPreviewsByMessageIDs retornou erro: %v", err)
	}
	if len(linked[msg.ID]) != 1 || linked[msg.ID][0].ID != seedB.ID {
		t.Errorf("preview A deveria ter sido removido no banco, obtive %v", linked)
	}

	cleared, err := EditMessage(ctx, msg.ID, author.ID, "")
	if err != nil {
		t.Fatalf("EditMessage com content vazio retornou erro: %v", err)
	}
	if len(cleared.Previews) != 0 {
		t.Errorf("esperava previews limpos após content vazio, obtive %v", cleared.Previews)
	}
	added, removed, updates = ProcessEditedMessagePreviews(ctx, msg.ID, author.ID, "")
	if len(added) != 0 {
		t.Errorf("não esperava previews adicionados após content vazio, obtive %v", added)
	}
	if len(removed) != 1 || removed[0].ID != seedB.ID {
		t.Errorf("esperava B removido após content vazio, obtive %v", removed)
	}
	if len(updates) != 0 {
		t.Errorf("content vazio não deve gerar updates, obtive %v", updates)
	}
	linked, err = storage.ListPreviewsByMessageIDs(ctx, []string{msg.ID})
	if err != nil {
		t.Fatalf("ListPreviewsByMessageIDs retornou erro: %v", err)
	}
	if len(linked[msg.ID]) != 0 {
		t.Errorf("vinculos deveriam ter sido limpos no banco, obtive %v", linked)
	}
}

func TestDownloadAttachmentThumbnail(t *testing.T) {
	ctx := testCtx()
	cleanServers(ctx)
	author := newTestMessageUser(t)
	reader := newTestMessageUser(t)
	outsider := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &author.ID)

	pngBytes := pngAvatarBytes(64, 32)
	pngHash := newTestMediaHash(t, pngBytes)
	attachment, err := storage.CreateAttachment(ctx, models.Attachments{
		OriginalFileName: "foto.png",
		MediaShaHash:     pngHash,
		CreatedBy:        &author.ID,
	})
	if err != nil {
		t.Fatalf("CreateAttachment retornou erro: %v", err)
	}
	ensureAttachmentThumbnail(ctx, attachment.ID, mediaBlobPath(pngHash), "image/png")

	message, err := storage.CreateMessage(ctx, channel.ID, author.ID, "com imagem", "", []string{attachment.ID})
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}
	_ = message

	t.Run("dono baixa a thumbnail", func(t *testing.T) {
		thumb, err := DownloadAttachmentThumbnail(ctx, attachment.ID, author.ID)
		if err != nil {
			t.Fatalf("DownloadAttachmentThumbnail para o autor retornou erro: %v", err)
		}
		if thumb.AttachmentID != attachment.ID {
			t.Errorf("esperava attachment %s, obtive %s", attachment.ID, thumb.AttachmentID)
		}
		if thumb.MimeType != "image/webp" {
			t.Errorf("esperava mime image/webp, obtive %s", thumb.MimeType)
		}
		if _, err := os.Stat(thumb.FilePath); err != nil {
			t.Errorf("arquivo da thumbnail deveria existir: %v", err)
		}
	})

	// Canal fechado: o reader ganha a role, o outsider fica de fora.
	grantChannelPermission(t, channel, reader, models.ChannelPermission{ReadChannel: true})

	t.Run("leitor com role baixa", func(t *testing.T) {
		if _, err := DownloadAttachmentThumbnail(ctx, attachment.ID, reader.ID); err != nil {
			t.Errorf("leitor com role deveria baixar, obtive %v", err)
		}
	})

	t.Run("canal fechado nega o outsider", func(t *testing.T) {
		if _, err := DownloadAttachmentThumbnail(ctx, attachment.ID, outsider.ID); !errors.Is(err, ErrPermissionDenied) {
			t.Errorf("esperava ErrPermissionDenied, obtive %v", err)
		}
	})

	t.Run("sem thumbnail vira 404", func(t *testing.T) {
		plain, err := storage.CreateAttachment(ctx, models.Attachments{
			OriginalFileName: "texto.txt",
			MediaShaHash:     newTestMediaHash(t, []byte("texto de apoio")),
		})
		if err != nil {
			t.Fatalf("CreateAttachment retornou erro: %v", err)
		}
		if _, err := storage.CreateMessage(ctx, channel.ID, author.ID, "texto", "", []string{plain.ID}); err != nil {
			t.Fatalf("falha ao criar mensagem de apoio: %v", err)
		}
		if _, err := DownloadAttachmentThumbnail(ctx, plain.ID, author.ID); !errors.Is(err, ErrAttachmentNotFound) {
			t.Errorf("esperava ErrAttachmentNotFound, obtive %v", err)
		}
	})

	t.Run("attachment inexistente vira 404", func(t *testing.T) {
		if _, err := DownloadAttachmentThumbnail(ctx, randUUID(), author.ID); !errors.Is(err, ErrAttachmentNotFound) {
			t.Errorf("esperava ErrAttachmentNotFound, obtive %v", err)
		}
	})

	t.Run("ids vazios", func(t *testing.T) {
		if _, err := DownloadAttachmentThumbnail(ctx, "", author.ID); !errors.Is(err, ErrAttachmentNotFound) {
			t.Errorf("fileID vazio: esperava ErrAttachmentNotFound, obtive %v", err)
		}
		if _, err := DownloadAttachmentThumbnail(ctx, attachment.ID, ""); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("userID vazio: esperava ErrInvalidInput, obtive %v", err)
		}
	})
}

func TestListMessagesWithThumbnailsAndPreviews(t *testing.T) {
	ctx := testCtx()
	cleanServers(ctx)
	author := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &author.ID)

	pngBytes := pngAvatarBytes(64, 32)
	pngHash := newTestMediaHash(t, pngBytes)
	attachment, err := storage.CreateAttachment(ctx, models.Attachments{
		OriginalFileName: "foto.png",
		MediaShaHash:     pngHash,
		CreatedBy:        &author.ID,
	})
	if err != nil {
		t.Fatalf("CreateAttachment retornou erro: %v", err)
	}
	ensureAttachmentThumbnail(ctx, attachment.ID, mediaBlobPath(pngHash), "image/png")

	title := "Preview da listagem"
	seed, err := storage.UpsertPreview(ctx, models.LinkPreview{
		URL:   "https://list-preview.example.com/x",
		Kind:  "og",
		Title: &title,
	})
	if err != nil {
		t.Fatalf("UpsertPreview retornou erro: %v", err)
	}

	message, err := storage.CreateMessage(ctx, channel.ID, author.ID, "https://list-preview.example.com/x", "", []string{attachment.ID})
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}
	if err := storage.AddMessagePreviews(ctx, message.ID, []string{seed.ID}); err != nil {
		t.Fatalf("falha ao vincular preview: %v", err)
	}

	list, err := ListMessages(ctx, channel.ID, author.ID, nil, "")
	if err != nil {
		t.Fatalf("ListMessages retornou erro: %v", err)
	}
	if len(list.Messages) != 1 {
		t.Fatalf("esperava 1 mensagem, obtive %d", len(list.Messages))
	}
	m := list.Messages[0]
	if len(m.Previews) != 1 || m.Previews[0].ID != seed.ID {
		t.Errorf("mensagem deveria carregar o preview, obtive %v", m.Previews)
	}
	if len(m.Attachments) != 1 {
		t.Fatalf("mensagem deveria carregar 1 attachment, obtive %d", len(m.Attachments))
	}
	if m.Attachments[0].ThumbnailID == nil {
		t.Error("attachment deveria carregar o ThumbnailID")
	}
}

// --- PinMessage ---

func TestPinMessageOwner(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	pinned, created, err := PinMessage(testCtx(), channel.ID, message.ID, owner.ID)
	if err != nil {
		t.Fatalf("PinMessage retornou erro: %v", err)
	}
	if !created {
		t.Error("esperava created=true")
	}
	if pinned.ChannelID != channel.ID || pinned.MessageID != message.ID {
		t.Errorf("pin não confere: %+v", pinned)
	}
	if pinned.PinnedBy == nil || *pinned.PinnedBy != owner.ID {
		t.Errorf("esperava pinned_by %s, obtive %v", owner.ID, pinned.PinnedBy)
	}
}

func TestPinMessageWithPermissionRole(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	role, err := storage.CreateRole(testCtx(), newRandomRoleName(), nil, models.RolePermissions{PinMessage: true})
	if err != nil {
		t.Fatalf("CreateRole retornou erro: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), actor.ID, role.ID); err != nil {
		t.Fatalf("AssignUserRole retornou erro: %v", err)
	}

	if _, created, err := PinMessage(testCtx(), channel.ID, message.ID, actor.ID); err != nil || !created {
		t.Fatalf("PinMessage com role falhou: created=%v err=%v", created, err)
	}
}

func TestPinMessageWithoutPermission(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	if _, _, err := PinMessage(testCtx(), channel.ID, message.ID, actor.ID); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("esperava ErrPermissionDenied, obtive %v", err)
	}
}

func TestPinMessageNotFound(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)

	// mensagem inexistente
	if _, _, err := PinMessage(testCtx(), channel.ID, randUUID(), owner.ID); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("esperava ErrMessageNotFound para mensagem inexistente, obtive %v", err)
	}

	// mensagem de outro canal
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}
	other, err := storage.CreateChannel(testCtx(), "channel_"+randHex(8), "text", "")
	if err != nil {
		t.Fatalf("CreateChannel (outro) retornou erro: %v", err)
	}
	if _, _, err := PinMessage(testCtx(), other.ID, message.ID, owner.ID); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("esperava ErrMessageNotFound para canal divergente, obtive %v", err)
	}
}

func TestPinMessageInvalidInput(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	for _, tc := range []struct {
		name      string
		channelID string
		messageID string
		userID    string
	}{
		{"canal vazio", "", message.ID, owner.ID},
		{"mensagem vazia", channel.ID, "", owner.ID},
		{"usuário vazio", channel.ID, message.ID, ""},
	} {
		if _, _, err := PinMessage(testCtx(), tc.channelID, tc.messageID, tc.userID); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("[%s] esperava ErrInvalidInput, obtive %v", tc.name, err)
		}
	}
}

func TestPinMessageLimit(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)

	// limite documentado: 100 pins por canal
	for i := 0; i < 100; i++ {
		message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, fmt.Sprintf("msg %d", i), "", nil)
		if err != nil {
			t.Fatalf("CreateMessage[%d] retornou erro: %v", i, err)
		}
		if _, created, err := PinMessage(testCtx(), channel.ID, message.ID, owner.ID); err != nil || !created {
			t.Fatalf("PinMessage[%d] falhou: created=%v err=%v", i, created, err)
		}
	}

	overflow, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "estouro", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage (estouro) retornou erro: %v", err)
	}
	if _, _, err := PinMessage(testCtx(), channel.ID, overflow.ID, owner.ID); !errors.Is(err, ErrTooManyPinnedMessages) {
		t.Errorf("esperava ErrTooManyPinnedMessages, obtive %v", err)
	}
}

// --- UnpinMessage ---

func TestUnpinMessageOwner(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	if _, _, err := PinMessage(testCtx(), channel.ID, message.ID, owner.ID); err != nil {
		t.Fatalf("PinMessage retornou erro: %v", err)
	}

	removed, err := UnpinMessage(testCtx(), channel.ID, message.ID, owner.ID)
	if err != nil {
		t.Fatalf("UnpinMessage retornou erro: %v", err)
	}
	if !removed {
		t.Error("esperava removed=true")
	}
}

func TestUnpinMessageNotPinned(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	if _, err := UnpinMessage(testCtx(), channel.ID, message.ID, owner.ID); !errors.Is(err, ErrMessageNotPinned) {
		t.Errorf("esperava ErrMessageNotPinned, obtive %v", err)
	}
}

func TestUnpinMessageWithPermissionRole(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	role, err := storage.CreateRole(testCtx(), newRandomRoleName(), nil, models.RolePermissions{PinMessage: true})
	if err != nil {
		t.Fatalf("CreateRole retornou erro: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), actor.ID, role.ID); err != nil {
		t.Fatalf("AssignUserRole retornou erro: %v", err)
	}

	if _, _, err := PinMessage(testCtx(), channel.ID, message.ID, owner.ID); err != nil {
		t.Fatalf("PinMessage retornou erro: %v", err)
	}

	if _, err := UnpinMessage(testCtx(), channel.ID, message.ID, actor.ID); err != nil {
		t.Errorf("UnpinMessage com role pin_message falhou: %v", err)
	}
}

func TestUnpinMessageWithoutPermission(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	if _, _, err := PinMessage(testCtx(), channel.ID, message.ID, owner.ID); err != nil {
		t.Fatalf("PinMessage retornou erro: %v", err)
	}

	if _, err := UnpinMessage(testCtx(), channel.ID, message.ID, actor.ID); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("esperava ErrPermissionDenied, obtive %v", err)
	}
}

func TestUnpinMessageWithoutReadPermission(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	// canal restrito: o ator tem pin_message mas não read_channel
	role, err := storage.CreateRole(testCtx(), newRandomRoleName(), nil, models.RolePermissions{PinMessage: true})
	if err != nil {
		t.Fatalf("CreateRole retornou erro: %v", err)
	}
	if _, err := UpdateChannelPermissions(testCtx(), testActorID(), channel.ID, role.ID, models.ChannelPermission{ReadChannel: false}); err != nil {
		t.Fatalf("UpdateChannelPermissions retornou erro: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), actor.ID, role.ID); err != nil {
		t.Fatalf("AssignUserRole retornou erro: %v", err)
	}

	if _, _, err := PinMessage(testCtx(), channel.ID, message.ID, owner.ID); err != nil {
		t.Fatalf("PinMessage retornou erro: %v", err)
	}

	if _, err := UnpinMessage(testCtx(), channel.ID, message.ID, actor.ID); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("esperava ErrPermissionDenied sem read_channel, obtive %v", err)
	}
}

func TestUnpinMessageNotFound(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)

	// mensagem inexistente
	if _, err := UnpinMessage(testCtx(), channel.ID, randUUID(), owner.ID); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("esperava ErrMessageNotFound para mensagem inexistente, obtive %v", err)
	}

	// mensagem de outro canal
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}
	other, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("CreateChannel (outro) retornou erro: %v", err)
	}
	if _, err := UnpinMessage(testCtx(), other.ID, message.ID, owner.ID); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("esperava ErrMessageNotFound para canal divergente, obtive %v", err)
	}
}

func TestUnpinMessageInvalidInput(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	for _, tc := range []struct {
		name      string
		channelID string
		messageID string
		userID    string
	}{
		{"canal vazio", "", message.ID, owner.ID},
		{"mensagem vazia", channel.ID, "", owner.ID},
		{"usuário vazio", channel.ID, message.ID, ""},
	} {
		if _, err := UnpinMessage(testCtx(), tc.channelID, tc.messageID, tc.userID); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("[%s] esperava ErrInvalidInput, obtive %v", tc.name, err)
		}
	}
}

// --- ListPinnedMessages ---

func TestListPinnedMessages(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)

	m1, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "uma", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage (uma) retornou erro: %v", err)
	}
	m2, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "duas", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage (duas) retornou erro: %v", err)
	}

	for _, m := range []models.Message{m2, m1} {
		if _, _, err := PinMessage(testCtx(), channel.ID, m.ID, owner.ID); err != nil {
			t.Fatalf("PinMessage retornou erro: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	list, err := ListPinnedMessages(testCtx(), channel.ID, owner.ID)
	if err != nil {
		t.Fatalf("ListPinnedMessages retornou erro: %v", err)
	}
	if list.ChannelID != channel.ID {
		t.Errorf("esperava channel_id %s, obtive %s", channel.ID, list.ChannelID)
	}
	if len(list.Pinned) != 2 {
		t.Fatalf("esperava 2 mensagens pinadas, obtive %d", len(list.Pinned))
	}
	if list.Pinned[0].ID != m2.ID || list.Pinned[1].ID != m1.ID {
		t.Errorf("ordem incorreta: obtive [%s, %s]", list.Pinned[0].ID, list.Pinned[1].ID)
	}

	// canal sem pins: lista vazia (não nil)
	emptyChannel, err := CreateChannel(testCtx(), testActorID(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("CreateChannel (vazio) retornou erro: %v", err)
	}
	empty, err := ListPinnedMessages(testCtx(), emptyChannel.ID, owner.ID)
	if err != nil {
		t.Fatalf("ListPinnedMessages (vazio) retornou erro: %v", err)
	}
	if empty.Pinned == nil || len(empty.Pinned) != 0 {
		t.Errorf("esperava lista vazia não nil, obtive %v", empty.Pinned)
	}
}

func TestListPinnedMessagesPermissionDenied(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)

	// canal restrito: o ator não tem read_channel
	role, err := storage.CreateRole(testCtx(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("CreateRole retornou erro: %v", err)
	}
	if _, err := UpdateChannelPermissions(testCtx(), testActorID(), channel.ID, role.ID, models.ChannelPermission{ReadChannel: false}); err != nil {
		t.Fatalf("UpdateChannelPermissions retornou erro: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), actor.ID, role.ID); err != nil {
		t.Fatalf("AssignUserRole retornou erro: %v", err)
	}

	if _, err := ListPinnedMessages(testCtx(), channel.ID, actor.ID); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("esperava ErrPermissionDenied, obtive %v", err)
	}
}

func TestListPinnedMessagesNotFound(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)

	if _, err := ListPinnedMessages(testCtx(), randUUID(), owner.ID); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("esperava ErrChannelNotFound, obtive %v", err)
	}
}

// --- ListChannels (last_read) ---

func TestListChannelsLastRead(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "lida", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}
	if err := storage.TouchLastReadMessage(testCtx(), owner.ID, channel.ID, message); err != nil {
		t.Fatalf("TouchLastReadMessage retornou erro: %v", err)
	}

	summaries, err := ListChannels(testCtx(), owner.ID)
	if err != nil {
		t.Fatalf("ListChannels retornou erro: %v", err)
	}
	var got *models.ChannelSummary
	for i := range summaries {
		if summaries[i].ID == channel.ID {
			got = &summaries[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("canal %s não encontrado na listagem", channel.ID)
	}
	if got.LastReadMessage == nil || *got.LastReadMessage != message.ID {
		t.Errorf("esperava last_read_message %s, obtive %v", message.ID, got.LastReadMessage)
	}
	if got.LastReadAt == nil || got.LastReadAt.IsZero() {
		t.Errorf("esperava last_read_at preenchido, obtive %v", got.LastReadAt)
	}

	// outro usuário: last_read nulo
	other, err := ListChannels(testCtx(), testActorID())
	if err != nil {
		t.Fatalf("ListChannels (outro usuário) retornou erro: %v", err)
	}
	for _, s := range other {
		if s.ID == channel.ID && (s.LastReadMessage != nil || s.LastReadAt != nil) {
			t.Errorf("esperava last_read nulo para outro usuário, obtive %v / %v", s.LastReadMessage, s.LastReadAt)
		}
	}
}

// --- reactions ---

func TestAddReactionToMessageOwner(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	unicode := "👍"
	reaction, created, count, err := AddReactionToMessage(testCtx(), channel.ID, message.ID, owner.ID, "", unicode)
	if err != nil {
		t.Fatalf("AddReactionToMessage retornou erro: %v", err)
	}
	if !created || count != 1 {
		t.Fatalf("esperava created=true count=1, obtive created=%v count=%d", created, count)
	}
	if reaction.MessageID != message.ID || reaction.UserID != owner.ID {
		t.Errorf("reação não confere: %+v", reaction)
	}
	if reaction.EmojiID != nil || reaction.Unicode == nil || *reaction.Unicode != unicode {
		t.Errorf("esperava unicode %q e emoji_id null, obtive %+v", unicode, reaction)
	}
}

func TestAddReactionToMessageWithSendRole(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}
	grantChannelPermission(t, channel, actor, models.ChannelPermission{SendMessages: true})

	unicode := "👍"
	if _, created, _, err := AddReactionToMessage(testCtx(), channel.ID, message.ID, actor.ID, "", unicode); err != nil || !created {
		t.Fatalf("AddReactionToMessage com role falhou: created=%v err=%v", created, err)
	}
}

func TestAddReactionToMessageWithoutPermission(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}
	// Define uma permissão no canal para que ele deixe de ser "aberto" (em
	// canais sem roles o envio é livre). O actor, sem role, é negado.
	grantChannelPermission(t, channel, owner, models.ChannelPermission{SendMessages: true})

	unicode := "👍"
	if _, _, _, err := AddReactionToMessage(testCtx(), channel.ID, message.ID, actor.ID, "", unicode); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("esperava ErrPermissionDenied, obtive %v", err)
	}
}

func TestAddReactionToMessageInvalidInput(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	for _, tc := range []struct {
		name    string
		emojiID string
		unicode string
	}{
		{"nenhum emoji", "", ""},
		{"ambos emojis", randUUID(), "👍"},
		{"unicode com 17 caracteres", "", strings.Repeat("👍", 17)},
	} {
		if _, _, _, err := AddReactionToMessage(testCtx(), channel.ID, message.ID, owner.ID, tc.emojiID, tc.unicode); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("[%s] esperava ErrInvalidInput, obtive %v", tc.name, err)
		}
	}

	// 16 runes é o limite exato e deve ser aceito
	if _, created, _, err := AddReactionToMessage(testCtx(), channel.ID, message.ID, owner.ID, "", strings.Repeat("👍", 16)); err != nil || !created {
		t.Errorf("unicode com 16 caracteres deveria ser aceita: created=%v err=%v", created, err)
	}
}

func TestAddReactionToMessageNotFound(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	unicode := "👍"

	// mensagem inexistente
	if _, _, _, err := AddReactionToMessage(testCtx(), channel.ID, randUUID(), owner.ID, "", unicode); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("esperava ErrMessageNotFound para mensagem inexistente, obtive %v", err)
	}

	// mensagem de outro canal
	other, err := storage.CreateChannel(testCtx(), "channel_"+randHex(8), "text", "")
	if err != nil {
		t.Fatalf("CreateChannel (outro) retornou erro: %v", err)
	}
	if _, _, _, err := AddReactionToMessage(testCtx(), other.ID, message.ID, owner.ID, "", unicode); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("esperava ErrMessageNotFound para canal divergente, obtive %v", err)
	}

	// canal da URL divergente do canal real da mensagem: a checagem de
	// pertencimento da mensagem precede a de existência do canal.
	if _, _, _, err := AddReactionToMessage(testCtx(), randUUID(), message.ID, owner.ID, "", unicode); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("esperava ErrMessageNotFound para canal divergente, obtive %v", err)
	}
}

func TestAddReactionToMessageEmojiNotFound(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	if _, _, _, err := AddReactionToMessage(testCtx(), channel.ID, message.ID, owner.ID, randUUID(), ""); !errors.Is(err, ErrEmojiNotFound) {
		t.Errorf("esperava ErrEmojiNotFound, obtive %v", err)
	}
}

func TestAddReactionToMessageLimit(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	// limite documentado: 20 tipos de reação por mensagem
	types := []string{"👍", "👎", "😂", "😢", "😮", "😡", "🔥", "🎉", "💀", "❤️", "🙏", "👀", "✨", "🍕", "🚀", "🎯", "💯", "🥳", "🫡", "🤝"}
	for i, unicode := range types {
		if _, created, _, err := AddReactionToMessage(testCtx(), channel.ID, message.ID, owner.ID, "", unicode); err != nil || !created {
			t.Fatalf("AddReactionToMessage[%d] falhou: created=%v err=%v", i, created, err)
		}
	}

	fresh := "🆕"
	if _, _, _, err := AddReactionToMessage(testCtx(), channel.ID, message.ID, owner.ID, "", fresh); !errors.Is(err, ErrTooManyReactions) {
		t.Errorf("esperava ErrTooManyReactions, obtive %v", err)
	}
}

func TestRemoveReactionFromMessage(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	unicode := "👍"
	if _, _, _, err := AddReactionToMessage(testCtx(), channel.ID, message.ID, owner.ID, "", unicode); err != nil {
		t.Fatalf("AddReactionToMessage retornou erro: %v", err)
	}

	count, err := RemoveReactionFromMessage(testCtx(), channel.ID, message.ID, owner.ID, "", unicode)
	if err != nil || count != 0 {
		t.Fatalf("RemoveReactionFromMessage falhou: count=%d err=%v", count, err)
	}
	if _, err := RemoveReactionFromMessage(testCtx(), channel.ID, message.ID, owner.ID, "", unicode); !errors.Is(err, ErrReactionNotFound) {
		t.Errorf("esperava ErrReactionNotFound removendo de novo, obtive %v", err)
	}
}

func TestRemoveReactionFromMessageOnlyOwn(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}
	grantChannelPermission(t, channel, actor, models.ChannelPermission{ReadChannel: true})

	unicode := "👍"
	if _, _, _, err := AddReactionToMessage(testCtx(), channel.ID, message.ID, owner.ID, "", unicode); err != nil {
		t.Fatalf("AddReactionToMessage retornou erro: %v", err)
	}

	// actor lê o canal, mas a reação é do owner → ErrReactionNotFound
	if _, err := RemoveReactionFromMessage(testCtx(), channel.ID, message.ID, actor.ID, "", unicode); !errors.Is(err, ErrReactionNotFound) {
		t.Errorf("esperava ErrReactionNotFound para reação de outro usuário, obtive %v", err)
	}
}

func TestRemoveReactionFromMessageWithoutPermission(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}
	// Torna o canal "fechado" para que o actor, sem read_channel, seja negado.
	grantChannelPermission(t, channel, owner, models.ChannelPermission{ReadChannel: true})

	unicode := "👍"
	if _, err := RemoveReactionFromMessage(testCtx(), channel.ID, message.ID, actor.ID, "", unicode); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("esperava ErrPermissionDenied, obtive %v", err)
	}
}

func TestListMessageReactions(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	u1 := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}

	unicode := "👍"
	if _, _, _, err := AddReactionToMessage(testCtx(), channel.ID, message.ID, owner.ID, "", unicode); err != nil {
		t.Fatalf("AddReactionToMessage (owner) retornou erro: %v", err)
	}
	if _, _, _, err := AddReactionToMessage(testCtx(), channel.ID, message.ID, u1.ID, "", unicode); err != nil {
		t.Fatalf("AddReactionToMessage (u1) retornou erro: %v", err)
	}

	list, err := ListMessageReactions(testCtx(), channel.ID, message.ID, owner.ID, nil, "")
	if err != nil {
		t.Fatalf("ListMessageReactions retornou erro: %v", err)
	}
	if list.MessageID != message.ID {
		t.Errorf("esperava message_id %s, obtive %s", message.ID, list.MessageID)
	}
	if len(list.Reactions) != 1 {
		t.Fatalf("esperava 1 tipo de reação, obtive %d: %+v", len(list.Reactions), list.Reactions)
	}
	g := list.Reactions[0]
	if g.EmojiID != nil || g.Unicode == nil || *g.Unicode != unicode || g.Count != 2 {
		t.Errorf("esperava 👍 count=2, obtive %+v", g)
	}
	if len(g.Users) != 2 || !containsServiceReactionUser(g.Users, owner.ID) || !containsServiceReactionUser(g.Users, u1.ID) {
		t.Errorf("esperava users owner e u1, obtive %v", g.Users)
	}

	// mensagem sem reações → lista vazia
	empty, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "sem reações", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage (vazia) retornou erro: %v", err)
	}
	list, err = ListMessageReactions(testCtx(), channel.ID, empty.ID, owner.ID, nil, "")
	if err != nil {
		t.Fatalf("ListMessageReactions (vazia) retornou erro: %v", err)
	}
	if len(list.Reactions) != 0 {
		t.Errorf("esperava lista vazia, obtive %+v", list.Reactions)
	}
}

func TestListMessageReactionsErrors(t *testing.T) {
	cleanServers(testCtx())
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage retornou erro: %v", err)
	}
	// Torna o canal "fechado" para que o actor, sem read_channel, seja negado.
	grantChannelPermission(t, channel, owner, models.ChannelPermission{ReadChannel: true})

	if _, err := ListMessageReactions(testCtx(), channel.ID, randUUID(), owner.ID, nil, ""); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("esperava ErrMessageNotFound, obtive %v", err)
	}
	// canal da URL divergente do canal real da mensagem: a checagem de
	// pertencimento da mensagem precede a de existência do canal.
	if _, err := ListMessageReactions(testCtx(), randUUID(), message.ID, owner.ID, nil, ""); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("esperava ErrMessageNotFound para canal divergente, obtive %v", err)
	}
	if _, err := ListMessageReactions(testCtx(), channel.ID, message.ID, actor.ID, nil, ""); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("esperava ErrPermissionDenied, obtive %v", err)
	}
	if _, err := ListMessageReactions(testCtx(), "", message.ID, owner.ID, nil, ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("esperava ErrInvalidInput, obtive %v", err)
	}
}

func containsServiceReactionUser(users []models.MessageReactionUser, id string) bool {
	for _, u := range users {
		if u.UserID == id {
			return true
		}
	}
	return false
}

// --- Conexões de sessão (auth híbrida) ---

func TestCreateSessionConnection(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	token, info, err := CreateSessionConnection(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("CreateSessionConnection retornou erro: %v", err)
	}
	if token == "" {
		t.Error("esperava token preenchido")
	}
	if uid, verr := utils.ValidateToken(token, config.LoadConfig().JWTSecret); verr != nil || uid != user.ID {
		t.Errorf("token não valida para o usuário: uid=%q err=%v", uid, verr)
	}
	if info.ID == "" {
		t.Error("esperava connection.id preenchido")
	}
	if err := storage.CheckUserConnection(testCtx(), user.ID, utils.HashToken(token)); err != nil {
		t.Errorf("esperava a conexão ativa, obtive %v", err)
	}
}

// TestCreateSessionConnectionUniqueTokens garante que dois logins em sequência
// (mesmo segundo) geram tokens distintos — o retry de iat evita a colisão na
// UNIQUE(user_id, token_hash) que geraria falso reuso.
func TestCreateSessionConnectionUniqueTokens(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	tokenA, infoA, err := CreateSessionConnection(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("falha na primeira conexão: %v", err)
	}
	tokenB, infoB, err := CreateSessionConnection(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("falha na segunda conexão: %v", err)
	}
	if tokenA == tokenB {
		t.Error("esperava tokens distintos para conexões distintas")
	}
	if infoA.ID == infoB.ID {
		t.Error("esperava ids de conexão distintos")
	}
	conns, err := storage.ListUserConnections(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("falha ao listar conexões: %v", err)
	}
	if len(conns) != 2 {
		t.Errorf("esperava 2 conexões ativas, obtive %d", len(conns))
	}
}

func TestRefreshConnection(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	oldToken, oldConn, err := CreateSessionConnection(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("falha ao criar a conexão: %v", err)
	}

	newToken, newConn, err := RefreshConnection(testCtx(), user.ID, oldToken)
	if err != nil {
		t.Fatalf("RefreshConnection retornou erro: %v", err)
	}
	if newToken == "" || newToken == oldToken {
		t.Errorf("esperava um novo token distinto, obtive %q", newToken)
	}
	if newConn.ID == oldConn.ID {
		t.Error("esperava uma nova conexão distinta")
	}
	// o token antigo está na janela de graça (ainda aceito)
	if err := storage.CheckUserConnection(testCtx(), user.ID, utils.HashToken(oldToken)); err != nil {
		t.Errorf("esperava o token antigo na janela de graça, obtive %v", err)
	}
	// o token novo está ativo
	if err := storage.CheckUserConnection(testCtx(), user.ID, utils.HashToken(newToken)); err != nil {
		t.Errorf("esperava o token novo ativo, obtive %v", err)
	}
}

func TestRefreshConnectionUnknownToken(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	token, err := utils.GenerateSessionToken(user.ID, time.Now().Add(77*time.Second), config.LoadConfig().JWTSecret)
	if err != nil {
		t.Fatalf("falha ao gerar token: %v", err)
	}
	if _, _, err := RefreshConnection(testCtx(), user.ID, token); !errors.Is(err, ErrConnectionNotFound) {
		t.Errorf("esperava ErrConnectionNotFound, obtive %v", err)
	}
}

// TestRefreshConnectionGracePeriod garante que reapresentar um token recém-
// substituído dentro da janela de graça devolve a ponta da cadeia sem
// rotacionar de novo (não gera segunda conexão).
func TestRefreshConnectionGracePeriod(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	tokenA, _, err := CreateSessionConnection(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("falha ao criar a conexão: %v", err)
	}
	tokenB, _, err := RefreshConnection(testCtx(), user.ID, tokenA)
	if err != nil {
		t.Fatalf("falha na primeira rotação: %v", err)
	}
	tokenC, _, err := RefreshConnection(testCtx(), user.ID, tokenA)
	if err != nil {
		t.Fatalf("falha na segunda rotação: %v", err)
	}
	if tokenC != tokenB {
		t.Errorf("esperava a ponta da cadeia %q, obtive %q", tokenB, tokenC)
	}
	conns, err := storage.ListUserConnections(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("falha ao listar conexões: %v", err)
	}
	if len(conns) != 1 {
		t.Errorf("esperava 1 conexão ativa (a ponta), obtive %d", len(conns))
	}
}

func TestLogoutRevokes(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	token, _, err := CreateSessionConnection(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("falha ao criar a conexão: %v", err)
	}
	if err := Logout(testCtx(), user.ID, token); err != nil {
		t.Fatalf("Logout retornou erro: %v", err)
	}
	if err := storage.CheckUserConnection(testCtx(), user.ID, utils.HashToken(token)); err == nil {
		t.Error("esperava a conexão revogada ser rejeitada")
	}
}

func TestLogoutUnknownToken(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	token, err := utils.GenerateSessionToken(user.ID, time.Now().Add(77*time.Second), config.LoadConfig().JWTSecret)
	if err != nil {
		t.Fatalf("falha ao gerar token: %v", err)
	}
	// token desconhecido: logout é no-op (o cliente só remove o cookie)
	if err := Logout(testCtx(), user.ID, token); err != nil {
		t.Errorf("esperava logout no-op para token desconhecido, obtive %v", err)
	}
}

// TestLogoutReuse garante que reapresentar um token substituído FORA da janela
// de graça é tratado como reuso: todas as conexões são revogadas e
// users.connection_violation é marcado.
func TestLogoutReuse(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	tokenA, _, err := CreateSessionConnection(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("falha ao criar a conexão: %v", err)
	}
	tokenB, _, err := RefreshConnection(testCtx(), user.ID, tokenA)
	if err != nil {
		t.Fatalf("falha na rotação: %v", err)
	}
	// simula que a substituição foi há mais de 1 minuto (fora da janela de graça)
	if _, err := storage.GetDB().ExecContext(testCtx(),
		"UPDATE user_connections SET replaced_at = now() - interval '2 minutes' WHERE token_hash = $1",
		utils.HashToken(tokenA)); err != nil {
		t.Fatalf("falha ao antecipar replaced_at: %v", err)
	}
	if err := Logout(testCtx(), user.ID, tokenA); err != nil {
		t.Fatalf("Logout retornou erro: %v", err)
	}
	// todas as conexões foram revogadas (inclusive tokenB)
	if err := storage.CheckUserConnection(testCtx(), user.ID, utils.HashToken(tokenB)); err == nil {
		t.Error("esperava tokenB revogado após o reuso")
	}
	var violation bool
	if err := storage.GetDB().QueryRowContext(testCtx(),
		"SELECT connection_violation FROM users WHERE id = $1", user.ID).Scan(&violation); err != nil {
		t.Fatalf("falha ao ler connection_violation: %v", err)
	}
	if !violation {
		t.Error("esperava connection_violation = TRUE")
	}
}

func TestListConnections(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	if _, _, err := CreateSessionConnection(testCtx(), user.ID); err != nil {
		t.Fatalf("falha ao criar a 1ª conexão: %v", err)
	}
	if _, _, err := CreateSessionConnection(testCtx(), user.ID); err != nil {
		t.Fatalf("falha ao criar a 2ª conexão: %v", err)
	}
	conns, err := ListConnections(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("ListConnections retornou erro: %v", err)
	}
	if len(conns) != 2 {
		t.Errorf("esperava 2 conexões, obtive %d: %+v", len(conns), conns)
	}
}

func TestDropConnection(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	tokenA, connA, err := CreateSessionConnection(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("falha ao criar a 1ª conexão: %v", err)
	}
	tokenB, _, err := CreateSessionConnection(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("falha ao criar a 2ª conexão: %v", err)
	}

	// revoga uma conexão específica
	n, err := DropConnection(testCtx(), user.ID, connA.ID)
	if err != nil {
		t.Fatalf("DropConnection retornou erro: %v", err)
	}
	if n != 1 {
		t.Errorf("esperava dropped=1, obtive %d", n)
	}
	if err := storage.CheckUserConnection(testCtx(), user.ID, utils.HashToken(tokenA)); err == nil {
		t.Error("esperava tokenA revogado")
	}
	if err := storage.CheckUserConnection(testCtx(), user.ID, utils.HashToken(tokenB)); err != nil {
		t.Errorf("esperava tokenB ativo, obtive %v", err)
	}

	// revoga todas (case-insensitive)
	n, err = DropConnection(testCtx(), user.ID, "all")
	if err != nil {
		t.Fatalf("DropConnection (ALL) retornou erro: %v", err)
	}
	if n != 1 {
		t.Errorf("esperava dropped=1 (só tokenB ativa), obtive %d", n)
	}
	if err := storage.CheckUserConnection(testCtx(), user.ID, utils.HashToken(tokenB)); err == nil {
		t.Error("esperava tokenB revogado")
	}
}

func TestDropConnectionInvalid(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	if _, err := DropConnection(testCtx(), user.ID, "nao-um-uuid"); !errors.Is(err, ErrInvalidConnection) {
		t.Errorf("esperava ErrInvalidConnection, obtive %v", err)
	}
}

func TestDropConnectionNotFound(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	if _, err := DropConnection(testCtx(), user.ID, randUUID()); !errors.Is(err, ErrConnectionNotFound) {
		t.Errorf("esperava ErrConnectionNotFound, obtive %v", err)
	}
}
