package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"papo/internal/models"
)

const channelColumns = "id, server_id, name, permissions, type, position, created_at"

// ErrPositionConflict indica que a posição atual do canal não corresponde
// à posição informada na requisição.
var ErrPositionConflict = errors.New("posição do canal desatualizada")

// ErrInvalidPosition indica que a posição informada está fora do intervalo
// de posições válidas do servidor.
var ErrInvalidPosition = errors.New("posição de canal inválida")

// channelPositionLockKey é a chave do advisory lock que serializa a
// alocação de posições de canais por servidor (criação e mudança de
// posição), evitando posições duplicadas em escrita concorrente.
func channelPositionLockKey(serverID string) string {
	return "papo:channel-position:" + serverID
}

func scanChannel(row rowScanner) (models.Channel, error) {
	var channel models.Channel
	var permissions []byte
	err := row.Scan(
		&channel.ID,
		&channel.ServerID,
		&channel.Name,
		&permissions,
		&channel.Type,
		&channel.Position,
		&channel.CreatedAt,
	)
	if err != nil {
		return models.Channel{}, err
	}

	channel.Permissions = make(map[string]models.ChannelPermission)
	if len(permissions) > 0 {
		if err := json.Unmarshal(permissions, &channel.Permissions); err != nil {
			return models.Channel{}, fmt.Errorf("falha ao decodificar permissões do canal: %w", err)
		}
	}

	return channel, nil
}

// CreateChannel cria um novo canal com permissões vazias e retorna o registro criado.
// A position é calculada pelo backend: próxima posição do servidor (máximo + 1),
// com a alocação serializada por servidor para evitar posições duplicadas.
func CreateChannel(ctx context.Context, serverID, name, channelType string) (models.Channel, error) {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return models.Channel{}, fmt.Errorf("falha ao criar canal: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		"SELECT pg_advisory_xact_lock(hashtext($1))",
		channelPositionLockKey(serverID),
	); err != nil {
		return models.Channel{}, fmt.Errorf("falha ao criar canal: %w", err)
	}

	row := tx.QueryRowContext(ctx,
		`INSERT INTO channels (server_id, name, type, position)
		 VALUES ($1, $2, $3, (SELECT COALESCE(MAX(position), 0) + 1 FROM channels WHERE server_id = $1))
		 RETURNING `+channelColumns,
		serverID, name, channelType,
	)

	channel, err := scanChannel(row)
	if err != nil {
		return models.Channel{}, mapStorageError(err)
	}

	if err := tx.Commit(); err != nil {
		return models.Channel{}, fmt.Errorf("falha ao criar canal: %w", err)
	}

	return channel, nil
}

// CountChannelsByServer conta os canais de um servidor.
func CountChannelsByServer(ctx context.Context, serverID string) (int, error) {
	var count int
	err := GetDB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM channels WHERE server_id = $1",
		serverID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("falha ao contar canais: %w", err)
	}

	return count, nil
}

// GetChannelByID busca um canal pelo id.
func GetChannelByID(ctx context.Context, id string) (models.Channel, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+channelColumns+" FROM channels WHERE id = $1",
		id,
	)

	channel, err := scanChannel(row)
	if err != nil {
		return models.Channel{}, mapStorageError(err)
	}

	return channel, nil
}

