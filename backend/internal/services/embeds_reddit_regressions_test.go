package services

import (
	"context"
	"encoding/json"
	"errors"
	stdhtml "html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/storage"
)

func redditTranslatedHTML() string {
	data := `{"post":{"id":"t3_abc123","translation_state":true,"translation_language":"en"}}`
	return `<shreddit-screenview-data data="` + stdhtml.EscapeString(data) + `"></shreddit-screenview-data>` + redditHTML(`<img src="https://cf.preview.redd.it/post.jpeg?width=640&amp;s=fixture">`)
}

func TestRedditPostImageIgnoresCommunityIcons(t *testing.T) {
	u, _ := url.Parse(redditTestPost)
	target, _ := redditPostTarget(u)
	icon := `<img alt="subreddit icon" src="https://a.thumbs.redditmedia.com/community.png">`
	body := icon + redditHTML(`<img src="https://cf.preview.redd.it/post.jpeg?width=640&amp;s=fixture">`)
	metadata, err := parseRedditDocument([]byte(body), target)
	if err != nil || metadata.image != "https://cf.preview.redd.it/post.jpeg?width=640&s=fixture" {
		t.Fatalf("community icon replaced the post image: image=%q err=%v", metadata.image, err)
	}
	metadata, err = parseRedditDocument([]byte(icon+redditHTML("")), target)
	if err != nil || metadata.image != "" {
		t.Fatalf("text-only post used the community icon: image=%q err=%v", metadata.image, err)
	}
	if redditImageURL("https://cf.preview.redd.it.attacker.invalid/post.jpeg") != "" {
		t.Fatal("lookalike CDN host accepted")
	}
}

func TestRedditTranslatedDocumentsUseOriginalTitleAndRetainMedia(t *testing.T) {
	for _, original := range []string{"“É a pesquisa eleitoral que não erra” eles disseram", "Une publication française"} {
		t.Run(original, func(t *testing.T) {
			documents, lookups := 0, 0
			client := redditClientFixture(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "embed.reddit.com" {
					documents++
					return redditResponse(r, 200, redditTranslatedHTML()), nil
				}
				lookups++
				if r.URL.Host != "www.reddit.com" || r.URL.Path != "/oembed" || r.URL.Query().Get("url") != redditTestPost {
					t.Fatalf("unexpected lookup: %s", r.URL)
				}
				body, _ := json.Marshal(map[string]string{"title": original, "provider_name": "reddit", "author_name": "alice"})
				return redditResponse(r, 200, string(body)), nil
			})
			u, _ := url.Parse(redditTestPost)
			metadata, err := loadRedditMetadata(context.Background(), client, u)
			if err != nil || metadata.title != original || metadata.description != "" || metadata.image != "https://cf.preview.redd.it/post.jpeg?width=640&s=fixture" || metadata.author != "alice" || documents != 1 || lookups != 1 {
				t.Fatalf("original content/media lost: %+v documents=%d lookups=%d err=%v", metadata, documents, lookups, err)
			}
		})
	}
}

func TestRedditTranslationFlagIsScopedToRequestedPost(t *testing.T) {
	u, _ := url.Parse(redditTestPost)
	target, _ := redditPostTarget(u)
	body := strings.Replace(redditTranslatedHTML(), `t3_abc123`, `t3_other`, 1)
	metadata, err := parseRedditDocument([]byte(body), target)
	if err != nil || metadata.translated || metadata.description == "" {
		t.Fatalf("unrelated translation flag affected the requested post: %+v %v", metadata, err)
	}
}

func TestRedditDoesNotPublishTranslatedTitleWhenOriginalUnavailable(t *testing.T) {
	client := redditClientFixture(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "embed.reddit.com" {
			return redditResponse(r, 200, redditTranslatedHTML()), nil
		}
		return redditResponse(r, http.StatusTooManyRequests, ""), nil
	})
	u, _ := url.Parse(redditTestPost)
	if _, err := loadRedditMetadata(context.Background(), client, u); err == nil {
		t.Fatal("translated title was published when the original could not be retrieved")
	}
}

