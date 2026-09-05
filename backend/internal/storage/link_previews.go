package storage

import (
	"context"
	"fmt"

	"papo/internal/models"
)

// linkPreviewColumns inclui image_mime_type e image_size_bytes da tabela
// media (join): o preview só guarda a referência content-addressable da
// thumbnail.
const linkPreviewColumns = "lp.id, lp.url, lp.kind, lp.title, lp.description, lp.provider_name, lp.embed_url, lp.image_media, m.mime_type AS image_mime_type, m.size_bytes AS image_size_bytes, lp.fetched_at"

// linkPreviewMediaJoin faz o join com a tabela media (LEFT: preview sem
// imagem é válido).
const linkPreviewMediaJoin = "LEFT JOIN media m ON m.sha_hash = lp.image_media"

func scanLinkPreview(row rowScanner) (models.LinkPreview, error) {
	var preview models.LinkPreview
	err := row.Scan(
		&preview.ID,
		&preview.URL,
		&preview.Kind,
		&preview.Title,
		&preview.Description,
		&preview.ProviderName,
		&preview.EmbedURL,
		&preview.ImageMedia,
		&preview.ImageMimeType,
		&preview.ImageSizeBytes,
		&preview.FetchedAt,
	)
	if err != nil {
		return models.LinkPreview{}, err
	}

	return preview, nil
}

// GetPreviewByURL busca o preview em cache pela URL normalizada. Retorna
// ErrNotFound quando não existe.
func GetPreviewByURL(ctx context.Context, url string) (models.LinkPreview, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+linkPreviewColumns+" FROM link_previews lp "+linkPreviewMediaJoin+" WHERE lp.url = $1",
		url,
	)

	preview, err := scanLinkPreview(row)
	if err != nil {
		return models.LinkPreview{}, mapStorageError(err)
	}

	return preview, nil
}

// UpsertPreview insere o preview ou atualiza o registro existente para a
// mesma URL normalizada (cache expirado → refetch). O fetched_at é
// atualizado para NOW(). Retorna o registro gravado.
func UpsertPreview(ctx context.Context, p models.LinkPreview) (models.LinkPreview, error) {
	// O SELECT lê o resultado do RETURNING (não a tabela link_previews): a
	// query principal e o CTE de dados compartilham o mesmo snapshot, então
	// a linha inserida/atualizada ainda não seria visível na tabela.
	row := GetDB().QueryRowContext(ctx,
		`WITH upserted AS (
		 INSERT INTO link_previews (url, kind, title, description, provider_name, embed_url, image_media)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (url) DO UPDATE SET
		    kind = EXCLUDED.kind,
		    title = EXCLUDED.title,
		    description = EXCLUDED.description,
		    provider_name = EXCLUDED.provider_name,
		    embed_url = EXCLUDED.embed_url,
		    image_media = EXCLUDED.image_media,
		    fetched_at = NOW()
		 RETURNING id, url, kind, title, description, provider_name, embed_url, image_media, fetched_at
		 )
		 SELECT u.id, u.url, u.kind, u.title, u.description, u.provider_name, u.embed_url, u.image_media,
		        m.mime_type AS image_mime_type, m.size_bytes AS image_size_bytes, u.fetched_at
		 FROM upserted u LEFT JOIN media m ON m.sha_hash = u.image_media`,
		p.URL, p.Kind, p.Title, p.Description, p.ProviderName, p.EmbedURL,
		p.ImageMedia,
	)

	preview, err := scanLinkPreview(row)
	if err != nil {
		return models.LinkPreview{}, mapStorageError(err)
	}

	return preview, nil
}

// GetPreviewByID busca um preview pelo id. Retorna ErrNotFound quando não
// existe.
func GetPreviewByID(ctx context.Context, id string) (models.LinkPreview, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+linkPreviewColumns+" FROM link_previews lp "+linkPreviewMediaJoin+" WHERE lp.id = $1",
		id,
	)

	preview, err := scanLinkPreview(row)
	if err != nil {
		return models.LinkPreview{}, mapStorageError(err)
	}

	return preview, nil
}

// GetChannelIDByPreviewID resolve o channel_id de um preview via
// message_previews → messages. Retorna ErrNotFound quando o preview não está
// vinculado a nenhuma mensagem. Usado na autorização do endpoint de imagem
// do preview.
func GetChannelIDByPreviewID(ctx context.Context, previewID string) (string, error) {
	var channelID string
	err := GetDB().QueryRowContext(ctx,
		`SELECT m.channel_id
		 FROM link_previews lp
		 JOIN message_previews mp ON mp.preview_id = lp.id
		 JOIN messages m ON m.id = mp.message_id
		 WHERE lp.id = $1
		 LIMIT 1`,
		previewID,
	).Scan(&channelID)
	if err != nil {
		return "", mapStorageError(err)
	}

	return channelID, nil
}

