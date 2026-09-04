package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"papo/internal/config"
	"papo/internal/middleware"
	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/utils"
	"papo/internal/websocket"

	"github.com/labstack/echo/v4"
)

type registerRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type registerResponse struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
}

// RegisterHandler implementa POST /auth/register.
func RegisterHandler(baseURL string, c echo.Context) error {
	//Lê a configuração do tamanho máximo dos campos
	cfg := config.LoadConfig()

	MaxPasswordLength := cfg.MaxPasswordLength
	MaxUsernameLength := cfg.MaxUsernameLength

	var req registerRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	if req.Username == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo 'username' é obrigatório")
	}
	if utf8.RuneCountInString(req.Username) > MaxUsernameLength {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			fmt.Sprintf("campo 'username' deve ter no máximo %d caracteres", MaxUsernameLength))
	}
	if req.Password == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo 'password' é obrigatório")
	}
	if utf8.RuneCountInString(req.Password) > MaxPasswordLength {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			fmt.Sprintf("campo 'password' deve ter no máximo %d caracteres", MaxPasswordLength))
	}

	if !checkServerAccess(baseURL, c) {
		// a Função já enviou a resposta, o writeheader do sendProblem no echo já responde a requisição
		// o usuário não está autenticado no servidor!
		return nil
	}

	user, err := services.Register(c.Request().Context(), req.Username, req.Password, c.RealIP())
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo ausente ou inválido")
	case errors.Is(err, services.ErrUsernameTaken):
		return utils.SendProblem(c, baseURL, http.StatusConflict,
			"username-taken", "Username já existe", "o username informado já está em uso")
	case errors.Is(err, services.ErrBannedIP):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"banned", "Usuário banido", "o IP informado já foi usado por um usuário banido")
	case err != nil:
		utils.Errorf("request_id=%s falha no registro de usuário: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao registrar usuário")
	}

	// Notifica os clientes conectados do novo usuário (user_join).
	websocket.GetHub().Broadcast(websocket.UserJoinOutbound{
		Type:   websocket.EventTypeUserJoin,
		UserID: user.ID,
	})

	return c.JSON(http.StatusCreated, registerResponse{
		ID:        user.ID,
		Username:  user.Username,
		CreatedAt: user.CreatedAt,
	})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	// ConnectionViolation indica que o reuso de um token de sessão foi
	// detectado neste usuário (todas as conexões foram revogadas); o cliente
	// usa a flag para avisar o usuário.
	ConnectionViolation bool `json:"connection_violation"`
}

// LoginHandler implementa POST /auth/login.
func LoginHandler(baseURL string, c echo.Context) error {
	//Lê a configuração do tamanho máximo dos campos
	cfg := config.LoadConfig()

	MaxPasswordLength := cfg.MaxPasswordLength
	MaxUsernameLength := cfg.MaxUsernameLength

	var req loginRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	if req.Username == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo 'username' é obrigatório")
	}
	if utf8.RuneCountInString(req.Username) > MaxUsernameLength {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			fmt.Sprintf("campo 'username' deve ter no máximo %d caracteres", MaxUsernameLength))
	}
	if req.Password == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo 'password' é obrigatório")
	}
	if utf8.RuneCountInString(req.Password) > MaxPasswordLength {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			fmt.Sprintf("campo 'password' deve ter no máximo %d caracteres", MaxPasswordLength))
	}

	if !checkServerAccess(baseURL, c) {
		// a Função já enviou a resposta, o writeheader do sendProblem no echo já responde a requisição
		// o usuário não está autenticado no servidor!
		return nil
	}

	user, err := services.Login(c.Request().Context(), req.Username, req.Password, c.RealIP())
	switch {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo ausente ou inválido")
	case errors.Is(err, services.ErrInvalidCredentials):
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"invalid-credentials", "Credenciais inválidas", "username ou senha incorretos")
	case errors.Is(err, services.ErrBannedIP):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"banned", "Usuário banido", "IP ou usuário banido")
	case err != nil:
		utils.Errorf("request_id=%s falha no login de usuário: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao fazer login")
	}

	// O token é uma função pura de (user_id, iat, segredo): a emissão do token
	// e o registro da conexão de sessão são atômicos (com retry de iat em caso
	// de colisão), garantindo um token único por conexão.
	token, _, err := services.CreateSessionConnection(c.Request().Context(), user.ID)
	if err != nil {
		utils.Errorf("request_id=%s falha ao registrar a conexão de sessão: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao registrar a sessão")
	}

	sameSite := http.SameSiteStrictMode
	if !cfg.SameSite {
		sameSite = http.SameSiteNoneMode
	}

	c.SetCookie(&http.Cookie{
		Name:     "Auth",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: sameSite,
		MaxAge:   int(utils.JWTExpiration.Seconds()),
	})

	// O token não é retornado no corpo: a sessão é entregue exclusivamente
	// pelo cookie Auth (HttpOnly), evitando que o cliente o persista em
	// estado JS/localStorage.
	resp := loginResponse{ConnectionViolation: user.ConnectionViolation}
	resp.User.ID = user.ID
	resp.User.Username = user.Username

	return c.JSON(http.StatusOK, resp)
}

