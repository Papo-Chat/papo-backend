package models

import "time"

// AuditLog representa a tabela audit_logs (registro completo, append-only).
// ActorUsername é um snapshot do username do ator no momento da operação.
type AuditLog struct {
	ID            string         `db:"id" json:"id"`
	ActorID       *string        `db:"actor_id" json:"actor_id"`
	ActorUsername string         `db:"actor_username" json:"actor_username"`
	Action        string         `db:"action" json:"action"`
	EntityType    string         `db:"entity_type" json:"entity_type"`
	EntityID      *string        `db:"entity_id" json:"entity_id"`
	TargetUserID  *string        `db:"target_user_id" json:"target_user_id"`
	Metadata      map[string]any `db:"metadata" json:"metadata"`
	IPAddress     *string        `db:"ip_address" json:"ip_address"`
	UserAgent     *string        `db:"user_agent" json:"user_agent"`
	CreatedAt     time.Time      `db:"created_at" json:"created_at"`
}

// AuditLogEntry é a entrada exposta em GET /admin/audit-logs.
type AuditLogEntry struct {
	ID            string         `json:"id"`
	ActorUsername string         `json:"actor_username"`
	Action        string         `json:"action"`
	EntityType    string         `json:"entity_type"`
	TargetUserID  *string        `json:"target_user_id"`
	Metadata      map[string]any `json:"metadata"`
	CreatedAt     time.Time      `json:"created_at"`
}

// AuditLogList é a resposta paginada de GET /admin/audit-logs.
type AuditLogList struct {
	Logs    []AuditLogEntry `json:"logs"`
	HasMore bool            `json:"has_more"`
}
