package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"

	"golang.org/x/net/html"
)

type redditPostMetadata struct {
	title        string
	description  string
	subreddit    string
	author       string
	canonicalURL string
	imageURL     string
	imageWidth   int
	imageHeight  int
	score        string
	comments     string
}

// isRedditRobotsExemptHost dispensa a consulta ao robots.txt para as
// páginas públicas de posts e CDNs próprios de imagens do Reddit. A exceção
// é restrita a hosts conhecidos; URLs de outros domínios continuam usando
// RobotsAllowed, e SafeFetch mantém suas validações SSRF/HTTPS.
func isRedditRobotsExemptHost(host string) bool {
	switch strings.ToLower(strings.TrimSuffix(host, ".")) {
	case "reddit.com", "www.reddit.com", "old.reddit.com", "new.reddit.com",
		"m.reddit.com", "redd.it",
		"preview.redd.it", "external-preview.redd.it", "i.redd.it",
		"a.thumbs.redditmedia.com", "b.thumbs.redditmedia.com":
		return true
	default:
		return false
	}
}

// isRedditHost aceita somente os hosts públicos usados para páginas de posts.
// Não usa suffix matching genérico: reddit.com.attacker.example não é Reddit.
func isRedditHost(host string) bool {
	switch strings.ToLower(host) {
	case "reddit.com", "www.reddit.com", "old.reddit.com", "new.reddit.com",
		"m.reddit.com", "redd.it":
		return true
	default:
		return false
	}
}

// isRedditPostURL reconhece URLs de posts/comentários do Reddit e links curtos
// redd.it. Não encaminha páginas de subreddit, pesquisa, perfis ou endpoints
// JSON para o scraper de posts.
func isRedditPostURL(u *url.URL) bool {
	if u == nil || !strings.EqualFold(u.Scheme, "https") || u.User != nil ||
		(u.Port() != "" && u.Port() != "443") || !isRedditHost(u.Hostname()) {
		return false
	}

	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return false
	}
	for _, part := range parts {
		if strings.HasSuffix(strings.ToLower(part), ".json") {
			return false
		}
	}
	if strings.EqualFold(u.Hostname(), "redd.it") {
		return len(parts) == 1 && redditPathToken(parts[0])
	}

	if strings.EqualFold(parts[0], "comments") {
		return len(parts) >= 2 && redditPathToken(parts[1])
	}
	if !strings.EqualFold(parts[0], "r") || len(parts) < 3 || !redditPathToken(parts[1]) {
		return false
	}
	switch strings.ToLower(parts[2]) {
	case "comments":
		return len(parts) >= 4 && redditPathToken(parts[3])
	case "s":
		// Links de compartilhamento /r/<subreddit>/s/<token> redirecionam
		// para o post canônico durante o fetch.
		return len(parts) >= 4 && redditPathToken(parts[3])
	default:
		return false
	}
}

