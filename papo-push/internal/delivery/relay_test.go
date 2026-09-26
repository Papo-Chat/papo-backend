package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"papo/push/internal/config"
)

// writeJSON escreve v como JSON no response writer (usada nos testes).
// Encode retorna apenas um erro, então o padrão _ = ... é seguro.
func writeJSON(w http.ResponseWriter, v interface{}) {
	_ = json.NewEncoder(w).Encode(v)
}

// testMsg cria uma mensagem padrão para os testes.
func testMsg() Message {
	return Message{
		Title: "João",
		Body:  "olá mundo",
		Data:  map[string]string{"type": "message", "message_id": "m1", "channel_id": "c1"},
	}
}

// newTestRelay cria o RelayDelivery para os testes.
func newTestRelay(t *testing.T, cfg *config.Config) Delivery {
	d, err := NewRelay(cfg)
	t.Helper()
	if err != nil {
		t.Fatalf("falha em NewRelay: %v", err)
	}
	return d
}

// TestRelayAllSent verifica que todos sent → AllSent.
func TestRelayAllSent(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeJSON(w, map[string]interface{}{
			"results": []map[string]string{
				{"token": "tok-1", "status": "sent"},
				{"token": "tok-2", "status": "sent"},
			},
		})
	}))
	defer server.Close()

	cfg := &config.Config{FCMRelayURL: server.URL, FCMRelayToken: "test-token", PushRequestTimeout: 5 * time.Second}
	d := newTestRelay(t, cfg)

	res, err := d.Send(ctx, testMsg(), []Target{{Token: "tok-1", Platform: "android"}, {Token: "tok-2", Platform: "ios"}})
	if err != nil {
		t.Fatalf("esperava sem erro, obtive %v", err)
	}
	if !res.AllSent() {
		t.Fatalf("esperava AllSent=true, obtive false")
	}
}

// TestRelayInvalidToken verifica que invalid_token é detectado.
func TestRelayInvalidToken(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeJSON(w, map[string]interface{}{
			"results": []map[string]string{
				{"token": "tok-1", "status": "sent"},
				{"token": "tok-2", "status": "invalid_token"},
			},
		})
	}))
	defer server.Close()

	cfg := &config.Config{FCMRelayURL: server.URL, FCMRelayToken: "test-token", PushRequestTimeout: 5 * time.Second}
	d := newTestRelay(t, cfg)

	res, err := d.Send(ctx, testMsg(), []Target{{Token: "tok-1", Platform: "android"}, {Token: "tok-2", Platform: "ios"}})
	if err != nil {
		t.Fatalf("esperava sem erro, obtive %v", err)
	}
	invalid := res.InvalidTokens()
	if len(invalid) != 1 || invalid[0] != "tok-2" {
		t.Fatalf("esperava [tok-2], obtive %v", invalid)
	}
}

// TestRelayRetryableError verifica que retryable_error é detectado.
func TestRelayRetryableError(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeJSON(w, map[string]interface{}{
			"results": []map[string]string{{"token": "tok-1", "status": "retryable_error"}},
		})
	}))
	defer server.Close()

	cfg := &config.Config{FCMRelayURL: server.URL, FCMRelayToken: "test-token", PushRequestTimeout: 5 * time.Second}
	d := newTestRelay(t, cfg)

	res, err := d.Send(ctx, testMsg(), []Target{{Token: "tok-1", Platform: "android"}})
	if err != nil {
		t.Fatalf("esperava sem erro, obtive %v", err)
	}
	if !res.HasRetryable() {
		t.Fatalf("esperava HasRetryable=true, obtive false")
	}
}

// TestRelayPermanentError verifica que permanent_error é detectado.
func TestRelayPermanentError(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeJSON(w, map[string]interface{}{
			"results": []map[string]string{{"token": "tok-1", "status": "permanent_error"}},
		})
	}))
	defer server.Close()

	cfg := &config.Config{FCMRelayURL: server.URL, FCMRelayToken: "test-token", PushRequestTimeout: 5 * time.Second}
	d := newTestRelay(t, cfg)

	res, err := d.Send(ctx, testMsg(), []Target{{Token: "tok-1", Platform: "android"}})
	if err != nil {
		t.Fatalf("esperava sem erro, obtive %v", err)
	}
	if !res.HasPermanent() {
		t.Fatalf("esperava HasPermanent=true, obtive false")
	}
}

// TestRelayNonOKStatus verifica que status != 200 → retryable + erro.
func TestRelayNonOKStatus(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	cfg := &config.Config{FCMRelayURL: server.URL, FCMRelayToken: "test-token", PushRequestTimeout: 5 * time.Second}
	d := newTestRelay(t, cfg)

	res, err := d.Send(ctx, testMsg(), []Target{{Token: "tok-1", Platform: "android"}})
	if err == nil {
		t.Fatalf("esperava erro, obtive nenhum")
	}
	if !res.HasRetryable() {
		t.Fatalf("esperava HasRetryable=true, obtive false")
	}
}

// TestRelayAuthHeader verifica que o header Bearer é enviado.
func TestRelayAuthHeader(t *testing.T) {
	ctx := context.Background()
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		writeJSON(w, map[string]interface{}{"results": []interface{}{}})
	}))
	defer server.Close()

	cfg := &config.Config{FCMRelayURL: server.URL, FCMRelayToken: "secret-token", PushRequestTimeout: 5 * time.Second}
	d := newTestRelay(t, cfg)

	if _, err := d.Send(ctx, testMsg(), []Target{{Token: "tok-1", Platform: "android"}}); err != nil {
		t.Fatalf("falha ao enviar: %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("esperava 'Bearer secret-token', obtive %q", gotAuth)
	}
}

