package storage

import (
	"testing"

	"papo/internal/models"
)

func TestEmbedFromRowPreservesDirectKlipyThumbnailURL(t *testing.T) {
	raw := "https://static.klipy.com/ii/a/b.gif?opaque=token"
	row := embedRow{
		ID: "embed-id",
		SourceType: "link",
		FetchMethod: "klipy",
		ThumbnailURL: &raw,
	}
	got := embedFromRow(row)
	if got.Thumbnail == nil {
		t.Fatal("expected direct thumbnail")
	}
	if got.Thumbnail.URL == nil || *got.Thumbnail.URL != raw {
		t.Fatalf("thumbnail URL = %v, want original URL %q", got.Thumbnail.URL, raw)
	}
	if got.Thumbnail.MimeType == nil || *got.Thumbnail.MimeType != "image/gif" {
		t.Fatalf("thumbnail MIME type = %v, want image/gif", got.Thumbnail.MimeType)
	}

	args := embedInsertArgs(models.Embed{
		SourceType: "link",
		FetchMethod: "klipy",
		Thumbnail: &models.EmbedMedia{URL: &raw},
	})
	if got := args[len(args)-1]; got != &raw {
		t.Fatalf("last INSERT arg = %v, want pointer to direct thumbnail URL", got)
	}
	ordinaryArgs := embedInsertArgs(models.Embed{
		FetchMethod: "manual",
		Thumbnail: &models.EmbedMedia{URL: &raw},
	})
	if got := ordinaryArgs[len(ordinaryArgs)-1]; got != nil {
		t.Fatalf("non-Klipy INSERT must not persist a direct URL, got %v", got)
	}
}
