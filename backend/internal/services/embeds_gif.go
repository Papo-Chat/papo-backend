package services

// GIF embeds de tenor.com / giphy.com / klipy.com: port do resolvedor do
// gif-scrapper para o pipeline existente de link embeds (GetOrCreateEmbed).
//
// O link na mensagem é resolvido no page URL do provedor (HTML aberto no
// tenor/giphy; JSON API no klipy — a página HTML do klipy é bloqueada por
// Cloudflare, mas o endpoint JSON que a própria página usa não é). Do page
// URL saem o título e o mapa de renditions; a rendition GIF animada é baixada
// e gravada na tabela media (content-addressable) como veio do provedor: o
// re-encode do pipeline de thumbnail (utils.GenerateThumbnail) produz WebP
// estático e perderia a animação, que é o conteúdo do embed.
//
// O embed resultante é a versão simples de link embed: title (linkando ao
// page URL original na UI), provider e thumbnail GIF. source_type='link' e
// fetch_method='gif' (cache_key = URL normalizada da mensagem, como todo
// link embed).
//
// O token proxy do klipy NÃO é usado aqui: ele só existe no endpoint de
// busca do klipy; a resolução de link (/api/proxy/gifs/<slug>) é aberta.
//
// O fluxo é o mesmo dos outros link embeds: roda no goroutine de crawl
// (ProcessMessageEmbeds), nunca no caminho da resposta HTTP da mensagem.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/gif"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"
)

// Nomes de provedor GIF (valores internos de roteamento; o label exposto no
// embed vem de gifProviderLabel).
const (
	gifProviderTenor = "tenor"
	gifProviderGiphy = "giphy"
	gifProviderKlipy = "klipy"
)

// maxKlipyJSONBytes é o teto do corpo da API JSON do klipy (o payload de um
// item é pequeno; 1MB fecha respostas aberrantes).
const maxKlipyJSONBytes = 1 << 20

// gifMimeType é o MIME da rendition GIF animada gravada como thumbnail do embed.
const gifMimeType = "image/gif"

// gifRendition é uma rendition normalizada do provedor (port do modelo do
// gif-scrapper). Bytes é o tamanho declarado pelo provedor (0 = desconhecido).
type gifRendition struct {
	URL    string
	Format string // gif | webp | jpg | png | mp4 | webm
	Kind   string // rótulo nativo: md.gif / fixed_height / mediumgif
	Tier   string // xs | sm | md | hd
	Width  int
	Height int
	Bytes  int64
	Still  bool // poster estático (*_still, *preview, png): nunca preferida
}

// gifItem é o item normalizado de um provedor GIF.
type gifItem struct {
	Provider   string
	ID         string
	PageURL    string
	Title      string
	Renditions []gifRendition
}

// gifProviderHost retorna o provedor GIF cujo host casa com o hostname dado
// (exato ou subdomínio: media.giphy.com, i.giphy.com, static.klipy.com,
// www.tenor.com ...), ou "" quando o host não é de provedor GIF.
func gifProviderHost(host string) string {
	host = normalizeHost(host)
	for _, entry := range []struct {
		provider string
		domains  []string
	}{
		{gifProviderTenor, []string{"tenor.com"}},
		{gifProviderGiphy, []string{"giphy.com", "giphy.media"}},
		{gifProviderKlipy, []string{"klipy.com"}},
	} {
		for _, domain := range entry.domains {
			if host == domain || strings.HasSuffix(host, "."+domain) {
				return entry.provider
			}
		}
	}
	return ""
}

func gifProviderLabel(provider string) string {
	switch provider {
	case gifProviderTenor:
		return "Tenor"
	case gifProviderGiphy:
		return "GIPHY"
	case gifProviderKlipy:
		return "KLIPY"
	}
	return ""
}

