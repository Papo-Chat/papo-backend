package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"

	"papo/internal/middleware"
	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/utils"
	"papo/internal/websocket"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

// createWebhookRequest é o corpo de POST /webhooks.
type createWebhookRequest struct {
	ChannelID string `json:"channel_id"`
	Name      string `json:"name"`
}

// createWebhookResponse é a resposta de POST /webhooks: o token em texto puro
// aparece somente aqui (a única vez em que existe fora do hash armazenado).
type createWebhookResponse struct {
	models.Webhook
	Token string `json:"token"`
}

// webhookEditRequest é o corpo de PUT /webhooks/:webhook_id/messages/:message_id.
// Embeds ausentes preservam os embeds existentes (edição apenas de texto),
// como na edição de mensagem humana.
type webhookEditRequest struct {
	Content string                `json:"content"`
	Embeds  []services.EmbedInput `json:"embeds"`
}

// webhookIdentityRequest é o corpo de PATCH /webhooks/:webhook_id (identidade
// persistente). Campos ausentes preservam o valor atual; avatar "" remove o
// avatar; display_name "" remove o nickname (volta a usar o name).
type webhookIdentityRequest struct {
	DisplayName  *string `json:"display_name"`
	Avatar       *string `json:"avatar"`
	AvatarFormat string  `json:"avatar_format"`
}

// CreateWebhookHandler implementa POST /webhooks (JWT + manage_webhooks):
// cria um webhook em um canal text/voice e retorna o token de acesso.
func CreateWebhookHandler(baseURL string, c echo.Context) error {
	actor, _ := c.Get(middleware.UserIDContextKey).(string)

	var req createWebhookRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "JSON inválido")
	}

	webhook, token, err := services.CreateWebhook(c.Request().Context(), actor, req.ChannelID, req.Name)
	if err != nil {
		return webhookProblem(c, baseURL, err)
	}
	return c.JSON(http.StatusCreated, createWebhookResponse{Webhook: webhook, Token: token})
}

// ListWebhooksHandler implementa GET /webhooks (JWT + manage_webhooks):
// lista os webhooks ativos de todos os canais.
func ListWebhooksHandler(baseURL string, c echo.Context) error {
	items, err := services.ListWebhooks(c.Request().Context())
	if err != nil {
		return webhookProblem(c, baseURL, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"webhooks": items})
}

// UpdateWebhookHandler implementa PATCH /webhooks/:webhook_id (JWT +
// manage_webhooks): atualiza a identidade persistente do webhook (nickname e
// avatar base64, mesmas regras do avatar de usuário).
func UpdateWebhookHandler(baseURL string, c echo.Context) error {
	actor, _ := c.Get(middleware.UserIDContextKey).(string)

	var req webhookIdentityRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "JSON inválido")
	}

	webhook, err := services.UpdateWebhookIdentity(c.Request().Context(), actor, c.Param("webhook_id"),
		req.DisplayName, req.Avatar, req.AvatarFormat)
	if err != nil {
		return webhookProblem(c, baseURL, err)
	}
	return c.JSON(http.StatusOK, webhook)
}

// GetWebhookAvatarHandler implementa GET /webhooks/:webhook_id/avatar (público,
// sem JWT): serve o avatar persistente do webhook a partir do storage
// content-addressable, como GET /users/:user_id/avatar. Consumidores externos
// usam essa URL absoluta sem cookie de sessão; quando o webhook não tem
// avatar, 404.
func GetWebhookAvatarHandler(baseURL string, c echo.Context) error {
	webhookID := c.Param("webhook_id")
	if _, err := uuid.Parse(webhookID); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "webhook não encontrado")
	}

	media, filePath, err := services.GetWebhookAvatar(c.Request().Context(), webhookID)
	switch {
	case errors.Is(err, services.ErrWebhookNotFound),
		errors.Is(err, services.ErrAvatarNotFound),
		errors.Is(err, services.ErrMediaNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "avatar não encontrado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao buscar o avatar do webhook: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao buscar o avatar do webhook")
	}

	file, err := os.Open(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "avatar não encontrado")
	}
	if err != nil {
		utils.Errorf("request_id=%s falha ao abrir o avatar do webhook: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao buscar o avatar do webhook")
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		utils.Errorf("request_id=%s falha ao ler o avatar do webhook: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao buscar o avatar do webhook")
	}

	resp := c.Response()
	resp.Header().Set(echo.HeaderContentType, media.MimeType)
	resp.Header().Set(echo.HeaderContentDisposition, "inline")
	// A URL é por webhook_id (não por sha), então o cache precisa ser curto o
	// suficiente para refletir trocas de avatar.
	resp.Header().Set("Cache-Control", "public, max-age=3600")

	// ServeContent trata Range requests e If-Modified-Since (304).
	http.ServeContent(resp, c.Request(), webhookID, info.ModTime(), file)

	return nil
}

