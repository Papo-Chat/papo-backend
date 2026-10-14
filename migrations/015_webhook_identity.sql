-- +goose Up
-- Identidade persistente do webhook: nickname de exibição e avatar
-- content-addressable (mesma referência media usada por avatares de usuário).
-- display_name NULL significa "usar o name do webhook" como nickname.
-- A identidade pode ser sobrescrita por mensagem (external_author no envio),
-- que é sempre temporária e nunca altera estas colunas.
ALTER TABLE webhooks
    ADD COLUMN display_name TEXT,
    ADD COLUMN avatar_media TEXT REFERENCES media(sha_hash);

-- +goose Down
ALTER TABLE webhooks
    DROP COLUMN avatar_media,
    DROP COLUMN display_name;
