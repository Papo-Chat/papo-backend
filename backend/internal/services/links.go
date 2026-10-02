package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
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

// previewRateUserTTL é o tempo máximo que um bucket por usuário fica no mapa
// sem uso: os obsoletos são removidos pela rotina de manutenção (sem isso o
// previewRateUsers cresce sem limite — 1 entrada por usuário que enviou
// link). O TTL é bem maior que o tempo de refill completo (~6min a 10/min),
// então remover um bucket não altera a taxa de um usuário ativo. (var: os
// testes usam um TTL curto.)
var previewRateUserTTL = time.Hour

// stale indica se o bucket está sem uso há mais que ttl (o último uso é a
// referência do refill).
func (b *tokenBucket) stale(ttl time.Duration) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Since(b.last) > ttl
}

// cleanupStalePreviewRateBuckets remove dos buckets de preview por usuário os
// que estão sem uso há mais que previewRateUserTTL (chamado pela rotina de
// manutenção). CompareAndDelete evita remover um bucket recém-recriado para
// o mesmo usuário. Retorna a quantidade removida.
func cleanupStalePreviewRateBuckets() int {
	removed := 0
	previewRateUsers.Range(func(key, value any) bool {
		bucket := value.(*tokenBucket)
		if bucket.stale(previewRateUserTTL) && previewRateUsers.CompareAndDelete(key, bucket) {
			removed++
		}
		return true
	})
	return removed
}

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
	previewURLRe    = regexp.MustCompile(`(?:https?://|www\.)\S+`)
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
		// URL sem scheme (www.) → assume https (nunca http).
		if !strings.HasPrefix(cleaned, "http://") && !strings.HasPrefix(cleaned, "https://") {
			cleaned = "https://" + cleaned
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
// O segundo retorno indica se houve refetch (fetch + upsert, cache expirado
// ou URL nova): true significa que a row do preview foi atualizada e as
// mensagens já vinculadas a ele podem estar com o objeto defasado (evento
// link_preview_update). Cache hit dentro do TTL → false.
//
// O ctx deve carregar o budget total da fase de previews (compartilhado
// entre as URLs da mensagem, §6.1).
type twitterStatusRef struct {
	ID     string
	Handle string
}

func twitterStatusParts(u *url.URL) (twitterStatusRef, bool) {
	switch strings.ToLower(u.Hostname()) {
	case "twitter.com", "www.twitter.com", "mobile.twitter.com",
		"x.com", "www.x.com",
		"fxtwitter.com", "www.fxtwitter.com",
		"fixupx.com", "www.fixupx.com":
	default:
		return twitterStatusRef{}, false
	}

	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if !strings.EqualFold(parts[i], "status") {
			continue
		}
		id, err := url.PathUnescape(parts[i+1])
		if err != nil || id == "" {
			return twitterStatusRef{}, false
		}
		for _, r := range id {
			if r < '0' || r > '9' {
				return twitterStatusRef{}, false
			}
		}

		handle := ""
		if i > 0 {
			candidate, err := url.PathUnescape(parts[i-1])
			if err == nil && !strings.EqualFold(candidate, "i") {
				valid := candidate != ""
				for _, r := range candidate {
					if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
						(r >= '0' && r <= '9') || r == '_') {
						valid = false
						break
					}
				}
				if valid {
					handle = candidate
				}
			}
		}
		return twitterStatusRef{ID: id, Handle: handle}, true
	}
	return twitterStatusRef{}, false
}

func twitterStatusID(u *url.URL) (string, bool) {
	ref, ok := twitterStatusParts(u)
	return ref.ID, ok
}

type fxTwitterStatusResponse struct {
	Code   int              `json:"code"`
	Status *fxTwitterStatus `json:"status"`
}

type fxTwitterLegacyResponse struct {
	Code    int              `json:"code"`
	Message string           `json:"message"`
	Tweet   *fxTwitterStatus `json:"tweet"`
}

type fxTwitterStatus struct {
	Text   string        `json:"text"`
	Author fxTwitterUser `json:"author"`
	Media  fxTwitterMedia `json:"media"`
	Card   *fxTwitterCard `json:"card"`
}

type fxTwitterUser struct {
	Name       string `json:"name"`
	ScreenName string `json:"screen_name"`
}

type fxTwitterMedia struct {
	Photos   []fxTwitterPhoto `json:"photos"`
	Videos   []fxTwitterVideo `json:"videos"`
	External *struct {
		ThumbnailURL string `json:"thumbnail_url"`
	} `json:"external"`
}

type fxTwitterPhoto struct {
	URL string `json:"url"`
}

type fxTwitterVideo struct {
	ThumbnailURL string `json:"thumbnail_url"`
}

type fxTwitterCard struct {
	Image *struct {
		URL string `json:"url"`
	} `json:"image"`
}

