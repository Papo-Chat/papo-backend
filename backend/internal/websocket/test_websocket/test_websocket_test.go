package test_websocket

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/storage"
	"papo/internal/websocket"

	ws "github.com/gorilla/websocket"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// migrationsDir é o caminho relativo ao diretório deste pacote (backend/internal/websocket/test_websocket).
const migrationsDir = "../../../../migrations"

// defaultDatabaseURL corresponde aos padrões do infra/docker-compose.yml.
const defaultDatabaseURL = "postgres://papo:papo123@localhost:5432/papo"

// wsReadTimeout é a espera máxima por um evento que deveria chegar.
const wsReadTimeout = 5 * time.Second

// wsNoEventTimeout é a janela usada para afirmar que nenhum evento chegou.
const wsNoEventTimeout = 700 * time.Millisecond

func TestMain(m *testing.M) {
	os.Exit(runWebsocketTests(m))
}

// runWebsocketTests prepara um banco temporário com as migrations do projeto,
// inicializa o storage contra ele, executa os testes e remove o banco ao final
// (mesmo padrão dos demais pacotes de teste).
func runWebsocketTests(m *testing.M) int {
	baseURL := testDatabaseURL()

	baseDB, err := sql.Open("pgx", baseURL)
	if err != nil {
		fmt.Printf("testes de websocket ignorados: falha ao abrir conexão: %v\n", err)
		return 0
	}
	defer baseDB.Close()

	if err := ping(baseDB); err != nil {
		fmt.Printf("testes de websocket ignorados: não foi possível conectar ao PostgreSQL (%v). Inicie o PostgreSQL (infra/docker-compose.yml) ou defina TEST_DATABASE_URL/DATABASE_URL.\n", err)
		return 0
	}

	removeOldTempDatabases(baseDB)

	tempDBName, err := createTempDatabase(baseDB)
	if err != nil {
		fmt.Printf("testes de websocket ignorados: falha ao criar banco temporário: %v\n", err)
		return 0
	}
	defer dropTempDatabase(baseDB, tempDBName)

	tempURL, err := withDatabase(baseURL, tempDBName)
	if err != nil {
		fmt.Printf("testes de websocket ignorados: %v\n", err)
		return 0
	}

	tempDB, err := sql.Open("pgx", tempURL)
	if err != nil {
		fmt.Printf("testes de websocket ignorados: %v\n", err)
		return 0
	}
	defer tempDB.Close()

	if err := ping(tempDB); err != nil {
		fmt.Printf("testes de websocket ignorados: falha ao conectar no banco temporário: %v\n", err)
	}

	if err := applyMigrations(tempDB); err != nil {
		fmt.Printf("testes de websocket FALHARAM na preparação: %v\n", err)
		return 1
	}

	if err := storage.InitDB(tempURL); err != nil {
		fmt.Printf("testes de websocket FALHARAM na preparação: %v\n", err)
		return 1
	}

	code := m.Run()

	storage.CloseDB()
	return code
}

// --- infraestrutura de teste ---

// newWSHub cria um Hub com o loop Run ativo e o encerra no final do teste.
func newWSHub(t *testing.T) *websocket.Hub {
	t.Helper()
	hub := websocket.NewHub()
	go hub.Run()
	t.Cleanup(func() { hub.Shutdown() })
	return hub
}

// wsTestServer é um servidor HTTP de teste que faz o upgrade para WebSocket e
// registra a conexão no Hub com o usuário informado no query param "user"
// (mesmo fluxo do handler GET /ws, sem a autenticação JWT).
type wsTestServer struct {
	srv     *httptest.Server
	mu      sync.Mutex
	clients []*websocket.Client
}

func newWSTestServer(t *testing.T, hub *websocket.Hub, statusMessages map[string]*string) *wsTestServer {
	t.Helper()
	environment := &wsTestServer{}
	upgrader := ws.Upgrader{}
	environment.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := r.URL.Query().Get("user")
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		var statusMessage *string
		if statusMessages != nil {
			statusMessage = statusMessages[userID]
		}
		client := websocket.Connect(hub, conn, userID, statusMessage)
		environment.mu.Lock()
		environment.clients = append(environment.clients, client)
		environment.mu.Unlock()
	}))
	t.Cleanup(environment.srv.Close)
	return environment
}

