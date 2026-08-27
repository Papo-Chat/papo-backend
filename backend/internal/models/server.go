package models

import "time"

// Server representa a tabela servers.
// IconMedia referencia o blob do ícone na tabela media (nil quando não há
// ícone); IconBlob e IconFormat são resolvidos do disco pelo service para as
// respostas da API.
type Server struct {
	ID           string    `db:"id" json:"id"`
	OwnerID      *string   `db:"owner_id" json:"owner_id"`
	Name         string    `db:"name" json:"name"`
	IconMedia    *string   `db:"icon_media" json:"-"`
	IconBlob     []byte    `json:"icon_blob"`
	PublicServer bool      `db:"public_server" json:"-"`
	PasswordHash *string   `db:"password_hash" json:"-"`
	IconFormat   string    `json:"icon_format"`
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
	IconMedia     *string   `db:"icon_media" json:"-"`
	IconBlob      []byte    `json:"icon_blob"`
	Public        bool      `db:"public_server" json:"public"`
	IconFormat    string    `json:"icon_format"`
	CreatedAt     time.Time `db:"created_at" json:"created_at"`
	ChannelCount  int       `db:"channel_count" json:"channel_count"`
	MemberCount   int       `db:"member_count" json:"member_count"`
	RoleCount     int       `db:"role_count" json:"role_count"`
}
