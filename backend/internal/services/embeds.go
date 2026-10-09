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
	"unicode/utf8"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"

	"golang.org/x/net/html"
)

// ErrEmbedNotFound indica que o embed não existe, não tem a mídia pedida ou não
// está vinculado a mensagem acessível (o endpoint responde 404 nos três casos —
// não vazar a existência do embed).
var ErrEmbedNotFound = errors.New("embed não encontrado")

// Limites de embed (IMPLEMENTACAO_EMBEDS.md §5). Aplicados aos embeds
// customizados na validação e aos metadados externos antes de persistir.
//
// Contagem de caracteres: runes (pontos de código Unicode). É a mesma contagem
// usada pelo frontend ([...str].length em TypeScript), que mantém os limites
// consistentes entre Go e TypeScript.
const (
	maxEmbedsPerMessage = 10

	embedTitleMax       = 256
	embedDescriptionMax = 4192
	embedFieldsMax      = 25
	embedFieldNameMax   = 256
	embedFieldValueMax  = 1024
	embedAuthorNameMax  = 256
	embedFooterTextMax  = 2048
	embedTotalTextMax   = 6000
	embedURLMax         = 2048

	// site_name e provider não têm limite próprio na tabela de limites; usam o
	// mesmo teto do nome do autor para não deixar texto externo sem teto.
	embedSiteNameMax = embedAuthorNameMax
	embedProviderMax = embedAuthorNameMax
)

// Valores de source_type e fetch_method da tabela embeds.
const (
	embedSourceLink   = "link"
	embedSourceCustom = "custom"

	embedFetchOpenGraph = "opengraph"
	embedFetchOEmbed    = "oembed"
	embedFetchManual    = "manual"
)

// maxEmbedHTMLBytes é o teto do corpo HTML (pós-descompressão) do fetch de
// embed (§6.2 passo 6).
const maxEmbedHTMLBytes = 5 << 20

// maxEmbedImageBytes é o teto do download da imagem do embed (§6.2 passo 8).
const maxEmbedImageBytes = 5 << 20

// allowedVideoMIMEs é o allowlist de MIME type de vídeo direto (og:video e
// vídeo de embed customizado). Formato de página (text/html,
// application/x-shockwave-flash) não é vídeo: iframe só é renderizado para
// provedor allowlistado (embed_url derivado por padrão hardcoded).
var allowedVideoMIMEs = map[string]bool{
	"video/mp4":  true,
	"video/webm": true,
	"video/ogg":  true,
}

// Client SSRF-safe e semáforo de concorrência outbound (HTML, imagem, oEmbed e
// robots.txt contam no mesmo teto, §6.4).
var (
	outboundClientOnce sync.Once
	outboundClient     *http.Client
	outboundSem        chan struct{}

	embedVideoClientOnce sync.Once
	embedVideoClient     *http.Client
	embedVideoSem        chan struct{}
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

func initEmbedVideoProxy() {
	embedVideoClientOnce.Do(func() {
		cfg := config.LoadConfig()
		// Vídeo é servido por Range e pode ficar aberto por mais tempo que o
		// fetch curto de metadata/thumbnail.
		embedVideoClient = utils.SafeHTTPClient(utils.SafeClientOpts{Timeout: 5 * time.Minute})
		cap := cfg.OutboundMaxConc
		if cap <= 0 {
			cap = 4
		}
		embedVideoSem = make(chan struct{}, cap)
	})
}

func acquireEmbedVideoSlot() bool {
	initEmbedVideoProxy()
	select {
	case embedVideoSem <- struct{}{}:
		return true
	default:
		return false
	}
}

func releaseEmbedVideoSlot() {
	<-embedVideoSem
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
	embedRateOnce   sync.Once
	embedRateGlobal *tokenBucket
	embedRateUsers  sync.Map // userID → *tokenBucket
)

// embedRateUserTTL é o tempo máximo que um bucket por usuário fica no mapa sem
// uso: os obsoletos são removidos pela rotina de manutenção (sem isso o
// embedRateUsers cresce sem limite — 1 entrada por usuário que enviou link ou
// mídia de embed). O TTL é bem maior que o tempo de refill completo (~6min a
// 10/min), então remover um bucket não altera a taxa de um usuário ativo.
// (var: os testes usam um TTL curto.)
var embedRateUserTTL = time.Hour

// stale indica se o bucket está sem uso há mais que ttl (o último uso é a
// referência do refill).
func (b *tokenBucket) stale(ttl time.Duration) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Since(b.last) > ttl
}

