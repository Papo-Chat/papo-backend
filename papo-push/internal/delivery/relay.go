package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"papo/push/internal/config"
)

// RelayDelivery envia push via o relay remoto (POST {FCM_RELAY_URL}/v1/push).
// O worker autentica com FCM_RELAY_TOKEN (Bearer). O relay é o único responsável
// por falar com o FCM; não conhece usuários nem o PostgreSQL.
type RelayDelivery struct {
	url      string
	token    string
	client   *http.Client
}

type relayTarget struct {
	Token    string `json:"token"`
	Platform string `json:"platform"`
}

type relayRequest struct {
	Targets      []relayTarget   `json:"targets"`
	Notification map[string]string `json:"notification"`
	Data         map[string]string `json:"data"`
}

type relayResult struct {
	Token  string `json:"token"`
	Status string `json:"status"`
}

type relayResponse struct {
	Results []relayResult `json:"results"`
}

// NewRelay cria um RelayDelivery.
func NewRelay(cfg *config.Config) (*RelayDelivery, error) {
	return &RelayDelivery{
		url:    cfg.FCMRelayURL,
		token:  cfg.FCMRelayToken,
		client: &http.Client{Timeout: cfg.PushRequestTimeout},
	}, nil
}

// Send envia a mensagem para todos os targets via o relay.
func (d *RelayDelivery) Send(ctx context.Context, message Message, targets []Target) (Result, error) {
	reqBody := relayRequest{
		Targets: make([]relayTarget, len(targets)),
		Notification: map[string]string{
			"title": message.Title,
			"body":  message.Body,
		},
		Data: message.Data,
	}
	for i, t := range targets {
		reqBody.Targets[i] = relayTarget{Token: t.Token, Platform: t.Platform}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return Result{retryable: true}, fmt.Errorf("falha ao serializar o request do relay: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url+"/v1/push", bytes.NewReader(body))
	if err != nil {
		return Result{retryable: true}, fmt.Errorf("falha ao criar o request do relay: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.client.Do(req)
	if err != nil {
		return Result{retryable: true}, fmt.Errorf("falha ao enviar ao relay: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Result{retryable: true}, fmt.Errorf("relay retornou status %d", resp.StatusCode)
	}

	var relayResp relayResponse
	if err := json.NewDecoder(resp.Body).Decode(&relayResp); err != nil {
		return Result{retryable: true}, fmt.Errorf("falha ao decodificar a resposta do relay: %w", err)
	}

	// Mapeia o status por token.
	statusByToken := make(map[string]Status)
	for _, r := range relayResp.Results {
		statusByToken[r.Token] = mapRelayStatus(r.Status)
	}

	result := Result{}
	for _, t := range targets {
		status, ok := statusByToken[t.Token]
		if !ok {
			// Token ausente na resposta → conservador: retryable (tentar de novo).
			status = StatusRetryableError
		}
		result.results = append(result.results, TargetResult{Token: t.Token, Status: status})
		switch status {
		case StatusInvalidToken:
			result.invalidTokens = append(result.invalidTokens, t.Token)
		case StatusRetryableError:
			result.retryable = true
		case StatusPermanentError:
			result.permanent = true
		}
	}
	return result, nil
}

// mapRelayStatus converte o status do relay para o Status do papo-push.
func mapRelayStatus(s string) Status {
	switch s {
	case "sent":
		return StatusSent
	case "invalid_token":
		return StatusInvalidToken
	case "retryable_error":
		return StatusRetryableError
	case "permanent_error":
		return StatusPermanentError
	default:
		return StatusRetryableError
	}
}
