package test_routes

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
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"papo/internal/config"
	"papo/internal/handlers"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/labstack/echo/v4"
	echoMiddleware "github.com/labstack/echo/v4/middleware"
)

// migrationsDir é o caminho relativo ao diretório deste pacote (backend/internal/handlers/test_routes).
const migrationsDir = "../../../../migrations"

// defaultDatabaseURL corresponde aos padrões do infra/docker-compose.yml.
const defaultDatabaseURL = "postgres://papo:papo123@localhost:5432/papo"

// testJWTSecret fixa o segredo JWT dos testes. Os handlers recebem o cfg
// passado em Register*Routes e o middleware chama config.LoadConfig()
// sozinho, então ambos precisam ler o mesmo valor do ambiente.
const testJWTSecret = "test-jwt-secret-rotas"

func TestMain(m *testing.M) {
	os.Exit(runRoutesTests(m))
}

// runRoutesTests prepara um banco temporário com as migrations do projeto,
// inicializa o storage contra ele, executa os testes e remove o banco ao final.
func runRoutesTests(m *testing.M) int {
	os.Setenv("JWT_SECRET", testJWTSecret)

	baseURL := testDatabaseURL()

	baseDB, err := sql.Open("pgx", baseURL)
	if err != nil {
		fmt.Printf("testes de rotas ignorados: falha ao abrir conexão: %v\n", err)
		return 0
	}
	defer baseDB.Close()

	if err := ping(baseDB); err != nil {
		fmt.Printf("testes de rotas ignorados: não foi possível conectar ao PostgreSQL (%v). Inicie o PostgreSQL (infra/docker-compose.yml) ou defina TEST_DATABASE_URL/DATABASE_URL.\n", err)
		return 0
	}

	removeOldTempDatabases(baseDB)

	tempDBName, err := createTempDatabase(baseDB)
	if err != nil {
		fmt.Printf("testes de rotas ignorados: falha ao criar banco temporário: %v\n", err)
		return 0
	}
	defer dropTempDatabase(baseDB, tempDBName)

	tempURL, err := withDatabase(baseURL, tempDBName)
	if err != nil {
		fmt.Printf("testes de rotas ignorados: %v\n", err)
		return 0
	}

	tempDB, err := sql.Open("pgx", tempURL)
	if err != nil {
		fmt.Printf("testes de rotas ignorados: %v\n", err)
		return 0
	}
	defer tempDB.Close()

	if err := ping(tempDB); err != nil {
		fmt.Printf("testes de rotas ignorados: falha ao conectar no banco temporário: %v\n", err)
	}

	if err := applyMigrations(tempDB); err != nil {
		fmt.Printf("testes de rotas FALHARAM na preparação: %v\n", err)
		return 1
	}

	if err := storage.InitDB(tempURL); err != nil {
		fmt.Printf("testes de rotas FALHARAM na preparação: %v\n", err)
		return 1
	}

	code := m.Run()

	storage.CloseDB()
	return code
}

