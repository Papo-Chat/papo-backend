package handlers

import (
	"errors"
	"net/http"

	"papo/internal/middleware"
	"papo/internal/services"
	"papo/internal/utils"

	"github.com/labstack/echo/v4"
)

func ListUserBlocksHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado", "token de autenticação ausente, inválido ou expirado")
	}
	list, err := services.ListUserBlocks(c.Request().Context(), userID)
	if err != nil {
		utils.Errorf("request_id=%s falha ao listar bloqueios: %v", c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError, "internal", "Erro interno", "falha ao listar usuários bloqueados")
	}
	return c.JSON(http.StatusOK, list)
}

func BlockUserHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado", "token de autenticação ausente, inválido ou expirado")
	}
	err := services.BlockUser(c.Request().Context(), userID, c.Param("user_id"))
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "user_id inválido ou igual ao usuário autenticado")
	case errors.Is(err, services.ErrUserNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound, "not-found", "Recurso não encontrado", "usuário não encontrado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao bloquear usuário: %v", c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError, "internal", "Erro interno", "falha ao bloquear usuário")
	}
	return c.NoContent(http.StatusNoContent)
}

func UnblockUserHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized, "unauthorized", "Token inválido ou expirado", "token de autenticação ausente, inválido ou expirado")
	}
	err := services.UnblockUser(c.Request().Context(), userID, c.Param("user_id"))
	if errors.Is(err, services.ErrInvalidInput) {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "user_id inválido ou igual ao usuário autenticado")
	}
	if err != nil {
		utils.Errorf("request_id=%s falha ao desbloquear usuário: %v", c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError, "internal", "Erro interno", "falha ao desbloquear usuário")
	}
	return c.NoContent(http.StatusNoContent)
}
