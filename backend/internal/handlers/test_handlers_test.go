package handlers

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
	"papo/internal/middleware"
	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/storage"
	"papo/internal/utils"
	"papo/internal/websocket"

	ws "github.com/gorilla/websocket"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/labstack/echo/v4"
	echoMiddleware "github.com/labstack/echo/v4/middleware"
)

// testJWTSecret fixa o segredo JWT dos testes. Os handlers recebem o cfg
// passado em Register*Routes e o middleware chama config.LoadConfig()
// sozinho, então ambos precisam ler o mesmo valor do ambiente.
const testJWTSecret = "test-jwt-secret-rotas"

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

// newApp monta a instância do Echo com as rotas de autenticação, usuários,
// servidores, canais e roles consolidadas em handlers/routes.go
// (tarefas 4.9, 5.2, 5.4 e 6.2).
func newApp() *echo.Echo {
	e := echo.New()
	e.Use(echoMiddleware.RequestID())
	e.Use(echoMiddleware.Recover())
	e.Use(middleware.AuditContext)

	cfg := config.LoadConfig()
	RegisterAuthRoutes(e, cfg)
	RegisterAdminRoutes(e, cfg)
	RegisterUserRoutes(e, cfg)
	RegisterServerRoutes(e, cfg)
	RegisterChannelRoutes(e, cfg)
	RegisterMessageRoutes(e, cfg)
	RegisterAttachmentRoutes(e, cfg)
	RegisterMediaRoutes(e, cfg)
	RegisterLinkPreviewRoutes(e, cfg)
	RegisterEmojiRoutes(e, cfg)
	RegisterRoleRoutes(e, cfg)
	RegisterSearchRoutes(e, cfg)
	RegisterWebSocketRoutes(e, cfg)
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
		{http.MethodGet, "/users"},
		{http.MethodGet, "/users/" + userID + "/profile"},
		{http.MethodPost, "/users/profile_batch"},
		{http.MethodPut, "/users/settings"},
		{http.MethodPut, "/users/" + userID},
		{http.MethodPut, "/users/" + userID + "/status"},
		{http.MethodPut, "/users/" + userID + "/avatar"},
		{http.MethodPut, "/users/" + userID + "/banner"},
		{http.MethodPut, "/users/" + userID + "/password"},
		{http.MethodPut, "/users/" + userID + "/ban"},
		{http.MethodPost, "/users/" + userID + "/reset"},
		{http.MethodPost, "/server"},
		{http.MethodPut, "/server"},
		{http.MethodGet, "/channels"},
		{http.MethodPost, "/channels"},
		{http.MethodPut, "/channels/00000000-0000-4000-8000-000000000000"},
		{http.MethodPut, "/channels/00000000-0000-4000-8000-000000000000/change_position"},
		{http.MethodDelete, "/channels/00000000-0000-4000-8000-000000000000"},
		{http.MethodGet, "/channels/00000000-0000-4000-8000-000000000000/permissions"},
		{http.MethodPut, "/channels/00000000-0000-4000-8000-000000000000/permissions/00000000-0000-4000-8000-000000000000"},
		{http.MethodGet, "/channels/00000000-0000-4000-8000-000000000000/messages"},
		{http.MethodPost, "/messages"},
		{http.MethodPut, "/messages/00000000-0000-4000-8000-000000000000"},
		{http.MethodDelete, "/messages/00000000-0000-4000-8000-000000000000"},
		{http.MethodGet, "/attachments/00000000-0000-4000-8000-000000000000"},
		{http.MethodGet, "/attachments/00000000-0000-4000-8000-000000000000/thumbnail"},
		{http.MethodGet, "/media/0000000000000000000000000000000000000000000000000000000000000000"},
		{http.MethodGet, "/link-previews/00000000-0000-4000-8000-000000000000"},
		{http.MethodGet, "/emojis"},
		{http.MethodPost, "/emojis"},
		{http.MethodDelete, "/emojis/00000000-0000-4000-8000-000000000000"},
		{http.MethodGet, "/roles"},
		{http.MethodPost, "/roles"},
		{http.MethodPut, "/roles/00000000-0000-4000-8000-000000000000"},
		{http.MethodDelete, "/roles/00000000-0000-4000-8000-000000000000"},
		{http.MethodPost, "/users/" + userID + "/roles"},
		{http.MethodDelete, "/users/" + userID + "/roles/00000000-0000-4000-8000-000000000000"},
		{http.MethodPost, "/search"},
		{http.MethodGet, "/admin/audit-logs"},
		{http.MethodGet, "/ws"},
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

// assertWhoamiStatus afirma o status persistido retornado por GET /auth/whoami
// (wantStatus vazio significa status nil).
func assertWhoamiStatus(t *testing.T, e *echo.Echo, token, wantStatus string) {
	t.Helper()
	rec := do(t, e, http.MethodGet, "/auth/whoami", nil, authCookie(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 no whoami, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status *string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if wantStatus == "" {
		if resp.Status != nil {
			t.Errorf("esperava status nil, obtive %q", *resp.Status)
		}
		return
	}
	if resp.Status == nil || *resp.Status != wantStatus {
		t.Errorf("esperava status %q, obtive %v", wantStatus, resp.Status)
	}
}

// TestUpdateStatusRoute garante que PUT /users/:id/status persiste o status
// (away/busy; null remove) e que o status persistido é exposto no whoami.
func TestUpdateStatusRoute(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{"status": "away"})
	rec := do(t, e, http.MethodPut, "/users/"+userID+"/status", body, authCookie(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	assertWhoamiStatus(t, e, token, "away")

	body, _ = json.Marshal(map[string]string{"status": "busy"})
	rec = do(t, e, http.MethodPut, "/users/"+userID+"/status", body, authCookie(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	assertWhoamiStatus(t, e, token, "busy")

	// null remove o status persistido
	rec = do(t, e, http.MethodPut, "/users/"+userID+"/status", []byte(`{"status":null}`), authCookie(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	assertWhoamiStatus(t, e, token, "")
}

// TestUpdateStatusRouteInvalidStatus garante que status fora de {away, busy,
// null} responde 400.
func TestUpdateStatusRouteInvalidStatus(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{"status": "online"})
	rec := do(t, e, http.MethodPut, "/users/"+userID+"/status", body, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"status deve ser away, busy ou null")
}

// TestUpdateStatusRouteForbidden garante que um usuário não atualiza o status
// de outro usuário.
func TestUpdateStatusRouteForbidden(t *testing.T) {
	e := newApp()
	_, tokenA := registerAndLogin(t, e)
	userB, _ := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{"status": "away"})
	rec := do(t, e, http.MethodPut, "/users/"+userB+"/status", body, authCookie(tokenA))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"não é possível atualizar o status de outro usuário")
}

// TestUpdateStatusRouteUnauthorized garante que a rota sem autenticação
// responde 401.
func TestUpdateStatusRouteUnauthorized(t *testing.T) {
	e := newApp()

	body, _ := json.Marshal(map[string]string{"status": "away"})
	rec := do(t, e, http.MethodPut, "/users/00000000-0000-4000-8000-000000000000/status", body, nil)

	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

// TestRegisterBroadcastsUserJoin garante que o registro de um novo usuário
// distribui o evento user_join aos clientes websocket conectados.
func TestRegisterBroadcastsUserJoin(t *testing.T) {
	e := newApp()
	cfg := config.LoadConfig()
	RegisterWebSocketRoutes(e, cfg)
	go websocket.GetHub().Run()

	srv := httptest.NewServer(e)
	defer srv.Close()

	_, tokenA := registerAndLogin(t, e)

	header := http.Header{}
	header.Set(echo.HeaderCookie, "Auth="+tokenA)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := ws.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("falha ao abrir conexão websocket: %v", err)
	}
	defer conn.Close()

	// primeiro evento: presence_sync da própria conexão
	readWSMessage(t, conn)

	userB, _ := registerAndLogin(t, e)

	data := readWSMessage(t, conn)
	var event struct {
		Type   string `json:"type"`
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("falha ao decodificar evento: %v", err)
	}
	if event.Type != "user_join" || event.UserID != userB {
		t.Errorf("esperava user_join do %s, obtive %+v", userB, event)
	}
}

// readWSMessage lê a próxima mensagem de texto da conexão websocket com
// deadline, falhando o teste se ela não chegar a tempo.
func readWSMessage(t *testing.T, conn *ws.Conn) []byte {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("falha ao ler mensagem websocket: %v", err)
	}
	return data
}

// TestProfileRouteWithAuth garante que GET /users/:id/profile responde o perfil
// do usuário autenticado pelo cookie.
func TestProfileRouteWithAuth(t *testing.T) {
	e := newApp()
	userID, _ := registerAndLogin(t, e)

	c := newContext(t, http.MethodGet, "/users/"+userID+"/profile", nil, "")
	c.Set(middleware.UserIDContextKey, userID)
	c.SetParamNames("user_id")
	c.SetParamValues(userID)
	rec := recorder(c)

	if err := ProfileHandler(testBaseURL, c); err != nil {
		t.Fatalf("ProfileHandler retornou erro: %v", err)
	}

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

// TestProfileBatchRouteWithAuth garante que POST /users/profileBatch é
// roteado para o handler de batch (rota estática coexistindo com
// /users/:user_id) e responde os perfis na ordem da requisição.
func TestProfileBatchRouteWithAuth(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string][]string{"ids": {userID, randUUID()}})
	rec := do(t, e, http.MethodPost, "/users/profile_batch", body, authCookie(token))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Profiles []struct {
			ID string `json:"id"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if len(resp.Profiles) != 1 {
		t.Fatalf("esperava 1 perfil (id inexistente deve ser pulado), obtive %d", len(resp.Profiles))
	}
	if resp.Profiles[0].ID != userID {
		t.Errorf("esperava id %q, obtive %q", userID, resp.Profiles[0].ID)
	}
}

// TestProfileBatchRouteUnauthorized garante que POST /users/profileBatch sem
// autenticação é rejeitado com 401 pelo middleware JWT.
func TestProfileBatchRouteUnauthorized(t *testing.T) {
	e := newApp()

	body, _ := json.Marshal(map[string][]string{"ids": {randUUID()}})
	rec := do(t, e, http.MethodPost, "/users/profile_batch", body, nil)

	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
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

	body, _ := json.Marshal(map[string]string{"nickname": "nick-teste", "status": "disponível", "description": "sobre mim"})
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
	if err := cleanServers(context.Background()); err != nil {
		t.Fatalf("falha ao limpar tabelas de servidor: %v", err)
	}
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

	createServerFor(t, ownerID)
	role := createRoleFor(t, models.RolePermissions{ManageServer: true})
	assignRoleToUser(t, actorID, role.ID)

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
		{http.MethodDelete, "/server"},
		{http.MethodDelete, "/channels"},
		{http.MethodGet, "/channels/00000000-0000-4000-8000-000000000000"},
		{http.MethodPost, "/channels/00000000-0000-4000-8000-000000000000"},
		{http.MethodGet, "/channels/00000000-0000-4000-8000-000000000000/permissions/00000000-0000-4000-8000-000000000000"},
		{http.MethodPost, "/channels/00000000-0000-4000-8000-000000000000/messages"},
		{http.MethodGet, "/messages"},
		{http.MethodGet, "/messages/00000000-0000-4000-8000-000000000000"},
		{http.MethodDelete, "/messages"},
		{http.MethodDelete, "/roles"},
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

// TestGetServerRouteWithAuth garante que GET /server responde o detalhe do
// único servidor do backend para o usuário autenticado pelo cookie.
func TestGetServerRouteWithAuth(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	server := createServerFor(t, userID)

	rec := do(t, e, http.MethodGet, "/server", nil, authCookie(token))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID            string  `json:"id"`
		Name          string  `json:"name"`
		OwnerID       *string `json:"owner_id"`
		OwnerUsername *string `json:"owner_username"`
		RoleCount     int     `json:"role_count"`
		MemberCount   int     `json:"member_count"`
		ChannelCount  int     `json:"channel_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID != server.ID {
		t.Errorf("esperava id %q, obtive %q", server.ID, resp.ID)
	}
	if resp.Name != server.Name {
		t.Errorf("esperava name %q, obtive %q", server.Name, resp.Name)
	}
	if resp.OwnerID == nil || *resp.OwnerID != userID {
		t.Errorf("esperava owner_id %q, obtive %v", userID, resp.OwnerID)
	}
	if resp.OwnerUsername == nil {
		t.Error("esperava owner_username preenchido")
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

// TestGetServerRouteNotFound garante que GET /server responde 404 quando o
// backend ainda não tem servidor.
func TestGetServerRouteNotFound(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)
	removeAllServersTest(t)

	rec := do(t, e, http.MethodGet, "/server", nil, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "servidor não encontrado")
}

// TestUpdateServerRouteWithAuth garante que PUT /server atualiza o servidor
// do usuário autenticado pelo cookie.
func TestUpdateServerRouteWithAuth(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	server := createServerFor(t, userID)

	newName := "srv_" + randHex(4)
	body, _ := json.Marshal(map[string]string{"name": newName})
	rec := do(t, e, http.MethodPut, "/server", body, authCookie(token))

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

	stored, err := storage.GetServer(context.Background())
	if err != nil {
		t.Fatalf("GetServer retornou erro: %v", err)
	}
	if stored.Name != newName {
		t.Errorf("esperava name %q persistido, obtive %q", newName, stored.Name)
	}
}

// TestUpdateServerRouteOtherUserForbidden garante que a atualização do
// servidor por um usuário que não é dono nem possui a role manage_server é
// negada com 403.
func TestUpdateServerRouteOtherUserForbidden(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	_, token := registerAndLogin(t, e)

	createServerFor(t, ownerID)

	body, _ := json.Marshal(map[string]string{"name": "srv_" + randHex(4)})
	rec := do(t, e, http.MethodPut, "/server", body, authCookie(token))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado", "usuário não possui a permissão necessária para esta operação")
}

// --- POST /server ---

// TestCreateServerRouteWithAuth garante que POST /server cria o servidor
// com o usuário autenticado como dono.
func TestCreateServerRouteWithAuth(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	userID, token := registerAndLogin(t, e)

	serverName := "srv_" + randHex(4)
	body, _ := json.Marshal(map[string]any{"name": serverName, "Public": true})
	rec := do(t, e, http.MethodPost, "/server", body, authCookie(token))

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

	stored, err := storage.GetServer(context.Background())
	if err != nil {
		t.Fatalf("GetServer retornou erro: %v", err)
	}
	if stored.Name != serverName {
		t.Errorf("esperava name %q persistido, obtive %q", serverName, stored.Name)
	}
	if stored.OwnerID == nil || *stored.OwnerID != userID {
		t.Errorf("esperava owner_id %s persistido, obtive %v", userID, stored.OwnerID)
	}
}

// TestCreateServerRouteAlreadyExists garante que POST /server responde 409
// quando o backend já possui o servidor único.
func TestCreateServerRouteAlreadyExists(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)

	body, _ := json.Marshal(map[string]any{"name": "srv_" + randHex(4), "public": true})
	rec := do(t, e, http.MethodPost, "/server", body, authCookie(token))

	assertProblem(t, rec, http.StatusConflict, "server-already-exists", "Ação Proibida",
		"O servidor já foi criado, não há como criar mais de 1 servidor")
}

// TestCreateServerRouteUnauthenticated garante que POST /server exige
// autenticação.
func TestCreateServerRouteUnauthenticated(t *testing.T) {
	e := newApp()

	body, _ := json.Marshal(map[string]string{"name": "srv_" + randHex(4)})
	rec := do(t, e, http.MethodPost, "/server", body, nil)

	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized",
		"Token inválido ou expirado", "token de autenticação ausente, inválido ou expirado")
}

// TestCreateServerRouteInvalidInput garante que POST /server responde 400
// para nome ausente.
func TestCreateServerRouteInvalidInput(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{"icon_format": "PNG"})
	rec := do(t, e, http.MethodPost, "/server", body, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"name é obrigatório e deve ter no máximo 32 caracteres; icon_blob deve ser base64 de um GIF, JPEG ou PNG de até 2MB; servidor privado (public=false) exige password")
}

// --- helpers de setup para canais e roles ---

// createServerFor cria o servidor do backend para o usuário e retorna o
// registro criado (limpa o estado anterior, pois existe 1 servidor por backend).
func createServerFor(t *testing.T, userID string) models.Server {
	t.Helper()
	if err := cleanServers(context.Background()); err != nil {
		t.Fatalf("falha ao limpar tabelas de servidor: %v", err)
	}
	server, err := storage.CreateServer(context.Background(), "srv_"+randHex(4), &userID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	return server
}

// createChannelFor cria um canal text e retorna o registro criado.
func createChannelFor(t *testing.T, name string) models.Channel {
	t.Helper()
	channel, err := storage.CreateChannel(context.Background(), name, "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	return channel
}

// createRoleFor cria uma role e retorna o registro criado.
func createRoleFor(t *testing.T, permissions models.RolePermissions) models.Role {
	t.Helper()
	role, err := storage.CreateRole(context.Background(), "role_"+randHex(8), nil, permissions)
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
	createServerFor(t, userID)
	channelName := "chn_" + randHex(4)
	channel := createChannelFor(t, channelName)

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

// TestCreateChannelRouteOwner garante que o dono do servidor cria um canal
// via POST /channels e que o canal é persistido.
func TestCreateChannelRouteOwner(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channelName := "chn_" + randHex(4)

	body, _ := json.Marshal(map[string]string{"name": channelName})
	rec := do(t, e, http.MethodPost, "/channels", body, authCookie(token))

	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.ChannelSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID == "" || resp.Name != channelName {
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
	if stored.Name != channelName {
		t.Errorf("persistência inesperada: %+v", stored)
	}
}

// TestCreateChannelRouteWithTopic garante que um POST /channels com topic
// persiste o tópico e o expõe na resposta.
func TestCreateChannelRouteWithTopic(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)

	topic := "tópico do canal"
	body, _ := json.Marshal(map[string]string{"name": "chn_" + randHex(4), "topic": topic})
	rec := do(t, e, http.MethodPost, "/channels", body, authCookie(token))

	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.ChannelSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.Topic == nil || *resp.Topic != topic {
		t.Errorf("esperava topic %q, obtive %v", topic, resp.Topic)
	}

	stored, err := storage.GetChannelByID(context.Background(), resp.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Topic == nil || *stored.Topic != topic {
		t.Errorf("esperava topic %q persistido, obtive %v", topic, stored.Topic)
	}
}

// TestCreateChannelRouteForbiddenWithoutPermission garante que um usuário sem
// permissão manage_channels é negado com 403.
func TestCreateChannelRouteForbiddenWithoutPermission(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	_, actorToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)

	body, _ := json.Marshal(map[string]string{"name": "chn_" + randHex(4)})
	rec := do(t, e, http.MethodPost, "/channels", body, authCookie(actorToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação")
}

// TestCreateChannelRouteWithManageChannelsRole garante que um usuário com a
// permissão manage_channels cria canal sem ser dono.
func TestCreateChannelRouteWithManageChannelsRole(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	actorID, actorToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	role := createRoleFor(t, models.RolePermissions{ManageChannels: true})
	assignRoleToUser(t, actorID, role.ID)

	body, _ := json.Marshal(map[string]string{"name": "chn_" + randHex(4)})
	rec := do(t, e, http.MethodPost, "/channels", body, authCookie(actorToken))

	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// TestCreateChannelRouteInvalidInput garante que POST /channels responde 400
// para nome ausente ou acima de 32 caracteres.
func TestCreateChannelRouteInvalidInput(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)

	for _, name := range []string{"", strings.Repeat("c", 33)} {
		body, _ := json.Marshal(map[string]string{"name": name})
		rec := do(t, e, http.MethodPost, "/channels", body, authCookie(token))
		assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
			"name é obrigatório e deve ter no máximo 32 caracteres; type deve ser 'text' ou 'category'; topic tem no máximo 512 caracteres e é válido apenas para canais de texto")
	}
}

// TestCreateChannelRouteNameTaken garante que POST /channels responde 409
// quando o nome do canal já está em uso.
func TestCreateChannelRouteNameTaken(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channelName := "chn_" + randHex(4)
	createChannelFor(t, channelName)

	body, _ := json.Marshal(map[string]string{"name": channelName})
	rec := do(t, e, http.MethodPost, "/channels", body, authCookie(token))

	assertProblem(t, rec, http.StatusConflict, "channel-name-taken", "Nome de canal já existe",
		"o nome informado já está em uso")
}

// TestUpdateChannelRouteOwner garante que o dono do servidor renomeia o canal
// via PUT /channels/:channel_id e que o novo nome é persistido.
func TestUpdateChannelRouteOwner(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))
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

// TestUpdateChannelRouteWithTopic garante que um PUT /channels/:channel_id com
// topic atualiza o tópico e o expõe na resposta.
func TestUpdateChannelRouteWithTopic(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))

	topic := "novo tópico"
	body, _ := json.Marshal(map[string]string{"name": channel.Name, "topic": topic})
	rec := do(t, e, http.MethodPut, "/channels/"+channel.ID, body, authCookie(token))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.ChannelSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.Topic == nil || *resp.Topic != topic {
		t.Errorf("esperava topic %q, obtive %v", topic, resp.Topic)
	}

	stored, err := storage.GetChannelByID(context.Background(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelByID retornou erro: %v", err)
	}
	if stored.Topic == nil || *stored.Topic != topic {
		t.Errorf("esperava topic %q persistido, obtive %v", topic, stored.Topic)
	}
}

// TestUpdateChannelRouteForbiddenWithoutPermission garante que um usuário sem
// permissão manage_channels no servidor do canal é negado com 403.
func TestUpdateChannelRouteForbiddenWithoutPermission(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	_, actorToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))

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
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))

	body, _ := json.Marshal(map[string]string{"name": ""})
	rec := do(t, e, http.MethodPut, "/channels/"+channel.ID, body, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"name é obrigatório e deve ter no máximo 32 caracteres; topic tem no máximo 512 caracteres e é válido apenas para canais de texto")
}

