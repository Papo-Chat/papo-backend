package services

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

// TestCleanupStalePreviewRateBuckets garante que a limpeza remove os buckets
// de preview por usuário sem uso há mais que previewRateUserTTL e mantém os
// usados recentemente (o previewRateUsers não pode crescer sem limite).
func TestCleanupStalePreviewRateBuckets(t *testing.T) {
	oldTTL := previewRateUserTTL
	previewRateUserTTL = time.Hour
	t.Cleanup(func() { previewRateUserTTL = oldTTL })

	staleBucket := newTokenBucket(10)
	staleBucket.last = time.Now().Add(-2 * time.Hour) // sem uso há 2h
	freshBucket := newTokenBucket(10)

	previewRateUsers.Store("user_stale", staleBucket)
	previewRateUsers.Store("user_fresh", freshBucket)
	t.Cleanup(func() {
		previewRateUsers.Delete("user_stale")
		previewRateUsers.Delete("user_fresh")
	})

	removed := cleanupStalePreviewRateBuckets()
	if removed != 1 {
		t.Errorf("esperava 1 bucket removido, obtive %d", removed)
	}
	if _, ok := previewRateUsers.Load("user_stale"); ok {
		t.Errorf("bucket sem uso há mais que o TTL deveria ter sido removido")
	}
	if _, ok := previewRateUsers.Load("user_fresh"); !ok {
		t.Errorf("bucket usado recentemente deveria ter sido mantido")
	}
}

// mediaHashOf calcula o hmac-sha256 (hex) do conteúdo, como StoreMediaFromBytes.
func mediaHashOf(t *testing.T, content []byte) string {
	t.Helper()
	cfg := config.LoadConfig()
	mac := hmac.New(sha256.New, []byte(cfg.HMACSecret))
	mac.Write(content)
	return hex.EncodeToString(mac.Sum(nil))
}

// TestCleanupMediaUnreferenced garante que o GC remove mídia antiga sem
// referência (row + arquivo) — o estado que sobra quando avatar/banner é
// substituído, ícone trocado ou mensagem/emoji removido — e mantém: mídia
// antiga ainda referenciada, mídia recente (janela de proteção) e blob
// compartilhado ainda referenciado por outro usuário.
func TestCleanupMediaUnreferenced(t *testing.T) {
	withTempMediaDir(t)

	// row antiga + arquivo, sem referência → row e arquivo removidos
	unreferenced := randHex(32)
	insertOldMediaRow(t, unreferenced)
	unrefPath := writeMediaBlob(t, unreferenced, []byte("sem referência"))

	// row antiga + arquivo, referenciada por avatar → mantida
	referenced := randHex(32)
	insertOldMediaRow(t, referenced)
	writeMediaBlob(t, referenced, []byte("referenciado"))
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário de apoio: %v", err)
	}
	if _, err := storage.GetDB().ExecContext(testCtx(),
		"UPDATE users SET avatar_media = $2 WHERE id = $1", user.ID, referenced); err != nil {
		t.Fatalf("falha ao referenciar a mídia no avatar: %v", err)
	}

	// row recente + arquivo, sem referência (upload em andamento) → mantida
	recent := randHex(32)
	if _, _, err := storage.InsertMediaIfAbsent(testCtx(), recent, "image/png", 10); err != nil {
		t.Fatalf("falha ao inserir row de mídia: %v", err)
	}
	writeMediaBlob(t, recent, []byte("recente"))

	// blob compartilhado: banner do 1º usuário e avatar do 2º referenciam; o
	// banner remove a referência → ainda referenciado pelo avatar → mantido
	shared := randHex(32)
	insertOldMediaRow(t, shared)
	writeMediaBlob(t, shared, []byte("compartilhado"))
	user2, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar 2º usuário de apoio: %v", err)
	}
	if _, err := storage.GetDB().ExecContext(testCtx(),
		"UPDATE users SET banner_media = $2 WHERE id = $1", user.ID, shared); err != nil {
		t.Fatalf("falha ao referenciar a mídia compartilhada no banner: %v", err)
	}
	if _, err := storage.GetDB().ExecContext(testCtx(),
		"UPDATE users SET avatar_media = $2 WHERE id = $1", user2.ID, shared); err != nil {
		t.Fatalf("falha ao referenciar a mídia compartilhada no avatar: %v", err)
	}
	if _, err := storage.GetDB().ExecContext(testCtx(),
		"UPDATE users SET banner_media = NULL WHERE id = $1", user.ID); err != nil {
		t.Fatalf("falha ao remover a referência do banner: %v", err)
	}

	if err := cleanupMedia(testCtx()); err != nil {
		t.Fatalf("cleanupMedia retornou erro: %v", err)
	}

	if mediaRowExists(t, unreferenced) {
		t.Errorf("row antiga sem referência deveria ter sido removida")
	}
	if _, err := os.Stat(unrefPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("arquivo sem referência deveria ter sido removido (stat err = %v)", err)
	}
	if !mediaRowExists(t, referenced) {
		t.Errorf("row referenciada deveria ter sido mantida")
	}
	if !mediaRowExists(t, recent) {
		t.Errorf("row recente (janela de proteção) deveria ter sido mantida")
	}
	if !mediaRowExists(t, shared) {
		t.Errorf("blob compartilhado ainda referenciado deveria ter sido mantido")
	}
}

// TestCleanupMediaAfterAvatarSwap garante o cenário de DoS de disco: trocar o
// avatar repetidamente não acumula blobs — o avatar antigo (sem referência)
// tem row e arquivo removidos pelo GC, e o avatar atual é mantido.
func TestCleanupMediaAfterAvatarSwap(t *testing.T) {
	withTempMediaDir(t)

	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}

	// Dimensões incomuns: hashes não colidem com mídia de outros testes.
	oldAvatar := pngAvatarBytes(131, 173)
	newAvatar := pngAvatarBytes(149, 191)
	for _, av := range [][]byte{oldAvatar, newAvatar} {
		if err := UpdateAvatar(testCtx(), user.ID, base64.StdEncoding.EncodeToString(av), "png"); err != nil {
			t.Fatalf("UpdateAvatar retornou erro: %v", err)
		}
	}

	oldHash := mediaHashOf(t, oldAvatar)
	newHash := mediaHashOf(t, newAvatar)

	// Retrocede o created_at do blob antigo além da janela de proteção
	// (simula uma troca feita há mais que mediaGracePeriod).
	if _, err := storage.GetDB().ExecContext(testCtx(),
		"UPDATE media SET created_at = now() - interval '2 hours' WHERE sha_hash = $1", oldHash); err != nil {
		t.Fatalf("falha ao retroceder a row de mídia antiga: %v", err)
	}

	if err := cleanupMedia(testCtx()); err != nil {
		t.Fatalf("cleanupMedia retornou erro: %v", err)
	}

	if mediaRowExists(t, oldHash) {
		t.Errorf("avatar antigo (sem referência) deveria ter tido a row removida")
	}
	if _, err := os.Stat(mediaBlobPath(oldHash)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("arquivo do avatar antigo deveria ter sido removido (stat err = %v)", err)
	}
	if !mediaRowExists(t, newHash) {
		t.Errorf("avatar atual deveria ter sido mantido")
	}
	if _, err := os.Stat(mediaBlobPath(newHash)); err != nil {
		t.Errorf("arquivo do avatar atual deveria ter sido mantido: %v", err)
	}
}
