package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"papo/internal/models"
)

// embedColumns lista as colunas de embeds (prefixo alias) com mime_type e
// size_bytes de cada mídia (join com a tabela media): o embed só guarda a
// referência content-addressable da mídia.
func embedColumns(alias string) string {
	return alias + ".id, " +
		alias + ".source_type, " +
		alias + ".fetch_method, " +
		alias + ".provider, " +
		alias + ".site_name, " +
		alias + ".url, " +
		alias + ".title, " +
		alias + ".description, " +
		alias + ".color, " +
		alias + ".author_name, " +
		alias + ".author_url, " +
		alias + ".author_media, am.mime_type, am.size_bytes, " +
		alias + ".thumbnail_media, " +
		alias + ".thumbnail_width, " +
		alias + ".thumbnail_height, tm.mime_type, tm.size_bytes, " +
		alias + ".image_media, " +
		alias + ".image_width, " +
		alias + ".image_height, im.mime_type, im.size_bytes, " +
		alias + ".video_url, " +
		alias + ".video_type, " +
		alias + ".video_width, " +
		alias + ".video_height, " +
		alias + ".embed_url, " +
		alias + ".footer_text, " +
		alias + ".footer_icon, fm.mime_type, fm.size_bytes, " +
		alias + ".cache_key, " +
		alias + ".created_at, " +
		alias + ".fetched_at"
}

// embedMediaJoins faz o join com a tabela media (LEFT: embed sem mídia é
// válido). Os aliases am/tm/im/fm são fixos e casam com embedColumns.
func embedMediaJoins(alias string) string {
	return " LEFT JOIN media am ON am.sha_hash = " + alias + ".author_media" +
		" LEFT JOIN media tm ON tm.sha_hash = " + alias + ".thumbnail_media" +
		" LEFT JOIN media im ON im.sha_hash = " + alias + ".image_media" +
		" LEFT JOIN media fm ON fm.sha_hash = " + alias + ".footer_icon"
}

// embedRow é a forma plana de uma row de embeds. embedFromRow monta o
// models.Embed (autor/thumbnail/image/video/footer compostos) a partir dela.
type embedRow struct {
	ID          string
	SourceType  string
	FetchMethod string
	Provider    *string
	SiteName    *string
	URL         *string
	Title       *string
	Description *string
	Color       *string

	AuthorName      *string
	AuthorURL       *string
	AuthorMediaHash *string
	AuthorMimeType  *string
	AuthorSizeBytes *int64

	ThumbnailMediaHash *string
	ThumbnailWidth     *int
	ThumbnailHeight    *int
	ThumbnailMimeType  *string
	ThumbnailSizeBytes *int64

	ImageMediaHash *string
	ImageWidth     *int
	ImageHeight    *int
	ImageMimeType  *string
	ImageSizeBytes *int64

	VideoURL    *string
	VideoType   *string
	VideoWidth  *int
	VideoHeight *int

	EmbedURL *string

	FooterText      *string
	FooterIcon      *string
	FooterMimeType  *string
	FooterSizeBytes *int64

	CacheKey  *string
	CreatedAt time.Time
	FetchedAt sql.NullTime
}

// scanEmbedRowTargets retorna os alvos de Scan na mesma ordem de
// embedColumns.
func scanEmbedRowTargets(r *embedRow) []any {
	return []any{
		&r.ID,
		&r.SourceType,
		&r.FetchMethod,
		&r.Provider,
		&r.SiteName,
		&r.URL,
		&r.Title,
		&r.Description,
		&r.Color,
		&r.AuthorName,
		&r.AuthorURL,
		&r.AuthorMediaHash,
		&r.AuthorMimeType,
		&r.AuthorSizeBytes,
		&r.ThumbnailMediaHash,
		&r.ThumbnailWidth,
		&r.ThumbnailHeight,
		&r.ThumbnailMimeType,
		&r.ThumbnailSizeBytes,
		&r.ImageMediaHash,
		&r.ImageWidth,
		&r.ImageHeight,
		&r.ImageMimeType,
		&r.ImageSizeBytes,
		&r.VideoURL,
		&r.VideoType,
		&r.VideoWidth,
		&r.VideoHeight,
		&r.EmbedURL,
		&r.FooterText,
		&r.FooterIcon,
		&r.FooterMimeType,
		&r.FooterSizeBytes,
		&r.CacheKey,
		&r.CreatedAt,
		&r.FetchedAt,
	}
}

