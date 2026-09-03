package moderation

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

const (
	// maxResponseBytes limita a linha de resposta do worker (poucos scores).
	maxResponseBytes = 8 * 1024
	// dialTimeout limita a conexão com o socket Unix.
	dialTimeout = 5 * time.Second
)

// classifyRequest é a requisição enviada ao worker Python (NDJSON: um objeto
// JSON por linha).
type classifyRequest struct {
	Type      string `json:"type,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Path      string `json:"path,omitempty"`
	MIME      string `json:"mime,omitempty"`
}

// classifyResponse é a resposta do worker Python.
type classifyResponse struct {
	Type      string   `json:"type"`
	RequestID string   `json:"request_id"`
	SFW       *float64 `json:"sfw"`
	Nudity    *float64 `json:"nudity"`
	Gore      *float64 `json:"gore"`
	Model     string   `json:"model"`
	Status    string   `json:"status"`
	Error     string   `json:"error"`
}

// Client conversa com o worker Python via socket Unix (uma requisição JSON
// por linha, uma resposta JSON por linha). Abre uma conexão por requisição:
// o worker é sequencial e não há estado compartilhado a reaproveitar.
type Client struct {
	socketPath string
	timeout    time.Duration
}

// NewClient cria o cliente do socket do worker.
func NewClient(socketPath string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{socketPath: socketPath, timeout: timeout}
}

// Classify envia o caminho da imagem ao worker e retorna as probabilidades.
func (c *Client) Classify(ctx context.Context, path, mime string) (Result, error) {
	resp, err := c.roundTrip(ctx, classifyRequest{
		RequestID: fmt.Sprintf("classify-%d", time.Now().UnixNano()),
		Path:      path,
		MIME:      mime,
	})
	if err != nil {
		return Result{}, err
	}
	if resp.Error != "" {
		return Result{}, fmt.Errorf("erro de inferência: %s", resp.Error)
	}
	if resp.SFW == nil || resp.Nudity == nil || resp.Gore == nil {
		return Result{}, fmt.Errorf("resposta malformada do worker (scores ausentes)")
	}

	return Result{
		SFW:    *resp.SFW,
		Nudity: *resp.Nudity,
		Gore:   *resp.Gore,
		Model:  resp.Model,
	}, nil
}

// Health verifica se o worker está de pé com o modelo carregado.
func (c *Client) Health(ctx context.Context) error {
	resp, err := c.roundTrip(ctx, classifyRequest{Type: "health"})
	if err != nil {
		return err
	}
	if resp.Status != "ok" {
		return fmt.Errorf("health do worker: %s", resp.Status)
	}
	return nil
}

// roundTrip faz uma ida e volta no socket com deadline (o ctx do chamador
// normalmente não tem deadline; o timeout da inferência é aplicado aqui).
func (c *Client) roundTrip(ctx context.Context, req classifyRequest) (classifyResponse, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
		deadline, _ = ctx.Deadline()
	}

	conn, err := net.DialTimeout("unix", c.socketPath, dialTimeout)
	if err != nil {
		return classifyResponse{}, fmt.Errorf("falha ao conectar no worker de moderação: %w", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return classifyResponse{}, err
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return classifyResponse{}, err
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return classifyResponse{}, fmt.Errorf("falha ao enviar a requisição ao worker: %w", err)
	}

	reader := bufio.NewReaderSize(conn, maxResponseBytes)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return classifyResponse{}, fmt.Errorf("falha ao ler a resposta do worker: %w", err)
	}
	if len(line) > maxResponseBytes {
		return classifyResponse{}, fmt.Errorf("resposta do worker excede o limite")
	}

	var resp classifyResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return classifyResponse{}, fmt.Errorf("falha ao interpretar a resposta do worker: %w", err)
	}
	return resp, nil
}
