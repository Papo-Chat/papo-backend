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
// embed + a imagem embutida em base64 (image_data) quando existe. A referência
// interna da mídia (ThumbnailFilePath) é excluída da serialização (json:"-").
type embedResponse struct {
	models.Embed
	ImageData *string `json:"image_data"`
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
	if embed.ThumbnailFilePath != nil {
		data, readErr := os.ReadFile(*embed.ThumbnailFilePath)
		switch {
		case errors.Is(readErr, os.ErrNotExist):
			// imagem ausente em disco: devolve o embed sem a imagem
		case readErr != nil:
			utils.Errorf("request_id=%s falha ao ler a imagem do embed: %v",
				c.Request().Header.Get(echo.HeaderXRequestID), readErr)
			return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
				"internal", "Erro interno", "falha ao buscar o embed")
		default:
			b64 := base64.StdEncoding.EncodeToString(data)
			resp.ImageData = &b64
		}
	}

	return c.JSON(http.StatusOK, resp)
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
