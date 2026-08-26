package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"

	"golang.org/x/net/html"
)

// ErrPreviewNotFound indica que o preview não existe, não tem imagem ou não
// está vinculado a mensagem acessível (o endpoint de imagem responde 404 nos
// três casos — não vazar a existência do preview).
var ErrPreviewNotFound = errors.New("link preview não encontrado")

// maxPreviewHTMLBytes é o teto do corpo HTML (pós-descompressão) do fetch de
// link preview (§6.2 passo 6).
const maxPreviewHTMLBytes = 5 << 20

// maxPreviewImageBytes é o teto do download da imagem do preview (§6.2
// passo 8).
const maxPreviewImageBytes = 5 << 20

// Client SSRF-safe e semáforo de concorrência outbound (HTML, imagem,
// oEmbed e robots.txt contam no mesmo teto, §6.4).
var (
	outboundClientOnce sync.Once
	outboundClient     *http.Client
	outboundSem        chan struct{}
)

func initOutbound() {
	outboundClientOnce.Do(func() {
		cfg := config.LoadConfig()
		timeout := cfg.LinkPreviewTimeout
		if timeout <= 0 {
			timeout = 8 * time.Second
		}
		outboundClient = utils.SafeHTTPClient(utils.SafeClientOpts{Timeout: timeout})

		cap := cfg.OutboundMaxConc
		if cap <= 0 {
			cap = 4
		}
		outboundSem = make(chan struct{}, cap)
	})
}

func outboundHTTPClient() *http.Client {
	initOutbound()
	return outboundClient
}

func acquireOutboundSlot() bool {
	initOutbound()
	select {
	case outboundSem <- struct{}{}:
		return true
	default:
		return false
	}
}

func releaseOutboundSlot() {
	<-outboundSem
}

// tokenBucket é um bucket de tokens com refill contínuo (rate por minuto).
type tokenBucket struct {
	mu     sync.Mutex
	tokens float64
	max    float64
	last   time.Time
}

func newTokenBucket(perMinute int) *tokenBucket {
	if perMinute <= 0 {
		perMinute = 1
	}
	return &tokenBucket{tokens: float64(perMinute), max: float64(perMinute), last: time.Now()}
}

// take consome 1 token se disponível.
func (b *tokenBucket) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens = math.Min(b.max, b.tokens+b.max*now.Sub(b.last).Seconds()/60)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

var (
	previewRateOnce   sync.Once
	previewRateGlobal *tokenBucket
	previewRateUsers  sync.Map // userID → *tokenBucket
)

// previewRateAllow consome token do bucket global (PREVIEW_FETCH_RATE_GLOBAL,
// 30/min) E do bucket por usuário (PREVIEW_FETCH_RATE_USER, 10/min). Estourou
// algum → false (pular o preview, best-effort). Fecha o cache-busting por
// URLs únicas (§6.2 passo 3).
func previewRateAllow(userID string) bool {
	cfg := config.LoadConfig()
	previewRateOnce.Do(func() {
		previewRateGlobal = newTokenBucket(cfg.PreviewFetchRateGlob)
	})
	if !previewRateGlobal.take() {
		return false
	}

	var userBucket *tokenBucket
	if v, ok := previewRateUsers.Load(userID); ok {
		userBucket = v.(*tokenBucket)
	} else {
		userBucket = newTokenBucket(cfg.PreviewFetchRateUser)
		actual, _ := previewRateUsers.LoadOrStore(userID, userBucket)
		userBucket = actual.(*tokenBucket)
	}
	return userBucket.take()
}

var (
	previewURLRe    = regexp.MustCompile(`https?://\S+`)
	trailingPunctRe = regexp.MustCompile(`[.,;!?]+$`)
)

// extractPreviewURLs extrai até max URLs do content (regex conservadora
// https?://\S+), removendo pontuação de cauda e parênteses desbalanceados.
// Duplicatas (mesma string) são ignoradas; as primeiras prevalecem (§6.1).
func extractPreviewURLs(content string, max int) []string {
	if max <= 0 {
		max = 2
	}
	seen := make(map[string]struct{}, max)
	urls := make([]string, 0, max)
	for _, match := range previewURLRe.FindAllString(content, -1) {
		cleaned := stripTrailingPunctuation(match)
		if cleaned == "" {
			continue
		}
		if _, ok := seen[cleaned]; ok {
			continue
		}
		seen[cleaned] = struct{}{}
		urls = append(urls, cleaned)
		if len(urls) == max {
			break
		}
	}
	return urls
}

