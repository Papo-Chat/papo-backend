package utils

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net"
	"strings"
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
		{"webp 20x20", makeWebP(t, 20, 20)},
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

// makeWebP gera um WebP válido usando o output do GenerateThumbnail (que
// sai em WebP) — sem depender de encoder externo.
func makeWebP(t *testing.T, w, h int) []byte {
	t.Helper()
	webp, _, _, _, err := GenerateThumbnail(makePNG(t, w, h), 512, 0)
	if err != nil {
		t.Fatalf("falha ao gerar WebP: %v", err)
	}
	return webp
}

func makeAnimatedGIF(t *testing.T, w, h, frames int) []byte {
	t.Helper()
	pal := color.Palette{color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 0, 255}}
	imgs := make([]*image.Paletted, frames)
	delay := make([]int, frames)
	for f := 0; f < frames; f++ {
		img := image.NewPaletted(image.Rect(0, 0, w, h), pal)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				img.SetColorIndex(x, y, uint8(f%2))
			}
		}
		imgs[f] = img
		delay[f] = 10
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, &gif.GIF{Image: imgs, Delay: delay, LoopCount: 0}); err != nil {
		t.Fatalf("falha ao codificar GIF animado: %v", err)
	}
	return buf.Bytes()
}

func TestGenerateThumbnailStaticWebP(t *testing.T) {
	thumb, mime, w, h, err := GenerateThumbnail(makePNG(t, 1200, 800), 512, 0)
	if err != nil {
		t.Fatalf("GenerateThumbnail: %v", err)
	}
	if mime != "image/webp" {
		t.Errorf("mime esperado image/webp, obtido %s", mime)
	}
	if w > 512 || h > 512 {
		t.Errorf("dimensão máxima excedida: %dx%d", w, h)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(thumb))
	if err != nil {
		t.Fatalf("falha ao decodificar o WebP gerado: %v", err)
	}
	if cfg.Width != w || cfg.Height != h {
		t.Errorf("dimensões reportadas %dx%d não batem com o decodificado %dx%d", w, h, cfg.Width, cfg.Height)
	}
}

func TestGenerateThumbnailJPEGInput(t *testing.T) {
	_, mime, _, _, err := GenerateThumbnail(makeJPEG(t, 640, 480), 512, 0)
	if err != nil {
		t.Fatalf("GenerateThumbnail: %v", err)
	}
	if mime != "image/webp" {
		t.Errorf("mime esperado image/webp, obtido %s", mime)
	}
}

func TestGenerateThumbnailNoUpscale(t *testing.T) {
	thumb, _, w, h, err := GenerateThumbnail(makePNG(t, 100, 50), 512, 0)
	if err != nil {
		t.Fatalf("GenerateThumbnail: %v", err)
	}
	if w != 100 || h != 50 {
		t.Errorf("imagem menor que o step não deve ser redimensionada: %dx%d", w, h)
	}
	if len(thumb) == 0 {
		t.Error("thumbnail vazia")
	}
}

func TestGenerateThumbnailAnimatedGIF(t *testing.T) {
	_, mime, w, h, err := GenerateThumbnail(makeAnimatedGIF(t, 200, 100, 2), 128, 0)
	if err != nil {
		t.Fatalf("GenerateThumbnail: %v", err)
	}
	if mime != "image/webp" {
		t.Fatalf("mime esperado image/webp, obtido %s", mime)
	}
	if w != 128 || h != 64 {
		t.Errorf("canvas esperado 128x64, obtido %dx%d", w, h)
	}
}

func TestGenerateThumbnailStaticGIFWebP(t *testing.T) {
	_, mime, _, _, err := GenerateThumbnail(makeGIF(t, 10, 10), 512, 0)
	if err != nil {
		t.Fatalf("GenerateThumbnail: %v", err)
	}
	if mime != "image/webp" {
		t.Errorf("GIF estático deve virar WebP, obtido %s", mime)
	}
}

