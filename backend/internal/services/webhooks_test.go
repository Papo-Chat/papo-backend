package services

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"papo/internal/models"
	"papo/internal/storage"
)

// newWebhookTestUser cria um usuário comum via storage (ator de gestão nos
// testes de service; a checagem de permissão acontece no middleware HTTP).
func newWebhookTestUser(t *testing.T) models.User {
	t.Helper()
	user, _, err := storage.CreateUser(testCtx(), newRandomUsername(), "hash_"+randHex(8), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário de apoio: %v", err)
	}
	return user
}

// newWebhookTestChannel cria um canal text direto no storage.
func newWebhookTestChannel(t *testing.T, channelType string) models.Channel {
	t.Helper()
	channel, err := storage.CreateChannel(testCtx(), "ch_"+randHex(4), channelType, "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	return channel
}

// deleteWebhookTestUser limpa webhooks e mensagens criados pelo teste. O
// usuário NÃO é removido: ele fica referenciado no audit log append-only
// (actor_id), que bloqueia o SET NULL do FK (mesma convenção dos testes de
// mensagens diretas). Usuários extras de teste são tolerados pelos demais
// testes do pacote.
func deleteWebhookTestUser(t *testing.T, user models.User) {
	t.Helper()
	ctx := testCtx()
	if _, err := storage.GetDB().ExecContext(ctx,
		"DELETE FROM messages WHERE webhook_id IN (SELECT id FROM webhooks WHERE created_by = $1)", user.ID); err != nil {
		t.Errorf("falha ao limpar mensagens de webhook: %v", err)
	}
	if _, err := storage.GetDB().ExecContext(ctx, "DELETE FROM webhooks WHERE created_by = $1", user.ID); err != nil {
		t.Errorf("falha ao limpar webhooks: %v", err)
	}
}

func TestValidateExternalAuthor(t *testing.T) {
	cases := []struct {
		name      string
		author    *models.ExternalAuthor
		wantError bool
	}{
		{"no attribution", nil, false},
		{"discord", &models.ExternalAuthor{Source: "discord", ID: "123", Name: "Original author", AvatarURL: "https://cdn.discordapp.com/avatar.png"}, false},
		{"irc", &models.ExternalAuthor{Source: "irc", ID: "nickname!host", Name: "Nickname"}, false},
		{"missing id", &models.ExternalAuthor{Source: "discord", Name: "Me"}, true},
		{"missing source", &models.ExternalAuthor{ID: "123", Name: "Me"}, true},
		{"bad source", &models.ExternalAuthor{Source: "DISCORD", ID: "123", Name: "Me"}, true},
		{"control character", &models.ExternalAuthor{Source: "discord", ID: "123", Name: "Me\nInjected"}, true},
		{"local avatar", &models.ExternalAuthor{Source: "discord", ID: "123", Name: "Me", AvatarURL: "http://localhost:3000/private"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validateExternalAuthor(tc.author)
			if (got != nil) != tc.wantError {
				t.Fatalf("got error %v; want error=%v", got, tc.wantError)
			}
		})
	}
}

// TestWebhookLifecycle cobre criação, autenticação do token e revogação:
// o token em texto puro só existe na resposta de criação; a tabela guarda o
// hash; revoke (soft delete) derruba a autenticação mas preserva o registro.
func TestWebhookLifecycle(t *testing.T) {
	user := newWebhookTestUser(t)
	defer deleteWebhookTestUser(t, user)
	channel := newWebhookTestChannel(t, "text")

	webhook, token, err := CreateWebhook(testCtx(), user.ID, channel.ID, "Integração")
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	if webhook.ID == "" || webhook.ChannelID != channel.ID || webhook.Name != "Integração" {
		t.Fatalf("webhook inesperado: %+v", webhook)
	}
	if len(token) != 64 {
		t.Fatalf("token deveria ter 64 hex chars, tem %d", len(token))
	}

	// A tabela guarda somente o hash, nunca o token.
	var storedHash string
	if err := storage.GetDB().QueryRowContext(testCtx(),
		"SELECT secret_hash FROM webhooks WHERE id = $1", webhook.ID).Scan(&storedHash); err != nil {
		t.Fatalf("secret_hash: %v", err)
	}
	if storedHash == token || len(storedHash) != 64 {
		t.Fatalf("secret_hash armazenado incorretamente: %q", storedHash)
	}

	if _, err := AuthenticateWebhook(testCtx(), webhook.ID, token); err != nil {
		t.Fatalf("AuthenticateWebhook com token válido: %v", err)
	}
	if _, err := AuthenticateWebhook(testCtx(), webhook.ID, token+"deadbeef"); !errors.Is(err, ErrWebhookUnauthorized) {
		t.Fatalf("token errado deveria dar ErrWebhookUnauthorized, recebeu %v", err)
	}
	if _, err := AuthenticateWebhook(testCtx(), "00000000-0000-0000-0000-000000000000", token); !errors.Is(err, ErrWebhookUnauthorized) {
		t.Fatalf("webhook inexistente deveria dar ErrWebhookUnauthorized, recebeu %v", err)
	}

	if err := RevokeWebhook(testCtx(), user.ID, webhook.ID); err != nil {
		t.Fatalf("RevokeWebhook: %v", err)
	}
	if _, err := AuthenticateWebhook(testCtx(), webhook.ID, token); !errors.Is(err, ErrWebhookUnauthorized) {
		t.Fatalf("webhook revogado não deveria autenticar, recebeu %v", err)
	}
	if err := RevokeWebhook(testCtx(), user.ID, webhook.ID); !errors.Is(err, ErrWebhookNotFound) {
		t.Fatalf("revoke duplicado deveria dar ErrWebhookNotFound, recebeu %v", err)
	}

	// Soft delete: o registro permanece para a atribuição das mensagens.
	stored, err := GetWebhook(testCtx(), webhook.ID)
	if err != nil {
		t.Fatalf("GetWebhook após revoke: %v", err)
	}
	if stored.RevokedAt == nil {
		t.Fatal("revoked_at deveria estar definido")
	}
}

// TestCreateWebhookValidation cobre nome, canal inexistente e tipos de canal
// (dm é barrado na criação; voice é aceito).
func TestCreateWebhookValidation(t *testing.T) {
	user := newWebhookTestUser(t)
	defer deleteWebhookTestUser(t, user)

	if _, _, err := CreateWebhook(testCtx(), user.ID, "00000000-0000-0000-0000-000000000000", "x"); !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("canal inexistente deveria dar ErrChannelNotFound, recebeu %v", err)
	}

	channel := newWebhookTestChannel(t, "text")
	if _, _, err := CreateWebhook(testCtx(), user.ID, channel.ID, "   "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nome vazio deveria dar ErrInvalidInput, recebeu %v", err)
	}
	if _, _, err := CreateWebhook(testCtx(), user.ID, channel.ID, strings.Repeat("n", 81)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nome com 81 chars deveria dar ErrInvalidInput, recebeu %v", err)
	}

	peer := newWebhookTestUser(t)
	defer deleteWebhookTestUser(t, peer)
	dm, _, err := OpenDirectConversation(testCtx(), user.ID, peer.ID)
	if err != nil {
		t.Fatalf("OpenDirectConversation: %v", err)
	}
	if _, _, err := CreateWebhook(testCtx(), user.ID, dm.ID, "dm"); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("webhook em DM deveria dar ErrPermissionDenied, recebeu %v", err)
	}

	voice := newWebhookTestChannel(t, "voice")
	if _, _, err := CreateWebhook(testCtx(), user.ID, voice.ID, "voz"); err != nil {
		t.Fatalf("webhook em canal voice deveria ser aceito, recebeu %v", err)
	}
}

