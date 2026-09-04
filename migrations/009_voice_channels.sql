-- +goose Up
-- Canais de voz (SFU embutido no backend): o tipo 'voice' se junta a
-- 'text' e 'category'. Sem nova tabela nem coluna: o estado da call é
-- efêmero em memória (mesma filosofia da presença). A constraint
-- channels_topic_text_only já permite topic em canais voice (ela só
-- proíbe topic em category).
ALTER TABLE channels DROP CONSTRAINT IF EXISTS channels_type_check;
ALTER TABLE channels ADD CONSTRAINT channels_type_check CHECK (type IN ('text', 'category', 'voice'));

-- +goose Down
ALTER TABLE channels DROP CONSTRAINT IF EXISTS channels_type_check;
ALTER TABLE channels ADD CONSTRAINT channels_type_check CHECK (type IN ('text', 'category'));
