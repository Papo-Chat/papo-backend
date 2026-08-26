package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"papo/internal/middleware"
	"papo/internal/services"
	"papo/internal/utils"
	"papo/internal/websocket"

	"github.com/labstack/echo/v4"
)

// ListMessagesHandler implementa GET /channels/:channel_id/messages.
// O parâmetro de query since é opcional: timestamp ISO 8601 para polling de
// novas mensagens. last_id é opcional: id da última mensagem da página
// anterior; usado com since como cursor exato (created_at, id).
func ListMessagesHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	channelID := c.Param("channel_id")
	if channelID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "channel_id ausente")
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

	list, err := services.ListMessages(c.Request().Context(), channelID, userID, since, lastID)
	switch {
	case errors.Is(err, services.ErrChannelNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "canal não encontrado")
	case errors.Is(err, services.ErrPermissionDenied):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado",
			"usuário não tem permissão para ler o canal")
	case err != nil:
		utils.Errorf("request_id=%s falha ao listar mensagens: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao listar as mensagens")
	}

	return c.JSON(http.StatusOK, list)
}

// CreateMessageHandler implementa POST /messages (multipart/form-data).
// Campos: channel_id (obrigatório), content (opcional) e attachments
// (arquivos, opcionais, campo repetível). Permissão: send_messages do canal
// (livre em canais sem roles definidas) e send_attachment no servidor quando
// há attachments.
func CreateMessageHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	//110 << 20 nos dá 110MB de tamanho máximo no form multipart
	//Attachments podem ter no máximo 100MB e os outros 10MB é buffer pro content da message
	if err := c.Request().ParseMultipartForm(110 << 20); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"corpo da requisição deve ser multipart/form-data válido")
	}

	channelID := c.FormValue("channel_id")
	content := c.FormValue("content")

	var inputs []services.AttachmentInput
	if c.Request().MultipartForm != nil {
		for _, fileHeader := range c.Request().MultipartForm.File["attachments"] {
			file, err := fileHeader.Open()
			if err != nil {
				return utils.SendProblem(c, baseURL, http.StatusBadRequest,
					"invalid-param", "Parâmetro inválido",
					"falha ao ler o attachment enviado")
			}
			defer file.Close()

			inputs = append(inputs, services.AttachmentInput{
				OriginalFileName: fileHeader.Filename,
				Content:          file,
			})
		}
	}

	message, err := services.CreateMessage(c.Request().Context(), channelID, userID, content, inputs)
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"channel_id é obrigatório; content tem no máximo 8192 caracteres; a mensagem precisa de content ou attachment; nome do attachment inválido")
	case errors.Is(err, services.ErrTooManyAttachments):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"máximo de 10 attachments por mensagem")
	case errors.Is(err, services.ErrChannelNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "canal não encontrado")
	case errors.Is(err, services.ErrPermissionDenied):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado",
			"usuário não tem permissão para enviar esta mensagem")
	case errors.Is(err, services.ErrAttachmentTooLarge):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"attachment excede o tamanho máximo de 100MB")
	case err != nil:
		utils.Errorf("request_id=%s falha ao criar mensagem: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao criar a mensagem")
	}

	// Distribui a nova mensagem aos clientes autorizados a ler o canal
	// (evento message).
	broadcastChannelEvent(c, message.ChannelID, websocket.MessageOutbound{
		Type:        websocket.EventTypeMessage,
		ID:          message.ID,
		ChannelID:   message.ChannelID,
		AuthorID:    derefString(message.AuthorID),
		Content:     derefString(message.Content),
		CreatedAt:   message.CreatedAt,
		Attachments: message.Attachments,
	})

	// Processa os link previews em background (o crawl não bloqueia a
	// resposta); os previews chegam via WS new_preview.
	requestID := c.Request().Header.Get(echo.HeaderXRequestID)
	go processNewMessagePreviews(context.Background(), requestID, message.ChannelID, message.ID, userID, content)

	return c.JSON(http.StatusCreated, message)
}

type updateMessageRequest struct {
	Content string `json:"content"`
}

// UpdateMessageHandler implementa PUT /messages/:message_id.
// Somente o autor da mensagem pode editá-la.
func UpdateMessageHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	messageID := c.Param("message_id")
	if messageID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "message_id ausente")
	}

	var req updateMessageRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	message, err := services.EditMessage(c.Request().Context(), messageID, userID, req.Content)
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"content tem no máximo 8192 caracteres")
	case errors.Is(err, services.ErrMessageNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "mensagem não encontrada")
	case errors.Is(err, services.ErrPermissionDenied):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado",
			"somente o autor da mensagem pode editá-la")
	case err != nil:
		utils.Errorf("request_id=%s falha ao editar mensagem: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao editar a mensagem")
	}

	// Distribui a edição aos clientes autorizados a ler o canal
	// (evento message_edit).
	broadcastChannelEvent(c, message.ChannelID, websocket.MessageEditOutbound{
		Type:      websocket.EventTypeMessageEdit,
		ID:        message.ID,
		ChannelID: message.ChannelID,
		Content:   derefString(message.Content),
		EditedAt:  derefTime(message.EditedAt),
	})

	// Processa os link previews do content novo em background (o crawl não
	// bloqueia a resposta); as mudanças chegam via WS new_preview /
	// remove_preview.
	requestID := c.Request().Header.Get(echo.HeaderXRequestID)
	go processEditedMessagePreviews(context.Background(), requestID, message.ChannelID, message.ID, userID, derefString(message.Content))

	return c.JSON(http.StatusOK, message)
}

