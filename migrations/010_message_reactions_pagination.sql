-- +goose Up
-- Índice de paginação de reações por mensagem: serve a listagem paginada
-- (GET /channels/:channel_id/messages/:message_id/reactions) com
-- ORDER BY created_at DESC, id DESC, evitando sort em memória (o scanner
-- percorre o índice já na ordem do cursor (created_at, id)).
CREATE INDEX IF NOT EXISTS idx_message_reactions_message_created
    ON message_reactions (message_id, created_at DESC, id DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_message_reactions_message_created;
