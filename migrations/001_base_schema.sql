-- Go Migration File
-- GOOS=linux GOARCH=amd64 go run github.com/pressly/goose/v3/cmd/goose

-- +goose Up
-- Mídia content-addressable: toda imagem/arquivo do sistema (avatar, ícone,
-- emoji, attachment, thumbnail, imagem de link preview) é gravada uma única
-- vez em disco (media/<2hex>/<2hex>/<sha256>) e referenciada pelo sha256.
-- Append-only: nada é excluído (GC futuro remove o sem referência).
CREATE TABLE IF NOT EXISTS media (
    sha_hash TEXT PRIMARY KEY,
    mime_type TEXT NOT NULL,
    size_bytes BIGINT NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username TEXT UNIQUE NOT NULL,
    nickname TEXT,
    password_hash TEXT NOT NULL,
    avatar_media TEXT REFERENCES media(sha_hash),
    banner_media TEXT REFERENCES media(sha_hash),
    description TEXT,
    banned BOOLEAN NOT NULL DEFAULT FALSE,
    reset_password BOOLEAN NOT NULL DEFAULT FALSE,
    last_ip TEXT,
    status TEXT CHECK (status IN ('away', 'busy')),
    status_message TEXT,
    status_updated_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Singleton: o sistema tem exatamente 1 servidor (1 backend = 1 server).
-- A coluna singleton garante no máximo 1 row via UNIQUE.
CREATE TABLE IF NOT EXISTS servers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id UUID REFERENCES users(id),
    name TEXT NOT NULL,
    icon_media TEXT REFERENCES media(sha_hash),
    public_server BOOLEAN NOT NULL DEFAULT TRUE,
    password_hash TEXT,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    singleton BOOLEAN NOT NULL DEFAULT TRUE CHECK (singleton) UNIQUE
);

CREATE TABLE IF NOT EXISTS channels (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL UNIQUE,
    permissions JSONB DEFAULT '{}',
    position INTEGER NOT NULL,
    type TEXT NOT NULL DEFAULT 'text' CHECK (type IN ('text', 'category')),
    created_at TIMESTAMPTZ DEFAULT NOW(),
    tsv_content TSVECTOR GENERATED ALWAYS AS (to_tsvector('portuguese', name)) STORED
);

CREATE TABLE IF NOT EXISTS messages (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    channel_id UUID NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    author_id UUID REFERENCES users(id),
    content TEXT,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    edited_at TIMESTAMPTZ DEFAULT NULL,
    tsv_content TSVECTOR GENERATED ALWAYS AS (to_tsvector('portuguese', content)) STORED
);

CREATE TABLE IF NOT EXISTS attachments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    original_file_name TEXT NOT NULL,
    messages_id UUID REFERENCES messages(id) ON DELETE CASCADE,
    media_sha_hash TEXT NOT NULL REFERENCES media(sha_hash),
    created_by UUID REFERENCES users(id),
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS emojis (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT UNIQUE NOT NULL,
    image_media TEXT NOT NULL REFERENCES media(sha_hash),
    created_by UUID REFERENCES users(id),
    created_at TIMESTAMPTZ DEFAULT NOW()
);


CREATE TABLE IF NOT EXISTS user_channel_state (
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel_id UUID NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    last_read_message_id UUID NOT NULL,
    last_read_at TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (user_id, channel_id)
);

CREATE TABLE IF NOT EXISTS user_settings (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    version INTEGER NOT NULL DEFAULT 1,
    config JSONB NOT NULL DEFAULT '{}',
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS roles (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL,
    color TEXT,
    permissions JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (name)
);

CREATE TABLE IF NOT EXISTS user_roles (
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role_id UUID NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    assigned_at TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (user_id, role_id)
);

-- +goose Down
DROP TABLE IF EXISTS user_roles;
DROP TABLE IF EXISTS roles;
DROP TABLE IF EXISTS user_channel_state;
DROP TABLE IF EXISTS user_settings;
DROP TABLE IF EXISTS emojis;
DROP TABLE IF EXISTS attachments;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS channels;
DROP TABLE IF EXISTS servers;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS media;
