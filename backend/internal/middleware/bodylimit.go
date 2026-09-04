package middleware

import (
	"errors"
	"io"
	"net/http"

	"papo/internal/config"
	"papo/internal/utils"

	"github.com/labstack/echo/v4"
)

// MaxJSONBodySize é o limite global de corpo de requisição JSON (4MB): o
// maior payload JSON é uma imagem de 2MB em base64 (avatar/banner/ícone),
// ~2.7MB codificado.
const MaxJSONBodySize = 4 << 20

// MaxUploadBodySize é o limite de POST /messages (multipart/form-data com
// attachments de até 100MB + overhead do form), coerente com o
// ParseMultipartForm(110 << 20) do handler.
const MaxUploadBodySize = 110 << 20

// ErrBodyLimitExceeded indica que o corpo excedeu o limite durante a leitura
// (streaming/chunked, sem Content-Length confiável).
var ErrBodyLimitExceeded = errors.New("corpo da requisição excede o limite")

// BodyLimit limita o tamanho do corpo da requisição em dois estágios:
//  1. Content-Length declarado acima do limite → 413 (RFC 7807) imediato,
//     sem ler o corpo;
//  2. corpo lido em streaming (chunked) acima do limite → leitura interrompida
//     com ErrBodyLimitExceeded (o handler responde 400 no bind, mesmo
//     comportamento do BodyLimit oficial do Echo).
//
// skip (opcional) dispensa o limite para rotas específicas (ex.: upload com
// limite próprio maior).
func BodyLimit(limit int64, skip func(echo.Context) bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if skip != nil && skip(c) {
				return next(c)
			}

			req := c.Request()
			if req.ContentLength > limit {
				cfg := config.LoadConfig()
				return utils.SendProblem(c, cfg.BaseURL, http.StatusRequestEntityTooLarge,
					"payload-too-large", "Payload muito grande",
					"corpo da requisição excede o tamanho máximo permitido")
			}

			req.Body = &bodyLimitReader{r: req.Body, limit: limit}
			return next(c)
		}
	}
}

// bodyLimitReader interrompe a leitura do corpo quando o total lido excede o
// limite (proteção para corpos chunked sem Content-Length).
type bodyLimitReader struct {
	r     io.ReadCloser
	read  int64
	limit int64
}

func (b *bodyLimitReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read += int64(n)
	if b.read > b.limit {
		return n, ErrBodyLimitExceeded
	}
	return n, err
}

func (b *bodyLimitReader) Close() error {
	return b.r.Close()
}
