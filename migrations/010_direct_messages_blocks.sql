-- Go Migration File
-- GOOS=linux GOARCH=amd64 go run github.com/pressly/goose/v3/cmd/goose

-- +goose Up
ALTER TABLE channels DROP CONSTRAINT IF EXISTS channels_type_check;
ALTER TABLE channels
    ADD CONSTRAINT channels_type_check
    CHECK (type IN ('text', 'category', 'voice', 'dm'));

CREATE TABLE IF NOT EXISTS direct_conversations (
    channel_id UUID PRIMARY KEY REFERENCES channels(id) ON DELETE CASCADE,
    user_low_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    user_high_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (user_low_id <> user_high_id),
    CHECK (user_low_id::text < user_high_id::text),
    UNIQUE (user_low_id, user_high_id)
);

CREATE INDEX IF NOT EXISTS idx_direct_conversations_high_user
    ON direct_conversations (user_high_id);

CREATE TABLE IF NOT EXISTS direct_conversation_state (
    channel_id UUID NOT NULL REFERENCES direct_conversations(channel_id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    hidden_at TIMESTAMPTZ,
    PRIMARY KEY (channel_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_direct_conversation_state_visible
    ON direct_conversation_state (user_id, channel_id)
    WHERE hidden_at IS NULL;

CREATE TABLE IF NOT EXISTS user_blocks (
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    blocked_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, blocked_user_id),
    CHECK (user_id <> blocked_user_id)
);

CREATE INDEX IF NOT EXISTS idx_user_blocks_blocked_user
    ON user_blocks (blocked_user_id, user_id);

-- +goose Down
DROP TABLE IF EXISTS user_blocks;
DROP TABLE IF EXISTS direct_conversation_state;
DROP TABLE IF EXISTS direct_conversations;

ALTER TABLE channels DROP CONSTRAINT IF EXISTS channels_type_check;
ALTER TABLE channels
    ADD CONSTRAINT channels_type_check
    CHECK (type IN ('text', 'category', 'voice'));
