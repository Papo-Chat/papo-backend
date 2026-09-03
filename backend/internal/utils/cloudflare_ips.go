package utils

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	// cloudflareIPsURL é o endpoint público (sem auth) com as faixas de IPs do
	// Cloudflare. Schema: { success, result: { ipv4_cidrs, ipv6_cidrs, etag } }.
	cloudflareIPsURL = "https://api.cloudflare.com/client/v4/ips"

	// cloudflareRefreshInterval é o intervalo entre atualizações da lista de
	// IPs (a rotina também roda no boot).
	cloudflareRefreshInterval = 12 * time.Hour

	// cloudflareRefreshTimeout limita uma execução do refresh (a API não deve
	// demorar além disso; o job não pode segurar o scheduler).
	cloudflareRefreshTimeout = 30 * time.Second

	// cloudflareClientTimeout é o timeout total do client HTTP do refresh.
	cloudflareClientTimeout = 10 * time.Second

	// cloudflareMaxResponseBytes é o teto do corpo da resposta da API (a
	// resposta real tem ~1KB; o teto fecha resposta anômala/gigante).
	cloudflareMaxResponseBytes = 1 << 20
)

// fallbackCloudflareCIDRs é a lista de faixas de IPs do Cloudflare usada
// quando a API não está acessível (ex.: boot sem rede) e como baseline para o
// aviso de desatualização (warnHardcodedDrift). Fonte: resposta da API em
// 2026-09-03. Quando a API divergir desta lista, o operador recebe um WARN e
// deve atualizá-la.
var fallbackCloudflareCIDRs = []string{
	// IPv4
	"103.21.244.0/22",
	"103.22.200.0/22",
	"103.31.4.0/22",
	"104.16.0.0/13",
	"104.24.0.0/14",
	"108.162.192.0/18",
	"131.0.72.0/22",
	"141.101.64.0/18",
	"162.158.0.0/15",
	"172.64.0.0/13",
	"173.245.48.0/20",
	"188.114.96.0/20",
	"190.93.240.0/20",
	"197.234.240.0/22",
	"198.41.128.0/17",
	// IPv6
	"2400:cb00::/32",
	"2405:8100::/32",
	"2405:b500::/32",
	"2606:4700::/32",
	"2803:f800::/32",
	"2a06:98c0::/29",
	"2c0f:f248::/32",
}

var (
	cloudflareClient   *http.Client
	cloudflareClientOn sync.Once
)

// getCloudflareClient retorna o client dedicado ao fetch da API de IPs do
// Cloudflare (proteções SSRF padrão do projeto; o endpoint é fixo e público).
func getCloudflareClient() *http.Client {
	cloudflareClientOn.Do(func() {
		cloudflareClient = SafeHTTPClient(SafeClientOpts{Timeout: cloudflareClientTimeout})
	})
	return cloudflareClient
}

// CloudflareIPs é a lista em memória das faixas de IPs do Cloudflare,
// atualizada pela API (cloudflareIPsURL) no boot e a cada
// cloudflareRefreshInterval. A troca da lista é atômica (troca do fatiamento
// sob lock de escrita), então IsCloudflareIP nunca vê estado parcial.
type CloudflareIPs struct {
	mu   sync.RWMutex
	nets []*net.IPNet
}

// NewCloudflareIPs cria a lista já populada com o fallback hardcoded: se a
// primeira busca na API falhar (ex.: boot sem rede), o serviço continua
// funcionando com a lista local (possivelmente defasada) em vez de barrar
// todo o tráfego.
func NewCloudflareIPs() *CloudflareIPs {
	nets, err := parseCloudflareCIDRs(fallbackCloudflareCIDRs)
	if err != nil {
		// Falha inescapável apenas se o fallback hardcoded estiver corrompido:
		// lista vazia barraria todo tráfego (fail-closed) até a API responder.
		Errorf("cloudflare: fallback hardcoded de IPs inválido: %v", err)
		nets = nil
	}
	return &CloudflareIPs{nets: nets}
}

