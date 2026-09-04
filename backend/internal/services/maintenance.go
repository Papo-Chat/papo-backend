package services

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"papo/internal/config"
	"papo/internal/storage"
	"papo/internal/utils"
)

// maintenanceInterval é o intervalo entre execuções da rotina de manutenção
// (a rotina roda também no boot do servidor).
const maintenanceInterval = 12 * time.Hour

// mediaGracePeriod é a janela de proteção do GC de mídia: estados quebrados
// (mídia sem referência, arquivo sem row, row sem arquivo, attachment sem
// mensagem) só são limpos quando persistem por mais que esta janela, evitando
// apagar uploads em andamento (a gravação não é atômica entre disco e banco).
const mediaGracePeriod = time.Hour

// auditPurgeBatchSize é o limite de rows por DELETE do purge de auditoria
// (lotes pequenos para o vacuum do PostgreSQL acompanhar a geração de
// tuplas mortas).
const auditPurgeBatchSize = 1000

// auditPurgeBatchSleep é a pausa entre lotes cheios do purge de auditoria.
const auditPurgeBatchSleep = 250 * time.Millisecond

// maintenanceRunTimeout limita a duração de uma execução da rotina (um job
// travado não pode segurar o scheduler para sempre).
const maintenanceRunTimeout = 30 * time.Minute

// connectionArchiveDelay é o tempo após a substituição (replaced_at) que uma
// conexão de sessão fica na tabela ativa antes de ser movida para a history.
// Deve ser maior que a janela de graça da rotação (a row precisa continuar
// consultável durante a janela).
const connectionArchiveDelay = 12 * time.Hour

// connectionHistoryRetention é o tempo máximo que uma conexão substituída fica
// na history: após ele, o JWT correspondente já expirou (24h) e o histórico
// não serve mais para detecção de reuso.
const connectionHistoryRetention = 25 * time.Hour

