package models

import "time"

// ChannelPermission define as permissões de uma role em um canal (JSONB channel_permissions).
type ChannelPermission struct {
	ReadChannel    bool `json:"read_channel"`
	SendMessages   bool `json:"send_messages"`
	DeleteMessages bool `json:"delete_messages"`
}

// Channel representa a tabela channels.
// Permissions mapeia role_id para as permissões dessa role no canal.
// Position é a posição do canal na visualização do servidor (1-based,
// pseudo-unique por servidor, calculada pelo backend).
type Channel struct {
	ID          string                       `db:"id" json:"id"`
	ServerID    string                       `db:"server_id" json:"server_id"`
	Name        string                       `db:"name" json:"name"`
	Permissions map[string]ChannelPermission `db:"permissions" json:"permissions"`
	Type        string                       `db:"type" json:"type"`
	Position    int                          `db:"position" json:"position"`
	CreatedAt   time.Time                    `db:"created_at" json:"created_at"`
}

// ChannelPermissionEntry é uma entrada da lista de permissões de um canal:
// a role e o que ela pode fazer nesse canal (respostas da API de canais).
type ChannelPermissionEntry struct {
	RoleID      string            `json:"role_id"`
	RoleName    string            `json:"role_name"`
	Permissions ChannelPermission `json:"permissions"`
}

// ChannelLastMessage é a última mensagem de um canal, como exibida na
// listagem de canais.
type ChannelLastMessage struct {
	ID             string    `json:"id"`
	Content        *string   `json:"content"`
	AuthorID       *string   `json:"author_id"`
	AuthorUsername *string   `json:"author_username"`
	CreatedAt      time.Time `json:"created_at"`
}

// ChannelSummary é a visão de canal exposta pela API (GET /channels e
// POST /channels): permissões expandidas por role e a última mensagem do
// canal (null quando o canal não tem mensagens).
type ChannelSummary struct {
	ID          string                   `json:"id"`
	ServerID    string                   `json:"server_id"`
	Name        string                   `json:"name"`
	Type        string                   `json:"type"`
	Position    int                      `json:"position"`
	Permissions []ChannelPermissionEntry `json:"permissions"`
	CreatedAt   time.Time                `json:"created_at"`
	LastMessage *ChannelLastMessage      `json:"last_message"`
}
