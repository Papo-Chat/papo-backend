package utils

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	_ "image/jpeg" // registra decoder JPEG para image.Decode/DecodeConfig
	_ "image/png"  // registra decoder PNG para image.Decode/DecodeConfig
	"math"
	"net/http"
	"time"

	"papo/internal/config"

	"github.com/deepteams/webp" // encoder + decoder WebP (pure Go)
	"golang.org/x/image/draw"
)

// MaxImageDimension é a dimensão máxima (px) de largura ou altura aceita nos
// endpoints de imagens (avatar, ícone de servidor e emoji).
const MaxImageDimension = 512

// MaxThumbnailDimCeiling é o teto absoluto de largura/altura (px) aceito na
// pré-check de thumbnail: bloqueia decompression bomb antes de tocar nos
// pixels (512 * 32). O limite de pixels (ThumbnailMaxPixels) é a guarda real
// de memória; este teto só rejeita dimensões patológicas.
const MaxThumbnailDimCeiling = 16384

// MaxThumbnailSize é o tamanho máximo (bytes) da thumbnail estática
// (WebP) após o re-encode.
const MaxThumbnailSize = 1 << 20

// MaxGIFFrames é o número máximo de frames de uma thumbnail de GIF animado.
const MaxGIFFrames = 100

// MaxGIFThumbSize é o tamanho máximo (bytes) da thumbnail de GIF animado após
// o re-encode.
const MaxGIFThumbSize = 2 << 20

// thumbnailStep é a granularidade (px) do alvo da long edge: o alvo é sempre
// um múltiplo de 128 (ou o tamanho original, quando menor).
const thumbnailStep = 128

// thumbnailQuality é a qualidade do re-encode WebP (0-100).
const thumbnailQuality = 80

// ErrNotProcessableImage indica que o conteúdo não é uma imagem processável
// (jpeg, png, webp ou gif) e não gera thumbnail.
var ErrNotProcessableImage = errors.New("imagem não processável")

// ValidateImage protege contra decompression bomb: decodifica apenas o
// cabeçalho da imagem (image.DecodeConfig, sem decodificar os pixels) e
// rejeita imagens cuja largura ou altura declarada exceda maxDim px. Um
// arquivo pequeno pode declarar um buffer de pixels enorme, que estouraria a
// memória em uma decodificação completa; o limite bloqueia isso na entrada.
// Retorna erro quando o conteúdo não é uma imagem reconhecível (GIF, JPEG ou
// PNG) ou quando as dimensões excedem o limite.

func ValidateImage(content []byte, maxDim int) error {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("imagem inválida: %w", err)
	}

	if cfg.Width > maxDim || cfg.Height > maxDim {
		return fmt.Errorf("dimensões da imagem (%dx%d) excedem o máximo de %dpx", cfg.Width, cfg.Height, maxDim)
	}

	return nil
}

