package services

import (
	"context"
	"errors"
	"testing"
)

// TestPushProviderSupported cobre a regra de aceite do provider: sem
// PUSH_PROVIDERS (modo legado) qualquer valor vale; definido, só os nomes
// listados, comparados exatamente.
func TestPushProviderSupported(t *testing.T) {
	cases := []struct {
		name      string
		providers string
		provider  string
		want      bool
	}{
		{"legado aceita fcm", "", "fcm", true},
		{"legado aceita qualquer provider", "", "meu-fork", true},
		{"configurado aceita nome listado", "fcm,meu-fork", "meu-fork", true},
		{"configurado aceita lista com espaços", "fcm, meu-fork", "meu-fork", true},
		{"configurado rejeita nome fora da lista", "fcm,meu-fork", "outro", false},
		{"configurado diferencia maiúsculas", "fcm", "FCM", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PUSH_PROVIDERS", tc.providers)
			if got := pushProviderSupported(tc.provider); got != tc.want {
				t.Fatalf("pushProviderSupported(%q) com PUSH_PROVIDERS=%q = %v, esperado %v",
					tc.provider, tc.providers, got, tc.want)
			}
		})
	}
}

// TestRegisterPushDeviceRejectsUnsupportedProvider garante que o registro para
// antes de gravar quando o provider não está em PUSH_PROVIDERS, inclusive o
// provider vazio, que vale "fcm" antes de ser validado.
func TestRegisterPushDeviceRejectsUnsupportedProvider(t *testing.T) {
	t.Setenv("PUSH_PROVIDERS", "meu-fork")
	for _, provider := range []string{"outro", ""} {
		_, err := RegisterPushDevice(context.Background(), "00000000-0000-0000-0000-000000000000",
			"tok-provider", "android", provider, "Pixel")
		if !errors.Is(err, ErrPushProviderUnsupported) {
			t.Fatalf("provider %q: erro %v, esperado ErrPushProviderUnsupported", provider, err)
		}
	}
}
