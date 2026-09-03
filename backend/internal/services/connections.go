package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"
)

// Erros de negócio de conexões de sessão.
var (
	ErrConnectionNotFound = errors.New("conexão de sessão não encontrada")
	ErrInvalidConnection  = errors.New("conexão inválida")
)

// ConnectionInfo é a visão de uma conexão de sessão exposta ao cliente
// (GET /auth/connected_devices, POST /auth/refresh). ExpiresAt é o exp do
// JWT correspondente (token_issued_at + JWTExpiration).
type ConnectionInfo struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ConnectionInfoFrom converte a conexão do banco na visão do cliente.
func ConnectionInfoFrom(conn models.UserConnection) ConnectionInfo {
	return ConnectionInfo{
		ID:        conn.ID,
		CreatedAt: conn.CreatedAt,
		ExpiresAt: conn.TokenIssuedAt.Add(utils.JWTExpiration),
	}
}

// CreateConnection registra a conexão de sessão do usuário em um login.
// issuedAt é o iat do token emitido: como o token é uma função pura de
// (user_id, iat, segredo), o banco guarda apenas o hash do token e o
// token_issued_at.
func CreateConnection(ctx context.Context, userID, token string, issuedAt time.Time) error {
	if _, err := storage.CreateUserConnection(ctx, userID, utils.HashToken(token), issuedAt); err != nil {
		return fmt.Errorf("falha ao registrar a conexão de sessão: %w", err)
	}

	return nil
}

// RefreshConnection rotaciona o token de sessão do usuário: substitui
// atomicamente a conexão atual (identificada pelo token antigo) por uma nova e
// retorna o novo token e a nova conexão.
// Quando o token antigo já foi substituído (rotação concorrente dentro da
// janela de graça), retorna o token atual da ponta da cadeia sem rotacionar
// de novo.
// Retorna ErrConnectionNotFound quando o token antigo não é a conexão ativa
// do usuário.
func RefreshConnection(ctx context.Context, userID, oldToken string) (string, ConnectionInfo, error) {
	cfg := config.LoadConfig()

	conn, err := storage.GetUserConnectionByHash(ctx, userID, utils.HashToken(oldToken))
	if errors.Is(err, storage.ErrNotFound) {
		return "", ConnectionInfo{}, ErrConnectionNotFound
	}
	if err != nil {
		return "", ConnectionInfo{}, err
	}

	if conn.ReplacedAt != nil {
		// Já substituído (janela de graça): serve a ponta da cadeia.
		return tipConnection(ctx, conn, cfg.JWTSecret)
	}

	newIssuedAt := time.Now()
	newToken, err := utils.GenerateSessionToken(userID, newIssuedAt, cfg.JWTSecret)
	if err != nil {
		return "", ConnectionInfo{}, fmt.Errorf("falha ao gerar o novo token: %w", err)
	}

	newConn, err := storage.RotateUserConnection(ctx, userID, utils.HashToken(oldToken), utils.HashToken(newToken), newIssuedAt)
	if errors.Is(err, storage.ErrConnectionReplaced) {
		// Rotação concorrente ganhou a corrida: serve a ponta da cadeia.
		latest, lerr := storage.GetUserConnectionByHash(ctx, userID, utils.HashToken(oldToken))
		if lerr != nil {
			return "", ConnectionInfo{}, lerr
		}
		return tipConnection(ctx, latest, cfg.JWTSecret)
	}
	if err != nil {
		return "", ConnectionInfo{}, err
	}

	return newToken, ConnectionInfoFrom(newConn), nil
}

// tipConnection segue a cadeia de substituições de uma conexão substituída e
// re-deriva o token atual da sessão. A conexão retornada é a ponta ativa da
// cadeia.
func tipConnection(ctx context.Context, conn models.UserConnection, secret string) (string, ConnectionInfo, error) {
	for conn.ReplacedAt != nil && conn.ReplacedBy != nil {
		next, err := storage.GetUserConnectionByID(ctx, *conn.ReplacedBy)
		if errors.Is(err, storage.ErrNotFound) {
			// O sucessor saiu da tabela ativa (arquivado): nada a servir.
			return "", ConnectionInfo{}, ErrConnectionNotFound
		}
		if err != nil {
			return "", ConnectionInfo{}, err
		}
		conn = next
	}

	if conn.ReplacedAt != nil {
		// A ponta da cadeia não está mais ativa (revogada): nada a servir.
		return "", ConnectionInfo{}, ErrConnectionNotFound
	}

	token, err := utils.GenerateSessionToken(conn.UserID, conn.TokenIssuedAt, secret)
	if err != nil {
		return "", ConnectionInfo{}, fmt.Errorf("falha ao re-derivar o token: %w", err)
	}
	if utils.HashToken(token) != conn.TokenHash {
		// O token re-derivado não bate com o hash registrado (ex.: segredo
		// trocado): não servir.
		return "", ConnectionInfo{}, fmt.Errorf("falha ao re-derivar o token: hash não confere")
	}

	return token, ConnectionInfoFrom(conn), nil
}