// dial conecta um cliente websocket ao servidor de teste como o usuário dado.
func (s *wsTestServer) dial(t *testing.T, userID string) *ws.Conn {
	t.Helper()
	wsURL := "ws://" + s.srv.Listener.Addr().String() + "/?user=" + userID
	conn, _, err := ws.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("falha ao conectar o cliente websocket (user=%s): %v", userID, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// clientAt retorna o cliente registrado no Hub na posição informada
// (ordem das conexões do servidor de teste).
func (s *wsTestServer) clientAt(t *testing.T, index int) *websocket.Client {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < 0 || index >= len(s.clients) {
		t.Fatalf("índice %d fora do intervalo de clientes registrados (%d)", index, len(s.clients))
	}
	return s.clients[index]
}

// wsEvent é a visão genérica dos eventos outbound usada pelas asserções.
type wsEvent struct {
	Type          string          `json:"type"`
	Message       string          `json:"message"`
	ID            string          `json:"id"`
	ChannelID     string          `json:"channel_id"`
	AuthorID      string          `json:"author_id"`
	Content       string          `json:"content"`
	UserID        string          `json:"user_id"`
	IsTyping      bool            `json:"is_typing"`
	Status        string          `json:"status"`
	StatusMessage *string         `json:"status_message"`
	Members       []wsEventMember `json:"members"`
}

type wsEventMember struct {
	UserID        string  `json:"user_id"`
	Status        string  `json:"status"`
	StatusMessage *string `json:"status_message"`
}

// readEvent lê o próximo evento da conexão, falhando se ele não chegar a tempo.
func readEvent(t *testing.T, conn *ws.Conn) wsEvent {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("falha ao ler evento websocket: %v", err)
	}
	var event wsEvent
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("falha ao decodificar o evento %s: %v", data, err)
	}
	return event
}

// expectNoEvent afirma que nenhum evento chega dentro do timeout.
// Atenção: um timeout de leitura corrompe o estado da conexão no gorilla
// (toda leitura subsequente retorna o erro em cache), então use apenas em
// conexões que não serão lidas novamente.
func expectNoEvent(t *testing.T, conn *ws.Conn, timeout time.Duration) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(timeout))
	if _, data, err := conn.ReadMessage(); err == nil {
		t.Fatalf("esperava sem evento, mas recebeu: %s", data)
	}
}

// sendRaw envia uma mensagem de texto crua para o servidor.
func sendRaw(t *testing.T, conn *ws.Conn, raw string) {
	t.Helper()
	if err := conn.WriteMessage(ws.TextMessage, []byte(raw)); err != nil {
		t.Fatalf("falha ao enviar a mensagem %s: %v", raw, err)
	}
}

// sendTyping envia um evento de typing para o canal informado.
func sendTyping(t *testing.T, conn *ws.Conn, channelID string) {
	t.Helper()
	sendRaw(t, conn, fmt.Sprintf(`{"type":"typing","channel_id":%q}`, channelID))
}

// assertTyping valida o payload de um evento typing.
func assertTyping(t *testing.T, event wsEvent, channelID, userID string) {
	t.Helper()
	if event.Type != string(websocket.EventTypeTyping) {
		t.Fatalf("esperava evento typing, obtive %q", event.Type)
	}
	if event.ChannelID != channelID {
		t.Errorf("channel_id inesperado: %q", event.ChannelID)
	}
	if event.UserID != userID {
		t.Errorf("user_id inesperado: %q", event.UserID)
	}
	if !event.IsTyping {
		t.Error("is_typing deveria ser true")
	}
}

// --- helpers de banco (mesmo padrão dos testes de services) ---

func testCtx() context.Context {
	return context.Background()
}

func newTestUser(t *testing.T) models.User {
	t.Helper()
	user, err := services.Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário de teste: %v", err)
	}
	return user
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

