package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// User é um usuário do stress com seu próprio cookie JWT.
type User struct {
	ID       string
	Username string
	Password string

	mu     sync.Mutex
	cookie string
}

func (u *User) Cookie() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cookie
}

func (u *User) SetCookie(v string) {
	u.mu.Lock()
	u.cookie = v
	u.mu.Unlock()
}

// resp é o resultado de uma requisição HTTP.
type resp struct {
	status int
	body   []byte
	dur    time.Duration
	err    error
}

func (r resp) ok() bool { return r.err == nil && r.status >= 200 && r.status < 300 }

func (r resp) problem() string {
	if r.err != nil {
		return r.err.Error()
	}
	var p struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(r.body, &p); err == nil && (p.Title != "" || p.Detail != "") {
		return strings.TrimSpace(p.Title + " | " + p.Detail)
	}
	s := string(r.body)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// Client faz HTTP com tratamento manual de cookie: o backend define o cookie
// Auth com Secure=true, e um cookie jar sobre HTTP puro descartaria esse
// cookie. O cookie é extraído do Set-Cookie e enviado manualmente.
type Client struct {
	base string
	http *http.Client

	seedOps atomic.Int64
	seedErr atomic.Int64
	seedRL  atomic.Int64
}

func NewClient(base string) *Client {
	tr := &http.Transport{
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 512,
		MaxConnsPerHost:     512,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		base: strings.TrimSuffix(base, "/"),
		http: &http.Client{Transport: tr, Timeout: 120 * time.Second},
	}
}

// do executa a requisição. Se u != nil, envia o cookie do usuário e captura
// Set-Cookie: Auth=... (rotacionado no login/refresh). 429 é repetido com
// backoff (até 4 repetições) para o seeding não travar nos rate limits.
func (c *Client) do(u *User, method, path string, hdr map[string]string, body []byte) resp {
	var last resp
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			// rand global é seguro para concorrência (Go 1.20+).
			time.Sleep(time.Duration(100+rand.Intn(500)) * time.Millisecond)
		}
		var rd io.Reader
		if len(body) > 0 {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, c.base+path, rd)
		if err != nil {
			return resp{err: err}
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		if u != nil {
			if ck := u.Cookie(); ck != "" {
				req.Header.Set("Cookie", "Auth="+ck)
			}
		}
		start := time.Now()
		httpResp, err := c.http.Do(req)
		if err != nil {
			last = resp{err: err, dur: time.Since(start)}
			break
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, io.LimitReader(httpResp.Body, 32<<20)); err != nil {
			httpResp.Body.Close()
			last = resp{err: err, dur: time.Since(start)}
			break
		}
		httpResp.Body.Close()
		last = resp{status: httpResp.StatusCode, body: buf.Bytes(), dur: time.Since(start)}
		if u != nil {
			for _, sc := range httpResp.Header.Values("Set-Cookie") {
				if !strings.HasPrefix(sc, "Auth=") {
					continue
				}
				val := strings.TrimPrefix(sc, "Auth=")
				if i := strings.IndexByte(val, ';'); i >= 0 {
					val = val[:i]
				}
				u.SetCookie(val)
				break
			}
		}
		if last.status == http.StatusTooManyRequests && attempt < 4 {
			continue
		}
		break
	}
	return last
}

// seedOp conta requisições de seeding (ficam fora das estatísticas de fase).
func (c *Client) seedOp(r resp) {
	c.seedOps.Add(1)
	if !r.ok() {
		c.seedErr.Add(1)
	}
	if r.status == http.StatusTooManyRequests {
		c.seedRL.Add(1)
	}
}

func (c *Client) SeedTotals() (ops, errs, rl int64) {
	return c.seedOps.Load(), c.seedErr.Load(), c.seedRL.Load()
}

// EpStats acumula amostras de latência e erros de um endpoint.
type EpStats struct {
	Name      string
	N         int64
	Err       int64
	RL        int64
	ErrStatus map[int]int
	Dur       []time.Duration
}

// PhaseStats é o resultado de uma passada de medição.
type PhaseStats struct {
	Index int
	Label string
	Start time.Time
	End   time.Time
	eps   map[string]*EpStats
}

func (p *PhaseStats) Ep(name string) *EpStats {
	return p.eps[name]
}

// Stats coleta as fases de medição.
type Stats struct {
	mu     sync.Mutex
	phases []*PhaseStats
	cur    *PhaseStats
}

func NewStats() *Stats { return &Stats{} }

func (s *Stats) BeginPhase(i int, label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = &PhaseStats{Index: i, Label: label, Start: time.Now(), eps: map[string]*EpStats{}}
	s.phases = append(s.phases, s.cur)
}

func (s *Stats) EndPhase() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil {
		s.cur.End = time.Now()
	}
	s.cur = nil
}

// Record registra uma requisição medida.
func (s *Stats) Record(ep string, d time.Duration, status int) {
	s.mu.Lock()
	ph := s.cur
	if ph == nil {
		s.mu.Unlock()
		return
	}
	e := ph.eps[ep]
	if e == nil {
		e = &EpStats{Name: ep, ErrStatus: map[int]int{}}
		ph.eps[ep] = e
	}
	e.N++
	e.Dur = append(e.Dur, d)
	if status >= 400 {
		e.Err++
		e.ErrStatus[status]++
	}
	if status == http.StatusTooManyRequests {
		e.RL++
	}
	s.mu.Unlock()
}

func (s *Stats) Phases() []*PhaseStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*PhaseStats, len(s.phases))
	copy(out, s.phases)
	return out
}

// Percentile calcula o percentil (0..1) de uma amostra de durações.
func Percentile(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := make([]time.Duration, len(d))
	copy(s, d)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(p*float64(len(s)-1) + 0.5)
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

func jsonBody(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func b64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

func jsonPtr(b []byte, dst any) bool {
	return json.Unmarshal(b, dst) == nil
}