// fetchGIFEmbed resolve o link do provedor GIF, baixa a rendition GIF e grava
// os bytes do próprio provedor na media (thumbnail do embed), então upserta o
// link embed (cache_key = URL normalizada). Qualquer falha retorna erro (o
// chamador loga e segue sem embed — best-effort, como os outros link embeds).
func fetchGIFEmbed(ctx context.Context, cfg *config.Config, original *url.URL, provider string) (models.Embed, error) {
	// O embed GIF é a mídia do provedor: sem mídia habilitada ele não tem
	// sentido. Falha antes de gastar rede.
	if !cfg.ThumbnailEnabled {
		return models.Embed{}, errors.New("processamento de thumbnail desabilitado")
	}

	var item gifItem
	var err error
	switch provider {
	case gifProviderTenor:
		item, err = resolveTenorItem(ctx, original)
	case gifProviderGiphy:
		item, err = resolveGiphyItem(ctx, original)
	case gifProviderKlipy:
		item, err = resolveKlipyItem(ctx, original)
	default:
		return models.Embed{}, fmt.Errorf("provedor GIF desconhecido: %q", provider)
	}
	if err != nil {
		return models.Embed{}, err
	}

	rendition, err := pickGIFRendition(item, maxEmbedImageBytes)
	if err != nil {
		return models.Embed{}, err
	}

	body, err := downloadGIFRendition(ctx, rendition.URL)
	if err != nil {
		return models.Embed{}, err
	}

	// O GIF é gravado como veio do provedor: o re-encode do pipeline de
	// thumbnail (utils.GenerateThumbnail) produz WebP estático e destruiria a
	// animação, que é exatamente o que o embed GIF é. As guardas são o teto de
	// bytes do download (maxEmbedImageBytes) e as dimensões declaradas no
	// cabeçalho.
	width, height, err := gifHeaderDims(body)
	if err != nil {
		return models.Embed{}, err
	}
	mediaSHA, _, err := StoreMediaFromBytes(ctx, body, gifMimeType)
	if err != nil {
		return models.Embed{}, fmt.Errorf("falha ao gravar o gif do embed: %w", err)
	}

	normalized := original.String()
	pageURL := normalized
	if validated, err := validateEmbedURL(item.PageURL); err == nil {
		pageURL = validated
	}

	providerLabel := gifProviderLabel(provider)
	thumbMime := gifMimeType
	// gifHeaderDims garante dimensões > 0.
	thumbnail := &models.EmbedMedia{
		MediaSHA: &mediaSHA,
		MimeType: &thumbMime,
		Width:    &width,
		Height:   &height,
	}

	embed := models.Embed{
		SourceType:  embedSourceLink,
		FetchMethod: embedFetchGIF,
		CacheKey:    &normalized,
		URL:         &pageURL,
		Title:       nullableText(truncateRune(item.Title, embedTitleMax)),
		Provider:    &providerLabel,
		Thumbnail:   thumbnail,
	}
	return storage.UpsertLinkEmbed(ctx, embed)
}

// pickGIFRendition escolhe a rendition GIF animada (port do pickRendition do
// gif-scrapper, restrito a GIF): md → sm → xs → hd, ignorando renditions cujo
// tamanho declarado excede maxBytes; sem candidata dentro do teto, aceita a
// melhor por tier mesmo estourando o declarado (o teto real é aplicado no
// download).
//
// As renditions vêm de maps do Go, cuja ordem de iteração é aleatória: dentro
// do tier a escolha é determinística (maior área, depois kind) para a mesma URL
// sempre gravar a mesma mídia — sem isso, cada refetch poderia gravar um
// arquivo diferente do mesmo gif.
func pickGIFRendition(item gifItem, maxBytes int64) (gifRendition, error) {
	tiers := []string{"md", "sm", "xs", "hd"}

	animated := func(r gifRendition) bool { return r.Format == "gif" && !r.Still }
	fits := func(r gifRendition) bool {
		return animated(r) && (r.Bytes <= 0 || r.Bytes <= maxBytes)
	}

	// best escolhe a melhor candidata que passa em pred; tier "" aceita
	// qualquer tier (inclusive desconhecido).
	best := func(pred func(gifRendition) bool, tier string) (gifRendition, bool) {
		var out gifRendition
		var found bool
		for _, r := range item.Renditions {
			if !pred(r) || (tier != "" && r.Tier != tier) {
				continue
			}
			if !found || betterRendition(r, out) {
				out, found = r, true
			}
		}
		return out, found
	}

	for _, tier := range tiers {
		if r, ok := best(fits, tier); ok {
			return r, nil
		}
	}
	if r, ok := best(fits, ""); ok {
		return r, nil
	}
	for _, tier := range tiers {
		if r, ok := best(animated, tier); ok {
			return r, nil
		}
	}
	if r, ok := best(animated, ""); ok {
		return r, nil
	}
	return gifRendition{}, errors.New("item GIF não tem rendition animada disponível")
}

// betterRendition ordena candidatas do mesmo tier: maior área primeiro
// (qualidade), desempate por kind.
func betterRendition(a, b gifRendition) bool {
	areaA := int64(a.Width) * int64(a.Height)
	areaB := int64(b.Width) * int64(b.Height)
	if areaA != areaB {
		return areaA > areaB
	}
	return a.Kind < b.Kind
}

// downloadGIFRendition baixa os bytes da rendition (robots check na origem +
// semáforo outbound + teto maxEmbedImageBytes) e valida o magic number: só
// image/gif é aceito.
func downloadGIFRendition(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := utils.NormalizeURL(rawURL)
	if err != nil {
		return nil, err
	}
	if !embedRobotsAllowed(ctx, u) {
		return nil, errors.New("origem da mídia não permitida pelo robots.txt")
	}
	if !acquireOutboundSlot() {
		return nil, errors.New("semáforo outbound cheio")
	}
	defer releaseOutboundSlot()

	body, _, err := utils.SafeFetch(ctx, outboundHTTPClient(), maxEmbedImageBytes, u.String())
	if err != nil {
		return nil, err
	}
	if mime := utils.DetectMimeType(body); mime != gifMimeType {
		return nil, fmt.Errorf("rendition não é GIF (mime=%q)", mime)
	}
	return body, nil
}

