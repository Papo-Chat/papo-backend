package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"

	"github.com/labstack/echo/v4"
)

var (
	errNotFoundChannel  = errors.New("canal não encontrado")
	errNotFoundRole     = errors.New("role não encontrada")
	errInvalidBody      = errors.New("corpo da requisição inválido")
	errBodyRead         = errors.New("falha ao ler o corpo da requisição")
	errServerUnresolved = errors.New("servidor não pôde ser resolvido")
)

// RequireRolePermission retorna um middleware que autoriza o acesso com base
// nas permissões de roles. O usuário autenticado deve ser o dono do servidor
// alvo (o dono possui implicitamente todas as permissões) ou possuir a
// permissão informada em ao menos uma das roles atribuídas a ele naquele
// servidor.
//
// O servidor alvo é resolvido, nesta ordem: parâmetro de rota server_id,
// parâmetro de rota channel_id (servidor do canal), parâmetro de rota role_id
// (servidor da role) ou corpo JSON server_id/role_id.
func RequireRolePermission(hasPermission func(models.RolePermissions) bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			cfg := config.LoadConfig()
			ctx := c.Request().Context()

			userID, ok := c.Get(UserIDContextKey).(string)
			if !ok || userID == "" {
				return utils.SendProblem(c, cfg.BaseURL, http.StatusUnauthorized,
					"unauthorized", "Token inválido ou expirado",
					"token de autenticação ausente, inválido ou expirado")
			}

			serverID, err := resolveServerID(ctx, c)
			if err != nil {
				switch {
				case errors.Is(err, storage.ErrNotFound) || errors.Is(err, errNotFoundChannel) || errors.Is(err, errNotFoundRole):
					return utils.SendProblem(c, cfg.BaseURL, http.StatusNotFound,
						"not-found", "Recurso não encontrado", notFoundDetail(err))
				case errors.Is(err, errInvalidBody) || errors.Is(err, errBodyRead) || errors.Is(err, errServerUnresolved):
					return utils.SendProblem(c, cfg.BaseURL, http.StatusBadRequest,
						"invalid-param", "Parâmetro inválido",
						"não foi possível determinar o servidor alvo da operação")
				default:
					utils.Errorf("request_id=%s falha ao resolver o servidor para verificação de permissão: %v",
						c.Request().Header.Get(echo.HeaderXRequestID), err)
					return utils.SendProblem(c, cfg.BaseURL, http.StatusInternalServerError,
						"internal", "Erro interno", "erro inesperado ao verificar permissões")
				}
			}

			server, err := storage.GetServerByID(ctx, serverID)
			if errors.Is(err, storage.ErrNotFound) {
				return utils.SendProblem(c, cfg.BaseURL, http.StatusNotFound,
					"not-found", "Recurso não encontrado", "servidor não encontrado")
			}
			if err != nil {
				utils.Errorf("request_id=%s falha ao recuperar o servidor para verificação de permissão: %v",
					c.Request().Header.Get(echo.HeaderXRequestID), err)
				return utils.SendProblem(c, cfg.BaseURL, http.StatusInternalServerError,
					"internal", "Erro interno", "erro inesperado ao verificar permissões")
			}

			// O dono do servidor possui implicitamente todas as permissões.
			if server.OwnerID != nil && *server.OwnerID == userID {
				return next(c)
			}

			roles, err := storage.GetRolesByUser(ctx, userID)
			if err != nil {
				utils.Errorf("request_id=%s falha ao recuperar as roles do usuário para verificação de permissão: %v",
					c.Request().Header.Get(echo.HeaderXRequestID), err)
				return utils.SendProblem(c, cfg.BaseURL, http.StatusInternalServerError,
					"internal", "Erro interno", "erro inesperado ao verificar permissões")
			}

			for _, role := range roles {
				if role.ServerID == serverID && hasPermission(role.Permissions) {
					return next(c)
				}
			}

			return utils.SendProblem(c, cfg.BaseURL, http.StatusForbidden,
				"forbidden", "Acesso negado",
				"usuário não possui a permissão necessária para esta operação")
		}
	}
}

// RequireManageServer autoriza operações que exigem a permissão manage_server.
func RequireManageServer() echo.MiddlewareFunc {
	return RequireRolePermission(func(p models.RolePermissions) bool { return p.ManageServer })
}

