package handlers

import (
	"errors"
	"net/http"
	"time"

	"papo/internal/middleware"
	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/utils"
	"papo/internal/websocket"

	"github.com/labstack/echo/v4"
)

type profileResponse struct {
	ID              string               `json:"id"`
	Username        string               `json:"username"`
	Nickname        *string              `json:"nickname"`
	AvatarBlob      []byte               `json:"avatar_blob"`
	AvatarFormat    string               `json:"avatar_format"`
	BannerMedia     *string              `json:"banner_media"`
	Description     *string              `json:"description"`
	Status          *string              `json:"status"`
	StatusMessage   *string              `json:"status_message"`
	StatusUpdatedAt *time.Time           `json:"status_updated_at"`
	CreatedAt       time.Time            `json:"created_at"`
	Roles           []models.RoleSummary `json:"roles"`
}

// ProfileHandler implementa GET /users/:user_id/profile.
// Qualquer usuário autenticado pode consultar o perfil de qualquer usuário.
func ProfileHandler(baseURL string, c echo.Context) error {
	if _, ok := c.Get(middleware.UserIDContextKey).(string); !ok {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	targetID := c.Param("user_id")
	if targetID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "user_id ausente")
	}

	user, err := services.Profile(c.Request().Context(), targetID)
	switch {
	case errors.Is(err, services.ErrUserNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "usuário não encontrado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao recuperar o perfil do usuário: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao recuperar o perfil do usuário")
	}

	return c.JSON(http.StatusOK, profileResponse{
		ID:              user.ID,
		Username:        user.Username,
		Nickname:        user.Nickname,
		AvatarBlob:      user.AvatarBlob,
		AvatarFormat:    user.AvatarFormat,
		BannerMedia:     user.BannerMedia,
		Description:     user.Description,
		Status:          user.Status,
		StatusMessage:   user.StatusMessage,
		StatusUpdatedAt: user.StatusUpdatedAt,
		CreatedAt:       user.CreatedAt,
		Roles:           user.Roles,
	})
}

type profileBatchRequest struct {
	IDs []string `json:"ids"`
}

type profileBatchResponse struct {
	Profiles []profileResponse `json:"profiles"`
}

// ProfileBatchHandler implementa POST /users/profile_batch.
// Retorna os perfis dos usuários solicitados (mesma forma do profile
// individual), na ordem da requisição, pulando ids que não existem.
// Máximo de 50 ids por requisição.
func ProfileBatchHandler(baseURL string, c echo.Context) error {
	if _, ok := c.Get(middleware.UserIDContextKey).(string); !ok {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	var req profileBatchRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}
	if len(req.IDs) == 0 {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo 'ids' é obrigatório")
	}
	for _, id := range req.IDs {
		if id == "" {
			return utils.SendProblem(c, baseURL, http.StatusBadRequest,
				"invalid-param", "Parâmetro inválido", "ids não podem ser vazios")
		}
	}

	users, err := services.ProfilesBatch(c.Request().Context(), req.IDs)
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "máximo de 50 ids por requisição")
	case err != nil:
		utils.Errorf("request_id=%s falha ao recuperar os perfis dos usuários: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao recuperar os perfis dos usuários")
	}

	profiles := make([]profileResponse, 0, len(users))
	for _, user := range users {
		profiles = append(profiles, profileResponse{
			ID:              user.ID,
			Username:        user.Username,
			Nickname:        user.Nickname,
			AvatarBlob:      user.AvatarBlob,
			AvatarFormat:    user.AvatarFormat,
			BannerMedia:     user.BannerMedia,
			Description:     user.Description,
			Status:          user.Status,
			StatusMessage:   user.StatusMessage,
			StatusUpdatedAt: user.StatusUpdatedAt,
			CreatedAt:       user.CreatedAt,
			Roles:           user.Roles,
		})
	}

	return c.JSON(http.StatusOK, profileBatchResponse{Profiles: profiles})
}