// fetchProviderPage faz o fetch SSRF-safe de uma página/endpoint de provedor
// GIF (semáforo + robots check), com o User-Agent padrão do preview.
func fetchProviderPage(ctx context.Context, rawURL string, maxBytes int64) ([]byte, *url.URL, error) {
	u, err := utils.NormalizeURL(rawURL)
	if err != nil {
		return nil, nil, err
	}
	if !embedRobotsAllowed(ctx, u) {
		return nil, nil, errors.New("origem não permitida pelo robots.txt")
	}
	if !acquireOutboundSlot() {
		return nil, nil, errors.New("semáforo outbound cheio")
	}
	defer releaseOutboundSlot()
	return utils.SafeFetch(ctx, outboundHTTPClient(), maxBytes, u.String())
}

/* ------------------------------------------------------------------- tenor */

// tenorIDRe extrai o id numérico do fim do path (…-gif-7058550686737920259).
var tenorIDRe = regexp.MustCompile(`\d{6,25}`)

// parseTenorPageURL normaliza a URL do tenor (port do parseTenorUrl): locale
// (/pt-BR/view/…) é removida, /embed/<id> e link curto são preservados e o id
// numérico é extraído do path.
func parseTenorPageURL(u *url.URL) (id string, pageURL string, err error) {
	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}

	var matches []string
	for _, m := range tenorIDRe.FindAllString(u.EscapedPath(), -1) {
		matches = append(matches, m)
	}
	if len(matches) > 0 {
		id = matches[len(matches)-1]
	}

	viewAt := -1
	for i, p := range parts {
		if p == "view" {
			viewAt = i
			break
		}
	}
	if viewAt != -1 && viewAt+1 < len(parts) && parts[viewAt+1] != "" {
		return id, "https://tenor.com/view/" + parts[viewAt+1], nil
	}
	if len(parts) > 0 && parts[0] == "embed" && len(parts) > 1 && parts[1] != "" {
		return id, "https://tenor.com/embed/" + parts[1], nil
	}
	if len(parts) == 1 {
		// Link curto (tenor.com/<token>.gif): o fetch segue o 301 e o id é
		// re-derivado da URL final.
		return id, u.String(), nil
	}
	if id != "" {
		return id, u.String(), nil
	}
	return "", "", fmt.Errorf("URL do tenor não reconhecida: %s", u.String())
}

// tenorHydrationBlob localiza e parseia o blob de hidratação do tenor
// (<script nonce="…">{"appConfig",…}</script>).
func tenorHydrationBlob(body []byte) *tenorBlob {
	for _, m := range tenorScriptRe.FindAllSubmatch(body, -1) {
		var blob tenorBlob
		if err := json.Unmarshal(m[1], &blob); err == nil {
			return &blob
		}
	}
	return nil
}

var tenorScriptRe = regexp.MustCompile(`(?s)<script[^>]*nonce="[^"]*"[^>]*>(\{"appConfig".*?)</script>`)

type tenorBlob struct {
	GIFs struct {
		ByID map[string]struct {
			Results []tenorRawItem `json:"results"`
		} `json:"byId"`
	} `json:"gifs"`
}

type tenorMediaFormat struct {
	URL  string `json:"url"`
	Dims [2]int `json:"dims"`
	Size int64  `json:"size"`
}

type tenorRawItem struct {
	ID           string                      `json:"id"`
	Title        string                      `json:"title"`
	H1Title      string                      `json:"h1_title"`
	MediaFormats map[string]tenorMediaFormat `json:"media_formats"`
}

// tenorItemFromBlob resolve o item do blob pelo id (ou o único id presente,
// quando o link curto não expunha o id antes do redirect).
func tenorItemFromBlob(blob *tenorBlob, id string) *tenorRawItem {
	if blob == nil {
		return nil
	}
	if id != "" {
		if results := blob.GIFs.ByID[id].Results; len(results) > 0 {
			return &results[0]
		}
	}
	if len(blob.GIFs.ByID) == 1 {
		for _, entry := range blob.GIFs.ByID {
			if len(entry.Results) > 0 {
				return &entry.Results[0]
			}
		}
	}
	return nil
}

var tenorTierByKind = map[string]string{
	"gif":            "hd",
	"webp":           "hd",
	"webm":           "hd",
	"mp4":            "hd",
	"mediumgif":      "md",
	"gifpreview":     "md",
	"tinygif":        "sm",
	"tinywebp":       "sm",
	"tinymp4":        "sm",
	"tinywebm":       "sm",
	"tinygifpreview": "xs",
}