// TestWebhookIdentity cobre a identidade persistente (nickname + avatar) e a
// identidade temporária informada no envio, incluindo o preenchimento do
// avatar persistente quando a autoria temporária não traz avatar_url.
func TestWebhookIdentity(t *testing.T) {
	user := newWebhookTestUser(t)
	defer deleteWebhookTestUser(t, user)
	channel := newWebhookTestChannel(t, "text")

	webhook, _, err := CreateWebhook(testCtx(), user.ID, channel.ID, "origem")
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	// Sem identidade persistente: nickname cai no name do webhook, sem avatar.
	msg, created, err := CreateWebhookMessage(testCtx(), webhook, nil, "src-1", "olá", "", nil, nil)
	if err != nil || !created {
		t.Fatalf("CreateWebhookMessage: created=%v err=%v", created, err)
	}
	if msg.ExternalAuthor == nil || msg.ExternalAuthor.Name != "origem" || msg.ExternalAuthor.AvatarURL != "" {
		t.Fatalf("autoria persistente padrão inesperada: %+v", msg.ExternalAuthor)
	}

	// Avatar inválido é rejeitado.
	bad := "nao-e-base64"
	if _, err := UpdateWebhookIdentity(testCtx(), user.ID, webhook.ID, nil, &bad, "png"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("avatar inválido deveria dar ErrInvalidInput, recebeu %v", err)
	}

	// Identidade persistente: nickname + avatar.
	avatar := base64.StdEncoding.EncodeToString(pngAvatarBytes(64, 64))
	updated, err := UpdateWebhookIdentity(testCtx(), user.ID, webhook.ID, strPtr("Bot Oficial"), &avatar, "png")
	if err != nil {
		t.Fatalf("UpdateWebhookIdentity: %v", err)
	}
	if updated.DisplayName == nil || *updated.DisplayName != "Bot Oficial" || updated.AvatarMedia == nil {
		t.Fatalf("identidade não persistida: %+v", updated)
	}

	msg, _, err = CreateWebhookMessage(testCtx(), updated, nil, "src-2", "com identidade", "", nil, nil)
	if err != nil {
		t.Fatalf("CreateWebhookMessage com identidade: %v", err)
	}
	if msg.ExternalAuthor.Name != "Bot Oficial" {
		t.Fatalf("nickname persistente não aplicado: %+v", msg.ExternalAuthor)
	}
	if msg.ExternalAuthor.AvatarURL != webhookAvatarPath(webhook.ID) {
		t.Fatalf("avatar persistente não aplicado: %+v", msg.ExternalAuthor)
	}

	// Autoria temporária vale integralmente; sem avatar_url, o avatar
	// persistente preenche o campo.
	temp := &models.ExternalAuthor{Source: "discord", ID: "42", Name: "Autor Externo"}
	msg, _, err = CreateWebhookMessage(testCtx(), updated, temp, "src-3", "temporária", "", nil, nil)
	if err != nil {
		t.Fatalf("CreateWebhookMessage temporária: %v", err)
	}
	if msg.ExternalAuthor.Source != "discord" || msg.ExternalAuthor.Name != "Autor Externo" ||
		msg.ExternalAuthor.AvatarURL != webhookAvatarPath(webhook.ID) {
		t.Fatalf("merge de autoria temporária inesperado: %+v", msg.ExternalAuthor)
	}

	// Avatar_url explícito na autoria temporária não é sobrescrito.
	tempAvatar := &models.ExternalAuthor{Source: "discord", ID: "43", Name: "Com Avatar", AvatarURL: "https://cdn.example.com/a.png"}
	msg, _, err = CreateWebhookMessage(testCtx(), updated, tempAvatar, "src-4", "avatar próprio", "", nil, nil)
	if err != nil {
		t.Fatalf("CreateWebhookMessage com avatar externo: %v", err)
	}
	if msg.ExternalAuthor.AvatarURL != "https://cdn.example.com/a.png" {
		t.Fatalf("avatar_url explícito foi sobrescrito: %+v", msg.ExternalAuthor)
	}

	// Remover o nickname faz o webhook voltar a usar o name.
	cleared, err := UpdateWebhookIdentity(testCtx(), user.ID, webhook.ID, strPtr(""), nil, "")
	if err != nil {
		t.Fatalf("UpdateWebhookIdentity (limpar nickname): %v", err)
	}
	msg, _, err = CreateWebhookMessage(testCtx(), cleared, nil, "src-5", "sem nickname", "", nil, nil)
	if err != nil {
		t.Fatalf("CreateWebhookMessage sem nickname: %v", err)
	}
	if msg.ExternalAuthor.Name != "origem" {
		t.Fatalf("fallback para name do webhook falhou: %+v", msg.ExternalAuthor)
	}

	if _, err := UpdateWebhookIdentity(testCtx(), user.ID, "00000000-0000-0000-0000-000000000000", nil, nil, ""); !errors.Is(err, ErrWebhookNotFound) {
		t.Fatalf("webhook inexistente deveria dar ErrWebhookNotFound, recebeu %v", err)
	}
}

