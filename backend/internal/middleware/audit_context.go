package middleware

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/labstack/echo/v4"
)

// Chaves privadas para os valores de auditoria injetados no request context.
// Usam um tipo próprio para evitar colisão com outras chaves de contexto.
type auditContextKey int

const (
	auditIPKey auditContextKey = iota
	auditUserAgentKey
)

// AuditContext injeta o IP real e o User-Agent da requisição no contexto do
// request, para que a camada de service possa registrá-los na auditoria
// (services recebem apenas o request context, sem acesso ao echo.Context).
func AuditContext(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx := c.Request().Context()
		ctx = context.WithValue(ctx, auditIPKey, maskIP(c.RealIP()))
		ctx = context.WithValue(ctx, auditUserAgentKey, c.Request().UserAgent())
		c.SetRequest(c.Request().WithContext(ctx))
		return next(c)
	}
}

// maskIP maska o IP antes de gravar na auditoria (LGPD/GDPR): em IPv4 mantém os
// três primeiros octetos e substitui o último por "xxx" (ex.: 192.168.1.xxx);
// em IPv6 mantém apenas o primeiro hexteto. IP vazio retorna vazio; valores não
// reconhecidos viram "xxx". O objetivo é preservar contexto de rede sem
// armazenar o endereço completo do cliente.
func maskIP(ip string) string {
	if ip == "" {
		return ""
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "xxx"
	}
	if v4 := parsed.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d.%d.xxx", v4[0], v4[1], v4[2])
	}
	hextets := strings.Split(parsed.String(), ":")
	if hextets[0] != "" {
		return hextets[0] + ".xxx"
	}
	return "xxx"
}

// AuditIP retorna o IP real injetado por AuditContext, ou nil se ausente.
func AuditIP(ctx context.Context) *string {
	ip, _ := ctx.Value(auditIPKey).(string)
	if ip == "" {
		return nil
	}
	return &ip
}

// AuditUserAgent retorna o User-Agent injetado por AuditContext, ou nil se
// ausente.
func AuditUserAgent(ctx context.Context) *string {
	ua, _ := ctx.Value(auditUserAgentKey).(string)
	if ua == "" {
		return nil
	}
	return &ua
}
