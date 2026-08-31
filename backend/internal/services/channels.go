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

// ErrChannelLimitReached indica que o limite de canais foi atingido.
var ErrChannelLimitReached = errors.New("limite de canais atingido")

// ErrChannelPositionConflict indica que a posição atual do canal não
// corresponde à old_position informada (a ordem mudou após a leitura).
var ErrChannelPositionConflict = errors.New("posição do canal desatualizada")

// maxChannelNameLength é o tamanho máximo do nome de um canal (32 caracteres, README).
const maxChannelNameLength = 32

// maxChannels é o número máximo de canais (500, README).
const maxChannels = 500

// ListChannels lista os canais com permissões expandidas e a última mensagem
// de cada um (README).
func ListChannels(ctx context.Context) ([]models.ChannelSummary, error) {
	return storage.ListChannelSummaries(ctx)
}

// CreateChannel cria um novo canal
// (README: o body de criação tem name e type; type é opcional e padrão
// "text", aceita "text" ou "category").
// Retorna ErrInvalidInput quando o nome está vazio ou acima de 32
// caracteres, quando o type é inválido, ErrChannelLimitReached quando o
// limite de 500 canais já foi atingido e ErrChannelNameTaken quando o nome
// já está em uso.
func CreateChannel(ctx context.Context, name, channelType string) (models.ChannelSummary, error) {
	if channelType == "" {
		channelType = "text"
	}
	if channelType != "text" && channelType != "category" {
		return models.ChannelSummary{}, ErrInvalidInput
	}
	if name == "" || utf8.RuneCountInString(name) > maxChannelNameLength {
		return models.ChannelSummary{}, ErrInvalidInput
	}

	count, err := storage.CountChannels(ctx)
	if err != nil {
		return models.ChannelSummary{}, err
	}
	if count >= maxChannels {
		return models.ChannelSummary{}, ErrChannelLimitReached
	}

	channel, err := storage.CreateChannel(ctx, name, channelType)
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
// Retorna ErrChannelNotFound quando o canal não existe.
func DeleteChannel(ctx context.Context, id string) error {
	if id == "" {
		return ErrChannelNotFound
	}

	if _, err := storage.GetChannelByID(ctx, id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrChannelNotFound
		}
		return err
	}

	if err := storage.DeleteChannel(ctx, id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrChannelNotFound
		}
		return err
	}

	return nil
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

	server, err := storage.GetServer(ctx)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
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
// Retorna ErrChannelNotFound quando o canal não existe e ErrRoleNotFound
// quando a role não existe.
func UpdateChannelPermissions(ctx context.Context, channelID, roleID string, permission models.ChannelPermission) (models.ChannelPermission, error) {
	if channelID == "" || roleID == "" {
		if channelID == "" {
			return models.ChannelPermission{}, ErrChannelNotFound
		}
		return models.ChannelPermission{}, ErrRoleNotFound
	}

	if _, err := storage.GetChannelByID(ctx, channelID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return models.ChannelPermission{}, ErrChannelNotFound
		}
		return models.ChannelPermission{}, err
	}

	if _, err := storage.GetRoleByID(ctx, roleID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return models.ChannelPermission{}, ErrRoleNotFound
		}
		return models.ChannelPermission{}, err
	}

	if _, err := storage.UpdateChannelPermissions(ctx, channelID, roleID, permission); err != nil {
		return models.ChannelPermission{}, err
	}

	return permission, nil
}
