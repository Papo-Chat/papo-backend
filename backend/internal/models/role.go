package models

import "time"

// RolePermissions define as permissões de uma role (JSONB role_permissions).
type RolePermissions struct {
	ManageServer    bool `json:"manage_server"`
	ManageChannels  bool `json:"manage_channels"`
	ManageRoles     bool `json:"manage_roles"`
	BanMembers      bool `json:"ban_members"`
	PinMessage      bool `json:"pin_message"`
	EveryoneMessage bool `json:"everyone_message"`
	SendAttachment  bool `json:"send_attachment"`
}

// Role representa a tabela roles.
type Role struct {
	ID          string          `db:"id" json:"id"`
	ServerID    string          `db:"server_id" json:"server_id"`
	Name        string          `db:"name" json:"name"`
	Color       *string         `db:"color" json:"color"`
	Permissions RolePermissions `db:"permissions" json:"permissions"`
	CreatedAt   time.Time       `db:"created_at" json:"created_at"`
}