// IsCloudflareIP indica se o IP pertence a alguma faixa conhecida do
// Cloudflare (lista da API ou fallback).
func (c *CloudflareIPs) IsCloudflareIP(ip net.IP) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, n := range c.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Run mantém a lista atualizada: busca a API imediatamente e depois a cada
// cloudflareRefreshInterval, até o ctx ser cancelado. Falha em qualquer busca
// mantém a última lista válida (no boot, o fallback) e só é logada.
func (c *CloudflareIPs) Run(ctx context.Context) {
	refresh := func() {
		jobCtx, cancel := context.WithTimeout(ctx, cloudflareRefreshTimeout)
		defer cancel()
		if err := c.Refresh(jobCtx); err != nil {
			Errorf("cloudflare: falha ao atualizar a lista de IPs (mantendo a lista atual): %v", err)
			return
		}
		Infof("cloudflare: lista de IPs atualizada via API")
	}

	refresh()

	ticker := time.NewTicker(cloudflareRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}

// Refresh busca a lista de IPs na API do Cloudflare e a aplica atomicamente.
// Qualquer falha (rede, HTTP, JSON, lista vazia, CIDR inválida) retorna erro
// e mantém a lista atual. Em sucesso, compara a lista recebida com o fallback
// hardcoded e avisa o operador sobre divergência.
func (c *CloudflareIPs) Refresh(ctx context.Context) error {
	body, _, err := SafeFetch(ctx, getCloudflareClient(), cloudflareMaxResponseBytes, cloudflareIPsURL)
	if err != nil {
		return fmt.Errorf("fetch da API do Cloudflare: %w", err)
	}
	return c.applyAPIResponse(body)
}

// applyAPIResponse processa o corpo JSON da API do Cloudflare e aplica a lista
// de forma atômica. Qualquer falha (JSON inválido, success=false, lista
// vazia, CIDR inválida) retorna erro e mantém a lista atual. Em sucesso,
// compara a lista recebida com o fallback hardcoded e avisa o operador sobre
// divergência.
func (c *CloudflareIPs) applyAPIResponse(body []byte) error {
	var resp cloudflareIPsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse da resposta da API do Cloudflare: %w", err)
	}
	if !resp.Success {
		return errors.New("API do Cloudflare retornou success=false")
	}

	cidrs := append(append([]string(nil), resp.Result.IPv4CIDRs...), resp.Result.IPv6CIDRs...)
	if len(cidrs) == 0 {
		return errors.New("API do Cloudflare retornou lista de CIDRs vazia")
	}

	nets, err := parseCloudflareCIDRs(cidrs)
	if err != nil {
		return fmt.Errorf("resposta da API do Cloudflare: %w", err)
	}

	c.mu.Lock()
	c.nets = nets
	c.mu.Unlock()

	warnHardcodedDrift(cidrs)
	return nil
}

// cloudflareIPsResponse é o schema de GET /client/v4/ips (campos opcionais
// conforme a documentação oficial da API).
type cloudflareIPsResponse struct {
	Success bool `json:"success"`
	Result  struct {
		IPv4CIDRs []string `json:"ipv4_cidrs"`
		IPv6CIDRs []string `json:"ipv6_cidrs"`
	} `json:"result"`
}

// parseCloudflareCIDRs converte a lista de CIDRs em redes. Qualquer CIDR
// inválida falha a conversão inteira: a lista vem de fonte confiável, entrada
// inválida indica resposta corrompida, e é mais seguro manter a lista
// anterior do que aplicar uma lista parcial.
func parseCloudflareCIDRs(cidrs []string) ([]*net.IPNet, error) {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("CIDR inválida %q: %w", cidr, err)
		}
		nets = append(nets, ipnet)
	}
	return nets, nil
}

// warnHardcodedDrift compara a lista recebida da API com o fallback hardcoded
// e avisa o operador quando divergem: faixas ausentes no fallback deixariam o
// fallback barrar tráfego legítimo; faixas obsoletas no fallback deixariam o
// fallback aceitar IPs que não são mais do Cloudflare.
func warnHardcodedDrift(apiCIDRs []string) {
	apiSet := make(map[string]struct{}, len(apiCIDRs))
	for _, cidr := range apiCIDRs {
		apiSet[cidr] = struct{}{}
	}
	fallbackSet := make(map[string]struct{}, len(fallbackCloudflareCIDRs))
	for _, cidr := range fallbackCloudflareCIDRs {
		fallbackSet[cidr] = struct{}{}
	}

	var missing, stale []string
	for _, cidr := range apiCIDRs {
		if _, ok := fallbackSet[cidr]; !ok {
			missing = append(missing, cidr)
		}
	}
	for _, cidr := range fallbackCloudflareCIDRs {
		if _, ok := apiSet[cidr]; !ok {
			stale = append(stale, cidr)
		}
	}

	if len(missing) > 0 || len(stale) > 0 {
		Warnf("cloudflare: fallback hardcoded de IPs desatualizado (ausentes na lista local: %v; obsoletos na lista local: %v) — atualize fallbackCloudflareCIDRs em utils/cloudflare_ips.go", missing, stale)
	}
}
