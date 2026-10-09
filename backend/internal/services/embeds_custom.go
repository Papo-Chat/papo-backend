package services

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"
)

// ErrInvalidEmbed indica um embed customizado inválido: limite de campo ou de
// soma ultrapassado, cor fora do formato, esquema de URL não permitido ou MIME
// type não suportado.
var ErrInvalidEmbed = errors.New("embed customizado inválido")

// embedColorRe aceita somente #RRGGBB.
var embedColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// EmbedInput é um embed customizado enviado na criação/edição de mensagem
// (campo JSON "embeds"). O cliente não controla source_type, fetch_method nem
// cache_key: o service fixa source_type='custom' e fetch_method='manual', e
// nunca sobrescreve um embed gerado pelo crawler.
type EmbedInput struct {
	Title       string            `json:"title"`
	Description string            `json:"description"`
	URL         string            `json:"url"`
	Color       string            `json:"color"`
	SiteName    string            `json:"site_name"`
	Author      *EmbedAuthorInput `json:"author"`
	Footer      *EmbedFooterInput `json:"footer"`
	Thumbnail   *EmbedMediaInput  `json:"thumbnail"`
	Image       *EmbedMediaInput  `json:"image"`
	Video       *EmbedMediaInput  `json:"video"`
	Fields      []EmbedFieldInput `json:"fields"`
}

// EmbedAuthorInput é o autor declarado do embed customizado. IconURL é a imagem
// do autor (HTTPS, MIME de imagem): o backend baixa e re-serva na tabela media,
// igual à thumbnail — a URL original nunca é exposta ao cliente.
type EmbedAuthorInput struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	IconURL string `json:"icon_url"`
}

// EmbedFooterInput é o rodapé do embed customizado.
type EmbedFooterInput struct {
	Text string `json:"text"`
}

// EmbedMediaInput é uma mídia do embed customizado informada por URL.
// Thumbnail/Image: imagem baixada e gravada na tabela media (o backend nunca
// serve a URL original do cliente). Video: URL HTTPS de vídeo direto, servida
// pelo relay autenticado (GET /embeds/:embed_id/video); mime_type é obrigatório
// e precisa estar no allowlist.
type EmbedMediaInput struct {
	URL      string `json:"url"`
	MimeType string `json:"mime_type"`
}

// EmbedFieldInput é uma field do embed customizado. A ordem de exibição é a
// ordem do array (position é derivado pelo backend).
type EmbedFieldInput struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

// validateCustomEmbeds aplica os limites de embed customizado (§5): limites por
// campo, MIME/cor/esquema de URL e a soma total do texto de todos os embeds da
// mensagem (validar campo a campo não basta).
//
// Contagem por runes (pontos de código Unicode) — a mesma contagem do frontend.
func validateCustomEmbeds(inputs []EmbedInput) error {
	if len(inputs) == 0 {
		return nil
	}
	if len(inputs) > maxEmbedsPerMessage {
		return fmt.Errorf("%w: máximo de %d embeds por mensagem", ErrInvalidEmbed, maxEmbedsPerMessage)
	}

	total := 0
	for i, in := range inputs {
		if err := validateCustomEmbed(in); err != nil {
			return fmt.Errorf("%w (embed %d): %v", ErrInvalidEmbed, i+1, err)
		}
		total += embedInputTextLength(in)
	}
	if total > embedTotalTextMax {
		return fmt.Errorf("%w: texto total dos embeds excede %d caracteres", ErrInvalidEmbed, embedTotalTextMax)
	}

	return nil
}

func validateCustomEmbed(in EmbedInput) error {
	if utf8.RuneCountInString(in.Title) > embedTitleMax {
		return fmt.Errorf("título excede %d caracteres", embedTitleMax)
	}
	if utf8.RuneCountInString(in.Description) > embedDescriptionMax {
		return fmt.Errorf("descrição excede %d caracteres", embedDescriptionMax)
	}
	if utf8.RuneCountInString(in.SiteName) > embedSiteNameMax {
		return fmt.Errorf("site_name excede %d caracteres", embedSiteNameMax)
	}
	if in.Color != "" && !embedColorRe.MatchString(in.Color) {
		return fmt.Errorf("cor deve usar o formato #RRGGBB")
	}
	if in.URL != "" {
		if _, err := validateEmbedURL(in.URL); err != nil {
			return err
		}
	}

	if in.Author != nil {
		if utf8.RuneCountInString(in.Author.Name) > embedAuthorNameMax {
			return fmt.Errorf("nome do autor excede %d caracteres", embedAuthorNameMax)
		}
		if in.Author.URL != "" {
			if _, err := validateEmbedURL(in.Author.URL); err != nil {
				return err
			}
		}
		if in.Author.IconURL != "" {
			if _, err := validateEmbedMediaURL(in.Author.IconURL, ""); err != nil {
				return err
			}
		}
	}

	if in.Footer != nil && utf8.RuneCountInString(in.Footer.Text) > embedFooterTextMax {
		return fmt.Errorf("texto do footer excede %d caracteres", embedFooterTextMax)
	}

	if in.Thumbnail != nil {
		if _, err := validateEmbedMediaURL(in.Thumbnail.URL, in.Thumbnail.MimeType); err != nil {
			return err
		}
	}
	if in.Image != nil {
		if _, err := validateEmbedMediaURL(in.Image.URL, in.Image.MimeType); err != nil {
			return err
		}
	}
	if in.Video != nil {
		if _, err := validateEmbedVideoInput(in.Video); err != nil {
			return err
		}
	}

	if len(in.Fields) > embedFieldsMax {
		return fmt.Errorf("máximo de %d fields por embed", embedFieldsMax)
	}
	for _, field := range in.Fields {
		if utf8.RuneCountInString(field.Name) > embedFieldNameMax {
			return fmt.Errorf("nome de field excede %d caracteres", embedFieldNameMax)
		}
		if utf8.RuneCountInString(field.Value) > embedFieldValueMax {
			return fmt.Errorf("valor de field excede %d caracteres", embedFieldValueMax)
		}
	}

	return nil
}

