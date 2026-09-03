package middleware

import (
	"net"
	"net/http"

	"papo/internal/config"
	"papo/internal/utils"

	"github.com/labstack/echo/v4"
)

// cfConnectingIPHeader é o header definido pelo próprio Cloudflare com o IP
// real do cliente (presente em todo tráfego proxied; o Cloudflare remove o
// valor enviado pelo cliente e define o real, então não pode ser forjado).
const cfConnectingIPHeader = "CF-Connecting-IP"

// peerIP extrai o IP da conexão direta (RemoteAddr), sem confiar em nenhum
// header. Retorna ok=false se o RemoteAddr não contiver um IP válido.
func peerIP(r *http.Request) (net.IP, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return nil, false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, false
	}
	return ip, true
}

// CloudflareProxy (CLOUDFLARE_PROXY=true) barra toda requisição cuja conexão
// direta não vem de um IP do Cloudflare (403). Requisições vindas do
// Cloudflare precisam ter o header CF-Connecting-IP com um IP válido (o IP
// real do cliente); sem ele, a requisição é barrada (400, fail-closed).
// Deve ser registrado antes de AuditContext e RateLimit, que usam o IP real.
func CloudflareProxy(ips *utils.CloudflareIPs) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			peer, ok := peerIP(c.Request())
			if !ok || !ips.IsCloudflareIP(peer) {
				cfg := config.LoadConfig()
				return utils.SendProblem(c, cfg.BaseURL, http.StatusForbidden,
					"forbidden", "Acesso negado",
					"conexão direta não permitida: o servidor está atrás do proxy do Cloudflare")
			}

			clientIP := c.Request().Header.Get(cfConnectingIPHeader)
			if net.ParseIP(clientIP) == nil {
				cfg := config.LoadConfig()
				return utils.SendProblem(c, cfg.BaseURL, http.StatusBadRequest,
					"invalid-param", "Parâmetro inválido",
					"header CF-Connecting-IP ausente ou inválido")
			}

			return next(c)
		}
	}
}

// CloudflareIPExtractor é o echo.Echo.IPExtractor para CLOUDFLARE_PROXY=true:
// conexão vinda de um IP do Cloudflare → IP do header CF-Connecting-IP (IP
// real do cliente); caso contrário → IP da conexão direta (usado, por exemplo,
// no log de uma requisição barrada pelo CloudflareProxy).
func CloudflareIPExtractor(ips *utils.CloudflareIPs) func(r *http.Request) string {
	return func(r *http.Request) string {
		peer, ok := peerIP(r)
		if !ok {
			return ""
		}
		if ips.IsCloudflareIP(peer) {
			if clientIP := r.Header.Get(cfConnectingIPHeader); clientIP != "" && net.ParseIP(clientIP) != nil {
				return clientIP
			}
		}
		return peer.String()
	}
}

// DirectIPExtractor é o echo.Echo.IPExtractor usado sem CLOUDFLARE_PROXY:
// sempre o IP da conexão direta. Nunca confia em X-Forwarded-For/X-Real-IP —
// o fallback legacy do Echo os aceita sem validação de proxy confiável,
// permitindo spoofing de IP (burla de rate limit e de banimento, adulteração
// de auditoria).
func DirectIPExtractor(r *http.Request) string {
	peer, ok := peerIP(r)
	if !ok {
		return ""
	}
	return peer.String()
}
