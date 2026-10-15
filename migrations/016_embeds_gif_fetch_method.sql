-- Go Migration File
-- GOOS=linux GOARCH=amd64 go run github.com/pressly/goose/v3/cmd/goose

-- +goose Up
-- GIF embeds de tenor/giphy/klipy (resolvedor em services/embeds_gif.go)
-- ganham um método de fetch próprio: o resolvedor baixa a rendition GIF do
-- provedor e a grava como thumbnail animada na media. A deduplicação continua
-- sendo por cache_key (URL normalizada da mensagem): uma URL já scrapada é
-- reutilizada do banco até o TTL do cache expirar.
ALTER TABLE embeds DROP CONSTRAINT embeds_fetch_method_check;
ALTER TABLE embeds ADD CONSTRAINT embeds_fetch_method_check
    CHECK (fetch_method IN ('opengraph', 'oembed', 'manual', 'gif'));

-- +goose Down
ALTER TABLE embeds DROP CONSTRAINT embeds_fetch_method_check;
UPDATE embeds SET fetch_method = 'opengraph' WHERE fetch_method = 'gif';
ALTER TABLE embeds ADD CONSTRAINT embeds_fetch_method_check
    CHECK (fetch_method IN ('opengraph', 'oembed', 'manual'));
