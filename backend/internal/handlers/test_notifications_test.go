package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/websocket"

	ws "github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
)

// --- helpers de notificações ---

// setChannelNotificationSetting altera a configuração de notificação do
// usuário no canal via rota, falhando o teste se a resposta não for 200.
func setChannelNotificationSetting(t *testing.T, e *echo.Echo, token, channelID, userID, setting string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"notification_settings": setting})
	rec := do(t, e, http.MethodPost, "/channels/"+channelID+"/user/"+userID+"/settings", body, authCookie(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST settings: esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// getUserNotifications lista as notificações do usuário via rota.
func getUserNotifications(t *testing.T, e *echo.Echo, token, userID string) models.NotificationList {
	t.Helper()
	rec := do(t, e, http.MethodGet, "/users/"+userID+"/notifications", nil, authCookie(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET notifications: esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var list models.NotificationList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("falha ao decodificar notificações: %v", err)
	}
	return list
}

// waitForUserNotifications consulta a listagem do usuário até a quantidade
// esperada de notificações aparecer (o disparo roda em goroutine de
// background), falhando o teste se não chegar a tempo.
func waitForUserNotifications(t *testing.T, e *echo.Echo, token, userID string, want int) models.NotificationList {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		list := getUserNotifications(t, e, token, userID)
		if len(list.Notifications) == want {
			return list
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout aguardando %d notificações do usuário %s, obtive %d", want, userID, len(list.Notifications))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertNoUserNotification dá tempo para a goroutine de disparo assentar e
// garante que o usuário não recebeu notificações.
func assertNoUserNotification(t *testing.T, e *echo.Echo, token, userID string) {
	t.Helper()
	time.Sleep(500 * time.Millisecond)
	if list := getUserNotifications(t, e, token, userID); len(list.Notifications) != 0 {
		t.Errorf("esperava 0 notificações do usuário %s, obtive %d", userID, len(list.Notifications))
	}
}

// notificationIDForMessage retorna o id da notificação do usuário para a
// mensagem (o id da notificação difere do id da mensagem).
func notificationIDForMessage(t *testing.T, e *echo.Echo, token, userID, messageID string) string {
	t.Helper()
	for _, n := range getUserNotifications(t, e, token, userID).Notifications {
		if n.MessageID == messageID {
			return n.ID
		}
	}
	t.Fatalf("notificação da mensagem %s não encontrada", messageID)
	return ""
}

// createNotificationMessage cria uma mensagem do autor no canal e a
// notificação do usuário para ela, retornando a mensagem.
func createNotificationMessage(t *testing.T, channelID, authorID, userID, content string) models.Message {
	t.Helper()
	message, err := storage.CreateMessage(context.Background(), channelID, authorID, content, "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	if _, err := storage.CreateNotification(context.Background(), userID, message.ID); err != nil {
		t.Fatalf("falha ao criar notificação: %v", err)
	}
	return message
}

// --- POST /channels/:channel_id/user/:user_id/settings ---

// TestUpdateChannelUserSettingRouteOwn garante que o usuário autenticado
// altera a própria configuração de notificação no canal (upsert idempotente)
// e que a configuração efetiva aparece no GET /channels.
func TestUpdateChannelUserSettingRouteOwn(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))

	setChannelNotificationSetting(t, e, token, channel.ID, userID, "only_mentions")

	body, _ := json.Marshal(map[string]string{"notification_settings": "all"})
	rec := do(t, e, http.MethodPost, "/channels/"+channel.ID+"/user/"+userID+"/settings", body, authCookie(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var setting models.ChannelUserSetting
	if err := json.Unmarshal(rec.Body.Bytes(), &setting); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if setting.UserID != userID || setting.ChannelID != channel.ID || setting.NotificationSettings != "all" {
		t.Errorf("resposta inesperada: %+v", setting)
	}

	rec = do(t, e, http.MethodGet, "/channels", nil, authCookie(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /channels: esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Channels []models.ChannelSummary `json:"channels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar canais: %v", err)
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
	if found.NotificationSettings != "all" {
		t.Errorf("GET /channels: esperava notification_settings all, obtive %q", found.NotificationSettings)
	}
}

// TestUpdateChannelUserSettingRouteOtherUserForbidden garante que o usuário
// não altera a configuração de outro usuário.
func TestUpdateChannelUserSettingRouteOtherUserForbidden(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	otherID, _ := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))

	body, _ := json.Marshal(map[string]string{"notification_settings": "all"})
	rec := do(t, e, http.MethodPost, "/channels/"+channel.ID+"/user/"+otherID+"/settings", body, authCookie(token))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"somente o próprio usuário pode alterar esta configuração")
}

// TestUpdateChannelUserSettingRouteInvalidSetting garante que um valor fora do
// enum responde 400.
func TestUpdateChannelUserSettingRouteInvalidSetting(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))

	body, _ := json.Marshal(map[string]string{"notification_settings": "sempre"})
	rec := do(t, e, http.MethodPost, "/channels/"+channel.ID+"/user/"+userID+"/settings", body, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"notification_settings deve ser off, only_mentions ou all")
}

// TestUpdateChannelUserSettingRouteChannelNotFound garante que um canal
// inexistente responde 404.
func TestUpdateChannelUserSettingRouteChannelNotFound(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)

	body, _ := json.Marshal(map[string]string{"notification_settings": "all"})
	rec := do(t, e, http.MethodPost, "/channels/"+randUUID()+"/user/"+userID+"/settings", body, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "canal não encontrado")
}

// TestUpdateChannelUserSettingRouteInvalidBody garante que um corpo inválido
// responde 400.
func TestUpdateChannelUserSettingRouteInvalidBody(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))

	rec := do(t, e, http.MethodPost, "/channels/"+channel.ID+"/user/"+userID+"/settings", []byte("{invalido"), authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

// TestUpdateChannelUserSettingRouteUnauthenticated garante que a rota exige
// autenticação.
func TestUpdateChannelUserSettingRouteUnauthenticated(t *testing.T) {
	e := newApp()
	userID := randUUID()

	body, _ := json.Marshal(map[string]string{"notification_settings": "all"})
	rec := do(t, e, http.MethodPost, "/channels/"+randUUID()+"/user/"+userID+"/settings", body, nil)

	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

// --- GET /users/:user_id/notifications ---

// TestListUserNotificationsRouteUnauthenticated garante que a rota exige
// autenticação.
func TestListUserNotificationsRouteUnauthenticated(t *testing.T) {
	e := newApp()

	rec := do(t, e, http.MethodGet, "/users/"+randUUID()+"/notifications", nil, nil)

	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

// TestListUserNotificationsRouteOtherUserForbidden garante que o usuário não
// lista as notificações de outro usuário.
func TestListUserNotificationsRouteOtherUserForbidden(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)
	otherID, _ := registerAndLogin(t, e)

	rec := do(t, e, http.MethodGet, "/users/"+otherID+"/notifications", nil, authCookie(token))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"somente o próprio usuário pode listar as notificações")
}

// TestListUserNotificationsRouteEmpty garante que a listagem sem notificações
// responde 200 com lista vazia (nunca null).
func TestListUserNotificationsRouteEmpty(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	list := getUserNotifications(t, e, token, userID)
	if list.Notifications == nil {
		t.Fatal("esperava notifications como lista, obtive null")
	}
	if len(list.Notifications) != 0 {
		t.Errorf("esperava lista vazia, obtive %d notificações", len(list.Notifications))
	}
	if list.HasMore {
		t.Error("esperava has_more false, obtive true")
	}
}

// TestListUserNotificationsRouteInvalidSince garante que um since malformado
// responde 400.
func TestListUserNotificationsRouteInvalidSince(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	rec := do(t, e, http.MethodGet, "/users/"+userID+"/notifications?since=nao-e-data", nil, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"since deve ser um timestamp ISO 8601")
}

// TestListUserNotificationsRouteReturnsNotifications garante que a listagem
// retorna as notificações do usuário com os dados da mensagem (join) e em
// ordem decrescente.
func TestListUserNotificationsRouteReturnsNotifications(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))

	first := createNotificationMessage(t, channel.ID, userID, userID, "primeira")
	time.Sleep(10 * time.Millisecond)
	second := createNotificationMessage(t, channel.ID, userID, userID, "segunda")

	list := getUserNotifications(t, e, token, userID)
	if len(list.Notifications) != 2 {
		t.Fatalf("esperava 2 notificações, obtive %d", len(list.Notifications))
	}

	newest, oldest := list.Notifications[0], list.Notifications[1]
	if newest.MessageID != second.ID || oldest.MessageID != first.ID {
		t.Errorf("ordem inesperada: %+v", list.Notifications)
	}
	if newest.ChannelID != channel.ID {
		t.Errorf("esperava channel_id %q, obtive %q", channel.ID, newest.ChannelID)
	}
	if newest.AuthorID == nil || *newest.AuthorID != userID {
		t.Errorf("esperava author_id %q, obtive %v", userID, newest.AuthorID)
	}
	if newest.MessageContent != "segunda" {
		t.Errorf("esperava message_content %q, obtive %q", "segunda", newest.MessageContent)
	}
	if newest.Read {
		t.Error("esperava read false, obtive true")
	}
}

// TestListUserNotificationsRouteTruncatesContent garante que o preview do
// conteúdo é truncado a 512 caracteres (rune-safe).
func TestListUserNotificationsRouteTruncatesContent(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))

	longContent := strings.Repeat("ç", 600)
	createNotificationMessage(t, channel.ID, userID, userID, longContent)

	list := getUserNotifications(t, e, token, userID)
	if len(list.Notifications) != 1 {
		t.Fatalf("esperava 1 notificação, obtive %d", len(list.Notifications))
	}
	if got := list.Notifications[0].MessageContent; len([]rune(got)) != 512 {
		t.Errorf("esperava message_content com 512 caracteres, obtive %d", len([]rune(got)))
	}
}

// TestListUserNotificationsRouteSinceFilter garante que since (timestamp ISO
// 8601) retorna apenas notificações criadas após ele.
func TestListUserNotificationsRouteSinceFilter(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))

	first := createNotificationMessage(t, channel.ID, userID, userID, "primeira")
	time.Sleep(10 * time.Millisecond)
	second := createNotificationMessage(t, channel.ID, userID, userID, "segunda")

	// since usa o created_at da notificação (que difere do da mensagem)
	var firstCreatedAt *time.Time
	for _, n := range getUserNotifications(t, e, token, userID).Notifications {
		if n.MessageID == first.ID {
			ts := n.CreatedAt
			firstCreatedAt = &ts
		}
	}
	if firstCreatedAt == nil {
		t.Fatal("notificação da primeira mensagem não encontrada")
	}

	rec := do(t, e, http.MethodGet,
		"/users/"+userID+"/notifications?since="+firstCreatedAt.Format(time.RFC3339Nano), nil, authCookie(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var list models.NotificationList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("falha ao decodificar notificações: %v", err)
	}
	if len(list.Notifications) != 1 || list.Notifications[0].MessageID != second.ID {
		t.Errorf("esperava somente a notificação da segunda mensagem, obtive %+v", list.Notifications)
	}
}

// --- PUT /users/:user_id/read_notification ---

// TestReadNotificationRouteUnauthenticated garante que a rota exige
// autenticação.
func TestReadNotificationRouteUnauthenticated(t *testing.T) {
	e := newApp()

	body, _ := json.Marshal(map[string][]string{"notification_ids": {randUUID()}})
	rec := do(t, e, http.MethodPut, "/users/"+randUUID()+"/read_notification", body, nil)

	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado",
		"token de autenticação ausente, inválido ou expirado")
}

// TestReadNotificationRouteOtherUserForbidden garante que o usuário não marca
// as notificações de outro usuário como lidas.
func TestReadNotificationRouteOtherUserForbidden(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)
	otherID, _ := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string][]string{"notification_ids": {randUUID()}})
	rec := do(t, e, http.MethodPut, "/users/"+otherID+"/read_notification", body, authCookie(token))

	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"somente o próprio usuário pode marcar as notificações como lidas")
}

// TestReadNotificationRouteEmptyIDs garante que uma lista vazia responde 400.
func TestReadNotificationRouteEmptyIDs(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string][]string{"notification_ids": {}})
	rec := do(t, e, http.MethodPut, "/users/"+userID+"/read_notification", body, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"notification_ids deve ter entre 1 e 1000 ids")
}

// TestReadNotificationRouteTooManyIDs garante que mais de 1000 ids responde
// 400.
func TestReadNotificationRouteTooManyIDs(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	ids := make([]string, 0, 1001)
	for i := 0; i < 1001; i++ {
		ids = append(ids, randUUID())
	}
	body, _ := json.Marshal(map[string][]string{"notification_ids": ids})
	rec := do(t, e, http.MethodPut, "/users/"+userID+"/read_notification", body, authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"notification_ids deve ter entre 1 e 1000 ids")
}

// TestReadNotificationRouteInvalidBody garante que um corpo inválido responde
// 400.
func TestReadNotificationRouteInvalidBody(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	rec := do(t, e, http.MethodPut, "/users/"+userID+"/read_notification", []byte("{invalido"), authCookie(token))

	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
}

// TestReadNotificationRouteNotFound garante que ids inexistentes respondem
// 404.
func TestReadNotificationRouteNotFound(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	body, _ := json.Marshal(map[string][]string{"notification_ids": {randUUID()}})
	rec := do(t, e, http.MethodPut, "/users/"+userID+"/read_notification", body, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "notificação não encontrada")
}

// TestReadNotificationRouteMarksRead garante que a rota marca as
// notificações do usuário como lidas e responde o número de linhas afetadas.
func TestReadNotificationRouteMarksRead(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))

	first := createNotificationMessage(t, channel.ID, userID, userID, "primeira")
	second := createNotificationMessage(t, channel.ID, userID, userID, "segunda")

	body, _ := json.Marshal(map[string][]string{"notification_ids": {
		notificationIDForMessage(t, e, token, userID, first.ID),
		notificationIDForMessage(t, e, token, userID, second.ID),
	}})
	rec := do(t, e, http.MethodPut, "/users/"+userID+"/read_notification", body, authCookie(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Updated int `json:"updated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}
	if resp.Updated != 2 {
		t.Errorf("esperava updated 2, obtive %d", resp.Updated)
	}

	list := getUserNotifications(t, e, token, userID)
	for _, n := range list.Notifications {
		if !n.Read {
			t.Errorf("esperava a notificação %s marcada como lida", n.ID)
		}
	}
}

// TestReadNotificationRouteIgnoresOtherUsersNotifications garante que o
// usuário não marca como lidas notificações de outro usuário (a query
// restringe pelo user_id autenticado).
func TestReadNotificationRouteIgnoresOtherUsersNotifications(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)
	otherID, _ := registerAndLogin(t, e)
	createServerFor(t, userID)
	channel := createChannelFor(t, "chn_"+randHex(4))

	// notificação pertencente ao outro usuário
	notification, err := storage.CreateNotification(context.Background(), otherID,
		storageMustCreateMessage(t, channel.ID, otherID))
	if err != nil {
		t.Fatalf("falha ao criar notificação: %v", err)
	}

	// o usuário autenticado tenta marcar como lida a notificação do outro
	body, _ := json.Marshal(map[string][]string{"notification_ids": {notification.ID}})
	rec := do(t, e, http.MethodPut, "/users/"+userID+"/read_notification", body, authCookie(token))

	assertProblem(t, rec, http.StatusNotFound, "not-found", "Recurso não encontrado", "notificação não encontrada")
}

// storageMustCreateMessage cria uma mensagem via storage (helper de apoio).
func storageMustCreateMessage(t *testing.T, channelID, authorID string) string {
	t.Helper()
	message, err := storage.CreateMessage(context.Background(), channelID, authorID, "apoio", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem de apoio: %v", err)
	}
	return message.ID
}

// --- disparo de notificações (POST /messages) ---

// TestCreateMessageRouteMentionCreatesNotification garante que uma menção
// direta @<user_id> gera notificação persistida para o usuário mencionado
// (configuração padrão only_mentions).
func TestCreateMessageRouteMentionCreatesNotification(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	otherID, otherToken := registerAndLogin(t, e)

	rec := doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": "olá @" + otherID}, nil, authCookie(ownerToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var message models.MessageWithAttachment
	if err := json.Unmarshal(rec.Body.Bytes(), &message); err != nil {
		t.Fatalf("falha ao decodificar mensagem: %v", err)
	}

	list := waitForUserNotifications(t, e, otherToken, otherID, 1)
	if list.Notifications[0].MessageID != message.ID {
		t.Errorf("esperava notificação da mensagem %s, obtive %s", message.ID, list.Notifications[0].MessageID)
	}
	if list.Notifications[0].Read {
		t.Error("esperava notificação não lida")
	}
}

// TestCreateMessageRouteReplyToCreatesNotification garante que uma resposta
// (reply_to) gera notificação para o autor da mensagem referenciada.
func TestCreateMessageRouteReplyToCreatesNotification(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	otherID, otherToken := registerAndLogin(t, e)

	targetRec := doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": "mensagem alvo"}, nil, authCookie(otherToken))
	if targetRec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201 para mensagem alvo, obtive %d (corpo: %s)", targetRec.Code, targetRec.Body.String())
	}
	var target models.MessageWithAttachment
	if err := json.Unmarshal(targetRec.Body.Bytes(), &target); err != nil {
		t.Fatalf("falha ao decodificar mensagem alvo: %v", err)
	}

	rec := doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": "resposta", "reply_to": target.ID}, nil, authCookie(ownerToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var reply models.MessageWithAttachment
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("falha ao decodificar resposta: %v", err)
	}

	// a notificação aponta para a mensagem de resposta (nova), não para a
	// mensagem referenciada
	list := waitForUserNotifications(t, e, otherToken, otherID, 1)
	if list.Notifications[0].MessageID != reply.ID {
		t.Errorf("esperava notificação da mensagem %s, obtive %s", reply.ID, list.Notifications[0].MessageID)
	}
}

// TestCreateMessageRouteEveryoneCreatesNotifications garante que @everyone
// enviado pelo dono do servidor (permissão everyone_message implícita) gera
// notificação para os demais usuários do canal.
func TestCreateMessageRouteEveryoneCreatesNotifications(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	otherID, otherToken := registerAndLogin(t, e)

	rec := doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": "aviso @everyone"}, nil, authCookie(ownerToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var message models.MessageWithAttachment
	if err := json.Unmarshal(rec.Body.Bytes(), &message); err != nil {
		t.Fatalf("falha ao decodificar mensagem: %v", err)
	}

	list := waitForUserNotifications(t, e, otherToken, otherID, 1)
	if list.Notifications[0].MessageID != message.ID {
		t.Errorf("esperava notificação da mensagem %s, obtive %s", message.ID, list.Notifications[0].MessageID)
	}
}

// TestCreateMessageRouteEveryoneWithoutPermission garante que @everyone
// enviado por um usuário sem a permissão everyone_message não gera nenhuma
// notificação.
func TestCreateMessageRouteEveryoneWithoutPermission(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	_, otherToken := registerAndLogin(t, e)

	rec := doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": "aviso @everyone"}, nil, authCookie(otherToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	// o autor (dono do servidor, sem permissão everyone_message) não é
	// notificado
	assertNoUserNotification(t, e, ownerToken, ownerID)
}

// TestCreateMessageRouteOffSettingSuppressesNotification garante que um
// usuário com a configuração off não recebe notificação nem por menção.
func TestCreateMessageRouteOffSettingSuppressesNotification(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	otherID, otherToken := registerAndLogin(t, e)

	setChannelNotificationSetting(t, e, otherToken, channel.ID, otherID, "off")

	rec := doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": "olá @" + otherID}, nil, authCookie(ownerToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	assertNoUserNotification(t, e, otherToken, otherID)
}

// TestCreateMessageRouteAuthorNotNotified garante que o autor da mensagem
// nunca é notificado, nem quando se menciona.
func TestCreateMessageRouteAuthorNotNotified(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))

	rec := doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": "olá @" + ownerID}, nil, authCookie(ownerToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	assertNoUserNotification(t, e, ownerToken, ownerID)
}

// TestCreateMessageRouteAllSettingWithoutTriggerNoRow garante que um usuário
// com a configuração all recebe apenas o evento (sem trigger) e não gera row
// persistida.
func TestCreateMessageRouteAllSettingWithoutTriggerNoRow(t *testing.T) {
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	otherID, otherToken := registerAndLogin(t, e)

	setChannelNotificationSetting(t, e, otherToken, channel.ID, otherID, "all")

	rec := doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": "mensagem comum"}, nil, authCookie(ownerToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	assertNoUserNotification(t, e, otherToken, otherID)
}

// TestCreateMessageRouteSendsNewNotificationEvent garante que a menção
// dispara o evento new_notification em unicast para o usuário mencionado,
// com o autor da mensagem e o preview do conteúdo.
func TestCreateMessageRouteSendsNewNotificationEvent(t *testing.T) {
	e := newApp()
	go websocket.GetHub().Run()

	srv := httptest.NewServer(e)
	defer srv.Close()

	ownerID, ownerToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	otherID, otherToken := registerAndLogin(t, e)

	header := http.Header{}
	header.Set(echo.HeaderCookie, "Auth="+otherToken)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := ws.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("falha ao abrir conexão websocket: %v", err)
	}
	defer conn.Close()

	// primeiro evento: presence_sync da própria conexão
	readWSMessage(t, conn)

	content := "olá @" + otherID
	rec := doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": content}, nil, authCookie(ownerToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var message models.MessageWithAttachment
	if err := json.Unmarshal(rec.Body.Bytes(), &message); err != nil {
		t.Fatalf("falha ao decodificar mensagem: %v", err)
	}

	// o usuário também recebe o broadcast `message` do canal; lê eventos até
	// encontrar o new_notification (unicast).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data := readWSMessage(t, conn)
		var event struct {
			Type           string `json:"type"`
			ID             string `json:"id"`
			UserID         string `json:"user_id"`
			MessageID      string `json:"message_id"`
			MessageContent string `json:"message_content"`
		}
		if err := json.Unmarshal(data, &event); err != nil {
			t.Fatalf("falha ao decodificar evento: %v", err)
		}
		if event.Type != "new_notification" {
			continue
		}
		if event.UserID != ownerID {
			t.Errorf("esperava user_id do autor %q, obtive %q", ownerID, event.UserID)
		}
		if event.MessageID != message.ID {
			t.Errorf("esperava message_id %q, obtive %q", message.ID, event.MessageID)
		}
		if event.MessageContent != content {
			t.Errorf("esperava message_content %q, obtive %q", content, event.MessageContent)
		}
		if event.ID == "" {
			t.Error("esperava id preenchido no evento")
		}
		return
	}
	t.Fatal("timeout: evento new_notification não chegou")
}