// newTestServerChannel cria um servidor (com o dono informado) e um canal nele.
func newTestServerChannel(t *testing.T, ownerID *string) (models.Server, models.ChannelSummary) {
	t.Helper()
	server, err := services.CreateServer(testCtx(), "server_"+randHex(8), ownerID)
	if err != nil {
		t.Fatalf("falha ao criar servidor de teste: %v", err)
	}
	channel, err := services.CreateChannel(testCtx(), server.ID, "chan_"+randHex(8))
	if err != nil {
		t.Fatalf("falha ao criar canal de teste: %v", err)
	}
	return server, channel
}

// grantChannelReadPermission cria uma role com read_channel no canal e a
// atribui ao usuário (mesmo padrão dos testes de services).
func grantChannelReadPermission(t *testing.T, server models.Server, channel models.ChannelSummary, user models.User) {
	t.Helper()
	role, err := storage.CreateRole(testCtx(), server.ID, "role_"+randHex(8), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role de teste: %v", err)
	}
	if _, err := services.UpdateChannelPermissions(testCtx(), channel.ID, role.ID, models.ChannelPermission{ReadChannel: true}); err != nil {
		t.Fatalf("falha ao definir a permissão de leitura no canal: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), user.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir a role ao usuário: %v", err)
	}
}

// --- helpers de banco temporário (mesmo padrão dos demais pacotes de teste) ---

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

// --- testes ---

func TestEventTypeIsInbound(t *testing.T) {
	cases := []struct {
		name      string
		eventType websocket.EventType
		inbound   bool
	}{
		{"typing", websocket.EventTypeTyping, true},
		{"heartbeat", websocket.EventTypeHeartbeat, true},
		{"message", websocket.EventTypeMessage, false},
		{"message_edit", websocket.EventTypeMessageEdit, false},
		{"message_delete", websocket.EventTypeMessageDelete, false},
		{"channel_create", websocket.EventTypeChannelCreate, false},
		{"channel_update", websocket.EventTypeChannelUpdate, false},
		{"channel_delete", websocket.EventTypeChannelDelete, false},
		{"presence_update", websocket.EventTypePresenceUpdate, false},
		{"presence_sync", websocket.EventTypePresenceSync, false},
		{"heartbeat_ack", websocket.EventTypeHeartbeatAck, false},
		{"error", websocket.EventTypeError, false},
		{"vazio", websocket.EventType(""), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.eventType.IsInbound(); got != tc.inbound {
				t.Errorf("IsInbound() de %q = %v, esperado %v", tc.name, got, tc.inbound)
			}
		})
	}
}

