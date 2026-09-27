-- +goose Up
-- push_devices: dispositivos registrados para notificação push (FCM),
-- associados ao usuário autenticado. token UNIQUE (1 token = 1 dispositivo).
-- enabled=false desativa a entrega (token inválido detectado pelo papo-push);
-- o token permanece para auditoria/reativação manual.
CREATE TABLE IF NOT EXISTS push_devices (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token TEXT NOT NULL UNIQUE,
    platform TEXT NOT NULL CHECK (platform IN ('android', 'ios')),
    provider TEXT NOT NULL DEFAULT 'fcm',
    device_name TEXT,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Listagem de dispositivos ativos do usuário pelo papo-push.
CREATE INDEX IF NOT EXISTS push_devices_user_id_idx ON push_devices(user_id);

-- push_outbox: fila persistente de jobs de entrega push. O backend grava o
-- job na mesma transação da notificação; o papo-push consome, retry,
-- invalida tokens e finaliza os jobs.
-- Uma linha = "esta notificação precisa ser entregue aos dispositivos
-- deste usuário". notification_id NULL para eventos efêmeros (sem row).
-- delivered_tokens persiste os tokens já entregues em tentativas anteriores,
-- para que um retry não reenvie (e duplicar) a notificação em dispositivos
-- que já receberam.
CREATE TABLE IF NOT EXISTS push_outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    notification_id UUID,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    payload JSONB NOT NULL,
    attempts INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error TEXT,
    delivered_tokens JSONB NOT NULL DEFAULT '[]'::jsonb
);

CREATE INDEX IF NOT EXISTS push_outbox_due_idx ON push_outbox(next_attempt_at);

-- +goose Down
DROP INDEX IF EXISTS push_outbox_due_idx;
DROP TABLE IF EXISTS push_outbox;
DROP INDEX IF EXISTS push_devices_user_id_idx;
DROP TABLE IF EXISTS push_devices;
