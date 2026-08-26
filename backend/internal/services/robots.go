package services

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"papo/internal/config"
	"papo/internal/utils"
)

// robotsEntry é a entrada do cache de robots.txt por origem.
// allowedAll = true quando o robots.txt não existe (404): sem restrição.
// rules = nil e allowedAll = false quando o fetch/parse falhou (fail-closed).
type robotsEntry struct {
	allowedAll bool
	rules      *robotsRules
	fetchedAt  time.Time
}

var (
	robotsCacheMu  sync.RWMutex
	robotsCache    = make(map[string]robotsEntry)
	robotsClient   *http.Client
	robotsClientOn sync.Once
)

// getRobotsClient retorna o client SSRF-safe dedicado ao fetch de robots.txt
// (timeout 2s, §6.4).
func getRobotsClient() *http.Client {
	robotsClientOn.Do(func() {
		robotsClient = utils.SafeHTTPClient(utils.SafeClientOpts{Timeout: 2 * time.Second})
	})
	return robotsClient
}

// RobotsAllowed verifica o robots.txt da origem da URL alvo para o nosso
// User-Agent (PapoBot). O robots.txt é cacheado por origem (TTL
// ROBOTS_CACHE_TTL, default 1h; perda no restart é aceitável).
//
// Decisão (§6.4):
//   - 200 + parse OK → regras decidem (Allow/Disallow por path);
//   - 404 (não existe) → permitido (sem restrição);
//   - 401/403/5xx / timeout / parse inválido / excedeu 512KB → negado
//     (fail-closed).
//
// ROBOTS_ENABLED=false desativa o check (sempre permitido).
func RobotsAllowed(ctx context.Context, target *url.URL) bool {
	cfg := config.LoadConfig()
	if !cfg.RobotsEnabled {
		return true
	}

	origin := utils.OriginURL(target)

	robotsCacheMu.RLock()
	entry, ok := robotsCache[origin]
	robotsCacheMu.RUnlock()
	if ok && time.Since(entry.fetchedAt) < cfg.RobotsCacheTTL {
		if entry.allowedAll {
			return true
		}
		if entry.rules != nil {
			return robotsAllowed(entry.rules, utils.SafeClientUserAgent, robotsTargetPath(target))
		}
		return false
	}

	entry = fetchRobotsEntry(ctx, origin)

	robotsCacheMu.Lock()
	robotsCache[origin] = entry
	robotsCacheMu.Unlock()

	if entry.allowedAll {
		return true
	}
	if entry.rules != nil {
		return robotsAllowed(entry.rules, utils.SafeClientUserAgent, robotsTargetPath(target))
	}
	return false
}

// fetchRobotsEntry busca e parseia o robots.txt da origem (teto 512KB).
// Qualquer falha → entry fail-closed (rules nil, allowedAll false).
func fetchRobotsEntry(ctx context.Context, origin string) robotsEntry {
	const maxRobotsBytes = 512 << 10

	robotsURL := strings.TrimSuffix(origin, "/") + "/robots.txt"

	body, _, err := utils.SafeFetch(ctx, getRobotsClient(), maxRobotsBytes, robotsURL)
	if err != nil {
		var statusErr *utils.HTTPStatusError
		if errors.As(err, &statusErr) && statusErr.Status == http.StatusNotFound {
			return robotsEntry{allowedAll: true, fetchedAt: time.Now()}
		}
		// 401/403/5xx, timeout, SSRF, body > 512KB → fail-closed
		return robotsEntry{fetchedAt: time.Now()}
	}

	return robotsEntry{rules: parseRobots(body), fetchedAt: time.Now()}
}

// robotsTargetPath monta o alvo do matching de robots (path + query, sem
// fragment; path vazio → "/").
func robotsTargetPath(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p
}

