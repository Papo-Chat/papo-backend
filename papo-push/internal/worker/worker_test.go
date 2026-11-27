package worker

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"papo/push/internal/config"
	"papo/push/internal/delivery"
	"papo/push/internal/store"
)

const testPayload = `{"type":"message","message_id":"m1","channel_id":"c1","author":"Ana","preview":"oi"}`

// fakeStore registra as operações do worker sobre a outbox e os dispositivos.
type fakeStore struct {
	mu        sync.Mutex
	devices   []store.PushDevice
	completed []string
	resched   []reschedule
	merged    []string
	disabled  []string
}

type reschedule struct {
	JobID    string
	Attempts int
}

func (f *fakeStore) ClaimDueJobs(context.Context, int) ([]store.PushJob, error) { return nil, nil }

func (f *fakeStore) ListEnabledPushDevices(context.Context, string) ([]store.PushDevice, error) {
	return f.devices, nil
}

func (f *fakeStore) CompletePushJob(_ context.Context, jobID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed = append(f.completed, jobID)
	return nil
}

func (f *fakeStore) ReschedulePushJob(_ context.Context, jobID string, attempts int, _ time.Time, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resched = append(f.resched, reschedule{jobID, attempts})
	return nil
}

func (f *fakeStore) MergeDeliveredTokens(_ context.Context, _ string, tokens []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merged = append(f.merged, tokens...)
	return nil
}

func (f *fakeStore) DisablePushDeviceByToken(_ context.Context, token string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disabled = append(f.disabled, token)
	return 1, nil
}

// fakeDelivery responde por token (padrão: sent) e guarda os tokens recebidos.
type fakeDelivery struct {
	status map[string]delivery.Status
	err    error
	calls  [][]string
}

func (f *fakeDelivery) Send(_ context.Context, _ delivery.Message, targets []delivery.Target) (delivery.Result, error) {
	if f.err != nil {
		// Falha total do envio (ex.: relay fora do ar): nada foi entregue.
		return delivery.Result{}, f.err
	}
	tokens := make([]string, 0, len(targets))
	results := make([]delivery.TargetResult, 0, len(targets))
	for _, tg := range targets {
		tokens = append(tokens, tg.Token)
		status, ok := f.status[tg.Token]
		if !ok {
			status = delivery.StatusSent
		}
		results = append(results, delivery.TargetResult{Token: tg.Token, Status: status})
	}
	f.calls = append(f.calls, tokens)
	return delivery.NewTestResult(results...), nil
}

func (f *fakeDelivery) sent() []string {
	var all []string
	for _, c := range f.calls {
		all = append(all, c...)
	}
	return all
}

func device(token, provider string) store.PushDevice {
	return store.PushDevice{Token: token, Platform: "android", Provider: provider, Enabled: true}
}

func newTestWorker(providers []config.ProviderConfig, s *fakeStore, d map[string]delivery.Delivery) *Worker {
	cfg := &config.Config{PushMaxAttempts: 8, PushPreview: "full", Providers: providers}
	return New(cfg, s, d)
}

// configured devolve a configuração de PUSH_PROVIDERS com os nomes dados.
func configured(names ...string) []config.ProviderConfig {
	out := make([]config.ProviderConfig, 0, len(names))
	for _, n := range names {
		out = append(out, config.ProviderConfig{Name: n, Credentials: "/x.json"})
	}
	return out
}

