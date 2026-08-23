package models

import "time"

// Esse valor se refere a versão da estrutura da configuração dos usuários
// Ele só é atualizado quando mudamos algo nas possibilidades de configuração
const CurrentVersion = 1

// UserSettings representa a tabela user_settings.
type UserSettings struct {
	UserID    string     `db:"user_id" json:"user_id"`
	Version   int        `db:"version" json:"version"`
	Config    UserConfig `db:"config" json:"config"`
	UpdatedAt time.Time  `db:"updated_at" json:"updated_at"`
}

// UserConfig define a estrutura do JSONB config de user_settings (user_config).
type UserConfig struct {
	Theme         string        `json:"theme"`
	Notifications Notifications `json:"notifications"`
	Display       Display       `json:"display"`
}

// Notifications define as configurações de notificação do usuário.
type Notifications struct {
	Enabled        bool `json:"enabled"`
	MessagePreview bool `json:"messagePreview"`
	Sound          bool `json:"sound"`
	Mentions       bool `json:"mentions"`
}

// Display define as configurações de exibição do usuário.
type Display struct {
	FontSize       string `json:"fontSize"`
	MessageDensity string `json:"messageDensity"`
	ShowTimestamps bool   `json:"showTimestamps"`
	ShowAvatars    bool   `json:"showAvatars"`
}