func TestGenerateThumbnailWebPInput(t *testing.T) {
	// WebP é formato de entrada aceito (upload): gera um WebP e o usa como input.
	first, _, _, _, err := GenerateThumbnail(makePNG(t, 300, 200), 512, 0)
	if err != nil {
		t.Fatalf("GenerateThumbnail (primeira): %v", err)
	}
	second, mime, w, h, err := GenerateThumbnail(first, 512, 0)
	if err != nil {
		t.Fatalf("GenerateThumbnail (WebP como input): %v", err)
	}
	if mime != "image/webp" {
		t.Errorf("mime esperado image/webp, obtido %s", mime)
	}
	// 300x200 → alvo 256 (múltiplo de 128) → 256x171; segunda passada sem resize.
	if w != 256 || h != 171 {
		t.Errorf("dimensões esperadas 256x171, obtidas %dx%d", w, h)
	}
	if len(second) == 0 {
		t.Error("thumbnail vazia")
	}
}

func TestGIFFrameInfoGlobalColorTable(t *testing.T) {
	// GIF minimal com global color table (packed 0x80 → 2 cores) e frame sem
	// local color table: testa o skip do GCT no walker.
	gif := []byte{
		'G', 'I', 'F', '8', '9', 'a', // header
		0x0a, 0x00, 0x0a, 0x00, 0x80, 0x00, 0x00, // LSD 10x10, GCT 2 cores
		0xff, 0x00, 0x00, 0x00, 0x00, 0x00, // GCT (2 cores)
		0x21, 0xf9, 0x04, 0x00, 0x0a, 0x00, 0x00, 0x00, // GCE delay=10
		0x2c, 0x00, 0x00, 0x00, 0x00, 0x0a, 0x00, 0x0a, 0x00, 0x00, // image descriptor 10x10, sem LCT
		0x02,             // LZW min code size
		0x01, 0x41, 0x00, // sub-block + terminator
		0x3b, // trailer
	}
	delays, area, ok := gifFrameInfo(gif)
	if !ok {
		t.Fatal("ok esperado true para GIF com GCT")
	}
	if len(delays) != 1 || delays[0] != 10 {
		t.Errorf("delays esperados [10], obtidos %v", delays)
	}
	if area != 100 {
		t.Errorf("área esperada 100, obtida %d", area)
	}
}

func TestGenerateThumbnailNotImage(t *testing.T) {
	cases := []struct {
		name    string
		content []byte
	}{
		{"lixo", []byte("isso não é uma imagem")},
		{"vazio", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, _, err := GenerateThumbnail(tc.content, 512, 0); err != ErrNotProcessableImage {
				t.Errorf("erro esperado ErrNotProcessableImage, obtido %v", err)
			}
		})
	}
}

func TestGIFFrameInfo(t *testing.T) {
	t.Run("animado com local color table", func(t *testing.T) {
		delays, area, ok := gifFrameInfo(makeAnimatedGIF(t, 200, 100, 2))
		if !ok {
			t.Fatal("ok esperado true")
		}
		if len(delays) != 2 || delays[0] != 10 || delays[1] != 10 {
			t.Errorf("delays esperados [10 10], obtidos %v", delays)
		}
		if area != 2*200*100 {
			t.Errorf("área total esperada %d, obtida %d", 2*200*100, area)
		}
	})
	t.Run("estático", func(t *testing.T) {
		delays, area, ok := gifFrameInfo(makeGIF(t, 30, 20))
		if !ok {
			t.Fatal("ok esperado true")
		}
		if len(delays) != 1 {
			t.Errorf("frames esperados 1, obtidos %d", len(delays))
		}
		if area != 30*20 {
			t.Errorf("área total esperada %d, obtida %d", 30*20, area)
		}
	})
	t.Run("inválido", func(t *testing.T) {
		if _, _, ok := gifFrameInfo([]byte("GIF89a lixo")); ok {
			t.Error("ok esperado false para GIF inválido")
		}
	})
}

func TestScaleTarget(t *testing.T) {
	cases := []struct {
		w, h, maxDim int
		tw, th       int
		changed      bool
	}{
		{1200, 800, 512, 512, 341, true},
		{800, 1200, 512, 341, 512, true},
		{300, 200, 512, 256, 171, true},
		{100, 50, 512, 100, 50, false},
		{512, 100, 512, 512, 100, false},
		{0, 0, 512, 0, 0, false},
	}
	for _, tc := range cases {
		tw, th, changed := scaleTarget(tc.w, tc.h, tc.maxDim)
		if tw != tc.tw || th != tc.th || changed != tc.changed {
			t.Errorf("scaleTarget(%d,%d,%d) = (%d,%d,%v), esperado (%d,%d,%v)",
				tc.w, tc.h, tc.maxDim, tw, th, changed, tc.tw, tc.th, tc.changed)
		}
	}
}

