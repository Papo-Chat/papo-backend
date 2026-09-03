package middleware

import (
	"errors"
	"net/http"

	"papo/internal/config"
	"papo/internal/storage"
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
//
// Auth híbrida: a assinatura do JWT não basta — o token também precisa ser a
// conexão de sessão ativa do usuário no banco (ou estar dentro da janela de
// graça após a rotação). Reapresentar um token já substituído fora da janela
// é reuso: todas as conexões do usuário são revogadas e
// users.connection_violation é marcado (o cliente usa a flag para avisar o
// usuário).
func JWTMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		cfg := config.LoadConfig()
		ctx := c.Request().Context()

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

		if err := storage.CheckUserConnection(ctx, userID, utils.HashToken(cookie.Value)); err != nil {
			switch {
			case errors.Is(err, storage.ErrConnectionReuse):
				revoked, herr := storage.HandleConnectionReuse(ctx, userID)
				if herr != nil {
					utils.Errorf("request_id=%s reuso de token: falha ao revogar as conexões do usuário %s: %v",
						c.Request().Header.Get(echo.HeaderXRequestID), userID, herr)
				} else {
					utils.Errorf("request_id=%s reuso de token detectado para o usuário %s: %d conexão(ões) revogada(s)",
						c.Request().Header.Get(echo.HeaderXRequestID), userID, revoked)
				}
				return utils.SendProblem(c, cfg.BaseURL, http.StatusUnauthorized,
					"connection-reused", "Sessão invalidada",
					"token substituído foi reapresentado: todas as conexões foram revogadas")
			case errors.Is(err, storage.ErrNotFound):
				return utils.SendProblem(c, cfg.BaseURL, http.StatusUnauthorized,
					"unauthorized", "Token inválido ou expirado",
					"token de autenticação ausente, inválido ou expirado")
			default:
				utils.Errorf("request_id=%s falha ao validar a conexão de sessão do usuário %s: %v",
					c.Request().Header.Get(echo.HeaderXRequestID), userID, err)
				return utils.SendProblem(c, cfg.BaseURL, http.StatusInternalServerError,
					"internal", "Erro interno", "falha ao validar a sessão")
			}
		}

		c.Set(UserIDContextKey, userID)
		return next(c)
	}
}
