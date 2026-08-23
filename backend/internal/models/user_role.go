package models

import "time"

// UserRole representa a tabela user_roles.
type UserRole struct {
	UserID     string    `db:"user_id" json:"user_id"`
	RoleID     string    `db:"role_id" json:"role_id"`
	AssignedAt time.Time `db:"assigned_at" json:"assigned_at"`
}