func scanEmbedRow(row rowScanner) (embedRow, error) {
	var r embedRow
	if err := row.Scan(scanEmbedRowTargets(&r)...); err != nil {
		return embedRow{}, err
	}
	return r, nil
}

// embedMedia monta o objeto de mídia a partir da referência e dos metadados do
// join. Retorna nil quando não há mídia.
func embedMedia(hash *string, mimeType *string, sizeBytes *int64, width *int, height *int) *models.EmbedMedia {
	if hash == nil && mimeType == nil && sizeBytes == nil && width == nil && height == nil {
		return nil
	}
	return &models.EmbedMedia{
		MimeType:  mimeType,
		Width:     width,
		Height:    height,
		SizeBytes: sizeBytes,
		MediaSHA:  hash,
	}
}

// embedFromRow converte a row plana em models.Embed.
func embedFromRow(r embedRow) models.Embed {
	embed := models.Embed{
		ID:          r.ID,
		SourceType:  r.SourceType,
		FetchMethod: r.FetchMethod,
		Provider:    r.Provider,
		SiteName:    r.SiteName,
		URL:         r.URL,
		Title:       r.Title,
		Description: r.Description,
		Color:       r.Color,
		EmbedURL:    r.EmbedURL,
		CacheKey:    r.CacheKey,
	}

	if author := embedAuthorFromRow(r); author != nil {
		embed.Author = author
	}
	if thumbnail := embedMedia(r.ThumbnailMediaHash, r.ThumbnailMimeType, r.ThumbnailSizeBytes, r.ThumbnailWidth, r.ThumbnailHeight); thumbnail != nil {
		embed.Thumbnail = thumbnail
	}
	if image := embedMedia(r.ImageMediaHash, r.ImageMimeType, r.ImageSizeBytes, r.ImageWidth, r.ImageHeight); image != nil {
		embed.Image = image
	}
	if video := embedVideoFromRow(r); video != nil {
		embed.Video = video
	}
	if footer := embedFooterFromRow(r); footer != nil {
		embed.Footer = footer
	}

	embed.CreatedAt = r.CreatedAt
	if r.FetchedAt.Valid {
		embed.FetchedAt = &r.FetchedAt.Time
	}

	return embed
}

func embedAuthorFromRow(r embedRow) *models.EmbedAuthor {
	media := embedMedia(r.AuthorMediaHash, r.AuthorMimeType, r.AuthorSizeBytes, nil, nil)
	if r.AuthorName == nil && r.AuthorURL == nil && media == nil {
		return nil
	}
	return &models.EmbedAuthor{Name: r.AuthorName, URL: r.AuthorURL, Media: media}
}

func embedVideoFromRow(r embedRow) *models.EmbedMedia {
	if r.VideoURL == nil && r.VideoType == nil && r.VideoWidth == nil && r.VideoHeight == nil {
		return nil
	}
	return &models.EmbedMedia{
		URL:      r.VideoURL,
		MimeType: r.VideoType,
		Width:    r.VideoWidth,
		Height:   r.VideoHeight,
	}
}

func embedFooterFromRow(r embedRow) *models.EmbedFooter {
	icon := embedMedia(r.FooterIcon, r.FooterMimeType, r.FooterSizeBytes, nil, nil)
	if r.FooterText == nil && icon == nil {
		return nil
	}
	return &models.EmbedFooter{Text: r.FooterText, Icon: icon}
}

// embedInsertColumns/Values: INSERT comum a link embeds (upsert por cache_key)
// e embeds customizados.
const embedInsertColumns = `(source_type, fetch_method, cache_key, url, provider, site_name,
    title, description, color, author_name, author_url, author_media,
    thumbnail_media, thumbnail_width, thumbnail_height,
    image_media, image_width, image_height,
    video_url, video_type, video_width, video_height,
    embed_url, footer_text, footer_icon, fetched_at)`

// embedInsertValues monta a lista de valores do INSERT. O último valor é
// fetched_at: $26 para embed customizado (NULL — não foi obtido por fetch) e
// COALESCE($26, NOW()) para link embed, que é sempre resultado de um fetch
// (o DEFAULT da coluna não se aplica quando ela é listada explicitamente).
func embedInsertValues(fetchedAtValue string) string {
	return `($1, $2, $3, $4, $5, $6,
     $7, $8, $9, $10, $11, $12,
     $13, $14, $15,
     $16, $17, $18,
     $19, $20, $21, $22,
     $23, $24, $25, ` + fetchedAtValue + `)`
}

