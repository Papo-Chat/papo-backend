package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdhtml "html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"
)

const redditTestPost = "https://www.reddit.com/r/test/comments/abc123/title/"

type redditTransport func(*http.Request) (*http.Response, error)

func (f redditTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func redditResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func redditClientFixture(t *testing.T, transport redditTransport) *http.Client {
	t.Helper()
	initOutbound()
	previous := outboundClient
	client := &http.Client{Transport: transport}
	outboundClient = client
	t.Cleanup(func() { outboundClient = previous })
	return client
}

// EN-US: Minimal first-party DOM fixtures contain no tracking data, credentials or executable scripts.
// PT-BR: Fixtures mínimas do DOM oficial não contêm rastreamento, credenciais ou scripts executáveis.
func redditHTML(extra string) string {
	return `<div id="canonical-url-updater" value="` + redditTestPost + `"></div>
<a href="https://www.reddit.com/user/alice/">alice</a>
<a id="embed-title" href="` + redditTestPost + `?utm_source=embed"><h1>A real post &amp; title</h1></a>
<div id="t3_abc123-post-rtjson-content"><p>Olá <b>mundo</b>.</p><p>Second paragraph.</p></div>` + extra
}

func redditVideoFixture(expires int64) string {
	return fmt.Sprintf(`{"playbackMp4s":{"permutations":[{"source":{"url":"https://packaged-media.redd.it/clip/pb/720.mp4?e=%d&s=fixture","videoCodec":"H264","dimensions":{"width":1280,"height":720}}}]}}`, expires)
}

func redditVideoHTML(expires int64) string {
	return redditHTML(`<shreddit-player post-id="t3_abc123" poster="https://preview.redd.it/post.jpg" packaged-media-json="` + stdhtml.EscapeString(redditVideoFixture(expires)) + `"></shreddit-player>`)
}

func TestRedditURLValidation(t *testing.T) {
	for _, raw := range []string{redditTestPost, "https://redd.it/abc123", "https://www.redd.it/abc123", "https://www.reddit.com/gallery/abc123", "https://www.reddit.com/r/test/s/Share123", "https://old.reddit.com/comments/abc123/", "https://np.reddit.com/user/alice/comments/abc123/title/", "https://www.reddit.com/r/test/comments/abc123/title/def456/", "https://www.reddit.com:443/comments/abc123/"} {
		u, _ := url.Parse(raw)
		if !isRedditPostURL(u) {
			t.Errorf("valid post rejected: %s", raw)
		}
	}
	for _, raw := range []string{"https://www.reddit.com/", "https://www.reddit.com/r/test/", "https://www.reddit.com/search/", "https://reddit.com.attacker.test/comments/abc123/", "http://www.reddit.com/comments/abc123/", "https://alice@www.reddit.com/comments/abc123/", "https://www.reddit.com:8443/comments/abc123/", "https://www.reddit.com/r/test/comments/abc123/.json", "https://www.reddit.com/r/test/comments/abc123/title/def456/extra", "https://www.reddit.com/r/test/s/share/extra", "https://redd.it/abc123/extra", "https://www.reddit.com/r/test/comments/%2e%2e/"} {
		u, _ := url.Parse(raw)
		if isRedditPostURL(u) {
			t.Errorf("invalid post accepted: %s", raw)
		}
	}
	if isRedditPostURL(nil) {
		t.Fatal("nil URL accepted")
	}
}

func TestRedditDocumentExtractsOnlyPostContent(t *testing.T) {
	u, _ := url.Parse(redditTestPost)
	target, _ := redditPostTarget(u)
	body := redditHTML(`<script>alert("not post text")</script><img src="https://tracker.invalid/x"><img src="https://i.redd.it/image.png"><div>unrelated footer</div>`)
	metadata, err := parseRedditDocument([]byte(body), target)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.title != "A real post & title" || metadata.author != "alice" || metadata.community != "test" || metadata.canonical != redditTestPost || metadata.image != "https://i.redd.it/image.png" || metadata.description != "Olá mundo.\nSecond paragraph." {
		t.Fatalf("incorrect metadata: %+v", metadata)
	}
	for _, invalid := range []string{`<title>Reddit</title><script>challenge()</script>`, `<title>Just a moment...</title>`, strings.ReplaceAll(body, "abc123", "wrong123"), `<meta property="og:title" content="Unrelated title">`} {
		if _, err := parseRedditDocument([]byte(invalid), target); err == nil {
			t.Fatal("challenge/unrelated document accepted")
		}
	}
}

func TestRedditDocumentBoundsUnicodeAndComments(t *testing.T) {
	u, _ := url.Parse(redditTestPost)
	target, _ := redditPostTarget(u)
	body := strings.ReplaceAll(redditHTML(""), "A real post &amp; title", strings.Repeat("á", 1000))
	body = strings.ReplaceAll(body, "Olá <b>mundo</b>.", strings.Repeat("é", 20000))
	metadata, err := parseRedditDocument([]byte(body), target)
	if err != nil || !utf8.ValidString(metadata.title) || !utf8.ValidString(metadata.description) || len(metadata.description) > embedDescriptionMax*4 {
		t.Fatalf("Unicode bounds: %+v %v", metadata, err)
	}
	commentURL := redditTestPost + "def456/"
	u, _ = url.Parse(commentURL)
	target, _ = redditPostTarget(u)
	comment := `<a id="embed-title" href="` + commentURL + `">Alice's comment</a><div id="t1_def456-comment-rtjson-content"><p>The requested comment.</p></div><div id="t3_abc123-post-rtjson-content">Parent text.</div>`
	metadata, err = parseRedditDocument([]byte(comment), target)
	if err != nil || metadata.description != "The requested comment." || metadata.canonical != commentURL {
		t.Fatalf("comment context mixed with parent: %+v %v", metadata, err)
	}
	if _, err := parseRedditDocument([]byte(redditHTML("")), target); err == nil {
		t.Fatal("a parent post was presented as the requested comment")
	}
}

func TestRedditOfficialEmbedLoadAndShareResolution(t *testing.T) {
	for _, raw := range []string{redditTestPost, "https://redd.it/abc123", "https://www.reddit.com/r/test/s/Share123"} {
		t.Run(raw, func(t *testing.T) {
			requests := 0
			client := redditClientFixture(t, func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
					t.Fatal("public requests forwarded credentials")
				}
				if r.URL.Host == "embed.reddit.com" {
					return redditResponse(r, 200, redditHTML("")), nil
				}
				if strings.Contains(r.URL.Path, "/s/") {
					response := redditResponse(r, 302, "")
					response.Header.Set("Location", redditTestPost+"?utm_source=share")
					return response, nil
				}
				response := redditResponse(r, 200, "")
				response.Body = &redditUnreadBody{}
				return response, nil
			})
			jar, _ := cookiejar.New(nil)
			for _, host := range []string{"www.reddit.com", "embed.reddit.com"} {
				u, _ := url.Parse("https://" + host)
				jar.SetCookies(u, []*http.Cookie{{Name: "unused", Value: "synthetic"}})
			}
			client.Jar = jar
			u, _ := url.Parse(raw)
			metadata, err := loadRedditMetadata(context.Background(), client, u)
			wantRequests := 1
			if strings.Contains(raw, "/s/") {
				wantRequests = 3
			}
			if err != nil || metadata.title != "A real post & title" || metadata.canonical != redditTestPost || requests != wantRequests || client.Jar != jar {
				t.Fatalf("metadata=%+v requests=%d err=%v", metadata, requests, err)
			}
		})
	}
}