// exclui servidores nos testes para manter a regra de negócio de 1 servidor por backend
func cleanServers(ctx context.Context) error {
	_, err := storage.GetDB().ExecContext(ctx, "DELETE FROM servers")

	if err != nil {
		return err
	}

	return nil
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

// newApp monta a instância do Echo com as rotas de autenticação, usuários,
// servidores, canais e roles consolidadas em handlers/routes.go
// (tarefas 4.9, 5.2, 5.4 e 6.2).
func newApp() *echo.Echo {
	e := echo.New()
	e.Use(echoMiddleware.RequestID())
	e.Use(echoMiddleware.Recover())

	cfg := config.LoadConfig()
	handlers.RegisterAuthRoutes(e, cfg)
	handlers.RegisterUserRoutes(e, cfg)
	handlers.RegisterServerRoutes(e, cfg)
	handlers.RegisterChannelRoutes(e, cfg)
	handlers.RegisterMessageRoutes(e, cfg)
	handlers.RegisterAttachmentRoutes(e, cfg)
	handlers.RegisterEmojiRoutes(e, cfg)
	handlers.RegisterRoleRoutes(e, cfg)
	handlers.RegisterSearchRoutes(e, cfg)
	return e
}

// do executa uma requisição HTTP real contra o roteador do app, passando por
// rotas e middlewares registrados.
func do(t *testing.T, e *echo.Echo, method, path string, body []byte, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if cookie != nil {
		req.AddCookie(cookie)
	}

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// doMultipart executa uma requisição HTTP real multipart/form-data contra o
// roteador do app. fields são os campos de texto e files mapeia o nome do
// campo para pares (nome do arquivo, conteúdo).
func doMultipart(t *testing.T, e *echo.Echo, method, path string, fields map[string]string, files map[string][][2]string, cookie *http.Cookie) *httptest.ResponseRecorder {
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
	if cookie != nil {
		req.AddCookie(cookie)
	}

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func authCookie(token string) *http.Cookie {
	return &http.Cookie{Name: "Auth", Value: token}
}

// registerAndLogin cria um usuário via POST /auth/register e autentica via
// POST /auth/login, retornando o ID do usuário e o JWT da resposta do login.
func registerAndLogin(t *testing.T, e *echo.Echo) (string, string) {
	t.Helper()

	body, _ := json.Marshal(map[string]string{
		"username": newRandomUsername(),
		"password": newRandomPassword(),
	})

	rec := do(t, e, http.MethodPost, "/auth/register", body, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var reg struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reg); err != nil {
		t.Fatalf("register: falha ao decodificar resposta: %v", err)
	}
	if reg.ID == "" {
		t.Fatal("register: esperava id preenchido")
	}

	rec = do(t, e, http.MethodPost, "/auth/login", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil {
		t.Fatalf("login: falha ao decodificar resposta: %v", err)
	}
	if login.Token == "" {
		t.Fatal("login: esperava token preenchido")
	}

	return reg.ID, login.Token
}

// validSettingsBody retorna um corpo com um user config válido.
func validSettingsBody() []byte {
	cfg := models.UserConfig{
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
	body, _ := json.Marshal(map[string]models.UserConfig{"config": cfg})
	return body
}

// pngAvatarBody retorna um corpo com um PNG mínimo válido em base64.
func pngAvatarBody() []byte {
	png := pngAvatarBytes(100, 100)
	png = append(png, []byte("conteudo-teste")...)
	body, _ := json.Marshal(map[string]string{
		"avatar":        base64.StdEncoding.EncodeToString(png),
		"avatar_format": "png",
	})
	return body
}

// problem é o corpo de erro RFC 7807 retornado pelos handlers e pelo middleware.
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

// assertProblem confere um erro RFC 7807. A URL do campo "type" é calculada
// com a base da configuração, pois o middleware e os handlers usam
// config.LoadConfig() (e não uma base fixa de teste).
func assertProblem(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, slug, wantTitle, wantDetail string) {
	t.Helper()

	if rec.Code != wantStatus {
		t.Fatalf("esperava status %d, obtive %d (corpo: %s)", wantStatus, rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get(echo.HeaderContentType); ct != "application/problem+json" {
		t.Errorf("esperava content-type application/problem+json, obtive %q", ct)
	}

	var p problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("falha ao decodificar problem+json: %v (corpo: %s)", err, rec.Body.String())
	}
	if p.Type != utils.ProblemTypeURL(config.LoadConfig().BaseURL, slug) {
		t.Errorf("esperava type %q, obtive %q", utils.ProblemTypeURL(config.LoadConfig().BaseURL, slug), p.Type)
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

// --- rotas públicas (sem autenticação) ---

// TestRegisterRoute garante que POST /auth/register está mapeado e acessível
// sem cookie.
func TestRegisterRoute(t *testing.T) {
	e := newApp()

	body, _ := json.Marshal(map[string]string{
		"username": newRandomUsername(),
		"password": newRandomPassword(),
	})
	rec := do(t, e, http.MethodPost, "/auth/register", body, nil)

	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID == "" || resp.Username == "" {
		t.Errorf("esperava id e username preenchidos, obtive id=%q username=%q", resp.ID, resp.Username)
	}
}

// TestLoginRoute garante que POST /auth/login está mapeado e acessível sem
// cookie, respondendo o problema de credenciais inválidas para usuário
// inexistente.
func TestLoginRoute(t *testing.T) {
	e := newApp()

	body, _ := json.Marshal(map[string]string{
		"username": newRandomUsername(),
		"password": newRandomPassword(),
	})
	rec := do(t, e, http.MethodPost, "/auth/login", body, nil)

	assertProblem(t, rec, http.StatusUnauthorized, "invalid-credentials", "Credenciais inválidas", "username ou senha incorretos")
}

// TestLogoutRoute garante que POST /auth/logout está mapeado, é público e
// remove o cookie Auth.
func TestLogoutRoute(t *testing.T) {
	e := newApp()

	rec := do(t, e, http.MethodPost, "/auth/logout", nil, nil)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	setCookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "Auth=") {
		t.Errorf("esperava Set-Cookie removendo o cookie Auth, obtive %q", setCookie)
	}
}

// --- middleware JWT nas rotas protegidas ---

// TestProtectedRoutesRequireAuth garante que todas as rotas protegidas
// aplicam o JWTMiddleware: sem o cookie Auth, o roteador responde 401 antes
// de chegar ao handler.
func TestProtectedRoutesRequireAuth(t *testing.T) {
	e := newApp()
	userID, _ := registerAndLogin(t, e)

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/auth/whoami"},
		{http.MethodGet, "/users/profile"},
		{http.MethodPut, "/users/settings"},
		{http.MethodPut, "/users/" + userID},
		{http.MethodPut, "/users/" + userID + "/avatar"},
		{http.MethodPut, "/users/" + userID + "/ban"},
		{http.MethodPost, "/users/" + userID + "/reset"},
		{http.MethodGet, "/servers"},
		{http.MethodPost, "/servers"},
		{http.MethodGet, "/servers/00000000-0000-4000-8000-000000000000"},
		{http.MethodPut, "/servers/00000000-0000-4000-8000-000000000000"},
		{http.MethodGet, "/channels"},
		{http.MethodPost, "/channels"},
		{http.MethodPut, "/channels/00000000-0000-4000-8000-000000000000"},
		{http.MethodDelete, "/channels/00000000-0000-4000-8000-000000000000"},
		{http.MethodGet, "/channels/00000000-0000-4000-8000-000000000000/permissions"},
		{http.MethodPut, "/channels/00000000-0000-4000-8000-000000000000/permissions/00000000-0000-4000-8000-000000000000"},
		{http.MethodGet, "/channels/00000000-0000-4000-8000-000000000000/messages"},
		{http.MethodPost, "/messages"},
		{http.MethodPut, "/messages/00000000-0000-4000-8000-000000000000"},
		{http.MethodDelete, "/messages/00000000-0000-4000-8000-000000000000"},
		{http.MethodGet, "/attachments/00000000-0000-4000-8000-000000000000"},
		{http.MethodGet, "/emojis"},
		{http.MethodPost, "/emojis"},
		{http.MethodDelete, "/emojis/00000000-0000-4000-8000-000000000000"},
		{http.MethodGet, "/servers/00000000-0000-4000-8000-000000000000/roles"},
		{http.MethodPost, "/servers/00000000-0000-4000-8000-000000000000/roles"},
		{http.MethodPut, "/roles/00000000-0000-4000-8000-000000000000"},
		{http.MethodDelete, "/roles/00000000-0000-4000-8000-000000000000"},
		{http.MethodPost, "/users/" + userID + "/roles"},
		{http.MethodDelete, "/users/" + userID + "/roles/00000000-0000-4000-8000-000000000000"},
		{http.MethodPost, "/search"},
	}

	for _, tc := range cases {
		rec := do(t, e, tc.method, tc.path, nil, nil)
		assertProblem(t, rec, http.StatusUnauthorized, "unauthorized",
			"Token inválido ou expirado", "token de autenticação ausente, inválido ou expirado")
	}
}

// TestWhoamiRouteInvalidToken garante que o middleware rejeita token inválido.
func TestWhoamiRouteInvalidToken(t *testing.T) {
	e := newApp()

	rec := do(t, e, http.MethodGet, "/auth/whoami", nil, authCookie("token-invalido"))

	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized",
		"Token inválido ou expirado", "token de autenticação ausente, inválido ou expirado")
}

// --- rotas protegidas com autenticação válida ---

// TestWhoamiRouteWithAuth garante que GET /auth/whoami responde o usuário
// autenticado pelo cookie.
func TestWhoamiRouteWithAuth(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	rec := do(t, e, http.MethodGet, "/auth/whoami", nil, authCookie(token))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID != userID {
		t.Errorf("esperava id %q, obtive %q", userID, resp.ID)
	}
}

// TestProfileRouteWithAuth garante que GET /users/profile responde o perfil
// do usuário autenticado pelo cookie.
func TestProfileRouteWithAuth(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	rec := do(t, e, http.MethodGet, "/users/profile", nil, authCookie(token))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID != userID {
		t.Errorf("esperava id %q, obtive %q", userID, resp.ID)
	}
}

// TestSettingsRoutePrecedence garante que PUT /users/settings é roteado para
// o handler de settings (rota estática) e não para /users/:user_id, em que
// user_id seria "settings" e a resposta seria 403.
func TestSettingsRoutePrecedence(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	rec := do(t, e, http.MethodPut, "/users/settings", validSettingsBody(), authCookie(token))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.UserSettings
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.UserID != userID {
		t.Errorf("esperava user_id %q, obtive %q", userID, resp.UserID)
	}
	if resp.Version != models.CurrentVersion {
		t.Errorf("esperava version %d, obtive %d", models.CurrentVersion, resp.Version)
	}
}

// TestUpdateUserRouteOwn garante que PUT /users/:user_id atualiza o próprio
// usuário autenticado.
func TestUpdateUserRouteOwn(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{"nickname": "nick-teste", "status": "disponível"})
	rec := do(t, e, http.MethodPut, "/users/"+userID, body, authCookie(token))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// TestUpdateUserRouteOtherUserForbidden garante que a atualização do perfil
// de outro usuário é negada com 403 quando a rota é alcançada com
// autenticação válida.
func TestUpdateUserRouteOtherUserForbidden(t *testing.T) {
	e := newApp()
	_, tokenA := registerAndLogin(t, e)
	userB, _ := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{"nickname": "x", "status": "y"})
	rec := do(t, e, http.MethodPut, "/users/"+userB, body, authCookie(tokenA))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado", "não é possível atualizar o perfil de outro usuário")
}

// TestAvatarRouteWithAuth garante que PUT /users/:user_id/avatar aceita um
// PNG válido do próprio usuário autenticado.
func TestAvatarRouteWithAuth(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	rec := do(t, e, http.MethodPut, "/users/"+userID+"/avatar", pngAvatarBody(), authCookie(token))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// --- PUT /users/:user_id/ban ---

// newBanBody monta o corpo do endpoint de banimento.
func newBanBody(targetID string, banState bool) []byte {
	body, _ := json.Marshal(map[string]interface{}{
		"user_id":   targetID,
		"ban_state": banState,
	})
	return body
}

// ownerTokenFor cria um servidor para o usuário autenticado e retorna o
// token, garantindo que ele passa em RequireServerOwnerOrManageServer.
func ownerTokenFor(t *testing.T, e *echo.Echo) (string, string) {
	t.Helper()
	userID, token := registerAndLogin(t, e)
	if _, err := storage.CreateServer(context.Background(), "srv_"+randHex(4), &userID); err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	return userID, token
}

// cleanupBan reverte o banimento do usuário ao final do teste. Sem isso o
// bloqueio por IP (todos os testes compartilham o mesmo IP de cliente)
// impediria os registros dos testes subsequentes.
func cleanupBan(t *testing.T, userID string) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := storage.SetUserBanned(context.Background(), userID, false); err != nil {
			t.Errorf("falha ao desbanir usuário no cleanup: %v", err)
		}
	})
}

// TestBanRouteOwnerBansUser garante que o dono de um servidor consegue banir
// o usuário alvo e que o estado é persistido.
func TestBanRouteOwnerBansUser(t *testing.T) {
	e := newApp()
	_, ownerToken := ownerTokenFor(t, e)
	targetID, _ := registerAndLogin(t, e)
	cleanupBan(t, targetID)

	rec := do(t, e, http.MethodPut, "/users/"+targetID+"/ban", newBanBody(targetID, true), authCookie(ownerToken))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.Response != "User state changed successfully" {
		t.Errorf("esperava response %q, obtive %q", "User state changed successfully", resp.Response)
	}

	stored, err := storage.GetUserByID(context.Background(), targetID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if !stored.Banned {
		t.Error("esperava banned = true persistido")
	}
}

// TestBanRouteOwnerUnbansUser garante que ban_state=false reverte o
// banimento.
func TestBanRouteOwnerUnbansUser(t *testing.T) {
	e := newApp()
	_, ownerToken := ownerTokenFor(t, e)
	targetID, _ := registerAndLogin(t, e)
	cleanupBan(t, targetID)

	if rec := do(t, e, http.MethodPut, "/users/"+targetID+"/ban", newBanBody(targetID, true), authCookie(ownerToken)); rec.Code != http.StatusOK {
		t.Fatalf("ban: esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	rec := do(t, e, http.MethodPut, "/users/"+targetID+"/ban", newBanBody(targetID, false), authCookie(ownerToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("unban: esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	stored, err := storage.GetUserByID(context.Background(), targetID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if stored.Banned {
		t.Error("esperava banned = false após o unban")
	}
}

// TestBanRouteWithManageServerRole garante que um usuário com a permissão
// manage_server (sem ser dono de servidor) consegue banir.
func TestBanRouteWithManageServerRole(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	actorID, actorToken := registerAndLogin(t, e)
	targetID, _ := registerAndLogin(t, e)
	cleanupBan(t, targetID)

	server, err := storage.CreateServer(context.Background(), "srv_"+randHex(4), &ownerID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	role, err := storage.CreateRole(context.Background(), server.ID, "role_"+randHex(8), nil, models.RolePermissions{ManageServer: true})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	if _, err := storage.AssignUserRole(context.Background(), actorID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}

	rec := do(t, e, http.MethodPut, "/users/"+targetID+"/ban", newBanBody(targetID, true), authCookie(actorToken))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// TestBanRouteForbiddenWithoutPermission garante que um usuário sem servidor
// próprio nem permissão manage_server é negado com 403.
func TestBanRouteForbiddenWithoutPermission(t *testing.T) {
	e := newApp()
	_, actorToken := registerAndLogin(t, e)
	targetID, _ := registerAndLogin(t, e)

	rec := do(t, e, http.MethodPut, "/users/"+targetID+"/ban", newBanBody(targetID, true), authCookie(actorToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação")
}

// TestBanRouteUnauthenticated garante que PUT /users/:user_id/ban exige
// autenticação.
func TestBanRouteUnauthenticated(t *testing.T) {
	e := newApp()
	targetID, _ := registerAndLogin(t, e)

	rec := do(t, e, http.MethodPut, "/users/"+targetID+"/ban", newBanBody(targetID, true), nil)

	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized",
		"Token inválido ou expirado", "token de autenticação ausente, inválido ou expirado")
}

// TestBanRouteMissingBanState garante que o campo ban_state é obrigatório.
func TestBanRouteMissingBanState(t *testing.T) {
	e := newApp()
	_, ownerToken := ownerTokenFor(t, e)
	targetID, _ := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{"user_id": targetID})
	rec := do(t, e, http.MethodPut, "/users/"+targetID+"/ban", body, authCookie(ownerToken))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"campo 'ban_state' é obrigatório")
}

// TestBanRouteNotFound garante que o endpoint responde 404 para usuário alvo
// inexistente.
func TestBanRouteNotFound(t *testing.T) {
	e := newApp()
	_, ownerToken := ownerTokenFor(t, e)

	rec := do(t, e, http.MethodPut, "/users/00000000-0000-4000-8000-000000000000/ban",
		newBanBody("00000000-0000-4000-8000-000000000000", true), authCookie(ownerToken))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "usuário não encontrado")
}

// TestBanRouteURLIDTakesPrecedenceOverBody garante que o id da URL é o
// autoritativo: o user_id do corpo é ignorado.
func TestBanRouteURLIDTakesPrecedenceOverBody(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := ownerTokenFor(t, e)
	targetID, _ := registerAndLogin(t, e)
	cleanupBan(t, targetID)

	rec := do(t, e, http.MethodPut, "/users/"+targetID+"/ban", newBanBody(ownerID, true), authCookie(ownerToken))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	target, err := storage.GetUserByID(context.Background(), targetID)
	if err != nil {
		t.Fatalf("GetUserByID (alvo) retornou erro: %v", err)
	}
	if !target.Banned {
		t.Error("esperava o alvo da URL banido")
	}

	owner, err := storage.GetUserByID(context.Background(), ownerID)
	if err != nil {
		t.Fatalf("GetUserByID (dono) retornou erro: %v", err)
	}
	if owner.Banned {
		t.Error("não esperava o user_id do corpo ser banido")
	}
}

// --- POST /users/:user_id/reset ---

// TestResetRouteOwnerResetsUser garante que o dono de um servidor consegue
// marcar o usuário alvo para reset de senha e que o estado é persistido.
func TestResetRouteOwnerResetsUser(t *testing.T) {
	e := newApp()
	_, ownerToken := ownerTokenFor(t, e)
	targetID, _ := registerAndLogin(t, e)

	rec := do(t, e, http.MethodPost, "/users/"+targetID+"/reset", nil, authCookie(ownerToken))

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

	stored, err := storage.GetUserByID(context.Background(), targetID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if !stored.ResetPassword {
		t.Error("esperava reset_password = true persistido")
	}
}

// TestResetRouteSelfResetsSelf garante que o usuário pode marcar a si mesmo
// para reset, mesmo sem ser dono de nenhum servidor.
func TestResetRouteSelfResetsSelf(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	rec := do(t, e, http.MethodPost, "/users/"+userID+"/reset", nil, authCookie(token))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	stored, err := storage.GetUserByID(context.Background(), userID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if !stored.ResetPassword {
		t.Error("esperava reset_password = true persistido")
	}
}

// TestResetRouteForbiddenWithoutPermission garante que um usuário sem
// servidor próprio é negado com 403 ao tentar resetar outro usuário.
func TestResetRouteForbiddenWithoutPermission(t *testing.T) {
	e := newApp()
	_, actorToken := registerAndLogin(t, e)
	targetID, _ := registerAndLogin(t, e)

	rec := do(t, e, http.MethodPost, "/users/"+targetID+"/reset", nil, authCookie(actorToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação")
}

// TestResetRouteUnauthenticated garante que POST /users/:user_id/reset exige
// autenticação.
func TestResetRouteUnauthenticated(t *testing.T) {
	e := newApp()
	targetID, _ := registerAndLogin(t, e)

	rec := do(t, e, http.MethodPost, "/users/"+targetID+"/reset", nil, nil)

	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized",
		"Token inválido ou expirado", "token de autenticação ausente, inválido ou expirado")
}

// TestResetRouteNotFound garante que o endpoint responde 404 para usuário
// alvo inexistente.
func TestResetRouteNotFound(t *testing.T) {
	e := newApp()
	_, ownerToken := ownerTokenFor(t, e)

	rec := do(t, e, http.MethodPost, "/users/00000000-0000-4000-8000-000000000000/reset", nil, authCookie(ownerToken))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "usuário não encontrado")
}

// --- GET /users (tarefa 6.4) ---

func TestListUsersRouteWithAuth(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	rec := do(t, e, http.MethodGet, "/users", nil, authCookie(token))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Users []struct {
			ID string `json:"id"`
		} `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	found := false
	for _, u := range resp.Users {
		if u.ID == userID {
			found = true
		}
	}
	if !found {
		t.Error("usuário autenticado não aparece na listagem")
	}
}

func TestListUsersRouteUnauthenticated(t *testing.T) {
	e := newApp()

	rec := do(t, e, http.MethodGet, "/users", nil, nil)

	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized",
		"Token inválido ou expirado", "token de autenticação ausente, inválido ou expirado")
}

// --- PUT /users/:user_id/password (tarefa 6.4) ---

func TestChangePasswordRouteSelf(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	user, err := storage.GetUserByID(context.Background(), userID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}

	if err := storage.SetUserResetPassword(context.Background(), userID); err != nil {
		t.Fatalf("falha ao marcar usuário para reset: %v", err)
	}

	newPassword := newRandomPassword()
	body, _ := json.Marshal(map[string]string{"password": newPassword})

	rec := do(t, e, http.MethodPut, "/users/"+userID+"/password", body, authCookie(token))

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

	stored, err := storage.GetUserByUsername(context.Background(), user.Username)
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

func TestChangePasswordRouteOtherUserForbidden(t *testing.T) {
	e := newApp()
	_, actorToken := registerAndLogin(t, e)
	targetID, _ := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{"password": newRandomPassword()})

	rec := do(t, e, http.MethodPut, "/users/"+targetID+"/password", body, authCookie(actorToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"não é possível alterar a senha de outro usuário")
}

func TestChangePasswordRouteUnauthenticated(t *testing.T) {
	e := newApp()
	userID, _ := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{"password": newRandomPassword()})

	rec := do(t, e, http.MethodPut, "/users/"+userID+"/password", body, nil)

	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized",
		"Token inválido ou expirado", "token de autenticação ausente, inválido ou expirado")
}

func TestChangePasswordRouteMissingPassword(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{})

	err := storage.SetUserResetPassword(context.Background(), userID)

	if err != nil {
		t.Fatalf("SetUserResetPassword retornou erro: %v", err)
	}

	rec := do(t, e, http.MethodPut, "/users/"+userID+"/password", body, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"campo 'password' é obrigatório")
}

// --- mapeamento de método e path ---

// TestWrongMethodReturns405 garante que os métodos registrados são os
// corretos: método errado em path conhecido responde 405 (e não 401/404).
func TestWrongMethodReturns405(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/auth/register"},
		{http.MethodGet, "/auth/login"},
		{http.MethodGet, "/auth/logout"},
		{http.MethodGet, "/users/settings"},
		{http.MethodPost, "/users"},
		{http.MethodGet, "/users/" + userID + "/password"},
		{http.MethodDelete, "/servers"},
		{http.MethodDelete, "/servers/00000000-0000-4000-8000-000000000000"},
		{http.MethodPost, "/servers/00000000-0000-4000-8000-000000000000"},
		{http.MethodDelete, "/channels"},
		{http.MethodGet, "/channels/00000000-0000-4000-8000-000000000000"},
		{http.MethodPost, "/channels/00000000-0000-4000-8000-000000000000"},
		{http.MethodGet, "/channels/00000000-0000-4000-8000-000000000000/permissions/00000000-0000-4000-8000-000000000000"},
		{http.MethodPost, "/channels/00000000-0000-4000-8000-000000000000/messages"},
		{http.MethodGet, "/messages"},
		{http.MethodGet, "/messages/00000000-0000-4000-8000-000000000000"},
		{http.MethodDelete, "/messages"},
		{http.MethodDelete, "/servers/00000000-0000-4000-8000-000000000000/roles"},
		{http.MethodGet, "/roles/00000000-0000-4000-8000-000000000000"},
		{http.MethodDelete, "/users/" + userID + "/roles"},
		{http.MethodPut, "/users/" + userID + "/roles/00000000-0000-4000-8000-000000000000"},
		{http.MethodGet, "/search"},
	}

	for _, tc := range cases {
		rec := do(t, e, tc.method, tc.path, nil, authCookie(token))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: esperava status 405, obtive %d (corpo: %s)", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// TestUnknownRouteReturns404 garante que paths desconhecidos respondem 404.
func TestUnknownRouteReturns404(t *testing.T) {
	e := newApp()

	rec := do(t, e, http.MethodGet, "/auth/rota-inexistente", nil, nil)

	if rec.Code != http.StatusNotFound {
		t.Errorf("esperava status 404, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// --- rotas de servidores (tarefa 5.2) ---

// TestListServersRouteWithAuth garante que GET /servers responde a listagem
// de servidores para o usuário autenticado pelo cookie.
func TestListServersRouteWithAuth(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	serverName := "srv_" + randHex(4)
	server, err := storage.CreateServer(context.Background(), serverName, &userID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	rec := do(t, e, http.MethodGet, "/servers", nil, authCookie(token))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Servers []struct {
			ID            string  `json:"id"`
			Name          string  `json:"name"`
			OwnerID       *string `json:"owner_id"`
			OwnerUsername *string `json:"owner_username"`
			ChannelCount  int     `json:"channel_count"`
			MemberCount   int     `json:"member_count"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.Servers == nil {
		t.Fatal("esperava servers como lista, obtive null")
	}

	var found bool
	for _, s := range resp.Servers {
		if s.ID != server.ID {
			continue
		}
		found = true
		if s.Name != serverName {
			t.Errorf("esperava name %q, obtive %q", serverName, s.Name)
		}
		if s.OwnerID == nil || *s.OwnerID != userID {
			t.Errorf("esperava owner_id %q, obtive %v", userID, s.OwnerID)
		}
		if s.OwnerUsername == nil {
			t.Error("esperava owner_username preenchido")
		}
		if s.ChannelCount != 0 {
			t.Errorf("esperava channel_count 0, obtive %d", s.ChannelCount)
		}
		if s.MemberCount < 1 {
			t.Errorf("esperava member_count >= 1, obtive %d", s.MemberCount)
		}
		break
	}
	if !found {
		t.Errorf("servidor %s não apareceu na listagem", server.ID)
	}
}

// TestGetServerRouteWithAuth garante que GET /servers/:server_id responde o
// detalhe do servidor para o usuário autenticado pelo cookie.
func TestGetServerRouteWithAuth(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	serverName := "srv_" + randHex(4)
	server, err := storage.CreateServer(context.Background(), serverName, &userID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	rec := do(t, e, http.MethodGet, "/servers/"+server.ID, nil, authCookie(token))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID           string  `json:"id"`
		Name         string  `json:"name"`
		OwnerID      *string `json:"owner_id"`
		RoleCount    int     `json:"role_count"`
		MemberCount  int     `json:"member_count"`
		ChannelCount int     `json:"channel_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID != server.ID {
		t.Errorf("esperava id %q, obtive %q", server.ID, resp.ID)
	}
	if resp.Name != serverName {
		t.Errorf("esperava name %q, obtive %q", serverName, resp.Name)
	}
	if resp.OwnerID == nil || *resp.OwnerID != userID {
		t.Errorf("esperava owner_id %q, obtive %v", userID, resp.OwnerID)
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
}

// TestGetServerRouteNotFound garante que GET /servers/:server_id responde
// 404 para servidor inexistente.
func TestGetServerRouteNotFound(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	rec := do(t, e, http.MethodGet, "/servers/00000000-0000-4000-8000-000000000000", nil, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "servidor não encontrado")
}

// TestUpdateServerRouteWithAuth garante que PUT /servers/:server_id atualiza
// o servidor do usuário autenticado pelo cookie.
func TestUpdateServerRouteWithAuth(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	serverName := "srv_" + randHex(4)
	server, err := storage.CreateServer(context.Background(), serverName, &userID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	newName := "srv_" + randHex(4)
	body, _ := json.Marshal(map[string]string{"name": newName})
	rec := do(t, e, http.MethodPut, "/servers/"+server.ID, body, authCookie(token))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID != server.ID {
		t.Errorf("esperava id %q, obtive %q", server.ID, resp.ID)
	}
	if resp.Name != newName {
		t.Errorf("esperava name %q, obtive %q", newName, resp.Name)
	}

	stored, err := storage.GetServerByID(context.Background(), server.ID)
	if err != nil {
		t.Fatalf("GetServerByID retornou erro: %v", err)
	}
	if stored.Name != newName {
		t.Errorf("esperava name %q persistido, obtive %q", newName, stored.Name)
	}
}

// TestUpdateServerRouteOtherUserForbidden garante que a atualização de um
// servidor de outro usuário é negada com 403.
func TestUpdateServerRouteOtherUserForbidden(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	_, token := registerAndLogin(t, e)

	server, err := storage.CreateServer(context.Background(), "srv_"+randHex(4), &ownerID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"name": "srv_" + randHex(4)})
	rec := do(t, e, http.MethodPut, "/servers/"+server.ID, body, authCookie(token))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado", "usuário não possui a permissão necessária para esta operação")
}

// --- POST /servers ---

// TestCreateServerRouteWithAuth garante que POST /servers cria o servidor
// com o usuário autenticado como dono.
func TestCreateServerRouteWithAuth(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	userID, token := registerAndLogin(t, e)

	serverName := "srv_" + randHex(4)
	body, _ := json.Marshal(map[string]any{"name": serverName, "Public": true})
	rec := do(t, e, http.MethodPost, "/servers", body, authCookie(token))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID            string  `json:"id"`
		Name          string  `json:"name"`
		OwnerID       *string `json:"owner_id"`
		OwnerUsername *string `json:"owner_username"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID == "" {
		t.Error("esperava id preenchido")
	}
	if resp.Name != serverName {
		t.Errorf("esperava name %q, obtive %q", serverName, resp.Name)
	}
	if resp.OwnerID == nil || *resp.OwnerID != userID {
		t.Errorf("esperava owner_id %s, obtive %v", userID, resp.OwnerID)
	}

	stored, err := storage.GetServerByID(context.Background(), resp.ID)
	if err != nil {
		t.Fatalf("GetServerByID retornou erro: %v", err)
	}
	if stored.Name != serverName {
		t.Errorf("esperava name %q persistido, obtive %q", serverName, stored.Name)
	}
	if stored.OwnerID == nil || *stored.OwnerID != userID {
		t.Errorf("esperava owner_id %s persistido, obtive %v", userID, stored.OwnerID)
	}
}

// TestCreateServerRouteUnauthenticated garante que POST /servers exige
// autenticação.
func TestCreateServerRouteUnauthenticated(t *testing.T) {
	e := newApp()

	body, _ := json.Marshal(map[string]string{"name": "srv_" + randHex(4)})
	rec := do(t, e, http.MethodPost, "/servers", body, nil)

	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized",
		"Token inválido ou expirado", "token de autenticação ausente, inválido ou expirado")
}

// TestCreateServerRouteInvalidInput garante que POST /servers responde 400
// para nome ausente.
func TestCreateServerRouteInvalidInput(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{"icon_format": "PNG"})
	rec := do(t, e, http.MethodPost, "/servers", body, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"name é obrigatório e deve ter no máximo 32 caracteres; icon_blob deve ser base64 de um GIF, JPEG ou PNG de até 2MB; servidor privado (public=false) exige password")
}

// --- helpers de setup para canais e roles ---

// createServerFor cria um servidor para o usuário e retorna o registro criado.
func createServerFor(t *testing.T, userID string) models.Server {
	t.Helper()
	server, err := storage.CreateServer(context.Background(), "srv_"+randHex(4), &userID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	return server
}

// createChannelFor cria um canal text em um servidor e retorna o registro criado.
func createChannelFor(t *testing.T, serverID, name string) models.Channel {
	t.Helper()
	channel, err := storage.CreateChannel(context.Background(), serverID, name, "text")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	return channel
}

// createRoleFor cria uma role em um servidor e retorna o registro criado.
func createRoleFor(t *testing.T, serverID string, permissions models.RolePermissions) models.Role {
	t.Helper()
	role, err := storage.CreateRole(context.Background(), serverID, "role_"+randHex(8), nil, permissions)
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	return role
}

// assignRoleToUser atribui uma role a um usuário via storage.
func assignRoleToUser(t *testing.T, userID, roleID string) {
	t.Helper()
	if _, err := storage.AssignUserRole(context.Background(), userID, roleID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}
}

// --- rotas de canais (tarefas 5.3 e 5.4) ---

// TestListChannelsRouteWithAuth garante que GET /channels responde a
// listagem de canais para o usuário autenticado pelo cookie.
func TestListChannelsRouteWithAuth(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	channelName := "chn_" + randHex(4)
	channel := createChannelFor(t, server.ID, channelName)

	rec := do(t, e, http.MethodGet, "/channels", nil, authCookie(token))

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
		t.Fatal("esperava channels como lista, obtive null")
	}

	var found *models.ChannelSummary
	for i := range resp.Channels {
		if resp.Channels[i].ID == channel.ID {
			found = &resp.Channels[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("canal %s não apareceu na listagem", channel.ID)
	}
	if found.ServerID != server.ID {
		t.Errorf("esperava server_id %q, obtive %q", server.ID, found.ServerID)
	}
	if found.Name != channelName {
		t.Errorf("esperava name %q, obtive %q", channelName, found.Name)
	}
	if found.Type != "text" {
		t.Errorf("esperava type %q, obtive %q", "text", found.Type)
	}
	if len(found.Permissions) != 0 {
		t.Errorf("esperava permissions vazia, obtive %v", found.Permissions)
	}
	if found.LastMessage != nil {
		t.Errorf("esperava last_message null, obtive %v", found.LastMessage)
	}
}

// TestListChannelsRouteFilterByServer garante que GET /channels?server_id
// filtra os canais por servidor.
func TestListChannelsRouteFilterByServer(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	serverA := createServerFor(t, userID)
	serverB := createServerFor(t, userID)
	channel := createChannelFor(t, serverA.ID, "chn_"+randHex(4))

	type channelListResponse struct {
		Channels []models.ChannelSummary `json:"channels"`
	}

	rec := do(t, e, http.MethodGet, "/channels?server_id="+serverA.ID, nil, authCookie(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var respA channelListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &respA); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if len(respA.Channels) != 1 || respA.Channels[0].ID != channel.ID {
		t.Fatalf("esperava apenas o canal do servidor A, obtive %d canais", len(respA.Channels))
	}

	rec = do(t, e, http.MethodGet, "/channels?server_id="+serverB.ID, nil, authCookie(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var respB channelListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &respB); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if len(respB.Channels) != 0 {
		t.Errorf("esperava lista vazia para o servidor B, obtive %d canais", len(respB.Channels))
	}
}

// TestCreateChannelRouteOwner garante que o dono do servidor cria um canal
// via POST /channels e que o canal é persistido.
func TestCreateChannelRouteOwner(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	channelName := "chn_" + randHex(4)

	body, _ := json.Marshal(map[string]string{"server_id": server.ID, "name": channelName})
	rec := do(t, e, http.MethodPost, "/channels", body, authCookie(token))

	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.ChannelSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID == "" || resp.ServerID != server.ID || resp.Name != channelName {
		t.Errorf("resposta inesperada: %+v", resp)
	}
	if resp.Type != "text" {
		t.Errorf("esperava type %q, obtive %q", "text", resp.Type)
	}
	if resp.LastMessage != nil {
		t.Errorf("esperava last_message null, obtive %v", resp.LastMessage)
	}

	stored, err := storage.GetChannelByID(context.Background(), resp.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Name != channelName || stored.ServerID != server.ID {
		t.Errorf("persistência inesperada: %+v", stored)
	}
}

// TestCreateChannelRouteForbiddenWithoutPermission garante que um usuário sem
// permissão manage_channels é negado com 403.
func TestCreateChannelRouteForbiddenWithoutPermission(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	_, actorToken := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)

	body, _ := json.Marshal(map[string]string{"server_id": server.ID, "name": "chn_" + randHex(4)})
	rec := do(t, e, http.MethodPost, "/channels", body, authCookie(actorToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação")
}

// TestCreateChannelRouteWithManageChannelsRole garante que um usuário com a
// permissão manage_channels no servidor cria canal sem ser dono.
func TestCreateChannelRouteWithManageChannelsRole(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	actorID, actorToken := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)
	role := createRoleFor(t, server.ID, models.RolePermissions{ManageChannels: true})
	assignRoleToUser(t, actorID, role.ID)

	body, _ := json.Marshal(map[string]string{"server_id": server.ID, "name": "chn_" + randHex(4)})
	rec := do(t, e, http.MethodPost, "/channels", body, authCookie(actorToken))

	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// TestCreateChannelRouteRoleOnOtherServerForbidden garante que a permissão
// manage_channels de outro servidor não autoriza a criação neste servidor.
func TestCreateChannelRouteRoleOnOtherServerForbidden(t *testing.T) {
	e := newApp()
	ownerA, _ := registerAndLogin(t, e)
	ownerB, _ := registerAndLogin(t, e)
	actorID, actorToken := registerAndLogin(t, e)
	serverA := createServerFor(t, ownerA)
	serverB := createServerFor(t, ownerB)
	role := createRoleFor(t, serverB.ID, models.RolePermissions{ManageChannels: true})
	assignRoleToUser(t, actorID, role.ID)

	body, _ := json.Marshal(map[string]string{"server_id": serverA.ID, "name": "chn_" + randHex(4)})
	rec := do(t, e, http.MethodPost, "/channels", body, authCookie(actorToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação")
}

// TestCreateChannelRouteInvalidInput garante que POST /channels responde 400
// para nome ausente ou acima de 32 caracteres.
func TestCreateChannelRouteInvalidInput(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)

	for _, name := range []string{"", strings.Repeat("c", 33)} {
		body, _ := json.Marshal(map[string]string{"server_id": server.ID, "name": name})
		rec := do(t, e, http.MethodPost, "/channels", body, authCookie(token))
		assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
			"server_id e name são obrigatórios; name deve ter no máximo 32 caracteres")
	}
}

// TestCreateChannelRouteServerNotFound garante que POST /channels responde
// 404 para servidor inexistente.
func TestCreateChannelRouteServerNotFound(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{
		"server_id": "00000000-0000-4000-8000-000000000000",
		"name":      "chn_" + randHex(4),
	})
	rec := do(t, e, http.MethodPost, "/channels", body, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "servidor não encontrado")
}

// TestCreateChannelRouteNameTaken garante que POST /channels responde 409
// quando o nome do canal já está em uso.
func TestCreateChannelRouteNameTaken(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	channelName := "chn_" + randHex(4)
	createChannelFor(t, server.ID, channelName)

	body, _ := json.Marshal(map[string]string{"server_id": server.ID, "name": channelName})
	rec := do(t, e, http.MethodPost, "/channels", body, authCookie(token))

	assertProblem(t, rec, http.StatusConflict, "channel-name-taken", "Nome de canal já existe",
		"o nome informado já está em uso")
}

// TestUpdateChannelRouteOwner garante que o dono do servidor renomeia o canal
// via PUT /channels/:channel_id e que o novo nome é persistido.
func TestUpdateChannelRouteOwner(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))
	newName := "chn_" + randHex(4)

	body, _ := json.Marshal(map[string]string{"name": newName})
	rec := do(t, e, http.MethodPut, "/channels/"+channel.ID, body, authCookie(token))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.ChannelSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID != channel.ID || resp.Name != newName {
		t.Errorf("resposta inesperada: %+v", resp)
	}

	stored, err := storage.GetChannelByID(context.Background(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Name != newName {
		t.Errorf("esperava name %q persistido, obtive %q", newName, stored.Name)
	}
}

// TestUpdateChannelRouteForbiddenWithoutPermission garante que um usuário sem
// permissão manage_channels no servidor do canal é negado com 403.
func TestUpdateChannelRouteForbiddenWithoutPermission(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	_, actorToken := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))

	body, _ := json.Marshal(map[string]string{"name": "chn_" + randHex(4)})
	rec := do(t, e, http.MethodPut, "/channels/"+channel.ID, body, authCookie(actorToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação")
}

// TestUpdateChannelRouteInvalidInput garante que PUT /channels/:channel_id
// responde 400 para nome ausente.
func TestUpdateChannelRouteInvalidInput(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))

	body, _ := json.Marshal(map[string]string{"name": ""})
	rec := do(t, e, http.MethodPut, "/channels/"+channel.ID, body, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"name é obrigatório e deve ter no máximo 32 caracteres")
}

// TestUpdateChannelRouteNotFound garante que PUT /channels/:channel_id
// responde 404 para canal inexistente.
func TestUpdateChannelRouteNotFound(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{"name": "chn_" + randHex(4)})
	rec := do(t, e, http.MethodPut, "/channels/00000000-0000-4000-8000-000000000000", body, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

// TestDeleteChannelRouteOwner garante que o dono do servidor exclui o canal
// via DELETE /channels/:channel_id.
func TestDeleteChannelRouteOwner(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))

	rec := do(t, e, http.MethodDelete, "/channels/"+channel.ID, nil, authCookie(token))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	if _, err := storage.GetChannelByID(context.Background(), channel.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava o canal removido do banco, obtive erro %v", err)
	}
}

// TestDeleteChannelRouteNotFound garante que DELETE /channels/:channel_id
// responde 404 para canal inexistente.
func TestDeleteChannelRouteNotFound(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	rec := do(t, e, http.MethodDelete, "/channels/00000000-0000-4000-8000-000000000000", nil, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

// TestGetChannelPermissionsRouteWithAuth garante que
// GET /channels/:channel_id/permissions responde as permissões do canal
// expandidas por role.
func TestGetChannelPermissionsRouteWithAuth(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))
	role := createRoleFor(t, server.ID, models.RolePermissions{})

	permission := models.ChannelPermission{ReadChannel: true, SendMessages: true}
	if _, err := storage.UpdateChannelPermissions(context.Background(), channel.ID, role.ID, permission); err != nil {
		t.Fatalf("falha ao atualizar permissões do canal: %v", err)
	}

	rec := do(t, e, http.MethodGet, "/channels/"+channel.ID+"/permissions", nil, authCookie(token))

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
		t.Errorf("esperava channel_id %q, obtive %q", channel.ID, resp.ChannelID)
	}
	if len(resp.Permissions) != 1 {
		t.Fatalf("esperava 1 entrada de permissão, obtive %d", len(resp.Permissions))
	}
	entry := resp.Permissions[0]
	if entry.RoleID != role.ID || entry.RoleName != role.Name {
		t.Errorf("entrada inesperada: %+v", entry)
	}
	if entry.Permissions != permission {
		t.Errorf("esperava permissions %+v, obtive %+v", permission, entry.Permissions)
	}
}

// TestGetChannelPermissionsRouteNotFound garante que
// GET /channels/:channel_id/permissions responde 404 para canal inexistente.
func TestGetChannelPermissionsRouteNotFound(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	rec := do(t, e, http.MethodGet, "/channels/00000000-0000-4000-8000-000000000000/permissions", nil, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

// TestUpdateChannelPermissionsRouteOwner garante que o dono do servidor
// atualiza as permissões de uma role no canal via
// PUT /channels/:channel_id/permissions/:role_id.
func TestUpdateChannelPermissionsRouteOwner(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))
	role := createRoleFor(t, server.ID, models.RolePermissions{})

	permission := models.ChannelPermission{ReadChannel: true, DeleteMessages: true}
	body, _ := json.Marshal(map[string]models.ChannelPermission{"permissions": permission})
	rec := do(t, e, http.MethodPut, "/channels/"+channel.ID+"/permissions/"+role.ID, body, authCookie(token))

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
	if resp.ChannelID != channel.ID || resp.RoleID != role.ID || resp.Permissions != permission {
		t.Errorf("resposta inesperada: %+v", resp)
	}

	stored, err := storage.GetChannelByID(context.Background(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Permissions[role.ID] != permission {
		t.Errorf("esperava permissões %+v persistidas para a role, obtive %v", permission, stored.Permissions[role.ID])
	}
}

// TestUpdateChannelPermissionsRouteRoleFromOtherServer garante que uma role
// de outro servidor é tratada como inexistente para o canal (404).
func TestUpdateChannelPermissionsRouteRoleFromOtherServer(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	serverA := createServerFor(t, userID)
	serverB := createServerFor(t, userID)
	channel := createChannelFor(t, serverA.ID, "chn_"+randHex(4))
	role := createRoleFor(t, serverB.ID, models.RolePermissions{})

	body, _ := json.Marshal(map[string]models.ChannelPermission{
		"permissions": {ReadChannel: true},
	})
	rec := do(t, e, http.MethodPut, "/channels/"+channel.ID+"/permissions/"+role.ID, body, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "role não encontrada")
}

// TestUpdateChannelPermissionsRouteNotFound garante que
// PUT /channels/:channel_id/permissions/:role_id responde 404 para canal
// inexistente.
func TestUpdateChannelPermissionsRouteNotFound(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]models.ChannelPermission{
		"permissions": {ReadChannel: true},
	})
	rec := do(t, e, http.MethodPut,
		"/channels/00000000-0000-4000-8000-000000000000/permissions/00000000-0000-4000-8000-000000000000",
		body, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

// --- rotas de posição de canal (tarefa 8.4) ---

// TestChangeChannelPositionRouteOwner garante que o dono do servidor muda a
// posição de um canal via PUT /channels/:channel_id/change_position e que a
// ordem é persistida.
func TestChangeChannelPositionRouteOwner(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	c1 := createChannelFor(t, server.ID, "chn_"+randHex(4))
	c2 := createChannelFor(t, server.ID, "chn_"+randHex(4))
	c3 := createChannelFor(t, server.ID, "chn_"+randHex(4))

	body, _ := json.Marshal(map[string]int{"old_position": 1, "new_position": 3})
	rec := do(t, e, http.MethodPut, "/channels/"+c1.ID+"/change_position", body, authCookie(token))

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

	channels, err := storage.ListChannelsByServer(context.Background(), server.ID)
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

// TestChangeChannelPositionRouteForbiddenWithoutPermission garante que um
// usuário sem a permissão manage_channels no servidor é negado com 403.
func TestChangeChannelPositionRouteForbiddenWithoutPermission(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	_, actorToken := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))

	body, _ := json.Marshal(map[string]int{"old_position": 1, "new_position": 1})
	rec := do(t, e, http.MethodPut, "/channels/"+channel.ID+"/change_position", body, authCookie(actorToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação")
}

// TestChangeChannelPositionRouteWithManageChannelsRole garante que um usuário
// com a permissão manage_channels no servidor muda a posição sem ser dono.
func TestChangeChannelPositionRouteWithManageChannelsRole(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	actorID, actorToken := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)
	createChannelFor(t, server.ID, "chn_"+randHex(4))
	c2 := createChannelFor(t, server.ID, "chn_"+randHex(4))
	role := createRoleFor(t, server.ID, models.RolePermissions{ManageChannels: true})
	assignRoleToUser(t, actorID, role.ID)

	body, _ := json.Marshal(map[string]int{"old_position": 2, "new_position": 1})
	rec := do(t, e, http.MethodPut, "/channels/"+c2.ID+"/change_position", body, authCookie(actorToken))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var summary models.ChannelSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if summary.ID != c2.ID || summary.Position != 1 {
		t.Errorf("esperava canal %s na posição 1, obtive %s na posição %d", c2.ID, summary.ID, summary.Position)
	}
}

// TestChangeChannelPositionRouteRoleOnOtherServerForbidden garante que uma
// role com manage_channels em outro servidor não autoriza o servidor do canal.
func TestChangeChannelPositionRouteRoleOnOtherServerForbidden(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	actorID, actorToken := registerAndLogin(t, e)
	serverA := createServerFor(t, ownerID)
	serverB := createServerFor(t, ownerID)
	channel := createChannelFor(t, serverA.ID, "chn_"+randHex(4))
	role := createRoleFor(t, serverB.ID, models.RolePermissions{ManageChannels: true})
	assignRoleToUser(t, actorID, role.ID)

	body, _ := json.Marshal(map[string]int{"old_position": 1, "new_position": 1})
	rec := do(t, e, http.MethodPut, "/channels/"+channel.ID+"/change_position", body, authCookie(actorToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação")
}

// TestChangeChannelPositionRouteInvalidInput garante que posições inválidas
// respondem 400.
func TestChangeChannelPositionRouteInvalidInput(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))

	body, _ := json.Marshal(map[string]int{"old_position": 1, "new_position": 2})
	rec := do(t, e, http.MethodPut, "/channels/"+channel.ID+"/change_position", body, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"old_position e new_position devem ser posições válidas (1 até o número de canais do servidor)")
}

// TestChangeChannelPositionRouteNotFound garante que
// PUT /channels/:channel_id/change_position responde 404 para canal
// inexistente.
func TestChangeChannelPositionRouteNotFound(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]int{"old_position": 1, "new_position": 1})
	rec := do(t, e, http.MethodPut, "/channels/00000000-0000-4000-8000-000000000000/change_position", body, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

// TestChangeChannelPositionRouteConflict garante que old_position divergente
// da posição atual responde 409 channel-position-conflict.
func TestChangeChannelPositionRouteConflict(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	c1 := createChannelFor(t, server.ID, "chn_"+randHex(4))
	createChannelFor(t, server.ID, "chn_"+randHex(4))

	body, _ := json.Marshal(map[string]int{"old_position": 2, "new_position": 1})
	rec := do(t, e, http.MethodPut, "/channels/"+c1.ID+"/change_position", body, authCookie(token))

	assertProblem(t, rec, http.StatusConflict, "channel-position-conflict", "Posição do canal desatualizada",
		"a posição atual do canal não corresponde à old_position informada")
}

// --- rotas de roles (tarefas 6.1 a 6.4) ---

// TestListRolesRouteWithAuth garante que GET /servers/:server_id/roles
// responde as roles do servidor para o usuário autenticado.
func TestListRolesRouteWithAuth(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	roleA := createRoleFor(t, server.ID, models.RolePermissions{ManageChannels: true})
	roleB := createRoleFor(t, server.ID, models.RolePermissions{ManageRoles: true})

	rec := do(t, e, http.MethodGet, "/servers/"+server.ID+"/roles", nil, authCookie(token))

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
		t.Fatal("esperava roles como lista, obtive null")
	}

	byID := make(map[string]models.Role, len(resp.Roles))
	for _, r := range resp.Roles {
		byID[r.ID] = r
	}
	for _, want := range []struct {
		id          string
		permissions models.RolePermissions
	}{
		{roleA.ID, roleA.Permissions},
		{roleB.ID, roleB.Permissions},
	} {
		got, ok := byID[want.id]
		if !ok {
			t.Errorf("role %s não apareceu na listagem", want.id)
			continue
		}
		if got.ServerID != server.ID {
			t.Errorf("esperava server_id %q, obtive %q", server.ID, got.ServerID)
		}
		if got.Permissions != want.permissions {
			t.Errorf("esperava permissions %+v, obtive %+v", want.permissions, got.Permissions)
		}
	}
}

// TestListRolesRouteServerNotFound garante que GET /servers/:server_id/roles
// responde 404 para servidor inexistente.
func TestListRolesRouteServerNotFound(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	rec := do(t, e, http.MethodGet, "/servers/00000000-0000-4000-8000-000000000000/roles", nil, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "servidor não encontrado")
}

// TestCreateRoleRouteOwner garante que o dono do servidor cria uma role via
// POST /servers/:server_id/roles e que a role é persistida.
func TestCreateRoleRouteOwner(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	roleName := "role_" + randHex(8)
	color := "#ff0000"
	permissions := models.RolePermissions{ManageChannels: true}

	body, _ := json.Marshal(map[string]any{
		"name":        roleName,
		"color":       color,
		"permissions": permissions,
	})
	rec := do(t, e, http.MethodPost, "/servers/"+server.ID+"/roles", body, authCookie(token))

	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.Role
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID == "" || resp.ServerID != server.ID || resp.Name != roleName {
		t.Errorf("resposta inesperada: %+v", resp)
	}
	if resp.Color == nil || *resp.Color != color {
		t.Errorf("esperava color %q, obtive %v", color, resp.Color)
	}
	if resp.Permissions != permissions {
		t.Errorf("esperava permissions %+v, obtive %+v", permissions, resp.Permissions)
	}

	stored, err := storage.GetRoleByID(context.Background(), resp.ID)
	if err != nil {
		t.Fatalf("GetRoleByID retornou erro: %v", err)
	}
	if stored.Name != roleName || stored.Permissions != permissions {
		t.Errorf("persistência inesperada: %+v", stored)
	}
}

// TestCreateRoleRouteForbiddenWithoutPermission garante que um usuário sem
// permissão manage_roles no servidor é negado com 403.
func TestCreateRoleRouteForbiddenWithoutPermission(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	_, actorToken := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)

	body, _ := json.Marshal(map[string]string{"name": "role_" + randHex(8)})
	rec := do(t, e, http.MethodPost, "/servers/"+server.ID+"/roles", body, authCookie(actorToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação")
}

// TestCreateRoleRouteWithManageRolesRole garante que um usuário com a
// permissão manage_roles no servidor cria role sem ser dono.
func TestCreateRoleRouteWithManageRolesRole(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	actorID, actorToken := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)
	role := createRoleFor(t, server.ID, models.RolePermissions{ManageRoles: true})
	assignRoleToUser(t, actorID, role.ID)

	body, _ := json.Marshal(map[string]string{"name": "role_" + randHex(8)})
	rec := do(t, e, http.MethodPost, "/servers/"+server.ID+"/roles", body, authCookie(actorToken))

	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// TestCreateRoleRouteRoleOnOtherServerForbidden garante que a permissão
// manage_roles de outro servidor não autoriza a criação neste servidor.
func TestCreateRoleRouteRoleOnOtherServerForbidden(t *testing.T) {
	e := newApp()
	ownerA, _ := registerAndLogin(t, e)
	ownerB, _ := registerAndLogin(t, e)
	actorID, actorToken := registerAndLogin(t, e)
	serverA := createServerFor(t, ownerA)
	serverB := createServerFor(t, ownerB)
	role := createRoleFor(t, serverB.ID, models.RolePermissions{ManageRoles: true})
	assignRoleToUser(t, actorID, role.ID)

	body, _ := json.Marshal(map[string]string{"name": "role_" + randHex(8)})
	rec := do(t, e, http.MethodPost, "/servers/"+serverA.ID+"/roles", body, authCookie(actorToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação")
}

// TestCreateRoleRouteInvalidInput garante que POST /servers/:server_id/roles
// responde 400 para cor inválida ou nome ausente.
func TestCreateRoleRouteInvalidInput(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)

	cases := []map[string]any{
		{"name": "role_" + randHex(8), "color": "vermelho"},
		{"color": "#ff0000"},
	}
	for _, raw := range cases {
		body, _ := json.Marshal(raw)
		rec := do(t, e, http.MethodPost, "/servers/"+server.ID+"/roles", body, authCookie(token))
		assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
			"name é obrigatório e deve ter no máximo 32 caracteres; color deve ser hexadecimal #RRGGBB")
	}
}

// TestCreateRoleRouteServerNotFound garante que POST /servers/:server_id/roles
// responde 404 para servidor inexistente.
func TestCreateRoleRouteServerNotFound(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{"name": "role_" + randHex(8)})
	rec := do(t, e, http.MethodPost, "/servers/00000000-0000-4000-8000-000000000000/roles", body, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "servidor não encontrado")
}

// TestCreateRoleRouteNameTaken garante que POST /servers/:server_id/roles
// responde 409 quando o nome da role já existe no servidor.
func TestCreateRoleRouteNameTaken(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	roleName := "role_" + randHex(8)

	body, _ := json.Marshal(map[string]string{"name": roleName})
	do(t, e, http.MethodPost, "/servers/"+server.ID+"/roles", body, authCookie(token))
	rec := do(t, e, http.MethodPost, "/servers/"+server.ID+"/roles", body, authCookie(token))

	assertProblem(t, rec, http.StatusConflict, "role-name-taken", "Nome de role já existe",
		"o nome informado já está em uso no servidor")
}

// TestUpdateRoleRouteOwner garante que o dono do servidor atualiza a role via
// PUT /roles/:role_id e que as alterações são persistidas.
func TestUpdateRoleRouteOwner(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	role := createRoleFor(t, server.ID, models.RolePermissions{})
	newName := "role_" + randHex(8)
	color := "#00ff00"
	permissions := models.RolePermissions{ManageRoles: true}

	body, _ := json.Marshal(map[string]any{
		"name":        newName,
		"color":       color,
		"permissions": permissions,
	})
	rec := do(t, e, http.MethodPut, "/roles/"+role.ID, body, authCookie(token))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.Role
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID != role.ID || resp.Name != newName || resp.Color == nil || *resp.Color != color {
		t.Errorf("resposta inesperada: %+v", resp)
	}
	if resp.Permissions != permissions {
		t.Errorf("esperava permissions %+v, obtive %+v", permissions, resp.Permissions)
	}

	stored, err := storage.GetRoleByID(context.Background(), role.ID)
	if err != nil {
		t.Fatalf("GetRoleByID retornou erro: %v", err)
	}
	if stored.Name != newName || stored.Permissions != permissions {
		t.Errorf("persistência inesperada: %+v", stored)
	}
}

// TestUpdateRoleRouteForbiddenWithoutPermission garante que um usuário sem
// permissão manage_roles no servidor da role é negado com 403.
func TestUpdateRoleRouteForbiddenWithoutPermission(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	_, actorToken := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)
	role := createRoleFor(t, server.ID, models.RolePermissions{})

	body, _ := json.Marshal(map[string]string{"name": "role_" + randHex(8)})
	rec := do(t, e, http.MethodPut, "/roles/"+role.ID, body, authCookie(actorToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação")
}

// TestUpdateRoleRouteInvalidInput garante que PUT /roles/:role_id responde
// 400 para cor inválida.
func TestUpdateRoleRouteInvalidInput(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	role := createRoleFor(t, server.ID, models.RolePermissions{})

	body, _ := json.Marshal(map[string]any{"name": "role_" + randHex(8), "color": "azul"})
	rec := do(t, e, http.MethodPut, "/roles/"+role.ID, body, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"name é obrigatório e deve ter no máximo 32 caracteres; color deve ser hexadecimal #RRGGBB")
}

// TestUpdateRoleRouteNotFound garante que PUT /roles/:role_id responde 404
// para role inexistente.
func TestUpdateRoleRouteNotFound(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{"name": "role_" + randHex(8)})
	rec := do(t, e, http.MethodPut, "/roles/00000000-0000-4000-8000-000000000000", body, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "role não encontrada")
}

// TestDeleteRoleRouteOwner garante que o dono do servidor exclui a role via
// DELETE /roles/:role_id.
func TestDeleteRoleRouteOwner(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	role := createRoleFor(t, server.ID, models.RolePermissions{})

	rec := do(t, e, http.MethodDelete, "/roles/"+role.ID, nil, authCookie(token))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	if _, err := storage.GetRoleByID(context.Background(), role.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava a role removida do banco, obtive erro %v", err)
	}
}

// TestDeleteRoleRouteNotFound garante que DELETE /roles/:role_id responde
// 404 para role inexistente.
func TestDeleteRoleRouteNotFound(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	rec := do(t, e, http.MethodDelete, "/roles/00000000-0000-4000-8000-000000000000", nil, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "role não encontrada")
}

// --- atribuição de roles a usuários ---

// TestAssignUserRoleRouteOwner garante que o dono do servidor atribui uma
// role ao usuário via POST /users/:user_id/roles.
func TestAssignUserRoleRouteOwner(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	targetID, _ := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)
	role := createRoleFor(t, server.ID, models.RolePermissions{ManageChannels: true})

	body, _ := json.Marshal(map[string]string{"role_id": role.ID})
	rec := do(t, e, http.MethodPost, "/users/"+targetID+"/roles", body, authCookie(ownerToken))

	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.UserRole
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.UserID != targetID || resp.RoleID != role.ID {
		t.Errorf("resposta inesperada: %+v", resp)
	}

	stored, err := storage.GetUserRole(context.Background(), targetID, role.ID)
	if err != nil {
		t.Fatalf("GetUserRole retornou erro: %v", err)
	}
	if stored.UserID != targetID || stored.RoleID != role.ID {
		t.Errorf("persistência inesperada: %+v", stored)
	}
}

// TestAssignUserRoleRouteForbiddenWithoutPermission garante que um usuário
// sem permissão manage_roles no servidor da role é negado com 403.
func TestAssignUserRoleRouteForbiddenWithoutPermission(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	_, actorToken := registerAndLogin(t, e)
	targetID, _ := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)
	role := createRoleFor(t, server.ID, models.RolePermissions{})

	body, _ := json.Marshal(map[string]string{"role_id": role.ID})
	rec := do(t, e, http.MethodPost, "/users/"+targetID+"/roles", body, authCookie(actorToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação")
}

// TestAssignUserRoleRouteMissingRoleID garante que o middleware responde 400
// quando o corpo não permite determinar o servidor alvo da operação.
func TestAssignUserRoleRouteMissingRoleID(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{})
	rec := do(t, e, http.MethodPost, "/users/"+userID+"/roles", body, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"não foi possível determinar o servidor alvo da operação")
}

// TestAssignUserRoleRouteUserNotFound garante que POST /users/:user_id/roles
// responde 404 para usuário inexistente.
func TestAssignUserRoleRouteUserNotFound(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)
	role := createRoleFor(t, server.ID, models.RolePermissions{})

	body, _ := json.Marshal(map[string]string{"role_id": role.ID})
	rec := do(t, e, http.MethodPost, "/users/00000000-0000-4000-8000-000000000000/roles", body, authCookie(ownerToken))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "usuário não encontrado")
}

// TestRemoveUserRoleRouteOwner garante que o dono do servidor remove a role
// do usuário via DELETE /users/:user_id/roles/:role_id.
func TestRemoveUserRoleRouteOwner(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	targetID, _ := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)
	role := createRoleFor(t, server.ID, models.RolePermissions{})
	assignRoleToUser(t, targetID, role.ID)

	rec := do(t, e, http.MethodDelete, "/users/"+targetID+"/roles/"+role.ID, nil, authCookie(ownerToken))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	if _, err := storage.GetUserRole(context.Background(), targetID, role.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava a atribuição removida do banco, obtive erro %v", err)
	}
}

// TestRemoveUserRoleRouteForbiddenWithoutPermission garante que um usuário
// sem permissão manage_roles no servidor da role é negado com 403.
func TestRemoveUserRoleRouteForbiddenWithoutPermission(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	_, actorToken := registerAndLogin(t, e)
	targetID, _ := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)
	role := createRoleFor(t, server.ID, models.RolePermissions{})
	assignRoleToUser(t, targetID, role.ID)

	rec := do(t, e, http.MethodDelete, "/users/"+targetID+"/roles/"+role.ID, nil, authCookie(actorToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação")
}

// TestRemoveUserRoleRouteNotAssigned garante que DELETE
// /users/:user_id/roles/:role_id responde 404 quando o usuário não possui a
// role.
func TestRemoveUserRoleRouteNotAssigned(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	targetID, _ := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)
	role := createRoleFor(t, server.ID, models.RolePermissions{})

	rec := do(t, e, http.MethodDelete, "/users/"+targetID+"/roles/"+role.ID, nil, authCookie(ownerToken))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado",
		"role não atribuída ao usuário")
}

// TestRemoveUserRoleRouteUserNotFound garante que DELETE
// /users/:user_id/roles/:role_id responde 404 para usuário inexistente.
func TestRemoveUserRoleRouteUserNotFound(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)
	role := createRoleFor(t, server.ID, models.RolePermissions{})

	rec := do(t, e, http.MethodDelete, "/users/00000000-0000-4000-8000-000000000000/roles/"+role.ID, nil, authCookie(ownerToken))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "usuário não encontrado")
}

// --- rotas de mensagens (tarefa 7.2) ---

// TestListMessagesRouteWithAuth garante que GET /channels/:channel_id/messages
// responde a listagem (vazia) para o usuário autenticado pelo cookie.
func TestListMessagesRouteWithAuth(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))

	rec := do(t, e, http.MethodGet, "/channels/"+channel.ID+"/messages", nil, authCookie(token))

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

// TestListMessagesRouteSuccess garante que a listagem retorna as mensagens do
// canal em ordem decrescente.
func TestListMessagesRouteSuccess(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))
	first, err := storage.CreateMessage(context.Background(), channel.ID, userID, "primeira", nil)
	if err != nil {
		t.Fatalf("falha ao criar primeira mensagem: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	second, err := storage.CreateMessage(context.Background(), channel.ID, userID, "segunda", nil)
	if err != nil {
		t.Fatalf("falha ao criar segunda mensagem: %v", err)
	}

	rec := do(t, e, http.MethodGet, "/channels/"+channel.ID+"/messages", nil, authCookie(token))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.MessageList
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if len(resp.Messages) != 2 {
		t.Fatalf("esperava 2 mensagens, obtive %d", len(resp.Messages))
	}
	if resp.Messages[0].ID != second.ID || resp.Messages[1].ID != first.ID {
		t.Errorf("ordem inesperada: %+v", resp.Messages)
	}
}

// TestCreateMessageRouteOwner garante que o dono do servidor envia uma
// mensagem via POST /messages (multipart) e que ela é persistida.
func TestCreateMessageRouteOwner(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))

	rec := doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": "olá mundo"}, nil, authCookie(token))

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
	if resp.AuthorID == nil || *resp.AuthorID != userID {
		t.Errorf("esperava author_id %q, obtive %v", userID, resp.AuthorID)
	}
	if resp.Content == nil || *resp.Content != "olá mundo" {
		t.Errorf("esperava content %q, obtive %v", "olá mundo", resp.Content)
	}
	if len(resp.Attachments) != 0 {
		t.Errorf("esperava attachments vazia, obtive %v", resp.Attachments)
	}

	stored, err := storage.GetMessageByID(context.Background(), resp.ID)
	if err != nil {
		t.Fatalf("GetMessageByID retornou erro: %v", err)
	}
	if stored.Content == nil || *stored.Content != "olá mundo" {
		t.Errorf("esperava content %q persistido, obtive %v", "olá mundo", stored.Content)
	}
}

