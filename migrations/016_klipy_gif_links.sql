-- +goose Up
-- Allow a direct Klipy GIF URL to be returned as a thumbnail without
-- downloading, transforming, proxying, or caching the GIF bytes.
ALTER TABLE embeds ADD COLUMN thumbnail_url TEXT;

ALTER TABLE embeds DROP CONSTRAINT IF EXISTS embeds_fetch_method_check;
ALTER TABLE embeds ADD CONSTRAINT embeds_fetch_method_check
    CHECK (fetch_method IN ('opengraph', 'oembed', 'manual', 'klipy'));

-- +goose Down
UPDATE embeds SET fetch_method = 'opengraph' WHERE fetch_method = 'klipy';
ALTER TABLE embeds DROP CONSTRAINT IF EXISTS embeds_fetch_method_check;
ALTER TABLE embeds ADD CONSTRAINT embeds_fetch_method_check
    CHECK (fetch_method IN ('opengraph', 'oembed', 'manual'));
ALTER TABLE embeds DROP COLUMN IF EXISTS thumbnail_url;
