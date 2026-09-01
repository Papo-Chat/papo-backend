package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"papo/internal/config"
	"papo/internal/storage"
)

// withTempMediaDir aponta mediaBaseDir para uma pasta temporária durante o
// teste e restaura o valor original ao final (os testes não podem tocar a
// pasta media/ compartilhada do pacote).
func withTempMediaDir(t *testing.T) {
	t.Helper()
	old := mediaBaseDir
	mediaBaseDir = t.TempDir()
	t.Cleanup(func() { mediaBaseDir = old })
}

// writeMediaBlob grava o blob content-addressable em disco no caminho do hash
// (media/<ab>/<cd>/<hash>) e retorna o caminho.
func writeMediaBlob(t *testing.T, hash string, content []byte) string {
	t.Helper()
	path := mediaBlobPath(hash)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("falha ao criar diretório do blob: %v", err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("falha ao gravar o blob: %v", err)
	}
	return path
}

// setOldMtime retrocede o mtime do arquivo (simula um estado que já excedeu a
// janela de proteção do GC).
func setOldMtime(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("falha ao ajustar mtime: %v", err)
	}
}

// insertOldMediaRow insere uma row de media com created_at antigo (além da
// janela de proteção), sem gravar arquivo em disco.
func insertOldMediaRow(t *testing.T, hash string) {
	t.Helper()
	if _, err := storage.GetDB().ExecContext(testCtx(),
		"INSERT INTO media (sha_hash, mime_type, size_bytes, created_at) VALUES ($1, 'image/png', 10, now() - interval '2 hours')",
		hash,
	); err != nil {
		t.Fatalf("falha ao inserir row de mídia: %v", err)
	}
}

// mediaRowExists indica se a row de mídia existe no banco.
func mediaRowExists(t *testing.T, hash string) bool {
	t.Helper()
	_, err := storage.GetMediaByHash(testCtx(), hash)
	if errors.Is(err, storage.ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("falha ao consultar row de mídia: %v", err)
	}
	return true
}

// TestCleanupMediaOrphanFiles garante que o GC remove arquivos sem row na
// tabela media (incluindo temporários .upload-*), respeitando a janela de
// proteção (mtime recente é mantido) e preservando arquivos com row.
func TestCleanupMediaOrphanFiles(t *testing.T) {
	withTempMediaDir(t)

	// arquivo órfão antigo (sem row) → removido
	orphan := randHex(32)
	orphanPath := writeMediaBlob(t, orphan, []byte("órfão"))
	setOldMtime(t, orphanPath)

	// arquivo sem row, mtime recente (upload em andamento) → mantido
	recent := randHex(32)
	recentPath := writeMediaBlob(t, recent, []byte("recente"))

	// arquivo com row → mantido
	legit := randHex(32)
	legitPath := writeMediaBlob(t, legit, []byte("legítimo"))
	if _, _, err := storage.InsertMediaIfAbsent(testCtx(), legit, "image/png", int64(len([]byte("legítimo")))); err != nil {
		t.Fatalf("falha ao inserir row de mídia: %v", err)
	}
	setOldMtime(t, legitPath)

	// temporário de upload interrompido (sem row) → removido
	tmpPath := filepath.Join(mediaBaseDir, ".upload-teste")
	if err := os.WriteFile(tmpPath, []byte("temp"), 0o644); err != nil {
		t.Fatalf("falha ao gravar temporário: %v", err)
	}
	setOldMtime(t, tmpPath)

	if err := cleanupMedia(testCtx()); err != nil {
		t.Fatalf("cleanupMedia retornou erro: %v", err)
	}

	if _, err := os.Stat(orphanPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("arquivo órfão antigo deveria ter sido removido (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Dir(orphanPath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("subpasta vazia do arquivo órfão deveria ter sido removida (stat err = %v)", err)
	}
	if _, err := os.Stat(recentPath); err != nil {
		t.Errorf("arquivo recente (janela de proteção) deveria ter sido mantido: %v", err)
	}
	if _, err := os.Stat(legitPath); err != nil {
		t.Errorf("arquivo com row deveria ter sido mantido: %v", err)
	}
	if _, err := os.Stat(tmpPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temporário órfão antigo deveria ter sido removido (stat err = %v)", err)
	}
}

// TestCleanupMediaRowWithoutFile garante que o GC remove rows de media antigas
// sem arquivo no disco quando sem referência, e mantém rows referenciadas
// (conteúdo perdido: a FK impede a remoção) e rows recentes (janela de
// proteção).
func TestCleanupMediaRowWithoutFile(t *testing.T) {
	withTempMediaDir(t)

	// row antiga sem arquivo e sem referência → removida
	unreferenced := randHex(32)
	insertOldMediaRow(t, unreferenced)

	// row antiga sem arquivo, referenciada por avatar → mantida
	referenced := randHex(32)
	insertOldMediaRow(t, referenced)
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário de apoio: %v", err)
	}
	if _, err := storage.GetDB().ExecContext(testCtx(),
		"UPDATE users SET avatar_media = $2 WHERE id = $1", user.ID, referenced); err != nil {
		t.Fatalf("falha ao referenciar a mídia no avatar: %v", err)
	}

	// row recente sem arquivo (upload em andamento) → mantida
	recent := randHex(32)
	if _, _, err := storage.InsertMediaIfAbsent(testCtx(), recent, "image/png", 10); err != nil {
		t.Fatalf("falha ao inserir row de mídia: %v", err)
	}

	if err := cleanupMedia(testCtx()); err != nil {
		t.Fatalf("cleanupMedia retornou erro: %v", err)
	}

	if mediaRowExists(t, unreferenced) {
		t.Errorf("row antiga sem arquivo e sem referência deveria ter sido removida")
	}
	if !mediaRowExists(t, referenced) {
		t.Errorf("row referenciada deveria ter sido mantida (conteúdo perdido, FK impede remoção)")
	}
	if !mediaRowExists(t, recent) {
		t.Errorf("row recente (janela de proteção) deveria ter sido mantida")
	}
}

