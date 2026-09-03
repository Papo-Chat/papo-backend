package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"papo/internal/models"
)

// RefreshGracePeriod é a janela de graça da rotação de token de sessão: um
// token recém-substituído continua aceito por este período, para que pedidos
// concorrentes do mesmo usuário (F5 simultâneo em várias abas) não percam a
// sessão. Reapresentar o token substituído FORA desta janela é reuso
// (suspeita de sequestro de cookie).
const RefreshGracePeriod = time.Minute

// ErrConnectionReplaced indica que o token já foi substituído por outra
// rotação (a corrida pela rotação atômica foi perdida).
var ErrConnectionReplaced = errors.New("conexão de token já substituída")

// ErrConnectionReuse indica que um token já substituído foi reapresentado
// fora da janela de graça (reuso de token).
var ErrConnectionReuse = errors.New("reuso de token de sessão")

const userConnectionColumns = "id, user_id, token_hash, token_issued_at, replaced_at, replaced_by, created_at"

func scanUserConnection(row rowScanner) (models.UserConnection, error) {
	var conn models.UserConnection
	err := row.Scan(
		&conn.ID,
		&conn.UserID,
		&conn.TokenHash,
		&conn.TokenIssuedAt,
		&conn.ReplacedAt,
		&conn.ReplacedBy,
		&conn.CreatedAt,
	)
	if err != nil {
		return models.UserConnection{}, err
	}

	return conn, nil
}

// CreateUserConnection registra uma nova conexão de autenticação do usuário
// (login). O banco guarda somente o hash SHA-256 do token; issuedAt é o iat
// do token. Quando o mesmo token já existe como conexão ativa (dois logins no
// mesmo segundo produzem o mesmo JWT), retorna a conexão existente.
func CreateUserConnection(ctx context.Context, userID, tokenHash string, issuedAt time.Time) (models.UserConnection, error) {
	conn, err := scanUserConnection(GetDB().QueryRowContext(ctx,
		"INSERT INTO user_connections (user_id, token_hash, token_issued_at) VALUES ($1, $2, $3) RETURNING "+userConnectionColumns,
		userID, tokenHash, issuedAt))
	if err != nil {
		if errors.Is(err, ErrUniqueViolation) {
			// Mesmo token já registrado como conexão ativa: idempotente.
			return GetUserConnectionByHash(ctx, userID, tokenHash)
		}
		return models.UserConnection{}, mapStorageError(err)
	}

	return conn, nil
}

// GetUserConnectionByHash busca a conexão (ativa ou substituída) pelo hash do
// token, limitada ao usuário. Retorna ErrNotFound quando o token não é uma
// conexão registrada do usuário.
func GetUserConnectionByHash(ctx context.Context, userID, tokenHash string) (models.UserConnection, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+userConnectionColumns+" FROM user_connections WHERE user_id = $1 AND token_hash = $2",
		userID, tokenHash,
	)

	conn, err := scanUserConnection(row)
	if err != nil {
		return models.UserConnection{}, mapStorageError(err)
	}

	return conn, nil
}

// GetUserConnectionByID busca a conexão pelo id (seguimento da cadeia de
// substituições). Retorna ErrNotFound quando não existe.
func GetUserConnectionByID(ctx context.Context, id string) (models.UserConnection, error) {
	row := GetDB().QueryRowContext(ctx,
		"SELECT "+userConnectionColumns+" FROM user_connections WHERE id = $1",
		id,
	)

	conn, err := scanUserConnection(row)
	if err != nil {
		return models.UserConnection{}, mapStorageError(err)
	}

	return conn, nil
}

// GetUserConnectionHistory busca a conexão na tabela de history (tokens
// substituídos arquivados pela manutenção). Retorna (false, nil, nil) quando
// o token não existe na history. replacedBy permite distinguir substituição
// por rotação (não nil) de revogação (nil).
func GetUserConnectionHistory(ctx context.Context, userID, tokenHash string) (bool, *string, error) {
	var replacedBy *string
	err := GetDB().QueryRowContext(ctx,
		"SELECT replaced_by FROM user_connections_history WHERE user_id = $1 AND token_hash = $2 LIMIT 1",
		userID, tokenHash,
	).Scan(&replacedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, fmt.Errorf("falha ao consultar o histórico de conexões: %w", err)
	}

	return true, replacedBy, nil
}