// TestWebhookMessageIdempotency cobre a chave external_message_id: retry com a
// mesma chave devolve a mesma mensagem (com attachments no mesmo shape), sem
// sourceID cada envio é uma mensagem nova, e chaves diferentes não colidem.
func TestWebhookMessageIdempotency(t *testing.T) {
	user := newWebhookTestUser(t)
	defer deleteWebhookTestUser(t, user)
	channel := newWebhookTestChannel(t, "text")

	webhook, _, err := CreateWebhook(testCtx(), user.ID, channel.ID, "idempotência")
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	attachment := AttachmentInput{OriginalFileName: "img.png", Content: bytes.NewReader(pngAvatarBytes(64, 64))}
	first, created, err := CreateWebhookMessage(testCtx(), webhook, nil, "src-dup", "primeira", "", []AttachmentInput{attachment}, nil)
	if err != nil || !created {
		t.Fatalf("primeiro envio: created=%v err=%v", created, err)
	}
	if len(first.Attachments) != 1 {
		t.Fatalf("primeira mensagem deveria ter 1 attachment, tem %d", len(first.Attachments))
	}

	retry, created, err := CreateWebhookMessage(testCtx(), webhook, nil, "src-dup", "retry ignorado", "", nil, nil)
	if err != nil || created {
		t.Fatalf("retry deveria retornar created=false, recebeu created=%v err=%v", created, err)
	}
	if retry.ID != first.ID {
		t.Fatalf("retry deveria devolver a mesma mensagem: %s != %s", retry.ID, first.ID)
	}
	if len(retry.Attachments) != 1 {
		t.Fatalf("retry deveria devolver a mensagem completa (attachments), tem %d", len(retry.Attachments))
	}

	// Sem chave: dois envios independentes criam mensagens distintas.
	a, _, err := CreateWebhookMessage(testCtx(), webhook, nil, "", "sem chave 1", "", nil, nil)
	if err != nil {
		t.Fatalf("envio sem chave 1: %v", err)
	}
	b, _, err := CreateWebhookMessage(testCtx(), webhook, nil, "", "sem chave 2", "", nil, nil)
	if err != nil {
		t.Fatalf("envio sem chave 2: %v", err)
	}
	if a.ID == b.ID {
		t.Fatal("envios sem chave não deveriam colidir")
	}

	// Chaves diferentes: mensagens distintas.
	if _, _, err := CreateWebhookMessage(testCtx(), webhook, nil, "src-outra", "outra", "", nil, nil); err != nil {
		t.Fatalf("envio com outra chave: %v", err)
	}

	// sourceID acima de 200 caracteres é rejeitado.
	if _, _, err := CreateWebhookMessage(testCtx(), webhook, nil, strings.Repeat("s", 201), "x", "", nil, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("sourceID longo deveria dar ErrInvalidInput, recebeu %v", err)
	}
}

