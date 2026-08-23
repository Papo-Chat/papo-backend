package services

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"
)

// ErrMessageNotFound indica que a mensagem não existe.
var ErrMessageNotFound = errors.New("mensagem não encontrada")

// ErrPermissionDenied indica que o usuário autenticado não tem permissão
// para a operação.
var ErrPermissionDenied = errors.New("permissão negada")

// ErrTooManyAttachments indica que a mensagem excede o limite de attachments
// por mensagem (10, README).
var ErrTooManyAttachments = errors.New("máximo de attachments por mensagem excedido")

// maxMessageContentLength é o tamanho máximo do content de uma mensagem
// (8192 caracteres, README).
const maxMessageContentLength = 8192

// maxAttachmentsPerMessage é o limite de attachments por mensagem (10,
// README).
const maxAttachmentsPerMessage = 10

// messageListLimit é o limite de mensagens por requisição de listagem
// (README: limite de 100 por requisição).
const messageListLimit = 100

// ListMessages lista as mensagens de um canal com seus attachments, em ordem
// decrescente de criação (README: GET /channels/:channel_id/messages).
// Se since for fornecido, retorna apenas mensagens criadas após esse
// timestamp (polling de novas mensagens); se lastID for fornecido junto, o
// cursor é o par (created_at, id) e mensagens do mesmo timestamp com id
// menor que lastID também são incluídas (evita pular mensagens com timestamp
// igual).
// O dono do servidor do canal sempre pode ler; em canais sem roles com
// permissões definidas a leitura é livre para todos; nos demais, o usuário
// precisa da permissão read_channel em ao menos uma das roles do servidor.
// Retorna ErrChannelNotFound quando o canal não existe e
// ErrPermissionDenied quando o usuário não pode ler o canal.
func ListMessages(ctx context.Context, channelID, userID string, since *time.Time, lastID string) (models.MessageList, error) {
	if channelID == "" {
		return models.MessageList{}, ErrChannelNotFound
	}

	channel, err := storage.GetChannelByID(ctx, channelID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.MessageList{}, ErrChannelNotFound
	}
	if err != nil {
		return models.MessageList{}, err
	}

	allowed, err := userHasChannelPermission(ctx, channel, userID, true, func(p models.ChannelPermission) bool {
		return p.ReadChannel
	})
	if err != nil {
		return models.MessageList{}, err
	}
	if !allowed {
		return models.MessageList{}, ErrPermissionDenied
	}

	messages, err := storage.ListMessagesWithAttachmentsByChannel(ctx, channelID, since, lastID, messageListLimit)
	if err != nil {
		return models.MessageList{}, err
	}

	hasMore := len(messages) > messageListLimit
	if hasMore {
		messages = messages[:messageListLimit]
	}

	return models.MessageList{ChannelID: channelID, Messages: messages, HasMore: hasMore}, nil
}