func TestPresenceStore(t *testing.T) {
	t.Run("transições online/offline", func(t *testing.T) {
		p := websocket.NewPresenceStore()

		if !p.AddConnection("user_a", nil) {
			t.Error("a primeira conexão deveria transicionar para online")
		}
		if p.AddConnection("user_a", nil) {
			t.Error("a segunda conexão do mesmo usuário não deveria transicionar")
		}
		if p.RemoveConnection("user_a") {
			t.Error("removendo 1 de 2 conexões, o usuário não deveria ficar offline")
		}
		if !p.RemoveConnection("user_a") {
			t.Error("a última conexão deveria transicionar para offline")
		}
		if p.RemoveConnection("user_a") {
			t.Error("remover a conexão de um usuário desconhecido não deveria transicionar")
		}
	})

	t.Run("mensagem de status", func(t *testing.T) {
		p := websocket.NewPresenceStore()
		oi := "oi"

		if p.SetStatusMessage("user_a", &oi) {
			t.Error("SetStatusMessage em usuário offline deveria retornar false")
		}
		if got := p.StatusMessage("user_a"); got != nil {
			t.Errorf("StatusMessage de usuário offline deveria ser nil, obtive %v", got)
		}

		p.AddConnection("user_a", &oi)
		if got := p.StatusMessage("user_a"); got == nil || *got != "oi" {
			t.Errorf("StatusMessage deveria ser a mensagem da primeira conexão, obtive %v", got)
		}

		if !p.SetStatusMessage("user_a", nil) {
			t.Error("SetStatusMessage em usuário online deveria retornar true")
		}
		if got := p.StatusMessage("user_a"); got != nil {
			t.Errorf("StatusMessage deveria ter sido limpa, obtive %v", got)
		}
	})

	t.Run("OnlineMembers ordenado por user_id", func(t *testing.T) {
		p := websocket.NewPresenceStore()
		z := "status zeta"
		p.AddConnection("zeta", &z)
		p.AddConnection("alpha", nil)

		members := p.OnlineMembers()
		if len(members) != 2 {
			t.Fatalf("esperava 2 membros online, obtive %d", len(members))
		}
		if members[0].UserID != "alpha" || members[1].UserID != "zeta" {
			t.Errorf("membros deveriam estar ordenados por user_id: %+v", members)
		}
		for _, member := range members {
			if member.Status != websocket.StatusOnline {
				t.Errorf("membro online deveria ter status online: %+v", member)
			}
		}
		if members[1].StatusMessage == nil || *members[1].StatusMessage != "status zeta" {
			t.Errorf("StatusMessage do zeta inesperada: %v", members[1].StatusMessage)
		}

		p.RemoveConnection("alpha")
		p.RemoveConnection("zeta")
		if got := p.OnlineMembers(); len(got) != 0 {
			t.Errorf("esperava lista vazia após todos saírem, obtive %d membros", len(got))
		}
	})
}

func TestHubPresenceSyncOnConnect(t *testing.T) {
	status := "status de teste"
	hub := newWSHub(t)
	env := newWSTestServer(t, hub, map[string]*string{"user_a": &status})

	conn := env.dial(t, "user_a")
	event := readEvent(t, conn)
	if event.Type != string(websocket.EventTypePresenceSync) {
		t.Fatalf("esperava presence_sync ao conectar, obtive %q", event.Type)
	}
	if len(event.Members) != 1 {
		t.Fatalf("esperava 1 membro online, obtive %d", len(event.Members))
	}
	member := event.Members[0]
	if member.UserID != "user_a" || member.Status != websocket.StatusOnline {
		t.Errorf("membro inesperado no presence_sync: %+v", member)
	}
	if member.StatusMessage == nil || *member.StatusMessage != status {
		t.Errorf("status_message inesperado no presence_sync: %v", member.StatusMessage)
	}
}

func TestHubPresenceUpdateOnJoinAndLeave(t *testing.T) {
	hub := newWSHub(t)
	env := newWSTestServer(t, hub, nil)

	connA := env.dial(t, "user_a")
	readEvent(t, connA) // presence_sync

	connB := env.dial(t, "user_b")
	if event := readEvent(t, connB); len(event.Members) != 2 {
		t.Fatalf("presence_sync do user_b deveria conter 2 membros, obtive %d", len(event.Members))
	}

	// user_a é notificado da entrada do user_b.
	event := readEvent(t, connA)
	if event.Type != string(websocket.EventTypePresenceUpdate) || event.UserID != "user_b" || event.Status != websocket.StatusOnline {
		t.Errorf("esperava presence_update online do user_b, obtive %+v", event)
	}

	// user_b sai: user_a é notificado do offline.
	connB.Close()
	event = readEvent(t, connA)
	if event.Type != string(websocket.EventTypePresenceUpdate) || event.UserID != "user_b" || event.Status != websocket.StatusOffline {
		t.Errorf("esperava presence_update offline do user_b, obtive %+v", event)
	}
}