// TestCreateMessageRouteWithAttachment garante que um POST /messages com
// attachment persiste o attachment e o expõe na resposta.
func TestCreateMessageRouteWithAttachment(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))
	// PNG mínimo válido (magic number + bytes adicionais)
	png := append([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, []byte("conteudo-teste")...)

	rec := doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": "com anexo"},
		map[string][][2]string{"attachments": {{"foto.png", string(png)}}}, authCookie(token))

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
	if att.OriginalFileName != "foto.png" {
		t.Errorf("esperava original_file_name %q, obtive %q", "foto.png", att.OriginalFileName)
	}
	if att.MimeType != "image/png" {
		t.Errorf("esperava mime_type %q, obtive %q", "image/png", att.MimeType)
	}
	if att.SizeBytes != int64(len(png)) {
		t.Errorf("esperava size_bytes %d, obtive %d", len(png), att.SizeBytes)
	}

	stored, err := storage.GetAttachmentByID(context.Background(), att.ID)
	if err != nil {
		t.Fatalf("GetAttachmentByID retornou erro: %v", err)
	}
	if stored.OriginalFileName != "foto.png" || stored.MimeType != "image/png" || stored.SizeBytes != int64(len(png)) {
		t.Errorf("persistência inesperada: %+v", stored)
	}
}

