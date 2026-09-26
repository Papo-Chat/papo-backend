package services

import (
	"context"
	"errors"
	"strings"

	"papo/internal/models"
	"papo/internal/storage"
)

const (
	maxPushTokenLength   = 1024
	maxPushDeviceNameLen = 32
)

var (
	ErrPushTokenInvalid      = errors.New("token de push inválido")
	ErrPushPlatformInvalid   = errors.New("plataforma inválida")
	ErrPushDeviceNameInvalid = errors.New("nome do dispositivo inválido")
	ErrPushDeviceNotFound    = errors.New("dispositivo não encontrado")
)

// RegisterPushDevice registra (upsert) o dispositivo do usuário para push.
// userID é o usuário autenticado (nunca vem do cliente).
func RegisterPushDevice(ctx context.Context, userID, token, platform, provider, deviceName string) (models.PushDevice, error) {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > maxPushTokenLength {
		return models.PushDevice{}, ErrPushTokenInvalid
	}
	switch platform {
	case "android", "ios":
		// ok
	default:
		return models.PushDevice{}, ErrPushPlatformInvalid
	}
	deviceName = strings.TrimSpace(deviceName)
	if len(deviceName) > maxPushDeviceNameLen {
		return models.PushDevice{}, ErrPushDeviceNameInvalid
	}
	if provider == "" {
		provider = "fcm"
	}

	return storage.UpsertPushDevice(ctx, userID, token, platform, provider, deviceName)
}

// RemovePushDevice remove o dispositivo do usuário por token.
// Retorna ErrPushDeviceNotFound quando o token não existe para o usuário.
func RemovePushDevice(ctx context.Context, userID, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return ErrPushTokenInvalid
	}

	affected, err := storage.DeletePushDevice(ctx, userID, token)
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrPushDeviceNotFound
	}

	return nil
}

// ListEnabledPushDevices lista os dispositivos ativos do usuário.
func ListEnabledPushDevices(ctx context.Context, userID string) ([]models.PushDevice, error) {
	return storage.ListEnabledPushDevices(ctx, userID)
}

// DisablePushDeviceByToken desativa o dispositivo pelo token (chamado pelo
// papo-push ao detectar token inválido no FCM/relay).
func DisablePushDeviceByToken(ctx context.Context, token string) error {
	_, err := storage.DisablePushDeviceByToken(ctx, token)
	return err
}