// TestRelayTimeout verifica que o timeout do request é respeitado.
func TestRelayTimeout(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer server.Close()

	cfg := &config.Config{FCMRelayURL: server.URL, FCMRelayToken: "test-token", PushRequestTimeout: 500 * time.Millisecond}
	d := newTestRelay(t, cfg)

	deadline := time.Now().Add(3 * time.Second)
	res, err := d.Send(ctx, testMsg(), []Target{{Token: "tok-1", Platform: "android"}})
	if time.Now().After(deadline) {
		t.Fatalf("timeout não foi respeitado (demorou mais de 3s)")
	}
	if err == nil {
		t.Fatalf("esperava erro de timeout, obtive nenhum")
	}
	if !res.HasRetryable() {
		t.Fatalf("esperava HasRetryable=true, obtive false")
	}
}

// TestRelayMissingTokenInResponse verifica que token ausente na resposta → retryable.
func TestRelayMissingTokenInResponse(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeJSON(w, map[string]interface{}{
			"results": []map[string]string{{"token": "tok-1", "status": "sent"}},
		})
		// tok-2 não está na resposta.
	}))
	defer server.Close()

	cfg := &config.Config{FCMRelayURL: server.URL, FCMRelayToken: "test-token", PushRequestTimeout: 5 * time.Second}
	d := newTestRelay(t, cfg)

	res, err := d.Send(ctx, testMsg(), []Target{{Token: "tok-1", Platform: "android"}, {Token: "tok-2", Platform: "ios"}})
	if err != nil {
		t.Fatalf("esperava sem erro, obtive %v", err)
	}
	if !res.HasRetryable() {
		t.Fatalf("esperava HasRetryable=true (tok-2 ausente na resposta), obtive false")
	}
}

// TestRelayRequestShape verifica o shape do request enviado ao relay.
func TestRelayRequestShape(t *testing.T) {
	ctx := context.Background()
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = buf[:n]
		w.WriteHeader(http.StatusOK)
		writeJSON(w, map[string]interface{}{"results": []interface{}{}})
	}))
	defer server.Close()

	cfg := &config.Config{FCMRelayURL: server.URL, FCMRelayToken: "test-token", PushRequestTimeout: 5 * time.Second}
	d := newTestRelay(t, cfg)

	msg := testMsg()
	msg.Data["notification_id"] = "n1"
	if _, err := d.Send(ctx, msg, []Target{{Token: "tok-1", Platform: "android"}}); err != nil {
		t.Fatalf("falha ao enviar: %v", err)
	}
	s := string(gotBody)
	if !contains(s, `"targets"`) || !contains(s, `"notification"`) || !contains(s, `"data"`) {
		t.Fatalf("request sem as chaves esperadas: %s", s)
	}
	if !contains(s, `"tok-1"`) || !contains(s, `"android"`) {
		t.Fatalf("request sem o target esperado: %s", s)
	}
	if !contains(s, `"n1"`) {
		t.Fatalf("request sem notification_id: %s", s)
	}
}

// TestRelayRequestBodyContent verifica que o corpo contém title/body corretos.
func TestRelayRequestBodyContent(t *testing.T) {
	ctx := context.Background()
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = buf[:n]
		w.WriteHeader(http.StatusOK)
		writeJSON(w, map[string]interface{}{"results": []interface{}{}})
	}))
	defer server.Close()

	cfg := &config.Config{FCMRelayURL: server.URL, FCMRelayToken: "test-token", PushRequestTimeout: 5 * time.Second}
	d := newTestRelay(t, cfg)

	if _, err := d.Send(ctx, testMsg(), []Target{{Token: "tok-1", Platform: "android"}}); err != nil {
		t.Fatalf("falha ao enviar: %v", err)
	}
	s := string(gotBody)
	if !contains(s, `"title":"João"`) {
		t.Fatalf("request sem title: %s", s)
	}
	if !contains(s, `"body":"olá mundo"`) {
		t.Fatalf("request sem body: %s", s)
	}
}

// TestRelayDecodesResultCorrectly verifica que o parse da resposta é correto.
func TestRelayDecodesResultCorrectly(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeJSON(w, map[string]interface{}{
			"results": []map[string]string{
				{"token": "tok-a", "status": "sent"},
				{"token": "tok-b", "status": "sent"},
			},
		})
	}))
	defer server.Close()

	cfg := &config.Config{FCMRelayURL: server.URL, FCMRelayToken: "test-token", PushRequestTimeout: 5 * time.Second}
	d := newTestRelay(t, cfg)

	res, err := d.Send(ctx, testMsg(), []Target{{Token: "tok-a", Platform: "android"}, {Token: "tok-b", Platform: "ios"}})
	if err != nil {
		t.Fatalf("esperava sem erro, obtive %v", err)
	}
	if !res.AllSent() {
		t.Fatalf("esperava AllSent=true, obtive false")
	}
	if len(res.results) != 2 {
		t.Fatalf("esperava 2 results, obtive %d", len(res.results))
	}
}

// TestRelayConfigValidateRelayToken verifica que a validação do config exige
// FCM_RELAY_TOKEN quando FCM_RELAY_URL está definido.
func TestRelayConfigValidate(t *testing.T) {
	cfg := &config.Config{FCMRelayURL: "https://relay.example.com", FCMRelayToken: "", DatabaseURL: "postgres://localhost/papo"}
	if err := cfg.Validate(); err == nil {
		t.Fatalf("esperava erro por FCM_RELAY_TOKEN ausente, obtive nenhum")
	}
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