// ListChannelsByServer lista os canais de um servidor ordenados por posição
// (position), com created_at e id como desempate.
func ListChannelsByServer(ctx context.Context, serverID string) ([]models.Channel, error) {
	rows, err := GetDB().QueryContext(ctx,
		"SELECT "+channelColumns+" FROM channels WHERE server_id = $1 ORDER BY position, created_at, id LIMIT 500",
		serverID,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar canais: %w", err)
	}
	defer rows.Close()

	channels := make([]models.Channel, 0)
	for rows.Next() {
		channel, err := scanChannel(rows)
		if err != nil {
			return nil, fmt.Errorf("falha ao ler canal: %w", err)
		}
		channels = append(channels, channel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar canais: %w", err)
	}

	return channels, nil
}

// UpdateChannel renomeia um canal pelo id e retorna o canal atualizado.
// Nome duplicado retorna ErrUniqueViolation.
func UpdateChannel(ctx context.Context, id, name string) (models.Channel, error) {
	row := GetDB().QueryRowContext(ctx,
		"UPDATE channels SET name = $2 WHERE id = $1 RETURNING "+channelColumns,
		id, name,
	)

	channel, err := scanChannel(row)
	if err != nil {
		return models.Channel{}, mapStorageError(err)
	}

	return channel, nil
}

// UpdateChannelPermissions define as permissões de uma role em um canal
// e retorna o canal atualizado.
func UpdateChannelPermissions(ctx context.Context, channelID, roleID string, permission models.ChannelPermission) (models.Channel, error) {
	permissionJSON, err := json.Marshal(permission)
	if err != nil {
		return models.Channel{}, fmt.Errorf("falha ao codificar permissões do canal: %w", err)
	}

	row := GetDB().QueryRowContext(ctx,
		`UPDATE channels
		 SET permissions = COALESCE(permissions, '{}'::jsonb) || jsonb_build_object($2::text, $3::jsonb)
		 WHERE id = $1
		 RETURNING `+channelColumns,
		channelID, roleID, string(permissionJSON),
	)

	channel, err := scanChannel(row)
	if err != nil {
		return models.Channel{}, mapStorageError(err)
	}

	return channel, nil
}

// ChangeChannelPosition move um canal para newPosition e recalcula as
// posições dos demais canais do servidor (as posições permanecem contíguas,
// de 1 até o número de canais). A operação é serializada por servidor com o
// mesmo advisory lock da criação.
// Retorna ErrNotFound quando o canal não existe, ErrPositionConflict quando
// o canal não está em oldPosition e ErrInvalidPosition quando newPosition
// está fora do intervalo.
func ChangeChannelPosition(ctx context.Context, channelID string, oldPosition, newPosition int) (models.Channel, error) {
	var serverID string
	if err := GetDB().QueryRowContext(ctx,
		"SELECT server_id FROM channels WHERE id = $1",
		channelID,
	).Scan(&serverID); err != nil {
		return models.Channel{}, mapStorageError(err)
	}

	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return models.Channel{}, fmt.Errorf("falha ao mudar posição do canal: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		"SELECT pg_advisory_xact_lock(hashtext($1))",
		channelPositionLockKey(serverID),
	); err != nil {
		return models.Channel{}, fmt.Errorf("falha ao mudar posição do canal: %w", err)
	}

	var current int
	if err := tx.QueryRowContext(ctx,
		"SELECT position FROM channels WHERE id = $1 FOR UPDATE",
		channelID,
	).Scan(&current); err != nil {
		return models.Channel{}, mapStorageError(err)
	}

	if current != oldPosition {
		return models.Channel{}, ErrPositionConflict
	}

	var count int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM channels WHERE server_id = $1",
		serverID,
	).Scan(&count); err != nil {
		return models.Channel{}, fmt.Errorf("falha ao mudar posição do canal: %w", err)
	}
	if newPosition < 1 || newPosition > count {
		return models.Channel{}, ErrInvalidPosition
	}

	// Desloca o intervalo de posições entre o ponto atual e o destino para
	// abrir (ou fechar) espaço para o canal movido.
	if newPosition > oldPosition {
		if _, err := tx.ExecContext(ctx,
			"UPDATE channels SET position = position - 1 WHERE server_id = $1 AND position > $2 AND position <= $3",
			serverID, oldPosition, newPosition,
		); err != nil {
			return models.Channel{}, fmt.Errorf("falha ao mudar posição do canal: %w", err)
		}
	} else if newPosition < oldPosition {
		if _, err := tx.ExecContext(ctx,
			"UPDATE channels SET position = position + 1 WHERE server_id = $1 AND position >= $2 AND position < $3",
			serverID, newPosition, oldPosition,
		); err != nil {
			return models.Channel{}, fmt.Errorf("falha ao mudar posição do canal: %w", err)
		}
	}

	row := tx.QueryRowContext(ctx,
		"UPDATE channels SET position = $2 WHERE id = $1 RETURNING "+channelColumns,
		channelID, newPosition,
	)

	channel, err := scanChannel(row)
	if err != nil {
		return models.Channel{}, mapStorageError(err)
	}

	if err := tx.Commit(); err != nil {
		return models.Channel{}, fmt.Errorf("falha ao mudar posição do canal: %w", err)
	}

	return channel, nil
}

// DeleteChannel exclui um canal pelo id.
func DeleteChannel(ctx context.Context, id string) error {
	result, err := GetDB().ExecContext(ctx,
		"DELETE FROM channels WHERE id = $1",
		id,
	)
	if err != nil {
		return fmt.Errorf("falha ao excluir canal: %w", err)
	}

	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	return nil
}

// channelSummaryColumns é a seleção da visão ChannelSummary: dados do canal,
// última mensagem (LATERAL, pode ser NULL) e o username do autor da última
// mensagem (LEFT JOIN, pode ser NULL).
const channelSummaryColumns = `c.id, c.server_id, c.name, c.permissions, c.type, c.position, c.created_at,
	lm.id, lm.content, lm.author_id, u.username, lm.created_at`

