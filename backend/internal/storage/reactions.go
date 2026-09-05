package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"papo/internal/models"
)

// ErrReactionLimitReached indica que a mensagem atingiu o limite de tipos de
// reação (20).
var ErrReactionLimitReached = errors.New("limite de tipos de reação atingido")

// maxReactionTypesPerMessage é o número máximo de tipos distintos de reação
// por mensagem (cada tipo pode ter quantos usuários quiser).
const maxReactionTypesPerMessage = 20

// reactionLockKeyPrefix é o prefixo da chave do advisory lock que serializa as
// adições por mensagem, evitando estourar o limite de tipos em escrita
// concorrente.
const reactionLockKeyPrefix = "papo:reactions:"

// zeroUUID é o valor de COALESCE do índice único idx_message_reactions_unique
// quando emoji_id é NULL.
const zeroUUID = "00000000-0000-0000-0000-000000000000"

const reactionColumns = "message_id, user_id, emoji_id, unicode, created_at"

// queryer é implementado por *sql.DB e *sql.Tx.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func scanReaction(row rowScanner) (models.MessageReaction, error) {
	var reaction models.MessageReaction
	err := row.Scan(
		&reaction.MessageID,
		&reaction.UserID,
		&reaction.EmojiID,
		&reaction.Unicode,
		&reaction.CreatedAt,
	)
	if err != nil {
		return models.MessageReaction{}, err
	}
	return reaction, nil
}

// countReactionType conta quantos usuários reagiram com um tipo específico de
// emoji em uma mensagem.
func countReactionType(ctx context.Context, q queryer, messageID string, emojiID, unicode *string) (int, error) {
	var count int
	err := q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM message_reactions WHERE message_id = $1 AND emoji_id IS NOT DISTINCT FROM $2 AND unicode IS NOT DISTINCT FROM $3",
		messageID, emojiID, unicode,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("falha ao contar reação: %w", err)
	}
	return count, nil
}

// AddReaction adiciona a reação de um usuário a uma mensagem. A operação é
// idempotente: se o usuário já reagiu com o mesmo emoji, o registro existente
// é retornado (created=false) sem alterar created_at. O limite de 20 tipos por
// mensagem é aplicado apenas na criação de um tipo novo; a escrita é
// serializada por um advisory lock por mensagem para evitar estourar o limite
// em escrita concorrente.
// emojiID e unicode são mutuamente exatos-um (garantido pelo service e pela
// CHECK do banco): um é o id de um emoji custom do banco e o outro é NULL.
// Retorna (reaction, created, count), onde count é o número de usuários que
// reagiram com aquele emoji após a operação. Retorna
// ErrReactionLimitReached quando a mensagem já tem 20 tipos e o emoji é um
// tipo novo.
func AddReaction(ctx context.Context, messageID, userID string, emojiID, unicode *string) (models.MessageReaction, bool, int, error) {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return models.MessageReaction{}, false, 0, fmt.Errorf("falha ao reagir: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		"SELECT pg_advisory_xact_lock(hashtext($1))",
		reactionLockKeyPrefix+messageID,
	); err != nil {
		return models.MessageReaction{}, false, 0, fmt.Errorf("falha ao reagir: %w", err)
	}

	// Idempotência: se já reagiu com o mesmo emoji, retorna o registro
	// existente.
	existing, err := scanReaction(tx.QueryRowContext(ctx,
		"SELECT "+reactionColumns+" FROM message_reactions WHERE message_id = $1 AND user_id = $2 AND emoji_id IS NOT DISTINCT FROM $3 AND unicode IS NOT DISTINCT FROM $4",
		messageID, userID, emojiID, unicode,
	))
	if err == nil {
		count, err := countReactionType(ctx, tx, messageID, emojiID, unicode)
		if err != nil {
			return models.MessageReaction{}, false, 0, err
		}
		return existing, false, count, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return models.MessageReaction{}, false, 0, mapStorageError(err)
	}

	var distinct int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM (SELECT DISTINCT COALESCE(emoji_id, '"+zeroUUID+"'::uuid), COALESCE(unicode, '') FROM message_reactions WHERE message_id = $1) tipos",
		messageID,
	).Scan(&distinct); err != nil {
		return models.MessageReaction{}, false, 0, fmt.Errorf("falha ao reagir: %w", err)
	}
	if distinct >= maxReactionTypesPerMessage {
		return models.MessageReaction{}, false, 0, ErrReactionLimitReached
	}

	reaction, err := scanReaction(tx.QueryRowContext(ctx,
		"INSERT INTO message_reactions (message_id, user_id, emoji_id, unicode) VALUES ($1, $2, $3, $4) RETURNING "+reactionColumns,
		messageID, userID, emojiID, unicode,
	))
	if err != nil {
		return models.MessageReaction{}, false, 0, mapStorageError(err)
	}

	count, err := countReactionType(ctx, tx, messageID, emojiID, unicode)
	if err != nil {
		return models.MessageReaction{}, false, 0, err
	}

	if err := tx.Commit(); err != nil {
		return models.MessageReaction{}, false, 0, fmt.Errorf("falha ao reagir: %w", err)
	}

	return reaction, true, count, nil
}