// robotsGroup é um grupo de regras do robots.txt (um ou mais User-agent +
// Allow/Disallow). hasRule indica se o grupo já recebeu qualquer diretiva
// (qualquer linha "chave: valor" que não seja User-agent, ex.: Crawl-delay):
// a partir daí, um novo User-agent inicia um grupo novo (não é mais
// "consecutivo").
type robotsGroup struct {
	agents   []string
	allow    []string
	disallow []string
	hasRule  bool
}

// robotsRules é o robots.txt parseado (lista de grupos).
type robotsRules struct {
	groups []robotsGroup
}

// parseRobots parseia o robots.txt em grupos de regras. Linhas antes de um
// User-agent são ignoradas; linhas de User-agent consecutivas formam o mesmo
// grupo; campos desconhecidos (Crawl-delay etc.) são ignorados.
func parseRobots(body []byte) *robotsRules {
	var rules robotsRules
	var current *robotsGroup

	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if i := strings.Index(value, "#"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}

		switch key {
		case "user-agent":
			if value == "" {
				continue
			}
			if current != nil && !current.hasRule {
				// User-agent consecutivo (sem diretiva entre) → mesmo grupo
				current.agents = append(current.agents, value)
				continue
			}
			rules.groups = append(rules.groups, robotsGroup{agents: []string{value}})
			current = &rules.groups[len(rules.groups)-1]
		case "allow":
			if current != nil {
				current.allow = append(current.allow, value)
				current.hasRule = true
			}
		case "disallow":
			if current != nil {
				current.disallow = append(current.disallow, value)
				current.hasRule = true
			}
		default:
			// Diretiva não rastreada (Crawl-delay, Sitemap, ...): fecha o
			// bloco de User-agent do grupo atual (separador de grupo).
			if current != nil {
				current.hasRule = true
			}
		}
	}

	return &rules
}

// robotsAllowed decide se o UA pode acessar o caminho (RFC 9309, MVP):
//   - grupo aplicável = o com o token de User-agent mais específico que casa
//     (case-insensitive substring; "*" casa tudo);
//   - sem grupo aplicável → permitido;
//   - regra mais específica (prefixo mais longo) vence; empate → Allow.
func robotsAllowed(rules *robotsRules, ua, target string) bool {
	uaLower := strings.ToLower(ua)

	var group *robotsGroup
	bestToken := 0
	for i := range rules.groups {
		for _, token := range rules.groups[i].agents {
			tok := strings.ToLower(strings.TrimSpace(token))
			if tok == "" {
				continue
			}
			if (tok == "*" || strings.Contains(uaLower, tok)) && len(tok) > bestToken {
				bestToken = len(tok)
				group = &rules.groups[i]
			}
		}
	}
	if group == nil {
		return true
	}

	allowLen, disallowLen := -1, -1
	for _, p := range group.allow {
		if robotsRuleMatch(p, target) && len(p) > allowLen {
			allowLen = len(p)
		}
	}
	for _, p := range group.disallow {
		if robotsRuleMatch(p, target) && len(p) > disallowLen {
			disallowLen = len(p)
		}
	}
	if allowLen == -1 && disallowLen == -1 {
		return true
	}
	return allowLen >= disallowLen
}

// robotsRuleMatch verifica se a regra de robots.txt casa com o caminho alvo.
// Padrão: prefixo (case-sensitive). Suporta '*' (qualquer sequência) e '$'
// no fim da regra (fim do caminho).
func robotsRuleMatch(rulePath, target string) bool {
	if rulePath == "" {
		return false // Disallow: (vazio) → permite tudo
	}
	if !strings.ContainsAny(rulePath, "*$") {
		return strings.HasPrefix(target, rulePath)
	}

	anchored := strings.HasSuffix(rulePath, "$")
	body := strings.TrimSuffix(rulePath, "$")
	var re strings.Builder
	re.WriteString("^")
	for _, ch := range body {
		switch ch {
		case '*':
			re.WriteString(".*")
		default:
			re.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	if anchored {
		re.WriteString("$")
	}
	matched, err := regexp.MatchString(re.String(), target)
	return err == nil && matched
}