// cleanupStaleEmbedRateBuckets remove dos buckets de rate limit de embed os que
// estão sem uso há mais que embedRateUserTTL (chamado pela rotina de
// manutenção). CompareAndDelete evita remover um bucket recém-recriado para o
// mesmo usuário. Retorna a quantidade removida.
func cleanupStaleEmbedRateBuckets() int {
	removed := 0
	embedRateUsers.Range(func(key, value any) bool {
		bucket := value.(*tokenBucket)
		if bucket.stale(embedRateUserTTL) && embedRateUsers.CompareAndDelete(key, bucket) {
			removed++
		}
		return true
	})
	return removed
}

// embedRateAllow consome token do bucket global (PREVIEW_FETCH_RATE_GLOBAL,
// 30/min) E do bucket por usuário (PREVIEW_FETCH_RATE_PER_USER, 10/min).
// Estourou algum → false (pular o fetch, best-effort). Fecha o cache-busting
// por URLs únicas (§6.2 passo 3) e limita também o download de mídia de embed
// customizado (mesmo vetor de abuso: URL outbound informada pelo cliente).
func embedRateAllow(userID string) bool {
	cfg := config.LoadConfig()
	embedRateOnce.Do(func() {
		embedRateGlobal = newTokenBucket(cfg.PreviewFetchRateGlob)
	})
	if !embedRateGlobal.take() {
		return false
	}

	var userBucket *tokenBucket
	if v, ok := embedRateUsers.Load(userID); ok {
		userBucket = v.(*tokenBucket)
	} else {
		userBucket = newTokenBucket(cfg.PreviewFetchRateUser)
		actual, _ := embedRateUsers.LoadOrStore(userID, userBucket)
		userBucket = actual.(*tokenBucket)
	}
	return userBucket.take()
}

var (
	embedURLRe      = regexp.MustCompile(`(?:https?://|www\.)\S+`)
	trailingPunctRe = regexp.MustCompile(`[.,;!?]+$`)
)

