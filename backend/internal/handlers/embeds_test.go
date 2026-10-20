package handlers

import (
	"testing"

	"papo/internal/models"
)

func TestEmbedImagePath(t *testing.T) {
	thumbnailPath := "/cache/thumbnail.webp"
	imagePath := "/cache/image.webp"

	tests := []struct {
		name string
		embed models.Embed
		want *string
	}{
		{
			name: "prefers thumbnail when present",
			embed: models.Embed{
				ThumbnailFilePath: &thumbnailPath,
				ImageFilePath:     &imagePath,
			},
			want: &thumbnailPath,
		},
		{
			name: "falls back to image-only media",
			embed: models.Embed{
				ImageFilePath: &imagePath,
			},
			want: &imagePath,
		},
		{
			name: "returns nil when there is no image",
			embed: models.Embed{},
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := embedImagePath(tt.embed)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("esperava nil, obtive %q", *got)
				}
				return
			}
			if got == nil || *got != *tt.want {
				t.Fatalf("esperava %q, obtive %v", *tt.want, got)
			}
		})
	}
}
