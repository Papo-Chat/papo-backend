package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"papo/internal/storage"
)

// TestRegisterPushDeviceProviders verifica a validação do provider conforme
// PUSH_PROVIDERS: ausente (modo legado) aceita qualquer valor; definido aceita
// só os listados, e provider vazio vale "fcm" antes de validar.
func TestRegisterPushDeviceProviders(t *testing.T) {
	cases := []struct {
		name         string
		providers    string // valor de PUSH_PROVIDERS; vazio = não definido
		provider     string // vazio = campo ausente
		wantStatus   int
		wantProvider string // provider gravado quando 204
	}{
		{"legado aceita fcm", "", "fcm", http.StatusNoContent, "fcm"},
		{"legado aceita qualquer provider", "", "meu-fork", http.StatusNoContent, "meu-fork"},
		{"legado: vazio vira fcm", "", "", http.StatusNoContent, "fcm"},
		{"configurado aceita o primeiro", "fcm,meu-fork", "fcm", http.StatusNoContent, "fcm"},
		{"configurado aceita o segundo", "fcm,meu-fork", "meu-fork", http.StatusNoContent, "meu-fork"},
		{"configurado aceita lista com espaços", "fcm, meu-fork", "meu-fork", http.StatusNoContent, "meu-fork"},
		{"configurado rejeita provider desconhecido", "fcm,meu-fork", "outro", http.StatusBadRequest, ""},
		{"configurado diferencia maiúsculas", "fcm", "FCM", http.StatusBadRequest, ""},
		{"configurado: vazio vira fcm e é aceito", "fcm,meu-fork", "", http.StatusNoContent, "fcm"},
		{"configurado: vazio vira fcm e é rejeitado se fcm não está na lista", "meu-fork", "", http.StatusBadRequest, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PUSH_PROVIDERS", tc.providers)
			e := newApp()
			userID, authToken := registerAndLogin(t, e)

			body := map[string]string{"token": "tok-" + randHex(6), "platform": "android"}
			if tc.provider != "" {
				body["provider"] = tc.provider
			}
			payload, _ := json.Marshal(body)
			rec := do(t, e, http.MethodPut, "/users/me/push-device", payload, authCookie(authToken))

			devices, err := storage.ListEnabledPushDevices(context.Background(), userID)
			if err != nil {
				t.Fatalf("falha ao listar dispositivos: %v", err)
			}

			if tc.wantStatus == http.StatusBadRequest {
				assertProblem(t, rec, http.StatusBadRequest, "push-provider-unsupported",
					"Provedor de push não suportado", "provedor de push não suportado por este servidor")
				if len(devices) != 0 {
					t.Fatalf("dispositivo não deveria ser gravado, obtive %d", len(devices))
				}
				return
			}
			if rec.Code != http.StatusNoContent {
				t.Fatalf("esperava 204, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
			}
			if len(devices) != 1 || devices[0].Provider != tc.wantProvider {
				t.Fatalf("esperava 1 dispositivo com provider %q, obtive %+v", tc.wantProvider, devices)
			}
		})
	}
}

// TestRemovePushDeviceIgnoresProvider verifica que o DELETE não valida o
// provider: um dispositivo registrado antes de o provider sair de
// PUSH_PROVIDERS ainda pode ser removido por token.
func TestRemovePushDeviceIgnoresProvider(t *testing.T) {
	t.Setenv("PUSH_PROVIDERS", "")
	e := newApp()
	userID, authToken := registerAndLogin(t, e)

	registerBody, _ := json.Marshal(map[string]string{"token": "tok-antigo", "platform": "android", "provider": "antigo"})
	rec := do(t, e, http.MethodPut, "/users/me/push-device", registerBody, authCookie(authToken))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("registro: esperava 204, obtive %d", rec.Code)
	}

	t.Setenv("PUSH_PROVIDERS", "fcm")
	removeBody, _ := json.Marshal(map[string]string{"token": "tok-antigo", "provider": "antigo"})
	rec = do(t, e, http.MethodDelete, "/users/me/push-device", removeBody, authCookie(authToken))
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
