package store

import (
	"context"
	"fmt"
)

// ListEnabledPushDevices lista os dispositivos ativos (enabled = TRUE) do
// usuário, em ordem de criação. O worker usa esta lista como alvos de entrega.
func (s *Store) ListEnabledPushDevices(ctx context.Context, userID string) ([]PushDevice, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+pushDevicesColumnsNoID+" "+
			"FROM push_devices "+
			"WHERE user_id = $1 AND enabled = TRUE "+
			"ORDER BY created_at ASC, id ASC",
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar os dispositivos do usuário: %w", err)
	}
	defer rows.Close()

	devices := make([]PushDevice, 0)
	for rows.Next() {
		var device PushDevice
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
// independentemente do usuário (chamado quando o FCM/relay reporta token
// inválido). Retorna o número de linhas afetadas.
func (s *Store) DisablePushDeviceByToken(ctx context.Context, token string) (int64, error) {
	result, err := s.db.ExecContext(ctx,
		"UPDATE push_devices SET enabled = FALSE, updated_at = NOW() WHERE token = $1",
		token,
	)
	if err != nil {
		return 0, fmt.Errorf("falha ao desativar o dispositivo: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("falha ao contar linhas afetadas: %w", err)
	}
	return affected, nil
}
