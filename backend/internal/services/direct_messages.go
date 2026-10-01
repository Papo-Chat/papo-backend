package services

import (
	"context"
	"errors"

	"papo/internal/models"
	"papo/internal/storage"

	"github.com/google/uuid"
)

var ErrDirectMessageNotFound = errors.New("DM não encontrada")
var ErrDirectMessageBlocked = errors.New("DM bloqueada")

func enrichDirectConversationUsers(ctx context.Context, dms []models.DirectConversation) error {
	ids := make([]string, 0, len(dms))
	for _, dm := range dms {
		ids = append(ids, dm.User.ID)
	}
	rolesByUser, err := storage.GetRoleSummariesByUsers(ctx, ids)
	if err != nil {
		return err
	}
	for i := range dms {
		dms[i].User.Roles = rolesByUser[dms[i].User.ID]
		if dms[i].User.Roles == nil {
			dms[i].User.Roles = []models.RoleSummary{}
		}
	}
	return nil
}

func ListDirectConversations(ctx context.Context, userID string) (models.DirectConversationList, error) {
	dms, err := storage.ListDirectConversations(ctx, userID)
	if err != nil {
		return models.DirectConversationList{}, err
	}
	if err := enrichDirectConversationUsers(ctx, dms); err != nil {
		return models.DirectConversationList{}, err
	}
	return models.DirectConversationList{DMs: dms}, nil
}

func GetDirectConversation(ctx context.Context, userID, channelID string) (models.DirectConversation, error) {
	if _, err := uuid.Parse(channelID); err != nil {
		return models.DirectConversation{}, ErrDirectMessageNotFound
	}
	member, blocked, err := storage.DirectConversationAccess(ctx, channelID, userID)
	if errors.Is(err, storage.ErrNotFound) || !member {
		return models.DirectConversation{}, ErrDirectMessageNotFound
	}
	if err != nil {
		return models.DirectConversation{}, err
	}
	if blocked {
		return models.DirectConversation{}, ErrDirectMessageBlocked
	}

	dm, err := storage.GetDirectConversation(ctx, channelID, userID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.DirectConversation{}, ErrDirectMessageNotFound
	}
	if err != nil {
		return models.DirectConversation{}, err
	}
	items := []models.DirectConversation{dm}
	if err := enrichDirectConversationUsers(ctx, items); err != nil {
		return models.DirectConversation{}, err
	}
	return items[0], nil
}

func OpenDirectConversation(ctx context.Context, userID, targetID string) (models.DirectConversation, bool, error) {
	if _, err := uuid.Parse(targetID); err != nil || targetID == userID {
		return models.DirectConversation{}, false, ErrInvalidInput
	}
	target, err := storage.GetUserByID(ctx, targetID)
	if errors.Is(err, storage.ErrNotFound) || (err == nil && target.Banned) {
		return models.DirectConversation{}, false, ErrUserNotFound
	}
	if err != nil {
		return models.DirectConversation{}, false, err
	}

	blocked, err := storage.UsersBlocked(ctx, userID, targetID)
	if err != nil {
		return models.DirectConversation{}, false, err
	}
	if blocked {
		return models.DirectConversation{}, false, ErrDirectMessageBlocked
	}

	channelID, created, err := storage.CreateOrShowDirectConversation(ctx, userID, targetID)
	if err != nil {
		return models.DirectConversation{}, false, err
	}
	dm, err := GetDirectConversation(ctx, userID, channelID)
	return dm, created, err
}

func HideDirectConversation(ctx context.Context, userID, channelID string) error {
	if _, err := uuid.Parse(channelID); err != nil {
		return ErrDirectMessageNotFound
	}
	member, _, err := storage.DirectConversationAccess(ctx, channelID, userID)
	if errors.Is(err, storage.ErrNotFound) || !member {
		return ErrDirectMessageNotFound
	}
	if err != nil {
		return err
	}
	if err := storage.HideDirectConversation(ctx, channelID, userID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrDirectMessageNotFound
		}
		return err
	}
	return nil
}

func DirectConversationMembers(ctx context.Context, channelID string) ([]string, error) {
	members, err := storage.GetDirectConversationMembers(ctx, channelID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, ErrDirectMessageNotFound
	}
	return members, err
}