type redditUnreadBody struct{}

func (*redditUnreadBody) Read([]byte) (int, error) {
	panic("share resolution must not read the website body")
}
func (*redditUnreadBody) Close() error { return nil }

func TestRedditBlockedDocumentFallsBackToOfficialMetadata(t *testing.T) {
	for _, failure := range []string{"403", "challenge", "timeout", "oversize"} {
		t.Run(failure, func(t *testing.T) {
			calls := 0
			client := redditClientFixture(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host == "embed.reddit.com" {
					switch failure {
					case "403":
						return redditResponse(r, 403, ""), nil
					case "timeout":
						return nil, context.DeadlineExceeded
					case "oversize":
						return redditResponse(r, 200, strings.Repeat("x", int(maxEmbedHTMLBytes)+1)), nil
					default:
						return redditResponse(r, 200, `<title>Reddit</title><script>challenge()</script>`), nil
					}
				}
				if r.URL.Host != "www.reddit.com" || r.URL.Path != "/oembed" || r.URL.Query().Get("url") != redditTestPost {
					t.Fatalf("unexpected fallback URL: %s", r.URL)
				}
				return redditResponse(r, 200, `{"provider_name":"reddit","title":"Official post","author_name":"alice","html":"<script src='https://evil.invalid/'></script>"}`), nil
			})
			u, _ := url.Parse(redditTestPost)
			metadata, err := loadRedditMetadata(context.Background(), client, u)
			if err != nil || metadata.title != "Official post" || metadata.author != "alice" || metadata.method != embedFetchOEmbed || calls != 2 || metadata.video != nil || metadata.image != "" {
				t.Fatalf("fallback=%+v calls=%d err=%v", metadata, calls, err)
			}
		})
	}
}