type loginServerRequest struct {
	ServerPassword string `json:"server_password"`
}

// LoginServerHandler implementa POST /auth/login_server.
// Valida a senha do servidor (quando o servidor é não público) e emite a
// autorização temporária: define o cookie Auth com um JWT de curta duração
// (Max-Age 1800s). O token não é retornado no corpo (resposta 204): a
// autorização é entregue exclusivamente pelo cookie Auth (HttpOnly), evitando
// que o cliente o persista em estado JS/localStorage.
func LoginServerHandler(baseURL string, c echo.Context) error {
	cfg := config.LoadConfig()

	var req loginServerRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}

	if req.ServerPassword == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo 'server_password' é obrigatório")
	}
	if utf8.RuneCountInString(req.ServerPassword) > cfg.MaxPasswordLength {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			fmt.Sprintf("campo 'server_password' deve ter no máximo %d caracteres", cfg.MaxPasswordLength))
	}

	switch err := services.LoginServer(c.Request().Context(), req.ServerPassword, c.RealIP()); {
	case errors.Is(err, services.ErrInvalidInput):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo ausente ou inválido")
	case errors.Is(err, services.ErrBannedIP):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"banned", "Usuário banido", "o IP informado já foi usado por um usuário banido")
	case errors.Is(err, services.ErrServerNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "servidor não encontrado")
	case errors.Is(err, services.ErrInvalidCredentials):
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"invalid-credentials", "Credenciais inválidas", "senha do servidor incorreta")
	case err != nil:
		utils.Errorf("request_id=%s falha na autenticação no servidor: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao validar a senha do servidor")
	}

	tempToken, err := utils.GenerateTempToken(cfg.JWTSecret)
	if err != nil {
		utils.Errorf("request_id=%s falha ao gerar o token temporário: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao gerar a autorização temporária")
	}

	sameSite := http.SameSiteStrictMode
	if !cfg.SameSite {
		sameSite = http.SameSiteNoneMode
	}

	c.SetCookie(&http.Cookie{
		Name:     "Auth",
		Value:    tempToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: sameSite,
		MaxAge:   int(utils.TempTokenExpiration.Seconds()),
	})

	return c.NoContent(http.StatusNoContent)
}

// requireServerAccessGate aplica a regra de servidores não públicos antes de
// login/registro: lê o cookie Auth e delega a verificação ao service.
// Retorna true quando o handler deve continuar. Em caso de negação, já envia
// a resposta HTTP (problem+json) e retorna false.
func checkServerAccess(baseURL string, c echo.Context) bool {
	authCookie := ""
	if cookie, err := c.Cookie("Auth"); err == nil {
		authCookie = cookie.Value
	}

	err := services.RequireServerAccess(c.Request().Context(), authCookie)
	if err == nil {
		return true
	}

	switch {
	case errors.Is(err, services.ErrServerAccessRequired):
		_ = utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"server-access-required", "Acesso ao servidor negado",
			"servidor não público: informe a senha do servidor em /auth/login_server antes de continuar")
	default:
		utils.Errorf("request_id=%s falha ao verificar o acesso ao servidor: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		_ = utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao verificar o acesso ao servidor")
	}
	return false
}

