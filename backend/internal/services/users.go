package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
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

// maxBannerBytes é o tamanho máximo de um banner decodificado (2MB, README).
const maxBannerBytes = 2 << 20

// maxBannerDimension é a dimensão máxima (px) de largura ou altura de um
// banner (1024px, README).
const maxBannerDimension = 1024

// maxNicknameLength é o tamanho máximo do nickname de um usuário
// (32 caracteres, README).
const maxNicknameLength = 32

// maxDescriptionLength é o tamanho máximo da description de um usuário
// (512 caracteres, README).
const maxDescriptionLength = 512

// maxStatusLength é o tamanho máximo do status de um usuário
// (64 caracteres, README).
const maxStatusLength = 64

// userListLimit é o limite de usuários por requisição de listagem.
const userListLimit = 100

// profileBatchLimit é o limite de ids por requisição de perfis em lote.
const profileBatchLimit = 50

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

	settings, err := storage.UpsertUserSettings(ctx, userID, config)
	if err != nil {
		return models.UserSettings{}, err
	}

	RecordAudit(ctx, AuditEntry{
		ActorID:    userID,
		Action:     ActionUserUpdateSettings,
		EntityType: EntityUser,
		EntityID:   &userID,
	})

	return settings, nil
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
// (password_hash, last_ip, banned) e sem as configurações. O blob do avatar
// e o formato são resolvidos da tabela media e do disco; as roles
// atribuídas ao usuário são resolvidas de user_roles/roles.
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

	if err := resolveAvatar(ctx, &user); err != nil {
		return models.User{}, err
	}

	roles, err := storage.GetRoleSummariesByUser(ctx, userID)
	if err != nil {
		return models.User{}, err
	}
	user.Roles = roles

	return user, nil
}

// resolveAvatar preenche AvatarBlob e AvatarFormat a partir da referência
// media do usuário (sem efeito quando o usuário não tem avatar).
func resolveAvatar(ctx context.Context, user *models.User) error {
	if user.AvatarMedia == nil {
		return nil
	}

	media, err := storage.GetMediaByHash(ctx, *user.AvatarMedia)
	if err != nil {
		return err
	}
	blob, err := MediaContent(*user.AvatarMedia)
	if err != nil {
		return err
	}

	user.AvatarBlob = blob
	user.AvatarFormat = mimeToFormat(media.MimeType)
	return nil
}

// ProfilesBatch retorna os perfis dos usuários solicitados (mesma forma de
// Profile), na ordem da requisição, pulando ids que não existem. Ids
// duplicados são considerados uma única vez. Retorna ErrInvalidInput quando a
// lista de ids é vazia ou excede 50 ids.
func ProfilesBatch(ctx context.Context, ids []string) ([]models.User, error) {
	if len(ids) == 0 || len(ids) > profileBatchLimit {
		return nil, ErrInvalidInput
	}

	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}

	users, err := storage.GetUsersByIDs(ctx, unique)
	if err != nil {
		return nil, err
	}

	foundIDs := make([]string, 0, len(users))
	byID := make(map[string]models.User, len(users))
	for _, user := range users {
		byID[user.ID] = user
		foundIDs = append(foundIDs, user.ID)
	}

	rolesByUser, err := storage.GetRoleSummariesByUsers(ctx, foundIDs)
	if err != nil {
		return nil, err
	}

	profiles := make([]models.User, 0, len(unique))
	for _, id := range unique {
		user, ok := byID[id]
		if !ok {
			continue
		}
		if err := resolveAvatar(ctx, &user); err != nil {
			return nil, err
		}
		user.Roles = rolesByUser[id]
		profiles = append(profiles, user)
	}

	return profiles, nil
}

// ListUsers lista os usuários cadastrados com keyset pagination, sem campos
// sensíveis ou densos (password_hash, avatar, banned, last_ip), incluindo as
// roles atribuídas a cada usuário.
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

	userIDs := make([]string, 0, len(users))
	for _, user := range users {
		userIDs = append(userIDs, user.ID)
	}
	rolesByUser, err := storage.GetRoleSummariesByUsers(ctx, userIDs)
	if err != nil {
		return models.UserList{}, err
	}
	for i := range users {
		users[i].Roles = rolesByUser[users[i].ID]
	}

	return models.UserList{Users: users, HasMore: hasMore}, nil
}

