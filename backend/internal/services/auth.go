package services

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"
)

// Erros de negócio de autenticação.
var (
	ErrInvalidInput         = errors.New("campo ausente ou inválido")
	ErrUsernameTaken        = errors.New("username já existe")
	ErrBannedIP             = errors.New("usuário banido")
	ErrServerOwner          = errors.New("usuário é dono do servidor")
	ErrInvalidCredentials   = errors.New("credenciais inválidas")
	ErrUserNotFound         = errors.New("usuário não encontrado")
	ErrServerAccessRequired = errors.New("autorização temporária de acesso ao servidor ausente ou inválida")
)

// Register cria um novo usuário.
// Valida username e senha, recusa o cadastro quando o IP já foi usado por um
// usuário banido, gera o hash bcrypt da senha e grava o último IP do usuário
// para que a regra de banimento por IP se aplique desde a primeira conexão.
// O usuário retornado não contém o password_hash.
func Register(ctx context.Context, username, password, ip string) (models.User, error) {
	//Lê a configuração do tamanho máximo dos campos
	cfg := config.LoadConfig()

	MaxPasswordLength := cfg.MaxPasswordLength
	MaxUsernameLength := cfg.MaxUsernameLength

	if username == "" || password == "" ||
		utf8.RuneCountInString(username) > MaxUsernameLength || utf8.RuneCountInString(password) > MaxPasswordLength {
		return models.User{}, ErrInvalidInput
	}

	banned, err := storage.HasBannedUserByIP(ctx, ip)
	if err != nil {
		return models.User{}, err
	}
	if banned {
		return models.User{}, ErrBannedIP
	}

	passwordHash, err := utils.HashPassword(password)
	if err != nil {
		return models.User{}, fmt.Errorf("falha ao gerar hash da senha: %w", err)
	}

	user, _, err := storage.CreateUser(ctx, username, passwordHash, ip)
	if errors.Is(err, storage.ErrUniqueViolation) {
		return models.User{}, ErrUsernameTaken
	}
	if err != nil {
		return models.User{}, err
	}

	RecordAudit(ctx, AuditEntry{
		ActorID:    user.ID,
		Action:     ActionUserRegister,
		EntityType: EntityUser,
		EntityID:   &user.ID,
	})

	return user, nil
}

// Login autentica um usuário por username e senha.
// Recusa o acesso quando o IP já foi usado por um usuário banido ou quando
// o próprio usuário está banido.
// Usuário inexistente e senha incorreta retornam o mesmo erro, para não
// revelar qual dos dois falhou.
// O usuário retornado não contém o password_hash.
func Login(ctx context.Context, username, password, ip string) (models.User, error) {
	//Lê a configuração do tamanho máximo dos campos
	cfg := config.LoadConfig()

	MaxPasswordLength := cfg.MaxPasswordLength
	MaxUsernameLength := cfg.MaxUsernameLength

	if username == "" || password == "" ||
		utf8.RuneCountInString(username) > MaxUsernameLength || utf8.RuneCountInString(password) > MaxPasswordLength {
		return models.User{}, ErrInvalidInput
	}

	banned, err := storage.HasBannedUserByIP(ctx, ip)
	if err != nil {
		return models.User{}, err
	}
	if banned {
		return models.User{}, ErrBannedIP
	}

	user, err := storage.GetUserByUsername(ctx, username)
	if err != nil {
		return models.User{}, ErrInvalidCredentials
	}

	if user.Banned {
		return models.User{}, ErrBannedIP
	}

	if err := utils.CheckPassword(password, user.PasswordHash); err != nil {
		return models.User{}, ErrInvalidCredentials
	}

	if err := storage.UpdateUserLastIP(ctx, user.ID, ip); err != nil {
		return models.User{}, fmt.Errorf("falha ao atualizar last_ip do usuário: %w", err)
	}

	// o hash é usado somente internamente para validação da senha
	user.PasswordHash = ""

	return user, nil
}

// LoginServer valida a senha do servidor e sinaliza sucesso, para que o
// handler emita a autorização temporária. Servidores públicos não têm senha,
// então a validação é pulada.
// Retorna ErrInvalidInput quando a senha está vazia ou acima do tamanho
// máximo, ErrBannedIP quando o IP foi usado por um usuário banido,
// ErrServerNotFound quando o servidor ainda não existe e
// ErrInvalidCredentials quando a senha do servidor não confere.
func LoginServer(ctx context.Context, serverPassword, ip string) error {
	cfg := config.LoadConfig()

	if serverPassword == "" || utf8.RuneCountInString(serverPassword) > cfg.MaxPasswordLength {
		return ErrInvalidInput
	}

	banned, err := storage.HasBannedUserByIP(ctx, ip)
	if err != nil {
		return err
	}
	if banned {
		return ErrBannedIP
	}

	server, err := storage.GetServerWithPasswordHash(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrServerNotFound
		}
		return err
	}

	if !server.PublicServer {
		if server.PasswordHash == nil {
			return ErrInvalidCredentials
		}
		if err := utils.CheckPassword(serverPassword, *server.PasswordHash); err != nil {
			return ErrInvalidCredentials
		}
	}

	return nil
}

// RequireServerAccess aplica a regra de servidores não públicos: antes de
// logar ou registrar, a requisição deve carregar uma autorização temporária
// válida (emitida por /auth/login_server) no cookie Auth.
// Servidores públicos e o caso de bootstrap (servidor ainda não criado) não
// têm essa exigência. authCookie é o valor bruto do cookie Auth ("" quando
// ausente).
func RequireServerAccess(ctx context.Context, authCookie string) error {
	server, err := storage.GetServerWithPasswordHash(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// Bootstrap: o servidor ainda não foi criado, não há senha a validar.
			return nil
		}
		return err
	}

	if server.PublicServer {
		// Servidor público: sem exigência de senha.
		return nil
	}

	cfg := config.LoadConfig()
	valid, err := utils.ValidateTempToken(authCookie, cfg.JWTSecret)
	if err != nil || !valid {
		return ErrServerAccessRequired
	}

	return nil
}

// Whoami retorna o usuário autenticado e suas configurações.
func Whoami(ctx context.Context, userID string) (models.User, models.UserSettings, error) {
	if userID == "" {
		return models.User{}, models.UserSettings{}, ErrUserNotFound
	}

	user, err := storage.GetUserByID(ctx, userID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.User{}, models.UserSettings{}, ErrUserNotFound
	}
	if err != nil {
		return models.User{}, models.UserSettings{}, err
	}

	if err := resolveAvatar(ctx, &user); err != nil {
		return models.User{}, models.UserSettings{}, err
	}

	settings, err := storage.GetUserSettings(ctx, userID)
	if err != nil {
		return models.User{}, models.UserSettings{}, err
	}

	return user, settings, nil
}
