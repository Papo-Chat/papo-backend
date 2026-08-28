package handlers

import (
	"errors"
	"net/http"
	"os"

	"papo/internal/middleware"
	"papo/internal/services"
	"papo/internal/utils"

	"github.com/labstack/echo/v4"
)

// GetMediaHandler implementa GET /media/:sha_hash.
// Serve o blob de mídia content-addressable (banner, avatar, ícone, ...) com o
// Content-Type do MIME type registrado na tabela media e Content-Disposition:
// inline (usado diretamente em <img>). Suporta Range requests (206 Partial
// Content) via http.ServeContent. O sha_hash é unguessable (sha256), então
// não há check de permissão além da autenticação.
func GetMediaHandler(baseURL string, c echo.Context) error {
	if _, ok := c.Get(middleware.UserIDContextKey).(string); !ok {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	shaHash := c.Param("sha_hash")
	if shaHash == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "sha_hash ausente")
	}

	media, filePath, err := services.GetMedia(c.Request().Context(), shaHash)
	switch {
	case errors.Is(err, services.ErrMediaNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "mídia não encontrada")
	case err != nil:
		utils.Errorf("request_id=%s falha ao buscar a mídia: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao buscar a mídia")
	}

	file, err := os.Open(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "mídia não encontrada")
	}
	if err != nil {
		utils.Errorf("request_id=%s falha ao abrir o blob da mídia: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao buscar a mídia")
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		utils.Errorf("request_id=%s falha ao ler o blob da mídia: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao buscar a mídia")
	}

	resp := c.Response()
	resp.Header().Set(echo.HeaderContentType, media.MimeType)
	resp.Header().Set(echo.HeaderContentDisposition, "inline")

	// ServeContent trata Range requests (206 Partial Content, 416 para
	// intervalo fora do alcance) e If-Modified-Since (304). O blob é
	// content-addressable e imutável, então o ModTime é estável.
	http.ServeContent(resp, c.Request(), shaHash, info.ModTime(), file)

	return nil
}