// TestUpdateChannelRouteNotFound garante que PUT /channels/:channel_id
// responde 404 para canal inexistente.
func TestUpdateChannelRouteNotFound(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)

	body, _ := json.Marshal(map[string]string{"name": "chn_" + randHex(4)})
	rec := do(t, e, http.MethodPut, "/channels/00000000-0000-4000-8000-000000000000", body, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

// TestDeleteChannelRouteOwner garante que o dono do servidor exclui o canal
// via DELETE /channels/:channel_id.
func TestDeleteChannelRouteOwner(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))

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
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)

	rec := do(t, e, http.MethodDelete, "/channels/00000000-0000-4000-8000-000000000000", nil, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

// TestGetChannelPermissionsRouteWithAuth garante que
// GET /channels/:channel_id/permissions responde as permissões do canal
// expandidas por role.
func TestGetChannelPermissionsRouteWithAuth(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	role := createRoleFor(t, models.RolePermissions{})

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
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	role := createRoleFor(t, models.RolePermissions{})

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

// TestUpdateChannelPermissionsRouteNotFound garante que
// PUT /channels/:channel_id/permissions/:role_id responde 404 para canal
// inexistente.
func TestUpdateChannelPermissionsRouteNotFound(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)

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
	createServerFor(t, userID)
	c1 := createChannelFor(t, "chn_"+randHex(4))
	c2 := createChannelFor(t, "chn_"+randHex(4))
	c3 := createChannelFor(t, "chn_"+randHex(4))

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

	channels, err := storage.ListChannels(context.Background())
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
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))

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
	createServerFor(t, ownerID)
	createChannelFor(t, "chn_"+randHex(4))
	c2 := createChannelFor(t, "chn_"+randHex(4))
	role := createRoleFor(t, models.RolePermissions{ManageChannels: true})
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

// TestChangeChannelPositionRouteInvalidInput garante que posições inválidas
// respondem 400.
func TestChangeChannelPositionRouteInvalidInput(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))

	body, _ := json.Marshal(map[string]int{"old_position": 1, "new_position": 2})
	rec := do(t, e, http.MethodPut, "/channels/"+channel.ID+"/change_position", body, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"old_position e new_position devem ser posições válidas (1 até o número de canais)")
}

// TestChangeChannelPositionRouteNotFound garante que
// PUT /channels/:channel_id/change_position responde 404 para canal
// inexistente.
func TestChangeChannelPositionRouteNotFound(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)

	body, _ := json.Marshal(map[string]int{"old_position": 1, "new_position": 1})
	rec := do(t, e, http.MethodPut, "/channels/00000000-0000-4000-8000-000000000000/change_position", body, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

// TestChangeChannelPositionRouteConflict garante que old_position divergente
// da posição atual responde 409 channel-position-conflict.
func TestChangeChannelPositionRouteConflict(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	c1 := createChannelFor(t, "chn_"+randHex(4))
	createChannelFor(t, "chn_"+randHex(4))

	body, _ := json.Marshal(map[string]int{"old_position": 2, "new_position": 1})
	rec := do(t, e, http.MethodPut, "/channels/"+c1.ID+"/change_position", body, authCookie(token))

	assertProblem(t, rec, http.StatusConflict, "channel-position-conflict", "Posição do canal desatualizada",
		"a posição atual do canal não corresponde à old_position informada")
}

// --- rotas de roles (tarefas 6.1 a 6.4) ---

// TestListRolesRouteWithAuth garante que GET /roles responde as roles do
// backend para o usuário autenticado.
func TestListRolesRouteWithAuth(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	roleA := createRoleFor(t, models.RolePermissions{ManageChannels: true})
	roleB := createRoleFor(t, models.RolePermissions{ManageRoles: true})

	rec := do(t, e, http.MethodGet, "/roles", nil, authCookie(token))

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
		if got.Permissions != want.permissions {
			t.Errorf("esperava permissions %+v, obtive %+v", want.permissions, got.Permissions)
		}
	}
}

// TestCreateRoleRouteOwner garante que o dono do servidor cria uma role via
// POST /roles e que a role é persistida.
func TestCreateRoleRouteOwner(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	roleName := "role_" + randHex(8)
	color := "#ff0000"
	permissions := models.RolePermissions{ManageChannels: true}

	body, _ := json.Marshal(map[string]any{
		"name":        roleName,
		"color":       color,
		"permissions": permissions,
	})
	rec := do(t, e, http.MethodPost, "/roles", body, authCookie(token))

	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.Role
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID == "" || resp.Name != roleName {
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
	createServerFor(t, ownerID)

	body, _ := json.Marshal(map[string]string{"name": "role_" + randHex(8)})
	rec := do(t, e, http.MethodPost, "/roles", body, authCookie(actorToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação")
}

// TestCreateRoleRouteWithManageRolesRole garante que um usuário com a
// permissão manage_roles no servidor cria role sem ser dono.
func TestCreateRoleRouteWithManageRolesRole(t *testing.T) {
	e := newApp()
	ownerID, _ := registerAndLogin(t, e)
	actorID, actorToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	role := createRoleFor(t, models.RolePermissions{ManageRoles: true})
	assignRoleToUser(t, actorID, role.ID)

	body, _ := json.Marshal(map[string]string{"name": "role_" + randHex(8)})
	rec := do(t, e, http.MethodPost, "/roles", body, authCookie(actorToken))

	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// TestCreateRoleRouteInvalidInput garante que POST /roles responde 400 para
// cor inválida ou nome ausente.
func TestCreateRoleRouteInvalidInput(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)

	cases := []map[string]any{
		{"name": "role_" + randHex(8), "color": "vermelho"},
		{"color": "#ff0000"},
	}
	for _, raw := range cases {
		body, _ := json.Marshal(raw)
		rec := do(t, e, http.MethodPost, "/roles", body, authCookie(token))
		assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
			"name é obrigatório e deve ter no máximo 32 caracteres; color deve ser hexadecimal #RRGGBB")
	}
}

// TestCreateRoleRouteNameTaken garante que POST /roles responde 409 quando o
// nome da role já existe.
func TestCreateRoleRouteNameTaken(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	roleName := "role_" + randHex(8)

	body, _ := json.Marshal(map[string]string{"name": roleName})
	do(t, e, http.MethodPost, "/roles", body, authCookie(token))
	rec := do(t, e, http.MethodPost, "/roles", body, authCookie(token))

	assertProblem(t, rec, http.StatusConflict, "role-name-taken", "Nome de role já existe",
		"o nome informado já está em uso")
}

// TestUpdateRoleRouteOwner garante que o dono do servidor atualiza a role via
// PUT /roles/:role_id e que as alterações são persistidas.
func TestUpdateRoleRouteOwner(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	role := createRoleFor(t, models.RolePermissions{})
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
	createServerFor(t, ownerID)
	role := createRoleFor(t, models.RolePermissions{})

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
	createServerFor(t, userID)
	role := createRoleFor(t, models.RolePermissions{})

	body, _ := json.Marshal(map[string]any{"name": "role_" + randHex(8), "color": "azul"})
	rec := do(t, e, http.MethodPut, "/roles/"+role.ID, body, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"name é obrigatório e deve ter no máximo 32 caracteres; color deve ser hexadecimal #RRGGBB")
}

// TestUpdateRoleRouteNotFound garante que PUT /roles/:role_id responde 404
// para role inexistente.
func TestUpdateRoleRouteNotFound(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)

	body, _ := json.Marshal(map[string]string{"name": "role_" + randHex(8)})
	rec := do(t, e, http.MethodPut, "/roles/00000000-0000-4000-8000-000000000000", body, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "role não encontrada")
}

// TestDeleteRoleRouteOwner garante que o dono do servidor exclui a role via
// DELETE /roles/:role_id.
func TestDeleteRoleRouteOwner(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	role := createRoleFor(t, models.RolePermissions{})

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
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)

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
	createServerFor(t, ownerID)
	role := createRoleFor(t, models.RolePermissions{ManageChannels: true})

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
	createServerFor(t, ownerID)
	role := createRoleFor(t, models.RolePermissions{})

	body, _ := json.Marshal(map[string]string{"role_id": role.ID})
	rec := do(t, e, http.MethodPost, "/users/"+targetID+"/roles", body, authCookie(actorToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação")
}

// TestAssignUserRoleRouteMissingRoleID garante que POST /users/:user_id/roles
// responde 400 quando o corpo não informa role_id.
func TestAssignUserRoleRouteMissingRoleID(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)

	body, _ := json.Marshal(map[string]string{})
	rec := do(t, e, http.MethodPost, "/users/"+userID+"/roles", body, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"role_id é obrigatório")
}

// TestAssignUserRoleRouteUserNotFound garante que POST /users/:user_id/roles
// responde 404 para usuário inexistente.
func TestAssignUserRoleRouteUserNotFound(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	role := createRoleFor(t, models.RolePermissions{})

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
	createServerFor(t, ownerID)
	role := createRoleFor(t, models.RolePermissions{})
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
	createServerFor(t, ownerID)
	role := createRoleFor(t, models.RolePermissions{})
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
	createServerFor(t, ownerID)
	role := createRoleFor(t, models.RolePermissions{})

	rec := do(t, e, http.MethodDelete, "/users/"+targetID+"/roles/"+role.ID, nil, authCookie(ownerToken))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado",
		"role não atribuída ao usuário")
}

// TestRemoveUserRoleRouteUserNotFound garante que DELETE
// /users/:user_id/roles/:role_id responde 404 para usuário inexistente.
func TestRemoveUserRoleRouteUserNotFound(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	role := createRoleFor(t, models.RolePermissions{})

	rec := do(t, e, http.MethodDelete, "/users/00000000-0000-4000-8000-000000000000/roles/"+role.ID, nil, authCookie(ownerToken))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "usuário não encontrado")
}

// --- rotas de mensagens (tarefa 7.2) ---

// TestListMessagesRouteWithAuth garante que GET /channels/:channel_id/messages
// responde a listagem (vazia) para o usuário autenticado pelo cookie.
func TestListMessagesRouteWithAuth(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))

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
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	first, err := storage.CreateMessage(context.Background(), channel.ID, userID, "primeira", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar primeira mensagem: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	second, err := storage.CreateMessage(context.Background(), channel.ID, userID, "segunda", "", nil)
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
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))

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

// TestCreateMessageRouteWithReplyTo garante que um POST /messages com reply_to
// persiste a referência e a expõe na resposta.
func TestCreateMessageRouteWithReplyTo(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))

	targetRec := doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": "mensagem alvo"}, nil, authCookie(token))
	if targetRec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201 para mensagem alvo, obtive %d (corpo: %s)", targetRec.Code, targetRec.Body.String())
	}
	var target models.MessageWithAttachment
	if err := json.Unmarshal(targetRec.Body.Bytes(), &target); err != nil {
		t.Fatalf("falha ao decodificar mensagem alvo: %v", err)
	}

	rec := doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": "resposta", "reply_to": target.ID}, nil, authCookie(token))
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp models.MessageWithAttachment
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ReplyTo == nil || *resp.ReplyTo != target.ID {
		t.Errorf("esperava reply_to %q, obtive %v", target.ID, resp.ReplyTo)
	}

	stored, err := storage.GetMessageByID(context.Background(), resp.ID)
	if err != nil {
		t.Fatalf("GetMessageByID retornou erro: %v", err)
	}
	if stored.ReplyTo == nil || *stored.ReplyTo != target.ID {
		t.Errorf("esperava reply_to %q persistido, obtive %v", target.ID, stored.ReplyTo)
	}
}

// TestCreateMessageRouteReplyToNotFound garante que um POST /messages com
// reply_to inexistente retorna 404.
func TestCreateMessageRouteReplyToNotFound(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))

	rec := doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": "resposta", "reply_to": randUUID()}, nil, authCookie(token))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("esperava status 404, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// TestCreateMessageRouteWithAttachment garante que um POST /messages com
// attachment persiste o attachment e o expõe na resposta.
func TestCreateMessageRouteWithAttachment(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))
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
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	role := createRoleFor(t, models.RolePermissions{})
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
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))

	rec := doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": strings.Repeat("a", 8193)}, nil, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"channel_id é obrigatório; content tem no máximo 8192 caracteres; a mensagem precisa de content ou attachment; nome do attachment inválido; reply_to deve referenciar uma mensagem do mesmo canal")
}

// TestUpdateMessageRouteAuthor garante que o autor edita sua mensagem via
// PUT /messages/:message_id.
func TestUpdateMessageRouteAuthor(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	message, err := storage.CreateMessage(context.Background(), channel.ID, userID, "original", "", nil)
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
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	message, err := storage.CreateMessage(context.Background(), channel.ID, ownerID, "x", "", nil)
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
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	message, err := storage.CreateMessage(context.Background(), channel.ID, userID, "a excluir", "", nil)
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
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	message, err := storage.CreateMessage(context.Background(), channel.ID, ownerID, "x", "", nil)
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
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	message, err := storage.CreateMessage(context.Background(), channel.ID, ownerID, "x", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	role := createRoleFor(t, models.RolePermissions{})
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
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	message, err := storage.CreateMessage(context.Background(), channel.ID, otherID, "x", "", nil)
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
func closeChannelRoute(t *testing.T, e *echo.Echo, channelID, token string) models.Role {
	t.Helper()
	role := createRoleFor(t, models.RolePermissions{})
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
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))
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
	blob, err := os.ReadFile(mediaPathFor(attachment.MediaShaHash))
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
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	role := closeChannelRoute(t, e, channel.ID, ownerToken)
	assignRoleToUser(t, readerID, role.ID)
	attachment := newMessageWithAttachmentRoute(t, e, channel.ID, ownerToken)

	rec := do(t, e, http.MethodGet, "/attachments/"+attachment.ID, nil, authCookie(readerToken))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	blob, err := os.ReadFile(mediaPathFor(attachment.MediaShaHash))
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
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	closeChannelRoute(t, e, channel.ID, ownerToken)
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
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	attachment := newMessageWithAttachmentRoute(t, e, channel.ID, token)

	if err := os.Remove(mediaPathFor(attachment.MediaShaHash)); err != nil {
		t.Fatalf("falha ao remover o blob de apoio: %v", err)
	}

	rec := do(t, e, http.MethodGet, "/attachments/"+attachment.ID, nil, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "arquivo não encontrado")
}

// newMessageWithRealImageRoute cria uma mensagem com um PNG real (a
// thumbnail é gerada em background pelo pipeline de attachments).
func newMessageWithRealImageRoute(t *testing.T, e *echo.Echo, channelID, token string, png []byte) models.Attachments {
	t.Helper()

	rec := doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channelID, "content": "com imagem real"},
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

