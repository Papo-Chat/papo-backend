package handlers

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"

	"papo/internal/middleware"
	"papo/internal/services"
	"papo/internal/utils"

	"github.com/labstack/echo/v4"
)

// GetLinkPreviewImageHandler implementa GET /link-previews/:preview_id/image.
// Autorização: preview_id → message_previews → mensagem → canal → mesmo check
// de read_channel (reutilizado no service). Preview sem imagem ou sem vínculo
// com mensagem acessível → 404. A resposta é a thumbnail da imagem do
// preview com Content-Disposition: inline (usada em <img>).
func GetLinkPreviewImageHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	preview, err := services.GetLinkPreviewImage(c.Request().Context(), c.Param("preview_id"), userID)
	switch {
	case errors.Is(err, services.ErrPreviewNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "imagem do preview não encontrada")
	case errors.Is(err, services.ErrChannelNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "canal não encontrado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao buscar imagem do preview: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao buscar a imagem do preview")
	}

	file, err := os.Open(*preview.ImageFilePath)
	if errors.Is(err, os.ErrNotExist) {
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "imagem do preview não encontrada")
	}
	if err != nil {
		utils.Errorf("request_id=%s falha ao abrir a imagem do preview: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao buscar a imagem do preview")
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		utils.Errorf("request_id=%s falha ao ler a imagem do preview: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao buscar a imagem do preview")
	}

	resp := c.Response()
	resp.Header().Set(echo.HeaderContentType, derefString(preview.ImageMimeType))
	resp.Header().Set(echo.HeaderContentDisposition, "inline")
	resp.Header().Set(echo.HeaderContentLength, strconv.FormatInt(info.Size(), 10))
	resp.WriteHeader(http.StatusOK)

	if _, err := io.Copy(resp, file); err != nil {
		utils.Errorf("request_id=%s falha ao enviar a imagem do preview: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return err
	}

	return nil
}
