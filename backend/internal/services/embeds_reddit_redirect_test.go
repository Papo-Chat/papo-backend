package services

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"papo/internal/config"
	"papo/internal/utils"
)

type redditRedirectTransport func(*http.Request) (*http.Response, error)

func (f redditRedirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func redditRedirectResponse(r *http.Request, status int, location, body string) *http.Response {
	header := make(http.Header)
	if location != "" {
		header.Set("Location", location)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

// EN-US: Replace only the outbound transport; the real provider, limits and storage path still run.
// PT-BR: Substitui só o transporte externo; provedor, limites e armazenamento reais continuam em uso.
func redditRedirectClient(t *testing.T, client *http.Client) {
	t.Helper()
	initOutbound()
	original := outboundClient
	outboundClient = client
	t.Cleanup(func() { outboundClient = original })
}

func TestRedditShareRedirectPersistsHTMLPreview(t *testing.T) {
	const canonical = "https://www.reddit.com/r/factorio/comments/1xlmd0s/zenith_3d_factorio_mod_out_now/"
	for _, share := range []string{"https://www.reddit.com/r/factorio/s/S2nxyUEQsw", "https://redd.it/1xlmd0s"} {
		t.Run(share, func(t *testing.T) {
			calls, redirects := 0, 0
			jar, _ := cookiejar.New(nil)
			target, _ := url.Parse(share)
			jar.SetCookies(target, []*http.Cookie{{Name: "unused_session", Value: "synthetic"}})
			client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { redirects++; return nil }}
			client.Transport = redditRedirectTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("public previews must not forward credentials")
				}
				switch r.URL.String() {
				case share:
					return redditRedirectResponse(r, 302, canonical, ""), nil
				case canonical:
					return redditRedirectResponse(r, 200, "", `<html><body><shreddit-post post-title="Zenith | 3D Factorio Mod Out NOW" subreddit-name="factorio" author="unjustodin" permalink="`+canonical+`"></shreddit-post></body></html>`), nil
				default:
					t.Fatalf("unexpected URL: %s", r.URL)
					return nil, errors.New("unexpected request")
				}
			})
			redditRedirectClient(t, client)
			embed, err := fetchRedditPostEmbed(context.Background(), &config.Config{}, target)
			if err != nil {
				t.Fatal(err)
			}
			if embed.Title == nil || *embed.Title != "Zenith | 3D Factorio Mod Out NOW" || embed.Author == nil || embed.Author.Name == nil || *embed.Author.Name != "u/unjustodin" {
				t.Fatalf("missing post metadata: %+v", embed)
			}
			if embed.CacheKey == nil || *embed.CacheKey != share || embed.URL == nil || *embed.URL != canonical || target.String() != share {
				t.Fatalf("original cache key or canonical destination changed: %+v", embed)
			}
			if calls != 2 || redirects != 1 || client.Jar != jar {
				t.Fatalf("calls=%d redirects=%d; original client must stay unchanged", calls, redirects)
			}
		})
	}
}

func TestRedditRedirectRejectsUnapprovedHopsBeforeFetching(t *testing.T) {
	for _, destination := range []string{
		"https://example.com/r/factorio/comments/abc123/",
		"https://www.reddit.com.attacker.example/r/factorio/comments/abc123/",
		"https://127.0.0.1/r/factorio/comments/abc123/",
		"http://www.reddit.com/r/factorio/comments/abc123/",
		"https://alice@www.reddit.com/r/factorio/comments/abc123/",
		"https://www.reddit.com:8443/r/factorio/comments/abc123/",
		"https://www.reddit.com/login/",
		"https://www.reddit.com/r/factorio/comments/abc123/.json",
	} {
		t.Run(destination, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: redditRedirectTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return redditRedirectResponse(r, 302, destination, ""), nil
				}
				return redditRedirectResponse(r, 200, "", `<html><title>Unapproved page</title></html>`), nil
			})}
			redditRedirectClient(t, client)
			target, _ := url.Parse("https://www.reddit.com/r/factorio/s/share")
			if _, err := fetchRedditPostEmbed(context.Background(), &config.Config{}, target); !errors.Is(err, utils.ErrSSRFBlocked) {
				t.Errorf("expected redirect rejection, got %v", err)
			}
			if calls != 1 {
				t.Fatalf("unapproved redirect was fetched (%d requests)", calls)
			}
		})
	}
}

func TestRedditRedirectPreservesOriginalPolicyAndLimit(t *testing.T) {
	target, _ := url.Parse("https://www.reddit.com/r/factorio/s/share")
	sentinel := errors.New("existing redirect policy")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return sentinel }, Transport: redditRedirectTransport(func(r *http.Request) (*http.Response, error) {
		return redditRedirectResponse(r, 302, "https://old.reddit.com/r/factorio/comments/abc123/", ""), nil
	})}
	redditRedirectClient(t, client)
	if _, err := fetchRedditPostEmbed(context.Background(), &config.Config{}, target); !errors.Is(err, sentinel) {
		t.Fatalf("original policy lost: %v", err)
	}
	calls := 0
	client.CheckRedirect = nil
	client.Transport = redditRedirectTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return redditRedirectResponse(r, 302, target.String(), ""), nil
	})
	if _, err := fetchRedditPostEmbed(context.Background(), &config.Config{}, target); !errors.Is(err, utils.ErrSSRFBlocked) || calls != 5 {
		t.Fatalf("redirect loop: calls=%d err=%v", calls, err)
	}
}

func TestRedditRedirectPreservesCancellationAndBodyLimit(t *testing.T) {
	target, _ := url.Parse("https://www.reddit.com/r/factorio/s/share")
	client := &http.Client{Transport: redditRedirectTransport(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	redditRedirectClient(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := fetchRedditPostEmbed(ctx, &config.Config{}, target); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline lost: %v", err)
	}
	client.Transport = redditRedirectTransport(func(r *http.Request) (*http.Response, error) {
		return redditRedirectResponse(r, 200, "", strings.Repeat("x", int(maxEmbedHTMLBytes)+1)), nil
	})
	if _, err := fetchRedditPostEmbed(context.Background(), &config.Config{}, target); !errors.Is(err, utils.ErrBodyTooLarge) {
		t.Fatalf("HTML size limit lost: %v", err)
	}
}

func TestRedditRedirectRejectsUnavailablePosts(t *testing.T) {
	for _, status := range []int{403, 404, 429} {
		client := &http.Client{Transport: redditRedirectTransport(func(r *http.Request) (*http.Response, error) {
			return redditRedirectResponse(r, status, "", ""), nil
		})}
		redditRedirectClient(t, client)
		target, _ := url.Parse("https://www.reddit.com/r/factorio/s/share")
		var responseErr *utils.HTTPStatusError
		if _, err := fetchRedditPostEmbed(context.Background(), &config.Config{}, target); !errors.As(err, &responseErr) || responseErr.Status != status {
			t.Fatalf("HTTP %d: %v", status, err)
		}
	}
}
