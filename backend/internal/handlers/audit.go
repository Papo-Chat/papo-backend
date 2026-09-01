package handlers

import (
	"net/http"
	"time"

	"papo/internal/services"
	"papo/internal/utils"

	"github.com/labstack/echo/v4"
)

// ListAuditLogsHandler implementa GET /admin/audit-logs.
// Filtros opcionais de query: action, actor_id, entity_type, since
// (created_at >=), until (created_at <=) e last_id (cursor de paginação, id do
// último item da página anterior). Máximo de 100 logs por resposta.
func ListAuditLogsHandler(baseURL string, c echo.Context) error {
	var since, until *time.Time
	if value := c.QueryParam("since"); value != "" {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return utils.SendProblem(c, baseURL, http.StatusBadRequest,
				"invalid-param", "Parâmetro inválido",
				"since deve ser um timestamp ISO 8601")
		}
		since = &parsed
	}
	if value := c.QueryParam("until"); value != "" {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return utils.SendProblem(c, baseURL, http.StatusBadRequest,
				"invalid-param", "Parâmetro inválido",
				"until deve ser um timestamp ISO 8601")
		}
		until = &parsed
	}

	resp, err := services.ListAuditLogs(c.Request().Context(),
		c.QueryParam("action"), c.QueryParam("actor_id"), c.QueryParam("entity_type"),
		since, until, c.QueryParam("last_id"))
	if err != nil {
		utils.Errorf("request_id=%s falha ao listar auditoria: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao listar os logs de auditoria")
	}

	return c.JSON(http.StatusOK, resp)
}
