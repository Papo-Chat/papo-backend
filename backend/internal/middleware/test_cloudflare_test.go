package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"papo/internal/utils"

	"github.com/labstack/echo/v4"
)

// testCloudflareIP está dentro de 104.16.0.0/13 (fallback hardcoded).
// testNonCloudflareIP é um IP público fora de qualquer faixa do Cloudflare.
const (
	testCloudflareIP    = "104.16.1.1"
	testNonCloudflareIP = "198.51.100.7"
)

// doCloudflareRequest passa uma requisição (conexão direta de ip, com ou sem o
// header CF-Connecting-IP) pelo CloudflareProxy e por um handler que responde
// 200.
func doCloudflareRequest(t *testing.T, ips *utils.CloudflareIPs, ip, connectingIP string) *httptest.ResponseRecorder {
	t.Helper()

	handler := CloudflareProxy(ips)(func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	c := newContext(t, http.MethodGet, "/", ip)
	if connectingIP != "" {
		c.Request().Header.Set(cfConnectingIPHeader, connectingIP)
	}
	if err := handler(c); err != nil {
		t.Fatalf("handler retornou erro: %v", err)
	}
	return recorder(c)
}

// --- CloudflareProxy ---

func TestCloudflareProxyAllowsCloudflarePeerWithValidHeader(t *testing.T) {
	ips := utils.NewCloudflareIPs()

	rec := doCloudflareRequest(t, ips, testCloudflareIP, "203.0.113.9")
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava status 200, obtive %d (corpo: %s)", rec.Code, rec.Body.String())
	}
}

func TestCloudflareProxyBlocksNonCloudflarePeer(t *testing.T) {
	ips := utils.NewCloudflareIPs()

	rec := doCloudflareRequest(t, ips, testNonCloudflareIP, "203.0.113.9")
	assertProblem(t, rec, http.StatusForbidden, "forbidden", "Acesso negado",
		"conexão direta não permitida: o servidor está atrás do proxy do Cloudflare", "")
}

func TestCloudflareProxyBlocksMissingConnectingIP(t *testing.T) {
	ips := utils.NewCloudflareIPs()

	rec := doCloudflareRequest(t, ips, testCloudflareIP, "")
	assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
		"header CF-Connecting-IP ausente ou inválido", "")
}

func TestCloudflareProxyBlocksInvalidConnectingIP(t *testing.T) {
	ips := utils.NewCloudflareIPs()

	for _, invalid := range []string{"ip-invalido", "203.0.113.9, 198.51.100.7"} {
		rec := doCloudflareRequest(t, ips, testCloudflareIP, invalid)
		assertProblem(t, rec, http.StatusBadRequest, "invalid-param", "Parâmetro inválido",
			"header CF-Connecting-IP ausente ou inválido", "")
	}
}

// --- CloudflareIPExtractor ---

func TestCloudflareIPExtractorCloudflarePeerUsesHeader(t *testing.T) {
	ips := utils.NewCloudflareIPs()
	extractor := CloudflareIPExtractor(ips)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = testCloudflareIP + ":12345"
	req.Header.Set(cfConnectingIPHeader, "203.0.113.9")

	if got := extractor(req); got != "203.0.113.9" {
		t.Errorf("esperava o IP do CF-Connecting-IP (203.0.113.9), obtive %q", got)
	}
}

func TestCloudflareIPExtractorNonCloudflarePeerIgnoresHeader(t *testing.T) {
	ips := utils.NewCloudflareIPs()
	extractor := CloudflareIPExtractor(ips)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = testNonCloudflareIP + ":12345"
	req.Header.Set(cfConnectingIPHeader, "203.0.113.9")

	if got := extractor(req); got != testNonCloudflareIP {
		t.Errorf("esperava o IP da conexão (%s), obtive %q (header de peer não-CF vazou)", testNonCloudflareIP, got)
	}
}

func TestCloudflareIPExtractorInvalidHeaderFallsBackToPeer(t *testing.T) {
	// Peer CF com header inválido: o middleware barraria a requisição, mas o
	// extractor é usado no log do bloqueio e deve retornar o peer.
	ips := utils.NewCloudflareIPs()
	extractor := CloudflareIPExtractor(ips)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = testCloudflareIP + ":12345"
	req.Header.Set(cfConnectingIPHeader, "lixo")

	if got := extractor(req); got != testCloudflareIP {
		t.Errorf("esperava o IP da conexão (%s), obtive %q", testCloudflareIP, got)
	}
}

func TestCloudflareIPExtractorIPv4MappedIPv6(t *testing.T) {
	// Peer dual-stack visto como IPv4-mapped deve casar com as CIDRs IPv4.
	ips := utils.NewCloudflareIPs()
	extractor := CloudflareIPExtractor(ips)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "[::ffff:104.16.1.1]:12345"
	req.Header.Set(cfConnectingIPHeader, "203.0.113.9")

	if got := extractor(req); got != "203.0.113.9" {
		t.Errorf("esperava o IP do CF-Connecting-IP (203.0.113.9), obtive %q", got)
	}
}

// --- DirectIPExtractor ---

func TestDirectIPExtractorIgnoresForwardedHeaders(t *testing.T) {
	// Regressão: o fallback legacy do Echo honra X-Forwarded-For/X-Real-IP
	// (spoofing); com DirectIPExtractor o IP é sempre o da conexão direta.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = testNonCloudflareIP + ":12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-Real-IP", "203.0.113.9")

	if got := DirectIPExtractor(req); got != testNonCloudflareIP {
		t.Errorf("esperava o IP da conexão (%s), obtive %q (headers forwarded foram honrados)", testNonCloudflareIP, got)
	}
}

func TestDirectIPExtractorIPv6(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "[2001:db8::1]:12345"

	if got := DirectIPExtractor(req); got != "2001:db8::1" {
		t.Errorf("esperava 2001:db8::1, obtive %q", got)
	}
}

// --- Integração com echo.Echo.IPExtractor ---

func TestCloudflareChainRealIPUsesConnectingIP(t *testing.T) {
	// Caminho real do main.go: e.IPExtractor + CloudflareProxy; o c.RealIP()
	// (usado por auditoria, rate limit e auth) deve retornar o IP do cliente.
	ips := utils.NewCloudflareIPs()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = testCloudflareIP + ":12345"
	req.Header.Set(cfConnectingIPHeader, "203.0.113.9")

	e := echo.New()
	e.IPExtractor = CloudflareIPExtractor(ips)
	c := e.NewContext(req, httptest.NewRecorder())

	var gotIP string
	handler := CloudflareProxy(ips)(func(cc echo.Context) error {
		gotIP = cc.RealIP()
		return cc.String(http.StatusOK, "ok")
	})
	if err := handler(c); err != nil {
		t.Fatalf("handler retornou erro: %v", err)
	}
	if gotIP != "203.0.113.9" {
		t.Errorf("esperava c.RealIP()=203.0.113.9, obtive %q", gotIP)
	}
}

func TestDirectChainRealIPIgnoresForwardedHeaders(t *testing.T) {
	// Caminho real do main.go sem CLOUDFLARE_PROXY: c.RealIP() nunca pode
	// vir de X-Forwarded-For/X-Real-IP.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = testNonCloudflareIP + ":12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-Real-IP", "203.0.113.9")

	e := echo.New()
	e.IPExtractor = DirectIPExtractor
	c := e.NewContext(req, httptest.NewRecorder())

	if got := c.RealIP(); got != testNonCloudflareIP {
		t.Errorf("esperava c.RealIP()=%s, obtive %q (fallback XFF/X-Real-IP foi honrado)", testNonCloudflareIP, got)
	}
}
