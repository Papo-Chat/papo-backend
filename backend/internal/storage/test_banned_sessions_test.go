package storage

import (
	"testing"
	"time"
)

func TestListUsersWithActiveConnectionsExcludesBannedUsers(t *testing.T) {
	user := newTestUser(t)
	if _, err := CreateUserConnection(
		testCtx(),
		randUUID(),
		user.ID,
		"hash_"+randHex(32),
		time.Now(),
	); err != nil {
		t.Fatalf("CreateUserConnection: %v", err)
	}

	if _, err := GetDB().ExecContext(testCtx(),
		"UPDATE users SET banned = TRUE WHERE id = $1", user.ID); err != nil {
		t.Fatalf("falha ao marcar usuário como banido: %v", err)
	}

	active, err := ListUsersWithActiveConnections(testCtx(), []string{user.ID})
	if err != nil {
		t.Fatalf("ListUsersWithActiveConnections: %v", err)
	}
	if active[user.ID] {
		t.Fatal("usuário banido não deve ser considerado com sessão WebSocket ativa")
	}
}
