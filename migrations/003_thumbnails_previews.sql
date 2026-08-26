-- Go Migration File
-- GOOS=linux GOARCH=amd64 go run github.com/pressly/goose/v3/cmd/goose

-- +goose Up
CREATE TABLE IF NOT EXISTS attachment_thumbnails (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    attachment_id UUID NOT NULL REFERENCES attachments(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    mime_type TEXT NOT NULL,
    file_path TEXT NOT NULL,
    size_bytes BIGINT NOT NULL,
    width INT NOT NULL,
    height INT NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (attachment_id, kind)
);

CREATE TABLE IF NOT EXISTS link_previews (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    url TEXT NOT NULL UNIQUE,
    kind TEXT NOT NULL,
    title TEXT,
    description TEXT,
    provider_name TEXT,
    embed_url TEXT,
    image_file_path TEXT,
    image_mime_type TEXT,
    image_size_bytes BIGINT,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS message_previews (
    message_id UUID NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    preview_id UUID NOT NULL REFERENCES link_previews(id) ON DELETE CASCADE,
    PRIMARY KEY (message_id, preview_id)
);

-- +goose Down
DROP TABLE IF EXISTS message_previews;
DROP TABLE IF EXISTS link_previews;
DROP TABLE IF EXISTS attachment_thumbnails;