type whoamiSettings struct {
	Version int               `json:"version"`
	Config  models.UserConfig `json:"config"`
}

type whoamiResponse struct {
	ID              string               `json:"id"`
	Username        string               `json:"username"`
	Nickname        *string              `json:"nickname"`
	AvatarBlob      []byte               `json:"avatar_blob"`
	AvatarFormat    string               `json:"avatar_format"`
	Status          *string              `json:"status"`
	StatusMessage   *string              `json:"status_message"`
	StatusUpdatedAt *time.Time           `json:"status_updated_at"`
	CreatedAt       time.Time            `json:"created_at"`
	Roles           []models.RoleSummary `json:"roles"`
	Settings        whoamiSettings       `json:"settings"`
	// ConnectionViolation indica que o reuso de um token de sessão foi
	// detectado neste usuário (todas as conexões foram revogadas); o cliente
	// usa a flag para avisar o usuário.
	ConnectionViolation bool `json:"connection_violation"`
}

// WhoamiHandler implementa GET /auth/whoami.
func WhoamiHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	user, settings, err := services.Whoami(c.Request().Context(), userID)
	switch {
	case errors.Is(err, services.ErrUserNotFound):
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao recuperar o usuário autenticado: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao recuperar o usuário autenticado")
	}

	return c.JSON(http.StatusOK, whoamiResponse{
		ID:              user.ID,
		Username:        user.Username,
		Nickname:        user.Nickname,
		AvatarBlob:      user.AvatarBlob,
		AvatarFormat:    user.AvatarFormat,
		Status:          user.Status,
		StatusMessage:   user.StatusMessage,
		StatusUpdatedAt: user.StatusUpdatedAt,
		CreatedAt:       user.CreatedAt,
		Roles:           user.Roles,
		Settings: whoamiSettings{
			Version: settings.Version,
			Config:  settings.Config,
		},
		ConnectionViolation: user.ConnectionViolation,
	})
}

// setAuthCookie define o cookie Auth com o token de sessão.
func setAuthCookie(c echo.Context, cfg *config.Config, token string) {
	sameSite := http.SameSiteStrictMode
	if !cfg.SameSite {
		sameSite = http.SameSiteNoneMode
	}

	c.SetCookie(&http.Cookie{
		Name:     "Auth",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: sameSite,
		MaxAge:   int(utils.JWTExpiration.Seconds()),
	})
}

// clearAuthCookie remove o cookie Auth do cliente.
func clearAuthCookie(c echo.Context, cfg *config.Config) {
	sameSite := http.SameSiteStrictMode
	if !cfg.SameSite {
		sameSite = http.SameSiteNoneMode
	}

	c.SetCookie(&http.Cookie{
		Name:     "Auth",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: sameSite,
		Expires:  time.Unix(0, 0),
	})
}

// authCookieValue retorna o valor do cookie Auth ("" quando ausente).
func authCookieValue(c echo.Context) string {
	cookie, err := c.Cookie("Auth")
	if err != nil || cookie.Value == "" {
		return ""
	}
	return cookie.Value
}

// LogoutHandler implementa POST /auth/logout.
// A rota permanece pública (sem JWTMiddleware): lê o cookie Auth, valida o
// JWT e revoga a conexão de sessão correspondente no banco (auth híbrida).
// Reuso de token já substituído revoga todas as conexões e marca
// users.connection_violation. O cookie Auth é removido do cliente em todos os
// casos.
func LogoutHandler(baseURL string, c echo.Context) error {
	cfg := config.LoadConfig()
	ctx := c.Request().Context()
	requestID := c.Request().Header.Get(echo.HeaderXRequestID)

	if token := authCookieValue(c); token != "" {
		if userID, verr := utils.ValidateToken(token, cfg.JWTSecret); verr == nil && userID != "" {
			if err := services.Logout(ctx, userID, token); err != nil {
				utils.Errorf("request_id=%s falha ao revogar a conexão no logout: %v", requestID, err)
			}
		}
	}

	clearAuthCookie(c, cfg)
	return c.NoContent(http.StatusNoContent)
}

