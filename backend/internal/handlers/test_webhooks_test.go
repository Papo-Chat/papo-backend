package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"papo/internal/models"

	"github.com/labstack/echo/v4"
)

// doWithBearer executa uma requisição JSON com um header Authorization
// explícito (usado pelos endpoints de execução de webhook, que não aceitam
// cookie de sessão).
func doWithBearer(t *testing.T, e *echo.Echo, method, path string, body []byte, bearer string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// doMultipartWithBearer executa uma requisição multipart com header
// Authorization explícito (envio de mensagem via webhook).
func doMultipartWithBearer(t *testing.T, e *echo.Echo, method, path string, fields map[string]string, files map[string][][2]string, bearer string) *httptest.ResponseRecorder {
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
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// createWebhookViaRoute cria um webhook pela rota de gestão (JWT) e retorna o
// webhook e o token de execução.
func createWebhookViaRoute(t *testing.T, e *echo.Echo, token, channelID, name string) (models.Webhook, string) {
	t.Helper()

	body, _ := json.Marshal(map[string]string{"channel_id": channelID, "name": name})
	rec := do(t, e, http.MethodPost, "/webhooks", body, authCookie(token))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /webhooks: esperava 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		models.Webhook
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("POST /webhooks: falha ao decodificar: %v", err)
	}
	if resp.ID == "" || resp.Token == "" {
		t.Fatalf("POST /webhooks: id/token vazios: %s", rec.Body.String())
	}
	return resp.Webhook, resp.Token
}

// webhookTestServer cria o servidor único e retorna o token do dono.
func webhookTestServer(t *testing.T, e *echo.Echo) string {
	t.Helper()

	_, ownerToken := registerAndLogin(t, e)
	body, _ := json.Marshal(map[string]any{"name": "srv_" + randHex(4), "Public": true})
	if rec := do(t, e, http.MethodPost, "/server", body, authCookie(ownerToken)); rec.Code != http.StatusOK {
		t.Fatalf("POST /server: esperava 200, obtive %d", rec.Code)
	}
	return ownerToken
}

// TestWebhookManagementRequiresManageWebhooks garante que a gestão de webhooks
// exige a permissão manage_webhooks (ou ser dono do servidor): manage_channels
// sozinho não basta, e a role correta autoriza.
func TestWebhookManagementRequiresManageWebhooks(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	ownerToken := webhookTestServer(t, e)

	channel := createChannelFor(t, "ch_wh_"+randHex(4))

	// Usuário comum sem roles: 403.
	plainID, plainToken := registerAndLogin(t, e)
	body, _ := json.Marshal(map[string]string{"channel_id": channel.ID, "name": "hook"})
	assertProblem(t, do(t, e, http.MethodPost, "/webhooks", body, authCookie(plainToken)),
		http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação")

	// manage_channels não concede manage_webhooks.
	chanRoleID := createRoleFor(t, models.RolePermissions{ManageChannels: true}).ID
	assignRoleToUser(t, plainID, chanRoleID)
	assertProblem(t, do(t, e, http.MethodPost, "/webhooks", body, authCookie(plainToken)),
		http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação")

	// manage_webhooks autoriza.
	hookRoleID := createRoleFor(t, models.RolePermissions{ManageWebhooks: true}).ID
	assignRoleToUser(t, plainID, hookRoleID)
	createWebhookViaRoute(t, e, plainToken, channel.ID, "hook")

	// Dono do servidor sempre pode.
	createWebhookViaRoute(t, e, ownerToken, channel.ID, "hook-owner")
}

// TestWebhookExecuteAuthentication garante que a execução aceita somente o
// token Bearer do webhook: token errado, ausente ou cookie de sessão no lugar
// do token resultam em 401.
func TestWebhookExecuteAuthentication(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	ownerToken := webhookTestServer(t, e)
	channel := createChannelFor(t, "ch_wh_"+randHex(4))
	hook, hookToken := createWebhookViaRoute(t, e, ownerToken, channel.ID, "auth-hook")

	// Token válido: 201 com atribuição externa na resposta.
	rec := doMultipartWithBearer(t, e, http.MethodPost, "/webhooks/"+hook.ID+"/messages",
		map[string]string{"content": "olá via webhook", "external_message_id": "auth-1"}, nil, hookToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("esperava 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var msg models.MessageWithAttachment
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("falha ao decodificar mensagem: %v", err)
	}
	if msg.WebhookID == nil || *msg.WebhookID != hook.ID {
		t.Fatalf("webhook_id ausente na resposta: %s", rec.Body.String())
	}
	if msg.ExternalAuthor == nil || msg.ExternalAuthor.Name != "auth-hook" {
		t.Fatalf("external_author ausente na resposta: %+v", msg.ExternalAuthor)
	}

	// Token errado.
	rec = doMultipartWithBearer(t, e, http.MethodPost, "/webhooks/"+hook.ID+"/messages",
		map[string]string{"content": "x"}, nil, "token-errado")
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Não autorizado", "webhook inválido")

	// Sem Authorization.
	rec = doMultipartWithBearer(t, e, http.MethodPost, "/webhooks/"+hook.ID+"/messages",
		map[string]string{"content": "x"}, nil, "")
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Não autorizado", "webhook inválido")

	// Cookie de sessão no lugar do token Bearer: execução não aceita sessão.
	rec = doMultipart(t, e, http.MethodPost, "/webhooks/"+hook.ID+"/messages",
		map[string]string{"content": "x"}, nil, authCookie(ownerToken))
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized", "Não autorizado", "webhook inválido")
}

// TestWebhookIdempotentRetryRoute garante que retry com a mesma
// external_message_id devolve 200 com a mensagem original, e que o primeiro
// envio devolve 201.
func TestWebhookIdempotentRetryRoute(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	ownerToken := webhookTestServer(t, e)
	channel := createChannelFor(t, "ch_wh_"+randHex(4))
	hook, hookToken := createWebhookViaRoute(t, e, ownerToken, channel.ID, "retry-hook")

	first := doMultipartWithBearer(t, e, http.MethodPost, "/webhooks/"+hook.ID+"/messages",
		map[string]string{"content": "primeira", "external_message_id": "dup-1"}, nil, hookToken)
	if first.Code != http.StatusCreated {
		t.Fatalf("primeiro envio: esperava 201, obtive %d", first.Code)
	}
	var firstMsg models.Message
	if err := json.Unmarshal(first.Body.Bytes(), &firstMsg); err != nil {
		t.Fatalf("decodificar primeira: %v", err)
	}

	retry := doMultipartWithBearer(t, e, http.MethodPost, "/webhooks/"+hook.ID+"/messages",
		map[string]string{"content": "retry", "external_message_id": "dup-1"}, nil, hookToken)
	if retry.Code != http.StatusOK {
		t.Fatalf("retry: esperava 200, obtive %d (corpo: %s)", retry.Code, retry.Body.String())
	}
	var retryMsg models.Message
	if err := json.Unmarshal(retry.Body.Bytes(), &retryMsg); err != nil {
		t.Fatalf("decodificar retry: %v", err)
	}
	if retryMsg.ID != firstMsg.ID {
		t.Fatalf("retry deveria devolver a mesma mensagem: %s != %s", retryMsg.ID, firstMsg.ID)
	}
}

// TestWebhookUploadUsesUploadLimit é a regressão do limite de corpo: o upload
// de webhook (multipart até 100MB) não pode ser barrado pelo limite global de
// 4MB JSON. Um anexo de ~5MB deve ser aceito.
func TestWebhookUploadUsesUploadLimit(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	ownerToken := webhookTestServer(t, e)
	channel := createChannelFor(t, "ch_wh_"+randHex(4))
	hook, hookToken := createWebhookViaRoute(t, e, ownerToken, channel.ID, "upload-hook")

	big := make([]byte, 5<<20)
	rec := doMultipartWithBearer(t, e, http.MethodPost, "/webhooks/"+hook.ID+"/messages",
		map[string]string{"content": "com anexo grande", "external_message_id": "big-1"},
		map[string][][2]string{"attachments": {{"big.bin", string(big)}}}, hookToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload de 5MB via webhook: esperava 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var msg models.MessageWithAttachment
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("decodificar: %v", err)
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("esperava 1 attachment, obtive %d", len(msg.Attachments))
	}
}

// TestWebhookIdentityRoutes cobre PATCH (nickname + avatar base64), o avatar
// público GET /webhooks/:id/avatar e a atribuição do avatar em mensagens.
func TestWebhookIdentityRoutes(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	ownerToken := webhookTestServer(t, e)
	channel := createChannelFor(t, "ch_wh_"+randHex(4))
	hook, hookToken := createWebhookViaRoute(t, e, ownerToken, channel.ID, "identity-hook")

	// Sem avatar: GET público responde 404.
	assertProblem(t, do(t, e, http.MethodGet, "/webhooks/"+hook.ID+"/avatar", nil, nil),
		http.StatusNotFound, "not-found", "Recurso não encontrado", "avatar não encontrado")

	// PATCH identidade: nickname + avatar.
	avatar := base64.StdEncoding.EncodeToString(pngAvatarBytes(64, 64))
	patch, _ := json.Marshal(map[string]any{"display_name": "Bot Oficial", "avatar": avatar, "avatar_format": "png"})
	rec := do(t, e, http.MethodPatch, "/webhooks/"+hook.ID, patch, authCookie(ownerToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH /webhooks/:id: esperava 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
	var updated models.Webhook
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decodificar webhook: %v", err)
	}
	if updated.DisplayName == nil || *updated.DisplayName != "Bot Oficial" {
		t.Fatalf("display_name não atualizado: %s", rec.Body.String())
	}

	// Avatar público sem JWT.
	rec = do(t, e, http.MethodGet, "/webhooks/"+hook.ID+"/avatar", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET avatar: esperava 200, obtive %d", rec.Code)
	}
	if ct := rec.Header().Get(echo.HeaderContentType); ct != "image/png" {
		t.Fatalf("Content-Type do avatar: %q", ct)
	}

	// Mensagem carrega nickname + caminho do avatar na autoria.
	rec = doMultipartWithBearer(t, e, http.MethodPost, "/webhooks/"+hook.ID+"/messages",
		map[string]string{"content": "identidade aplicada", "external_message_id": "id-1"}, nil, hookToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("envio: esperava 201, obtive %d", rec.Code)
	}
	var msg models.MessageWithAttachment
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("decodificar mensagem: %v", err)
	}
	if msg.ExternalAuthor == nil || msg.ExternalAuthor.Name != "Bot Oficial" ||
		msg.ExternalAuthor.AvatarURL != "/webhooks/"+hook.ID+"/avatar" {
		t.Fatalf("identidade persistente não aplicada na mensagem: %+v", msg.ExternalAuthor)
	}

	// Autoria temporária sobrescreve o nickname, mantém o avatar persistente.
	external, _ := json.Marshal(models.ExternalAuthor{Source: "discord", ID: "9", Name: "Externo"})
	rec = doMultipartWithBearer(t, e, http.MethodPost, "/webhooks/"+hook.ID+"/messages",
		map[string]string{"content": "autoria temporária", "external_message_id": "id-2", "external_author": string(external)}, nil, hookToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("envio com external_author: esperava 201, obtive %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("decodificar mensagem: %v", err)
	}
	if msg.ExternalAuthor.Source != "discord" || msg.ExternalAuthor.Name != "Externo" ||
		msg.ExternalAuthor.AvatarURL != "/webhooks/"+hook.ID+"/avatar" {
		t.Fatalf("autoria temporária incorreta: %+v", msg.ExternalAuthor)
	}

	// Gestão exige manage_webhooks (usuário comum não patcheia).
	_, plainToken := registerAndLogin(t, e)
	assertProblem(t, do(t, e, http.MethodPatch, "/webhooks/"+hook.ID, patch, authCookie(plainToken)),
		http.StatusForbidden, "forbidden", "Acesso negado",
		"usuário não possui a permissão necessária para esta operação")
}

// TestWebhookEditDeleteOwnershipRoute garante que a execução de edição/exclusão
// por token só funciona para o webhook autor da mensagem.
func TestWebhookEditDeleteOwnershipRoute(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	ownerToken := webhookTestServer(t, e)
	channel := createChannelFor(t, "ch_wh_"+randHex(4))
	hookA, tokenA := createWebhookViaRoute(t, e, ownerToken, channel.ID, "hook-a")
	hookB, tokenB := createWebhookViaRoute(t, e, ownerToken, channel.ID, "hook-b")

	rec := doMultipartWithBearer(t, e, http.MethodPost, "/webhooks/"+hookA.ID+"/messages",
		map[string]string{"content": "original", "external_message_id": "own-r-1"}, nil, tokenA)
	if rec.Code != http.StatusCreated {
		t.Fatalf("envio: %d", rec.Code)
	}
	var msg models.Message
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("decodificar: %v", err)
	}

	// Outro webhook não edita.
	editBody, _ := json.Marshal(map[string]any{"content": "sequestrada"})
	assertProblem(t, doWithBearer(t, e, http.MethodPut, "/webhooks/"+hookB.ID+"/messages/"+msg.ID, editBody, tokenB),
		http.StatusForbidden, "forbidden", "Acesso negado", "operação não permitida")

	// O autor edita.
	rec = doWithBearer(t, e, http.MethodPut, "/webhooks/"+hookA.ID+"/messages/"+msg.ID, editBody, tokenA)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT do autor: esperava 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	// Outro webhook não exclui; o autor exclui.
	assertProblem(t, doWithBearer(t, e, http.MethodDelete, "/webhooks/"+hookB.ID+"/messages/"+msg.ID, nil, tokenB),
		http.StatusForbidden, "forbidden", "Acesso negado", "operação não permitida")
	rec = doWithBearer(t, e, http.MethodDelete, "/webhooks/"+hookA.ID+"/messages/"+msg.ID, nil, tokenA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE do autor: esperava 204, obtive %d", rec.Code)
	}
}
