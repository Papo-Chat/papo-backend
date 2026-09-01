package services

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"papo/internal/models"
	"papo/internal/storage"
)

// fetchAuditLogs executa a query com a cláusula WHERE já montada e devolve as
// linhas de audit_logs na ordem das colunas fixas (mesma ordem de auditLogColumns).
func fetchAuditLogs(t *testing.T, where string, args ...any) []models.AuditLog {
	t.Helper()
	query := "SELECT id, actor_id, actor_username, action, entity_type, entity_id, " +
		"target_user_id, metadata, ip_address, user_agent, created_at FROM audit_logs"
	if where != "" {
		query += " WHERE " + where
	}
	query += " ORDER BY created_at DESC, id DESC"

	rows, err := storage.GetDB().QueryContext(testCtx(), query, args...)
	if err != nil {
		t.Fatalf("falha ao consultar audit_logs: %v", err)
	}
	defer rows.Close()

	logs := make([]models.AuditLog, 0)
	for rows.Next() {
		var log models.AuditLog
		var metadata []byte
		if err := rows.Scan(&log.ID, &log.ActorID, &log.ActorUsername, &log.Action,
			&log.EntityType, &log.EntityID, &log.TargetUserID, &metadata,
			&log.IPAddress, &log.UserAgent, &log.CreatedAt); err != nil {
			t.Fatalf("falha ao ler audit_log: %v", err)
		}
		log.Metadata = map[string]any{}
		if len(metadata) > 0 {
			if err := json.Unmarshal(metadata, &log.Metadata); err != nil {
				t.Fatalf("falha ao decodificar metadata: %v", err)
			}
		}
		logs = append(logs, log)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("falha ao iterar audit_logs: %v", err)
	}
	return logs
}

func insertAuditLog(t *testing.T, log models.AuditLog) {
	t.Helper()
	if err := storage.InsertAuditLog(testCtx(), log); err != nil {
		t.Fatalf("InsertAuditLog: %v", err)
	}
}

// --- storage ---

func TestInsertAuditLogAndReadBack(t *testing.T) {
	actor := testActorID()
	entityID := randUUID()
	action := "audit.test.insert." + randHex(6)

	insertAuditLog(t, models.AuditLog{
		ActorID:       &actor,
		ActorUsername: "snapshot-user",
		Action:        action,
		EntityType:    "user",
		EntityID:      &entityID,
		Metadata:      map[string]any{"chave": "valor"},
	})

	logs := fetchAuditLogs(t, "action = $1", action)
	if len(logs) != 1 {
		t.Fatalf("esperava 1 log, obtive %d", len(logs))
	}
	log := logs[0]
	if *log.ActorID != actor {
		t.Errorf("actor_id = %s, esperado %s", *log.ActorID, actor)
	}
	if log.ActorUsername != "snapshot-user" {
		t.Errorf("actor_username = %q, esperado %q", log.ActorUsername, "snapshot-user")
	}
	if *log.EntityID != entityID {
		t.Errorf("entity_id = %s, esperado %s", *log.EntityID, entityID)
	}
	if log.Metadata["chave"] != "valor" {
		t.Errorf("metadata[chave] = %v, esperado %q", log.Metadata["chave"], "valor")
	}
	if log.IPAddress != nil || log.UserAgent != nil {
		t.Errorf("esperava ip_address/user_agent nulos, obtive %v/%v", log.IPAddress, log.UserAgent)
	}
}

func TestStorageListAuditLogsFilters(t *testing.T) {
	actor := testActorID()
	suffix := randHex(6)
	aUser := "audit.filter.a." + suffix
	aChan := "audit.filter.b." + suffix

	insertAuditLog(t, models.AuditLog{ActorID: &actor, ActorUsername: "u", Action: aUser, EntityType: "user"})
	insertAuditLog(t, models.AuditLog{ActorID: &actor, ActorUsername: "u", Action: aChan, EntityType: "channel"})

	// filtro por action
	logs, err := storage.ListAuditLogs(testCtx(), storage.AuditLogParams{Action: aUser, Limit: 100})
	if err != nil {
		t.Fatalf("ListAuditLogs(action): %v", err)
	}
	if len(logs) != 1 || logs[0].Action != aUser {
		t.Errorf("filtro action: %d linhas, esperado 1", len(logs))
	}

	// filtro por entity_type (combinado com action para isolar)
	logs, err = storage.ListAuditLogs(testCtx(), storage.AuditLogParams{Action: aChan, EntityType: "channel", Limit: 100})
	if err != nil {
		t.Fatalf("ListAuditLogs(entity): %v", err)
	}
	if len(logs) != 1 || logs[0].EntityType != "channel" {
		t.Errorf("filtro entity_type: %d linhas, esperado 1", len(logs))
	}

	// entity_type incompatível → vazio
	logs, err = storage.ListAuditLogs(testCtx(), storage.AuditLogParams{Action: aChan, EntityType: "user", Limit: 100})
	if err != nil {
		t.Fatalf("ListAuditLogs(entity inválido): %v", err)
	}
	if len(logs) != 0 {
		t.Errorf("entity_type incompatível deveria ser vazio, obtive %d", len(logs))
	}

	// filtro por actor_id (combinado com action para isolar do ator compartilhado)
	logs, err = storage.ListAuditLogs(testCtx(), storage.AuditLogParams{ActorID: actor, Action: aUser, Limit: 100})
	if err != nil {
		t.Fatalf("ListAuditLogs(actor): %v", err)
	}
	if len(logs) != 1 {
		t.Errorf("filtro actor_id: %d linhas, esperado 1", len(logs))
	}
}

