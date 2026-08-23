-- Go Migration File
-- GOOS=linux GOARCH=amd64 go run github.com/pressly/goose/v3/cmd/goose

-- +goose Up
CREATE INDEX IF NOT EXISTS idx_servers_owner_id ON servers (owner_id);

CREATE INDEX IF NOT EXISTS idx_channels_server_id ON channels (server_id);

CREATE INDEX IF NOT EXISTS idx_messages_author_id ON messages (author_id);
CREATE INDEX IF NOT EXISTS idx_messages_tsv_content ON messages USING GIN (tsv_content);
CREATE INDEX IF NOT EXISTS idx_messages_channel_created_id ON messages (channel_id, created_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS idx_attachments_messages_id ON attachments (messages_id);
CREATE INDEX IF NOT EXISTS idx_attachments_sha_hash ON attachments (sha_hash);

CREATE INDEX IF NOT EXISTS idx_emojis_server_id ON emojis (server_id);

CREATE INDEX IF NOT EXISTS idx_roles_server_id ON roles (server_id);

CREATE INDEX IF NOT EXISTS idx_users_banned_last_ip ON users (last_ip) WHERE banned = TRUE;

CREATE INDEX IF NOT EXISTS idx_user_channel_state_channel_id ON user_channel_state (channel_id);

CREATE INDEX IF NOT EXISTS idx_user_roles_role_id ON user_roles (role_id);

-- +goose Down
DROP INDEX IF EXISTS idx_user_roles_role_id;
DROP INDEX IF EXISTS idx_user_channel_state_channel_id;
DROP INDEX IF EXISTS idx_users_banned_last_ip;
DROP INDEX IF EXISTS idx_roles_server_id;
DROP INDEX IF EXISTS idx_emojis_server_id;
DROP INDEX IF EXISTS idx_attachments_sha_hash;
DROP INDEX IF EXISTS idx_attachments_messages_id;
DROP INDEX IF EXISTS idx_messages_channel_created_id;
DROP INDEX IF EXISTS idx_messages_tsv_content;
DROP INDEX IF EXISTS idx_messages_author_id;
DROP INDEX IF EXISTS idx_channels_server_id;
DROP INDEX IF EXISTS idx_servers_owner_id;
