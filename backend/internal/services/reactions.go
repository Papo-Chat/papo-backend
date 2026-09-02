package services

import (
	"context"
	"errors"
	"unicode/utf8"

	"papo/internal/models"
	"papo/internal/storage"
)

// ErrTooManyReactions indica que a mensagem atingiu o limite de tipos de
// reação (20).
var ErrTooManyReactions = errors.New("limite de tipos de reação por mensagem atingido")

// ErrReactionNotFound indica que o usuário não reagiu à mensagem com aquele
// emoji.
var ErrReactionNotFound = errors.New("reação não encontrada")

// maxReactionUnicodeLength é o tamanho máximo (em runes) de um emoji unicode
// de reação (cobre sequências ZWJ, modificadores de tom de pele e VS16).
const maxReactionUnicodeLength = 16

// normalizeReactionInput valida o input de reação: exatamente um de emojiID
// (emoji custom do banco) ou unicode (emoji unicode) deve ser informado.
// Retorna os valores como ponteiros para o storage (nil = ausente/NULL).
// Retorna ErrInvalidInput quando ambos ou nenhum são informados ou quando o
// unicode excede 16 runes.
func normalizeReactionInput(emojiID, unicode string) (*string, *string, error) {
	hasEmoji := emojiID != ""
	hasUnicode := unicode != ""
	if hasEmoji == hasUnicode {
		return nil, nil, ErrInvalidInput
	}
	if hasUnicode && utf8.RuneCountInString(unicode) > maxReactionUnicodeLength {
		return nil, nil, ErrInvalidInput
	}

	var emojiPtr *string
	var unicodePtr *string
	if hasEmoji {
		emojiPtr = &emojiID
	} else {
		unicodePtr = &unicode
	}
	return emojiPtr, unicodePtr, nil
}

// getChannelForMessage carrega a mensagem e o canal da URL e verifica que a
// mensagem pertence ao canal. Retorna ErrMessageNotFound quando a mensagem não
// existe ou não pertence ao canal e ErrChannelNotFound quando o canal não
// existe.
func getChannelForMessage(ctx context.Context, channelID, messageID string) (models.Channel, error) {
	message, err := storage.GetMessageByID(ctx, messageID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.Channel{}, ErrMessageNotFound
	}
	if err != nil {
		return models.Channel{}, err
	}

	// A mensagem deve pertencer ao canal da URL.
	if message.ChannelID != channelID {
		return models.Channel{}, ErrMessageNotFound
	}

	channel, err := storage.GetChannelByID(ctx, channelID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.Channel{}, ErrChannelNotFound
	}
	if err != nil {
		return models.Channel{}, err
	}

	return channel, nil
}

// AddReactionToMessage adiciona a reação de um usuário a uma mensagem
// (POST /channels/:channel_id/messages/:message_id/reactions). A mensagem deve
// existir e pertencer ao canal da URL. O usuário precisa da permissão
// send_messages do canal (o dono do servidor sempre pode e em canais sem
// roles definidas o envio é livre). Exatamente um de emojiID (emoji custom do
// banco) ou unicode (emoji unicode) deve ser informado.
// A operação é idempotente: reagir de novo com o mesmo emoji retorna o
// registro existente (created=false).
// Retorna (reaction, created, count, err), onde count é o número de usuários
// que reagiram com aquele emoji após a operação.
// Retorna ErrInvalidInput quando ambos ou nenhum emoji são informados ou o
// unicode excede 16 runes, ErrMessageNotFound quando a mensagem não existe ou
// não pertence ao canal, ErrChannelNotFound quando o canal não existe,
// ErrEmojiNotFound quando emoji_id referencia um emoji inexistente,
// ErrPermissionDenied quando o usuário não pode enviar mensagens no canal e
// ErrTooManyReactions quando a mensagem já tem 20 tipos e o emoji é um tipo
// novo.
func AddReactionToMessage(ctx context.Context, channelID, messageID, userID, emojiID, unicode string) (models.MessageReaction, bool, int, error) {
	if channelID == "" || messageID == "" || userID == "" {
		return models.MessageReaction{}, false, 0, ErrInvalidInput
	}

	emojiPtr, unicodePtr, err := normalizeReactionInput(emojiID, unicode)
	if err != nil {
		return models.MessageReaction{}, false, 0, err
	}

	channel, err := getChannelForMessage(ctx, channelID, messageID)
	if err != nil {
		return models.MessageReaction{}, false, 0, err
	}

	allowed, err := userHasChannelPermission(ctx, channel, userID, true, func(p models.ChannelPermission) bool {
		return p.SendMessages
	})
	if err != nil {
		return models.MessageReaction{}, false, 0, err
	}
	if !allowed {
		return models.MessageReaction{}, false, 0, ErrPermissionDenied
	}

	if emojiPtr != nil {
		if _, err := storage.GetEmojiByID(ctx, *emojiPtr); errors.Is(err, storage.ErrNotFound) {
			return models.MessageReaction{}, false, 0, ErrEmojiNotFound
		} else if err != nil {
			return models.MessageReaction{}, false, 0, err
		}
	}

	reaction, created, count, err := storage.AddReaction(ctx, messageID, userID, emojiPtr, unicodePtr)
	if errors.Is(err, storage.ErrReactionLimitReached) {
		return models.MessageReaction{}, false, 0, ErrTooManyReactions
	}
	if err != nil {
		return models.MessageReaction{}, false, 0, err
	}

	return reaction, created, count, nil
}