func TestStorageListAuditLogsSinceUntil(t *testing.T) {
	actor := testActorID()
	action := "audit.filter.range." + randHex(6)

	var first, second models.AuditLog
	insertAuditLog(t, models.AuditLog{ActorID: &actor, ActorUsername: "u", Action: action, EntityType: "user"})
	time.Sleep(15 * time.Millisecond)
	insertAuditLog(t, models.AuditLog{ActorID: &actor, ActorUsername: "u", Action: action, EntityType: "user"})

	logs := fetchAuditLogs(t, "action = $1", action)
	if len(logs) != 2 {
		t.Fatalf("esperava 2 logs, obtive %d", len(logs))
	}
	first, second = logs[1], logs[0] // logs[0] é o mais recente (DESC)

	// until = created_at do mais antigo → só o mais antigo
	until := first.CreatedAt
	logs, err := storage.ListAuditLogs(testCtx(), storage.AuditLogParams{Action: action, Until: &until, Limit: 100})
	if err != nil {
		t.Fatalf("ListAuditLogs(until): %v", err)
	}
	if len(logs) != 1 || logs[0].ID != first.ID {
		t.Errorf("filtro until: %d linhas, esperado apenas o mais antigo", len(logs))
	}

	// since = created_at do mais recente → só o mais recente
	since := second.CreatedAt
	logs, err = storage.ListAuditLogs(testCtx(), storage.AuditLogParams{Action: action, Since: &since, Limit: 100})
	if err != nil {
		t.Fatalf("ListAuditLogs(since): %v", err)
	}
	if len(logs) != 1 || logs[0].ID != second.ID {
		t.Errorf("filtro since: %d linhas, esperado apenas o mais recente", len(logs))
	}
}

func TestListAuditLogsPagination(t *testing.T) {
	actor := testActorID()
	action := "audit.page." + randHex(6)
	const total = 101
	for i := 0; i < total; i++ {
		insertAuditLog(t, models.AuditLog{ActorID: &actor, ActorUsername: "u", Action: action, EntityType: "user"})
	}

	page1, err := ListAuditLogs(testCtx(), action, "", "", nil, nil, "")
	if err != nil {
		t.Fatalf("ListAuditLogs(page1): %v", err)
	}
	if len(page1.Logs) != 100 {
		t.Fatalf("page1 = %d logs, esperado 100", len(page1.Logs))
	}
	if !page1.HasMore {
		t.Error("page1.HasMore deveria ser true")
	}

	last := page1.Logs[len(page1.Logs)-1]
	page2, err := ListAuditLogs(testCtx(), action, "", "", nil, nil, last.ID)
	if err != nil {
		t.Fatalf("ListAuditLogs(page2): %v", err)
	}
	if len(page2.Logs) != 1 {
		t.Fatalf("page2 = %d logs, esperado 1", len(page2.Logs))
	}
	if page2.HasMore {
		t.Error("page2.HasMore deveria ser false")
	}
	if page2.Logs[0].ID == last.ID {
		t.Error("page2 repetiu o último log da page1")
	}

	// cursor inexistente (stale) → página vazia, sem erro
	page3, err := ListAuditLogs(testCtx(), action, "", "", nil, nil, randUUID())
	if err != nil {
		t.Fatalf("ListAuditLogs(cursor stale): %v", err)
	}
	if len(page3.Logs) != 0 {
		t.Errorf("cursor inexistente deveria retornar vazio, obtive %d", len(page3.Logs))
	}
}