func TestHubMultipleConnectionsSameUser(t *testing.T) {
	status := "status original"
	hub := newWSHub(t)
	env := newWSTestServer(t, hub, map[string]*string{"user_a": &status})

	connB := env.dial(t, "user_b")
	readEvent(t, connB) // presence_sync

	connA1 := env.dial(t, "user_a")
	readEvent(t, connA1) // presence_sync
	readEvent(t, connB)  // presence_update user_a online

	// Segunda conexão do mesmo usuário: sem transição de presença.
	// A 1ª conexão (que não será lida novamente) observa as janelas sem
	// evento; a 2ª recebe apenas o presence_sync.
	connA2 := env.dial(t, "user_a")
	event := readEvent(t, connA2)
	if len(event.Members) != 2 {
		t.Fatalf("presence_sync da 2ª conexão deveria conter 2 membros, obtive %d", len(event.Members))
	}
	for _, member := range event.Members {
		if member.UserID == "user_a" && (member.StatusMessage == nil || *member.StatusMessage != status) {
			t.Errorf("a 2ª conexão deveria manter o status da 1ª, obtive %v", member.StatusMessage)
		}
	}
	expectNoEvent(t, connA1, wsNoEventTimeout) // nenhum presence_update online

	// Encerrar a 2ª conexão não tira o usuário do ar.
	connA2.Close()
	expectNoEvent(t, connA1, wsNoEventTimeout) // nenhum presence_update offline

	// Encerrar a última conexão notifica offline.
	connA1.Close()
	event = readEvent(t, connB)
	if event.Type != string(websocket.EventTypePresenceUpdate) || event.UserID != "user_a" || event.Status != websocket.StatusOffline {
		t.Errorf("esperava presence_update offline do user_a, obtive %+v", event)
	}
}

func TestHubBroadcast(t *testing.T) {
	hub := newWSHub(t)
	env := newWSTestServer(t, hub, nil)

	connA := env.dial(t, "user_a")
	readEvent(t, connA)
	connB := env.dial(t, "user_b")
	readEvent(t, connB)
	readEvent(t, connA) // presence_update user_b online

	hub.Broadcast(websocket.MessageOutbound{
		Type:      websocket.EventTypeMessage,
		ID:        "msg_1",
		ChannelID: "chan_1",
		AuthorID:  "user_a",
		Content:   "olá",
		CreatedAt: time.Now().UTC(),
	})

	for name, conn := range map[string]*ws.Conn{"user_a": connA, "user_b": connB} {
		t.Run(name, func(t *testing.T) {
			event := readEvent(t, conn)
			if event.Type != string(websocket.EventTypeMessage) {
				t.Fatalf("esperava evento message, obtive %q", event.Type)
			}
			if event.ID != "msg_1" || event.ChannelID != "chan_1" || event.AuthorID != "user_a" || event.Content != "olá" {
				t.Errorf("payload inesperado: %+v", event)
			}
		})
	}
}

func TestHubBroadcastToUsers(t *testing.T) {
	hub := newWSHub(t)
	env := newWSTestServer(t, hub, nil)

	connA := env.dial(t, "user_a")
	readEvent(t, connA)
	connB := env.dial(t, "user_b")
	readEvent(t, connB)
	readEvent(t, connA) // presence_update user_b online

	hub.BroadcastToUsers(websocket.MessageOutbound{
		Type:      websocket.EventTypeMessage,
		ID:        "msg_1",
		ChannelID: "chan_1",
		AuthorID:  "user_a",
		Content:   "só para o user_a",
		CreatedAt: time.Now().UTC(),
	}, map[string]bool{"user_a": true})

	event := readEvent(t, connA)
	if event.Type != string(websocket.EventTypeMessage) || event.ID != "msg_1" {
		t.Fatalf("user_a deveria receber o evento, obtive %+v", event)
	}

	// user_b não está no conjunto autorizado: nenhum evento.
	expectNoEvent(t, connB, wsNoEventTimeout)
}