// embedInsertArgs ordena os argumentos do INSERT na mesma ordem de
// embedInsertColumns. Mídia ausente vira NULL; fetched_at é NULL quando o
// embed não foi obtido por fetch (embed customizado).
func embedInsertArgs(e models.Embed) []any {
	var fetchedAt any
	if e.FetchedAt != nil {
		fetchedAt = *e.FetchedAt
	}

	var authorName, authorURL, authorHash any
	if e.Author != nil {
		authorName, authorURL, authorHash = e.Author.Name, e.Author.URL, mediaHashArg(e.Author.Media)
	}

	var footerText, footerIcon any
	if e.Footer != nil {
		footerText, footerIcon = e.Footer.Text, mediaHashArg(e.Footer.Icon)
	}

	var videoURL, videoType, videoWidth, videoHeight any
	if e.Video != nil {
		videoURL, videoType, videoWidth, videoHeight = e.Video.URL, e.Video.MimeType, e.Video.Width, e.Video.Height
	}

	return []any{
		e.SourceType, e.FetchMethod, e.CacheKey, e.URL, e.Provider, e.SiteName,
		e.Title, e.Description, e.Color, authorName, authorURL, authorHash,
		mediaHashArg(e.Thumbnail), mediaWidthArg(e.Thumbnail), mediaHeightArg(e.Thumbnail),
		mediaHashArg(e.Image), mediaWidthArg(e.Image), mediaHeightArg(e.Image),
		videoURL, videoType, videoWidth, videoHeight,
		e.EmbedURL, footerText, footerIcon, fetchedAt,
	}
}

// mediaHashArg/mediaWidthArg/mediaHeightArg convertem a mídia em argumentos do
// INSERT (nil quando a mídia ou o campo são nulos).
func mediaHashArg(m *models.EmbedMedia) any {
	if m == nil {
		return nil
	}
	return m.MediaSHA
}

func mediaWidthArg(m *models.EmbedMedia) any {
	if m == nil {
		return nil
	}
	return m.Width
}

func mediaHeightArg(m *models.EmbedMedia) any {
	if m == nil {
		return nil
	}
	return m.Height
}

// GetEmbedByCacheKey busca o link embed em cache pela URL normalizada. Retorna
// ErrNotFound quando não existe.
func GetEmbedByCacheKey(ctx context.Context, cacheKey string) (models.Embed, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+embedColumns("e")+" FROM embeds e"+embedMediaJoins("e")+" WHERE e.cache_key = $1",
		cacheKey,
	)

	r, err := scanEmbedRow(row)
	if err != nil {
		return models.Embed{}, mapStorageError(err)
	}
	return embedFromRow(r), nil
}

