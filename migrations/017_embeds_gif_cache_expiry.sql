-- Go Migration File
-- GOOS=linux GOARCH=amd64 go run github.com/pressly/goose/v3/cmd/goose

-- +goose Up
-- Invalida o cache de embeds de GIF (tenor/giphy/klipy) produzido antes do
-- resolvedor (services/embeds_gif.go) entregar o GIF animado do provedor:
-- essas rows gravaram thumbnail WebP estática (re-encode do pipeline de
-- thumbnail) ou vídeo mp4 vindo do OpenGraph da página.
--
-- A limpeza é por expiração (fetched_at no epoch → fora do TTL do cache em
-- GetOrCreateEmbed), nunca por DELETE: embeds são rows compartilhadas por
-- cache_key e vinculadas às mensagens via message_embeds (ON DELETE CASCADE),
-- e link embed só é raspado na criação/edição de mensagem. Apagar as rows
-- apagaria o embed das mensagens antigas; expirar mantém as mensagens antigas
-- como estão e faz a próxima mensagem com a mesma URL refazer o scrape — o
-- upsert reescreve a mesma row e o evento message_embeds_update atualiza as
-- mensagens que já a usam. A media antiga (WebP estático) deixa de ser
-- referenciada no refetch e é coletada pelo GC de mídia da manutenção.
--
-- Escopo: embeds de link raspados pelo resolvedor GIF e embeds de link de
-- página de provedor GIF raspados pelas rotas antigas (OpenGraph/oEmbed), que
-- hoje são roteadas para o resolvedor.
UPDATE embeds
   SET fetched_at = to_timestamp(0)
 WHERE source_type = 'link'
   AND cache_key IS NOT NULL
   AND (
        fetch_method = 'gif'
        OR lower(split_part(cache_key, '/', 3)) ~ '^(.+\.)?(tenor\.com|giphy\.com|giphy\.media|klipy\.com)$'
   );

-- +goose Down
-- A marcação de expirado não é reversível (os timestamps originais não são
-- guardados): o down re-valida o cache das mesmas rows, devolvendo-as ao TTL
-- normal sem refazer o scrape.
UPDATE embeds
   SET fetched_at = NOW()
 WHERE source_type = 'link'
   AND cache_key IS NOT NULL
   AND fetched_at = to_timestamp(0)
   AND (
        fetch_method = 'gif'
        OR lower(split_part(cache_key, '/', 3)) ~ '^(.+\.)?(tenor\.com|giphy\.com|giphy\.media|klipy\.com)$'
   );
