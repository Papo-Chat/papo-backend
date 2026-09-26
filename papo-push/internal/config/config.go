// Package config carrega a configuração do papo-push a partir das variáveis
// de ambiente (godotenv + fallbacks). O worker é o único processo que possui
// credenciais de entrega (FCM direto ou relay remoto).
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
	"github.com/sirupsen/logrus"
)

// Config é a configuração do papo-push.
type Config struct {
	// Banco (mesmo PostgreSQL do backend).
	DatabaseURL string

	// Entrega: FCM direto (FCMRelayURL vazio) ou relay remoto (URL definido).
	FCMRelayURL          string
	FCMRelayToken        string
	GoogleApplicationCreds string

	// Worker.
	PushWorkers          int
	PushBatchSize        int
	PushMaxAttempts      int
	PushRequestTimeout   time.Duration
	PushHealthPort       int
	PushPreview          string
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
	}
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
