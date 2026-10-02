-- +goose Up
ALTER TABLE link_previews
    ADD COLUMN IF NOT EXISTS video_url TEXT;

-- +goose Down
ALTER TABLE link_previews
    DROP COLUMN IF EXISTS video_url;
