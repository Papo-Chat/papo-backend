-- +goose Up
CREATE TABLE IF NOT EXISTS audit_logs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_id UUID REFERENCES users(id) ON DELETE SET NULL,
    actor_username TEXT NOT NULL,
    action TEXT NOT NULL,
    entity_type TEXT NOT NULL,
    entity_id UUID,
    target_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    metadata JSONB NOT NULL DEFAULT '{}',
    ip_address TEXT,
    user_agent TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_audit_logs_server_created ON audit_logs (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_actor ON audit_logs (actor_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_entity ON audit_logs (entity_type, entity_id);
CREATE INDEX IF NOT EXISTS idx_audit_logs_action ON audit_logs (action);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION prevent_audit_logs_modification()
RETURNS trigger
AS $function$
BEGIN
    RAISE EXCEPTION 'audit_logs é append-only: UPDATE e DELETE não são permitidos';
END;
$function$
LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER prevent_audit_logs_modification
BEFORE UPDATE OR DELETE ON audit_logs
FOR EACH ROW
EXECUTE FUNCTION prevent_audit_logs_modification();

-- +goose Down

DROP TRIGGER IF EXISTS prevent_audit_logs_modification ON audit_logs;
DROP TRIGGER IF EXISTS audit_logs_no_update_delete ON audit_logs;

DROP FUNCTION IF EXISTS prevent_audit_logs_modification();

DROP INDEX IF EXISTS idx_audit_logs_action;
DROP INDEX IF EXISTS idx_audit_logs_entity;
DROP INDEX IF EXISTS idx_audit_logs_actor;
DROP INDEX IF EXISTS idx_audit_logs_server_created;

DROP TABLE IF EXISTS audit_logs;