// RequireServerOwnerOrManageServer autoriza operações globais — sem servidor
// alvo na requisição — exigindo que o usuário autenticado seja dono de algum
// servidor (o dono possui implicitamente todas as permissões) ou possua a
// permissão manage_server em ao menos uma das roles atribuídas a ele.
func RequireServerOwnerOrManageServer() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			cfg := config.LoadConfig()
			ctx := c.Request().Context()

			userID, ok := c.Get(UserIDContextKey).(string)
			if !ok || userID == "" {
				return utils.SendProblem(c, cfg.BaseURL, http.StatusUnauthorized,
					"unauthorized", "Token inválido ou expirado",
					"token de autenticação ausente, inválido ou expirado")
			}

			ownsServer, err := storage.UserOwnsAnyServer(ctx, userID)
			if err != nil {
				utils.Errorf("request_id=%s falha ao verificar posse de servidor para verificação de permissão: %v",
					c.Request().Header.Get(echo.HeaderXRequestID), err)
				return utils.SendProblem(c, cfg.BaseURL, http.StatusInternalServerError,
					"internal", "Erro interno", "erro inesperado ao verificar permissões")
			}
			if ownsServer {
				return next(c)
			}

			roles, err := storage.GetRolesByUser(ctx, userID)
			if err != nil {
				utils.Errorf("request_id=%s falha ao recuperar as roles do usuário para verificação de permissão: %v",
					c.Request().Header.Get(echo.HeaderXRequestID), err)
				return utils.SendProblem(c, cfg.BaseURL, http.StatusInternalServerError,
					"internal", "Erro interno", "erro inesperado ao verificar permissões")
			}

			for _, role := range roles {
				if role.Permissions.ManageServer {
					return next(c)
				}
			}

			return utils.SendProblem(c, cfg.BaseURL, http.StatusForbidden,
				"forbidden", "Acesso negado",
				"usuário não possui a permissão necessária para esta operação")
		}
	}
}

// RequireSelfOrServerOwner autoriza operações sobre um usuário alvo em que o
// usuário autenticado pode agir sobre si mesmo ou, sendo dono de algum
// servidor, sobre qualquer usuário (README: POST /users/:user_id/reset).
func RequireSelfOrServerOwner() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			cfg := config.LoadConfig()
			ctx := c.Request().Context()

			userID, ok := c.Get(UserIDContextKey).(string)
			if !ok || userID == "" {
				return utils.SendProblem(c, cfg.BaseURL, http.StatusUnauthorized,
					"unauthorized", "Token inválido ou expirado",
					"token de autenticação ausente, inválido ou expirado")
			}

			targetID := c.Param("user_id")
			if targetID == "" {
				return utils.SendProblem(c, cfg.BaseURL, http.StatusBadRequest,
					"invalid-param", "Parâmetro inválido", "user_id ausente")
			}

			if targetID == userID {
				return next(c)
			}

			ownsServer, err := storage.UserOwnsAnyServer(ctx, userID)
			if err != nil {
				utils.Errorf("request_id=%s falha ao verificar posse de servidor para verificação de permissão: %v",
					c.Request().Header.Get(echo.HeaderXRequestID), err)
				return utils.SendProblem(c, cfg.BaseURL, http.StatusInternalServerError,
					"internal", "Erro interno", "erro inesperado ao verificar permissões")
			}
			if ownsServer {
				return next(c)
			}

			return utils.SendProblem(c, cfg.BaseURL, http.StatusForbidden,
				"forbidden", "Acesso negado",
				"usuário não possui a permissão necessária para esta operação")
		}
	}
}

// RequireManageChannels autoriza operações que exigem a permissão manage_channels.
func RequireManageChannels() echo.MiddlewareFunc {
	return RequireRolePermission(func(p models.RolePermissions) bool { return p.ManageChannels })
}

// RequireManageRoles autoriza operações que exigem a permissão manage_roles.
func RequireManageRoles() echo.MiddlewareFunc {
	return RequireRolePermission(func(p models.RolePermissions) bool { return p.ManageRoles })
}

// resolveServerID determina o servidor alvo da operação a partir da requisição.
func resolveServerID(ctx context.Context, c echo.Context) (string, error) {
	if id := c.Param("server_id"); id != "" {
		return id, nil
	}

	if id := c.Param("channel_id"); id != "" {
		channel, err := storage.GetChannelByID(ctx, id)
		if errors.Is(err, storage.ErrNotFound) {
			return "", errNotFoundChannel
		}
		if err != nil {
			return "", err
		}
		return channel.ServerID, nil
	}

	if id := c.Param("role_id"); id != "" {
		role, err := storage.GetRoleByID(ctx, id)
		if errors.Is(err, storage.ErrNotFound) {
			return "", errNotFoundRole
		}
		if err != nil {
			return "", err
		}
		return role.ServerID, nil
	}

	return serverIDFromBody(ctx, c)
}

// serverIDFromBody lê o corpo JSON da requisição para determinar o servidor
// alvo (server_id direto ou via role_id) e restaura o corpo para o handler.
func serverIDFromBody(ctx context.Context, c echo.Context) (string, error) {
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return "", errBodyRead
	}
	c.Request().Body = io.NopCloser(bytes.NewReader(body))

	var payload struct {
		ServerID *string `json:"server_id"`
		RoleID   *string `json:"role_id"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", errInvalidBody
	}

	if payload.ServerID != nil && *payload.ServerID != "" {
		return *payload.ServerID, nil
	}

	if payload.RoleID != nil && *payload.RoleID != "" {
		role, err := storage.GetRoleByID(ctx, *payload.RoleID)
		if errors.Is(err, storage.ErrNotFound) {
			return "", errNotFoundRole
		}
		if err != nil {
			return "", err
		}
		return role.ServerID, nil
	}

	return "", errServerUnresolved
}

// notFoundDetail retorna o detalhe específico para erros de recurso não encontrado.
func notFoundDetail(err error) string {
	switch {
	case errors.Is(err, errNotFoundChannel):
		return "canal não encontrado"
	case errors.Is(err, errNotFoundRole):
		return "role não encontrada"
	default:
		return "recurso não encontrado"
	}
}