// extractEmbedURLs extrai até max URLs do content (regex conservadora
// https?://\S+), removendo pontuação de cauda e parênteses desbalanceados.
// Duplicatas (mesma string) são ignoradas; as primeiras prevalecem (§6.1).
func extractEmbedURLs(content string, max int) []string {
	if max <= 0 {
		max = 2
	}
	seen := make(map[string]struct{}, max)
	urls := make([]string, 0, max)
	for _, match := range embedURLRe.FindAllString(content, -1) {
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

// twitterStatusID identifica a URL de status do X/Twitter (host allowlistado +
// segmento /status/<id> numérico).
func twitterStatusID(u *url.URL) (string, bool) {
	switch strings.ToLower(u.Hostname()) {
	case "twitter.com", "www.twitter.com", "mobile.twitter.com",
		"x.com", "www.x.com",
		"fxtwitter.com", "www.fxtwitter.com",
		"fixupx.com", "www.fixupx.com", "girlcockx.com",
		"www.girlcockx.com":
	default:
		return "", false
	}

	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if !strings.EqualFold(parts[i], "status") {
			continue
		}
		id, err := url.PathUnescape(parts[i+1])
		if err != nil || id == "" {
			return "", false
		}
		for _, r := range id {
			if r < '0' || r > '9' {
				return "", false
			}
		}
		return id, true
	}
	return "", false
}

type fxTwitterStatusResponse struct {
	Code   int              `json:"code"`
	Status *fxTwitterStatus `json:"status"`
}

type fxTwitterStatus struct {
	Text   string         `json:"text"`
	Author fxTwitterUser  `json:"author"`
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
	URL          string `json:"url"`
	ThumbnailURL string `json:"thumbnail_url"`
}

type fxTwitterCard struct {
	Image *struct {
		URL string `json:"url"`
	} `json:"image"`
}

// normalizeTwitterVideoURL restringe o vídeo do X ao CDN oficial (hotlink
// direto a video.twimg.com é bloqueado por referrer; o relay autenticado é o
// único caminho) e descarta o parâmetro transitório ?tag=.
func normalizeTwitterVideoURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("URL de vídeo vazia")
	}

	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return "", errors.New("URL de vídeo inválida")
	}
	host := strings.ToLower(u.Hostname())
	if host != "video.twimg.com" && !strings.HasSuffix(host, ".video.twimg.com") {
		return "", errors.New("host de vídeo não permitido")
	}

	// O CDN do X pode rejeitar variantes com o parâmetro transitório ?tag=.
	q := u.Query()
	q.Del("tag")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func fxTwitterVideoURL(status *fxTwitterStatus) string {
	if status == nil || len(status.Media.Videos) == 0 {
		return ""
	}
	normalized, err := normalizeTwitterVideoURL(status.Media.Videos[0].URL)
	if err != nil {
		return ""
	}
	return normalized
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

// fetchFxTwitterEmbed obtém o embed de um status do X pela API JSON oficial do
// FxEmbed (api.fxtwitter.com/2), a superfície estável documentada pelo próprio
// projeto: os realms fxtwitter/fixupx alteram a resposta por User-Agent e
// podem redirecionar clientes humanos.
func fetchFxTwitterEmbed(ctx context.Context, cfg *config.Config, original *url.URL, statusID string) (models.Embed, error) {
	if statusID == "" {
		return models.Embed{}, errors.New("tweet id ausente")
	}
	if !acquireOutboundSlot() {
		return models.Embed{}, errors.New("semáforo outbound cheio")
	}
	body, _, err := utils.SafeFetch(
		ctx,
		outboundHTTPClient(),
		2<<20,
		"https://api.fxtwitter.com/2/status/"+url.PathEscape(statusID),
	)
	releaseOutboundSlot()
	if err != nil {
		return models.Embed{}, err
	}

	var payload fxTwitterStatusResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return models.Embed{}, fmt.Errorf("FxTwitter: JSON inválido: %w", err)
	}
	if payload.Code != http.StatusOK || payload.Status == nil {
		return models.Embed{}, fmt.Errorf("FxTwitter: status indisponível (code=%d)", payload.Code)
	}

	title := strings.TrimSpace(payload.Status.Author.Name)
	if handle := strings.TrimSpace(payload.Status.Author.ScreenName); handle != "" {
		if title != "" {
			title += " (@" + handle + ")"
		} else {
			title = "@" + handle
		}
	}
	provider := "X"

	cacheKey := original.String()
	embed := models.Embed{
		SourceType:  embedSourceLink,
		FetchMethod: embedFetchOpenGraph,
		CacheKey:    &cacheKey,
		URL:         &cacheKey,
		Title:       nullableText(truncateRune(title, embedTitleMax)),
		Description: nullableText(truncateRune(strings.TrimSpace(payload.Status.Text), embedDescriptionMax)),
		Provider:    &provider,
	}

	if videoURL := fxTwitterVideoURL(payload.Status); videoURL != "" {
		videoURLCopy := videoURL
		videoType := "video/mp4"
		embed.Video = &models.EmbedMedia{URL: &videoURLCopy, MimeType: &videoType}
	}
	if imageURL := fxTwitterImageURL(payload.Status); imageURL != "" {
		if media, err := downloadEmbedImage(ctx, cfg, imageURL); err == nil {
			embed.Thumbnail = media
		}
	}

	return storage.UpsertLinkEmbed(ctx, embed)
}

func embedRobotsAllowed(ctx context.Context, u *url.URL) bool {
	switch strings.ToLower(u.Hostname()) {
	case "fxtwitter.com", "www.fxtwitter.com", "api.fxtwitter.com",
		"fixupx.com", "www.fixupx.com",
		"pbs.twimg.com", "video.twimg.com", "girlcockx.com",
		"www.girlcockx.com":
		return true
	default:
		return RobotsAllowed(ctx, u)
	}
}

