package models

import "time"

// Embed representa a tabela embeds: o preview automático de um link
// (source_type='link', reutilizado pelo cache por URL normalizada em
// cache_key) e o embed personalizado enviado pela mensagem (source_type=
// 'custom', dono do próprio registro, cache_key nulo).
//
// FetchMethod: 'opengraph' (parser OpenGraph), 'oembed' (provedor
// allowlistado) ou 'manual' (embed customizado).
//
// Os campos de mídia guardam apenas a referência interna (EmbedMedia.MediaSHA
// → tabela media); os bytes são servidos por GET /embeds/:embed_id (imagem) e
// GET /embeds/:embed_id/video (relay de vídeo). Campos ausentes são omitidos.
type Embed struct {
	ID          string  `json:"id"`
	SourceType  string  `json:"source_type"`
	FetchMethod string  `json:"fetch_method"`
	Provider    *string `json:"provider,omitempty"`
	SiteName    *string `json:"site_name,omitempty"`
	URL         *string `json:"url,omitempty"`
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
	Color       *string `json:"color,omitempty"`

	Author    *EmbedAuthor `json:"author,omitempty"`
	Thumbnail *EmbedMedia  `json:"thumbnail,omitempty"`
	Image     *EmbedMedia  `json:"image,omitempty"`
	Video     *EmbedMedia  `json:"video,omitempty"`
	Footer    *EmbedFooter `json:"footer,omitempty"`
	Fields    []EmbedField `json:"fields,omitempty"`

	// EmbedURL é o embed de iframe derivado de padrão hardcoded do backend
	// (MVP: YouTube, https://www.youtube.com/embed/<id>). Nunca vem de HTML
	// recebido do site: o frontend só renderiza iframe para provedor
	// explicitamente allowlistado.
	EmbedURL *string `json:"embed_url,omitempty"`

	CreatedAt time.Time  `json:"created_at"`
	FetchedAt *time.Time `json:"fetched_at,omitempty"`

	// CacheKey é a URL normalizada que identifica o cache do link embed
	// (nulo em embed customizado). Não é exposto pela API.
	CacheKey *string `json:"-"`

	// ThumbnailFilePath é o caminho do blob da thumbnail em disco, resolvido
	// do hash pelo service (GET /embeds/:embed_id).
	ThumbnailFilePath *string `json:"-"`

	// AuthorFilePath é o caminho do blob do ícone do autor em disco, resolvido
	// do hash pelo service (GET /embeds/:embed_id).
	AuthorFilePath *string `json:"-"`
}

// EmbedAuthor é o autor declarado pelo site (oEmbed) ou pelo embed
// customizado.
type EmbedAuthor struct {
	Name  *string     `json:"name,omitempty"`
	URL   *string     `json:"url,omitempty"`
	Media *EmbedMedia `json:"media,omitempty"`
}

// EmbedMedia é uma mídia do embed: dimensões e MIME para renderização, com a
// referência content-addressable mantida internamente.
type EmbedMedia struct {
	URL       *string `json:"url,omitempty"`
	MimeType  *string `json:"mime_type,omitempty"`
	Width     *int    `json:"width,omitempty"`
	Height    *int    `json:"height,omitempty"`
	SizeBytes *int64  `json:"size_bytes,omitempty"`

	// MediaSHA é o sha_hash do blob na tabela media. A imagem nunca é servida
	// pelo hash: o cliente usa GET /embeds/:embed_id (leitura autorizada do
	// canal da mensagem).
	MediaSHA *string `json:"-"`
}

// EmbedFooter é o rodapé do embed (texto e ícone opcionais).
type EmbedFooter struct {
	Text *string     `json:"text,omitempty"`
	Icon *EmbedMedia `json:"icon,omitempty"`
}

// EmbedField é uma linha de nome/valor do embed (tabela embed_fields).
// Position é a ordem de exibição (0..N) e Inline marca as fields que podem
// ser renderizadas lado a lado.
type EmbedField struct {
	Position int    `json:"position"`
	Name     string `json:"name"`
	Value    string `json:"value"`
	Inline   bool   `json:"inline"`
}

// EmbedMessageRef identifica uma mensagem vinculada a um embed (e o canal da
// mensagem) — alvo do evento message_embeds_update quando o embed é refetchado
// pelo cache.
type EmbedMessageRef struct {
	MessageID string
	ChannelID string
}
