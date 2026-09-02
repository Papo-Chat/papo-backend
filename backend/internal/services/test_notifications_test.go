package services

import (
	"context"
	"strings"
	"testing"
	"time"

	"papo/internal/models"
	"papo/internal/storage"
)

// --- helpers de notificações (services) ---

// notificationTestUser registra um novo usuário no banco de teste.
func notificationTestUser(t *testing.T) models.User {
	t.Helper()
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao registrar usuário: %v", err)
	}
	return user
}

// notificationTestChannel cria um servidor e um canal de texto próprios para
// o teste (isolando as configurações de notificação por canal).
func notificationTestChannel(t *testing.T, ownerID string) models.Channel {
	t.Helper()
	server, err := storage.CreateServer(context.Background(), newRandomServerName(), &ownerID)
	if err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	channel, err := storage.CreateChannel(context.Background(), newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	_ = server
	return channel
}

// notificationTestMessage cria uma mensagem do autor no canal e a
// notificação do usuário para ela, retornando a mensagem.
func notificationTestMessage(t *testing.T, channelID, authorID, userID, content string) models.Message {
	t.Helper()
	message, err := storage.CreateMessage(context.Background(), channelID, authorID, content, "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	if _, err := storage.CreateNotification(context.Background(), userID, message.ID); err != nil {
		t.Fatalf("falha ao criar notificação: %v", err)
	}
	return message
}

// notificationForMessage retorna a notificação do usuário para a mensagem
// (o id e o created_at da notificação diferem dos da mensagem).
func notificationForMessage(t *testing.T, userID, messageID string) models.NotificationSummary {
	t.Helper()
	list, err := ListUserNotifications(testCtx(), userID, userID, nil, "")
	if err != nil {
		t.Fatalf("falha ao listar notificações: %v", err)
	}
	for _, n := range list.Notifications {
		if n.MessageID == messageID {
			return n
		}
	}
	t.Fatalf("notificação da mensagem %s não encontrada", messageID)
	return models.NotificationSummary{}
}

// --- services.UpdateChannelUserSetting ---

// TestUpdateChannelUserSettingOk garante que o usuário altera a própria
// configuração no canal (upsert idempotente).
func TestUpdateChannelUserSettingOk(t *testing.T) {
	cleanServers(testCtx())
	user := notificationTestUser(t)
	channel := notificationTestChannel(t, user.ID)

	setting, err := UpdateChannelUserSetting(testCtx(), user.ID, channel.ID, user.ID, "all")
	if err != nil {
		t.Fatalf("falha ao atualizar configuração: %v", err)
	}
	if setting.NotificationSettings != "all" {
		t.Errorf("esperava notification_settings all, obtive %q", setting.NotificationSettings)
	}

	setting, err = UpdateChannelUserSetting(testCtx(), user.ID, channel.ID, user.ID, "off")
	if err != nil {
		t.Fatalf("falha ao atualizar configuração novamente: %v", err)
	}
	if setting.NotificationSettings != "off" {
		t.Errorf("esperava notification_settings off, obtive %q", setting.NotificationSettings)
	}
}

// TestUpdateChannelUserSettingOtherUserDenied garante que o usuário não
// altera a configuração de outro usuário.
func TestUpdateChannelUserSettingOtherUserDenied(t *testing.T) {
	cleanServers(testCtx())
	user := notificationTestUser(t)
	other := notificationTestUser(t)
	channel := notificationTestChannel(t, user.ID)

	_, err := UpdateChannelUserSetting(testCtx(), user.ID, channel.ID, other.ID, "all")
	if err != ErrPermissionDenied {
		t.Fatalf("esperava ErrPermissionDenied, obtive %v", err)
	}
}

// TestUpdateChannelUserSettingInvalidType garante que um valor fora do enum
// responde erro de entrada inválida.
func TestUpdateChannelUserSettingInvalidType(t *testing.T) {
	cleanServers(testCtx())
	user := notificationTestUser(t)
	channel := notificationTestChannel(t, user.ID)

	_, err := UpdateChannelUserSetting(testCtx(), user.ID, channel.ID, user.ID, "sempre")
	if err != ErrInvalidInput {
		t.Fatalf("esperava ErrInvalidInput, obtive %v", err)
	}
}

// TestUpdateChannelUserSettingChannelNotFound garante que um canal
// inexistente responde erro de recurso não encontrado.
func TestUpdateChannelUserSettingChannelNotFound(t *testing.T) {
	cleanServers(testCtx())
	user := notificationTestUser(t)

	_, err := UpdateChannelUserSetting(testCtx(), user.ID, randUUID(), user.ID, "all")
	if err != ErrChannelNotFound {
		t.Fatalf("esperava ErrChannelNotFound, obtive %v", err)
	}
}

// --- services.ListUserNotifications ---

// TestListUserNotificationsOk garante que a listagem retorna as
// notificações do usuário com os dados da mensagem e em ordem decrescente.
func TestListUserNotificationsOk(t *testing.T) {
	cleanServers(testCtx())
	user := notificationTestUser(t)
	channel := notificationTestChannel(t, user.ID)

	first := notificationTestMessage(t, channel.ID, user.ID, user.ID, "primeira")
	time.Sleep(10 * time.Millisecond)
	second := notificationTestMessage(t, channel.ID, user.ID, user.ID, "segunda")

	list, err := ListUserNotifications(testCtx(), user.ID, user.ID, nil, "")
	if err != nil {
		t.Fatalf("falha ao listar notificações: %v", err)
	}
	if len(list.Notifications) != 2 {
		t.Fatalf("esperava 2 notificações, obtive %d", len(list.Notifications))
	}
	newest, oldest := list.Notifications[0], list.Notifications[1]
	if newest.MessageID != second.ID || oldest.MessageID != first.ID {
		t.Errorf("ordem inesperada: %+v", list.Notifications)
	}
	if newest.ChannelID != channel.ID {
		t.Errorf("esperava channel_id %q, obtive %q", channel.ID, newest.ChannelID)
	}
	if newest.AuthorID == nil || *newest.AuthorID != user.ID {
		t.Errorf("esperava author_id %q, obtive %v", user.ID, newest.AuthorID)
	}
	if newest.MessageContent != "segunda" {
		t.Errorf("esperava message_content %q, obtive %q", "segunda", newest.MessageContent)
	}
	if newest.Read {
		t.Error("esperava read false, obtive true")
	}
	if list.HasMore {
		t.Error("esperava has_more false, obtive true")
	}
}

// TestListUserNotificationsEmpty garante que a listagem sem notificações
// retorna lista vazia (nunca nil).
func TestListUserNotificationsEmpty(t *testing.T) {
	cleanServers(testCtx())
	user := notificationTestUser(t)

	list, err := ListUserNotifications(testCtx(), user.ID, user.ID, nil, "")
	if err != nil {
		t.Fatalf("falha ao listar notificações: %v", err)
	}
	if list.Notifications == nil {
		t.Fatal("esperava lista vazia, obtive nil")
	}
	if len(list.Notifications) != 0 {
		t.Errorf("esperava 0 notificações, obtive %d", len(list.Notifications))
	}
}

// TestListUserNotificationsOtherUserDenied garante que o usuário não lista as
// notificações de outro usuário.
func TestListUserNotificationsOtherUserDenied(t *testing.T) {
	cleanServers(testCtx())
	user := notificationTestUser(t)
	other := notificationTestUser(t)

	_, err := ListUserNotifications(testCtx(), user.ID, other.ID, nil, "")
	if err != ErrPermissionDenied {
		t.Fatalf("esperava ErrPermissionDenied, obtive %v", err)
	}
}

// TestListUserNotificationsSinceFilter garante que since retorna apenas
// notificações criadas após o timestamp.
func TestListUserNotificationsSinceFilter(t *testing.T) {
	cleanServers(testCtx())
	user := notificationTestUser(t)
	channel := notificationTestChannel(t, user.ID)

	first := notificationTestMessage(t, channel.ID, user.ID, user.ID, "primeira")
	time.Sleep(10 * time.Millisecond)
	second := notificationTestMessage(t, channel.ID, user.ID, user.ID, "segunda")

	since := notificationForMessage(t, user.ID, first.ID).CreatedAt
	list, err := ListUserNotifications(testCtx(), user.ID, user.ID, &since, "")
	if err != nil {
		t.Fatalf("falha ao listar notificações: %v", err)
	}
	if len(list.Notifications) != 1 || list.Notifications[0].MessageID != second.ID {
		t.Errorf("esperava somente a notificação da segunda mensagem, obtive %+v", list.Notifications)
	}
}

// TestListUserNotificationsCursor garante que o cursor (since + last_id)
// retorna apenas as notificações anteriores à última id recebida.
func TestListUserNotificationsCursor(t *testing.T) {
	cleanServers(testCtx())
	user := notificationTestUser(t)
	channel := notificationTestChannel(t, user.ID)

	first := notificationTestMessage(t, channel.ID, user.ID, user.ID, "primeira")
	time.Sleep(10 * time.Millisecond)
	second := notificationTestMessage(t, channel.ID, user.ID, user.ID, "segunda")

	secondNotif := notificationForMessage(t, user.ID, second.ID)
	list, err := ListUserNotifications(testCtx(), user.ID, user.ID, &secondNotif.CreatedAt, secondNotif.ID)
	if err != nil {
		t.Fatalf("falha ao listar notificações: %v", err)
	}
	if len(list.Notifications) != 1 || list.Notifications[0].MessageID != first.ID {
		t.Errorf("esperava somente a notificação da primeira mensagem, obtive %+v", list.Notifications)
	}
}

// TestListUserNotificationsHasMore garante que a listagem pagina com limite
// de 100 e sinaliza has_more quando há mais itens.
func TestListUserNotificationsHasMore(t *testing.T) {
	cleanServers(testCtx())
	user := notificationTestUser(t)
	channel := notificationTestChannel(t, user.ID)

	// timestamps distintos (NOW() tem precisão de microsegundo e o loop é
	// rápido demais para garantir ordem estável)
	for i := 0; i < 101; i++ {
		notificationTestMessage(t, channel.ID, user.ID, user.ID, "mensagem "+strings.Repeat("x", i%7))
		time.Sleep(2 * time.Millisecond)
	}

	list, err := ListUserNotifications(testCtx(), user.ID, user.ID, nil, "")
	if err != nil {
		t.Fatalf("falha ao listar notificações: %v", err)
	}
	if len(list.Notifications) != 100 {
		t.Errorf("esperava 100 notificações, obtive %d", len(list.Notifications))
	}
	if !list.HasMore {
		t.Error("esperava has_more true, obtive false")
	}

	// segunda página: cursor a partir da última notificação da primeira
	since := list.Notifications[99].CreatedAt
	lastID := list.Notifications[99].ID
	list, err = ListUserNotifications(testCtx(), user.ID, user.ID, &since, lastID)
	if err != nil {
		t.Fatalf("falha ao listar segunda página: %v", err)
	}
	if len(list.Notifications) != 1 {
		t.Errorf("esperava 1 notificação na segunda página, obtive %d", len(list.Notifications))
	}
	if list.HasMore {
		t.Error("esperava has_more false na última página, obtive true")
	}
}

// TestListUserNotificationsTruncatesContent garante que o preview do
// conteúdo é truncado a 512 caracteres (rune-safe).
func TestListUserNotificationsTruncatesContent(t *testing.T) {
	cleanServers(testCtx())
	user := notificationTestUser(t)
	channel := notificationTestChannel(t, user.ID)

	notificationTestMessage(t, channel.ID, user.ID, user.ID, strings.Repeat("ç", 600))

	list, err := ListUserNotifications(testCtx(), user.ID, user.ID, nil, "")
	if err != nil {
		t.Fatalf("falha ao listar notificações: %v", err)
	}
	if len(list.Notifications) != 1 {
		t.Fatalf("esperava 1 notificação, obtive %d", len(list.Notifications))
	}
	if got := list.Notifications[0].MessageContent; len([]rune(got)) != 512 {
		t.Errorf("esperava message_content com 512 caracteres, obtive %d", len([]rune(got)))
	}
}

// --- services.MarkUserNotificationsRead ---

// TestMarkUserNotificationsReadOk garante que a marcação atualiza apenas as
// notificações do usuário e retorna o número de linhas afetadas.
func TestMarkUserNotificationsReadOk(t *testing.T) {
	cleanServers(testCtx())
	user := notificationTestUser(t)
	channel := notificationTestChannel(t, user.ID)

	first := notificationTestMessage(t, channel.ID, user.ID, user.ID, "primeira")
	second := notificationTestMessage(t, channel.ID, user.ID, user.ID, "segunda")
	notificationTestMessage(t, channel.ID, user.ID, user.ID, "terceira")

	firstNotif := notificationForMessage(t, user.ID, first.ID)
	secondNotif := notificationForMessage(t, user.ID, second.ID)
	updated, err := MarkUserNotificationsRead(testCtx(), user.ID, user.ID, []string{firstNotif.ID, secondNotif.ID})
	if err != nil {
		t.Fatalf("falha ao marcar como lidas: %v", err)
	}
	if updated != 2 {
		t.Errorf("esperava 2 atualizadas, obtive %d", updated)
	}

	list, err := ListUserNotifications(testCtx(), user.ID, user.ID, nil, "")
	if err != nil {
		t.Fatalf("falha ao listar notificações: %v", err)
	}
	readCount := 0
	for _, n := range list.Notifications {
		if n.Read {
			readCount++
		}
	}
	if readCount != 2 {
		t.Errorf("esperava 2 notificações lidas, obtive %d", readCount)
	}
}

// TestMarkUserNotificationsReadDedup garante que ids duplicados na entrada
// não atualizam duas vezes.
func TestMarkUserNotificationsReadDedup(t *testing.T) {
	cleanServers(testCtx())
	user := notificationTestUser(t)
	channel := notificationTestChannel(t, user.ID)

	first := notificationTestMessage(t, channel.ID, user.ID, user.ID, "primeira")
	firstNotif := notificationForMessage(t, user.ID, first.ID)

	updated, err := MarkUserNotificationsRead(testCtx(), user.ID, user.ID, []string{firstNotif.ID, firstNotif.ID})
	if err != nil {
		t.Fatalf("falha ao marcar como lidas: %v", err)
	}
	if updated != 1 {
		t.Errorf("esperava 1 atualizada, obtive %d", updated)
	}
}

// TestMarkUserNotificationsReadEmptyIDs garante que uma lista vazia responde
// erro de entrada inválida.
func TestMarkUserNotificationsReadEmptyIDs(t *testing.T) {
	cleanServers(testCtx())
	user := notificationTestUser(t)

	_, err := MarkUserNotificationsRead(testCtx(), user.ID, user.ID, nil)
	if err != ErrInvalidInput {
		t.Fatalf("esperava ErrInvalidInput, obtive %v", err)
	}
}

// TestMarkUserNotificationsReadTooManyIDs garante que mais de 1000 ids
// responde erro de entrada inválida.
func TestMarkUserNotificationsReadTooManyIDs(t *testing.T) {
	cleanServers(testCtx())
	user := notificationTestUser(t)

	ids := make([]string, 0, 1001)
	for i := 0; i < 1001; i++ {
		ids = append(ids, randUUID())
	}
	_, err := MarkUserNotificationsRead(testCtx(), user.ID, user.ID, ids)
	if err != ErrInvalidInput {
		t.Fatalf("esperava ErrInvalidInput, obtive %v", err)
	}
}

// TestMarkUserNotificationsReadEmptyID garante que um id vazio responde erro
// de entrada inválida.
func TestMarkUserNotificationsReadEmptyID(t *testing.T) {
	cleanServers(testCtx())
	user := notificationTestUser(t)

	_, err := MarkUserNotificationsRead(testCtx(), user.ID, user.ID, []string{""})
	if err != ErrInvalidInput {
		t.Fatalf("esperava ErrInvalidInput, obtive %v", err)
	}
}

// TestMarkUserNotificationsReadNotFound garante que ids inexistentes
// respondem erro de recurso não encontrado.
func TestMarkUserNotificationsReadNotFound(t *testing.T) {
	cleanServers(testCtx())
	user := notificationTestUser(t)

	_, err := MarkUserNotificationsRead(testCtx(), user.ID, user.ID, []string{randUUID()})
	if err != ErrNotificationNotFound {
		t.Fatalf("esperava ErrNotificationNotFound, obtive %v", err)
	}
}

// TestMarkUserNotificationsReadOtherUserDenied garante que o usuário não
// marca como lidas notificações de outro usuário.
func TestMarkUserNotificationsReadOtherUserDenied(t *testing.T) {
	cleanServers(testCtx())
	user := notificationTestUser(t)
	other := notificationTestUser(t)
	channel := notificationTestChannel(t, user.ID)

	notification := notificationTestMessage(t, channel.ID, user.ID, user.ID, "primeira")
	notificationID := notificationForMessage(t, user.ID, notification.ID).ID

	_, err := MarkUserNotificationsRead(testCtx(), user.ID, user.ID, []string{notificationID})
	if err != nil {
		t.Fatalf("falha ao marcar as próprias notificações: %v", err)
	}
	// o outro usuário tenta marcar a notificação do primeiro (a query
	// restringe pelo user_id do alvo)
	_, err = MarkUserNotificationsRead(testCtx(), other.ID, user.ID, []string{notificationID})
	if err != ErrPermissionDenied {
		t.Fatalf("esperava ErrPermissionDenied, obtive %v", err)
	}
}

// --- services.DispatchMessageNotifications ---

// TestDispatchMessageNotificationsMention garante que uma menção direta gera
// entrega para o usuário mencionado (configuração padrão only_mentions).
func TestDispatchMessageNotificationsMention(t *testing.T) {
	cleanServers(testCtx())
	owner := notificationTestUser(t)
	other := notificationTestUser(t)
	channel := notificationTestChannel(t, owner.ID)

	message, err := storage.CreateMessage(context.Background(), channel.ID, owner.ID, "olá @"+other.ID, "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	deliveries := dispatchMessageNotifications(t, "req-1", message)
	if len(deliveries) != 1 {
		t.Fatalf("esperava 1 entrega, obtive %d", len(deliveries))
	}
	if deliveries[0].UserID != other.ID {
		t.Errorf("esperava entrega para %q, obtive %q", other.ID, deliveries[0].UserID)
	}
	if deliveries[0].EventID == "" {
		t.Error("esperava event_id preenchido (row persistida)")
	}
	if deliveries[0].MessageContent != "olá @"+other.ID {
		t.Errorf("conteúdo inesperado: %q", deliveries[0].MessageContent)
	}
}

// TestDispatchMessageNotificationsReplyTo garante que uma resposta gera
// entrega para o autor da mensagem referenciada.
func TestDispatchMessageNotificationsReplyTo(t *testing.T) {
	cleanServers(testCtx())
	owner := notificationTestUser(t)
	other := notificationTestUser(t)
	channel := notificationTestChannel(t, owner.ID)

	target, err := storage.CreateMessage(context.Background(), channel.ID, other.ID, "mensagem alvo", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem alvo: %v", err)
	}
	message, err := storage.CreateMessage(context.Background(), channel.ID, owner.ID, "resposta", target.ID, nil)
	if err != nil {
		t.Fatalf("falha ao criar resposta: %v", err)
	}

	deliveries := dispatchMessageNotifications(t, "req-1", message)
	if len(deliveries) != 1 {
		t.Fatalf("esperava 1 entrega, obtive %d", len(deliveries))
	}
	if deliveries[0].UserID != other.ID {
		t.Errorf("esperava entrega para o autor da mensagem referenciada %q, obtive %q", other.ID, deliveries[0].UserID)
	}
}

// TestDispatchMessageNotificationsEveryone garante que @everyone enviado pelo
// dono do servidor (permissão everyone_message implícita) gera entrega para
// os demais usuários do canal, nunca para o autor.
func TestDispatchMessageNotificationsEveryone(t *testing.T) {
	cleanServers(testCtx())
	owner := notificationTestUser(t)
	other := notificationTestUser(t)
	channel := notificationTestChannel(t, owner.ID)

	message, err := storage.CreateMessage(context.Background(), channel.ID, owner.ID, "aviso @everyone", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	// os candidatos são todos os usuários do sistema com configuração
	// efetiva diferente de off no canal (usuários persistem entre testes),
	// então o teste não asserciona a quantidade total: apenas que o usuário
	// do canal recebe uma entrega e o autor nunca é notificado.
	var forOther, forOwner int
	for _, d := range dispatchMessageNotifications(t, "req-1", message) {
		switch d.UserID {
		case other.ID:
			forOther++
		case owner.ID:
			forOwner++
		}
	}
	if forOther != 1 {
		t.Errorf("esperava 1 entrega para %q, obtive %d", other.ID, forOther)
	}
	if forOwner != 0 {
		t.Errorf("o autor %q não deveria ser notificado", owner.ID)
	}
}

// TestDispatchMessageNotificationsEveryoneWithoutPermission garante que
// @everyone enviado por um usuário sem a permissão everyone_message não gera
// entregas.
func TestDispatchMessageNotificationsEveryoneWithoutPermission(t *testing.T) {
	cleanServers(testCtx())
	owner := notificationTestUser(t)
	other := notificationTestUser(t)
	channel := notificationTestChannel(t, owner.ID)

	message, err := storage.CreateMessage(context.Background(), channel.ID, other.ID, "aviso @everyone", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	deliveries := dispatchMessageNotifications(t, "req-1", message)
	if len(deliveries) != 0 {
		t.Errorf("esperava 0 entregas, obtive %d", len(deliveries))
	}
}

// TestDispatchMessageNotificationsOffSetting garante que um usuário com a
// configuração off não recebe entregas nem por menção.
func TestDispatchMessageNotificationsOffSetting(t *testing.T) {
	cleanServers(testCtx())
	owner := notificationTestUser(t)
	other := notificationTestUser(t)
	channel := notificationTestChannel(t, owner.ID)

	if _, err := UpdateChannelUserSetting(testCtx(), other.ID, channel.ID, other.ID, "off"); err != nil {
		t.Fatalf("falha ao atualizar configuração: %v", err)
	}
	message, err := storage.CreateMessage(context.Background(), channel.ID, owner.ID, "olá @"+other.ID, "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	deliveries := dispatchMessageNotifications(t, "req-1", message)
	if len(deliveries) != 0 {
		t.Errorf("esperava 0 entregas, obtive %d", len(deliveries))
	}
}

// TestDispatchMessageNotificationsAllWithoutTrigger garante que um usuário
// com a configuração all recebe entrega (evento efêmero) mesmo sem trigger,
// sem gerar row persistida.
func TestDispatchMessageNotificationsAllWithoutTrigger(t *testing.T) {
	cleanServers(testCtx())
	owner := notificationTestUser(t)
	other := notificationTestUser(t)
	channel := notificationTestChannel(t, owner.ID)

	if _, err := UpdateChannelUserSetting(testCtx(), other.ID, channel.ID, other.ID, "all"); err != nil {
		t.Fatalf("falha ao atualizar configuração: %v", err)
	}
	message, err := storage.CreateMessage(context.Background(), channel.ID, owner.ID, "mensagem comum", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	deliveries := dispatchMessageNotifications(t, "req-1", message)
	if len(deliveries) != 1 {
		t.Fatalf("esperava 1 entrega, obtive %d", len(deliveries))
	}
	if deliveries[0].UserID != other.ID {
		t.Errorf("esperava entrega para %q, obtive %q", other.ID, deliveries[0].UserID)
	}

	list, err := ListUserNotifications(testCtx(), other.ID, other.ID, nil, "")
	if err != nil {
		t.Fatalf("falha ao listar notificações: %v", err)
	}
	if len(list.Notifications) != 0 {
		t.Errorf("esperava 0 notificações persistidas, obtive %d", len(list.Notifications))
	}
}

// TestDispatchMessageNotificationsOnlyMentionsWithoutTrigger garante que um
// usuário com a configuração only_mentions não recebe entrega sem trigger.
func TestDispatchMessageNotificationsOnlyMentionsWithoutTrigger(t *testing.T) {
	cleanServers(testCtx())
	owner := notificationTestUser(t)
	other := notificationTestUser(t)
	channel := notificationTestChannel(t, owner.ID)

	message, err := storage.CreateMessage(context.Background(), channel.ID, owner.ID, "mensagem comum", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	deliveries := dispatchMessageNotifications(t, "req-1", message)
	for _, d := range deliveries {
		if d.UserID == other.ID {
			t.Errorf("o usuário %q não deveria ser notificado sem trigger", other.ID)
		}
	}
}

// TestDispatchMessageNotificationsAuthorNeverNotified garante que o autor da
// mensagem nunca é notificado, nem quando se menciona.
func TestDispatchMessageNotificationsAuthorNeverNotified(t *testing.T) {
	cleanServers(testCtx())
	owner := notificationTestUser(t)
	channel := notificationTestChannel(t, owner.ID)

	message, err := storage.CreateMessage(context.Background(), channel.ID, owner.ID, "olá "+owner.ID, "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	deliveries := dispatchMessageNotifications(t, "req-1", message)
	for _, d := range deliveries {
		if d.UserID == owner.ID {
			t.Errorf("o autor %q não deveria ser notificado", owner.ID)
		}
	}
}

// TestDispatchMessageNotificationsIdempotent garante que disparar duas vezes
// para a mesma mensagem gera uma única row (idempotência por user+message).
func TestDispatchMessageNotificationsIdempotent(t *testing.T) {
	cleanServers(testCtx())
	owner := notificationTestUser(t)
	other := notificationTestUser(t)
	channel := notificationTestChannel(t, owner.ID)

	message, err := storage.CreateMessage(context.Background(), channel.ID, owner.ID, "olá @"+other.ID, "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}

	first := dispatchMessageNotifications(t, "req-1", message)
	if len(first) != 1 {
		t.Fatalf("esperava 1 entrega no primeiro disparo, obtive %d", len(first))
	}
	second := dispatchMessageNotifications(t, "req-2", message)
	if len(second) != 1 {
		t.Fatalf("esperava 1 entrega no segundo disparo, obtive %d", len(second))
	}
	if first[0].EventID == "" || first[0].EventID != second[0].EventID {
		t.Errorf("esperava o mesmo event_id (row idempotente), obtive %q e %q", first[0].EventID, second[0].EventID)
	}

	list, err := ListUserNotifications(testCtx(), other.ID, other.ID, nil, "")
	if err != nil {
		t.Fatalf("falha ao listar notificações: %v", err)
	}
	if len(list.Notifications) != 1 {
		t.Errorf("esperava 1 notificação persistida, obtive %d", len(list.Notifications))
	}
}

// TestDispatchMessageNotificationsRespectsChannelPermissions garante que um
// usuário sem permissão de leitura do canal não recebe notificações (nem por
// @everyone, nem por menção), enquanto um usuário com permissão de leitura
// continua recebendo.
func TestDispatchMessageNotificationsRespectsChannelPermissions(t *testing.T) {
	cleanServers(testCtx())
	owner := notificationTestUser(t)
	reader := notificationTestUser(t)
	stranger := notificationTestUser(t)
	channel := notificationTestChannel(t, owner.ID)

	role, err := storage.CreateRole(context.Background(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	if _, err := storage.AssignUserRole(context.Background(), reader.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}

	// Canal restrito: somente o dono do servidor e a role com ReadChannel
	// podem ler.
	if _, err := UpdateChannelPermissions(testCtx(), owner.ID, channel.ID, role.ID, models.ChannelPermission{ReadChannel: true}); err != nil {
		t.Fatalf("falha ao atualizar permissões do canal: %v", err)
	}

	// @everyone: leitor recebe, sem-leitura não.
	message, err := storage.CreateMessage(context.Background(), channel.ID, owner.ID, "aviso @everyone", "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	deliveries := dispatchMessageNotifications(t, "req-1", message)
	if got := notificationDeliveryCount(deliveries, reader.ID); got != 1 {
		t.Errorf("esperava 1 entrega ao leitor, obtive %d", got)
	}
	if got := notificationDeliveryCount(deliveries, stranger.ID); got != 0 {
		t.Errorf("usuário sem leitura não deveria receber entrega, obtive %d", got)
	}

	// Menção ao sem-leitura: não notifica.
	message, err = storage.CreateMessage(context.Background(), channel.ID, owner.ID, "olá @"+stranger.ID, "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	deliveries = dispatchMessageNotifications(t, "req-2", message)
	if got := notificationDeliveryCount(deliveries, stranger.ID); got != 0 {
		t.Errorf("menção a usuário sem leitura não deveria notificar, obtive %d", got)
	}

	// Menção ao leitor: notifica.
	message, err = storage.CreateMessage(context.Background(), channel.ID, owner.ID, "olá @"+reader.ID, "", nil)
	if err != nil {
		t.Fatalf("falha ao criar mensagem: %v", err)
	}
	deliveries = dispatchMessageNotifications(t, "req-3", message)
	if got := notificationDeliveryCount(deliveries, reader.ID); got != 1 {
		t.Errorf("menção a leitor deveria notificar, obtive %d", got)
	}
}

// notificationDeliveryCount conta as entregas para um usuário.
func notificationDeliveryCount(deliveries []NotificationDelivery, userID string) int {
	count := 0
	for _, d := range deliveries {
		if d.UserID == userID {
			count++
		}
	}
	return count
}

// dispatchMessageNotifications encapsula a chamada ao dispatch.
func dispatchMessageNotifications(t *testing.T, requestID string, message models.Message) []NotificationDelivery {
	t.Helper()
	return DispatchMessageNotifications(context.Background(), requestID, message)
}
