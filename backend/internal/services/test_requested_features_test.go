package services

import (
	"errors"
	"testing"

	"papo/internal/storage"
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
