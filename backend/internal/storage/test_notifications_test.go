package storage

import (
	"testing"
	"time"

	"papo/internal/models"
)

// --- notifications ---

// notificationTestMessage cria uma mensagem do autor no canal e a
// notificação do usuário para ela, retornando a mensagem.
func notificationTestMessage(t *testing.T, channelID, authorID, userID, content string) models.Message {
	t.Helper()
	message, err := CreateMessage(testCtx(), channelID, authorID, content, "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	if _, err := CreateNotification(testCtx(), userID, message.ID); err != nil {
		t.Fatalf("falha ao criar notificação: %v", err)
	}
	return message
}

// TestUpsertChannelUserSetting garante a criação e a atualização da
// configuração do usuário no canal (upsert idempotente).
func TestUpsertChannelUserSetting(t *testing.T) {
	user := newTestUser(t)
	_ = newTestServer(t, &user.ID)
	channel := newTestChannel(t)

	setting, err := UpsertChannelUserSetting(testCtx(), user.ID, channel.ID, "all")
	if err != nil {
		t.Fatalf("UpsertChannelUserSetting retornou erro: %v", err)
	}
	if setting.UserID != user.ID || setting.ChannelID != channel.ID || setting.NotificationSettings != "all" {
		t.Errorf("registro inesperado: %+v", setting)
	}

	updated, err := UpsertChannelUserSetting(testCtx(), user.ID, channel.ID, "off")
	if err != nil {
		t.Fatalf("UpsertChannelUserSetting (atualização) retornou erro: %v", err)
	}
	if updated.NotificationSettings != "off" {
		t.Errorf("esperava configuração off, obtive %q", updated.NotificationSettings)
	}
	if updated.UpdatedAt.Before(setting.UpdatedAt) {
		t.Errorf("esperava updated_at avançar na atualização: %v -> %v", setting.UpdatedAt, updated.UpdatedAt)
	}
}

// TestListChannelNotificationCandidates garante que os candidatos são todos
// os usuários com configuração efetiva diferente de off (padrão
// only_mentions quando não há row).
func TestListChannelNotificationCandidates(t *testing.T) {
	defaultUser := newTestUser(t)
	allUser := newTestUser(t)
	offUser := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)

	if _, err := UpsertChannelUserSetting(testCtx(), allUser.ID, channel.ID, "all"); err != nil {
		t.Fatalf("falha ao configurar all: %v", err)
	}
	if _, err := UpsertChannelUserSetting(testCtx(), offUser.ID, channel.ID, "off"); err != nil {
		t.Fatalf("falha ao configurar off: %v", err)
	}

	candidates, err := ListChannelNotificationCandidates(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("ListChannelNotificationCandidates retornou erro: %v", err)
	}

	byUser := make(map[string]string, len(candidates))
	for _, candidate := range candidates {
		byUser[candidate.UserID] = candidate.NotificationSettings
	}
	if got := byUser[defaultUser.ID]; got != "only_mentions" {
		t.Errorf("esperava o usuário padrão como candidato com only_mentions, obtive %q", got)
	}
	if got := byUser[allUser.ID]; got != "all" {
		t.Errorf("esperava o usuário all como candidato com all, obtive %q", got)
	}
	if _, ok := byUser[offUser.ID]; ok {
		t.Errorf("usuário com configuração off não deveria ser candidato")
	}
}