func TestRedditPreviewCacheMigrationRetainsMessagesAndOtherProviders(t *testing.T) {
	ctx := testCtx()
	if err := cleanServers(ctx); err != nil {
		t.Fatal(err)
	}
	owner := newTestMessageUser(t)
	channel := newTestMessageChannel(t, &owner.ID)
	title := "Previously translated title"
	redditURL := "https://www.reddit.com/r/test/comments/migrate1/title/"
	otherURL := "https://example.invalid/reddit-cache-migration-test"
	seed := func(key string) models.Embed {
		embed, err := storage.UpsertLinkEmbed(ctx, models.Embed{SourceType: embedSourceLink, FetchMethod: embedFetchOpenGraph, CacheKey: &key, URL: &key, Title: &title})
		if err != nil {
			t.Fatal(err)
		}
		return embed
	}
	reddit, other := seed(redditURL), seed(otherURL)
	message, err := storage.CreateMessage(ctx, channel.ID, owner.ID, "Synthetic cached preview", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.AddMessageEmbeds(ctx, message.ID, []string{reddit.ID}); err != nil {
		t.Fatal(err)
	}
	sql, err := os.ReadFile(filepath.Join(migrationsDir, "019_reddit_original_previews_cache.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetDB().ExecContext(ctx, gooseUpSection(string(sql))); err != nil {
		t.Fatal(err)
	}
	got, err := storage.GetEmbedByID(ctx, reddit.ID)
	if err != nil || got.FetchedAt == nil || got.FetchedAt.Unix() != 0 || got.Title == nil || *got.Title != title {
		t.Fatalf("Reddit cache was deleted instead of expired: %+v %v", got, err)
	}
	untouched, err := storage.GetEmbedByID(ctx, other.ID)
	if err != nil || untouched.FetchedAt == nil || !untouched.FetchedAt.Equal(*other.FetchedAt) {
		t.Fatalf("other provider changed: %v", err)
	}
	linked, err := storage.ListEmbedsByMessageIDs(ctx, []string{message.ID})
	if err != nil || len(linked[message.ID]) != 1 || linked[message.ID][0].ID != reddit.ID {
		t.Fatalf("message/embed association lost: %v", err)
	}
}

func TestRedditLiveOriginalLanguageImagePreviews(t *testing.T) {
	if os.Getenv("PAPO_REDDIT_LIVE") != "1" {
		t.Skip("optional public-network image/language regression")
	}
	for _, example := range []struct{ post, title, imageID string }{
		{"https://www.reddit.com/r/pcmasterrace/comments/1xn2zjn/i_am_a_complete_and_utter_idiot_rip_5090/", "I am a complete and utter idiot. RIP 5090.", "zjk3v1e3bgzh1"},
		{"https://www.reddit.com/r/farialimabets/comments/1xnbu24/%C3%A9_a_pesquisa_eleitoral_que_n%C3%A3o_erra_eles_disseram/", "“É a pesquisa eleitoral que não erra” eles disseram", "frmi0xg75izh1"},
	} {
		u, _ := url.Parse(example.post)
		metadata, err := loadRedditMetadata(context.Background(), outboundHTTPClient(), u)
		if err != nil || metadata.title != example.title {
			t.Fatalf("original public title mismatch: title=%q err=%v", metadata.title, err)
		}
		image, _ := url.Parse(metadata.image)
		if image == nil || redditImageURL(metadata.image) == "" || !strings.Contains(image.Path, example.imageID) {
			t.Fatalf("wrong post image: %q", metadata.image)
		}
		embed, err := fetchRedditPostEmbed(testCtx(), config.LoadConfig(), u)
		if err != nil || embed.Title == nil || *embed.Title != example.title || embed.Thumbnail == nil || embed.Thumbnail.MediaSHA == nil {
			t.Fatalf("original title/thumbnail not stored: %v", err)
		}
		if _, err := os.Stat(mediaBlobPath(*embed.Thumbnail.MediaSHA)); errors.Is(err, os.ErrNotExist) {
			t.Fatal("thumbnail blob not persisted")
		}
		t.Logf("live original title and image verified: %q (%s)", *embed.Title, image.Host)
	}
}
