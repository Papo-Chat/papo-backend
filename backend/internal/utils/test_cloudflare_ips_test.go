package utils

import (
	"bytes"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
)

// cloudflareAPIBody monta o corpo JSON da API do Cloudflare (schema de
// GET /client/v4/ips) para os testes de applyAPIResponse.
func cloudflareAPIBody(success bool, ipv4, ipv6 []string) []byte {
	payload := map[string]any{
		"success": success,
		"result": map[string]any{
			"ipv4_cidrs": ipv4,
			"ipv6_cidrs": ipv6,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return body
}

// withCapturedLogger redireciona a saída do logger do projeto para um buffer
// durante o teste (permite asserting o WARN de drift sem poluir a saída).
func withCapturedLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := Logger.Out
	Logger.SetOutput(buf)
	t.Cleanup(func() { Logger.SetOutput(prev) })
	return buf
}

// --- parseCloudflareCIDRs ---

func TestParseCloudflareCIDRsValid(t *testing.T) {
	nets, err := parseCloudflareCIDRs([]string{"192.0.2.0/24", "2001:db8::/32"})
	if err != nil {
		t.Fatalf("esperava sucesso, obtive erro: %v", err)
	}
	if len(nets) != 2 {
		t.Fatalf("esperava 2 redes, obtive %d", len(nets))
	}
	if !nets[0].Contains(net.ParseIP("192.0.2.55")) {
		t.Errorf("192.0.2.55 deveria estar em 192.0.2.0/24")
	}
	if !nets[1].Contains(net.ParseIP("2001:db8::1")) {
		t.Errorf("2001:db8::1 deveria estar em 2001:db8::/32")
	}
}

func TestParseCloudflareCIDRsInvalid(t *testing.T) {
	nets, err := parseCloudflareCIDRs([]string{"192.0.2.0/24", "cidr-invalida"})
	if err == nil {
		t.Fatal("esperava erro para CIDR inválida")
	}
	if nets != nil {
		t.Errorf("esperava nil em falha (sem lista parcial), obtive %v", nets)
	}
}

// --- NewCloudflareIPs / IsCloudflareIP ---

func TestNewCloudflareIPsFallback(t *testing.T) {
	c := NewCloudflareIPs()

	if !c.IsCloudflareIP(net.ParseIP("104.16.1.1")) {
		t.Errorf("104.16.1.1 (104.16.0.0/13) deveria ser IP do Cloudflare")
	}
	if !c.IsCloudflareIP(net.ParseIP("2606:4700::1")) {
		t.Errorf("2606:4700::1 (2606:4700::/32) deveria ser IP do Cloudflare")
	}
	if c.IsCloudflareIP(net.ParseIP("8.8.8.8")) {
		t.Errorf("8.8.8.8 não é IP do Cloudflare")
	}
	if c.IsCloudflareIP(net.ParseIP("198.51.100.7")) {
		t.Errorf("198.51.100.7 não é IP do Cloudflare")
	}
}

func TestIsCloudflareIPv4MappedIPv6(t *testing.T) {
	// Servidores dual-stack podem ver o peer como ::ffff:a.b.c.d; o match
	// contra CIDRs IPv4 precisa funcionar.
	c := NewCloudflareIPs()
	if !c.IsCloudflareIP(net.ParseIP("::ffff:104.16.1.1")) {
		t.Errorf("::ffff:104.16.1.1 (IPv4-mapped) deveria casar com 104.16.0.0/13")
	}
}

func TestIsCloudflareIPConcurrentSwap(t *testing.T) {
	// IsCloudflareIP (leitura) e applyAPIResponse (troca da lista) rodam
	// concorrentes no servidor real; este teste valida a ausência de race
	// (rodar com -race).
	logBuf := withCapturedLogger(t)
	c := NewCloudflareIPs()
	body := cloudflareAPIBody(true, []string{"192.0.2.0/24"}, nil)

	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				_ = c.IsCloudflareIP(net.ParseIP("104.16.1.1"))
			}
		}
	}()

	for i := 0; i < 200; i++ {
		if err := c.applyAPIResponse(body); err != nil {
			t.Fatalf("applyAPIResponse: %v", err)
		}
	}
	close(done)
	wg.Wait()
	_ = logBuf
}

// --- applyAPIResponse ---