func tenorRenditionsFromFormats(formats map[string]tenorMediaFormat) []gifRendition {
	out := make([]gifRendition, 0, len(formats))
	seen := make(map[string]struct{}, len(formats))
	for kind, entry := range formats {
		if entry.URL == "" {
			continue
		}
		if _, dup := seen[entry.URL]; dup {
			continue
		}
		seen[entry.URL] = struct{}{}

		format := mediaFormatFromURL(entry.URL)
		if format == "" {
			continue
		}
		tier := tenorTierByKind[kind]
		if tier == "" {
			tier = tierByArea(entry.Dims[0], entry.Dims[1])
		}
		out = append(out, gifRendition{
			URL:    entry.URL,
			Format: format,
			Kind:   kind,
			Tier:   tier,
			Width:  entry.Dims[0],
			Height: entry.Dims[1],
			Bytes:  entry.Size,
			Still:  strings.Contains(kind, "preview") || format == "png",
		})
	}
	return out
}

func resolveTenorItem(ctx context.Context, original *url.URL) (gifItem, error) {
	id, pageURL, err := parseTenorPageURL(original)
	if err != nil {
		return gifItem{}, err
	}

	body, finalURL, err := fetchProviderPage(ctx, pageURL, maxEmbedHTMLBytes)
	if err != nil {
		return gifItem{}, err
	}

	// O link curto pode ter redirecionado; o id da URL final prevalece.
	if matches := tenorIDRe.FindAllString(finalURL.EscapedPath(), -1); len(matches) > 0 {
		id = matches[len(matches)-1]
	}
	canonical := metaTagContent(body, "og:url")
	if canonical == "" {
		canonical = finalURL.String()
	}

	raw := tenorItemFromBlob(tenorHydrationBlob(body), id)
	if raw != nil {
		renditions := tenorRenditionsFromFormats(raw.MediaFormats)
		if len(renditions) > 0 {
			title := firstNonEmpty(raw.H1Title, raw.Title, metaTagContent(body, "og:title"))
			return gifItem{Provider: gifProviderTenor, ID: firstNonEmpty(raw.ID, id), PageURL: canonical, Title: title, Renditions: renditions}, nil
		}
	}

	// Caminho degradado quando o blob sumiu: renditions dos meta tags.
	renditions := metaTagRenditions(body)
	if len(renditions) == 0 {
		return gifItem{}, fmt.Errorf("não foi possível extrair o mídia do tenor em %s (blob de hidratação ausente)", canonical)
	}
	return gifItem{
		Provider:   gifProviderTenor,
		ID:         id,
		PageURL:    canonical,
		Title:      metaTagContent(body, "og:title"),
		Renditions: renditions,
	}, nil
}

/* ------------------------------------------------------------------- giphy */

// isGiphyID valida o id do giphy: 8–24 alfanuméricos com ao menos uma
// maiúscula ou dígito (ids puros em minúsculo não existem; slug words são
// minúsculos). O teto é 24, não 16: os ids atuais do giphy têm 12–19
// caracteres (ex.: QBd2kLB5qDmysEXre9, 18). Com 16 o tail do slug é descartado
// e o id do JSON-LD também — a resolução do link falha por completo.
func isGiphyID(s string) bool {
	if len(s) < 8 || len(s) > 24 {
		return false
	}
	hasUpperOrDigit := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			hasUpperOrDigit = true
		default:
			return false
		}
	}
	return hasUpperOrDigit
}

// parseGiphyPageURL normaliza a URL do giphy (port do parseGiphyUrl): page
// URLs (/gifs/<slug>, /clips/…, /embed/…), URLs diretas do CDN
// (/media/<id>/giphy.gif) e slug cujo tail é o id.
func parseGiphyPageURL(u *url.URL) (id string, pageURL string, err error) {
	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}

	if len(parts) > 0 && parts[0] == "media" {
		id = giphyIDFromMediaPath(parts)
		if id == "" {
			return "", "", fmt.Errorf("nenhum id de gif no path %s", u.EscapedPath())
		}
		return id, "https://giphy.com/gifs/" + id, nil
	}

	if len(parts) > 1 && (parts[0] == "gifs" || parts[0] == "clips" || parts[0] == "embed") {
		tail := parts[1]
		// O slug pode não terminar em id reconhecível: o id é recuperado do
		// JSON-LD no corpo (o page URL já é conhecido).
		if candidate := giphyIDFromSlugTail(tail); candidate != "" {
			id = candidate
		}
		if parts[0] == "embed" {
			return id, "https://giphy.com/embed/" + tail, nil
		}
		return id, "https://giphy.com/" + parts[0] + "/" + tail, nil
	}

	if len(parts) > 0 && isGiphyID(parts[len(parts)-1]) {
		id = parts[len(parts)-1]
		return id, "https://giphy.com/gifs/" + id, nil
	}
	return "", "", fmt.Errorf("URL do giphy não reconhecida: %s", u.String())
}

