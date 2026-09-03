package services

import (
	"context"
	"time"

	"papo/internal/middleware"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"
)

// auditLogPageSize é o número máximo de logs por página de GET /admin/audit-logs.
const auditLogPageSize = 100

// Ações auditáveis (valor da coluna action).
const (
	ActionAuthConnectionDrop       = "auth.connection_drop"
	ActionUserRegister             = "user.register"
	ActionUserUpdateSettings       = "user.update_settings"
	ActionUserUpdateProfile        = "user.update_profile"
	ActionUserUpdateStatus         = "user.update_status"
	ActionUserUpdateAvatar         = "user.update_avatar"
	ActionUserUpdateBanner         = "user.update_banner"
	ActionUserChangePassword       = "user.change_password"
	ActionUserBan                  = "user.ban"
	ActionUserUnban                = "user.unban"
	ActionUserResetPassword        = "user.reset_password"
	ActionServerCreate             = "server.create"
	ActionServerUpdate             = "server.update"
	ActionChannelCreate            = "channel.create"
	ActionChannelUpdate            = "channel.update"
	ActionChannelMovePosition      = "channel.move_position"
	ActionChannelDelete            = "channel.delete"
	ActionChannelPermUpdate        = "channel.permissions_update"
	ActionMessageCreate            = "message.create"
	ActionMediaUpload              = "media.upload"
	ActionMessageEdit              = "message.edit"
	ActionMessageDelete            = "message.delete"
	ActionMessagePin               = "message.pin"
	ActionMessageUnpin             = "message.unpin"
	ActionMessageModerationBlocked = "message.moderation_blocked"
	ActionRoleCreate               = "role.create"
	ActionRoleUpdate               = "role.update"
	ActionRoleDelete               = "role.delete"
	ActionUserRoleAssign           = "user_role.assign"
	ActionUserRoleRemove           = "user_role.remove"
	ActionEmojiCreate              = "emoji.create"
	ActionEmojiDelete              = "emoji.delete"
)

// Tipos de entidade auditáveis (valor da coluna entity_type).
const (
	EntityUser       = "user"
	EntityServer     = "server"
	EntityChannel    = "channel"
	EntityMessage    = "message"
	EntityAttachment = "attachment"
	EntityRole       = "role"
	EntityUserRole   = "user_role"
	EntityEmoji      = "emoji"
)

// AuditEntry descreve um evento de auditoria a registrar.
type AuditEntry struct {
	ActorID      string
	Action       string
	EntityType   string
	EntityID     *string
	TargetUserID *string
	Metadata     map[string]any
}

// RecordAudit registra um evento de auditoria de forma best-effort. O INSERT
// acontece após a operação principal ter sucesso; falhas são logadas, mas não
// propagam para a requisição (a auditoria não pode quebrar a operação).
// O actor_username é um snapshot do username do ator no momento do evento.
// ActorID vazio é uma ação de sistema (ex.: moderação de imagens): actor_id
// fica NULL e actor_username vira "sistema".
func RecordAudit(ctx context.Context, e AuditEntry) {
	var actorID *string
	username := "sistema"
	if e.ActorID != "" {
		actorID = &e.ActorID
		var err error
		username, err = storage.GetUsernameByID(ctx, e.ActorID)
		if err != nil {
			utils.Warnf("auditoria: falha ao resolver actor_username (actor_id=%s): %v", e.ActorID, err)
			username = "desconhecido"
		}
	}

	metadata := e.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}

	log := models.AuditLog{
		ActorID:       actorID,
		ActorUsername: username,
		Action:        e.Action,
		EntityType:    e.EntityType,
		EntityID:      e.EntityID,
		TargetUserID:  e.TargetUserID,
		Metadata:      metadata,
		IPAddress:     middleware.AuditIP(ctx),
		UserAgent:     middleware.AuditUserAgent(ctx),
	}

	if err := storage.InsertAuditLog(ctx, log); err != nil {
		utils.Warnf("auditoria: falha ao inserir log (action=%s, actor_id=%s): %v", e.Action, e.ActorID, err)
	}
}

// ListAuditLogs lista os logs de auditoria (GET /admin/audit-logs) com filtros
// (action, actor_id, entity_type, since, until) e paginação cursor-based
// (last_id), 100 por página. A autorização (manage_server) é feita no
// middleware da rota; este serviço apenas aplica os filtros e monta a página.
func ListAuditLogs(ctx context.Context, action, actorID, entityType string, since, until *time.Time, lastID string) (models.AuditLogList, error) {
	logs, err := storage.ListAuditLogs(ctx, storage.AuditLogParams{
		Action:     action,
		ActorID:    actorID,
		EntityType: entityType,
		Since:      since,
		Until:      until,
		LastID:     lastID,
		Limit:      auditLogPageSize,
	})
	if err != nil {
		return models.AuditLogList{}, err
	}

	hasMore := len(logs) > auditLogPageSize
	if hasMore {
		logs = logs[:auditLogPageSize]
	}

	entries := make([]models.AuditLogEntry, 0, len(logs))
	for _, log := range logs {
		entries = append(entries, models.AuditLogEntry{
			ID:            log.ID,
			ActorUsername: log.ActorUsername,
			Action:        log.Action,
			EntityType:    log.EntityType,
			TargetUserID:  log.TargetUserID,
			Metadata:      log.Metadata,
			CreatedAt:     log.CreatedAt,
		})
	}

	return models.AuditLogList{Logs: entries, HasMore: hasMore}, nil
}
