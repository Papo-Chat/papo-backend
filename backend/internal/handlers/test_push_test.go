package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"papo/internal/models"
	"papo/internal/storage"
)

// pushJobsForUser lista os jobs de push de um usuário (apenas para testes;
// consulta direta da push_outbox).
func pushJobsForUser(t *testing.T, userID string) ([]storage.PushJob, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := storage.GetDB().QueryContext(ctx,
		`SELECT id, user_id, payload, notification_id, attempts, next_attempt_at, created_at, last_error
		 FROM push_outbox
		 WHERE user_id = $1
		 ORDER BY created_at ASC, id ASC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	jobs := make([]storage.PushJob, 0)
	for rows.Next() {
		var job storage.PushJob
		var lastError sql.NullString
		if err := rows.Scan(
			&job.ID,
			&job.UserID,
			&job.Payload,
			&job.NotificationID,
			&job.Attempts,
			&job.NextAttemptAt,
			&job.CreatedAt,
			&lastError,
		); err != nil {
			return nil, err
		}
		job.LastError = lastError.String
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return jobs, nil
}

// parsePushPayload decodifica o payload (PushJobPayload) de um job.
func parsePushPayload(t *testing.T, payload string) models.PushJobPayload {
	var p models.PushJobPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatalf("falha ao decodificar payload: %v", err)
	}
	return p
}

// waitForPushJobs consulta a push_outbox até a quantidade esperada de jobs
// aparecer (o disparo roda em goroutine de background), falhando o teste se
// não chegar a tempo.
func waitForPushJobs(t *testing.T, userID string, want int) []storage.PushJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		jobs, err := pushJobsForUser(t, userID)
		if err != nil {
			t.Fatalf("falha ao listar jobs de push: %v", err)
		}
		if len(jobs) >= want {
			return jobs
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout aguardando %d jobs de push para o usuário %s, obtive %d", want, userID, len(jobs))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRegisterPushDeviceHandler registra um dispositivo de push via
// PUT /users/me/push-device e verifica o 204 e a persistência.
func TestRegisterPushDeviceHandler(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	payload, _ := json.Marshal(map[string]string{
		"token":       "fcm-token-abc",
		"platform":    "android",
		"device_name": "Pixel",
		"provider":    "fcm",
	})

	rec := do(t, e, http.MethodPut, "/users/me/push-device", payload, authCookie(token))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("esperava 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	devices, err := storage.ListEnabledPushDevices(context.Background(), userID)
	if err != nil {
		t.Fatalf("falha ao listar dispositivos: %v", err)
	}
	found := false
	for _, d := range devices {
		if d.Token == "fcm-token-abc" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("dispositivo fcm-token-abc não foi encontrado para o usuário %s", userID)
	}
}

// TestRegisterPushDeviceHandlerUpsert verifica que o registro é idempotente
// (upsert): o mesmo token não cria duplicata e atualiza os metadados.
func TestRegisterPushDeviceHandlerUpsert(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	payload, _ := json.Marshal(map[string]string{
		"token":       "fcm-token-upsert",
		"platform":    "android",
		"device_name": "Pixel",
	})
	rec := do(t, e, http.MethodPut, "/users/me/push-device", payload, authCookie(token))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("primeiro registro: esperava 204, obtive %d", rec.Code)
	}

	payload, _ = json.Marshal(map[string]string{
		"token":       "fcm-token-upsert",
		"platform":    "android",
		"device_name": "Pixel 2",
	})
	rec = do(t, e, http.MethodPut, "/users/me/push-device", payload, authCookie(token))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("segundo registro (upsert): esperava 204, obtive %d", rec.Code)
	}

	devices, err := storage.ListEnabledPushDevices(context.Background(), userID)
	if err != nil {
		t.Fatalf("falha ao listar dispositivos: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("esperava 1 dispositivo, obtive %d", len(devices))
	}
	if devices[0].DeviceName == nil || *devices[0].DeviceName != "Pixel 2" {
		t.Fatalf("nome do dispositivo não foi atualizado: %v", devices[0].DeviceName)
	}
}

// TestRegisterPushDeviceHandlerInvalidPlatform verifica a rejeição de
// plataforma inválida.
func TestRegisterPushDeviceHandlerInvalidPlatform(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	payload, _ := json.Marshal(map[string]string{
		"token":    "fcm-token-bad",
		"platform": "linux",
	})
	rec := do(t, e, http.MethodPut, "/users/me/push-device", payload, authCookie(token))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("esperava 400, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// TestRegisterPushDeviceHandlerMissingToken verifica a rejeição de token ausente.
func TestRegisterPushDeviceHandlerMissingToken(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	payload, _ := json.Marshal(map[string]string{
		"platform": "android",
	})
	rec := do(t, e, http.MethodPut, "/users/me/push-device", payload, authCookie(token))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("esperava 400, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// TestRegisterPushDeviceHandlerUnauthenticated verifica a rejeição sem cookie.
func TestRegisterPushDeviceHandlerUnauthenticated(t *testing.T) {
	e := newApp()
	payload, _ := json.Marshal(map[string]string{
		"token":    "fcm-token-unauth",
		"platform": "android",
	})
	rec := do(t, e, http.MethodPut, "/users/me/push-device", payload, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("esperava 401, obtive %d", rec.Code)
	}
}

// TestRemovePushDeviceHandler remove um dispositivo de push via
// DELETE /users/me/push-device e verifica o 204 e a remoção.
func TestRemovePushDeviceHandler(t *testing.T) {
	e := newApp()
	userID, token := registerAndLogin(t, e)

	registerBody, _ := json.Marshal(map[string]string{
		"token":    "fcm-token-remove",
		"platform": "android",
	})
	rec := do(t, e, http.MethodPut, "/users/me/push-device", registerBody, authCookie(token))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("registro: esperava 204, obtive %d", rec.Code)
	}

	removeBody, _ := json.Marshal(map[string]string{
		"token": "fcm-token-remove",
	})
	rec = do(t, e, http.MethodDelete, "/users/me/push-device", removeBody, authCookie(token))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("remoção: esperava 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	devices, err := storage.ListEnabledPushDevices(context.Background(), userID)
	if err != nil {
		t.Fatalf("falha ao listar dispositivos: %v", err)
	}
	if len(devices) != 0 {
		t.Fatalf("esperava 0 dispositivos após remoção, obtive %d", len(devices))
	}
}

// TestRemovePushDeviceHandlerNotFound verifica a rejeição (404) para token inexistente.
func TestRemovePushDeviceHandlerNotFound(t *testing.T) {
	e := newApp()
	_, token := registerAndLogin(t, e)

	removeBody, _ := json.Marshal(map[string]string{
		"token": "fcm-token-nonexistent",
	})
	rec := do(t, e, http.MethodDelete, "/users/me/push-device", removeBody, authCookie(token))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("esperava 404, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

// TestRemovePushDeviceHandlerUnauthenticated verifica a rejeição sem cookie.
func TestRemovePushDeviceHandlerUnauthenticated(t *testing.T) {
	e := newApp()
	removeBody, _ := json.Marshal(map[string]string{
		"token": "fcm-token-unauth",
	})
	rec := do(t, e, http.MethodDelete, "/users/me/push-device", removeBody, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("esperava 401, obtive %d", rec.Code)
	}
}

// TestPushJobCreatedOnMention verifica que ao criar uma mensagem com menção,
// um job de push é gravado na outbox para o usuário mencionado (integração
// backend → push_outbox).
func TestPushJobCreatedOnMention(t *testing.T) {
	t.Setenv("USE_FCM_RELAY", "true")
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	otherID, otherToken := registerAndLogin(t, e)

	// Registra um dispositivo para o usuário mencionado (alvo do push).
	registerBody, _ := json.Marshal(map[string]string{
		"token":    "fcm-push-job-test",
		"platform": "android",
	})
	rec := do(t, e, http.MethodPut, "/users/me/push-device", registerBody, authCookie(otherToken))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("registro dispositivo: esperava 204, obtive %d", rec.Code)
	}

	// Menção direta @mention(<@user_id>) gera notificação (e job de push) para o alvo.
	rec = doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": "olá @mention(<@" + otherID + ">)"}, nil, authCookie(ownerToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mensagem: esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	jobs := waitForPushJobs(t, otherID, 1)

	payload := parsePushPayload(t, jobs[0].Payload)
	if payload.Type != "message" {
		t.Fatalf("esperava type=message, obtive %s", payload.Type)
	}
	if payload.MessageID == "" {
		t.Fatal("payload sem message_id")
	}
	if payload.ChannelID != channel.ID {
		t.Fatalf("esperava channel_id=%s, obtive %s", channel.ID, payload.ChannelID)
	}
	if payload.Author == "" {
		t.Fatal("payload sem author")
	}
	mentioned, err := storage.GetUserByID(context.Background(), otherID)
	if err != nil {
		t.Fatalf("falha ao buscar usuário mencionado: %v", err)
	}
	if payload.Preview != "olá @"+mentioned.Username {
		t.Fatalf("preview deveria traduzir a menção para username; obtive %q", payload.Preview)
	}
}

// TestPushJobCreatedOnEphemeralAllSetting verifica que uma mensagem 'all' sem
// trigger (sem row de notificação) também grava um job de push com
// notification_id NULL (id efêmero no payload).
func TestPushJobCreatedOnEphemeralAllSetting(t *testing.T) {
	t.Setenv("USE_FCM_RELAY", "true")
	e := newApp()
	ownerID, ownerToken := registerAndLogin(t, e)
	createServerFor(t, ownerID)
	channel := createChannelFor(t, "chn_"+randHex(4))
	otherID, otherToken := registerAndLogin(t, e)

	// Registra um dispositivo para o alvo.
	registerBody, _ := json.Marshal(map[string]string{
		"token":    "fcm-ephemeral-test",
		"platform": "android",
	})
	rec := do(t, e, http.MethodPut, "/users/me/push-device", registerBody, authCookie(otherToken))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("registro dispositivo: esperava 204, obtive %d", rec.Code)
	}

	// Configura o alvo para 'all' (recebe tudo).
	settingBody, _ := json.Marshal(map[string]string{"notification_settings": "all"})
	rec = do(t, e, http.MethodPost,
		fmt.Sprintf("/channels/%s/user/%s/settings", channel.ID, otherID),
		settingBody, authCookie(otherToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("configuração: esperava 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	// Mensagem sem menção: com 'all', gera evento efêmero (sem row) + job.
	rec = doMultipart(t, e, http.MethodPost, "/messages",
		map[string]string{"channel_id": channel.ID, "content": "mensagem sem menção"}, nil, authCookie(ownerToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mensagem: esperava status 201, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}

	jobs := waitForPushJobs(t, otherID, 1)

	payload := parsePushPayload(t, jobs[0].Payload)
	if payload.Type != "message" {
		t.Fatalf("esperava type=message, obtive %s", payload.Type)
	}
	if payload.ChannelID != channel.ID {
		t.Fatalf("esperava channel_id=%s, obtive %s", channel.ID, payload.ChannelID)
	}
}