// giphyIDFromMediaPath lê o id de /media/<id>/… ou /media/v1.<token>/<id>/…:
// o último segmento é o nome do arquivo e é descartado; "media" e os prefixes
// "v1." também.
func giphyIDFromMediaPath(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	segs := make([]string, 0, len(parts))
	for _, s := range parts[:len(parts)-1] {
		if s == "media" || strings.HasPrefix(s, "v1.") {
			continue
		}
		segs = append(segs, s)
	}
	if len(segs) == 0 {
		return ""
	}
	candidate := segs[len(segs)-1]
	if isGiphyID(candidate) {
		return candidate
	}
	return ""
}

// giphyIDFromSlugTail extrai o id do tail do slug (funny-happy-okfvUCpgArv3y).
func giphyIDFromSlugTail(slug string) string {
	tail := slug
	if i := strings.LastIndex(slug, "-"); i >= 0 {
		tail = slug[i+1:]
	}
	if isGiphyID(tail) {
		return tail
	}
	return ""
}

var giphyRSCRe = regexp.MustCompile(`self\.__next_f\.push\(\[1,"((?:[^"\\]|\\.)*)"\]\)`)

// giphyRSCBlob desescapa e concatena os chunks do stream RSC do Next.js
// (self.__next_f.push([1,"…"])).
func giphyRSCBlob(body []byte) string {
	var out strings.Builder
	for _, m := range giphyRSCRe.FindAllSubmatch(body, -1) {
		unquoted, err := strconv.Unquote(`"` + string(m[1]) + `"`)
		if err != nil {
			continue // chunk que não é string literal JSON limpa
		}
		out.WriteString(unquoted)
	}
	return out.String()
}

// balancedAt recorta o objeto/array balanceado a partir de start (string- e
// escape-aware).
func balancedAt(text string, start int) string {
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(text); i++ {
		ch := text[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return text[start : i+1]
			}
			if depth < 0 {
				return ""
			}
		}
	}
	return ""
}

type giphyRawGif struct {
	ID       string                     `json:"id"`
	Slug     string                     `json:"slug"`
	Title    string                     `json:"title"`
	URL      string                     `json:"url"`
	Username string                     `json:"username"`
	Images   map[string]giphyImageEntry `json:"images"`
}

// giphyImageEntry é uma rendition do mapa "images" do giphy. Height/Width/Size
// são json.RawMessage porque o payload mistura shapes (número, string JSON e,
// no JSON-LD, objeto QuantitativeValue): um int rígido faz o Unmarshal do objeto
// inteiro falhar e o resolvedor cai no caminho degradado.
type giphyImageEntry struct {
	Height json.RawMessage `json:"height"`
	Width  json.RawMessage `json:"width"`
	Size   json.RawMessage `json:"size"`
	URL    string          `json:"url"`
	MP4    string          `json:"mp4"`
	WebP   string          `json:"webp"`
	Gif    string          `json:"gif"`
}

var giphyGifObjectRe = regexp.MustCompile(`\{"type":"(?:gif|clip|sticker)"`)

// findGifObject localiza o objeto do gif no blob RSC: brace-match dos objetos
// {"type":"gif"…} com id igual; fallback: o mapa "images" que segue o id.
func findGifObject(blob string, id string) *giphyRawGif {
	for _, m := range giphyGifObjectRe.FindAllStringIndex(blob, -1) {
		raw := balancedAt(blob, m[0])
		if raw == "" {
			continue
		}
		var parsed giphyRawGif
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			continue // tokens de referência do RSC podem tornar isso imparsável
		}
		if parsed.ID == id {
			return &parsed
		}
	}

	idAt := strings.Index(blob, `"id":"`+id+`"`)
	if idAt == -1 {
		return nil
	}
	imagesAt := strings.Index(blob[idAt:], `"images":{`)
	if imagesAt == -1 {
		return nil
	}
	start := idAt + imagesAt + len(`"images":`)
	raw := balancedAt(blob, start)
	if raw == "" {
		return nil
	}
	var images map[string]giphyImageEntry
	if err := json.Unmarshal([]byte(raw), &images); err != nil {
		return nil
	}
	return &giphyRawGif{ID: id, Images: images}
}

var giphyTierByKind = map[string]string{
	"original":                 "hd",
	"original_mp4":             "hd",
	"original_still":           "hd",
	"source":                   "hd",
	"downsized_large":          "hd",
	"downsized":                "md",
	"downsized_medium":         "md",
	"downsized_still":          "md",
	"480w_still":               "md",
	"fixed_height":             "md",
	"fixed_width":              "md",
	"fixed_height_still":       "md",
	"fixed_width_still":        "md",
	"downsized_small":          "sm",
	"fixed_height_downsampled": "sm",
	"fixed_width_downsampled":  "sm",
	"looping":                  "sm",
	"loops":                    "sm",
	"preview":                  "sm",
	"preview_gif":              "sm",
	"preview_webp":             "sm",
	"fixed_height_small":       "xs",
	"fixed_width_small":        "xs",
	"fixed_height_small_still": "xs",
	"fixed_width_small_still":  "xs",
}

