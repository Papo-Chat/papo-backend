package services

import (
	"net/url"
	"testing"
)

func TestKlipyGIFSlug(t *testing.T) {
	cases := []struct {
		name string
		raw string
		want string
		ok bool
	}{
		{"standard link", "https://klipy.com/gifs/cute-cat-gif-123?source=share", "cute-cat-gif-123", true},
		{"www link", "https://www.klipy.com/gifs/hello-world", "hello-world", true},
		{"gif path", "https://klipy.com/gif/dancing-cat", "dancing-cat", true},
		{"wrong host", "https://klipy.example/gifs/cat", "", false},
		{"wrong path", "https://klipy.com/users/cat", "", false},
		{"encoded slash", "https://klipy.com/gifs/cat%2Fother", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.raw)
			if err != nil {
				t.Fatalf("url.Parse: %v", err)
			}
			got, ok := klipyGIFSlug(u)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("klipyGIFSlug(%q) = (%q, %v), want (%q, %v)", tc.raw, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestIsKlipyGIFAssetURL(t *testing.T) {
	cases := []struct {
		raw string
		want bool
	}{
		{"https://static.klipy.com/ii/hash/file.gif?token=opaque", true},
		{"https://static1.klipy.com/ii/hash/file.gif", true},
		{"http://static.klipy.com/file.gif", false},
		{"https://static.klipy.com.evil.test/file.gif", false},
		{"https://example.com/file.gif", false},
		{"https://user@static.klipy.com/file.gif", false},
		{"https://static.klipy.com:8443/file.gif", false},
	}
	for _, tc := range cases {
		if got := isKlipyGIFAssetURL(tc.raw); got != tc.want {
			t.Errorf("isKlipyGIFAssetURL(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
