package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"
)

var errRedditDocument = errors.New("Reddit: documento sem publicação identificável")

type redditMetadata struct {
	title, description, author, community, canonical, image, method string
	video                                                           *models.EmbedMedia
	translated                                                      bool
}

// EN-US: Read the first-party embed document, never execute its scripts; oEmbed is a text-only fallback.
// PT-BR: Lê o documento de incorporação oficial sem executar scripts; o oEmbed é a alternativa textual.
func loadRedditMetadata(ctx context.Context, client *http.Client, original *url.URL) (redditMetadata, error) {
	target, ok := redditPostTarget(original)
	if !ok {
		return redditMetadata{}, errors.New("Reddit: URL de publicação inválida")
	}
	if target.share {
		resolved, err := resolveRedditShare(ctx, client, original)
		if err != nil {
			return redditMetadata{}, err
		}
		target, _ = redditPostTarget(resolved)
	}
	document := *target.url
	document.Host = "embed.reddit.com"
	documentCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	body, _, fetchErr := fetchRedditResource(documentCtx, client, &document, maxEmbedHTMLBytes, func(u *url.URL) bool {
		if u.Hostname() != "embed.reddit.com" {
			return false
		}
		copy := *u
		copy.Host = "www.reddit.com"
		post, valid := redditPostTarget(&copy)
		return valid && post.id == target.id && post.comment == target.comment
	})
	cancel()
	if fetchErr == nil {
		if metadata, err := parseRedditDocument(body, target); err == nil {
			if metadata.translated {
				// EN-US: Locale selects UI language, not an original-content guarantee. Recover the original title via oEmbed.
				// PT-BR: O locale seleciona o idioma da interface, sem garantir conteúdo original. Recupera o título pelo oEmbed.
				original, err := loadRedditOEmbedMetadata(ctx, client, target)
				if err != nil {
					return redditMetadata{}, err
				}
				metadata.title, metadata.description = original.title, ""
			}
			return metadata, nil
		}
	} else {
		var status *utils.HTTPStatusError
		if ctx.Err() != nil || errors.Is(fetchErr, utils.ErrSSRFBlocked) ||
			errors.As(fetchErr, &status) && (status.Status == 404 || status.Status == 410 || status.Status == 429) {
			return redditMetadata{}, fetchErr
		}
	}
	if err := ctx.Err(); err != nil {
		return redditMetadata{}, err
	}
	return loadRedditOEmbedMetadata(ctx, client, target)
}

func loadRedditOEmbedMetadata(ctx context.Context, client *http.Client, target redditTarget) (redditMetadata, error) {
	endpoint, _ := url.Parse("https://www.reddit.com/oembed")
	query := url.Values{"url": {target.url.String()}}
	endpoint.RawQuery = query.Encode()
	body, _, err := fetchRedditResource(ctx, client, endpoint, 1<<20, func(u *url.URL) bool {
		return u.Hostname() == "www.reddit.com" && u.Path == "/oembed"
	})
	if err != nil {
		return redditMetadata{}, fmt.Errorf("Reddit: metadados originais do oEmbed indisponíveis: %w", err)
	}
	var response oembedResult
	if err := json.Unmarshal(body, &response); err != nil {
		return redditMetadata{}, fmt.Errorf("Reddit: resposta oEmbed inválida: %w", err)
	}
	if !strings.EqualFold(response.ProviderName, "reddit") || !redditTitle(response.Title) {
		return redditMetadata{}, errRedditDocument
	}
	metadata := redditMetadata{title: response.Title, canonical: target.url.String(), community: target.community, method: embedFetchOEmbed}
	if redditToken(response.AuthorName, 128) {
		metadata.author = response.AuthorName
	}
	metadata.image = redditImageURL(response.ThumbnailURL)
	return metadata, nil
}