// ListUsersHandler implementa GET /users.
// Os parâmetros de query since (timestamp ISO 8601 para polling de novos
// usuários) e last_id (id do último usuário da página anterior, usado com
// since como cursor exato) são opcionais; máx. 100 usuários por resposta.
func ListUsersHandler(baseURL string, c echo.Context) error {
	if _, ok := c.Get(middleware.UserIDContextKey).(string); !ok {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	var since *time.Time
	if value := c.QueryParam("since"); value != "" {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return utils.SendProblem(c, baseURL, http.StatusBadRequest,
				"invalid-param", "Parâmetro inválido",
				"since deve ser um timestamp ISO 8601")
		}
		since = &parsed
	}
	lastID := c.QueryParam("last_id")

	list, err := services.ListUsers(c.Request().Context(), since, lastID)
	if err != nil {
		utils.Errorf("request_id=%s falha ao listar usuários: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao listar usuários")
	}

	return c.JSON(http.StatusOK, list)
}

type updateSettingsRequest struct {
	Config models.UserConfig `json:"config"`
}

// UpdateSettingsHandler implementa PUT /users/settings.
func UpdateSettingsHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	var req updateSettingsRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	settings, err := services.UpdateSettings(c.Request().Context(), userID, req.Config)
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo ausente ou inválido")
	case errors.Is(err, services.ErrUserNotFound):
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao atualizar as configurações do usuário: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao atualizar as configurações do usuário")
	}

	return c.JSON(http.StatusOK, settings)
}

type updateUserRequest struct {
	Nickname    *string `json:"nickname"`
	Status      *string `json:"status"`
	Description *string `json:"description"`
}

// UpdateUserHandler implementa PUT /users/:user_id.
func UpdateUserHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	targetID := c.Param("user_id")
	if targetID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "user_id ausente")
	}
	if targetID != userID {
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado", "não é possível atualizar o perfil de outro usuário")
	}

	var req updateUserRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}
	if req.Nickname == nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo 'nickname' é obrigatório")
	}
	if req.Status == nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo 'status' é obrigatório")
	}
	if req.Description == nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo 'description' é obrigatório")
	}

	switch err := services.UpdateUser(c.Request().Context(), userID, *req.Nickname, *req.Status, *req.Description); {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"nickname deve ter no máximo 32 caracteres, status no máximo 64 caracteres e description no máximo 512 caracteres")
	case errors.Is(err, services.ErrUserNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "usuário não encontrado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao atualizar o perfil do usuário: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao atualizar o perfil do usuário")
	}

	// Notifica os clientes conectados do novo nickname e status
	// (presence_update); se o usuário estiver offline, o estado efêmero é
	// atualizado apenas na próxima conexão.
	websocket.GetHub().UpdateStatusMessage(userID, req.Status, req.Nickname)

	return c.JSON(http.StatusOK, map[string]string{
		"response": "User status updated successfully",
	})
}

type updateStatusRequest struct {
	Status *string `json:"status"`
}

// UpdateStatusHandler implementa PUT /users/:user_id/status.
// Persiste o status do usuário (away/busy; null remove). O status vale
// enquanto o usuário estiver online (presence_update com o status efetivo);
// offline, o status persistido vale na próxima conexão.
func UpdateStatusHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	targetID := c.Param("user_id")
	if targetID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "user_id ausente")
	}
	if targetID != userID {
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado", "não é possível atualizar o status de outro usuário")
	}

	var req updateStatusRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	switch err := services.UpdateStatus(c.Request().Context(), userID, req.Status); {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"status deve ser away, busy ou null")
	case errors.Is(err, services.ErrUserNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "usuário não encontrado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao atualizar o status do usuário: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao atualizar o status do usuário")
	}

	// Notifica os clientes conectados do novo status (presence_update); se o
	// usuário estiver offline, o valor persistido vale na próxima conexão.
	websocket.GetHub().UpdatePersistedStatus(userID, req.Status)

	return c.JSON(http.StatusOK, map[string]string{
		"response": "User status updated successfully",
	})
}

type updateAvatarRequest struct {
	Avatar       string `json:"avatar"`
	AvatarFormat string `json:"avatar_format"`
}

// UpdateAvatarHandler implementa PUT /users/:user_id/avatar.
func UpdateAvatarHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	targetID := c.Param("user_id")
	if targetID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "user_id ausente")
	}
	if targetID != userID {
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado", "não é possível atualizar o avatar de outro usuário")
	}

	var req updateAvatarRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	switch err := services.UpdateAvatar(c.Request().Context(), userID, req.Avatar, req.AvatarFormat); {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"avatar inválido: deve ser base64 de um GIF, JPEG/JPG, PNG ou WEBP de até 2MB")
	case errors.Is(err, services.ErrUserNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "usuário não encontrado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao atualizar o avatar do usuário: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao atualizar o avatar do usuário")
	}

	// Notifica os clientes conectados da atualização do avatar (avatar_update).
	websocket.GetHub().Broadcast(websocket.AvatarUpdateOutbound{
		Type:   websocket.EventTypeAvatarUpdate,
		UserID: userID,
	})

	return c.JSON(http.StatusOK, map[string]string{
		"response": "User avatar updated successfully",
	})
}

