package middleware

import (
	"net/http"

	"papo/internal/config"
	"papo/internal/utils"

	"github.com/labstack/echo/v4"
)

// UserIDContextKey é a chave sob a qual o ID do usuário autenticado é
// armazenado no contexto do Echo.
const UserIDContextKey = "user_id"

// authCookieName é o nome do cookie que carrega o JWT de autenticação.
const authCookieName = "Auth"

// JWTMiddleware valida o JWT presente no cookie Auth e, quando válido,
// armazena o ID do usuário no contexto. Cookie ausente, token inválido,
// expirado ou sem subject retornam erro 401 (RFC 7807).
func JWTMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		cfg := config.LoadConfig()

		cookie, err := c.Cookie(authCookieName)
		if err != nil || cookie.Value == "" {
			return utils.SendProblem(c, cfg.BaseURL, http.StatusUnauthorized,
				"unauthorized", "Token inválido ou expirado",
				"token de autenticação ausente, inválido ou expirado")
		}

		userID, err := utils.ValidateToken(cookie.Value, cfg.JWTSecret)
		if err != nil || userID == "" {
			return utils.SendProblem(c, cfg.BaseURL, http.StatusUnauthorized,
				"unauthorized", "Token inválido ou expirado",
				"token de autenticação ausente, inválido ou expirado")
		}

		c.Set(UserIDContextKey, userID)
		return next(c)
	}
}
