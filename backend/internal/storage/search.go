package storage

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"papo/internal/models"
)

// SearchParams agrupa os filtros da busca de mensagens (POST /search).
// Text usa o tsvector 'portuguese' da coluna tsv_content (full-text search).
// DateEndExclusive já é o limite superior exclusivo (dia de date_end + 1 dia,
// calculado pelo serviço para manter date_end inclusivo).
// Since/LastID formam o cursor de paginação (created_at, id) e exigem os dois.
type SearchParams struct {
	UserID             string
	Text               string
	AuthorID           string
	DateStart          *time.Time
	DateEndExclusive   *time.Time
	ContainsAttachment *bool
	Since              *time.Time
	LastID             string
	OrderAsc           bool
	Limit              int
}

// SearchMessages busca mensagens com full-text search e filtros combináveis,
// retornando apenas mensagens de canais legíveis pelo usuário: dono do
// servidor, canais abertos (sem permissões definidas) ou canais em que uma
// das suas roles tem read_channel.
// A ordenação é por created_at, id (asc ou desc); quando há texto, o score
// (ts_rank) é retornado por resultado. O LIMIT é limit+1 para o chamador
// determinar has_more.
func SearchMessages(ctx context.Context, p SearchParams) ([]models.SearchResult, error) {
	if p.Limit <= 0 || p.Limit > 100 {
		p.Limit = 100
	}
	fetch := p.Limit + 1

	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}

	userID := arg(p.UserID)

	var scoreExpr, textArg string
	if p.Text != "" {
		textArg = arg(p.Text)
		scoreExpr = "ts_rank(m.tsv_content, plainto_tsquery('portuguese', " + textArg + "))"
	} else {
		scoreExpr = "NULL::float8"
	}

	conds := make([]string, 0, 8)
	if textArg != "" {
		conds = append(conds, "m.tsv_content @@ plainto_tsquery('portuguese', "+textArg+")")
	}
	if p.AuthorID != "" {
		conds = append(conds, "m.author_id = "+arg(p.AuthorID))
	}
	if p.DateStart != nil {
		conds = append(conds, "m.created_at >= "+arg(*p.DateStart))
	}
	if p.DateEndExclusive != nil {
		conds = append(conds, "m.created_at < "+arg(*p.DateEndExclusive))
	}
	if p.ContainsAttachment != nil {
		if *p.ContainsAttachment {
			conds = append(conds, "EXISTS (SELECT 1 FROM attachments a WHERE a.messages_id = m.id)")
		} else {
			conds = append(conds, "NOT EXISTS (SELECT 1 FROM attachments a WHERE a.messages_id = m.id)")
		}
	}
	if p.Since != nil {
		sinceArg := arg(*p.Since)
		lastIDArg := arg(p.LastID)
		if p.OrderAsc {
			conds = append(conds, "(m.created_at > "+sinceArg+" OR (m.created_at = "+sinceArg+" AND m.id > "+lastIDArg+"))")
		} else {
			conds = append(conds, "(m.created_at < "+sinceArg+" OR (m.created_at = "+sinceArg+" AND m.id < "+lastIDArg+"))")
		}
	}

	conds = append(conds, "( "+
		"EXISTS (SELECT 1 FROM servers s WHERE s.owner_id = "+userID+") "+
		"OR c.permissions IS NULL "+
		"OR c.permissions = '{}'::jsonb "+
		"OR EXISTS (SELECT 1 FROM user_roles ur "+
		"JOIN roles r ON r.id = ur.role_id "+
		"WHERE ur.user_id = "+userID+" AND (c.permissions -> r.id::text ->> 'read_channel') = 'true') "+
		")")

	order := "DESC"
	if p.OrderAsc {
		order = "ASC"
	}

	query := "SELECT m.id, m.content, m.created_at, c.id, c.name, m.author_id, u.username, " +
		scoreExpr + " AS score " +
		"FROM messages m " +
		"JOIN channels c ON c.id = m.channel_id " +
		"LEFT JOIN users u ON u.id = m.author_id " +
		"WHERE " + strings.Join(conds, " AND ") +
		" ORDER BY m.created_at " + order + ", m.id " + order + " LIMIT " + arg(fetch)

	rows, err := GetDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("falha ao buscar mensagens: %w", err)
	}
	defer rows.Close()

	results := make([]models.SearchResult, 0, fetch)
	for rows.Next() {
		var result models.SearchResult
		if err := rows.Scan(
			&result.ID,
			&result.Content,
			&result.CreatedAt,
			&result.ChannelID,
			&result.ChannelName,
			&result.AuthorID,
			&result.AuthorUsername,
			&result.Score,
		); err != nil {
			return nil, fmt.Errorf("falha ao ler resultado da busca: %w", err)
		}
		result.Type = "message"
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao buscar mensagens: %w", err)
	}

	return results, nil
}