func giphyRenditionsFromImages(images map[string]giphyImageEntry) []gifRendition {
	out := make([]gifRendition, 0, len(images)*2)
	seen := make(map[string]struct{}, len(images)*2)

	for kind, entry := range images {
		tier := giphyTierByKind[kind]
		width, height := jsonInt(entry.Width), jsonInt(entry.Height)
		if tier == "" {
			tier = tierByArea(width, height)
		}
		isStill := strings.HasSuffix(kind, "_still") || kind == "480w_still"
		size := jsonNumber(entry.Size)

		candidates := []struct {
			url  string
			size int64
		}{
			{entry.URL, size},
			{entry.MP4, 0},
			{entry.WebP, 0},
			{entry.Gif, size},
		}
		for _, c := range candidates {
			if c.url == "" {
				continue
			}
			if _, dup := seen[c.url]; dup {
				continue
			}
			format := mediaFormatFromURL(c.url)
			if format == "" {
				continue
			}
			seen[c.url] = struct{}{}
			out = append(out, gifRendition{
				URL:    c.url,
				Format: format,
				Kind:   kind,
				Tier:   tier,
				Width:  width,
				Height: height,
				Bytes:  c.size,
				Still:  isStill || format == "png",
			})
		}
	}
	return out
}

// giphyLegacyRenditions são as renditions de último recurso do padrão CDN
// legado estável (media.giphy.com/media/<id>/…).
func giphyLegacyRenditions(id string) []gifRendition {
	base := "https://media.giphy.com/media/" + id
	spec := []struct {
		kind string
		tier string
		file string
	}{
		{"original", "hd", "giphy.gif"},
		{"fixed_height", "md", "200.gif"},
		{"preview", "sm", "giphy_s.gif"},
		{"fixed_height_small", "xs", "100.gif"},
	}
	out := make([]gifRendition, 0, len(spec))
	for _, s := range spec {
		out = append(out, gifRendition{
			URL:    base + "/" + s.file,
			Format: "gif",
			Kind:   s.kind,
			Tier:   s.tier,
		})
	}
	return out
}

type giphyJSONLD struct {
	Title string `json:"headline"`
	// Width/Height são json.RawMessage: o JSON-LD do giphy os entrega como
	// objeto QuantitativeValue ({"@type":"QuantitativeValue","value":194}), e
	// um int rígido faria o parse do documento inteiro falhar (o título e a
	// media URL autoritativa iriam embora com ele).
	Image struct {
		URL    string          `json:"url"`
		Width  json.RawMessage `json:"width"`
		Height json.RawMessage `json:"height"`
	} `json:"image"`
}

var giphyJSONLDRe = regexp.MustCompile(`(?s)<script[^>]*type="application/ld\+json"[^>]*>(.*?)</script>`)

func giphyJSONLDFrom(body []byte) *giphyJSONLD {
	var out *giphyJSONLD
	for _, m := range giphyJSONLDRe.FindAllSubmatch(body, -1) {
		var doc giphyJSONLD
		if err := json.Unmarshal(m[1], &doc); err != nil {
			continue
		}
		if out == nil {
			out = &giphyJSONLD{}
		}
		if doc.Title != "" {
			out.Title = doc.Title
		}
		if doc.Image.URL != "" {
			out.Image = doc.Image
		}
	}
	return out
}