// GenerateThumbnail decodifica a imagem, limita dimensões, redimensiona
// (long edge alvo = min(maxDim, floor(L/128)*128); nunca upscale) e
// re-encodeia. maxDim: 512 para imagens não-GIF, 128 para GIFs. Retorna
// bytes, mime, largura, altura. Erro para qualquer entrada inválida (nunca
// panico).
//
// Formatos aceitos: jpeg, png, webp e gif (detectados por magic bytes).
// Saída: WebP q80 (deepteams/webp, pure Go) — re-encode neutraliza
// metadados hostis/EXIF do original. GIF animado vira thumbnail animada
// (GIF, máx. MaxGIFFrames frames, saída ≤ MaxGIFThumbSize); qualquer falha
// no caminho animado faz fallback para thumbnail estática (WebP) do frame 0.
// Demais MIMEs retornam ErrNotProcessableImage.
func GenerateThumbnail(content []byte, maxDim int, timeout time.Duration) ([]byte, string, int, int, error) {
	if len(content) == 0 {
		return nil, "", 0, 0, ErrNotProcessableImage
	}
	if maxDim <= 0 {
		return nil, "", 0, 0, fmt.Errorf("maxDim inválido: %d", maxDim)
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	mime := http.DetectContentType(content)
	switch mime {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
	default:
		return nil, "", 0, 0, ErrNotProcessableImage
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Pré-check: dimensões declaradas no cabeçalho, antes de tocar nos pixels.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("imagem inválida: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, "", 0, 0, fmt.Errorf("dimensões inválidas da imagem: %dx%d", cfg.Width, cfg.Height)
	}
	limitPixels := config.LoadConfig().ThumbnailMaxPixels
	if int64(cfg.Width)*int64(cfg.Height) > int64(limitPixels) ||
		cfg.Width > MaxThumbnailDimCeiling || cfg.Height > MaxThumbnailDimCeiling {
		return nil, "", 0, 0, fmt.Errorf("dimensões da imagem (%dx%d) excedem o limite", cfg.Width, cfg.Height)
	}

	if mime == "image/gif" {
		return generateGIFThumbnail(content, maxDim, ctx)
	}
	return generateStaticThumbnail(content, maxDim, ctx)
}

// generateStaticThumbnail decodifica a imagem completa (jpeg/png/webp),
// redimensiona e re-encodifica (WebP).
func generateStaticThumbnail(content []byte, maxDim int, ctx context.Context) ([]byte, string, int, int, error) {
	if ctx.Err() != nil {
		return nil, "", 0, 0, ctx.Err()
	}

	img, _, err := image.Decode(bytes.NewReader(content))
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("imagem inválida: %w", err)
	}
	b := img.Bounds()
	limitPixels := config.LoadConfig().ThumbnailMaxPixels
	if b.Dx() <= 0 || b.Dy() <= 0 || int64(b.Dx())*int64(b.Dy()) > int64(limitPixels) {
		return nil, "", 0, 0, fmt.Errorf("dimensões da imagem (%dx%d) excedem o limite", b.Dx(), b.Dy())
	}

	img = resizeImage(img, maxDim)
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	return encodeStatic(img, w, h)
}

// staticGIFFallback gera thumbnail estática (WebP) do frame 0 do GIF.
func staticGIFFallback(content []byte, maxDim int) ([]byte, string, int, int, error) {
	first, err := gif.Decode(bytes.NewReader(content))
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("imagem inválida: %w", err)
	}
	return encodeStaticImage(first, maxDim)
}