// RemoveReactionFromMessage remove a própria reação do usuário de uma
// mensagem (DELETE /channels/:channel_id/messages/:message_id/reactions). A
// mensagem deve existir e pertencer ao canal da URL. O usuário precisa da
// permissão read_channel do canal. Somente a própria reação do usuário
// autenticado pode ser removida.
// Retorna count = o número de usuários que reagiram com aquele emoji após a
// remoção (0 quando era o último).
// Retorna ErrInvalidInput quando ambos ou nenhum emoji são informados ou o
// unicode excede 16 runes, ErrMessageNotFound quando a mensagem não existe ou
// não pertence ao canal, ErrChannelNotFound quando o canal não existe,
// ErrEmojiNotFound quando emoji_id referencia um emoji inexistente,
// ErrPermissionDenied quando o usuário não pode ler o canal e
// ErrReactionNotFound quando o usuário não reagiu com aquele emoji.
func RemoveReactionFromMessage(ctx context.Context, channelID, messageID, userID, emojiID, unicode string) (int, error) {
	if channelID == "" || messageID == "" || userID == "" {
		return 0, ErrInvalidInput
	}

	emojiPtr, unicodePtr, err := normalizeReactionInput(emojiID, unicode)
	if err != nil {
		return 0, err
	}

	channel, err := getChannelForMessage(ctx, channelID, messageID)
	if err != nil {
		return 0, err
	}

	allowed, err := userHasChannelPermission(ctx, channel, userID, true, func(p models.ChannelPermission) bool {
		return p.ReadChannel
	})
	if err != nil {
		return 0, err
	}
	if !allowed {
		return 0, ErrPermissionDenied
	}

	if emojiPtr != nil {
		if _, err := storage.GetEmojiByID(ctx, *emojiPtr); errors.Is(err, storage.ErrNotFound) {
			return 0, ErrEmojiNotFound
		} else if err != nil {
			return 0, err
		}
	}

	count, err := storage.RemoveReaction(ctx, messageID, userID, emojiPtr, unicodePtr)
	if errors.Is(err, storage.ErrNotFound) {
		return 0, ErrReactionNotFound
	}
	if err != nil {
		return 0, err
	}

	return count, nil
}

// ListMessageReactions lista as reações de uma mensagem com os usuários que
// reagiram (GET /channels/:channel_id/messages/:message_id/reactions). A
// mensagem deve existir e pertencer ao canal da URL. O usuário precisa da
// permissão read_channel do canal (o dono do servidor sempre pode e em canais
// sem roles definidas a leitura é livre).
// Retorna ErrInvalidInput quando um parâmetro está ausente,
// ErrMessageNotFound quando a mensagem não existe ou não pertence ao canal,
// ErrChannelNotFound quando o canal não existe e ErrPermissionDenied quando o
// usuário não pode ler o canal.
func ListMessageReactions(ctx context.Context, channelID, messageID, userID string) (models.MessageReactionList, error) {
	if channelID == "" || messageID == "" || userID == "" {
		return models.MessageReactionList{}, ErrInvalidInput
	}

	channel, err := getChannelForMessage(ctx, channelID, messageID)
	if err != nil {
		return models.MessageReactionList{}, err
	}

	allowed, err := userHasChannelPermission(ctx, channel, userID, true, func(p models.ChannelPermission) bool {
		return p.ReadChannel
	})
	if err != nil {
		return models.MessageReactionList{}, err
	}
	if !allowed {
		return models.MessageReactionList{}, ErrPermissionDenied
	}

	groups, err := storage.ListReactionsByMessage(ctx, messageID)
	if err != nil {
		return models.MessageReactionList{}, err
	}

	return models.MessageReactionList{MessageID: messageID, Reactions: groups}, nil
}
