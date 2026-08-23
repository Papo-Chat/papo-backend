package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

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

// attachmentsBaseDir é a pasta (relativa ao diretório de trabalho do backend)
// onde os blobs de attachment são guardados em content-addressable storage.
const attachmentsBaseDir = "attachments"

// AttachmentInput é o conteúdo de um arquivo enviado em um upload (upload
// avulso ou como attachment de uma mensagem). Apenas o nome e os bytes são
// recebidos: o MIME type é detectado a partir do conteúdo e o tamanho é
// calculado dos bytes gravados (nada do upload é confiado).
type AttachmentInput struct {
	OriginalFileName string
	Content          io.Reader
}

// UploadAttachment recebe o conteúdo de um upload, calcula o sha256, registra
// o attachment no banco e salva o blob em content-addressable storage
// (attachments/ab/cd/<hash>), se o hash não estiver duplicado.
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

// storeAttachment grava um attachment em duas etapas, não atômicas entre si:
//  1. insere o registro no banco (com o sha_hash do conteúdo);
//  2. grava o blob no storage, apenas se o sha_hash não estiver duplicado.
//
// O campo sha_hash é o sinal de deduplicação: se já existe um attachment com
// o mesmo hash, o blob do conteúdo já está em disco e a gravação é pulada.
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

	duplicated, err := storage.ExistsAttachmentByHash(ctx, hash)
	if err != nil {
		return models.Attachments{}, err
	}

	attachment, err := storage.CreateAttachment(ctx, models.Attachments{
		OriginalFileName: fileName,
		MimeType:         mimeType,
		FilePath:         blobPath(hash),
		ShaHash:          hash,
		SizeBytes:        size,
		CreatedBy:        &userID,
	})
	if err != nil {
		return models.Attachments{}, err
	}

	if duplicated {
		return attachment, nil
	}

	if err := moveToBlob(tmpName, hash); err != nil {
		return models.Attachments{}, err
	}

	return attachment, nil
}

// hashToTempFile calcula o sha256 do conteúdo e o grava em um arquivo
// temporário dentro da pasta de attachments. Retorna o hash (hex), o tamanho
// em bytes e o caminho do temporário (a limpeza é responsabilidade do
// chamador). Retorna ErrAttachmentTooLarge quando o conteúdo excede o
// tamanho máximo.
func hashToTempFile(content io.Reader) (string, int64, string, error) {
	if err := os.MkdirAll(attachmentsBaseDir, 0o755); err != nil {
		return "", 0, "", fmt.Errorf("falha ao criar pasta de attachments: %w", err)
	}

	tmp, err := os.CreateTemp(attachmentsBaseDir, ".upload-*")
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

	hasher := sha256.New()
	limited := &sizeLimitWriter{w: io.MultiWriter(tmp, hasher), limit: maxAttachmentSize}
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

	hash := hex.EncodeToString(hasher.Sum(nil))
	return hash, size, tmpName, nil
}

// blobPath retorna o caminho do blob no content-addressable storage: os 2
// primeiros bytes (4 hex) do hash viram subpastas para não estourar o limite
// de arquivos por diretório.
func blobPath(hash string) string {
	return filepath.Join(attachmentsBaseDir, hash[:2], hash[2:4], hash)
}

// moveToBlob move o arquivo temporário para o caminho final do blob.
func moveToBlob(tmpName, hash string) error {
	target := blobPath(hash)
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
