package storage

import (
	"testing"
	"time"

	"papo/internal/models"
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

func TestSetUserBannedRevokesOtherSessionsWithSameIP(t *testing.T) {
	ip := "198.51.100.77"
	bannedUser := newTestUser(t)
	altUser := newTestUser(t)
	otherUser := newTestUser(t)

	for _, user := range []struct{ id, ip string }{
		{bannedUser.ID, ip},
		{altUser.ID, ip},
		{otherUser.ID, "203.0.113.8"},
	} {
		if err := UpdateUserLastIP(testCtx(), user.id, user.ip); err != nil {
			t.Fatalf("UpdateUserLastIP(%s): %v", user.id, err)
		}
	}
	for _, user := range []models.User{bannedUser, altUser, otherUser} {
		if _, err := CreateUserConnection(testCtx(), randUUID(), user.ID, "hash_"+randHex(32), time.Now()); err != nil {
			t.Fatalf("CreateUserConnection(%s): %v", user.ID, err)
		}
	}

	if _, err := SetUserBanned(testCtx(), bannedUser.ID, true); err != nil {
		t.Fatalf("SetUserBanned: %v", err)
	}
	for _, user := range []models.User{bannedUser, altUser} {
		conns, err := ListUserConnections(testCtx(), user.ID)
		if err != nil {
			t.Fatalf("ListUserConnections(%s): %v", user.ID, err)
		}
		if len(conns) != 0 {
			t.Errorf("esperava sessões revogadas para %s, encontrei %d", user.ID, len(conns))
		}
	}
	conns, err := ListUserConnections(testCtx(), otherUser.ID)
	if err != nil {
		t.Fatalf("ListUserConnections(other): %v", err)
	}
	if len(conns) != 1 {
		t.Errorf("sessão de IP diferente não deve ser revogada, encontrei %d", len(conns))
	}
}

func TestSetUserBannedWithoutIPDoesNotRevokeOtherUsers(t *testing.T) {
	bannedUser := newTestUser(t)
	otherUser := newTestUser(t)
	// Remove the fixture IP: newTestUser uses a default IP, so an empty-IP
	// case must explicitly clear it to verify the no-IP behavior.
	if err := UpdateUserLastIP(testCtx(), bannedUser.ID, ""); err != nil {
		t.Fatalf("UpdateUserLastIP: %v", err)
	}
	if _, err := CreateUserConnection(testCtx(), randUUID(), otherUser.ID, "hash_"+randHex(32), time.Now()); err != nil {
		t.Fatalf("CreateUserConnection: %v", err)
	}
	if _, err := SetUserBanned(testCtx(), bannedUser.ID, true); err != nil {
		t.Fatalf("SetUserBanned: %v", err)
	}
	conns, err := ListUserConnections(testCtx(), otherUser.ID)
	if err != nil {
		t.Fatalf("ListUserConnections: %v", err)
	}
	if len(conns) != 1 {
		t.Errorf("IP ausente não deve revogar sessão de terceiros, encontrei %d", len(conns))
	}
}