// TestCreateMessageRouteForbiddenWithoutPermission garante que um usuário sem
// permissão send_messages em canal fechado é negado com 403.
func TestCreateMessageRouteForbiddenWithoutPermission(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	actorID, actorToken := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))
	role := createRoleFor(t, server.ID, models.RolePermissions{})
	if _, err := storage.UpdateChannelPermissions(context.Background(), channel.ID, role.ID, models.ChannelPermission{}); err != nil {
		t.Fatalf("falha ao definir permissões do canal: %v", err)
	}
	assignRoleToUser(t, actorID, role.ID)

	rec := doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": "x"}, nil, authCookie(actorToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não tem permissão para enviar esta mensagem")
}

// TestCreateMessageRouteContentTooLong garante que content acima de 8192
// caracteres é rejeitado com 400.
func TestCreateMessageRouteContentTooLong(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))

	rec := doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": strings.Repeat("a", 8193)}, nil, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"channel_id é obrigatório; content tem no máximo 8192 caracteres; a mensagem precisa de content ou attachment; nome do attachment inválido")
}

// TestUpdateMessageRouteAuthor garante que o autor edita sua mensagem via
// PUT /messages/:message_id.
func TestUpdateMessageRouteAuthor(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))
	message, err := storage.CreateMessage(context.Background(), channel.ID, userID, "original", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"content": "editada"})
	rec := do(t, e, http.MethodPut, "/messages/"+message.ID, body, authCookie(token))

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