// RunMaintenance inicia a rotina de manutenção: roda os jobs imediatamente e
// depois a cada maintenanceInterval, até o ctx ser cancelado. Os jobs são
// sequenciais e independentes: a falha de um é logada e não bloqueia o outro.
func RunMaintenance(ctx context.Context, cfg *config.Config) {
	run := func() {
		jobCtx, cancel := context.WithTimeout(ctx, maintenanceRunTimeout)
		defer cancel()

		if err := cleanupMedia(jobCtx); err != nil {
			utils.Errorf("manutenção: GC de mídia: %v", err)
		}
		if err := purgeAuditLogs(jobCtx, cfg.LogDuration); err != nil {
			utils.Errorf("manutenção: purge de auditoria: %v", err)
		}
		if err := archiveUserConnections(jobCtx); err != nil {
			utils.Errorf("manutenção: arquivamento de conexões de sessão: %v", err)
		}
		if removed := cleanupStalePreviewRateBuckets(); removed > 0 {
			utils.Infof("manutenção: %d bucket(s) obsoleto(s) de rate limit de preview removido(s)", removed)
		}
	}

	run()

	ticker := time.NewTicker(maintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// cleanupMedia limpa os estados inválidos do storage de mídia (a gravação não
// é atômica entre disco e banco), sempre respeitando a mediaGracePeriod:
//  1. attachments órfãos (messages_id NULL) — órfãos de uma gravação
//     incompleta de mensagem, nunca expostos pela API; a remoção libera a FK
//     para a fase 2;
//  2. mídia antiga sem referência — avatar/banner substituído, ícone do
//     servidor trocado, mensagem/emoji removido: a row é removida e o arquivo
//     também, se existir (sem isso cada substituição acumularia blobs no
//     disco); row ainda referenciada com arquivo perdido é logada e mantida
//     (o conteúdo já está perdido e a FK impede a remoção);
//  3. arquivos órfãos no disco (sem row na tabela media) — inclui
//     temporários .upload-* de upload interrompido; diretórios vazios
//     resultantes são removidos.
func cleanupMedia(ctx context.Context) error {
	cutoff := time.Now().Add(-mediaGracePeriod)

	removedAttachments, err := storage.DeleteOrphanAttachments(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("attachments órfãos: %w", err)
	}
	if removedAttachments > 0 {
		utils.Infof("manutenção: %d attachment(s) órfão(s) removido(s)", removedAttachments)
	}

	staleRows, staleFiles, err := cleanupStaleMedia(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("mídia sem referência: %w", err)
	}

	orphanFiles, err := cleanupOrphanMediaFiles(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("arquivos órfãos: %w", err)
	}

	if staleRows > 0 || staleFiles > 0 || orphanFiles > 0 {
		utils.Infof("manutenção: GC de mídia: %d row(s) e %d arquivo(s) sem referência e %d arquivo(s) órfão(s) removido(s)",
			staleRows, staleFiles, orphanFiles)
	}
	return nil
}

// cleanupStaleMedia remove da tabela media as rows criadas antes do cutoff
// que perderam toda referência (avatar/banner substituído, ícone do servidor
// trocado, mensagem com attachments removida, emoji removido) e o arquivo
// correspondente, se existir. Rows ainda referenciadas são mantidas; as
// referenciadas com arquivo perdido são logadas (o conteúdo já está perdido e
// a FK impede o delete — o operador investiga).
func cleanupStaleMedia(ctx context.Context, cutoff time.Time) (int, int, error) {
	hashes, err := storage.ListMediaHashesBefore(ctx, cutoff)
	if err != nil {
		return 0, 0, err
	}

	deletedRows, deletedFiles := 0, 0
	for _, hash := range hashes {
		if ctx.Err() != nil {
			return deletedRows, deletedFiles, ctx.Err()
		}

		path := mediaBlobPath(hash)
		_, statErr := os.Stat(path)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			continue // stat falhou por outro motivo: mantém (conservador)
		}
		fileExists := statErr == nil

		referenced, err := storage.MediaIsReferenced(ctx, hash)
		if err != nil {
			return deletedRows, deletedFiles, err
		}
		if referenced {
			if !fileExists {
				utils.Errorf("manutenção: mídia %s sem arquivo e com referência: conteúdo perdido, row mantida (verificar backup)", hash)
			}
			continue
		}

		if err := storage.DeleteMediaByHash(ctx, hash); err != nil {
			return deletedRows, deletedFiles, err
		}
		deletedRows++
		if fileExists {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return deletedRows, deletedFiles, fmt.Errorf("falha ao remover arquivo de mídia sem referência %s: %w", hash, err)
			}
			removeEmptyDir(filepath.Dir(path))
			deletedFiles++
		}
	}
	return deletedRows, deletedFiles, nil
}

// mediaFile é um arquivo encontrado no varredura da pasta de mídia.
type mediaFile struct {
	path    string
	hash    string
	modTime time.Time
}