func TestRedditUnavailableAndCanceledRequestsDoNotRetry(t *testing.T) {
	for _, status := range []int{404, 410, 429} {
		calls := 0
		client := redditClientFixture(t, func(r *http.Request) (*http.Response, error) { calls++; return redditResponse(r, status, ""), nil })
		u, _ := url.Parse(redditTestPost)
		var statusErr *utils.HTTPStatusError
		if _, err := loadRedditMetadata(context.Background(), client, u); !errors.As(err, &statusErr) || statusErr.Status != status || calls != 1 {
			t.Fatalf("HTTP %d was retried: calls=%d err=%v", status, calls, err)
		}
	}
	client := redditClientFixture(t, func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })
	u, _ := url.Parse(redditTestPost)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := loadRedditMetadata(ctx, client, u); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestRedditRedirectBoundariesAndOriginalPolicy(t *testing.T) {
	for _, destination := range []string{"https://example.test/", "https://embed.reddit.com.evil.test/r/test/comments/abc123/title/", "https://127.0.0.1/r/test/comments/abc123/title/", "http://embed.reddit.com/r/test/comments/abc123/title/", "https://alice@embed.reddit.com/r/test/comments/abc123/title/", "https://embed.reddit.com:8443/r/test/comments/abc123/title/", "https://embed.reddit.com/login/", "https://embed.reddit.com/r/test/comments/wrong/title/", "https://embed.reddit.com/r/test/comments/abc123/.json"} {
		t.Run(destination, func(t *testing.T) {
			calls := 0
			client := redditClientFixture(t, func(r *http.Request) (*http.Response, error) {
				calls++
				response := redditResponse(r, 302, "")
				response.Header.Set("Location", destination)
				return response, nil
			})
			u, _ := url.Parse(redditTestPost)
			if _, err := loadRedditMetadata(context.Background(), client, u); !errors.Is(err, utils.ErrSSRFBlocked) || calls != 1 {
				t.Fatalf("forbidden hop fetched: calls=%d err=%v", calls, err)
			}
		})
	}
	calls := 0
	client := redditClientFixture(t, func(r *http.Request) (*http.Response, error) {
		calls++
		response := redditResponse(r, 302, "")
		response.Header.Set("Location", r.URL.String())
		return response, nil
	})
	u, _ := url.Parse(redditTestPost)
	if _, err := loadRedditMetadata(context.Background(), client, u); !errors.Is(err, utils.ErrSSRFBlocked) || calls != 5 {
		t.Fatalf("redirect loop unbounded: calls=%d err=%v", calls, err)
	}
	sentinel := errors.New("original policy")
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return sentinel }
	u.Host = "embed.reddit.com"
	if _, err := scopedRedditClient(client, func(*url.URL) bool { return true }).Get(u.String()); !errors.Is(err, sentinel) {
		t.Fatalf("caller redirect policy lost: %v", err)
	}
}