// UpdateUser atualiza o nickname, o status e a description do usuário e marca
// o horário da atualização do status. Retorna ErrInvalidInput quando o
// nickname excede 32 caracteres, o status excede 64 caracteres ou a
// description excede 512 caracteres e ErrUserNotFound quando o usuário não
// existe.
func UpdateUser(ctx context.Context, userID, nickname, status, description string) error {
	if userID == "" {
		return ErrUserNotFound
	}
	if utf8.RuneCountInString(nickname) > maxNicknameLength ||
		utf8.RuneCountInString(status) > maxStatusLength ||
		utf8.RuneCountInString(description) > maxDescriptionLength {
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
		Description:     &description,
	}); err != nil {
		return fmt.Errorf("falha ao atualizar o perfil do usuário: %w", err)
	}

	RecordAudit(ctx, AuditEntry{
		ActorID:    userID,
		Action:     ActionUserUpdateProfile,
		EntityType: EntityUser,
		EntityID:   &userID,
		Metadata: map[string]any{
			"nickname":    nickname,
			"status":      status,
			"description": description,
		},
	})

	return nil
}

// UpdateAvatar valida e salva o avatar do usuário. Quando avatar e
// avatar_format são vazios, o avatar é removido (referência media NULL).
// Retorna ErrInvalidInput quando o avatar não é um GIF, JPEG, PNG ou WEBP
// válido de até 2MB com dimensões de até 512px e ErrUserNotFound quando o
// usuário não existe.
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
		if err := storage.UpdateUserAvatar(ctx, nil, userID); err != nil {
			return err
		}
		RecordAudit(ctx, AuditEntry{
			ActorID:    userID,
			Action:     ActionUserUpdateAvatar,
			EntityType: EntityUser,
			EntityID:   &userID,
		})
		return nil
	}

	decoded, err := base64.StdEncoding.DecodeString(avatar)
	if err != nil {
		return ErrInvalidInput
	}

	upperFormat := normalizeImageFormat(avatarFormat)
	if !avatarContentMatchesFormat(decoded, upperFormat) {
		return ErrInvalidInput
	}

	if len(decoded) > maxAvatarBytes {
		return ErrInvalidInput
	}

	if err := utils.ValidateImage(decoded, utils.MaxImageDimension); err != nil {
		return ErrInvalidInput
	}

	sha, _, err := StoreMediaFromBytes(ctx, decoded, formatToMime(upperFormat))
	if err != nil {
		return fmt.Errorf("falha ao gravar o avatar do usuário: %w", err)
	}

	if err := storage.UpdateUserAvatar(ctx, &sha, userID); err != nil {
		return fmt.Errorf("falha ao atualizar o avatar do usuário: %w", err)
	}

	RecordAudit(ctx, AuditEntry{
		ActorID:    userID,
		Action:     ActionUserUpdateAvatar,
		EntityType: EntityUser,
		EntityID:   &userID,
	})

	return nil
}

// UpdateBanner valida e salva o banner do usuário (content-addressable,
// como o avatar). Quando banner e banner_format são vazios, o banner é
// removido (referência media NULL). Retorna ErrInvalidInput quando o banner
// não é um GIF, JPEG, PNG ou WEBP válido de até 2MB com dimensões de até
// 1024px e ErrUserNotFound quando o usuário não existe.
func UpdateBanner(ctx context.Context, userID, banner, bannerFormat string) error {
	if userID == "" {
		return ErrUserNotFound
	}

	if _, err := storage.GetUserByID(ctx, userID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrUserNotFound
		}
		return err
	}

	if banner == "" && bannerFormat == "" {
		if err := storage.UpdateUserBanner(ctx, nil, userID); err != nil {
			return err
		}
		RecordAudit(ctx, AuditEntry{
			ActorID:    userID,
			Action:     ActionUserUpdateBanner,
			EntityType: EntityUser,
			EntityID:   &userID,
		})
		return nil
	}

	decoded, err := base64.StdEncoding.DecodeString(banner)
	if err != nil {
		return ErrInvalidInput
	}

	upperFormat := normalizeImageFormat(bannerFormat)
	if !avatarContentMatchesFormat(decoded, upperFormat) {
		return ErrInvalidInput
	}

	if len(decoded) > maxBannerBytes {
		return ErrInvalidInput
	}

	if err := utils.ValidateImage(decoded, maxBannerDimension); err != nil {
		return ErrInvalidInput
	}

	sha, _, err := StoreMediaFromBytes(ctx, decoded, formatToMime(upperFormat))
	if err != nil {
		return fmt.Errorf("falha ao gravar o banner do usuário: %w", err)
	}

	if err := storage.UpdateUserBanner(ctx, &sha, userID); err != nil {
		return fmt.Errorf("falha ao atualizar o banner do usuário: %w", err)
	}

	RecordAudit(ctx, AuditEntry{
		ActorID:    userID,
		Action:     ActionUserUpdateBanner,
		EntityType: EntityUser,
		EntityID:   &userID,
	})

	return nil
}

