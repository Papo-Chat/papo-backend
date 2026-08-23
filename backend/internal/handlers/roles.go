package handlers

import (
	"errors"
	"net/http"

	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/utils"

	"github.com/labstack/echo/v4"
)

type roleListResponse struct {
	Roles []models.Role `json:"roles"`
}

// ListRolesHandler implementa GET /servers/:server_id/roles.
func ListRolesHandler(baseURL string, c echo.Context) error {
	serverID := c.Param("server_id")
	if serverID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "server_id ausente")
	}

	roles, err := services.ListRoles(c.Request().Context(), serverID)
	switch {
	case errors.Is(err, services.ErrServerNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "servidor não encontrado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao listar roles: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao listar as roles")
	}

	return c.JSON(http.StatusOK, roleListResponse{Roles: roles})
}

type createRoleRequest struct {
	Name        string                 `json:"name"`
	Color       *string                `json:"color"`
	Permissions models.RolePermissions `json:"permissions"`
}

// CreateRoleHandler implementa POST /servers/:server_id/roles.
// Permissão: dono do servidor ou role `manage_roles`
// (middleware RequireManageRoles).
func CreateRoleHandler(baseURL string, c echo.Context) error {
	serverID := c.Param("server_id")
	if serverID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "server_id ausente")
	}

	var req createRoleRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	role, err := services.CreateRole(c.Request().Context(), serverID, req.Name, req.Color, req.Permissions)
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"name é obrigatório e deve ter no máximo 32 caracteres; color deve ser hexadecimal #RRGGBB")
	case errors.Is(err, services.ErrServerNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "servidor não encontrado")
	case errors.Is(err, services.ErrRoleNameTaken):
		return utils.SendProblem(c, baseURL, http.StatusConflict,
			"role-name-taken", "Nome de role já existe",
			"o nome informado já está em uso no servidor")
	case err != nil:
		utils.Errorf("request_id=%s falha ao criar role: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao criar a role")
	}

	return c.JSON(http.StatusCreated, role)
}

type updateRoleRequest struct {
	Name        string                 `json:"name"`
	Color       *string                `json:"color"`
	Permissions models.RolePermissions `json:"permissions"`
}

// UpdateRoleHandler implementa PUT /roles/:role_id.
// Permissão: dono do servidor ou role `manage_roles`
// (middleware RequireManageRoles).
func UpdateRoleHandler(baseURL string, c echo.Context) error {
	roleID := c.Param("role_id")
	if roleID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "role_id ausente")
	}

	var req updateRoleRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	role, err := services.UpdateRole(c.Request().Context(), roleID, req.Name, req.Color, req.Permissions)
	switch {
	case errors.Is(err, services.ErrRoleNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "role não encontrada")
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"name é obrigatório e deve ter no máximo 32 caracteres; color deve ser hexadecimal #RRGGBB")
	case errors.Is(err, services.ErrRoleNameTaken):
		return utils.SendProblem(c, baseURL, http.StatusConflict,
			"role-name-taken", "Nome de role já existe",
			"o nome informado já está em uso no servidor")
	case err != nil:
		utils.Errorf("request_id=%s falha ao atualizar role: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao atualizar a role")
	}

	return c.JSON(http.StatusOK, role)
}

// DeleteRoleHandler implementa DELETE /roles/:role_id.
// Permissão: dono do servidor ou role `manage_roles`
// (middleware RequireManageRoles).
func DeleteRoleHandler(baseURL string, c echo.Context) error {
	roleID := c.Param("role_id")
	if roleID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "role_id ausente")
	}

	err := services.DeleteRole(c.Request().Context(), roleID)
	switch {
	case errors.Is(err, services.ErrRoleNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "role não encontrada")
	case err != nil:
		utils.Errorf("request_id=%s falha ao excluir role: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao excluir a role")
	}

	return c.NoContent(http.StatusNoContent)
}

type assignUserRoleRequest struct {
	RoleID string `json:"role_id"`
}

// AssignUserRoleHandler implementa POST /users/:user_id/roles.
// Permissão: dono do servidor ou role `manage_roles`
// (middleware RequireManageRoles).
func AssignUserRoleHandler(baseURL string, c echo.Context) error {
	userID := c.Param("user_id")
	if userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "user_id ausente")
	}

	var req assignUserRoleRequest
	if err := c.Bind(&req); err != nil || req.RoleID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "role_id é obrigatório")
	}

	userRole, err := services.AssignUserRole(c.Request().Context(), userID, req.RoleID)
	switch {
	case errors.Is(err, services.ErrUserNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "usuário não encontrado")
	case errors.Is(err, services.ErrRoleNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "role não encontrada")
	case err != nil:
		utils.Errorf("request_id=%s falha ao atribuir role: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao atribuir a role")
	}

	return c.JSON(http.StatusCreated, userRole)
}

// RemoveUserRoleHandler implementa DELETE /users/:user_id/roles/:role_id.
// Permissão: dono do servidor ou role `manage_roles`
// (middleware RequireManageRoles).
func RemoveUserRoleHandler(baseURL string, c echo.Context) error {
	userID := c.Param("user_id")
	if userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "user_id ausente")
	}
	roleID := c.Param("role_id")
	if roleID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "role_id ausente")
	}

	err := services.RemoveUserRole(c.Request().Context(), userID, roleID)
	switch {
	case errors.Is(err, services.ErrUserNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "usuário não encontrado")
	case errors.Is(err, services.ErrRoleNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "role não encontrada")
	case errors.Is(err, services.ErrUserRoleNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "role não atribuída ao usuário")
	case err != nil:
		utils.Errorf("request_id=%s falha ao remover role do usuário: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao remover a role do usuário")
	}

	return c.NoContent(http.StatusNoContent)
}
