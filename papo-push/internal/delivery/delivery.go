// Package delivery abstrai o envio de push. O papo-push nunca decide o destino;
// ele apenas consome a outbox, resolve os dispositivos do usuário e delega a
// entrega a uma implementação (FCM direto ou relay remoto).
package delivery

import "context"

// Message é a mensagem de push a ser entregue (título, corpo e dados para o app
// abrir a conversa).
type Message struct {
	Title string
	Body  string
	// Data são os dados enviados ao app (IDs para abrir a conversa). Chave
	// "notification_id" é incluída apenas quando existe (eventos efêmeros
	// não têm row de notificação).
	Data map[string]string
}

// Target é um dispositivo (token + plataforma).
type Target struct {
	Token    string
	Platform string
}

// Status é o resultado da entrega de um target (dispositivo).
type Status string

const (
	StatusSent          Status = "sent"
	StatusInvalidToken  Status = "invalid_token"
	StatusRetryableError Status = "retryable_error"
	StatusPermanentError Status = "permanent_error"
)

// TargetResult é o resultado da entrega de um target (dispositivo).
type TargetResult struct {
	Token  string
	Status Status
}

// Result resume a entrega de um job.
type Result struct {
	results       []TargetResult
	invalidTokens []string
	retryable     bool
	permanent     bool
}

// AllSent indica se todos os targets foram entregues (sent).
func (r Result) AllSent() bool {
	if len(r.results) == 0 {
		return false
	}
	for _, tr := range r.results {
		if tr.Status != StatusSent {
			return false
		}
	}
	return true
}

// InvalidTokens retorna os tokens com status invalid_token (a desativar).
func (r Result) InvalidTokens() []string {
	return r.invalidTokens
}

// HasRetryable indica se houve retryable_error (algum target não recebeu e
// pode ser tentado novamente).
func (r Result) HasRetryable() bool {
	return r.retryable
}

// HasPermanent indica se houve permanent_error (algum target não recebeu e
// não deve ser tentado além do limite).
func (r Result) HasPermanent() bool {
	return r.permanent
}

// TargetResults retorna os resultados por target (token + status), na ordem
// dos targets enviados.
func (r Result) TargetResults() []TargetResult {
	return r.results
}

// SentTokens retorna os tokens com status sent, para o worker persistir em
// delivered_tokens (evitando reenvio em retry).
func (r Result) SentTokens() []string {
	var out []string
	for _, tr := range r.results {
		if tr.Status == StatusSent {
			out = append(out, tr.Token)
		}
	}
	return out
}

// NewTestResult constrói um Result a partir dos resultados por target,
// derivando os flags (retryable/permanent/invalidTokens) do status de cada
// target. Utilizado exclusivamente pelos testes (o worker não pode acessar
// os campos não expostos diretamente).
func NewTestResult(results ...TargetResult) Result {
	r := Result{}
	r.results = results
	for _, tr := range results {
		switch tr.Status {
		case StatusInvalidToken:
			r.invalidTokens = append(r.invalidTokens, tr.Token)
		case StatusRetryableError:
			r.retryable = true
		case StatusPermanentError:
			r.permanent = true
		}
	}
	return r
}

// Delivery entrega push para uma lista de targets.
type Delivery interface {
	Send(ctx context.Context, message Message, targets []Target) (Result, error)
}
