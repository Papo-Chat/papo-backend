package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"papo/internal/models"
)

// AuditLogParams agrupa os filtros de GET /admin/audit-logs.
// Since/Until delimitam o intervalo de created_at (inclusive). LastID é o
// cursor de paginação (id do último item da página anterior), combinado com o
// created_at desse item para a paginação keyset (created_at, id) DESC.
type AuditLogParams struct {
	Action     string
	ActorID    string
	EntityType string
	Since      *time.Time
	Until      *time.Time
	LastID     string
	Limit      int
}

const auditLogColumns = "id, actor_id, actor_username, action, entity_type, entity_id, " +
	"target_user_id, metadata, ip_address, user_agent, created_at"

func scanAuditLog(row rowScanner) (models.AuditLog, error) {
	var log models.AuditLog
	var metadata []byte
	err := row.Scan(
		&log.ID,
		&log.ActorID,
		&log.ActorUsername,
		&log.Action,
		&log.EntityType,
		&log.EntityID,
		&log.TargetUserID,
		&metadata,
		&log.IPAddress,
		&log.UserAgent,
		&log.CreatedAt,
	)
	if err != nil {
		return models.AuditLog{}, err
	}

	log.Metadata = map[string]any{}
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &log.Metadata); err != nil {
			return models.AuditLog{}, fmt.Errorf("falha ao decodificar metadata de auditoria: %w", err)
		}
	}

	return log, nil
}

// GetUsernameByID retorna o username de um usuário (para o snapshot
// actor_username). Retorna ErrNotFound se o usuário não existir.
func GetUsernameByID(ctx context.Context, id string) (string, error) {
	var username string
	err := GetDB().QueryRowContext(ctx, "SELECT username FROM users WHERE id = $1", id).Scan(&username)
	if err != nil {
		return "", mapStorageError(err)
	}
	return username, nil
}

// InsertAuditLog insere um registro de auditoria (append-only).
func InsertAuditLog(ctx context.Context, log models.AuditLog) error {
	metadataJSON, err := json.Marshal(log.Metadata)
	if err != nil {
		return fmt.Errorf("falha ao codificar metadata de auditoria: %w", err)
	}

	_, err = GetDB().ExecContext(ctx,
		`INSERT INTO audit_logs (actor_id, actor_username, action, entity_type, entity_id,
		 target_user_id, metadata, ip_address, user_agent)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		log.ActorID, log.ActorUsername, log.Action, log.EntityType, log.EntityID,
		log.TargetUserID, string(metadataJSON), log.IPAddress, log.UserAgent,
	)
	if err != nil {
		return mapStorageError(err)
	}
	return nil
}

// ListAuditLogs lista registros de auditoria com filtros e paginação
// cursor-based (created_at, id) DESC. O LIMIT é limit+1 para o chamador
// determinar has_more. Se LastID não existir (cursor stale), retorna vazio.
func ListAuditLogs(ctx context.Context, p AuditLogParams) ([]models.AuditLog, error) {
	if p.Limit <= 0 || p.Limit > 100 {
		p.Limit = 100
	}
	fetch := p.Limit + 1

	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}

	var lastCreatedAt *time.Time
	if p.LastID != "" {
		if err := GetDB().QueryRowContext(ctx,
			"SELECT created_at FROM audit_logs WHERE id = $1", p.LastID).Scan(&lastCreatedAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return []models.AuditLog{}, nil
			}
			return nil, mapStorageError(err)
		}
	}

	conds := make([]string, 0, 8)
	if p.Action != "" {
		conds = append(conds, "action = "+arg(p.Action))
	}
	if p.ActorID != "" {
		conds = append(conds, "actor_id = "+arg(p.ActorID))
	}
	if p.EntityType != "" {
		conds = append(conds, "entity_type = "+arg(p.EntityType))
	}
	if p.Since != nil {
		conds = append(conds, "created_at >= "+arg(*p.Since))
	}
	if p.Until != nil {
		conds = append(conds, "created_at <= "+arg(*p.Until))
	}
	if lastCreatedAt != nil {
		cursorTimeArg := arg(*lastCreatedAt)
		lastIDArg := arg(p.LastID)
		conds = append(conds, "(created_at < "+cursorTimeArg+
			" OR (created_at = "+cursorTimeArg+" AND id < "+lastIDArg+"))")
	}

	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	query := "SELECT " + auditLogColumns + " FROM audit_logs" + where +
		" ORDER BY created_at DESC, id DESC LIMIT " + arg(fetch)

	rows, err := GetDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar auditoria: %w", err)
	}
	defer rows.Close()

	logs := make([]models.AuditLog, 0, fetch)
	for rows.Next() {
		log, err := scanAuditLog(rows)
		if err != nil {
			return nil, fmt.Errorf("falha ao ler registro de auditoria: %w", err)
		}
		logs = append(logs, log)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("falha ao listar auditoria: %w", err)
	}

	return logs, nil
}

// auditLogsTriggerName é o nome da trigger que torna audit_logs append-only
// (migration 004).
const auditLogsTriggerName = "prevent_audit_logs_modification"

// AuditTriggerExists indica se a trigger append-only de audit_logs está
// instalada.
func AuditTriggerExists(ctx context.Context) (bool, error) {
	var exists bool
	err := GetDB().QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = 'audit_logs'::regclass AND tgname = $1)",
		auditLogsTriggerName,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("falha ao verificar trigger de auditoria: %w", err)
	}
	return exists, nil
}

// DropAuditLogsTrigger remove a trigger append-only de audit_logs (usada pelo
// purge de retenção; deve ser recriada logo em seguida).
func DropAuditLogsTrigger(ctx context.Context) error {
	if _, err := GetDB().ExecContext(ctx,
		"DROP TRIGGER IF EXISTS "+auditLogsTriggerName+" ON audit_logs"); err != nil {
		return fmt.Errorf("falha ao remover trigger de auditoria: %w", err)
	}
	return nil
}

// CreateAuditLogsTrigger reinstala a trigger append-only de audit_logs (mesma
// definição da migration 004; a função já existe no banco).
func CreateAuditLogsTrigger(ctx context.Context) error {
	if _, err := GetDB().ExecContext(ctx,
		"CREATE TRIGGER "+auditLogsTriggerName+" BEFORE UPDATE OR DELETE ON audit_logs "+
			"FOR EACH ROW EXECUTE FUNCTION "+auditLogsTriggerName+"()"); err != nil {
		return fmt.Errorf("falha ao recriar trigger de auditoria: %w", err)
	}
	return nil
}

// PurgeAuditLogsBatch remove até limit logs de auditoria com created_at
// anterior ao cutoff e retorna a quantidade removida (0 = nada a remover).
// (PostgreSQL não suporta DELETE ... LIMIT: o lote é selecionado em um
// subselect e o DELETE apaga por id.)
func PurgeAuditLogsBatch(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	res, err := GetDB().ExecContext(ctx,
		"DELETE FROM audit_logs WHERE id IN (SELECT id FROM audit_logs WHERE created_at < $1 ORDER BY created_at LIMIT $2)",
		cutoff, limit,
	)
	if err != nil {
		return 0, fmt.Errorf("falha ao purgar auditoria: %w", err)
	}
	return res.RowsAffected()
}