// GetOrCreateEmbed busca o embed em cache (cache_key normalizada, fetched_at
// recente) ou faz fetch + parse + thumbnail (§6.2). Best-effort: qualquer falha
// retorna erro (o chamador loga e segue sem embed).
//
// O segundo retorno indica se houve refetch (fetch + upsert, cache expirado ou
// URL nova): true significa que a row do embed foi atualizada e as mensagens já
// vinculadas a ele podem estar com o objeto defasado (evento
// message_embeds_update). Cache hit dentro do TTL → false.
//
// O ctx deve carregar o budget total da fase de embeds (compartilhado entre as
// URLs da mensagem, §6.1).
func GetOrCreateEmbed(ctx context.Context, userID, rawURL string) (models.Embed, bool, error) {
	cfg := config.LoadConfig()

	// 1. Parse + validação inicial (rejeição rápida sem gastar rede).
	u, err := utils.NormalizeURL(rawURL)
	if err != nil {
		return models.Embed{}, false, err
	}
	normalized := u.String()
	statusID, isTwitterStatus := twitterStatusID(u)

	// 2. Cache (mesma cache_key, fetched_at dentro do TTL).
	if cached, err := storage.GetEmbedByCacheKey(ctx, normalized); err == nil {
		age := time.Duration(0)
		if cached.FetchedAt != nil {
			age = time.Since(*cached.FetchedAt)
		} else {
			age = cfg.LinkPreviewCacheTTL + 1 // sem fetched_at: trata como expirado
		}
		if age < cfg.LinkPreviewCacheTTL {
			// Embeds antigos de X/Twitter podiam ter sido persistidos antes do
			// fetch via FxEmbed, sem thumbnail/vídeo. Damos uma janela curta
			// antes de tentar completar esse cache incompleto, sem transformar
			// tweets realmente text-only em fetch a cada mensagem.
			missingTwitterMedia := isTwitterStatus && cached.Thumbnail == nil && cached.Video == nil
			if !missingTwitterMedia || age < 5*time.Minute {
				return cached, false, nil
			}
		}
		// expirado ou X/Twitter sem mídia → refetch (o upsert atualiza a row)
	} else if !errors.Is(err, storage.ErrNotFound) {
		return models.Embed{}, false, err
	}

	// 3. Rate limit de URL-nova (global + por usuário).
	if !embedRateAllow(userID) {
		return models.Embed{}, false, errors.New("rate limit de embed estourado")
	}

	// 4. X/Twitter: API JSON oficial do FxEmbed (ver fetchFxTwitterEmbed).
	if isTwitterStatus {
		embed, err := fetchFxTwitterEmbed(ctx, cfg, u, statusID)
		return embed, err == nil, err
	}

	// 5. oEmbed first (host allowlistado) — falha → fallback para OpenGraph.
	if oembedProviderHost(u.Hostname()) != "" {
		if embed, err := fetchOEmbedLinkEmbed(ctx, cfg, u); err == nil {
			return embed, true, nil
		}
	}

	// 6. robots.txt da origem.
	if !embedRobotsAllowed(ctx, u) {
		return models.Embed{}, false, errors.New("origem não permitida pelo robots.txt")
	}

	// 7. Fetch HTML (teto 5MB pós-descompressão).
	if !acquireOutboundSlot() {
		return models.Embed{}, false, errors.New("semáforo outbound cheio")
	}
	body, finalURL, err := utils.SafeFetch(ctx, outboundHTTPClient(), maxEmbedHTMLBytes, u.String())
	releaseOutboundSlot()
	if err != nil {
		return models.Embed{}, false, err
	}

	// 7. Parse OpenGraph (og > twitter > fallbacks). URL relativa é resolvida
	// contra a URL final pós-redirects.
	metadata := parseOpenGraph(body, finalURL)

	normalizedCopy := normalized
	embed := models.Embed{
		SourceType:  embedSourceLink,
		FetchMethod: embedFetchOpenGraph,
		CacheKey:    &normalizedCopy,
		Title:       nullableText(truncateRune(metadata.title, embedTitleMax)),
		Description: nullableText(truncateRune(metadata.description, embedDescriptionMax)),
		SiteName:    nullableText(truncateRune(metadata.siteName, embedSiteNameMax)),
	}
	// og:url é a URL canônica declarada pelo site; ausente → a própria URL
	// normalizada (chave do cache).
	pageURL := metadata.canonicalURL
	if pageURL == "" {
		pageURL = normalized
	}
	if validated, err := validateEmbedURL(pageURL); err == nil {
		embed.URL = &validated
	} else {
		embed.URL = &normalizedCopy
	}

	// 8. Imagem (og:image): robots na origem da imagem + thumbnail na media.
	if metadata.imageURL != "" {
		if media, err := downloadEmbedImage(ctx, cfg, metadata.imageURL); err == nil {
			if metadata.imageWidth > 0 {
				media.Width = &metadata.imageWidth
			}
			if metadata.imageHeight > 0 {
				media.Height = &metadata.imageHeight
			}
			embed.Thumbnail = media
		}
		// sem imagem → embed só com title/description (aceitável, §6.2 passo 9)
	}

	// 9. Vídeo direto (og:video): HTTPS + MIME type de vídeo permitido. O relay
	// autenticado (GET /embeds/:embed_id/video) é o único caminho de playback.
	if metadata.videoURL != "" && allowedVideoMIMEs[metadata.videoType] {
		if validated, err := normalizeEmbedVideoURL(metadata.videoURL); err == nil {
			videoType := metadata.videoType
			videoURL := validated
			embed.Video = &models.EmbedMedia{
				URL:      &videoURL,
				MimeType: &videoType,
				Width:    intOrNil(metadata.videoWidth),
				Height:   intOrNil(metadata.videoHeight),
			}
		}
	}

	embed, err = storage.UpsertLinkEmbed(ctx, embed)
	return embed, err == nil, err
}