// RemoveReaction remove a reação de um usuário de uma mensagem. Retorna
// ErrNotFound quando o usuário não reagiu com aquele emoji. O count retornado
// é o número de usuários que reagiram com aquele emoji após a remoção (0
// quando era o último).
func RemoveReaction(ctx context.Context, messageID, userID string, emojiID, unicode *string) (int, error) {
	db := GetDB()
	res, err := db.ExecContext(ctx,
		"DELETE FROM message_reactions WHERE message_id = $1 AND user_id = $2 AND emoji_id IS NOT DISTINCT FROM $3 AND unicode IS NOT DISTINCT FROM $4",
		messageID, userID, emojiID, unicode,
	)
	if err != nil {
		return 0, mapStorageError(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}

	count, err := countReactionType(ctx, db, messageID, emojiID, unicode)
	if err != nil {
		return 0, err
	}
	return count, nil
}

// derefString retorna o valor de um *string ou "" quando nil.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ListReactionsByMessage lista as reações de uma mensagem agrupadas por tipo
// de emoji, em ordem decrescente de criação, paginadas como as demais
// listagens: limit reações por página (padrão e máximo 100) e has_more quando
// existe próxima página (é buscada 1 row a mais que o limite; a row extra é
// excluída da página). Se since for fornecido, retorna apenas reações criadas
// após esse timestamp; se lastID for fornecido junto, o cursor é o par
// (created_at, id) e o filtro retorna as reações anteriores ao cursor na
// ordem decrescente (created_at, id), incluindo as do mesmo timestamp com id
// menor que lastID (evita pular reações com timestamp igual). As linhas da
// página são lidas individualmente e agrupadas em Go (sem array_agg),
// seguindo a convenção do restante do storage; o count de cada grupo é o
// número de usuários do grupo na página.
func ListReactionsByMessage(ctx context.Context, messageID string, since *time.Time, lastID string, limit int) ([]models.MessageReactionGroup, bool, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	fetch := limit + 1

	query := "SELECT id, emoji_id, unicode, user_id, created_at FROM message_reactions WHERE message_id = $1"
	args := []any{messageID}

	if since != nil {
		if lastID != "" {
			query += " AND (created_at < $2 OR (created_at = $2 AND id < $3))"
			args = append(args, *since, lastID)
			query += " ORDER BY created_at DESC, id DESC LIMIT $4"
		} else {
			query += " AND created_at > $2"
			args = append(args, *since)
			query += " ORDER BY created_at DESC, id DESC LIMIT $3"
		}
	} else {
		query += " ORDER BY created_at DESC, id DESC LIMIT $2"
	}
	args = append(args, fetch)

	rows, err := GetDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("falha ao listar reações: %w", err)
	}
	defer rows.Close()

	type pageRow struct {
		id        string
		emojiID   *string
		unicode   *string
		userID    string
		createdAt time.Time
	}
	page := make([]pageRow, 0, fetch)
	for rows.Next() {
		var row pageRow
		if err := rows.Scan(&row.id, &row.emojiID, &row.unicode, &row.userID, &row.createdAt); err != nil {
			return nil, false, fmt.Errorf("falha ao ler reação: %w", err)
		}
		page = append(page, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("falha ao listar reações: %w", err)
	}

	hasMore := len(page) > limit
	if hasMore {
		page = page[:limit]
	}

	type reactionKey struct {
		emojiID string
		unicode string
	}
	groups := []models.MessageReactionGroup{}
	index := map[reactionKey]int{}
	for _, row := range page {
		key := reactionKey{emojiID: derefString(row.emojiID), unicode: derefString(row.unicode)}
		i, ok := index[key]
		if !ok {
			groups = append(groups, models.MessageReactionGroup{
				EmojiID: row.emojiID,
				Unicode: row.unicode,
				Users:   []models.MessageReactionUser{},
			})
			i = len(groups) - 1
			index[key] = i
		}
		groups[i].Users = append(groups[i].Users, models.MessageReactionUser{
			ID:        row.id,
			UserID:    row.userID,
			CreatedAt: row.createdAt,
		})
	}
	for i := range groups {
		groups[i].Count = len(groups[i].Users)
	}
	return groups, hasMore, nil
}

// UserReactionsByMessages retorna as reações de um usuário em cada mensagem
// informada (expostas como user_reactions nas respostas de mensagem). O mapa
// é populado com slice vazio para todas as mensagens informadas (o JSON
// responde [] e nunca null); a ordem é decrescente de criação.
func UserReactionsByMessages(ctx context.Context, messageIDs []string, userID string) (map[string][]models.MessageUserReaction, error) {
	reactions := make(map[string][]models.MessageUserReaction, len(messageIDs))
	for _, id := range messageIDs {
		reactions[id] = []models.MessageUserReaction{}
	}
	if len(messageIDs) == 0 {
		return reactions, nil
	}

	rows, err := GetDB().QueryContext(ctx,
		`SELECT message_id, id, emoji_id, unicode
		 FROM message_reactions WHERE message_id = ANY($1) AND user_id = $2
		 ORDER BY created_at DESC, id DESC`,
		messageIDs, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar reações do usuário: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var messageID string
		var reaction models.MessageUserReaction
		if err := rows.Scan(&messageID, &reaction.ID, &reaction.EmojiID, &reaction.Unicode); err != nil {
			return nil, fmt.Errorf("falha ao ler reação do usuário: %w", err)
		}
		reactions[messageID] = append(reactions[messageID], reaction)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar reações do usuário: %w", err)
	}
	return reactions, nil
}

// ReactionCountsByMessages retorna a contagem de tipos de reação de cada
// mensagem informada (apenas mensagens com pelo menos uma reação aparecem no
// mapa).
func ReactionCountsByMessages(ctx context.Context, messageIDs []string) (map[string][]models.MessageReactionSummary, error) {
	counts := map[string][]models.MessageReactionSummary{}
	if len(messageIDs) == 0 {
		return counts, nil
	}

	rows, err := GetDB().QueryContext(ctx,
		`SELECT message_id, emoji_id, unicode, COUNT(*)
		 FROM message_reactions WHERE message_id = ANY($1)
		 GROUP BY message_id, emoji_id, unicode`,
		messageIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao contar reações: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var messageID string
		var summary models.MessageReactionSummary
		if err := rows.Scan(&messageID, &summary.EmojiID, &summary.Unicode, &summary.Count); err != nil {
			return nil, fmt.Errorf("falha ao contar reações: %w", err)
		}
		counts[messageID] = append(counts[messageID], summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao contar reações: %w", err)
	}
	return counts, nil
}
