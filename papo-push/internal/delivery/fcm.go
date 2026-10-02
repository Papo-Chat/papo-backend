package delivery

import (
	"context"
	"fmt"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"google.golang.org/api/option"

	"papo/push/internal/config"
)

// FCMDelivery envia push diretamente ao FCM via Admin SDK (firebase.google.com/go/v4).
// Usada quando FCM_RELAY_URL está vazio; exige GOOGLE_APPLICATION_CREDENTIALS.
type FCMDelivery struct {
	client *messaging.Client
}

// NewFCM cria um FCMDelivery carregando as credenciais do app do
// GOOGLE_APPLICATION_CREDENTIALS (o project_id vem do próprio service account).
func NewFCM(cfg *config.Config) (*FCMDelivery, error) {
	ctx := context.Background()
	app, err := firebase.NewApp(
		ctx,
		nil,
		option.WithAuthCredentialsFile(option.ServiceAccount, cfg.GoogleApplicationCreds),
	)
	if err != nil {
		return nil, fmt.Errorf("falha ao inicializar o Admin SDK do Firebase: %w", err)
	}
	client, err := app.Messaging(ctx)
	if err != nil {
		return nil, fmt.Errorf("falha ao obter o cliente de FCM: %w", err)
	}
	return &FCMDelivery{client: client}, nil
}

// buildMessage converte o Message + target em uma mensagem do FCM (token +
// notificação + data).
func (d *FCMDelivery) buildMessage(m Message, t Target) *messaging.Message {
	msg := &messaging.Message{
		Token: t.Token,
		Notification: &messaging.Notification{
			Title: m.Title,
			Body:  m.Body,
		},
		Data: m.Data,
	}
	return msg
}

// Send envia a mensagem para todos os targets via FCM (SendAll em lote).
func (d *FCMDelivery) Send(ctx context.Context, message Message, targets []Target) (Result, error) {
	msgs := make([]*messaging.Message, 0, len(targets))
	for _, t := range targets {
		msgs = append(msgs, d.buildMessage(message, t))
	}
	if len(msgs) == 0 {
		return Result{}, nil
	}

	responses, err := d.client.SendEach(ctx, msgs)
	if err != nil {
		// Falha na batch (rede, credencial, etc.) → retryable.
		return Result{retryable: true}, fmt.Errorf("falha ao enviar ao FCM: %w", err)
	}

	result := Result{}
	for i, resp := range responses.Responses {
		status := mapFCMError(resp.Error)
		result.results = append(result.results, TargetResult{Token: targets[i].Token, Status: status})
		switch status {
		case StatusInvalidToken:
			result.invalidTokens = append(result.invalidTokens, targets[i].Token)
		case StatusRetryableError:
			result.retryable = true
		case StatusPermanentError:
			result.permanent = true
		}
	}
	return result, nil
}

// mapFCMError mapeia o erro do FCM para o Status do papo-push.
func mapFCMError(err error) Status {
	if err == nil {
		return StatusSent
	}
	switch {
	case messaging.IsRegistrationTokenNotRegistered(err), messaging.IsUnregistered(err):
		return StatusInvalidToken
	case messaging.IsInvalidArgument(err):
		return StatusPermanentError
	default:
		// quota, rede, indisponível, desconhecido → tentável novamente.
		return StatusRetryableError
	}
}
