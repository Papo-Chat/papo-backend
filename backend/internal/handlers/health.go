package handlers

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// HealthHandler retorna o status de health do servidor
func HealthHandler(c echo.Context) error {
	return c.String(http.StatusOK, "OK")
}