// fetchOEmbedLinkEmbed executa o fluxo oEmbed (§9.3): fetch do endpoint
// (isento de robots), parse do subconjunto permitido, thumbnail do
// thumbnail_url (com robots) e embed do YouTube (padrão hardcoded).
func fetchOEmbedLinkEmbed(ctx context.Context, cfg *config.Config, target *url.URL) (models.Embed, error) {
	if !acquireOutboundSlot() {
		return models.Embed{}, errors.New("semáforo outbound cheio")
	}
	result, err := fetchOEmbed(ctx, outboundHTTPClient(), target)
	releaseOutboundSlot()
	if err != nil {
		return models.Embed{}, err
	}

	targetURL := target.String()
	embed := models.Embed{
		SourceType:  embedSourceLink,
		FetchMethod: embedFetchOEmbed,
		CacheKey:    &targetURL,
		URL:         &targetURL,
		Title:       nullableText(truncateRune(result.Title, embedTitleMax)),
		Provider:    nullableText(truncateRune(result.ProviderName, embedProviderMax)),
		Author:      oEmbedAuthor(result),
	}

	// Embed do YouTube: derivado de padrão hardcoded da URL (§9.4), nunca do
	// campo html da resposta.
	if embedURL := youtubeEmbedURL(target); embedURL != "" {
		embedURLCopy := embedURL
		embed.Video = &models.EmbedMedia{URL: &embedURLCopy}
	}

	if result.ThumbnailURL != "" {
		if media, err := downloadEmbedImage(ctx, cfg, result.ThumbnailURL); err == nil {
			embed.Thumbnail = media
		}
	}

	return storage.UpsertLinkEmbed(ctx, embed)
}

func oEmbedAuthor(result oembedResult) *models.EmbedAuthor {
	name := nullableText(truncateRune(result.AuthorName, embedAuthorNameMax))
	// author_url vem do provedor: só http/https e dentro do teto de URL, para
	// não expor um esquema perigoso (ex.: javascript:) na API.
	var authorURL *string
	if validated, err := validateEmbedURL(result.AuthorURL); err == nil {
		authorURL = &validated
	}
	if name == nil && authorURL == nil {
		return nil
	}
	return &models.EmbedAuthor{Name: name, URL: authorURL}
}

