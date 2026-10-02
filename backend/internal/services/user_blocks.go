package services

import (
	"context"
	"errors"

	"papo/internal/models"
	"papo/internal/storage"

	"github.com/google/uuid"
)

type UserBlockList struct {
	Users []models.UserSummary `json:"users"`
}

func ListUserBlocks(ctx context.Context, userID string) (UserBlockList, error) {
	users, err := storage.ListBlockedUsers(ctx, userID)
	if err != nil {
		return UserBlockList{}, err
	}
	ids := make([]string, 0, len(users))
	for _, user := range users {
		ids = append(ids, user.ID)
	}
	rolesByUser, err := storage.GetRoleSummariesByUsers(ctx, ids)
	if err != nil {
		return UserBlockList{}, err
	}
	for i := range users {
		users[i].Roles = rolesByUser[users[i].ID]
		if users[i].Roles == nil {
			users[i].Roles = []models.RoleSummary{}
		}
	}
	return UserBlockList{Users: users}, nil
}

func BlockUser(ctx context.Context, userID, targetID string) error {
	parsedTarget, err := uuid.Parse(targetID)
	if err != nil {
		return ErrInvalidInput
	}
	targetID = parsedTarget.String()
	if targetID == userID {
		return ErrInvalidInput
	}
	if _, err := storage.GetUserByID(ctx, targetID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrUserNotFound
		}
		return err
	}
	return storage.CreateUserBlock(ctx, userID, targetID)
}

// UnblockUser é idempotente: desbloquear um usuário que já não está
// bloqueado retorna sucesso.
func UnblockUser(ctx context.Context, userID, targetID string) error {
	parsedTarget, err := uuid.Parse(targetID)
	if err != nil {
		return ErrInvalidInput
	}
	targetID = parsedTarget.String()
	if targetID == userID {
		return ErrInvalidInput
	}
	return storage.DeleteUserBlock(ctx, userID, targetID)
}
