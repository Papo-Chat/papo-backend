-- +goose Up
-- Reações: guarda a lista de usuários que reagiram a uma mensagem específica.
-- Cada reação é um emoji custom do banco (emoji_id) OU um emoji unicode
-- (unicode), nunca os dois (CHECK). O mesmo usuário reage no máximo uma vez
-- por emoji por mensagem (índice único abaixo).
-- A PK é uma chave artificial (id) porque a regra de unicidade real envolve
-- colunas nullable (emoji_id/unicode) e PK não pode conter NULL.
CREATE TABLE IF NOT EXISTS message_reactions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id UUID NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    emoji_id UUID REFERENCES emojis(id) ON DELETE CASCADE,
    unicode TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT message_reactions_exactly_one_emoji
        CHECK ((emoji_id IS NULL AND unicode IS NOT NULL)
             OR (emoji_id IS NOT NULL AND unicode IS NULL))
);

-- Um usuário reage no máximo uma vez por emoji (custom ou unicode) por
-- mensagem. Como emoji_id/unicode são nullable (exatos-um), a unicidade é
-- garantida por este índice com COALESCE, não pela PK.
CREATE UNIQUE INDEX IF NOT EXISTS idx_message_reactions_unique
    ON message_reactions (message_id, user_id,
        COALESCE(emoji_id, '00000000-0000-0000-0000-000000000000'::uuid),
        COALESCE(unicode, ''));

CREATE INDEX IF NOT EXISTS idx_message_reactions_message ON message_reactions (message_id);

-- +goose Down
DROP INDEX IF EXISTS idx_message_reactions_message;
DROP INDEX IF EXISTS idx_message_reactions_unique;
DROP TABLE IF EXISTS message_reactions;