// downloadEmbedImage baixa a imagem de um embed (og:image / thumbnail_url /
// mídia de embed customizado) com o client SSRF-safe (robots check na origem
// da imagem, §6.4), valida o MIME por magic bytes, gera a thumbnail e a grava
// na tabela media (content-addressable) — a thumbnail é o único artefato
// persistido (§6.2 passo 8). Retorna a mídia com hash, MIME e dimensões.
func downloadEmbedImage(ctx context.Context, cfg *config.Config, rawImageURL string) (*models.EmbedMedia, error) {
	// THUMBNAIL_ENABLED=false: nenhuma imagem de embed (o embed segue apenas
	// com title/description — best-effort).
	if !cfg.ThumbnailEnabled {
		return nil, errors.New("processamento de thumbnail desabilitado")
	}
	u, err := utils.NormalizeURL(rawImageURL)
	if err != nil {
		return nil, err
	}
	if !embedRobotsAllowed(ctx, u) {
		return nil, errors.New("origem da imagem não permitida pelo robots.txt")
	}
	if !acquireOutboundSlot() {
		return nil, errors.New("semáforo outbound cheio")
	}
	defer releaseOutboundSlot()

	body, _, err := utils.SafeFetch(ctx, outboundHTTPClient(), maxEmbedImageBytes, u.String())
	if err != nil {
		return nil, err
	}

	mime := utils.DetectMimeType(body)
	if !isProcessableImage(mime) {
		return nil, errors.New("conteúdo não é uma imagem processável")
	}

	maxDim := cfg.ThumbnailMaxDim
	if mime == "image/gif" {
		maxDim = cfg.GIFThumbnailMaxDim
	}
	thumb, thumbMime, width, height, err := utils.GenerateThumbnail(body, maxDim, cfg.ThumbnailTimeout)
	if err != nil {
		return nil, err
	}

	mediaSHA, _, err := StoreMediaFromBytes(ctx, thumb, thumbMime)
	if err != nil {
		return nil, fmt.Errorf("falha ao gravar a thumbnail: %w", err)
	}

	media := &models.EmbedMedia{MediaSHA: &mediaSHA, MimeType: &thumbMime}
	if width > 0 {
		media.Width = &width
	}
	if height > 0 {
		media.Height = &height
	}
	return media, nil
}

var whitespaceRe = regexp.MustCompile(`\s+`)

// openGraphMetadata é o subconjunto de metadados coletado do HTML (§3):
// título, descrição, URL canônica, site_name, imagem (+dimensões) e vídeo
// direto (+ tipo e dimensões).
type openGraphMetadata struct {
	title        string
	description  string
	canonicalURL string
	siteName     string

	imageURL    string
	imageWidth  int
	imageHeight int

	videoURL    string
	videoType   string
	videoWidth  int
	videoHeight int
}

