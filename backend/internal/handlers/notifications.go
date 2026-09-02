package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"papo/internal/middleware"
	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/utils"
	"papo/internal/websocket"

	"github.com/labstack/echo/v4"
)

// updateChannelUserSettingRequest é o corpo de
// POST /channels/:channel_id/user/:user_id/settings.
type updateChannelUserSettingRequest struct {
	NotificationSettings string `json:"notification_settings"`
}

// UpdateChannelUserSettingHandler implementa
// POST /channels/:channel_id/user/:user_id/settings: define a configuração de
// notificação do usuário no canal (off/only_mentions/all). Somente o próprio
// usuário pode alterar a configuração.
func UpdateChannelUserSettingHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	channelID := c.Param("channel_id")
	targetID := c.Param("user_id")
	if channelID == "" || targetID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"channel_id e user_id são obrigatórios")
	}

	var req updateChannelUserSettingRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	setting, err := services.UpdateChannelUserSetting(c.Request().Context(), userID, channelID, targetID, req.NotificationSettings)
	switch {
	case errors.Is(err, services.ErrPermissionDenied):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado",
			"somente o próprio usuário pode alterar esta configuração")
	case errors.Is(err, services.ErrChannelNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "canal não encontrado")
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"notification_settings deve ser off, only_mentions ou all")
	case err != nil:
		utils.Errorf("request_id=%s falha ao atualizar a configuração de notificação: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao atualizar a configuração de notificação")
	}

	return c.JSON(http.StatusOK, setting)
}

// ListUserNotificationsHandler implementa GET /users/:user_id/notifications:
// lista as notificações do usuário (somente o próprio usuário), mais recentes
// primeiro, limite de 100. O parâmetro de query since é opcional: timestamp
// ISO 8601 para polling. last_id é opcional: id da última notificação da
// página anterior; usado com since como cursor exato (created_at, id).
func ListUserNotificationsHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	targetID := c.Param("user_id")
	if targetID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "user_id ausente")
	}

	var since *time.Time
	if value := c.QueryParam("since"); value != "" {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return utils.SendProblem(c, baseURL, http.StatusBadRequest,
				"invalid-param", "Parâmetro inválido",
				"since deve ser um timestamp ISO 8601")
		}
		since = &parsed
	}
	lastID := c.QueryParam("last_id")

	list, err := services.ListUserNotifications(c.Request().Context(), userID, targetID, since, lastID)
	switch {
	case errors.Is(err, services.ErrPermissionDenied):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado",
			"somente o próprio usuário pode listar as notificações")
	case err != nil:
		utils.Errorf("request_id=%s falha ao listar as notificações: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao listar as notificações")
	}

	return c.JSON(http.StatusOK, list)
}

// readNotificationRequest é o corpo de PUT /users/:user_id/read_notification.
type readNotificationRequest struct {
	NotificationIDs []string `json:"notification_ids"`
}

// ReadNotificationHandler implementa PUT /users/:user_id/read_notification:
// marca as notificações do usuário (1 a 1000 ids) como lidas. Somente o
// próprio usuário.
func ReadNotificationHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	targetID := c.Param("user_id")
	if targetID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "user_id ausente")
	}

	var req readNotificationRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	updated, err := services.MarkUserNotificationsRead(c.Request().Context(), userID, targetID, req.NotificationIDs)
	switch {
	case errors.Is(err, services.ErrPermissionDenied):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado",
			"somente o próprio usuário pode marcar as notificações como lidas")
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"notification_ids deve ter entre 1 e 1000 ids")
	case errors.Is(err, services.ErrNotificationNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "notificação não encontrada")
	case err != nil:
		utils.Errorf("request_id=%s falha ao marcar as notificações como lidas: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao marcar as notificações como lidas")
	}

	return c.JSON(http.StatusOK, map[string]int{"updated": updated})
}

// dispatchMessageNotifications processa em background as notificações de uma
// mensagem recém-criada (menções, replies e @everyone) e distribui os eventos
// new_notification em unicast. Best-effort: falhas são logadas e não afetam a
// mensagem já criada.
func dispatchMessageNotifications(ctx context.Context, requestID string, message models.Message) {
	deliveries := services.DispatchMessageNotifications(ctx, requestID, message)
	if len(deliveries) == 0 {
		return
	}

	hub := websocket.GetHub()
	authorID := derefString(message.AuthorID)
	for _, d := range deliveries {
		hub.SendToUser(d.UserID, websocket.NewNotificationOutbound{
			Type:           websocket.EventTypeNewNotification,
			ID:             d.EventID,
			UserID:         authorID,
			MessageID:      message.ID,
			MessageContent: d.MessageContent,
		})
	}
}
