package services

import (
	"net/url"
	"strings"
	"testing"
)

func TestIsRedditPostURL(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"https://www.reddit.com/r/brdev/comments/1esc9k6/golang_titulo", true},
		{"https://old.reddit.com/r/golang/comments/abc123/title/", true},
		{"https://reddit.com/comments/abc123", true},
		{"https://redd.it/abc123", true},
		{"https://www.reddit.com/r/brdev/s/abcd12", true},
		{"https://www.reddit.com/r/brdev", false},
		{"https://www.reddit.com/r/brdev/hot", false},
		{"https://www.reddit.com/r/brdev/comments/", false},
		{"https://www.reddit.com/r/brdev/comments/1esc9k6.json", false},
		{"https://www.reddit.com/r/brdev/comments/1esc9k6/.json", false},
		{"http://www.reddit.com/r/brdev/comments/1esc9k6/title", false},
		{"https://www.reddit.com.attacker.example/r/brdev/comments/1esc9k6", false},
		{"https://example.com/r/brdev/comments/1esc9k6", false},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatalf("URL inválida %q: %v", tc.raw, err)
		}
		if got := isRedditPostURL(u); got != tc.want {
			t.Errorf("isRedditPostURL(%q) = %v, esperado %v", tc.raw, got, tc.want)
		}
	}
}

func TestParseRedditPostHTML(t *testing.T) {
	pageURL, err := url.Parse("https://www.reddit.com/r/brdev/comments/1esc9k6/golang_titulo/")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`<!doctype html>
<html><head>
	<meta property="og:title" content="Golang é realmente isso tudo? : r/brdev">
	<meta property="og:description" content="Descrição OG que deve perder para o corpo do post.">
	<meta property="og:url" content="https://www.reddit.com/r/brdev/comments/1esc9k6/golang_titulo/">
	<meta property="og:image" content="https://preview.redd.it/post-preview.jpg">
	<meta property="og:image:width" content="1200">
	<meta property="og:image:height" content="630">
	<meta name="author" content="devbr">
</head><body>
	<shreddit-post post-title="Golang é realmente isso tudo?" subreddit-name="brdev" author="devbr" score="60" comment-count="72" permalink="/r/brdev/comments/1esc9k6/golang_titulo/">
		<div slot="text-body"><p>Olá a todos.</p><p>Eu sou desenvolvedor C# e gosto do ecossistema .NET.</p></div>
	</shreddit-post>
</body></html>`)
	meta, err := parseRedditPostHTML(body, pageURL)
	if err != nil {
		t.Fatal(err)
	}
	if meta.title != "Golang é realmente isso tudo?" {
		t.Errorf("title = %q", meta.title)
	}
	if meta.subreddit != "brdev" {
		t.Errorf("subreddit = %q", meta.subreddit)
	}
	if meta.author != "devbr" {
		t.Errorf("author = %q", meta.author)
	}
	if meta.score != "60" || meta.comments != "72" {
		t.Errorf("post stats = %q votes, %q comments", meta.score, meta.comments)
	}
	if !strings.Contains(meta.description, "Olá a todos.") ||
		!strings.Contains(meta.description, "ecossistema .NET.") {
		t.Errorf("description = %q", meta.description)
	}
	if meta.imageURL != "https://preview.redd.it/post-preview.jpg" {
		t.Errorf("imageURL = %q", meta.imageURL)
	}
	if meta.canonicalURL != "https://www.reddit.com/r/brdev/comments/1esc9k6/golang_titulo/" {
		t.Errorf("canonicalURL = %q", meta.canonicalURL)
	}
	if meta.imageWidth != 1200 || meta.imageHeight != 630 {
		t.Errorf("image dimensions = %dx%d", meta.imageWidth, meta.imageHeight)
	}
}

func TestParseRedditPostHTMLFallsBackToOpenGraph(t *testing.T) {
	pageURL, _ := url.Parse("https://www.reddit.com/r/golang/comments/abc123/")
	body := []byte(`<html><head>
		<title>Post title : r/golang</title>
		<meta property="og:description" content="Post description">
		<meta property="og:image" content="//preview.redd.it/thumb.png">
	</head><body></body></html>`)
	meta, err := parseRedditPostHTML(body, pageURL)
	if err != nil {
		t.Fatal(err)
	}
	if meta.title != "Post title" {
		t.Errorf("title = %q", meta.title)
	}
	if meta.description != "Post description" {
		t.Errorf("description = %q", meta.description)
	}
	if meta.subreddit != "golang" {
		t.Errorf("subreddit = %q", meta.subreddit)
	}
	if meta.imageURL != "https://preview.redd.it/thumb.png" {
		t.Errorf("imageURL = %q", meta.imageURL)
	}
}

func TestStripRedditTitleSuffix(t *testing.T) {
	if got := stripRedditTitleSuffix("A title : r/brdev"); got != "A title" {
		t.Errorf("stripRedditTitleSuffix() = %q", got)
	}
	if got := stripRedditTitleSuffix("A title"); got != "A title" {
		t.Errorf("stripRedditTitleSuffix() = %q", got)
	}
}