// TestCreateNotification garante a criação da notificação do usuário para a
// mensagem e a idempotência do re-disparo (mesmo usuário + mesma mensagem).
func TestCreateNotification(t *testing.T) {
	user := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)
	message, err := CreateMessage(testCtx(), channel.ID, user.ID, "mensagem", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	first, err := CreateNotification(testCtx(), user.ID, message.ID)
	if err != nil {
		t.Fatalf("CreateNotification retornou erro: %v", err)
	}
	if first.ID == "" || first.UserID != user.ID || first.MessageID != message.ID {
		t.Errorf("notificação inesperada: %+v", first)
	}
	if first.Read {
		t.Errorf("esperava a notificação nova como não lida")
	}

	second, err := CreateNotification(testCtx(), user.ID, message.ID)
	if err != nil {
		t.Fatalf("CreateNotification (re-disparo) retornou erro: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("esperava o mesmo id no re-disparo, obtive %q e %q", first.ID, second.ID)
	}
}

// TestListUserNotifications garante a listagem com join da mensagem, ordem
// decrescente, filtro since, cursor (created_at, id) e busca limit+1.
func TestListUserNotifications(t *testing.T) {
	user := newTestUser(t)
	other := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)

	first := notificationTestMessage(t, channel.ID, user.ID, user.ID, "primeira")
	time.Sleep(10 * time.Millisecond)
	second := notificationTestMessage(t, channel.ID, user.ID, user.ID, "segunda")
	// notificação de outro usuário que não pode vazar
	notificationTestMessage(t, channel.ID, other.ID, other.ID, "de outro usuário")

	notifications, err := ListUserNotifications(testCtx(), user.ID, nil, "", 0)
	if err != nil {
		t.Fatalf("ListUserNotifications retornou erro: %v", err)
	}
	if len(notifications) != 2 {
		t.Fatalf("esperava 2 notificações, obtive %d", len(notifications))
	}
	if notifications[0].MessageID != second.ID {
		t.Errorf("esperava a mais recente primeiro, obtive %s", notifications[0].ID)
	}
	if notifications[1].MessageID != first.ID {
		t.Errorf("esperava a mais antiga por último, obtive %s", notifications[1].ID)
	}

	// join com a mensagem
	if notifications[0].ChannelID != channel.ID {
		t.Errorf("esperava channel_id %s, obtive %s", channel.ID, notifications[0].ChannelID)
	}
	if notifications[0].AuthorID == nil || *notifications[0].AuthorID != user.ID {
		t.Errorf("esperava author_id %s, obtive %v", user.ID, notifications[0].AuthorID)
	}
	if notifications[0].MessageContent != "segunda" {
		t.Errorf("esperava content %q, obtive %q", "segunda", notifications[0].MessageContent)
	}
	if notifications[0].Read {
		t.Errorf("esperava a notificação como não lida")
	}

	// since: apenas notificações criadas após o timestamp
	since := notifications[1].CreatedAt
	sinceNotifications, err := ListUserNotifications(testCtx(), user.ID, timePtr(since), "", 0)
	if err != nil {
		t.Fatalf("ListUserNotifications com since retornou erro: %v", err)
	}
	if len(sinceNotifications) != 1 || sinceNotifications[0].ID != notifications[0].ID {
		t.Errorf("esperava apenas a notificação após o since, obtive %d", len(sinceNotifications))
	}

	// since + last_id: cursor (created_at, id) na ordem decrescente — retorna
	// as notificações anteriores ao cursor
	cursorNotifications, err := ListUserNotifications(testCtx(), user.ID, timePtr(notifications[0].CreatedAt), notifications[0].ID, 0)
	if err != nil {
		t.Fatalf("ListUserNotifications com cursor retornou erro: %v", err)
	}
	if len(cursorNotifications) != 1 || cursorNotifications[0].ID != notifications[1].ID {
		t.Errorf("esperava apenas a notificação anterior ao cursor, obtive %d", len(cursorNotifications))
	}

	// limit: busca limit+1 (o chamador decide has_more)
	limited, err := ListUserNotifications(testCtx(), user.ID, nil, "", 1)
	if err != nil {
		t.Fatalf("ListUserNotifications com limit retornou erro: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("esperava limit+1 notificações, obtive %d", len(limited))
	}
}

// TestListUserNotificationsCursorSameTimestamp garante que o cursor não
// pula notificações com timestamp igual (tiebreak por id).
func TestListUserNotificationsCursorSameTimestamp(t *testing.T) {
	user := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)

	notificationTestMessage(t, channel.ID, user.ID, user.ID, "primeira")
	notificationTestMessage(t, channel.ID, user.ID, user.ID, "segunda")

	// iguala os timestamps para forçar o tiebreak por id
	list, err := ListUserNotifications(testCtx(), user.ID, nil, "", 0)
	if err != nil {
		t.Fatalf("ListUserNotifications retornou erro: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("esperava 2 notificações, obtive %d", len(list))
	}
	if _, err := GetDB().ExecContext(testCtx(),
		"UPDATE notifications SET created_at = $1 WHERE id IN ($2, $3)",
		list[0].CreatedAt, list[0].ID, list[1].ID,
	); err != nil {
		t.Fatalf("falha ao igualar os timestamps: %v", err)
	}

	// com o mesmo timestamp, a ordem é por id DESC
	list, err = ListUserNotifications(testCtx(), user.ID, nil, "", 0)
	if err != nil {
		t.Fatalf("ListUserNotifications retornou erro: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("esperava 2 notificações, obtive %d", len(list))
	}

	cursor, err := ListUserNotifications(testCtx(), user.ID, timePtr(list[0].CreatedAt), list[0].ID, 0)
	if err != nil {
		t.Fatalf("ListUserNotifications com cursor retornou erro: %v", err)
	}
	if len(cursor) != 1 || cursor[0].ID != list[1].ID {
		t.Errorf("esperava apenas a notificação anterior ao cursor, obtive %d", len(cursor))
	}
}

// TestListUserNotificationsExcludesDeletedMessages garante que a notificação
// cuja mensagem foi excluída não aparece na listagem (join).
func TestListUserNotificationsExcludesDeletedMessages(t *testing.T) {
	user := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)

	kept := notificationTestMessage(t, channel.ID, user.ID, user.ID, "mantida")
	removed := notificationTestMessage(t, channel.ID, user.ID, user.ID, "removida")

	if err := DeleteMessage(testCtx(), removed.ID); err != nil {
		t.Fatalf("falha ao excluir mensagem: %v", err)
	}

	notifications, err := ListUserNotifications(testCtx(), user.ID, nil, "", 0)
	if err != nil {
		t.Fatalf("ListUserNotifications retornou erro: %v", err)
	}
	if len(notifications) != 1 || notifications[0].MessageID != kept.ID {
		t.Errorf("esperava apenas a notificação da mensagem remanescente, obtive %d", len(notifications))
	}
}

// TestMarkUserNotificationsRead garante o mark-as-read por ids restrito ao
// usuário (ids de outros usuários são ignorados) e a contagem de linhas
// afetadas.
func TestMarkUserNotificationsRead(t *testing.T) {
	user := newTestUser(t)
	other := newTestUser(t)
	_ = newTestServer(t, nil)
	channel := newTestChannel(t)

	first := notificationTestMessage(t, channel.ID, user.ID, user.ID, "primeira")
	time.Sleep(10 * time.Millisecond)
	second := notificationTestMessage(t, channel.ID, user.ID, user.ID, "segunda")
	// notificação de outro usuário
	notificationTestMessage(t, channel.ID, other.ID, other.ID, "de outro")

	list, err := ListUserNotifications(testCtx(), user.ID, nil, "", 0)
	if err != nil {
		t.Fatalf("ListUserNotifications retornou erro: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("esperava 2 notificações do usuário, obtive %d", len(list))
	}
	idsByMessage := make(map[string]string, len(list))
	for _, n := range list {
		idsByMessage[n.MessageID] = n.ID
	}
	otherList, err := ListUserNotifications(testCtx(), other.ID, nil, "", 0)
	if err != nil {
		t.Fatalf("ListUserNotifications retornou erro: %v", err)
	}
	if len(otherList) != 1 {
		t.Fatalf("esperava 1 notificação do outro usuário, obtive %d", len(otherList))
	}

	affected, err := MarkUserNotificationsRead(testCtx(), user.ID, []string{idsByMessage[first.ID], idsByMessage[second.ID], otherList[0].ID})
	if err != nil {
		t.Fatalf("MarkUserNotificationsRead retornou erro: %v", err)
	}
	if affected != 2 {
		t.Errorf("esperava 2 linhas afetadas, obtive %d", affected)
	}

	list, err = ListUserNotifications(testCtx(), user.ID, nil, "", 0)
	if err != nil {
		t.Fatalf("ListUserNotifications retornou erro: %v", err)
	}
	for _, n := range list {
		if !n.Read {
			t.Errorf("esperava a notificação %s como lida", n.ID)
		}
	}
	otherList, err = ListUserNotifications(testCtx(), other.ID, nil, "", 0)
	if err != nil {
		t.Fatalf("ListUserNotifications retornou erro: %v", err)
	}
	if otherList[0].Read {
		t.Errorf("a notificação de outro usuário não deveria ter sido marcada")
	}

	// ids vazios: nenhuma linha afetada, sem erro
	affected, err = MarkUserNotificationsRead(testCtx(), user.ID, []string{})
	if err != nil {
		t.Errorf("ids vazios não deveriam falhar: %v", err)
	}
	if affected != 0 {
		t.Errorf("esperava 0 linhas afetadas, obtive %d", affected)
	}
}
