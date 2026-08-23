package middleware

import (
	"net/http"

	"papo/internal/config"
	"papo/internal/utils"

	"github.com/labstack/echo/v4"
	echoMiddleware "github.com/labstack/echo/v4/middleware"
	"golang.org/x/time/rate"
)

// RateLimit retorna um middleware que limita as requisições por IP do cliente
// (identificador padrão do Echo) usando um token bucket com a taxa sustentada
// informada (requisições por segundo) e a capacidade de burst.
// Quando o limite é excedido, a resposta é 429 no formato RFC 7807 do projeto.
func RateLimit(requestsPerSecond int, burst int) echo.MiddlewareFunc {
	store := echoMiddleware.NewRateLimiterMemoryStoreWithConfig(
		echoMiddleware.RateLimiterMemoryStoreConfig{
			Rate:  rate.Limit(requestsPerSecond),
			Burst: burst,
		})

	return echoMiddleware.RateLimiterWithConfig(echoMiddleware.RateLimiterConfig{
		Store: store,
		DenyHandler: func(c echo.Context, identifier string, err error) error {
			cfg := config.LoadConfig()
			return utils.SendProblem(c, cfg.BaseURL, http.StatusTooManyRequests,
				"rate-limit", "Limite de requisições excedido",
				"muitas requisições, tente novamente mais tarde")
		},
	})
}
