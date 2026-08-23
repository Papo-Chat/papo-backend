package utils

import (
	"bytes"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestValidateImageValid(t *testing.T) {
	cases := []struct {
		name    string
		content []byte
	}{
		{"png 100x50", makePNG(t, 100, 50)},
		{"png 512x512", makePNG(t, 512, 512)},
		{"gif 10x10", makeGIF(t, 10, 10)},
		{"jpeg 20x20", makeJPEG(t, 20, 20)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateImage(tc.content, MaxImageDimension); err != nil {
				t.Fatalf("ValidateImage retornou erro inesperado: %v", err)
			}
		})
	}
}

func TestValidateImageDimensionExceeded(t *testing.T) {
	cases := []struct {
		name    string
		content []byte
		maxDim  int
	}{
		{"png 513x100", makePNG(t, 513, 100), 512},
		{"png 100x513", makePNG(t, 100, 513), 512},
		{"png 600x600", makePNG(t, 600, 600), 512},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateImage(tc.content, tc.maxDim); err == nil {
				t.Fatal("ValidateImage deveria retornar erro para dimensão acima do limite")
			}
		})
	}
}

func TestValidateImageInvalidContent(t *testing.T) {
	cases := []struct {
		name    string
		content []byte
	}{
		{"vazio", []byte{}},
		{"lixo", []byte("isso não é uma imagem")},
		{"truncada", makePNG(t, 10, 10)[:10]},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateImage(tc.content, MaxImageDimension); err == nil {
				t.Fatal("ValidateImage deveria retornar erro para conteúdo inválido")
			}
		})
	}
}

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("falha ao codificar PNG: %v", err)
	}
	return buf.Bytes()
}

func makeGIF(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatalf("falha ao codificar GIF: %v", err)
	}
	return buf.Bytes()
}

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("falha ao codificar JPEG: %v", err)
	}
	return buf.Bytes()
}