// statusAway e statusBusy são os valores persistidos de status do usuário
// (users.status); o status efetivo na presença é online/offline quando não
// há status persistido.
const (
	statusAway = "away"
	statusBusy = "busy"
)

// UpdateStatus valida e persiste o status do usuário (away/busy; nil remove
// o status). Retorna ErrInvalidInput quando o status não é away/busy e
// ErrUserNotFound quando o usuário não existe.
func UpdateStatus(ctx context.Context, userID string, status *string) error {
	if userID == "" {
		return ErrUserNotFound
	}
	if status != nil && *status != statusAway && *status != statusBusy {
		return ErrInvalidInput
	}

	if err := storage.UpdateUserStatus(ctx, userID, status); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrUserNotFound
		}
		return fmt.Errorf("falha ao atualizar o status do usuário: %w", err)
	}

	statusMeta := map[string]any{}
	if status != nil {
		statusMeta["status"] = *status
	}
	RecordAudit(ctx, AuditEntry{
		ActorID:    userID,
		Action:     ActionUserUpdateStatus,
		EntityType: EntityUser,
		EntityID:   &userID,
		Metadata:   statusMeta,
	})

	return nil
}

// BanUser altera o estado de banimento global do usuário alvo (users.banned).
// O usuário banido não pode voltar a autenticar, inclusive pelo mesmo IP
// (README). A autorização (dono de servidor ou manage_server) é feita no
// middleware antes de chegar aqui.
// Retorna ErrUserNotFound quando o usuário não existe.
func BanUser(ctx context.Context, actorID, targetID string, banState bool) error {
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

	action := ActionUserBan
	if !banState {
		action = ActionUserUnban
	}
	RecordAudit(ctx, AuditEntry{
		ActorID:      actorID,
		Action:       action,
		EntityType:   EntityUser,
		EntityID:     &targetID,
		TargetUserID: &targetID,
		Metadata:     map[string]any{"banned": banState},
	})

	return nil
}

// ResetUserPassword marca o usuário alvo para redefinir a senha
// (users.reset_password = TRUE). A autorização (usuário agindo sobre si mesmo
// ou dono de um servidor) é feita no middleware RequireSelfOrServerOwner antes
// de chegar aqui. Retorna ErrUserNotFound quando o usuário não existe.
func ResetUserPassword(ctx context.Context, actorID, targetID string) error {
	if targetID == "" {
		return ErrUserNotFound
	}

	if err := storage.SetUserResetPassword(ctx, targetID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrUserNotFound
		}
		return fmt.Errorf("falha ao marcar o usuário para reset de senha: %w", err)
	}

	RecordAudit(ctx, AuditEntry{
		ActorID:      actorID,
		Action:       ActionUserResetPassword,
		EntityType:   EntityUser,
		EntityID:     &targetID,
		TargetUserID: &targetID,
	})

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

	RecordAudit(ctx, AuditEntry{
		ActorID:    userID,
		Action:     ActionUserChangePassword,
		EntityType: EntityUser,
		EntityID:   &userID,
	})

	return nil
}

// avatarContentMatchesFormat confere se o conteúdo decodificado corresponde a
// um dos formatos aceitos (GIF, JPEG, PNG, WEBP) comparando o magic number, e
// se o formato declarado é um dos aceitos.
func avatarContentMatchesFormat(content []byte, format string) bool {
	switch format {
	case "PNG":
		return len(content) >= 8 && bytes.Equal(content[:8], []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	case "JPEG":
		return len(content) >= 3 && bytes.Equal(content[:3], []byte{0xFF, 0xD8, 0xFF})
	case "GIF":
		return bytes.HasPrefix(content, []byte("GIF87a")) || bytes.HasPrefix(content, []byte("GIF89a"))
	case "WEBP":
		return len(content) >= 12 && bytes.Equal(content[:4], []byte("RIFF")) && bytes.Equal(content[8:12], []byte("WEBP"))
	default:
		return false
	}
}
