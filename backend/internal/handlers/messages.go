package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"papo/internal/middleware"
	"papo/internal/models"
	"papo/internal/moderation"
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

	list, err := services.ListMessagesOrdered(c.Request().Context(), channelID, userID, since, lastID, c.QueryParam("order"))
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", "order deve ser asc ou desc")
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
	replyTo := c.FormValue("reply_to")

	// embeds: campo JSON (string no multipart) com os embeds customizados da
	// mensagem. O service valida os limites e rejeita source_type de link.
	embeds, err := parseEmbedsField(c.FormValue("embeds"))
	if err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"campo embeds deve ser um array JSON de embeds")
	}

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

	message, err := services.CreateMessage(c.Request().Context(), channelID, userID, content, replyTo, inputs, embeds)
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"channel_id é obrigatório; content tem no máximo 8192 caracteres; a mensagem precisa de content ou attachment; nome do attachment inválido; reply_to deve referenciar uma mensagem do mesmo canal")
	case errors.Is(err, services.ErrTooManyAttachments):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"máximo de 10 attachments por mensagem")
	case errors.Is(err, services.ErrInvalidEmbed):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", err.Error())
	case errors.Is(err, services.ErrMessageNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado",
			"reply_to referencia uma mensagem inexistente")
	case errors.Is(err, services.ErrChannelNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "canal não encontrado")
	case errors.Is(err, services.ErrDirectMessageBlocked):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"dm-blocked", "Mensagem direta indisponível",
			"não é possível enviar mensagens nesta conversa")
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
		ReplyTo:     message.ReplyTo,
		Attachments: message.Attachments,
	})
	broadcastDirectConversationUpdates(c.Request().Context(), message.ChannelID)

	// Enfileira os attachments na moderação assíncrona de imagens
	// (nudez/gore): o worker processa em background e, se blocked, exclui a
	// mensagem e distribui message_delete.
	for _, attachment := range message.Attachments {
		moderation.Enqueue(attachment.ID)
	}

	// Embeds customizados já estão gravados: distribui message_embeds_update com
	// a lista atual da mensagem (os link embeds ainda serão processados).
	if len(message.Embeds) > 0 {
		broadcastChannelEvent(c, message.ChannelID, websocket.MessageEmbedsUpdateOutbound{
			Type:      websocket.EventTypeMessageEmbedsUpdate,
			ChannelID: message.ChannelID,
			MessageID: message.ID,
			Embeds:    message.Embeds,
		})
	}

	// Processa os link embeds do content em background (o crawl não bloqueia a
	// resposta); eles chegam via WS message_embeds_update.
	requestID := c.Request().Header.Get(echo.HeaderXRequestID)
	go processNewMessageEmbeds(context.Background(), requestID, message.ChannelID, message.ID, userID, content, message.Embeds)

	// Dispara as notificações da mensagem em background (menções, replies e
	// @everyone); as entregas chegam via WS new_notification (unicast).
	go dispatchMessageNotifications(context.Background(), requestID, message.Message)

	return c.JSON(http.StatusCreated, message)
}

type updateMessageRequest struct {
	Content string                `json:"content"`
	Embeds  []services.EmbedInput `json:"embeds"`
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

	message, err := services.EditMessage(c.Request().Context(), messageID, userID, req.Content, req.Embeds)
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"content tem no máximo 8192 caracteres")
	case errors.Is(err, services.ErrInvalidEmbed):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", err.Error())
	case errors.Is(err, services.ErrMessageNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "mensagem não encontrada")
	case errors.Is(err, services.ErrDirectMessageBlocked):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"dm-blocked", "Mensagem direta indisponível",
			"não é possível editar mensagens nesta conversa")
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

	// Distribui a lista atual de embeds da mensagem (os customizados gravados
	// agora + os de link ainda vinculados, que o crawl ainda vai substituir).
	requestID := c.Request().Header.Get(echo.HeaderXRequestID)
	broadcastMessageEmbedsUpdate(c.Request().Context(), requestID, message.ChannelID, message.ID)

	// Processa os link embeds do content novo em background (o crawl não
	// bloqueia a resposta); as mudanças chegam via WS message_embeds_update.
	go processEditedMessageEmbeds(context.Background(), requestID, message.ChannelID, message.ID, userID, derefString(message.Content), message.Embeds)

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

