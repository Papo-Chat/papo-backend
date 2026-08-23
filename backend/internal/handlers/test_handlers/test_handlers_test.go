package test_handlers

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/big"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"papo/internal/config"
	"papo/internal/handlers"
	"papo/internal/middleware"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/labstack/echo/v4"
)

// migrationsDir é o caminho relativo ao diretório deste pacote (backend/internal/handlers/test_handlers).
const migrationsDir = "../../../../migrations"

// defaultDatabaseURL corresponde aos padrões do infra/docker-compose.yml.
const defaultDatabaseURL = "postgres://papo:papo123@localhost:5432/papo"

// testBaseURL é a base usada para montar o campo "type" dos erros RFC 7807.
const testBaseURL = "https://papo.test/"

func TestMain(m *testing.M) {
	os.Exit(runHandlersTests(m))
}

// exclui servidores nos testes para manter a regra de negócio de 1 servidor por backend
func cleanServers(ctx context.Context) error {
	_, err := storage.GetDB().ExecContext(ctx, "DELETE FROM servers")

	if err != nil {
		return err
	}

	return nil
}

// runHandlersTests prepara um banco temporário com as migrations do projeto,
// inicializa o storage contra ele, executa os testes e remove o banco ao final.
func runHandlersTests(m *testing.M) int {
	baseURL := testDatabaseURL()

	baseDB, err := sql.Open("pgx", baseURL)
	if err != nil {
		fmt.Printf("testes de handlers ignorados: falha ao abrir conexão: %v\n", err)
		return 0
	}
	defer baseDB.Close()

	if err := ping(baseDB); err != nil {
		fmt.Printf("testes de handlers ignorados: não foi possível conectar ao PostgreSQL (%v). Inicie o PostgreSQL (infra/docker-compose.yml) ou defina TEST_DATABASE_URL/DATABASE_URL.\n", err)
		return 0
	}

	removeOldTempDatabases(baseDB)

	tempDBName, err := createTempDatabase(baseDB)
	if err != nil {
		fmt.Printf("testes de handlers ignorados: falha ao criar banco temporário: %v\n", err)
		return 0
	}
	defer dropTempDatabase(baseDB, tempDBName)

	tempURL, err := withDatabase(baseURL, tempDBName)
	if err != nil {
		fmt.Printf("testes de handlers ignorados: %v\n", err)
		return 0
	}

	tempDB, err := sql.Open("pgx", tempURL)
	if err != nil {
		fmt.Printf("testes de handlers ignorados: %v\n", err)
		return 0
	}
	defer tempDB.Close()

	if err := ping(tempDB); err != nil {
		fmt.Printf("testes de handlers ignorados: falha ao conectar no banco temporário: %v\n", err)
	}

	if err := applyMigrations(tempDB); err != nil {
		fmt.Printf("testes de handlers FALHARAM na preparação: %v\n", err)
		return 1
	}

	if err := storage.InitDB(tempURL); err != nil {
		fmt.Printf("testes de handlers FALHARAM na preparação: %v\n", err)
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

// newContext monta um echo.Context a partir de uma requisição HTTP de teste.
func newContext(t *testing.T, method, path string, body []byte, ip string) echo.Context {
	t.Helper()

	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if ip != "" {
		req.RemoteAddr = ip + ":12345"
	}

	e := echo.New()
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec)
}

// recorder retorna o http.ResponseWriter do contexto.
func recorder(c echo.Context) *httptest.ResponseRecorder {
	return c.Response().Writer.(*httptest.ResponseRecorder)
}

func testCtx() context.Context {
	return context.Background()
}

// problem é o corpo de erro RFC 7807 retornado pelos handlers.
type problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance"`
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) problem {
	t.Helper()

	var p problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("falha ao decodificar problem+json: %v (corpo: %s)", err, rec.Body.String())
	}
	return p
}

