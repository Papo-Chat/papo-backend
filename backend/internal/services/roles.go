package services

import (
	"context"
	"errors"
	"regexp"
	"unicode/utf8"

	"papo/internal/models"
	"papo/internal/storage"
)

// ErrRoleNameTaken indica que o nome da role já existe.
var ErrRoleNameTaken = errors.New("nome da role já existe")

// ErrUserRoleNotFound indica que o usuário não possui a role atribuída.
var ErrUserRoleNotFound = errors.New("role não atribuída ao usuário")

// maxRoleNameLength é o tamanho máximo do nome de uma role (32 caracteres, README).
const maxRoleNameLength = 32

// hexColorPattern valida cores no formato hexadecimal #RRGGBB (ex.: #FF0000).
var hexColorPattern = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// ListRoles lista as roles (README: GET /roles).
func ListRoles(ctx context.Context) ([]models.Role, error) {
	return storage.ListRoles(ctx)
}

// CreateRole cria uma role (README: POST /roles).
// Retorna ErrInvalidInput quando o nome está vazio ou acima de 32 caracteres ou
// quando a cor não é um hexadecimal #RRGGBB e ErrRoleNameTaken quando o nome
// já está em uso.
func CreateRole(ctx context.Context, actorID, name string, color *string, permissions models.RolePermissions) (models.Role, error) {
	if name == "" || utf8.RuneCountInString(name) > maxRoleNameLength {
		return models.Role{}, ErrInvalidInput
	}

	if color != nil && !hexColorPattern.MatchString(*color) {
		return models.Role{}, ErrInvalidInput
	}

	role, err := storage.CreateRole(ctx, name, color, permissions)
	if errors.Is(err, storage.ErrUniqueViolation) {
		return models.Role{}, ErrRoleNameTaken
	}
	if err != nil {
		return models.Role{}, err
	}

	RecordAudit(ctx, AuditEntry{
		ActorID:    actorID,
		Action:     ActionRoleCreate,
		EntityType: EntityRole,
		EntityID:   &role.ID,
		Metadata:   map[string]any{"name": name},
	})

	return role, nil
}

// UpdateRole atualiza o nome, a cor e as permissões de uma role
// (README: PUT /roles/:role_id).
// Retorna ErrRoleNotFound quando a role não existe, ErrInvalidInput quando o
// nome está vazio ou acima de 32 caracteres ou quando a cor não é um
// hexadecimal #RRGGBB e ErrRoleNameTaken quando o nome já está em uso.
func UpdateRole(ctx context.Context, actorID, roleID, name string, color *string, permissions models.RolePermissions) (models.Role, error) {
	if roleID == "" {
		return models.Role{}, ErrRoleNotFound
	}

	if name == "" || utf8.RuneCountInString(name) > maxRoleNameLength {
		return models.Role{}, ErrInvalidInput
	}

	if color != nil && !hexColorPattern.MatchString(*color) {
		return models.Role{}, ErrInvalidInput
	}

	updated, err := storage.UpdateRole(ctx, roleID, models.Role{
		Name:        name,
		Color:       color,
		Permissions: permissions,
	})
	if errors.Is(err, storage.ErrUniqueViolation) {
		return models.Role{}, ErrRoleNameTaken
	}
	if errors.Is(err, storage.ErrNotFound) {
		return models.Role{}, ErrRoleNotFound
	}
	if err != nil {
		return models.Role{}, err
	}

	RecordAudit(ctx, AuditEntry{
		ActorID:    actorID,
		Action:     ActionRoleUpdate,
		EntityType: EntityRole,
		EntityID:   &roleID,
		Metadata:   map[string]any{"name": name},
	})

	return updated, nil
}

// DeleteRole exclui uma role (README: DELETE /roles/:role_id). O storage remove
// a role, as atribuições dos usuários (ON DELETE CASCADE) e as entradas das
// permissões dos canais, de forma atômica.
// Retorna ErrRoleNotFound quando a role não existe.
func DeleteRole(ctx context.Context, actorID, roleID string) error {
	if roleID == "" {
		return ErrRoleNotFound
	}

	if err := storage.DeleteRole(ctx, roleID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrRoleNotFound
		}
		return err
	}

	RecordAudit(ctx, AuditEntry{
		ActorID:    actorID,
		Action:     ActionRoleDelete,
		EntityType: EntityRole,
		EntityID:   &roleID,
	})

	return nil
}

// AssignUserRole atribui uma role a um usuário (README: POST /users/:user_id/roles).
// Atribuir uma role já atribuída é idempotente: retorna a atribuição existente.
// Retorna ErrUserNotFound quando o usuário não existe e ErrRoleNotFound quando
// a role não existe.
func AssignUserRole(ctx context.Context, actorID, userID, roleID string) (models.UserRole, error) {
	if userID == "" {
		return models.UserRole{}, ErrUserNotFound
	}
	if roleID == "" {
		return models.UserRole{}, ErrRoleNotFound
	}

	if _, err := storage.GetUserByID(ctx, userID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return models.UserRole{}, ErrUserNotFound
		}
		return models.UserRole{}, err
	}

	if _, err := storage.GetRoleByID(ctx, roleID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return models.UserRole{}, ErrRoleNotFound
		}
		return models.UserRole{}, err
	}

	userRole, err := storage.AssignUserRole(ctx, userID, roleID)
	if errors.Is(err, storage.ErrUniqueViolation) {
		// Atribuição já existente (idempotente): retorna a atribuição atual.
		userRole, err = storage.GetUserRole(ctx, userID, roleID)
		if err != nil {
			return models.UserRole{}, err
		}
	}
	if err != nil {
		return models.UserRole{}, err
	}

	RecordAudit(ctx, AuditEntry{
		ActorID:      actorID,
		Action:       ActionUserRoleAssign,
		EntityType:   EntityUserRole,
		TargetUserID: &userID,
		Metadata:     map[string]any{"role_id": roleID},
	})

	return userRole, nil
}

// RemoveUserRole remove a atribuição de uma role de um usuário
// (README: DELETE /users/:user_id/roles/:role_id).
// Retorna ErrUserNotFound quando o usuário não existe, ErrRoleNotFound quando a
// role não existe e ErrUserRoleNotFound quando o usuário não possui a role.
func RemoveUserRole(ctx context.Context, actorID, userID, roleID string) error {
	if userID == "" {
		return ErrUserNotFound
	}
	if roleID == "" {
		return ErrRoleNotFound
	}

	if _, err := storage.GetUserByID(ctx, userID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrUserNotFound
		}
		return err
	}

	if _, err := storage.GetRoleByID(ctx, roleID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrRoleNotFound
		}
		return err
	}

	if err := storage.RemoveUserRole(ctx, userID, roleID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrUserRoleNotFound
		}
		return err
	}

	RecordAudit(ctx, AuditEntry{
		ActorID:      actorID,
		Action:       ActionUserRoleRemove,
		EntityType:   EntityUserRole,
		TargetUserID: &userID,
		Metadata:     map[string]any{"role_id": roleID},
	})

	return nil
}