func job(delivered ...string) store.PushJob {
	return store.PushJob{ID: "job1", UserID: "u1", Payload: testPayload, DeliveredTokens: delivered}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestProcessJobRoutesByProvider: cada provider recebe só os seus dispositivos.
func TestProcessJobRoutesByProvider(t *testing.T) {
	s := &fakeStore{devices: []store.PushDevice{
		device("a1", "fcm"), device("b1", "meu-fork"), device("a2", "fcm"),
	}}
	fcm, fork := &fakeDelivery{}, &fakeDelivery{}
	w := newTestWorker(configured("fcm", "meu-fork"), s, map[string]delivery.Delivery{"fcm": fcm, "meu-fork": fork})

	if err := w.processJob(context.Background(), job()); err != nil {
		t.Fatalf("processJob: %v", err)
	}
	if got := fcm.sent(); !reflect.DeepEqual(got, []string{"a1", "a2"}) {
		t.Errorf("fcm recebeu %v, esperado [a1 a2]", got)
	}
	if got := fork.sent(); !reflect.DeepEqual(got, []string{"b1"}) {
		t.Errorf("meu-fork recebeu %v, esperado [b1]", got)
	}
	if !reflect.DeepEqual(s.completed, []string{"job1"}) || len(s.resched) != 0 {
		t.Errorf("job deveria ser concluído: completed=%v resched=%v", s.completed, s.resched)
	}
	if got := sorted(s.merged); !reflect.DeepEqual(got, []string{"a1", "a2", "b1"}) {
		t.Errorf("tokens entregues = %v", got)
	}
}

// TestProcessJobSkipsUnknownProvider: provider não configurado é ignorado,
// sem falha nem retry.
func TestProcessJobSkipsUnknownProvider(t *testing.T) {
	s := &fakeStore{devices: []store.PushDevice{device("a1", "fcm"), device("x1", "outro")}}
	fcm := &fakeDelivery{}
	w := newTestWorker(configured("fcm"), s, map[string]delivery.Delivery{"fcm": fcm})

	if err := w.processJob(context.Background(), job()); err != nil {
		t.Fatalf("processJob: %v", err)
	}
	if got := fcm.sent(); !reflect.DeepEqual(got, []string{"a1"}) {
		t.Errorf("fcm recebeu %v, esperado [a1]", got)
	}
	if len(s.completed) != 1 || len(s.resched) != 0 {
		t.Errorf("esperava job concluído sem retry: completed=%v resched=%v", s.completed, s.resched)
	}
}

// TestProcessJobOnlyUnknownProviders: sem nenhum dispositivo entregável o job
// é concluído sem enviar nada.
func TestProcessJobOnlyUnknownProviders(t *testing.T) {
	s := &fakeStore{devices: []store.PushDevice{device("x1", "outro")}}
	fcm := &fakeDelivery{}
	w := newTestWorker(configured("fcm"), s, map[string]delivery.Delivery{"fcm": fcm})

	if err := w.processJob(context.Background(), job()); err != nil {
		t.Fatalf("processJob: %v", err)
	}
	if len(fcm.calls) != 0 {
		t.Errorf("não deveria enviar: %v", fcm.calls)
	}
	if len(s.completed) != 1 || len(s.resched) != 0 {
		t.Errorf("esperava job concluído: completed=%v resched=%v", s.completed, s.resched)
	}
}

// TestProcessJobLegacySendsEverythingToSingleDeliverer: sem PUSH_PROVIDERS o
// provider registrado é irrelevante.
func TestProcessJobLegacySendsEverythingToSingleDeliverer(t *testing.T) {
	s := &fakeStore{devices: []store.PushDevice{device("a1", "fcm"), device("b1", "meu-fork"), device("c1", "")}}
	single := &fakeDelivery{}
	w := newTestWorker(nil, s, map[string]delivery.Delivery{config.LegacyProviderName: single})

	if err := w.processJob(context.Background(), job()); err != nil {
		t.Fatalf("processJob: %v", err)
	}
	if len(single.calls) != 1 || !reflect.DeepEqual(single.calls[0], []string{"a1", "b1", "c1"}) {
		t.Errorf("provider único recebeu %v, esperado uma chamada com [a1 b1 c1]", single.calls)
	}
	if len(s.completed) != 1 {
		t.Errorf("esperava job concluído: completed=%v", s.completed)
	}
}

// TestProcessJobRetryableInOneGroup: falha retryable em um grupo mantém o job
// em retry; o grupo entregue é registrado e não é reenviado.
func TestProcessJobRetryableInOneGroup(t *testing.T) {
	s := &fakeStore{devices: []store.PushDevice{device("a1", "fcm"), device("b1", "meu-fork")}}
	fcm := &fakeDelivery{}
	fork := &fakeDelivery{status: map[string]delivery.Status{"b1": delivery.StatusRetryableError}}
	w := newTestWorker(configured("fcm", "meu-fork"), s, map[string]delivery.Delivery{"fcm": fcm, "meu-fork": fork})

	if err := w.processJob(context.Background(), job()); err != nil {
		t.Fatalf("processJob: %v", err)
	}
	if len(s.completed) != 0 || !reflect.DeepEqual(s.resched, []reschedule{{"job1", 1}}) {
		t.Fatalf("esperava reagendar: completed=%v resched=%v", s.completed, s.resched)
	}
	if !reflect.DeepEqual(s.merged, []string{"a1"}) {
		t.Fatalf("tokens entregues = %v, esperado [a1]", s.merged)
	}

	// Retry: a1 já está em delivered_tokens, só b1 é reenviado.
	fcm.calls, fork.calls = nil, nil
	fork.status = nil
	s.completed, s.resched = nil, nil
	if err := w.processJob(context.Background(), job("a1")); err != nil {
		t.Fatalf("processJob (retry): %v", err)
	}
	if len(fcm.calls) != 0 {
		t.Errorf("fcm não deveria receber de novo: %v", fcm.calls)
	}
	if !reflect.DeepEqual(fork.sent(), []string{"b1"}) {
		t.Errorf("meu-fork recebeu %v, esperado [b1]", fork.sent())
	}
	if len(s.completed) != 1 || len(s.resched) != 0 {
		t.Errorf("retry deveria concluir: completed=%v resched=%v", s.completed, s.resched)
	}
}

// TestProcessJobErrorInOneGroupDoesNotStopOthers: erro de envio em um provider
// não impede o outro, e o job é reagendado.
func TestProcessJobErrorInOneGroupDoesNotStopOthers(t *testing.T) {
	s := &fakeStore{devices: []store.PushDevice{device("a1", "fcm"), device("b1", "meu-fork")}}
	fcm := &fakeDelivery{err: errors.New("relay fora do ar")}
	fork := &fakeDelivery{}
	w := newTestWorker(configured("fcm", "meu-fork"), s, map[string]delivery.Delivery{"fcm": fcm, "meu-fork": fork})

	if err := w.processJob(context.Background(), job()); err != nil {
		t.Fatalf("processJob: %v", err)
	}
	if !reflect.DeepEqual(fork.sent(), []string{"b1"}) {
		t.Errorf("meu-fork deveria receber b1: %v", fork.sent())
	}
	if len(s.resched) != 1 || len(s.completed) != 0 {
		t.Errorf("esperava reagendar: completed=%v resched=%v", s.completed, s.resched)
	}
	if !reflect.DeepEqual(s.merged, []string{"b1"}) {
		t.Errorf("tokens entregues = %v, esperado [b1]", s.merged)
	}
}

// TestProcessJobInvalidTokenPerGroup: tokens inválidos de cada grupo são
// desativados e não causam retry.
func TestProcessJobInvalidTokenPerGroup(t *testing.T) {
	s := &fakeStore{devices: []store.PushDevice{device("a1", "fcm"), device("b1", "meu-fork")}}
	fcm := &fakeDelivery{status: map[string]delivery.Status{"a1": delivery.StatusInvalidToken}}
	fork := &fakeDelivery{status: map[string]delivery.Status{"b1": delivery.StatusInvalidToken}}
	w := newTestWorker(configured("fcm", "meu-fork"), s, map[string]delivery.Delivery{"fcm": fcm, "meu-fork": fork})

	if err := w.processJob(context.Background(), job()); err != nil {
		t.Fatalf("processJob: %v", err)
	}
	if got := sorted(s.disabled); !reflect.DeepEqual(got, []string{"a1", "b1"}) {
		t.Errorf("tokens desativados = %v, esperado [a1 b1]", got)
	}
	if len(s.completed) != 1 || len(s.resched) != 0 {
		t.Errorf("esperava job concluído: completed=%v resched=%v", s.completed, s.resched)
	}
}

// TestProcessJobNoDevices e TestProcessJobAllDelivered cobrem os retornos
// antecipados que já existiam.
func TestProcessJobNoDevices(t *testing.T) {
	s := &fakeStore{}
	w := newTestWorker(nil, s, map[string]delivery.Delivery{config.LegacyProviderName: &fakeDelivery{}})
	if err := w.processJob(context.Background(), job()); err != nil {
		t.Fatalf("processJob: %v", err)
	}
	if len(s.completed) != 1 {
		t.Errorf("esperava job concluído: %v", s.completed)
	}
}

func TestProcessJobAllDelivered(t *testing.T) {
	s := &fakeStore{devices: []store.PushDevice{device("a1", "fcm")}}
	fcm := &fakeDelivery{}
	w := newTestWorker(configured("fcm"), s, map[string]delivery.Delivery{"fcm": fcm})
	if err := w.processJob(context.Background(), job("a1")); err != nil {
		t.Fatalf("processJob: %v", err)
	}
	if len(fcm.calls) != 0 || len(s.completed) != 1 {
		t.Errorf("não deveria reenviar: calls=%v completed=%v", fcm.calls, s.completed)
	}
}

// TestProcessJobInvalidPayload: payload ilegível não envia e segue o caminho
// de retry/descarte.
func TestProcessJobInvalidPayload(t *testing.T) {
	s := &fakeStore{devices: []store.PushDevice{device("a1", "fcm")}}
	fcm := &fakeDelivery{}
	w := newTestWorker(configured("fcm"), s, map[string]delivery.Delivery{"fcm": fcm})
	bad := job()
	bad.Payload = "{"
	if err := w.processJob(context.Background(), bad); err != nil {
		t.Fatalf("processJob: %v", err)
	}
	if len(fcm.calls) != 0 || len(s.resched) != 1 {
		t.Errorf("esperava reagendar sem enviar: calls=%v resched=%v", fcm.calls, s.resched)
	}
}

// TestProcessJobMaxAttemptsDiscards: no limite de tentativas o job é
// descartado mesmo com falha retryable.
func TestProcessJobMaxAttemptsDiscards(t *testing.T) {
	s := &fakeStore{devices: []store.PushDevice{device("a1", "fcm")}}
	fcm := &fakeDelivery{status: map[string]delivery.Status{"a1": delivery.StatusRetryableError}}
	w := newTestWorker(configured("fcm"), s, map[string]delivery.Delivery{"fcm": fcm})
	last := job()
	last.Attempts = 7
	if err := w.processJob(context.Background(), last); err != nil {
		t.Fatalf("processJob: %v", err)
	}
	if len(s.completed) != 1 || len(s.resched) != 0 {
		t.Errorf("esperava descarte: completed=%v resched=%v", s.completed, s.resched)
	}
}