func assertProblem(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantType, wantTitle, wantDetail string) {
	t.Helper()

	if rec.Code != wantStatus {
		t.Fatalf("esperava status %d, obtive %d (corpo: %s)", wantStatus, rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get(echo.HeaderContentType); ct != "application/problem+json" {
		t.Errorf("esperava content-type application/problem+json, obtive %q", ct)
	}

	p := decodeProblem(t, rec)
	if p.Type != testBaseURL+"errors/"+wantType {
		t.Errorf("esperava type %q, obtive %q", testBaseURL+"errors/"+wantType, p.Type)
	}
	if p.Title != wantTitle {
		t.Errorf("esperava title %q, obtive %q", wantTitle, p.Title)
	}
	if p.Status != wantStatus {
		t.Errorf("esperava status %d no corpo, obtive %d", wantStatus, p.Status)
	}
	if p.Detail != wantDetail {
		t.Errorf("esperava detail %q, obtive %q", wantDetail, p.Detail)
	}
}

// --- HealthHandler ---

func TestHealthHandler(t *testing.T) {
	c := newContext(t, http.MethodGet, "/health", nil, "")
	rec := recorder(c)

	if err := handlers.HealthHandler(c); err != nil {
		t.Fatalf("HealthHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("esperava status 200, obtive %d", rec.Code)
	}
	if rec.Body.String() != "OK" {
		t.Errorf("esperava corpo %q, obtive %q", "OK", rec.Body.String())
	}
}

// --- RegisterHandler ---

func TestRegisterHandlerSuccess(t *testing.T) {
	username := newRandomUsername()
	password := newRandomPassword()
	ip := newRandomIP()

	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	c := newContext(t, http.MethodPost, "/auth/register", body, ip)
	rec := recorder(c)

	if err := handlers.RegisterHandler(testBaseURL, c); err != nil {
		t.Fatalf("RegisterHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID        string    `json:"id"`
		Username  string    `json:"username"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID == "" {
		t.Error("esperava id preenchido")
	}
	if resp.Username != username {
		t.Errorf("esperava username %q, obtive %q", username, resp.Username)
	}
	if resp.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}
}

func TestRegisterHandlerInvalidJSON(t *testing.T) {
	c := newContext(t, http.MethodPost, "/auth/register", []byte("{invalido"), newRandomIP())
	rec := recorder(c)

	if err := handlers.RegisterHandler(testBaseURL, c); err != nil {
		t.Fatalf("RegisterHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestRegisterHandlerMissingUsername(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"password": "senha123"})
	c := newContext(t, http.MethodPost, "/auth/register", body, newRandomIP())
	rec := recorder(c)

	if err := handlers.RegisterHandler(testBaseURL, c); err != nil {
		t.Fatalf("RegisterHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "campo 'username' é obrigatório")
}

func TestRegisterHandlerMissingPassword(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"username": "user123"})
	c := newContext(t, http.MethodPost, "/auth/register", body, newRandomIP())
	rec := recorder(c)

	if err := handlers.RegisterHandler(testBaseURL, c); err != nil {
		t.Fatalf("RegisterHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "campo 'password' é obrigatório")
}

func TestRegisterHandlerUsernameTooLong(t *testing.T) {
	cfg := config.LoadConfig()

	MaxUsernameLength := cfg.MaxUsernameLength
	body, _ := json.Marshal(map[string]string{"username": strings.Repeat("a", 18), "password": "senha123"})
	c := newContext(t, http.MethodPost, "/auth/register", body, newRandomIP())
	rec := recorder(c)

	if err := handlers.RegisterHandler(testBaseURL, c); err != nil {
		t.Fatalf("RegisterHandler retornou erro: %v", err)
	}

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"campo 'username' deve ter no máximo "+strconv.Itoa(MaxUsernameLength)+" caracteres")
}

func TestRegisterHandlerPasswordTooLong(t *testing.T) {
	cfg := config.LoadConfig()

	MaxPasswordLength := cfg.MaxPasswordLength
	body, _ := json.Marshal(map[string]string{"username": "user123", "password": strings.Repeat("a", 65)})
	c := newContext(t, http.MethodPost, "/auth/register", body, newRandomIP())
	rec := recorder(c)

	if err := handlers.RegisterHandler(testBaseURL, c); err != nil {
		t.Fatalf("RegisterHandler retornou erro: %v", err)
	}

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"campo 'password' deve ter no máximo "+strconv.Itoa(MaxPasswordLength)+" caracteres")
}

func TestRegisterHandlerUsernameTaken(t *testing.T) {
	username := newRandomUsername()
	password := newRandomPassword()

	body, _ := json.Marshal(map[string]string{"username": username, "password": password})

	// primeiro registro cria o usuário
	c := newContext(t, http.MethodPost, "/auth/register", body, newRandomIP())
	if err := handlers.RegisterHandler(testBaseURL, c); err != nil {
		t.Fatalf("falha no primeiro registro: %v", err)
	}
	if rec := recorder(c); rec.Code != http.StatusCreated {
		t.Fatalf("esperava 201 no primeiro registro, obtive %d", rec.Code)
	}

	// segundo registro com o mesmo username resulta em conflito
	c = newContext(t, http.MethodPost, "/auth/register", body, newRandomIP())
	rec := recorder(c)
	if err := handlers.RegisterHandler(testBaseURL, c); err != nil {
		t.Fatalf("RegisterHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusConflict, "username-taken", "Username já existe", "o username informado já está em uso")
}

func TestRegisterHandlerBannedIP(t *testing.T) {
	ip := newRandomIP()
	bannedUser, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), ip)
	if err != nil {
		t.Fatalf("falha ao criar usuário para banir: %v", err)
	}
	if _, err := storage.SetUserBanned(testCtx(), bannedUser.ID, true); err != nil {
		t.Fatalf("falha ao banir usuário: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"username": newRandomUsername(), "password": newRandomPassword()})
	c := newContext(t, http.MethodPost, "/auth/register", body, ip)
	rec := recorder(c)

	if err := handlers.RegisterHandler(testBaseURL, c); err != nil {
		t.Fatalf("RegisterHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "banned", "Usuário banido", "o IP informado já foi usado por um usuário banido")
}

// --- LoginHandler ---

func TestLoginHandlerSuccess(t *testing.T) {
	username := newRandomUsername()
	password := newRandomPassword()
	ip := newRandomIP()

	// cria o usuário com a senha correta para o login
	hash, err := utils.HashPassword(password)
	if err != nil {
		t.Fatalf("falha ao gerar hash da senha: %v", err)
	}
	if _, _, err := storage.CreateUser(testCtx(), username, hash, ip); err != nil {
		t.Fatalf("falha ao criar usuário para login: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	c := newContext(t, http.MethodPost, "/auth/login", body, ip)
	rec := recorder(c)

	if err := handlers.LoginHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		User struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"user"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.User.ID == "" {
		t.Error("esperava user.id preenchido")
	}
	if resp.User.Username != username {
		t.Errorf("esperava user.username %q, obtive %q", username, resp.User.Username)
	}
	if resp.Token == "" {
		t.Error("esperava token preenchido")
	}

	// o cookie Auth deve ser definido com o token
	setCookieHeaders := rec.Header().Values("Set-Cookie")
	if len(setCookieHeaders) == 0 {
		t.Fatal("esperava header Set-Cookie definido")
	}
	var authCookieValue string
	var hasHttpOnly, hasSecure bool
	for _, header := range setCookieHeaders {
		// o primeiro segmento do Set-Cookie é "name=value"
		segment := header
		if idx := strings.Index(header, ";"); idx >= 0 {
			segment = header[:idx]
		}
		if idx := strings.Index(segment, "="); idx >= 0 {
			name := strings.TrimSpace(segment[:idx])
			value := strings.TrimSpace(segment[idx+1:])
			if name == "Auth" {
				authCookieValue = value
				lower := strings.ToLower(header)
				hasHttpOnly = strings.Contains(lower, "httponly")
				hasSecure = strings.Contains(lower, "secure")
				break
			}
		}
	}
	if authCookieValue == "" {
		t.Fatal("esperava cookie Auth definido")
	}
	if authCookieValue != resp.Token {
		t.Errorf("esperava cookie Auth com valor %q, obtive %q", resp.Token, authCookieValue)
	}
	if !hasHttpOnly {
		t.Error("esperava cookie Auth com HttpOnly")
	}
	if !hasSecure {
		t.Error("esperava cookie Auth com Secure")
	}
}

func TestLoginHandlerInvalidJSON(t *testing.T) {
	c := newContext(t, http.MethodPost, "/auth/login", []byte("{invalido"), newRandomIP())
	rec := recorder(c)

	if err := handlers.LoginHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestLoginHandlerMissingUsername(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"password": "senha123"})
	c := newContext(t, http.MethodPost, "/auth/login", body, newRandomIP())
	rec := recorder(c)

	if err := handlers.LoginHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "campo 'username' é obrigatório")
}

func TestLoginHandlerMissingPassword(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"username": "user123"})
	c := newContext(t, http.MethodPost, "/auth/login", body, newRandomIP())
	rec := recorder(c)

	if err := handlers.LoginHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "campo 'password' é obrigatório")
}

func TestLoginHandlerUsernameTooLong(t *testing.T) {
	cfg := config.LoadConfig()
	MaxUsernameLength := cfg.MaxUsernameLength
	body, _ := json.Marshal(map[string]string{"username": strings.Repeat("a", 18), "password": "senha123"})
	c := newContext(t, http.MethodPost, "/auth/login", body, newRandomIP())
	rec := recorder(c)

	if err := handlers.LoginHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginHandler retornou erro: %v", err)
	}

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"campo 'username' deve ter no máximo "+strconv.Itoa(MaxUsernameLength)+" caracteres")
}

func TestLoginHandlerPasswordTooLong(t *testing.T) {
	cfg := config.LoadConfig()
	MaxPasswordLength := cfg.MaxPasswordLength
	body, _ := json.Marshal(map[string]string{"username": "user123", "password": strings.Repeat("a", 65)})
	c := newContext(t, http.MethodPost, "/auth/login", body, newRandomIP())
	rec := recorder(c)

	if err := handlers.LoginHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginHandler retornou erro: %v", err)
	}

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"campo 'password' deve ter no máximo "+strconv.Itoa(MaxPasswordLength)+" caracteres")
}

func TestLoginHandlerInvalidCredentials(t *testing.T) {
	username := newRandomUsername()
	password := newRandomPassword()
	ip := newRandomIP()

	// cria o usuário com uma senha diferente da que será usada no login
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	c := newContext(t, http.MethodPost, "/auth/register", body, ip)
	if err := handlers.RegisterHandler(testBaseURL, c); err != nil {
		t.Fatalf("falha ao registrar usuário: %v", err)
	}
	if rec := recorder(c); rec.Code != http.StatusCreated {
		t.Fatalf("esperava 201 no registro, obtive %d", rec.Code)
	}

	// login com senha incorreta
	body, _ = json.Marshal(map[string]string{"username": username, "password": "senha_errada_" + randHex(4)})
	c = newContext(t, http.MethodPost, "/auth/login", body, ip)
	rec := recorder(c)

	if err := handlers.LoginHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "invalid-credentials", "Credenciais inválidas", "username ou senha incorretos")
}

func TestLoginHandlerNonexistentUser(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"username": newRandomUsername(), "password": newRandomPassword()})
	c := newContext(t, http.MethodPost, "/auth/login", body, newRandomIP())
	rec := recorder(c)

	if err := handlers.LoginHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "invalid-credentials", "Credenciais inválidas", "username ou senha incorretos")
}

func TestLoginHandlerBannedUser(t *testing.T) {
	username := newRandomUsername()
	password := newRandomPassword()
	ip := newRandomIP()

	user, _, err := storage.CreateUser(testCtx(), username, "hash_"+randHex(8), ip)
	if err != nil {
		t.Fatalf("falha ao criar usuário para banir: %v", err)
	}
	if _, err := storage.SetUserBanned(testCtx(), user.ID, true); err != nil {
		t.Fatalf("falha ao banir usuário: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	c := newContext(t, http.MethodPost, "/auth/login", body, ip)
	rec := recorder(c)

	if err := handlers.LoginHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "banned", "Usuário banido", "IP ou usuário banido")
}

func TestLoginHandlerBannedIP(t *testing.T) {
	ip := newRandomIP()
	bannedUser, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), ip)
	if err != nil {
		t.Fatalf("falha ao criar usuário para banir: %v", err)
	}
	if _, err := storage.SetUserBanned(testCtx(), bannedUser.ID, true); err != nil {
		t.Fatalf("falha ao banir usuário: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"username": newRandomUsername(), "password": newRandomPassword()})
	c := newContext(t, http.MethodPost, "/auth/login", body, ip)
	rec := recorder(c)

	if err := handlers.LoginHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "banned", "Usuário banido", "IP ou usuário banido")
}

// --- WhoamiHandler ---

func TestWhoamiHandlerSuccess(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	c := newContext(t, http.MethodGet, "/auth/whoami", nil, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	rec := recorder(c)

	if err := handlers.WhoamiHandler(testBaseURL, c); err != nil {
		t.Fatalf("WhoamiHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID              string     `json:"id"`
		Username        string     `json:"username"`
		Nickname        *string    `json:"nickname"`
		AvatarBlob      []byte     `json:"avatar_blob"`
		AvatarFormat    string     `json:"avatar_format"`
		StatusMessage   *string    `json:"status_message"`
		StatusUpdatedAt *time.Time `json:"status_updated_at"`
		CreatedAt       time.Time  `json:"created_at"`
		Settings        struct {
			Version int               `json:"version"`
			Config  models.UserConfig `json:"config"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID != user.ID {
		t.Errorf("esperava id %s, obtive %s", user.ID, resp.ID)
	}
	if resp.Username != user.Username {
		t.Errorf("esperava username %q, obtive %q", user.Username, resp.Username)
	}
	if resp.Nickname != nil {
		t.Errorf("esperava nickname null, obtive %q", *resp.Nickname)
	}
	if len(resp.AvatarBlob) != 0 {
		t.Errorf("esperava avatar_blob vazio, obtive %v", resp.AvatarBlob)
	}
	if resp.AvatarFormat != "" {
		t.Errorf("esperava avatar_format vazio, obtive %q", resp.AvatarFormat)
	}
	if resp.StatusMessage != nil {
		t.Errorf("esperava status_message null, obtive %q", *resp.StatusMessage)
	}
	if resp.StatusUpdatedAt != nil {
		t.Errorf("esperava status_updated_at null, obtive %v", *resp.StatusUpdatedAt)
	}
	if resp.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}
	if resp.Settings.Version != models.CurrentVersion {
		t.Errorf("esperava settings.version %d, obtive %d", models.CurrentVersion, resp.Settings.Version)
	}
	if resp.Settings.Config != (models.UserConfig{}) {
		t.Errorf("esperava settings.config vazio, obtive %+v", resp.Settings.Config)
	}
}

func TestWhoamiHandlerSuccessWithProfile(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	nickname := "nick_" + randHex(4)
	status := "disponível"
	avatar := []byte{0x89, 0x50, 0x4e, 0x47}
	updatedAt := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := storage.UpdateUser(testCtx(), user.ID, models.User{
		Nickname:        &nickname,
		AvatarBlob:      avatar,
		AvatarFormat:    "PNG",
		StatusMessage:   &status,
		StatusUpdatedAt: &updatedAt,
	}); err != nil {
		t.Fatalf("falha ao atualizar perfil: %v", err)
	}

	c := newContext(t, http.MethodGet, "/auth/whoami", nil, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	rec := recorder(c)

	if err := handlers.WhoamiHandler(testBaseURL, c); err != nil {
		t.Fatalf("WhoamiHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID              string     `json:"id"`
		Username        string     `json:"username"`
		Nickname        *string    `json:"nickname"`
		StatusMessage   *string    `json:"status_message"`
		StatusUpdatedAt *time.Time `json:"status_updated_at"`
		CreatedAt       time.Time  `json:"created_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.Nickname == nil || *resp.Nickname != nickname {
		t.Errorf("esperava nickname %q, obtive %v", nickname, resp.Nickname)
	}
	if resp.StatusMessage == nil || *resp.StatusMessage != status {
		t.Errorf("esperava status_message %q, obtive %v", status, resp.StatusMessage)
	}
	if resp.StatusUpdatedAt == nil || !resp.StatusUpdatedAt.Equal(updatedAt) {
		t.Errorf("esperava status_updated_at %v, obtive %v", updatedAt, resp.StatusUpdatedAt)
	}
}

func TestWhoamiHandlerUserNotFound(t *testing.T) {
	c := newContext(t, http.MethodGet, "/auth/whoami", nil, "")
	c.Set(middleware.UserIDContextKey, randUUID())
	rec := recorder(c)

	if err := handlers.WhoamiHandler(testBaseURL, c); err != nil {
		t.Fatalf("WhoamiHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

func TestWhoamiHandlerMissingUserID(t *testing.T) {
	c := newContext(t, http.MethodGet, "/auth/whoami", nil, "")
	rec := recorder(c)

	if err := handlers.WhoamiHandler(testBaseURL, c); err != nil {
		t.Fatalf("WhoamiHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

// --- LogoutHandler ---

func TestLogoutHandler(t *testing.T) {
	c := newContext(t, http.MethodPost, "/auth/logout", nil, "")
	rec := recorder(c)

	if err := handlers.LogoutHandler(c); err != nil {
		t.Fatalf("LogoutHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "" {
		t.Errorf("esperava corpo vazio, obtive %q", rec.Body.String())
	}

	// o cookie Auth deve ser removido: valor vazio com expiração no passado
	setCookieHeaders := rec.Header().Values("Set-Cookie")
	if len(setCookieHeaders) == 0 {
		t.Fatal("esperava header Set-Cookie definido")
	}
	var foundAuth bool
	for _, header := range setCookieHeaders {
		segment := header
		if idx := strings.Index(header, ";"); idx >= 0 {
			segment = header[:idx]
		}
		if !strings.HasPrefix(segment, "Auth=") {
			continue
		}
		foundAuth = true
		value := strings.TrimPrefix(segment, "Auth=")
		if value != "" {
			t.Errorf("esperava cookie Auth com valor vazio, obtive %q", value)
		}
		lower := strings.ToLower(header)
		if !strings.Contains(lower, "httponly") {
			t.Error("esperava cookie Auth com HttpOnly")
		}
		if !strings.Contains(lower, "secure") {
			t.Error("esperava cookie Auth com Secure")
		}
		if !strings.Contains(lower, "path=/") {
			t.Error("esperava cookie Auth com Path=/")
		}
		if !strings.Contains(lower, "expires=thu, 01 jan 1970 00:00:00 gmt") {
			t.Errorf("esperava cookie Auth expirado em 01/01/1970, obtive %q", header)
		}
	}
	if !foundAuth {
		t.Fatal("esperava cookie Auth no header Set-Cookie")
	}
}

// --- ProfileHandler ---

func TestProfileHandlerSuccess(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	c := newContext(t, http.MethodGet, "/users/profile", nil, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	rec := recorder(c)

	if err := handlers.ProfileHandler(testBaseURL, c); err != nil {
		t.Fatalf("ProfileHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID              string     `json:"id"`
		Username        string     `json:"username"`
		Nickname        *string    `json:"nickname"`
		AvatarBlob      []byte     `json:"avatar_blob"`
		AvatarFormat    string     `json:"avatar_format"`
		StatusMessage   *string    `json:"status_message"`
		StatusUpdatedAt *time.Time `json:"status_updated_at"`
		CreatedAt       time.Time  `json:"created_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID != user.ID {
		t.Errorf("esperava id %s, obtive %s", user.ID, resp.ID)
	}
	if resp.Username != user.Username {
		t.Errorf("esperava username %q, obtive %q", user.Username, resp.Username)
	}
	if resp.Nickname != nil {
		t.Errorf("esperava nickname null, obtive %q", *resp.Nickname)
	}
	if len(resp.AvatarBlob) != 0 {
		t.Errorf("esperava avatar_blob vazio, obtive %v", resp.AvatarBlob)
	}
	if resp.AvatarFormat != "" {
		t.Errorf("esperava avatar_format vazio, obtive %q", resp.AvatarFormat)
	}
	if resp.StatusMessage != nil {
		t.Errorf("esperava status_message null, obtive %q", *resp.StatusMessage)
	}
	if resp.StatusUpdatedAt != nil {
		t.Errorf("esperava status_updated_at null, obtive %v", *resp.StatusUpdatedAt)
	}
	if resp.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}
}

func TestProfileHandlerSuccessWithProfile(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	nickname := "nick_" + randHex(4)
	status := "disponível"
	avatar := []byte{0x89, 0x50, 0x4e, 0x47}
	updatedAt := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := storage.UpdateUser(testCtx(), user.ID, models.User{
		Nickname:        &nickname,
		AvatarBlob:      avatar,
		AvatarFormat:    "PNG",
		StatusMessage:   &status,
		StatusUpdatedAt: &updatedAt,
	}); err != nil {
		t.Fatalf("falha ao atualizar perfil: %v", err)
	}

	c := newContext(t, http.MethodGet, "/users/profile", nil, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	rec := recorder(c)

	if err := handlers.ProfileHandler(testBaseURL, c); err != nil {
		t.Fatalf("ProfileHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID              string     `json:"id"`
		Username        string     `json:"username"`
		Nickname        *string    `json:"nickname"`
		StatusMessage   *string    `json:"status_message"`
		StatusUpdatedAt *time.Time `json:"status_updated_at"`
		CreatedAt       time.Time  `json:"created_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.Nickname == nil || *resp.Nickname != nickname {
		t.Errorf("esperava nickname %q, obtive %v", nickname, resp.Nickname)
	}
	if resp.StatusMessage == nil || *resp.StatusMessage != status {
		t.Errorf("esperava status_message %q, obtive %v", status, resp.StatusMessage)
	}
	if resp.StatusUpdatedAt == nil || !resp.StatusUpdatedAt.Equal(updatedAt) {
		t.Errorf("esperava status_updated_at %v, obtive %v", updatedAt, resp.StatusUpdatedAt)
	}
}

func TestProfileHandlerUserNotFound(t *testing.T) {
	c := newContext(t, http.MethodGet, "/users/profile", nil, "")
	c.Set(middleware.UserIDContextKey, randUUID())
	rec := recorder(c)

	if err := handlers.ProfileHandler(testBaseURL, c); err != nil {
		t.Fatalf("ProfileHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

func TestProfileHandlerMissingUserID(t *testing.T) {
	c := newContext(t, http.MethodGet, "/users/profile", nil, "")
	rec := recorder(c)

	if err := handlers.ProfileHandler(testBaseURL, c); err != nil {
		t.Fatalf("ProfileHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

// --- UpdateUserHandler ---

func TestUpdateUserHandlerSuccess(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	nickname := "nick_" + randHex(4)
	status := "disponível"
	body, _ := json.Marshal(map[string]string{"nickname": nickname, "status": status})
	c := newContext(t, http.MethodPut, "/users/"+user.ID, body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := handlers.UpdateUserHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateUserHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.Response != "User status updated successfully" {
		t.Errorf("esperava response %q, obtive %q", "User status updated successfully", resp.Response)
	}

	// o perfil deve ter sido persistido
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
	if stored.StatusUpdatedAt == nil {
		t.Error("esperava status_updated_at preenchido")
	}
}

func TestUpdateUserHandlerInvalidJSON(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	c := newContext(t, http.MethodPut, "/users/"+user.ID, []byte("{invalido"), "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := handlers.UpdateUserHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateUserHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestUpdateUserHandlerMissingNickname(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"status": "disponível"})
	c := newContext(t, http.MethodPut, "/users/"+user.ID, body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := handlers.UpdateUserHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateUserHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "campo 'nickname' é obrigatório")
}

func TestUpdateUserHandlerMissingStatus(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"nickname": "nick"})
	c := newContext(t, http.MethodPut, "/users/"+user.ID, body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := handlers.UpdateUserHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateUserHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "campo 'status' é obrigatório")
}

func TestUpdateUserHandlerForbiddenOtherUser(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	other, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar segundo usuário: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"nickname": "nick", "status": "disponível"})
	c := newContext(t, http.MethodPut, "/users/"+other.ID, body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(other.ID)
	rec := recorder(c)

	if err := handlers.UpdateUserHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateUserHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado", "não é possível atualizar o perfil de outro usuário")
}

func TestUpdateUserHandlerUserNotFound(t *testing.T) {
	nonexistentID := randUUID()
	body, _ := json.Marshal(map[string]string{"nickname": "nick", "status": "disponível"})
	c := newContext(t, http.MethodPut, "/users/"+nonexistentID, body, "")
	c.Set(middleware.UserIDContextKey, nonexistentID)
	c.SetParamNames("user_id")
	c.SetParamValues(nonexistentID)
	rec := recorder(c)

	if err := handlers.UpdateUserHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateUserHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "usuário não encontrado")
}

func TestUpdateUserHandlerMissingUserID(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"nickname": "nick", "status": "disponível"})
	c := newContext(t, http.MethodPut, "/users/some-id", body, "")
	rec := recorder(c)

	if err := handlers.UpdateUserHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateUserHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

// --- UpdateAvatarHandler ---

func TestUpdateAvatarHandlerSuccess(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	avatar := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	body, _ := json.Marshal(map[string]string{"avatar": avatar, "avatar_format": "PNG"})
	c := newContext(t, http.MethodPut, "/users/"+user.ID+"/avatar", body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := handlers.UpdateAvatarHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateAvatarHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.Response != "User avatar updated successfully" {
		t.Errorf("esperava response %q, obtive %q", "User avatar updated successfully", resp.Response)
	}

	// o avatar deve ter sido persistido
	stored, err := storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if stored.AvatarFormat != "PNG" {
		t.Errorf("esperava avatar_format %q, obtive %q", "PNG", stored.AvatarFormat)
	}
	if !bytes.Equal(stored.AvatarBlob, pngAvatarBytes(100, 100)) {
		t.Errorf("avatar_blob não confere:\n got  %x\n want %x", stored.AvatarBlob, pngAvatarBytes(100, 100))
	}
}

func TestUpdateAvatarHandlerInvalidJSON(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	c := newContext(t, http.MethodPut, "/users/"+user.ID+"/avatar", []byte("{invalido"), "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := handlers.UpdateAvatarHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateAvatarHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestUpdateAvatarHandlerInvalidAvatar(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
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
		{"conteúdo não corresponde ao formato", base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100)), "GIF"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"avatar": tc.avatar, "avatar_format": tc.avatarFormat})
			c := newContext(t, http.MethodPut, "/users/"+user.ID+"/avatar", body, "")
			c.Set(middleware.UserIDContextKey, user.ID)
			c.SetParamNames("user_id")
			c.SetParamValues(user.ID)
			rec := recorder(c)

			if err := handlers.UpdateAvatarHandler(testBaseURL, c); err != nil {
				t.Fatalf("UpdateAvatarHandler retornou erro: %v", err)
			}
			assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
				"avatar inválido: deve ser base64 de um GIF, JPEG ou PNG de até 2MB")
		})
	}
}

func TestUpdateAvatarHandlerForbiddenOtherUser(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	other, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar segundo usuário: %v", err)
	}

	avatar := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	body, _ := json.Marshal(map[string]string{"avatar": avatar, "avatar_format": "PNG"})
	c := newContext(t, http.MethodPut, "/users/"+other.ID+"/avatar", body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(other.ID)
	rec := recorder(c)

	if err := handlers.UpdateAvatarHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateAvatarHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado", "não é possível atualizar o avatar de outro usuário")
}

func TestUpdateAvatarHandlerUserNotFound(t *testing.T) {
	nonexistentID := randUUID()
	avatar := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	body, _ := json.Marshal(map[string]string{"avatar": avatar, "avatar_format": "PNG"})
	c := newContext(t, http.MethodPut, "/users/"+nonexistentID+"/avatar", body, "")
	c.Set(middleware.UserIDContextKey, nonexistentID)
	c.SetParamNames("user_id")
	c.SetParamValues(nonexistentID)
	rec := recorder(c)

	if err := handlers.UpdateAvatarHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateAvatarHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "usuário não encontrado")
}

func TestUpdateAvatarHandlerMissingUserID(t *testing.T) {
	avatar := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	body, _ := json.Marshal(map[string]string{"avatar": avatar, "avatar_format": "PNG"})
	c := newContext(t, http.MethodPut, "/users/some-id/avatar", body, "")
	rec := recorder(c)

	if err := handlers.UpdateAvatarHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateAvatarHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

// --- UpdateSettingsHandler ---

func TestUpdateSettingsHandlerSuccess(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
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
	body, _ := json.Marshal(map[string]models.UserConfig{"config": config})
	c := newContext(t, http.MethodPut, "/users/settings", body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	rec := recorder(c)

	if err := handlers.UpdateSettingsHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateSettingsHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp models.UserSettings
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.UserID != user.ID {
		t.Errorf("esperava user_id %s, obtive %s", user.ID, resp.UserID)
	}
	if resp.Version != models.CurrentVersion {
		t.Errorf("esperava version %d, obtive %d", models.CurrentVersion, resp.Version)
	}
	if resp.Config != config {
		t.Errorf("config não confere:\n got  %+v\n want %+v", resp.Config, config)
	}
	if resp.UpdatedAt.IsZero() {
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
}

func TestUpdateSettingsHandlerInvalidJSON(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	c := newContext(t, http.MethodPut, "/users/settings", []byte("{invalido"), "")
	c.Set(middleware.UserIDContextKey, user.ID)
	rec := recorder(c)

	if err := handlers.UpdateSettingsHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateSettingsHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestUpdateSettingsHandlerInvalidConfig(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	cases := []struct {
		name   string
		config models.UserConfig
	}{
		{"theme inválido", models.UserConfig{Theme: "blue", Display: models.Display{FontSize: "medium", MessageDensity: "normal"}}},
		{"fontSize inválido", models.UserConfig{Theme: "dark", Display: models.Display{FontSize: "large", MessageDensity: "normal"}}},
		{"messageDensity inválido", models.UserConfig{Theme: "dark", Display: models.Display{FontSize: "medium", MessageDensity: "wide"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]models.UserConfig{"config": tc.config})
			c := newContext(t, http.MethodPut, "/users/settings", body, "")
			c.Set(middleware.UserIDContextKey, user.ID)
			rec := recorder(c)

			if err := handlers.UpdateSettingsHandler(testBaseURL, c); err != nil {
				t.Fatalf("UpdateSettingsHandler retornou erro: %v", err)
			}
			assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "campo ausente ou inválido")
		})
	}
}

func TestUpdateSettingsHandlerUserNotFound(t *testing.T) {
	config := models.UserConfig{
		Theme: "dark",
		Display: models.Display{
			FontSize:       "medium",
			MessageDensity: "normal",
		},
	}
	body, _ := json.Marshal(map[string]models.UserConfig{"config": config})
	c := newContext(t, http.MethodPut, "/users/settings", body, "")
	c.Set(middleware.UserIDContextKey, randUUID())
	rec := recorder(c)

	if err := handlers.UpdateSettingsHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateSettingsHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

func TestUpdateSettingsHandlerMissingUserID(t *testing.T) {
	config := models.UserConfig{
		Theme: "dark",
		Display: models.Display{
			FontSize:       "medium",
			MessageDensity: "normal",
		},
	}
	body, _ := json.Marshal(map[string]models.UserConfig{"config": config})
	c := newContext(t, http.MethodPut, "/users/settings", body, "")
	rec := recorder(c)

	if err := handlers.UpdateSettingsHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateSettingsHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

// --- ResetUserHandler ---

func TestResetUserHandlerSuccess(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	c := newContext(t, http.MethodPost, "/users/"+user.ID+"/reset", nil, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := handlers.ResetUserHandler(testBaseURL, c); err != nil {
		t.Fatalf("ResetUserHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.Response != "User password is set to reset" {
		t.Errorf("esperava response %q, obtive %q", "User password is set to reset", resp.Response)
	}

	stored, err := storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if !stored.ResetPassword {
		t.Error("esperava reset_password = true persistido")
	}
}

func TestResetUserHandlerMissingAuth(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	c := newContext(t, http.MethodPost, "/users/"+user.ID+"/reset", nil, "")
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := handlers.ResetUserHandler(testBaseURL, c); err != nil {
		t.Fatalf("ResetUserHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

func TestResetUserHandlerMissingUserID(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	c := newContext(t, http.MethodPost, "/users//reset", nil, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues("")
	rec := recorder(c)

	if err := handlers.ResetUserHandler(testBaseURL, c); err != nil {
		t.Fatalf("ResetUserHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "user_id ausente")
}

func TestResetUserHandlerNonexistentUser(t *testing.T) {
	actor, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	nonexistentID := randUUID()

	c := newContext(t, http.MethodPost, "/users/"+nonexistentID+"/reset", nil, "")
	c.Set(middleware.UserIDContextKey, actor.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(nonexistentID)
	rec := recorder(c)

	if err := handlers.ResetUserHandler(testBaseURL, c); err != nil {
		t.Fatalf("ResetUserHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "usuário não encontrado")
}

// --- ListUsersHandler (tarefa 6.4) ---

func TestListUsersHandler(t *testing.T) {
	authUser, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	other, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	c := newContext(t, http.MethodGet, "/users", nil, "")
	c.Set(middleware.UserIDContextKey, authUser.ID)
	rec := recorder(c)

	if err := handlers.ListUsersHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListUsersHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	if strings.Contains(rec.Body.String(), "password_hash") {
		t.Error("a listagem não deveria expor password_hash")
	}

	var resp struct {
		Users []struct {
			ID              string     `json:"id"`
			Username        string     `json:"username"`
			Nickname        *string    `json:"nickname"`
			StatusMessage   *string    `json:"status_message"`
			StatusUpdatedAt *time.Time `json:"status_updated_at"`
			CreatedAt       time.Time  `json:"created_at"`
		} `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}

	byID := make(map[string]string, len(resp.Users))
	for _, u := range resp.Users {
		byID[u.ID] = u.Username
	}
	if byID[authUser.ID] != authUser.Username {
		t.Error("usuário autenticado não aparece na listagem com o username correto")
	}
	if byID[other.ID] != other.Username {
		t.Error("outro usuário não aparece na listagem com o username correto")
	}
}

func TestListUsersHandlerMissingAuth(t *testing.T) {
	c := newContext(t, http.MethodGet, "/users", nil, "")
	rec := recorder(c)

	if err := handlers.ListUsersHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListUsersHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

// --- ChangePasswordHandler (tarefa 6.4) ---

func TestChangePasswordHandlerSuccess(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	if err := storage.SetUserResetPassword(testCtx(), user.ID); err != nil {
		t.Fatalf("falha ao marcar usuário para reset: %v", err)
	}

	newPassword := newRandomPassword()
	body, _ := json.Marshal(map[string]string{"password": newPassword})

	c := newContext(t, http.MethodPut, "/users/"+user.ID+"/password", body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := handlers.ChangePasswordHandler(testBaseURL, c); err != nil {
		t.Fatalf("ChangePasswordHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.Response != "User password updated successfully" {
		t.Errorf("esperava response %q, obtive %q", "User password updated successfully", resp.Response)
	}

	stored, err := storage.GetUserByUsername(testCtx(), user.Username)
	if err != nil {
		t.Fatalf("GetUserByUsername retornou erro: %v", err)
	}
	if err := utils.CheckPassword(newPassword, stored.PasswordHash); err != nil {
		t.Errorf("CheckPassword falhou para o novo hash persistido: %v", err)
	}
	if stored.ResetPassword {
		t.Error("esperava reset_password = false após trocar a senha")
	}
}

func TestChangePasswordHandlerMissingAuth(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"password": newRandomPassword()})
	c := newContext(t, http.MethodPut, "/users/"+user.ID+"/password", body, "")
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := handlers.ChangePasswordHandler(testBaseURL, c); err != nil {
		t.Fatalf("ChangePasswordHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

func TestChangePasswordHandlerMissingUserID(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"password": newRandomPassword()})
	c := newContext(t, http.MethodPut, "/users//password", body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues("")
	rec := recorder(c)

	if err := handlers.ChangePasswordHandler(testBaseURL, c); err != nil {
		t.Fatalf("ChangePasswordHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "user_id ausente")
}

func TestChangePasswordHandlerInvalidJSON(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	c := newContext(t, http.MethodPut, "/users/"+user.ID+"/password", []byte("{invalido"), "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := handlers.ChangePasswordHandler(testBaseURL, c); err != nil {
		t.Fatalf("ChangePasswordHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestChangePasswordHandlerEmptyPassword(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"password": ""})
	c := newContext(t, http.MethodPut, "/users/"+user.ID+"/password", body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)
	err = storage.SetUserResetPassword(testCtx(), user.ID)

	if err != nil {
		t.Fatalf("SetUserResetPassword retornou erro: %v", err)
	}

	if err := handlers.ChangePasswordHandler(testBaseURL, c); err != nil {
		t.Fatalf("ChangePasswordHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "campo 'password' é obrigatório")
}

func TestChangePasswordHandlerPasswordTooLong(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	err = storage.SetUserResetPassword(testCtx(), user.ID)

	if err != nil {
		t.Fatalf("SetUserResetPassword retornou erro: %v", err)
	}

	longPassword := strings.Repeat("a", config.LoadConfig().MaxPasswordLength+1)
	body, _ := json.Marshal(map[string]string{"password": longPassword})
	c := newContext(t, http.MethodPut, "/users/"+user.ID+"/password", body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := handlers.ChangePasswordHandler(testBaseURL, c); err != nil {
		t.Fatalf("ChangePasswordHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "campo 'password' é obrigatório")
}

func TestChangePasswordHandlerForbiddenOtherUser(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	actor, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"password": newRandomPassword()})
	c := newContext(t, http.MethodPut, "/users/"+user.ID+"/password", body, "")
	c.Set(middleware.UserIDContextKey, actor.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := handlers.ChangePasswordHandler(testBaseURL, c); err != nil {
		t.Fatalf("ChangePasswordHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"não é possível alterar a senha de outro usuário")
}

// --- ListServersHandler (tarefa 5.2) ---

func TestListServersHandlerEmpty(t *testing.T) {
	c := newContext(t, http.MethodGet, "/servers", nil, "")
	rec := recorder(c)

	if err := handlers.ListServersHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListServersHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Servers []struct {
			ID string `json:"id"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.Servers == nil {
		t.Fatal("esperava servers como lista vazia, obtive null")
	}
	if len(resp.Servers) != 0 {
		t.Errorf("esperava lista de servidores vazia, obtive %d", len(resp.Servers))
	}
}

func TestListServersHandlerSuccess(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	serverName := "srv_" + randHex(4)
	server, err := storage.CreateServer(testCtx(), serverName, &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	if _, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text"); err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	c := newContext(t, http.MethodGet, "/servers", nil, "")
	rec := recorder(c)

	if err := handlers.ListServersHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListServersHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Servers []struct {
			ID            string    `json:"id"`
			Name          string    `json:"name"`
			IconBlob      []byte    `json:"icon_blob"`
			IconFormat    string    `json:"icon_format"`
			OwnerID       *string   `json:"owner_id"`
			OwnerUsername *string   `json:"owner_username"`
			CreatedAt     time.Time `json:"created_at"`
			ChannelCount  int       `json:"channel_count"`
			MemberCount   int       `json:"member_count"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if len(resp.Servers) != 1 {
		t.Fatalf("esperava 1 servidor na listagem, obtive %d", len(resp.Servers))
	}
	item := resp.Servers[0]
	if item.ID != server.ID {
		t.Errorf("esperava id %s, obtive %s", server.ID, item.ID)
	}
	if item.Name != serverName {
		t.Errorf("esperava name %q, obtive %q", serverName, item.Name)
	}
	if len(item.IconBlob) != 0 {
		t.Errorf("esperava icon_blob vazio, obtive %v", item.IconBlob)
	}
	if item.IconFormat != "" {
		t.Errorf("esperava icon_format vazio, obtive %q", item.IconFormat)
	}
	if item.OwnerID == nil || *item.OwnerID != owner.ID {
		t.Errorf("esperava owner_id %s, obtive %v", owner.ID, item.OwnerID)
	}
	if item.OwnerUsername == nil || *item.OwnerUsername != owner.Username {
		t.Errorf("esperava owner_username %q, obtive %v", owner.Username, item.OwnerUsername)
	}
	if item.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}
	if item.ChannelCount != 1 {
		t.Errorf("esperava channel_count 1, obtive %d", item.ChannelCount)
	}
	if item.MemberCount < 1 {
		t.Errorf("esperava member_count >= 1, obtive %d", item.MemberCount)
	}

	// a listagem não inclui role_count (presente apenas no detalhe)
	var rawResp struct {
		Servers []map[string]json.RawMessage `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rawResp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	for _, rawItem := range rawResp.Servers {
		if _, ok := rawItem["role_count"]; ok {
			t.Error("a listagem não deve incluir role_count")
		}
	}
}

// --- GetServerHandler

func TestGetServerHandlerSuccess(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	if _, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text"); err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	if _, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(4), nil, models.RolePermissions{}); err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	c := newContext(t, http.MethodGet, "/servers/"+server.ID, nil, "")
	c.SetParamNames("server_id")
	c.SetParamValues(server.ID)
	rec := recorder(c)

	if err := handlers.GetServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("GetServerHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID            string    `json:"id"`
		Name          string    `json:"name"`
		IconBlob      []byte    `json:"icon_blob"`
		IconFormat    string    `json:"icon_format"`
		OwnerID       *string   `json:"owner_id"`
		OwnerUsername *string   `json:"owner_username"`
		CreatedAt     time.Time `json:"created_at"`
		RoleCount     int       `json:"role_count"`
		MemberCount   int       `json:"member_count"`
		ChannelCount  int       `json:"channel_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID != server.ID {
		t.Errorf("esperava id %s, obtive %s", server.ID, resp.ID)
	}
	if resp.Name != server.Name {
		t.Errorf("esperava name %q, obtive %q", server.Name, resp.Name)
	}
	if len(resp.IconBlob) != 0 {
		t.Errorf("esperava icon_blob vazio, obtive %v", resp.IconBlob)
	}
	if resp.IconFormat != "" {
		t.Errorf("esperava icon_format vazio, obtive %q", resp.IconFormat)
	}
	if resp.OwnerID == nil || *resp.OwnerID != owner.ID {
		t.Errorf("esperava owner_id %s, obtive %v", owner.ID, resp.OwnerID)
	}
	if resp.OwnerUsername == nil || *resp.OwnerUsername != owner.Username {
		t.Errorf("esperava owner_username %q, obtive %v", owner.Username, resp.OwnerUsername)
	}
	if resp.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}
	if resp.RoleCount != 1 {
		t.Errorf("esperava role_count 1, obtive %d", resp.RoleCount)
	}
	if resp.ChannelCount != 1 {
		t.Errorf("esperava channel_count 1, obtive %d", resp.ChannelCount)
	}
	if resp.MemberCount < 1 {
		t.Errorf("esperava member_count >= 1, obtive %d", resp.MemberCount)
	}
}

func TestGetServerHandlerNotFound(t *testing.T) {
	serverID := randUUID()
	c := newContext(t, http.MethodGet, "/servers/"+serverID, nil, "")
	c.SetParamNames("server_id")
	c.SetParamValues(serverID)
	rec := recorder(c)

	if err := handlers.GetServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("GetServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "servidor não encontrado")
}

func TestGetServerHandlerMissingParam(t *testing.T) {
	c := newContext(t, http.MethodGet, "/servers/", nil, "")
	c.SetParamNames("server_id")
	c.SetParamValues("")
	rec := recorder(c)

	if err := handlers.GetServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("GetServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "server_id ausente")
}

// --- UpdateServerHandler ---

func TestUpdateServerHandlerSuccess(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário dono: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	// segundo servidor para garantir que o campo "id" do corpo é ignorado
	other, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar segundo servidor: %v", err)
	}

	newName := "srv_" + randHex(4)
	body, _ := json.Marshal(map[string]string{
		"id":          other.ID,
		"name":        newName,
		"icon_blob":   base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100)),
		"icon_format": "png",
	})
	c := newContext(t, http.MethodPut, "/servers/"+server.ID, body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("server_id")
	c.SetParamValues(server.ID)
	rec := recorder(c)

	if err := handlers.UpdateServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateServerHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID            string    `json:"id"`
		Name          string    `json:"name"`
		IconBlob      []byte    `json:"icon_blob"`
		IconFormat    string    `json:"icon_format"`
		OwnerID       *string   `json:"owner_id"`
		OwnerUsername *string   `json:"owner_username"`
		CreatedAt     time.Time `json:"created_at"`
		RoleCount     int       `json:"role_count"`
		MemberCount   int       `json:"member_count"`
		ChannelCount  int       `json:"channel_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID != server.ID {
		t.Errorf("esperava id %s, obtive %s", server.ID, resp.ID)
	}
	if resp.Name != newName {
		t.Errorf("esperava name %q, obtive %q", newName, resp.Name)
	}
	if !bytes.Equal(resp.IconBlob, pngAvatarBytes(100, 100)) {
		t.Errorf("icon_blob não confere:\n got  %x\n want %x", resp.IconBlob, pngAvatarBytes(100, 100))
	}
	// o formato deve ser normalizado para maiúsculas
	if resp.IconFormat != "PNG" {
		t.Errorf("esperava icon_format %q, obtive %q", "PNG", resp.IconFormat)
	}
	if resp.OwnerID == nil || *resp.OwnerID != owner.ID {
		t.Errorf("esperava owner_id %s, obtive %v", owner.ID, resp.OwnerID)
	}
	if resp.OwnerUsername == nil || *resp.OwnerUsername != owner.Username {
		t.Errorf("esperava owner_username %q, obtive %v", owner.Username, resp.OwnerUsername)
	}
	if resp.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}
	if resp.RoleCount != 0 {
		t.Errorf("esperava role_count 0, obtive %d", resp.RoleCount)
	}
	if resp.ChannelCount != 0 {
		t.Errorf("esperava channel_count 0, obtive %d", resp.ChannelCount)
	}
	if resp.MemberCount < 1 {
		t.Errorf("esperava member_count >= 1, obtive %d", resp.MemberCount)
	}

	// a atualização deve ter sido persistida no servidor da URL
	stored, err := storage.GetServerByID(testCtx(), server.ID)
	if err != nil {
		t.Fatalf("GetServerByID retornou erro: %v", err)
	}
	if stored.Name != newName {
		t.Errorf("esperava name %q persistido, obtive %q", newName, stored.Name)
	}
	if !bytes.Equal(stored.IconBlob, pngAvatarBytes(100, 100)) {
		t.Errorf("icon_blob persistido não confere: %x", stored.IconBlob)
	}
	if stored.IconFormat != "PNG" {
		t.Errorf("esperava icon_format %q persistido, obtive %q", "PNG", stored.IconFormat)
	}

	// o "id" do corpo é ignorado: o outro servidor não deve ter mudado
	storedOther, err := storage.GetServerByID(testCtx(), other.ID)
	if err != nil {
		t.Fatalf("GetServerByID retornou erro: %v", err)
	}
	if storedOther.Name != other.Name {
		t.Errorf("o servidor do id do corpo não deveria ter mudado: esperado %q, obtive %q", other.Name, storedOther.Name)
	}
	if len(storedOther.IconBlob) != 0 {
		t.Errorf("o servidor do id do corpo não deveria ter ícone, obtive %x", storedOther.IconBlob)
	}
}

func TestUpdateServerHandlerNotFound(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	serverID := randUUID()
	body, _ := json.Marshal(map[string]string{"name": "srv_" + randHex(4)})
	c := newContext(t, http.MethodPut, "/servers/"+serverID, body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("server_id")
	c.SetParamValues(serverID)
	rec := recorder(c)

	if err := handlers.UpdateServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "servidor não encontrado")
}

func TestUpdateServerHandlerMissingUser(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"name": "srv_" + randHex(4)})
	c := newContext(t, http.MethodPut, "/servers/"+randUUID(), body, "")
	rec := recorder(c)

	if err := handlers.UpdateServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "server_id ausente")
}

func TestUpdateServerHandlerMissingParam(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"name": "srv_" + randHex(4)})
	c := newContext(t, http.MethodPut, "/servers/", body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("server_id")
	c.SetParamValues("")
	rec := recorder(c)

	if err := handlers.UpdateServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "server_id ausente")
}

func TestUpdateServerHandlerInvalidJSON(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário dono: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	c := newContext(t, http.MethodPut, "/servers/"+server.ID, []byte("{invalido"), "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("server_id")
	c.SetParamValues(server.ID)
	rec := recorder(c)

	if err := handlers.UpdateServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestUpdateServerHandlerInvalidInput(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário dono: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	validIcon := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))

	cases := []struct {
		name string
		body map[string]string
	}{
		{"nome ausente", map[string]string{"icon_blob": validIcon, "icon_format": "PNG"}},
		{"ícone com base64 inválido", map[string]string{"name": "srv_" + randHex(4), "icon_blob": "!!!nao-e-base64!!!", "icon_format": "PNG"}},
		{"ícone não corresponde ao formato", map[string]string{"name": "srv_" + randHex(4), "icon_blob": validIcon, "icon_format": "GIF"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(tc.body)
			c := newContext(t, http.MethodPut, "/servers/"+server.ID, body, "")
			c.Set(middleware.UserIDContextKey, owner.ID)
			c.SetParamNames("server_id")
			c.SetParamValues(server.ID)
			rec := recorder(c)

			if err := handlers.UpdateServerHandler(testBaseURL, c); err != nil {
				t.Fatalf("UpdateServerHandler retornou erro: %v", err)
			}
			assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
				"name é obrigatório e deve ter no máximo 32 caracteres; icon_blob deve ser base64 de um GIF, JPEG ou PNG de até 2MB servidor privado (public=false) exige password")
		})
	}

	// tentativas inválidas não devem alterar o servidor
	stored, err := storage.GetServerByID(testCtx(), server.ID)
	if err != nil {
		t.Fatalf("GetServerByID retornou erro: %v", err)
	}
	if stored.Name != server.Name {
		t.Errorf("name mudou após tentativas inválidas: esperado %q, obtive %q", server.Name, stored.Name)
	}
	if len(stored.IconBlob) != 0 {
		t.Errorf("esperava icon_blob vazio, obtive %x", stored.IconBlob)
	}
}

// --- CreateServerHandler ---

func TestCreateServerHandlerSuccess(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	name := "srv_" + randHex(4)
	body, _ := json.Marshal(map[string]string{
		"name":        name,
		"icon_blob":   base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100)),
		"icon_format": "png",
	})
	c := newContext(t, http.MethodPost, "/servers", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := handlers.CreateServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateServerHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID            string    `json:"id"`
		Name          string    `json:"name"`
		IconBlob      []byte    `json:"icon_blob"`
		IconFormat    string    `json:"icon_format"`
		OwnerID       *string   `json:"owner_id"`
		OwnerUsername *string   `json:"owner_username"`
		CreatedAt     time.Time `json:"created_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID == "" {
		t.Error("esperava id preenchido")
	}
	if resp.Name != name {
		t.Errorf("esperava name %q, obtive %q", name, resp.Name)
	}
	if !bytes.Equal(resp.IconBlob, pngAvatarBytes(100, 100)) {
		t.Errorf("icon_blob não confere:\n got  %x\n want %x", resp.IconBlob, pngAvatarBytes(100, 100))
	}
	// o formato deve ser normalizado para maiúsculas
	if resp.IconFormat != "PNG" {
		t.Errorf("esperava icon_format %q, obtive %q", "PNG", resp.IconFormat)
	}
	if resp.OwnerID == nil || *resp.OwnerID != owner.ID {
		t.Errorf("esperava owner_id %s, obtive %v", owner.ID, resp.OwnerID)
	}
	if resp.OwnerUsername == nil || *resp.OwnerUsername != owner.Username {
		t.Errorf("esperava owner_username %q, obtive %v", owner.Username, resp.OwnerUsername)
	}
	if resp.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}

	// a resposta do POST não inclui contagens (presentes apenas na listagem/detalhe)
	var rawResp map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &rawResp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	for _, field := range []string{"channel_count", "member_count", "role_count"} {
		if _, ok := rawResp[field]; ok {
			t.Errorf("a resposta do POST não deve incluir %s", field)
		}
	}

	// o servidor deve ter sido persistido com o usuário como dono
	stored, err := storage.GetServerByID(testCtx(), resp.ID)
	if err != nil {
		t.Fatalf("GetServerByID retornou erro: %v", err)
	}
	if stored.Name != name {
		t.Errorf("esperava name %q persistido, obtive %q", name, stored.Name)
	}
	if stored.OwnerID == nil || *stored.OwnerID != owner.ID {
		t.Errorf("esperava owner_id %s persistido, obtive %v", owner.ID, stored.OwnerID)
	}
	if !bytes.Equal(stored.IconBlob, pngAvatarBytes(100, 100)) {
		t.Errorf("icon_blob persistido não confere: %x", stored.IconBlob)
	}
}

func TestCreateServerHandlerNoIcon(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"name": "srv_" + randHex(4)})
	c := newContext(t, http.MethodPost, "/servers", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := handlers.CreateServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateServerHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID         string `json:"id"`
		IconBlob   []byte `json:"icon_blob"`
		IconFormat string `json:"icon_format"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID == "" {
		t.Error("esperava id preenchido")
	}
	if len(resp.IconBlob) != 0 {
		t.Errorf("esperava icon_blob vazio, obtive %v", resp.IconBlob)
	}
	if resp.IconFormat != "" {
		t.Errorf("esperava icon_format vazio, obtive %q", resp.IconFormat)
	}
}

func TestCreateServerHandlerMissingUser(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"name": "srv_" + randHex(4)})
	c := newContext(t, http.MethodPost, "/servers", body, "")
	rec := recorder(c)

	if err := handlers.CreateServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

func TestCreateServerHandlerInvalidJSON(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	c := newContext(t, http.MethodPost, "/servers", []byte("{invalido"), "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := handlers.CreateServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestCreateServerHandlerInvalidInput(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	before, err := storage.ListServers(testCtx())
	if err != nil {
		t.Fatalf("ListServers retornou erro: %v", err)
	}

	validIcon := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))

	cases := []struct {
		name string
		body map[string]string
	}{
		{"nome ausente", map[string]string{"icon_format": "PNG"}},
		{"nome acima de 32 caracteres", map[string]string{"name": strings.Repeat("a", 33)}},
		{"ícone com base64 inválido", map[string]string{"name": "srv_" + randHex(4), "icon_blob": "!!!nao-e-base64!!!", "icon_format": "PNG"}},
		{"ícone não corresponde ao formato", map[string]string{"name": "srv_" + randHex(4), "icon_blob": validIcon, "icon_format": "GIF"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(tc.body)
			c := newContext(t, http.MethodPost, "/servers", body, "")
			c.Set(middleware.UserIDContextKey, owner.ID)
			rec := recorder(c)

			if err := handlers.CreateServerHandler(testBaseURL, c); err != nil {
				t.Fatalf("CreateServerHandler retornou erro: %v", err)
			}
			assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
				"name é obrigatório e deve ter no máximo 32 caracteres; icon_blob deve ser base64 de um GIF, JPEG ou PNG de até 2MB; servidor privado (public=false) exige password")
		})
	}

	// tentativas inválidas não devem criar servidores
	after, err := storage.ListServers(testCtx())
	if err != nil {
		t.Fatalf("ListServers retornou erro: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("tentativas inválidas não deveriam criar servidores: %d antes, %d depois", len(before), len(after))
	}
}

// --- ListChannelsHandler (tarefa 5.4) ---

func TestListChannelsHandlerEmpty(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	c := newContext(t, http.MethodGet, "/channels?server_id="+server.ID, nil, "")
	rec := recorder(c)

	if err := handlers.ListChannelsHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListChannelsHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Channels []models.ChannelSummary `json:"channels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.Channels == nil {
		t.Fatal("esperava channels como lista vazia, obtive null")
	}
	if len(resp.Channels) != 0 {
		t.Errorf("esperava lista de canais vazia, obtive %d", len(resp.Channels))
	}
}

func TestListChannelsHandlerSuccess(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channelA, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	channelB, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	c := newContext(t, http.MethodGet, "/channels", nil, "")
	rec := recorder(c)

	if err := handlers.ListChannelsHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListChannelsHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Channels []models.ChannelSummary `json:"channels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}

	found := make(map[string]models.ChannelSummary)
	for _, item := range resp.Channels {
		found[item.ID] = item
	}

	for _, want := range []models.Channel{channelA, channelB} {
		item, ok := found[want.ID]
		if !ok {
			t.Errorf("canal %s não encontrado na listagem", want.ID)
			continue
		}
		if item.ServerID != server.ID {
			t.Errorf("esperava server_id %s, obtive %s", server.ID, item.ServerID)
		}
		if item.Name != want.Name {
			t.Errorf("esperava name %q, obtive %q", want.Name, item.Name)
		}
		if item.Type != "text" {
			t.Errorf("esperava type %q, obtive %q", "text", item.Type)
		}
		if len(item.Permissions) != 0 {
			t.Errorf("esperava permissions vazia, obtive %d entradas", len(item.Permissions))
		}
		if item.CreatedAt.IsZero() {
			t.Error("esperava created_at preenchido")
		}
		if item.LastMessage != nil {
			t.Errorf("esperava last_message null, obtive %+v", item.LastMessage)
		}
	}
}

func TestListChannelsHandlerFilterByServer(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	serverA, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	serverB, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar segundo servidor: %v", err)
	}
	channelA, err := storage.CreateChannel(testCtx(), serverA.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	if _, err := storage.CreateChannel(testCtx(), serverB.ID, "canal_"+randHex(4), "text"); err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	c := newContext(t, http.MethodGet, "/channels?server_id="+serverA.ID, nil, "")
	rec := recorder(c)

	if err := handlers.ListChannelsHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListChannelsHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Channels []models.ChannelSummary `json:"channels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if len(resp.Channels) != 1 {
		t.Fatalf("esperava 1 canal do servidor filtrado, obtive %d", len(resp.Channels))
	}
	if resp.Channels[0].ID != channelA.ID {
		t.Errorf("esperava id %s, obtive %s", channelA.ID, resp.Channels[0].ID)
	}
	if resp.Channels[0].ServerID != serverA.ID {
		t.Errorf("esperava server_id %s, obtive %s", serverA.ID, resp.Channels[0].ServerID)
	}
}

// --- CreateChannelHandler (tarefa 5.4) ---

func TestCreateChannelHandlerSuccess(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channelName := "canal_" + randHex(4)

	body, _ := json.Marshal(map[string]string{"server_id": server.ID, "name": channelName})
	c := newContext(t, http.MethodPost, "/channels", body, "")
	rec := recorder(c)

	if err := handlers.CreateChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateChannelHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp models.ChannelSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID == "" {
		t.Error("esperava id preenchido")
	}
	if resp.ServerID != server.ID {
		t.Errorf("esperava server_id %s, obtive %s", server.ID, resp.ServerID)
	}
	if resp.Name != channelName {
		t.Errorf("esperava name %q, obtive %q", channelName, resp.Name)
	}
	if resp.Type != "text" {
		t.Errorf("esperava type %q, obtive %q", "text", resp.Type)
	}
	if len(resp.Permissions) != 0 {
		t.Errorf("esperava permissions vazia, obtive %d entradas", len(resp.Permissions))
	}
	if resp.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}
	if resp.LastMessage != nil {
		t.Errorf("esperava last_message null, obtive %+v", resp.LastMessage)
	}

	// o canal deve ter sido persistido
	stored, err := storage.GetChannelByID(testCtx(), resp.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Name != channelName {
		t.Errorf("esperava name %q persistido, obtive %q", channelName, stored.Name)
	}
	if stored.Type != "text" {
		t.Errorf("esperava type %q persistido, obtive %q", "text", stored.Type)
	}
}

func TestCreateChannelHandlerInvalidJSON(t *testing.T) {
	c := newContext(t, http.MethodPost, "/channels", []byte("{invalido"), "")
	rec := recorder(c)

	if err := handlers.CreateChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestCreateChannelHandlerMissingServerID(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"name": "canal_" + randHex(4)})
	c := newContext(t, http.MethodPost, "/channels", body, "")
	rec := recorder(c)

	if err := handlers.CreateChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "server_id e name são obrigatórios; name deve ter no máximo 32 caracteres")
}

func TestCreateChannelHandlerMissingName(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"server_id": server.ID})
	c := newContext(t, http.MethodPost, "/channels", body, "")
	rec := recorder(c)

	if err := handlers.CreateChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "server_id e name são obrigatórios; name deve ter no máximo 32 caracteres")
}

func TestCreateChannelHandlerServerNotFound(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"server_id": randUUID(), "name": "canal_" + randHex(4)})
	c := newContext(t, http.MethodPost, "/channels", body, "")
	rec := recorder(c)

	if err := handlers.CreateChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "servidor não encontrado")
}

func TestCreateChannelHandlerNameTaken(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channelName := "canal_" + randHex(4)
	if _, err := storage.CreateChannel(testCtx(), server.ID, channelName, "text"); err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	// o nome é UNIQUE global na tabela channels: o mesmo nome em outro servidor também conflita
	body, _ := json.Marshal(map[string]string{"server_id": server.ID, "name": channelName})
	c := newContext(t, http.MethodPost, "/channels", body, "")
	rec := recorder(c)

	if err := handlers.CreateChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusConflict, "channel-name-taken", "Nome de canal já existe", "o nome informado já está em uso")
}

// --- UpdateChannelHandler (tarefa 5.4) ---

func TestUpdateChannelHandlerSuccess(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	newName := "canal_" + randHex(4)

	body, _ := json.Marshal(map[string]string{"name": newName})
	c := newContext(t, http.MethodPut, "/channels/"+channel.ID, body, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	rec := recorder(c)

	if err := handlers.UpdateChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp models.ChannelSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID != channel.ID {
		t.Errorf("esperava id %s, obtive %s", channel.ID, resp.ID)
	}
	if resp.ServerID != server.ID {
		t.Errorf("esperava server_id %s, obtive %s", server.ID, resp.ServerID)
	}
	if resp.Name != newName {
		t.Errorf("esperava name %q, obtive %q", newName, resp.Name)
	}
	if resp.Type != "text" {
		t.Errorf("esperava type %q, obtive %q", "text", resp.Type)
	}
	if len(resp.Permissions) != 0 {
		t.Errorf("esperava permissions vazia, obtive %d entradas", len(resp.Permissions))
	}
	if resp.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}
	if resp.LastMessage != nil {
		t.Errorf("esperava last_message null, obtive %+v", resp.LastMessage)
	}

	// o nome deve ter sido persistido
	stored, err := storage.GetChannelByID(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Name != newName {
		t.Errorf("esperava name %q persistido, obtive %q", newName, stored.Name)
	}
}

func TestUpdateChannelHandlerMissingParam(t *testing.T) {
	c := newContext(t, http.MethodPut, "/channels/", []byte(`{"name":"canal"}`), "")
	c.SetParamNames("channel_id")
	c.SetParamValues("")
	rec := recorder(c)

	if err := handlers.UpdateChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "channel_id ausente")
}

func TestUpdateChannelHandlerInvalidJSON(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	c := newContext(t, http.MethodPut, "/channels/"+channel.ID, []byte("{invalido"), "")
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	rec := recorder(c)

	if err := handlers.UpdateChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestUpdateChannelHandlerMissingName(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"name": ""})
	c := newContext(t, http.MethodPut, "/channels/"+channel.ID, body, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	rec := recorder(c)

	if err := handlers.UpdateChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "name é obrigatório e deve ter no máximo 32 caracteres")
}

func TestUpdateChannelHandlerNotFound(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"name": "canal_" + randHex(4)})
	c := newContext(t, http.MethodPut, "/channels/"+randUUID(), body, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(randUUID())
	rec := recorder(c)

	if err := handlers.UpdateChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

func TestUpdateChannelHandlerNameTaken(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	takenName := "canal_" + randHex(4)
	if _, err := storage.CreateChannel(testCtx(), server.ID, takenName, "text"); err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"name": takenName})
	c := newContext(t, http.MethodPut, "/channels/"+channel.ID, body, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	rec := recorder(c)

	if err := handlers.UpdateChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusConflict, "channel-name-taken", "Nome de canal já existe", "o nome informado já está em uso")

	// a renomeação inválida não deve alterar o canal
	stored, err := storage.GetChannelByID(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Name != channel.Name {
		t.Errorf("o canal não deveria ter mudado: esperado %q, obtive %q", channel.Name, stored.Name)
	}
}

// --- DeleteChannelHandler (tarefa 5.4) ---

func TestDeleteChannelHandlerSuccess(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	c := newContext(t, http.MethodDelete, "/channels/"+channel.ID, nil, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	rec := recorder(c)

	if err := handlers.DeleteChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteChannelHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "" {
		t.Errorf("esperava corpo vazio, obtive %q", rec.Body.String())
	}

	// o canal deve ter sido removido
	if _, err := storage.GetChannelByID(testCtx(), channel.ID); err == nil {
		t.Error("esperava o canal removido, mas ele ainda existe")
	}
}

func TestDeleteChannelHandlerMissingParam(t *testing.T) {
	c := newContext(t, http.MethodDelete, "/channels/", nil, "")
	c.SetParamNames("channel_id")
	c.SetParamValues("")
	rec := recorder(c)

	if err := handlers.DeleteChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "channel_id ausente")
}

func TestDeleteChannelHandlerNotFound(t *testing.T) {
	c := newContext(t, http.MethodDelete, "/channels/"+randUUID(), nil, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(randUUID())
	rec := recorder(c)

	if err := handlers.DeleteChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

// --- GetChannelPermissionsHandler (tarefa 5.4) ---

func TestGetChannelPermissionsHandlerEmpty(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	c := newContext(t, http.MethodGet, "/channels/"+channel.ID+"/permissions", nil, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	rec := recorder(c)

	if err := handlers.GetChannelPermissionsHandler(testBaseURL, c); err != nil {
		t.Fatalf("GetChannelPermissionsHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		ChannelID   string                          `json:"channel_id"`
		Permissions []models.ChannelPermissionEntry `json:"permissions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ChannelID != channel.ID {
		t.Errorf("esperava channel_id %s, obtive %s", channel.ID, resp.ChannelID)
	}
	if resp.Permissions == nil {
		t.Fatal("esperava permissions como lista vazia, obtive null")
	}
	if len(resp.Permissions) != 0 {
		t.Errorf("esperava permissions vazia, obtive %d entradas", len(resp.Permissions))
	}
}

func TestGetChannelPermissionsHandlerSuccess(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	roleName := "role_" + randHex(4)
	role, err := storage.CreateRole(testCtx(), server.ID, roleName, nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	permissions := models.ChannelPermission{ReadChannel: true, SendMessages: false, DeleteMessages: true}
	if _, err := storage.UpdateChannelPermissions(testCtx(), channel.ID, role.ID, permissions); err != nil {
		t.Fatalf("falha ao atualizar permissões do canal: %v", err)
	}

	c := newContext(t, http.MethodGet, "/channels/"+channel.ID+"/permissions", nil, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	rec := recorder(c)

	if err := handlers.GetChannelPermissionsHandler(testBaseURL, c); err != nil {
		t.Fatalf("GetChannelPermissionsHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		ChannelID   string                          `json:"channel_id"`
		Permissions []models.ChannelPermissionEntry `json:"permissions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ChannelID != channel.ID {
		t.Errorf("esperava channel_id %s, obtive %s", channel.ID, resp.ChannelID)
	}
	if len(resp.Permissions) != 1 {
		t.Fatalf("esperava 1 entrada de permissão, obtive %d", len(resp.Permissions))
	}
	entry := resp.Permissions[0]
	if entry.RoleID != role.ID {
		t.Errorf("esperava role_id %s, obtive %s", role.ID, entry.RoleID)
	}
	if entry.RoleName != roleName {
		t.Errorf("esperava role_name %q, obtive %q", roleName, entry.RoleName)
	}
	if entry.Permissions != permissions {
		t.Errorf("permissões não conferem:\n got  %+v\n want %+v", entry.Permissions, permissions)
	}
}

func TestGetChannelPermissionsHandlerNotFound(t *testing.T) {
	channelID := randUUID()
	c := newContext(t, http.MethodGet, "/channels/"+channelID+"/permissions", nil, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(channelID)
	rec := recorder(c)

	if err := handlers.GetChannelPermissionsHandler(testBaseURL, c); err != nil {
		t.Fatalf("GetChannelPermissionsHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

func TestGetChannelPermissionsHandlerMissingParam(t *testing.T) {
	c := newContext(t, http.MethodGet, "/channels//permissions", nil, "")
	c.SetParamNames("channel_id")
	c.SetParamValues("")
	rec := recorder(c)

	if err := handlers.GetChannelPermissionsHandler(testBaseURL, c); err != nil {
		t.Fatalf("GetChannelPermissionsHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "channel_id ausente")
}

// --- UpdateChannelPermissionsHandler (tarefa 5.4) ---

func TestUpdateChannelPermissionsHandlerSuccess(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	permissions := models.ChannelPermission{ReadChannel: true, SendMessages: true, DeleteMessages: false}

	body, _ := json.Marshal(map[string]models.ChannelPermission{"permissions": permissions})
	c := newContext(t, http.MethodPut, "/channels/"+channel.ID+"/permissions/"+role.ID, body, "")
	c.SetParamNames("channel_id", "role_id")
	c.SetParamValues(channel.ID, role.ID)
	rec := recorder(c)

	if err := handlers.UpdateChannelPermissionsHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelPermissionsHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		ChannelID   string                   `json:"channel_id"`
		RoleID      string                   `json:"role_id"`
		Permissions models.ChannelPermission `json:"permissions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ChannelID != channel.ID {
		t.Errorf("esperava channel_id %s, obtive %s", channel.ID, resp.ChannelID)
	}
	if resp.RoleID != role.ID {
		t.Errorf("esperava role_id %s, obtive %s", role.ID, resp.RoleID)
	}
	if resp.Permissions != permissions {
		t.Errorf("permissões não conferem:\n got  %+v\n want %+v", resp.Permissions, permissions)
	}

	// as permissões devem ter sido persistidas no canal
	stored, err := storage.GetChannelByID(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	storedPermission, ok := stored.Permissions[role.ID]
	if !ok {
		t.Fatalf("esperava permissões da role %s no canal, obtive %v", role.ID, stored.Permissions)
	}
	if storedPermission != permissions {
		t.Errorf("permissões persistidas não conferem:\n got  %+v\n want %+v", storedPermission, permissions)
	}
}

func TestUpdateChannelPermissionsHandlerMissingChannelID(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	body, _ := json.Marshal(map[string]models.ChannelPermission{"permissions": {ReadChannel: true}})
	c := newContext(t, http.MethodPut, "/channels//permissions/"+role.ID, body, "")
	c.SetParamNames("channel_id", "role_id")
	c.SetParamValues("", role.ID)
	rec := recorder(c)

	if err := handlers.UpdateChannelPermissionsHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelPermissionsHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "channel_id ausente")
}

func TestUpdateChannelPermissionsHandlerMissingRoleID(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	body, _ := json.Marshal(map[string]models.ChannelPermission{"permissions": {ReadChannel: true}})
	c := newContext(t, http.MethodPut, "/channels/"+channel.ID+"/permissions/", body, "")
	c.SetParamNames("channel_id", "role_id")
	c.SetParamValues(channel.ID, "")
	rec := recorder(c)

	if err := handlers.UpdateChannelPermissionsHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelPermissionsHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "role_id ausente")
}

func TestUpdateChannelPermissionsHandlerInvalidJSON(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	c := newContext(t, http.MethodPut, "/channels/"+channel.ID+"/permissions/"+role.ID, []byte("{invalido"), "")
	c.SetParamNames("channel_id", "role_id")
	c.SetParamValues(channel.ID, role.ID)
	rec := recorder(c)

	if err := handlers.UpdateChannelPermissionsHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelPermissionsHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestUpdateChannelPermissionsHandlerChannelNotFound(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	channelID := randUUID()

	body, _ := json.Marshal(map[string]models.ChannelPermission{"permissions": {ReadChannel: true}})
	c := newContext(t, http.MethodPut, "/channels/"+channelID+"/permissions/"+role.ID, body, "")
	c.SetParamNames("channel_id", "role_id")
	c.SetParamValues(channelID, role.ID)
	rec := recorder(c)

	if err := handlers.UpdateChannelPermissionsHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelPermissionsHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

func TestUpdateChannelPermissionsHandlerRoleNotFound(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	roleID := randUUID()

	body, _ := json.Marshal(map[string]models.ChannelPermission{"permissions": {ReadChannel: true}})
	c := newContext(t, http.MethodPut, "/channels/"+channel.ID+"/permissions/"+roleID, body, "")
	c.SetParamNames("channel_id", "role_id")
	c.SetParamValues(channel.ID, roleID)
	rec := recorder(c)

	if err := handlers.UpdateChannelPermissionsHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelPermissionsHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "role não encontrada")
}

func TestUpdateChannelPermissionsHandlerRoleFromOtherServer(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	serverA, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	serverB, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar segundo servidor: %v", err)
	}
	channel, err := storage.CreateChannel(testCtx(), serverA.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), serverB.ID, "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	body, _ := json.Marshal(map[string]models.ChannelPermission{"permissions": {ReadChannel: true}})
	c := newContext(t, http.MethodPut, "/channels/"+channel.ID+"/permissions/"+role.ID, body, "")
	c.SetParamNames("channel_id", "role_id")
	c.SetParamValues(channel.ID, role.ID)
	rec := recorder(c)

	if err := handlers.UpdateChannelPermissionsHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelPermissionsHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "role não encontrada")
}

// --- ChangeChannelPositionHandler (tarefa 8.4) ---

func TestChangeChannelPositionHandlerSuccess(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	c1, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar primeiro canal: %v", err)
	}
	c2, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar segundo canal: %v", err)
	}
	c3, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar terceiro canal: %v", err)
	}

	body, _ := json.Marshal(map[string]int{"old_position": 1, "new_position": 3})
	c := newContext(t, http.MethodPut, "/channels/"+c1.ID+"/change_position", body, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(c1.ID)
	rec := recorder(c)

	if err := handlers.ChangeChannelPositionHandler(testBaseURL, c); err != nil {
		t.Fatalf("ChangeChannelPositionHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var summary models.ChannelSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if summary.ID != c1.ID || summary.Position != 3 {
		t.Errorf("esperava canal %s na posição 3, obtive %s na posição %d", c1.ID, summary.ID, summary.Position)
	}

	stored, err := storage.GetChannelByID(testCtx(), c1.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Position != 3 {
		t.Errorf("esperava posição 3 persistida, obtive %d", stored.Position)
	}

	channels, err := storage.ListChannelsByServer(testCtx(), server.ID)
	if err != nil {
		t.Fatalf("ListChannelsByServer retornou erro: %v", err)
	}
	expected := []string{c2.ID, c3.ID, c1.ID}
	for i, want := range expected {
		if channels[i].ID != want || channels[i].Position != i+1 {
			t.Errorf("posição %d: esperava canal %s, obtive %s (position %d)", i+1, want, channels[i].ID, channels[i].Position)
		}
	}
}

func TestChangeChannelPositionHandlerMissingParam(t *testing.T) {
	c := newContext(t, http.MethodPut, "/channels//change_position", []byte(`{"old_position":1,"new_position":2}`), "")
	c.SetParamNames("channel_id")
	c.SetParamValues("")
	rec := recorder(c)

	if err := handlers.ChangeChannelPositionHandler(testBaseURL, c); err != nil {
		t.Fatalf("ChangeChannelPositionHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "channel_id ausente")
}

func TestChangeChannelPositionHandlerInvalidJSON(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	c := newContext(t, http.MethodPut, "/channels/"+channel.ID+"/change_position", []byte("{invalido"), "")
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	rec := recorder(c)

	if err := handlers.ChangeChannelPositionHandler(testBaseURL, c); err != nil {
		t.Fatalf("ChangeChannelPositionHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestChangeChannelPositionHandlerInvalidPosition(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	body, _ := json.Marshal(map[string]int{"old_position": 1, "new_position": 2})
	c := newContext(t, http.MethodPut, "/channels/"+channel.ID+"/change_position", body, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	rec := recorder(c)

	if err := handlers.ChangeChannelPositionHandler(testBaseURL, c); err != nil {
		t.Fatalf("ChangeChannelPositionHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"old_position e new_position devem ser posições válidas (1 até o número de canais do servidor)")
}

func TestChangeChannelPositionHandlerNotFound(t *testing.T) {
	body, _ := json.Marshal(map[string]int{"old_position": 1, "new_position": 1})
	c := newContext(t, http.MethodPut, "/channels/"+randUUID()+"/change_position", body, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(randUUID())
	rec := recorder(c)

	if err := handlers.ChangeChannelPositionHandler(testBaseURL, c); err != nil {
		t.Fatalf("ChangeChannelPositionHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

func TestChangeChannelPositionHandlerConflict(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	c1, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar primeiro canal: %v", err)
	}
	if _, err := storage.CreateChannel(testCtx(), server.ID, "canal_"+randHex(4), "text"); err != nil {
		t.Fatalf("falha ao criar segundo canal: %v", err)
	}

	body, _ := json.Marshal(map[string]int{"old_position": 2, "new_position": 1})
	c := newContext(t, http.MethodPut, "/channels/"+c1.ID+"/change_position", body, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(c1.ID)
	rec := recorder(c)

	if err := handlers.ChangeChannelPositionHandler(testBaseURL, c); err != nil {
		t.Fatalf("ChangeChannelPositionHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusConflict, "channel-position-conflict", "Posição do canal desatualizada",
		"a posição atual do canal não corresponde à old_position informada")
}

// --- ListRolesHandler (tarefa 6.2) ---

func TestListRolesHandlerEmpty(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	c := newContext(t, http.MethodGet, "/servers/"+server.ID+"/roles", nil, "")
	c.SetParamNames("server_id")
	c.SetParamValues(server.ID)
	rec := recorder(c)

	if err := handlers.ListRolesHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListRolesHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Roles []models.Role `json:"roles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.Roles == nil {
		t.Fatal("esperava roles como lista vazia, obtive null")
	}
	if len(resp.Roles) != 0 {
		t.Errorf("esperava lista de roles vazia, obtive %d", len(resp.Roles))
	}
}

func TestListRolesHandlerSuccess(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	colorA := "#FF0000"
	colorB := "#00FF00"
	roleA, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(4), &colorA, models.RolePermissions{ManageRoles: true})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	roleB, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(4), &colorB, models.RolePermissions{BanMembers: true})
	if err != nil {
		t.Fatalf("falha ao criar segunda role: %v", err)
	}

	c := newContext(t, http.MethodGet, "/servers/"+server.ID+"/roles", nil, "")
	c.SetParamNames("server_id")
	c.SetParamValues(server.ID)
	rec := recorder(c)

	if err := handlers.ListRolesHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListRolesHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Roles []models.Role `json:"roles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if len(resp.Roles) != 2 {
		t.Fatalf("esperava 2 roles na listagem, obtive %d", len(resp.Roles))
	}

	found := make(map[string]models.Role)
	for _, item := range resp.Roles {
		found[item.ID] = item
	}

	for _, want := range []models.Role{roleA, roleB} {
		item, ok := found[want.ID]
		if !ok {
			t.Errorf("role %s não encontrada na listagem", want.ID)
			continue
		}
		if item.ServerID != server.ID {
			t.Errorf("esperava server_id %s, obtive %s", server.ID, item.ServerID)
		}
		if item.Name != want.Name {
			t.Errorf("esperava name %q, obtive %q", want.Name, item.Name)
		}
		if item.Color == nil || *item.Color != *want.Color {
			t.Errorf("esperava color %v, obtive %v", want.Color, item.Color)
		}
		if item.Permissions != want.Permissions {
			t.Errorf("esperava permissions %v, obtive %v", want.Permissions, item.Permissions)
		}
		if item.CreatedAt.IsZero() {
			t.Error("esperava created_at preenchido")
		}
	}
}

func TestListRolesHandlerServerNotFound(t *testing.T) {
	serverID := randUUID()

	c := newContext(t, http.MethodGet, "/servers/"+serverID+"/roles", nil, "")
	c.SetParamNames("server_id")
	c.SetParamValues(serverID)
	rec := recorder(c)

	if err := handlers.ListRolesHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListRolesHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "servidor não encontrado")
}

func TestListRolesHandlerMissingParam(t *testing.T) {
	c := newContext(t, http.MethodGet, "/servers//roles", nil, "")
	c.SetParamNames("server_id")
	c.SetParamValues("")
	rec := recorder(c)

	if err := handlers.ListRolesHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListRolesHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "server_id ausente")
}

// --- CreateRoleHandler (tarefa 6.2) ---

func TestCreateRoleHandlerSuccess(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	name := "role_" + randHex(4)
	color := "#FF0000"
	permissions := models.RolePermissions{ManageRoles: true, BanMembers: true}
	body, _ := json.Marshal(map[string]any{
		"name":        name,
		"color":       color,
		"permissions": permissions,
	})
	c := newContext(t, http.MethodPost, "/servers/"+server.ID+"/roles", body, "")
	c.SetParamNames("server_id")
	c.SetParamValues(server.ID)
	rec := recorder(c)

	if err := handlers.CreateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateRoleHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp models.Role
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID == "" {
		t.Error("esperava id preenchido")
	}
	if resp.ServerID != server.ID {
		t.Errorf("esperava server_id %s, obtive %s", server.ID, resp.ServerID)
	}
	if resp.Name != name {
		t.Errorf("esperava name %q, obtive %q", name, resp.Name)
	}
	if resp.Color == nil || *resp.Color != color {
		t.Errorf("esperava color %q, obtive %v", color, resp.Color)
	}
	if resp.Permissions != permissions {
		t.Errorf("esperava permissions %v, obtive %v", permissions, resp.Permissions)
	}
	if resp.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}

	// a role deve ter sido persistida
	stored, err := storage.GetRoleByID(testCtx(), resp.ID)
	if err != nil {
		t.Fatalf("GetRoleByID retornou erro: %v", err)
	}
	if stored.Name != name {
		t.Errorf("esperava name %q persistido, obtive %q", name, stored.Name)
	}
	if stored.Color == nil || *stored.Color != color {
		t.Errorf("esperava color %q persistida, obtive %v", color, stored.Color)
	}
	if stored.Permissions != permissions {
		t.Errorf("esperava permissions %v persistidas, obtive %v", permissions, stored.Permissions)
	}
}

func TestCreateRoleHandlerNoColor(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	name := "role_" + randHex(4)
	body, _ := json.Marshal(map[string]string{"name": name})
	c := newContext(t, http.MethodPost, "/servers/"+server.ID+"/roles", body, "")
	c.SetParamNames("server_id")
	c.SetParamValues(server.ID)
	rec := recorder(c)

	if err := handlers.CreateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateRoleHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp models.Role
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.Color != nil {
		t.Errorf("esperava color null, obtive %q", *resp.Color)
	}
	if resp.Permissions != (models.RolePermissions{}) {
		t.Errorf("esperava permissions zeradas, obtive %v", resp.Permissions)
	}
}

func TestCreateRoleHandlerInvalidJSON(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	c := newContext(t, http.MethodPost, "/servers/"+server.ID+"/roles", []byte("{invalido"), "")
	c.SetParamNames("server_id")
	c.SetParamValues(server.ID)
	rec := recorder(c)

	if err := handlers.CreateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestCreateRoleHandlerMissingName(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"name": ""})
	c := newContext(t, http.MethodPost, "/servers/"+server.ID+"/roles", body, "")
	c.SetParamNames("server_id")
	c.SetParamValues(server.ID)
	rec := recorder(c)

	if err := handlers.CreateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"name é obrigatório e deve ter no máximo 32 caracteres; color deve ser hexadecimal #RRGGBB")
}

func TestCreateRoleHandlerNameTooLong(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"name": strings.Repeat("r", 33)})
	c := newContext(t, http.MethodPost, "/servers/"+server.ID+"/roles", body, "")
	c.SetParamNames("server_id")
	c.SetParamValues(server.ID)
	rec := recorder(c)

	if err := handlers.CreateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"name é obrigatório e deve ter no máximo 32 caracteres; color deve ser hexadecimal #RRGGBB")
}

func TestCreateRoleHandlerInvalidColor(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"name": "role_" + randHex(4), "color": "FF0000"})
	c := newContext(t, http.MethodPost, "/servers/"+server.ID+"/roles", body, "")
	c.SetParamNames("server_id")
	c.SetParamValues(server.ID)
	rec := recorder(c)

	if err := handlers.CreateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"name é obrigatório e deve ter no máximo 32 caracteres; color deve ser hexadecimal #RRGGBB")
}

func TestCreateRoleHandlerServerNotFound(t *testing.T) {
	serverID := randUUID()

	body, _ := json.Marshal(map[string]string{"name": "role_" + randHex(4)})
	c := newContext(t, http.MethodPost, "/servers/"+serverID+"/roles", body, "")
	c.SetParamNames("server_id")
	c.SetParamValues(serverID)
	rec := recorder(c)

	if err := handlers.CreateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "servidor não encontrado")
}

func TestCreateRoleHandlerNameTaken(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	name := "role_" + randHex(4)
	if _, err := storage.CreateRole(testCtx(), server.ID, name, nil, models.RolePermissions{}); err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"name": name})
	c := newContext(t, http.MethodPost, "/servers/"+server.ID+"/roles", body, "")
	c.SetParamNames("server_id")
	c.SetParamValues(server.ID)
	rec := recorder(c)

	if err := handlers.CreateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusConflict, "role-name-taken", "Nome de role já existe",
		"o nome informado já está em uso no servidor")
}

func TestCreateRoleHandlerMissingParam(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"name": "role_" + randHex(4)})
	c := newContext(t, http.MethodPost, "/servers//roles", body, "")
	c.SetParamNames("server_id")
	c.SetParamValues("")
	rec := recorder(c)

	if err := handlers.CreateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "server_id ausente")
}

// --- UpdateRoleHandler (tarefa 6.2) ---

func TestUpdateRoleHandlerSuccess(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	newName := "role_" + randHex(4)
	newColor := "#00FF00"
	newPermissions := models.RolePermissions{ManageServer: true, PinMessage: true}
	body, _ := json.Marshal(map[string]any{
		"name":        newName,
		"color":       newColor,
		"permissions": newPermissions,
	})
	c := newContext(t, http.MethodPut, "/roles/"+role.ID, body, "")
	c.SetParamNames("role_id")
	c.SetParamValues(role.ID)
	rec := recorder(c)

	if err := handlers.UpdateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateRoleHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp models.Role
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID != role.ID {
		t.Errorf("esperava id %s, obtive %s", role.ID, resp.ID)
	}
	if resp.ServerID != server.ID {
		t.Errorf("esperava server_id %s, obtive %s", server.ID, resp.ServerID)
	}
	if resp.Name != newName {
		t.Errorf("esperava name %q, obtive %q", newName, resp.Name)
	}
	if resp.Color == nil || *resp.Color != newColor {
		t.Errorf("esperava color %q, obtive %v", newColor, resp.Color)
	}
	if resp.Permissions != newPermissions {
		t.Errorf("esperava permissions %v, obtive %v", newPermissions, resp.Permissions)
	}
	if resp.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}

	// os campos devem ter sido persistidos
	stored, err := storage.GetRoleByID(testCtx(), role.ID)
	if err != nil {
		t.Fatalf("GetRoleByID retornou erro: %v", err)
	}
	if stored.Name != newName {
		t.Errorf("esperava name %q persistido, obtive %q", newName, stored.Name)
	}
	if stored.Color == nil || *stored.Color != newColor {
		t.Errorf("esperava color %q persistida, obtive %v", newColor, stored.Color)
	}
	if stored.Permissions != newPermissions {
		t.Errorf("esperava permissions %v persistidas, obtive %v", newPermissions, stored.Permissions)
	}
}

func TestUpdateRoleHandlerInvalidJSON(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	c := newContext(t, http.MethodPut, "/roles/"+role.ID, []byte("{invalido"), "")
	c.SetParamNames("role_id")
	c.SetParamValues(role.ID)
	rec := recorder(c)

	if err := handlers.UpdateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestUpdateRoleHandlerMissingName(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"name": ""})
	c := newContext(t, http.MethodPut, "/roles/"+role.ID, body, "")
	c.SetParamNames("role_id")
	c.SetParamValues(role.ID)
	rec := recorder(c)

	if err := handlers.UpdateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"name é obrigatório e deve ter no máximo 32 caracteres; color deve ser hexadecimal #RRGGBB")
}

func TestUpdateRoleHandlerInvalidColor(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"name": "role_" + randHex(4), "color": "#GGG"})
	c := newContext(t, http.MethodPut, "/roles/"+role.ID, body, "")
	c.SetParamNames("role_id")
	c.SetParamValues(role.ID)
	rec := recorder(c)

	if err := handlers.UpdateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"name é obrigatório e deve ter no máximo 32 caracteres; color deve ser hexadecimal #RRGGBB")
}

func TestUpdateRoleHandlerNotFound(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"name": "role_" + randHex(4)})
	c := newContext(t, http.MethodPut, "/roles/"+randUUID(), body, "")
	c.SetParamNames("role_id")
	c.SetParamValues(randUUID())
	rec := recorder(c)

	if err := handlers.UpdateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "role não encontrada")
}

func TestUpdateRoleHandlerNameTaken(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	roleA, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	roleB, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar segunda role: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"name": roleB.Name})
	c := newContext(t, http.MethodPut, "/roles/"+roleA.ID, body, "")
	c.SetParamNames("role_id")
	c.SetParamValues(roleA.ID)
	rec := recorder(c)

	if err := handlers.UpdateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusConflict, "role-name-taken", "Nome de role já existe",
		"o nome informado já está em uso no servidor")
}

func TestUpdateRoleHandlerMissingParam(t *testing.T) {
	c := newContext(t, http.MethodPut, "/roles/", []byte(`{"name":"role"}`), "")
	c.SetParamNames("role_id")
	c.SetParamValues("")
	rec := recorder(c)

	if err := handlers.UpdateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "role_id ausente")
}

// --- DeleteRoleHandler (tarefa 6.2) ---

func TestDeleteRoleHandlerSuccess(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	member, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar membro: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), member.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role ao membro: %v", err)
	}

	c := newContext(t, http.MethodDelete, "/roles/"+role.ID, nil, "")
	c.SetParamNames("role_id")
	c.SetParamValues(role.ID)
	rec := recorder(c)

	if err := handlers.DeleteRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteRoleHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	// a role deve ter sido removida
	if _, err := storage.GetRoleByID(testCtx(), role.ID); err == nil {
		t.Error("esperava GetRoleByID falhar após a exclusão")
	}
	// a atribuição do membro deve ter sido removida em cascata
	if _, err := storage.GetUserRole(testCtx(), member.ID, role.ID); err == nil {
		t.Error("esperava GetUserRole falhar após a exclusão da role")
	}
}

func TestDeleteRoleHandlerNotFound(t *testing.T) {
	roleID := randUUID()

	c := newContext(t, http.MethodDelete, "/roles/"+roleID, nil, "")
	c.SetParamNames("role_id")
	c.SetParamValues(roleID)
	rec := recorder(c)

	if err := handlers.DeleteRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "role não encontrada")
}

func TestDeleteRoleHandlerMissingParam(t *testing.T) {
	c := newContext(t, http.MethodDelete, "/roles/", nil, "")
	c.SetParamNames("role_id")
	c.SetParamValues("")
	rec := recorder(c)

	if err := handlers.DeleteRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "role_id ausente")
}

// --- AssignUserRoleHandler (tarefa 6.2) ---

func TestAssignUserRoleHandlerSuccess(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	member, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar membro: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"role_id": role.ID})
	c := newContext(t, http.MethodPost, "/users/"+member.ID+"/roles", body, "")
	c.SetParamNames("user_id")
	c.SetParamValues(member.ID)
	rec := recorder(c)

	if err := handlers.AssignUserRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("AssignUserRoleHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp models.UserRole
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.UserID != member.ID {
		t.Errorf("esperava user_id %s, obtive %s", member.ID, resp.UserID)
	}
	if resp.RoleID != role.ID {
		t.Errorf("esperava role_id %s, obtive %s", role.ID, resp.RoleID)
	}
	if resp.AssignedAt.IsZero() {
		t.Error("esperava assigned_at preenchido")
	}

	// a atribuição deve ter sido persistida
	stored, err := storage.GetUserRole(testCtx(), member.ID, role.ID)
	if err != nil {
		t.Fatalf("GetUserRole retornou erro: %v", err)
	}
	if stored.UserID != member.ID || stored.RoleID != role.ID {
		t.Errorf("atribuição persistida não confere: %+v", stored)
	}
}

func TestAssignUserRoleHandlerIdempotent(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	member, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar membro: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"role_id": role.ID})
	c := newContext(t, http.MethodPost, "/users/"+member.ID+"/roles", body, "")
	c.SetParamNames("user_id")
	c.SetParamValues(member.ID)
	rec := recorder(c)

	if err := handlers.AssignUserRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("AssignUserRoleHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201 na primeira atribuição, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var first models.UserRole
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}

	// atribuir a mesma role novamente é idempotente e retorna a atribuição existente
	c2 := newContext(t, http.MethodPost, "/users/"+member.ID+"/roles", body, "")
	c2.SetParamNames("user_id")
	c2.SetParamValues(member.ID)
	rec2 := recorder(c2)

	if err := handlers.AssignUserRoleHandler(testBaseURL, c2); err != nil {
		t.Fatalf("AssignUserRoleHandler retornou erro: %v", err)
	}
	if rec2.Code != http.StatusCreated {
		t.Fatalf("esperava status 201 na segunda atribuição, obtive %d (corpo: %s)", rec2.Code, rec2.Body.String())
	}
	var second models.UserRole
	if err := json.Unmarshal(rec2.Body.Bytes(), &second); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if !second.AssignedAt.Equal(first.AssignedAt) {
		t.Errorf("esperava a mesma atribuição (assigned_at %s), obtive %s", first.AssignedAt, second.AssignedAt)
	}
}

func TestAssignUserRoleHandlerInvalidJSON(t *testing.T) {
	member, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar membro: %v", err)
	}

	c := newContext(t, http.MethodPost, "/users/"+member.ID+"/roles", []byte("{invalido"), "")
	c.SetParamNames("user_id")
	c.SetParamValues(member.ID)
	rec := recorder(c)

	if err := handlers.AssignUserRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("AssignUserRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "role_id é obrigatório")
}

func TestAssignUserRoleHandlerMissingRoleID(t *testing.T) {
	member, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar membro: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"role_id": ""})
	c := newContext(t, http.MethodPost, "/users/"+member.ID+"/roles", body, "")
	c.SetParamNames("user_id")
	c.SetParamValues(member.ID)
	rec := recorder(c)

	if err := handlers.AssignUserRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("AssignUserRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "role_id é obrigatório")
}

func TestAssignUserRoleHandlerUserNotFound(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	userID := randUUID()

	body, _ := json.Marshal(map[string]string{"role_id": role.ID})
	c := newContext(t, http.MethodPost, "/users/"+userID+"/roles", body, "")
	c.SetParamNames("user_id")
	c.SetParamValues(userID)
	rec := recorder(c)

	if err := handlers.AssignUserRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("AssignUserRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "usuário não encontrado")
}

func TestAssignUserRoleHandlerRoleNotFound(t *testing.T) {
	member, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar membro: %v", err)
	}
	roleID := randUUID()

	body, _ := json.Marshal(map[string]string{"role_id": roleID})
	c := newContext(t, http.MethodPost, "/users/"+member.ID+"/roles", body, "")
	c.SetParamNames("user_id")
	c.SetParamValues(member.ID)
	rec := recorder(c)

	if err := handlers.AssignUserRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("AssignUserRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "role não encontrada")
}

func TestAssignUserRoleHandlerMissingUserParam(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"role_id": randUUID()})
	c := newContext(t, http.MethodPost, "/users//roles", body, "")
	c.SetParamNames("user_id")
	c.SetParamValues("")
	rec := recorder(c)

	if err := handlers.AssignUserRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("AssignUserRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "user_id ausente")
}

// --- RemoveUserRoleHandler (tarefa 6.2) ---

func TestRemoveUserRoleHandlerSuccess(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	member, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar membro: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), member.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role ao membro: %v", err)
	}

	c := newContext(t, http.MethodDelete, "/users/"+member.ID+"/roles/"+role.ID, nil, "")
	c.SetParamNames("user_id", "role_id")
	c.SetParamValues(member.ID, role.ID)
	rec := recorder(c)

	if err := handlers.RemoveUserRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("RemoveUserRoleHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	// a atribuição deve ter sido removida
	if _, err := storage.GetUserRole(testCtx(), member.ID, role.ID); err == nil {
		t.Error("esperava GetUserRole falhar após a remoção")
	}
	// a role em si deve continuar existindo
	if _, err := storage.GetRoleByID(testCtx(), role.ID); err != nil {
		t.Errorf("esperava a role continuar existindo após a remoção da atribuição: %v", err)
	}
}

func TestRemoveUserRoleHandlerUserNotFound(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	userID := randUUID()

	c := newContext(t, http.MethodDelete, "/users/"+userID+"/roles/"+role.ID, nil, "")
	c.SetParamNames("user_id", "role_id")
	c.SetParamValues(userID, role.ID)
	rec := recorder(c)

	if err := handlers.RemoveUserRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("RemoveUserRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "usuário não encontrado")
}

func TestRemoveUserRoleHandlerRoleNotFound(t *testing.T) {
	member, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar membro: %v", err)
	}
	roleID := randUUID()

	c := newContext(t, http.MethodDelete, "/users/"+member.ID+"/roles/"+roleID, nil, "")
	c.SetParamNames("user_id", "role_id")
	c.SetParamValues(member.ID, roleID)
	rec := recorder(c)

	if err := handlers.RemoveUserRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("RemoveUserRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "role não encontrada")
}

func TestRemoveUserRoleHandlerUserRoleNotFound(t *testing.T) {
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	member, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar membro: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	c := newContext(t, http.MethodDelete, "/users/"+member.ID+"/roles/"+role.ID, nil, "")
	c.SetParamNames("user_id", "role_id")
	c.SetParamValues(member.ID, role.ID)
	rec := recorder(c)

	if err := handlers.RemoveUserRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("RemoveUserRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "role não atribuída ao usuário")
}

func TestRemoveUserRoleHandlerMissingParams(t *testing.T) {
	c := newContext(t, http.MethodDelete, "/users//roles/", nil, "")
	c.SetParamNames("user_id", "role_id")
	c.SetParamValues("", "")
	rec := recorder(c)

	if err := handlers.RemoveUserRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("RemoveUserRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "user_id ausente")
}

// --- LoginServerHandler / regra de acesso a servidores não públicos ---

// newContextWithAuthCookie monta um contexto com o cookie Auth definido na requisição.
func newContextWithAuthCookie(t *testing.T, method, path string, body []byte, ip, authCookie string) echo.Context {
	t.Helper()

	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if ip != "" {
		req.RemoteAddr = ip + ":12345"
	}
	if authCookie != "" {
		req.AddCookie(&http.Cookie{Name: "Auth", Value: authCookie})
	}

	e := echo.New()
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec)
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

// createNonPublicServerTest cria o servidor do backend como não público, com a
// senha informada (hash bcrypt).
func createNonPublicServerTest(t *testing.T, password string) {
	t.Helper()
	hash, err := utils.HashPassword(password)
	if err != nil {
		t.Fatalf("falha ao gerar hash da senha do servidor: %v", err)
	}
	if _, err := storage.CreateServerWithIcon(testCtx(), "srv_"+randHex(4), nil, "", false, nil, &hash); err != nil {
		t.Fatalf("falha ao criar servidor não público: %v", err)
	}
}

// createPublicServerTest cria o servidor do backend como público.
func createPublicServerTest(t *testing.T) {
	t.Helper()
	if _, err := storage.CreateServerWithIcon(testCtx(), "srv_"+randHex(4), nil, "", true, nil, nil); err != nil {
		t.Fatalf("falha ao criar servidor público: %v", err)
	}
}

// assertAuthCookie verifica o cookie Auth no Set-Cookie da resposta: valor,
// HttpOnly, Secure e Max-Age (wantMaxAge vazio não verifica o Max-Age).
func assertAuthCookie(t *testing.T, rec *httptest.ResponseRecorder, wantValue, wantMaxAge string) {
	t.Helper()

	setCookieHeaders := rec.Header().Values("Set-Cookie")
	if len(setCookieHeaders) == 0 {
		t.Fatal("esperava header Set-Cookie definido")
	}
	for _, header := range setCookieHeaders {
		segment := header
		if idx := strings.Index(header, ";"); idx >= 0 {
			segment = header[:idx]
		}
		if idx := strings.Index(segment, "="); idx >= 0 {
			name := strings.TrimSpace(segment[:idx])
			value := strings.TrimSpace(segment[idx+1:])
			if name != "Auth" {
				continue
			}
			if value != wantValue {
				t.Errorf("esperava cookie Auth com valor %q, obtive %q", wantValue, value)
			}
			lower := strings.ToLower(header)
			if !strings.Contains(lower, "httponly") {
				t.Error("esperava cookie Auth com HttpOnly")
			}
			if !strings.Contains(lower, "secure") {
				t.Error("esperava cookie Auth com Secure")
			}
			if wantMaxAge != "" && !strings.Contains(lower, "max-age="+wantMaxAge) {
				t.Errorf("esperava cookie Auth com Max-Age=%s, obtive %q", wantMaxAge, header)
			}
			return
		}
	}
	t.Fatal("esperava cookie Auth definido")
}

func TestLoginServerHandlerSuccess(t *testing.T) {
	password := "server_pw_" + randHex(4)
	removeAllServersTest(t)
	createNonPublicServerTest(t, password)

	body, _ := json.Marshal(map[string]string{"server_password": password})
	c := newContext(t, http.MethodPost, "/auth/loginServer", body, newRandomIP())
	rec := recorder(c)

	if err := handlers.LoginServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginServerHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		TempToken string `json:"temp_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.TempToken == "" {
		t.Fatal("esperava temp_token preenchido")
	}

	// o cookie Auth deve ser o token temporário com Max-Age de 30min
	assertAuthCookie(t, rec, resp.TempToken, strconv.Itoa(int(utils.TempTokenExpiration.Seconds())))
}

func TestLoginServerHandlerPublicServer(t *testing.T) {
	removeAllServersTest(t)
	createPublicServerTest(t)

	body, _ := json.Marshal(map[string]string{"server_password": "qualquer_senha"})
	c := newContext(t, http.MethodPost, "/auth/loginServer", body, newRandomIP())
	rec := recorder(c)

	if err := handlers.LoginServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginServerHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 em servidor público, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

func TestLoginServerHandlerInvalidJSON(t *testing.T) {
	c := newContext(t, http.MethodPost, "/auth/loginServer", []byte("{invalido"), newRandomIP())
	rec := recorder(c)

	if err := handlers.LoginServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestLoginServerHandlerMissingPassword(t *testing.T) {
	body, _ := json.Marshal(map[string]string{})
	c := newContext(t, http.MethodPost, "/auth/loginServer", body, newRandomIP())
	rec := recorder(c)

	if err := handlers.LoginServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "campo 'server_password' é obrigatório")
}

func TestLoginServerHandlerPasswordTooLong(t *testing.T) {
	cfg := config.LoadConfig()

	body, _ := json.Marshal(map[string]string{"server_password": strings.Repeat("a", cfg.MaxPasswordLength+1)})
	c := newContext(t, http.MethodPost, "/auth/loginServer", body, newRandomIP())
	rec := recorder(c)

	if err := handlers.LoginServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"campo 'server_password' deve ter no máximo "+strconv.Itoa(cfg.MaxPasswordLength)+" caracteres")
}

func TestLoginServerHandlerWrongPassword(t *testing.T) {
	removeAllServersTest(t)
	createNonPublicServerTest(t, "server_pw_"+randHex(4))

	body, _ := json.Marshal(map[string]string{"server_password": "senha_incorreta_" + randHex(4)})
	c := newContext(t, http.MethodPost, "/auth/loginServer", body, newRandomIP())
	rec := recorder(c)

	if err := handlers.LoginServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "invalid-credentials", "Credenciais inválidas", "senha do servidor incorreta")
}

func TestLoginServerHandlerServerNotFound(t *testing.T) {
	removeAllServersTest(t)

	body, _ := json.Marshal(map[string]string{"server_password": "qualquer_senha"})
	c := newContext(t, http.MethodPost, "/auth/loginServer", body, newRandomIP())
	rec := recorder(c)

	if err := handlers.LoginServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "servidor não encontrado")
}

func TestLoginServerHandlerBannedIP(t *testing.T) {
	ip := newRandomIP()
	bannedUser, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), ip)
	if err != nil {
		t.Fatalf("falha ao criar usuário para banir: %v", err)
	}
	if _, err := storage.SetUserBanned(testCtx(), bannedUser.ID, true); err != nil {
		t.Fatalf("falha ao banir usuário: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"server_password": "qualquer_senha"})
	c := newContext(t, http.MethodPost, "/auth/loginServer", body, ip)
	rec := recorder(c)

	if err := handlers.LoginServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "banned", "Usuário banido", "o IP informado já foi usado por um usuário banido")
}

func TestRegisterHandlerServerAccessRequired(t *testing.T) {
	removeAllServersTest(t)
	createNonPublicServerTest(t, "server_pw_"+randHex(4))

	body, _ := json.Marshal(map[string]string{"username": newRandomUsername(), "password": newRandomPassword()})
	c := newContext(t, http.MethodPost, "/auth/register", body, newRandomIP())
	rec := recorder(c)

	if err := handlers.RegisterHandler(testBaseURL, c); err != nil {
		t.Fatalf("RegisterHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "server-access-required", "Acesso ao servidor negado",
		"servidor não público: informe a senha do servidor em /auth/loginServer antes de continuar")
}

func TestLoginHandlerServerAccessRequired(t *testing.T) {
	removeAllServersTest(t)
	createNonPublicServerTest(t, "server_pw_"+randHex(4))

	body, _ := json.Marshal(map[string]string{"username": newRandomUsername(), "password": newRandomPassword()})
	c := newContext(t, http.MethodPost, "/auth/login", body, newRandomIP())
	rec := recorder(c)

	if err := handlers.LoginHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "server-access-required", "Acesso ao servidor negado",
		"servidor não público: informe a senha do servidor em /auth/loginServer antes de continuar")
}

func TestLoginHandlerSessionTokenNotAccepted(t *testing.T) {
	cfg := config.LoadConfig()

	removeAllServersTest(t)
	createNonPublicServerTest(t, "server_pw_"+randHex(4))

	// um token de sessão válido não substitui a autorização temporária
	sessionToken, err := utils.GenerateToken(randUUID(), cfg.JWTSecret)
	if err != nil {
		t.Fatalf("falha ao gerar token de sessão: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"username": newRandomUsername(), "password": newRandomPassword()})
	c := newContextWithAuthCookie(t, http.MethodPost, "/auth/login", body, newRandomIP(), sessionToken)
	rec := recorder(c)

	if err := handlers.LoginHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "server-access-required", "Acesso ao servidor negado",
		"servidor não público: informe a senha do servidor em /auth/loginServer antes de continuar")
}

func TestRegisterHandlerWithTempToken(t *testing.T) {
	cfg := config.LoadConfig()

	password := "server_pw_" + randHex(4)
	removeAllServersTest(t)
	createNonPublicServerTest(t, password)

	tempToken, err := utils.GenerateTempToken(cfg.JWTSecret)
	if err != nil {
		t.Fatalf("falha ao gerar token temporário: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"username": newRandomUsername(), "password": newRandomPassword()})
	c := newContextWithAuthCookie(t, http.MethodPost, "/auth/register", body, newRandomIP(), tempToken)
	rec := recorder(c)

	if err := handlers.RegisterHandler(testBaseURL, c); err != nil {
		t.Fatalf("RegisterHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

func TestLoginHandlerWithTempToken(t *testing.T) {
	cfg := config.LoadConfig()

	username := newRandomUsername()
	password := newRandomPassword()
	ip := newRandomIP()

	hash, err := utils.HashPassword(password)
	if err != nil {
		t.Fatalf("falha ao gerar hash da senha: %v", err)
	}
	if _, _, err := storage.CreateUser(testCtx(), username, hash, ip); err != nil {
		t.Fatalf("falha ao criar usuário para login: %v", err)
	}

	removeAllServersTest(t)
	createNonPublicServerTest(t, "server_pw_"+randHex(4))

	tempToken, err := utils.GenerateTempToken(cfg.JWTSecret)
	if err != nil {
		t.Fatalf("falha ao gerar token temporário: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	c := newContextWithAuthCookie(t, http.MethodPost, "/auth/login", body, ip, tempToken)
	rec := recorder(c)

	if err := handlers.LoginHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.Token == "" {
		t.Fatal("esperava token preenchido")
	}

	// o login consolida o cookie Auth com o token de sessão (24h)
	assertAuthCookie(t, rec, resp.Token, strconv.Itoa(int(utils.JWTExpiration.Seconds())))
}

// --- handlers de mensagens (tarefa 7.2) ---

// newMultipartContext cria um contexto com corpo multipart/form-data.
// fields são os campos de texto e files mapeia o nome do campo para pares
// (nome do arquivo, conteúdo).
func newMultipartContext(t *testing.T, method, path string, fields map[string]string, files map[string][][2]string, authCookie string) echo.Context {
	t.Helper()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for name, value := range fields {
		if err := w.WriteField(name, value); err != nil {
			t.Fatalf("falha ao escrever campo %s: %v", name, err)
		}
	}
	for name, file := range files {
		for _, f := range file {
			fw, err := w.CreateFormFile(name, f[0])
			if err != nil {
				t.Fatalf("falha ao criar arquivo %s: %v", name, err)
			}
			if _, err := fw.Write([]byte(f[1])); err != nil {
				t.Fatalf("falha ao gravar arquivo %s: %v", name, err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("falha ao fechar o multipart writer: %v", err)
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(buf.Bytes()))
	req.Header.Set(echo.HeaderContentType, w.FormDataContentType())
	if authCookie != "" {
		req.AddCookie(&http.Cookie{Name: "Auth", Value: authCookie})
	}

	e := echo.New()
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec)
}

// createServerAndChannelTest cria um servidor para o dono e um canal text
// nele, retornando os dois registros.
func createServerAndChannelTest(t *testing.T, ownerID string) (models.Server, models.Channel) {
	t.Helper()
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &ownerID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := storage.CreateChannel(testCtx(), server.ID, "chn_"+randHex(4), "text")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	return server, channel
}

// newTestMessageUser cria um usuário genérico para os testes de mensagens.
func newTestMessageUser(t *testing.T) models.User {
	t.Helper()
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	return user
}

// --- ListMessagesHandler ---

func TestListMessagesHandlerEmpty(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)

	c := newContext(t, http.MethodGet, "/channels/"+channel.ID+"/messages", nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	rec := recorder(c)

	if err := handlers.ListMessagesHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListMessagesHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.MessageList
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ChannelID != channel.ID {
		t.Errorf("esperava channel_id %q, obtive %q", channel.ID, resp.ChannelID)
	}
	if resp.HasMore {
		t.Error("esperava has_more false, obtive true")
	}
	if len(resp.Messages) != 0 {
		t.Errorf("esperava lista vazia, obtive %d mensagens", len(resp.Messages))
	}
}

func TestListMessagesHandlerSuccess(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	first, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "primeira mensagem", nil)
	if err != nil {
		t.Fatalf("falha ao criar primeira mensagem: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	second, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "segunda mensagem", nil)
	if err != nil {
		t.Fatalf("falha ao criar segunda mensagem: %v", err)
	}

	c := newContext(t, http.MethodGet, "/channels/"+channel.ID+"/messages", nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	rec := recorder(c)

	if err := handlers.ListMessagesHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListMessagesHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.MessageList
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ChannelID != channel.ID {
		t.Errorf("esperava channel_id %q, obtive %q", channel.ID, resp.ChannelID)
	}
	if len(resp.Messages) != 2 {
		t.Fatalf("esperava 2 mensagens, obtive %d", len(resp.Messages))
	}
	// ordem decrescente: a mais recente primeiro
	if resp.Messages[0].ID != second.ID || resp.Messages[1].ID != first.ID {
		t.Errorf("ordem inesperada: %+v", resp.Messages)
	}
	if resp.Messages[0].Content == nil || *resp.Messages[0].Content != "segunda mensagem" {
		t.Errorf("esperava content %q, obtive %v", "segunda mensagem", resp.Messages[0].Content)
	}
	if resp.Messages[1].AuthorID == nil || *resp.Messages[1].AuthorID != owner.ID {
		t.Errorf("esperava author_id %q, obtive %v", owner.ID, resp.Messages[1].AuthorID)
	}
	if len(resp.Messages[0].Attachments) != 0 {
		t.Errorf("esperava attachments vazia, obtive %v", resp.Messages[0].Attachments)
	}
}

func TestListMessagesHandlerSince(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	first, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "primeira", nil)
	if err != nil {
		t.Fatalf("falha ao criar primeira mensagem: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "segunda", nil); err != nil {
		t.Fatalf("falha ao criar segunda mensagem: %v", err)
	}

	// since com precisão de microssegundos (o client envia o created_at da
	// última mensagem recebida, com a precisão exposta no JSON)
	c := newContext(t, http.MethodGet, "/channels/"+channel.ID+"/messages?since="+first.CreatedAt.Format(time.RFC3339Nano), nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	rec := recorder(c)

	if err := handlers.ListMessagesHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListMessagesHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.MessageList
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	// apenas a mensagem criada após o since
	if len(resp.Messages) != 1 || resp.Messages[0].Content == nil || *resp.Messages[0].Content != "segunda" {
		t.Fatalf("esperava apenas a segunda mensagem, obtive %+v", resp.Messages)
	}
}

func TestListMessagesHandlerChannelNotFound(t *testing.T) {
	owner := newTestMessageUser(t)
	missingID := randUUID()

	c := newContext(t, http.MethodGet, "/channels/"+missingID+"/messages", nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("channel_id")
	c.SetParamValues(missingID)
	rec := recorder(c)

	if err := handlers.ListMessagesHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListMessagesHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

func TestListMessagesHandlerMissingUserID(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)

	c := newContext(t, http.MethodGet, "/channels/"+channel.ID+"/messages", nil, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	rec := recorder(c)

	if err := handlers.ListMessagesHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListMessagesHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

func TestListMessagesHandlerInvalidSince(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)

	c := newContext(t, http.MethodGet, "/channels/"+channel.ID+"/messages?since=nao-e-data", nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	rec := recorder(c)

	if err := handlers.ListMessagesHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListMessagesHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"since deve ser um timestamp ISO 8601")
}

func TestListMessagesHandlerForbiddenWithoutPermission(t *testing.T) {
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	server, channel := createServerAndChannelTest(t, owner.ID)
	role, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(8), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	// canal fechado: role existe no canal sem read_channel
	if _, err := storage.UpdateChannelPermissions(testCtx(), channel.ID, role.ID, models.ChannelPermission{}); err != nil {
		t.Fatalf("falha ao definir permissões do canal: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), actor.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}

	c := newContext(t, http.MethodGet, "/channels/"+channel.ID+"/messages", nil, "")
	c.Set(middleware.UserIDContextKey, actor.ID)
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	rec := recorder(c)

	if err := handlers.ListMessagesHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListMessagesHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não tem permissão para ler o canal")
}

// --- CreateMessageHandler ---

func TestCreateMessageHandlerSuccess(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)

	c := newMultipartContext(t, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": "olá mundo"}, nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := handlers.CreateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateMessageHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.MessageWithAttachment
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID == "" {
		t.Error("esperava id preenchido")
	}
	if resp.ChannelID != channel.ID {
		t.Errorf("esperava channel_id %q, obtive %q", channel.ID, resp.ChannelID)
	}
	if resp.AuthorID == nil || *resp.AuthorID != owner.ID {
		t.Errorf("esperava author_id %q, obtive %v", owner.ID, resp.AuthorID)
	}
	if resp.Content == nil || *resp.Content != "olá mundo" {
		t.Errorf("esperava content %q, obtive %v", "olá mundo", resp.Content)
	}
	if resp.CreatedAt.IsZero() {
		t.Error("esperava created_at preenchido")
	}
	if len(resp.Attachments) != 0 {
		t.Errorf("esperava attachments vazia, obtive %v", resp.Attachments)
	}

	stored, err := storage.GetMessageByID(testCtx(), resp.ID)
	if err != nil {
		t.Fatalf("GetMessageByID retornou erro: %v", err)
	}
	if stored.Content == nil || *stored.Content != "olá mundo" {
		t.Errorf("esperava content %q persistido, obtive %v", "olá mundo", stored.Content)
	}
}

func TestCreateMessageHandlerWithAttachment(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	png := pngAvatarBytes(100, 100)

	c := newMultipartContext(t, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": "com anexo"},
		map[string][][2]string{"attachments": {{"foto.png", string(png)}}}, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := handlers.CreateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateMessageHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.MessageWithAttachment
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if len(resp.Attachments) != 1 {
		t.Fatalf("esperava 1 attachment, obtive %d", len(resp.Attachments))
	}
	att := resp.Attachments[0]
	if att.ID == "" {
		t.Error("esperava id do attachment preenchido")
	}
	if att.OriginalFileName != "foto.png" {
		t.Errorf("esperava original_file_name %q, obtive %q", "foto.png", att.OriginalFileName)
	}
	if att.MimeType != "image/png" {
		t.Errorf("esperava mime_type %q, obtive %q", "image/png", att.MimeType)
	}
	if att.SizeBytes != int64(len(png)) {
		t.Errorf("esperava size_bytes %d, obtive %d", len(png), att.SizeBytes)
	}

	stored, err := storage.GetAttachmentByID(testCtx(), att.ID)
	if err != nil {
		t.Fatalf("GetAttachmentByID retornou erro: %v", err)
	}
	if stored.OriginalFileName != "foto.png" || stored.MimeType != "image/png" || stored.SizeBytes != int64(len(png)) {
		t.Errorf("persistência inesperada: %+v", stored)
	}
}

func TestCreateMessageHandlerMissingChannelID(t *testing.T) {
	owner := newTestMessageUser(t)

	c := newMultipartContext(t, http.MethodPost, "/messages",
		map[string]string{"content": "sem canal"}, nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := handlers.CreateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

func TestCreateMessageHandlerMissingContentAndAttachment(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)

	c := newMultipartContext(t, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID}, nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := handlers.CreateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"channel_id é obrigatório; content tem no máximo 8192 caracteres; a mensagem precisa de content ou attachment; nome do attachment inválido")
}

func TestCreateMessageHandlerContentTooLong(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)

	c := newMultipartContext(t, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": strings.Repeat("a", 8193)}, nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := handlers.CreateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"channel_id é obrigatório; content tem no máximo 8192 caracteres; a mensagem precisa de content ou attachment; nome do attachment inválido")
}

func TestCreateMessageHandlerChannelNotFound(t *testing.T) {
	owner := newTestMessageUser(t)
	missingID := randUUID()

	c := newMultipartContext(t, http.MethodPost, "/messages",
		map[string]string{"channel_id": missingID, "content": "x"}, nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := handlers.CreateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

func TestCreateMessageHandlerForbiddenWithoutPermission(t *testing.T) {
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	server, channel := createServerAndChannelTest(t, owner.ID)
	role, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(8), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	// canal fechado: role existe no canal sem send_messages
	if _, err := storage.UpdateChannelPermissions(testCtx(), channel.ID, role.ID, models.ChannelPermission{}); err != nil {
		t.Fatalf("falha ao definir permissões do canal: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), actor.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}

	c := newMultipartContext(t, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": "x"}, nil, "")
	c.Set(middleware.UserIDContextKey, actor.ID)
	rec := recorder(c)

	if err := handlers.CreateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não tem permissão para enviar esta mensagem")
}

func TestCreateMessageHandlerInvalidMultipart(t *testing.T) {
	owner := newTestMessageUser(t)

	// corpo JSON (não multipart) deve ser rejeitado
	body, _ := json.Marshal(map[string]string{"channel_id": "x", "content": "y"})
	c := newContext(t, http.MethodPost, "/messages", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := handlers.CreateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"corpo da requisição deve ser multipart/form-data válido")
}

// --- UpdateMessageHandler ---

func TestUpdateMessageHandlerSuccess(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "original", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"content": "editada"})
	c := newContext(t, http.MethodPut, "/messages/"+message.ID, body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("message_id")
	c.SetParamValues(message.ID)
	rec := recorder(c)

	if err := handlers.UpdateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateMessageHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.MessageWithAttachment
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID != message.ID {
		t.Errorf("esperava id %q, obtive %q", message.ID, resp.ID)
	}
	if resp.Content == nil || *resp.Content != "editada" {
		t.Errorf("esperava content %q, obtive %v", "editada", resp.Content)
	}
	if resp.EditedAt == nil {
		t.Error("esperava edited_at preenchido")
	}
}

func TestUpdateMessageHandlerClearContent(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "conteúdo", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"content": ""})
	c := newContext(t, http.MethodPut, "/messages/"+message.ID, body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("message_id")
	c.SetParamValues(message.ID)
	rec := recorder(c)

	if err := handlers.UpdateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateMessageHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.MessageWithAttachment
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.Content != nil {
		t.Errorf("esperava content null, obtive %q", *resp.Content)
	}
}

func TestUpdateMessageHandlerNotFound(t *testing.T) {
	owner := newTestMessageUser(t)
	missingID := randUUID()

	body, _ := json.Marshal(map[string]string{"content": "x"})
	c := newContext(t, http.MethodPut, "/messages/"+missingID, body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("message_id")
	c.SetParamValues(missingID)
	rec := recorder(c)

	if err := handlers.UpdateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "mensagem não encontrada")
}

func TestUpdateMessageHandlerForbiddenOtherUser(t *testing.T) {
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "x", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"content": "hacker"})
	c := newContext(t, http.MethodPut, "/messages/"+message.ID, body, "")
	c.Set(middleware.UserIDContextKey, actor.ID)
	c.SetParamNames("message_id")
	c.SetParamValues(message.ID)
	rec := recorder(c)

	if err := handlers.UpdateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"somente o autor da mensagem pode editá-la")
}

func TestUpdateMessageHandlerContentTooLong(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "x", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"content": strings.Repeat("a", 8193)})
	c := newContext(t, http.MethodPut, "/messages/"+message.ID, body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("message_id")
	c.SetParamValues(message.ID)
	rec := recorder(c)

	if err := handlers.UpdateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"content tem no máximo 8192 caracteres")
}

func TestUpdateMessageHandlerInvalidJSON(t *testing.T) {
	owner := newTestMessageUser(t)

	c := newContext(t, http.MethodPut, "/messages/"+randUUID(), []byte("{invalido"), "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("message_id")
	c.SetParamValues(randUUID())
	rec := recorder(c)

	if err := handlers.UpdateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"corpo da requisição inválido")
}

// --- DeleteMessageHandler ---

func TestDeleteMessageHandlerSuccess(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "a excluir", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	c := newContext(t, http.MethodDelete, "/messages/"+message.ID, nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("message_id")
	c.SetParamValues(message.ID)
	rec := recorder(c)

	if err := handlers.DeleteMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteMessageHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	if _, err := storage.GetMessageByID(testCtx(), message.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava mensagem removida do banco, obtive erro %v", err)
	}
}

func TestDeleteMessageHandlerNotFound(t *testing.T) {
	owner := newTestMessageUser(t)
	missingID := randUUID()

	c := newContext(t, http.MethodDelete, "/messages/"+missingID, nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("message_id")
	c.SetParamValues(missingID)
	rec := recorder(c)

	if err := handlers.DeleteMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "mensagem não encontrada")
}

func TestDeleteMessageHandlerForbiddenOtherUser(t *testing.T) {
	// canal aberto (sem roles): delete_messages não é livre — só o autor
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "x", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	c := newContext(t, http.MethodDelete, "/messages/"+message.ID, nil, "")
	c.Set(middleware.UserIDContextKey, actor.ID)
	c.SetParamNames("message_id")
	c.SetParamValues(message.ID)
	rec := recorder(c)

	if err := handlers.DeleteMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não tem permissão para excluir a mensagem")
}

func TestDeleteMessageHandlerWithDeleteMessagesRole(t *testing.T) {
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	server, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "x", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(8), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	if _, err := storage.UpdateChannelPermissions(testCtx(), channel.ID, role.ID, models.ChannelPermission{DeleteMessages: true}); err != nil {
		t.Fatalf("falha ao definir permissões do canal: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), actor.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}

	c := newContext(t, http.MethodDelete, "/messages/"+message.ID, nil, "")
	c.Set(middleware.UserIDContextKey, actor.ID)
	c.SetParamNames("message_id")
	c.SetParamValues(message.ID)
	rec := recorder(c)

	if err := handlers.DeleteMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteMessageHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if _, err := storage.GetMessageByID(testCtx(), message.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava mensagem removida do banco, obtive erro %v", err)
	}
}

func TestDeleteMessageHandlerOwnerDeletesOtherMessage(t *testing.T) {
	owner := newTestMessageUser(t)
	other := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, other.ID, "x", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	c := newContext(t, http.MethodDelete, "/messages/"+message.ID, nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("message_id")
	c.SetParamValues(message.ID)
	rec := recorder(c)

	if err := handlers.DeleteMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteMessageHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// --- handlers.Emojis (tarefa 7.4) ---

func TestListEmojisHandlerEmpty(t *testing.T) {
	owner := newTestMessageUser(t)
	server, _ := createServerAndChannelTest(t, owner.ID)

	c := newContext(t, http.MethodGet, "/emojis?server_id="+server.ID, nil, "")
	rec := recorder(c)

	if err := handlers.ListEmojisHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListEmojisHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.EmojiList
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.HasMore {
		t.Error("esperava has_more false, obtive true")
	}
	if len(resp.Emojis) != 0 {
		t.Errorf("esperava lista vazia, obtive %d emojis", len(resp.Emojis))
	}
}

func TestListEmojisHandlerSuccess(t *testing.T) {
	owner := newTestMessageUser(t)
	server, _ := createServerAndChannelTest(t, owner.ID)
	e1, err := storage.CreateEmoji(testCtx(), server.ID, "emoji_"+randHex(8), "PNG", []byte{1}, &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	e2, err := storage.CreateEmoji(testCtx(), server.ID, "emoji_"+randHex(8), "PNG", []byte{2}, &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}

	c := newContext(t, http.MethodGet, "/emojis?server_id="+server.ID, nil, "")
	rec := recorder(c)

	if err := handlers.ListEmojisHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListEmojisHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.EmojiList
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if len(resp.Emojis) != 2 {
		t.Fatalf("esperava 2 emojis, obtive %d", len(resp.Emojis))
	}
	// ordem crescente: o mais antigo primeiro
	if resp.Emojis[0].ID != e1.ID || resp.Emojis[1].ID != e2.ID {
		t.Errorf("ordem inesperada (esperava created_at ascendente): %+v", resp.Emojis)
	}
	// image_blob é exposto no conteúdo original (base64 no JSON)
	if string(resp.Emojis[0].ImageBlob) != string([]byte{1}) {
		t.Errorf("esperava image_blob %v, obtive %v", []byte{1}, resp.Emojis[0].ImageBlob)
	}
	if resp.Emojis[0].CreatedBy == nil || *resp.Emojis[0].CreatedBy != owner.ID {
		t.Errorf("esperava created_by %s, obtive %v", owner.ID, resp.Emojis[0].CreatedBy)
	}
	if resp.HasMore {
		t.Error("esperava has_more false, obtive true")
	}
}

func TestListEmojisHandlerPagination(t *testing.T) {
	owner := newTestMessageUser(t)
	server, _ := createServerAndChannelTest(t, owner.ID)
	for i := 0; i < 26; i++ {
		if _, err := storage.CreateEmoji(testCtx(), server.ID, "emoji_"+randHex(8), "PNG", []byte{1}, &owner.ID); err != nil {
			t.Fatalf("falha ao criar emoji %d: %v", i, err)
		}
	}

	c := newContext(t, http.MethodGet, "/emojis?server_id="+server.ID, nil, "")
	rec := recorder(c)

	if err := handlers.ListEmojisHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListEmojisHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.EmojiList
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if len(resp.Emojis) != 25 {
		t.Fatalf("esperava 25 emojis no limite da página, obtive %d", len(resp.Emojis))
	}
	if !resp.HasMore {
		t.Error("esperava has_more true com 26 emojis criados")
	}
}

func TestListEmojisHandlerSince(t *testing.T) {
	owner := newTestMessageUser(t)
	server, _ := createServerAndChannelTest(t, owner.ID)
	first, err := storage.CreateEmoji(testCtx(), server.ID, "emoji_"+randHex(8), "PNG", []byte{1}, &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	second, err := storage.CreateEmoji(testCtx(), server.ID, "emoji_"+randHex(8), "PNG", []byte{2}, &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}

	c := newContext(t, http.MethodGet, "/emojis?server_id="+server.ID+"&since="+first.CreatedAt.Format(time.RFC3339Nano), nil, "")
	rec := recorder(c)

	if err := handlers.ListEmojisHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListEmojisHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.EmojiList
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if len(resp.Emojis) != 1 || resp.Emojis[0].ID != second.ID {
		t.Fatalf("esperava apenas o segundo emoji, obtive %+v", resp.Emojis)
	}
}

func TestListEmojisHandlerInvalidSince(t *testing.T) {
	owner := newTestMessageUser(t)
	server, _ := createServerAndChannelTest(t, owner.ID)

	c := newContext(t, http.MethodGet, "/emojis?server_id="+server.ID+"&since=nao-e-data", nil, "")
	rec := recorder(c)

	if err := handlers.ListEmojisHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListEmojisHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"since deve ser um timestamp ISO 8601")
}

func TestCreateEmojiHandlerSuccess(t *testing.T) {
	owner := newTestMessageUser(t)
	server, _ := createServerAndChannelTest(t, owner.ID)
	name := "emoji_" + randHex(8)
	body, _ := json.Marshal(map[string]string{
		"server_id":  server.ID,
		"name":       name,
		"format":     "png",
		"image_blob": base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100)),
	})

	c := newContext(t, http.MethodPost, "/emojis", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := handlers.CreateEmojiHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateEmojiHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var emoji models.Emoji
	if err := json.Unmarshal(rec.Body.Bytes(), &emoji); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if emoji.ID == "" || emoji.ServerID != server.ID || emoji.Name != name {
		t.Errorf("emoji retornado não confere: %+v", emoji)
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
}

func TestCreateEmojiHandlerInvalidBody(t *testing.T) {
	owner := newTestMessageUser(t)
	server, _ := createServerAndChannelTest(t, owner.ID)
	blob := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	gif := base64.StdEncoding.EncodeToString([]byte{0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 1, 1, 0})

	cases := []struct {
		desc string
		body map[string]string
	}{
		{"server_id ausente", map[string]string{"name": "x", "format": "PNG", "image_blob": blob}},
		{"name ausente", map[string]string{"server_id": server.ID, "format": "PNG", "image_blob": blob}},
		{"format ausente", map[string]string{"server_id": server.ID, "name": "x", "image_blob": blob}},
		{"image_blob ausente", map[string]string{"server_id": server.ID, "name": "x", "format": "PNG"}},
		{"name acima de 32 caracteres", map[string]string{"server_id": server.ID, "name": strings.Repeat("a", 33), "format": "PNG", "image_blob": blob}},
		{"format inválido", map[string]string{"server_id": server.ID, "name": "x", "format": "SVG", "image_blob": blob}},
		{"base64 inválido", map[string]string{"server_id": server.ID, "name": "x", "format": "PNG", "image_blob": "!!!"}},
		{"conteúdo não corresponde ao formato", map[string]string{"server_id": server.ID, "name": "x", "format": "PNG", "image_blob": gif}},
	}
	for _, tc := range cases {
		body, _ := json.Marshal(tc.body)
		c := newContext(t, http.MethodPost, "/emojis", body, "")
		c.Set(middleware.UserIDContextKey, owner.ID)
		rec := recorder(c)

		if err := handlers.CreateEmojiHandler(testBaseURL, c); err != nil {
			t.Fatalf("%s: CreateEmojiHandler retornou erro: %v", tc.desc, err)
		}
		assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
			"server_id, name, format e image_blob são obrigatórios; name deve ter no máximo 32 caracteres; "+
				"format deve ser GIF, JPEG ou PNG; image_blob deve ser base64 de uma imagem com no máximo 256kb")
	}
}

func TestCreateEmojiHandlerServerNotFound(t *testing.T) {
	owner := newTestMessageUser(t)
	body, _ := json.Marshal(map[string]string{
		"server_id":  randUUID(),
		"name":       "emoji_" + randHex(8),
		"format":     "PNG",
		"image_blob": base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100)),
	})

	c := newContext(t, http.MethodPost, "/emojis", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := handlers.CreateEmojiHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateEmojiHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "servidor não encontrado")
}

func TestCreateEmojiHandlerNameTaken(t *testing.T) {
	owner := newTestMessageUser(t)
	server, _ := createServerAndChannelTest(t, owner.ID)
	name := "emoji_" + randHex(8)
	body, _ := json.Marshal(map[string]string{
		"server_id":  server.ID,
		"name":       name,
		"format":     "PNG",
		"image_blob": base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100)),
	})

	c := newContext(t, http.MethodPost, "/emojis", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)
	if err := handlers.CreateEmojiHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateEmojiHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201 no primeiro POST, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	// segundo POST com o mesmo nome: 409
	c = newContext(t, http.MethodPost, "/emojis", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec = recorder(c)
	if err := handlers.CreateEmojiHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateEmojiHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusConflict, "emoji-name-taken", "Nome de emoji já existe",
		"o nome informado já está em uso")
}

func TestCreateEmojiHandlerLimitReached(t *testing.T) {
	owner := newTestMessageUser(t)
	server, _ := createServerAndChannelTest(t, owner.ID)
	for i := 0; i < 500; i++ {
		if _, err := storage.CreateEmoji(testCtx(), server.ID, "emoji_"+randHex(8), "PNG", []byte{1}, &owner.ID); err != nil {
			t.Fatalf("falha ao criar emoji %d: %v", i, err)
		}
	}

	body, _ := json.Marshal(map[string]string{
		"server_id":  server.ID,
		"name":       "emoji_" + randHex(8),
		"format":     "PNG",
		"image_blob": base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100)),
	})

	c := newContext(t, http.MethodPost, "/emojis", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := handlers.CreateEmojiHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateEmojiHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusConflict, "emoji-limit-reached", "Limite de emojis atingido",
		"o servidor já possui o número máximo de emojis (500)")
}

func TestCreateEmojiHandlerMissingUserID(t *testing.T) {
	body, _ := json.Marshal(map[string]string{
		"server_id":  "x",
		"name":       "x",
		"format":     "PNG",
		"image_blob": "x",
	})
	c := newContext(t, http.MethodPost, "/emojis", body, "")
	rec := recorder(c)

	if err := handlers.CreateEmojiHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateEmojiHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

func TestDeleteEmojiHandlerSuccess(t *testing.T) {
	owner := newTestMessageUser(t)
	server, _ := createServerAndChannelTest(t, owner.ID)
	emoji, err := storage.CreateEmoji(testCtx(), server.ID, "emoji_"+randHex(8), "PNG", []byte{1}, &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}

	c := newContext(t, http.MethodDelete, "/emojis/"+emoji.ID, nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("emoji_id")
	c.SetParamValues(emoji.ID)
	rec := recorder(c)

	if err := handlers.DeleteEmojiHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteEmojiHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if _, err := storage.GetEmojiByID(testCtx(), emoji.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava emoji removido do banco, obtive erro %v", err)
	}
}

func TestDeleteEmojiHandlerNotFound(t *testing.T) {
	owner := newTestMessageUser(t)

	c := newContext(t, http.MethodDelete, "/emojis/"+randUUID(), nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("emoji_id")
	c.SetParamValues(randUUID())
	rec := recorder(c)

	if err := handlers.DeleteEmojiHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteEmojiHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "emoji não encontrado")
}

func TestDeleteEmojiHandlerForbidden(t *testing.T) {
	owner := newTestMessageUser(t)
	stranger := newTestMessageUser(t)
	server, _ := createServerAndChannelTest(t, owner.ID)
	emoji, err := storage.CreateEmoji(testCtx(), server.ID, "emoji_"+randHex(8), "PNG", []byte{1}, &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}

	c := newContext(t, http.MethodDelete, "/emojis/"+emoji.ID, nil, "")
	c.Set(middleware.UserIDContextKey, stranger.ID)
	c.SetParamNames("emoji_id")
	c.SetParamValues(emoji.ID)
	rec := recorder(c)

	if err := handlers.DeleteEmojiHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteEmojiHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não pode excluir este emoji")
}

func TestDeleteEmojiHandlerMissingUserID(t *testing.T) {
	c := newContext(t, http.MethodDelete, "/emojis/"+randUUID(), nil, "")
	c.SetParamNames("emoji_id")
	c.SetParamValues(randUUID())
	rec := recorder(c)

	if err := handlers.DeleteEmojiHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteEmojiHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

func TestDeleteEmojiHandlerMissingParam(t *testing.T) {
	owner := newTestMessageUser(t)

	c := newContext(t, http.MethodDelete, "/emojis/", nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("emoji_id")
	c.SetParamValues("")
	rec := recorder(c)

	if err := handlers.DeleteEmojiHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteEmojiHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "emoji_id ausente")
}

// --- SearchHandler ---

func TestSearchHandlerSuccess(t *testing.T) {
	owner := newTestMessageUser(t)
	server, channel := createServerAndChannelTest(t, owner.ID)
	unique := "w" + randHex(8)
	msg, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "mensagem "+unique, nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	if _, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "outra mensagem", nil); err != nil {
		t.Fatalf("falha ao criar outra mensagem: %v", err)
	}

	body, _ := json.Marshal(models.SearchRequest{Text: unique})
	c := newContext(t, http.MethodPost, "/search", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := handlers.SearchHandler(testBaseURL, c); err != nil {
		t.Fatalf("SearchHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.SearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("esperava 1 resultado, obtive %d", len(resp.Results))
	}
	r := resp.Results[0]
	if r.Type != "message" || r.ID != msg.ID || r.ChannelID != channel.ID || r.ServerID != server.ID {
		t.Errorf("resultado inesperado: %+v", r)
	}
	if r.AuthorID == nil || *r.AuthorID != owner.ID {
		t.Errorf("esperava author_id %q, obtive %v", owner.ID, r.AuthorID)
	}
	if r.AuthorUsername == nil || *r.AuthorUsername != owner.Username {
		t.Errorf("esperava author_username %q, obtive %v", owner.Username, r.AuthorUsername)
	}
	if r.ChannelName != channel.Name {
		t.Errorf("esperava channel_name %q, obtive %q", channel.Name, r.ChannelName)
	}
	if r.Content == nil || *r.Content != "mensagem "+unique {
		t.Errorf("content inesperado: %v", r.Content)
	}
	if r.Score == nil {
		t.Error("esperava score preenchido para busca com texto")
	}
	if resp.HasMore {
		t.Error("esperava has_more false, obtive true")
	}
}

func TestSearchHandlerSinceCursor(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	unique := "w" + randHex(8)
	m1, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "busca "+unique, nil)
	if err != nil {
		t.Fatalf("falha ao criar m1: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	m2, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "busca "+unique, nil)
	if err != nil {
		t.Fatalf("falha ao criar m2: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	m3, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "busca "+unique, nil)
	if err != nil {
		t.Fatalf("falha ao criar m3: %v", err)
	}

	body, _ := json.Marshal(models.SearchRequest{Text: unique})

	// primeira página: ordem decrescente
	c := newContext(t, http.MethodPost, "/search", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := handlers.SearchHandler(testBaseURL, c); err != nil {
		t.Fatalf("SearchHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.SearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if len(resp.Results) != 3 {
		t.Fatalf("esperava 3 resultados, obtive %d", len(resp.Results))
	}
	if resp.Results[0].ID != m3.ID || resp.Results[1].ID != m2.ID || resp.Results[2].ID != m1.ID {
		t.Errorf("ordem inesperada: %+v", resp.Results)
	}

	// segunda página: cursor do resultado m2 (since + last_id)
	c2 := newContext(t, http.MethodPost, "/search?since="+m2.CreatedAt.Format(time.RFC3339Nano)+"&last_id="+m2.ID, body, "")
	c2.Set(middleware.UserIDContextKey, owner.ID)
	rec2 := recorder(c2)

	if err := handlers.SearchHandler(testBaseURL, c2); err != nil {
		t.Fatalf("SearchHandler retornou erro: %v", err)
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec2.Code, rec2.Body.String())
	}
	var resp2 models.SearchResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if len(resp2.Results) != 1 || resp2.Results[0].ID != m1.ID {
		t.Fatalf("esperava apenas m1 na segunda página, obtive %+v", resp2.Results)
	}
	if resp2.HasMore {
		t.Error("esperava has_more false na segunda página, obtive true")
	}
}

func TestSearchHandlerMissingUserID(t *testing.T) {
	body, _ := json.Marshal(models.SearchRequest{Text: "qualquer"})

	c := newContext(t, http.MethodPost, "/search", body, "")
	rec := recorder(c)

	if err := handlers.SearchHandler(testBaseURL, c); err != nil {
		t.Fatalf("SearchHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

func TestSearchHandlerInvalidJSON(t *testing.T) {
	owner := newTestMessageUser(t)

	c := newContext(t, http.MethodPost, "/search", []byte("{invalido"), "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := handlers.SearchHandler(testBaseURL, c); err != nil {
		t.Fatalf("SearchHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestSearchHandlerNoFilter(t *testing.T) {
	owner := newTestMessageUser(t)

	body, _ := json.Marshal(models.SearchRequest{})
	c := newContext(t, http.MethodPost, "/search", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := handlers.SearchHandler(testBaseURL, c); err != nil {
		t.Fatalf("SearchHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"pelo menos 1 filtro é obrigatório (text, author, date_start, date_end ou contains_attachment); "+
			"order deve ser asc ou desc; date_start e date_end devem estar no formato YYYY-MM-DD com date_start <= date_end; "+
			"since e last_id devem ser informados juntos")
}

func TestSearchHandlerInvalidSince(t *testing.T) {
	owner := newTestMessageUser(t)

	body, _ := json.Marshal(models.SearchRequest{Text: "qualquer"})
	c := newContext(t, http.MethodPost, "/search?since=nao-e-data", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := handlers.SearchHandler(testBaseURL, c); err != nil {
		t.Fatalf("SearchHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "since deve ser um timestamp ISO 8601")
}

func TestSearchHandlerSinceWithoutLastID(t *testing.T) {
	owner := newTestMessageUser(t)

	body, _ := json.Marshal(models.SearchRequest{Text: "qualquer"})
	c := newContext(t, http.MethodPost, "/search?since=2026-01-01T00:00:00Z", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := handlers.SearchHandler(testBaseURL, c); err != nil {
		t.Fatalf("SearchHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"pelo menos 1 filtro é obrigatório (text, author, date_start, date_end ou contains_attachment); "+
			"order deve ser asc ou desc; date_start e date_end devem estar no formato YYYY-MM-DD com date_start <= date_end; "+
			"since e last_id devem ser informados juntos")
}

func TestUpdateUserHandlerBoundaryLengths(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	// 32 runes for nickname and 64 runes for status (multibyte) is accepted
	nickname := "n" + strings.Repeat("ç", 31)
	status := "s" + strings.Repeat("ç", 63)
	body, _ := json.Marshal(map[string]string{"nickname": nickname, "status": status})
	c := newContext(t, http.MethodPut, "/users/"+user.ID, body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := handlers.UpdateUserHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateUserHandler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body: %s)", rec.Code, rec.Body.String())
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
}

func TestUpdateUserHandlerNicknameTooLong(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	// 33 runes
	nickname := "n" + strings.Repeat("ç", 32)
	body, _ := json.Marshal(map[string]string{"nickname": nickname, "status": "ok"})
	c := newContext(t, http.MethodPut, "/users/"+user.ID, body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := handlers.UpdateUserHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateUserHandler retornou erro: %v", err)
	}

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"nickname deve ter no máximo 32 caracteres e status no máximo 64 caracteres")
}
