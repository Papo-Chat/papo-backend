package middleware

import (
	"errors"
	"net/http"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"

	"github.com/labstack/echo/v4"
)

// RequireRolePermission retorna um middleware que autoriza o acesso com base
// nas permissões de roles. O usuário autenticado deve ser o dono do servidor
// (o dono possui implicitamente todas as permissões) ou possuir a permissão
// informada em ao menos uma das roles atribuídas a ele.
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

			server, err := storage.GetServer(ctx)
			if err != nil && !errors.Is(err, storage.ErrNotFound) {
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
				if hasPermission(role.Permissions) {
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
// alvo na requisição — exigindo que o usuário autenticado seja dono do
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
// usuário autenticado pode agir sobre si mesmo ou, sendo dono do servidor,
// sobre qualquer usuário (README: POST /users/:user_id/reset).
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