// scanMediaFiles lista todos os arquivos da pasta de mídia (recursivo). O
// hash é o nome do arquivo (content-addressable: media/<ab>/<cd>/<hash>);
// temporários .upload-* também entram como candidatos (o nome é o hash que o
// upload tentaria registrar).
func scanMediaFiles() ([]mediaFile, error) {
	files := make([]mediaFile, 0)
	err := filepath.WalkDir(mediaBaseDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil // pasta de mídia ainda não criada
			}
			return walkErr
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files = append(files, mediaFile{
			path:    path,
			hash:    d.Name(),
			modTime: info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("falha ao varrer a pasta de mídia: %w", err)
	}
	return files, nil
}

// cleanupOrphanMediaFiles remove do disco os arquivos de mídia sem row na
// tabela media, com mtime anterior ao cutoff (janela de proteção para
// uploads em andamento). Diretórios vazios resultantes são removidos (a
// pasta raiz de mídia é mantida).
func cleanupOrphanMediaFiles(ctx context.Context, cutoff time.Time) (int, error) {
	files, err := scanMediaFiles()
	if err != nil {
		return 0, err
	}

	candidates := make([]mediaFile, 0, len(files))
	for _, f := range files {
		if f.modTime.Before(cutoff) {
			candidates = append(candidates, f)
		}
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	hashes := make([]string, 0, len(candidates))
	for _, f := range candidates {
		hashes = append(hashes, f.hash)
	}
	existing, err := storage.FindExistingMediaHashes(ctx, hashes)
	if err != nil {
		return 0, err
	}

	deleted := 0
	for _, f := range candidates {
		if ctx.Err() != nil {
			return deleted, ctx.Err()
		}
		if existing[f.hash] {
			continue
		}
		if err := os.Remove(f.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return deleted, fmt.Errorf("falha ao remover arquivo órfão %s: %w", f.path, err)
		}
		deleted++
		removeEmptyDir(filepath.Dir(f.path))
	}
	return deleted, nil
}

// removeEmptyDir remove o diretório se estiver vazio (best-effort) e
// sobe para o pai; a pasta raiz de mídia é mantida.
func removeEmptyDir(dir string) {
	if dir == mediaBaseDir {
		return
	}
	if err := os.Remove(dir); err != nil {
		return // não vazio ou já removido: nada a fazer
	}
	removeEmptyDir(filepath.Dir(dir))
}

// purgeAuditLogs remove logs de auditoria com created_at anterior ao período
// de retenção (LOG_DURATION), em lotes com pausa entre eles. A trigger
// append-only é removida durante a purga e recriada ao final — o defer usa
// context.Background() de propósito: a recriação é crítica e precisa acontecer
// mesmo com o ctx do job cancelado. Se a trigger estiver ausente no início
// (crash em uma purga anterior), ela é recriada antes de prosseguir
// (auto-cura).
func purgeAuditLogs(ctx context.Context, retention time.Duration) error {
	exists, err := storage.AuditTriggerExists(ctx)
	if err != nil {
		return err
	}
	if !exists {
		utils.Warn("manutenção: trigger append-only de audit_logs ausente, recriando antes do purge")
		if err := storage.CreateAuditLogsTrigger(ctx); err != nil {
			return err
		}
	}

	if err := storage.DropAuditLogsTrigger(ctx); err != nil {
		return err
	}
	defer func() {
		if err := storage.CreateAuditLogsTrigger(context.Background()); err != nil {
			utils.Errorf("manutenção: FALHA ao recriar a trigger append-only de audit_logs: %v", err)
		}
	}()

	cutoff := time.Now().Add(-retention)
	total := int64(0)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := storage.PurgeAuditLogsBatch(ctx, cutoff, auditPurgeBatchSize)
		if err != nil {
			return err
		}
		total += n
		if n < auditPurgeBatchSize {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(auditPurgeBatchSleep):
		}
	}

	if total > 0 {
		utils.Infof("manutenção: %d log(s) de auditoria expirado(s) removido(s)", total)
	}
	return nil
}

// archiveUserConnections move para a history as conexões de sessão
// substituídas com replaced_at anterior a connectionArchiveDelay e purga da
// history as conexões com replaced_at anterior a connectionHistoryRetention
// (aí o JWT já expirou e o histórico não serve mais para detecção de reuso).
func archiveUserConnections(ctx context.Context) error {
	moved, err := storage.MoveUserConnectionsToHistory(ctx, time.Now().Add(-connectionArchiveDelay))
	if err != nil {
		return fmt.Errorf("arquivar: %w", err)
	}

	purged, err := storage.PurgeUserConnectionHistory(ctx, time.Now().Add(-connectionHistoryRetention))
	if err != nil {
		return fmt.Errorf("purgar: %w", err)
	}

	if moved > 0 || purged > 0 {
		utils.Infof("manutenção: %d conexão(ões) de sessão arquivada(s) e %d removida(s) da history", moved, purged)
	}
	return nil
}
