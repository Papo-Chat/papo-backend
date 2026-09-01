-- Go Migration File
-- GOOS=linux GOARCH=amd64 go run github.com/pressly/goose/v3/cmd/goose

-- +goose Up
-- Auditoria append-only de operações mutadoras. O sistema tem 1 servidor
-- (1 backend = 1 server), então não há coluna server_id: toda a tabela é
-- global. actor_username é um snapshot do username do ator no momento da
-- operação (o actor_id pode virar NULL se o usuário for excluído, mas o
-- snapshot permanece para rastreabilidade).
CREATE TABLE IF NOT EXISTS audit_logs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_id UUID REFERENCES users(id) ON DELETE SET NULL,
    actor_username TEXT NOT NULL,
    action TEXT NOT NULL,
    entity_type TEXT NOT NULL,
    entity_id UUID,
    target_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    metadata JSONB NOT NULL DEFAULT '{}',
    ip_address INET,
    user_agent TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_audit_logs_server_created ON audit_logs (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_actor ON audit_logs (actor_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_entity ON audit_logs (entity_type, entity_id);
CREATE INDEX IF NOT EXISTS idx_audit_logs_action ON audit_logs (action);

-- Append-only: impede UPDATE/DELETE em audit_logs.
CREATE OR REPLACE FUNCTION prevent_audit_logs_modification() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_logs é append-only: UPDATE e DELETE não são permitidos';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS audit_logs_no_update_delete ON audit_logs;
CREATE TRIGGER audit_logs_no_update_delete
BEFORE UPDATE OR DELETE ON audit_logs
FOR EACH ROW
EXECUTE FUNCTION prevent_audit_logs_modification();

-- +goose Down
DROP TRIGGER IF EXISTS audit_logs_no_update_delete ON audit_logs;
DROP FUNCTION IF EXISTS prevent_audit_logs_modification();
DROP INDEX IF EXISTS idx_audit_logs_action;
DROP INDEX IF EXISTS idx_audit_logs_entity;
DROP INDEX IF EXISTS idx_audit_logs_actor;
DROP INDEX IF EXISTS idx_audit_logs_server_created;
DROP TABLE IF EXISTS audit_logs;