// TestCleanupMediaOrphanAttachments garante que o GC remove attachments
// órfãos (messages_id NULL) antigos — órfãos de uma gravação incompleta de
// mensagem — e mantém os recentes (janela de proteção).
func TestCleanupMediaOrphanAttachments(t *testing.T) {
	withTempMediaDir(t)

	insertOrphanAttachment := func(t *testing.T, old bool) string {
		t.Helper()
		mediaHash := randHex(32)
		if _, _, err := storage.InsertMediaIfAbsent(testCtx(), mediaHash, "text/plain", 10); err != nil {
			t.Fatalf("falha ao inserir row de mídia: %v", err)
		}
		created := "now()"
		if old {
			created = "now() - interval '2 hours'"
		}
		var id string
		err := storage.GetDB().QueryRowContext(testCtx(),
			"INSERT INTO attachments (original_file_name, media_sha_hash, created_at) VALUES ('orfa.txt', $1, "+created+") RETURNING id",
			mediaHash,
		).Scan(&id)
		if err != nil {
			t.Fatalf("falha ao inserir attachment órfão: %v", err)
		}
		return id
	}

	attachmentExists := func(t *testing.T, id string) bool {
		t.Helper()
		var exists bool
		err := storage.GetDB().QueryRowContext(testCtx(),
			"SELECT EXISTS (SELECT 1 FROM attachments WHERE id = $1)", id,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("falha ao consultar attachment: %v", err)
		}
		return exists
	}

	oldID := insertOrphanAttachment(t, true)
	recentID := insertOrphanAttachment(t, false)

	if err := cleanupMedia(testCtx()); err != nil {
		t.Fatalf("cleanupMedia retornou erro: %v", err)
	}

	if attachmentExists(t, oldID) {
		t.Errorf("attachment órfão antigo deveria ter sido removido")
	}
	if !attachmentExists(t, recentID) {
		t.Errorf("attachment recente (janela de proteção) deveria ter sido mantido")
	}
}

