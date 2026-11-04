package services

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"time"

	"papo/internal/models"

	"golang.org/x/net/html"
)

// EN-US: Stream only allowlisted metadata/text from the embed DOM. Scripts, trackers and arbitrary links are ignored.
// PT-BR: Lê somente metadados/texto permitidos do DOM de incorporação. Ignora scripts, rastreadores e links arbitrários.
func parseRedditDocument(body []byte, target redditTarget) (redditMetadata, error) {
	metadata := redditMetadata{canonical: target.url.String(), community: target.community, method: embedFetchOpenGraph}
	if int64(len(body)) > maxEmbedHTMLBytes {
		return metadata, errRedditDocument
	}
	tokens := html.NewTokenizer(bytes.NewReader(body))
	var title, description strings.Builder
	identity, titleOpen, bodySeen := false, false, false
	bodyDivs := 0
	skip := ""
	for {
		kind := tokens.Next()
		if kind == html.ErrorToken {
			if tokens.Err() != io.EOF {
				return metadata, errRedditDocument
			}
			break
		}
		token := tokens.Token()
		if kind == html.StartTagToken || kind == html.SelfClosingTagToken {
			if token.Data == "script" || token.Data == "style" || token.Data == "noscript" || token.Data == "template" {
				skip = token.Data
				continue
			}
			if skip != "" {
				continue
			}
			attrs := make(map[string]string, len(token.Attr))
			for _, a := range token.Attr {
				attrs[a.Key] = a.Val
			}
			matches := func(raw string) bool {
				u, err := url.Parse(raw)
				if err != nil {
					return false
				}
				post, valid := redditPostTarget(u)
				if !valid || post.id != target.id || post.comment != target.comment {
					return false
				}
				metadata.canonical = post.url.String()
				if post.community != "" {
					metadata.community = post.community
				}
				return true
			}
			switch token.Data {
			case "div":
				if attrs["id"] == "canonical-url-updater" && matches(attrs["value"]) {
					identity = true
				}
				if bodyDivs > 0 {
					bodyDivs++
				} else {
					id := "t3_" + target.id
					if target.comment != "" {
						id = "t1_" + target.comment
					}
					if attrs["id"] == id+"-post-rtjson-content" || attrs["id"] == id+"-comment-rtjson-content" {
						bodyDivs, bodySeen = 1, true
					}
				}
			case "a":
				if attrs["id"] == "embed-title" && matches(attrs["href"]) {
					identity, titleOpen = true, true
				}
				if metadata.author == "" {
					u, err := url.Parse(attrs["href"])
					if err == nil && redditHTTPS(u) && u.Hostname() == "www.reddit.com" {
						parts := strings.Split(strings.Trim(u.Path, "/"), "/")
						if len(parts) == 2 && parts[0] == "user" && redditToken(parts[1], 128) {
							metadata.author = parts[1]
						}
					}
				}
			case "meta":
				switch attrs["property"] {
				case "og:title":
					metadata.title = attrs["content"]
				case "og:description":
					metadata.description = attrs["content"]
				case "og:url":
					identity = matches(attrs["content"]) || identity
				case "og:image":
					metadata.image = redditImageURL(attrs["content"])
				}
			case "shreddit-player":
				if attrs["post-id"] == "t3_"+target.id && target.comment == "" {
					metadata.video = redditPackagedVideo(attrs["packaged-media-json"], time.Now())
					if poster := redditImageURL(attrs["poster"]); poster != "" {
						metadata.image = poster
					}
				}
			case "img":
				if metadata.image == "" && target.comment == "" {
					metadata.image = redditImageURL(attrs["src"])
				}
			}
			if bodyDivs > 0 && (token.Data == "br" || token.Data == "li") {
				redditAppend(&description, "\n", embedDescriptionMax*4)
			}
		} else if kind == html.EndTagToken {
			if skip != "" {
				if token.Data == skip {
					skip = ""
				}
				continue
			}
			if token.Data == "a" {
				titleOpen = false
			}
			if bodyDivs > 0 {
				if token.Data == "p" || token.Data == "li" {
					redditAppend(&description, "\n", embedDescriptionMax*4)
				}
				if token.Data == "div" {
					bodyDivs--
				}
			}
		} else if kind == html.TextToken && skip == "" {
			if titleOpen {
				redditAppend(&title, token.Data, embedTitleMax*4)
			}
			if bodyDivs > 0 {
				redditAppend(&description, token.Data, embedDescriptionMax*4)
			}
		}
	}
	if title.Len() != 0 {
		metadata.title = strings.Join(strings.Fields(title.String()), " ")
	}
	if bodySeen {
		lines := strings.Split(description.String(), "\n")
		for i, line := range lines {
			lines[i] = strings.Join(strings.Fields(line), " ")
		}
		metadata.description = strings.TrimSpace(strings.Join(lines, "\n"))
	}
	if !identity || !redditTitle(metadata.title) || target.comment != "" && !bodySeen {
		return metadata, errRedditDocument
	}
	return metadata, nil
}

func redditAppend(builder *strings.Builder, value string, limit int) {
	remaining := limit - builder.Len()
	if remaining > 0 {
		// EN-US: Limit by whole Unicode code points, including when a token straddles the byte budget.
		// PT-BR: Limita por pontos de código Unicode completos, mesmo quando um token cruza o orçamento em bytes.
		for _, r := range value {
			if len(string(r)) > remaining {
				break
			}
			builder.WriteRune(r)
			remaining -= len(string(r))
		}
	}
}

func redditPackagedVideo(raw string, now time.Time) *models.EmbedMedia {
	if len(raw) > 64<<10 {
		return nil
	}
	var payload struct {
		Playback struct {
			Permutations []struct {
				Source struct {
					URL        string                      `json:"url"`
					Codec      string                      `json:"videoCodec"`
					Dimensions struct{ Width, Height int } `json:"dimensions"`
				} `json:"source"`
			} `json:"permutations"`
		} `json:"playbackMp4s"`
	}
	if json.Unmarshal([]byte(raw), &payload) != nil {
		return nil
	}
	var selected *models.EmbedMedia
	for _, p := range payload.Playback.Permutations {
		source := p.Source
		validated := redditVideoURL(source.URL)
		if validated == "" || !strings.EqualFold(source.Codec, "H264") || source.Dimensions.Width <= 0 || source.Dimensions.Height <= 0 || source.Dimensions.Width > 3840 || source.Dimensions.Height > 2160 {
			continue
		}
		mime := "video/mp4"
		candidate := &models.EmbedMedia{URL: &validated, MimeType: &mime, Width: &source.Dimensions.Width, Height: &source.Dimensions.Height}
		if redditVideoExpires(models.Embed{Video: candidate}, now) {
			continue
		}
		if selected == nil || *selected.Height > 720 && *candidate.Height < *selected.Height || *candidate.Height <= 720 && *candidate.Height > *selected.Height {
			selected = candidate
		}
	}
	return selected
}
