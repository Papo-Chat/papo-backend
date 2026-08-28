package services

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"unicode/utf8"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"
)

// ErrAttachmentTooLarge indica que o arquivo excede o tamanho máximo (100MB).
var ErrAttachmentTooLarge = errors.New("arquivo excede o tamanho máximo")

// ErrAttachmentNotFound indica que o attachment não existe.
var ErrAttachmentNotFound = errors.New("attachment não encontrado")

// maxAttachmentSize é o tamanho máximo de um attachment (100MB, README).
const maxAttachmentSize = 100 * 1024 * 1024

// maxAttachmentNameLength é o tamanho máximo do nome do attachment (128, README).
const maxAttachmentNameLength = 128

// AttachmentInput é o conteúdo de um arquivo enviado em um upload (upload
// avulso ou como attachment de uma mensagem). Apenas o nome e os bytes são
// recebidos: o MIME type é detectado a partir do conteúdo e o tamanho é
// calculado dos bytes gravados (nada do upload é confiado).
type AttachmentInput struct {
	OriginalFileName string
	Content          io.Reader
}

// UploadAttachment recebe o conteúdo de um upload, calcula o sha256, registra
// o blob na tabela media (content-addressable storage, media/ab/cd/<hash>) e
// o attachment no banco (referência pelo sha_hash do blob).
//
// O nome recebido é sanitizado e o MIME type é detectado a partir do
// conteúdo (o header de content type do upload não é confiado).
//
// A gravação não é atômica: se uma etapa falhar, o registro fica órfão (ou o
// blob fica sem registro) e é limpo pela rotina de manutenção (cron).
//
// Retorna ErrInvalidInput quando o nome é inválido e ErrAttachmentTooLarge
// quando o arquivo excede 100MB.
func UploadAttachment(ctx context.Context, originalFileName string, content io.Reader, userID string) (models.Attachments, error) {
	return storeAttachment(ctx, AttachmentInput{
		OriginalFileName: originalFileName,
		Content:          content,
	}, userID)
}

// DownloadAttachment busca o attachment e verifica o acesso do usuário
// (README: GET /attachments/:file_id). O usuário precisa da permissão
// read_channel do canal da mensagem que possui o attachment; o dono do
// servidor do canal sempre pode ler e em canais sem roles com permissões
// definidas a leitura é livre para todos.
//
// Attachments não vinculados a mensagem (messages_id NULL, órfãos de uma
// gravação incompleta) não são expostos pela API e retornam
// ErrAttachmentNotFound.
//
// Retorna ErrAttachmentNotFound quando o attachment não existe,
// ErrInvalidInput quando o user_id está ausente, ErrChannelNotFound quando o
// canal da mensagem não existe e ErrPermissionDenied quando o usuário não
// pode ler o canal.
func DownloadAttachment(ctx context.Context, fileID, userID string) (models.Attachments, error) {
	return resolveReadableAttachment(ctx, fileID, userID)
}

// DownloadAttachmentThumbnail busca a thumbnail de um attachment com o MESMO
// check de acesso do download original (read_channel do canal da mensagem).
// Retorna ErrAttachmentNotFound quando o attachment não existe, não está
// vinculado a mensagem, o usuário não pode ler o canal ou a thumbnail não
// foi gerada.
func DownloadAttachmentThumbnail(ctx context.Context, fileID, userID string) (models.AttachmentThumbnail, error) {
	if _, err := resolveReadableAttachment(ctx, fileID, userID); err != nil {
		return models.AttachmentThumbnail{}, err
	}

	thumbnail, err := storage.GetThumbnailByAttachmentID(ctx, fileID, thumbnailKind)
	if errors.Is(err, storage.ErrNotFound) {
		return models.AttachmentThumbnail{}, ErrAttachmentNotFound
	}
	if err != nil {
		return models.AttachmentThumbnail{}, err
	}

	// O caminho do blob em disco é derivado do sha_hash (content-addressable).
	thumbnail.FilePath = mediaBlobPath(thumbnail.MediaShaHash)

	return thumbnail, nil
}

