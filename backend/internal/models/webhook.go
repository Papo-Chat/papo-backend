package models

import "time"

// Webhook representa a tabela webhooks: um remetente independentemente
// autenticado (token Bearer próprio, nunca um JWT de usuário), limitado a um
// único canal. secret_hash jamais é serializado para clientes.
//
// Identidade de exibição (nickname + avatar):
//   - DisplayName é o nickname persistente; NULL significa usar Name.
//   - AvatarMedia referencia o blob do avatar na tabela media (json:"-");
//     a imagem é servida por GET /webhooks/:webhook_id/avatar.
//   - Um external_author informado no envio de mensagem sobrescreve essa
//     identidade apenas para aquela mensagem (atribuição temporária).
type Webhook struct {
	ID          string     `json:"id"`
	ChannelID   string     `json:"channel_id"`
	Name        string     `json:"name"`
	DisplayName *string    `json:"display_name,omitempty"`
	AvatarMedia *string    `json:"-"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

// ExternalAuthor é uma identidade de exibição atribuída a uma mensagem
// recebida por webhook. NÃO é um usuário do Papo e nunca determina
// autorização. AvatarURL pode ser uma URL absoluta externa (ex.: CDN de
// origem) ou um caminho do próprio backend (avatar persistente do webhook,
// ex.: /webhooks/<id>/avatar).
type ExternalAuthor struct {
	Source     string `json:"source"`
	ID         string `json:"id"`
	Name       string `json:"name"`
	AvatarURL  string `json:"avatar_url,omitempty"`
	ProfileURL string `json:"profile_url,omitempty"`
}