type updateBannerRequest struct {
	Banner       string `json:"banner"`
	BannerFormat string `json:"banner_format"`
}

// UpdateBannerHandler implementa PUT /users/:user_id/banner.
func UpdateBannerHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	targetID := c.Param("user_id")
	if targetID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "user_id ausente")
	}
	if targetID != userID {
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado", "não é possível atualizar o banner de outro usuário")
	}

	var req updateBannerRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	switch err := services.UpdateBanner(c.Request().Context(), userID, req.Banner, req.BannerFormat); {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"banner inválido: deve ser base64 de um GIF, JPEG/JPG, PNG ou WEBP de até 2MB")
	case errors.Is(err, services.ErrUserNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "usuário não encontrado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao atualizar o banner do usuário: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao atualizar o banner do usuário")
	}

	return c.JSON(http.StatusOK, map[string]string{
		"response": "User banner updated successfully",
	})
}

type banUserRequest struct {
	UserID   string `json:"user_id"`
	BanState *bool  `json:"ban_state"`
}

// BanUserHandler implementa PUT /users/:user_id/ban.
// Permissão: dono de algum servidor ou role `manage_server`
// (middleware RequireServerOwnerOrManageServer). O id da URL é o autoritativo.
func BanUserHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	targetID := c.Param("user_id")
	if targetID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "user_id ausente")
	}

	var req banUserRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}
	if req.BanState == nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo 'ban_state' é obrigatório")
	}

	switch err := services.BanUser(c.Request().Context(), userID, targetID, *req.BanState); {
	case errors.Is(err, services.ErrUserNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "usuário não encontrado")
	case errors.Is(err, services.ErrServerOwner):
		return utils.SendProblem(c, baseURL, http.StatusConflict,
			"conflict", "Ação proibida", "usuário dono do servidor")
	case err != nil:
		utils.Errorf("request_id=%s falha ao alterar o estado de banimento do usuário: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao alterar o estado de banimento do usuário")
	}

	return c.JSON(http.StatusOK, map[string]string{
		"response": "User state changed successfully",
	})
}

// ResetUserHandler implementa POST /users/:user_id/reset.
// Permissão: usuário agindo sobre si mesmo ou dono de um servidor
// (middleware RequireSelfOrServerOwner). O id da URL é o autoritativo.
func ResetUserHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	targetID := c.Param("user_id")
	if targetID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "user_id ausente")
	}

	switch err := services.ResetUserPassword(c.Request().Context(), userID, targetID); {
	case errors.Is(err, services.ErrUserNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "usuário não encontrado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao marcar o usuário para reset de senha: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao marcar o usuário para reset de senha")
	}

	return c.JSON(http.StatusOK, map[string]string{
		"response": "User password is set to reset",
	})
}

type changePasswordRequest struct {
	Password string `json:"password"`
}

// ChangePasswordHandler implementa PUT /users/:user_id/password.
// Somente o próprio usuário pode alterar a senha; a flag de reset de senha
// é reiniciada junto com a troca (README).
func ChangePasswordHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	targetID := c.Param("user_id")
	if targetID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "user_id ausente")
	}
	if targetID != userID {
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado", "não é possível alterar a senha de outro usuário")
	}

	var req changePasswordRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	switch err := services.ChangePassword(c.Request().Context(), userID, req.Password); {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo 'password' é obrigatório")
	case errors.Is(err, services.ErrUserNotReset):
		return utils.SendProblem(c, baseURL, http.StatusConflict,
			"no-reset-password", "Ação proibida", "reset_password ausente")
	case errors.Is(err, services.ErrUserNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "usuário não encontrado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao alterar a senha do usuário: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao alterar a senha do usuário")
	}

	return c.JSON(http.StatusOK, map[string]string{
		"response": "User password updated successfully",
	})
}
