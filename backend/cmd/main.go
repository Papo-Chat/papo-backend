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
	"papo/internal/moderation"
	"papo/internal/services"
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

	// Rotina de manutenção (GC de mídia + purge de auditoria): roda no boot e
	// a cada 12h. O cancelamento do ctx para o scheduler e interrompe o job em
	// curso (a trigger append-only de audit_logs é recriada mesmo assim).
	maintenanceCtx, stopMaintenance := context.WithCancel(context.Background())
	defer stopMaintenance()
	go services.RunMaintenance(maintenanceCtx, cfg)

	// Moderação assíncrona de imagens (nudez/gore): fila limitada + worker
	// Python supervisionado por este processo (no-op quando
	// MODERATION_ENABLED=false); nunca bloqueia o envio de mensagem.
	moderationCtx, stopModeration := context.WithCancel(context.Background())
	defer stopModeration()
	moderation.Init(cfg, moderationCtx)

	e := echo.New()

	// IP do cliente (echo.Echo.IPExtractor): o fallback legacy do Echo confia
	// em X-Forwarded-For/X-Real-IP sem validação de proxy confiável (spoofing
	// de IP), então o extractor é sempre explícito.
	var cfIPs *utils.CloudflareIPs
	if cfg.CloudflareProxy {
		// Lista de IPs do Cloudflare: busca no boot e a cada 12h via API
		// (falha mantém a última lista válida; no boot, o fallback hardcoded).
		cfIPs = utils.NewCloudflareIPs()
		cfCtx, stopCF := context.WithCancel(context.Background())
		defer stopCF()
		go cfIPs.Run(cfCtx)

		// IP real = header CF-Connecting-IP (confiável porque o middleware
		// abaixo só deixa passar conexões vindas de IPs do Cloudflare).
		e.IPExtractor = middleware.CloudflareIPExtractor(cfIPs)
	} else {
		// Sem proxy: IP da conexão direta (nunca de headers).
		e.IPExtractor = middleware.DirectIPExtractor
	}

	// CORS antes dos demais middlewares: os preflights OPTIONS recebem os
	// cabeçalhos CORS mesmo quando as demais rotas respondem erro.
	e.Use(middleware.CORS(cfg.CORSOrigins))
	e.Use(echoMiddleware.RequestLogger())
	e.Use(echoMiddleware.Recover())
	e.Use(echoMiddleware.RequestID())
	e.Use(middleware.RequestIDMiddleware)
	// CLOUDFLARE_PROXY: barra conexões que não vêm de um IP do Cloudflare e
	// exige o header CF-Connecting-IP (IP real do cliente). Antes de
	// AuditContext/RateLimit, que usam o IP real.
	if cfIPs != nil {
		e.Use(middleware.CloudflareProxy(cfIPs))
	}
	// Injeta IP real e User-Agent no request context para a auditoria (a
	// camada de service só recebe o request context, sem o echo.Context).
	e.Use(middleware.AuditContext)
	// Rate limit global por IP em todos os endpoints (inclui o handshake
	// WebSocket em GET /ws). As rotas de auth mantêm um limite próprio mais
	// restrito, aplicado por cima deste.
	e.Use(middleware.RateLimit(cfg.RateLimit, cfg.RateBurst))
	// Limite global de corpo (4MB JSON). POST /messages é dispensado aqui e
	// usa o próprio limite de upload (110MB) na rota.
	e.Use(middleware.BodyLimit(middleware.MaxJSONBodySize, func(c echo.Context) bool {
		return c.Path() == "/messages"
	}))

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
	handlers.RegisterAdminRoutes(e, cfg)
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

	// Encerra a moderação de imagens (workers da fila + processo Python).
	moderation.Shutdown()

	// Encerra as conexões WebSocket ativas (close frame) e para o Hub.
	websocket.GetHub().Shutdown()

	utils.Info("Servidor desligado")
}
