package models

import "time"

// UserConnection representa a tabela user_connections: uma conexão de
// autenticação do usuário (auth híbrida). O banco guarda somente o hash
// SHA-256 do token (TokenHash); o token em si é re-derivado de
// (UserID, TokenIssuedAt) quando necessário (janela de graça da rotação).
// ReplacedAt/ReplacedBy ligam a conexão substituída à substituta (cadeia de
// rotações); connection ativa tem ReplacedAt = nil.
type UserConnection struct {
	ID            string     `db:"id" json:"id"`
	UserID        string     `db:"user_id" json:"-"`
	TokenHash     string     `db:"token_hash" json:"-"`
	TokenIssuedAt time.Time  `db:"token_issued_at" json:"-"`
	ReplacedAt    *time.Time `db:"replaced_at" json:"-"`
	ReplacedBy    *string    `db:"replaced_by" json:"-"`
	CreatedAt     time.Time  `db:"created_at" json:"created_at"`
}
