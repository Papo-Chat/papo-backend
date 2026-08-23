package models

import "time"

// Server representa a tabela servers.
type Server struct {
	ID           string    `db:"id" json:"id"`
	OwnerID      *string   `db:"owner_id" json:"owner_id"`
	Name         string    `db:"name" json:"name"`
	IconBlob     []byte    `db:"icon_blob" json:"icon_blob"`
	PublicServer bool      `db:"public_server" json:"-"`
	PasswordHash *string   `db:"password_hash" json:"-"`
	IconFormat   string    `db:"icon_format" json:"icon_format"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}

// ServerSummary representa a visão de servidor para listagem e detalhe
// (GET /servers e GET /servers/:server_id), com o username do dono e as
// contagens de canais, membros e roles.
type ServerSummary struct {
	ID            string    `db:"id" json:"id"`
	OwnerID       *string   `db:"owner_id" json:"owner_id"`
	OwnerUsername *string   `db:"owner_username" json:"owner_username"`
	Name          string    `db:"name" json:"name"`
	IconBlob      []byte    `db:"icon_blob" json:"icon_blob"`
	Public        bool      `db:"public_server" json:"public"`
	IconFormat    string    `db:"icon_format" json:"icon_format"`
	CreatedAt     time.Time `db:"created_at" json:"created_at"`
	ChannelCount  int       `db:"channel_count" json:"channel_count"`
	MemberCount   int       `db:"member_count" json:"member_count"`
	RoleCount     int       `db:"role_count" json:"role_count"`
}
