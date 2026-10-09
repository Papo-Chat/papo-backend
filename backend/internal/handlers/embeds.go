package handlers

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"

	"papo/internal/middleware"
	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/utils"

	"github.com/labstack/echo/v4"
)

// embedResponse é a resposta de GET /embeds/:embed_id: os campos públicos do
// embed + a imagem embutida em base64 (image_data) quando existe, e o ícone do
// autor em base64 (author_image_data) quando existe. As referências internas de
// mídia (ThumbnailFilePath, AuthorFilePath) são excluídas da serialização (json:"-").
type embedResponse struct {
	models.Embed
	ImageData       *string `json:"image_data"`
	AuthorImageData *string `json:"author_image_data"`
}

// GetEmbedHandler implementa GET /embeds/:embed_id.
// Autorização: embed_id → message_embeds → mensagem → canal → mesmo check de
// read_channel (reutilizado no service). Embed inexistente ou sem vínculo com
// mensagem acessível → 404 (não vaza existência). A resposta é o embed em JSON
// com a imagem embutida em base64 (image_data) quando existe.
func GetEmbedHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	embed, err := services.GetEmbed(c.Request().Context(), c.Param("embed_id"), userID)
	switch {
	case errors.Is(err, services.ErrEmbedNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "embed não encontrado")
	case errors.Is(err, services.ErrChannelNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "canal não encontrado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao buscar o embed: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao buscar o embed")
	}

	resp := embedResponse{Embed: embed}
	if data, err := readEmbedBlob(embed.ThumbnailFilePath); err != nil {
		utils.Errorf("request_id=%s falha ao ler a imagem do embed: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao buscar o embed")
	} else if data != nil {
		b64 := base64.StdEncoding.EncodeToString(data)
		resp.ImageData = &b64
	}
	if data, err := readEmbedBlob(embed.AuthorFilePath); err != nil {
		utils.Errorf("request_id=%s falha ao ler o ícone do autor do embed: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao buscar o embed")
	} else if data != nil {
		b64 := base64.StdEncoding.EncodeToString(data)
		resp.AuthorImageData = &b64
	}

	return c.JSON(http.StatusOK, resp)
}

// readEmbedBlob lê o blob de mídia apontado pelo caminho interno. nil, nil
// quando não há caminho ou o arquivo sumiu do disco (o embed segue sem a imagem);
// nil, err quando a leitura falha por outro motivo.
func readEmbedBlob(path *string) ([]byte, error) {
	if path == nil {
		return nil, nil
	}
	data, err := os.ReadFile(*path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	default:
		return data, nil
	}
}

// GetEmbedVideoHandler faz relay autenticado do vídeo associado ao embed
// (GET /embeds/:embed_id/video). Suporta Range para seek/playback e evita
// hotlink direto no CDN do vídeo.
func GetEmbedVideoHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	resp, release, err := services.OpenEmbedVideo(
		c.Request().Context(),
		c.Param("embed_id"),
		userID,
		c.Request().Header.Get("Range"),
	)
	switch {
	case errors.Is(err, services.ErrEmbedNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "vídeo de embed não encontrado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao abrir vídeo do embed: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusBadGateway,
			"upstream", "Mídia indisponível", "falha ao carregar vídeo do embed")
	}
	defer release()
	defer resp.Body.Close()

	out := c.Response().Header()
	for _, name := range []string{
		"Content-Type",
		"Content-Length",
		"Content-Range",
		"Accept-Ranges",
		"ETag",
		"Last-Modified",
		"Cache-Control",
	} {
		if value := resp.Header.Get(name); value != "" {
			out.Set(name, value)
		}
	}
	out.Set("Content-Disposition", "inline")
	out.Set("X-Content-Type-Options", "nosniff")

	c.Response().WriteHeader(resp.StatusCode)
	_, copyErr := io.Copy(c.Response().Writer, resp.Body)
	return copyErr
}
