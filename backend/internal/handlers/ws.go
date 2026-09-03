package handlers

import (
	"net/http"

	"papo/internal/config"
	"papo/internal/middleware"
	"papo/internal/services"
	"papo/internal/utils"
	"papo/internal/websocket"

	ws "github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
)

// wsCheckOrigin valida o header Origin contra a mesma allowlist usada no CORS
// (config.CORSOrigins). Navegadores sempre enviam Origin no handshake do
// WebSocket, então uma página cross-site (ex.: com o cookie Auth em
// SameSite=None quando SAME_SITE=false) é rejeitada aqui e não abre conexão.
// Requisição sem Origin (cliente não navegador) é aceita, seguindo o
// comportamento padrão do gorilla/websocket: a autorização real continua
// sendo o JWT do cookie Auth validado pelo JWTMiddleware.
func wsCheckOrigin(r *http.Request) bool {
	origin := r.Header.Get(echo.HeaderOrigin)
	if origin == "" {
		return true
	}
	for _, allowed := range config.LoadConfig().CORSOrigins {
		if origin == allowed {
			return true
		}
	}
	return false
}

// wsUpgrader faz o upgrade HTTP -> WebSocket. A autenticação é feita pelo
// JWTMiddleware (cookie Auth) antes do handler; CheckOrigin restringe o
// handshake às origens da allowlist de CORS (ver wsCheckOrigin).
var wsUpgrader = ws.Upgrader{
	CheckOrigin: wsCheckOrigin,
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

	// A mensagem de status (users.status_message), o nickname (users.nickname)
	// e o status persistido (users.status: away/busy) são carregados na
	// conexão e mantidos no estado efêmero de presença; em falha, a conexão
	// segue sem esses dados.
	var statusMessage, nickname, persistedStatus *string
	if user, err := services.Profile(c.Request().Context(), userID); err != nil {
		utils.Errorf("request_id=%s websocket: falha ao carregar o status do usuário: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
	} else {
		statusMessage = user.StatusMessage
		nickname = user.Nickname
		persistedStatus = user.Status
	}

	conn, err := wsUpgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		utils.Errorf("websocket: falha no upgrade da conexão: %v", err)
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"ws-upgrade", "Falha no WebSocket",
			"falha ao fazer o upgrade da conexão para WebSocket")
	}

	websocket.Connect(websocket.GetHub(), conn, userID, statusMessage, nickname, persistedStatus)
	return nil
}
