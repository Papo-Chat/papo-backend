package utils

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDetectVideoContainerMIMETypes(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "MP4",
			data: testMP4Box("ftyp", []byte("isom0000isommp42")),
			want: "video/mp4",
		},
		{
			name: "QuickTime MOV",
			data: testMP4Box("ftyp", []byte("qt  0000qt  ")),
			want: "video/quicktime",
		},
		{
			name: "M4A remains audio",
			data: testMP4Box("ftyp", []byte("M4A 0000isom")),
			want: "audio/mp4",
		},
		{
			name: "AVIF remains image",
			data: testMP4Box("ftyp", []byte("avif0000avif")),
			want: "image/avif",
		},
		{
			name: "HEIC remains image",
			data: testMP4Box("ftyp", []byte("heic0000mif1")),
			want: "image/heic",
		},
		{
			name: "WebM",
			data: testEBMLElement([]byte{0x1a, 0x45, 0xdf, 0xa3},
				testEBMLElement([]byte{0x42, 0x82}, []byte("webm"))),
			want: "video/webm",
		},
		{
			name: "Matroska",
			data: testEBMLElement([]byte{0x1a, 0x45, 0xdf, 0xa3},
				testEBMLElement([]byte{0x42, 0x82}, []byte("matroska"))),
			want: "video/x-matroska",
		},
		{
			name: "AVI",
			data: append([]byte("RIFF"), append([]byte{4, 0, 0, 0}, []byte("AVI ")...)...),
			want: "video/x-msvideo",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectMimeType(tt.data); got != tt.want {
				t.Fatalf("DetectMimeType() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectMimeTypeRejectsMalformedContainerHeaders(t *testing.T) {
	tests := [][]byte{
		{0, 0, 0, 0, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'},
		{0x1a, 0x45, 0xdf, 0xa3, 0xff}, // malformed EBML size
	}
	for _, data := range tests {
		if got := detectContainerMimeType(data); got != "" {
			t.Fatalf("malformed container detected as %q", got)
		}
	}
}

func TestDetectedVideoMIMEIsPreservedForFullAndRangeResponses(t *testing.T) {
	data := append(testMP4Box("ftyp", []byte("isom0000isommp42")), bytes.Repeat([]byte{0x01}, 64)...)
	mimeType := DetectMimeType(data)
	if mimeType != "video/mp4" {
		t.Fatalf("DetectMimeType() = %q, want video/mp4", mimeType)
	}

	tests := []struct {
		name       string
		rangeValue string
		wantStatus int
	}{
		{name: "full GET", wantStatus: http.StatusOK},
		{name: "seek range", rangeValue: "bytes=0-7", wantStatus: http.StatusPartialContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/attachments/video", nil)
			if tt.rangeValue != "" {
				req.Header.Set("Range", tt.rangeValue)
			}
			rec := httptest.NewRecorder()
			rec.Header().Set("Content-Type", mimeType)
			rec.Header().Set("X-Content-Type-Options", "nosniff")
			http.ServeContent(rec, req, "video.mp4", time.Unix(1, 0), bytes.NewReader(data))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Content-Type"); got != mimeType {
				t.Fatalf("Content-Type = %q, want %q", got, mimeType)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
			}
		})
	}
}
