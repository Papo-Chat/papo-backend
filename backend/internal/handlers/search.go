package handlers

import (
	"errors"
	"net/http"
	"time"

	"papo/internal/middleware"
	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/utils"

	"github.com/labstack/echo/v4"
)

// SearchHandler implementa POST /search.
// Os parâmetros de query server_id (filtro por servidor), since (cursor de
// paginação em created_at, ISO 8601) e last_id (id do último resultado da
// página anterior, usado com since como cursor exato) são opcionais; máx. 100
// resultados por resposta.
func SearchHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	var req models.SearchRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	serverID := c.QueryParam("server_id")

	var since *time.Time
	if value := c.QueryParam("since"); value != "" {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return utils.SendProblem(c, baseURL, http.StatusBadRequest,
				"invalid-param", "Parâmetro inválido",
				"since deve ser um timestamp ISO 8601")
		}
		since = &parsed
	}
	lastID := c.QueryParam("last_id")

	resp, err := services.SearchMessages(c.Request().Context(), req, serverID, since, lastID, userID)
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"pelo menos 1 filtro é obrigatório (text, author, date_start, date_end ou contains_attachment); "+
				"order deve ser asc ou desc; date_start e date_end devem estar no formato YYYY-MM-DD com date_start <= date_end; "+
				"since e last_id devem ser informados juntos")
	case err != nil:
		utils.Errorf("request_id=%s falha ao buscar mensagens: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao buscar as mensagens")
	}

	return c.JSON(http.StatusOK, resp)
}