// generateGIFThumbnail decide entre thumbnail animada e estática (frame 0).
// Antes de decodificar frames, um walker de blocos conta os frames e soma a
// área dos sub-rectângulos (sem decodificar pixels): bloqueia frame bomb e
// limita a memória de gif.DecodeAll (que retém todos os frames).
func generateGIFThumbnail(content []byte, maxDim int, ctx context.Context) ([]byte, string, int, int, error) {
	delays, totalArea, ok := gifFrameInfo(content)
	if !ok || len(delays) == 0 {
		// Malformado: tenta o frame 0 (best-effort).
		return staticGIFFallback(content, maxDim)
	}
	if len(delays) == 1 {
		// GIF estático (1 frame) → caminho WebP.
		return staticGIFFallback(content, maxDim)
	}
	if len(delays) > MaxGIFFrames || totalArea > int64(config.LoadConfig().ThumbnailMaxPixels) {
		// Frame bomb / teto de memória → fallback estático do frame 0.
		return staticGIFFallback(content, maxDim)
	}

	g, err := gif.DecodeAll(bytes.NewReader(content))
	if err != nil || ctx.Err() != nil {
		return staticGIFFallback(content, maxDim)
	}
	if len(g.Image) != len(delays) {
		return staticGIFFallback(content, maxDim)
	}

	W, H := g.Config.Width, g.Config.Height
	if W <= 0 || H <= 0 {
		return staticGIFFallback(content, maxDim)
	}
	TW, TH, changed := scaleTarget(W, H, maxDim)

	scaled := make([]*image.Paletted, 0, len(g.Image))
	if changed {
		sx := float64(TW) / float64(W)
		sy := float64(TH) / float64(H)
		for _, f := range g.Image {
			if ctx.Err() != nil {
				return staticGIFFallback(content, maxDim)
			}
			rb := f.Bounds()
			x0 := int(math.Round(float64(rb.Min.X) * sx))
			y0 := int(math.Round(float64(rb.Min.Y) * sy))
			x1 := int(math.Round(float64(rb.Max.X) * sx))
			y1 := int(math.Round(float64(rb.Max.Y) * sy))
			if x0 < 0 {
				x0 = 0
			}
			if y0 < 0 {
				y0 = 0
			}
			if x1 > TW {
				x1 = TW
			}
			if y1 > TH {
				y1 = TH
			}
			if x1-x0 < 1 {
				x0, x1 = TW-1, TW
			}
			if y1-y0 < 1 {
				y0, y1 = TH-1, TH
			}
			dst := image.NewPaletted(image.Rect(x0, y0, x1, y1), f.Palette)
			// Preenche com o índice transparente (se houver): image.NewPaletted
			// inicializa com índice 0, que pode ser uma cor opaca.
			if ti := transparentPaletteIndex(f.Palette); ti >= 0 {
				for p := range dst.Pix {
					dst.Pix[p] = byte(ti)
				}
			}
			draw.CatmullRom.Scale(dst, dst.Bounds(), f, rb, draw.Over, nil)
			scaled = append(scaled, dst)
		}
	} else {
		scaled = g.Image
	}

	out := &gif.GIF{
		Image:           scaled,
		Delay:           delays,
		Disposal:        g.Disposal,
		LoopCount:       g.LoopCount,
		Config:          image.Config{Width: TW, Height: TH},
		BackgroundIndex: g.BackgroundIndex,
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, out); err != nil {
		return staticGIFFallback(content, maxDim)
	}
	thumb := buf.Bytes()
	if len(thumb) > MaxGIFThumbSize {
		return staticGIFFallback(content, maxDim)
	}

	// Dimensões do canvas (não do sub-rectângulo do frame 0).
	return thumb, "image/gif", TW, TH, nil
}

// transparentPaletteIndex retorna o índice da entrada transparente
// (cor.RGBA{}) da paleta, ou -1 se não existir.
func transparentPaletteIndex(p color.Palette) int {
	for i, c := range p {
		if _, _, _, a := c.RGBA(); a == 0 {
			return i
		}
	}
	return -1
}

// gifFrameInfo extrai, caminhando os blocos do GIF (sem decodificar pixels),
// o delay (1/100s) de cada frame, a contagem de frames e a área total dos
// sub-rectângulos. A graphic control extension (0x21 0xF9) que precede cada
// image descriptor (0x2C) define o delay do frame; frames sem delay
// explícito usam 10 (100ms). ok=false em bytes inválidos.
func gifFrameInfo(content []byte) (delays []int, totalArea int64, ok bool) {
	if len(content) < 13 || content[0] != 'G' || content[1] != 'I' || content[2] != 'F' {
		return nil, 0, false
	}

	// "GIF89a" (6) + logical screen descriptor (7)
	i := 13
	// Global color table (opcional): bit 7 do packed field do descriptor.
	if packed := content[10]; packed&0x80 != 0 {
		i += int(3 << (uint(packed&0x07) + 1))
		if i > len(content) {
			return nil, 0, false
		}
	}

	delays = make([]int, 0, 16)
	current := 10

	for i < len(content) {
		switch content[i] {
		case 0x21: // extension introducer
			if i+1 >= len(content) {
				return nil, 0, false
			}
			switch content[i+1] {
			case 0xF9: // graphic control extension
				if i+6 >= len(content) {
					return nil, 0, false
				}
				if delay := int(content[i+4]) | int(content[i+5])<<8; delay > 0 {
					current = delay
				}
				i += 8 // 0x21 0xF9 0x04 + packed + delay(2) + transparent + 0x00
			case 0x01, 0xFF: // comment / application extension: pula sub-blocks
				i += 2
				if !skipSubBlocks(content, &i) {
					return nil, 0, false
				}
			default:
				return nil, 0, false
			}
		case 0x2C: // image descriptor → novo frame
			// Layout: 0x2C + left(2) + top(2) + width(2) + height(2) + packed(1)
			if i+10 > len(content) {
				return nil, 0, false
			}
			fw := int(content[i+5]) | int(content[i+6])<<8
			fh := int(content[i+7]) | int(content[i+8])<<8
			totalArea += int64(fw) * int64(fh)
			delays = append(delays, current)
			if content[i+9]&0x80 != 0 {
				// Local color table (entre o descriptor e o LZW min code size).
				i += 10 + int(3<<(uint(content[i+9]&0x07)+1))
			} else {
				i += 10
			}
			if i >= len(content) {
				return nil, 0, false
			}
			i++                              // LZW min code size
			if !skipSubBlocks(content, &i) { // LZW data sub-blocks
				return nil, 0, false
			}
		case 0x3B: // trailer
			return delays, totalArea, true
		default:
			return nil, 0, false
		}
	}
	return nil, 0, false // sem trailer: malformado
}