// UpsertLinkEmbed insere o link embed ou atualiza o registro existente para a
// mesma cache_key (cache expirado → refetch). fetched_at é atualizado para
// NOW(). Retorna o registro gravado.
//
// O SELECT lê o resultado do RETURNING (não a tabela embeds): a query principal
// e o CTE de dados compartilham o mesmo snapshot, então a linha
// inserida/atualizada ainda não seria visível na tabela.
func UpsertLinkEmbed(ctx context.Context, e models.Embed) (models.Embed, error) {
	args := embedInsertArgs(e)

	row := GetDB().QueryRowContext(ctx,
		`WITH upserted AS (
		 INSERT INTO embeds `+embedInsertColumns+`
		 VALUES `+embedInsertValues("COALESCE($26, NOW())")+`
		 ON CONFLICT (cache_key) WHERE cache_key IS NOT NULL DO UPDATE SET
		    source_type = EXCLUDED.source_type,
		    fetch_method = EXCLUDED.fetch_method,
		    url = EXCLUDED.url,
		    provider = EXCLUDED.provider,
		    site_name = EXCLUDED.site_name,
		    title = EXCLUDED.title,
		    description = EXCLUDED.description,
		    color = EXCLUDED.color,
		    author_name = EXCLUDED.author_name,
		    author_url = EXCLUDED.author_url,
		    author_media = EXCLUDED.author_media,
		    thumbnail_media = EXCLUDED.thumbnail_media,
		    thumbnail_width = EXCLUDED.thumbnail_width,
		    thumbnail_height = EXCLUDED.thumbnail_height,
		    image_media = EXCLUDED.image_media,
		    image_width = EXCLUDED.image_width,
		    image_height = EXCLUDED.image_height,
		    video_url = EXCLUDED.video_url,
		    video_type = EXCLUDED.video_type,
		    video_width = EXCLUDED.video_width,
		    video_height = EXCLUDED.video_height,
		    embed_url = EXCLUDED.embed_url,
		    footer_text = EXCLUDED.footer_text,
		    footer_icon = EXCLUDED.footer_icon,
		    fetched_at = NOW()
		 RETURNING id, source_type, fetch_method, provider, site_name, url, title, description, color,
		    author_name, author_url, author_media,
		    thumbnail_media, thumbnail_width, thumbnail_height,
		    image_media, image_width, image_height,
		    video_url, video_type, video_width, video_height,
		    embed_url, footer_text, footer_icon, cache_key, created_at, fetched_at
		 )
		 SELECT u.id, u.source_type, u.fetch_method, u.provider, u.site_name, u.url, u.title, u.description, u.color,
		        u.author_name, u.author_url, u.author_media, am.mime_type, am.size_bytes,
		        u.thumbnail_media, u.thumbnail_width, u.thumbnail_height, tm.mime_type, tm.size_bytes,
		        u.image_media, u.image_width, u.image_height, im.mime_type, im.size_bytes,
		        u.video_url, u.video_type, u.video_width, u.video_height,
		        u.embed_url, u.footer_text, u.footer_icon, fm.mime_type, fm.size_bytes,
		        u.cache_key, u.created_at, u.fetched_at
		 FROM upserted u
		 LEFT JOIN media am ON am.sha_hash = u.author_media
		 LEFT JOIN media tm ON tm.sha_hash = u.thumbnail_media
		 LEFT JOIN media im ON im.sha_hash = u.image_media
		 LEFT JOIN media fm ON fm.sha_hash = u.footer_icon`,
		args...,
	)

	r, err := scanEmbedRow(row)
	if err != nil {
		return models.Embed{}, mapStorageError(err)
	}
	return embedFromRow(r), nil
}

// CreateCustomEmbed grava um embed customizado (source_type='custom',
// cache_key NULL — cada embed customizado tem seu próprio registro) com suas
// fields. Retorna o registro gravado.
func CreateCustomEmbed(ctx context.Context, e models.Embed, fields []models.EmbedField) (models.Embed, error) {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return models.Embed{}, fmt.Errorf("falha ao criar embed customizado: %w", err)
	}
	defer tx.Rollback()

	args := embedInsertArgs(e)
	row := tx.QueryRowContext(ctx,
		"INSERT INTO embeds "+embedInsertColumns+" VALUES "+embedInsertValues("$26")+" RETURNING id",
		args...,
	)
	var embedID string
	if err := row.Scan(&embedID); err != nil {
		return models.Embed{}, mapStorageError(err)
	}

	if err := insertEmbedFields(ctx, tx, embedID, fields); err != nil {
		return models.Embed{}, err
	}

	if err := tx.Commit(); err != nil {
		return models.Embed{}, fmt.Errorf("falha ao criar embed customizado: %w", err)
	}

	return GetEmbedByID(ctx, embedID)
}

