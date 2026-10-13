-- +goose Up
-- Categorias persistidas; a coluna nula preserva os canais anteriores.
ALTER TABLE channels
    ADD COLUMN parent_id UUID REFERENCES channels(id) ON DELETE SET NULL;
ALTER TABLE channels
    ADD CONSTRAINT channels_parent_only_children
    CHECK (parent_id IS NULL OR type IN ('text', 'voice'));
CREATE INDEX channels_parent_id_idx ON channels(parent_id)
    WHERE parent_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS channels_parent_id_idx;
ALTER TABLE channels DROP CONSTRAINT IF EXISTS channels_parent_only_children;
ALTER TABLE channels DROP COLUMN IF EXISTS parent_id;
