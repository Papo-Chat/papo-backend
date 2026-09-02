package models

import "time"

// Message representa a tabela messages.
// ReplyTo referencia a mensagem respondida (sempre do mesmo canal). Não há FK:
// a mensagem referenciada pode ser excluída e o valor permanece como
// apontador pendente (o frontend exibe "mensagem não disponível").
type Message struct {
	ID        string     `db:"id" json:"id"`
	ChannelID string     `db:"channel_id" json:"channel_id"`
	AuthorID  *string    `db:"author_id" json:"author_id"`
	Content   *string    `db:"content" json:"content"`
	CreatedAt time.Time  `db:"created_at" json:"created_at"`
	EditedAt  *time.Time `db:"edited_at" json:"edited_at"`
	ReplyTo   *string    `db:"reply_to" json:"reply_to"`
}

// PinnedMessage representa a tabela pinned_messages: uma mensagem fixada em um
// canal. A PK (channel_id, message_id) torna a fixação idempotente.
type PinnedMessage struct {
	ChannelID string    `db:"channel_id" json:"channel_id"`
	MessageID string    `db:"message_id" json:"message_id"`
	PinnedBy  *string   `db:"pinned_by" json:"pinned_by"`
	PinnedAt  time.Time `db:"pinned_at" json:"pinned_at"`
}

// MessageAttachment é a informação mínima do attachment exposta nas respostas
// de mensagens (listagem, criação e edição).
type MessageAttachment struct {
	ID               string    `json:"id"`
	MimeType         string    `json:"mime_type"`
	OriginalFileName string    `json:"original_file_name"`
	SizeBytes        int64     `json:"size_bytes"`
	ThumbnailID      *string   `json:"thumbnail_id"`
	CreatedAt        time.Time `json:"created_at"`
}

// MessageWithAttachment é a mensagem com seus attachments, link previews e
// contagem de reações, como exposta pela API (respostas de listagem, criação
// e edição de mensagens).
type MessageWithAttachment struct {
	Message
	Attachments []MessageAttachment      `json:"attachments"`
	Previews    []LinkPreview            `json:"previews"`
	Reactions   []MessageReactionSummary `json:"reactions"`
}

// MessageList é a resposta de GET /channels/:channel_id/messages.
type MessageList struct {
	ChannelID string                  `json:"channel_id"`
	Messages  []MessageWithAttachment `json:"messages"`
	HasMore   bool                    `json:"has_more"`
}
