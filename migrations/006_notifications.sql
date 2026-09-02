-- +goose Up
-- Notificações por canal: channel_user_settings guarda a configuração de
-- notificação do usuário em cada canal (off/only_mentions/all). Ausência de
-- row equivale a only_mentions (padrão).
-- notifications guarda as notificações persistidas (usuário x mensagem,
-- read = false até o usuário marcá-las como lidas). A unicidade
-- (user_id, message_id) torna o disparo idempotente: uma mensagem gera no
-- máximo 1 notificação por usuário.
CREATE TABLE IF NOT EXISTS channel_user_settings (
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel_id UUID NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    notification_settings TEXT NOT NULL CHECK (notification_settings IN ('off', 'only_mentions', 'all')),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, channel_id)
);

CREATE TABLE IF NOT EXISTS notifications (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    message_id UUID NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    read BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, message_id)
);

-- Listagem por usuário em ordem decrescente (cursor created_at + id).
CREATE INDEX IF NOT EXISTS idx_notifications_user_created_id ON notifications (user_id, created_at DESC, id DESC);

-- CASCADE de exclusão de mensagens (mesma convenção de pinned_messages).
CREATE INDEX IF NOT EXISTS idx_notifications_message_id ON notifications (message_id);

-- +goose Down
DROP INDEX IF EXISTS idx_notifications_message_id;
DROP INDEX IF EXISTS idx_notifications_user_created_id;
DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS channel_user_settings;
