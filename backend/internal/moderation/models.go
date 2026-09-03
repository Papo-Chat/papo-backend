package moderation

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"papo/internal/utils"
)

// ModelSpec fixa um modelo (URL + SHA-256): o download é verificado e um
// arquivo adulterado nunca é carregado.
type ModelSpec struct {
	Name     string
	Filename string
	URL      string
	SHA256   string
}

// modelManifest é o conjunto de modelos usados pelo worker Python (Fase 1:
// classificador primário único). A URL é fixada em um commit específico do
// OwenElliott/image-safety-classifier-xs (HuggingFace); o ONNX já inclui a
// normalização e o softmax (entrada: 224x224 RGB, pixels 0-255).
var modelManifest = []ModelSpec{{
	Name:     "safety-xs-v1",
	Filename: "safety-xs-v1.onnx",
	URL:      "https://huggingface.co/OwenElliott/image-safety-classifier-xs/resolve/54f4560bd9c5ee92d45dc30418a8f8680e80de6d/onnx/image-safety-classifier-xs.onnx",
	SHA256:   "8c28c49d9075f3ad15ebdc2961f02d5b3f99be944815b848b49c9f0e6f3fb689",
}}

// EnsureModels garante que os modelos existem em modelsDir (baixando e
// verificando o SHA-256 quando necessário) e retorna o mapa nome do modelo →
// caminho absoluto.
func EnsureModels(ctx context.Context, modelsDir string) (map[string]string, error) {
	if err := os.MkdirAll(modelsDir, 0o755); err != nil {
		return nil, fmt.Errorf("falha ao criar o diretório de modelos: %w", err)
	}

	paths := make(map[string]string, len(modelManifest))
	for _, spec := range modelManifest {
		path := filepath.Join(modelsDir, spec.Filename)
		if err := ensureModelFile(ctx, spec, path); err != nil {
			return nil, err
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("falha ao resolver o caminho do modelo: %w", err)
		}
		paths[spec.Name] = abs
	}

	return paths, nil
}

// ensureModelFile usa o arquivo quando ele existe e o hash confere; caso
// contrário baixa para um arquivo temporário, verifica o SHA-256 e renomeia
// de forma atômica.
func ensureModelFile(ctx context.Context, spec ModelSpec, path string) error {
	if hash, err := fileSHA256(path); err == nil && hashMatches(hash, spec.SHA256) {
		utils.Infof("moderação: modelo %s já disponível (%s)", spec.Name, path)
		return nil
	}

	utils.Infof("moderação: baixando modelo %s (%s)", spec.Name, spec.URL)
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+spec.Filename+"-download-*")
	if err != nil {
		return fmt.Errorf("falha ao criar o arquivo temporário do modelo: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := downloadTo(ctx, spec.URL, tmp); err != nil {
		tmp.Close()
		return fmt.Errorf("falha ao baixar o modelo %s: %w", spec.Name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("falha ao fechar o arquivo temporário do modelo: %w", err)
	}

	hash, err := fileSHA256(tmpName)
	if err != nil {
		return fmt.Errorf("falha ao verificar o modelo %s: %w", spec.Name, err)
	}
	if !hashMatches(hash, spec.SHA256) {
		return fmt.Errorf("SHA-256 do modelo %s não confere (esperado %s, obtido %s)", spec.Name, spec.SHA256, hash)
	}

	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("falha ao ajustar as permissões do modelo: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("falha ao mover o modelo para o lugar: %w", err)
	}

	return nil
}

func hashMatches(actual, expected string) bool {
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}

func downloadTo(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status inesperado %s", resp.Status)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
