package handlers

import (
	"errors"
	"net/http"

	"papo/internal/middleware"
	"papo/internal/services"
	"papo/internal/utils"

	"github.com/labstack/echo/v4"
)

type createDirectMessageRequest struct {
	UserID string `json:"user_id"`
}

func ListDirectMessagesHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado", "token de autenticação ausente, inválido ou expirado")
	}
	list, err := services.ListDirectConversations(c.Request().Context(), userID)
	if err != nil {
		utils.Errorf("request_id=%s falha ao listar DMs: %v", c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError, "internal", "Erro interno", "falha ao listar mensagens diretas")
	}
	return c.JSON(http.StatusOK, list)
}

func OpenDirectMessageHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado", "token de autenticação ausente, inválido ou expirado")
	}
	var req createDirectMessageRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}
	dm, created, err := services.OpenDirectConversation(c.Request().Context(), userID, req.UserID)
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "user_id inválido ou igual ao usuário autenticado")
	case errors.Is(err, services.ErrUserNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound, "not-found", "Recurso não encontrado", "usuário não encontrado")
	case errors.Is(err, services.ErrDirectMessageBlocked):
		return utils.SendProblem(c, baseURL, http.StatusForbidden, "dm-blocked", "Mensagem direta indisponível", "não é possível iniciar uma mensagem direta com este usuário")
	case err != nil:
		utils.Errorf("request_id=%s falha ao abrir DM: %v", c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError, "internal", "Erro interno", "falha ao abrir mensagem direta")
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	return c.JSON(status, dm)
}

func GetDirectMessageHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado", "token de autenticação ausente, inválido ou expirado")
	}
	dm, err := services.GetDirectConversation(c.Request().Context(), userID, c.Param("dm_id"))
	switch {
	case errors.Is(err, services.ErrDirectMessageNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound, "not-found", "Recurso não encontrado", "mensagem direta não encontrada")
	case errors.Is(err, services.ErrDirectMessageBlocked):
		return utils.SendProblem(c, baseURL, http.StatusForbidden, "dm-blocked", "Mensagem direta indisponível", "não é possível acessar esta mensagem direta")
	case err != nil:
		utils.Errorf("request_id=%s falha ao buscar DM: %v", c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError, "internal", "Erro interno", "falha ao buscar mensagem direta")
	}
	return c.JSON(http.StatusOK, dm)
}

func HideDirectMessageHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado", "token de autenticação ausente, inválido ou expirado")
	}
	err := services.HideDirectConversation(c.Request().Context(), userID, c.Param("dm_id"))
	switch {
	case errors.Is(err, services.ErrDirectMessageNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound, "not-found", "Recurso não encontrado", "mensagem direta não encontrada")
	case err != nil:
		utils.Errorf("request_id=%s falha ao ocultar DM: %v", c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError, "internal", "Erro interno", "falha ao ocultar mensagem direta")
	}
	return c.NoContent(http.StatusNoContent)
}
