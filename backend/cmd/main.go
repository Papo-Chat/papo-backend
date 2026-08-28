package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"papo/internal/config"
	"papo/internal/handlers"
	"papo/internal/middleware"
	"papo/internal/storage"
	"papo/internal/utils"
	"papo/internal/websocket"

	"github.com/labstack/echo/v4"
	echoMiddleware "github.com/labstack/echo/v4/middleware"
)

// minJWTSecretLength é o tamanho mínimo (em bytes) do segredo HMAC-SHA256
// (256 bits, recomendação OWASP/NIST para HS256).
const minJWTSecretLength = 32

// validateJWTSecret valida o segredo usado para assinar/validar os tokens
// HS256. O servidor não deve iniciar sem uma chave válida.
func validateJWTSecret(secret string) error {
	if secret == "" {
		return errors.New("JWT_SECRET ausente: defina a variável de ambiente JWT_SECRET para iniciar o servidor")
	}
	if len(secret) < minJWTSecretLength {
		return fmt.Errorf("JWT_SECRET muito curto: use pelo menos %d caracteres", minJWTSecretLength)
	}
	return nil
}

func main() {
	cfg := config.LoadConfig()

	if err := validateJWTSecret(cfg.JWTSecret); err != nil {
		utils.Fatal(err.Error())
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	// Inicializa a conexão com PostgreSQL
	if err := storage.InitDB(cfg.DatabaseURL); err != nil {
		utils.Fatal("Falha ao iniciar conexão com PostgreSQL: " + err.Error())
	}
	defer storage.CloseDB()

	// Inicia o hub WebSocket (estado efêmero de transporte).
	go websocket.GetHub().Run()

	e := echo.New()

	// CORS antes dos demais middlewares: os preflights OPTIONS recebem os
	// cabeçalhos CORS mesmo quando as demais rotas respondem erro.
	e.Use(middleware.CORS(cfg.CORSOrigins))
	e.Use(echoMiddleware.RequestLogger())
	e.Use(echoMiddleware.Recover())
	e.Use(echoMiddleware.RequestID())
	e.Use(middleware.RequestIDMiddleware)
	// Rate limit global por IP em todos os endpoints (inclui o handshake
	// WebSocket em GET /ws). As rotas de auth mantêm um limite próprio mais
	// restrito, aplicado por cima deste.
	e.Use(middleware.RateLimit(cfg.RateLimit, cfg.RateBurst))

	e.GET("/health", handlers.HealthHandler)
	handlers.RegisterAuthRoutes(e, cfg)
	handlers.RegisterUserRoutes(e, cfg)
	handlers.RegisterServerRoutes(e, cfg)
	handlers.RegisterChannelRoutes(e, cfg)
	handlers.RegisterMessageRoutes(e, cfg)
	handlers.RegisterAttachmentRoutes(e, cfg)
	handlers.RegisterMediaRoutes(e, cfg)
	handlers.RegisterLinkPreviewRoutes(e, cfg)
	handlers.RegisterEmojiRoutes(e, cfg)
	handlers.RegisterRoleRoutes(e, cfg)
	handlers.RegisterSearchRoutes(e, cfg)
	handlers.RegisterWebSocketRoutes(e, cfg)

	addr := fmt.Sprintf(":%s", cfg.ServerPort)
	go func() {
		if err := e.Start(addr); err != nil {
			utils.Fatal("Falha ao iniciar o servidor: " + err.Error())
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)
	<-quit

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := e.Shutdown(ctx); err != nil {
		utils.Fatal("Falha ao desligar o servidor: " + err.Error())
	}

	// Encerra as conexões WebSocket ativas (close frame) e para o Hub.
	websocket.GetHub().Shutdown()

	utils.Info("Servidor desligado")
}
