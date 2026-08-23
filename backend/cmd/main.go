package main

import (
	"context"
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

func main() {
	cfg := config.LoadConfig()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	// Inicializa a conexão com PostgreSQL
	if err := storage.InitDB(cfg.DatabaseURL); err != nil {
		utils.Fatal("Falha ao iniciar conexão com PostgreSQL: " + err.Error())
	}
	defer storage.CloseDB()

	// Inicia o hub WebSocket (estado efêmero de transporte).
	go websocket.GetHub().Run()

	e := echo.New()

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
