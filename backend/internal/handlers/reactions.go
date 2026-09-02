package handlers

import (
	"errors"
	"net/http"

	"papo/internal/middleware"
	"papo/internal/services"
	"papo/internal/utils"
	"papo/internal/websocket"

	"github.com/labstack/echo/v4"
)

// reactionRequest é o corpo de POST/DELETE de reações: exatamente um de
// emoji_id (emoji custom do banco) ou unicode (emoji unicode).
type reactionRequest struct {
	EmojiID *string `json:"emoji_id"`
	Unicode *string `json:"unicode"`
}

// reactionFromRequest extrai o input de reação do corpo (nil/ausente vira "").
func reactionFromRequest(req reactionRequest) (string, string) {
	emojiID := ""
	if req.EmojiID != nil {
		emojiID = *req.EmojiID
	}
	unicode := ""
	if req.Unicode != nil {
		unicode = *req.Unicode
	}
	return emojiID, unicode
}

// reactionEventPtrs monta os ponteiros do evento react_update a partir do
// input (exatamente um dos dois é preenchido).
func reactionEventPtrs(emojiID, unicode string) (*string, *string) {
	if emojiID != "" {
		return &emojiID, nil
	}
	return nil, &unicode
}

// broadcastReactUpdate distribui o react_update aos clientes autorizados a
// ler o canal.
func broadcastReactUpdate(c echo.Context, channelID, messageID string, emojiID, unicode *string, count int) {
	broadcastChannelEvent(c, channelID, websocket.ReactUpdateOutbound{
		Type:      websocket.EventTypeReactUpdate,
		MessageID: messageID,
		EmojiID:   emojiID,
		Unicode:   unicode,
		Count:     count,
	})
}

// AddReactionHandler implementa POST /channels/:channel_id/messages/:message_id/reactions.
// Corpo: exatamente um de {"emoji_id": "<uuid>"} (emoji custom do banco) ou
// {"unicode": "<emoji>"}. Permissão: send_messages do canal (livre em canais
// sem roles definidas). Reagir de novo com o mesmo emoji é idempotente (200);
// a primeira reação retorna 201.
func AddReactionHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	channelID := c.Param("channel_id")
	messageID := c.Param("message_id")
	if channelID == "" || messageID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"channel_id e message_id são obrigatórios")
	}

	var req reactionRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}
	emojiID, unicode := reactionFromRequest(req)

	reaction, created, count, err := services.AddReactionToMessage(c.Request().Context(), channelID, messageID, userID, emojiID, unicode)
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"informe exatamente um de emoji_id ou unicode; unicode tem no máximo 16 caracteres")
	case errors.Is(err, services.ErrMessageNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado",
			"mensagem não encontrada neste canal")
	case errors.Is(err, services.ErrChannelNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "canal não encontrado")
	case errors.Is(err, services.ErrEmojiNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "emoji não encontrado")
	case errors.Is(err, services.ErrPermissionDenied):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado",
			"usuário não tem permissão para reagir neste canal")
	case errors.Is(err, services.ErrTooManyReactions):
		return utils.SendProblem(c, baseURL, http.StatusConflict,
			"reaction-limit-reached", "Limite de reações atingido",
			"a mensagem já tem o número máximo de 20 tipos de reação")
	case err != nil:
		utils.Errorf("request_id=%s falha ao reagir à mensagem: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao reagir à mensagem")
	}

	broadcastReactUpdate(c, channelID, messageID, reaction.EmojiID, reaction.Unicode, count)

	if created {
		return c.JSON(http.StatusCreated, reaction)
	}
	return c.JSON(http.StatusOK, reaction)
}

// RemoveReactionHandler implementa DELETE /channels/:channel_id/messages/:message_id/reactions.
// Corpo: exatamente um de {"emoji_id": "<uuid>"} ou {"unicode": "<emoji>"} —
// remove a própria reação do usuário autenticado. Permissão: read_channel do
// canal.
func RemoveReactionHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	channelID := c.Param("channel_id")
	messageID := c.Param("message_id")
	if channelID == "" || messageID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"channel_id e message_id são obrigatórios")
	}

	var req reactionRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}
	emojiID, unicode := reactionFromRequest(req)

	count, err := services.RemoveReactionFromMessage(c.Request().Context(), channelID, messageID, userID, emojiID, unicode)
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"informe exatamente um de emoji_id ou unicode; unicode tem no máximo 16 caracteres")
	case errors.Is(err, services.ErrMessageNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado",
			"mensagem não encontrada neste canal")
	case errors.Is(err, services.ErrChannelNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "canal não encontrado")
	case errors.Is(err, services.ErrEmojiNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "emoji não encontrado")
	case errors.Is(err, services.ErrPermissionDenied):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado",
			"usuário não tem permissão para ler o canal")
	case errors.Is(err, services.ErrReactionNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado",
			"usuário não reagiu à mensagem com este emoji")
	case err != nil:
		utils.Errorf("request_id=%s falha ao remover reação da mensagem: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao remover a reação")
	}

	emojiPtr, unicodePtr := reactionEventPtrs(emojiID, unicode)
	broadcastReactUpdate(c, channelID, messageID, emojiPtr, unicodePtr, count)

	return c.NoContent(http.StatusNoContent)
}

// ListReactionsHandler implementa GET /channels/:channel_id/messages/:message_id/reactions.
// Retorna os tipos de reação da mensagem com os usuários que reagiram.
// Permissão: read_channel do canal.
func ListReactionsHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	channelID := c.Param("channel_id")
	messageID := c.Param("message_id")
	if channelID == "" || messageID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"channel_id e message_id são obrigatórios")
	}

	list, err := services.ListMessageReactions(c.Request().Context(), channelID, messageID, userID)
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"channel_id e message_id são obrigatórios")
	case errors.Is(err, services.ErrMessageNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado",
			"mensagem não encontrada neste canal")
	case errors.Is(err, services.ErrChannelNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "canal não encontrado")
	case errors.Is(err, services.ErrPermissionDenied):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado",
			"usuário não tem permissão para ler o canal")
	case err != nil:
		utils.Errorf("request_id=%s falha ao listar reações da mensagem: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao listar as reações")
	}

	return c.JSON(http.StatusOK, list)
}