// TestWebhookMessageOwnershipEditDelete cobre a posse: somente o webhook autor
// edita/exclui suas mensagens; humanos e outros webhooks são barrados.
func TestWebhookMessageOwnershipEditDelete(t *testing.T) {
	user := newWebhookTestUser(t)
	defer deleteWebhookTestUser(t, user)
	other := newWebhookTestUser(t)
	defer deleteWebhookTestUser(t, other)
	channel := newWebhookTestChannel(t, "text")

	hookA, _, err := CreateWebhook(testCtx(), user.ID, channel.ID, "hook-a")
	if err != nil {
		t.Fatalf("CreateWebhook A: %v", err)
	}
	hookB, _, err := CreateWebhook(testCtx(), other.ID, channel.ID, "hook-b")
	if err != nil {
		t.Fatalf("CreateWebhook B: %v", err)
	}

	msg, _, err := CreateWebhookMessage(testCtx(), hookA, nil, "own-1", "original", "", nil, nil)
	if err != nil {
		t.Fatalf("CreateWebhookMessage: %v", err)
	}

	if _, err := EditWebhookMessage(testCtx(), hookB, msg.ID, "sequestrada", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("outro webhook não deveria editar, recebeu %v", err)
	}
	if _, err := EditMessage(testCtx(), msg.ID, user.ID, "humano editou", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("humano não deveria editar mensagem de webhook, recebeu %v", err)
	}
	if _, err := EditWebhookMessage(testCtx(), hookA, msg.ID, "editada pelo autor", nil); err != nil {
		t.Fatalf("webhook autor deveria poder editar, recebeu %v", err)
	}

	if _, err := DeleteWebhookMessage(testCtx(), hookB, msg.ID); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("outro webhook não deveria excluir, recebeu %v", err)
	}
	// O criador da credencial também não apaga pelo fluxo humano sem delete_messages.
	if _, err := DeleteMessage(testCtx(), msg.ID, user.ID); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("fluxo humano sem delete_messages deveria falhar, recebeu %v", err)
	}
	if channelID, err := DeleteWebhookMessage(testCtx(), hookA, msg.ID); err != nil {
		t.Fatalf("webhook autor deveria poder excluir, recebeu %v", err)
	} else if channelID != channel.ID {
		t.Fatalf("channel_id retornado incorreto: %s", channelID)
	}
	if _, err := DeleteWebhookMessage(testCtx(), hookA, msg.ID); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("exclusão duplicada deveria dar ErrMessageNotFound, recebeu %v", err)
	}
}