func fetchRedditPostEmbed(ctx context.Context, cfg *config.Config, original *url.URL) (models.Embed, error) {
	metadata, err := loadRedditMetadata(ctx, outboundHTTPClient(), original)
	if err != nil {
		return models.Embed{}, err
	}
	cacheKey, provider := original.String(), "Reddit"
	embed := models.Embed{
		SourceType: embedSourceLink, FetchMethod: metadata.method,
		CacheKey: &cacheKey, URL: &metadata.canonical, Provider: &provider,
		Title:       nullableText(truncateRune(metadata.title, embedTitleMax)),
		Description: nullableText(truncateRune(metadata.description, embedDescriptionMax)),
		Video:       metadata.video,
	}
	if metadata.community != "" {
		embed.SiteName = nullableText("r/" + metadata.community)
	}
	if metadata.author != "" && metadata.author != "[deleted]" {
		name, authorURL := "u/"+metadata.author, "https://www.reddit.com/user/"+url.PathEscape(metadata.author)+"/"
		embed.Author = &models.EmbedAuthor{Name: &name, URL: &authorURL}
	}
	if metadata.image != "" {
		client := scopedRedditClient(outboundHTTPClient(), func(u *url.URL) bool { return redditImageURL(u.String()) != "" })
		if media, err := downloadEmbedImageWithClient(ctx, cfg, metadata.image, client); err == nil {
			embed.Thumbnail = media
		} else {
			// EN-US: A failed thumbnail must not discard valid post metadata or silently hide its cause.
			// PT-BR: Uma miniatura falha não descarta metadados válidos nem oculta silenciosamente sua causa.
			utils.Errorf("Reddit: miniatura indisponível: %v", err)
		}
	}
	return storage.UpsertLinkEmbed(ctx, embed)
}

func fetchRedditResource(ctx context.Context, client *http.Client, target *url.URL, limit int64, allowed func(*url.URL) bool) ([]byte, *url.URL, error) {
	if !redditHTTPS(target) || !allowed(target) {
		return nil, nil, utils.ErrSSRFBlocked
	}
	if !acquireOutboundSlot() {
		return nil, nil, errors.New("semáforo outbound cheio")
	}
	defer releaseOutboundSlot()
	return utils.SafeFetch(ctx, scopedRedditClient(client, allowed), limit, target.String())
}

// EN-US: Scope each operation before its first request and every redirect, retaining the safe transport.
// PT-BR: Restringe cada operação antes da primeira requisição e de cada redirect, mantendo o transporte seguro.
func scopedRedditClient(client *http.Client, allowed func(*url.URL) bool) *http.Client {
	scoped := *client
	scoped.Jar = nil
	originalCheck := client.CheckRedirect
	scoped.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !redditHTTPS(req.URL) || !allowed(req.URL) {
			return utils.ErrSSRFBlocked
		}
		if originalCheck != nil {
			return originalCheck(req, via)
		}
		return nil
	}
	return &scoped
}

func resolveRedditShare(ctx context.Context, client *http.Client, original *url.URL) (*url.URL, error) {
	if !acquireOutboundSlot() {
		return nil, errors.New("semáforo outbound cheio")
	}
	defer releaseOutboundSlot()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, original.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", utils.SafeClientUserAgent)
	response, err := scopedRedditClient(client, isRedditPostURL).Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &utils.HTTPStatusError{Status: response.StatusCode}
	}
	// EN-US: The final URL resolves an opaque share token; do not read the site's challenge/HTML body.
	// PT-BR: A URL final resolve o token opaco de compartilhamento; não lê o desafio/HTML do site.
	target, valid := redditPostTarget(response.Request.URL)
	if !valid || target.share {
		return nil, errors.New("Reddit: link compartilhado não resolveu para uma publicação")
	}
	return target.url, nil
}

func redditVideoExpires(embed models.Embed, now time.Time) bool {
	if embed.Video == nil || embed.Video.URL == nil {
		return false
	}
	u, err := url.Parse(*embed.Video.URL)
	if err != nil || u.Hostname() != "packaged-media.redd.it" {
		return false
	}
	expires, err := strconv.ParseInt(u.Query().Get("e"), 10, 64)
	return err == nil && time.Unix(expires, 0).Before(now.Add(5*time.Minute))
}

func redditCacheNeedsRefresh(embed models.Embed, now time.Time) bool {
	if embed.CacheKey == nil {
		return false
	}
	u, err := url.Parse(*embed.CacheKey)
	if err != nil || !isRedditPostURL(u) {
		return false
	}
	return redditVideoExpires(embed, now) ||
		(embed.FetchMethod == embedFetchOEmbed || embed.Title == nil || !redditTitle(*embed.Title)) && embed.FetchedAt != nil && now.Sub(*embed.FetchedAt) >= 5*time.Minute
}
