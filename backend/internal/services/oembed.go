package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"papo/internal/utils"
)

// oembedProviders: host (lowercase, sem www) → template do endpoint ({url} é
// substituído pela URL alvo encoded). Match por host e subdomínios
// (m.youtube.com → youtube.com), §9.1.
var oembedProviders = map[string]string{
	"youtube.com":    "https://www.youtube.com/oembed?url={url}",
	"youtu.be":       "https://www.youtube.com/oembed?url={url}",
	"twitter.com":    "https://publish.x.com/oembed?url={url}",
	"x.com":          "https://publish.x.com/oembed?url={url}",
	"instagram.com":  "https://graph.facebook.com/v25.0/instagram_oembed?url={url}",
	"tiktok.com":     "https://www.tiktok.com/oembed?url={url}",
	"vimeo.com":      "https://vimeo.com/api/oembed.json?url={url}",
	"spotify.com":    "https://open.spotify.com/oembed?url={url}",
	"reddit.com":     "https://www.reddit.com/oembed?url={url}",
	"soundcloud.com": "https://soundcloud.com/oembed?url={url}&format=json",
	"flickr.com":     "https://www.flickr.com/services/oembed?url={url}&format=json",
}

// oembedResult é o subconjunto da resposta oEmbed usado (§9.2). O campo
// html NUNCA é lido/gravado (descartado por construção).
type oembedResult struct {
	Title        string `json:"title"`
	ProviderName string `json:"provider_name"`
	ThumbnailURL string `json:"thumbnail_url"`
	Type         string `json:"type"`
}

// normalizeHost lowercasa o host e remove os prefixos www./m. (para match de
// allowlist e extração de video ID).
func normalizeHost(host string) string {
	host = strings.ToLower(host)
	if h, ok := strings.CutPrefix(host, "www."); ok {
		host = h
	}
	if h, ok := strings.CutPrefix(host, "m."); ok {
		host = h
	}
	return host
}

// oembedProviderHost retorna o host da allowlist que casa com o host dado
// (exato ou subdomínio), ou "" se não houver.
func oembedProviderHost(host string) string {
	host = normalizeHost(host)
	for provider := range oembedProviders {
		if host == provider || strings.HasSuffix(host, "."+provider) {
			return provider
		}
	}
	return ""
}

// fetchOEmbed fetcha o endpoint oEmbed da URL alvo (isento de robots.txt,
// §6.4/§9.3 — é API do provedor, não crawl). Teto de 1MB no corpo. Qualquer
// falha → erro (o chamador faz fallback para OG, best-effort).
func fetchOEmbed(ctx context.Context, client *http.Client, target *url.URL) (oembedResult, error) {
	provider := oembedProviderHost(target.Hostname())
	if provider == "" {
		return oembedResult{}, errors.New("host fora da allowlist de oEmbed")
	}
	endpoint := strings.ReplaceAll(oembedProviders[provider], "{url}", url.QueryEscape(target.String()))

	body, _, err := utils.SafeFetch(ctx, client, 1<<20, endpoint)
	if err != nil {
		return oembedResult{}, err
	}

	var result oembedResult
	if err := json.Unmarshal(body, &result); err != nil {
		return oembedResult{}, fmt.Errorf("oEmbed: JSON inválido: %w", err)
	}

	return result, nil
}

var (
	// youtubeIDRe valida estritamente o video ID (§9.4): 11 chars [A-Za-z0-9_-].
	youtubeIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
)

// youtubeEmbedURL deriva o embed do YouTube de padrão hardcoded (§9.4), a
// partir da URL original (não da resposta oEmbed). Retorna "" quando o video
// ID não é extraível/inválido. Padrões aceitos:
//   - youtube.com/watch?v=ID (inclui m./www.)
//   - youtu.be/ID
//   - youtube.com/shorts/ID
//   - youtube.com/embed/ID
func youtubeEmbedURL(target *url.URL) string {
	var id string
	switch normalizeHost(target.Hostname()) {
	case "youtube.com":
		switch {
		case target.Path == "/watch":
			id = target.Query().Get("v")
		case strings.HasPrefix(target.Path, "/shorts/"):
			id = strings.TrimPrefix(target.Path, "/shorts/")
		case strings.HasPrefix(target.Path, "/embed/"):
			id = strings.TrimPrefix(target.Path, "/embed/")
		}
	case "youtu.be":
		id = strings.TrimPrefix(target.Path, "/")
	}

	if !youtubeIDRe.MatchString(id) {
		return ""
	}
	return "https://www.youtube.com/embed/" + id
}