func redditPathToken(token string) bool {
	if token == "" || strings.HasSuffix(strings.ToLower(token), ".json") {
		return false
	}
	for _, r := range token {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// EN-US: Validate every Reddit redirect before fetching it, retaining the safe transport and caller's policy.
// PT-BR: Valida cada redirect do Reddit antes da busca, mantendo o transporte seguro e a política do chamador.
func redditPostHTTPClient(client *http.Client) *http.Client {
	scoped := *client
	scoped.Jar = nil
	originalCheck := client.CheckRedirect
	scoped.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !isRedditPostURL(req.URL) {
			return fmt.Errorf("%w: redirect de Reddit inválido", utils.ErrSSRFBlocked)
		}
		if originalCheck != nil {
			return originalCheck(req, via)
		}
		return nil
	}
	return &scoped
}

// fetchRedditPostEmbed lê a página HTML pública do post, extrai os metadados
// renderizados pelo Reddit e baixa a imagem Open Graph para a media local.
// Não usa o endpoint oEmbed nem endpoints .json.
func fetchRedditPostEmbed(ctx context.Context, cfg *config.Config, target *url.URL) (models.Embed, error) {
	if !isRedditPostURL(target) {
		return models.Embed{}, errors.New("URL de post do Reddit não permitida")
	}
	if !embedRobotsAllowed(ctx, target) {
		return models.Embed{}, errors.New("post do Reddit não permitido pelo robots.txt")
	}
	if !acquireOutboundSlot() {
		return models.Embed{}, errors.New("semáforo outbound cheio")
	}
	body, finalURL, err := utils.SafeFetch(ctx, redditPostHTTPClient(outboundHTTPClient()), maxEmbedHTMLBytes, target.String())
	releaseOutboundSlot()
	if err != nil {
		return models.Embed{}, fmt.Errorf("Reddit: falha ao buscar HTML do post: %w", err)
	}
	if !isRedditHost(finalURL.Hostname()) || !isRedditPostURL(finalURL) {
		return models.Embed{}, errors.New("Reddit: redirect não terminou em uma página de post permitida")
	}

	metadata, err := parseRedditPostHTML(body, finalURL)
	if err != nil {
		return models.Embed{}, err
	}
	if metadata.title == "" {
		return models.Embed{}, errors.New("Reddit: título do post não encontrado no HTML")
	}

	cacheKey := target.String()
	pageURL := metadata.canonicalURL
	if pageURL == "" || !isRedditPostURL(mustNormalizeRedditURL(pageURL)) {
		pageURL = finalURL.String()
	}
	provider := "Reddit"
	siteName := ""
	if metadata.subreddit != "" {
		siteName = "r/" + metadata.subreddit
	}
	embed := models.Embed{
		SourceType:  embedSourceLink,
		FetchMethod: embedFetchOpenGraph,
		CacheKey:    &cacheKey,
		URL:         &pageURL,
		Title:       nullableText(truncateRune(metadata.title, embedTitleMax)),
		Description: nullableText(truncateRune(metadata.description, embedDescriptionMax)),
		Provider:    &provider,
		SiteName:    nullableText(truncateRune(siteName, embedSiteNameMax)),
	}
	if metadata.score != "" {
		embed.Fields = append(embed.Fields, models.EmbedField{
			Position: len(embed.Fields),
			Name:     "Votos",
			Value:    truncateRune(metadata.score, embedFieldValueMax),
		})
	}
	if metadata.comments != "" {
		embed.Fields = append(embed.Fields, models.EmbedField{
			Position: len(embed.Fields),
			Name:     "Comentários",
			Value:    truncateRune(metadata.comments, embedFieldValueMax),
		})
	}
	if metadata.author != "" && metadata.author != "[deleted]" {
		authorName := "u/" + metadata.author
		authorURL := "https://www.reddit.com/user/" + url.PathEscape(metadata.author) + "/"
		embed.Author = &models.EmbedAuthor{
			Name: nullableText(truncateRune(authorName, embedAuthorNameMax)),
			URL:  &authorURL,
		}
	}
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
	}

	return storage.UpsertLinkEmbed(ctx, embed)
}

func mustNormalizeRedditURL(raw string) *url.URL {
	u, err := utils.NormalizeURL(raw)
	if err != nil {
		return nil
	}
	return u
}