type refreshResponse struct {
	Connection services.ConnectionInfo `json:"connection"`
}

// RefreshHandler implementa POST /auth/refresh.
// Rotaciona o token de sessão: a conexão atual (identificada pelo cookie Auth)
// é substituída atomicamente por uma nova. O novo token é definido no cookie
// Auth (HttpOnly) e não é retornado no corpo, evitando que o cliente o
// persista em estado JS/localStorage.
func RefreshHandler(baseURL string, c echo.Context) error {
	cfg := config.LoadConfig()
	ctx := c.Request().Context()
	requestID := c.Request().Header.Get(echo.HeaderXRequestID)

	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	token := authCookieValue(c)
	if token == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	newToken, conn, err := services.RefreshConnection(ctx, userID, token)
	switch {
	case errors.Is(err, services.ErrConnectionNotFound):
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao rotacionar o token de sessão: %v", requestID, err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao rotacionar o token de sessão")
	}

	setAuthCookie(c, cfg, newToken)
	return c.JSON(http.StatusOK, refreshResponse{Connection: conn})
}

type connectedDevicesResponse struct {
	Connections []services.ConnectionInfo `json:"connections"`
}

// ConnectedDevicesHandler implementa GET /auth/connected_devices.
// Lista as conexões de sessão ativas do usuário autenticado.
func ConnectedDevicesHandler(baseURL string, c echo.Context) error {
	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	conns, err := services.ListConnections(c.Request().Context(), userID)
	if err != nil {
		utils.Errorf("request_id=%s falha ao listar as conexões de sessão: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao listar as conexões de sessão")
	}

	return c.JSON(http.StatusOK, connectedDevicesResponse{Connections: conns})
}

type dropConnectionRequest struct {
	ConnectionID string `json:"connection_id"`
}

type dropConnectionResponse struct {
	Dropped int `json:"dropped"`
}

// DropConnectionHandler implementa POST /auth/drop_connection.
// Revoga conexões de sessão do usuário autenticado: o corpo
// {"connection_id": "<uuid>"} revoga uma conexão específica;
// {"connection_id": "ALL"} (case-insensitive) revoga todas as conexões
// ativas, incluindo a atual. Quando a conexão do cookie atual é revogada, o
// cookie Auth é removido do cliente.
func DropConnectionHandler(baseURL string, c echo.Context) error {
	cfg := config.LoadConfig()
	ctx := c.Request().Context()
	requestID := c.Request().Header.Get(echo.HeaderXRequestID)

	userID, ok := c.Get(middleware.UserIDContextKey).(string)
	if !ok || userID == "" {
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Token inválido ou expirado",
			"token de autenticação ausente, inválido ou expirado")
	}

	var req dropConnectionRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "corpo da requisição inválido")
	}
	if req.ConnectionID == "" {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo 'connection_id' é obrigatório")
	}

	token := authCookieValue(c)
	currentID := ""
	if token != "" {
		if id, err := services.CurrentConnectionID(ctx, userID, token); err == nil {
			currentID = id
		}
	}

	dropped, err := services.DropConnection(ctx, userID, req.ConnectionID)
	switch {
	case errors.Is(err, services.ErrInvalidConnection):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"campo 'connection_id' deve ser um UUID ou 'ALL'")
	case errors.Is(err, services.ErrConnectionNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "conexão de sessão não encontrada")
	case err != nil:
		utils.Errorf("request_id=%s falha ao revogar a conexão de sessão: %v", requestID, err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao revogar a conexão de sessão")
	}

	if token != "" && (strings.EqualFold(req.ConnectionID, "ALL") || strings.EqualFold(currentID, req.ConnectionID)) {
		clearAuthCookie(c, cfg)
	}

	return c.JSON(http.StatusOK, dropConnectionResponse{Dropped: dropped})
}