// TestPurgeAuditLogs garante que o purge remove apenas logs além da retenção
// (LOG_DURATION), mantém os demais e recria a trigger append-only ao final.
func TestPurgeAuditLogs(t *testing.T) {
	insertLog := func(t *testing.T, age time.Duration) string {
		t.Helper()
		var id string
		err := storage.GetDB().QueryRowContext(testCtx(),
			"INSERT INTO audit_logs (actor_username, action, entity_type, created_at) VALUES ('purge_test', 'test.purge', 'user', $1) RETURNING id",
			time.Now().Add(-age),
		).Scan(&id)
		if err != nil {
			t.Fatalf("falha ao inserir log de auditoria: %v", err)
		}
		return id
	}

	logExists := func(t *testing.T, id string) bool {
		t.Helper()
		var exists bool
		err := storage.GetDB().QueryRowContext(testCtx(),
			"SELECT EXISTS (SELECT 1 FROM audit_logs WHERE id = $1)", id,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("falha ao consultar log de auditoria: %v", err)
		}
		return exists
	}

	expired := insertLog(t, 100*24*time.Hour) // além da retenção de 90 dias
	within := insertLog(t, 45*24*time.Hour)   // dentro da retenção
	recent := insertLog(t, 1*time.Hour)       // recente

	if err := purgeAuditLogs(testCtx(), 90*24*time.Hour); err != nil {
		t.Fatalf("purgeAuditLogs retornou erro: %v", err)
	}

	if logExists(t, expired) {
		t.Errorf("log além da retenção deveria ter sido removido")
	}
	if !logExists(t, within) {
		t.Errorf("log dentro da retenção deveria ter sido mantido")
	}
	if !logExists(t, recent) {
		t.Errorf("log recente deveria ter sido mantido")
	}

	// A trigger append-only deve ter sido recriada e bloquear DELETE.
	exists, err := storage.AuditTriggerExists(testCtx())
	if err != nil {
		t.Fatalf("falha ao verificar trigger: %v", err)
	}
	if !exists {
		t.Fatalf("trigger append-only deveria ter sido recriada após o purge")
	}
	if _, err := storage.GetDB().ExecContext(testCtx(), "DELETE FROM audit_logs WHERE id = $1", recent); err == nil {
		t.Errorf("DELETE em audit_logs deveria ter sido bloqueado pela trigger")
	}
}

// TestPurgeAuditLogsBatched garante que o purge remove mais de
// auditPurgeBatchSize logs expirados (o laço de lotes deve continuar até não
// sobrar nada) e preserva os logs dentro da retenção.
func TestPurgeAuditLogsBatched(t *testing.T) {
	const expiredCount = auditPurgeBatchSize + 5 // força 2 lotes

	// Logs expirados (além da retenção de 90 dias), inseridos em lote.
	if _, err := storage.GetDB().ExecContext(testCtx(),
		"INSERT INTO audit_logs (actor_username, action, entity_type, created_at) "+
			"SELECT 'purge_batch', 'test.purge_batch', 'user', now() - interval '100 days' "+
			"FROM generate_series(1, $1)",
		expiredCount,
	); err != nil {
		t.Fatalf("falha ao inserir logs expirados: %v", err)
	}

	// Log dentro da retenção → mantido.
	var recentID string
	if err := storage.GetDB().QueryRowContext(testCtx(),
		"INSERT INTO audit_logs (actor_username, action, entity_type, created_at) "+
			"VALUES ('purge_batch', 'test.purge_batch_recent', 'user', now()) RETURNING id",
	).Scan(&recentID); err != nil {
		t.Fatalf("falha ao inserir log recente: %v", err)
	}

	if err := purgeAuditLogs(testCtx(), 90*24*time.Hour); err != nil {
		t.Fatalf("purgeAuditLogs retornou erro: %v", err)
	}

	var expiredLeft int
	if err := storage.GetDB().QueryRowContext(testCtx(),
		"SELECT count(*) FROM audit_logs WHERE action = 'test.purge_batch'",
	).Scan(&expiredLeft); err != nil {
		t.Fatalf("falha ao contar logs restantes: %v", err)
	}
	if expiredLeft != 0 {
		t.Errorf("todos os %d logs expirados deveriam ter sido removidos, %d restante(s)", expiredCount, expiredLeft)
	}

	var recentExists bool
	if err := storage.GetDB().QueryRowContext(testCtx(),
		"SELECT EXISTS (SELECT 1 FROM audit_logs WHERE id = $1)", recentID,
	).Scan(&recentExists); err != nil {
		t.Fatalf("falha ao consultar log recente: %v", err)
	}
	if !recentExists {
		t.Errorf("log dentro da retenção deveria ter sido mantido")
	}
}