// validateEmbedURL valida e normaliza uma URL de página/autor (http ou https,
// sem userinfo, porta padrão, dentro do limite de tamanho).
func validateEmbedURL(raw string) (string, error) {
	if utf8.RuneCountInString(raw) > embedURLMax {
		return "", fmt.Errorf("URL excede %d caracteres", embedURLMax)
	}
	u, err := utils.NormalizeURL(raw)
	if err != nil {
		return "", fmt.Errorf("URL inválida ou com esquema não permitido")
	}
	return u.String(), nil
}

// validateEmbedMediaURL valida a URL de imagem de um embed customizado: HTTPS
// obrigatório (o backend baixa e re-serve a imagem; nunca http) e MIME type
// declarado dentro do allowlist de imagem, quando declarado.
func validateEmbedMediaURL(raw, mimeType string) (string, error) {
	if utf8.RuneCountInString(raw) > embedURLMax {
		return "", fmt.Errorf("URL de mídia excede %d caracteres", embedURLMax)
	}
	if mimeType != "" && !isProcessableImage(strings.ToLower(mimeType)) {
		return "", fmt.Errorf("MIME type de imagem não suportado")
	}
	u, err := utils.NormalizeURL(raw)
	if err != nil {
		return "", fmt.Errorf("URL de mídia inválida ou com esquema não permitido")
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("mídia de embed exige HTTPS")
	}
	return u.String(), nil
}

// validateEmbedVideoInput valida o vídeo direto de um embed customizado: HTTPS,
// MIME type obrigatório dentro do allowlist de vídeo.
func validateEmbedVideoInput(in *EmbedMediaInput) (string, error) {
	if utf8.RuneCountInString(in.URL) > embedURLMax {
		return "", fmt.Errorf("URL de vídeo excede %d caracteres", embedURLMax)
	}
	mimeType := strings.ToLower(strings.TrimSpace(strings.Split(in.MimeType, ";")[0]))
	if mimeType == "" {
		return "", fmt.Errorf("vídeo de embed exige mime_type")
	}
	if !allowedVideoMIMEs[mimeType] {
		return "", fmt.Errorf("MIME type de vídeo não suportado")
	}
	return normalizeEmbedVideoURL(in.URL)
}

// CreateCustomEmbeds valida e grava os embeds customizados de uma mensagem
// (source_type='custom', fetch_method='manual'), baixando as mídias informadas
// por URL com o client SSRF-safe. Best-effort na mídia: falha de download é
// logada e o embed é gravado sem ela (a validação de entrada já rejeitou MIME,
// esquema e tamanho inválidos).
func CreateCustomEmbeds(ctx context.Context, userID string, inputs []EmbedInput) ([]models.Embed, error) {
	if err := validateCustomEmbeds(inputs); err != nil {
		return nil, err
	}

	created := make([]models.Embed, 0, len(inputs))
	for _, in := range inputs {
		embed, fields := buildCustomEmbed(in)
		resolveCustomEmbedMedia(ctx, userID, &embed)

		stored, err := storage.CreateCustomEmbed(ctx, embed, fields)
		if err != nil {
			return nil, err
		}
		created = append(created, stored)
	}

	return created, nil
}

