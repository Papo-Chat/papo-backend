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

// maxSessionTokenAttempts limita as tentativas de emissão do token de sessão
// em caso de colisão de iat (dois logins do mesmo usuário no mesmo segundo
// produzem o mesmo JWT e colidem na UNIQUE(user_id, token_hash)).
const maxSessionTokenAttempts = 5

// CreateSessionConnection emite o token de sessão do login e registra a
// conexão de sessão correspondente. O token é uma função pura de
// (user_id, iat, segredo): dois logins no mesmo segundo produzem o mesmo
// token e colidem. Ao colidir, o iat avança um segundo e o token é re-emitido
// (o login "espera" um iat livre), garantindo que cada conexão tenha um token
// único. Retorna o token emitido e a conexão registrada.
func CreateSessionConnection(ctx context.Context, userID string) (string, ConnectionInfo, error) {
	cfg := config.LoadConfig()
	baseIssuedAt := time.Now()

	for attempt := 0; attempt < maxSessionTokenAttempts; attempt++ {
		issuedAt := baseIssuedAt.Add(time.Duration(attempt) * time.Second)
		token, err := utils.GenerateSessionToken(userID, issuedAt, cfg.JWTSecret)
		if err != nil {
			return "", ConnectionInfo{}, fmt.Errorf("falha ao gerar o token de sessão: %w", err)
		}

		conn, err := storage.CreateUserConnection(ctx, userID, utils.HashToken(token), issuedAt)
		if err != nil {
			if errors.Is(err, storage.ErrConnectionExists) {
				continue // iat colidiu: avança um segundo e tenta de novo
			}
			return "", ConnectionInfo{}, fmt.Errorf("falha ao registrar a conexão de sessão: %w", err)
		}

		return token, ConnectionInfoFrom(conn), nil
	}

	return "", ConnectionInfo{}, errors.New("falha ao registrar a conexão de sessão: colisões repetidas de token")
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

	// O token novo é uma função pura de (user_id, iat): se cair no mesmo
	// segundo do token antigo (ou de outra conexão), colide na UNIQUE e a
	// rotação é abortada. Ao colidir, o iat avança um segundo e a rotação é
	// tentada de novo — garantindo que o token novo seja sempre distinto.
	baseIssuedAt := time.Now()
	for attempt := 0; attempt < maxSessionTokenAttempts; attempt++ {
		newIssuedAt := baseIssuedAt.Add(time.Duration(attempt) * time.Second)
		newToken, err := utils.GenerateSessionToken(userID, newIssuedAt, cfg.JWTSecret)
		if err != nil {
			return "", ConnectionInfo{}, fmt.Errorf("falha ao gerar o novo token: %w", err)
		}

		newConn, err := storage.RotateUserConnection(ctx, userID, utils.HashToken(oldToken), utils.HashToken(newToken), newIssuedAt)
		if err == nil {
			return newToken, ConnectionInfoFrom(newConn), nil
		}
		if !errors.Is(err, storage.ErrConnectionReplaced) {
			return "", ConnectionInfo{}, err
		}

		// ErrConnectionReplaced: ou a rotação concorrente ganhou (o token
		// antigo já foi substituído) ou o token novo colidiu (mesma segunda,
		// transação abortada). Distingue pelo estado atual do token antigo.
		latest, lerr := storage.GetUserConnectionByHash(ctx, userID, utils.HashToken(oldToken))
		if lerr != nil {
			return "", ConnectionInfo{}, lerr
		}
		if latest.ReplacedAt != nil {
			// Rotação concorrente ganhou a corrida: serve a ponta da cadeia.
			return tipConnection(ctx, latest, cfg.JWTSecret)
		}
		// Token novo colidiu (transação abortada): avança o iat e tenta de novo.
	}

	return "", ConnectionInfo{}, errors.New("falha ao rotacionar o token de sessão: colisões repetidas de token")
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