func TestNormalizeURL(t *testing.T) {
	valid := []struct {
		raw, want string
	}{
		{"https://Exemplo.COM/Rota?b=2#frag", "https://exemplo.com/Rota?b=2"},
		{"https://exemplo.com:443/x", "https://exemplo.com/x"},
		{"http://exemplo.com:80/x", "http://exemplo.com/x"},
		{"  https://exemplo.com/  ", "https://exemplo.com/"},
	}
	for _, tc := range valid {
		u, err := NormalizeURL(tc.raw)
		if err != nil {
			t.Errorf("NormalizeURL(%q) retornou erro inesperado: %v", tc.raw, err)
			continue
		}
		if u.String() != tc.want {
			t.Errorf("NormalizeURL(%q) = %q, esperado %q", tc.raw, u.String(), tc.want)
		}
	}

	invalid := []string{
		"",
		"   ",
		"exemplo.com/x",
		"https://",
		"ftp://exemplo.com/x",
		"javascript:alert(1)",
		"https://user:pass@exemplo.com/",
		"https://exemplo.com:80/x",
		"http://exemplo.com:443/x",
		"http://exemplo.com:8080/x",
	}
	for _, raw := range invalid {
		if _, err := NormalizeURL(raw); !errors.Is(err, ErrInvalidURL) {
			t.Errorf("NormalizeURL(%q) deveria retornar ErrInvalidURL, obtive %v", raw, err)
		}
	}
}

func TestOriginURL(t *testing.T) {
	u, err := NormalizeURL("https://Exemplo.COM:443/a/b?c=1")
	if err != nil {
		t.Fatalf("NormalizeURL retornou erro: %v", err)
	}
	if got := OriginURL(u); got != "https://exemplo.com" {
		t.Errorf("OriginURL = %q, esperado %q", got, "https://exemplo.com")
	}
}

func TestCheckSSRFSafeIP(t *testing.T) {
	allowed := []string{
		"8.8.8.8",
		"1.1.1.1",
		"203.0.112.1",
		"2001:4860:4860::8888",
		"::ffff:8.8.8.8",
		"64:ff9b::8.8.8.8",
	}
	for _, s := range allowed {
		if err := checkSSRFSafeIP(net.ParseIP(s)); err != nil {
			t.Errorf("checkSSRFSafeIP(%s) deveria permitir, obtive %v", s, err)
		}
	}

	blocked := []string{
		"0.0.0.0",
		"10.0.0.1",
		"100.64.0.1",
		"127.0.0.1",
		"169.254.169.254",
		"172.16.0.1",
		"192.0.0.1",
		"192.168.1.1",
		"198.18.0.1",
		"224.0.0.1",
		"240.0.0.1",
		"255.255.255.255",
		"::",
		"::1",
		"fe80::1",
		"fc00::1",
		"ff02::1",
		"::ffff:127.0.0.1",
		"64:ff9b::7f00:1",
	}
	for _, s := range blocked {
		err := checkSSRFSafeIP(net.ParseIP(s))
		if !errors.Is(err, ErrSSRFBlocked) {
			t.Errorf("checkSSRFSafeIP(%s) deveria bloquear com ErrSSRFBlocked, obtive %v", s, err)
		}
	}
}

func TestReadLimitedBody(t *testing.T) {
	body, err := readLimitedBody(strings.NewReader("abc"), 3)
	if err != nil {
		t.Fatalf("corpo com exatamente o limite deveria ser aceito, obtive %v", err)
	}
	if string(body) != "abc" {
		t.Errorf("corpo = %q, esperado %q", body, "abc")
	}

	if _, err := readLimitedBody(strings.NewReader("abcd"), 3); !errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("corpo acima do limite deveria retornar ErrBodyTooLarge, obtive %v", err)
	}
}
