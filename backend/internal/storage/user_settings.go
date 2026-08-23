package storage

import (
	"context"
	"encoding/json"
	"fmt"

	"papo/internal/models"
)

const userSettingsColumns = "user_id, version, config, updated_at"

func scanUserSettings(row rowScanner) (models.UserSettings, error) {
	var settings models.UserSettings
	var config []byte
	err := row.Scan(
		&settings.UserID,
		&settings.Version,
		&config,
		&settings.UpdatedAt,
	)
	if err != nil {
		return models.UserSettings{}, err
	}

	settings.Config = models.UserConfig{}
	if len(config) > 0 {
		if err := json.Unmarshal(config, &settings.Config); err != nil {
			return models.UserSettings{}, fmt.Errorf("falha ao decodificar configurações do usuário: %w", err)
		}
	}

	return settings, nil
}

// GetUserSettings busca as configurações de um usuário pelo id.
func GetUserSettings(ctx context.Context, userID string) (models.UserSettings, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+userSettingsColumns+" FROM user_settings WHERE user_id = $1",
		userID,
	)

	settings, err := scanUserSettings(row)
	if err != nil {
		return models.UserSettings{}, mapStorageError(err)
	}

	return settings, nil
}

// UpsertUserSettings cria ou atualiza as configurações de um usuário.
// A version representa a versão do modelo de configurações (models.CurrentVersion) e
// só muda quando o shape do UserConfig é alterado. Na atualização, define
// updated_at com o tempo do banco. Retorna o registro resultante.
func UpsertUserSettings(ctx context.Context, userID string, config models.UserConfig) (models.UserSettings, error) {
	configJSON, err := json.Marshal(config)
	if err != nil {
		return models.UserSettings{}, fmt.Errorf("falha ao codificar configurações do usuário: %w", err)
	}

	row := GetDB().QueryRowContext(ctx,
		`INSERT INTO user_settings (user_id, config, version)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (user_id) DO UPDATE
		 SET config = EXCLUDED.config, version = $3, updated_at = NOW()
		 RETURNING `+userSettingsColumns,
		userID, string(configJSON), models.CurrentVersion,
	)

	settings, err := scanUserSettings(row)
	if err != nil {
		return models.UserSettings{}, mapStorageError(err)
	}

	return settings, nil
}