func resolveGiphyItem(ctx context.Context, original *url.URL) (gifItem, error) {
	id, pageURL, err := parseGiphyPageURL(original)
	if err != nil {
		return gifItem{}, err
	}

	body, _, err := fetchProviderPage(ctx, pageURL, maxEmbedHTMLBytes)
	if err != nil {
		return gifItem{}, err
	}

	ld := giphyJSONLDFrom(body)

	// A media URL do JSON-LD carrega o id autoritativo (cobre slug sem id
	// reconhecível).
	if id == "" && ld != nil && ld.Image.URL != "" {
		parsed, err := url.Parse(ld.Image.URL)
		if err == nil {
			id = giphyIDFromMediaPath(strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/"))
		}
	}
	if id == "" {
		return gifItem{}, fmt.Errorf("não foi possível determinar o id do gif em %s", pageURL)
	}

	raw := findGifObject(giphyRSCBlob(body), id)
	if raw != nil && len(raw.Images) > 0 {
		renditions := giphyRenditionsFromImages(raw.Images)
		if len(renditions) > 0 {
			canonical := firstNonEmpty(raw.URL, "https://giphy.com/gifs/"+id)
			title := firstNonEmpty(raw.Title, ldTitle(ld), metaTagContent(body, "og:title"))
			return gifItem{Provider: gifProviderGiphy, ID: id, PageURL: canonical, Title: title, Renditions: renditions}, nil
		}
	}

	// Caminho degradado: padrão CDN legado + media URL do JSON-LD.
	renditions := giphyLegacyRenditions(id)
	if ld != nil && ld.Image.URL != "" {
		renditions = append([]gifRendition{{
			URL:    ld.Image.URL,
			Format: firstNonEmpty(mediaFormatFromURL(ld.Image.URL), "gif"),
			Kind:   "original",
			Tier:   "hd",
			Width:  jsonInt(ld.Image.Width),
			Height: jsonInt(ld.Image.Height),
		}}, renditions...)
	}
	return gifItem{
		Provider:   gifProviderGiphy,
		ID:         id,
		PageURL:    "https://giphy.com/gifs/" + id,
		Title:      firstNonEmpty(ldTitle(ld), metaTagContent(body, "og:title")),
		Renditions: renditions,
	}, nil
}

func ldTitle(ld *giphyJSONLD) string {
	if ld == nil {
		return ""
	}
	return ld.Title
}

/* ------------------------------------------------------------------- klipy */

// parseKlipyPageURL normaliza a URL do klipy (port do parseKlipyUrl):
// /gifs/<slug>, /clips/<slug>, /stickers/<slug> (com sub-path /player opcional).
// Retorna o segmento da API (gifs|clips|stickers) e o slug.
func parseKlipyPageURL(u *url.URL) (segment string, slug string, err error) {
	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) < 2 {
		return "", "", fmt.Errorf("nenhum slug em %s", u.String())
	}

	switch parts[0] {
	case "gifs", "clips", "stickers":
	default:
		return "", "", fmt.Errorf("URL do klipy não reconhecida: %s", u.String())
	}
	slug = parts[1]
	if !klipySlugRe.MatchString(slug) {
		return "", "", fmt.Errorf("slug do klipy inválido: %q", slug)
	}
	return parts[0], slug, nil
}

var klipySlugRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// klipyEnvelope é o envelope da API JSON do klipy ({result, data, errors}).
type klipyEnvelope struct {
	Result bool            `json:"result"`
	Data   json.RawMessage `json:"data"`
	Errors struct {
		Message json.RawMessage `json:"message"`
	} `json:"errors"`
}

type klipyRenditionMeta struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Size   int64  `json:"size"`
}

type klipyItem struct {
	UUID  string                                   `json:"uuid"`
	Slug  string                                   `json:"slug"`
	Title string                                   `json:"title"`
	URL   string                                   `json:"url"`
	File  map[string]map[string]klipyRenditionMeta `json:"file"`
}

// klipyErrorMessage normaliza errors.message (string ou array de strings).
func klipyErrorMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "erro desconhecido"
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil && len(list) > 0 {
		return strings.Join(list, ", ")
	}
	return "erro desconhecido"
}

func resolveKlipyItem(ctx context.Context, original *url.URL) (gifItem, error) {
	segment, slug, err := parseKlipyPageURL(original)
	if err != nil {
		return gifItem{}, err
	}

	// A página HTML do klipy é protegida por Cloudflare; o endpoint JSON que a
	// própria página usa é aberto e não exige o token proxy (o token só existe
	// na busca).
	apiURL := "https://klipy.com/api/proxy/" + segment + "/" + url.PathEscape(slug)
	body, _, err := fetchProviderPage(ctx, apiURL, maxKlipyJSONBytes)
	if err != nil {
		return gifItem{}, err
	}

	var env klipyEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return gifItem{}, fmt.Errorf("klipy: JSON inválido em %s", apiURL)
	}
	if !env.Result || len(env.Data) == 0 {
		return gifItem{}, fmt.Errorf("klipy: erro da API para %s: %s", apiURL, klipyErrorMessage(env.Errors.Message))
	}

	var raw klipyItem
	if err := json.Unmarshal(env.Data, &raw); err != nil {
		return gifItem{}, fmt.Errorf("klipy: item inválido em %s", apiURL)
	}

	renditions := klipyRenditions(raw.File)
	if len(renditions) == 0 {
		return gifItem{}, fmt.Errorf("klipy: item %s não tem renditions", slug)
	}

	pageURL := firstNonEmpty(raw.URL, "https://klipy.com/"+segment+"/"+raw.Slug)
	return gifItem{
		Provider:   gifProviderKlipy,
		ID:         firstNonEmpty(raw.UUID, slug),
		PageURL:    pageURL,
		Title:      raw.Title,
		Renditions: renditions,
	}, nil
}

var klipyTierOrder = []string{"hd", "md", "sm", "xs"}

