package storage

import (
	"context"
	"fmt"

	"papo/internal/models"
)

const pushDeviceColumns = "id, user_id, token, platform, provider, device_name, enabled, created_at, updated_at, last_seen_at"

const pushDeviceColumnsNoID = "user_id, token, platform, provider, device_name, enabled, created_at, updated_at, last_seen_at"

func scanPushDevice(row rowScanner) (models.PushDevice, error) {
	var device models.PushDevice
	err := row.Scan(
		&device.ID,
		&device.UserID,
		&device.Token,
		&device.Platform,
		&device.Provider,
		&device.DeviceName,
		&device.Enabled,
		&device.CreatedAt,
		&device.UpdatedAt,
		&device.LastSeenAt,
	)
	if err != nil {
		return models.PushDevice{}, err
	}

	return device, nil
}

// UpsertPushDevice registra o dispositivo (token) do usuário, substituindo o
// registro anterior quando o mesmo token já existe (o token é a chave de
// identidade do dispositivo; user_id nunca vem do cliente, é o usuário
// autenticado). updated_at e last_seen_at ficam com o tempo do banco.
func UpsertPushDevice(ctx context.Context, userID, token, platform, provider, deviceName string) (models.PushDevice, error) {
	row := GetDB().QueryRowContext(ctx,
		`INSERT INTO push_devices (user_id, token, platform, provider, device_name)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (token) DO UPDATE
		 SET user_id = EXCLUDED.user_id,
				platform = EXCLUDED.platform,
				provider = EXCLUDED.provider,
				device_name = EXCLUDED.device_name,
				updated_at = NOW(),
				last_seen_at = NOW()
		 RETURNING `+pushDeviceColumns,
		userID, token, platform, provider, deviceName,
	)

	device, err := scanPushDevice(row)
	if err != nil {
		return models.PushDevice{}, mapStorageError(err)
	}

	return device, nil
}

// DeletePushDevice remove o dispositivo do usuário por token. Retorna o
// número de linhas afetadas (0 quando o token não existe).
func DeletePushDevice(ctx context.Context, userID, token string) (int64, error) {
	result, err := GetDB().ExecContext(ctx,
		"DELETE FROM push_devices WHERE user_id = $1 AND token = $2",
		userID, token,
	)
	if err != nil {
		return 0, fmt.Errorf("falha ao remover o dispositivo: %w", err)
	}

	return result.RowsAffected()
}

// ListEnabledPushDevices lista os dispositivos ativos (enabled = TRUE) do
// usuário, em ordem de criação.
func ListEnabledPushDevices(ctx context.Context, userID string) ([]models.PushDevice, error) {
	rows, err := GetDB().QueryContext(ctx,
		`SELECT `+pushDeviceColumnsNoID+`
		 FROM push_devices
		 WHERE user_id = $1 AND enabled = TRUE
		 ORDER BY created_at ASC, id ASC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar os dispositivos do usuário: %w", err)
	}
	defer rows.Close()

	devices := make([]models.PushDevice, 0)
	for rows.Next() {
		var device models.PushDevice
		if err := rows.Scan(
			&device.UserID,
			&device.Token,
			&device.Platform,
			&device.Provider,
			&device.DeviceName,
			&device.Enabled,
			&device.CreatedAt,
			&device.UpdatedAt,
			&device.LastSeenAt,
		); err != nil {
			return nil, fmt.Errorf("falha ao ler dispositivo: %w", err)
		}
		devices = append(devices, device)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar os dispositivos do usuário: %w", err)
	}

	return devices, nil
}

// DisablePushDeviceByToken desativa o dispositivo com o token informado,
// independentemente do usuário (chamado pelo papo-push ao detectar token
// inválido). Retorna o número de linhas afetadas.
func DisablePushDeviceByToken(ctx context.Context, token string) (int64, error) {
	result, err := GetDB().ExecContext(ctx,
		"UPDATE push_devices SET enabled = FALSE, updated_at = NOW() WHERE token = $1",
		token,
	)
	if err != nil {
		return 0, fmt.Errorf("falha ao desativar o dispositivo: %w", err)
	}

	return result.RowsAffected()
}