func TestRedditPackagedVideoSelectionAndExpiry(t *testing.T) {
	now := time.Now()
	video := redditPackagedVideo(redditVideoFixture(now.Add(time.Hour).Unix()), now)
	if video == nil || video.MimeType == nil || *video.MimeType != "video/mp4" || *video.Height != 720 {
		t.Fatalf("packaged MP4 missing: %+v", video)
	}
	if redditPackagedVideo(redditVideoFixture(now.Add(-time.Minute).Unix()), now) != nil {
		t.Fatal("expired signature selected")
	}
	for _, invalid := range []string{strings.ReplaceAll(redditVideoFixture(now.Add(time.Hour).Unix()), "packaged-media.redd.it", "evil.test"), strings.ReplaceAll(redditVideoFixture(now.Add(time.Hour).Unix()), ".mp4?", ".m3u8?"), strings.ReplaceAll(redditVideoFixture(now.Add(time.Hour).Unix()), "H264", "AV1")} {
		if redditPackagedVideo(invalid, now) != nil {
			t.Fatal("unapproved video source selected")
		}
	}
	var data map[string]any
	json.Unmarshal([]byte(redditVideoFixture(now.Add(time.Hour).Unix())), &data)
	playback := data["playbackMp4s"].(map[string]any)
	permutations := playback["permutations"].([]any)
	for _, height := range []int{1080, 360, 480} {
		permutations = append(permutations, map[string]any{"source": map[string]any{"url": "https://packaged-media.redd.it/clip/video.mp4", "videoCodec": "H264", "dimensions": map[string]any{"width": height * 2, "height": height}}})
	}
	playback["permutations"] = permutations
	encoded, _ := json.Marshal(data)
	if selected := redditPackagedVideo(string(encoded), now); selected == nil || *selected.Height != 720 {
		t.Fatal("720p selection regressed")
	}
}

func TestRedditImageURLsRejectTrackersAndUnsafeSchemes(t *testing.T) {
	for _, raw := range []string{"https://i.redd.it/image.png", "https://preview.redd.it/image.webp?s=signature", "https://external-preview.redd.it/image.jpg"} {
		if redditImageURL(raw) == "" {
			t.Fatalf("valid media rejected: %s", raw)
		}
	}
	for _, raw := range []string{"https://preview.redd.it.attacker.test/image.png", "http://i.redd.it/image.png", "https://alice@i.redd.it/image.png", "https://i.redd.it:8443/image.png", "https://styles.redditmedia.com/avatar.png", "data:image/png;base64,AAAA"} {
		if redditImageURL(raw) != "" {
			t.Fatalf("unsafe or unrelated image accepted: %s", raw)
		}
	}
}

func TestRedditProviderStoresThumbnailAndKeepsCache(t *testing.T) {
	t.Setenv("ROBOTS_ENABLED", "false")
	requests := 0
	redditClientFixture(t, func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Host == "embed.reddit.com" {
			return redditResponse(r, 200, redditHTML(`<img src="https://i.redd.it/post.png">`)), nil
		}
		if r.URL.Host != "i.redd.it" {
			t.Fatalf("unexpected media fetch: %s", r.URL)
		}
		return redditResponse(r, 200, string(pngAvatarBytes(32, 16))), nil
	})
	user := newTestMessageUser(t)
	stored, changed, err := GetOrCreateEmbed(testCtx(), user.ID, redditTestPost)
	if err != nil || !changed || stored.Thumbnail == nil || stored.Thumbnail.MediaSHA == nil || *stored.Thumbnail.Width != 32 || *stored.Thumbnail.Height != 16 || stored.Author == nil || *stored.Author.Name != "u/alice" {
		t.Fatalf("provider/storage integration: %+v changed=%v err=%v", stored, changed, err)
	}
	cached, changed, err := GetOrCreateEmbed(testCtx(), user.ID, redditTestPost)
	if err != nil || changed || cached.ID != stored.ID || requests != 2 {
		t.Fatalf("cache fetched again: requests=%d changed=%v err=%v", requests, changed, err)
	}
}

