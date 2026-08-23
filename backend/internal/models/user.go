package models

import "time"

// User representa a tabela users.
type User struct {
	ID              string     `db:"id" json:"id"`
	Username        string     `db:"username" json:"username"`
	Nickname        *string    `db:"nickname" json:"nickname"`
	PasswordHash    string     `db:"password_hash" json:"-"`
	AvatarBlob      []byte     `db:"avatar_blob" json:"avatar_blob"`
	AvatarFormat    string     `db:"avatar_format" json:"avatar_format"`
	Banned          bool       `db:"banned" json:"banned"`
	ResetPassword   bool       `db:"reset_password" json:"reset_password"`
	LastIP          *string    `db:"last_ip" json:"-"`
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
	StatusMessage   *string    `db:"status_message" json:"status_message"`
	StatusUpdatedAt *time.Time `db:"status_updated_at" json:"status_updated_at"`
	CreatedAt       time.Time  `db:"created_at" json:"created_at"`
}

// UserList é a resposta paginada de GET /users.
type UserList struct {
	Users   []UserSummary `json:"users"`
	HasMore bool          `json:"has_more"`
}
