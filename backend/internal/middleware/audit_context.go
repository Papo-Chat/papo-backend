package middleware

import (
	"context"

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
		ctx = context.WithValue(ctx, auditIPKey, c.RealIP())
		ctx = context.WithValue(ctx, auditUserAgentKey, c.Request().UserAgent())
		c.SetRequest(c.Request().WithContext(ctx))
		return next(c)
	}
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
