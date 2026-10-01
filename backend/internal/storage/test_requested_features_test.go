package storage

import (
	"fmt"
	"testing"

	"papo/internal/models"
)

func TestRequestedSearchFilters(t *testing.T) {
	wipeAppTables(t)
	owner := newTestUser(t)
	if _, err := CreateServer(testCtx(), "server_"+randHex(8), &owner.ID); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	channel := newTestChannel(t)
	otherChannel := newTestChannel(t)
	mentioned := newTestUser(t)

	content := fmt.Sprintf("oi @mention(<@%s>) veja https://example.com", mentioned.ID)
	expected, err := CreateMessage(testCtx(), channel.ID, owner.ID, content, "", nil)
	if err != nil {
		t.Fatalf("CreateMessage expected: %v", err)
	}
	if _, err := CreateMessage(testCtx(), otherChannel.ID, owner.ID, content, "", nil); err != nil {
		t.Fatalf("CreateMessage other channel: %v", err)
	}
	if _, err := CreateMessage(testCtx(), channel.ID, owner.ID, "sem link e sem menção", "", nil); err != nil {
		t.Fatalf("CreateMessage plain: %v", err)
	}

	results, err := SearchMessages(testCtx(), SearchParams{
		UserID: owner.ID, ChannelID: channel.ID,
		MentionToken: fmt.Sprintf("@mention(<@%s>)", mentioned.ID),
		HasLink: true, Limit: 100,
	})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(results) != 1 || results[0].ID != expected.ID {
		t.Fatalf("filtros channel_id+mention+has:link retornaram %+v", results)
	}
}

func TestDeleteChannelRolePermissionOnlyUnlinksChannel(t *testing.T) {
	wipeAppTables(t)
	if _, err := CreateServer(testCtx(), "server_"+randHex(8), nil); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	channel := newTestChannel(t)
	role, err := CreateRole(testCtx(), "role_"+randHex(8), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if _, err := UpdateChannelPermissions(testCtx(), channel.ID, role.ID, models.ChannelPermission{ReadChannel: true}); err != nil {
		t.Fatalf("UpdateChannelPermissions: %v", err)
	}
	if err := DeleteChannelRolePermission(testCtx(), channel.ID, role.ID); err != nil {
		t.Fatalf("DeleteChannelRolePermission: %v", err)
	}

	updated, err := GetChannelByID(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("GetChannelByID: %v", err)
	}
	if _, ok := updated.Permissions[role.ID]; ok {
		t.Fatalf("vínculo da role permaneceu no canal")
	}
	if _, err := GetRoleByID(testCtx(), role.ID); err != nil {
		t.Fatalf("a role foi removida junto com o vínculo: %v", err)
	}
}
