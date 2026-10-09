# Implementação de Embeds no Papo

## Objetivo

Unificar previews automáticos de links e embeds personalizados em um único modelo `Embed`, mantendo o crawler, cache, armazenamento de mídia e proteções existentes. “Link preview” passa a ser um `Embed` com `source_type = link`; embeds manuais usam `source_type = custom`.

## 1. Banco de dados

Criar uma migration que renomeie, preservando os dados:

- `link_previews` → `embeds`;
- `message_previews` → `message_embeds`;
- `preview_id` → `embed_id`;
- converter `kind` em `fetch_method` (`opengraph`, `oembed` ou `manual`);
- migrar `url` existente para `cache_key` nas linhas de link e criar índice único parcial para `cache_key IS NOT NULL`.

Adicionar em `embeds` os campos `source_type`, `provider`, `color`, `site_name`, metadados do autor, footer, mídia e dimensões. Adicionar `embed_fields` com `embed_id`, `position`, `name`, `value` e `inline`. Manter referências de mídia pela tabela `media` existente. Embeds de link são reutilizados pelo cache; cada embed customizado tem seu próprio registro.

## 2. Modelo Go e contrato da API

Renomear `models.LinkPreview` para `models.Embed` e incluir `EmbedAuthor`, `EmbedMedia`, `EmbedFooter` e `EmbedField`. A mensagem passa a expor `embeds: []Embed`; remover `previews` do contrato após atualizar todos os consumidores.

O objeto `Embed` contém `id`, `source_type`, `provider`, `fetch_method`, `url`, `title`, `description`, `color`, `author`, `thumbnail`, `image`, `video`, `footer`, `fields`, `created_at` e `fetched_at` quando aplicável. Campos ausentes são `null` ou omitidos de forma consistente.

Renomear `GET /link-previews/:preview_id` para `GET /embeds/:embed_id`, preservando autorização baseada no acesso de leitura ao canal. Adaptar o endpoint de vídeo para `GET /embeds/:embed_id/video`. A criação/edição de mensagem aceita embeds customizados em um campo JSON `embeds`, validado no service; o cliente não pode definir `source_type = link` nem sobrescrever embeds gerados pelo crawler.

## 3. Processamento de links

Renomear `GetOrCreatePreview` para `GetOrCreateEmbed`, e atualizar as operações de storage e services para usar `embeds` e `message_embeds`. O fluxo permanece assíncrono: a mensagem é persistida e respondida imediatamente; o crawler processa os links em background, reutiliza o cache e vincula os embeds encontrados à mensagem. Ao editar o conteúdo, substituir os vínculos antigos pelos novos.

Expandir o parser OpenGraph para coletar `og:title`, `og:description`, `og:url`, `og:site_name`, `og:image` e suas dimensões, além de `og:video`, `og:video:secure_url`, `og:video:type`, `og:video:width` e `og:video:height`. Usar a URL segura quando disponível. Normalizar e validar toda URL de página e mídia, continuar respeitando SSRF, `robots.txt`, limites de tamanho, timeout, concorrência e rate limit. `og:video` direto deve ser aceito apenas para HTTPS e MIME types de vídeo permitidos, servido pelo relay autenticado com suporte a `Range`; embeds em iframe só podem ser renderizados para provedores explicitamente allowlistados. Nunca renderizar HTML arbitrário recebido do site.

## 4. Eventos WebSocket

Substituir `new_preview`, `remove_preview` e `link_preview_update` por `message_embeds_update`, contendo `message_id` e a lista atual de embeds da mensagem. Emitir o evento após processamento, edição, remoção ou atualização por expiração de cache. A lista enviada pelo WS contém metadados; carregar mídia detalhada sob demanda, sem transmitir imagens em base64 no evento.

## 5. Limites de validação

Aplicar os limites no backend para embeds customizados e também limitar/truncar metadados externos antes de persistir:

| Campo | Limite |
|---|---:|
| Embeds por mensagem (total, automáticos + customizados) | 10 |
| Título | 256 caracteres |
| Descrição | 4.192 caracteres |
| Fields por embed | 25 |
| Nome de field | 256 caracteres |
| Valor de field | 1.024 caracteres |
| Nome do autor | 256 caracteres |
| Texto do footer | 2.048 caracteres |
| Soma do texto de todos os embeds da mensagem | 6.000 caracteres |
| URL de link/mídia | 2.048 caracteres |

Validar a soma total antes de gravar; não basta validar campo a campo. Contar caracteres Unicode de forma consistente entre Go e TypeScript. Rejeitar cores fora do formato hexadecimal `#RRGGBB`, URLs com esquemas não permitidos e mídia com MIME type não suportado. Manter `LINK_PREVIEW_MAX_URLS` como limite próprio do crawler por mensagem.

## 6. Testes e conclusão

Adicionar testes de cache por URL, autorização de `GET /embeds/:id`, limites de campos e total, edição/substituição de embeds, eventos WebSocket, parsing de OpenGraph e `og:video` (URL segura, MIME inválido, redirect inseguro e `Range`). Atualizar testes de API, store e renderização para `embeds` e executar a suíte Go e os testes frontend antes de remover os nomes antigos.