// parseOpenGraph extrai metadados do corpo HTML (já limitado a 5MB).
// Prioridade: og:* > twitter:* > <title>/<meta name=description> (§6.2
// passo 7). URL relativa é resolvida contra a URL da página.
//
// Vídeo: og:video:secure_url > og:video:url > og:video — a URL segura é usada
// quando disponível e o resultado precisa ser HTTPS (og:video em http é
// descartado). og:video:type é exigido para o vídeo ser aceito.
func parseOpenGraph(body []byte, pageURL *url.URL) openGraphMetadata {
	var meta openGraphMetadata
	if len(body) == 0 {
		return meta
	}

	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return meta
	}

	var (
		ogTitle, ogDesc, ogURL, ogSiteName, ogImage     string
		ogImageWidth, ogImageHeight                     int
		ogVideoSecure, ogVideoURL, ogVideo, ogVideoType string
		ogVideoWidth, ogVideoHeight                     int
		twTitle, twDesc, twImage                        string
		pageTitle, pageDesc                             string
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
				case "og:url":
					if ogURL == "" {
						ogURL = content
					}
				case "og:site_name":
					if ogSiteName == "" {
						ogSiteName = content
					}
				case "og:image", "og:image:url", "og:image:secure_url":
					if ogImage == "" {
						ogImage = content
					}
				case "og:image:width":
					if ogImageWidth == 0 {
						ogImageWidth = parsePositiveInt(content)
					}
				case "og:image:height":
					if ogImageHeight == 0 {
						ogImageHeight = parsePositiveInt(content)
					}
				case "og:video:secure_url":
					if ogVideoSecure == "" {
						ogVideoSecure = content
					}
				case "og:video:url":
					if ogVideoURL == "" {
						ogVideoURL = content
					}
				case "og:video":
					if ogVideo == "" {
						ogVideo = content
					}
				case "og:video:type":
					if ogVideoType == "" {
						ogVideoType = content
					}
				case "og:video:width":
					if ogVideoWidth == 0 {
						ogVideoWidth = parsePositiveInt(content)
					}
				case "og:video:height":
					if ogVideoHeight == 0 {
						ogVideoHeight = parsePositiveInt(content)
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

	meta.title = firstNonEmpty(ogTitle, twTitle, pageTitle)
	meta.description = firstNonEmpty(ogDesc, twDesc, pageDesc)
	meta.siteName = strings.TrimSpace(ogSiteName)
	meta.canonicalURL = resolveRelativeURL(pageURL, firstNonEmpty(ogURL))
	meta.imageURL = resolveRelativeURL(pageURL, firstNonEmpty(ogImage, twImage))
	meta.imageWidth = ogImageWidth
	meta.imageHeight = ogImageHeight

	// URL segura primeiro; og:video direto só é aceito se for HTTPS.
	for _, candidate := range []string{ogVideoSecure, ogVideoURL, ogVideo} {
		resolved := resolveRelativeURL(pageURL, candidate)
		if resolved == "" {
			continue
		}
		if _, err := normalizeEmbedVideoURL(resolved); err != nil {
			continue
		}
		meta.videoURL = resolved
		break
	}
	meta.videoType = strings.ToLower(strings.TrimSpace(strings.Split(ogVideoType, ";")[0]))
	meta.videoWidth = ogVideoWidth
	meta.videoHeight = ogVideoHeight

	return meta
}

// resolveRelativeURL resolve uma URL relativa contra a URL da página e
// normaliza o resultado. Retorna "" quando a URL é ausente ou inválida.
func resolveRelativeURL(pageURL *url.URL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if !ref.IsAbs() && pageURL != nil {
		ref = pageURL.ResolveReference(ref)
	}
	normalized, err := utils.NormalizeURL(ref.String())
	if err != nil {
		return raw
	}
	return normalized.String()
}

// normalizeEmbedVideoURL valida a URL de vídeo direto: HTTPS, sem userinfo,
// porta padrão, dentro do limite de tamanho. Vídeo em http é rejeitado (o
// relay nunca faz downgrade de scheme).
func normalizeEmbedVideoURL(raw string) (string, error) {
	if utf8.RuneCountInString(raw) > embedURLMax {
		return "", errors.New("URL de vídeo excede o limite")
	}
	u, err := utils.NormalizeURL(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" {
		return "", errors.New("vídeo direto exige HTTPS")
	}
	return u.String(), nil
}

// parsePositiveInt lê uma dimensão de metadado (0 quando ausente ou inválida).
func parsePositiveInt(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	var value int
	if _, err := fmt.Sscanf(raw, "%d", &value); err != nil || value < 0 {
		return 0
	}
	return value
}

// intOrNil retorna o inteiro como ponteiro (nil quando 0 → campo ausente).
func intOrNil(value int) *int {
	if value <= 0 {
		return nil
	}
	return &value
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

// nullableText converte string vazia para nil (campos *string do embed).
func nullableText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// GetEmbed resolve um embed com o MESMO check de acesso da mensagem à qual ele
// está vinculado (read_channel do canal). Embed inexistente ou sem vínculo com
// mensagem acessível → ErrEmbedNotFound (404, não vaza a existência). A imagem
// não é obrigatória: o embed é retornado mesmo sem imagem (ThumbnailFilePath
// nil).
func GetEmbed(ctx context.Context, embedID, userID string) (models.Embed, error) {
	if embedID == "" || userID == "" {
		return models.Embed{}, ErrEmbedNotFound
	}

	embed, err := storage.GetEmbedByID(ctx, embedID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.Embed{}, ErrEmbedNotFound
	}
	if err != nil {
		return models.Embed{}, err
	}

	channelID, err := storage.GetChannelIDByEmbedID(ctx, embedID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.Embed{}, ErrEmbedNotFound
	}
	if err != nil {
		return models.Embed{}, err
	}

	channel, err := storage.GetChannelByID(ctx, channelID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.Embed{}, ErrChannelNotFound
	}
	if err != nil {
		return models.Embed{}, err
	}

	allowed, err := userHasChannelPermission(ctx, channel, userID, true, func(p models.ChannelPermission) bool {
		return p.ReadChannel
	})
	if err != nil {
		return models.Embed{}, err
	}
	if !allowed {
		// canal não acessível → 404 (mesma regra do spec: não vinculado a
		// mensagem acessível)
		return models.Embed{}, ErrEmbedNotFound
	}

	// O caminho do blob em disco é derivado do sha_hash (content-addressable).
	if embed.Thumbnail != nil && embed.Thumbnail.MediaSHA != nil {
		path := mediaBlobPath(*embed.Thumbnail.MediaSHA)
		embed.ThumbnailFilePath = &path
	}
	if embed.Author != nil && embed.Author.Media != nil && embed.Author.Media.MediaSHA != nil {
		path := mediaBlobPath(*embed.Author.Media.MediaSHA)
		embed.AuthorFilePath = &path
	}

	return embed, nil
}

// OpenEmbedVideo abre o vídeo remoto associado ao embed depois do mesmo check
// de read_channel usado por GetEmbed. O navegador nunca acessa o CDN
// diretamente; isso evita o 403 de hotlink/referrer.
//
// O vídeo é aceito somente com HTTPS e MIME type do allowlist; um redirect que
// termine em http (downgrade) ou num MIME que não é vídeo é rejeitado. O caller
// deve fechar resp.Body e chamar release.
func OpenEmbedVideo(
	ctx context.Context,
	embedID, userID, rangeHeader string,
) (resp *http.Response, release func(), err error) {
	embed, err := GetEmbed(ctx, embedID, userID)
	if err != nil {
		return nil, nil, err
	}
	if embed.Video == nil || embed.Video.URL == nil {
		return nil, nil, ErrEmbedNotFound
	}
	// O tipo armazenado precisa ser vídeo: um og:video de página (text/html) é
	// rejeitado antes de qualquer saída de rede.
	if embed.Video.MimeType != nil && !allowedVideoMIMEs[strings.ToLower(*embed.Video.MimeType)] {
		return nil, nil, ErrEmbedNotFound
	}

	videoURL, err := normalizeEmbedVideoURL(*embed.Video.URL)
	if err != nil {
		return nil, nil, ErrEmbedNotFound
	}
	if !acquireEmbedVideoSlot() {
		return nil, nil, errors.New("semáforo de vídeo cheio")
	}
	release = releaseEmbedVideoSlot

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, videoURL, nil)
	if err != nil {
		release()
		return nil, nil, err
	}
	req.Header.Set("User-Agent", utils.SafeClientUserAgent)
	req.Header.Set("Accept", "video/*,*/*;q=0.8")
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	// Não enviar Referer: CDNs de vídeo bloqueam hotlink externo.

	initEmbedVideoProxy()
	resp, err = embedVideoClient.Do(req)
	if err != nil {
		release()
		return nil, nil, err
	}

	// Redirect inseguro: o relay não aceita downgrade de scheme (a URL final
	// precisa continuar HTTPS).
	if resp.Request.URL == nil || resp.Request.URL.Scheme != "https" {
		resp.Body.Close()
		release()
		return nil, nil, ErrEmbedNotFound
	}

	if resp.StatusCode != http.StatusOK &&
		resp.StatusCode != http.StatusPartialContent &&
		resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		resp.Body.Close()
		release()
		return nil, nil, &utils.HTTPStatusError{Status: resp.StatusCode}
	}

	// O que chega precisa ser vídeo: um og:video que na verdade é página
	// (text/html) não é servido pelo relay.
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if !allowedVideoMIMEs[contentType] {
		resp.Body.Close()
		release()
		return nil, nil, ErrEmbedNotFound
	}

	return resp, release, nil
}