func fxTwitterImageURL(status *fxTwitterStatus) string {
	if status == nil {
		return ""
	}
	if len(status.Media.Photos) > 0 && status.Media.Photos[0].URL != "" {
		return status.Media.Photos[0].URL
	}
	if len(status.Media.Videos) > 0 && status.Media.Videos[0].ThumbnailURL != "" {
		return status.Media.Videos[0].ThumbnailURL
	}
	if status.Media.External != nil && status.Media.External.ThumbnailURL != "" {
		return status.Media.External.ThumbnailURL
	}
	if status.Card != nil && status.Card.Image != nil {
		return status.Card.Image.URL
	}
	return ""
}

func fetchFxTwitterJSON(ctx context.Context, endpoint string, out any) error {
	if !acquireOutboundSlot() {
		return errors.New("semáforo outbound cheio")
	}
	body, _, err := utils.SafeFetch(ctx, outboundHTTPClient(), 2<<20, endpoint)
	releaseOutboundSlot()
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("FxTwitter: JSON inválido: %w", err)
	}
	return nil
}

func fetchFxTwitterStatus(ctx context.Context, ref twitterStatusRef) (*fxTwitterStatus, error) {
	// O endpoint legado continua sendo mantido pelo FxEmbed e passa pelo
	// pipeline usado pelos embeds. Para preview de um único post ele é a rota
	// mais direta e, diferente do realm HTML, já devolve JSON.
	legacyPath := "/status/" + url.PathEscape(ref.ID)
	if ref.Handle != "" {
		legacyPath = "/" + url.PathEscape(ref.Handle) + "/status/" + url.PathEscape(ref.ID)
	}
	var legacy fxTwitterLegacyResponse
	legacyErr := fetchFxTwitterJSON(ctx, "https://api.fxtwitter.com"+legacyPath, &legacy)
	if legacyErr == nil && legacy.Code == http.StatusOK && legacy.Tweet != nil {
		return legacy.Tweet, nil
	}

	// API v2 fica como fallback para manter compatibilidade futura caso o
	// endpoint legado deixe de responder.
	var v2 fxTwitterStatusResponse
	v2URL := "https://api.fxtwitter.com/2/status/" + url.PathEscape(ref.ID)
	v2Err := fetchFxTwitterJSON(ctx, v2URL, &v2)
	if v2Err == nil && v2.Code == http.StatusOK && v2.Status != nil {
		return v2.Status, nil
	}

	if legacyErr != nil && v2Err != nil {
		return nil, fmt.Errorf("FxTwitter v1 falhou: %v; fallback v2 falhou: %v", legacyErr, v2Err)
	}
	return nil, fmt.Errorf(
		"FxTwitter: post indisponível (v1 code=%d, v2 code=%d, v1err=%v, v2err=%v)",
		legacy.Code, v2.Code, legacyErr, v2Err,
	)
}

func fetchFxTwitterPreview(ctx context.Context, cfg *config.Config, original *url.URL, ref twitterStatusRef) (models.LinkPreview, error) {
	if ref.ID == "" {
		return models.LinkPreview{}, errors.New("tweet id ausente")
	}

	status, err := fetchFxTwitterStatus(ctx, ref)
	if err != nil {
		return models.LinkPreview{}, err
	}

	title := strings.TrimSpace(status.Author.Name)
	if handle := strings.TrimSpace(status.Author.ScreenName); handle != "" {
		if title != "" {
			title += " (@" + handle + ")"
		} else {
			title = "@" + handle
		}
	}
	provider := "X"

	preview := models.LinkPreview{
		URL:          original.String(),
		Kind:         "og",
		Title:        nullableText(truncateRune(title, cfg.PreviewTitleMax)),
		Description:  nullableText(truncateRune(strings.TrimSpace(status.Text), cfg.PreviewDescriptionMax)),
		ProviderName: &provider,
	}

	if imageURL := fxTwitterImageURL(status); imageURL != "" {
		if imgMedia, err := downloadPreviewImage(ctx, cfg, imageURL); err == nil {
			preview.ImageMedia = &imgMedia
		}
	}

	return storage.UpsertPreview(ctx, preview)
}

func previewRobotsAllowed(ctx context.Context, u *url.URL) bool {
	switch strings.ToLower(u.Hostname()) {
	case "fxtwitter.com", "www.fxtwitter.com", "api.fxtwitter.com",
		"fixupx.com", "www.fixupx.com",
		"pbs.twimg.com", "video.twimg.com":
		return true
	default:
		return RobotsAllowed(ctx, u)
	}
}

