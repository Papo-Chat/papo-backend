-- +goose Up
-- Categoria persistida de cada canal. Até aqui a categoria era só implícita
-- na ordem (os canais depois de uma categoria pertenciam a ela); parent_id
-- nulo mantém esse comportamento para os canais já existentes.
-- Excluir a categoria deixa os canais dela sem categoria, em vez de
-- excluí-los junto.
ALTER TABLE channels
    ADD COLUMN parent_id UUID REFERENCES channels(id) ON DELETE SET NULL;

-- Só canais de texto e de voz ficam dentro de uma categoria: categoria não
-- aninha categoria, e DM não pertence a categoria.
ALTER TABLE channels
    ADD CONSTRAINT channels_parent_only_children
    CHECK (parent_id IS NULL OR type IN ('text', 'voice'));

CREATE INDEX channels_parent_id_idx ON channels(parent_id)
    WHERE parent_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS channels_parent_id_idx;
ALTER TABLE channels DROP CONSTRAINT IF EXISTS channels_parent_only_children;
ALTER TABLE channels DROP COLUMN IF EXISTS parent_id;
