// Package config carrega a configuração do papo-push a partir das variáveis
// de ambiente (godotenv + fallbacks). O worker é o único processo que possui
// credenciais de entrega (FCM direto ou relay remoto).
package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/sirupsen/logrus"
)

// LegacyProviderName é o nome do único provider do modo legado (PUSH_PROVIDERS
// ausente), configurado por GOOGLE_APPLICATION_CREDENTIALS ou
// FCM_RELAY_URL + FCM_RELAY_TOKEN.
const LegacyProviderName = "fcm"

// providerNameRe é o formato válido de um nome de provider em PUSH_PROVIDERS.
var providerNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ProviderConfig é a configuração de entrega de um provider: FCM direto
// (Credentials) ou relay remoto (RelayURL + RelayToken). Exatamente um dos
// dois modos.
type ProviderConfig struct {
	Name        string
	Credentials string
	RelayURL    string
	RelayToken  string
}

// IsRelay indica se o provider entrega por relay remoto (senão, FCM direto).
func (p ProviderConfig) IsRelay() bool {
	return p.RelayURL != ""
}

// Config é a configuração do papo-push.
type Config struct {
	// Banco (mesmo PostgreSQL do backend).
	DatabaseURL string

	// Entrega: FCM direto (FCMRelayURL vazio) ou relay remoto (URL definido).
	FCMRelayURL            string
	FCMRelayToken          string
	GoogleApplicationCreds string

	// Providers lista os providers de PUSH_PROVIDERS, na ordem informada. Vazio
	// = modo legado (um único provider "fcm", ver LegacyProviderName).
	Providers []ProviderConfig

	// Worker.
	PushWorkers        int
	PushBatchSize      int
	PushMaxAttempts    int
	PushRequestTimeout time.Duration
	PushHealthPort     int
	PushPreview        string
}

// Load carrega a configuração a partir das variáveis de ambiente.
func Load() *Config {
	if err := godotenv.Load(); err != nil {
		logrus.Info("Arquivo .env não encontrado, usando variáveis de ambiente")
	}
	return &Config{
		DatabaseURL:            getEnv("DATABASE_URL", ""),
		FCMRelayURL:            getEnv("FCM_RELAY_URL", ""),
		FCMRelayToken:          getEnv("FCM_RELAY_TOKEN", ""),
		GoogleApplicationCreds: getEnv("GOOGLE_APPLICATION_CREDENTIALS", ""),
		PushWorkers:            getEnvInt("PUSH_WORKERS", 2),
		PushBatchSize:          getEnvInt("PUSH_BATCH_SIZE", 50),
		PushMaxAttempts:        getEnvInt("PUSH_MAX_ATTEMPTS", 8),
		PushRequestTimeout:     time.Duration(getEnvInt("PUSH_REQUEST_TIMEOUT", 5)) * time.Second,
		PushHealthPort:         getEnvInt("PUSH_HEALTH_PORT", 9473),
		PushPreview:            getEnv("PUSH_PREVIEW", "full"),
		Providers:              loadProviders(os.LookupEnv),
	}
}

// providerEnvPrefix devolve o prefixo das variáveis de um provider:
// PUSH_PROVIDER_<NOME>_ (nome em maiúsculas, "-" vira "_").
func providerEnvPrefix(name string) string {
	return "PUSH_PROVIDER_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_"
}

// loadProviders lê PUSH_PROVIDERS e as variáveis de cada provider. Itens
// vazios são ignorados; sem nenhum item o resultado é nil (modo legado). A
// validação dos nomes e dos modos fica em Validate.
func loadProviders(lookup func(string) (string, bool)) []ProviderConfig {
	get := func(key string) string {
		v, _ := lookup(key)
		return strings.TrimSpace(v)
	}
	var providers []ProviderConfig
	for _, name := range strings.Split(get("PUSH_PROVIDERS"), ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		prefix := providerEnvPrefix(name)
		providers = append(providers, ProviderConfig{
			Name:        name,
			Credentials: get(prefix + "CREDENTIALS"),
			RelayURL:    get(prefix + "RELAY_URL"),
			RelayToken:  get(prefix + "RELAY_TOKEN"),
		})
	}
	return providers
}