// TestUpdateMessageRouteForbiddenOtherUser garante que um usuário que não é o
// autor é negado com 403.
func TestUpdateMessageRouteForbiddenOtherUser(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	_, actorToken := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))
	message, err := storage.CreateMessage(context.Background(), channel.ID, ownerID, "x", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"content": "hacker"})
	rec := do(t, e, http.MethodPut, "/messages/"+message.ID, body, authCookie(actorToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"somente o autor da mensagem pode editá-la")
}

// TestUpdateMessageRouteNotFound garante que PUT /messages/:message_id
// responde 404 para mensagem inexistente.
func TestUpdateMessageRouteNotFound(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{"content": "x"})
	rec := do(t, e, http.MethodPut, "/messages/00000000-0000-4000-8000-000000000000", body, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "mensagem não encontrada")
}

// TestDeleteMessageRouteAuthor garante que o autor exclui sua mensagem via
// DELETE /messages/:message_id.
func TestDeleteMessageRouteAuthor(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))
	message, err := storage.CreateMessage(context.Background(), channel.ID, userID, "a excluir", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	rec := do(t, e, http.MethodDelete, "/messages/"+message.ID, nil, authCookie(token))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if _, err := storage.GetMessageByID(context.Background(), message.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava mensagem removida do banco, obtive erro %v", err)
	}
}

