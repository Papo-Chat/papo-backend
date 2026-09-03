package config

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"github.com/sirupsen/logrus"
)

type Config struct {
	ServerPort        string
	DatabaseURL       string
	JWTSecret         string
	HMACSecret        string
	BaseURL           string
	MaxUsernameLength int
	MaxPasswordLength int
	AuthRateLimit     int
	AuthRateBurst     int
	RateLimit         int
	RateBurst         int
	CORSOrigins       []string
	SameSite          bool
	CloudflareProxy   bool

	// Thumbnails de imagens (attachment e preview)
	ThumbnailEnabled   bool
	ThumbnailMaxDim    int
	GIFThumbnailMaxDim int
	ThumbnailMaxPixels int
	ThumbnailTimeout   time.Duration
	ThumbnailMaxConc   int

	// Link preview (OpenGraph/oEmbed)
	LinkPreviewEnabled    bool
	LinkPreviewTimeout    time.Duration
	LinkPreviewMaxURLs    int
	LinkPreviewCacheTTL   time.Duration
	OutboundMaxConc       int
	RobotsEnabled         bool
	RobotsCacheTTL        time.Duration
	PreviewFetchRateGlob  int
	PreviewFetchRateUser  int
	PreviewTitleMax       int
	PreviewDescriptionMax int

	// Retenção dos logs de auditoria (dias). A exclusão por retenção é feita
	// pela rotina de manutenção (services.RunMaintenance), que roda no boot e
	// a cada 12h.
	LogDuration time.Duration

	// Moderação de imagens (nudez/gore): worker Python supervisionado pelo
	// processo Go, inferência assíncrona fora do caminho crítico do envio de
	// mensagem. MODERATION_ENABLED=false desativa tudo (nenhum processo
	// extra; os attachments ficam 'pending' sem classificação).
	ModerationEnabled         bool
	ModerationWorkerCommand   string
	ModerationWorkerPath      string
	ModerationSocketPath      string
	ModerationModelsDir       string
	ModerationQueueSize       int
	ModerationConcurrency     int
	ModerationTimeout         time.Duration
	ModerationNudityMode      string
	ModerationGoreMode        string
	ModerationNudityThreshold float64
	ModerationGoreThreshold   float64
}

var (
	configInstance *Config
	once           sync.Once
)

