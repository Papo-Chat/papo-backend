-- +goose Up
-- Largura/altura originais para geometria reservada do placeholder de vídeo.
-- NULL significa formato não suportado, metadados ausentes ou dimensões suspeitas.
ALTER TABLE attachments
    ADD COLUMN width INTEGER,
    ADD COLUMN height INTEGER,
    ADD CONSTRAINT attachments_video_dimensions_valid CHECK (
        (width IS NULL AND height IS NULL)
        OR (
            width BETWEEN 1 AND 16384
            AND height BETWEEN 1 AND 16384
            AND width::BIGINT * height::BIGINT <= 100000000
            AND width::BIGINT <= height::BIGINT * 10
            AND height::BIGINT <= width::BIGINT * 10
        )
    );

-- +goose Down
ALTER TABLE attachments
    DROP CONSTRAINT IF EXISTS attachments_video_dimensions_valid,
    DROP COLUMN IF EXISTS width,
    DROP COLUMN IF EXISTS height;
