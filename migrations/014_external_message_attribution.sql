-- +goose Up
-- Incoming webhooks are independently scoped actors; secrets are stored only as hashes.
CREATE TABLE webhooks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    channel_id UUID NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    created_by UUID NOT NULL REFERENCES users(id),
    secret_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMPTZ
);

CREATE INDEX idx_webhooks_channel ON webhooks (channel_id);
ALTER TABLE messages
    ADD COLUMN webhook_id UUID REFERENCES webhooks(id),
    ADD COLUMN external_author JSONB,
    ADD COLUMN external_message_id TEXT;
CREATE UNIQUE INDEX idx_messages_webhook_external_id
    ON messages(webhook_id, external_message_id)
    WHERE webhook_id IS NOT NULL AND external_message_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_messages_webhook_external_id;
ALTER TABLE messages
    DROP COLUMN external_message_id,
    DROP COLUMN external_author,
    DROP COLUMN webhook_id;
DROP TABLE webhooks;
