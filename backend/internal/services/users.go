package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"
)

var ErrUserNotReset = errors.New("flag reset_password ausente")

// maxAvatarBytes é o tamanho máximo de um avatar decodificado (2MB, README).
const maxAvatarBytes = 2 << 20

// maxNicknameLength é o tamanho máximo do nickname de um usuário
// (32 caracteres, README).
const maxNicknameLength = 32

// maxStatusLength é o tamanho máximo do status de um usuário
// (64 caracteres, README).
const maxStatusLength = 64

// userListLimit é o limite de usuários por requisição de listagem.
const userListLimit = 100

// UpdateSettings valida e salva as configurações do usuário autenticado.
// Retorna ErrUserNotFound quando o usuário não existe e ErrInvalidInput
// quando a configuração contém valores fora dos permitidos.
func UpdateSettings(ctx context.Context, userID string, config models.UserConfig) (models.UserSettings, error) {
	if userID == "" {
		return models.UserSettings{}, ErrUserNotFound
	}

	if err := validateUserConfig(config); err != nil {
		return models.UserSettings{}, ErrInvalidInput
	}

	if _, err := storage.GetUserByID(ctx, userID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return models.UserSettings{}, ErrUserNotFound
		}
		return models.UserSettings{}, err
	}

	return storage.UpsertUserSettings(ctx, userID, config)
}

// validateUserConfig confere os valores permitidos do user_config (README):
// theme (dark, light, system), fontSize (small, medium, huge) e
// messageDensity (compact, normal, comfortable).
func validateUserConfig(config models.UserConfig) error {
	switch config.Theme {
	case "dark", "light", "system":
	default:
		return errors.New("theme inválido")
	}

	switch config.Display.FontSize {
	case "small", "medium", "huge":
	default:
		return errors.New("fontSize inválido")
	}

	switch config.Display.MessageDensity {
	case "compact", "normal", "comfortable":
	default:
		return errors.New("messageDensity inválido")
	}

	return nil
}

// Profile retorna o perfil do usuário, sem campos sensíveis
// (password_hash, last_ip, banned) e sem as configurações.
func Profile(ctx context.Context, userID string) (models.User, error) {
	if userID == "" {
		return models.User{}, ErrUserNotFound
	}

	user, err := storage.GetUserByID(ctx, userID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.User{}, ErrUserNotFound
	}
	if err != nil {
		return models.User{}, err
	}

	return user, nil
}

// ListUsers lista os usuários cadastrados com keyset pagination, sem campos
// sensíveis ou densos (password_hash, avatar, banned, last_ip).
// Se since for fornecido, retorna apenas usuários criados após esse
// timestamp (polling de novos usuários); se lastID for fornecido junto, o
// cursor é o par (created_at, id) e usuários do mesmo timestamp com id maior
// que lastID também são incluídos (evita pular usuários com timestamp igual).
func ListUsers(ctx context.Context, since *time.Time, lastID string) (models.UserList, error) {
	users, err := storage.ListUsers(ctx, since, lastID, userListLimit+1)
	if err != nil {
		return models.UserList{}, err
	}

	hasMore := len(users) > userListLimit
	if hasMore {
		users = users[:userListLimit]
	}

	return models.UserList{Users: users, HasMore: hasMore}, nil
}

// UpdateUser atualiza o nickname e o status do usuário e marca o horário da
// atualização do status. Retorna ErrInvalidInput quando o nickname excede 32
// caracteres ou o status excede 64 caracteres e ErrUserNotFound quando o
// usuário não existe.
func UpdateUser(ctx context.Context, userID, nickname, status string) error {
	if userID == "" {
		return ErrUserNotFound
	}
	if utf8.RuneCountInString(nickname) > maxNicknameLength || utf8.RuneCountInString(status) > maxStatusLength {
		return ErrInvalidInput
	}

	_, err := storage.GetUserByID(ctx, userID)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrUserNotFound
	}
	if err != nil {
		return err
	}

	now := time.Now()
	if _, err := storage.UpdateUser(ctx, userID, models.User{
		Nickname:        &nickname,
		StatusMessage:   &status,
		StatusUpdatedAt: &now,
	}); err != nil {
		return fmt.Errorf("falha ao atualizar o perfil do usuário: %w", err)
	}

	return nil
}