// stripTrailingPunctuation remove pontuação de cauda (.,;:!?) e parênteses
// fechados desbalanceados (ex.: "https://x.com/a(b)." → "https://x.com/a(b)").
func stripTrailingPunctuation(s string) string {
	for {
		if trimmed := trailingPunctRe.ReplaceAllString(s, ""); trimmed != s {
			s = trimmed
			continue
		}
		if strings.HasSuffix(s, ")") && strings.Count(s, ")") > strings.Count(s, "(") {
			s = s[:len(s)-1]
			continue
		}
		return s
	}
}

// GetOrCreatePreview busca o preview em cache (URL normalizada, fetched_at
// recente) ou faz fetch + parse + thumbnail (§6.2). Best-effort: qualquer
// falha retorna erro (o chamador loga e segue sem preview).
//
// O ctx deve carregar o budget total da fase de previews (compartilhado
// entre as URLs da mensagem, §6.1).
func GetOrCreatePreview(ctx context.Context, userID, rawURL string) (models.LinkPreview, error) {
	cfg := config.LoadConfig()

	// 1. Parse + validação inicial (rejeição rápida sem gastar rede).
	u, err := utils.NormalizeURL(rawURL)
	if err != nil {
		return models.LinkPreview{}, err
	}
	normalized := u.String()

	// 2. Cache (mesma URL normalizada, fetched_at dentro do TTL).
	if cached, err := storage.GetPreviewByURL(ctx, normalized); err == nil {
		if time.Since(cached.FetchedAt) < cfg.LinkPreviewCacheTTL {
			return cached, nil
		}
		// expirado → refetch (fall through; o upsert atualiza a row)
	} else if !errors.Is(err, storage.ErrNotFound) {
		return models.LinkPreview{}, err
	}

	// 3. Rate limit de URL-nova (global + por usuário).
	if !previewRateAllow(userID) {
		return models.LinkPreview{}, errors.New("rate limit de preview estourado")
	}

	// 4. oEmbed first (host allowlistado) — falha → fallback para OG.
	if oembedProviderHost(u.Hostname()) != "" {
		if preview, err := fetchOEmbedPreview(ctx, cfg, u); err == nil {
			return preview, nil
		}
	}

	// 5. robots.txt da origem.
	if !RobotsAllowed(ctx, u) {
		return models.LinkPreview{}, errors.New("origem não permitida pelo robots.txt")
	}

	// 6. Fetch HTML (teto 5MB pós-descompressão).
	if !acquireOutboundSlot() {
		return models.LinkPreview{}, errors.New("semáforo outbound cheio")
	}
	body, finalURL, err := utils.SafeFetch(ctx, outboundHTTPClient(), maxPreviewHTMLBytes, normalized)
	releaseOutboundSlot()
	if err != nil {
		return models.LinkPreview{}, err
	}

	// 7. Parse OpenGraph (og > twitter > fallbacks). Imagem relativa é
	// resolvida contra a URL final pós-redirects.
	title, description, imageURL := parseOpenGraph(body, finalURL)

	preview := models.LinkPreview{
		URL:         normalized,
		Kind:        "og",
		Title:       nullableText(truncateRune(title, cfg.PreviewTitleMax)),
		Description: nullableText(truncateRune(description, cfg.PreviewDescriptionMax)),
	}

	// 8. Imagem (og:image): robots na origem da imagem + thumbnail em CAS.
	if imageURL != "" {
		if imgPath, imgMime, imgSize, err := downloadPreviewImage(ctx, cfg, imageURL); err == nil {
			preview.ImageFilePath = &imgPath
			preview.ImageMimeType = &imgMime
			preview.ImageSizeBytes = &imgSize
		}
		// sem imagem → preview só com title/description (aceitável, §6.2 passo 9)
	}

	return storage.UpsertPreview(ctx, preview)
}

