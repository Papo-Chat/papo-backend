package services

import (
	"errors"
	"testing"

	"papo/internal/storage"
)

func TestDirectMessagesReuseMessagePipelineAndStayPrivate(t *testing.T) {
	if err := cleanServers(testCtx()); err != nil {
		t.Fatalf("cleanServers: %v", err)
	}

	owner := newTestUser(t)
	a := newTestUser(t)
	b := newTestUser(t)
	if _, err := storage.CreateServer(testCtx(), "server_"+randHex(8), &owner.ID); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}

	dm, created, err := OpenDirectConversation(testCtx(), a.ID, b.ID)
	if err != nil {
		t.Fatalf("OpenDirectConversation: %v", err)
	}
	if !created {
		t.Fatal("esperava DM nova")
	}

	// O backing channel não pode vazar para navegação/admin nem para o contador.
	channels, err := ListChannels(testCtx(), a.ID)
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	for _, channel := range channels {
		if channel.ID == dm.ID || channel.Type == "dm" {
			t.Fatal("DM vazou em ListChannels")
		}
	}
	count, err := storage.CountChannels(testCtx())
	if err != nil {
		t.Fatalf("CountChannels: %v", err)
	}
	if count != 0 {
		t.Fatalf("backing DM não deve contar como canal administrável: %d", count)
	}

	msg, err := CreateMessage(testCtx(), dm.ID, a.ID, "oi", "", nil)
	if err != nil {
		t.Fatalf("CreateMessage em DM: %v", err)
	}
	if msg.ChannelID != dm.ID {
		t.Fatalf("mensagem gravada no canal errado: %s", msg.ChannelID)
	}

	list, err := ListMessages(testCtx(), dm.ID, b.ID, nil, "")
	if err != nil || len(list.Messages) != 1 {
		t.Fatalf("participante deveria ler a DM: len=%d err=%v", len(list.Messages), err)
	}

	// Nem o dono do servidor recebe acesso implícito a uma conversa privada.
	if _, err := ListMessages(testCtx(), dm.ID, owner.ID, nil, ""); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("owner não participante deveria receber ErrPermissionDenied, recebeu %v", err)
	}

	// O outro participante não ganha delete_messages sobre mensagens alheias.
	if _, err := DeleteMessage(testCtx(), msg.ID, b.ID); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("peer não deve excluir mensagem alheia, recebeu %v", err)
	}

	if err := BlockUser(testCtx(), a.ID, b.ID); err != nil {
		t.Fatalf("BlockUser: %v", err)
	}
	if _, err := CreateMessage(testCtx(), dm.ID, b.ID, "não deve enviar", "", nil); !errors.Is(err, ErrDirectMessageBlocked) {
		t.Fatalf("envio em DM bloqueada deveria falhar com ErrDirectMessageBlocked, recebeu %v", err)
	}
	if _, err := GetDirectConversation(testCtx(), b.ID, dm.ID); !errors.Is(err, ErrDirectMessageBlocked) {
		t.Fatalf("GET de DM bloqueada deveria falhar, recebeu %v", err)
	}
}

func TestOpenDirectConversationEdgeCases(t *testing.T) {
	u := newTestUser(t)
	if _, _, err := OpenDirectConversation(testCtx(), u.ID, u.ID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("DM consigo mesmo deveria ser inválida, recebeu %v", err)
	}
	if err := BlockUser(testCtx(), u.ID, u.ID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bloquear a si mesmo deveria ser inválido, recebeu %v", err)
	}
}
