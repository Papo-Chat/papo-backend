package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"papo/internal/models"
)

const channelUserSettingColumns = "user_id, channel_id, notification_settings, updated_at"

const notificationColumns = "id, user_id, message_id, read, created_at"

func scanChannelUserSetting(row rowScanner) (models.ChannelUserSetting, error) {
	var setting models.ChannelUserSetting
	err := row.Scan(
		&setting.UserID,
		&setting.ChannelID,
		&setting.NotificationSettings,
		&setting.UpdatedAt,
	)
	if err != nil {
		return models.ChannelUserSetting{}, err
	}

	return setting, nil
}

// UpsertChannelUserSetting cria ou atualiza a configuração de notificação do
// usuário em um canal e define updated_at com o tempo do banco. Retorna o
// registro resultante.
func UpsertChannelUserSetting(ctx context.Context, userID, channelID, notificationSettings string) (models.ChannelUserSetting, error) {
	row := GetDB().QueryRowContext(ctx,
		`INSERT INTO channel_user_settings (user_id, channel_id, notification_settings)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (user_id, channel_id) DO UPDATE
		 SET notification_settings = EXCLUDED.notification_settings, updated_at = NOW()
		 RETURNING `+channelUserSettingColumns,
		userID, channelID, notificationSettings,
	)

	setting, err := scanChannelUserSetting(row)
	if err != nil {
		return models.ChannelUserSetting{}, mapStorageError(err)
	}

	return setting, nil
}

// ListChannelNotificationCandidates retorna todos os usuários cuja
// configuração de notificação efetiva no canal NÃO é 'off' (usuários sem row
// usam o padrão only_mentions), com a configuração efetiva de cada um.
func ListChannelNotificationCandidates(ctx context.Context, channelID string) ([]models.ChannelNotificationCandidate, error) {
	rows, err := GetDB().QueryContext(ctx,
		`SELECT u.id, COALESCE(s.notification_settings, 'only_mentions')
		 FROM users u
		 LEFT JOIN channel_user_settings s ON s.user_id = u.id AND s.channel_id = $1
		 WHERE COALESCE(s.notification_settings, 'only_mentions') <> 'off'`,
		channelID,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar os candidatos a notificação do canal: %w", err)
	}
	defer rows.Close()

	candidates := make([]models.ChannelNotificationCandidate, 0)
	for rows.Next() {
		var candidate models.ChannelNotificationCandidate
		if err := rows.Scan(&candidate.UserID, &candidate.NotificationSettings); err != nil {
			return nil, fmt.Errorf("falha ao ler candidato a notificação: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar os candidatos a notificação do canal: %w", err)
	}

	return candidates, nil
}

func scanNotification(row rowScanner) (models.Notification, error) {
	var notification models.Notification
	err := row.Scan(
		&notification.ID,
		&notification.UserID,
		&notification.MessageID,
		&notification.Read,
		&notification.CreatedAt,
	)
	if err != nil {
		return models.Notification{}, err
	}

	return notification, nil
}

// CreateNotification cria a notificação do usuário para a mensagem. Quando a
// row já existe (mesmo usuário, mesma mensagem — disparo idempotente),
// retorna a row existente sem alterá-la.
func CreateNotification(ctx context.Context, userID, messageID string) (models.Notification, error) {
	row := GetDB().QueryRowContext(ctx,
		"INSERT INTO notifications (user_id, message_id) VALUES ($1, $2) "+
			"ON CONFLICT (user_id, message_id) DO NOTHING "+
			"RETURNING "+notificationColumns,
		userID, messageID,
	)

	notification, err := scanNotification(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			existing, err := scanNotification(GetDB().QueryRowContext(ctx,
				"SELECT "+notificationColumns+" FROM notifications WHERE user_id = $1 AND message_id = $2",
				userID, messageID,
			))
			if err != nil {
				return models.Notification{}, mapStorageError(err)
			}
			return existing, nil
		}
		return models.Notification{}, mapStorageError(err)
	}

	return notification, nil
}

// ListUserNotifications lista as notificações do usuário (com o conteúdo da
// mensagem via join) em ordem decrescente de criação. Se since for
// fornecido, retorna apenas notificações criadas após esse timestamp; se
// lastID for fornecido junto, o cursor é o par (created_at, id) e o filtro
// retorna as notificações anteriores ao cursor na ordem decrescente
// (created_at, id), incluindo as do mesmo timestamp com id menor que lastID
// (evita pular notificações com timestamp igual). É buscada 1 row a mais que
// o limite para o chamador determinar has_more.
func ListUserNotifications(ctx context.Context, userID string, since *time.Time, lastID string, limit int) ([]models.NotificationSummary, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	fetch := limit + 1

	query := "SELECT n.id, n.message_id, m.channel_id, m.author_id, m.content, n.read, n.created_at " +
		"FROM notifications n " +
		"JOIN messages m ON m.id = n.message_id " +
		"WHERE n.user_id = $1"
	args := []any{userID}

	if since != nil {
		if lastID != "" {
			query += " AND (n.created_at < $2 OR (n.created_at = $2 AND n.id < $3))"
			args = append(args, *since, lastID)
			query += " ORDER BY n.created_at DESC, n.id DESC LIMIT $4"
		} else {
			query += " AND n.created_at > $2"
			args = append(args, *since)
			query += " ORDER BY n.created_at DESC, n.id DESC LIMIT $3"
		}
	} else {
		query += " ORDER BY n.created_at DESC, n.id DESC LIMIT $2"
	}
	args = append(args, fetch)

	rows, err := GetDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar as notificações do usuário: %w", err)
	}
	defer rows.Close()

	notifications := make([]models.NotificationSummary, 0)
	for rows.Next() {
		var summary models.NotificationSummary
		var content *string
		if err := rows.Scan(
			&summary.ID,
			&summary.MessageID,
			&summary.ChannelID,
			&summary.AuthorID,
			&content,
			&summary.Read,
			&summary.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("falha ao ler notificação: %w", err)
		}
		if content != nil {
			summary.MessageContent = *content
		}
		notifications = append(notifications, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar as notificações do usuário: %w", err)
	}

	return notifications, nil
}

// MarkUserNotificationsRead marca as notificações do usuário (dadas por id)
// como lidas e retorna o número de linhas afetadas.
func MarkUserNotificationsRead(ctx context.Context, userID string, ids []string) (int64, error) {
	result, err := GetDB().ExecContext(ctx,
		"UPDATE notifications SET read = TRUE WHERE user_id = $1 AND id = ANY($2)",
		userID, ids,
	)
	if err != nil {
		return 0, fmt.Errorf("falha ao marcar as notificações como lidas: %w", err)
	}

	return result.RowsAffected()
}