func TestHubUpdateStatusMessage(t *testing.T) {
	hub := newWSHub(t)
	env := newWSTestServer(t, hub, nil)

	connA := env.dial(t, "user_a")
	readEvent(t, connA)
	connB := env.dial(t, "user_b")
	readEvent(t, connB)
	readEvent(t, connA) // presence_update user_b online

	newStatus := "novo status"
	if !hub.UpdateStatusMessage("user_b", &newStatus) {
		t.Fatal("UpdateStatusMessage em usuário online deveria retornar true")
	}
	event := readEvent(t, connA)
	if event.Type != string(websocket.EventTypePresenceUpdate) || event.UserID != "user_b" || event.Status != websocket.StatusOnline {
		t.Fatalf("esperava presence_update do user_b, obtive %+v", event)
	}
	if event.StatusMessage == nil || *event.StatusMessage != newStatus {
		t.Errorf("status_message inesperado: %v", event.StatusMessage)
	}

	if hub.UpdateStatusMessage("user_offline", &newStatus) {
		t.Error("UpdateStatusMessage em usuário offline deveria retornar false")
	}
	expectNoEvent(t, connA, wsNoEventTimeout)
}

func TestHubShutdownClosesConnections(t *testing.T) {
	hub := newWSHub(t)
	env := newWSTestServer(t, hub, nil)

	connA := env.dial(t, "user_a")
	readEvent(t, connA)
	connB := env.dial(t, "user_b")
	readEvent(t, connB)
	readEvent(t, connA) // presence_update user_b online

	hub.Shutdown()

	for name, conn := range map[string]*ws.Conn{"user_a": connA, "user_b": connB} {
		t.Run(name, func(t *testing.T) {
			// O desregistro de cada conexão dispara presence_update offline
			// para as demais antes do close frame: drena eventos até a
			// conexão ser encerrada.
			for {
				if err := conn.SetReadDeadline(time.Now().Add(wsReadTimeout)); err != nil {
					return // conexão já encerrada
				}
				_, _, err := conn.ReadMessage()
				if err != nil {
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						t.Fatal("a conexão não foi encerrada pelo Shutdown")
					}
					return
				}
			}
		})
	}

	if clients := hub.Clients(); len(clients) != 0 {
		t.Errorf("esperava hub vazio após o Shutdown, obtive %d clientes", len(clients))
	}
}

func TestClientSendAndUnregister(t *testing.T) {
	hub := newWSHub(t)
	env := newWSTestServer(t, hub, nil)

	conn := env.dial(t, "user_a")
	readEvent(t, conn) // presence_sync

	client := env.clientAt(t, 0)

	// Send enfileira e a WritePump entrega na conexão.
	if err := client.Send([]byte(`{"type":"custom_event"}`)); err != nil {
		t.Fatalf("Send em conexão ativa retornou erro: %v", err)
	}
	if event := readEvent(t, conn); event.Type != "custom_event" {
		t.Fatalf("esperava o evento enviado, obtive %q", event.Type)
	}

	// Unregister fecha o canal de envio quando o Hub processa: a WritePump
	// encerra a conexão em seguida. O envio no canal não garante o
	// processamento, então espera o encerramento ser observado pelo cliente
	// (que só acontece depois do fechamento do canal de envio).
	hub.Unregister(client)
	conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("a conexão deveria ter sido encerrada após o Unregister")
	}

	// Com o canal de envio fechado, Send retorna erro.
	if err := client.Send([]byte(`{"type":"depois"}`)); err == nil {
		t.Fatal("Send após o Unregister deveria retornar erro")
	}
}

