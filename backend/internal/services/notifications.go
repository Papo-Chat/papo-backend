package services

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"
	"unicode/utf8"

	"papo/internal/config"
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
	notificationMentionRegex  = regexp.MustCompile(`(?i)@mention\(<@([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})>\)`)
	notificationEveryoneRegex = regexp.MustCompile(`(?i)@everyone\b`)
)

// translatePushMentions troca @mention(<@userId>) por @nickname, com
// fallback para @username, apenas no preview enviado ao push.
func translatePushMentions(ctx context.Context, content string) string {
	matches := notificationMentionRegex.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 { return content }

	seen := make(map[string]struct{}, len(matches))
	ids := make([]string, 0, len(matches))
	for _, match := range matches {
		id := match[1]
		if _, ok := seen[id]; ok { continue }
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	users, err := storage.GetUsersByIDs(ctx, ids)
	if err != nil {
		utils.Errorf("notificações: falha ao traduzir menções para push: %v", err)
		return content
	}
	names := make(map[string]string, len(users))
	for _, user := range users {
		name := user.Username
		if user.Nickname != nil && *user.Nickname != "" { name = *user.Nickname }
		names[user.ID] = name
	}

	return notificationMentionRegex.ReplaceAllStringFunc(content, func(raw string) string {
		match := notificationMentionRegex.FindStringSubmatch(raw)
		if len(match) != 2 { return raw }
		if name, ok := names[match[1]]; ok { return "@" + name }
		return raw
	})
}

// truncateNotificationContent limita o conteúdo ao preview de notificação
// (512 caracteres, rune-safe).
func truncateNotificationContent(content string) string {
	if utf8.RuneCountInString(content) <= maxNotificationPreviewLength {
		return content
	}

	return string([]rune(content)[:maxNotificationPreviewLength])
}

// buildPushJobPayload serializa o payload mínimo gravado em push_outbox.payload
// (JSONB). O papo-push usa `author` como título e `preview` (truncado) para
// montar o corpo conforme PUSH_PREVIEW (full/generic/hidden).
func buildPushJobPayload(notificationID *string, messageID, channelID, author string, content string) string {
	payload := models.PushJobPayload{
		Type:           "message",
		NotificationID: notificationID,
		MessageID:      messageID,
		ChannelID:      channelID,
		Author:         author,
		Preview:        truncateNotificationContent(content),
	}

	b, err := json.Marshal(payload)
	if err != nil {
		utils.Errorf("falha ao serializar o payload do job de push: %v", err)
		return ""
	}

	return string(b)
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
// Triggers (geram row + evento): menção direta @mention(<@user_id>) no conteúdo,
// reply_to de uma mensagem do usuário e @everyone (somente quando o autor
// tem a permissão everyone_message; sem a permissão, @everyone não faz
// nada). O autor da mensagem nunca é notificado. Configuração 'all' sem
// trigger: só evento (id efêmero, sem row). Configuração 'off': nada.
// Somente usuários que podem ler o canal (mesma regra do broadcast do
// canal) são notificados.
func DispatchMessageNotifications(ctx context.Context, requestID string, message models.Message) []NotificationDelivery {
	cfg := config.LoadConfig()
	authorID := ""
	content := ""
	if message.AuthorID != nil {
		authorID = *message.AuthorID
	}
	if message.Content != nil {
		content = *message.Content
	}
	pushContent := content
	if cfg.UseFCMRelay {
		pushContent = translatePushMentions(ctx, content)
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

	// Nome do autor para o título da notificação push (payload mínimo).
	authorUsername := ""
	if authorID != "" {
		author, err := storage.GetUserByID(ctx, authorID)
		if err != nil {
			utils.Errorf("request_id=%s notificações: falha ao buscar o autor %s: %v",
				requestID, authorID, err)
		} else {
			authorUsername = author.Username
		}
	}

	// Filtra os candidatos pelos leitores do canal (mesma regra do
	// broadcast): canal aberto permite todos; canal com permissões permite
	// o dono do servidor e os usuários de roles com ReadChannel.
	if len(candidates) > 0 {
		candidateIDs := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			candidateIDs = append(candidateIDs, candidate.UserID)
		}
		allowed, err := ChannelReaders(ctx, message.ChannelID, candidateIDs)
		if err != nil {
			utils.Errorf("request_id=%s notificações: falha ao verificar a leitura do canal %s: %v",
				requestID, message.ChannelID, err)
			return nil
		}
		readable := make([]models.ChannelNotificationCandidate, 0, len(candidates))
		for _, candidate := range candidates {
			if allowed[candidate.UserID] {
				readable = append(readable, candidate)
			}
		}
		candidates = readable
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
		var notificationID *string
		if isTriggered {
			// Notificação (row) + job de push na mesma transação: a entrega
			// push nunca depende da disponibilidade do worker.
			tx, err := storage.GetDB().BeginTx(ctx, nil)
			if err != nil {
				utils.Errorf("request_id=%s notificações: falha ao abrir transação para o usuário %s: %v",
					requestID, candidate.UserID, err)
				continue
			}
			defer tx.Rollback()

			notification, err := storage.CreateNotificationInTx(tx, ctx, candidate.UserID, message.ID)
			if err != nil {
				utils.Errorf("request_id=%s notificações: falha ao criar a notificação do usuário %s: %v",
					requestID, candidate.UserID, err)
				continue
			}

			//Não precisamos guardar no banco o que não vamos usar
			if cfg.UseFCMRelay {
				notificationID = &notification.ID

				payload := buildPushJobPayload(notificationID, message.ID, message.ChannelID, authorUsername, pushContent)
				if err := storage.CreatePushJobInTx(tx, ctx, candidate.UserID, notificationID, payload); err != nil {
					utils.Errorf("request_id=%s notificações: falha ao gravar o job de push do usuário %s: %v",
						requestID, candidate.UserID, err)
					continue
				}
			}
			eventID = notification.ID

			if err := tx.Commit(); err != nil {
				utils.Errorf("request_id=%s notificações: falha ao commitar a notificação do usuário %s: %v",
					requestID, candidate.UserID, err)
				continue
			}
		} else {
			// Configuração 'all' sem trigger: evento com id efêmero (sem row).
			ephemeralID, err := utils.NewUUIDv4()
			if err != nil {
				utils.Errorf("request_id=%s notificações: falha ao gerar UUID efêmero: %v", requestID, err)
				continue
			}
			eventID = ephemeralID

			if cfg.UseFCMRelay {
				notificationID = &ephemeralID
				payload := buildPushJobPayload(notificationID, message.ID, message.ChannelID, authorUsername, pushContent)
				if err := storage.CreatePushJob(ctx, candidate.UserID, notificationID, payload); err != nil {
					utils.Errorf("request_id=%s notificações: falha ao gravar o job de push do usuário %s: %v",
						requestID, candidate.UserID, err)
					continue
				}
			}
		}

		deliveries = append(deliveries, NotificationDelivery{
			UserID:         candidate.UserID,
			EventID:        eventID,
			MessageContent: truncateNotificationContent(content),
		})
	}

	return deliveries
}