func GetOrCreatePreview(ctx context.Context, userID, rawURL string) (models.LinkPreview, bool, error) {
	cfg := config.LoadConfig()

	// 1. Parse + validação inicial (rejeição rápida sem gastar rede).
	u, err := utils.NormalizeURL(rawURL)
	if err != nil {
		return models.LinkPreview{}, false, err
	}
	normalized := u.String()

	// 2. Cache (mesma URL normalizada, fetched_at dentro do TTL).
	if cached, err := storage.GetPreviewByURL(ctx, normalized); err == nil {
		if time.Since(cached.FetchedAt) < cfg.LinkPreviewCacheTTL {
			return cached, false, nil
		}
		// expirado → refetch (fall through; o upsert atualiza a row)
	} else if !errors.Is(err, storage.ErrNotFound) {
		return models.LinkPreview{}, false, err
	}

	// 3. Rate limit de URL-nova (global + por usuário).
	if !previewRateAllow(userID) {
		return models.LinkPreview{}, false, errors.New("rate limit de preview estourado")
	}

	// 4. X/Twitter: use a API JSON oficial do FxEmbed em vez de scraping do
	// realm fxtwitter/fixupx. Esses hosts alteram a resposta por User-Agent e
	// podem redirecionar clientes humanos, enquanto api.fxtwitter.com/2 é a
	// superfície estável documentada pelo próprio projeto.
	if statusRef, ok := twitterStatusParts(u); ok {
		preview, err := fetchFxTwitterPreview(ctx, cfg, u, statusRef)
		return preview, err == nil, err
	}

	// 5. oEmbed first (host allowlistado) — falha → fallback para OG.
	if oembedProviderHost(u.Hostname()) != "" {
		if preview, err := fetchOEmbedPreview(ctx, cfg, u); err == nil {
			return preview, true, nil
		}
	}

	// 6. robots.txt da origem.
	if !previewRobotsAllowed(ctx, u) {
		return models.LinkPreview{}, false, errors.New("origem não permitida pelo robots.txt")
	}

	// 7. Fetch HTML (teto 5MB pós-descompressão).
	if !acquireOutboundSlot() {
		return models.LinkPreview{}, false, errors.New("semáforo outbound cheio")
	}
	body, finalURL, err := utils.SafeFetch(ctx, outboundHTTPClient(), maxPreviewHTMLBytes, u.String())
	releaseOutboundSlot()
	if err != nil {
		return models.LinkPreview{}, false, err
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

	// 8. Imagem (og:image): robots na origem da imagem + thumbnail na media.
	if imageURL != "" {
		if imgMedia, err := downloadPreviewImage(ctx, cfg, imageURL); err == nil {
			preview.ImageMedia = &imgMedia
		}
		// sem imagem → preview só com title/description (aceitável, §6.2 passo 9)
	}

	preview, err = storage.UpsertPreview(ctx, preview)
	return preview, err == nil, err
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
		if imgMedia, err := downloadPreviewImage(ctx, cfg, result.ThumbnailURL); err == nil {
			preview.ImageMedia = &imgMedia
		}
	}

	return storage.UpsertPreview(ctx, preview)
}

// downloadPreviewImage baixa a imagem do preview (og:image / thumbnail_url)
// com o client SSRF-safe (robots check na origem da imagem, §6.4), valida o
// MIME por magic bytes, gera a thumbnail e a grava na tabela media
// (content-addressable) — a thumbnail é o único artefato persistido
// (§6.2 passo 8). Retorna o sha256 (hex) do blob gravado.
func downloadPreviewImage(ctx context.Context, cfg *config.Config, rawImageURL string) (mediaSha string, err error) {
	// THUMBNAIL_ENABLED=false: nenhuma imagem de preview (o preview segue
	// apenas com title/description — best-effort).
	if !cfg.ThumbnailEnabled {
		return "", errors.New("processamento de thumbnail desabilitado")
	}
	u, err := utils.NormalizeURL(rawImageURL)
	if err != nil {
		return "", err
	}
	if !previewRobotsAllowed(ctx, u) {
		return "", errors.New("origem da imagem não permitida pelo robots.txt")
	}
	if !acquireOutboundSlot() {
		return "", errors.New("semáforo outbound cheio")
	}
	defer releaseOutboundSlot()

	body, _, err := utils.SafeFetch(ctx, outboundHTTPClient(), maxPreviewImageBytes, u.String())
	if err != nil {
		return "", err
	}

	mime := utils.DetectMimeType(body)
	if !isProcessableImage(mime) {
		return "", errors.New("conteúdo não é uma imagem processável")
	}

	maxDim := cfg.ThumbnailMaxDim
	if mime == "image/gif" {
		maxDim = cfg.GIFThumbnailMaxDim
	}
	thumb, thumbMime, _, _, err := utils.GenerateThumbnail(body, maxDim, cfg.ThumbnailTimeout)
	if err != nil {
		return "", err
	}

	mediaSha, _, err = StoreMediaFromBytes(ctx, thumb, thumbMime)
	if err != nil {
		return "", fmt.Errorf("falha ao gravar a thumbnail: %w", err)
	}

	return mediaSha, nil
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

// GetLinkPreview resolve um preview com o MESMO check de acesso da mensagem à
// qual ele está vinculado (read_channel do canal). Preview inexistente ou sem
// vínculo com mensagem acessível → ErrPreviewNotFound (404, não vaza a
// existência). A imagem não é obrigatória: o preview é retornado mesmo sem
// imagem (ImageFilePath nil).
func GetLinkPreview(ctx context.Context, previewID, userID string) (models.LinkPreview, error) {
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

	// O caminho do blob em disco é derivado do sha_hash (content-addressable).
	if preview.ImageMedia != nil {
		path := mediaBlobPath(*preview.ImageMedia)
		preview.ImageFilePath = &path
	}

	return preview, nil
}
