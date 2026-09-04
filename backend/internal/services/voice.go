package services

import (
	"context"
	"errors"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"
)

// ErrInvalidChannelType indica que o canal existe mas não é um canal de voz.
var ErrInvalidChannelType = errors.New("canal não é de voz")

// CanConnectVoice verifica se o usuário pode entrar em calls do canal
// (permissão connect_voice, mesma regra de CanReadChannel: o dono do
// servidor sempre pode e em canal aberto a entrada é livre).
// Retorna ErrChannelNotFound quando o canal não existe,
// ErrInvalidChannelType quando o canal existe mas type != "voice" e
// ErrPermissionDenied quando o usuário não tem permissão.
func CanConnectVoice(ctx context.Context, channelID, userID string) error {
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
	if channel.Type != "voice" {
		return ErrInvalidChannelType
	}

	allowed, err := userHasChannelPermission(ctx, channel, userID, true, func(p models.ChannelPermission) bool {
		return p.ConnectVoice
	})
	if err != nil {
		return err
	}
	if !allowed {
		return ErrPermissionDenied
	}

	return nil
}

// VoiceConnectors retorna quais dos usuários informados podem entrar em
// calls do canal (permissão connect_voice, mesma regra de CanConnectVoice).
// Usado para autorizar quem escuta os eventos de presença de voz via
// WebSocket (audiência inclui quem está fora da call).
// Retorna ErrChannelNotFound quando o canal não existe e
// ErrInvalidChannelType quando o canal existe mas type != "voice".
func VoiceConnectors(ctx context.Context, channelID string, userIDs []string) (map[string]bool, error) {
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
	if channel.Type != "voice" {
		return allowed, ErrInvalidChannelType
	}

	// Canal aberto (sem roles definidas): entrada livre para todos.
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

	voiceRoleIDs := make([]string, 0, len(channel.Permissions))
	for roleID, permission := range channel.Permissions {
		if permission.ConnectVoice {
			voiceRoleIDs = append(voiceRoleIDs, roleID)
		}
	}
	if len(voiceRoleIDs) == 0 {
		return allowed, nil
	}

	users, err := storage.GetUsersByRoles(ctx, voiceRoleIDs)
	if err != nil {
		return allowed, err
	}
	for _, userID := range users {
		allowed[userID] = true
	}

	return allowed, nil
}

// ICEConfigForUser monta a lista de servidores ICE (STUN + TURN) do
// usuário autenticado. TURN recebe credencial efêmera RFC 5389 por usuário
// (username "<ttl>:<user_id>"); STUN não tem credencial.
func ICEConfigForUser(ctx context.Context, userID string) ([]models.ICEServer, error) {
	cfg := config.LoadConfig()

	servers := make([]models.ICEServer, 0, 2)
	if len(cfg.STUNURLs) > 0 {
		servers = append(servers, models.ICEServer{URLs: cfg.STUNURLs})
	}
	if len(cfg.TURNURLs) > 0 {
		username, credential := utils.GenerateTurnCredential([]byte(cfg.TURNSecret), userID, cfg.TURNTTL)
		servers = append(servers, models.ICEServer{
			URLs:       cfg.TURNURLs,
			Username:   username,
			Credential: credential,
		})
	}

	return servers, nil
}