// CheckUserConnection valida o estado de um token de sessão no banco (auth
// híbrida: a assinatura do JWT é checada antes, na camada de middleware).
// Retorna nil quando o token é a conexão ativa do usuário ou foi substituído
// por rotação e ainda está dentro da janela de graça.
// Retorna ErrConnectionReuse quando um token substituído por rotação é
// reapresentado fora da janela (na tabela ativa ou na history) — suspeita de
// sequestro de cookie.
// Retorna ErrNotFound quando o token não é uma conexão conhecida ou foi
// revogado (logout, drop de conexão, troca de senha): a sessão terminou e a
// recusa simples é a resposta correta (não é violação).
func CheckUserConnection(ctx context.Context, userID, tokenHash string) error {
	conn, err := GetUserConnectionByHash(ctx, userID, tokenHash)
	if errors.Is(err, ErrNotFound) {
		inHistory, replacedBy, herr := GetUserConnectionHistory(ctx, userID, tokenHash)
		if herr != nil {
			return herr
		}
		if inHistory && replacedBy != nil {
			return ErrConnectionReuse
		}
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	if conn.ReplacedAt == nil {
		return nil // conexão ativa
	}
	if conn.ReplacedBy == nil {
		// Revogado (logout / drop / troca de senha): sem janela de graça.
		return ErrNotFound
	}
	if time.Since(*conn.ReplacedAt) <= RefreshGracePeriod {
		return nil // janela de graça após rotação
	}

	return ErrConnectionReuse
}

// RotateUserConnection executa a rotação de token atomicamente: substitui a
// conexão do token antigo (replaced_at = now) e cria a conexão do token novo,
// ligando as duas (replaced_by). Se o token antigo já tiver sido substituído
// (rotação concorrente ganhou a corrida) ou se o token novo colidir com uma
// conexão existente (mesma segunda), a transação é abortada e retorna
// ErrConnectionReplaced.
func RotateUserConnection(ctx context.Context, userID, oldHash, newHash string, newIssuedAt time.Time) (models.UserConnection, error) {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return models.UserConnection{}, fmt.Errorf("falha ao rotacionar a conexão: %w", err)
	}
	defer tx.Rollback()

	var oldID string
	err = tx.QueryRowContext(ctx,
		"UPDATE user_connections SET replaced_at = now() WHERE user_id = $1 AND token_hash = $2 AND replaced_at IS NULL RETURNING id",
		userID, oldHash,
	).Scan(&oldID)
	if errors.Is(err, sql.ErrNoRows) {
		return models.UserConnection{}, ErrConnectionReplaced
	}
	if err != nil {
		return models.UserConnection{}, fmt.Errorf("falha ao rotacionar a conexão: %w", err)
	}

	newConn, err := scanUserConnection(tx.QueryRowContext(ctx,
		"INSERT INTO user_connections (user_id, token_hash, token_issued_at) VALUES ($1, $2, $3) RETURNING "+userConnectionColumns,
		userID, newHash, newIssuedAt))
	if err != nil {
		if errors.Is(err, ErrUniqueViolation) {
			// Token novo colidiu com uma conexão existente (rotação na mesma
			// segunda): trata como corrida perdida.
			return models.UserConnection{}, ErrConnectionReplaced
		}
		return models.UserConnection{}, fmt.Errorf("falha ao rotacionar a conexão: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE user_connections SET replaced_by = $2 WHERE id = $1",
		oldID, newConn.ID,
	); err != nil {
		return models.UserConnection{}, fmt.Errorf("falha ao rotacionar a conexão: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return models.UserConnection{}, fmt.Errorf("falha ao rotacionar a conexão: %w", err)
	}

	return newConn, nil
}

// RevokeUserConnection revoga UMA conexão ativa do usuário (logout, drop de
// conexão). Retorna ErrNotFound quando a conexão não existe, pertence a outro
// usuário ou já foi substituída.
func RevokeUserConnection(ctx context.Context, userID, connectionID string) error {
	result, err := GetDB().ExecContext(ctx,
		"UPDATE user_connections SET replaced_at = now() WHERE id = $1 AND user_id = $2 AND replaced_at IS NULL",
		connectionID, userID,
	)
	if err != nil {
		return fmt.Errorf("falha ao revogar a conexão: %w", err)
	}

	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	return nil
}

// RevokeAllUserConnections revoga todas as conexões ativas do usuário
// (reuso de token, drop de todas as conexões, troca de senha). Retorna o
// número de conexões revogadas.
func RevokeAllUserConnections(ctx context.Context, userID string) (int, error) {
	result, err := GetDB().ExecContext(ctx,
		"UPDATE user_connections SET replaced_at = now() WHERE user_id = $1 AND replaced_at IS NULL",
		userID,
	)
	if err != nil {
		return 0, fmt.Errorf("falha ao revogar as conexões do usuário: %w", err)
	}

	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("falha ao revogar as conexões do usuário: %w", err)
	}

	return int(n), nil
}

// HandleConnectionReuse é o efeito colateral atômico da detecção de reuso de
// token: revoga todas as conexões ativas do usuário e marca
// users.connection_violation = TRUE (o cliente usa a flag para avisar o
// usuário). Retorna o número de conexões revogadas.
func HandleConnectionReuse(ctx context.Context, userID string) (int, error) {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("falha ao tratar o reuso de token: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx,
		"UPDATE user_connections SET replaced_at = now() WHERE user_id = $1 AND replaced_at IS NULL",
		userID,
	)
	if err != nil {
		return 0, fmt.Errorf("falha ao tratar o reuso de token: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE users SET connection_violation = TRUE WHERE id = $1",
		userID,
	); err != nil {
		return 0, fmt.Errorf("falha ao tratar o reuso de token: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("falha ao tratar o reuso de token: %w", err)
	}

	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("falha ao tratar o reuso de token: %w", err)
	}

	return int(n), nil
}

// ListUserConnections retorna as conexões ativas do usuário (replaced_at IS
// NULL), mais antigas primeiro.
func ListUserConnections(ctx context.Context, userID string) ([]models.UserConnection, error) {
	rows, err := GetDB().QueryContext(ctx,
		"SELECT "+userConnectionColumns+" FROM user_connections WHERE user_id = $1 AND replaced_at IS NULL ORDER BY created_at, id",
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar as conexões do usuário: %w", err)
	}
	defer rows.Close()

	conns := make([]models.UserConnection, 0)
	for rows.Next() {
		conn, err := scanUserConnection(rows)
		if err != nil {
			return nil, fmt.Errorf("falha ao ler a conexão: %w", err)
		}
		conns = append(conns, conn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar as conexões do usuário: %w", err)
	}

	return conns, nil
}

// ListUsersWithActiveConnections retorna o conjunto de users com ao menos
// uma conexão de autenticação ativa entre os ids informados (revalidação das
// sessões dos clientes WebSocket).
func ListUsersWithActiveConnections(ctx context.Context, userIDs []string) (map[string]bool, error) {
	if len(userIDs) == 0 {
		return map[string]bool{}, nil
	}

	rows, err := GetDB().QueryContext(ctx,
		"SELECT DISTINCT user_id FROM user_connections WHERE user_id = ANY($1) AND replaced_at IS NULL",
		userIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao revalidar as conexões ativas: %w", err)
	}
	defer rows.Close()

	active := make(map[string]bool, len(userIDs))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("falha ao revalidar as conexões ativas: %w", err)
		}
		active[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao revalidar as conexões ativas: %w", err)
	}

	return active, nil
}

// MoveUserConnectionsToHistory move para a history as conexões substituídas
// com replaced_at anterior ao cutoff (arquivamento pela manutenção). A
// movimentação é atômica (CTE DELETE ... RETURNING + INSERT).
func MoveUserConnectionsToHistory(ctx context.Context, cutoff time.Time) (int, error) {
	result, err := GetDB().ExecContext(ctx,
		`WITH moved AS (
			DELETE FROM user_connections
			WHERE replaced_at IS NOT NULL AND replaced_at < $1
			RETURNING id, user_id, token_hash, token_issued_at, replaced_at, replaced_by, created_at
		)
		INSERT INTO user_connections_history (id, user_id, token_hash, token_issued_at, replaced_at, replaced_by, created_at)
		SELECT id, user_id, token_hash, token_issued_at, replaced_at, replaced_by, created_at FROM moved`,
		cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("falha ao arquivar as conexões: %w", err)
	}

	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("falha ao arquivar as conexões: %w", err)
	}

	return int(n), nil
}

// PurgeUserConnectionHistory remove da history as conexões com replaced_at
// anterior ao cutoff. Após o cutoff, o JWT correspondente já expirou e o
// histórico não serve mais para detecção de reuso.
func PurgeUserConnectionHistory(ctx context.Context, cutoff time.Time) (int, error) {
	result, err := GetDB().ExecContext(ctx,
		"DELETE FROM user_connections_history WHERE replaced_at < $1",
		cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("falha ao purgar o histórico de conexões: %w", err)
	}

	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("falha ao purgar o histórico de conexões: %w", err)
	}

	return int(n), nil
}
