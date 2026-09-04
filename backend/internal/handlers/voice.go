package handlers

import (
	"net/http"

	"papo/internal/middleware"
	"papo/internal/services"
	"papo/internal/utils"

	"github.com/labstack/echo/v4"
)

// ICEServersHandler responde GET /voice/ice-servers (autenticado via
// JWTMiddleware): a lista de servidores ICE do usuário (STUN + TURN com
// credencial efêmera RFC 5389). O shape é idêntico ao RTCIceServer do
// browser — o frontend passa direto para new RTCPeerConnection({ iceServers }).
// 401 via middleware; 500 em falha interna (RFC 7807).
func ICEServersHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"usuário autenticado ausente no contexto")
	}

	servers, err := services.ICEConfigForUser(c.Request().Context(), userID)
	if err != nil {
		utils.Errorf("request_id=%s falha ao montar os servidores ICE (user=%s): %v",
			c.Request().Header.Get(echo.HeaderXRequestID), userID, err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao montar os servidores ICE")
	}

	return c.JSON(http.StatusOK, map[string]any{
		"ice_servers": servers,
	})
}
