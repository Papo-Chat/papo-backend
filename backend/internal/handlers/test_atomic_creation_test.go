package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/websocket"

	ws "github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
)

// --- criação atômica: categoria, acesso inicial e membros iniciais ---

// openEventSocket abre o websocket do usuário e descarta o presence_sync da
// própria conexão, para o próximo evento lido ser o da ação do teste.
func openEventSocket(t *testing.T, e *echo.Echo, token string) *ws.Conn {
	t.Helper()
	RegisterWebSocketRoutes(e, config.LoadConfig())
	go websocket.GetHub().Run()

	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)

	header := http.Header{}
	header.Set(echo.HeaderCookie, "Auth="+token)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := ws.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("falha ao abrir conexão websocket: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	readWSMessage(t, conn)
	return conn
}

// channelEvent é o que os testes leem de channel_create e channel_update.
type channelEvent struct {
	Type      string  `json:"type"`
	ChannelID string  `json:"channel_id"`
	ParentID  *string `json:"parent_id"`
}

func readChannelEvent(t *testing.T, conn *ws.Conn, eventType string) channelEvent {
	t.Helper()
	data := readWSMessage(t, conn)
	var event channelEvent
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("falha ao decodificar evento: %v", err)
	}
	if event.Type != eventType {
		t.Fatalf("esperava tipo %s, obtive %q (corpo: %s)", eventType, event.Type, data)
	}
	return event
}

func createCategoryFor(t *testing.T) models.Channel {
	t.Helper()
	category, err := storage.CreateChannel(context.Background(), "cat_"+randHex(4), "category", "")
	if err != nil {
		t.Fatalf("falha ao criar categoria: %v", err)
	}
	return category
}

// TestCreateChannelInCategoryWithAccessRoles garante que POST /channels com
// parent_id e access_roles cria o canal já dentro da categoria e já
// restrito às roles: a resposta e o channel_create trazem a categoria, a
// role recebe ler/enviar/voz, e quem não tem a role não lê o canal.
func TestCreateChannelInCategoryWithAccessRoles(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	_, memberToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	category := createCategoryFor(t)
	role := createRoleFor(t, models.RolePermissions{})
	conn := openEventSocket(t, e, ownerToken)

	body, _ := json.Marshal(map[string]any{
		"name":         "restrito_" + randHex(4),
		"type":         "text",
		"parent_id":    category.ID,
		"access_roles": []string{role.ID, role.ID},
	})
	rec := do(t, e, http.MethodPost, "/channels", body, authCookie(ownerToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var created models.ChannelSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("falha ao decodificar canal: %v", err)
	}
	if created.ParentID == nil || *created.ParentID != category.ID {
		t.Fatalf("esperava parent_id %s, obtive %v", category.ID, created.ParentID)
	}
	want := models.ChannelPermission{ReadChannel: true, SendMessages: true, ConnectVoice: true}
	if len(created.Permissions) != 1 || created.Permissions[0].RoleID != role.ID ||
		created.Permissions[0].Permissions != want {
		t.Fatalf("esperava só a role %s com %+v, obtive %+v", role.ID, want, created.Permissions)
	}

	event := readChannelEvent(t, conn, "channel_create")
	if event.ChannelID != created.ID || event.ParentID == nil || *event.ParentID != category.ID {
		t.Errorf("channel_create sem a categoria: %+v", event)
	}

	rec = do(t, e, http.MethodGet, "/channels/"+created.ID+"/messages", nil, authCookie(memberToken))
	if rec.Code != http.StatusForbidden {
		t.Errorf("quem não tem a role não deveria ler o canal: status %d", rec.Code)
	}
}

// TestCreateChannelWithMissingReferenceCreatesNothing garante que uma role
// de access_roles ou uma categoria inexistente responde 404 sem criar o
// canal: antes, o canal era criado aberto e a restrição falhava depois.
func TestCreateChannelWithMissingReferenceCreatesNothing(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	notCategory := createChannelFor(t, "chn_"+randHex(4))

	for name, fields := range map[string]map[string]any{
		"role inexistente":        {"access_roles": []string{randUUID()}},
		"categoria inexistente":   {"parent_id": randUUID()},
		"pai que não é categoria": {"parent_id": notCategory.ID},
	} {
		channelName := "falha_" + randHex(4)
		fields["name"] = channelName
		body, _ := json.Marshal(fields)
		rec := do(t, e, http.MethodPost, "/channels", body, authCookie(ownerToken))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: esperava status 404, obtive %d (corpo: %s)", name, rec.Code, rec.Body.String())
		}
		var count int
		if err := storage.GetDB().QueryRowContext(context.Background(),
			"SELECT count(*) FROM channels WHERE name = $1", channelName,
		).Scan(&count); err != nil {
			t.Fatalf("falha ao contar canais: %v", err)
		}
		if count != 0 {
			t.Errorf("%s: o canal foi criado mesmo com erro", name)
		}
	}
}