// resolveReadableAttachment busca o attachment e verifica o acesso do
// usuário: a permissão read_channel do canal da mensagem que possui o
// attachment (o dono do servidor do canal sempre pode e em canais sem roles
// com permissões definidas a leitura é livre para todos).
//
// Attachments não vinculados a mensagem (messages_id NULL, órfãos de uma
// gravação incompleta) não são expostos pela API e retornam
// ErrAttachmentNotFound.
func resolveReadableAttachment(ctx context.Context, fileID, userID string) (models.Attachments, error) {
	if fileID == "" {
		return models.Attachments{}, ErrAttachmentNotFound
	}
	if userID == "" {
		return models.Attachments{}, ErrInvalidInput
	}

	attachment, err := storage.GetAttachmentByID(ctx, fileID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.Attachments{}, ErrAttachmentNotFound
	}
	if err != nil {
		return models.Attachments{}, err
	}

	// O caminho do blob em disco é derivado do sha_hash (content-addressable).
	attachment.FilePath = mediaBlobPath(attachment.MediaShaHash)

	if attachment.MessagesID == nil {
		return models.Attachments{}, ErrAttachmentNotFound
	}

	message, err := storage.GetMessageByID(ctx, *attachment.MessagesID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.Attachments{}, ErrAttachmentNotFound
	}
	if err != nil {
		return models.Attachments{}, err
	}

	channel, err := storage.GetChannelByID(ctx, message.ChannelID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.Attachments{}, ErrChannelNotFound
	}
	if err != nil {
		return models.Attachments{}, err
	}

	allowed, err := userHasChannelPermission(ctx, channel, userID, true, func(p models.ChannelPermission) bool {
		return p.ReadChannel
	})
	if err != nil {
		return models.Attachments{}, err
	}
	if !allowed {
		return models.Attachments{}, ErrPermissionDenied
	}

	return attachment, nil
}

// storeAttachment grava um attachment em etapas, não atômicas entre si:
//  1. insere o blob na tabela media (deduplicação pelo sha256 do conteúdo);
//  2. move o arquivo temporário para o caminho content-addressable
//     (se o blob já existe, o rename reescreve o mesmo conteúdo — inofensivo);
//  3. insere o registro do attachment referenciando o blob pelo sha_hash.
func storeAttachment(ctx context.Context, input AttachmentInput, userID string) (models.Attachments, error) {
	fileName := utils.SanitizeFileName(input.OriginalFileName)
	if fileName == "" || utf8.RuneCountInString(fileName) > maxAttachmentNameLength {
		return models.Attachments{}, ErrInvalidInput
	}

	hash, size, tmpName, err := hashToTempFile(input.Content)
	if err != nil {
		return models.Attachments{}, err
	}
	defer removeIfExists(tmpName)

	mimeType, err := detectMimeTypeFromFile(tmpName)
	if err != nil {
		return models.Attachments{}, err
	}

	if _, _, err := storage.InsertMediaIfAbsent(ctx, hash, mimeType, size); err != nil {
		return models.Attachments{}, err
	}

	if err := moveToBlob(tmpName, hash); err != nil {
		return models.Attachments{}, err
	}

	attachment, err := storage.CreateAttachment(ctx, models.Attachments{
		OriginalFileName: fileName,
		MediaShaHash:     hash,
		CreatedBy:        &userID,
	})
	if err != nil {
		return models.Attachments{}, err
	}
	attachment.FilePath = mediaBlobPath(hash)

	// Thumbnail (best-effort): só para MIMEs de imagem processáveis.
	// Qualquer falha é logada e ignorada — nunca quebra o upload.
	if isProcessableImage(mimeType) {
		ensureAttachmentThumbnail(ctx, attachment.ID, mediaBlobPath(hash), mimeType)
	}

	return attachment, nil
}

// thumbnailKind é o kind único de thumbnail de attachment (a tabela
// suporta N kinds via UNIQUE (attachment_id, kind)).
const thumbnailKind = "preview"