func LoadConfig() *Config {
	once.Do(func() {
		err := godotenv.Load()
		if err != nil {
			logrus.Info("Arquivo .env não encontrado, usando variáveis de ambiente")
		}
	})
	configInstance = &Config{
		ServerPort:        getEnv("SERVER_PORT", ""),
		DatabaseURL:       getEnv("DATABASE_URL", ""),
		JWTSecret:         getEnv("JWT_SECRET", ""),
		HMACSecret:        getEnv("HMAC_SECRET", ""),
		BaseURL:           getEnv("BASE_URL", "https://papo.com/"),
		MaxUsernameLength: getEnvInt("MAX_USERNAME_LENGTH", 16),
		MaxPasswordLength: getEnvInt("MAX_PASSWORD_LENGTH", 64),
		AuthRateLimit:     getEnvInt("AUTH_RATE_LIMIT", 5),
		AuthRateBurst:     getEnvInt("AUTH_RATE_BURST", 10),
		RateLimit:         getEnvInt("RATE_LIMIT", 10),
		RateBurst:         getEnvInt("RATE_BURST", 20),
		// Padrão permite o frontend local em HTTP e HTTPS (README: localhost:5173).
		CORSOrigins: getEnvList("CORS_ORIGINS",
			[]string{"http://localhost:5173", "https://localhost:5173"}),
		SameSite: getEnvBool("SAME_SITE", true),

		// CLOUDFLARE_PROXY: servidor atrás do proxy do Cloudflare. Só conexões
		// vindas de IPs do Cloudflare são aceitas e o IP real do cliente vem do
		// header CF-Connecting-IP (ver middleware.CloudflareProxy).
		CloudflareProxy: getEnvBool("CLOUDFLARE_PROXY", false),

		ThumbnailEnabled:   getEnvBool("THUMBNAIL_ENABLED", true),
		ThumbnailMaxDim:    getEnvInt("THUMBNAIL_MAX_DIM", 1024),
		GIFThumbnailMaxDim: getEnvInt("GIF_THUMBNAIL_MAX_DIM", 512),
		ThumbnailMaxPixels: getEnvInt("THUMBNAIL_MAX_PIXELS", 50000000),
		ThumbnailTimeout:   time.Duration(getEnvInt("THUMBNAIL_TIMEOUT", 5)) * time.Second,
		ThumbnailMaxConc:   getEnvInt("THUMBNAIL_MAX_CONCURRENCY", 4),

		LinkPreviewEnabled:    getEnvBool("LINK_PREVIEW_ENABLED", true),
		LinkPreviewTimeout:    time.Duration(getEnvInt("LINK_PREVIEW_TIMEOUT", 8)) * time.Second,
		LinkPreviewMaxURLs:    getEnvInt("LINK_PREVIEW_MAX_URLS", 2),
		LinkPreviewCacheTTL:   time.Duration(getEnvInt("LINK_PREVIEW_CACHE_TTL", 86400)) * time.Second,
		OutboundMaxConc:       getEnvInt("OUTBOUND_MAX_CONCURRENCY", 4),
		RobotsEnabled:         getEnvBool("ROBOTS_ENABLED", true),
		RobotsCacheTTL:        time.Duration(getEnvInt("ROBOTS_CACHE_TTL", 3600)) * time.Second,
		PreviewFetchRateGlob:  getEnvInt("PREVIEW_FETCH_RATE_GLOBAL", 30),
		PreviewFetchRateUser:  getEnvInt("PREVIEW_FETCH_RATE_PER_USER", 10),
		PreviewTitleMax:       getEnvInt("PREVIEW_TITLE_MAX", 200),
		PreviewDescriptionMax: getEnvInt("PREVIEW_DESCRIPTION_MAX", 300),

		LogDuration: time.Duration(getEnvInt("LOG_DURATION", 90)) * 24 * time.Hour,

		ModerationEnabled:         getEnvBool("MODERATION_ENABLED", false),
		ModerationWorkerCommand:   getEnv("MODERATION_WORKER_COMMAND", "python3"),
		ModerationWorkerPath:      getEnv("MODERATION_WORKER_PATH", "moderation_worker/worker.py"),
		ModerationSocketPath:      getEnv("MODERATION_SOCKET_PATH", "data/moderation.sock"),
		ModerationModelsDir:       getEnv("MODERATION_MODELS_DIR", "data/models"),
		ModerationQueueSize:       getEnvInt("MODERATION_QUEUE_SIZE", 256),
		ModerationConcurrency:     getEnvInt("MODERATION_CONCURRENCY", 1),
		ModerationTimeout:         time.Duration(getEnvInt("MODERATION_TIMEOUT_SECONDS", 10)) * time.Second,
		ModerationNudityMode:      getEnv("MODERATION_NUDITY_MODE", "off"),
		ModerationGoreMode:        getEnv("MODERATION_GORE_MODE", "off"),
		ModerationNudityThreshold: getEnvFloat("MODERATION_NUDITY_THRESHOLD", 0.8),
		ModerationGoreThreshold:   getEnvFloat("MODERATION_GORE_THRESHOLD", 0.8),
	}

	return configInstance
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

// getEnvBool lê uma variável de ambiente booleana (strconv.ParseBool).
// Valores ausentes ou inválidos usam o padrão.
func getEnvBool(key string, defaultValue bool) bool {
	if value, exists := os.LookupEnv(key); exists {
		if parsed, err := strconv.ParseBool(value); err == nil {
			return parsed
		}
		logrus.Info("Valor inválido para " + key + ", usando padrão " + strconv.FormatBool(defaultValue))
	}

	return defaultValue
}

// getEnvFloat lê uma variável de ambiente float. Valores ausentes, inválidos
// ou fora de [0,1] usam o padrão.
func getEnvFloat(key string, defaultValue float64) float64 {
	if value, exists := os.LookupEnv(key); exists {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil && parsed >= 0 && parsed <= 1 {
			return parsed
		}
		logrus.Info("Valor inválido para " + key + ", usando padrão " + strconv.FormatFloat(defaultValue, 'f', -1, 64))
	}

	return defaultValue
}

// getEnvList lê uma variável de ambiente com valores separados por vírgula.
// Valores ausentes ou vazios usam o padrão.
func getEnvList(key string, defaultValue []string) []string {
	value, exists := os.LookupEnv(key)
	if !exists || value == "" {
		return defaultValue
	}

	origins := make([]string, 0)
	for _, item := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			origins = append(origins, trimmed)
		}
	}
	if len(origins) == 0 {
		return defaultValue
	}

	return origins
}