func TestRedditCacheAndCrawlBudgets(t *testing.T) {
	now := time.Now()
	old := now.Add(-6 * time.Minute)
	key, title := redditTestPost, "Reddit"
	embed := models.Embed{CacheKey: &key, Title: &title, FetchedAt: &old, FetchMethod: embedFetchOpenGraph}
	if !redditCacheNeedsRefresh(embed, now) {
		t.Fatal("challenge card remains cached")
	}
	title = "A valid title"
	if redditCacheNeedsRefresh(embed, now) {
		t.Fatal("valid document unexpectedly refreshed")
	}
	embed.FetchMethod = embedFetchOEmbed
	if !redditCacheNeedsRefresh(embed, now) {
		t.Fatal("metadata fallback never gets another opportunity for media")
	}
	for _, urls := range [][]string{{redditTestPost}, {redditTestPost, "https://gif.fxtwitter.com/tweet_video/test.webp"}, {"https://gif.fxtwitter.com/tweet_video/test.webp", redditTestPost}} {
		want := redditPreviewTimeout
		if len(urls) > 1 {
			want = xProxyGIFProcessingTimeout
		}
		if got := embedCrawlTimeout(time.Second, urls); got != want {
			t.Fatalf("crawl budget=%v want=%v", got, want)
		}
	}
}

func TestRedditThumbnailFailurePreservesPostAndRejectsRedirect(t *testing.T) {
	t.Setenv("ROBOTS_ENABLED", "false")
	requests := 0
	redditClientFixture(t, func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Host == "embed.reddit.com" {
			return redditResponse(r, 200, redditHTML(`<img src="https://i.redd.it/post.png">`)), nil
		}
		if r.URL.Host != "i.redd.it" {
			t.Fatal("thumbnail followed a redirect outside Reddit media")
		}
		response := redditResponse(r, 302, "")
		response.Header.Set("Location", "https://unrelated.invalid/image.png")
		return response, nil
	})
	u, _ := url.Parse(redditTestPost)
	embed, err := fetchRedditPostEmbed(testCtx(), config.LoadConfig(), u)
	if err != nil || embed.Title == nil || embed.Thumbnail != nil || requests != 2 {
		t.Fatalf("thumbnail failure discarded the post: requests=%d err=%v", requests, err)
	}
}

func TestRedditVideoRefreshRunsAfterAuthorization(t *testing.T) {
	t.Setenv("THUMBNAIL_ENABLED", "false")
	ctx := testCtx()
	if err := cleanServers(ctx); err != nil {
		t.Fatal(err)
	}
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	outsider := newTestMessageUser(t)
	grantChannelPermission(t, channel, owner, models.ChannelPermission{ReadChannel: true})
	key, expired, title, mime := redditTestPost, "https://packaged-media.redd.it/clip/expired.mp4?e=1", "Old video", "video/mp4"
	embed, err := storage.UpsertLinkEmbed(ctx, models.Embed{SourceType: embedSourceLink, FetchMethod: embedFetchOpenGraph, CacheKey: &key, URL: &key, Title: &title, Video: &models.EmbedMedia{URL: &expired, MimeType: &mime}})
	if err != nil {
		t.Fatal(err)
	}
	message, err := storage.CreateMessage(ctx, channel.ID, owner.ID, "Synthetic Reddit video", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.AddMessageEmbeds(ctx, message.ID, []string{embed.ID}); err != nil {
		t.Fatal(err)
	}
	documents, streams := 0, 0
	redditClientFixture(t, func(r *http.Request) (*http.Response, error) {
		documents++
		return redditResponse(r, 200, redditVideoHTML(time.Now().Add(time.Hour).Unix())), nil
	})
	initEmbedVideoProxy()
	previous := embedVideoClient
	embedVideoClient = &http.Client{Transport: redditTransport(func(r *http.Request) (*http.Response, error) {
		streams++
		if r.URL.Query().Get("e") == "1" || r.Header.Get("Range") != "bytes=0-31" {
			t.Fatal("expired URL or missing range on video relay")
		}
		response := redditResponse(r, http.StatusPartialContent, "synthetic video bytes")
		response.Header.Set("Content-Type", "video/mp4")
		return response, nil
	})}
	t.Cleanup(func() { embedVideoClient = previous })
	if response, release, err := OpenEmbedVideo(ctx, embed.ID, outsider.ID, "bytes=0-31"); err == nil || response != nil || release != nil || documents != 0 || streams != 0 {
		t.Fatalf("unauthorized access triggered a refresh: %v", err)
	}
	for i := 0; i < 2; i++ {
		response, release, err := OpenEmbedVideo(ctx, embed.ID, owner.ID, "bytes=0-31")
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		release()
	}
	if documents != 1 || streams != 2 {
		t.Fatalf("refresh repeated per range: documents=%d streams=%d", documents, streams)
	}
}

func TestRedditRecordedEmbedDocuments(t *testing.T) {
	root := os.Getenv("PAPO_REDDIT_SAMPLES")
	if root == "" {
		t.Skip("optional locally recorded first-party documents")
	}
	for _, example := range []struct {
		file, post         string
		video, image, text bool
	}{
		{"embed-post.body", "https://www.reddit.com/r/factorio/comments/1xlmd0s/zenith_3d_factorio_mod_out_now/", true, true, false},
		{"text-embed.body", "https://www.reddit.com/r/reddit/comments/135shj5/making_it_easier_to_share_your_favorite_reddit/", false, false, true},
		{"image-embed.body", "https://www.reddit.com/r/aww/comments/crpjwk/every_year_this_mama_duck_brings_her_babies_to_my/", false, true, false},
	} {
		t.Run(example.file, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(root, example.file))
			if err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(example.post)
			target, _ := redditPostTarget(u)
			metadata, err := parseRedditDocument(body, target)
			if err != nil || example.video && metadata.video == nil || example.image && metadata.image == "" || example.text && metadata.description == "" {
				t.Fatalf("recorded document failed: title=%q image=%v video=%v text=%v err=%v", metadata.title, metadata.image != "", metadata.video != nil, metadata.description != "", err)
			}
		})
	}
}

