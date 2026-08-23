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

// DownloadAttachmentHandler implementa GET /attachments/:file_id.
// O usuário precisa da permissão read_channel do canal da mensagem que possui
// o attachment (o dono do servidor do canal sempre pode). A resposta é o
// arquivo binário com o Content-Type do MIME type detectado no upload.
func DownloadAttachmentHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	attachment, err := services.DownloadAttachment(c.Request().Context(), c.Param("file_id"), userID)
	switch {
	case errors.Is(err, services.ErrAttachmentNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "arquivo não encontrado")
	case errors.Is(err, services.ErrChannelNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "canal não encontrado")
	case errors.Is(err, services.ErrPermissionDenied):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado",
			"usuário não tem permissão para baixar o arquivo")
	case err != nil:
		utils.Errorf("request_id=%s falha ao baixar attachment: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao baixar o arquivo")
	}

	file, err := os.Open(attachment.FilePath)
	if errors.Is(err, os.ErrNotExist) {
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "arquivo não encontrado")
	}
	if err != nil {
		utils.Errorf("request_id=%s falha ao abrir o blob do attachment: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao baixar o arquivo")
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		utils.Errorf("request_id=%s falha ao ler o blob do attachment: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao baixar o arquivo")
	}

	resp := c.Response()
	resp.Header().Set(echo.HeaderContentType, attachment.MimeType)
	resp.Header().Set(echo.HeaderContentDisposition, utils.ContentDisposition(attachment.OriginalFileName))
	resp.Header().Set(echo.HeaderContentLength, strconv.FormatInt(info.Size(), 10))
	resp.WriteHeader(http.StatusOK)

	if _, err := io.Copy(resp, file); err != nil {
		utils.Errorf("request_id=%s falha ao enviar o blob do attachment: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return err
	}

	return nil
}