// CurrentConnectionID retorna o id da conexão de sessão correspondente ao
// token (ativa ou já substituída), ou "" quando o token não é uma conexão
// conhecida do usuário.
func CurrentConnectionID(ctx context.Context, userID, token string) (string, error) {
	conn, err := storage.GetUserConnectionByHash(ctx, userID, utils.HashToken(token))
	if errors.Is(err, storage.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}

	return conn.ID, nil
}

// Logout revoga a conexão de sessão correspondente ao token (logout).
// Token ativo: a conexão é revogada. Token substituído por rotação fora da
// janela de graça: reuso — todas as conexões são revogadas e
// users.connection_violation é marcado. Token desconhecido ou revogado:
// nada a fazer (o cliente apenas remove o cookie).
func Logout(ctx context.Context, userID, token string) error {
	tokenHash := utils.HashToken(token)

	switch err := storage.CheckUserConnection(ctx, userID, tokenHash); {
	case err == nil:
		conn, gerr := storage.GetUserConnectionByHash(ctx, userID, tokenHash)
		if gerr != nil {
			return gerr
		}
		if conn.ReplacedAt != nil {
			// Janela de graça: já foi substituída, nada a revogar.
			return nil
		}
		return storage.RevokeUserConnection(ctx, userID, conn.ID)
	case errors.Is(err, storage.ErrConnectionReuse):
		_, rerr := storage.HandleConnectionReuse(ctx, userID)
		return rerr
	case errors.Is(err, storage.ErrNotFound):
		return nil
	default:
		return err
	}
}

// ListConnections retorna as conexões de sessão ativas do usuário.
func ListConnections(ctx context.Context, userID string) ([]ConnectionInfo, error) {
	conns, err := storage.ListUserConnections(ctx, userID)
	if err != nil {
		return nil, err
	}

	infos := make([]ConnectionInfo, 0, len(conns))
	for _, conn := range conns {
		infos = append(infos, ConnectionInfoFrom(conn))
	}

	return infos, nil
}

// DropConnection revoga conexões de sessão do usuário: o target "ALL"
// (case-insensitive) revoga todas as conexões ativas (incluindo a atual);
// caso contrário deve ser o id de uma conexão ativa. Retorna o número de
// conexões revogadas.
// Retorna ErrInvalidConnection para target malformado e
// ErrConnectionNotFound quando a conexão indicada não existe.
func DropConnection(ctx context.Context, userID, target string) (int, error) {
	if strings.EqualFold(target, "ALL") {
		n, err := storage.RevokeAllUserConnections(ctx, userID)
		if err != nil {
			return 0, err
		}

		RecordAudit(ctx, AuditEntry{
			ActorID:    userID,
			Action:     ActionAuthConnectionDrop,
			EntityType: EntityUser,
			EntityID:   &userID,
			Metadata:   map[string]any{"all": true, "dropped": n},
		})

		return n, nil
	}

	if !validConnectionID(target) {
		return 0, ErrInvalidConnection
	}

	if err := storage.RevokeUserConnection(ctx, userID, target); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return 0, ErrConnectionNotFound
		}
		return 0, err
	}

	RecordAudit(ctx, AuditEntry{
		ActorID:    userID,
		Action:     ActionAuthConnectionDrop,
		EntityType: EntityUser,
		EntityID:   &userID,
		Metadata:   map[string]any{"all": false, "connection_id": target},
	})

	return 1, nil
}

// validConnectionID valida o formato de UUID (8-4-4-4-12) sem parsing.
func validConnectionID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if r != '-' {
				return false
			}
		case (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F'):
		default:
			return false
		}
	}
	return true
}