func TestClientHandleInboundEvents(t *testing.T) {
	hub := newWSHub(t)
	env := newWSTestServer(t, hub, nil)

	conn := env.dial(t, "user_a")
	readEvent(t, conn) // presence_sync

	cases := []struct {
		name string
		raw  string
	}{
		{"json inválido", "isto não é json"},
		{"tipo ausente", `{"foo":"bar"}`},
		{"tipo outbound usado como inbound", `{"type":"message","id":"m1"}`},
		{"typing sem canal", `{"type":"typing"}`},
		{"typing com canal vazio", `{"type":"typing","channel_id":""}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sendRaw(t, conn, tc.raw)
			event := readEvent(t, conn)
			if event.Type != string(websocket.EventTypeError) {
				t.Fatalf("esperava evento error, obtive %q", event.Type)
			}
			if event.Message != "evento inválido" {
				t.Errorf("mensagem de erro inesperada: %q", event.Message)
			}
		})
	}

	t.Run("heartbeat responde com ack", func(t *testing.T) {
		sendRaw(t, conn, `{"type":"heartbeat"}`)
		if event := readEvent(t, conn); event.Type != string(websocket.EventTypeHeartbeatAck) {
			t.Errorf("esperava heartbeat_ack, obtive %q", event.Type)
		}
	})
}

func TestClientTyping(t *testing.T) {
	owner := newTestUser(t)
	alice := newTestUser(t)
	bob := newTestUser(t)

	t.Run("canal não encontrado", func(t *testing.T) {
		hub := newWSHub(t)
		env := newWSTestServer(t, hub, nil)

		conn := env.dial(t, alice.ID)
		readEvent(t, conn) // presence_sync

		sendTyping(t, conn, randUUID())
		event := readEvent(t, conn)
		if event.Type != string(websocket.EventTypeError) || event.Message != "canal não encontrado" {
			t.Errorf("esperava erro 'canal não encontrado', obtive %+v", event)
		}
	})

	t.Run("canal aberto", func(t *testing.T) {
		removeAllServersTest(t)
		_, channel := newTestServerChannel(t, &owner.ID)

		hub := newWSHub(t)
		env := newWSTestServer(t, hub, nil)

		connAlice := env.dial(t, alice.ID)
		readEvent(t, connAlice) // presence_sync
		connBob := env.dial(t, bob.ID)
		readEvent(t, connBob)   // presence_sync
		readEvent(t, connAlice) // presence_update user bob online

		sendTyping(t, connAlice, channel.ID)

		// Em canal aberto (sem roles) a leitura é livre: bob recebe o typing.
		assertTyping(t, readEvent(t, connBob), channel.ID, alice.ID)

		// O remetente também está no conjunto de leitores do canal.
		assertTyping(t, readEvent(t, connAlice), channel.ID, alice.ID)
	})

	t.Run("sem permissão", func(t *testing.T) {
		removeAllServersTest(t)
		server, channel := newTestServerChannel(t, &owner.ID)
		grantChannelReadPermission(t, server, channel, bob)

		hub := newWSHub(t)
		env := newWSTestServer(t, hub, nil)

		connAlice := env.dial(t, alice.ID)
		readEvent(t, connAlice) // presence_sync
		connBob := env.dial(t, bob.ID)
		readEvent(t, connBob)   // presence_sync
		readEvent(t, connAlice) // presence_update user bob online

		sendTyping(t, connAlice, channel.ID)

		event := readEvent(t, connAlice)
		if event.Type != string(websocket.EventTypeError) || event.Message != "sem permissão para o canal" {
			t.Errorf("esperava erro 'sem permissão para o canal', obtive %+v", event)
		}

		// O typing não autorizado não é distribuído.
		expectNoEvent(t, connBob, wsNoEventTimeout)
	})

	t.Run("leitura restrita", func(t *testing.T) {
		removeAllServersTest(t)
		server, channel := newTestServerChannel(t, &owner.ID)
		grantChannelReadPermission(t, server, channel, bob) // bob e o dono leem

		hub := newWSHub(t)
		env := newWSTestServer(t, hub, nil)

		connAlice := env.dial(t, alice.ID)
		readEvent(t, connAlice) // presence_sync
		connBob := env.dial(t, bob.ID)
		readEvent(t, connBob) // presence_sync
		connOwner := env.dial(t, owner.ID)
		readEvent(t, connOwner) // presence_sync
		readEvent(t, connAlice) // presence_update user bob online
		readEvent(t, connAlice) // presence_update user owner online
		readEvent(t, connBob)   // presence_update user owner online

		sendTyping(t, connBob, channel.ID)

		// Dono (permissão implícita) e remetente (role) recebem.
		assertTyping(t, readEvent(t, connOwner), channel.ID, bob.ID)
		assertTyping(t, readEvent(t, connBob), channel.ID, bob.ID)

		// alice não lê o canal: nenhum evento.
		expectNoEvent(t, connAlice, wsNoEventTimeout)
	})
}
