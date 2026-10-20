package services

import (
	"context"
	"errors"
	"testing"

	"papo/internal/models"
)

func TestEmbedAttachmentReferenceValidation(t *testing.T) {
	input := []EmbedInput{{
		Title: "custom image",
		Thumbnail: &EmbedMediaInput{
			AttachmentName: "discord-embed-123.png",
			MimeType:       "image/png",
		},
	}}

	if err := validateCustomEmbeds(input); err != nil {
		t.Fatalf("attachment-backed thumbnail should validate: %v", err)
	}
	if err := validateEmbedAttachmentReferences(input, []AttachmentInput{
		{OriginalFileName: "discord-embed-123.png"},
	}); err != nil {
		t.Fatalf("reference to an attachment in the same message should validate: %v", err)
	}

	if err := validateEmbedAttachmentReferences(input, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing referenced attachment should be rejected, got %v", err)
	}
	if err := validateEmbedAttachmentReferences(input, []AttachmentInput{
		{OriginalFileName: "discord-embed-123.png"},
		{OriginalFileName: "discord-embed-123.png"},
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("ambiguous attachment name should be rejected, got %v", err)
	}
}

func TestBuildCustomEmbedUsesAttachmentBytesForThumbnail(t *testing.T) {
	attachment := models.Attachments{
		ID:               "attachment-id",
		OriginalFileName: "discord-embed-123.png",
		MediaShaHash:     "abc123",
		MimeType:         "image/png",
		SizeBytes:        1024,
	}
	input := EmbedInput{
		Title: "Sua imagem de perfil.",
		Thumbnail: &EmbedMediaInput{
			AttachmentName: attachment.OriginalFileName,
			MimeType:       "image/png",
		},
	}

	embed, _ := buildCustomEmbedWithAttachments(input, map[string]models.Attachments{
		attachment.OriginalFileName: attachment,
	})
	if embed.Thumbnail == nil {
		t.Fatal("expected a thumbnail backed by the uploaded attachment")
	}
	if embed.Thumbnail.URL != nil {
		t.Fatalf("attachment-backed thumbnail must not require a remote URL, got %q", *embed.Thumbnail.URL)
	}
	if embed.Thumbnail.MediaSHA == nil || *embed.Thumbnail.MediaSHA != attachment.MediaShaHash {
		t.Fatalf("expected media hash %q, got %+v", attachment.MediaShaHash, embed.Thumbnail.MediaSHA)
	}
	if embed.Thumbnail.MimeType == nil || *embed.Thumbnail.MimeType != "image/png" {
		t.Fatalf("expected stored image MIME, got %+v", embed.Thumbnail.MimeType)
	}

	resolved := resolveCustomEmbedImage(context.Background(), "user-id", embed.Thumbnail)
	if resolved == nil || resolved.MediaSHA == nil || *resolved.MediaSHA != attachment.MediaShaHash {
		t.Fatalf("attachment-backed thumbnail should bypass external download and remain resolved, got %+v", resolved)
	}
}
