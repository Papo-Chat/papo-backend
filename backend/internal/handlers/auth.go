package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"

	"papo/internal/config"
	"papo/internal/middleware"
	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/utils"

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
	Token string `json:"token"`
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

	token, err := utils.GenerateToken(user.ID, cfg.JWTSecret)
	if err != nil {
		utils.Errorf("request_id=%s falha ao gerar token JWT: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao gerar token de autenticação")
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

	resp := loginResponse{Token: token}
	resp.User.ID = user.ID
	resp.User.Username = user.Username

	return c.JSON(http.StatusOK, resp)
}

type loginServerRequest struct {
	ServerPassword string `json:"server_password"`
}

type loginServerResponse struct {
	TempToken string `json:"temp_token"`
}

// LoginServerHandler implementa POST /auth/loginServer.
// Valida a senha do servidor (quando o servidor é não público) e emite a
// autorização temporária: define o cookie Auth com um JWT de curta duração
// (Max-Age 1800s) e retorna o token no corpo.
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

	return c.JSON(http.StatusOK, loginServerResponse{TempToken: tempToken})
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
			"servidor não público: informe a senha do servidor em /auth/loginServer antes de continuar")
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
	ID              string         `json:"id"`
	Username        string         `json:"username"`
	Nickname        *string        `json:"nickname"`
	AvatarBlob      []byte         `json:"avatar_blob"`
	AvatarFormat    string         `json:"avatar_format"`
	StatusMessage   *string        `json:"status_message"`
	StatusUpdatedAt *time.Time     `json:"status_updated_at"`
	CreatedAt       time.Time      `json:"created_at"`
	Settings        whoamiSettings `json:"settings"`
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
		StatusMessage:   user.StatusMessage,
		StatusUpdatedAt: user.StatusUpdatedAt,
		CreatedAt:       user.CreatedAt,
		Settings: whoamiSettings{
			Version: settings.Version,
			Config:  settings.Config,
		},
	})
}

// LogoutHandler implementa POST /auth/logout.
// A autenticação é stateless (JWT): o servidor não revoga o token, o logout
// apenas remove o cookie Auth do cliente.
func LogoutHandler(c echo.Context) error {
	cfg := config.LoadConfig()

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

	return c.NoContent(http.StatusNoContent)
}
