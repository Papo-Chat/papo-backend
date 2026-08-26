package utils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// ErrSSRFBlocked indica que a URL/IP foi bloqueada pelas proteções de SSRF
// (IP privado/reservado, redirect para alvo inválido, excesso de redirects).
var ErrSSRFBlocked = errors.New("alvo bloqueado por proteção SSRF")

// ErrBodyTooLarge indica que o corpo da resposta excedeu o limite
// (pós-descompressão; fecha gzip bomb).
var ErrBodyTooLarge = errors.New("corpo da resposta excede o limite")

// HTTPStatusError indica que a resposta HTTP teve um status fora de 2xx.
// O chamador pode inspecionar Status (ex.: robots.txt trata 404 como
// "sem restrição").
type HTTPStatusError struct {
	Status int
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("status HTTP %d", e.Status)
}

// SafeClientUserAgent é o User-Agent usado em todos os fetches outbound
// (HTML, imagem, robots.txt e endpoints oEmbed) e no matching de robots.txt.
const SafeClientUserAgent = "PapoBot/1.0 (+link preview)"

// Redes privadas/reservadas bloqueadas no dial (IPv4).
var blockedIPv4CIDRs = parseCIDRs(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "255.255.255.255/32",
)

// Redes privadas/reservadas bloqueadas no dial (IPv6). ::ffff:0:0/96
// (IPv4-mapped) é tratado como IPv4 via ip.To4(); 64:ff9b::/96 (NAT64) tem
// as regras IPv4 aplicadas ao sufixo.
var blockedIPv6CIDRs = parseCIDRs(
	"::/128", "::1/128", "100::/64", "2001:db8::/32",
	"fc00::/7", "fe80::/10", "ff00::/8",
)

var nat64CIDR = parseOneCIDR("64:ff9b::/96")

func parseCIDRs(cidrs ...string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, ipnet, err := net.ParseCIDR(c); err == nil {
			nets = append(nets, ipnet)
		}
	}
	return nets
}

func parseOneCIDR(cidr string) *net.IPNet {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		panic("CIDR inválida: " + cidr)
	}
	return ipnet
}

func inNAT64CIDR(ip6 net.IP) bool {
	return nat64CIDR.Contains(ip6)
}

// SafeClientOpts configura o SafeHTTPClient.
type SafeClientOpts struct {
	// Timeout é o tempo máximo total da requisição (padrão 8s).
	Timeout time.Duration
}

// safeDialControl valida o IP efetivamente resolvido no momento do connect
// (fecha DNS rebinding: o hostname pode ser público no check inicial e
// privado no dial). IPv4-mapped IPv6 (::ffff:a.b.c.d) é tratado como IPv4;
// NAT64 (64:ff9b::/96) tem as regras IPv4 aplicadas ao sufixo.
func safeDialControl(network, address string, _ syscall.RawConn) error {
	if network != "tcp4" && network != "tcp6" && network != "tcp" {
		return fmt.Errorf("%w: rede não suportada %s", ErrSSRFBlocked, network)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: endereço inválido", ErrSSRFBlocked)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("%w: endereço não é um IP", ErrSSRFBlocked)
	}
	if err := checkSSRFSafeIP(ip); err != nil {
		return err
	}
	return nil
}

func checkSSRFSafeIP(ip net.IP) error {
	if v4 := ip.To4(); v4 != nil {
		for _, cidr := range blockedIPv4CIDRs {
			if cidr.Contains(v4) {
				return fmt.Errorf("%w: IP privado/reservado", ErrSSRFBlocked)
			}
		}
		return nil
	}

	ip6 := ip.To16()
	if ip6 == nil {
		return fmt.Errorf("%w: IP inválido", ErrSSRFBlocked)
	}
	// NAT64: aplica as regras IPv4 ao sufixo de 32 bits.
	if inNAT64CIDR(ip6) {
		return checkSSRFSafeIP(net.IPv4(ip6[12], ip6[13], ip6[14], ip6[15]))
	}
	for _, cidr := range blockedIPv6CIDRs {
		if cidr.Contains(ip6) {
			return fmt.Errorf("%w: IP privado/reservado", ErrSSRFBlocked)
		}
	}
	return nil
}

// SafeHTTPClient retorna *http.Client com todas as proteções de SSRF:
// apenas http/https sem userinfo, porta 80/443, validação de IP no dial
// (anti DNS rebinding), máx. 5 redirects revalidados por hop, timeout
// total, sem cookie jar, sem header de autenticação e User-Agent fixo.
// Todo URL processado passa por NormalizeURL antes da rede.
func SafeHTTPClient(opts SafeClientOpts) *http.Client {
	if opts.Timeout <= 0 {
		opts.Timeout = 8 * time.Second
	}

	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   safeDialControl,
	}

	return &http.Client{
		Timeout: opts.Timeout,
		Transport: &http.Transport{
			Proxy:                 nil, // nunca usar proxy (bypass de política)
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          8,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			// TLS: verificação de certificado padrão (nunca InsecureSkipVerify).
			// Sem cookie jar e sem headers de autenticação.
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("%w: máximo de 5 redirects excedido", ErrSSRFBlocked)
			}
			// Revalida cada Location (scheme/porta/userinfo); a validação de IP
			// do dialer cobre o rebinding por hop.
			if _, err := NormalizeURL(req.URL.String()); err != nil {
				return fmt.Errorf("%w: redirect para URL inválida", ErrSSRFBlocked)
			}
			return nil
		},
	}
}

// SafeFetch executa um GET com as proteções SSRF e de limite de corpo
// (máx. maxBodyBytes pós-descompressão) e retorna o corpo e a URL final
// pós-redirects (normalizada, para chave de cache e resolução de URLs
// relativas). O ctx controla o timeout; o client já tem timeout próprio.
func SafeFetch(ctx context.Context, client *http.Client, maxBodyBytes int64, rawURL string) ([]byte, *url.URL, error) {
	if maxBodyBytes <= 0 {
		maxBodyBytes = 1 << 20
	}

	u, err := NormalizeURL(rawURL)
	if err != nil {
		return nil, nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("falha ao criar requisição: %w", err)
	}
	req.Header.Set("User-Agent", SafeClientUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, &HTTPStatusError{Status: resp.StatusCode}
	}

	body, err := readLimitedBody(resp.Body, maxBodyBytes)
	if err != nil {
		return nil, nil, err
	}

	final, err := NormalizeURL(resp.Request.URL.String())
	if err != nil {
		return nil, nil, fmt.Errorf("%w: URL final inválida", ErrSSRFBlocked)
	}
	return body, final, nil
}

// readLimitedBody lê o corpo com teto pós-descompressão (o client do Go
// descomprime gzip sozinho; o teto conta os bytes descomprimidos → fecha
// gzip bomb). Retorna ErrBodyTooLarge quando o limite é excedido; um corpo
// com exatamente o limite é aceito.
func readLimitedBody(r io.Reader, limit int64) ([]byte, error) {
	buf := make([]byte, 0, min(limit, 1<<20))
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if int64(len(buf)) > limit {
			return nil, ErrBodyTooLarge
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buf, nil
			}
			return nil, fmt.Errorf("falha ao ler o corpo da resposta: %w", err)
		}
	}
}
