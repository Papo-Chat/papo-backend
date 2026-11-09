package storage

import (
	"testing"
)

// TestDeletingCategoryDetachesItsChannels garante o que a migration 020
// promete: excluir uma categoria deixa os canais dela sem categoria
// (ON DELETE SET NULL) em vez de excluí-los, e uma categoria não pode ter
// categoria nem por escrita direta no banco (channels_parent_only_children).
func TestDeletingCategoryDetachesItsChannels(t *testing.T) {
	_ = newTestServer(t, nil)
	category, err := CreateChannel(testCtx(), "cat_"+randHex(7), "category", "")
	if err != nil {
		t.Fatalf("falha ao criar categoria: %v", err)
	}
	child, err := CreateChannelWithOptions(testCtx(), "chn_"+randHex(7), "text", "", &category.ID, nil)
	if err != nil {
		t.Fatalf("falha ao criar canal na categoria: %v", err)
	}
	if child.ParentID == nil || *child.ParentID != category.ID {
		t.Fatalf("esperava parent_id %s, obtive %v", category.ID, child.ParentID)
	}

	other, err := CreateChannel(testCtx(), "cat_"+randHex(7), "category", "")
	if err != nil {
		t.Fatalf("falha ao criar categoria: %v", err)
	}
	if _, err := GetDB().ExecContext(testCtx(),
		"UPDATE channels SET parent_id = $1 WHERE id = $2", category.ID, other.ID,
	); err == nil {
		t.Error("o banco aceitou uma categoria dentro de outra")
	}

	if err := DeleteChannel(testCtx(), category.ID); err != nil {
		t.Fatalf("falha ao excluir categoria: %v", err)
	}
	detached, err := GetChannelByID(testCtx(), child.ID)
	if err != nil {
		t.Fatalf("o canal da categoria excluída deveria continuar existindo: %v", err)
	}
	if detached.ParentID != nil {
		t.Errorf("esperava o canal sem categoria, obtive parent_id %s", *detached.ParentID)
	}
}