// DeleteWebhookHandler implementa DELETE /webhooks/:webhook_id (JWT +
// manage_webhooks): revoga o webhook (soft delete). As mensagens já enviadas
// mantêm a atribuição.
func DeleteWebhookHandler(baseURL string, c echo.Context) error {
	actor, _ := c.Get(middleware.UserIDContextKey).(string)
	if err := services.RevokeWebhook(c.Request().Context(), actor, c.Param("webhook_id")); err != nil {
		return webhookProblem(c, baseURL, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// webhookProblem mapeia os erros dos services de webhook para RFC 7807, com os
// mesmos status do fluxo equivalente de mensagens humanas.
func webhookProblem(c echo.Context, baseURL string, err error) error {
	switch {
	case errors.Is(err, services.ErrWebhookUnauthorized):
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Não autorizado", "webhook inválido")
	case errors.Is(err, services.ErrWebhookNotFound),
		errors.Is(err, services.ErrChannelNotFound),
		errors.Is(err, services.ErrMessageNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Não encontrado", "recurso inexistente")
	case errors.Is(err, services.ErrPermissionDenied),
		errors.Is(err, services.ErrDirectMessageBlocked):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado", "operação não permitida")
	case errors.Is(err, services.ErrInvalidInput),
		errors.Is(err, services.ErrInvalidEmbed),
		errors.Is(err, services.ErrInvalidExternalAuthor),
		errors.Is(err, services.ErrTooManyAttachments),
		errors.Is(err, services.ErrTooManyEmbeds),
		errors.Is(err, services.ErrAttachmentTooLarge):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", err.Error())
	default:
		utils.Errorf("request_id=%s falha em operação de webhook: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha no webhook")
	}
}

// webhookFromRequest extrai a credencial escopada do webhook do header
// Authorization. Somente "Bearer <token>" é aceito; cookie de sessão e JWT
// jamais autorizam execução de webhook.
func webhookFromRequest(baseURL string, c echo.Context) (models.Webhook, error) {
	raw := c.Request().Header.Get("Authorization")
	if !strings.HasPrefix(raw, "Bearer ") {
		return models.Webhook{}, services.ErrWebhookUnauthorized
	}
	token := strings.TrimPrefix(raw, "Bearer ")
	if token == "" {
		return models.Webhook{}, services.ErrWebhookUnauthorized
	}
	return services.AuthenticateWebhook(c.Request().Context(), c.Param("webhook_id"), token)
}

// ExecuteWebhookHandler implementa POST /webhooks/:webhook_id/messages:
// envio de mensagem com a credencial escopada do webhook (Bearer do token,
// nunca JWT de usuário). multipart/form-data com content, embeds (JSON),
// external_author (JSON de autoria temporária), external_message_id (chave de
// idempotência opcional), reply_to e attachments.
//
// Compartilha com mensagens humanas todo o fluxo pós-criação (WS message,
// embeds, moderação, notificações). Retry com a mesma external_message_id
// devolve a mensagem existente com 200, sem reprocessar side effects.
func ExecuteWebhookHandler(baseURL string, c echo.Context) error {
	webhook, err := webhookFromRequest(baseURL, c)
	if err != nil {
		return webhookProblem(c, baseURL, err)
	}

	if err := c.Request().ParseMultipartForm(110 << 20); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"corpo da requisição deve ser multipart/form-data válido")
	}

	content := c.FormValue("content")
	sourceID := c.FormValue("external_message_id")
	replyTo := c.FormValue("reply_to")

	var external *models.ExternalAuthor
	if raw := c.FormValue("external_author"); raw != "" {
		external = new(models.ExternalAuthor)
		if err := json.Unmarshal([]byte(raw), external); err != nil {
			return utils.SendProblem(c, baseURL, http.StatusBadRequest,
				"invalid-param", "Parâmetro inválido", "external_author inválido")
		}
	}

	embeds, err := parseEmbedsField(c.FormValue("embeds"))
	if err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo embeds deve ser um array JSON de embeds")
	}

	var attachments []services.AttachmentInput
	if c.Request().MultipartForm != nil {
		for _, fileHeader := range c.Request().MultipartForm.File["attachments"] {
			file, err := fileHeader.Open()
			if err != nil {
				return utils.SendProblem(c, baseURL, http.StatusBadRequest,
					"invalid-param", "Parâmetro inválido", "falha ao ler o attachment enviado")
			}
			defer file.Close()
			attachments = append(attachments, services.AttachmentInput{
				OriginalFileName: fileHeader.Filename,
				Content:          file,
			})
		}
	}

	message, created, err := services.CreateWebhookMessage(c.Request().Context(), webhook,
		external, sourceID, content, replyTo, attachments, embeds)
	if err != nil {
		return webhookProblem(c, baseURL, err)
	}

	// Retry idempotente: a mensagem original já distribuiu seus eventos; não
	// reprocessar broadcast, moderação ou notificações.
	if created {
		publishCreatedMessage(c, message, webhook.CreatedBy)
	}

	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	return c.JSON(status, message)
}

// EditWebhookMessageHandler implementa PUT /webhooks/:webhook_id/messages/:message_id:
// somente o webhook autor pode editar mensagem que ele mesmo enviou
// (verificação no service). O fluxo de eventos é idêntico ao da edição humana.
func EditWebhookMessageHandler(baseURL string, c echo.Context) error {
	webhook, err := webhookFromRequest(baseURL, c)
	if err != nil {
		return webhookProblem(c, baseURL, err)
	}

	var req webhookEditRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "JSON inválido")
	}

	message, err := services.EditWebhookMessage(c.Request().Context(), webhook, c.Param("message_id"), req.Content, req.Embeds)
	if err != nil {
		return webhookProblem(c, baseURL, err)
	}

	broadcastChannelEvent(c, message.ChannelID, websocket.MessageEditOutbound{
		Type:      websocket.EventTypeMessageEdit,
		ID:        message.ID,
		ChannelID: message.ChannelID,
		Content:   derefString(message.Content),
		EditedAt:  derefTime(message.EditedAt),
	})
	requestID := c.Request().Header.Get(echo.HeaderXRequestID)
	broadcastMessageEmbedsUpdate(c.Request().Context(), requestID, message.ChannelID, message.ID)
	go processEditedMessageEmbeds(context.Background(), requestID, message.ChannelID, message.ID,
		webhook.CreatedBy, derefString(message.Content), message.Embeds)

	return c.JSON(http.StatusOK, message)
}

// DeleteWebhookMessageHandler implementa DELETE /webhooks/:webhook_id/messages/:message_id:
// somente o webhook autor pode excluir mensagem que ele mesmo enviou.
func DeleteWebhookMessageHandler(baseURL string, c echo.Context) error {
	webhook, err := webhookFromRequest(baseURL, c)
	if err != nil {
		return webhookProblem(c, baseURL, err)
	}

	channelID, err := services.DeleteWebhookMessage(c.Request().Context(), webhook, c.Param("message_id"))
	if err != nil {
		return webhookProblem(c, baseURL, err)
	}

	broadcastChannelEvent(c, channelID, websocket.MessageDeleteOutbound{
		Type:      websocket.EventTypeMessageDelete,
		ID:        c.Param("message_id"),
		ChannelID: channelID,
	})
	return c.NoContent(http.StatusNoContent)
}