// UpdateAvatar valida e salva o avatar do usuário. Quando avatar e
// avatar_format são vazios, o avatar é removido (blob NULL e formato ”).
// Retorna ErrInvalidInput quando o avatar não é um GIF, JPEG ou PNG válido
// de até 2MB com dimensões de até 512px e ErrUserNotFound quando o usuário
// não existe.
func UpdateAvatar(ctx context.Context, userID, avatar, avatarFormat string) error {
	if userID == "" {
		return ErrUserNotFound
	}

	if _, err := storage.GetUserByID(ctx, userID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrUserNotFound
		}
		return err
	}

	if avatar == "" && avatarFormat == "" {
		return storage.UpdateUserAvatar(ctx, nil, "", userID)
	}

	decoded, err := base64.StdEncoding.DecodeString(avatar)
	if err != nil {
		return ErrInvalidInput
	}

	upperFormat := strings.ToUpper(avatarFormat)
	if !avatarContentMatchesFormat(decoded, upperFormat) {
		return ErrInvalidInput
	}

	if len(decoded) > maxAvatarBytes {
		return ErrInvalidInput
	}

	if err := utils.ValidateImage(decoded, utils.MaxImageDimension); err != nil {
		return ErrInvalidInput
	}

	if err := storage.UpdateUserAvatar(ctx, decoded, upperFormat, userID); err != nil {
		return fmt.Errorf("falha ao atualizar o avatar do usuário: %w", err)
	}

	return nil
}

// BanUser altera o estado de banimento global do usuário alvo (users.banned).
// O usuário banido não pode voltar a autenticar, inclusive pelo mesmo IP
// (README). A autorização (dono de servidor ou manage_server) é feita no
// middleware antes de chegar aqui.
// Retorna ErrUserNotFound quando o usuário não existe.
func BanUser(ctx context.Context, targetID string, banState bool) error {
	if targetID == "" {
		return ErrUserNotFound
	}

	userOwner, err := storage.SetUserBanned(ctx, targetID, banState)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrUserNotFound
		}
		if errors.Is(err, ErrServerOwner) {
			return ErrServerOwner
		}
		return fmt.Errorf("falha ao alterar o estado de banimento do usuário: %w", err)
	}
	if userOwner {
		return ErrServerOwner
	}

	return nil
}

// ResetUserPassword marca o usuário alvo para redefinir a senha
// (users.reset_password = TRUE). A autorização (usuário agindo sobre si mesmo
// ou dono de um servidor) é feita no middleware RequireSelfOrServerOwner antes
// de chegar aqui. Retorna ErrUserNotFound quando o usuário não existe.
func ResetUserPassword(ctx context.Context, targetID string) error {
	if targetID == "" {
		return ErrUserNotFound
	}

	if err := storage.SetUserResetPassword(ctx, targetID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrUserNotFound
		}
		return fmt.Errorf("falha ao marcar o usuário para reset de senha: %w", err)
	}

	return nil
}

// ChangePassword altera a senha do usuário e reinicia a flag de reset de
// senha (users.reset_password = FALSE). O hash bcrypt é gerado aqui; a senha
// em texto claro nunca chega ao storage.
// Retorna ErrInvalidInput quando a senha é vazia ou excede o tamanho máximo
// configurado e ErrUserNotFound quando o usuário não existe.
func ChangePassword(ctx context.Context, userID, password string) error {
	if userID == "" {
		return ErrUserNotFound
	}

	user, err := storage.GetUserByID(ctx, userID)

	if user.ID == "" {
		return ErrUserNotFound
	}
	if !user.ResetPassword {
		return ErrUserNotReset
	}

	cfg := config.LoadConfig()
	if password == "" || utf8.RuneCountInString(password) > cfg.MaxPasswordLength {
		return ErrInvalidInput
	}

	passwordHash, err := utils.HashPassword(password)
	if err != nil {
		return fmt.Errorf("falha ao gerar hash da senha: %w", err)
	}

	if err := storage.UpdateUserPassword(ctx, userID, passwordHash); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrUserNotFound
		}
		return fmt.Errorf("falha ao atualizar a senha do usuário: %w", err)
	}

	return nil
}

// avatarContentMatchesFormat confere se o conteúdo decodificado corresponde a
// um dos formatos aceitos (GIF, JPEG, PNG) comparando o magic number, e se o
// formato declarado é um dos aceitos.
func avatarContentMatchesFormat(content []byte, format string) bool {
	switch format {
	case "PNG":
		return len(content) >= 8 && bytes.Equal(content[:8], []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	case "JPEG":
		return len(content) >= 3 && bytes.Equal(content[:3], []byte{0xFF, 0xD8, 0xFF})
	case "GIF":
		return bytes.HasPrefix(content, []byte("GIF87a")) || bytes.HasPrefix(content, []byte("GIF89a"))
	default:
		return false
	}
}
