-- +goose Up
-- EN-US: Expire translated/icon-only previews once, retaining shared embed IDs, media and message links.
-- PT-BR: Expira uma vez os previews traduzidos/com ícone, preservando IDs compartilhados, mídia e vínculos.
UPDATE embeds
   SET fetched_at = to_timestamp(0)
 WHERE source_type = 'link'
   AND cache_key IS NOT NULL
   AND lower(split_part(cache_key, '/', 3)) IN (
       'reddit.com', 'www.reddit.com', 'old.reddit.com', 'new.reddit.com',
       'm.reddit.com', 'sh.reddit.com', 'np.reddit.com', 'redd.it', 'www.redd.it'
   );

-- +goose Down
-- EN-US: Original timestamps are unavailable; revalidate only still-expired Reddit entries on rollback.
-- PT-BR: Timestamps originais não estão disponíveis; revalida apenas entradas do Reddit ainda expiradas.
UPDATE embeds
   SET fetched_at = NOW()
 WHERE source_type = 'link'
   AND cache_key IS NOT NULL
   AND fetched_at = to_timestamp(0)
   AND lower(split_part(cache_key, '/', 3)) IN (
       'reddit.com', 'www.reddit.com', 'old.reddit.com', 'new.reddit.com',
       'm.reddit.com', 'sh.reddit.com', 'np.reddit.com', 'redd.it', 'www.redd.it'
   );
