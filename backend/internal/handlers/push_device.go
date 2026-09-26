package handlers

import (
	"errors"
	"net/http"

	"papo/internal/middleware"
	"papo/internal/services"
	"papo/internal/utils"

	"github.com/labstack/echo/v4"
)

// pushDeviceRequest é o corpo de PUT/DELETE /users/me/push-device.
// `provider` é opcional e padrão "fcm" (apenas FCM, que é o único provedor
// suportado pelo papo-push).
type pushDeviceRequest struct {
	Token      string `json:"token"`
	Platform   string `json:"platform"`
	DeviceName string `json:"device_name"`
	Provider   string `json:"provider"`
}

// RegisterPushDeviceHandler implementa PUT /users/me/push-device.
// Registro (upsert) do dispositivo do usuário autenticado. O user_id nunca
// vem do cliente — é o usuário autenticado.
func RegisterPushDeviceHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	var req pushDeviceRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	_, err := services.RegisterPushDevice(c.Request().Context(), userID, req.Token, req.Platform, req.Provider, req.DeviceName)
	switch {
	case errors.Is(err, services.ErrPushTokenInvalid):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo 'token' é obrigatório e inválido")
	case errors.Is(err, services.ErrPushPlatformInvalid):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo 'platform' deve ser android ou ios")
	case errors.Is(err, services.ErrPushDeviceNameInvalid):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo 'device_name' excede o tamanho máximo")
	case err != nil:
		utils.Errorf("request_id=%s falha ao registrar o dispositivo de push do usuário %s: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), userID, err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao registrar o dispositivo de push")
	}

	return c.NoContent(http.StatusNoContent)
}

// RemovePushDeviceHandler implementa DELETE /users/me/push-device.
// Remove o dispositivo do usuário autenticado por token.
func RemovePushDeviceHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	var req pushDeviceRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	err := services.RemovePushDevice(c.Request().Context(), userID, req.Token)
	switch {
	case errors.Is(err, services.ErrPushTokenInvalid):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo 'token' é obrigatório")
	case errors.Is(err, services.ErrPushDeviceNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "dispositivo não encontrado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao remover o dispositivo de push do usuário %s: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), userID, err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao remover o dispositivo de push")
	}

	return c.NoContent(http.StatusNoContent)
}
