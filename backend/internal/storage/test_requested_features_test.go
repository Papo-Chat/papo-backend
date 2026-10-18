package storage

import (
	"fmt"
	"testing"
	"time"

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


func TestListUsersOrderedBothDirections(t *testing.T) {
	wipeAppTables(t)
	first := newTestUser(t)
	time.Sleep(10 * time.Millisecond)
	second := newTestUser(t)

	firstCursor := first.CreatedAt
	newer, err := ListUsersOrdered(testCtx(), &firstCursor, first.ID, true, 100)
	if err != nil {
		t.Fatalf("ListUsersOrdered asc cursor: %v", err)
	}
	if len(newer) == 0 || newer[0].ID != second.ID {
		t.Fatalf("cursor asc deveria começar pelo usuário seguinte, obtive %+v", newer)
	}

	secondCursor := second.CreatedAt
	older, err := ListUsersOrdered(testCtx(), &secondCursor, second.ID, false, 100)
	if err != nil {
		t.Fatalf("ListUsersOrdered desc cursor: %v", err)
	}
	if len(older) == 0 || older[0].ID != first.ID {
		t.Fatalf("cursor desc deveria começar pelo usuário anterior, obtive %+v", older)
	}
}

// TestWebhookIdentityInSearchAndNotifications garante que busca e notificações
// exponham a identidade de exibição das mensagens de webhook. Nessas mensagens
// author_id é NULL, então sem webhook_id/external_author o cliente não tem como
// desenhar nome e avatar fora do chat (termina mostrando "Usuário").
func TestWebhookIdentityInSearchAndNotifications(t *testing.T) {
	wipeAppTables(t)
	creator := newTestUser(t)
	if _, err := CreateServer(testCtx(), "server_"+randHex(8), &creator.ID); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	channel := newTestChannel(t)

	webhook, err := CreateWebhook(testCtx(), channel.ID, "robo", creator.ID, "hash")
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	external := models.ExternalAuthor{
		Source:    "webhook",
		ID:        webhook.ID,
		Name:      "Robo do Papo",
		AvatarURL: "/webhooks/" + webhook.ID + "/avatar",
	}
	message, _, err := CreateMessageAs(testCtx(), channel.ID, "", webhook.ID, &external,
		"src_"+randHex(8), "borboleta identidade webhook", "", nil)
	if err != nil {
		t.Fatalf("CreateMessageAs: %v", err)
	}
	if message.AuthorID != nil {
		t.Fatalf("mensagem de webhook deveria ter author_id NULL, obtive %q", *message.AuthorID)
	}
	if _, err := CreateNotification(testCtx(), creator.ID, message.ID); err != nil {
		t.Fatalf("CreateNotification: %v", err)
	}

	results, err := SearchMessages(testCtx(), SearchParams{UserID: creator.ID, Text: "borboleta", Limit: 100})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	var searched bool
	for _, result := range results {
		if result.ID != message.ID {
			continue
		}
		searched = true
		if result.AuthorID != nil {
			t.Fatalf("resultado da busca para mensagem de webhook tem author_id %q", *result.AuthorID)
		}
		if result.WebhookID == nil || *result.WebhookID != webhook.ID {
			t.Fatalf("resultado da busca perdeu webhook_id: %+v", result)
		}
		if result.ExternalAuthor == nil || result.ExternalAuthor.Name != external.Name ||
			result.ExternalAuthor.AvatarURL != external.AvatarURL {
			t.Fatalf("resultado da busca perdeu external_author: %+v", result)
		}
	}
	if !searched {
		t.Fatalf("a mensagem de webhook não apareceu na busca: %+v", results)
	}

	notifications, err := ListUserNotifications(testCtx(), creator.ID, nil, "", 0)
	if err != nil {
		t.Fatalf("ListUserNotifications: %v", err)
	}
	var notified bool
	for _, notification := range notifications {
		if notification.MessageID != message.ID {
			continue
		}
		notified = true
		if notification.AuthorID != nil {
			t.Fatalf("notificação de mensagem de webhook tem author_id %q", *notification.AuthorID)
		}
		if notification.WebhookID == nil || *notification.WebhookID != webhook.ID {
			t.Fatalf("notificação perdeu webhook_id: %+v", notification)
		}
		if notification.ExternalAuthor == nil || notification.ExternalAuthor.Name != external.Name ||
			notification.ExternalAuthor.AvatarURL != external.AvatarURL {
			t.Fatalf("notificação perdeu external_author: %+v", notification)
		}
	}
	if !notified {
		t.Fatalf("a notificação da mensagem de webhook não apareceu na listagem: %+v", notifications)
	}
}