func TestAuditLogsAppendOnly(t *testing.T) {
	actor := testActorID()
	action := "audit.appendonly." + randHex(6)
	insertAuditLog(t, models.AuditLog{ActorID: &actor, ActorUsername: "u", Action: action, EntityType: "user"})

	logs := fetchAuditLogs(t, "action = $1", action)
	if len(logs) != 1 {
		t.Fatalf("esperava 1 log, obtive %d", len(logs))
	}
	rowID := logs[0].ID

	if _, err := storage.GetDB().ExecContext(testCtx(),
		"UPDATE audit_logs SET actor_username = 'x' WHERE id = $1", rowID); err == nil {
		t.Error("UPDATE em audit_logs deveria ser bloqueado pelo trigger")
	}
	if _, err := storage.GetDB().ExecContext(testCtx(),
		"DELETE FROM audit_logs WHERE id = $1", rowID); err == nil {
		t.Error("DELETE em audit_logs deveria ser bloqueado pelo trigger")
	}
}

func TestGetUsernameByID(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	username, err := storage.GetUsernameByID(testCtx(), user.ID)
	if err != nil {
		t.Fatalf("GetUsernameByID: %v", err)
	}
	if username != user.Username {
		t.Errorf("username = %q, esperado %q", username, user.Username)
	}

	if _, err := storage.GetUsernameByID(testCtx(), randUUID()); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("esperava ErrNotFound para id inexistente, obtive %v", err)
	}
}

// --- services (RecordAudit + operações) ---

func TestRecordAudit(t *testing.T) {
	actor, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	entityID := randUUID()
	action := "audit.record." + randHex(6)

	RecordAudit(testCtx(), AuditEntry{
		ActorID:    actor.ID,
		Action:     action,
		EntityType: "user",
		EntityID:   &entityID,
		Metadata:   map[string]any{"foo": "bar"},
	})

	logs := fetchAuditLogs(t, "action = $1", action)
	if len(logs) != 1 {
		t.Fatalf("esperava 1 log, obtive %d", len(logs))
	}
	log := logs[0]
	if log.ActorUsername != actor.Username {
		t.Errorf("snapshot actor_username = %q, esperado %q", log.ActorUsername, actor.Username)
	}
	if *log.ActorID != actor.ID {
		t.Errorf("actor_id = %s, esperado %s", *log.ActorID, actor.ID)
	}
	if log.Metadata["foo"] != "bar" {
		t.Errorf("metadata[foo] = %v, esperado %q", log.Metadata["foo"], "bar")
	}
	if log.IPAddress != nil || log.UserAgent != nil {
		t.Errorf("ip_address/user_agent deveriam ser nulos (sem contexto de auditoria)")
	}
}

func TestOperationCreatesAuditLog(t *testing.T) {
	cleanServers(testCtx())
	actor := testActorID()

	if _, err := CreateServer(testCtx(), newRandomServerName(), &actor); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	target, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// user.ban
	if err := BanUser(testCtx(), actor, target.ID, true); err != nil {
		t.Fatalf("BanUser: %v", err)
	}
	logs := fetchAuditLogs(t, "action = $1 AND actor_id = $2", ActionUserBan, actor)
	if len(logs) != 1 {
		t.Fatalf("user.ban: %d logs, esperado 1", len(logs))
	}
	if logs[0].TargetUserID == nil || *logs[0].TargetUserID != target.ID {
		t.Errorf("user.ban target_user_id = %v, esperado %s", logs[0].TargetUserID, target.ID)
	}
	if logs[0].Metadata["banned"] != true {
		t.Errorf("user.ban metadata[banned] = %v, esperado true", logs[0].Metadata["banned"])
	}

	// role.create
	role, err := CreateRole(testCtx(), actor, newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	logs = fetchAuditLogs(t, "action = $1 AND entity_id = $2", ActionRoleCreate, role.ID)
	if len(logs) != 1 {
		t.Fatalf("role.create: %d logs, esperado 1", len(logs))
	}
	if logs[0].Metadata["name"] != role.Name {
		t.Errorf("role.create metadata[name] = %v, esperado %q", logs[0].Metadata["name"], role.Name)
	}

	// channel.create
	channel, err := CreateChannel(testCtx(), actor, newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	logs = fetchAuditLogs(t, "action = $1 AND entity_id = $2", ActionChannelCreate, channel.ID)
	if len(logs) != 1 {
		t.Fatalf("channel.create: %d logs, esperado 1", len(logs))
	}
	if logs[0].Metadata["type"] != "text" {
		t.Errorf("channel.create metadata[type] = %v, esperado %q", logs[0].Metadata["type"], "text")
	}
}
