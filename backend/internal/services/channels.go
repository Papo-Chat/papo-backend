package services

import (
	"context"
	"errors"
	"unicode/utf8"

	"papo/internal/models"
	"papo/internal/storage"
)

// ErrChannelNotFound indica que o canal não existe.
var ErrChannelNotFound = errors.New("canal não encontrado")

// ErrChannelNameTaken indica que o nome do canal já está em uso.
var ErrChannelNameTaken = errors.New("nome do canal já existe")

// ErrRoleNotFound indica que a role não existe.
var ErrRoleNotFound = errors.New("role não encontrada")

// ErrChannelLimitReached indica que o servidor atingiu o limite de canais.
var ErrChannelLimitReached = errors.New("servidor atingiu o limite de canais")

// ErrChannelPositionConflict indica que a posição atual do canal não
// corresponde à old_position informada (a ordem mudou após a leitura).
var ErrChannelPositionConflict = errors.New("posição do canal desatualizada")

// maxChannelNameLength é o tamanho máximo do nome de um canal (32 caracteres, README).
const maxChannelNameLength = 32

// maxChannelsPerServer é o número máximo de canais por servidor (500, README).
const maxChannelsPerServer = 500

// ListChannels lista os canais com permissões expandidas e a última mensagem
// de cada um (README). serverID é opcional: quando informado, filtra os
// canais por servidor.
func ListChannels(ctx context.Context, serverID *string) ([]models.ChannelSummary, error) {
	if serverID != nil && *serverID == "" {
		serverID = nil
	}

	return storage.ListChannelSummaries(ctx, serverID)
}

// CreateChannel cria um novo canal em um servidor
// (README: o body de criação tem server_id, name e type; type é
// opcional e padrão "text", aceita "text" ou "category").
// Retorna ErrInvalidInput quando o nome está vazio ou acima de 32
// caracteres, quando o type é inválido, ErrServerNotFound quando o
// servidor não existe, ErrChannelLimitReached quando o servidor já
// possui 500 canais e ErrChannelNameTaken quando o nome já está em uso.
func CreateChannel(ctx context.Context, serverID, name, channelType string) (models.ChannelSummary, error) {
	if channelType == "" {
		channelType = "text"
	}
	if channelType != "text" && channelType != "category" {
		return models.ChannelSummary{}, ErrInvalidInput
	}
	if serverID == "" || name == "" || utf8.RuneCountInString(name) > maxChannelNameLength {
		return models.ChannelSummary{}, ErrInvalidInput
	}

	if _, err := storage.GetServerByID(ctx, serverID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return models.ChannelSummary{}, ErrServerNotFound
		}
		return models.ChannelSummary{}, err
	}

	count, err := storage.CountChannelsByServer(ctx, serverID)
	if err != nil {
		return models.ChannelSummary{}, err
	}
	if count >= maxChannelsPerServer {
		return models.ChannelSummary{}, ErrChannelLimitReached
	}

	channel, err := storage.CreateChannel(ctx, serverID, name, channelType)
	if errors.Is(err, storage.ErrUniqueViolation) {
		return models.ChannelSummary{}, ErrChannelNameTaken
	}
	if err != nil {
		return models.ChannelSummary{}, err
	}

	// Reconsulta a visão summary para que a resposta da criação tenha a
	// mesma forma da listagem (permissões expandidas e last_message).
	return storage.GetChannelSummary(ctx, channel.ID)
}

// UpdateChannel renomeia um canal pelo id (README: PUT /channels/:channel_id).
// Retorna ErrInvalidInput quando o nome está vazio ou acima de 32
// caracteres, ErrChannelNotFound quando o canal não existe e
// ErrChannelNameTaken quando o nome já está em uso.
func UpdateChannel(ctx context.Context, id, name string) (models.ChannelSummary, error) {
	if id == "" {
		return models.ChannelSummary{}, ErrChannelNotFound
	}
	if name == "" || utf8.RuneCountInString(name) > maxChannelNameLength {
		return models.ChannelSummary{}, ErrInvalidInput
	}

	if _, err := storage.GetChannelByID(ctx, id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return models.ChannelSummary{}, ErrChannelNotFound
		}
		return models.ChannelSummary{}, err
	}

	if _, err := storage.UpdateChannel(ctx, id, name); err != nil {
		if errors.Is(err, storage.ErrUniqueViolation) {
			return models.ChannelSummary{}, ErrChannelNameTaken
		}
		if errors.Is(err, storage.ErrNotFound) {
			return models.ChannelSummary{}, ErrChannelNotFound
		}
		return models.ChannelSummary{}, err
	}

	// Reconsulta a visão summary para que a resposta da edição tenha a
	// mesma forma da listagem (permissões expandidas e last_message).
	return storage.GetChannelSummary(ctx, id)
}

// ChangeChannelPosition move um canal para newPosition e recalcula as
// posições dos demais canais do servidor
// (README: PUT /channels/:channel_id/change_position).
// Retorna ErrInvalidInput quando as posições são inválidas,
// ErrChannelNotFound quando o canal não existe e
// ErrChannelPositionConflict quando o canal não está em old_position.
func ChangeChannelPosition(ctx context.Context, channelID string, oldPosition, newPosition int) (models.ChannelSummary, error) {
	if channelID == "" {
		return models.ChannelSummary{}, ErrChannelNotFound
	}
	if oldPosition < 1 || newPosition < 1 {
		return models.ChannelSummary{}, ErrInvalidInput
	}

	channel, err := storage.ChangeChannelPosition(ctx, channelID, oldPosition, newPosition)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return models.ChannelSummary{}, ErrChannelNotFound
	case errors.Is(err, storage.ErrPositionConflict):
		return models.ChannelSummary{}, ErrChannelPositionConflict
	case errors.Is(err, storage.ErrInvalidPosition):
		return models.ChannelSummary{}, ErrInvalidInput
	case err != nil:
		return models.ChannelSummary{}, err
	}

	// Reconsulta a visão summary para que a resposta tenha a mesma forma da
	// listagem (permissões expandidas e last_message).
	return storage.GetChannelSummary(ctx, channel.ID)
}