// ResolvedProviders devolve os providers efetivos: os de PUSH_PROVIDERS ou,
// no modo legado, o provider único "fcm" montado das variáveis antigas.
func (c *Config) ResolvedProviders() []ProviderConfig {
	if len(c.Providers) > 0 {
		return c.Providers
	}
	return []ProviderConfig{{
		Name:        LegacyProviderName,
		Credentials: c.GoogleApplicationCreds,
		RelayURL:    c.FCMRelayURL,
		RelayToken:  c.FCMRelayToken,
	}}
}

// LegacyProviders indica o modo legado (PUSH_PROVIDERS ausente): todos os
// dispositivos do usuário são enviados pelo provider único, qualquer que seja
// o provider registrado.
func (c *Config) LegacyProviders() bool {
	return len(c.Providers) == 0
}

// validateLegacy valida o modo legado, como antes de PUSH_PROVIDERS existir.
func (c *Config) validateLegacy() error {
	switch {
	case c.FCMRelayURL == "":
		if c.GoogleApplicationCreds == "" {
			return fmt.Errorf("GOOGLE_APPLICATION_CREDENTIALS ausente: necessário para envio direto ao FCM")
		}
	case c.FCMRelayToken == "":
		return fmt.Errorf("FCM_RELAY_TOKEN ausente: necessário para autenticar no relay %s", c.FCMRelayURL)
	}
	return nil
}

// validateProvider valida o nome e o modo de entrega de um provider de
// PUSH_PROVIDERS: exatamente um entre FCM direto e relay.
func validateProvider(p ProviderConfig) error {
	if !providerNameRe.MatchString(p.Name) {
		return fmt.Errorf("nome de provider inválido em PUSH_PROVIDERS: %q (use %s)", p.Name, providerNameRe)
	}
	prefix := providerEnvPrefix(p.Name)
	credsVar, urlVar, tokenVar := prefix+"CREDENTIALS", prefix+"RELAY_URL", prefix+"RELAY_TOKEN"
	direct := p.Credentials != ""
	relay := p.RelayURL != "" || p.RelayToken != ""
	switch {
	case direct && relay:
		return fmt.Errorf("provider %s com modos misturados: %s e %s/%s definidos; use FCM direto ou relay, não os dois",
			p.Name, credsVar, urlVar, tokenVar)
	case !direct && !relay:
		return fmt.Errorf("provider %s sem configuração: defina %s (FCM direto) ou %s + %s (relay)",
			p.Name, credsVar, urlVar, tokenVar)
	case relay && p.RelayURL == "":
		return fmt.Errorf("%s ausente: necessário para o relay do provider %s", urlVar, p.Name)
	case relay && p.RelayToken == "":
		return fmt.Errorf("%s ausente: necessário para autenticar no relay %s", tokenVar, p.RelayURL)
	}
	return nil
}

// validateProviders valida o modo legado ou, com PUSH_PROVIDERS, cada provider
// e a unicidade dos nomes.
func (c *Config) validateProviders() error {
	if c.LegacyProviders() {
		return c.validateLegacy()
	}
	seen := make(map[string]struct{}, len(c.Providers))
	for _, p := range c.Providers {
		if _, dup := seen[p.Name]; dup {
			return fmt.Errorf("provider duplicado em PUSH_PROVIDERS: %q", p.Name)
		}
		seen[p.Name] = struct{}{}
		if err := validateProvider(p); err != nil {
			return err
		}
	}
	return nil
}

// Validate valida a configuração conforme o modo de entrega.
func (c *Config) Validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("DATABASE_URL ausente: defina a DSN do PostgreSQL")
	}
	if c.PushWorkers < 1 {
		c.PushWorkers = 1
	}
	if c.PushBatchSize < 1 {
		c.PushBatchSize = 50
	}
	if c.PushMaxAttempts < 1 {
		c.PushMaxAttempts = 8
	}
	if c.PushHealthPort <= 0 || c.PushHealthPort > 65535 {
		c.PushHealthPort = 9473
	}
	switch c.PushPreview {
	case "full", "generic", "hidden":
		// ok
	default:
		return fmt.Errorf("PUSH_PREVIEW inválido: use full, generic ou hidden")
	}
	return c.validateProviders()
}

func getEnv(key string, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value, exists := os.LookupEnv(key); exists {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			return parsed
		}
		logrus.Info("Valor inválido para " + key + ", usando padrão " + strconv.Itoa(defaultValue))
	}
	return defaultValue
}
