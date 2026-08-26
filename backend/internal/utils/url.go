package utils

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrInvalidURL indica que a URL é inválida para uso como chave de cache,
// origem de robots ou alvo de fetch outbound.
var ErrInvalidURL = errors.New("URL inválida")

// NormalizeURL parseia e normaliza uma URL http/https para uso como chave de
// cache, origem (robots) e matching de host. Retorna erro para URL inválida,
// scheme != http/https, porta não padrão ou userinfo presente.
//
// Normalização: scheme e host em lowercase; porta padrão (:443 em https, :80
// em http) removida; fragment removido. Porta não padrão e userinfo são
// rejeitados (a fronteira SSRF é host/porta, §ssrf). Path e query são
// preservados como parseados pelo Go (sem canonização RFC 3986 completa: o
// pior caso é 2 entradas de cache para a mesma página, sem impacto de
// segurança).
func NormalizeURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, ErrInvalidURL
	}
	if u.User != nil {
		return nil, ErrInvalidURL
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, ErrInvalidURL
	}

	host := strings.ToLower(u.Hostname())
	if host == "" {
		return nil, ErrInvalidURL
	}

	port := u.Port()
	if port != "" {
		if (scheme == "https" && port != "443") || (scheme == "http" && port != "80") {
			return nil, ErrInvalidURL
		}
		// porta padrão do scheme: remove (mesma origem, mesma chave de cache)
	}

	u.Scheme = scheme
	u.Host = host
	u.User = nil
	u.Fragment = ""
	return u, nil
}

// OriginURL retorna a origem canônica (scheme://host[:porta]) da URL
// normalizada, para uso como chave do cache de robots.
func OriginURL(u *url.URL) string {
	return u.Scheme + "://" + u.Host
}