// PinMessageHandler implementa POST /channels/:channel_id/messages/:message_id/pin.
// Permissão: pin_message (dono do servidor ou role com a permissão). Fixar uma
// mensagem já pinada é idempotente (200); a primeira fixação retorna 201.
func PinMessageHandler(baseURL string, c echo.Context) error {
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

	pinned, created, err := services.PinMessage(c.Request().Context(), channelID, messageID, userID)
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"channel_id e message_id são obrigatórios")
	case errors.Is(err, services.ErrMessageNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado",
			"mensagem não encontrada neste canal")
	case errors.Is(err, services.ErrPermissionDenied):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado",
			"usuário não tem permissão para fixar a mensagem")
	case errors.Is(err, services.ErrTooManyPinnedMessages):
		return utils.SendProblem(c, baseURL, http.StatusConflict,
			"pinned-limit-reached", "Limite de mensagens pinadas atingido",
			"o canal já tem o número máximo de 100 mensagens pinadas")
	case err != nil:
		utils.Errorf("request_id=%s falha ao fixar mensagem: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao fixar a mensagem")
	}

	if created {
		// Distribui a fixação aos clientes autorizados a ler o canal
		// (evento message_pin).
		broadcastChannelEvent(c, channelID, websocket.MessagePinOutbound{
			Type:      websocket.EventTypeMessagePin,
			MessageID: messageID,
			IsPinned:  true,
		})
		return c.JSON(http.StatusCreated, pinned)
	}
	return c.JSON(http.StatusOK, pinned)
}

// UnpinMessageHandler implementa DELETE /channels/:channel_id/messages/:message_id/pin.
// Permissão: read_channel do canal e pin_message (dono do servidor ou role com
// a permissão). A mensagem não pinada retorna 404 (não é idempotente).
func UnpinMessageHandler(baseURL string, c echo.Context) error {
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

	_, err := services.UnpinMessage(c.Request().Context(), channelID, messageID, userID)
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"channel_id e message_id são obrigatórios")
	case errors.Is(err, services.ErrMessageNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado",
			"mensagem não encontrada neste canal")
	case errors.Is(err, services.ErrMessageNotPinned):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado",
			"mensagem não está pinada neste canal")
	case errors.Is(err, services.ErrPermissionDenied):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado",
			"usuário não tem permissão para remover a fixação da mensagem")
	case err != nil:
		utils.Errorf("request_id=%s falha ao remover fixação da mensagem: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao remover a fixação da mensagem")
	}

	// Distribui a remoção da fixação aos clientes autorizados a ler o canal
	// (evento message_pin).
	broadcastChannelEvent(c, channelID, websocket.MessagePinOutbound{
		Type:      websocket.EventTypeMessagePin,
		MessageID: messageID,
		IsPinned:  false,
	})

	return c.NoContent(http.StatusNoContent)
}

// ListPinnedMessagesHandler implementa GET /channels/:channel_id/pinned.
// Permissão: read_channel do canal.
func ListPinnedMessagesHandler(baseURL string, c echo.Context) error {
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

	list, err := services.ListPinnedMessages(c.Request().Context(), channelID, userID)
	switch {
	case errors.Is(err, services.ErrChannelNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "canal não encontrado")
	case errors.Is(err, services.ErrPermissionDenied):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado",
			"usuário não tem permissão para ler o canal")
	case err != nil:
		utils.Errorf("request_id=%s falha ao listar mensagens pinadas: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao listar as mensagens pinadas")
	}

	return c.JSON(http.StatusOK, list)
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

// processNewMessageEmbeds processa em background os link embeds de uma mensagem
// recém-criada e distribui message_embeds_update com a lista atual de embeds da
// mensagem (customizados + automáticos) e, para cada embed refetchado, um evento
// por mensagem já vinculada a ele. Best-effort: falhas são logadas e não afetam
// a mensagem já criada.
func processNewMessageEmbeds(ctx context.Context, requestID, channelID, messageID, authorID, content string, customs []models.Embed) {
	linked, updates := services.ProcessMessageEmbeds(ctx, messageID, authorID, content, customs)
	if len(linked) > 0 {
		broadcastMessageEmbedsUpdate(ctx, requestID, channelID, messageID)
	}
	broadcastEmbedUpdates(ctx, requestID, updates)
}