// CreateMessage cria uma nova mensagem em um canal (README: POST /messages),
// com attachments opcionais enviados no mesmo multipart.
// O content é opcional (a mensagem pode ter apenas attachments), mas a
// mensagem precisa de content ou de pelo menos um attachment; content vazio
// é gravado como NULL.
// O usuário precisa da permissão send_messages do canal (o dono do servidor
// do canal sempre pode e em canais sem roles definidas o envio é livre); se
// a mensagem tiver attachments, o usuário também precisa da permissão
// send_attachment em uma das roles do servidor.
//
// A gravação com attachment acontece em 3 etapas, não atômicas entre si:
//  1. grava o registro de attachment no banco (com o sha_hash do conteúdo);
//  2. grava o blob no storage, apenas se o sha_hash não estiver duplicado;
//  3. grava o registro de mensagem e vincula os attachments.
//
// Se uma etapa falhar, os registros órfãos são limpos pela rotina de
// manutenção (cron).
//
// O nome de cada attachment é sanitizado e o MIME type é detectado a partir
// do conteúdo (o header de content type do upload não é confiado); o
// tamanho é calculado dos bytes gravados.
//
// Retorna ErrInvalidInput quando channel_id ou author_id estão ausentes,
// quando content excede 8192 caracteres, quando a mensagem não tem content
// nem attachments ou quando um attachment tem nome inválido,
// ErrTooManyAttachments quando a mensagem tem mais de 10 attachments,
// ErrChannelNotFound quando o canal não existe,
// ErrPermissionDenied quando o autor não pode enviar mensagens no canal ou
// enviar attachments no servidor e ErrAttachmentTooLarge quando um arquivo
// excede 100MB.
func CreateMessage(ctx context.Context, channelID, authorID, content string, attachments []AttachmentInput) (models.MessageWithAttachment, error) {
	if channelID == "" {
		return models.MessageWithAttachment{}, ErrChannelNotFound
	}
	if authorID == "" || utf8.RuneCountInString(content) > maxMessageContentLength {
		return models.MessageWithAttachment{}, ErrInvalidInput
	}
	if content == "" && len(attachments) == 0 {
		return models.MessageWithAttachment{}, ErrInvalidInput
	}
	if len(attachments) > maxAttachmentsPerMessage {
		return models.MessageWithAttachment{}, ErrTooManyAttachments
	}
	for _, att := range attachments {
		fileName := utils.SanitizeFileName(att.OriginalFileName)
		if fileName == "" || utf8.RuneCountInString(fileName) > maxAttachmentNameLength {
			return models.MessageWithAttachment{}, ErrInvalidInput
		}
	}

	channel, err := storage.GetChannelByID(ctx, channelID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.MessageWithAttachment{}, ErrChannelNotFound
	}
	if err != nil {
		return models.MessageWithAttachment{}, err
	}

	allowed, err := userHasChannelPermission(ctx, channel, authorID, true, func(p models.ChannelPermission) bool {
		return p.SendMessages
	})
	if err != nil {
		return models.MessageWithAttachment{}, err
	}
	if !allowed {
		return models.MessageWithAttachment{}, ErrPermissionDenied
	}

	if len(attachments) > 0 {
		allowed, err := userHasRolePermission(ctx, channel.ServerID, authorID, func(p models.RolePermissions) bool {
			return p.SendAttachment
		})
		if err != nil {
			return models.MessageWithAttachment{}, err
		}
		if !allowed {
			return models.MessageWithAttachment{}, ErrPermissionDenied
		}
	}

	createdAttachments := make([]models.Attachments, 0, len(attachments))
	attachmentIDs := make([]string, 0, len(attachments))
	for _, att := range attachments {
		created, err := storeAttachment(ctx, att, authorID)
		if err != nil {
			return models.MessageWithAttachment{}, err
		}
		createdAttachments = append(createdAttachments, created)
		attachmentIDs = append(attachmentIDs, created.ID)
	}

	message, err := storage.CreateMessage(ctx, channelID, authorID, content, attachmentIDs)
	if err != nil {
		return models.MessageWithAttachment{}, err
	}

	return models.MessageWithAttachment{Message: message, Attachments: toMessageAttachments(createdAttachments)}, nil
}

// EditMessage edita o conteúdo de uma mensagem (README:
// PUT /messages/:message_id). Somente o autor da mensagem pode editá-la.
// Content vazio é aceito e limpa o texto da mensagem (NULL); os attachments
// não são afetados.
// Retorna ErrMessageNotFound quando a mensagem não existe, ErrInvalidInput
// quando content excede 8192 caracteres e ErrPermissionDenied quando o
// usuário não é o autor.
func EditMessage(ctx context.Context, messageID, authorID, content string) (models.MessageWithAttachment, error) {
	if messageID == "" {
		return models.MessageWithAttachment{}, ErrMessageNotFound
	}
	if authorID == "" || utf8.RuneCountInString(content) > maxMessageContentLength {
		return models.MessageWithAttachment{}, ErrInvalidInput
	}

	message, err := storage.GetMessageByID(ctx, messageID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.MessageWithAttachment{}, ErrMessageNotFound
	}
	if err != nil {
		return models.MessageWithAttachment{}, err
	}

	if message.AuthorID == nil || *message.AuthorID != authorID {
		return models.MessageWithAttachment{}, ErrPermissionDenied
	}

	// content vazio vira NULL (limpa o texto da mensagem)
	var contentPtr *string
	if content != "" {
		contentPtr = &content
	}

	updated, err := storage.UpdateMessage(ctx, messageID, models.Message{Content: contentPtr})
	if errors.Is(err, storage.ErrNotFound) {
		return models.MessageWithAttachment{}, ErrMessageNotFound
	}
	if err != nil {
		return models.MessageWithAttachment{}, err
	}

	attachments, err := storage.ListAttachmentsByMessage(ctx, updated.ID)
	if err != nil {
		return models.MessageWithAttachment{}, err
	}

	return models.MessageWithAttachment{Message: updated, Attachments: toMessageAttachments(attachments)}, nil
}

