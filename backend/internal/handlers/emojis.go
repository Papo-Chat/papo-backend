package handlers

import (
	"errors"
	"net/http"
	"time"

	"papo/internal/middleware"
	"papo/internal/services"
	"papo/internal/utils"

	"github.com/labstack/echo/v4"
)

// ListEmojisHandler implementa GET /emojis.
// Os parâmetros de query server_id (filtro por servidor), since (cursor de
// paginação em created_at, ISO 8601) e last_id (id do último emoji da página
// anterior, usado com since como cursor exato) são opcionais; máx. 25 emojis
// por resposta.
func ListEmojisHandler(baseURL string, c echo.Context) error {
	var serverID *string
	if value := c.QueryParam("server_id"); value != "" {
		serverID = &value
	}

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

	list, err := services.ListEmojis(c.Request().Context(), serverID, since, lastID)
	if err != nil {
		utils.Errorf("request_id=%s falha ao listar emojis: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao listar emojis")
	}

	return c.JSON(http.StatusOK, list)
}

type createEmojiRequest struct {
	ServerID  string `json:"server_id"`
	Name      string `json:"name"`
	Format    string `json:"format"`
	ImageBlob string `json:"image_blob"`
}

// CreateEmojiHandler implementa POST /emojis.
// Permissão: dono do servidor ou role `manage_server`
// (middleware RequireManageServer).
func CreateEmojiHandler(baseURL string, c echo.Context) error {
	var req createEmojiRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	emoji, err := services.CreateEmoji(c.Request().Context(), req.ServerID, req.Name, req.Format, req.ImageBlob, userID)
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"server_id, name, format e image_blob são obrigatórios; name deve ter no máximo 32 caracteres; "+
				"format deve ser GIF, JPEG ou PNG; image_blob deve ser base64 de uma imagem com no máximo 256kb")
	case errors.Is(err, services.ErrServerNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "servidor não encontrado")
	case errors.Is(err, services.ErrEmojiLimitReached):
		return utils.SendProblem(c, baseURL, http.StatusConflict,
			"emoji-limit-reached", "Limite de emojis atingido",
			"o servidor já possui o número máximo de emojis (500)")
	case errors.Is(err, services.ErrEmojiNameTaken):
		return utils.SendProblem(c, baseURL, http.StatusConflict,
			"emoji-name-taken", "Nome de emoji já existe",
			"o nome informado já está em uso")
	case err != nil:
		utils.Errorf("request_id=%s falha ao criar emoji: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao criar o emoji")
	}

	return c.JSON(http.StatusCreated, emoji)
}

// DeleteEmojiHandler implementa DELETE /emojis/:emoji_id.
// Permissão: autor do emoji, dono do servidor ou role `manage_server`
// do servidor do emoji.
func DeleteEmojiHandler(baseURL string, c echo.Context) error {
	emojiID := c.Param("emoji_id")
	if emojiID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "emoji_id ausente")
	}

	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	err := services.DeleteEmoji(c.Request().Context(), emojiID, userID)
	switch {
	case errors.Is(err, services.ErrEmojiNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "emoji não encontrado")
	case errors.Is(err, services.ErrPermissionDenied):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado",
			"usuário não pode excluir este emoji")
	case err != nil:
		utils.Errorf("request_id=%s falha ao excluir emoji: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao excluir o emoji")
	}

	return c.NoContent(http.StatusNoContent)
}
