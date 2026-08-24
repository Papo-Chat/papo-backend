package handlers

import (
	"papo/internal/config"
	"papo/internal/middleware"

	"github.com/labstack/echo/v4"
)

// RegisterAuthRoutes registra as rotas de autenticação.
// Todas as rotas usam o mesmo rate limiter (token bucket por IP) para
// proteção contra abuso.
func RegisterAuthRoutes(e *echo.Echo, cfg *config.Config) {
	authRateLimit := middleware.RateLimit(cfg.AuthRateLimit, cfg.AuthRateBurst)

	e.POST("/auth/register", func(c echo.Context) error {
		return RegisterHandler(cfg.BaseURL, c)
	}, authRateLimit)
	e.POST("/auth/login", func(c echo.Context) error {
		return LoginHandler(cfg.BaseURL, c)
	}, authRateLimit)
	e.POST("/auth/loginServer", func(c echo.Context) error {
		return LoginServerHandler(cfg.BaseURL, c)
	}, authRateLimit)
	e.GET("/auth/whoami", func(c echo.Context) error {
		return WhoamiHandler(cfg.BaseURL, c)
	}, authRateLimit, middleware.JWTMiddleware)
	e.POST("/auth/logout", LogoutHandler, authRateLimit)
}

// RegisterUserRoutes registra as rotas de usuários.
func RegisterUserRoutes(e *echo.Echo, cfg *config.Config) {
	e.GET("/users", func(c echo.Context) error {
		return ListUsersHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
	e.GET("/users/:user_id/profile", func(c echo.Context) error {
		return ProfileHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
	e.PUT("/users/settings", func(c echo.Context) error {
		return UpdateSettingsHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
	e.PUT("/users/:user_id", func(c echo.Context) error {
		return UpdateUserHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
	e.PUT("/users/:user_id/avatar", func(c echo.Context) error {
		return UpdateAvatarHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
	e.PUT("/users/:user_id/password", func(c echo.Context) error {
		return ChangePasswordHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
	e.PUT("/users/:user_id/ban", func(c echo.Context) error {
		return BanUserHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware, middleware.RequireServerOwnerOrManageServer())
	e.POST("/users/:user_id/reset", func(c echo.Context) error {
		return ResetUserHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware, middleware.RequireSelfOrServerOwner())
}

// RegisterServerRoutes registra as rotas de servidores.
func RegisterServerRoutes(e *echo.Echo, cfg *config.Config) {
	e.GET("/servers", func(c echo.Context) error {
		return ListServersHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
	e.POST("/servers", func(c echo.Context) error {
		return CreateServerHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
	e.GET("/servers/:server_id", func(c echo.Context) error {
		return GetServerHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
	e.PUT("/servers/:server_id", func(c echo.Context) error {
		return UpdateServerHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware, middleware.RequireServerOwnerOrManageServer())
}

// RegisterChannelRoutes registra as rotas de canais.
func RegisterChannelRoutes(e *echo.Echo, cfg *config.Config) {
	e.GET("/channels", func(c echo.Context) error {
		return ListChannelsHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
	e.POST("/channels", func(c echo.Context) error {
		return CreateChannelHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware, middleware.RequireManageChannels())
	e.PUT("/channels/:channel_id", func(c echo.Context) error {
		return UpdateChannelHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware, middleware.RequireManageChannels())
	e.PUT("/channels/:channel_id/change_position", func(c echo.Context) error {
		return ChangeChannelPositionHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware, middleware.RequireManageChannels())
	e.DELETE("/channels/:channel_id", func(c echo.Context) error {
		return DeleteChannelHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware, middleware.RequireManageChannels())
	e.GET("/channels/:channel_id/permissions", func(c echo.Context) error {
		return GetChannelPermissionsHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
	e.PUT("/channels/:channel_id/permissions/:role_id", func(c echo.Context) error {
		return UpdateChannelPermissionsHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware, middleware.RequireManageChannels())
}

// RegisterMessageRoutes registra as rotas de mensagens.
func RegisterMessageRoutes(e *echo.Echo, cfg *config.Config) {
	e.GET("/channels/:channel_id/messages", func(c echo.Context) error {
		return ListMessagesHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
	e.POST("/messages", func(c echo.Context) error {
		return CreateMessageHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
	e.PUT("/messages/:message_id", func(c echo.Context) error {
		return UpdateMessageHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
	e.DELETE("/messages/:message_id", func(c echo.Context) error {
		return DeleteMessageHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
}

// RegisterAttachmentRoutes registra as rotas de attachments.
func RegisterAttachmentRoutes(e *echo.Echo, cfg *config.Config) {
	e.GET("/attachments/:file_id", func(c echo.Context) error {
		return DownloadAttachmentHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
}

// RegisterEmojiRoutes registra as rotas de emojis.
func RegisterEmojiRoutes(e *echo.Echo, cfg *config.Config) {
	e.GET("/emojis", func(c echo.Context) error {
		return ListEmojisHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
	e.POST("/emojis", func(c echo.Context) error {
		return CreateEmojiHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware, middleware.RequireManageServer())
	e.DELETE("/emojis/:emoji_id", func(c echo.Context) error {
		return DeleteEmojiHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
}

// RegisterRoleRoutes registra as rotas de roles e de atribuição de roles a usuários.
func RegisterRoleRoutes(e *echo.Echo, cfg *config.Config) {
	e.GET("/servers/:server_id/roles", func(c echo.Context) error {
		return ListRolesHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
	e.POST("/servers/:server_id/roles", func(c echo.Context) error {
		return CreateRoleHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware, middleware.RequireManageRoles())
	e.PUT("/roles/:role_id", func(c echo.Context) error {
		return UpdateRoleHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware, middleware.RequireManageRoles())
	e.DELETE("/roles/:role_id", func(c echo.Context) error {
		return DeleteRoleHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware, middleware.RequireManageRoles())
	e.POST("/users/:user_id/roles", func(c echo.Context) error {
		return AssignUserRoleHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware, middleware.RequireManageRoles())
	e.DELETE("/users/:user_id/roles/:role_id", func(c echo.Context) error {
		return RemoveUserRoleHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware, middleware.RequireManageRoles())
}

// RegisterSearchRoutes registra as rotas de busca.
func RegisterSearchRoutes(e *echo.Echo, cfg *config.Config) {
	e.POST("/search", func(c echo.Context) error {
		return SearchHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
}

// RegisterWebSocketRoutes registra a rota do WebSocket.
// Usa o mesmo cookie Auth da API REST: o JWT é validado pelo
// JWTMiddleware durante o handshake, antes do upgrade da conexão.
func RegisterWebSocketRoutes(e *echo.Echo, cfg *config.Config) {
	e.GET("/ws", func(c echo.Context) error {
		return WebSocketHandler(cfg.BaseURL, c)
	}, middleware.JWTMiddleware)
}