// toMessageAttachments converte os attachments completos na informação mínima
// exposta nas respostas de mensagens.
func toMessageAttachments(attachments []models.Attachments) []models.MessageAttachment {
	converted := make([]models.MessageAttachment, 0, len(attachments))
	for _, attachment := range attachments {
		converted = append(converted, models.MessageAttachment{
			ID:               attachment.ID,
			MimeType:         attachment.MimeType,
			OriginalFileName: attachment.OriginalFileName,
			SizeBytes:        attachment.SizeBytes,
			CreatedAt:        attachment.CreatedAt,
		})
	}
	return converted
}

// DeleteMessage exclui uma mensagem (README: DELETE /messages/:message_id).
// O autor da mensagem ou um usuário com a permissão delete_messages do canal
// pode excluí-la (o dono do servidor do canal sempre pode). A permissão
// delete_messages precisa ser concedida explicitamente em uma role; ela não
// é livre em canal aberto.
// Retorna o channel_id da mensagem excluída (para o evento WebSocket de
// exclusão) e ErrMessageNotFound quando a mensagem não existe ou
// ErrPermissionDenied quando o usuário não pode excluí-la.
func DeleteMessage(ctx context.Context, messageID, authorID string) (string, error) {
	if messageID == "" {
		return "", ErrMessageNotFound
	}
	if authorID == "" {
		return "", ErrInvalidInput
	}

	message, err := storage.GetMessageByID(ctx, messageID)
	if errors.Is(err, storage.ErrNotFound) {
		return "", ErrMessageNotFound
	}
	if err != nil {
		return "", err
	}

	if message.AuthorID != nil && *message.AuthorID == authorID {
		return message.ChannelID, deleteMessage(ctx, messageID)
	}

	channel, err := storage.GetChannelByID(ctx, message.ChannelID)
	if errors.Is(err, storage.ErrNotFound) {
		return "", ErrChannelNotFound
	}
	if err != nil {
		return "", err
	}

	allowed, err := userHasChannelPermission(ctx, channel, authorID, false, func(p models.ChannelPermission) bool {
		return p.DeleteMessages
	})
	if err != nil {
		return "", err
	}
	if !allowed {
		return "", ErrPermissionDenied
	}

	return message.ChannelID, deleteMessage(ctx, messageID)
}

// deleteMessage exclui a mensagem e mapeia o erro de registro inexistente.
func deleteMessage(ctx context.Context, messageID string) error {
	if err := storage.DeleteMessage(ctx, messageID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrMessageNotFound
		}
		return err
	}

	return nil
}

// userHasChannelPermission verifica se o usuário possui a permissão de canal
// informada. O dono do servidor do canal possui implicitamente todas as
// permissões (mesma regra do middleware de permissão de roles).
//
// O parâmetro freeIfOpen controla o comportamento em canal aberto (canal sem
// roles com permissões definidas): quando true, a permissão é livre para
// todos (usado para read_channel e send_messages); quando false, a permissão
// precisa ser concedida explicitamente em uma role mesmo em canal aberto
// (usado para delete_messages). Nos demais casos, o usuário precisa da
// permissão em ao menos uma das roles atribuídas a ele no servidor do canal.
func userHasChannelPermission(ctx context.Context, channel models.Channel, userID string, freeIfOpen bool, hasPermission func(models.ChannelPermission) bool) (bool, error) {
	server, err := storage.GetServerByID(ctx, channel.ServerID)
	if err != nil {
		return false, err
	}

	if server.OwnerID != nil && *server.OwnerID == userID {
		return true, nil
	}

	if freeIfOpen && len(channel.Permissions) == 0 {
		return true, nil
	}

	roles, err := storage.GetRolesByUser(ctx, userID)
	if err != nil {
		return false, err
	}

	for _, role := range roles {
		if role.ServerID != channel.ServerID {
			continue
		}
		if permission, ok := channel.Permissions[role.ID]; ok && hasPermission(permission) {
			return true, nil
		}
	}

	return false, nil
}

// userHasRolePermission verifica se o usuário possui a permissão de role
// informada no servidor. O dono do servidor possui implicitamente todas as
// permissões (mesma regra do middleware de permissão de roles); os demais
// usuários precisam da permissão em ao menos uma das roles atribuídas a eles
// no servidor.
func userHasRolePermission(ctx context.Context, serverID, userID string, hasPermission func(models.RolePermissions) bool) (bool, error) {
	server, err := storage.GetServerByID(ctx, serverID)
	if err != nil {
		return false, err
	}

	if server.OwnerID != nil && *server.OwnerID == userID {
		return true, nil
	}

	roles, err := storage.GetRolesByUser(ctx, userID)
	if err != nil {
		return false, err
	}

	for _, role := range roles {
		if role.ServerID == serverID && hasPermission(role.Permissions) {
			return true, nil
		}
	}

	return false, nil
}
