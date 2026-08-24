package middleware

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/labstack/echo/v4"
)

// migrationsDir é o caminho relativo ao diretório deste pacote (backend/internal/middleware/test_middleware).
const migrationsDir = "../../../migrations"

// defaultDatabaseURL corresponde aos padrões do infra/docker-compose.yml.
const defaultDatabaseURL = "postgres://papo:papo123@localhost:5432/papo"

// nonexistentID é um UUID que não existe em nenhuma tabela dos testes.
const nonexistentID = "00000000-0000-0000-0000-000000000000"

func TestMain(m *testing.M) {
	os.Exit(runMiddlewareTests(m))
}

// runMiddlewareTests prepara um banco temporário com as migrations do projeto,
// inicializa o storage contra ele, executa os testes e remove o banco ao final.
func runMiddlewareTests(m *testing.M) int {
	baseURL := testDatabaseURL()

	baseDB, err := sql.Open("pgx", baseURL)
	if err != nil {
		fmt.Printf("testes de middleware ignorados: falha ao abrir conexão: %v\n", err)
		return 0
	}
	defer baseDB.Close()

	if err := ping(baseDB); err != nil {
		fmt.Printf("testes de middleware ignorados: não foi possível conectar ao PostgreSQL (%v). Inicie o PostgreSQL (infra/docker-compose.yml) ou defina TEST_DATABASE_URL/DATABASE_URL.\n", err)
		return 0
	}

	removeOldTempDatabases(baseDB)

	tempDBName, err := createTempDatabase(baseDB)
	if err != nil {
		fmt.Printf("testes de middleware ignorados: falha ao criar banco temporário: %v\n", err)
		return 0
	}
	defer dropTempDatabase(baseDB, tempDBName)

	tempURL, err := withDatabase(baseURL, tempDBName)
	if err != nil {
		fmt.Printf("testes de middleware ignorados: %v\n", err)
		return 0
	}

	tempDB, err := sql.Open("pgx", tempURL)
	if err != nil {
		fmt.Printf("testes de middleware ignorados: %v\n", err)
		return 0
	}
	defer tempDB.Close()

	if err := ping(tempDB); err != nil {
		fmt.Printf("testes de middleware ignorados: falha ao conectar no banco temporário: %v\n", err)
	}

	if err := applyMigrations(tempDB); err != nil {
		fmt.Printf("testes de middleware FALHARAM na preparação: %v\n", err)
		return 1
	}

	if err := storage.InitDB(tempURL); err != nil {
		fmt.Printf("testes de middleware FALHARAM na preparação: %v\n", err)
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

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// newContext monta um echo.Context a partir de uma requisição HTTP de teste.
func newContext(t *testing.T, method, path, ip string) echo.Context {
	t.Helper()

	req := httptest.NewRequest(method, path, nil)
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

// doLimitedRequest passa uma requisição pelo middleware de rate limit e por um
// handler que responde 200. O IP é o identificador usado pelo
func doLimitedRequest(t *testing.T, mw echo.MiddlewareFunc, path, ip string) *httptest.ResponseRecorder {
	t.Helper()

	handler := mw(func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	c := newContext(t, http.MethodGet, path, ip)
	if err := handler(c); err != nil {
		t.Fatalf("handler retornou erro: %v", err)
	}
	return recorder(c)
}

// problem é o corpo de erro RFC 7807 retornado pelo
type problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance"`
}

// assertProblem valida o status, o content-type e o corpo RFC 7807 da resposta.
func assertProblem(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantSlug, wantTitle, wantDetail, wantInstance string) {
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

	wantType := utils.ProblemTypeURL(config.LoadConfig().BaseURL, wantSlug)
	if p.Type != wantType {
		t.Errorf("esperava type %q, obtive %q", wantType, p.Type)
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
	if wantInstance != "" && p.Instance != wantInstance {
		t.Errorf("esperava instance %q, obtive %q", wantInstance, p.Instance)
	}
}

// --- RateLimit ---

func TestRateLimitAllowsWithinBurst(t *testing.T) {
	mw := RateLimit(1, 3)
	ip := "10.0.0.1"

	for i := 1; i <= 3; i++ {
		rec := doLimitedRequest(t, mw, "/auth/login", ip)
		if rec.Code != http.StatusOK {
			t.Fatalf("requisição %d: esperava status 200, obtive %d (corpo: %s)", i, rec.Code, rec.Body.String())
		}
	}
}

func TestRateLimitDeniesWhenBurstExceeded(t *testing.T) {
	mw := RateLimit(1, 2)
	ip := "10.0.0.1"

	for i := 1; i <= 2; i++ {
		rec := doLimitedRequest(t, mw, "/auth/login", ip)
		if rec.Code != http.StatusOK {
			t.Fatalf("requisição %d: esperava status 200, obtive %d (corpo: %s)", i, rec.Code, rec.Body.String())
		}
	}

	rec := doLimitedRequest(t, mw, "/auth/login", ip)
	assertProblem(t, rec, http.StatusTooManyRequests, "rate-limit",
		"Limite de requisições excedido", "muitas requisições, tente novamente mais tarde", "/auth/login")
}

func TestRateLimitIsolatedPerIP(t *testing.T) {
	mw := RateLimit(1, 1)
	ipA := "10.0.0.1"
	ipB := "10.0.0.2"

	// o IP A consome o único token do burst e é bloqueado na sequência
	if rec := doLimitedRequest(t, mw, "/auth/login", ipA); rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 para o IP A, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	assertProblem(t, doLimitedRequest(t, mw, "/auth/login", ipA), http.StatusTooManyRequests,
		"rate-limit", "Limite de requisições excedido", "muitas requisições, tente novamente mais tarde", "/auth/login")

	// o IP B tem bucket próprio e continua liberado
	if rec := doLimitedRequest(t, mw, "/auth/login", ipB); rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 para o IP B, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

func TestRateLimitRefillsOverTime(t *testing.T) {
	// 100 req/s = 1 token a cada 10ms, com burst de 1
	mw := RateLimit(100, 1)
	ip := "10.0.0.1"

	if rec := doLimitedRequest(t, mw, "/auth/login", ip); rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 na primeira requisição, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	assertProblem(t, doLimitedRequest(t, mw, "/auth/login", ip), http.StatusTooManyRequests,
		"rate-limit", "Limite de requisições excedido", "muitas requisições, tente novamente mais tarde", "/auth/login")

	// após o refill do token, o IP volta a ser liberado
	time.Sleep(100 * time.Millisecond)
	if rec := doLimitedRequest(t, mw, "/auth/login", ip); rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 após o refill, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// --- Permissions ---

// newUser cria um usuário de apoio e retorna o ID.
func newUser(t *testing.T) string {
	t.Helper()
	user, _, err := storage.CreateUser(context.Background(), "user_"+randHex(8), "hash_"+randHex(8), "123.123.123.123")
	if err != nil {
		t.Fatalf("falha ao criar usuário de apoio: %v", err)
	}
	return user.ID
}

// newServer cria um servidor de apoio; ownerID pode ser nil.
func newServer(t *testing.T, ownerID *string) models.Server {
	t.Helper()
	server, err := storage.CreateServer(context.Background(), "server_"+randHex(8), ownerID)
	if err != nil {
		t.Fatalf("falha ao criar servidor de apoio: %v", err)
	}
	return server
}

// newRole cria uma role de apoio no servidor informado.
func newRole(t *testing.T, serverID string, permissions models.RolePermissions) models.Role {
	t.Helper()
	role, err := storage.CreateRole(context.Background(), serverID, "role_"+randHex(8), nil, permissions)
	if err != nil {
		t.Fatalf("falha ao criar role de apoio: %v", err)
	}
	return role
}

// assignRole atribui a role ao usuário de apoio.
func assignRole(t *testing.T, userID, roleID string) {
	t.Helper()
	if _, err := storage.AssignUserRole(context.Background(), userID, roleID); err != nil {
		t.Fatalf("falha ao atribuir role ao usuário: %v", err)
	}
}

// newChannel cria um canal de apoio no servidor informado.
func newChannel(t *testing.T, serverID string) models.Channel {
	t.Helper()
	channel, err := storage.CreateChannel(context.Background(), serverID, "channel_"+randHex(8), "text")
	if err != nil {
		t.Fatalf("falha ao criar canal de apoio: %v", err)
	}
	return channel
}

// newPermissionContext monta um echo.Context simulando uma rota já resolvida,
// com parâmetros de rota, corpo JSON e usuário autenticado (se userID != "").
func newPermissionContext(t *testing.T, method, path string, body []byte, paramNames, paramValues []string, userID string) echo.Context {
	t.Helper()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}

	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if len(paramNames) > 0 {
		c.SetParamNames(paramNames...)
		c.SetParamValues(paramValues...)
	}
	if userID != "" {
		c.Set(UserIDContextKey, userID)
	}
	return c
}

// doPermissionRequest passa o contexto pelo middleware e por um handler que responde 200.
func doPermissionRequest(t *testing.T, mw echo.MiddlewareFunc, c echo.Context) *httptest.ResponseRecorder {
	t.Helper()

	handler := mw(func(ctx echo.Context) error {
		return ctx.String(http.StatusOK, "ok")
	})
	if err := handler(c); err != nil {
		t.Fatalf("handler retornou erro: %v", err)
	}
	return recorder(c)
}

func TestPermissionDeniesWithoutAuthentication(t *testing.T) {
	c := newPermissionContext(t, http.MethodPut, "/servers/"+nonexistentID, nil,
		[]string{"server_id"}, []string{nonexistentID}, "")

	rec := doPermissionRequest(t, RequireManageServer(), c)
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized",
		"Token inválido ou expirado", "token de autenticação ausente, inválido ou expirado", "")
}

func TestPermissionAllowsOwnerWithoutRoles(t *testing.T) {
	ownerID := newUser(t)
	server := newServer(t, &ownerID)

	c := newPermissionContext(t, http.MethodPut, "/servers/"+server.ID, nil,
		[]string{"server_id"}, []string{server.ID}, ownerID)

	if rec := doPermissionRequest(t, RequireManageServer(), c); rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 para o dono, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

func TestPermissionAllowsRoleWithPermission(t *testing.T) {
	ownerID := newUser(t)
	userID := newUser(t)
	server := newServer(t, &ownerID)
	role := newRole(t, server.ID, models.RolePermissions{ManageServer: true})
	assignRole(t, userID, role.ID)

	c := newPermissionContext(t, http.MethodPut, "/servers/"+server.ID, nil,
		[]string{"server_id"}, []string{server.ID}, userID)

	if rec := doPermissionRequest(t, RequireManageServer(), c); rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 com a permissão, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

func TestPermissionDeniesRoleWithoutPermission(t *testing.T) {
	ownerID := newUser(t)
	userID := newUser(t)
	server := newServer(t, &ownerID)
	role := newRole(t, server.ID, models.RolePermissions{ManageChannels: true})
	assignRole(t, userID, role.ID)

	c := newPermissionContext(t, http.MethodPut, "/servers/"+server.ID, nil,
		[]string{"server_id"}, []string{server.ID}, userID)

	rec := doPermissionRequest(t, RequireManageServer(), c)
	assertProblem(t, rec, http.StatusForbidden, "forbidden",
		"Acesso negado", "usuário não possui a permissão necessária para esta operação", "")
}

func TestPermissionDeniesUserWithoutRoles(t *testing.T) {
	ownerID := newUser(t)
	userID := newUser(t)
	server := newServer(t, &ownerID)

	c := newPermissionContext(t, http.MethodPut, "/servers/"+server.ID, nil,
		[]string{"server_id"}, []string{server.ID}, userID)

	rec := doPermissionRequest(t, RequireManageServer(), c)
	assertProblem(t, rec, http.StatusForbidden, "forbidden",
		"Acesso negado", "usuário não possui a permissão necessária para esta operação", "")
}

func TestPermissionIgnoresRolesFromOtherServers(t *testing.T) {
	ownerA := newUser(t)
	ownerB := newUser(t)
	userID := newUser(t)
	serverA := newServer(t, &ownerA)
	serverB := newServer(t, &ownerB)
	role := newRole(t, serverB.ID, models.RolePermissions{ManageServer: true})
	assignRole(t, userID, role.ID)

	// a role com permissão pertence ao servidor B; o alvo é o servidor A
	c := newPermissionContext(t, http.MethodPut, "/servers/"+serverA.ID, nil,
		[]string{"server_id"}, []string{serverA.ID}, userID)

	rec := doPermissionRequest(t, RequireManageServer(), c)
	assertProblem(t, rec, http.StatusForbidden, "forbidden",
		"Acesso negado", "usuário não possui a permissão necessária para esta operação", "")
}

func TestPermissionServerNotFound(t *testing.T) {
	userID := newUser(t)

	c := newPermissionContext(t, http.MethodPut, "/servers/"+nonexistentID, nil,
		[]string{"server_id"}, []string{nonexistentID}, userID)

	rec := doPermissionRequest(t, RequireManageServer(), c)
	assertProblem(t, rec, http.StatusNotFound, "not-found",
		"Recurso não encontrado", "servidor não encontrado", "")
}

// --- RequireServerOwnerOrManageServer ---

func TestServerOwnerOrManageServerDeniesWithoutAuthentication(t *testing.T) {
	c := newPermissionContext(t, http.MethodPut, "/users/"+nonexistentID+"/ban", nil,
		[]string{"user_id"}, []string{nonexistentID}, "")

	rec := doPermissionRequest(t, RequireServerOwnerOrManageServer(), c)
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized",
		"Token inválido ou expirado", "token de autenticação ausente, inválido ou expirado", "")
}

func TestServerOwnerOrManageServerAllowsOwnerWithoutRoles(t *testing.T) {
	ownerID := newUser(t)
	newServer(t, &ownerID)

	c := newPermissionContext(t, http.MethodPut, "/users/"+nonexistentID+"/ban", nil,
		[]string{"user_id"}, []string{nonexistentID}, ownerID)

	if rec := doPermissionRequest(t, RequireServerOwnerOrManageServer(), c); rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 para o dono, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

func TestServerOwnerOrManageServerAllowsRoleWithPermission(t *testing.T) {
	ownerID := newUser(t)
	userID := newUser(t)
	server := newServer(t, &ownerID)
	role := newRole(t, server.ID, models.RolePermissions{ManageServer: true})
	assignRole(t, userID, role.ID)

	c := newPermissionContext(t, http.MethodPut, "/users/"+nonexistentID+"/ban", nil,
		[]string{"user_id"}, []string{nonexistentID}, userID)

	if rec := doPermissionRequest(t, RequireServerOwnerOrManageServer(), c); rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 com a permissão, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

func TestServerOwnerOrManageServerDeniesRoleWithoutPermission(t *testing.T) {
	ownerID := newUser(t)
	userID := newUser(t)
	server := newServer(t, &ownerID)
	role := newRole(t, server.ID, models.RolePermissions{ManageChannels: true})
	assignRole(t, userID, role.ID)

	c := newPermissionContext(t, http.MethodPut, "/users/"+nonexistentID+"/ban", nil,
		[]string{"user_id"}, []string{nonexistentID}, userID)

	rec := doPermissionRequest(t, RequireServerOwnerOrManageServer(), c)
	assertProblem(t, rec, http.StatusForbidden, "forbidden",
		"Acesso negado", "usuário não possui a permissão necessária para esta operação", "")
}

func TestServerOwnerOrManageServerDeniesUserWithoutServersOrRoles(t *testing.T) {
	userID := newUser(t)

	c := newPermissionContext(t, http.MethodPut, "/users/"+nonexistentID+"/ban", nil,
		[]string{"user_id"}, []string{nonexistentID}, userID)

	rec := doPermissionRequest(t, RequireServerOwnerOrManageServer(), c)
	assertProblem(t, rec, http.StatusForbidden, "forbidden",
		"Acesso negado", "usuário não possui a permissão necessária para esta operação", "")
}

func TestServerOwnerOrManageServerAllowsRoleFromAnyServer(t *testing.T) {
	ownerA := newUser(t)
	ownerB := newUser(t)
	userID := newUser(t)
	newServer(t, &ownerA)
	serverB := newServer(t, &ownerB)
	role := newRole(t, serverB.ID, models.RolePermissions{ManageServer: true})
	assignRole(t, userID, role.ID)

	// operação global (sem servidor alvo): a role vale independente do
	// servidor em que foi atribuída
	c := newPermissionContext(t, http.MethodPut, "/users/"+nonexistentID+"/ban", nil,
		[]string{"user_id"}, []string{nonexistentID}, userID)

	if rec := doPermissionRequest(t, RequireServerOwnerOrManageServer(), c); rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 via role de qualquer servidor, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// --- RequireSelfOrServerOwner ---

func TestSelfOrServerOwnerDeniesWithoutAuthentication(t *testing.T) {
	c := newPermissionContext(t, http.MethodPost, "/users/"+nonexistentID+"/reset", nil,
		[]string{"user_id"}, []string{nonexistentID}, "")

	rec := doPermissionRequest(t, RequireSelfOrServerOwner(), c)
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized",
		"Token inválido ou expirado", "token de autenticação ausente, inválido ou expirado", "")
}

// O usuário pode agir sobre si mesmo mesmo sem ser dono de nenhum servidor.
func TestSelfOrServerOwnerAllowsSelfWithoutServer(t *testing.T) {
	userID := newUser(t)

	c := newPermissionContext(t, http.MethodPost, "/users/"+userID+"/reset", nil,
		[]string{"user_id"}, []string{userID}, userID)

	if rec := doPermissionRequest(t, RequireSelfOrServerOwner(), c); rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 para o próprio usuário, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

func TestSelfOrServerOwnerAllowsOwnerWithoutRoles(t *testing.T) {
	ownerID := newUser(t)
	newServer(t, &ownerID)
	targetID := newUser(t)

	c := newPermissionContext(t, http.MethodPost, "/users/"+targetID+"/reset", nil,
		[]string{"user_id"}, []string{targetID}, ownerID)

	if rec := doPermissionRequest(t, RequireSelfOrServerOwner(), c); rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 para o dono, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// Diferente de RequireServerOwnerOrManageServer, a role manage_server NÃO
// autoriza: apenas o próprio usuário ou o dono de um servidor.
func TestSelfOrServerOwnerDeniesRoleWithoutOwnership(t *testing.T) {
	ownerID := newUser(t)
	userID := newUser(t)
	server := newServer(t, &ownerID)
	role := newRole(t, server.ID, models.RolePermissions{ManageServer: true})
	assignRole(t, userID, role.ID)
	targetID := newUser(t)

	c := newPermissionContext(t, http.MethodPost, "/users/"+targetID+"/reset", nil,
		[]string{"user_id"}, []string{targetID}, userID)

	rec := doPermissionRequest(t, RequireSelfOrServerOwner(), c)
	assertProblem(t, rec, http.StatusForbidden, "forbidden",
		"Acesso negado", "usuário não possui a permissão necessária para esta operação", "")
}

func TestSelfOrServerOwnerDeniesUserWithoutServers(t *testing.T) {
	userID := newUser(t)
	targetID := newUser(t)

	c := newPermissionContext(t, http.MethodPost, "/users/"+targetID+"/reset", nil,
		[]string{"user_id"}, []string{targetID}, userID)

	rec := doPermissionRequest(t, RequireSelfOrServerOwner(), c)
	assertProblem(t, rec, http.StatusForbidden, "forbidden",
		"Acesso negado", "usuário não possui a permissão necessária para esta operação", "")
}

func TestPermissionResolvesServerByChannel(t *testing.T) {
	ownerID := newUser(t)
	userID := newUser(t)
	server := newServer(t, &ownerID)
	channel := newChannel(t, server.ID)
	role := newRole(t, server.ID, models.RolePermissions{ManageChannels: true})
	assignRole(t, userID, role.ID)

	c := newPermissionContext(t, http.MethodPut, "/channels/"+channel.ID, nil,
		[]string{"channel_id"}, []string{channel.ID}, userID)

	if rec := doPermissionRequest(t, RequireManageChannels(), c); rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 via canal, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

func TestPermissionChannelNotFound(t *testing.T) {
	userID := newUser(t)

	c := newPermissionContext(t, http.MethodPut, "/channels/"+nonexistentID, nil,
		[]string{"channel_id"}, []string{nonexistentID}, userID)

	rec := doPermissionRequest(t, RequireManageChannels(), c)
	assertProblem(t, rec, http.StatusNotFound, "not-found",
		"Recurso não encontrado", "canal não encontrado", "")
}

func TestPermissionResolvesServerByRole(t *testing.T) {
	ownerID := newUser(t)
	userID := newUser(t)
	server := newServer(t, &ownerID)
	targetRole := newRole(t, server.ID, models.RolePermissions{})
	role := newRole(t, server.ID, models.RolePermissions{ManageRoles: true})
	assignRole(t, userID, role.ID)

	c := newPermissionContext(t, http.MethodDelete, "/roles/"+targetRole.ID, nil,
		[]string{"role_id"}, []string{targetRole.ID}, userID)

	if rec := doPermissionRequest(t, RequireManageRoles(), c); rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 via role, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

func TestPermissionRoleNotFound(t *testing.T) {
	userID := newUser(t)

	c := newPermissionContext(t, http.MethodDelete, "/roles/"+nonexistentID, nil,
		[]string{"role_id"}, []string{nonexistentID}, userID)

	rec := doPermissionRequest(t, RequireManageRoles(), c)
	assertProblem(t, rec, http.StatusNotFound, "not-found",
		"Recurso não encontrado", "role não encontrada", "")
}

func TestPermissionResolvesServerFromBody(t *testing.T) {
	ownerID := newUser(t)
	userID := newUser(t)
	server := newServer(t, &ownerID)
	role := newRole(t, server.ID, models.RolePermissions{ManageChannels: true})
	assignRole(t, userID, role.ID)

	body, _ := json.Marshal(map[string]string{"server_id": server.ID, "name": "geral"})
	c := newPermissionContext(t, http.MethodPost, "/channels", body, nil, nil, userID)

	if rec := doPermissionRequest(t, RequireManageChannels(), c); rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 via corpo, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

func TestPermissionResolvesServerFromBodyRoleID(t *testing.T) {
	ownerID := newUser(t)
	userID := newUser(t)
	server := newServer(t, &ownerID)
	targetRole := newRole(t, server.ID, models.RolePermissions{})
	role := newRole(t, server.ID, models.RolePermissions{ManageRoles: true})
	assignRole(t, userID, role.ID)

	body, _ := json.Marshal(map[string]string{"role_id": targetRole.ID})
	c := newPermissionContext(t, http.MethodPost, "/users/"+userID+"/roles", body, nil, nil, userID)

	if rec := doPermissionRequest(t, RequireManageRoles(), c); rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 via role_id no corpo, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

func TestPermissionServerIDParamTakesPrecedenceOverBody(t *testing.T) {
	ownerA := newUser(t)
	ownerB := newUser(t)
	userID := newUser(t)
	serverA := newServer(t, &ownerA)
	serverB := newServer(t, &ownerB)
	role := newRole(t, serverB.ID, models.RolePermissions{ManageChannels: true})
	assignRole(t, userID, role.ID)

	// o corpo aponta para o servidor B (permitido), mas o parâmetro aponta para o A
	body, _ := json.Marshal(map[string]string{"server_id": serverB.ID, "name": "geral"})
	c := newPermissionContext(t, http.MethodPost, "/channels", body,
		[]string{"server_id"}, []string{serverA.ID}, userID)

	rec := doPermissionRequest(t, RequireManageChannels(), c)
	assertProblem(t, rec, http.StatusForbidden, "forbidden",
		"Acesso negado", "usuário não possui a permissão necessária para esta operação", "")
}

func TestPermissionBodyWithoutServerOrRole(t *testing.T) {
	userID := newUser(t)

	body, _ := json.Marshal(map[string]string{"name": "geral"})
	c := newPermissionContext(t, http.MethodPost, "/channels", body, nil, nil, userID)

	rec := doPermissionRequest(t, RequireManageChannels(), c)
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param",
		"Parâmetro inválido", "não foi possível determinar o servidor alvo da operação", "")
}

func TestPermissionInvalidJSONBody(t *testing.T) {
	userID := newUser(t)

	c := newPermissionContext(t, http.MethodPost, "/channels", []byte("não é json"), nil, nil, userID)

	rec := doPermissionRequest(t, RequireManageChannels(), c)
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param",
		"Parâmetro inválido", "não foi possível determinar o servidor alvo da operação", "")
}

func TestPermissionHelpersMapToCorrectPermission(t *testing.T) {
	ownerID := newUser(t)
	userID := newUser(t)
	server := newServer(t, &ownerID)
	role := newRole(t, server.ID, models.RolePermissions{ManageChannels: true})
	assignRole(t, userID, role.ID)

	names := []string{"server_id"}
	values := []string{server.ID}

	if rec := doPermissionRequest(t, RequireManageChannels(),
		newPermissionContext(t, http.MethodPost, "/channels", nil, names, values, userID)); rec.Code != http.StatusOK {
		t.Fatalf("RequireManageChannels: esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	assertProblem(t, doPermissionRequest(t, RequireManageServer(),
		newPermissionContext(t, http.MethodPut, "/servers/"+server.ID, nil, names, values, userID)),
		http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação", "")
	assertProblem(t, doPermissionRequest(t, RequireManageRoles(),
		newPermissionContext(t, http.MethodPost, "/roles", nil, names, values, userID)),
		http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação", "")
}

func TestPermissionRestoresBodyForHandler(t *testing.T) {
	ownerID := newUser(t)
	userID := newUser(t)
	server := newServer(t, &ownerID)
	role := newRole(t, server.ID, models.RolePermissions{ManageChannels: true})
	assignRole(t, userID, role.ID)

	body, _ := json.Marshal(map[string]string{"server_id": server.ID, "name": "geral"})
	c := newPermissionContext(t, http.MethodPost, "/channels", body, nil, nil, userID)

	handler := RequireManageChannels()(func(ctx echo.Context) error {
		data, err := io.ReadAll(ctx.Request().Body)
		if err != nil {
			t.Fatalf("falha ao ler o corpo no handler: %v", err)
		}
		if !bytes.Equal(data, body) {
			t.Fatalf("corpo restaurado difere do original: %s", data)
		}
		return ctx.String(http.StatusOK, "ok")
	})

	if err := handler(c); err != nil {
		t.Fatalf("handler retornou erro: %v", err)
	}
	if rec := recorder(c); rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// --- CORS ---

// corsTestOrigins espelha o padrão de CORS_ORIGINS (frontend local em HTTP e HTTPS).
var corsTestOrigins = []string{"http://localhost:5173", "https://localhost:5173"}

// doCORSRequest passa uma requisição pelo middleware CORS e por um handler
// que responde 200. origin e requestMethod são headers opcionais.
func doCORSRequest(t *testing.T, mw echo.MiddlewareFunc, method, path, origin, requestMethod string) *httptest.ResponseRecorder {
	t.Helper()

	handler := mw(func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	c := newContext(t, method, path, "")
	if origin != "" {
		c.Request().Header.Set(echo.HeaderOrigin, origin)
	}
	if requestMethod != "" {
		c.Request().Header.Set(echo.HeaderAccessControlRequestMethod, requestMethod)
	}

	if err := handler(c); err != nil {
		// Erros do framework (*echo.HTTPError) já são gravados no
		// ResponseWriter; apenas erros inesperados falham o teste.
		if _, ok := err.(*echo.HTTPError); !ok {
			t.Fatalf("handler retornou erro inesperado: %v", err)
		}
	}
	return recorder(c)
}

func TestCORSPreflightAllowsHTTPOrigin(t *testing.T) {
	mw := CORS(corsTestOrigins)

	rec := doCORSRequest(t, mw, http.MethodOptions, "/api", "http://localhost:5173", http.MethodGet)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(echo.HeaderAccessControlAllowOrigin); got != "http://localhost:5173" {
		t.Errorf("esperava Access-Control-Allow-Origin http://localhost:5173, obtive %q", got)
	}
	if got := rec.Header().Get(echo.HeaderAccessControlAllowCredentials); got != "true" {
		t.Errorf("esperava Access-Control-Allow-Credentials true, obtive %q", got)
	}
	if got := rec.Header().Get(echo.HeaderAccessControlAllowMethods); !strings.Contains(got, http.MethodGet) {
		t.Errorf("esperava Access-Control-Allow-Methods contendo GET, obtive %q", got)
	}
	if got := rec.Header().Get(echo.HeaderVary); !strings.Contains(got, echo.HeaderOrigin) {
		t.Errorf("esperava Vary contendo Origin, obtive %q", got)
	}
}

func TestCORSPreflightAllowsHTTPSOrigin(t *testing.T) {
	mw := CORS(corsTestOrigins)

	rec := doCORSRequest(t, mw, http.MethodOptions, "/api", "https://localhost:5173", http.MethodGet)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(echo.HeaderAccessControlAllowOrigin); got != "https://localhost:5173" {
		t.Errorf("esperava Access-Control-Allow-Origin https://localhost:5173, obtive %q", got)
	}
}

func TestCORSActualRequestAllowedOrigin(t *testing.T) {
	mw := CORS(corsTestOrigins)

	rec := doCORSRequest(t, mw, http.MethodGet, "/api", "http://localhost:5173", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(echo.HeaderAccessControlAllowOrigin); got != "http://localhost:5173" {
		t.Errorf("esperava Access-Control-Allow-Origin http://localhost:5173, obtive %q", got)
	}
	if got := rec.Header().Get(echo.HeaderAccessControlExposeHeaders); !strings.Contains(got, echo.HeaderXRequestID) {
		t.Errorf("esperava Access-Control-Expose-Headers contendo X-Request-Id, obtive %q", got)
	}
}

// TestCORSPreflightDeniesDisallowedOrigin garante que um preflight de origin
// não permitida não recebe Access-Control-Allow-Origin (o navegador bloqueia
// a requisição real por falta do header).
func TestCORSPreflightDeniesDisallowedOrigin(t *testing.T) {
	mw := CORS(corsTestOrigins)

	rec := doCORSRequest(t, mw, http.MethodOptions, "/api", "http://exemplo.invalid", http.MethodGet)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(echo.HeaderAccessControlAllowOrigin); got != "" {
		t.Errorf("não esperava Access-Control-Allow-Origin para origin não permitido, obtive %q", got)
	}
	if got := rec.Header().Get(echo.HeaderAccessControlAllowCredentials); got != "" {
		t.Errorf("não esperava Access-Control-Allow-Credentials para origin não permitido, obtive %q", got)
	}
}

// TestCORSPreflightHidesUnconfiguredMethod garante que o preflight anuncia
// apenas os métodos configurados: o navegador bloqueia a requisição real
// quando o método pedido não está em Access-Control-Allow-Methods.
func TestCORSPreflightHidesUnconfiguredMethod(t *testing.T) {
	mw := CORS(corsTestOrigins)

	rec := doCORSRequest(t, mw, http.MethodOptions, "/api", "http://localhost:5173", http.MethodPatch)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(echo.HeaderAccessControlAllowMethods); strings.Contains(got, http.MethodPatch) {
		t.Errorf("não esperava %s em Access-Control-Allow-Methods, obtive %q", http.MethodPatch, got)
	}
}

func TestCORSWithoutOriginPassesThrough(t *testing.T) {
	mw := CORS(corsTestOrigins)

	rec := doCORSRequest(t, mw, http.MethodGet, "/api", "", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(echo.HeaderAccessControlAllowOrigin); got != "" {
		t.Errorf("não esperava Access-Control-Allow-Origin sem header Origin, obtive %q", got)
	}
}
