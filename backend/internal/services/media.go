package services

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/storage"
)

// mediaBaseDir é a pasta (relativa ao diretório de trabalho do backend) onde
// todos os blobs de mídia são guardados em content-addressable storage
// (media/<ab>/<cd>/<hash>). Avatar, ícone, emoji, attachment, thumbnail e
// imagem de link preview compartilham o mesmo storage: conteúdo idêntico é
// gravado uma única vez. (var: os testes apontam para uma pasta temporária.)
var mediaBaseDir = "media"

// mediaBlobPath retorna o caminho do blob no content-addressable storage: os
// 2 primeiros bytes (4 hex) do hash viram subpastas para não estourar o
// limite de arquivos por diretório.
func mediaBlobPath(shaHash string) string {
	return filepath.Join(mediaBaseDir, shaHash[:2], shaHash[2:4], shaHash)
}

// StoreMediaFromBytes grava o conteúdo em content-addressable storage e
// registra a mídia na tabela media (deduplicação pelo hmac-sha256 do conteúdo).
// Retorna o hmac-sha256 (hex) e o registro de mídia. Se o conteúdo já existe, o
// registro existente é reutilizado e a gravação em disco é pulada.
func StoreMediaFromBytes(ctx context.Context, content []byte, mimeType string) (string, models.Media, error) {
	cfg := config.LoadConfig()

	mac := hmac.New(sha256.New, []byte(cfg.HMACSecret))
	mac.Write(content)

	hash := hex.EncodeToString(mac.Sum(nil))

	media, _, err := storage.InsertMediaIfAbsent(ctx, hash, mimeType, int64(len(content)))
	if err != nil {
		return "", models.Media{}, err
	}

	// Grava o arquivo apenas se ainda não existe (deduplicação); se a linha
	// existe mas o arquivo sumiu, regrava (conteúdo idêntico, auto-cura).
	path := mediaBlobPath(hash)
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", models.Media{}, fmt.Errorf("falha ao criar pasta da mídia: %w", err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			return "", models.Media{}, fmt.Errorf("falha ao gravar a mídia: %w", err)
		}
	} else if statErr != nil {
		return "", models.Media{}, fmt.Errorf("falha ao verificar a mídia: %w", statErr)
	}

	return hash, media, nil
}

// ErrMediaNotFound indica que a mídia não existe.
var ErrMediaNotFound = errors.New("mídia não encontrada")

// GetMedia retorna o registro de mídia e o caminho do blob no disco pelo
// sha256 (hex). Retorna ErrMediaNotFound quando a mídia não existe.
func GetMedia(ctx context.Context, shaHash string) (models.Media, string, error) {
	if shaHash == "" {
		return models.Media{}, "", ErrMediaNotFound
	}

	media, err := storage.GetMediaByHash(ctx, shaHash)
	if errors.Is(err, storage.ErrNotFound) {
		return models.Media{}, "", ErrMediaNotFound
	}
	if err != nil {
		return models.Media{}, "", err
	}

	return media, mediaBlobPath(shaHash), nil
}

// MediaContent lê o blob do disco pelo sha256 (hex).
// Retorna ErrNotFound quando o arquivo não existe.
func MediaContent(shaHash string) ([]byte, error) {
	data, err := os.ReadFile(mediaBlobPath(shaHash))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("arquivo de mídia não encontrado: %w", storage.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("falha ao ler o arquivo de mídia: %w", err)
	}

	return data, nil
}

// mimeToFormat mapeia o MIME type da tabela media para o rótulo de formato
// usado na API (avatar_format / icon_format / format do emoji).
func mimeToFormat(mimeType string) string {
	switch mimeType {
	case "image/png":
		return "PNG"
	case "image/jpeg":
		return "JPEG"
	case "image/gif":
		return "GIF"
	case "image/webp":
		return "WEBP"
	default:
		return strings.ToUpper(mimeType)
	}
}

// formatToMime converte o rótulo de formato aceito na API (PNG, JPEG, GIF)
// para o MIME type gravado na tabela media.
func formatToMime(format string) string {
	switch strings.ToUpper(format) {
	case "PNG":
		return "image/png"
	case "JPEG":
		return "image/jpeg"
	case "GIF":
		return "image/gif"
	default:
		return "image/" + strings.ToLower(format)
	}
}
