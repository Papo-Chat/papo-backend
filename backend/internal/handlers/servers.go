package handlers

import (
	"errors"
	"net/http"
	"time"

	"papo/internal/config"
	"papo/internal/middleware"
	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/utils"
	"papo/internal/websocket"

	"github.com/labstack/echo/v4"
)

// serverDetail é a visão do servidor (GET /server; o sistema
// tem um único servidor, 1 backend = 1 server).
type serverDetail struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	IconBlob      []byte    `json:"icon_blob"`
	IconFormat    string    `json:"icon_format"`
	OwnerID       *string   `json:"owner_id"`
	Public        bool      `json:"public"`
	OwnerUsername *string   `json:"owner_username"`
	CreatedAt     time.Time `json:"created_at"`
	RoleCount     int       `json:"role_count"`
	MemberCount   int       `json:"member_count"`
	ChannelCount  int       `json:"channel_count"`
}

// GetServerHandler implementa GET /server (o único servidor do backend).
func GetServerHandler(baseURL string, c echo.Context) error {
	summary, err := services.GetServer(c.Request().Context())
	switch {
	case errors.Is(err, services.ErrServerNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "servidor não encontrado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao recuperar o servidor: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao recuperar o servidor")
	}

	return c.JSON(http.StatusOK, toServerDetail(summary))
}

type createServerRequest struct {
	Name       string  `json:"name"`
	IconBlob   string  `json:"icon_blob"`
	IconFormat string  `json:"icon_format"`
	Password   *string `json:"password"`
	Public     bool    `json:"public"`
}

// serverCreated é a visão de servidor do POST /server.
// Não inclui contagens, presente apenas no detalhe (openapi.yml).
type serverCreated struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	IconBlob      []byte    `json:"icon_blob"`
	IconFormat    string    `json:"icon_format"`
	OwnerID       *string   `json:"owner_id"`
	OwnerUsername *string   `json:"owner_username"`
	CreatedAt     time.Time `json:"created_at"`
}

// CreateServerHandler implementa POST /server.
// Permissão: a tabela servers não possui registry, então o usuário
// autenticado é o dono do servidor (README).
func CreateServerHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	var req createServerRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}
	password := req.Password

	cfg := config.LoadConfig()
	_, err := services.CreateServerWithIcon(c.Request().Context(), req.Name, req.IconBlob, req.IconFormat, req.Public, password, &userID)
	if resp, ok := passwordPolicyResponse(c, baseURL, err, cfg.MinPasswordLength); ok {
		return resp
	}
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"name é obrigatório e deve ter no máximo 32 caracteres; icon_blob deve ser base64 de um GIF, JPEG/JPG, PNG ou WEBP de até 2MB; servidor privado (public=false) exige password")
	case errors.Is(err, services.ErrServerAlreadyCreated):
		return utils.SendProblem(c, baseURL, http.StatusConflict,
			"server-already-exists", "Ação Proibida",
			"O servidor já foi criado, não há como criar mais de 1 servidor")
	case err != nil:
		utils.Errorf("request_id=%s falha ao criar o servidor: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao criar o servidor")
	}

	summary, err := services.GetServer(c.Request().Context())
	if err != nil {
		utils.Errorf("request_id=%s falha ao recuperar o servidor criado: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao recuperar o servidor criado")
	}

	return c.JSON(http.StatusOK, serverCreated{
		ID:            summary.ID,
		Name:          summary.Name,
		IconBlob:      summary.IconBlob,
		IconFormat:    summary.IconFormat,
		OwnerID:       summary.OwnerID,
		OwnerUsername: summary.OwnerUsername,
		CreatedAt:     summary.CreatedAt,
	})
}

type replaceServerRequest struct {
	Name       *string `json:"name"`
	IconBlob   *string `json:"icon_blob"`
	IconFormat *string `json:"icon_format"`
	Password   *string `json:"password"`
	Public     *bool   `json:"public"`
}

type patchServerRequest struct {
	Name       *string `json:"name"`
	IconBlob   *string `json:"icon_blob"`
	IconFormat *string `json:"icon_format"`
	Password   *string `json:"password"`
	Public     *bool   `json:"public"`
}

// ReplaceServerHandler implementa PUT /server como substituição completa dos
// campos mutáveis. Todos os campos do request são obrigatórios.
func ReplaceServerHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	var req replaceServerRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}
	if req.Name == nil || req.IconBlob == nil || req.IconFormat == nil || req.Password == nil || req.Public == nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"campos 'name', 'icon_blob', 'icon_format', 'password' e 'public' são obrigatórios")
	}

	result, err := services.ReplaceServerWithResult(c.Request().Context(), userID, *req.Name, *req.IconBlob, *req.IconFormat, *req.Password, *req.Public)
	return serverUpdateResponse(baseURL, c, result, err)
}

// PatchServerHandler implementa PATCH /server como atualização parcial.
// Campos ausentes preservam os valores atuais.
func PatchServerHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	var req patchServerRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	result, err := services.PatchServerWithResult(c.Request().Context(), userID, req.Name, req.IconBlob, req.IconFormat, req.Public, req.Password)
	return serverUpdateResponse(baseURL, c, result, err)
}

func serverUpdateResponse(baseURL string, c echo.Context, result services.ServerUpdateResult, err error) error {
	cfg := config.LoadConfig()
	if resp, ok := passwordPolicyResponse(c, baseURL, err, cfg.MinPasswordLength); ok {
		return resp
	}
	switch {
	case errors.Is(err, services.ErrServerNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "servidor não encontrado")
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"name, quando informado, deve ter entre 1 e 32 caracteres; icon_blob/icon_format devem representar GIF, JPEG/JPG, PNG ou WEBP de até 2MB; servidor privado exige senha válida")
	case err != nil:
		utils.Errorf("request_id=%s falha ao atualizar o servidor: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao atualizar o servidor")
	}

	if result.PasswordChanged {
		disconnected := websocket.GetHub().DisconnectAll()
		utils.Infof("request_id=%s senha do servidor alterada: %d websocket(s) encerrado(s)",
			c.Request().Header.Get(echo.HeaderXRequestID), disconnected)
	}

	updated, err := services.GetServer(c.Request().Context())
	if err != nil {
		utils.Errorf("request_id=%s falha ao recuperar o servidor atualizado: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao recuperar o servidor atualizado")
	}

	return c.JSON(http.StatusOK, toServerDetail(updated))
}

func toServerDetail(summary models.ServerSummary) serverDetail {
	return serverDetail{
		ID:            summary.ID,
		Name:          summary.Name,
		IconBlob:      summary.IconBlob,
		IconFormat:    summary.IconFormat,
		OwnerID:       summary.OwnerID,
		Public:        summary.Public,
		OwnerUsername: summary.OwnerUsername,
		CreatedAt:     summary.CreatedAt,
		RoleCount:     summary.RoleCount,
		MemberCount:   summary.MemberCount,
		ChannelCount:  summary.ChannelCount,
	}
}