// TestDeleteMessageRouteForbiddenOtherUser garante que delete_messages não é
// livre em canal aberto: um usuário que não é o autor é negado com 403.
func TestDeleteMessageRouteForbiddenOtherUser(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	_, strangerToken := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))
	message, err := storage.CreateMessage(context.Background(), channel.ID, ownerID, "x", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	rec := do(t, e, http.MethodDelete, "/messages/"+message.ID, nil, authCookie(strangerToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não tem permissão para excluir a mensagem")
}

// TestDeleteMessageRouteWithDeleteMessagesRole garante que um usuário com a
// permissão delete_messages no canal exclui a mensagem.
func TestDeleteMessageRouteWithDeleteMessagesRole(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	actorID, actorToken := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))
	message, err := storage.CreateMessage(context.Background(), channel.ID, ownerID, "x", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	role := createRoleFor(t, server.ID, models.RolePermissions{})
	if _, err := storage.UpdateChannelPermissions(context.Background(), channel.ID, role.ID, models.ChannelPermission{DeleteMessages: true}); err != nil {
		t.Fatalf("falha ao definir permissões do canal: %v", err)
	}
	assignRoleToUser(t, actorID, role.ID)

	rec := do(t, e, http.MethodDelete, "/messages/"+message.ID, nil, authCookie(actorToken))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if _, err := storage.GetMessageByID(context.Background(), message.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava mensagem removida do banco, obtive erro %v", err)
	}
}

// TestDeleteMessageRouteOwnerDeletesOtherMessage garante que o dono do
// servidor exclui uma mensagem de outro usuário.
func TestDeleteMessageRouteOwnerDeletesOtherMessage(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	otherID, _ := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))
	message, err := storage.CreateMessage(context.Background(), channel.ID, otherID, "x", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	rec := do(t, e, http.MethodDelete, "/messages/"+message.ID, nil, authCookie(ownerToken))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// TestDeleteMessageRouteNotFound garante que DELETE /messages/:message_id
// responde 404 para mensagem inexistente.
func TestDeleteMessageRouteNotFound(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	rec := do(t, e, http.MethodDelete, "/messages/00000000-0000-4000-8000-000000000000", nil, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "mensagem não encontrada")
}

// --- rotas de attachments (tarefa 7.3) ---

// newMessageWithAttachmentRoute cria uma mensagem com um attachment PNG via
// POST /messages (multipart) e retorna o registro completo do attachment.
func newMessageWithAttachmentRoute(t *testing.T, e *echo.Echo, channelID, token string) models.Attachments {
	t.Helper()
	png := append([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, []byte("conteudo-teste")...)

	rec := doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channelID, "content": "com anexo"},
		map[string][][2]string{"attachments": {{"foto.png", string(png)}}}, authCookie(token))
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var created models.MessageWithAttachment
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if len(created.Attachments) != 1 {
		t.Fatalf("esperava 1 attachment, obtive %d", len(created.Attachments))
	}
	stored, err := storage.GetAttachmentByID(context.Background(), created.Attachments[0].ID)
	if err != nil {
		t.Fatalf("GetAttachmentByID retornou erro: %v", err)
	}
	return stored
}