// DeleteMessageHandler implementa DELETE /messages/:message_id.
// Permissão: autor da mensagem, dono do servidor do canal ou role com
// delete_messages concedida explicitamente no canal.
func DeleteMessageHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	messageID := c.Param("message_id")
	if messageID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "message_id ausente")
	}

	channelID, err := services.DeleteMessage(c.Request().Context(), messageID, userID)
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "message_id ausente")
	case errors.Is(err, services.ErrMessageNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "mensagem não encontrada")
	case errors.Is(err, services.ErrChannelNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "canal não encontrado")
	case errors.Is(err, services.ErrPermissionDenied):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado",
			"usuário não tem permissão para excluir a mensagem")
	case err != nil:
		utils.Errorf("request_id=%s falha ao excluir mensagem: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao excluir a mensagem")
	}

	// Distribui a exclusão aos clientes autorizados a ler o canal
	// (evento message_delete).
	broadcastChannelEvent(c, channelID, websocket.MessageDeleteOutbound{
		Type:      websocket.EventTypeMessageDelete,
		ID:        messageID,
		ChannelID: channelID,
	})

	return c.NoContent(http.StatusNoContent)
}

// broadcastChannelEvent envia um evento via WebSocket no contexto da
// requisição (ver broadcastChannelEventCtx).
func broadcastChannelEvent(c echo.Context, channelID string, event any) {
	broadcastChannelEventCtx(c.Request().Context(), c.Request().Header.Get(echo.HeaderXRequestID), channelID, event)
}

// broadcastChannelEventCtx envia um evento via WebSocket somente aos clientes
// cujo usuário pode ler o canal (read_channel, mesma regra de ListMessages).
// Em falha da autorização, o evento não é enviado (fail closed) e a falha é
// registrada. Aceita um ctx próprio para uso fora da requisição (goroutines
// de background), onde o ctx da request já foi cancelado.
func broadcastChannelEventCtx(ctx context.Context, requestID, channelID string, event any) {
	hub := websocket.GetHub()
	allowed, err := services.ChannelReaders(ctx, channelID, hub.OnlineUserIDs())
	if err != nil {
		utils.Errorf("request_id=%s websocket: falha ao autorizar o broadcast do canal %s: %v",
			requestID, channelID, err)
		return
	}
	hub.BroadcastToUsers(event, allowed)
}

// processNewMessagePreviews processa em background os link previews de uma
// mensagem recém-criada e distribui os eventos new_preview (um por preview).
// Best-effort: falhas são logadas e não afetam a mensagem já criada.
func processNewMessagePreviews(ctx context.Context, requestID, channelID, messageID, authorID, content string) {
	for _, p := range services.ProcessMessagePreviews(ctx, messageID, authorID, content) {
		broadcastChannelEventCtx(ctx, requestID, channelID, websocket.NewPreviewOutbound{
			Type:      websocket.EventTypeNewPreview,
			MessageID: messageID,
			PreviewID: p.ID,
		})
	}
}

// processEditedMessagePreviews processa em background os link previews de uma
// mensagem editada e distribui os eventos new_preview (adicionados) e
// remove_preview (removidos), um por preview. Best-effort: falhas são
// logadas e não afetam a mensagem já editada.
func processEditedMessagePreviews(ctx context.Context, requestID, channelID, messageID, authorID, content string) {
	added, removed := services.ProcessEditedMessagePreviews(ctx, messageID, authorID, content)
	for _, p := range added {
		broadcastChannelEventCtx(ctx, requestID, channelID, websocket.NewPreviewOutbound{
			Type:      websocket.EventTypeNewPreview,
			MessageID: messageID,
			PreviewID: p.ID,
		})
	}
	for _, p := range removed {
		broadcastChannelEventCtx(ctx, requestID, channelID, websocket.RemovePreviewOutbound{
			Type:      websocket.EventTypeRemovePreview,
			MessageID: messageID,
			PreviewID: p.ID,
		})
	}
}

// derefString retorna o valor da ponteira de string ou "" quando nil.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// derefTime retorna o valor da ponteira de time ou o zero quando nil.
func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
