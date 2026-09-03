-- +goose Up
-- Moderação de imagens (nudez/gore) por attachment, assíncrona. O
-- attachment nasce 'pending'; o worker de moderação (worker Python
-- supervisionado pelo processo Go) classifica o blob e grava o resultado
-- final (clean/sensitive/blocked) ou 'failed' após esgotar as tentativas.
-- 'blocked' implica exclusão da mensagem inteira (ON DELETE CASCADE) e log
-- de auditoria explícito.
ALTER TABLE attachments
    ADD COLUMN IF NOT EXISTS moderation_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (moderation_status IN ('pending', 'processing', 'clean', 'sensitive', 'blocked', 'failed')),
    ADD COLUMN IF NOT EXISTS moderation_attempts INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS moderation_checked_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS moderation_updated_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS moderation_model_version TEXT,
    ADD COLUMN IF NOT EXISTS moderation_sfw_score REAL,
    ADD COLUMN IF NOT EXISTS moderation_nudity_score REAL,
    ADD COLUMN IF NOT EXISTS moderation_gore_score REAL;

-- Reconciler: attachments pendentes na ordem de criação. Os 'processing'
-- órfãos (crash em pleno processamento) são detectados por
-- moderation_updated_at, que não se beneficia deste índice (conjunto raro e
-- pequeno, varredura pontual do reconciler).
CREATE INDEX IF NOT EXISTS idx_attachments_moderation_pending
    ON attachments (created_at)
    WHERE moderation_status = 'pending';

-- +goose Down
DROP INDEX IF EXISTS idx_attachments_moderation_pending;

ALTER TABLE attachments
    DROP COLUMN IF EXISTS moderation_status,
    DROP COLUMN IF EXISTS moderation_attempts,
    DROP COLUMN IF EXISTS moderation_checked_at,
    DROP COLUMN IF EXISTS moderation_updated_at,
    DROP COLUMN IF EXISTS moderation_model_version,
    DROP COLUMN IF EXISTS moderation_sfw_score,
    DROP COLUMN IF EXISTS moderation_nudity_score,
    DROP COLUMN IF EXISTS moderation_gore_score;