// insertEmbedFields grava as fields do embed na ordem recebida (position é
// derivado da posição no slice, o cliente não controla a ordem).
func insertEmbedFields(ctx context.Context, tx *sql.Tx, embedID string, fields []models.EmbedField) error {
	for position, field := range fields {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO embed_fields (embed_id, position, name, value, inline)
			 VALUES ($1, $2, $3, $4, $5)`,
			embedID, position, field.Name, field.Value, field.Inline,
		); err != nil {
			return fmt.Errorf("falha ao gravar as fields do embed: %w", err)
		}
	}
	return nil
}

// GetEmbedByID busca um embed pelo id, com suas fields. Retorna ErrNotFound
// quando não existe.
func GetEmbedByID(ctx context.Context, id string) (models.Embed, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+embedColumns("e")+" FROM embeds e"+embedMediaJoins("e")+" WHERE e.id = $1",
		id,
	)

	r, err := scanEmbedRow(row)
	if err != nil {
		return models.Embed{}, mapStorageError(err)
	}

	embed := embedFromRow(r)
	fields, err := ListEmbedFieldsByEmbedIDs(ctx, []string{embed.ID})
	if err != nil {
		return models.Embed{}, err
	}
	embed.Fields = fields[embed.ID]

	return embed, nil
}

// ListEmbedFieldsByEmbedIDs busca as fields de vários embeds em uma única
// query, ordenadas por (embed_id, position).
func ListEmbedFieldsByEmbedIDs(ctx context.Context, embedIDs []string) (map[string][]models.EmbedField, error) {
	fieldsByEmbed := make(map[string][]models.EmbedField, len(embedIDs))
	if len(embedIDs) == 0 {
		return fieldsByEmbed, nil
	}

	rows, err := GetDB().QueryContext(ctx,
		`SELECT embed_id, position, name, value, inline
		 FROM embed_fields
		 WHERE embed_id = ANY($1)
		 ORDER BY embed_id, position`,
		embedIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar as fields dos embeds: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			embedID string
			field   models.EmbedField
		)
		if err := rows.Scan(&embedID, &field.Position, &field.Name, &field.Value, &field.Inline); err != nil {
			return nil, fmt.Errorf("falha ao ler a field do embed: %w", err)
		}
		fieldsByEmbed[embedID] = append(fieldsByEmbed[embedID], field)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar as fields dos embeds: %w", err)
	}

	return fieldsByEmbed, nil
}

// GetChannelIDByEmbedID resolve o channel_id de um embed via message_embeds →
// messages. Retorna ErrNotFound quando o embed não está vinculado a nenhuma
// mensagem. Usado na autorização do endpoint do embed.
func GetChannelIDByEmbedID(ctx context.Context, embedID string) (string, error) {
	var channelID string
	err := GetDB().QueryRowContext(ctx,
		`SELECT m.channel_id
		 FROM embeds e
		 JOIN message_embeds me ON me.embed_id = e.id
		 JOIN messages m ON m.id = me.message_id
		 WHERE e.id = $1
		 LIMIT 1`,
		embedID,
	).Scan(&channelID)
	if err != nil {
		return "", mapStorageError(err)
	}

	return channelID, nil
}

// ListMessageRefsByEmbedID retorna as mensagens atualmente vinculadas ao embed
// (message_embeds → messages) com o channel_id de cada uma (alvos do evento
// message_embeds_update). Slice vazio quando o embed não está vinculado a
// nenhuma mensagem.
func ListMessageRefsByEmbedID(ctx context.Context, embedID string) ([]models.EmbedMessageRef, error) {
	rows, err := GetDB().QueryContext(ctx,
		`SELECT me.message_id, m.channel_id
		 FROM message_embeds me
		 JOIN messages m ON m.id = me.message_id
		 WHERE me.embed_id = $1`,
		embedID,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar as mensagens do embed: %w", err)
	}
	defer rows.Close()

	var refs []models.EmbedMessageRef
	for rows.Next() {
		var ref models.EmbedMessageRef
		if err := rows.Scan(&ref.MessageID, &ref.ChannelID); err != nil {
			return nil, fmt.Errorf("falha ao ler as mensagens do embed: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar as mensagens do embed: %w", err)
	}

	return refs, nil
}

// AddMessageEmbeds vincula embeds a uma mensagem. Ids duplicados são ignorados
// (ON CONFLICT DO NOTHING).
func AddMessageEmbeds(ctx context.Context, messageID string, embedIDs []string) error {
	if len(embedIDs) == 0 {
		return nil
	}

	_, err := GetDB().ExecContext(ctx,
		"INSERT INTO message_embeds (message_id, embed_id) SELECT $1, unnest($2::uuid[]) ON CONFLICT (message_id, embed_id) DO NOTHING",
		messageID, embedIDs,
	)
	if err != nil {
		return fmt.Errorf("falha ao vincular embeds à mensagem: %w", err)
	}

	return nil
}

// ReplaceMessageLinkEmbeds substitui somente os vínculos de link embeds
// (source_type='link') de uma mensagem — DELETE + INSERT na mesma transação.
// Os vínculos de embeds customizados são preservados: o crawler não pode
// sobrescrever um embed criado pelo cliente.
func ReplaceMessageLinkEmbeds(ctx context.Context, messageID string, embedIDs []string) error {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("falha ao substituir os embeds de link da mensagem: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM message_embeds me
		 USING embeds e
		 WHERE me.message_id = $1 AND e.id = me.embed_id AND e.source_type = 'link'`,
		messageID,
	); err != nil {
		return fmt.Errorf("falha ao substituir os embeds de link da mensagem: %w", err)
	}

	if len(embedIDs) > 0 {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO message_embeds (message_id, embed_id) SELECT $1, unnest($2::uuid[]) ON CONFLICT (message_id, embed_id) DO NOTHING",
			messageID, embedIDs,
		); err != nil {
			return fmt.Errorf("falha ao substituir os embeds de link da mensagem: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("falha ao substituir os embeds de link da mensagem: %w", err)
	}

	return nil
}

// ReplaceMessageCustomEmbeds substitui os embeds customizados de uma mensagem:
// remove os vínculos e as rows dos embeds customizados antigos (cada embed
// customizado é dono do próprio registro) e vincula os novos. DELETE + INSERT
// na mesma transação.
func ReplaceMessageCustomEmbeds(ctx context.Context, messageID string, newEmbedIDs, oldEmbedIDs []string) error {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("falha ao substituir os embeds customizados da mensagem: %w", err)
	}
	defer tx.Rollback()

	if len(oldEmbedIDs) > 0 {
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM message_embeds WHERE message_id = $1 AND embed_id = ANY($2::uuid[])",
			messageID, oldEmbedIDs,
		); err != nil {
			return fmt.Errorf("falha ao substituir os embeds customizados da mensagem: %w", err)
		}
		// source_type = 'custom' protege um link embed compartilhado por engano.
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM embeds WHERE id = ANY($1::uuid[]) AND source_type = 'custom'",
			oldEmbedIDs,
		); err != nil {
			return fmt.Errorf("falha ao remover os embeds customizados antigos: %w", err)
		}
	}

	if len(newEmbedIDs) > 0 {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO message_embeds (message_id, embed_id) SELECT $1, unnest($2::uuid[]) ON CONFLICT (message_id, embed_id) DO NOTHING",
			messageID, newEmbedIDs,
		); err != nil {
			return fmt.Errorf("falha ao substituir os embeds customizados da mensagem: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("falha ao substituir os embeds customizados da mensagem: %w", err)
	}

	return nil
}

