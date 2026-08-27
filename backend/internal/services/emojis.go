package services

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"
)

// ErrEmojiNotFound indica que o emoji não existe.
var ErrEmojiNotFound = errors.New("emoji não encontrado")

// ErrEmojiNameTaken indica que o nome do emoji já está em uso.
var ErrEmojiNameTaken = errors.New("nome do emoji já existe")

// ErrEmojiLimitReached indica que o servidor atingiu o limite de emojis.
var ErrEmojiLimitReached = errors.New("servidor atingiu o limite de emojis")

// maxEmojiNameLength é o tamanho máximo do nome de um emoji (32 caracteres, README).
const maxEmojiNameLength = 32

// maxEmojiBytes é o tamanho máximo de um emoji decodificado (256kb, README).
const maxEmojiBytes = 256 << 10

// maxEmojisPerServer é o número máximo de emojis por servidor (500, README).
const maxEmojisPerServer = 500

// emojiListLimit é o número máximo de emojis por resposta de listagem
// (25, README).
const emojiListLimit = 25

// ListEmojis lista os emojis paginados (README: GET /emojis).
// serverID é opcional: quando informado, filtra os emojis por servidor.
// Se since for fornecido, retorna apenas emojis criados após esse timestamp
// (paginação via cursor em created_at); se lastID for fornecido junto, o
// cursor é o par (created_at, id) e emojis do mesmo timestamp com id maior
// que lastID também são incluídos (evita pular emojis com timestamp igual).
func ListEmojis(ctx context.Context, serverID *string, since *time.Time, lastID string) (models.EmojiList, error) {
	if serverID != nil && *serverID == "" {
		serverID = nil
	}

	// Busca limit+1 para determinar has_more.
	emojis, err := storage.ListEmojis(ctx, serverID, since, lastID, emojiListLimit+1)
	if err != nil {
		return models.EmojiList{}, err
	}

	hasMore := len(emojis) > emojiListLimit
	if hasMore {
		emojis = emojis[:emojiListLimit]
	}

	// Resolve o blob da imagem e o formato (derivado do MIME da tabela media).
	for i := range emojis {
		blob, err := MediaContent(emojis[i].ImageMedia)
		if err != nil {
			return models.EmojiList{}, err
		}
		emojis[i].ImageBlob = blob
		emojis[i].Format = mimeToFormat(emojis[i].MimeType)
	}

	return models.EmojiList{Emojis: emojis, HasMore: hasMore}, nil
}

// CreateEmoji cria um novo emoji em um servidor (README: POST /emojis).
// imageBlob é base64 e format deve ser GIF, JPEG ou PNG (maiúsculas ou
// minúsculas); o conteúdo decodificado deve corresponder ao formato
// declarado (magic number), ter no máximo 256kb e dimensões de até 512px.
// Retorna ErrInvalidInput quando um campo está ausente ou inválido,
// ErrServerNotFound quando o servidor não existe,
// ErrEmojiLimitReached quando o servidor já possui 500 emojis e
// ErrEmojiNameTaken quando o nome já está em uso.
func CreateEmoji(ctx context.Context, serverID, name, format, imageBlob, createdBy string) (models.Emoji, error) {
	if serverID == "" || name == "" || format == "" || imageBlob == "" || createdBy == "" ||
		utf8.RuneCountInString(name) > maxEmojiNameLength {
		return models.Emoji{}, ErrInvalidInput
	}

	upperFormat := strings.ToUpper(format)
	if upperFormat != "GIF" && upperFormat != "JPEG" && upperFormat != "PNG" {
		return models.Emoji{}, ErrInvalidInput
	}

	decoded, err := base64.StdEncoding.DecodeString(imageBlob)
	if err != nil || len(decoded) == 0 || len(decoded) > maxEmojiBytes {
		return models.Emoji{}, ErrInvalidInput
	}
	if !avatarContentMatchesFormat(decoded, upperFormat) {
		return models.Emoji{}, ErrInvalidInput
	}

	if err := utils.ValidateImage(decoded, utils.MaxImageDimension); err != nil {
		return models.Emoji{}, ErrInvalidInput
	}

	if _, err := storage.GetServerByID(ctx, serverID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return models.Emoji{}, ErrServerNotFound
		}
		return models.Emoji{}, err
	}

	count, err := storage.CountEmojisByServer(ctx, serverID)
	if err != nil {
		return models.Emoji{}, err
	}
	if count >= maxEmojisPerServer {
		return models.Emoji{}, ErrEmojiLimitReached
	}

	sha, _, err := StoreMediaFromBytes(ctx, decoded, formatToMime(upperFormat))
	if err != nil {
		return models.Emoji{}, fmt.Errorf("falha ao gravar a imagem do emoji: %w", err)
	}

	createdByPtr := createdBy
	emoji, err := storage.CreateEmoji(ctx, serverID, name, sha, &createdByPtr)
	if errors.Is(err, storage.ErrUniqueViolation) {
		return models.Emoji{}, ErrEmojiNameTaken
	}
	if err != nil {
		return models.Emoji{}, err
	}

	// O blob já está em memória (acabou de ser enviado); o formato é derivado
	// do MIME gravado na tabela media.
	emoji.ImageBlob = decoded
	emoji.Format = mimeToFormat(emoji.MimeType)

	return emoji, nil
}

// DeleteEmoji exclui um emoji (README: DELETE /emojis/:emoji_id).
// Somente o autor do emoji, o dono do servidor do emoji ou um usuário com a
// permissão manage_server no servidor do emoji pode excluí-lo.
// Retorna ErrEmojiNotFound quando o emoji não existe e
// ErrPermissionDenied quando o usuário não pode excluí-lo.
func DeleteEmoji(ctx context.Context, emojiID, userID string) error {
	if emojiID == "" {
		return ErrEmojiNotFound
	}
	if userID == "" {
		return ErrInvalidInput
	}

	emoji, err := storage.GetEmojiByID(ctx, emojiID)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrEmojiNotFound
	}
	if err != nil {
		return err
	}

	if emoji.CreatedBy != nil && *emoji.CreatedBy == userID {
		return deleteEmoji(ctx, emojiID)
	}

	allowed, err := userHasRolePermission(ctx, emoji.ServerID, userID, func(p models.RolePermissions) bool {
		return p.ManageServer
	})
	if err != nil {
		return err
	}
	if !allowed {
		return ErrPermissionDenied
	}

	return deleteEmoji(ctx, emojiID)
}

// deleteEmoji exclui o emoji e mapeia o erro de registro inexistente.
func deleteEmoji(ctx context.Context, emojiID string) error {
	if err := storage.DeleteEmoji(ctx, emojiID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrEmojiNotFound
		}
		return err
	}

	return nil
}
