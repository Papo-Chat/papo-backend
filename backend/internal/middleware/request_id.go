package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"
)

// RequestIDMiddleware adiciona um header X-Request-ID para rastreamento de requisições
// Este middleware é compatível com echo.MiddlewareFunc
// o erro é raro, caso houver um problema grave de OS no rand
func RequestIDMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		requestID := c.Request().Header.Get(echo.HeaderXRequestID)
		if requestID == "" {
			requestID, err := generateRequestID()
			if err != nil {
				c.Logger().Errorf("failed to generate request ID: %v", err)

				return echo.NewHTTPError(
					http.StatusInternalServerError,
					"internal server error",
				)
			}
			c.Request().Header.Set(echo.HeaderXRequestID, requestID)
		}
		c.Response().Header().Set(echo.HeaderXRequestID, requestID)
		return next(c)
	}
}

// generateRequestID gera um ID único para rastreamento
func generateRequestID() (reqId string, err error) {
	b := make([]byte, 16)

	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate request ID: %w", err)
	}

	return "req-" + hex.EncodeToString(b), nil
}