// ListEmbedsByMessageIDs busca os embeds de várias mensagens em duas queries
// (embeds + fields), evitando N+1 na listagem. O mapa é indexado por
// message_id; mensagens sem embed não aparecem.
func ListEmbedsByMessageIDs(ctx context.Context, messageIDs []string) (map[string][]models.Embed, error) {
	embedsByMessage := make(map[string][]models.Embed, len(messageIDs))
	if len(messageIDs) == 0 {
		return embedsByMessage, nil
	}

	rows, err := GetDB().QueryContext(ctx,
		`SELECT me.message_id, `+embedColumns("e")+`
		 FROM message_embeds me
		 JOIN embeds e ON e.id = me.embed_id
		 `+embedMediaJoins("e")+`
		 WHERE me.message_id = ANY($1)
		 ORDER BY me.message_id, e.created_at, e.id`,
		messageIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar embeds: %w", err)
	}
	defer rows.Close()

	embedIDs := make([]string, 0)
	for rows.Next() {
		var (
			messageID string
			r         embedRow
		)
		if err := rows.Scan(append([]any{&messageID}, scanEmbedRowTargets(&r)...)...); err != nil {
			return nil, fmt.Errorf("falha ao ler embed: %w", err)
		}
		embed := embedFromRow(r)
		embedIDs = append(embedIDs, embed.ID)
		embedsByMessage[messageID] = append(embedsByMessage[messageID], embed)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar embeds: %w", err)
	}

	fields, err := ListEmbedFieldsByEmbedIDs(ctx, embedIDs)
	if err != nil {
		return nil, err
	}
	for messageID := range embedsByMessage {
		for i := range embedsByMessage[messageID] {
			embedsByMessage[messageID][i].Fields = fields[embedsByMessage[messageID][i].ID]
		}
	}

	return embedsByMessage, nil
}