// processEditedMessageEmbeds processa em background os link embeds de uma
// mensagem editada (os vínculos de link são substituídos) e distribui
// message_embeds_update com a lista atual de embeds da mensagem — inclusive
// quando a edição remove todos os embeds — e, para cada embed refetchado, um
// evento por mensagem já vinculada a ele. Best-effort: falhas são logadas e não
// afetam a mensagem já editada.
func processEditedMessageEmbeds(ctx context.Context, requestID, channelID, messageID, authorID, content string, customs []models.Embed) {
	added, removed, updates := services.ProcessEditedMessageEmbeds(ctx, messageID, authorID, content, customs)
	if len(added) > 0 || len(removed) > 0 {
		broadcastMessageEmbedsUpdate(ctx, requestID, channelID, messageID)
	}
	broadcastEmbedUpdates(ctx, requestID, updates)
}

// broadcastMessageEmbedsUpdate distribui message_embeds_update com a lista
// atual de embeds da mensagem, apenas aos leitores do canal. A lista traz
// apenas metadados: a mídia é carregada sob demanda pelo cliente.
func broadcastMessageEmbedsUpdate(ctx context.Context, requestID, channelID, messageID string) {
	embedsByMessage, err := services.ListMessageEmbeds(ctx, []string{messageID})
	if err != nil {
		utils.Errorf("request_id=%s falha ao listar os embeds da mensagem %s: %v", requestID, messageID, err)
		return
	}
	broadcastChannelEventCtx(ctx, requestID, channelID, websocket.MessageEmbedsUpdateOutbound{
		Type:      websocket.EventTypeMessageEmbedsUpdate,
		ChannelID: channelID,
		MessageID: messageID,
		Embeds:    embedsByMessage[messageID],
	})
}

// broadcastEmbedUpdates distribui um evento message_embeds_update por mensagem
// vinculada a um embed refetchado (a row mudou no banco e os clientes têm a
// versão antiga), apenas aos leitores do canal da mensagem. As listas atuais
// são lidas em lote (uma única query).
func broadcastEmbedUpdates(ctx context.Context, requestID string, updates []services.EmbedUpdate) {
	if len(updates) == 0 {
		return
	}

	channelByMessage := make(map[string]string)
	for _, u := range updates {
		for _, ref := range u.Messages {
			channelByMessage[ref.MessageID] = ref.ChannelID
		}
	}
	messageIDs := make([]string, 0, len(channelByMessage))
	for messageID := range channelByMessage {
		messageIDs = append(messageIDs, messageID)
	}

	embedsByMessage, err := services.ListMessageEmbeds(ctx, messageIDs)
	if err != nil {
		utils.Errorf("request_id=%s falha ao listar os embeds das mensagens atualizadas: %v", requestID, err)
		return
	}

	for _, messageID := range messageIDs {
		channelID := channelByMessage[messageID]
		broadcastChannelEventCtx(ctx, requestID, channelID, websocket.MessageEmbedsUpdateOutbound{
			Type:      websocket.EventTypeMessageEmbedsUpdate,
			ChannelID: channelID,
			MessageID: messageID,
			Embeds:    embedsByMessage[messageID],
		})
	}
}

// parseEmbedsField decodifica o campo "embeds" do multipart (uma string JSON)
// em []services.EmbedInput. Campo ausente ou vazio → nil (mensagem sem embeds
// customizados). Os limites são validados no service.
func parseEmbedsField(raw string) ([]services.EmbedInput, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil, nil
	}
	var embeds []services.EmbedInput
	if err := json.Unmarshal([]byte(raw), &embeds); err != nil {
		return nil, err
	}
	return embeds, nil
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

// broadcastDirectConversationUpdates mantém a rail de DM sincronizada. Em
// canais normais a função termina silenciosamente.
func broadcastDirectConversationUpdates(ctx context.Context, channelID string) {
	members, err := services.DirectConversationMembers(ctx, channelID)
	if errors.Is(err, services.ErrDirectMessageNotFound) {
		return
	}
	if err != nil {
		utils.Errorf("DM %s: falha ao listar participantes para atualização websocket: %v", channelID, err)
		return
	}
	for _, userID := range members {
		dm, err := services.GetDirectConversation(ctx, userID, channelID)
		if err != nil {
			utils.Errorf("DM %s: falha ao montar atualização websocket para %s: %v", channelID, userID, err)
			continue
		}
		websocket.GetHub().SendToUser(userID, websocket.DMUpdateOutbound{
			Type: websocket.EventTypeDMUpdate,
			DM:   dm,
		})
	}
}
