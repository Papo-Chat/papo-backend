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

// MessageReactionUser é um usuário que reagiu a uma mensagem, com os dados
// da reação usados como cursor de paginação (id, created_at) no GET de
// reações.
type MessageReactionUser struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
}

// MessageReactionGroup é um tipo de reação em uma mensagem com a lista de
// usuários que reagiram (GET de reações). Count é o número de usuários do
// grupo na página retornada (o total do emoji está em `reactions` do corpo
// da mensagem).
type MessageReactionGroup struct {
	EmojiID *string               `json:"emoji_id"`
	Unicode *string               `json:"unicode"`
	Count   int                   `json:"count"`
	Users   []MessageReactionUser `json:"users"`
}

// MessageReactionList é a resposta de
// GET /channels/:channel_id/messages/:message_id/reactions (paginada por
// (created_at, id), 100 reações por página).
type MessageReactionList struct {
	MessageID string                 `json:"message_id"`
	Reactions []MessageReactionGroup `json:"reactions"`
	HasMore   bool                   `json:"has_more"`
}

// MessageUserReaction é a reação do usuário autenticado a uma mensagem
// (exposta como user_reactions nas respostas de mensagem).
type MessageUserReaction struct {
	ID      string  `json:"id"`
	EmojiID *string `json:"emoji_id"`
	Unicode *string `json:"unicode"`
}
