package handlers

import (
	"net/http"

	"papo/internal/middleware"
	"papo/internal/services"
	"papo/internal/utils"
	"papo/internal/websocket"

	ws "github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
)

// wsUpgrader faz o upgrade HTTP -> WebSocket. A autenticação é feita pelo
// JWTMiddleware (cookie Auth) antes do handler; CheckOrigin libera qualquer
// origem porque a autorização real é o JWT validado no handshake e o cookie
// Auth é HttpOnly + SameSite=Strict (não é enviado em requisições cross-site).
var wsUpgrader = ws.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

// WebSocketHandler implementa GET /ws. O JWT do cookie Auth é validado pelo
// JWTMiddleware durante o handshake (antes do upgrade); em seguida a conexão
// HTTP é promovida a WebSocket e o cliente é registrado no Hub.
func WebSocketHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	// A mensagem de status (users.status_message) e o nickname
	// (users.nickname) são carregados na conexão e mantidos no estado efêmero
	// de presença; em falha, a conexão segue sem esses dados.
	var statusMessage, nickname *string
	if user, err := services.Profile(c.Request().Context(), userID); err != nil {
		utils.Errorf("request_id=%s websocket: falha ao carregar o status do usuário: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
	} else {
		statusMessage = user.StatusMessage
		nickname = user.Nickname
	}

	conn, err := wsUpgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		utils.Errorf("websocket: falha no upgrade da conexão: %v", err)
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"ws-upgrade", "Falha no WebSocket",
			"falha ao fazer o upgrade da conexão para WebSocket")
	}

	websocket.Connect(websocket.GetHub(), conn, userID, statusMessage, nickname)
	return nil
}
