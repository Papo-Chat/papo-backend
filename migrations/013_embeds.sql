-- Go Migration File
-- GOOS=linux GOARCH=amd64 go run github.com/pressly/goose/v3/cmd/goose

-- +goose Up
-- Unifica previews automáticos de links e embeds personalizados em um único
-- modelo `embeds`: link preview é um embed com source_type='link' (reutilizado
-- pelo cache por URL normalizada); embed manual tem source_type='custom' e é
-- dono do próprio registro (cache_key NULL).

ALTER TABLE message_previews RENAME TO message_embeds;
ALTER TABLE link_previews RENAME TO embeds;

ALTER TABLE embeds RENAME COLUMN url TO cache_key;
ALTER TABLE embeds RENAME COLUMN kind TO fetch_method;
ALTER TABLE embeds RENAME COLUMN provider_name TO provider;
ALTER TABLE embeds RENAME COLUMN image_media TO thumbnail_media;

-- 'og' passa a ser o nome explícito do método de fetch.
UPDATE embeds SET fetch_method = 'opengraph' WHERE fetch_method = 'og';

-- A URL deixa de ser a identidade do embed: cache_key é a chave do cache dos
-- link embeds (NULL nos customizados) e url passa a ser a URL da página
-- exibida (og:url quando o site o declara).
ALTER TABLE embeds DROP CONSTRAINT link_previews_url_key;
ALTER TABLE embeds ALTER COLUMN cache_key DROP NOT NULL;
ALTER TABLE embeds ADD COLUMN url TEXT;
UPDATE embeds SET url = cache_key;

-- fetched_at só faz sentido para embed obtido por fetch (link embeds).
ALTER TABLE embeds ALTER COLUMN fetched_at DROP NOT NULL;

ALTER TABLE embeds
    ADD COLUMN source_type TEXT NOT NULL DEFAULT 'link' CHECK (source_type IN ('link', 'custom')),
    ADD COLUMN site_name TEXT,
    ADD COLUMN color TEXT,
    ADD COLUMN author_name TEXT,
    ADD COLUMN author_url TEXT,
    ADD COLUMN author_media TEXT REFERENCES media(sha_hash),
    ADD COLUMN footer_text TEXT,
    ADD COLUMN footer_icon TEXT REFERENCES media(sha_hash),
    ADD COLUMN thumbnail_width INT,
    ADD COLUMN thumbnail_height INT,
    ADD COLUMN image_media TEXT REFERENCES media(sha_hash),
    ADD COLUMN image_width INT,
    ADD COLUMN image_height INT,
    ADD COLUMN video_type TEXT,
    ADD COLUMN video_width INT,
    ADD COLUMN video_height INT,
    ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

ALTER TABLE embeds ADD CONSTRAINT embeds_fetch_method_check
    CHECK (fetch_method IN ('opengraph', 'oembed', 'manual'));

-- Link embed é sempre resultado de um fetch; embed customizado não tem
-- fetched_at (o registro é criado pela mensagem).
ALTER TABLE embeds ADD CONSTRAINT embeds_link_fetched_at_check
    CHECK (source_type <> 'link' OR fetched_at IS NOT NULL);

CREATE UNIQUE INDEX IF NOT EXISTS idx_embeds_cache_key ON embeds (cache_key)
    WHERE cache_key IS NOT NULL;

ALTER TABLE message_embeds RENAME COLUMN preview_id TO embed_id;
ALTER TABLE message_embeds DROP CONSTRAINT message_previews_pkey;
ALTER TABLE message_embeds ADD CONSTRAINT message_embeds_pkey
    PRIMARY KEY (message_id, embed_id);

CREATE INDEX IF NOT EXISTS idx_message_embeds_embed_id ON message_embeds (embed_id);

CREATE TABLE IF NOT EXISTS embed_fields (
    embed_id UUID NOT NULL REFERENCES embeds(id) ON DELETE CASCADE,
    position INT NOT NULL,
    name TEXT NOT NULL,
    value TEXT NOT NULL,
    inline BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (embed_id, position)
);

-- +goose Down
DROP TABLE IF EXISTS embed_fields;

DROP INDEX IF EXISTS idx_message_embeds_embed_id;

ALTER TABLE message_embeds DROP CONSTRAINT IF EXISTS message_embeds_pkey;
ALTER TABLE message_embeds RENAME COLUMN embed_id TO preview_id;
ALTER TABLE message_embeds ADD CONSTRAINT message_previews_pkey
    PRIMARY KEY (message_id, preview_id);

DROP INDEX IF EXISTS idx_embeds_cache_key;

ALTER TABLE embeds DROP CONSTRAINT IF EXISTS embeds_fetch_method_check;
ALTER TABLE embeds DROP CONSTRAINT IF EXISTS embeds_link_fetched_at_check;

ALTER TABLE embeds
    DROP COLUMN IF EXISTS source_type,
    DROP COLUMN IF EXISTS site_name,
    DROP COLUMN IF EXISTS color,
    DROP COLUMN IF EXISTS author_name,
    DROP COLUMN IF EXISTS author_url,
    DROP COLUMN IF EXISTS author_media,
    DROP COLUMN IF EXISTS footer_text,
    DROP COLUMN IF EXISTS footer_icon,
    DROP COLUMN IF EXISTS thumbnail_width,
    DROP COLUMN IF EXISTS thumbnail_height,
    DROP COLUMN IF EXISTS image_media,
    DROP COLUMN IF EXISTS image_width,
    DROP COLUMN IF EXISTS image_height,
    DROP COLUMN IF EXISTS video_type,
    DROP COLUMN IF EXISTS video_width,
    DROP COLUMN IF EXISTS video_height,
    DROP COLUMN IF EXISTS created_at;

UPDATE embeds SET fetch_method = 'og' WHERE fetch_method = 'opengraph';

-- Volta url a chave única e não nula dos link previews.
UPDATE embeds SET cache_key = url WHERE cache_key IS NULL;
ALTER TABLE embeds DROP COLUMN IF EXISTS url;
ALTER TABLE embeds ALTER COLUMN cache_key SET NOT NULL;
UPDATE embeds SET fetched_at = NOW() WHERE fetched_at IS NULL;
ALTER TABLE embeds ALTER COLUMN fetched_at SET DEFAULT NOW();
ALTER TABLE embeds ALTER COLUMN fetched_at SET NOT NULL;
ALTER TABLE embeds ADD CONSTRAINT link_previews_url_key UNIQUE (cache_key);

ALTER TABLE embeds RENAME COLUMN thumbnail_media TO image_media;
ALTER TABLE embeds RENAME COLUMN provider TO provider_name;
ALTER TABLE embeds RENAME COLUMN fetch_method TO kind;
ALTER TABLE embeds RENAME COLUMN cache_key TO url;

ALTER TABLE message_embeds RENAME TO message_previews;
ALTER TABLE embeds RENAME TO link_previews;