// fetchOEmbedPreview executa o fluxo oEmbed (§9.3): fetch do endpoint
// (isento de robots), parse do subconjunto permitido, thumbnail do
// thumbnail_url (com robots) e embed do YouTube (padrão hardcoded).
func fetchOEmbedPreview(ctx context.Context, cfg *config.Config, target *url.URL) (models.LinkPreview, error) {
	if !acquireOutboundSlot() {
		return models.LinkPreview{}, errors.New("semáforo outbound cheio")
	}
	result, err := fetchOEmbed(ctx, outboundHTTPClient(), target)
	releaseOutboundSlot()
	if err != nil {
		return models.LinkPreview{}, err
	}

	preview := models.LinkPreview{
		URL:          target.String(),
		Kind:         "oembed",
		Title:        nullableText(truncateRune(result.Title, cfg.PreviewTitleMax)),
		ProviderName: nullableText(result.ProviderName),
	}

	// Embed do YouTube: derivado de padrão hardcoded da URL (§9.4), nunca do
	// campo html da resposta.
	if embed := youtubeEmbedURL(target); embed != "" {
		preview.EmbedURL = &embed
	}

	if result.ThumbnailURL != "" {
		if imgPath, imgMime, imgSize, err := downloadPreviewImage(ctx, cfg, result.ThumbnailURL); err == nil {
			preview.ImageFilePath = &imgPath
			preview.ImageMimeType = &imgMime
			preview.ImageSizeBytes = &imgSize
		}
	}

	return storage.UpsertPreview(ctx, preview)
}

// downloadPreviewImage baixa a imagem do preview (og:image / thumbnail_url)
// com o client SSRF-safe (robots check na origem da imagem, §6.4), valida o
// MIME por magic bytes, gera a thumbnail e salva em CAS
// (attachments/<ab>/<cd>/<sha256 da imagem>.preview.<ext>) — o thumbnail é o
// único artefato persistido (§6.2 passo 8).
func downloadPreviewImage(ctx context.Context, cfg *config.Config, rawImageURL string) (filePath, mime string, size int64, err error) {
	// THUMBNAIL_ENABLED=false: nenhuma imagem de preview (o preview segue
	// apenas com title/description — best-effort).
	if !cfg.ThumbnailEnabled {
		return "", "", 0, errors.New("processamento de thumbnail desabilitado")
	}
	u, err := utils.NormalizeURL(rawImageURL)
	if err != nil {
		return "", "", 0, err
	}
	if !RobotsAllowed(ctx, u) {
		return "", "", 0, errors.New("origem da imagem não permitida pelo robots.txt")
	}
	if !acquireOutboundSlot() {
		return "", "", 0, errors.New("semáforo outbound cheio")
	}
	defer releaseOutboundSlot()

	body, _, err := utils.SafeFetch(ctx, outboundHTTPClient(), maxPreviewImageBytes, u.String())
	if err != nil {
		return "", "", 0, err
	}

	mime = utils.DetectMimeType(body)
	if !isProcessableImage(mime) {
		return "", "", 0, errors.New("conteúdo não é uma imagem processável")
	}

	maxDim := cfg.ThumbnailMaxDim
	if mime == "image/gif" {
		maxDim = cfg.GIFThumbnailMaxDim
	}
	thumb, thumbMime, _, _, err := utils.GenerateThumbnail(body, maxDim, cfg.ThumbnailTimeout)
	if err != nil {
		return "", "", 0, err
	}

	// CAS: hash da imagem original (thumbnail derivada deterministicamente).
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	ext := "webp"
	if thumbMime == "image/gif" {
		ext = "gif"
	}
	filePath = filepath.Join(attachmentsBaseDir, hash[:2], hash[2:4], hash+".preview."+ext)
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return "", "", 0, fmt.Errorf("falha ao criar pasta da thumbnail: %w", err)
	}
	if err := os.WriteFile(filePath, thumb, 0o644); err != nil {
		return "", "", 0, fmt.Errorf("falha ao gravar a thumbnail: %w", err)
	}

	return filePath, thumbMime, int64(len(thumb)), nil
}

var whitespaceRe = regexp.MustCompile(`\s+`)

