package handlers

import (
	"errors"
	"net/http"
	"time"

	"papo/internal/middleware"
	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/utils"

	"github.com/labstack/echo/v4"
)

// serverListItem é a visão de servidor na listagem (GET /servers).
// Não inclui role_count, presente apenas no detalhe (openapi.yml).
type serverListItem struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	IconBlob      []byte    `json:"icon_blob"`
	IconFormat    string    `json:"icon_format"`
	OwnerID       *string   `json:"owner_id"`
	Public        bool      `json:"public"`
	OwnerUsername *string   `json:"owner_username"`
	CreatedAt     time.Time `json:"created_at"`
	ChannelCount  int       `json:"channel_count"`
	MemberCount   int       `json:"member_count"`
}

type serverListResponse struct {
	Servers []serverListItem `json:"servers"`
}

// serverDetail é a visão de detalhe do servidor (GET /servers/:server_id).
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

// ListServersHandler implementa GET /servers.
func ListServersHandler(baseURL string, c echo.Context) error {
	servers, err := services.ListServers(c.Request().Context())
	if err != nil {
		utils.Errorf("request_id=%s falha ao listar servidores: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao listar servidores")
	}

	items := make([]serverListItem, 0, len(servers))
	for _, server := range servers {
		items = append(items, toServerListItem(server))
	}

	return c.JSON(http.StatusOK, serverListResponse{Servers: items})
}

// GetServerHandler implementa GET /servers/:server_id.
func GetServerHandler(baseURL string, c echo.Context) error {
	serverID := c.Param("server_id")
	if serverID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "server_id ausente")
	}

	summary, err := services.GetServer(c.Request().Context(), serverID)
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

// serverCreated é a visão de servidor do POST /servers.
// Não inclui contagens, presente apenas na listagem e no detalhe (openapi.yml).
type serverCreated struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	IconBlob      []byte    `json:"icon_blob"`
	IconFormat    string    `json:"icon_format"`
	OwnerID       *string   `json:"owner_id"`
	OwnerUsername *string   `json:"owner_username"`
	CreatedAt     time.Time `json:"created_at"`
}

// CreateServerHandler implementa POST /servers.
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

	server, err := services.CreateServerWithIcon(c.Request().Context(), req.Name, req.IconBlob, req.IconFormat, req.Public, password, &userID)
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"name é obrigatório e deve ter no máximo 32 caracteres; icon_blob deve ser base64 de um GIF, JPEG ou PNG de até 2MB; servidor privado (public=false) exige password")
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

	summary, err := services.GetServer(c.Request().Context(), server.ID)
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

type updateServerRequest struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	IconBlob   string  `json:"icon_blob"`
	IconFormat string  `json:"icon_format"`
	Password   *string `json:"password"`
	Public     *bool   `json:"public"`
}

// UpdateServerHandler implementa PUT /servers/:server_id.
// Permissão: dono do servidor ou role `manage_server`
// (middleware RequireManageServer).
func UpdateServerHandler(baseURL string, c echo.Context) error {
	serverID := c.Param("server_id")
	if serverID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "server_id ausente")
	}

	var req updateServerRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	switch err := services.UpdateServer(c.Request().Context(), serverID, req.Name, req.IconBlob, req.IconFormat, req.Public, req.Password); {
	case errors.Is(err, services.ErrServerNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "servidor não encontrado")
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"name é obrigatório e deve ter no máximo 32 caracteres; icon_blob deve ser base64 de um GIF, JPEG ou PNG de até 2MB servidor privado (public=false) exige password")
	case err != nil:
		utils.Errorf("request_id=%s falha ao atualizar o servidor: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao atualizar o servidor")
	}

	updated, err := services.GetServer(c.Request().Context(), serverID)
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

func toServerListItem(summary models.ServerSummary) serverListItem {
	return serverListItem{
		ID:            summary.ID,
		Name:          summary.Name,
		IconBlob:      summary.IconBlob,
		IconFormat:    summary.IconFormat,
		OwnerID:       summary.OwnerID,
		Public:        summary.Public,
		OwnerUsername: summary.OwnerUsername,
		CreatedAt:     summary.CreatedAt,
		ChannelCount:  summary.ChannelCount,
		MemberCount:   summary.MemberCount,
	}
}
