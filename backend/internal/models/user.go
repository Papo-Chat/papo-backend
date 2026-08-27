package models

import "time"

// User representa a tabela users.
// AvatarMedia referencia o blob do avatar na tabela media (nil quando não
// há avatar); AvatarBlob e AvatarFormat são resolvidos do disco pelo service
// para as respostas da API.
type User struct {
	ID              string     `db:"id" json:"id"`
	Username        string     `db:"username" json:"username"`
	Nickname        *string    `db:"nickname" json:"nickname"`
	PasswordHash    string     `db:"password_hash" json:"-"`
	AvatarMedia     *string    `db:"avatar_media" json:"-"`
	AvatarBlob      []byte     `json:"avatar_blob"`
	AvatarFormat    string     `json:"avatar_format"`
	Banned          bool       `db:"banned" json:"banned"`
	ResetPassword   bool       `db:"reset_password" json:"reset_password"`
	LastIP          *string    `db:"last_ip" json:"-"`
	Status          *string    `db:"status" json:"status"`
	StatusMessage   *string    `db:"status_message" json:"status_message"`
	StatusUpdatedAt *time.Time `db:"status_updated_at" json:"status_updated_at"`
	CreatedAt       time.Time  `db:"created_at" json:"created_at"`
}

// UserSummary representa uma visão reduzida de usuário para listagens (GET /users),
// sem campos sensíveis ou densos (password_hash, avatar, banned, last_ip).
type UserSummary struct {
	ID              string     `db:"id" json:"id"`
	Username        string     `db:"username" json:"username"`
	Nickname        *string    `db:"nickname" json:"nickname"`
	Status          *string    `db:"status" json:"status"`
	StatusMessage   *string    `db:"status_message" json:"status_message"`
	StatusUpdatedAt *time.Time `db:"status_updated_at" json:"status_updated_at"`
	CreatedAt       time.Time  `db:"created_at" json:"created_at"`
}

// UserList é a resposta paginada de GET /users.
type UserList struct {
	Users   []UserSummary `json:"users"`
	HasMore bool          `json:"has_more"`
}