func TestApplyAPIResponseReplacesList(t *testing.T) {
	withCapturedLogger(t)
	c := NewCloudflareIPs()

	body := cloudflareAPIBody(true, []string{"192.0.2.0/24"}, []string{"2001:db8::/32"})
	if err := c.applyAPIResponse(body); err != nil {
		t.Fatalf("applyAPIResponse: %v", err)
	}

	if !c.IsCloudflareIP(net.ParseIP("192.0.2.55")) {
		t.Errorf("192.0.2.55 deveria ser CF após a atualização")
	}
	if !c.IsCloudflareIP(net.ParseIP("2001:db8::1")) {
		t.Errorf("2001:db8::1 deveria ser CF após a atualização")
	}
	if c.IsCloudflareIP(net.ParseIP("104.16.1.1")) {
		t.Errorf("104.16.1.1 não é mais CF após a lista ser substituída")
	}
}

func TestApplyAPIResponseMalformedJSON(t *testing.T) {
	withCapturedLogger(t)
	c := NewCloudflareIPs()

	if err := c.applyAPIResponse([]byte("{ não é json")); err == nil {
		t.Fatal("esperava erro para JSON inválido")
	}
	if !c.IsCloudflareIP(net.ParseIP("104.16.1.1")) {
		t.Errorf("a lista atual (fallback) deveria ser mantida em falha")
	}
}

func TestApplyAPIResponseSuccessFalse(t *testing.T) {
	withCapturedLogger(t)
	c := NewCloudflareIPs()

	body := cloudflareAPIBody(false, []string{"192.0.2.0/24"}, nil)
	if err := c.applyAPIResponse(body); err == nil {
		t.Fatal("esperava erro para success=false")
	}
	if c.IsCloudflareIP(net.ParseIP("192.0.2.55")) {
		t.Errorf("a lista não deveria ser aplicada com success=false")
	}
}

func TestApplyAPIResponseEmptyList(t *testing.T) {
	withCapturedLogger(t)
	c := NewCloudflareIPs()

	body := cloudflareAPIBody(true, nil, nil)
	if err := c.applyAPIResponse(body); err == nil {
		t.Fatal("esperava erro para lista vazia")
	}
	if !c.IsCloudflareIP(net.ParseIP("104.16.1.1")) {
		t.Errorf("a lista atual (fallback) deveria ser mantida em falha")
	}
}

func TestApplyAPIResponseInvalidCIDR(t *testing.T) {
	withCapturedLogger(t)
	c := NewCloudflareIPs()

	body := cloudflareAPIBody(true, []string{"192.0.2.0/24", "cidr-invalida"}, nil)
	if err := c.applyAPIResponse(body); err == nil {
		t.Fatal("esperava erro para CIDR inválida na resposta")
	}
	if c.IsCloudflareIP(net.ParseIP("192.0.2.55")) {
		t.Errorf("não pode aplicar lista parcial (fail-closed)")
	}
	if !c.IsCloudflareIP(net.ParseIP("104.16.1.1")) {
		t.Errorf("a lista atual (fallback) deveria ser mantida em falha")
	}
}

// --- warnHardcodedDrift ---

func TestApplyAPIResponseDriftWarning(t *testing.T) {
	logBuf := withCapturedLogger(t)
	c := NewCloudflareIPs()

	// Lista divergente do fallback → WARN com a faixa ausente.
	body := cloudflareAPIBody(true, []string{"192.0.2.0/24"}, nil)
	if err := c.applyAPIResponse(body); err != nil {
		t.Fatalf("applyAPIResponse: %v", err)
	}
	out := logBuf.String()
	if !strings.Contains(out, "desatualizado") {
		t.Errorf("esperava WARN de fallback desatualizado, obtive: %s", out)
	}
	if !strings.Contains(out, "192.0.2.0/24") {
		t.Errorf("o WARN deveria citar a faixa ausente no fallback, obtive: %s", out)
	}

	// Lista idêntica ao fallback → sem WARN.
	logBuf.Reset()
	body = cloudflareAPIBody(true, fallbackCloudflareCIDRs, nil)
	if err := c.applyAPIResponse(body); err != nil {
		t.Fatalf("applyAPIResponse: %v", err)
	}
	if strings.Contains(logBuf.String(), "desatualizado") {
		t.Errorf("não deveria haver WARN para lista idêntica ao fallback: %s", logBuf.String())
	}
}