// DeleteChannel exclui um canal pelo id (README: 204 when successful).
// Retorna o server_id do canal excluído (para o evento WebSocket de
// exclusão) e ErrChannelNotFound quando o canal não existe.
func DeleteChannel(ctx context.Context, id string) (string, error) {
	if id == "" {
		return "", ErrChannelNotFound
	}

	channel, err := storage.GetChannelByID(ctx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return "", ErrChannelNotFound
	}
	if err != nil {
		return "", err
	}

	if err := storage.DeleteChannel(ctx, id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "", ErrChannelNotFound
		}
		return "", err
	}

	return channel.ServerID, nil
}

// GetChannelPermissions retorna as permissões de um canal expandidas por
// role, com o nome de cada role (README: GET /channels/:channel_id/permissions).
// Retorna ErrChannelNotFound quando o canal não existe.
func GetChannelPermissions(ctx context.Context, channelID string) ([]models.ChannelPermissionEntry, error) {
	if channelID == "" {
		return nil, ErrChannelNotFound
	}

	summary, err := storage.GetChannelSummary(ctx, channelID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, ErrChannelNotFound
	}
	if err != nil {
		return nil, err
	}

	return summary.Permissions, nil
}

// CanReadChannel verifica se o usuário pode ler o canal (permissão
// read_channel, mesma regra de ListMessages: o dono do servidor sempre pode
// e em canais sem roles definidas a leitura é livre).
// Retorna ErrChannelNotFound quando o canal não existe e
// ErrPermissionDenied quando o usuário não tem permissão.
func CanReadChannel(ctx context.Context, channelID, userID string) error {
	if channelID == "" {
		return ErrChannelNotFound
	}

	channel, err := storage.GetChannelByID(ctx, channelID)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrChannelNotFound
	}
	if err != nil {
		return err
	}

	allowed, err := userHasChannelPermission(ctx, channel, userID, true, func(p models.ChannelPermission) bool {
		return p.ReadChannel
	})
	if err != nil {
		return err
	}
	if !allowed {
		return ErrPermissionDenied
	}

	return nil
}

// ChannelReaders retorna quais dos usuários informados podem ler o canal
// (permissão read_channel, mesma regra de ListMessages: o dono do servidor
// sempre pode ler e em canais sem roles definidas a leitura é livre para
// todos). Usado para autorizar quem escuta os eventos de canal via
// WebSocket.
// Retorna ErrChannelNotFound quando o canal não existe.
func ChannelReaders(ctx context.Context, channelID string, userIDs []string) (map[string]bool, error) {
	allowed := make(map[string]bool, len(userIDs))
	if len(userIDs) == 0 {
		return allowed, nil
	}
	if channelID == "" {
		return allowed, ErrChannelNotFound
	}

	channel, err := storage.GetChannelByID(ctx, channelID)
	if errors.Is(err, storage.ErrNotFound) {
		return allowed, ErrChannelNotFound
	}
	if err != nil {
		return allowed, err
	}

	// Canal aberto (sem roles definidas): leitura livre para todos.
	if len(channel.Permissions) == 0 {
		for _, userID := range userIDs {
			allowed[userID] = true
		}
		return allowed, nil
	}

	server, err := storage.GetServerByID(ctx, channel.ServerID)
	if err != nil {
		return allowed, err
	}
	if server.OwnerID != nil {
		for _, userID := range userIDs {
			if userID == *server.OwnerID {
				allowed[userID] = true
			}
		}
	}

	readRoleIDs := make([]string, 0, len(channel.Permissions))
	for roleID, permission := range channel.Permissions {
		if permission.ReadChannel {
			readRoleIDs = append(readRoleIDs, roleID)
		}
	}
	if len(readRoleIDs) == 0 {
		return allowed, nil
	}

	users, err := storage.GetUsersByRoles(ctx, readRoleIDs)
	if err != nil {
		return allowed, err
	}
	for _, userID := range users {
		allowed[userID] = true
	}

	return allowed, nil
}

// UpdateChannelPermissions define as permissões de uma role em um canal
// (README: PUT /channels/:channel_id/permissions/:role_id) e retorna as
// permissões resultantes.
// Retorna ErrChannelNotFound quando o canal não existe, ErrRoleNotFound quando
// a role não existe ou pertence a outro servidor.
func UpdateChannelPermissions(ctx context.Context, channelID, roleID string, permission models.ChannelPermission) (models.ChannelPermission, error) {
	if channelID == "" || roleID == "" {
		if channelID == "" {
			return models.ChannelPermission{}, ErrChannelNotFound
		}
		return models.ChannelPermission{}, ErrRoleNotFound
	}

	channel, err := storage.GetChannelByID(ctx, channelID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.ChannelPermission{}, ErrChannelNotFound
	}
	if err != nil {
		return models.ChannelPermission{}, err
	}

	role, err := storage.GetRoleByID(ctx, roleID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.ChannelPermission{}, ErrRoleNotFound
	}
	if err != nil {
		return models.ChannelPermission{}, err
	}

	if role.ServerID != channel.ServerID {
		return models.ChannelPermission{}, ErrRoleNotFound
	}

	if _, err := storage.UpdateChannelPermissions(ctx, channelID, roleID, permission); err != nil {
		return models.ChannelPermission{}, err
	}

	return permission, nil
}