// parseRedditPostHTML extracts metadata from server-rendered Reddit HTML.
// Open Graph tags are preferred for the thumbnail; the shreddit-post custom
// element provides stable post fields when the meta description is absent.
func parseRedditPostHTML(body []byte, pageURL *url.URL) (redditPostMetadata, error) {
	var meta redditPostMetadata
	if len(body) == 0 {
		return meta, errors.New("Reddit: página HTML vazia")
	}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return meta, fmt.Errorf("Reddit: HTML inválido: %w", err)
	}

	var (
		ogTitle, ogDescription, ogURL, ogImage, twitterImage, htmlDescription string
		postThumbnail                                                         string
		ogImageWidth, ogImageHeight                                           int
	)
	var walk func(*html.Node, bool)
	walk = func(n *html.Node, insidePost bool) {
		if n.Type == html.ElementNode {
			if strings.EqualFold(n.Data, "shreddit-post") {
				insidePost = true
				if meta.title == "" {
					meta.title = htmlNodeAttr(n, "post-title")
				}
				if meta.subreddit == "" {
					meta.subreddit = normalizeRedditSubreddit(firstNonEmpty(
						htmlNodeAttr(n, "subreddit-prefixed-name"),
						htmlNodeAttr(n, "subreddit-name"),
					))
				}
				if meta.author == "" {
					meta.author = strings.TrimPrefix(htmlNodeAttr(n, "author"), "u/")
				}
				if meta.score == "" {
					meta.score = htmlNodeAttr(n, "score")
				}
				if meta.comments == "" {
					meta.comments = firstNonEmpty(
						htmlNodeAttr(n, "comment-count"),
						htmlNodeAttr(n, "num-comments"),
						htmlNodeAttr(n, "comment_count"),
					)
				}
				if meta.canonicalURL == "" {
					meta.canonicalURL = resolveRelativeURL(pageURL, htmlNodeAttr(n, "permalink"))
				}
				if postThumbnail == "" {
					postThumbnail = firstNonEmpty(
						redditImageCandidate(htmlNodeAttr(n, "thumbnail")),
						redditImageCandidate(htmlNodeAttr(n, "thumbnail-url")),
						redditImageCandidate(htmlNodeAttr(n, "post-thumbnail")),
						redditImageCandidate(htmlNodeAttr(n, "preview-url")),
					)
				}
			}
			if n.Data == "meta" {
				prop, name, content := "", "", ""
				for _, attr := range n.Attr {
					switch strings.ToLower(attr.Key) {
					case "property":
						prop = strings.ToLower(attr.Val)
					case "name":
						name = strings.ToLower(attr.Val)
					case "content":
						content = strings.TrimSpace(attr.Val)
					}
				}
				switch prop {
				case "og:title":
					if ogTitle == "" {
						ogTitle = content
					}
				case "og:description":
					if ogDescription == "" {
						ogDescription = content
					}
				case "og:url":
					if ogURL == "" {
						ogURL = content
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
				}
				switch name {
				case "twitter:image", "twitter:image:src":
					if twitterImage == "" {
						twitterImage = content
					}
				case "description":
					if htmlDescription == "" {
						htmlDescription = content
					}
				case "author":
					if meta.author == "" {
						meta.author = strings.TrimPrefix(content, "u/")
					}
				}
			}
			if n.Data == "title" && ogTitle == "" {
				ogTitle = nodeText(n)
			}
			if insidePost {
				if slot := htmlNodeAttr(n, "slot"); slot == "text-body" && meta.description == "" {
					meta.description = nodeText(n)
				}
				if postThumbnail == "" {
					slot := htmlNodeAttr(n, "slot")
					testID := htmlNodeAttr(n, "data-testid")
					if (n.Data == "img" && (htmlNodeAttr(n, "id") == "post-image" ||
						testID == "post-image" || slot == "thumbnail")) ||
						(n.Data == "faceplate-img" && (slot == "thumbnail" || testID == "post-thumbnail")) {
						postThumbnail = redditImageCandidate(firstNonEmpty(
							htmlNodeAttr(n, "src"),
							htmlNodeAttr(n, "data-src"),
							htmlNodeAttr(n, "image-url"),
						))
					}
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child, insidePost)
		}
	}
	walk(doc, false)

	if meta.title == "" {
		meta.title = stripRedditTitleSuffix(ogTitle)
	}
	if meta.description == "" {
		meta.description = firstNonEmpty(ogDescription, htmlDescription)
	}
	if meta.canonicalURL == "" {
		meta.canonicalURL = resolveRelativeURL(pageURL, ogURL)
	}
	if meta.subreddit == "" {
		meta.subreddit = redditSubredditFromURL(pageURL)
	}
	if meta.imageURL == "" {
		meta.imageURL = firstNonEmpty(ogImage, twitterImage, postThumbnail)
	}
	if meta.imageURL != "" {
		meta.imageURL = resolveRelativeURL(pageURL, meta.imageURL)
	}
	meta.imageWidth = ogImageWidth
	meta.imageHeight = ogImageHeight

	meta.title = strings.TrimSpace(meta.title)
	meta.description = strings.TrimSpace(meta.description)
	meta.author = strings.TrimPrefix(strings.TrimSpace(meta.author), "u/")
	meta.canonicalURL = strings.TrimSpace(meta.canonicalURL)
	return meta, nil
}

func redditImageCandidate(raw string) string {
	raw = strings.TrimSpace(raw)
	switch strings.ToLower(raw) {
	case "", "self", "nsfw", "default", "image", "spoiler":
		return ""
	}
	if strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://") ||
		strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, "/") {
		return raw
	}
	return ""
}

func htmlNodeAttr(n *html.Node, key string) string {
	for _, attr := range n.Attr {
		if strings.EqualFold(attr.Key, key) {
			return strings.TrimSpace(attr.Val)
		}
	}
	return ""
}

func normalizeRedditSubreddit(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "r/")
	return value
}

func redditSubredditFromURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) >= 2 && strings.EqualFold(parts[0], "r") {
		return parts[1]
	}
	return ""
}

func stripRedditTitleSuffix(title string) string {
	title = strings.TrimSpace(title)
	if idx := strings.LastIndex(title, " : r/"); idx >= 0 {
		title = strings.TrimSpace(title[:idx])
	}
	return title
}
