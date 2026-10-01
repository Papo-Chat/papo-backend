package storage

import "testing"

func TestDirectConversationLifecycleAndBlocks(t *testing.T) {
	a := newTestUser(t)
	b := newTestUser(t)
	defer func() {
		_, _ = GetDB().ExecContext(testCtx(), "DELETE FROM users WHERE id = $1", a.ID)
		_, _ = GetDB().ExecContext(testCtx(), "DELETE FROM users WHERE id = $1", b.ID)
	}()

	channelID, created, err := CreateOrShowDirectConversation(testCtx(), a.ID, b.ID)
	if err != nil {
		t.Fatalf("CreateOrShowDirectConversation: %v", err)
	}
	if !created {
		t.Fatal("esperava DM nova")
	}

	aList, err := ListDirectConversations(testCtx(), a.ID)
	if err != nil {
		t.Fatalf("ListDirectConversations(a): %v", err)
	}
	if len(aList) != 1 || aList[0].ID != channelID {
		t.Fatalf("DM deveria estar visível para o iniciador: %#v", aList)
	}

	bList, err := ListDirectConversations(testCtx(), b.ID)
	if err != nil {
		t.Fatalf("ListDirectConversations(b): %v", err)
	}
	if len(bList) != 0 {
		t.Fatalf("DM vazia não deve aparecer para o destinatário: %#v", bList)
	}

	sameID, created, err := CreateOrShowDirectConversation(testCtx(), b.ID, a.ID)
	if err != nil {
		t.Fatalf("reabrir DM invertida: %v", err)
	}
	if created || sameID != channelID {
		t.Fatalf("par invertido deve reutilizar a mesma DM: created=%v id=%s want=%s", created, sameID, channelID)
	}

	if err := HideDirectConversation(testCtx(), channelID, a.ID); err != nil {
		t.Fatalf("HideDirectConversation: %v", err)
	}
	aList, _ = ListDirectConversations(testCtx(), a.ID)
	if len(aList) != 0 {
		t.Fatal("DM fechada não deve aparecer na lista")
	}

	if err := ShowDirectConversationForMembers(testCtx(), channelID); err != nil {
		t.Fatalf("ShowDirectConversationForMembers: %v", err)
	}
	aList, _ = ListDirectConversations(testCtx(), a.ID)
	bList, _ = ListDirectConversations(testCtx(), b.ID)
	if len(aList) != 1 || len(bList) != 1 {
		t.Fatalf("nova mensagem deve reabrir DM para ambos: a=%d b=%d", len(aList), len(bList))
	}

	if err := CreateUserBlock(testCtx(), a.ID, b.ID); err != nil {
		t.Fatalf("CreateUserBlock: %v", err)
	}
	blocked, err := UsersBlocked(testCtx(), b.ID, a.ID)
	if err != nil || !blocked {
		t.Fatalf("bloqueio deve valer nas duas direções: blocked=%v err=%v", blocked, err)
	}
	readers, err := DirectConversationReaders(testCtx(), channelID, []string{a.ID, b.ID})
	if err != nil {
		t.Fatalf("DirectConversationReaders: %v", err)
	}
	if readers[a.ID] || readers[b.ID] {
		t.Fatalf("DM bloqueada não deve ter leitores: %#v", readers)
	}

	aList, _ = ListDirectConversations(testCtx(), a.ID)
	bList, _ = ListDirectConversations(testCtx(), b.ID)
	if len(aList) != 0 || len(bList) != 0 {
		t.Fatal("DM bloqueada deve desaparecer das listas")
	}

	if err := DeleteUserBlock(testCtx(), a.ID, b.ID); err != nil {
		t.Fatalf("DeleteUserBlock: %v", err)
	}
	blocked, err = UsersBlocked(testCtx(), a.ID, b.ID)
	if err != nil || blocked {
		t.Fatalf("desbloqueio não aplicado: blocked=%v err=%v", blocked, err)
	}

	// Desbloquear não reabre automaticamente uma conversa previamente escondida.
	aList, _ = ListDirectConversations(testCtx(), a.ID)
	if len(aList) != 0 {
		t.Fatal("desbloquear não deve reabrir a DM automaticamente")
	}
}

func TestUserBlockRejectsSelfAtDatabaseConstraint(t *testing.T) {
	u := newTestUser(t)
	defer func() { _, _ = GetDB().ExecContext(testCtx(), "DELETE FROM users WHERE id = $1", u.ID) }()
	if err := CreateUserBlock(testCtx(), u.ID, u.ID); err == nil {
		t.Fatal("esperava erro ao bloquear a si mesmo no storage")
	}
}
