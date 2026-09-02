package models

import "time"

// MessageReaction representa a tabela message_reactions: a reação de um
// usuário específico a uma mensagem específica. A reação é um emoji custom do
// banco (EmojiID) OU um emoji unicode (Unicode), nunca os dois.
type MessageReaction struct {
	MessageID string    `db:"message_id" json:"message_id"`
	UserID    string    `db:"user_id" json:"user_id"`
	EmojiID   *string   `db:"emoji_id" json:"emoji_id"`
	Unicode   *string   `db:"unicode" json:"unicode"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

// MessageReactionSummary é a contagem de um tipo de reação em uma mensagem
// (exposto nas respostas de mensagens).
type MessageReactionSummary struct {
	EmojiID *string `json:"emoji_id"`
	Unicode *string `json:"unicode"`
	Count   int     `json:"count"`
}

// MessageReactionGroup é um tipo de reação em uma mensagem com a lista de
// usuários que reagiram (GET de reações).
type MessageReactionGroup struct {
	EmojiID *string  `json:"emoji_id"`
	Unicode *string  `json:"unicode"`
	Count   int      `json:"count"`
	Users   []string `json:"users"`
}

// MessageReactionList é a resposta de
// GET /channels/:channel_id/messages/:message_id/reactions.
type MessageReactionList struct {
	MessageID string                 `json:"message_id"`
	Reactions []MessageReactionGroup `json:"reactions"`
}
