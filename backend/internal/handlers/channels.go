package handlers

import (
	"errors"
	"net/http"

	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/utils"
	"papo/internal/websocket"

	"github.com/labstack/echo/v4"
)

type channelListResponse struct {
	Channels []models.ChannelSummary `json:"channels"`
}

// ListChannelsHandler implementa GET /channels.
// O parâmetro de query server_id é opcional (filtro por servidor).
func ListChannelsHandler(baseURL string, c echo.Context) error {
	var serverID *string
	if value := c.QueryParam("server_id"); value != "" {
		serverID = &value
	}

	channels, err := services.ListChannels(c.Request().Context(), serverID)
	if err != nil {
		utils.Errorf("request_id=%s falha ao listar canais: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao listar canais")
	}

	return c.JSON(http.StatusOK, channelListResponse{Channels: channels})
}

type createChannelRequest struct {
	ServerID string `json:"server_id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
}

// CreateChannelHandler implementa POST /channels.
func CreateChannelHandler(baseURL string, c echo.Context) error {
	var req createChannelRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	channel, err := services.CreateChannel(c.Request().Context(), req.ServerID, req.Name, req.Type)
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"server_id e name são obrigatórios; name deve ter no máximo 32 caracteres; type deve ser 'text' ou 'category'")
	case errors.Is(err, services.ErrServerNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "servidor não encontrado")
	case errors.Is(err, services.ErrChannelLimitReached):
		return utils.SendProblem(c, baseURL, http.StatusConflict,
			"channel-limit-reached", "Limite de canais atingido",
			"o servidor já possui o número máximo de canais (500)")
	case errors.Is(err, services.ErrChannelNameTaken):
		return utils.SendProblem(c, baseURL, http.StatusConflict,
			"channel-name-taken", "Nome de canal já existe",
			"o nome informado já está em uso")
	case err != nil:
		utils.Errorf("request_id=%s falha ao criar canal: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao criar o canal")
	}

	// Distribui a criação aos clientes conectados (evento channel_create).
	websocket.GetHub().Broadcast(websocket.ChannelCreateOutbound{
		Type:        websocket.EventTypeChannelCreate,
		ChannelID:   channel.ID,
		ServerID:    channel.ServerID,
		Name:        channel.Name,
		Position:    channel.Position,
		ChannelType: channel.Type,
	})

	return c.JSON(http.StatusCreated, channel)
}

type updateChannelRequest struct {
	Name string `json:"name"`
}

// UpdateChannelHandler implementa PUT /channels/:channel_id.
// Permissão: dono do servidor ou role `manage_channels`
// (middleware RequireManageChannels).
func UpdateChannelHandler(baseURL string, c echo.Context) error {
	channelID := c.Param("channel_id")
	if channelID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "channel_id ausente")
	}

	var req updateChannelRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	channel, err := services.UpdateChannel(c.Request().Context(), channelID, req.Name)
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"name é obrigatório e deve ter no máximo 32 caracteres")
	case errors.Is(err, services.ErrChannelNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "canal não encontrado")
	case errors.Is(err, services.ErrChannelNameTaken):
		return utils.SendProblem(c, baseURL, http.StatusConflict,
			"channel-name-taken", "Nome de canal já existe",
			"o nome informado já está em uso")
	case err != nil:
		utils.Errorf("request_id=%s falha ao atualizar canal: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao atualizar o canal")
	}

	// Distribui a edição aos clientes conectados (evento channel_update).
	websocket.GetHub().Broadcast(websocket.ChannelUpdateOutbound{
		Type:      websocket.EventTypeChannelUpdate,
		ChannelID: channel.ID,
		ServerID:  channel.ServerID,
		Name:      channel.Name,
		Position:  channel.Position,
	})

	return c.JSON(http.StatusOK, channel)
}

// DeleteChannelHandler implementa DELETE /channels/:channel_id.
func DeleteChannelHandler(baseURL string, c echo.Context) error {
	channelID := c.Param("channel_id")
	if channelID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "channel_id ausente")
	}

	serverID, err := services.DeleteChannel(c.Request().Context(), channelID)
	switch {
	case errors.Is(err, services.ErrChannelNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "canal não encontrado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao excluir canal: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao excluir o canal")
	}

	// Distribui a exclusão aos clientes conectados (evento channel_delete).
	websocket.GetHub().Broadcast(websocket.ChannelDeleteOutbound{
		Type:      websocket.EventTypeChannelDelete,
		ChannelID: channelID,
		ServerID:  serverID,
	})

	return c.NoContent(http.StatusNoContent)
}

type channelPermissionsResponse struct {
	ChannelID   string                          `json:"channel_id"`
	Permissions []models.ChannelPermissionEntry `json:"permissions"`
}

// GetChannelPermissionsHandler implementa
// GET /channels/:channel_id/permissions.
func GetChannelPermissionsHandler(baseURL string, c echo.Context) error {
	channelID := c.Param("channel_id")
	if channelID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "channel_id ausente")
	}

	permissions, err := services.GetChannelPermissions(c.Request().Context(), channelID)
	switch {
	case errors.Is(err, services.ErrChannelNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "canal não encontrado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao recuperar permissões do canal: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao recuperar as permissões do canal")
	}

	return c.JSON(http.StatusOK, channelPermissionsResponse{
		ChannelID:   channelID,
		Permissions: permissions,
	})
}

type updateChannelPermissionsRequest struct {
	Permissions models.ChannelPermission `json:"permissions"`
}

type updateChannelPermissionsResponse struct {
	ChannelID   string                   `json:"channel_id"`
	RoleID      string                   `json:"role_id"`
	Permissions models.ChannelPermission `json:"permissions"`
}

// UpdateChannelPermissionsHandler implementa
// PUT /channels/:channel_id/permissions/:role_id.
func UpdateChannelPermissionsHandler(baseURL string, c echo.Context) error {
	channelID := c.Param("channel_id")
	if channelID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "channel_id ausente")
	}
	roleID := c.Param("role_id")
	if roleID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "role_id ausente")
	}

	var req updateChannelPermissionsRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	permissions, err := services.UpdateChannelPermissions(c.Request().Context(), channelID, roleID, req.Permissions)
	switch {
	case errors.Is(err, services.ErrChannelNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "canal não encontrado")
	case errors.Is(err, services.ErrRoleNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "role não encontrada")
	case err != nil:
		utils.Errorf("request_id=%s falha ao atualizar permissões do canal: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao atualizar as permissões do canal")
	}

	return c.JSON(http.StatusOK, updateChannelPermissionsResponse{
		ChannelID:   channelID,
		RoleID:      roleID,
		Permissions: permissions,
	})
}

type changeChannelPositionRequest struct {
	OldPosition int `json:"old_position"`
	NewPosition int `json:"new_position"`
}

// ChangeChannelPositionHandler implementa
// PUT /channels/:channel_id/change_position.
// Permissão: dono do servidor ou role `manage_channels`
// (middleware RequireManageChannels).
func ChangeChannelPositionHandler(baseURL string, c echo.Context) error {
	channelID := c.Param("channel_id")
	if channelID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "channel_id ausente")
	}

	var req changeChannelPositionRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	channel, err := services.ChangeChannelPosition(c.Request().Context(), channelID, req.OldPosition, req.NewPosition)
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"old_position e new_position devem ser posições válidas (1 até o número de canais do servidor)")
	case errors.Is(err, services.ErrChannelNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "canal não encontrado")
	case errors.Is(err, services.ErrChannelPositionConflict):
		return utils.SendProblem(c, baseURL, http.StatusConflict,
			"channel-position-conflict", "Posição do canal desatualizada",
			"a posição atual do canal não corresponde à old_position informada")
	case err != nil:
		utils.Errorf("request_id=%s falha ao mudar a posição do canal: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao mudar a posição do canal")
	}

	// Distribui a mudança de posição aos clientes conectados
	// (evento channel_update).
	websocket.GetHub().Broadcast(websocket.ChannelUpdateOutbound{
		Type:      websocket.EventTypeChannelUpdate,
		ChannelID: channel.ID,
		ServerID:  channel.ServerID,
		Name:      channel.Name,
		Position:  channel.Position,
	})

	return c.JSON(http.StatusOK, channel)
}
