package utils

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestSafeFetchWithUserAgent(t *testing.T) {
	const customUA = "Mozilla/5.0 (compatible; PapoBot/1.0; +https://github.com/Papo-Chat/papo-backend)"
	var receivedUA string
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			receivedUA = req.Header.Get("User-Agent")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("html")),
				Request:    req,
			}, nil
		}),
	}

	body, finalURL, err := SafeFetchWithUserAgent(
		context.Background(),
		client,
		1024,
		"https://www.reddit.com/r/example/comments/abc123/post",
		customUA,
	)
	if err != nil {
		t.Fatalf("SafeFetchWithUserAgent retornou erro: %v", err)
	}
	if receivedUA != customUA {
		t.Errorf("User-Agent recebido = %q, esperado %q", receivedUA, customUA)
	}
	if string(body) != "html" {
		t.Errorf("corpo = %q, esperado html", body)
	}
	if finalURL == nil || finalURL.Hostname() != "www.reddit.com" {
		t.Errorf("URL final inesperada: %v", finalURL)
	}
}

func TestSafeFetchWithUserAgentFallsBackWhenEmpty(t *testing.T) {
	var receivedUA string
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			receivedUA = req.Header.Get("User-Agent")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ok")),
				Request:    req,
			}, nil
		}),
	}

	if _, _, err := SafeFetchWithUserAgent(
		context.Background(),
		client,
		1024,
		"https://www.reddit.com/",
		"  ",
	); err != nil {
		t.Fatalf("SafeFetchWithUserAgent retornou erro: %v", err)
	}
	if receivedUA != SafeClientUserAgent {
		t.Errorf("User-Agent vazio deveria usar o padrão %q, recebido %q", SafeClientUserAgent, receivedUA)
	}
}