// skipSubBlocks avança i pastas os sub-blocks (length byte + payload) até o
// terminator 0x00. Retorna false em bytes inválidos.
func skipSubBlocks(content []byte, i *int) bool {
	for *i < len(content) && content[*i] != 0 {
		*i += 1 + int(content[*i])
		if *i > len(content) {
			return false
		}
	}
	if *i >= len(content) {
		return false
	}
	*i++ // terminator 0x00
	return true
}

// encodeStaticImage redimensiona a imagem (se necessário) e re-encodeia
// (WebP) com o teto de saída.
func encodeStaticImage(img image.Image, maxDim int) ([]byte, string, int, int, error) {
	img = resizeImage(img, maxDim)
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	return encodeStatic(img, w, h)
}

// encodeStatic re-encodeia a imagem (sempre — nunca copia bytes do
// original): WebP q80 (deepteams/webp, pure Go; Method 4 = default, bom
// equilíbrio velocidade/compressão). Re-encoding neutraliza metadados
// hostis/EXIF e garante saída pequena. Rejeita saída > MaxThumbnailSize.
func encodeStatic(img image.Image, w, h int) ([]byte, string, int, int, error) {
	var buf bytes.Buffer
	if err := webp.Encode(&buf, img, &webp.EncoderOptions{Quality: thumbnailQuality}); err != nil {
		return nil, "", 0, 0, fmt.Errorf("falha ao re-encodar imagem: %w", err)
	}
	out := buf.Bytes()
	if len(out) > MaxThumbnailSize {
		return nil, "", 0, 0, fmt.Errorf("thumbnail excede o tamanho máximo de %d bytes", MaxThumbnailSize)
	}
	return out, "image/webp", w, h, nil
}

// scaleTarget calcula o alvo (tw, th) de redimensionamento preservando o
// aspect ratio, com a long edge alvo = min(maxDim, floor(L/128)*128). Nunca
// upscale: L < 128 (alvo 0) ou alvo == L → changed=false (sem resize).
func scaleTarget(w, h, maxDim int) (tw, th int, changed bool) {
	longEdge := w
	if h > longEdge {
		longEdge = h
	}

	target := 0
	if longEdge >= thumbnailStep {
		target = (longEdge / thumbnailStep) * thumbnailStep
		if target > maxDim {
			target = maxDim
		}
	}
	if target <= 0 || target == longEdge {
		return w, h, false
	}

	if w >= h {
		tw = target
		th = (target*h + w/2) / w
	} else {
		th = target
		tw = (target*w + h/2) / h
	}
	if tw < 1 {
		tw = 1
	}
	if th < 1 {
		th = 1
	}
	return tw, th, true
}

// resizeImage redimensiona a imagem preservando o aspect ratio (ver
// scaleTarget). changed=false → retorna a imagem original.
func resizeImage(img image.Image, maxDim int) image.Image {
	b := img.Bounds()
	tw, th, changed := scaleTarget(b.Dx(), b.Dy(), maxDim)
	if !changed {
		return img
	}
	dst := image.NewNRGBA(image.Rect(0, 0, tw, th))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	return dst
}
