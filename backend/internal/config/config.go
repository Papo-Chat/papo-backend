package config

import (
	"os"
	"strconv"

	"github.com/joho/godotenv"
	"github.com/sirupsen/logrus"
)

type Config struct {
	ServerPort        string
	DatabaseURL       string
	JWTSecret         string
	BaseURL           string
	MaxUsernameLength int
	MaxPasswordLength int
	AuthRateLimit     int
	AuthRateBurst     int
	RateLimit         int
	RateBurst         int
}

func LoadConfig() *Config {
	err := godotenv.Load()
	if err != nil {
		logrus.Info("Arquivo .env não encontrado, usando variáveis de ambiente")
	}

	MaxUsernameLength := getEnvInt("MAX_USERNAME_LENGTH", 16)
	MaxPasswordLength := getEnvInt("MAX_PASSWORD_LENGTH", 64)
	AuthRateLimit := getEnvInt("AUTH_RATE_LIMIT", 5)
	AuthRateBurst := getEnvInt("AUTH_RATE_BURST", 10)
	RateLimit := getEnvInt("RATE_LIMIT", 10)
	RateBurst := getEnvInt("RATE_BURST", 20)

	return &Config{
		ServerPort:        getEnv("SERVER_PORT", ""),
		DatabaseURL:       getEnv("DATABASE_URL", ""),
		JWTSecret:         getEnv("JWT_SECRET", ""),
		BaseURL:           getEnv("BASE_URL", "https://papo.com/"),
		MaxUsernameLength: MaxUsernameLength,
		MaxPasswordLength: MaxPasswordLength,
		AuthRateLimit:     AuthRateLimit,
		AuthRateBurst:     AuthRateBurst,
		RateLimit:         RateLimit,
		RateBurst:         RateBurst,
	}
}

func getEnv(key string, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}

	return defaultValue
}

// getEnvInt lê uma variável de ambiente inteira. Valores ausentes, inválidos
// ou não positivos usam o padrão, evitando configurações que desativariam
// limites (ex.: rate.Limit(0) negaria todas as requisições).
func getEnvInt(key string, defaultValue int) int {
	if value, exists := os.LookupEnv(key); exists {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			return parsed
		}
		logrus.Info("Valor inválido para " + key + ", usando padrão " + strconv.Itoa(defaultValue))
	}

	return defaultValue
}