// TestDownloadAttachmentThumbnailRouteOwner garante que o dono baixa a
// thumbnail via GET /attachments/:file_id/thumbnail recebendo o WebP com
// Content-Disposition: inline e o Content-Length correto.
func TestDownloadAttachmentThumbnailRouteOwner(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	attachment := newMessageWithRealImageRoute(t, e, channel.ID, token, pngAvatarBytes(64, 32))

	thumb, err := storage.GetThumbnailByAttachmentID(context.Background(), attachment.ID, "preview")
	if err != nil {
		t.Fatalf("thumbnail nao deveria ter falhado na geracao: %v", err)
	}
	// size_bytes vem da tabela media (join); o caminho do blob é derivado do hash
	media, err := storage.GetMediaByHash(context.Background(), thumb.MediaShaHash)
	if err != nil {
		t.Fatalf("GetMediaByHash retornou erro: %v", err)
	}
	h := thumb.MediaShaHash
	blobPath := filepath.Join("media", h[:2], h[2:4], h)

	rec := do(t, e, http.MethodGet, "/attachments/"+attachment.ID+"/thumbnail", nil, authCookie(token))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get(echo.HeaderContentType); ct != "image/webp" {
		t.Errorf("esperava content-type image/webp, obtive %q", ct)
	}
	if cd := rec.Header().Get(echo.HeaderContentDisposition); cd != "inline" {
		t.Errorf("esperava content-disposition inline, obtive %q", cd)
	}
	if cl := rec.Header().Get(echo.HeaderContentLength); cl != fmt.Sprint(media.SizeBytes) {
		t.Errorf("esperava content-length %d, obtive %q", media.SizeBytes, cl)
	}
	blob, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("falha ao ler a thumbnail em disco: %v", err)
	}
	if !bytes.Equal(rec.Body.Bytes(), blob) {
		t.Errorf("corpo do download nao corresponde a thumbnail em disco")
	}
}

// TestDownloadAttachmentThumbnailRouteUnauthorized garante que a rota sem
// autenticação responde 401.
func TestDownloadAttachmentThumbnailRouteUnauthorized(t *testing.T) {
	e := newApp()

	rec := do(t, e, http.MethodGet, "/attachments/00000000-0000-4000-8000-000000000000/thumbnail", nil, nil)

	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

// TestDownloadAttachmentThumbnailRouteForbidden garante que um usuário sem
// read_channel em canal fechado é negado com 403.
func TestDownloadAttachmentThumbnailRouteForbidden(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	_, strangerToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	closeChannelRoute(t, e, channel.ID, ownerToken)
	attachment := newMessageWithRealImageRoute(t, e, channel.ID, ownerToken, pngAvatarBytes(64, 32))

	rec := do(t, e, http.MethodGet, "/attachments/"+attachment.ID+"/thumbnail", nil, authCookie(strangerToken))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não tem permissão para ver a thumbnail")
}

// TestDownloadAttachmentThumbnailRouteNotFound garante que a rota responde
// 404 para attachment inexistente e para attachment sem thumbnail.
func TestDownloadAttachmentThumbnailRouteNotFound(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))

	rec := do(t, e, http.MethodGet, "/attachments/00000000-0000-4000-8000-000000000000/thumbnail", nil, authCookie(token))
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "thumbnail não encontrada")

	// PNG inválido (só magic bytes) → sem thumbnail gerada
	attachment := newMessageWithAttachmentRoute(t, e, channel.ID, token)
	rec = do(t, e, http.MethodGet, "/attachments/"+attachment.ID+"/thumbnail", nil, authCookie(token))
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "thumbnail não encontrada")
}

// newPreviewWithImageRoute grava um preview com imagem real em disco e o
// vincula a uma mensagem nova no canal informado.
func newPreviewWithImageRoute(t *testing.T, e *echo.Echo, channelID, token string) models.LinkPreview {
	t.Helper()
	ctx := context.Background()

	img := pngAvatarBytes(16, 16)
	imgHash := newTestMediaHash(t, img)
	// URL unica por chamada: UpsertPreview faz ON CONFLICT (url) DO UPDATE,
	// entao uma URL fixa seria compartilhada entre testes e o preview ficaria
	// vinculado a mensagens de canais diferentes.
	preview, err := storage.UpsertPreview(ctx, models.LinkPreview{
		URL:        "https://preview-route.example.com/pagina-" + randHex(8),
		Kind:       "og",
		ImageMedia: &imgHash,
	})
	if err != nil {
		t.Fatalf("UpsertPreview retornou erro: %v", err)
	}

	rec := doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channelID, "content": "mensagem de apoio"},
		nil, authCookie(token))
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201 ao criar mensagem, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var created models.MessageWithAttachment
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if err := storage.AddMessagePreviews(ctx, created.Message.ID, []string{preview.ID}); err != nil {
		t.Fatalf("falha ao vincular preview à mensagem: %v", err)
	}
	return preview
}