// closeChannelRoute define uma role com read_channel no canal, fechando a
// leitura para quem não tiver a role (o dono do servidor continua lendo).
func closeChannelRoute(t *testing.T, e *echo.Echo, serverID, channelID, token string) models.Role {
	t.Helper()
	role := createRoleFor(t, serverID, models.RolePermissions{})
	body, _ := json.Marshal(map[string]models.ChannelPermission{
		"permissions": {ReadChannel: true},
	})
	rec := do(t, e, http.MethodPut, "/channels/"+channelID+"/permissions/"+role.ID, body, authCookie(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 ao fechar o canal, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	return role
}

// TestDownloadAttachmentRouteOwner garante que o dono do servidor baixa o
// arquivo via GET /attachments/:file_id recebendo o conteúdo original com o
// MIME type detectado, o Content-Disposition do nome original e o
// Content-Length correto.
func TestDownloadAttachmentRouteOwner(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))
	attachment := newMessageWithAttachmentRoute(t, e, channel.ID, token)

	rec := do(t, e, http.MethodGet, "/attachments/"+attachment.ID, nil, authCookie(token))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get(echo.HeaderContentType); ct != "image/png" {
		t.Errorf("esperava content-type image/png, obtive %q", ct)
	}
	if cd := rec.Header().Get(echo.HeaderContentDisposition); cd != `attachment; filename="foto.png"` {
		t.Errorf("esperava content-disposition %q, obtive %q", `attachment; filename="foto.png"`, cd)
	}
	if cl := rec.Header().Get(echo.HeaderContentLength); cl != fmt.Sprint(attachment.SizeBytes) {
		t.Errorf("esperava content-length %d, obtive %q", attachment.SizeBytes, cl)
	}
	blob, err := os.ReadFile(attachment.FilePath)
	if err != nil {
		t.Fatalf("falha ao ler o blob de apoio: %v", err)
	}
	if !bytes.Equal(rec.Body.Bytes(), blob) {
		t.Errorf("corpo do download não corresponde ao blob em disco")
	}
}

