package services

import (
	"errors"
	"fmt"
	"testing"

	"papo/internal/storage"
	"papo/internal/utils"
)

func TestUserSummariesBatchIncludesBannedAndPreservesOrder(t *testing.T) {
	if err := cleanServers(testCtx()); err != nil {
		t.Fatalf("cleanServers: %v", err)
	}
	first, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil { t.Fatalf("Register first: %v", err) }
	second, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil { t.Fatalf("Register second: %v", err) }

	if owner, err := storage.SetUserBanned(testCtx(), second.ID, true); err != nil || owner {
		t.Fatalf("SetUserBanned: owner=%v err=%v", owner, err)
	}

	got, err := UserSummariesBatch(testCtx(), []string{second.ID, first.ID, second.ID})
	if err != nil { t.Fatalf("UserSummariesBatch: %v", err) }
	if len(got) != 2 || got[0].ID != second.ID || got[1].ID != first.ID {
		t.Fatalf("ordem/deduplicação inesperada: %+v", got)
	}
	if !got[0].Banned {
		t.Fatalf("UserSummary não expôs banned=true")
	}
}

func TestCreateMessageRejectsCategoryAndAllowsVoice(t *testing.T) {
	if err := cleanServers(testCtx()); err != nil {
		t.Fatalf("cleanServers: %v", err)
	}
	owner, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil { t.Fatalf("Register: %v", err) }
	if _, err := storage.CreateServer(testCtx(), newRandomServerName(), &owner.ID); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}

	category, err := storage.CreateChannel(testCtx(), newRandomChannelName(), "category", "")
	if err != nil { t.Fatalf("CreateChannel category: %v", err) }
	if _, err := CreateMessage(testCtx(), category.ID, owner.ID, "não permitido", "", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("categoria deveria rejeitar mensagem com ErrPermissionDenied, obtive %v", err)
	}

	voice, err := storage.CreateChannel(testCtx(), newRandomChannelName(), "voice", "")
	if err != nil { t.Fatalf("CreateChannel voice: %v", err) }
	if _, err := CreateMessage(testCtx(), voice.ID, owner.ID, "permitido", "", nil); err != nil {
		t.Fatalf("voice deveria aceitar mensagem: %v", err)
	}
}


func TestTranslatePushMentionsPrefersNicknameThenUsername(t *testing.T) {
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil { t.Fatalf("Register: %v", err) }

	nickname := "apelido"
	user.Nickname = &nickname
	if _, err := storage.UpdateUser(testCtx(), user.ID, user); err != nil {
		t.Fatalf("UpdateUser nickname: %v", err)
	}

	content := fmt.Sprintf("oi @mention(<@%s>)", user.ID)
	if got := translatePushMentions(testCtx(), content); got != "oi @apelido" {
		t.Fatalf("menção com nickname = %q", got)
	}

	user.Nickname = nil
	if _, err := storage.UpdateUser(testCtx(), user.ID, user); err != nil {
		t.Fatalf("UpdateUser remove nickname: %v", err)
	}
	if got := translatePushMentions(testCtx(), content); got != "oi @"+user.Username {
		t.Fatalf("fallback para username = %q", got)
	}
}


func TestServerPasswordChangeRevokesAllSessions(t *testing.T) {
	if err := cleanServers(testCtx()); err != nil {
		t.Fatalf("cleanServers: %v", err)
	}

	owner, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("Register owner: %v", err)
	}
	other, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("Register other: %v", err)
	}

	oldPassword := newRandomPassword()
	if _, err := CreateServerWithIcon(testCtx(), newRandomServerName(), "", "", false, &oldPassword, &owner.ID); err != nil {
		t.Fatalf("CreateServerWithIcon: %v", err)
	}

	ownerToken, _, err := CreateSessionConnection(testCtx(), owner.ID)
	if err != nil {
		t.Fatalf("CreateSessionConnection owner: %v", err)
	}
	otherToken, _, err := CreateSessionConnection(testCtx(), other.ID)
	if err != nil {
		t.Fatalf("CreateSessionConnection other: %v", err)
	}

	newPassword := newRandomPassword()
	result, err := PatchServerWithResult(testCtx(), owner.ID, nil, nil, nil, nil, &newPassword)
	if err != nil {
		t.Fatalf("PatchServerWithResult: %v", err)
	}
	if !result.PasswordChanged {
		t.Fatal("esperava PasswordChanged=true")
	}

	for name, session := range map[string]struct {
		userID string
		token  string
	}{
		"owner": {owner.ID, ownerToken},
		"other": {other.ID, otherToken},
	} {
		if err := storage.CheckUserConnection(testCtx(), session.userID, utils.HashToken(session.token)); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("%s: sessão deveria estar revogada, obtive %v", name, err)
		}
	}
}

func TestServerPasswordUnchangedPreservesSessions(t *testing.T) {
	if err := cleanServers(testCtx()); err != nil {
		t.Fatalf("cleanServers: %v", err)
	}

	owner, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("Register owner: %v", err)
	}

	password := newRandomPassword()
	if _, err := CreateServerWithIcon(testCtx(), newRandomServerName(), "", "", false, &password, &owner.ID); err != nil {
		t.Fatalf("CreateServerWithIcon: %v", err)
	}

	token, _, err := CreateSessionConnection(testCtx(), owner.ID)
	if err != nil {
		t.Fatalf("CreateSessionConnection: %v", err)
	}

	result, err := PatchServerWithResult(testCtx(), owner.ID, nil, nil, nil, nil, &password)
	if err != nil {
		t.Fatalf("PatchServerWithResult: %v", err)
	}
	if result.PasswordChanged {
		t.Fatal("esperava PasswordChanged=false para a mesma senha")
	}
	if err := storage.CheckUserConnection(testCtx(), owner.ID, utils.HashToken(token)); err != nil {
		t.Fatalf("sessão deveria continuar ativa, obtive %v", err)
	}
}
