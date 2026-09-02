package services

import (
	"context"
	"errors"
	"regexp"
	"time"
	"unicode/utf8"

	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"
)

// ErrNotificationNotFound indica que a notificação não existe.
var ErrNotificationNotFound = errors.New("notificação não encontrada")

const (
	notificationListLimit        = 100
	maxNotificationIDsPerRead    = 1000
	maxNotificationPreviewLength = 512
)

var (
	notificationMentionRegex  = regexp.MustCompile(`(?i)@([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)
	notificationEveryoneRegex = regexp.MustCompile(`(?i)@everyone\b`)
)

// truncateNotificationContent limita o conteúdo ao preview de notificação
// (512 caracteres, rune-safe).
func truncateNotificationContent(content string) string {
	if utf8.RuneCountInString(content) <= maxNotificationPreviewLength {
		return content
	}

	return string([]rune(content)[:maxNotificationPreviewLength])
}

// UpdateChannelUserSetting define a configuração de notificação do usuário
// em um canal (POST /channels/:channel_id/user/:user_id/settings).
// Retorna ErrPermissionDenied quando o ator não é o usuário alvo,
// ErrChannelNotFound quando o canal não existe e ErrInvalidInput quando o
// tipo informado não é off/only_mentions/all.
func UpdateChannelUserSetting(ctx context.Context, actorID, channelID, targetID, notificationSettings string) (models.ChannelUserSetting, error) {
	if actorID != targetID {
		return models.ChannelUserSetting{}, ErrPermissionDenied
	}
	if notificationSettings != models.NotificationTypeOff &&
		notificationSettings != models.NotificationTypeOnlyMentions &&
		notificationSettings != models.NotificationTypeAll {
		return models.ChannelUserSetting{}, ErrInvalidInput
	}

	if _, err := storage.GetChannelByID(ctx, channelID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return models.ChannelUserSetting{}, ErrChannelNotFound
		}
		return models.ChannelUserSetting{}, err
	}

	return storage.UpsertChannelUserSetting(ctx, targetID, channelID, notificationSettings)
}

// ListUserNotifications lista as notificações do usuário (somente o próprio
// usuário, GET /users/:user_id/notifications), mais recentes primeiro, com
// o cursor since + lastID (mesma convenção de mensagens) e limite de 100.
// O preview do conteúdo da mensagem é truncado a 512 caracteres.
func ListUserNotifications(ctx context.Context, actorID, targetID string, since *time.Time, lastID string) (models.NotificationList, error) {
	if actorID != targetID {
		return models.NotificationList{}, ErrPermissionDenied
	}

	summaries, err := storage.ListUserNotifications(ctx, targetID, since, lastID, notificationListLimit)
	if err != nil {
		return models.NotificationList{}, err
	}

	hasMore := len(summaries) > notificationListLimit
	if hasMore {
		summaries = summaries[:notificationListLimit]
	}

	for i := range summaries {
		summaries[i].MessageContent = truncateNotificationContent(summaries[i].MessageContent)
	}

	return models.NotificationList{Notifications: summaries, HasMore: hasMore}, nil
}

// MarkUserNotificationsRead marca as notificações do usuário como lidas
// (somente o próprio usuário, PUT /users/:user_id/read_notification) e
// retorna o número de linhas afetadas.
// Retorna ErrInvalidInput quando a lista de ids está vazia, excede 1000 ou
// contém id vazio, ErrPermissionDenied quando o ator não é o usuário alvo e
// ErrNotificationNotFound quando nenhuma notificação foi atualizada.
func MarkUserNotificationsRead(ctx context.Context, actorID, targetID string, ids []string) (int, error) {
	if actorID != targetID {
		return 0, ErrPermissionDenied
	}
	if len(ids) == 0 || len(ids) > maxNotificationIDsPerRead {
		return 0, ErrInvalidInput
	}

	seen := make(map[string]struct{}, len(ids))
	uniqueIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			return 0, ErrInvalidInput
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniqueIDs = append(uniqueIDs, id)
	}

	affected, err := storage.MarkUserNotificationsRead(ctx, targetID, uniqueIDs)
	if err != nil {
		return 0, err
	}
	if affected == 0 {
		return 0, ErrNotificationNotFound
	}

	return int(affected), nil
}

// NotificationDelivery é uma entrega de notificação via WebSocket
// (new_notification): o usuário alvo, o id do evento (id da row de
// notificação quando há row, UUID efêmero quando a configuração 'all' não
// gera row) e o preview do conteúdo da mensagem (truncado a 512 caracteres).
type NotificationDelivery struct {
	UserID         string
	EventID        string
	MessageContent string
}

// DispatchMessageNotifications computa e persiste as notificações geradas
// por uma mensagem nova e retorna as entregas a enviar via WebSocket.
// Rotina async/best-effort: falhas individuais são logadas e puladas.
//
// Triggers (geram row + evento): menção direta @user_id no conteúdo,
// reply_to de uma mensagem do usuário e @everyone (somente quando o autor
// tem a permissão everyone_message; sem a permissão, @everyone não faz
// nada). O autor da mensagem nunca é notificado. Configuração 'all' sem
// trigger: só evento (id efêmero, sem row). Configuração 'off': nada.
func DispatchMessageNotifications(ctx context.Context, requestID string, message models.Message) []NotificationDelivery {
	authorID := ""
	content := ""
	if message.AuthorID != nil {
		authorID = *message.AuthorID
	}
	if message.Content != nil {
		content = *message.Content
	}

	triggered := make(map[string]bool)
	for _, match := range notificationMentionRegex.FindAllStringSubmatch(content, -1) {
		triggered[match[1]] = true
	}

	if message.ReplyTo != nil && *message.ReplyTo != "" {
		referenced, err := storage.GetMessageByID(ctx, *message.ReplyTo)
		if err != nil {
			if !errors.Is(err, storage.ErrNotFound) {
				utils.Errorf("request_id=%s notificações: falha ao buscar a mensagem referenciada %s: %v",
					requestID, *message.ReplyTo, err)
			}
		} else if referenced.AuthorID != nil && *referenced.AuthorID != "" {
			triggered[*referenced.AuthorID] = true
		}
	}

	hasEveryone := false
	if authorID != "" && notificationEveryoneRegex.MatchString(content) {
		allowed, err := userHasRolePermission(ctx, authorID, func(p models.RolePermissions) bool {
			return p.EveryoneMessage
		})
		if err != nil {
			utils.Errorf("request_id=%s notificações: falha ao verificar a permissão everyone_message do autor %s: %v",
				requestID, authorID, err)
		} else {
			hasEveryone = allowed
		}
	}

	candidates, err := storage.ListChannelNotificationCandidates(ctx, message.ChannelID)
	if err != nil {
		utils.Errorf("request_id=%s notificações: falha ao listar os candidatos do canal %s: %v",
			requestID, message.ChannelID, err)
		return nil
	}

	deliveries := make([]NotificationDelivery, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.UserID == authorID {
			continue
		}

		isTriggered := triggered[candidate.UserID] || hasEveryone
		if !isTriggered && candidate.NotificationSettings != models.NotificationTypeAll {
			continue
		}

		eventID := ""
		if isTriggered {
			notification, err := storage.CreateNotification(ctx, candidate.UserID, message.ID)
			if err != nil {
				utils.Errorf("request_id=%s notificações: falha ao criar a notificação do usuário %s: %v",
					requestID, candidate.UserID, err)
				continue
			}
			eventID = notification.ID
		} else {
			// Configuração 'all' sem trigger: evento com id efêmero (sem row).
			ephemeralID, err := utils.NewUUIDv4()
			if err != nil {
				utils.Errorf("request_id=%s notificações: falha ao gerar UUID efêmero: %v", requestID, err)
				continue
			}
			eventID = ephemeralID
		}

		deliveries = append(deliveries, NotificationDelivery{
			UserID:         candidate.UserID,
			EventID:        eventID,
			MessageContent: truncateNotificationContent(content),
		})
	}

	return deliveries
}