// buildCustomEmbed converte o input validado em models.Embed, truncando os
// campos ao limite (defesa em profundidade: a validação já os rejeitou acima do
// limite) e fixando source_type/fetch_method.
func buildCustomEmbed(in EmbedInput) (models.Embed, []models.EmbedField) {
	embed := models.Embed{
		SourceType:  embedSourceCustom,
		FetchMethod: embedFetchManual,
		Title:       nullableText(truncateRune(in.Title, embedTitleMax)),
		Description: nullableText(truncateRune(in.Description, embedDescriptionMax)),
		SiteName:    nullableText(truncateRune(in.SiteName, embedSiteNameMax)),
	}

	if in.URL != "" {
		if validated, err := validateEmbedURL(in.URL); err == nil {
			embed.URL = &validated
		}
	}
	if in.Color != "" && embedColorRe.MatchString(in.Color) {
		color := in.Color
		embed.Color = &color
	}

	if in.Author != nil {
		author := &models.EmbedAuthor{
			Name: nullableText(truncateRune(in.Author.Name, embedAuthorNameMax)),
		}
		if in.Author.URL != "" {
			if validated, err := validateEmbedURL(in.Author.URL); err == nil {
				author.URL = &validated
			}
		}
		// icon_url entra só como URL declarada; resolveCustomEmbedMedia baixa e
		// substitui pela mídia persistida (author_media).
		author.Media = declaredEmbedImage(&EmbedMediaInput{URL: in.Author.IconURL})
		if author.Name != nil || author.URL != nil || author.Media != nil {
			embed.Author = author
		}
	}

	if in.Footer != nil && in.Footer.Text != "" {
		embed.Footer = &models.EmbedFooter{
			Text: nullableText(truncateRune(in.Footer.Text, embedFooterTextMax)),
		}
	}

	if in.Video != nil {
		if validated, err := validateEmbedVideoInput(in.Video); err == nil {
			mimeType := strings.ToLower(strings.TrimSpace(strings.Split(in.Video.MimeType, ";")[0]))
			videoURL, videoType := validated, mimeType
			embed.Video = &models.EmbedMedia{URL: &videoURL, MimeType: &videoType}
		}
	}

	// thumbnail/image chegam aqui só com a URL declarada; resolveCustomEmbedMedia
	// baixa cada uma e substitui pela mídia persistida (ou descarta).
	embed.Thumbnail = declaredEmbedImage(in.Thumbnail)
	embed.Image = declaredEmbedImage(in.Image)

	fields := make([]models.EmbedField, 0, len(in.Fields))
	for _, field := range in.Fields {
		fields = append(fields, models.EmbedField{
			Position: len(fields),
			Name:     truncateRune(field.Name, embedFieldNameMax),
			Value:    truncateRune(field.Value, embedFieldValueMax),
			Inline:   field.Inline,
		})
	}

	return embed, fields
}

// declaredEmbedImage guarda a URL de imagem declarada pelo cliente (validada em
// validateCustomEmbed) até resolveCustomEmbedMedia baixá-la.
func declaredEmbedImage(in *EmbedMediaInput) *models.EmbedMedia {
	if in == nil || strings.TrimSpace(in.URL) == "" {
		return nil
	}
	url := strings.TrimSpace(in.URL)
	return &models.EmbedMedia{URL: &url}
}

// resolveCustomEmbedMedia baixa as mídias de imagem do embed customizado
// (thumbnail, image e ícone do autor) e as substitui pela mídia gravada na tabela
// media. Falha de download é logada e o embed segue sem a mídia (a validação de
// entrada já rejeitou esquema, MIME declarado e tamanho inválidos).
func resolveCustomEmbedMedia(ctx context.Context, userID string, embed *models.Embed) {
	embed.Thumbnail = resolveCustomEmbedImage(ctx, userID, embed.Thumbnail)
	embed.Image = resolveCustomEmbedImage(ctx, userID, embed.Image)
	if embed.Author != nil {
		embed.Author.Media = resolveCustomEmbedImage(ctx, userID, embed.Author.Media)
	}
}

func resolveCustomEmbedImage(ctx context.Context, userID string, declared *models.EmbedMedia) *models.EmbedMedia {
	if declared == nil || declared.URL == nil {
		return nil
	}
	// A mídia de embed usa o mesmo rate limit do crawler: URL outbound informada
	// pelo cliente é o mesmo vetor de abuso.
	if !embedRateAllow(userID) {
		utils.Errorf("embed custom: rate limit de mídia estourado (userID=%s)", userID)
		return nil
	}
	media, err := downloadEmbedImage(ctx, config.LoadConfig(), *declared.URL)
	if err != nil {
		utils.Errorf("embed custom: falha ao baixar mídia (%s): %v", *declared.URL, err)
		return nil
	}
	return media
}

// embedInputTextLength é a soma do texto de um embed customizado (título,
// descrição, autor, footer e fields) — entra na soma total da mensagem.
func embedInputTextLength(in EmbedInput) int {
	total := textLen(in.Title) + textLen(in.Description)
	if in.Author != nil {
		total += textLen(in.Author.Name)
	}
	if in.Footer != nil {
		total += textLen(in.Footer.Text)
	}
	for _, field := range in.Fields {
		total += textLen(field.Name) + textLen(field.Value)
	}
	return total
}

// embedTextLength é a soma do texto de um embed já persistido.
func embedTextLength(embed models.Embed) int {
	total := textLenPtr(embed.Title) + textLenPtr(embed.Description)
	if embed.Author != nil {
		total += textLenPtr(embed.Author.Name)
	}
	if embed.Footer != nil {
		total += textLenPtr(embed.Footer.Text)
	}
	for _, field := range embed.Fields {
		total += textLen(field.Name) + textLen(field.Value)
	}
	return total
}

func textLen(s string) int { return utf8.RuneCountInString(s) }
func textLenPtr(s *string) int {
	if s == nil {
		return 0
	}
	return utf8.RuneCountInString(*s)
}