// parseOpenGraph extrai metadados do corpo HTML (já limitado a 5MB).
// Prioridade: og:* > twitter:* > <title>/<meta name=description> (§6.2
// passo 7). Imagem relativa é resolvida contra a URL da página.
func parseOpenGraph(body []byte, pageURL *url.URL) (title, description, image string) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return "", "", ""
	}

	var (
		ogTitle, ogDesc, ogImage string
		twTitle, twDesc, twImage string
		pageTitle, pageDesc      string
	)

	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "title":
				if pageTitle == "" {
					pageTitle = nodeText(n)
				}
			case "meta":
				var prop, name, content string
				for _, a := range n.Attr {
					switch strings.ToLower(a.Key) {
					case "property":
						prop = a.Val
					case "name":
						name = a.Val
					case "content":
						content = a.Val
					}
				}
				switch prop {
				case "og:title":
					if ogTitle == "" {
						ogTitle = content
					}
				case "og:description":
					if ogDesc == "" {
						ogDesc = content
					}
				case "og:image", "og:image:url", "og:image:secure_url":
					if ogImage == "" {
						ogImage = content
					}
				}
				switch name {
				case "twitter:title":
					if twTitle == "" {
						twTitle = content
					}
				case "twitter:description":
					if twDesc == "" {
						twDesc = content
					}
				case "twitter:image":
					if twImage == "" {
						twImage = content
					}
				case "description":
					if pageDesc == "" {
						pageDesc = content
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	title = firstNonEmpty(ogTitle, twTitle, pageTitle)
	description = firstNonEmpty(ogDesc, twDesc, pageDesc)
	image = firstNonEmpty(ogImage, twImage)

	// Imagem relativa → resolver contra a URL da página (normalizada).
	if image != "" {
		if ref, err := url.Parse(image); err == nil && !ref.IsAbs() {
			image = pageURL.ResolveReference(ref).String()
		}
	}

	return title, description, image
}

// nodeText retorna o texto concatenado do nó (descendente), com whitespace
// colapsado.
func nodeText(n *html.Node) string {
	var sb strings.Builder
	var walk func(c *html.Node)
	walk = func(c *html.Node) {
		if c.Type == html.TextNode {
			sb.WriteString(c.Data)
		}
		for ch := c.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(n)
	return strings.TrimSpace(whitespaceRe.ReplaceAllString(sb.String(), " "))
}

// firstNonEmpty retorna o primeiro string não vazio.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// truncateRune truncada a string para no máximo n runes (preserva UTF-8).
func truncateRune(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// nullableText converte string vazia para nil (campos *string do preview).
func nullableText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// GetLinkPreviewImage resolve a imagem de um preview com o MESMO check de
// acesso da mensagem à qual ele está vinculado (read_channel do canal).
// Preview inexistente, sem imagem ou sem vínculo com mensagem acessível →
// ErrPreviewNotFound (404, não vaza a existência).
func GetLinkPreviewImage(ctx context.Context, previewID, userID string) (models.LinkPreview, error) {
	if previewID == "" || userID == "" {
		return models.LinkPreview{}, ErrPreviewNotFound
	}

	preview, err := storage.GetPreviewByID(ctx, previewID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.LinkPreview{}, ErrPreviewNotFound
	}
	if err != nil {
		return models.LinkPreview{}, err
	}
	if preview.ImageFilePath == nil {
		return models.LinkPreview{}, ErrPreviewNotFound
	}

	channelID, err := storage.GetChannelIDByPreviewID(ctx, previewID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.LinkPreview{}, ErrPreviewNotFound
	}
	if err != nil {
		return models.LinkPreview{}, err
	}

	channel, err := storage.GetChannelByID(ctx, channelID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.LinkPreview{}, ErrChannelNotFound
	}
	if err != nil {
		return models.LinkPreview{}, err
	}

	allowed, err := userHasChannelPermission(ctx, channel, userID, true, func(p models.ChannelPermission) bool {
		return p.ReadChannel
	})
	if err != nil {
		return models.LinkPreview{}, err
	}
	if !allowed {
		// canal não acessível → 404 (mesma regra do spec: não vinculado a
		// mensagem acessível)
		return models.LinkPreview{}, ErrPreviewNotFound
	}

	return preview, nil
}