func klipyRenditions(file map[string]map[string]klipyRenditionMeta) []gifRendition {
	out := make([]gifRendition, 0, 8)
	seen := make(map[string]struct{}, 8)
	for _, tier := range klipyTierOrder {
		byFormat := file[tier]
		for format, meta := range byFormat {
			if meta.URL == "" {
				continue
			}
			if _, dup := seen[meta.URL]; dup {
				continue
			}
			seen[meta.URL] = struct{}{}
			out = append(out, gifRendition{
				URL:    meta.URL,
				Format: firstNonEmpty(mediaFormatFromURL(meta.URL), format),
				Kind:   tier + "." + format,
				Tier:   tier,
				Width:  meta.Width,
				Height: meta.Height,
				Bytes:  meta.Size,
			})
		}
	}
	return out
}

/* -------------------------------------------------------- helpers comuns */

// gifHeaderDims lê as dimensões do cabeçalho do GIF (sem decodificar pixels).
// O blob do provedor é gravado sem re-encode, então esta é a única guarda de
// decompression bomb do caminho: as dimensões declaradas são validadas contra
// o teto absoluto de dimensão do pipeline de imagem antes de o arquivo entrar
// na media.
func gifHeaderDims(body []byte) (int, int, error) {
	cfg, err := gif.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		return 0, 0, fmt.Errorf("gif do provedor inválido: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 ||
		cfg.Width > utils.MaxThumbnailDimCeiling || cfg.Height > utils.MaxThumbnailDimCeiling {
		return 0, 0, fmt.Errorf("dimensões do gif do provedor (%dx%d) fora do aceitável", cfg.Width, cfg.Height)
	}
	return cfg.Width, cfg.Height, nil
}

// mediaFormatFromURL deriva o formato da extensão do path da URL.
var mediaFormatByExt = map[string]string{
	"gif": "gif", "webp": "webp", "jpg": "jpg", "jpeg": "jpg",
	"png": "png", "mp4": "mp4", "m4v": "mp4", "webm": "webm",
}

func mediaFormatFromURL(raw string) string {
	clean := raw
	if i := strings.IndexAny(clean, "#?"); i >= 0 {
		clean = clean[:i]
	}
	last := clean[strings.LastIndex(clean, "/")+1:]
	dot := strings.LastIndex(last, ".")
	if dot == -1 {
		return ""
	}
	return mediaFormatByExt[strings.ToLower(last[dot+1:])]
}

// tierByArea é o tiering de fallback quando o provedor usa um kind desconhecido.
func tierByArea(width, height int) string {
	longest := width
	if height > longest {
		longest = height
	}
	switch {
	case longest >= 480:
		return "hd"
	case longest >= 320:
		return "md"
	case longest >= 180:
		return "sm"
	default:
		return "xs"
	}
}

// jsonNumber lê um número do provedor que pode chegar como número JSON, string
// JSON ("1860111") ou objeto QuantitativeValue do JSON-LD ({"value":480}).
// 0 = desconhecido (mesma semântica de Bytes/Width/Height ausentes).
func jsonNumber(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}

	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0
		}
		return v
	}

	var quantitative struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &quantitative); err == nil {
		return jsonNumber(quantitative.Value)
	}
	return 0
}

// jsonInt é jsonNumber na largura de um int (dimensões de rendition).
func jsonInt(raw json.RawMessage) int {
	return int(jsonNumber(raw))
}

// metaTagContent lê content de <meta property|name="key" content="…"> ou
// <link rel="key" href="…"> (regex simples, para os fallbacks dos provedores).
func metaTagContent(body []byte, key string) string {
	escaped := regexp.QuoteMeta(key)
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)<meta[^>]+property="` + escaped + `"[^>]+content="([^"]*)"`),
		regexp.MustCompile(`(?i)<meta[^>]+name="` + escaped + `"[^>]+content="([^"]*)"`),
		regexp.MustCompile(`(?i)<link[^>]+rel="` + escaped + `"[^>]+href="([^"]*)"`),
	}
	for _, re := range patterns {
		if m := re.FindSubmatch(body); m != nil {
			return string(m[1])
		}
	}
	return ""
}

// metaTagRenditions monta renditions dos meta tags de mídia (fallback do
// tenor quando o blob de hidratação some).
func metaTagRenditions(body []byte) []gifRendition {
	specs := []struct {
		key  string
		kind string
		tier string
	}{
		{"og:image", "gif", "md"},
		{"og:video", "mp4", "hd"},
		{"twitter:image", "gif", "sm"},
	}
	out := make([]gifRendition, 0, len(specs))
	seen := make(map[string]struct{}, len(specs))
	for _, s := range specs {
		raw := metaTagContent(body, s.key)
		if raw == "" {
			continue
		}
		format := mediaFormatFromURL(raw)
		if format == "" {
			continue
		}
		if _, dup := seen[raw]; dup {
			continue
		}
		seen[raw] = struct{}{}
		out = append(out, gifRendition{URL: raw, Format: format, Kind: s.kind, Tier: s.tier})
	}
	return out
}
