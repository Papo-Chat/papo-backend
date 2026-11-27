package config

import (
	"strings"
	"testing"
)

// lookupFrom cria um lookup de ambiente a partir de um mapa (sem tocar no
// ambiente real do processo).
func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

// TestLoadProviders verifica a leitura de PUSH_PROVIDERS e das variáveis de
// cada provider (nome em maiúsculas, "-" vira "_").
func TestLoadProviders(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want []ProviderConfig
	}{
		{"ausente é legado", map[string]string{}, nil},
		{"vazio é legado", map[string]string{"PUSH_PROVIDERS": ""}, nil},
		{"só vírgulas e espaços é legado", map[string]string{"PUSH_PROVIDERS": " , ,"}, nil},
		{
			"um provider direto",
			map[string]string{
				"PUSH_PROVIDERS":                "fcm",
				"PUSH_PROVIDER_FCM_CREDENTIALS": "/etc/papo/fcm.json",
			},
			[]ProviderConfig{{Name: "fcm", Credentials: "/etc/papo/fcm.json"}},
		},
		{
			"dois providers, um direto e um relay, com hífen e espaços",
			map[string]string{
				"PUSH_PROVIDERS":                     " fcm , meu-fork ",
				"PUSH_PROVIDER_FCM_CREDENTIALS":      "/etc/papo/fcm.json",
				"PUSH_PROVIDER_MEU_FORK_RELAY_URL":   "https://relay.example.com",
				"PUSH_PROVIDER_MEU_FORK_RELAY_TOKEN": "segredo",
			},
			[]ProviderConfig{
				{Name: "fcm", Credentials: "/etc/papo/fcm.json"},
				{Name: "meu-fork", RelayURL: "https://relay.example.com", RelayToken: "segredo"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := loadProviders(lookupFrom(tc.env))
			if len(got) != len(tc.want) {
				t.Fatalf("providers = %+v, esperado %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("provider %d = %+v, esperado %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestValidateProviders verifica a validação dos modos de entrega: legado, um
// provider, dois providers e os erros de configuração.
func TestValidateProviders(t *testing.T) {
	base := func(c Config) *Config {
		c.DatabaseURL = "postgres://localhost/papo"
		c.PushPreview = "full"
		return &c
	}
	direct := func(name string) ProviderConfig {
		return ProviderConfig{Name: name, Credentials: "/etc/papo/" + name + ".json"}
	}
	relay := func(name string) ProviderConfig {
		return ProviderConfig{Name: name, RelayURL: "https://relay.example.com", RelayToken: "segredo"}
	}

	cases := []struct {
		name    string
		cfg     *Config
		wantErr string // vazio = sem erro
	}{
		{"legado direto", base(Config{GoogleApplicationCreds: "/etc/papo/fcm.json"}), ""},
		{"legado relay", base(Config{FCMRelayURL: "https://relay.example.com", FCMRelayToken: "segredo"}), ""},
		{"legado sem nada", base(Config{}), "GOOGLE_APPLICATION_CREDENTIALS ausente"},
		{"legado relay sem token", base(Config{FCMRelayURL: "https://relay.example.com"}), "FCM_RELAY_TOKEN ausente"},
		{"um provider", base(Config{Providers: []ProviderConfig{direct("fcm")}}), ""},
		{"dois providers", base(Config{Providers: []ProviderConfig{direct("fcm"), relay("meu-fork")}}), ""},
		{
			"PUSH_PROVIDERS ignora as variáveis legadas",
			base(Config{GoogleApplicationCreds: "/x.json", Providers: []ProviderConfig{relay("fcm")}}),
			"",
		},
		{
			"modos misturados",
			base(Config{Providers: []ProviderConfig{{Name: "fcm", Credentials: "/x.json", RelayURL: "https://r", RelayToken: "t"}}}),
			"modos misturados",
		},
		{
			"token de relay sem url e com credenciais",
			base(Config{Providers: []ProviderConfig{{Name: "fcm", Credentials: "/x.json", RelayToken: "t"}}}),
			"modos misturados",
		},
		{"provider sem configuração", base(Config{Providers: []ProviderConfig{{Name: "fcm"}}}), "PUSH_PROVIDER_FCM_CREDENTIALS"},
		{
			"relay sem token",
			base(Config{Providers: []ProviderConfig{{Name: "meu-fork", RelayURL: "https://r"}}}),
			"PUSH_PROVIDER_MEU_FORK_RELAY_TOKEN ausente",
		},
		{
			"relay sem url",
			base(Config{Providers: []ProviderConfig{{Name: "meu-fork", RelayToken: "t"}}}),
			"PUSH_PROVIDER_MEU_FORK_RELAY_URL ausente",
		},
		{"nome com maiúscula", base(Config{Providers: []ProviderConfig{direct("FCM")}}), "nome de provider inválido"},
		{"nome com underscore", base(Config{Providers: []ProviderConfig{direct("meu_fork")}}), "nome de provider inválido"},
		{"nome começando com hífen", base(Config{Providers: []ProviderConfig{direct("-fcm")}}), "nome de provider inválido"},
		{"nome com 32 caracteres", base(Config{Providers: []ProviderConfig{direct(strings.Repeat("a", 32))}}), ""},
		{"nome com 33 caracteres", base(Config{Providers: []ProviderConfig{direct(strings.Repeat("a", 33))}}), "nome de provider inválido"},
		{"nome duplicado", base(Config{Providers: []ProviderConfig{direct("fcm"), relay("fcm")}}), "duplicado"},
		{"sem banco", &Config{PushPreview: "full", Providers: []ProviderConfig{direct("fcm")}}, "DATABASE_URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("esperava sem erro, obtive %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("erro = %v, esperado conter %q", err, tc.wantErr)
			}
		})
	}
}

// TestResolvedProviders verifica o provider único "fcm" do modo legado e a
// lista de PUSH_PROVIDERS no modo configurado.
func TestResolvedProviders(t *testing.T) {
	legacy := &Config{FCMRelayURL: "https://r", FCMRelayToken: "t", GoogleApplicationCreds: "/x.json"}
	if !legacy.LegacyProviders() {
		t.Fatal("sem Providers deveria ser legado")
	}
	got := legacy.ResolvedProviders()
	want := ProviderConfig{Name: LegacyProviderName, Credentials: "/x.json", RelayURL: "https://r", RelayToken: "t"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("legado = %+v, esperado [%+v]", got, want)
	}
	if !got[0].IsRelay() {
		t.Fatal("legado com FCM_RELAY_URL deveria ser relay")
	}

	multi := &Config{Providers: []ProviderConfig{{Name: "a", Credentials: "/a.json"}, {Name: "b"}}}
	if multi.LegacyProviders() {
		t.Fatal("com Providers não deveria ser legado")
	}
	if got := multi.ResolvedProviders(); len(got) != 2 || got[0].Name != "a" || got[0].IsRelay() {
		t.Fatalf("providers = %+v", got)
	}
}
