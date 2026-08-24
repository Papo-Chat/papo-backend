package middleware

import (
	"net/http"

	"github.com/labstack/echo/v4"
	echoMiddleware "github.com/labstack/echo/v4/middleware"
)

// CORS aplica os cabeçalhos CORS para os origins permitidos, habilitando
// requisições cross-origin tanto em HTTP quanto em HTTPS (ex.: frontend de
// desenvolvimento local). AllowCredentials é necessário porque a
// autenticação usa o cookie Auth (HttpOnly) em requisições cross-origin.
func CORS(origins []string) echo.MiddlewareFunc {
	return echoMiddleware.CORSWithConfig(echoMiddleware.CORSConfig{
		AllowOrigins:     origins,
		AllowMethods:     []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions},
		AllowHeaders:     []string{echo.HeaderContentType, echo.HeaderAuthorization, echo.HeaderXRequestID},
		ExposeHeaders:    []string{echo.HeaderXRequestID},
		AllowCredentials: true,
	})
}