// ListMessageRefsByPreviewID retorna as mensagens atualmente vinculadas ao
// preview (message_previews → messages) com o channel_id de cada uma (alvos
// do evento link_preview_update). Slice vazio quando o preview não está
// vinculado a nenhuma mensagem.
func ListMessageRefsByPreviewID(ctx context.Context, previewID string) ([]models.PreviewMessageRef, error) {
	rows, err := GetDB().QueryContext(ctx,
		`SELECT mp.message_id, m.channel_id
		 FROM message_previews mp
		 JOIN messages m ON m.id = mp.message_id
		 WHERE mp.preview_id = $1`,
		previewID,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar as mensagens do preview: %w", err)
	}
	defer rows.Close()

	var refs []models.PreviewMessageRef
	for rows.Next() {
		var ref models.PreviewMessageRef
		if err := rows.Scan(&ref.MessageID, &ref.ChannelID); err != nil {
			return nil, fmt.Errorf("falha ao ler as mensagens do preview: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar as mensagens do preview: %w", err)
	}

	return refs, nil
}

// AddMessagePreviews vincula previews a uma mensagem nova. Ids duplicados
// são ignorados (ON CONFLICT DO NOTHING).
func AddMessagePreviews(ctx context.Context, messageID string, previewIDs []string) error {
	if len(previewIDs) == 0 {
		return nil
	}

	_, err := GetDB().ExecContext(ctx,
		"INSERT INTO message_previews (message_id, preview_id) SELECT $1, unnest($2::uuid[]) ON CONFLICT (message_id, preview_id) DO NOTHING",
		messageID, previewIDs,
	)
	if err != nil {
		return fmt.Errorf("falha ao vincular previews à mensagem: %w", err)
	}

	return nil
}

// ReplaceMessagePreviews substitui todos os previews de uma mensagem (fluxo
// de edição: o conteúdo novo pode gerar previews diferentes). DELETE +
// INSERT na mesma transação.
func ReplaceMessagePreviews(ctx context.Context, messageID string, previewIDs []string) error {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("falha ao substituir previews da mensagem: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		"DELETE FROM message_previews WHERE message_id = $1",
		messageID,
	); err != nil {
		return fmt.Errorf("falha ao substituir previews da mensagem: %w", err)
	}

	if len(previewIDs) > 0 {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO message_previews (message_id, preview_id) SELECT $1, unnest($2::uuid[]) ON CONFLICT (message_id, preview_id) DO NOTHING",
			messageID, previewIDs,
		); err != nil {
			return fmt.Errorf("falha ao substituir previews da mensagem: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("falha ao substituir previews da mensagem: %w", err)
	}

	return nil
}

// ListPreviewsByMessageIDs busca os previews de várias mensagens em uma
// única query (evita N+1 na listagem). O mapa é indexado por message_id;
// mensagens sem preview não aparecem.
func ListPreviewsByMessageIDs(ctx context.Context, messageIDs []string) (map[string][]models.LinkPreview, error) {
	previewsByMessage := make(map[string][]models.LinkPreview, len(messageIDs))
	if len(messageIDs) == 0 {
		return previewsByMessage, nil
	}

	rows, err := GetDB().QueryContext(ctx,
		`SELECT mp.message_id, `+linkPreviewColumns+`
		 FROM message_previews mp
		 JOIN link_previews lp ON lp.id = mp.preview_id
		 `+linkPreviewMediaJoin+`
		 WHERE mp.message_id = ANY($1)
		 ORDER BY mp.message_id, lp.fetched_at, lp.id`,
		messageIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar previews: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			messageID string
			preview   models.LinkPreview
		)
		err := rows.Scan(
			&messageID,
			&preview.ID,
			&preview.URL,
			&preview.Kind,
			&preview.Title,
			&preview.Description,
			&preview.ProviderName,
			&preview.EmbedURL,
			&preview.ImageMedia,
			&preview.ImageMimeType,
			&preview.ImageSizeBytes,
			&preview.FetchedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("falha ao ler preview: %w", err)
		}
		previewsByMessage[messageID] = append(previewsByMessage[messageID], preview)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar previews: %w", err)
	}

	return previewsByMessage, nil
}