// TestChangeChannelPositionChangesCategory garante que parent_id em
// PUT /channels/:channel_id/change_position coloca e tira o canal da
// categoria, e que o channel_update traz a categoria nova: mover de
// categoria sem mudar de posição não mudaria nenhum outro campo do evento.
// Uma categoria dentro de outra é recusada com 400.
func TestChangeChannelPositionChangesCategory(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	category := createCategoryFor(t)
	channel := createChannelFor(t, "chn_"+randHex(4))
	conn := openEventSocket(t, e, ownerToken)

	move := func(target models.Channel, parentID string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{
			"old_position": target.Position,
			"new_position": target.Position,
			"parent_id":    parentID,
		})
		return do(t, e, http.MethodPut, "/channels/"+target.ID+"/change_position", body, authCookie(ownerToken))
	}

	if rec := move(channel, category.ID); rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if event := readChannelEvent(t, conn, "channel_update"); event.ParentID == nil || *event.ParentID != category.ID {
		t.Errorf("channel_update sem a categoria nova: %+v", event)
	}

	if rec := move(channel, ""); rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	if event := readChannelEvent(t, conn, "channel_update"); event.ParentID != nil {
		t.Errorf("channel_update deveria trazer parent_id null: %+v", event)
	}

	other := createCategoryFor(t)
	if rec := move(other, category.ID); rec.Code != http.StatusBadRequest {
		t.Errorf("categoria dentro de categoria: esperava 400, obtive %d", rec.Code)
	}
}

// TestCreateRoleWithUsersBroadcastsRoleAdd garante que POST /roles com
// User cria a role já atribuída, distribui um role_add por pessoa (como
// POST /users/:user_id/roles) e conta repetições uma vez só.
func TestCreateRoleWithUsersBroadcastsRoleAdd(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	firstID, _ := registerAndLogin(t, e)
	secondID, _ := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	conn := openEventSocket(t, e, ownerToken)

	body, _ := json.Marshal(map[string]any{
		"name":   "role_" + randHex(4),
		"user": []string{firstID, secondID, firstID},
	})
	rec := do(t, e, http.MethodPost, "/roles", body, authCookie(ownerToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var role models.Role
	if err := json.Unmarshal(rec.Body.Bytes(), &role); err != nil {
		t.Fatalf("falha ao decodificar role: %v", err)
	}

	assertRoleEvent(t, conn, "role_add", firstID, role.ID)
	assertRoleEvent(t, conn, "role_add", secondID, role.ID)

	members, err := storage.GetUsersByRole(context.Background(), role.ID)
	if err != nil {
		t.Fatalf("falha ao listar membros da role: %v", err)
	}
	if len(members) != 2 {
		t.Errorf("esperava 2 membros, obtive %v", members)
	}
}

// TestCreateRoleWithMissingPersonCreatesNothing garante que uma pessoa
// inexistente em User responde 404 e não cria a role: antes, a role era
// criada e ficava só com as atribuições que deram certo.
func TestCreateRoleWithMissingPersonCreatesNothing(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	memberID, _ := registerAndLogin(t, e)
	createServerFor(t, ownerID)

	roleName := "role_" + randHex(4)
	body, _ := json.Marshal(map[string]any{
		"name":   roleName,
		"user": []string{memberID, randUUID()},
	})
	rec := do(t, e, http.MethodPost, "/roles", body, authCookie(ownerToken))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("esperava status 404, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	var count int
	if err := storage.GetDB().QueryRowContext(context.Background(),
		"SELECT count(*) FROM roles WHERE name = $1", roleName,
	).Scan(&count); err != nil {
		t.Fatalf("falha ao contar roles: %v", err)
	}
	if count != 0 {
		t.Error("a role foi criada mesmo com uma pessoa inexistente")
	}
}