// TestGetLinkPreviewRouteOwner garante que o dono do servidor busca o preview
// via GET /link-previews/:preview_id em JSON, com os campos do preview e a
// imagem em base64 (image_data) correspondente ao arquivo em disco.
func TestGetLinkPreviewRouteOwner(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	preview := newPreviewWithImageRoute(t, e, channel.ID, token)

	rec := do(t, e, http.MethodGet, "/link-previews/"+preview.ID, nil, authCookie(token))

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var got struct {
		ID            string  `json:"id"`
		URL           string  `json:"url"`
		ImageMimeType *string `json:"image_mime_type"`
		ImageData     *string `json:"image_data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if got.ID != preview.ID {
		t.Errorf("esperava preview %s, obtive %s", preview.ID, got.ID)
	}
	if got.URL != preview.URL {
		t.Errorf("esperava url %s, obtive %s", preview.URL, got.URL)
	}
	if got.ImageMimeType == nil || *got.ImageMimeType != "image/png" {
		t.Errorf("esperava image_mime_type image/png, obtive %v", got.ImageMimeType)
	}
	if got.ImageData == nil {
		t.Fatalf("esperava image_data presente")
	}
	decoded, err := base64.StdEncoding.DecodeString(*got.ImageData)
	if err != nil {
		t.Fatalf("falha ao decodificar image_data: %v", err)
	}
	blob, err := os.ReadFile(mediaPathFor(*preview.ImageMedia))
	if err != nil {
		t.Fatalf("falha ao ler a imagem em disco: %v", err)
	}
	if !bytes.Equal(decoded, blob) {
		t.Errorf("image_data nao corresponde a imagem em disco")
	}
}

// TestGetLinkPreviewRouteUnauthorized garante que a rota sem
// autenticação responde 401.
func TestGetLinkPreviewRouteUnauthorized(t *testing.T) {
	e := newApp()

	rec := do(t, e, http.MethodGet, "/link-previews/00000000-0000-4000-8000-000000000000", nil, nil)

	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

// TestGetLinkPreviewRouteNotFound garante que a rota responde 404 para
// preview inexistente, preview sem vinculo com mensagem e preview de canal
// fechado para quem não tem acesso (não vaza existência). Preview sem imagem
// é acessível (200, image_data nulo).
func TestGetLinkPreviewRouteNotFound(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	_, strangerToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	preview := newPreviewWithImageRoute(t, e, channel.ID, ownerToken)

	rec := do(t, e, http.MethodGet, "/link-previews/00000000-0000-4000-8000-000000000000", nil, authCookie(ownerToken))
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "preview não encontrado")

	ctx := context.Background()
	noImg, err := storage.UpsertPreview(ctx, models.LinkPreview{
		URL:  "https://preview-route.example.com/sem-imagem-" + randHex(8),
		Kind: "og",
	})
	if err != nil {
		t.Fatalf("UpsertPreview retornou erro: %v", err)
	}
	var msgID string
	if err := storage.GetDB().QueryRowContext(ctx,
		"SELECT id FROM messages WHERE channel_id = $1 LIMIT 1", channel.ID).Scan(&msgID); err != nil {
		t.Fatalf("falha ao buscar mensagem de apoio: %v", err)
	}
	if err := storage.AddMessagePreviews(ctx, msgID, []string{noImg.ID}); err != nil {
		t.Fatalf("falha ao vincular preview: %v", err)
	}
	// preview sem imagem é acessível: 200 com image_data nulo
	rec = do(t, e, http.MethodGet, "/link-previews/"+noImg.ID, nil, authCookie(ownerToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview sem imagem deveria ser acessível, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var noImgResp struct {
		ID        string  `json:"id"`
		ImageData *string `json:"image_data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &noImgResp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if noImgResp.ID != noImg.ID {
		t.Errorf("esperava preview %s, obtive %s", noImg.ID, noImgResp.ID)
	}
	if noImgResp.ImageData != nil {
		t.Errorf("esperava image_data nulo, obtive %q", *noImgResp.ImageData)
	}

	unlinked, err := storage.UpsertPreview(ctx, models.LinkPreview{
		URL:  "https://preview-route.example.com/sem-vinculo-" + randHex(8),
		Kind: "og",
	})
	if err != nil {
		t.Fatalf("UpsertPreview retornou erro: %v", err)
	}
	rec = do(t, e, http.MethodGet, "/link-previews/"+unlinked.ID, nil, authCookie(ownerToken))
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "preview não encontrado")

	// canal fechado: estranho sem acesso → 404 (não 403, não vaza existência)
	closeChannelRoute(t, e, channel.ID, ownerToken)
	rec = do(t, e, http.MethodGet, "/link-previews/"+preview.ID, nil, authCookie(strangerToken))
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "preview não encontrado")

	// dono continua acessando
	rec = do(t, e, http.MethodGet, "/link-previews/"+preview.ID, nil, authCookie(ownerToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("dono deveria continuar acessando, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// --- rotas de emojis (tarefa 7.4) ---

// newEmojiRoute cria um emoji via POST /emojis usando o cookie do usuário e
// retorna o registro criado.
func newEmojiRoute(t *testing.T, e *echo.Echo, token string) models.Emoji {
	t.Helper()

	body, _ := json.Marshal(map[string]string{
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
	createServerFor(t, userID)
	emoji := newEmojiRoute(t, e, token)

	rec := do(t, e, http.MethodGet, "/emojis", nil, authCookie(token))
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
	createServerFor(t, userID)
	modUserID, modToken := registerAndLogin(t, e)

	role := createRoleFor(t, models.RolePermissions{ManageServer: true})
	assignRoleToUser(t, modUserID, role.ID)

	newEmojiRoute(t, e, modToken)
}

func TestCreateEmojiRouteForbiddenWithoutPermission(t *testing.T) {
	e := newApp()
	userID, _ := registerAndLogin(t, e)
	createServerFor(t, userID)
	_, strangerToken := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string]string{
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
	createServerFor(t, userID)
	authorID, authorToken := registerAndLogin(t, e)

	// emoji criado pelo autor (não dono do servidor)
	emoji, err := storage.CreateEmoji(testCtx(), "emoji_"+randHex(8), newTestMediaHash(t, []byte{1}), &authorID)
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
	createServerFor(t, userID)
	modUserID, modToken := registerAndLogin(t, e)

	role := createRoleFor(t, models.RolePermissions{ManageServer: true})
	assignRoleToUser(t, modUserID, role.ID)
	emoji := newEmojiRoute(t, e, token)

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
	createServerFor(t, userID)
	emoji := newEmojiRoute(t, e, token)

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
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	unique := "w" + randHex(8)
	msg, err := storage.CreateMessage(context.Background(), channel.ID, userID, "mensagem "+unique, "", nil)
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
	if r.Type != "message" || r.ID != msg.ID || r.ChannelID != channel.ID {
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

// migrationsDir é o caminho relativo ao diretório deste pacote (backend/internal/handlers/test_handlers).
const migrationsDir = "../../../migrations"

// defaultDatabaseURL corresponde aos padrões do infra/docker-compose.yml.
const defaultDatabaseURL = "postgres://papo:papo123@localhost:5432/papo"

// testBaseURL é a base usada para montar o campo "type" dos erros RFC 7807.
const testBaseURL = "https://papo.com/"

func TestMain(m *testing.M) {
	os.Exit(runHandlersTests(m))
}

// exclui todo o estado de servidores/canais/roles/emojis nos testes para
// manter a regra de negócio de 1 servidor por backend (a ordem segue as FKs;
// usuários, user_settings e media não são removidos)
func cleanServers(ctx context.Context) error {
	for _, table := range []string{
		"user_roles",
		"roles",
		"attachment_thumbnails",
		"attachments",
		"message_reactions",
		"messages",
		"user_channel_state",
		"channels",
		"emojis",
		"servers",
	} {
		if _, err := storage.GetDB().ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return err
		}
	}

	return nil
}

// runHandlersTests prepara um banco temporário com as migrations do projeto,
// inicializa o storage contra ele, executa os testes e remove o banco ao final.
func runHandlersTests(m *testing.M) int {
	// blobs gravados pelos testes no content-addressable storage
	_ = os.RemoveAll("media")

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

// newTestMediaHash grava o conteúdo no content-addressable storage (tabela
// media + arquivo em disco) e retorna o sha256 em hex. Os services resolvem
// blobs a partir do disco, então emojis/ícones criados via storage precisam
// do arquivo gravado.
func newTestMediaHash(t *testing.T, content []byte) string {
	t.Helper()
	hash, _, err := services.StoreMediaFromBytes(testCtx(), content, "image/png")
	if err != nil {
		t.Fatalf("falha ao gravar mídia de apoio: %v", err)
	}
	return hash
}

// mediaPathFor deriva o caminho do blob no content-addressable storage a
// partir do sha256 em hex (media/<2>/<2>/<sha>).
func mediaPathFor(sha string) string {
	return filepath.Join("media", sha[:2], sha[2:4], sha)
}

// problem é o corpo de erro RFC 7807 retornado pelos
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

	if err := HealthHandler(c); err != nil {
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

	if err := RegisterHandler(testBaseURL, c); err != nil {
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

	if err := RegisterHandler(testBaseURL, c); err != nil {
		t.Fatalf("RegisterHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestRegisterHandlerMissingUsername(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"password": "senha123"})
	c := newContext(t, http.MethodPost, "/auth/register", body, newRandomIP())
	rec := recorder(c)

	if err := RegisterHandler(testBaseURL, c); err != nil {
		t.Fatalf("RegisterHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "campo 'username' é obrigatório")
}

func TestRegisterHandlerMissingPassword(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"username": "user123"})
	c := newContext(t, http.MethodPost, "/auth/register", body, newRandomIP())
	rec := recorder(c)

	if err := RegisterHandler(testBaseURL, c); err != nil {
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

	if err := RegisterHandler(testBaseURL, c); err != nil {
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

	if err := RegisterHandler(testBaseURL, c); err != nil {
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
	if err := RegisterHandler(testBaseURL, c); err != nil {
		t.Fatalf("falha no primeiro registro: %v", err)
	}
	if rec := recorder(c); rec.Code != http.StatusCreated {
		t.Fatalf("esperava 201 no primeiro registro, obtive %d", rec.Code)
	}

	// segundo registro com o mesmo username resulta em conflito
	c = newContext(t, http.MethodPost, "/auth/register", body, newRandomIP())
	rec := recorder(c)
	if err := RegisterHandler(testBaseURL, c); err != nil {
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

	if err := RegisterHandler(testBaseURL, c); err != nil {
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

	if err := LoginHandler(testBaseURL, c); err != nil {
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

	if err := LoginHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestLoginHandlerMissingUsername(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"password": "senha123"})
	c := newContext(t, http.MethodPost, "/auth/login", body, newRandomIP())
	rec := recorder(c)

	if err := LoginHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "campo 'username' é obrigatório")
}

func TestLoginHandlerMissingPassword(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"username": "user123"})
	c := newContext(t, http.MethodPost, "/auth/login", body, newRandomIP())
	rec := recorder(c)

	if err := LoginHandler(testBaseURL, c); err != nil {
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

	if err := LoginHandler(testBaseURL, c); err != nil {
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

	if err := LoginHandler(testBaseURL, c); err != nil {
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
	if err := RegisterHandler(testBaseURL, c); err != nil {
		t.Fatalf("falha ao registrar usuário: %v", err)
	}
	if rec := recorder(c); rec.Code != http.StatusCreated {
		t.Fatalf("esperava 201 no registro, obtive %d", rec.Code)
	}

	// login com senha incorreta
	body, _ = json.Marshal(map[string]string{"username": username, "password": "senha_errada_" + randHex(4)})
	c = newContext(t, http.MethodPost, "/auth/login", body, ip)
	rec := recorder(c)

	if err := LoginHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "invalid-credentials", "Credenciais inválidas", "username ou senha incorretos")
}

func TestLoginHandlerNonexistentUser(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"username": newRandomUsername(), "password": newRandomPassword()})
	c := newContext(t, http.MethodPost, "/auth/login", body, newRandomIP())
	rec := recorder(c)

	if err := LoginHandler(testBaseURL, c); err != nil {
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

	if err := LoginHandler(testBaseURL, c); err != nil {
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

	if err := LoginHandler(testBaseURL, c); err != nil {
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

	if err := WhoamiHandler(testBaseURL, c); err != nil {
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

	if err := WhoamiHandler(testBaseURL, c); err != nil {
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

	if err := WhoamiHandler(testBaseURL, c); err != nil {
		t.Fatalf("WhoamiHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

func TestWhoamiHandlerMissingUserID(t *testing.T) {
	c := newContext(t, http.MethodGet, "/auth/whoami", nil, "")
	rec := recorder(c)

	if err := WhoamiHandler(testBaseURL, c); err != nil {
		t.Fatalf("WhoamiHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

// --- LogoutHandler ---

func TestLogoutHandler(t *testing.T) {
	c := newContext(t, http.MethodPost, "/auth/logout", nil, "")
	rec := recorder(c)

	if err := LogoutHandler(c); err != nil {
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

	c := newContext(t, http.MethodGet, "/users/"+user.ID+"/profile", nil, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := ProfileHandler(testBaseURL, c); err != nil {
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
	description := "sobre mim"
	avatar := []byte{0x89, 0x50, 0x4e, 0x47}
	updatedAt := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := storage.UpdateUser(testCtx(), user.ID, models.User{
		Nickname:        &nickname,
		AvatarBlob:      avatar,
		AvatarFormat:    "PNG",
		Description:     &description,
		StatusMessage:   &status,
		StatusUpdatedAt: &updatedAt,
	}); err != nil {
		t.Fatalf("falha ao atualizar perfil: %v", err)
	}

	bannerHash, _, err := services.StoreMediaFromBytes(testCtx(), pngAvatarBytes(1024, 256), "image/png")
	if err != nil {
		t.Fatalf("falha ao gravar media de apoio: %v", err)
	}
	if err := storage.UpdateUserBanner(testCtx(), &bannerHash, user.ID); err != nil {
		t.Fatalf("falha ao atualizar banner: %v", err)
	}

	c := newContext(t, http.MethodGet, "/users/"+user.ID+"/profile", nil, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := ProfileHandler(testBaseURL, c); err != nil {
		t.Fatalf("ProfileHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID              string     `json:"id"`
		Username        string     `json:"username"`
		Nickname        *string    `json:"nickname"`
		BannerMedia     *string    `json:"banner_media"`
		Description     *string    `json:"description"`
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
	if resp.BannerMedia == nil || *resp.BannerMedia != bannerHash {
		t.Errorf("esperava banner_media %q, obtive %v", bannerHash, resp.BannerMedia)
	}
	if resp.Description == nil || *resp.Description != description {
		t.Errorf("esperava description %q, obtive %v", description, resp.Description)
	}
	if resp.StatusMessage == nil || *resp.StatusMessage != status {
		t.Errorf("esperava status_message %q, obtive %v", status, resp.StatusMessage)
	}
	if resp.StatusUpdatedAt == nil || !resp.StatusUpdatedAt.Equal(updatedAt) {
		t.Errorf("esperava status_updated_at %v, obtive %v", updatedAt, resp.StatusUpdatedAt)
	}
}

func TestProfileHandlerUserNotFound(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	randomId := randUUID()

	c := newContext(t, http.MethodGet, "/users/"+randomId+"/profile", nil, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(randomId)
	rec := recorder(c)

	if err := ProfileHandler(testBaseURL, c); err != nil {
		t.Fatalf("ProfileHandler retornou erro: %v", err)
	}

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado",
		"usuário não encontrado")
}

func TestProfileHandlerMissingUserAuth(t *testing.T) {
	randomId := randUUID()
	c := newContext(t, http.MethodGet, "/users/"+randomId+"/profile", nil, "")
	rec := recorder(c)

	if err := ProfileHandler(testBaseURL, c); err != nil {
		t.Fatalf("ProfileHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

// --- ProfileBatchHandler ---

func TestProfileBatchHandlerSuccess(t *testing.T) {
	u1, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	u2, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	body, _ := json.Marshal(map[string][]string{"ids": {u2.ID, u1.ID}})
	c := newContext(t, http.MethodPost, "/users/profile_batch", body, "")
	c.Set(middleware.UserIDContextKey, u1.ID)
	rec := recorder(c)

	if err := ProfileBatchHandler(testBaseURL, c); err != nil {
		t.Fatalf("ProfileBatchHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Profiles []struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if len(resp.Profiles) != 2 {
		t.Fatalf("esperava 2 perfis, obtive %d", len(resp.Profiles))
	}
	if resp.Profiles[0].ID != u2.ID || resp.Profiles[0].Username != u2.Username {
		t.Errorf("primeiro perfil não confere (ordem da requisição): got %+v", resp.Profiles[0])
	}
	if resp.Profiles[1].ID != u1.ID || resp.Profiles[1].Username != u1.Username {
		t.Errorf("segundo perfil não confere (ordem da requisição): got %+v", resp.Profiles[1])
	}
}

func TestProfileBatchHandlerSkipsMissingAndDedupes(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	body, _ := json.Marshal(map[string][]string{"ids": {user.ID, randUUID(), user.ID}})
	c := newContext(t, http.MethodPost, "/users/profile_batch", body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	rec := recorder(c)

	if err := ProfileBatchHandler(testBaseURL, c); err != nil {
		t.Fatalf("ProfileBatchHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Profiles []struct {
			ID string `json:"id"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if len(resp.Profiles) != 1 || resp.Profiles[0].ID != user.ID {
		t.Errorf("esperava 1 perfil do usuário %s, obtive %+v", user.ID, resp.Profiles)
	}
}

func TestProfileBatchHandlerEmptyIDs(t *testing.T) {
	t.Run("chave ausente", func(t *testing.T) {
		c := newContext(t, http.MethodPost, "/users/profile_batch", []byte("{}"), "")
		c.Set(middleware.UserIDContextKey, randUUID())
		rec := recorder(c)

		if err := ProfileBatchHandler(testBaseURL, c); err != nil {
			t.Fatalf("ProfileBatchHandler retornou erro: %v", err)
		}
		assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
			"campo 'ids' é obrigatório")
	})

	t.Run("lista vazia", func(t *testing.T) {
		body, _ := json.Marshal(map[string][]string{"ids": {}})
		c := newContext(t, http.MethodPost, "/users/profile_batch", body, "")
		c.Set(middleware.UserIDContextKey, randUUID())
		rec := recorder(c)

		if err := ProfileBatchHandler(testBaseURL, c); err != nil {
			t.Fatalf("ProfileBatchHandler retornou erro: %v", err)
		}
		assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
			"campo 'ids' é obrigatório")
	})
}

func TestProfileBatchHandlerEmptyIDEntry(t *testing.T) {
	body, _ := json.Marshal(map[string][]string{"ids": {""}})
	c := newContext(t, http.MethodPost, "/users/profile_batch", body, "")
	c.Set(middleware.UserIDContextKey, randUUID())
	rec := recorder(c)

	if err := ProfileBatchHandler(testBaseURL, c); err != nil {
		t.Fatalf("ProfileBatchHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"ids não podem ser vazios")
}

func TestProfileBatchHandlerTooManyIDs(t *testing.T) {
	ids := make([]string, 0, 51)
	for i := 0; i < 51; i++ {
		ids = append(ids, randUUID())
	}
	body, _ := json.Marshal(map[string][]string{"ids": ids})
	c := newContext(t, http.MethodPost, "/users/profile_batch", body, "")
	c.Set(middleware.UserIDContextKey, randUUID())
	rec := recorder(c)

	if err := ProfileBatchHandler(testBaseURL, c); err != nil {
		t.Fatalf("ProfileBatchHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"máximo de 50 ids por requisição")
}

func TestProfileBatchHandlerMissingUserAuth(t *testing.T) {
	body, _ := json.Marshal(map[string][]string{"ids": {randUUID()}})
	c := newContext(t, http.MethodPost, "/users/profile_batch", body, "")
	rec := recorder(c)

	if err := ProfileBatchHandler(testBaseURL, c); err != nil {
		t.Fatalf("ProfileBatchHandler retornou erro: %v", err)
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
	description := "sobre mim"
	body, _ := json.Marshal(map[string]string{"nickname": nickname, "status": status, "description": description})
	c := newContext(t, http.MethodPut, "/users/"+user.ID, body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := UpdateUserHandler(testBaseURL, c); err != nil {
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
	if stored.Description == nil || *stored.Description != description {
		t.Errorf("esperava description %q, obtive %v", description, stored.Description)
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

	if err := UpdateUserHandler(testBaseURL, c); err != nil {
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

	if err := UpdateUserHandler(testBaseURL, c); err != nil {
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

	if err := UpdateUserHandler(testBaseURL, c); err != nil {
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

	if err := UpdateUserHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateUserHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado", "não é possível atualizar o perfil de outro usuário")
}

func TestUpdateUserHandlerMissingDescription(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"nickname": "nick", "status": "disponível"})
	c := newContext(t, http.MethodPut, "/users/"+user.ID, body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := UpdateUserHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateUserHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "campo 'description' é obrigatório")
}

func TestUpdateUserHandlerUserNotFound(t *testing.T) {
	nonexistentID := randUUID()
	body, _ := json.Marshal(map[string]string{"nickname": "nick", "status": "disponível", "description": "sobre mim"})
	c := newContext(t, http.MethodPut, "/users/"+nonexistentID, body, "")
	c.Set(middleware.UserIDContextKey, nonexistentID)
	c.SetParamNames("user_id")
	c.SetParamValues(nonexistentID)
	rec := recorder(c)

	if err := UpdateUserHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateUserHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "usuário não encontrado")
}

func TestUpdateUserHandlerMissingUserID(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"nickname": "nick", "status": "disponível"})
	c := newContext(t, http.MethodPut, "/users/some-id", body, "")
	rec := recorder(c)

	if err := UpdateUserHandler(testBaseURL, c); err != nil {
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

	if err := UpdateAvatarHandler(testBaseURL, c); err != nil {
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

	// o avatar deve ter sido persistido (media) e resolvido pelo service
	stored, err := storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if stored.AvatarMedia == nil {
		t.Fatal("esperava avatar_media persistida")
	}
	profile, err := services.Profile(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("Profile retornou erro: %v", err)
	}
	if profile.AvatarFormat != "PNG" {
		t.Errorf("esperava avatar_format %q, obtive %q", "PNG", profile.AvatarFormat)
	}
	if !bytes.Equal(profile.AvatarBlob, pngAvatarBytes(100, 100)) {
		t.Errorf("avatar_blob não confere:\n got  %x\n want %x", profile.AvatarBlob, pngAvatarBytes(100, 100))
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

	if err := UpdateAvatarHandler(testBaseURL, c); err != nil {
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

			if err := UpdateAvatarHandler(testBaseURL, c); err != nil {
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

	if err := UpdateAvatarHandler(testBaseURL, c); err != nil {
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

	if err := UpdateAvatarHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateAvatarHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "usuário não encontrado")
}

func TestUpdateAvatarHandlerMissingUserID(t *testing.T) {
	avatar := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	body, _ := json.Marshal(map[string]string{"avatar": avatar, "avatar_format": "PNG"})
	c := newContext(t, http.MethodPut, "/users/some-id/avatar", body, "")
	rec := recorder(c)

	if err := UpdateAvatarHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateAvatarHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

// --- UpdateBannerHandler ---

func TestUpdateBannerHandlerSuccess(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	banner := base64.StdEncoding.EncodeToString(pngAvatarBytes(1024, 256))
	body, _ := json.Marshal(map[string]string{"banner": banner, "banner_format": "PNG"})
	c := newContext(t, http.MethodPut, "/users/"+user.ID+"/banner", body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := UpdateBannerHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateBannerHandler retornou erro: %v", err)
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
	if resp.Response != "User banner updated successfully" {
		t.Errorf("esperava response %q, obtive %q", "User banner updated successfully", resp.Response)
	}

	// o banner deve ter sido persistido como referência de media
	stored, err := storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if stored.BannerMedia == nil {
		t.Fatal("esperava banner_media persistida")
	}

	media, err := storage.GetMediaByHash(testCtx(), *stored.BannerMedia)
	if err != nil {
		t.Fatalf("GetMediaByHash retornou erro: %v", err)
	}
	if media.MimeType != "image/png" {
		t.Errorf("esperava mime %q, obtive %q", "image/png", media.MimeType)
	}
	blob, err := os.ReadFile(mediaPathFor(*stored.BannerMedia))
	if err != nil {
		t.Fatalf("falha ao ler o blob do banner: %v", err)
	}
	if !bytes.Equal(blob, pngAvatarBytes(1024, 256)) {
		t.Errorf("blob do banner não confere:\n got  %x\n want %x", blob, pngAvatarBytes(1024, 256))
	}
}

func TestUpdateBannerHandlerInvalidJSON(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	c := newContext(t, http.MethodPut, "/users/"+user.ID+"/banner", []byte("{invalido"), "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := UpdateBannerHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateBannerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestUpdateBannerHandlerInvalidBanner(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
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
		{"conteúdo não corresponde ao formato", base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100)), "GIF"},
		{"dimensão acima de 1024px", base64.StdEncoding.EncodeToString(pngAvatarBytes(1025, 100)), "PNG"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"banner": tc.banner, "banner_format": tc.bannerFormat})
			c := newContext(t, http.MethodPut, "/users/"+user.ID+"/banner", body, "")
			c.Set(middleware.UserIDContextKey, user.ID)
			c.SetParamNames("user_id")
			c.SetParamValues(user.ID)
			rec := recorder(c)

			if err := UpdateBannerHandler(testBaseURL, c); err != nil {
				t.Fatalf("UpdateBannerHandler retornou erro: %v", err)
			}
			assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
				"banner inválido: deve ser base64 de um GIF, JPEG ou PNG de até 2MB")
		})
	}
}

func TestUpdateBannerHandlerEmptyRemovesBanner(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	banner := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	body, _ := json.Marshal(map[string]string{"banner": banner, "banner_format": "PNG"})
	c := newContext(t, http.MethodPut, "/users/"+user.ID+"/banner", body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := UpdateBannerHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateBannerHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	stored, err := storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if stored.BannerMedia == nil {
		t.Fatal("esperava banner_media persistida")
	}

	body, _ = json.Marshal(map[string]string{"banner": "", "banner_format": ""})
	c = newContext(t, http.MethodPut, "/users/"+user.ID+"/banner", body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec = recorder(c)

	if err := UpdateBannerHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateBannerHandler(remove) retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	stored, err = storage.GetUserByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if stored.BannerMedia != nil {
		t.Errorf("esperava banner_media removida, obtive %v", stored.BannerMedia)
	}
}

func TestUpdateBannerHandlerForbiddenOtherUser(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	other, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar segundo usuário: %v", err)
	}

	banner := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	body, _ := json.Marshal(map[string]string{"banner": banner, "banner_format": "PNG"})
	c := newContext(t, http.MethodPut, "/users/"+other.ID+"/banner", body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(other.ID)
	rec := recorder(c)

	if err := UpdateBannerHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateBannerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado", "não é possível atualizar o banner de outro usuário")
}

func TestUpdateBannerHandlerUserNotFound(t *testing.T) {
	nonexistentID := randUUID()
	banner := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	body, _ := json.Marshal(map[string]string{"banner": banner, "banner_format": "PNG"})
	c := newContext(t, http.MethodPut, "/users/"+nonexistentID+"/banner", body, "")
	c.Set(middleware.UserIDContextKey, nonexistentID)
	c.SetParamNames("user_id")
	c.SetParamValues(nonexistentID)
	rec := recorder(c)

	if err := UpdateBannerHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateBannerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "usuário não encontrado")
}

func TestUpdateBannerHandlerMissingUserID(t *testing.T) {
	banner := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	body, _ := json.Marshal(map[string]string{"banner": banner, "banner_format": "PNG"})
	c := newContext(t, http.MethodPut, "/users/some-id/banner", body, "")
	rec := recorder(c)

	if err := UpdateBannerHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateBannerHandler retornou erro: %v", err)
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

	if err := UpdateSettingsHandler(testBaseURL, c); err != nil {
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

	if err := UpdateSettingsHandler(testBaseURL, c); err != nil {
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

			if err := UpdateSettingsHandler(testBaseURL, c); err != nil {
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

	if err := UpdateSettingsHandler(testBaseURL, c); err != nil {
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

	if err := UpdateSettingsHandler(testBaseURL, c); err != nil {
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

	if err := ResetUserHandler(testBaseURL, c); err != nil {
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

	if err := ResetUserHandler(testBaseURL, c); err != nil {
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

	if err := ResetUserHandler(testBaseURL, c); err != nil {
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

	if err := ResetUserHandler(testBaseURL, c); err != nil {
		t.Fatalf("ResetUserHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "usuário não encontrado")
}

// --- ListUsersHandler (tarefa 6.4) ---

func TestListUsersHandler(t *testing.T) {
	e := newApp()
	cleanServers(testCtx())
	var authUser struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	// A listagem é paginada (máx. 100 por created_at, id); since limita a
	// resposta aos usuários criados neste teste, independente dos criados
	// por outros testes.
	since := time.Now()
	bd, _ := json.Marshal(map[string]string{"username": newRandomUsername(), "password": newRandomPassword()})
	rec1 := do(t, e, http.MethodPost, "/auth/register", bd, nil)

	if rec1.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec1.Code, rec1.Body.String())
	}

	if err := json.Unmarshal(rec1.Body.Bytes(), &authUser); err != nil {
		t.Fatalf("register: falha ao decodificar resposta: %v", err)
	}

	var other struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}

	bd2, _ := json.Marshal(map[string]string{"username": newRandomUsername(), "password": newRandomPassword()})
	rec2 := do(t, e, http.MethodPost, "/auth/register", bd2, nil)

	if rec2.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec2.Code, rec2.Body.String())
	}

	if err := json.Unmarshal(rec2.Body.Bytes(), &other); err != nil {
		t.Fatalf("register: falha ao decodificar resposta: %v", err)
	}

	c := newContext(t, http.MethodGet, "/users?since="+url.QueryEscape(since.Format(time.RFC3339Nano)), nil, "")
	c.Set(middleware.UserIDContextKey, authUser.ID)
	rec := recorder(c)

	if err := ListUsersHandler(testBaseURL, c); err != nil {
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

	if err := ListUsersHandler(testBaseURL, c); err != nil {
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

	if err := ChangePasswordHandler(testBaseURL, c); err != nil {
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

	if err := ChangePasswordHandler(testBaseURL, c); err != nil {
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

	if err := ChangePasswordHandler(testBaseURL, c); err != nil {
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

	if err := ChangePasswordHandler(testBaseURL, c); err != nil {
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

	if err := ChangePasswordHandler(testBaseURL, c); err != nil {
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

	if err := ChangePasswordHandler(testBaseURL, c); err != nil {
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

	if err := ChangePasswordHandler(testBaseURL, c); err != nil {
		t.Fatalf("ChangePasswordHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"não é possível alterar a senha de outro usuário")
}

// --- GetServerHandler (GET /server e GET /server)

func TestGetServersHandlerNotFound(t *testing.T) {
	cleanServers(testCtx())
	c := newContext(t, http.MethodGet, "/server", nil, "")
	rec := recorder(c)

	if err := GetServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("GetServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "servidor não encontrado")
}

func TestGetServersHandlerSuccess(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	serverName := "srv_" + randHex(4)
	server, err := storage.CreateServer(testCtx(), serverName, &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	if _, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", ""); err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	c := newContext(t, http.MethodGet, "/server", nil, "")
	rec := recorder(c)

	if err := GetServerHandler(testBaseURL, c); err != nil {
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
		ChannelCount  int       `json:"channel_count"`
		MemberCount   int       `json:"member_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.ID != server.ID {
		t.Errorf("esperava id %s, obtive %s", server.ID, resp.ID)
	}
	if resp.Name != serverName {
		t.Errorf("esperava name %q, obtive %q", serverName, resp.Name)
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
	if resp.RoleCount != 0 {
		t.Errorf("esperava role_count 0, obtive %d", resp.RoleCount)
	}
	if resp.ChannelCount != 1 {
		t.Errorf("esperava channel_count 1, obtive %d", resp.ChannelCount)
	}
	if resp.MemberCount < 1 {
		t.Errorf("esperava member_count >= 1, obtive %d", resp.MemberCount)
	}
}

// --- GetServerHandler

func TestGetServerHandlerSuccess(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	if _, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", ""); err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	if _, err := storage.CreateRole(testCtx(), "role_"+randHex(4), nil, models.RolePermissions{}); err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	c := newContext(t, http.MethodGet, "/server", nil, "")
	rec := recorder(c)

	if err := GetServerHandler(testBaseURL, c); err != nil {
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
	cleanServers(testCtx())
	c := newContext(t, http.MethodGet, "/server", nil, "")
	rec := recorder(c)

	if err := GetServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("GetServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "servidor não encontrado")
}

// --- UpdateServerHandler ---

func TestUpdateServerHandlerSuccess(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário dono: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}

	newName := "srv_" + randHex(4)
	body, _ := json.Marshal(map[string]string{
		"name":        newName,
		"icon_blob":   base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100)),
		"icon_format": "png",
	})
	c := newContext(t, http.MethodPut, "/server", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := UpdateServerHandler(testBaseURL, c); err != nil {
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

	// a atualização deve ter sido persistida
	stored, err := storage.GetServer(testCtx())
	if err != nil {
		t.Fatalf("GetServer retornou erro: %v", err)
	}
	if stored.Name != newName {
		t.Errorf("esperava name %q persistido, obtive %q", newName, stored.Name)
	}
	if stored.IconMedia == nil {
		t.Fatal("esperava icon_media persistida")
	}
	summary, err := services.GetServer(testCtx())
	if err != nil {
		t.Fatalf("GetServer retornou erro: %v", err)
	}
	if !bytes.Equal(summary.IconBlob, pngAvatarBytes(100, 100)) {
		t.Errorf("icon_blob persistido não confere: %x", summary.IconBlob)
	}
	if summary.IconFormat != "PNG" {
		t.Errorf("esperava icon_format %q persistido, obtive %q", "PNG", summary.IconFormat)
	}
}

func TestUpdateServerHandlerNotFound(t *testing.T) {
	cleanServers(testCtx())
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"name": "srv_" + randHex(4)})
	c := newContext(t, http.MethodPut, "/server", body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	rec := recorder(c)

	if err := UpdateServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "servidor não encontrado")
}

func TestUpdateServerHandlerInvalidJSON(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário dono: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)

	c := newContext(t, http.MethodPut, "/server", []byte("{invalido"), "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := UpdateServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestUpdateServerHandlerInvalidInput(t *testing.T) {
	cleanServers(testCtx())
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
			c := newContext(t, http.MethodPut, "/server", body, "")
			c.Set(middleware.UserIDContextKey, owner.ID)
			rec := recorder(c)

			if err := UpdateServerHandler(testBaseURL, c); err != nil {
				t.Fatalf("UpdateServerHandler retornou erro: %v", err)
			}
			assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
				"name é obrigatório e deve ter no máximo 32 caracteres; icon_blob deve ser base64 de um GIF, JPEG ou PNG de até 2MB servidor privado (public=false) exige password")
		})
	}

	// tentativas inválidas não devem alterar o servidor
	stored, err := storage.GetServer(testCtx())
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
	c := newContext(t, http.MethodPost, "/server", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := CreateServerHandler(testBaseURL, c); err != nil {
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
	stored, err := storage.GetServer(testCtx())
	if err != nil {
		t.Fatalf("GetServerByID retornou erro: %v", err)
	}
	if stored.Name != name {
		t.Errorf("esperava name %q persistido, obtive %q", name, stored.Name)
	}
	if stored.OwnerID == nil || *stored.OwnerID != owner.ID {
		t.Errorf("esperava owner_id %s persistido, obtive %v", owner.ID, stored.OwnerID)
	}
	if stored.IconMedia == nil {
		t.Fatal("esperava icon_media persistida")
	}
	summary, err := services.GetServer(testCtx())
	if err != nil {
		t.Fatalf("GetServer retornou erro: %v", err)
	}
	if !bytes.Equal(summary.IconBlob, pngAvatarBytes(100, 100)) {
		t.Errorf("icon_blob persistido não confere: %x", summary.IconBlob)
	}
}

func TestCreateServerHandlerNoIcon(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"name": "srv_" + randHex(4)})
	c := newContext(t, http.MethodPost, "/server", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := CreateServerHandler(testBaseURL, c); err != nil {
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
	c := newContext(t, http.MethodPost, "/server", body, "")
	rec := recorder(c)

	if err := CreateServerHandler(testBaseURL, c); err != nil {
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

	c := newContext(t, http.MethodPost, "/server", []byte("{invalido"), "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := CreateServerHandler(testBaseURL, c); err != nil {
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
			c := newContext(t, http.MethodPost, "/server", body, "")
			c.Set(middleware.UserIDContextKey, owner.ID)
			rec := recorder(c)

			if err := CreateServerHandler(testBaseURL, c); err != nil {
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
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)

	c := newContext(t, http.MethodGet, "/channels", nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := ListChannelsHandler(testBaseURL, c); err != nil {
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
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	channelA, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	channelB, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	c := newContext(t, http.MethodGet, "/channels", nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := ListChannelsHandler(testBaseURL, c); err != nil {
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

// --- CreateChannelHandler (tarefa 5.4) ---

func TestCreateChannelHandlerSuccess(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	channelName := "canal_" + randHex(4)

	body, _ := json.Marshal(map[string]string{"name": channelName})
	c := newContext(t, http.MethodPost, "/channels", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := CreateChannelHandler(testBaseURL, c); err != nil {
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
	c.Set(middleware.UserIDContextKey, randUUID())
	rec := recorder(c)

	if err := CreateChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestCreateChannelHandlerMissingName(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)

	c := newContext(t, http.MethodPost, "/channels", nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := CreateChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"name é obrigatório e deve ter no máximo 32 caracteres; type deve ser 'text' ou 'category'; topic tem no máximo 512 caracteres e é válido apenas para canais de texto")
}

func TestCreateChannelHandlerNameTaken(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	channelName := "canal_" + randHex(4)
	if _, err := storage.CreateChannel(testCtx(), channelName, "text", ""); err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	// o nome é UNIQUE global na tabela channels
	body, _ := json.Marshal(map[string]string{"name": channelName})
	c := newContext(t, http.MethodPost, "/channels", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := CreateChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusConflict, "channel-name-taken", "Nome de canal já existe", "o nome informado já está em uso")
}

// --- UpdateChannelHandler (tarefa 5.4) ---

func TestUpdateChannelHandlerSuccess(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	channel, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	newName := "canal_" + randHex(4)

	body, _ := json.Marshal(map[string]string{"name": newName})
	c := newContext(t, http.MethodPut, "/channels/"+channel.ID, body, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := UpdateChannelHandler(testBaseURL, c); err != nil {
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
	c.Set(middleware.UserIDContextKey, randUUID())
	rec := recorder(c)

	if err := UpdateChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "channel_id ausente")
}

func TestUpdateChannelHandlerInvalidJSON(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	channel, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	c := newContext(t, http.MethodPut, "/channels/"+channel.ID, []byte("{invalido"), "")
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := UpdateChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestUpdateChannelHandlerMissingName(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	channel, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"name": ""})
	c := newContext(t, http.MethodPut, "/channels/"+channel.ID, body, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := UpdateChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "name é obrigatório e deve ter no máximo 32 caracteres; topic tem no máximo 512 caracteres e é válido apenas para canais de texto")
}

func TestUpdateChannelHandlerNotFound(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"name": "canal_" + randHex(4)})
	c := newContext(t, http.MethodPut, "/channels/"+randUUID(), body, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(randUUID())
	c.Set(middleware.UserIDContextKey, randUUID())
	rec := recorder(c)

	if err := UpdateChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

func TestUpdateChannelHandlerNameTaken(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	channel, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	takenName := "canal_" + randHex(4)
	if _, err := storage.CreateChannel(testCtx(), takenName, "text", ""); err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"name": takenName})
	c := newContext(t, http.MethodPut, "/channels/"+channel.ID, body, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := UpdateChannelHandler(testBaseURL, c); err != nil {
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
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	channel, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	c := newContext(t, http.MethodDelete, "/channels/"+channel.ID, nil, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := DeleteChannelHandler(testBaseURL, c); err != nil {
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
	c.Set(middleware.UserIDContextKey, randUUID())
	rec := recorder(c)

	if err := DeleteChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "channel_id ausente")
}

func TestDeleteChannelHandlerNotFound(t *testing.T) {
	c := newContext(t, http.MethodDelete, "/channels/"+randUUID(), nil, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(randUUID())
	c.Set(middleware.UserIDContextKey, randUUID())
	rec := recorder(c)

	if err := DeleteChannelHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteChannelHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

// --- GetChannelPermissionsHandler (tarefa 5.4) ---

func TestGetChannelPermissionsHandlerEmpty(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	channel, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	c := newContext(t, http.MethodGet, "/channels/"+channel.ID+"/permissions", nil, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	rec := recorder(c)

	if err := GetChannelPermissionsHandler(testBaseURL, c); err != nil {
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
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	channel, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	roleName := "role_" + randHex(4)
	role, err := storage.CreateRole(testCtx(), roleName, nil, models.RolePermissions{})
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

	if err := GetChannelPermissionsHandler(testBaseURL, c); err != nil {
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

	if err := GetChannelPermissionsHandler(testBaseURL, c); err != nil {
		t.Fatalf("GetChannelPermissionsHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

func TestGetChannelPermissionsHandlerMissingParam(t *testing.T) {
	c := newContext(t, http.MethodGet, "/channels//permissions", nil, "")
	c.SetParamNames("channel_id")
	c.SetParamValues("")
	rec := recorder(c)

	if err := GetChannelPermissionsHandler(testBaseURL, c); err != nil {
		t.Fatalf("GetChannelPermissionsHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "channel_id ausente")
}

// --- UpdateChannelPermissionsHandler (tarefa 5.4) ---

func TestUpdateChannelPermissionsHandlerSuccess(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	channel, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	permissions := models.ChannelPermission{ReadChannel: true, SendMessages: true, DeleteMessages: false}

	body, _ := json.Marshal(map[string]models.ChannelPermission{"permissions": permissions})
	c := newContext(t, http.MethodPut, "/channels/"+channel.ID+"/permissions/"+role.ID, body, "")
	c.SetParamNames("channel_id", "role_id")
	c.SetParamValues(channel.ID, role.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := UpdateChannelPermissionsHandler(testBaseURL, c); err != nil {
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
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	body, _ := json.Marshal(map[string]models.ChannelPermission{"permissions": {ReadChannel: true}})
	c := newContext(t, http.MethodPut, "/channels//permissions/"+role.ID, body, "")
	c.SetParamNames("channel_id", "role_id")
	c.SetParamValues("", role.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := UpdateChannelPermissionsHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelPermissionsHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "channel_id ausente")
}

func TestUpdateChannelPermissionsHandlerMissingRoleID(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	channel, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	body, _ := json.Marshal(map[string]models.ChannelPermission{"permissions": {ReadChannel: true}})
	c := newContext(t, http.MethodPut, "/channels/"+channel.ID+"/permissions/", body, "")
	c.SetParamNames("channel_id", "role_id")
	c.SetParamValues(channel.ID, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := UpdateChannelPermissionsHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelPermissionsHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "role_id ausente")
}

func TestUpdateChannelPermissionsHandlerInvalidJSON(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	channel, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	c := newContext(t, http.MethodPut, "/channels/"+channel.ID+"/permissions/"+role.ID, []byte("{invalido"), "")
	c.SetParamNames("channel_id", "role_id")
	c.SetParamValues(channel.ID, role.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := UpdateChannelPermissionsHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelPermissionsHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestUpdateChannelPermissionsHandlerChannelNotFound(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	channelID := randUUID()

	body, _ := json.Marshal(map[string]models.ChannelPermission{"permissions": {ReadChannel: true}})
	c := newContext(t, http.MethodPut, "/channels/"+channelID+"/permissions/"+role.ID, body, "")
	c.SetParamNames("channel_id", "role_id")
	c.SetParamValues(channelID, role.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := UpdateChannelPermissionsHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelPermissionsHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

func TestUpdateChannelPermissionsHandlerRoleNotFound(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	channel, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	roleID := randUUID()

	body, _ := json.Marshal(map[string]models.ChannelPermission{"permissions": {ReadChannel: true}})
	c := newContext(t, http.MethodPut, "/channels/"+channel.ID+"/permissions/"+roleID, body, "")
	c.SetParamNames("channel_id", "role_id")
	c.SetParamValues(channel.ID, roleID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := UpdateChannelPermissionsHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateChannelPermissionsHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "role não encontrada")
}

// --- ChangeChannelPositionHandler (tarefa 8.4) ---

func TestChangeChannelPositionHandlerSuccess(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	c1, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar primeiro canal: %v", err)
	}
	c2, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar segundo canal: %v", err)
	}
	c3, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar terceiro canal: %v", err)
	}

	body, _ := json.Marshal(map[string]int{"old_position": 1, "new_position": 3})
	c := newContext(t, http.MethodPut, "/channels/"+c1.ID+"/change_position", body, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(c1.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := ChangeChannelPositionHandler(testBaseURL, c); err != nil {
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

	channels, err := storage.ListChannels(testCtx())
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
	c.Set(middleware.UserIDContextKey, randUUID())
	rec := recorder(c)

	if err := ChangeChannelPositionHandler(testBaseURL, c); err != nil {
		t.Fatalf("ChangeChannelPositionHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "channel_id ausente")
}

func TestChangeChannelPositionHandlerInvalidJSON(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	channel, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	c := newContext(t, http.MethodPut, "/channels/"+channel.ID+"/change_position", []byte("{invalido"), "")
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := ChangeChannelPositionHandler(testBaseURL, c); err != nil {
		t.Fatalf("ChangeChannelPositionHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestChangeChannelPositionHandlerInvalidPosition(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	channel, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}

	body, _ := json.Marshal(map[string]int{"old_position": 1, "new_position": 2})
	c := newContext(t, http.MethodPut, "/channels/"+channel.ID+"/change_position", body, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := ChangeChannelPositionHandler(testBaseURL, c); err != nil {
		t.Fatalf("ChangeChannelPositionHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"old_position e new_position devem ser posições válidas (1 até o número de canais)")
}

func TestChangeChannelPositionHandlerNotFound(t *testing.T) {
	body, _ := json.Marshal(map[string]int{"old_position": 1, "new_position": 1})
	c := newContext(t, http.MethodPut, "/channels/"+randUUID()+"/change_position", body, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(randUUID())
	c.Set(middleware.UserIDContextKey, randUUID())
	rec := recorder(c)

	if err := ChangeChannelPositionHandler(testBaseURL, c); err != nil {
		t.Fatalf("ChangeChannelPositionHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

func TestChangeChannelPositionHandlerConflict(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	c1, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar primeiro canal: %v", err)
	}
	if _, err := storage.CreateChannel(testCtx(), "canal_"+randHex(4), "text", ""); err != nil {
		t.Fatalf("falha ao criar segundo canal: %v", err)
	}

	body, _ := json.Marshal(map[string]int{"old_position": 2, "new_position": 1})
	c := newContext(t, http.MethodPut, "/channels/"+c1.ID+"/change_position", body, "")
	c.SetParamNames("channel_id")
	c.SetParamValues(c1.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := ChangeChannelPositionHandler(testBaseURL, c); err != nil {
		t.Fatalf("ChangeChannelPositionHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusConflict, "channel-position-conflict", "Posição do canal desatualizada",
		"a posição atual do canal não corresponde à old_position informada")
}

// --- ListRolesHandler (tarefa 6.2) ---

func TestListRolesHandlerEmpty(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)

	c := newContext(t, http.MethodGet, "/roles", nil, "")
	rec := recorder(c)

	if err := ListRolesHandler(testBaseURL, c); err != nil {
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
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	colorA := "#FF0000"
	colorB := "#00FF00"
	roleA, err := storage.CreateRole(testCtx(), "role_"+randHex(4), &colorA, models.RolePermissions{ManageRoles: true})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	roleB, err := storage.CreateRole(testCtx(), "role_"+randHex(4), &colorB, models.RolePermissions{BanMembers: true})
	if err != nil {
		t.Fatalf("falha ao criar segunda role: %v", err)
	}

	c := newContext(t, http.MethodGet, "/roles", nil, "")
	rec := recorder(c)

	if err := ListRolesHandler(testBaseURL, c); err != nil {
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

// --- CreateRoleHandler (tarefa 6.2) ---

func TestCreateRoleHandlerSuccess(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)

	name := "role_" + randHex(4)
	color := "#FF0000"
	permissions := models.RolePermissions{ManageRoles: true, BanMembers: true}
	body, _ := json.Marshal(map[string]any{
		"name":        name,
		"color":       color,
		"permissions": permissions,
	})
	c := newContext(t, http.MethodPost, "/roles", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := CreateRoleHandler(testBaseURL, c); err != nil {
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
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)

	name := "role_" + randHex(4)
	body, _ := json.Marshal(map[string]string{"name": name})
	c := newContext(t, http.MethodPost, "/roles", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := CreateRoleHandler(testBaseURL, c); err != nil {
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
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)

	c := newContext(t, http.MethodPost, "/roles", []byte("{invalido"), "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := CreateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestCreateRoleHandlerMissingName(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)

	body, _ := json.Marshal(map[string]string{"name": ""})
	c := newContext(t, http.MethodPost, "/roles", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := CreateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"name é obrigatório e deve ter no máximo 32 caracteres; color deve ser hexadecimal #RRGGBB")
}

func TestCreateRoleHandlerNameTooLong(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)

	body, _ := json.Marshal(map[string]string{"name": strings.Repeat("r", 33)})
	c := newContext(t, http.MethodPost, "/roles", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := CreateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"name é obrigatório e deve ter no máximo 32 caracteres; color deve ser hexadecimal #RRGGBB")
}

func TestCreateRoleHandlerInvalidColor(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)

	body, _ := json.Marshal(map[string]string{"name": "role_" + randHex(4), "color": "FF0000"})
	c := newContext(t, http.MethodPost, "/roles", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := CreateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"name é obrigatório e deve ter no máximo 32 caracteres; color deve ser hexadecimal #RRGGBB")
}

func TestCreateRoleHandlerNameTaken(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	name := "role_" + randHex(4)
	if _, err := storage.CreateRole(testCtx(), name, nil, models.RolePermissions{}); err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"name": name})
	c := newContext(t, http.MethodPost, "/roles", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := CreateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusConflict, "role-name-taken", "Nome de role já existe",
		"o nome informado já está em uso")
}

// --- UpdateRoleHandler (tarefa 6.2) ---

func TestUpdateRoleHandlerSuccess(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(4), nil, models.RolePermissions{})
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
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := UpdateRoleHandler(testBaseURL, c); err != nil {
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
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	c := newContext(t, http.MethodPut, "/roles/"+role.ID, []byte("{invalido"), "")
	c.SetParamNames("role_id")
	c.SetParamValues(role.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := UpdateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestUpdateRoleHandlerMissingName(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"name": ""})
	c := newContext(t, http.MethodPut, "/roles/"+role.ID, body, "")
	c.SetParamNames("role_id")
	c.SetParamValues(role.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := UpdateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"name é obrigatório e deve ter no máximo 32 caracteres; color deve ser hexadecimal #RRGGBB")
}

func TestUpdateRoleHandlerInvalidColor(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"name": "role_" + randHex(4), "color": "#GGG"})
	c := newContext(t, http.MethodPut, "/roles/"+role.ID, body, "")
	c.SetParamNames("role_id")
	c.SetParamValues(role.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := UpdateRoleHandler(testBaseURL, c); err != nil {
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
	c.Set(middleware.UserIDContextKey, randUUID())
	rec := recorder(c)

	if err := UpdateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "role não encontrada")
}

func TestUpdateRoleHandlerNameTaken(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	roleA, err := storage.CreateRole(testCtx(), "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	roleB, err := storage.CreateRole(testCtx(), "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar segunda role: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"name": roleB.Name})
	c := newContext(t, http.MethodPut, "/roles/"+roleA.ID, body, "")
	c.SetParamNames("role_id")
	c.SetParamValues(roleA.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := UpdateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusConflict, "role-name-taken", "Nome de role já existe",
		"o nome informado já está em uso")
}

func TestUpdateRoleHandlerMissingParam(t *testing.T) {
	c := newContext(t, http.MethodPut, "/roles/", []byte(`{"name":"role"}`), "")
	c.SetParamNames("role_id")
	c.SetParamValues("")
	c.Set(middleware.UserIDContextKey, randUUID())
	rec := recorder(c)

	if err := UpdateRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "role_id ausente")
}

// --- DeleteRoleHandler (tarefa 6.2) ---

func TestDeleteRoleHandlerSuccess(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	member, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar membro: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), member.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role ao membro: %v", err)
	}

	c := newContext(t, http.MethodDelete, "/roles/"+role.ID, nil, "")
	c.SetParamNames("role_id")
	c.SetParamValues(role.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := DeleteRoleHandler(testBaseURL, c); err != nil {
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
	c.Set(middleware.UserIDContextKey, randUUID())
	rec := recorder(c)

	if err := DeleteRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "role não encontrada")
}

func TestDeleteRoleHandlerMissingParam(t *testing.T) {
	c := newContext(t, http.MethodDelete, "/roles/", nil, "")
	c.SetParamNames("role_id")
	c.SetParamValues("")
	c.Set(middleware.UserIDContextKey, randUUID())
	rec := recorder(c)

	if err := DeleteRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "role_id ausente")
}

// --- AssignUserRoleHandler (tarefa 6.2) ---

func TestAssignUserRoleHandlerSuccess(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	member, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar membro: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"role_id": role.ID})
	c := newContext(t, http.MethodPost, "/users/"+member.ID+"/roles", body, "")
	c.SetParamNames("user_id")
	c.SetParamValues(member.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := AssignUserRoleHandler(testBaseURL, c); err != nil {
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
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	member, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar membro: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"role_id": role.ID})
	c := newContext(t, http.MethodPost, "/users/"+member.ID+"/roles", body, "")
	c.SetParamNames("user_id")
	c.SetParamValues(member.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := AssignUserRoleHandler(testBaseURL, c); err != nil {
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
	c2.Set(middleware.UserIDContextKey, owner.ID)
	rec2 := recorder(c2)

	if err := AssignUserRoleHandler(testBaseURL, c2); err != nil {
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
	c.Set(middleware.UserIDContextKey, member.ID)
	rec := recorder(c)

	if err := AssignUserRoleHandler(testBaseURL, c); err != nil {
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
	c.Set(middleware.UserIDContextKey, member.ID)
	rec := recorder(c)

	if err := AssignUserRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("AssignUserRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "role_id é obrigatório")
}

func TestAssignUserRoleHandlerUserNotFound(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	userID := randUUID()

	body, _ := json.Marshal(map[string]string{"role_id": role.ID})
	c := newContext(t, http.MethodPost, "/users/"+userID+"/roles", body, "")
	c.SetParamNames("user_id")
	c.SetParamValues(userID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := AssignUserRoleHandler(testBaseURL, c); err != nil {
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
	c.Set(middleware.UserIDContextKey, member.ID)
	rec := recorder(c)

	if err := AssignUserRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("AssignUserRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "role não encontrada")
}

func TestAssignUserRoleHandlerMissingUserParam(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"role_id": randUUID()})
	c := newContext(t, http.MethodPost, "/users//roles", body, "")
	c.SetParamNames("user_id")
	c.SetParamValues("")
	c.Set(middleware.UserIDContextKey, randUUID())
	rec := recorder(c)

	if err := AssignUserRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("AssignUserRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "user_id ausente")
}

// --- RemoveUserRoleHandler (tarefa 6.2) ---

func TestRemoveUserRoleHandlerSuccess(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	member, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar membro: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), member.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role ao membro: %v", err)
	}

	c := newContext(t, http.MethodDelete, "/users/"+member.ID+"/roles/"+role.ID, nil, "")
	c.SetParamNames("user_id", "role_id")
	c.SetParamValues(member.ID, role.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := RemoveUserRoleHandler(testBaseURL, c); err != nil {
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
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	userID := randUUID()

	c := newContext(t, http.MethodDelete, "/users/"+userID+"/roles/"+role.ID, nil, "")
	c.SetParamNames("user_id", "role_id")
	c.SetParamValues(userID, role.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := RemoveUserRoleHandler(testBaseURL, c); err != nil {
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
	c.Set(middleware.UserIDContextKey, member.ID)
	rec := recorder(c)

	if err := RemoveUserRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("RemoveUserRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "role não encontrada")
}

func TestRemoveUserRoleHandlerUserRoleNotFound(t *testing.T) {
	cleanServers(testCtx())
	owner, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	member, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar membro: %v", err)
	}
	storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID)
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}

	c := newContext(t, http.MethodDelete, "/users/"+member.ID+"/roles/"+role.ID, nil, "")
	c.SetParamNames("user_id", "role_id")
	c.SetParamValues(member.ID, role.ID)
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := RemoveUserRoleHandler(testBaseURL, c); err != nil {
		t.Fatalf("RemoveUserRoleHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "role não atribuída ao usuário")
}

func TestRemoveUserRoleHandlerMissingParams(t *testing.T) {
	c := newContext(t, http.MethodDelete, "/users//roles/", nil, "")
	c.SetParamNames("user_id", "role_id")
	c.SetParamValues("", "")
	c.Set(middleware.UserIDContextKey, randUUID())
	rec := recorder(c)

	if err := RemoveUserRoleHandler(testBaseURL, c); err != nil {
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
	if err := cleanServers(testCtx()); err != nil {
		t.Fatalf("falha ao limpar tabelas de servidor: %v", err)
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
	if _, err := storage.CreateServerWithIcon(testCtx(), "srv_"+randHex(4), nil, false, nil, &hash); err != nil {
		t.Fatalf("falha ao criar servidor não público: %v", err)
	}
}

// createPublicServerTest cria o servidor do backend como público.
func createPublicServerTest(t *testing.T) {
	t.Helper()
	if _, err := storage.CreateServerWithIcon(testCtx(), "srv_"+randHex(4), nil, true, nil, nil); err != nil {
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

	if err := LoginServerHandler(testBaseURL, c); err != nil {
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

	if err := LoginServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginServerHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 em servidor público, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

func TestLoginServerHandlerInvalidJSON(t *testing.T) {
	c := newContext(t, http.MethodPost, "/auth/loginServer", []byte("{invalido"), newRandomIP())
	rec := recorder(c)

	if err := LoginServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

func TestLoginServerHandlerMissingPassword(t *testing.T) {
	body, _ := json.Marshal(map[string]string{})
	c := newContext(t, http.MethodPost, "/auth/loginServer", body, newRandomIP())
	rec := recorder(c)

	if err := LoginServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "campo 'server_password' é obrigatório")
}

func TestLoginServerHandlerPasswordTooLong(t *testing.T) {
	cfg := config.LoadConfig()

	body, _ := json.Marshal(map[string]string{"server_password": strings.Repeat("a", cfg.MaxPasswordLength+1)})
	c := newContext(t, http.MethodPost, "/auth/loginServer", body, newRandomIP())
	rec := recorder(c)

	if err := LoginServerHandler(testBaseURL, c); err != nil {
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

	if err := LoginServerHandler(testBaseURL, c); err != nil {
		t.Fatalf("LoginServerHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "invalid-credentials", "Credenciais inválidas", "senha do servidor incorreta")
}

func TestLoginServerHandlerServerNotFound(t *testing.T) {
	removeAllServersTest(t)

	body, _ := json.Marshal(map[string]string{"server_password": "qualquer_senha"})
	c := newContext(t, http.MethodPost, "/auth/loginServer", body, newRandomIP())
	rec := recorder(c)

	if err := LoginServerHandler(testBaseURL, c); err != nil {
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

	if err := LoginServerHandler(testBaseURL, c); err != nil {
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

	if err := RegisterHandler(testBaseURL, c); err != nil {
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

	if err := LoginHandler(testBaseURL, c); err != nil {
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

	if err := LoginHandler(testBaseURL, c); err != nil {
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

	if err := RegisterHandler(testBaseURL, c); err != nil {
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

	if err := LoginHandler(testBaseURL, c); err != nil {
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
	if err := cleanServers(testCtx()); err != nil {
		t.Fatalf("falha ao limpar tabelas de servidor: %v", err)
	}
	server, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &ownerID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := storage.CreateChannel(testCtx(), "chn_"+randHex(4), "text", "")
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

	if err := ListMessagesHandler(testBaseURL, c); err != nil {
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
	first, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "primeira mensagem", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar primeira mensagem: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	second, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "segunda mensagem", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar segunda mensagem: %v", err)
	}

	c := newContext(t, http.MethodGet, "/channels/"+channel.ID+"/messages", nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	rec := recorder(c)

	if err := ListMessagesHandler(testBaseURL, c); err != nil {
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
	first, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "primeira", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar primeira mensagem: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "segunda", "", nil); err != nil {
		t.Fatalf("falha ao criar segunda mensagem: %v", err)
	}

	// since com precisão de microssegundos (o client envia o created_at da
	// última mensagem recebida, com a precisão exposta no JSON)
	c := newContext(t, http.MethodGet, "/channels/"+channel.ID+"/messages?since="+first.CreatedAt.Format(time.RFC3339Nano), nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("channel_id")
	c.SetParamValues(channel.ID)
	rec := recorder(c)

	if err := ListMessagesHandler(testBaseURL, c); err != nil {
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

	if err := ListMessagesHandler(testBaseURL, c); err != nil {
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

	if err := ListMessagesHandler(testBaseURL, c); err != nil {
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

	if err := ListMessagesHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListMessagesHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"since deve ser um timestamp ISO 8601")
}

func TestListMessagesHandlerForbiddenWithoutPermission(t *testing.T) {
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(8), nil, models.RolePermissions{})
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

	if err := ListMessagesHandler(testBaseURL, c); err != nil {
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

	if err := CreateMessageHandler(testBaseURL, c); err != nil {
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

	if err := CreateMessageHandler(testBaseURL, c); err != nil {
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

	if err := CreateMessageHandler(testBaseURL, c); err != nil {
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

	if err := CreateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"channel_id é obrigatório; content tem no máximo 8192 caracteres; a mensagem precisa de content ou attachment; nome do attachment inválido; reply_to deve referenciar uma mensagem do mesmo canal")
}

func TestCreateMessageHandlerContentTooLong(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)

	c := newMultipartContext(t, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": strings.Repeat("a", 8193)}, nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := CreateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"channel_id é obrigatório; content tem no máximo 8192 caracteres; a mensagem precisa de content ou attachment; nome do attachment inválido; reply_to deve referenciar uma mensagem do mesmo canal")
}

func TestCreateMessageHandlerChannelNotFound(t *testing.T) {
	owner := newTestMessageUser(t)
	missingID := randUUID()

	c := newMultipartContext(t, http.MethodPost, "/messages",
		map[string]string{"channel_id": missingID, "content": "x"}, nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := CreateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

func TestCreateMessageHandlerForbiddenWithoutPermission(t *testing.T) {
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(8), nil, models.RolePermissions{})
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

	if err := CreateMessageHandler(testBaseURL, c); err != nil {
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

	if err := CreateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"corpo da requisição deve ser multipart/form-data válido")
}

// --- UpdateMessageHandler ---

func TestUpdateMessageHandlerSuccess(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "original", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"content": "editada"})
	c := newContext(t, http.MethodPut, "/messages/"+message.ID, body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("message_id")
	c.SetParamValues(message.ID)
	rec := recorder(c)

	if err := UpdateMessageHandler(testBaseURL, c); err != nil {
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
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "conteúdo", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"content": ""})
	c := newContext(t, http.MethodPut, "/messages/"+message.ID, body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("message_id")
	c.SetParamValues(message.ID)
	rec := recorder(c)

	if err := UpdateMessageHandler(testBaseURL, c); err != nil {
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

	if err := UpdateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "mensagem não encontrada")
}

func TestUpdateMessageHandlerForbiddenOtherUser(t *testing.T) {
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "x", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"content": "hacker"})
	c := newContext(t, http.MethodPut, "/messages/"+message.ID, body, "")
	c.Set(middleware.UserIDContextKey, actor.ID)
	c.SetParamNames("message_id")
	c.SetParamValues(message.ID)
	rec := recorder(c)

	if err := UpdateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"somente o autor da mensagem pode editá-la")
}

func TestUpdateMessageHandlerContentTooLong(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "x", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"content": strings.Repeat("a", 8193)})
	c := newContext(t, http.MethodPut, "/messages/"+message.ID, body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("message_id")
	c.SetParamValues(message.ID)
	rec := recorder(c)

	if err := UpdateMessageHandler(testBaseURL, c); err != nil {
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

	if err := UpdateMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"corpo da requisição inválido")
}

// --- DeleteMessageHandler ---

func TestDeleteMessageHandlerSuccess(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "a excluir", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	c := newContext(t, http.MethodDelete, "/messages/"+message.ID, nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("message_id")
	c.SetParamValues(message.ID)
	rec := recorder(c)

	if err := DeleteMessageHandler(testBaseURL, c); err != nil {
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

	if err := DeleteMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "mensagem não encontrada")
}

func TestDeleteMessageHandlerForbiddenOtherUser(t *testing.T) {
	// canal aberto (sem roles): delete_messages não é livre — só o autor
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "x", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	c := newContext(t, http.MethodDelete, "/messages/"+message.ID, nil, "")
	c.Set(middleware.UserIDContextKey, actor.ID)
	c.SetParamNames("message_id")
	c.SetParamValues(message.ID)
	rec := recorder(c)

	if err := DeleteMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não tem permissão para excluir a mensagem")
}

func TestDeleteMessageHandlerWithDeleteMessagesRole(t *testing.T) {
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "x", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(8), nil, models.RolePermissions{})
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

	if err := DeleteMessageHandler(testBaseURL, c); err != nil {
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
	message, err := storage.CreateMessage(testCtx(), channel.ID, other.ID, "x", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	c := newContext(t, http.MethodDelete, "/messages/"+message.ID, nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("message_id")
	c.SetParamValues(message.ID)
	rec := recorder(c)

	if err := DeleteMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteMessageHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// --- Emojis (tarefa 7.4) ---

func TestListEmojisHandlerEmpty(t *testing.T) {
	owner := newTestMessageUser(t)
	createServerAndChannelTest(t, owner.ID)

	c := newContext(t, http.MethodGet, "/emojis", nil, "")
	rec := recorder(c)

	if err := ListEmojisHandler(testBaseURL, c); err != nil {
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
	createServerAndChannelTest(t, owner.ID)
	e1, err := storage.CreateEmoji(testCtx(), "emoji_"+randHex(8), newTestMediaHash(t, []byte{1}), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	e2, err := storage.CreateEmoji(testCtx(), "emoji_"+randHex(8), newTestMediaHash(t, []byte{2}), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}

	c := newContext(t, http.MethodGet, "/emojis", nil, "")
	rec := recorder(c)

	if err := ListEmojisHandler(testBaseURL, c); err != nil {
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
	createServerAndChannelTest(t, owner.ID)
	for i := 0; i < 26; i++ {
		if _, err := storage.CreateEmoji(testCtx(), "emoji_"+randHex(8), newTestMediaHash(t, []byte{1}), &owner.ID); err != nil {
			t.Fatalf("falha ao criar emoji %d: %v", i, err)
		}
	}

	c := newContext(t, http.MethodGet, "/emojis", nil, "")
	rec := recorder(c)

	if err := ListEmojisHandler(testBaseURL, c); err != nil {
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
	createServerAndChannelTest(t, owner.ID)
	first, err := storage.CreateEmoji(testCtx(), "emoji_"+randHex(8), newTestMediaHash(t, []byte{1}), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	second, err := storage.CreateEmoji(testCtx(), "emoji_"+randHex(8), newTestMediaHash(t, []byte{2}), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}

	c := newContext(t, http.MethodGet, "/emojis?since="+first.CreatedAt.Format(time.RFC3339Nano), nil, "")
	rec := recorder(c)

	if err := ListEmojisHandler(testBaseURL, c); err != nil {
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
	createServerAndChannelTest(t, owner.ID)

	c := newContext(t, http.MethodGet, "/emojis?since=nao-e-data", nil, "")
	rec := recorder(c)

	if err := ListEmojisHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListEmojisHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"since deve ser um timestamp ISO 8601")
}

func TestCreateEmojiHandlerSuccess(t *testing.T) {
	owner := newTestMessageUser(t)
	createServerAndChannelTest(t, owner.ID)
	name := "emoji_" + randHex(8)
	body, _ := json.Marshal(map[string]string{
		"name":       name,
		"format":     "png",
		"image_blob": base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100)),
	})

	c := newContext(t, http.MethodPost, "/emojis", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := CreateEmojiHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateEmojiHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var emoji models.Emoji
	if err := json.Unmarshal(rec.Body.Bytes(), &emoji); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if emoji.ID == "" || emoji.Name != name {
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
	createServerAndChannelTest(t, owner.ID)
	blob := base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100))
	gif := base64.StdEncoding.EncodeToString([]byte{0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 1, 1, 0})

	cases := []struct {
		desc string
		body map[string]string
	}{
		{"name ausente", map[string]string{"format": "PNG", "image_blob": blob}},
		{"format ausente", map[string]string{"name": "x", "image_blob": blob}},
		{"image_blob ausente", map[string]string{"name": "x", "format": "PNG"}},
		{"name acima de 32 caracteres", map[string]string{"name": strings.Repeat("a", 33), "format": "PNG", "image_blob": blob}},
		{"format inválido", map[string]string{"name": "x", "format": "SVG", "image_blob": blob}},
		{"base64 inválido", map[string]string{"name": "x", "format": "PNG", "image_blob": "!!!"}},
		{"conteúdo não corresponde ao formato", map[string]string{"name": "x", "format": "PNG", "image_blob": gif}},
	}
	for _, tc := range cases {
		body, _ := json.Marshal(tc.body)
		c := newContext(t, http.MethodPost, "/emojis", body, "")
		c.Set(middleware.UserIDContextKey, owner.ID)
		rec := recorder(c)

		if err := CreateEmojiHandler(testBaseURL, c); err != nil {
			t.Fatalf("%s: CreateEmojiHandler retornou erro: %v", tc.desc, err)
		}
		assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
			"name, format e image_blob são obrigatórios; name deve ter no máximo 32 caracteres; "+
				"format deve ser GIF, JPEG ou PNG; image_blob deve ser base64 de uma imagem com no máximo 256kb")
	}
}

func TestCreateEmojiHandlerNameTaken(t *testing.T) {
	owner := newTestMessageUser(t)
	createServerAndChannelTest(t, owner.ID)
	name := "emoji_" + randHex(8)
	body, _ := json.Marshal(map[string]string{
		"name":       name,
		"format":     "PNG",
		"image_blob": base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100)),
	})

	c := newContext(t, http.MethodPost, "/emojis", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)
	if err := CreateEmojiHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateEmojiHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201 no primeiro POST, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	// segundo POST com o mesmo nome: 409
	c = newContext(t, http.MethodPost, "/emojis", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec = recorder(c)
	if err := CreateEmojiHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateEmojiHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusConflict, "emoji-name-taken", "Nome de emoji já existe",
		"o nome informado já está em uso")
}

func TestCreateEmojiHandlerLimitReached(t *testing.T) {
	owner := newTestMessageUser(t)
	createServerAndChannelTest(t, owner.ID)
	for i := 0; i < 500; i++ {
		if _, err := storage.CreateEmoji(testCtx(), "emoji_"+randHex(8), newTestMediaHash(t, []byte{1}), &owner.ID); err != nil {
			t.Fatalf("falha ao criar emoji %d: %v", i, err)
		}
	}

	body, _ := json.Marshal(map[string]string{
		"name":       "emoji_" + randHex(8),
		"format":     "PNG",
		"image_blob": base64.StdEncoding.EncodeToString(pngAvatarBytes(100, 100)),
	})

	c := newContext(t, http.MethodPost, "/emojis", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := CreateEmojiHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateEmojiHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusConflict, "emoji-limit-reached", "Limite de emojis atingido",
		"o número máximo de emojis (500) já foi atingido")
}

func TestCreateEmojiHandlerMissingUserID(t *testing.T) {
	body, _ := json.Marshal(map[string]string{
		"name":       "x",
		"format":     "PNG",
		"image_blob": "x",
	})
	c := newContext(t, http.MethodPost, "/emojis", body, "")
	rec := recorder(c)

	if err := CreateEmojiHandler(testBaseURL, c); err != nil {
		t.Fatalf("CreateEmojiHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

func TestDeleteEmojiHandlerSuccess(t *testing.T) {
	owner := newTestMessageUser(t)
	createServerAndChannelTest(t, owner.ID)
	emoji, err := storage.CreateEmoji(testCtx(), "emoji_"+randHex(8), newTestMediaHash(t, []byte{1}), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}

	c := newContext(t, http.MethodDelete, "/emojis/"+emoji.ID, nil, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	c.SetParamNames("emoji_id")
	c.SetParamValues(emoji.ID)
	rec := recorder(c)

	if err := DeleteEmojiHandler(testBaseURL, c); err != nil {
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

	if err := DeleteEmojiHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteEmojiHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "emoji não encontrado")
}

func TestDeleteEmojiHandlerForbidden(t *testing.T) {
	owner := newTestMessageUser(t)
	stranger := newTestMessageUser(t)
	createServerAndChannelTest(t, owner.ID)
	emoji, err := storage.CreateEmoji(testCtx(), "emoji_"+randHex(8), newTestMediaHash(t, []byte{1}), &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}

	c := newContext(t, http.MethodDelete, "/emojis/"+emoji.ID, nil, "")
	c.Set(middleware.UserIDContextKey, stranger.ID)
	c.SetParamNames("emoji_id")
	c.SetParamValues(emoji.ID)
	rec := recorder(c)

	if err := DeleteEmojiHandler(testBaseURL, c); err != nil {
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

	if err := DeleteEmojiHandler(testBaseURL, c); err != nil {
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

	if err := DeleteEmojiHandler(testBaseURL, c); err != nil {
		t.Fatalf("DeleteEmojiHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "emoji_id ausente")
}

// --- SearchHandler ---

func TestSearchHandlerSuccess(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	unique := "w" + randHex(8)
	msg, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "mensagem "+unique, "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	if _, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "outra mensagem", "", nil); err != nil {
		t.Fatalf("falha ao criar outra mensagem: %v", err)
	}

	body, _ := json.Marshal(models.SearchRequest{Text: unique})
	c := newContext(t, http.MethodPost, "/search", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := SearchHandler(testBaseURL, c); err != nil {
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
	if r.Type != "message" || r.ID != msg.ID || r.ChannelID != channel.ID {
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
	m1, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "busca "+unique, "", nil)
	if err != nil {
		t.Fatalf("falha ao criar m1: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	m2, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "busca "+unique, "", nil)
	if err != nil {
		t.Fatalf("falha ao criar m2: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	m3, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "busca "+unique, "", nil)
	if err != nil {
		t.Fatalf("falha ao criar m3: %v", err)
	}

	body, _ := json.Marshal(models.SearchRequest{Text: unique})

	// primeira página: ordem decrescente
	c := newContext(t, http.MethodPost, "/search", body, "")
	c.Set(middleware.UserIDContextKey, owner.ID)
	rec := recorder(c)

	if err := SearchHandler(testBaseURL, c); err != nil {
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

	if err := SearchHandler(testBaseURL, c2); err != nil {
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

	if err := SearchHandler(testBaseURL, c); err != nil {
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

	if err := SearchHandler(testBaseURL, c); err != nil {
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

	if err := SearchHandler(testBaseURL, c); err != nil {
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

	if err := SearchHandler(testBaseURL, c); err != nil {
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

	if err := SearchHandler(testBaseURL, c); err != nil {
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

	// 32 runes for nickname, 64 runes for status and 512 runes for description (multibyte) is accepted
	nickname := "n" + strings.Repeat("ç", 31)
	status := "s" + strings.Repeat("ç", 63)
	description := "d" + strings.Repeat("ç", 511)
	body, _ := json.Marshal(map[string]string{"nickname": nickname, "status": status, "description": description})
	c := newContext(t, http.MethodPut, "/users/"+user.ID, body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := UpdateUserHandler(testBaseURL, c); err != nil {
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
	if stored.Description == nil || *stored.Description != description {
		t.Errorf("expected description %q, got %v", description, stored.Description)
	}
}

func TestUpdateUserHandlerNicknameTooLong(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	// 33 runes
	nickname := "n" + strings.Repeat("ç", 32)
	body, _ := json.Marshal(map[string]string{"nickname": nickname, "status": "ok", "description": "sobre mim"})
	c := newContext(t, http.MethodPut, "/users/"+user.ID, body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := UpdateUserHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateUserHandler retornou erro: %v", err)
	}

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"nickname deve ter no máximo 32 caracteres, status no máximo 64 caracteres e description no máximo 512 caracteres")
}

func TestUpdateUserHandlerDescriptionTooLong(t *testing.T) {
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	// 513 runes
	description := "d" + strings.Repeat("ç", 512)
	body, _ := json.Marshal(map[string]string{"nickname": "nick", "status": "ok", "description": description})
	c := newContext(t, http.MethodPut, "/users/"+user.ID, body, "")
	c.Set(middleware.UserIDContextKey, user.ID)
	c.SetParamNames("user_id")
	c.SetParamValues(user.ID)
	rec := recorder(c)

	if err := UpdateUserHandler(testBaseURL, c); err != nil {
		t.Fatalf("UpdateUserHandler retornou erro: %v", err)
	}

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"nickname deve ter no máximo 32 caracteres, status no máximo 64 caracteres e description no máximo 512 caracteres")
}

// TestDownloadAttachmentRoutePartialContent garante que GET
// /attachments/:file_id atende Range requests (seek) com 206 Partial
// Content: intervalo inicial (bytes=0-4) e sufixo (bytes=6-) retornam a
// fatia do blob com Content-Range correto, e intervalo fora do alcance
// retorna 416.
func TestDownloadAttachmentRoutePartialContent(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	attachment := newMessageWithAttachmentRoute(t, e, channel.ID, token)

	blob, err := os.ReadFile(mediaPathFor(attachment.MediaShaHash))
	if err != nil {
		t.Fatalf("falha ao ler o blob de apoio: %v", err)
	}

	getRange := func(rangeHeader string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/attachments/"+attachment.ID, nil)
		req.Header.Set("Range", rangeHeader)
		req.AddCookie(authCookie(token))
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}

	rec := getRange("bytes=0-4")
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("esperava status 206, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if ar := rec.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("esperava accept-ranges bytes, obtive %q", ar)
	}
	if ct := rec.Header().Get(echo.HeaderContentType); ct != "image/png" {
		t.Errorf("esperava content-type image/png, obtive %q", ct)
	}
	if cl := rec.Header().Get(echo.HeaderContentLength); cl != "5" {
		t.Errorf("esperava content-length 5, obtive %q", cl)
	}
	if cr := rec.Header().Get("Content-Range"); cr != fmt.Sprintf("bytes 0-4/%d", len(blob)) {
		t.Errorf("esperava content-range %q, obtive %q", fmt.Sprintf("bytes 0-4/%d", len(blob)), cr)
	}
	if !bytes.Equal(rec.Body.Bytes(), blob[:5]) {
		t.Errorf("corpo do 206 não corresponde ao intervalo 0-4 do blob")
	}

	rec = getRange("bytes=6-")
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("esperava status 206 para bytes=6-, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if cr := rec.Header().Get("Content-Range"); cr != fmt.Sprintf("bytes 6-%d/%d", len(blob)-1, len(blob)) {
		t.Errorf("esperava content-range %q, obtive %q", fmt.Sprintf("bytes 6-%d/%d", len(blob)-1, len(blob)), cr)
	}
	if !bytes.Equal(rec.Body.Bytes(), blob[6:]) {
		t.Errorf("corpo do 206 não corresponde ao intervalo 6-fim do blob")
	}

	rec = getRange(fmt.Sprintf("bytes=%d-", len(blob)))
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("esperava status 416 para intervalo fora do alcance, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// TestGetMediaRoute garante que GET /media/:sha_hash serve o blob
// content-addressable com o MIME type da tabela media e Content-Disposition:
// inline, atende Range requests (206), retorna 404 para hash desconhecido e
// 401 sem autenticação.
func TestGetMediaRoute(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	banner := base64.StdEncoding.EncodeToString(pngAvatarBytes(1024, 256))
	body, _ := json.Marshal(map[string]string{"banner": banner, "banner_format": "PNG"})
	rec := do(t, e, http.MethodPut, "/users/"+userID+"/banner", body, authCookie(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200 ao atualizar o banner, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	user, err := storage.GetUserByID(testCtx(), userID)
	if err != nil {
		t.Fatalf("GetUserByID retornou erro: %v", err)
	}
	if user.BannerMedia == nil {
		t.Fatal("esperava banner_media persistida")
	}

	req := httptest.NewRequest(http.MethodGet, "/media/"+*user.BannerMedia, nil)
	req.AddCookie(authCookie(token))
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("esperava content-type %q, obtive %q", "image/png", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "inline" {
		t.Errorf("esperava content-disposition %q, obtive %q", "inline", cd)
	}
	if !bytes.Equal(rec.Body.Bytes(), pngAvatarBytes(1024, 256)) {
		t.Errorf("corpo da resposta não corresponde ao blob do banner")
	}

	// Range: 206 Partial Content
	req = httptest.NewRequest(http.MethodGet, "/media/"+*user.BannerMedia, nil)
	req.Header.Set("Range", "bytes=0-4")
	req.AddCookie(authCookie(token))
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("esperava status 206, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if cr := rec.Header().Get("Content-Range"); cr != fmt.Sprintf("bytes 0-4/%d", len(pngAvatarBytes(1024, 256))) {
		t.Errorf("esperava content-range %q, obtive %q", fmt.Sprintf("bytes 0-4/%d", len(pngAvatarBytes(1024, 256))), cr)
	}
	if !bytes.Equal(rec.Body.Bytes(), pngAvatarBytes(1024, 256)[:5]) {
		t.Errorf("corpo do 206 não corresponde ao intervalo 0-4 do blob")
	}

	// hash desconhecido: 404
	req = httptest.NewRequest(http.MethodGet, "/media/"+strings.Repeat("a", 64), nil)
	req.AddCookie(authCookie(token))
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "mídia não encontrada")

	// sem autenticação: 401
	req = httptest.NewRequest(http.MethodGet, "/media/"+*user.BannerMedia, nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

// --- PinMessageHandler ---

func pinContext(t *testing.T, channelID, messageID, userID string) echo.Context {
	t.Helper()
	c := newContext(t, http.MethodPost, "/channels/"+channelID+"/messages/"+messageID+"/pin", nil, "")
	if userID != "" {
		c.Set(middleware.UserIDContextKey, userID)
	}
	c.SetParamNames("channel_id", "message_id")
	c.SetParamValues(channelID, messageID)
	return c
}

func TestPinMessageHandlerCreated(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	c := pinContext(t, channel.ID, message.ID, owner.ID)
	rec := recorder(c)

	if err := PinMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("PinMessageHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var pinned models.PinnedMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &pinned); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if pinned.ChannelID != channel.ID || pinned.MessageID != message.ID {
		t.Errorf("pin não confere: %+v", pinned)
	}
	if pinned.PinnedBy == nil || *pinned.PinnedBy != owner.ID {
		t.Errorf("esperava pinned_by %s, obtive %v", owner.ID, pinned.PinnedBy)
	}
}

func TestPinMessageHandlerIdempotent(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	do := func() *httptest.ResponseRecorder {
		c := pinContext(t, channel.ID, message.ID, owner.ID)
		rec := recorder(c)
		if err := PinMessageHandler(testBaseURL, c); err != nil {
			t.Fatalf("PinMessageHandler retornou erro: %v", err)
		}
		return rec
	}

	if rec := do(); rec.Code != http.StatusCreated {
		t.Fatalf("primeira fixação: esperava 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if rec := do(); rec.Code != http.StatusOK {
		t.Fatalf("segunda fixação: esperava 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

func TestPinMessageHandlerUnauthorized(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	c := pinContext(t, channel.ID, message.ID, "")
	rec := recorder(c)

	if err := PinMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("PinMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

func TestPinMessageHandlerMissingParam(t *testing.T) {
	owner := newTestMessageUser(t)

	c := pinContext(t, "", "", owner.ID)
	rec := recorder(c)

	if err := PinMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("PinMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"channel_id e message_id são obrigatórios")
}

func TestPinMessageHandlerNotFound(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	missingID := randUUID()

	c := pinContext(t, channel.ID, missingID, owner.ID)
	rec := recorder(c)

	if err := PinMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("PinMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado",
		"mensagem não encontrada neste canal")
}

func TestPinMessageHandlerForbidden(t *testing.T) {
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	c := pinContext(t, channel.ID, message.ID, actor.ID)
	rec := recorder(c)

	if err := PinMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("PinMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não tem permissão para fixar a mensagem")
}

func TestPinMessageHandlerWithPinRole(t *testing.T) {
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(8), nil, models.RolePermissions{PinMessage: true})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), actor.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}

	c := pinContext(t, channel.ID, message.ID, actor.ID)
	rec := recorder(c)

	if err := PinMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("PinMessageHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// --- UnpinMessageHandler ---

func unpinContext(t *testing.T, channelID, messageID, userID string) echo.Context {
	t.Helper()
	c := newContext(t, http.MethodDelete, "/channels/"+channelID+"/messages/"+messageID+"/pin", nil, "")
	if userID != "" {
		c.Set(middleware.UserIDContextKey, userID)
	}
	c.SetParamNames("channel_id", "message_id")
	c.SetParamValues(channelID, messageID)
	return c
}

func TestUnpinMessageHandlerRemoved(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	if _, _, err := storage.PinMessage(testCtx(), channel.ID, message.ID, owner.ID); err != nil {
		t.Fatalf("falha ao fixar mensagem: %v", err)
	}

	c := unpinContext(t, channel.ID, message.ID, owner.ID)
	rec := recorder(c)

	if err := UnpinMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("UnpinMessageHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

func TestUnpinMessageHandlerNotPinned(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	c := unpinContext(t, channel.ID, message.ID, owner.ID)
	rec := recorder(c)

	if err := UnpinMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("UnpinMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado",
		"mensagem não está pinada neste canal")
}

func TestUnpinMessageHandlerNotFound(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)

	c := unpinContext(t, channel.ID, randUUID(), owner.ID)
	rec := recorder(c)

	if err := UnpinMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("UnpinMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado",
		"mensagem não encontrada neste canal")
}

func TestUnpinMessageHandlerForbidden(t *testing.T) {
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	if _, _, err := storage.PinMessage(testCtx(), channel.ID, message.ID, owner.ID); err != nil {
		t.Fatalf("falha ao fixar mensagem: %v", err)
	}

	c := unpinContext(t, channel.ID, message.ID, actor.ID)
	rec := recorder(c)

	if err := UnpinMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("UnpinMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não tem permissão para remover a fixação da mensagem")
}

func TestUnpinMessageHandlerUnauthorized(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	c := unpinContext(t, channel.ID, message.ID, "")
	rec := recorder(c)

	if err := UnpinMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("UnpinMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

func TestUnpinMessageHandlerMissingParam(t *testing.T) {
	owner := newTestMessageUser(t)

	c := unpinContext(t, "", "", owner.ID)
	rec := recorder(c)

	if err := UnpinMessageHandler(testBaseURL, c); err != nil {
		t.Fatalf("UnpinMessageHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"channel_id e message_id são obrigatórios")
}

// --- ListPinnedMessagesHandler ---

func pinnedContext(t *testing.T, channelID, userID string) echo.Context {
	t.Helper()
	c := newContext(t, http.MethodGet, "/channels/"+channelID+"/pinned", nil, "")
	if userID != "" {
		c.Set(middleware.UserIDContextKey, userID)
	}
	c.SetParamNames("channel_id")
	c.SetParamValues(channelID)
	return c
}

func TestListPinnedMessagesHandler(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	m1, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "uma", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	m2, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "duas", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	for _, m := range []models.Message{m2, m1} {
		if _, _, err := storage.PinMessage(testCtx(), channel.ID, m.ID, owner.ID); err != nil {
			t.Fatalf("falha ao fixar mensagem: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	c := pinnedContext(t, channel.ID, owner.ID)
	rec := recorder(c)

	if err := ListPinnedMessagesHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListPinnedMessagesHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var list models.PinnedMessageList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
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
}

func TestListPinnedMessagesHandlerEmpty(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)

	c := pinnedContext(t, channel.ID, owner.ID)
	rec := recorder(c)

	if err := ListPinnedMessagesHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListPinnedMessagesHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	// lista vazia serializa como [] (não null)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if string(raw["pinned"]) != "[]" {
		t.Errorf("esperava pinned como [], obtive %s", raw["pinned"])
	}
}

func TestListPinnedMessagesHandlerNotFound(t *testing.T) {
	owner := newTestMessageUser(t)

	c := pinnedContext(t, randUUID(), owner.ID)
	rec := recorder(c)

	if err := ListPinnedMessagesHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListPinnedMessagesHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado",
		"canal não encontrado")
}

func TestListPinnedMessagesHandlerForbidden(t *testing.T) {
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)

	role, err := storage.CreateRole(testCtx(), "role_"+randHex(8), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	if _, err := storage.UpdateChannelPermissions(testCtx(), channel.ID, role.ID, models.ChannelPermission{ReadChannel: false}); err != nil {
		t.Fatalf("falha ao definir permissões do canal: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), actor.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}

	c := pinnedContext(t, channel.ID, actor.ID)
	rec := recorder(c)

	if err := ListPinnedMessagesHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListPinnedMessagesHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não tem permissão para ler o canal")
}

func TestListPinnedMessagesHandlerUnauthorized(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)

	c := pinnedContext(t, channel.ID, "")
	rec := recorder(c)

	if err := ListPinnedMessagesHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListPinnedMessagesHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

// --- GET /channels (last_read) ---

func TestListChannelsRouteLastRead(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	_, channel := createServerAndChannelTest(t, userID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, userID, "lida", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	if err := storage.TouchLastReadMessage(testCtx(), userID, channel.ID, message); err != nil {
		t.Fatalf("falha ao registrar leitura: %v", err)
	}

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
	var got *models.ChannelSummary
	for i := range resp.Channels {
		if resp.Channels[i].ID == channel.ID {
			got = &resp.Channels[i]
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
}

// --- message_pin (websocket) ---

// TestPinUnpinBroadcastsMessagePin garante que fixar e desfixar uma mensagem
// distribuem o evento message_pin (com is_pinned true/false) aos clientes do
// canal.
func TestPinUnpinBroadcastsMessagePin(t *testing.T) {
	e := newApp()
	cfg := config.LoadConfig()
	RegisterWebSocketRoutes(e, cfg)
	go websocket.GetHub().Run()

	srv := httptest.NewServer(e)
	defer srv.Close()

	userID, token := registerAndLogin(t, e)
	_, channel := createServerAndChannelTest(t, userID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, userID, "fixar", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	header := http.Header{}
	header.Set(echo.HeaderCookie, "Auth="+token)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := ws.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("falha ao abrir conexão websocket: %v", err)
	}
	defer conn.Close()

	// primeiro evento: presence_sync da própria conexão
	readWSMessage(t, conn)

	// fixar → message_pin com is_pinned=true
	rec := do(t, e, http.MethodPost, "/channels/"+channel.ID+"/messages/"+message.ID+"/pin", nil, authCookie(token))
	if rec.Code != http.StatusCreated {
		t.Fatalf("pin: esperava 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	assertMessagePinEvent(t, conn, message.ID, true)

	// desfixar → message_pin com is_pinned=false
	rec = do(t, e, http.MethodDelete, "/channels/"+channel.ID+"/messages/"+message.ID+"/pin", nil, authCookie(token))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unpin: esperava 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	assertMessagePinEvent(t, conn, message.ID, false)
}

// assertMessagePinEvent lê a próxima mensagem websocket e valida o evento
// message_pin (type, message_id e is_pinned).
func assertMessagePinEvent(t *testing.T, conn *ws.Conn, messageID string, isPinned bool) {
	t.Helper()
	data := readWSMessage(t, conn)

	var event struct {
		Type      string `json:"type"`
		MessageID string `json:"message_id"`
		IsPinned  bool   `json:"is_pinned"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("falha ao decodificar evento: %v", err)
	}
	if event.Type != "message_pin" {
		t.Fatalf("esperava tipo message_pin, obtive %q (corpo: %s)", event.Type, data)
	}
	if event.MessageID != messageID {
		t.Errorf("esperava message_id %s, obtive %s", messageID, event.MessageID)
	}
	if event.IsPinned != isPinned {
		t.Errorf("esperava is_pinned=%v, obtive %v", isPinned, event.IsPinned)
	}
}

// reactionContext monta o contexto das rotas de reações com corpo JSON.
func reactionContext(t *testing.T, method, channelID, messageID, userID, body string) echo.Context {
	t.Helper()
	c := newContext(t, method, "/channels/"+channelID+"/messages/"+messageID+"/reactions", []byte(body), "")
	if userID != "" {
		c.Set(middleware.UserIDContextKey, userID)
	}
	c.SetParamNames("channel_id", "message_id")
	c.SetParamValues(channelID, messageID)
	return c
}

func TestAddReactionHandlerCreated(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	c := reactionContext(t, http.MethodPost, channel.ID, message.ID, owner.ID, `{"unicode":"👍"}`)
	rec := recorder(c)

	if err := AddReactionHandler(testBaseURL, c); err != nil {
		t.Fatalf("AddReactionHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var reaction models.MessageReaction
	if err := json.Unmarshal(rec.Body.Bytes(), &reaction); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if reaction.MessageID != message.ID || reaction.UserID != owner.ID {
		t.Errorf("reação não confere: %+v", reaction)
	}
	if reaction.EmojiID != nil {
		t.Errorf("esperava emoji_id null, obtive %v", *reaction.EmojiID)
	}
	if reaction.Unicode == nil || *reaction.Unicode != "👍" {
		t.Errorf("esperava unicode 👍, obtive %v", reaction.Unicode)
	}
}

func TestAddReactionHandlerIdempotent(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	do := func() *httptest.ResponseRecorder {
		c := reactionContext(t, http.MethodPost, channel.ID, message.ID, owner.ID, `{"unicode":"👍"}`)
		rec := recorder(c)
		if err := AddReactionHandler(testBaseURL, c); err != nil {
			t.Fatalf("AddReactionHandler retornou erro: %v", err)
		}
		return rec
	}

	if rec := do(); rec.Code != http.StatusCreated {
		t.Fatalf("primeira reação: esperava 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if rec := do(); rec.Code != http.StatusOK {
		t.Fatalf("segunda reação: esperava 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

func TestAddReactionHandlerCustomEmoji(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	hash := newTestMediaHash(t, []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a})
	emoji, err := storage.CreateEmoji(testCtx(), "emoji_"+randHex(8), hash, &owner.ID)
	if err != nil {
		t.Fatalf("falha ao criar emoji: %v", err)
	}

	c := reactionContext(t, http.MethodPost, channel.ID, message.ID, owner.ID, fmt.Sprintf(`{"emoji_id":"%s"}`, emoji.ID))
	rec := recorder(c)

	if err := AddReactionHandler(testBaseURL, c); err != nil {
		t.Fatalf("AddReactionHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var reaction models.MessageReaction
	if err := json.Unmarshal(rec.Body.Bytes(), &reaction); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if reaction.EmojiID == nil || *reaction.EmojiID != emoji.ID {
		t.Errorf("esperava emoji_id %s, obtive %v", emoji.ID, reaction.EmojiID)
	}
	if reaction.Unicode != nil {
		t.Errorf("esperava unicode null, obtive %q", *reaction.Unicode)
	}
}

func TestAddReactionHandlerUnauthorized(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	c := reactionContext(t, http.MethodPost, channel.ID, message.ID, "", `{"unicode":"👍"}`)
	rec := recorder(c)

	if err := AddReactionHandler(testBaseURL, c); err != nil {
		t.Fatalf("AddReactionHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

func TestAddReactionHandlerMissingParam(t *testing.T) {
	owner := newTestMessageUser(t)

	c := reactionContext(t, http.MethodPost, "", "", owner.ID, `{"unicode":"👍"}`)
	rec := recorder(c)

	if err := AddReactionHandler(testBaseURL, c); err != nil {
		t.Fatalf("AddReactionHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"channel_id e message_id são obrigatórios")
}

func TestAddReactionHandlerInvalidBody(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	for _, body := range []string{`{}`, `{"emoji_id":"` + randUUID() + `","unicode":"👍"}`} {
		c := reactionContext(t, http.MethodPost, channel.ID, message.ID, owner.ID, body)
		rec := recorder(c)

		if err := AddReactionHandler(testBaseURL, c); err != nil {
			t.Fatalf("AddReactionHandler retornou erro: %v", err)
		}
		assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
			"informe exatamente um de emoji_id ou unicode; unicode tem no máximo 16 caracteres")
	}
}

func TestAddReactionHandlerNotFound(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)

	c := reactionContext(t, http.MethodPost, channel.ID, randUUID(), owner.ID, `{"unicode":"👍"}`)
	rec := recorder(c)

	if err := AddReactionHandler(testBaseURL, c); err != nil {
		t.Fatalf("AddReactionHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado",
		"mensagem não encontrada neste canal")
}

func TestAddReactionHandlerForbidden(t *testing.T) {
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	// Fecha o canal (define permissões) para que o actor, sem role, seja negado.
	role := createRoleFor(t, models.RolePermissions{})
	if _, err := storage.UpdateChannelPermissions(context.Background(), channel.ID, role.ID, models.ChannelPermission{SendMessages: true}); err != nil {
		t.Fatalf("falha ao definir permissões do canal: %v", err)
	}

	c := reactionContext(t, http.MethodPost, channel.ID, message.ID, actor.ID, `{"unicode":"👍"}`)
	rec := recorder(c)

	if err := AddReactionHandler(testBaseURL, c); err != nil {
		t.Fatalf("AddReactionHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não tem permissão para reagir neste canal")
}

func TestAddReactionHandlerLimit(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	types := []string{"👍", "👎", "😂", "😢", "😮", "😡", "🔥", "🎉", "💀", "❤️", "🙏", "👀", "✨", "🍕", "🚀", "🎯", "💯", "🥳", "🫡", "🤝"}
	for i, unicode := range types {
		c := reactionContext(t, http.MethodPost, channel.ID, message.ID, owner.ID, fmt.Sprintf(`{"unicode":"%s"}`, unicode))
		rec := recorder(c)
		if err := AddReactionHandler(testBaseURL, c); err != nil {
			t.Fatalf("AddReactionHandler[%d] retornou erro: %v", i, err)
		}
		if rec.Code != http.StatusCreated {
			t.Fatalf("AddReactionHandler[%d]: esperava 201, obtive %d (corpo: %s)", i, rec.Code, rec.Body.String())
		}
	}

	c := reactionContext(t, http.MethodPost, channel.ID, message.ID, owner.ID, `{"unicode":"🆕"}`)
	rec := recorder(c)

	if err := AddReactionHandler(testBaseURL, c); err != nil {
		t.Fatalf("AddReactionHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusConflict, "reaction-limit-reached", "Limite de reações atingido",
		"a mensagem já tem o número máximo de 20 tipos de reação")
}

func TestRemoveReactionHandler(t *testing.T) {
	owner := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	add := reactionContext(t, http.MethodPost, channel.ID, message.ID, owner.ID, `{"unicode":"👍"}`)
	rec := recorder(add)
	if err := AddReactionHandler(testBaseURL, add); err != nil {
		t.Fatalf("AddReactionHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava 201 na criação, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	c := reactionContext(t, http.MethodDelete, channel.ID, message.ID, owner.ID, `{"unicode":"👍"}`)
	rec = recorder(c)
	if err := RemoveReactionHandler(testBaseURL, c); err != nil {
		t.Fatalf("RemoveReactionHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava status 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	// remover de novo → 404
	c = reactionContext(t, http.MethodDelete, channel.ID, message.ID, owner.ID, `{"unicode":"👍"}`)
	rec = recorder(c)
	if err := RemoveReactionHandler(testBaseURL, c); err != nil {
		t.Fatalf("RemoveReactionHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado",
		"usuário não reagiu à mensagem com este emoji")
}

func TestRemoveReactionHandlerForbidden(t *testing.T) {
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	// Fecha o canal (define permissões) para que o actor, sem role, seja negado.
	role := createRoleFor(t, models.RolePermissions{})
	if _, err := storage.UpdateChannelPermissions(context.Background(), channel.ID, role.ID, models.ChannelPermission{ReadChannel: true}); err != nil {
		t.Fatalf("falha ao definir permissões do canal: %v", err)
	}

	c := reactionContext(t, http.MethodDelete, channel.ID, message.ID, actor.ID, `{"unicode":"👍"}`)
	rec := recorder(c)

	if err := RemoveReactionHandler(testBaseURL, c); err != nil {
		t.Fatalf("RemoveReactionHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não tem permissão para ler o canal")
}

func TestListReactionsHandler(t *testing.T) {
	owner := newTestMessageUser(t)
	u1 := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	for _, user := range []models.User{owner, u1} {
		c := reactionContext(t, http.MethodPost, channel.ID, message.ID, user.ID, `{"unicode":"👍"}`)
		rec := recorder(c)
		if err := AddReactionHandler(testBaseURL, c); err != nil {
			t.Fatalf("AddReactionHandler retornou erro: %v", err)
		}
		if rec.Code != http.StatusCreated {
			t.Fatalf("esperava 201 na criação, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
		}
	}

	c := reactionContext(t, http.MethodGet, channel.ID, message.ID, owner.ID, "")
	rec := recorder(c)

	if err := ListReactionsHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListReactionsHandler retornou erro: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var list models.MessageReactionList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if list.MessageID != message.ID {
		t.Errorf("esperava message_id %s, obtive %s", message.ID, list.MessageID)
	}
	if len(list.Reactions) != 1 {
		t.Fatalf("esperava 1 tipo de reação, obtive %d: %+v", len(list.Reactions), list.Reactions)
	}
	g := list.Reactions[0]
	if g.EmojiID != nil || g.Unicode == nil || *g.Unicode != "👍" || g.Count != 2 {
		t.Errorf("esperava 👍 count=2, obtive %+v", g)
	}
	if len(g.Users) != 2 || !containsHandlerReactionUser(g.Users, owner.ID) || !containsHandlerReactionUser(g.Users, u1.ID) {
		t.Errorf("esperava users owner e u1, obtive %v", g.Users)
	}
}

func TestListReactionsHandlerForbidden(t *testing.T) {
	owner := newTestMessageUser(t)
	actor := newTestMessageUser(t)
	_, channel := createServerAndChannelTest(t, owner.ID)
	message, err := storage.CreateMessage(testCtx(), channel.ID, owner.ID, "reagir", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	// Fecha o canal (define permissões) para que o actor, sem role, seja negado.
	role := createRoleFor(t, models.RolePermissions{})
	if _, err := storage.UpdateChannelPermissions(context.Background(), channel.ID, role.ID, models.ChannelPermission{ReadChannel: true}); err != nil {
		t.Fatalf("falha ao definir permissões do canal: %v", err)
	}

	c := reactionContext(t, http.MethodGet, channel.ID, message.ID, actor.ID, "")
	rec := recorder(c)

	if err := ListReactionsHandler(testBaseURL, c); err != nil {
		t.Fatalf("ListReactionsHandler retornou erro: %v", err)
	}
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não tem permissão para ler o canal")
}

func containsHandlerReactionUser(users []string, id string) bool {
	for _, u := range users {
		if u == id {
			return true
		}
	}
	return false
}