// isProcessableImage indica se o MIME type é uma imagem com geração de
// thumbnail (jpeg, png, webp, gif).
func isProcessableImage(mimeType string) bool {
	switch mimeType {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

// Semáforo in-memory de geração de thumbnails (cap THUMBNAIL_MAX_CONCURRENCY,
// default 4). Se cheio, a geração é pulada (best-effort).
var (
	thumbnailSem     chan struct{}
	thumbnailSemOnce sync.Once
)

func thumbnailSemaphore() chan struct{} {
	thumbnailSemOnce.Do(func() {
		cap := config.LoadConfig().ThumbnailMaxConc
		if cap <= 0 {
			cap = 4
		}
		thumbnailSem = make(chan struct{}, cap)
	})
	return thumbnailSem
}

func acquireThumbnailSlot() bool {
	sem := thumbnailSemaphore()
	select {
	case sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func releaseThumbnailSlot() {
	<-thumbnailSemaphore()
}

// ensureAttachmentThumbnail gera a thumbnail do attachment (best-effort):
// lê o blob, gera (com semáforo de concorrência) e grava a thumbnail na
// tabela media (content-addressable) + registro no banco. Qualquer falha é
// logada e ignorada. Se já existir thumbnail para o attachment (upload
// duplicado), não faz nada. THUMBNAIL_ENABLED=false desliga totalmente o
// processamento (modo ultra-light: só a validação de entrada permanece).
func ensureAttachmentThumbnail(ctx context.Context, attachmentID, blobFile, mimeType string) {
	cfg := config.LoadConfig()
	if !cfg.ThumbnailEnabled {
		return
	}

	if !acquireThumbnailSlot() {
		utils.Infof("thumbnail pulada para %s: semáforo cheio", attachmentID)
		return
	}
	defer releaseThumbnailSlot()

	if _, err := storage.GetThumbnailByAttachmentID(ctx, attachmentID, thumbnailKind); err == nil {
		return // já existe (upload duplicado do mesmo conteúdo)
	} else if !errors.Is(err, storage.ErrNotFound) {
		utils.Errorf("falha ao verificar thumbnail existente para %s: %v", attachmentID, err)
		return
	}

	content, err := os.ReadFile(blobFile)
	if err != nil {
		utils.Errorf("falha ao ler blob para thumbnail de %s: %v", attachmentID, err)
		return
	}

	maxDim := cfg.ThumbnailMaxDim
	if mimeType == "image/gif" {
		maxDim = cfg.GIFThumbnailMaxDim
	}

	thumb, thumbMime, width, height, err := utils.GenerateThumbnail(content, maxDim, cfg.ThumbnailTimeout)
	if err != nil {
		if !errors.Is(err, utils.ErrNotProcessableImage) {
			utils.Errorf("thumbnail falhou para %s: %v", attachmentID, err)
		}
		return
	}

	thumbHash, _, err := StoreMediaFromBytes(ctx, thumb, thumbMime)
	if err != nil {
		utils.Errorf("falha ao gravar thumbnail de %s: %v", attachmentID, err)
		return
	}

	if err := storage.CreateAttachmentThumbnail(ctx, models.AttachmentThumbnail{
		AttachmentID: attachmentID,
		Kind:         thumbnailKind,
		MediaShaHash: thumbHash,
		Width:        width,
		Height:       height,
	}); err != nil {
		utils.Errorf("falha ao registrar thumbnail de %s: %v", attachmentID, err)
		return
	}
}

// hashToTempFile calcula o sha256 do conteúdo e o grava em um arquivo
// temporário dentro da pasta de attachments. Retorna o hash (hex), o tamanho
// em bytes e o caminho do temporário (a limpeza é responsabilidade do
// chamador). Retorna ErrAttachmentTooLarge quando o conteúdo excede o
// tamanho máximo.
func hashToTempFile(content io.Reader) (string, int64, string, error) {
	cfg := config.LoadConfig()
	if err := os.MkdirAll(mediaBaseDir, 0o755); err != nil {
		return "", 0, "", fmt.Errorf("falha ao criar pasta de mídia: %w", err)
	}

	tmp, err := os.CreateTemp(mediaBaseDir, ".upload-*")
	if err != nil {
		return "", 0, "", fmt.Errorf("falha ao criar arquivo temporário: %w", err)
	}
	tmpName := tmp.Name()

	fail := func(err error) (string, int64, string, error) {
		tmp.Close()
		removeIfExists(tmpName)
		return "", 0, "", err
	}

	if err := tmp.Chmod(0o644); err != nil {
		return fail(fmt.Errorf("falha ao ajustar permissão do arquivo: %w", err))
	}

	//queremos que o sha256 não seja guessable externamente
	mac := hmac.New(sha256.New, []byte(cfg.HMACSecret))

	limited := &sizeLimitWriter{
		w:     io.MultiWriter(tmp, mac),
		limit: maxAttachmentSize,
	}
	size, err := io.Copy(limited, content)
	if err != nil {
		if errors.Is(err, ErrAttachmentTooLarge) {
			return fail(ErrAttachmentTooLarge)
		}
		return fail(fmt.Errorf("falha ao gravar o arquivo: %w", err))
	}
	if err := tmp.Close(); err != nil {
		return fail(fmt.Errorf("falha ao gravar o arquivo: %w", err))
	}

	hash := hex.EncodeToString(mac.Sum(nil))
	return hash, size, tmpName, nil
}

// moveToBlob move o arquivo temporário para o caminho final do blob
// (mediaBlobPath).
func moveToBlob(tmpName, hash string) error {
	target := mediaBlobPath(hash)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("falha ao criar subpasta do blob: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("falha ao salvar o blob: %w", err)
	}
	return nil
}

// detectMimeTypeFromFile detecta o MIME type de um arquivo lendo os
// primeiros 512 bytes (magic bytes), sem confiar no header do upload.
func detectMimeTypeFromFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("falha ao detectar mime type: %w", err)
	}
	defer f.Close()

	buf := make([]byte, 512)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", fmt.Errorf("falha ao detectar mime type: %w", err)
	}

	return utils.DetectMimeType(buf[:n]), nil
}

// removeIfExists remove o arquivo se existir (ignora erro de arquivo
// ausente).
func removeIfExists(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		utils.Errorf("falha ao remover arquivo temporário %s: %v", path, err)
	}
}

// sizeLimitWriter limita o número de bytes gravados no writer subjacente e
// retorna ErrAttachmentTooLarge quando o limite é excedido.
type sizeLimitWriter struct {
	w     io.Writer
	n     int64
	limit int64
}

func (s *sizeLimitWriter) Write(p []byte) (int, error) {
	if s.n+int64(len(p)) > s.limit {
		return 0, ErrAttachmentTooLarge
	}
	s.n += int64(len(p))
	return s.w.Write(p)
}