// channelSummaryJoins traz a última mensagem de cada canal (mesma ordem de
// ListMessagesByChannel) e o username do autor.
const channelSummaryJoins = `FROM channels c
	LEFT JOIN LATERAL (
		SELECT m.id, m.content, m.author_id, m.created_at
		FROM messages m
		WHERE m.channel_id = c.id
		ORDER BY m.created_at DESC, m.id DESC
		LIMIT 1
	) lm ON true
	LEFT JOIN users u ON u.id = lm.author_id`

func scanChannelSummary(row rowScanner, roleNames map[string]string) (models.ChannelSummary, error) {
	var summary models.ChannelSummary
	var permissions []byte
	var lastMessageID *string
	var lastMessageContent *string
	var lastMessageAuthorID *string
	var lastMessageUsername *string
	var lastMessageCreatedAt *time.Time

	err := row.Scan(
		&summary.ID,
		&summary.ServerID,
		&summary.Name,
		&permissions,
		&summary.Type,
		&summary.Position,
		&summary.CreatedAt,
		&lastMessageID,
		&lastMessageContent,
		&lastMessageAuthorID,
		&lastMessageUsername,
		&lastMessageCreatedAt,
	)
	if err != nil {
		return models.ChannelSummary{}, err
	}

	if lastMessageID != nil && lastMessageCreatedAt != nil {
		summary.LastMessage = &models.ChannelLastMessage{
			ID:             *lastMessageID,
			Content:        lastMessageContent,
			AuthorID:       lastMessageAuthorID,
			AuthorUsername: lastMessageUsername,
			CreatedAt:      *lastMessageCreatedAt,
		}
	}

	summary.Permissions = make([]models.ChannelPermissionEntry, 0)
	if len(permissions) > 0 {
		raw := make(map[string]models.ChannelPermission)
		if err := json.Unmarshal(permissions, &raw); err != nil {
			return models.ChannelSummary{}, fmt.Errorf("falha ao decodificar permissões do canal: %w", err)
		}

		roleIDs := make([]string, 0, len(raw))
		for roleID := range raw {
			roleIDs = append(roleIDs, roleID)
		}
		sort.Strings(roleIDs)

		for _, roleID := range roleIDs {
			summary.Permissions = append(summary.Permissions, models.ChannelPermissionEntry{
				RoleID:      roleID,
				RoleName:    roleNames[roleID],
				Permissions: raw[roleID],
			})
		}
	}

	return summary, nil
}

// listRoleNames carrega os nomes de todas as roles (id -> name) para
// expandir as permissões dos canais.
func listRoleNames(ctx context.Context) (map[string]string, error) {
	rows, err := GetDB().QueryContext(ctx, "SELECT id, name FROM roles")
	if err != nil {
		return nil, fmt.Errorf("falha ao listar roles: %w", err)
	}
	defer rows.Close()

	names := make(map[string]string)
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("falha ao ler role: %w", err)
		}
		names[id] = name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar roles: %w", err)
	}

	return names, nil
}

// ListChannelSummaries lista os canais com a visão ChannelSummary, ordenados
// por posição (position). serverID é opcional (filtro por servidor).
func ListChannelSummaries(ctx context.Context, serverID *string) ([]models.ChannelSummary, error) {
	query := "SELECT " + channelSummaryColumns + " " + channelSummaryJoins
	args := []any{}
	if serverID != nil && *serverID != "" {
		query += " WHERE c.server_id = $1"
		args = append(args, *serverID)
	}
	query += " ORDER BY c.position, c.created_at, c.id LIMIT 500"

	rows, err := GetDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar canais: %w", err)
	}
	defer rows.Close()

	roleNames, err := listRoleNames(ctx)
	if err != nil {
		return nil, err
	}

	channels := make([]models.ChannelSummary, 0)
	for rows.Next() {
		summary, err := scanChannelSummary(rows, roleNames)
		if err != nil {
			return nil, fmt.Errorf("falha ao ler canal: %w", err)
		}
		channels = append(channels, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar canais: %w", err)
	}

	return channels, nil
}

// GetChannelSummary busca um canal pelo id com a visão ChannelSummary.
func GetChannelSummary(ctx context.Context, id string) (models.ChannelSummary, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+channelSummaryColumns+" "+channelSummaryJoins+" WHERE c.id = $1",
		id,
	)

	roleNames, err := listRoleNames(ctx)
	if err != nil {
		return models.ChannelSummary{}, err
	}

	summary, err := scanChannelSummary(row, roleNames)
	if err != nil {
		return models.ChannelSummary{}, mapStorageError(err)
	}

	return summary, nil
}
