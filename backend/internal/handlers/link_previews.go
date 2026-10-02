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

// linkPreviewResponse é a resposta de GET /link-previews/:preview_id: os
// campos públicos do preview + a imagem embutida em base64 (image_data) quando
// existe. ImageFilePath é excluída da serialização (json:"-").
type linkPreviewResponse struct {
	models.LinkPreview
	ImageData *string `json:"image_data"`
}

// GetLinkPreviewHandler implementa GET /link-previews/:preview_id.
// Autorização: preview_id → message_previews → mensagem → canal → mesmo check
// de read_channel (reutilizado no service). Preview inexistente ou sem vínculo
// com mensagem acessível → 404 (não vaza existência). A resposta é o preview
// em JSON com a imagem embutida em base64 (image_data) quando existe.
func GetLinkPreviewHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	preview, err := services.GetLinkPreview(c.Request().Context(), c.Param("preview_id"), userID)
	switch {
	case errors.Is(err, services.ErrPreviewNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "preview não encontrado")
	case errors.Is(err, services.ErrChannelNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "canal não encontrado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao buscar o preview: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao buscar o preview")
	}

	resp := linkPreviewResponse{LinkPreview: preview}
	if preview.ImageFilePath != nil {
		data, readErr := os.ReadFile(*preview.ImageFilePath)
		switch {
		case errors.Is(readErr, os.ErrNotExist):
			// imagem ausente em disco: devolve o preview sem a imagem
		case readErr != nil:
			utils.Errorf("request_id=%s falha ao ler a imagem do preview: %v",
				c.Request().Header.Get(echo.HeaderXRequestID), readErr)
			return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
				"internal", "Erro interno", "falha ao buscar o preview")
		default:
			b64 := base64.StdEncoding.EncodeToString(data)
			resp.ImageData = &b64
		}
	}

	return c.JSON(http.StatusOK, resp)
}


// GetLinkPreviewVideoHandler faz relay autenticado do vídeo associado ao
// preview. Suporta Range para seek/playback e evita hotlink direto no CDN do X.
func GetLinkPreviewVideoHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	resp, release, err := services.OpenLinkPreviewVideo(
		c.Request().Context(),
		c.Param("preview_id"),
		userID,
		c.Request().Header.Get("Range"),
	)
	switch {
	case errors.Is(err, services.ErrPreviewNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "vídeo de preview não encontrado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao abrir vídeo do preview: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusBadGateway,
			"upstream", "Mídia indisponível", "falha ao carregar vídeo do preview")
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