func TestRedditLivePublicPreviews(t *testing.T) {
	if os.Getenv("PAPO_REDDIT_LIVE") != "1" {
		t.Skip("optional public-network provider verification")
	}
	ctx := testCtx()
	if err := cleanServers(ctx); err != nil {
		t.Fatal(err)
	}
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	for _, example := range []struct {
		post               string
		video, image, text bool
	}{
		{"https://www.reddit.com/r/factorio/s/S2nxyUEQsw", true, true, false},
		{"https://www.reddit.com/r/reddit/comments/135shj5/making_it_easier_to_share_your_favorite_reddit/", false, false, true},
		{"https://www.reddit.com/r/aww/comments/crpjwk/every_year_this_mama_duck_brings_her_babies_to_my/", false, true, false},
	} {
		u, _ := url.Parse(example.post)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		embed, err := fetchRedditPostEmbed(ctx, config.LoadConfig(), u)
		cancel()
		if err != nil || embed.Title == nil || example.video && embed.Video == nil || example.image && embed.Thumbnail == nil || example.text && embed.Description == nil {
			t.Fatalf("live preview failed: title=%v image=%v video=%v text=%v err=%v", embed.Title != nil, embed.Thumbnail != nil, embed.Video != nil, embed.Description != nil, err)
		}
		if example.video {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			message, err := storage.CreateMessage(ctx, channel.ID, owner.ID, "Synthetic public Reddit video test", "", nil)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			if err := storage.AddMessageEmbeds(ctx, message.ID, []string{embed.ID}); err != nil {
				cancel()
				t.Fatal(err)
			}
			response, release, err := OpenEmbedVideo(ctx, embed.ID, owner.ID, "bytes=0-131071")
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 131072))
			response.Body.Close()
			release()
			cancel()
			if readErr != nil || response.StatusCode != http.StatusPartialContent || len(body) < 8 || string(body[4:8]) != "ftyp" {
				t.Fatalf("real authorized MP4 range failed: status=%d bytes=%d err=%v", response.StatusCode, len(body), readErr)
			}
			t.Log("authorized Reddit video relay verified: HTTP 206 and MP4 header")
		}
		t.Logf("public preview verified: title=%q image=%v video=%v text=%v", *embed.Title, embed.Thumbnail != nil, embed.Video != nil, embed.Description != nil)
	}
}