// TestPurgeAuditLogTriggerSelfHeal garante que, se a trigger estiver ausente
// (crash em uma purga anterior), o purge a recria antes de prosseguir.
func TestPurgeAuditLogTriggerSelfHeal(t *testing.T) {
	if err := storage.DropAuditLogsTrigger(testCtx()); err != nil {
		t.Fatalf("falha ao remover trigger (simulando crash): %v", err)
	}
	t.Cleanup(func() {
		// Garante o estado append-only mesmo se o teste falhar antes do purge.
		exists, err := storage.AuditTriggerExists(testCtx())
		if err != nil {
			t.Errorf("falha ao verificar trigger no cleanup: %v", err)
			return
		}
		if !exists {
			if err := storage.CreateAuditLogsTrigger(testCtx()); err != nil {
				t.Errorf("falha ao restaurar trigger no cleanup: %v", err)
			}
		}
	})

	if err := purgeAuditLogs(testCtx(), 90*24*time.Hour); err != nil {
		t.Fatalf("purgeAuditLogs retornou erro: %v", err)
	}

	exists, err := storage.AuditTriggerExists(testCtx())
	if err != nil {
		t.Fatalf("falha ao verificar trigger: %v", err)
	}
	if !exists {
		t.Errorf("trigger ausente deveria ter sido recriada (auto-cura)")
	}
}

// TestRunMaintenanceStartsAndStops garante que a rotina roda os jobs
// imediatamente no boot (sem esperar o intervalo de 12h) e retorna quando o
// contexto é cancelado (shutdown do servidor).
func TestRunMaintenanceStartsAndStops(t *testing.T) {
	withTempMediaDir(t)

	// Efeito observável do GC: arquivo órfão antigo (sem row na media).
	orphan := randHex(32)
	orphanPath := writeMediaBlob(t, orphan, []byte("órfão"))
	setOldMtime(t, orphanPath)

	// Efeito observável do purge: log de auditoria além da retenção.
	var expiredID string
	if err := storage.GetDB().QueryRowContext(testCtx(),
		"INSERT INTO audit_logs (actor_username, action, entity_type, created_at) "+
			"VALUES ('run_maintenance', 'test.run_maintenance', 'user', now() - interval '100 days') RETURNING id",
	).Scan(&expiredID); err != nil {
		t.Fatalf("falha ao inserir log expirado: %v", err)
	}

	logExists := func() (bool, error) {
		var exists bool
		err := storage.GetDB().QueryRowContext(testCtx(),
			"SELECT EXISTS (SELECT 1 FROM audit_logs WHERE id = $1)", expiredID,
		).Scan(&exists)
		return exists, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunMaintenance(ctx, &config.Config{LogDuration: 90 * 24 * time.Hour})
	}()

	// Os jobs devem rodar imediatamente (não após 12h): espera os efeitos.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, statErr := os.Stat(orphanPath)
		fileGone := errors.Is(statErr, os.ErrNotExist)
		exists, err := logExists()
		if err != nil {
			t.Fatalf("falha ao consultar log: %v", err)
		}
		if fileGone && !exists {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if _, err := os.Stat(orphanPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("arquivo órfão deveria ter sido removido no run inicial (stat err = %v)", err)
	}
	exists, err := logExists()
	if err != nil {
		t.Fatalf("falha ao consultar log: %v", err)
	}
	if exists {
		t.Errorf("log expirado deveria ter sido removido no run inicial")
	}

	// O cancelamento deve parar o scheduler.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Errorf("RunMaintenance não retornou após o cancelamento do contexto")
	}
}