// TestWebhookMessageChannelScope garante que a mensagem do webhook vai para o
// canal do webhook e que reply_to de outro canal é rejeitado.
func TestWebhookMessageChannelScope(t *testing.T) {
	user := newWebhookTestUser(t)
	defer deleteWebhookTestUser(t, user)
	channelA := newWebhookTestChannel(t, "text")
	channelB := newWebhookTestChannel(t, "text")

	hook, _, err := CreateWebhook(testCtx(), user.ID, channelA.ID, "escopo")
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	humanMsg, err := CreateMessage(testCtx(), channelB.ID, testActorID(), "mensagem de outro canal", "", nil, nil)
	if err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	if _, _, err := CreateWebhookMessage(testCtx(), hook, nil, "scope-1", "reply cruzado", humanMsg.ID, nil, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("reply_to de outro canal deveria dar ErrInvalidInput, recebeu %v", err)
	}

	msg, _, err := CreateWebhookMessage(testCtx(), hook, nil, "scope-2", "no meu canal", "", nil, nil)
	if err != nil {
		t.Fatalf("CreateWebhookMessage: %v", err)
	}
	if msg.ChannelID != channelA.ID {
		t.Fatalf("mensagem fora do canal do webhook: %s", msg.ChannelID)
	}
	if msg.WebhookID == nil || *msg.WebhookID != hook.ID {
		t.Fatalf("webhook_id não gravado: %+v", msg.WebhookID)
	}
}

// TestPinnedWebhookMessageAttribution garante que a listagem de mensagens
// pinadas carrega a atribuição externa, como a listagem normal do canal.
// O dono do servidor fica referenciado no audit log append-only e não é
// removido na limpeza (mesma convenção dos demais testes do pacote).
func TestPinnedWebhookMessageAttribution(t *testing.T) {
	if err := cleanServers(testCtx()); err != nil {
		t.Fatalf("cleanServers: %v", err)
	}
	owner := newWebhookTestUser(t)
	if _, err := storage.CreateServer(testCtx(), "server_"+randHex(4), &owner.ID); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	defer func() {
		deleteWebhookTestUser(t, owner)
		if err := cleanServers(testCtx()); err != nil {
			t.Errorf("cleanServers final: %v", err)
		}
	}()

	channel := newWebhookTestChannel(t, "text")
	hook, _, err := CreateWebhook(testCtx(), owner.ID, channel.ID, "pinável")
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	msg, _, err := CreateWebhookMessage(testCtx(), hook, nil, "pin-1", "mensagem pinável", "", nil, nil)
	if err != nil {
		t.Fatalf("CreateWebhookMessage: %v", err)
	}

	if _, _, err := PinMessage(testCtx(), channel.ID, msg.ID, owner.ID); err != nil {
		t.Fatalf("PinMessage: %v", err)
	}

	pinned, err := storage.ListPinnedMessagesWithAttachmentsByChannel(testCtx(), channel.ID)
	if err != nil {
		t.Fatalf("ListPinnedMessages: %v", err)
	}
	if len(pinned) != 1 {
		t.Fatalf("esperava 1 mensagem pinada, tem %d", len(pinned))
	}
	pin := pinned[0]
	if pin.WebhookID == nil || *pin.WebhookID != hook.ID {
		t.Fatalf("webhook_id ausente na mensagem pinada: %+v", pin.WebhookID)
	}
	if pin.ExternalAuthor == nil || pin.ExternalAuthor.Name != "pinável" {
		t.Fatalf("external_author ausente na mensagem pinada: %+v", pin.ExternalAuthor)
	}
}