// TestDownloadAttachmentRouteReaderRole garante que um usuário com a role
// read_channel em canal fechado baixa o arquivo com sucesso.
func TestDownloadAttachmentRouteReaderRole(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	readerID, readerToken := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))
	role := closeChannelRoute(t, e, server.ID, channel.ID, ownerToken)
	assignRoleToUser(t, readerID, role.ID)
	attachment := newMessageWithAttachmentRoute(t, e, channel.ID, ownerToken)

	rec := do(t, e, http.MethodGet, "/attachments/"+attachment.ID, nil, authCookie(readerToken))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	blob, err := os.ReadFile(attachment.FilePath)
	if err != nil {
		t.Fatalf("falha ao ler o blob de apoio: %v", err)
	}
	if !bytes.Equal(rec.Body.Bytes(), blob) {
		t.Errorf("corpo do download não corresponde ao blob em disco")
	}
}

// TestDownloadAttachmentRouteForbiddenWithoutPermission garante que um
// usuário sem read_channel em canal fechado é negado com 403 e que o dono do
// servidor continua baixando.
func TestDownloadAttachmentRouteForbiddenWithoutPermission(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	_, strangerToken := registerAndLogin(t, e)
	server := createServerFor(t, ownerID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))
	closeChannelRoute(t, e, server.ID, channel.ID, ownerToken)
	attachment := newMessageWithAttachmentRoute(t, e, channel.ID, ownerToken)

	rec := do(t, e, http.MethodGet, "/attachments/"+attachment.ID, nil, authCookie(strangerToken))
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não tem permissão para baixar o arquivo")

	rec = do(t, e, http.MethodGet, "/attachments/"+attachment.ID, nil, authCookie(ownerToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 para o dono, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// TestDownloadAttachmentRouteNotFound garante que GET /attachments/:file_id
// responde 404 para arquivo inexistente.
func TestDownloadAttachmentRouteNotFound(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	rec := do(t, e, http.MethodGet, "/attachments/00000000-0000-4000-8000-000000000000", nil, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "arquivo não encontrado")
}

// TestDownloadAttachmentRouteMissingBlob garante que um attachment com
// registro no banco mas blob ausente em disco responde 404 (e não 500).
func TestDownloadAttachmentRouteMissingBlob(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))
	attachment := newMessageWithAttachmentRoute(t, e, channel.ID, token)

	if err := os.Remove(attachment.FilePath); err != nil {
		t.Fatalf("falha ao remover o blob de apoio: %v", err)
	}

	rec := do(t, e, http.MethodGet, "/attachments/"+attachment.ID, nil, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "arquivo não encontrado")
}

// --- rotas de emojis (tarefa 7.4) ---

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

// newEmojiRoute cria um emoji via POST /emojis usando o cookie do usuário e
// retorna o registro criado.
func newEmojiRoute(t *testing.T, e *echo.Echo, serverID, token string) models.Emoji {
	t.Helper()

	body, _ := json.Marshal(map[string]string{
		"server_id":  serverID,
		"name":       "emoji_" + randHex(8),
		"format":     "PNG",
		"image_blob": base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100)),
	})
	rec := do(t, e, http.MethodPost, "/emojis", body, authCookie(token))

	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201 ao criar emoji, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var emoji models.Emoji
	if err := json.Unmarshal(rec.Body.Bytes(), &emoji); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	return emoji
}

func TestListEmojisRoute(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	emoji := newEmojiRoute(t, e, server.ID, token)

	rec := do(t, e, http.MethodGet, "/emojis?server_id="+server.ID, nil, authCookie(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.EmojiList
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if len(resp.Emojis) != 1 || resp.Emojis[0].ID != emoji.ID {
		t.Fatalf("esperava apenas o emoji criado, obtive %+v", resp.Emojis)
	}
	if resp.HasMore {
		t.Error("esperava has_more false, obtive true")
	}
}

func TestCreateEmojiRouteManageServerRole(t *testing.T) {
	e := newApp()
	userID, _ := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	modUserID, modToken := registerAndLogin(t, e)

	role := createRoleFor(t, server.ID, models.RolePermissions{ManageServer: true})
	assignRoleToUser(t, modUserID, role.ID)

	emoji := newEmojiRoute(t, e, server.ID, modToken)
	if emoji.ServerID != server.ID {
		t.Errorf("esperava server_id %s, obtive %s", server.ID, emoji.ServerID)
	}
}

func TestCreateEmojiRouteForbiddenWithoutPermission(t *testing.T) {
	e := newApp()
	userID, _ := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	_, strangerToken := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{
		"server_id":  server.ID,
		"name":       "emoji_" + randHex(8),
		"format":     "PNG",
		"image_blob": base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100)),
	})
	rec := do(t, e, http.MethodPost, "/emojis", body, authCookie(strangerToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação")
}

func TestDeleteEmojiRouteAuthor(t *testing.T) {
	e := newApp()
	userID, _ := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	authorID, authorToken := registerAndLogin(t, e)

	// emoji criado pelo autor (não dono do servidor)
	emoji, err := storage.CreateEmoji(context.Background(), server.ID, "emoji_"+randHex(8), "PNG", []byte{1}, &authorID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}

	rec := do(t, e, http.MethodDelete, "/emojis/"+emoji.ID, nil, authCookie(authorToken))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if _, err := storage.GetEmojiByID(context.Background(), emoji.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava emoji removido do banco, obtive erro %v", err)
	}
}

func TestDeleteEmojiRouteManageServerRole(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	modUserID, modToken := registerAndLogin(t, e)

	role := createRoleFor(t, server.ID, models.RolePermissions{ManageServer: true})
	assignRoleToUser(t, modUserID, role.ID)
	emoji := newEmojiRoute(t, e, server.ID, token)

	rec := do(t, e, http.MethodDelete, "/emojis/"+emoji.ID, nil, authCookie(modToken))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if _, err := storage.GetEmojiByID(context.Background(), emoji.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava emoji removido do banco, obtive erro %v", err)
	}
}

func TestDeleteEmojiRouteForbiddenWithoutPermission(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	_, strangerToken := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	emoji := newEmojiRoute(t, e, server.ID, token)

	rec := do(t, e, http.MethodDelete, "/emojis/"+emoji.ID, nil, authCookie(strangerToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não pode excluir este emoji")
}

func TestDeleteEmojiRouteNotFound(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	rec := do(t, e, http.MethodDelete, "/emojis/00000000-0000-4000-8000-000000000000", nil, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "emoji não encontrado")
}

// --- rotas de busca (tarefa 7.6) ---

// TestSearchRouteWithAuth garante que POST /search responde com os resultados
// da busca para o usuário autenticado pelo cookie.
func TestSearchRouteWithAuth(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	server := createServerFor(t, userID)
	channel := createChannelFor(t, server.ID, "chn_"+randHex(4))
	unique := "w" + randHex(8)
	msg, err := storage.CreateMessage(context.Background(), channel.ID, userID, "mensagem "+unique, nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"text": unique})
	rec := do(t, e, http.MethodPost, "/search", body, authCookie(token))

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
	if r.AuthorID == nil || *r.AuthorID != userID {
		t.Errorf("esperava author_id %q, obtive %v", userID, r.AuthorID)
	}
	if r.ChannelName != channel.Name {
		t.Errorf("esperava channel_name %q, obtive %q", channel.Name, r.ChannelName)
	}
	if r.Score == nil {
		t.Error("esperava score preenchido para busca com texto")
	}
	if resp.HasMore {
		t.Error("esperava has_more false, obtive true")
	}
}

// TestSearchRouteInvalidBody garante que um corpo JSON inválido responde 400.
func TestSearchRouteInvalidBody(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	rec := do(t, e, http.MethodPost, "/search", []byte("{invalido"), authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

// TestSearchRouteNoFilter garante que uma busca sem nenhum filtro responde 400.
func TestSearchRouteNoFilter(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{})
	rec := do(t, e, http.MethodPost, "/search", body, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"pelo menos 1 filtro é obrigatório (text, author, date_start, date_end ou contains_attachment); "+
			"order deve ser asc ou desc; date_start e date_end devem estar no formato YYYY-MM-DD com date_start <= date_end; "+
			"since e last_id devem ser informados juntos")
}

// TestSearchRouteInvalidSince garante que um parâmetro since malformado responde 400.
func TestSearchRouteInvalidSince(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{"text": "qualquer"})
	rec := do(t, e, http.MethodPost, "/search?since=nao-e-data", body, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "since deve ser um timestamp ISO 8601")
}
