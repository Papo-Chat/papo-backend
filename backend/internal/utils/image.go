package utils

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// MaxImageDimension é a dimensão máxima (px) de largura ou altura aceita nos
// endpoints de imagens (avatar, ícone de servidor e emoji).
const MaxImageDimension = 512

// ValidateImage protege contra decompression bomb: decodifica apenas o
// cabeçalho da imagem (image.DecodeConfig, sem decodificar os pixels) e
// rejeita imagens cuja largura ou altura declarada exceda maxDim px. Um
// arquivo pequeno pode declarar um buffer de pixels enorme, que estouraria a
// memória em uma decodificação completa; o limite bloqueia isso na entrada.
// Retorna erro quando o conteúdo não é uma imagem reconhecível (GIF, JPEG ou
// PNG) ou quando as dimensões excedem o limite.

func ValidateImage(content []byte, maxDim int) error {
	// CheckImageCSAM(ctx, content)
	// Se quiser implementar CSAM aqui provavelmente é o ponto mais recomendado pois
	// Essa função é utilizada para validar imagens por todo o código

	cfg, _, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("imagem inválida: %w", err)
	}

	if cfg.Width > maxDim || cfg.Height > maxDim {
		return fmt.Errorf("dimensões da imagem (%dx%d) excedem o máximo de %dpx", cfg.Width, cfg.Height, maxDim)
	}

	return nil
}
