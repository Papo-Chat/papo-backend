// Package push test: supervisor do papo-push com um worker de teste (fake).
package push

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"papo/internal/config"
)

// fakeWorkerSource é o fonte do worker de teste: serve /healthz, aceita
// SIGTERM/SIGINT e pode sair após N segundos (FAKE_EXIT_AFTER).
var fakeWorkerSource = `package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	port := os.Getenv("FAKE_PORT")
	if port == "" {
		os.Exit(1)
	}

	if marker := os.Getenv("FAKE_MARKER"); marker != "" {
		f, err := os.OpenFile(marker, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			fmt.Fprintln(f, os.Getpid())
			f.Close()
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	server := &http.Server{Addr: "127.0.0.1:" + port, Handler: mux}
	go server.ListenAndServe()

	if exitAfter := os.Getenv("FAKE_EXIT_AFTER"); exitAfter != "" {
		var n int
		if _, err := fmt.Sscanf(exitAfter, "%d", &n); err == nil {
			time.Sleep(time.Duration(n) * time.Second)
			return
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	<-sigCh
	ctx, cancel := context.WithTimeout(context.Background(), 2 * time.Second)
	defer cancel()
	server.Shutdown(ctx)
}
`

var (
	fakeWorkerPath string
	workerBuilt    bool
	workerBuildMu  sync.Mutex
)

// getFakeWorker compila o worker de teste uma única vez e retorna o caminho.
func getFakeWorker(t *testing.T) string {
	workerBuildMu.Lock()
	defer workerBuildMu.Unlock()
	if workerBuilt {
		return fakeWorkerPath
	}

	dir := filepath.Join(os.TempDir(), "papo_push_test_fakeworker")
	os.MkdirAll(dir, 0755)
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(fakeWorkerSource), 0644); err != nil {
		t.Fatalf("falha ao escrever main.go: %v", err)
	}
	goMod := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(goMod, []byte("module fakeworker\n go 1.21\n"), 0644); err != nil {
		t.Fatalf("falha ao escrever go.mod: %v", err)
	}
	outPath := filepath.Join(dir, "fakeworker")
	cmd := exec.Command("go", "build", "-o", outPath, ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("falha ao compilar o worker de teste: %v: %s", err, out)
	}
	fakeWorkerPath = outPath
	workerBuilt = true
	return outPath
}

// extractPort extrai o porta de uma URL tipo "http://127.0.0.1:12345".
func extractPort(u string) int {
	parsed, err := url.Parse(u)
	if err != nil {
		panic("extractPort: " + err.Error())
	}
	port, _ := strconv.Atoi(parsed.Port())
	return port
}

// newTestManager cria uma Manager com a config e o stopCh necessários.
func newTestManager(cfg *config.Config) *Manager {
	return &Manager{
		ctx:    context.Background(),
		cfg:    cfg,
		stopCh: make(chan struct{}),
	}
}

// TestCheckHealth verifica o polling do healthz (ok, não-ok, e down).
func TestCheckHealth(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("ok"))
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()
		port := extractPort(srv.URL)
		m := newTestManager(&config.Config{PushHealthPort: port})
		ctx := context.Background()
		if err := m.checkHealth(ctx); err != nil {
			t.Fatalf("esperava sem erro, obtive %v", err)
		}
	})
	t.Run("non-ok", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		port := extractPort(srv.URL)
		m := newTestManager(&config.Config{PushHealthPort: port})
		ctx := context.Background()
		if err := m.checkHealth(ctx); err == nil {
			t.Fatalf("esperava erro por status != 200, obtive nenhum")
		}
	})
	t.Run("down", func(t *testing.T) {
		m := newTestManager(&config.Config{PushHealthPort: 59999})
		ctx := context.Background()
		if err := m.checkHealth(ctx); err == nil {
			t.Fatalf("esperava erro por conexão recusada, obtive nenhum")
		}
	})
}

// TestNewCmd verifica que a comanda tem o binário certo e Setpgid.
func TestNewCmd(t *testing.T) {
	cfg := &config.Config{FCMRelayBinary: "/usr/bin/false"}
	m := newTestManager(cfg)
	cmd := m.newCmd()
	if cmd == nil {
		t.Fatal("esperava cmd não nil")
	}
	if cmd.Path != "/usr/bin/false" {
		t.Fatalf("esperava /usr/bin/false, obtive %s", cmd.Path)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("esperava Setpgid=true")
	}
}

// TestWaitHealthy verifica os cenários de readiness.
func TestWaitHealthy(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("ok"))
			}
		}))
		defer srv.Close()
		port := extractPort(srv.URL)
		m := newTestManager(&config.Config{PushHealthPort: port})
		exited := make(chan struct{})
		if !m.waitHealthy(context.Background(), exited) {
			t.Fatalf("esperava true (healthy), obtive false")
		}
	})
	t.Run("exited", func(t *testing.T) {
		m := newTestManager(&config.Config{PushHealthPort: 59998})
		exited := make(chan struct{})
		close(exited)
		if m.waitHealthy(context.Background(), exited) {
			t.Fatalf("esperava false (exited), obtive true")
		}
	})
	t.Run("ctx-done", func(t *testing.T) {
		m := newTestManager(&config.Config{PushHealthPort: 59997})
		exited := make(chan struct{})
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		if m.waitHealthy(ctx, exited) {
			t.Fatalf("esperava false (ctx done), obtive true")
		}
	})
	t.Run("stop", func(t *testing.T) {
		m := newTestManager(&config.Config{PushHealthPort: 59996})
		exited := make(chan struct{})
		close(m.stopCh)
		if m.waitHealthy(context.Background(), exited) {
			t.Fatalf("esperava false (stop), obtive true")
		}
	})
}

// TestRunReadyAndShutdown verifica que o worker fica pronto e que o
// shutdown encerra o worker (SIGTERM).
func TestRunReadyAndShutdown(t *testing.T) {
	port := freePort(t)
	marker := filepath.Join(t.TempDir(), "marker.txt")
	os.Remove(marker)

	worker := getFakeWorker(t)
	os.Setenv("FAKE_PORT", strconv.Itoa(port))
	os.Setenv("FAKE_MARKER", marker)
	os.Setenv("FAKE_EXIT_AFTER", "")

	cfg := &config.Config{
		UseFCMRelay:      true,
		FCMRelayBinary:   worker,
		PushHealthPort:   port,
		FCMRelayRestartMax: 10,
	}
	m := newTestManager(cfg)
	m.Start()

	if !waitFor(t, 5*time.Second, func() bool { return m.Ready() }) {
		m.Stop()
		t.Fatalf("worker não ficou pronto")
	}
	t.Log("worker pronto (ready)")

	m.Stop()
	if !waitFor(t, 5*time.Second, func() bool { return m.State() == StateDead }) {
		t.Fatalf("esperava StateDead após shutdown, obtive %s", m.State())
	}
}

// TestRunRestartBackoff verifica que o worker é reiniciado após morrer.
func TestRunRestartBackoff(t *testing.T) {
	port := freePort(t)
	marker := filepath.Join(t.TempDir(), "marker.txt")
	os.Remove(marker)

	worker := getFakeWorker(t)
	os.Setenv("FAKE_PORT", strconv.Itoa(port))
	os.Setenv("FAKE_MARKER", marker)
	os.Setenv("FAKE_EXIT_AFTER", "1") // worker morre após 1s

	cfg := &config.Config{
		UseFCMRelay:         true,
		FCMRelayBinary:      worker,
		PushHealthPort:      port,
		FCMRelayRestartMax:  10,
	}
	m := newTestManager(cfg)
	m.Start()

	// Espera o worker morrer e o supervisor reiniciar (backoff 1s + 2s).
	if !waitFor(t, 8*time.Second, func() bool {
		return countLines(marker) >= 2
	}) {
		m.Stop()
		t.Fatalf("worker não foi reiniciado (esperava 2+ starts)")
	}
	m.Stop()
}

// TestRunGiveUp verifica que o supervisor desiste após o limite de restart.
func TestRunGiveUp(t *testing.T) {
	port := freePort(t)
	marker := filepath.Join(t.TempDir(), "marker.txt")
	os.Remove(marker)

	worker := getFakeWorker(t)
	os.Setenv("FAKE_PORT", strconv.Itoa(port))
	os.Setenv("FAKE_MARKER", marker)
	os.Setenv("FAKE_EXIT_AFTER", "1") // worker morre após 1s

	cfg := &config.Config{
		UseFCMRelay:        true,
		FCMRelayBinary:     worker,
		PushHealthPort:     port,
		FCMRelayRestartMax: 2,
	}
	m := newTestManager(cfg)
	m.Start()

	// Espera o worker morrer 2 vezes (starts: 1 inicial + 1 reinício = 2).
	if !waitFor(t, 8*time.Second, func() bool {
		return countLines(marker) >= 2
	}) {
		m.Stop()
		t.Fatalf("worker não atingiu o limite de restart")
	}
	// Após o limite, o supervisor desiste: não deve haver mais starts.
	time.Sleep(4 * time.Second) // tempo suficiente para um reinício adicional
	if countLines(marker) > 2 {
		m.Stop()
		t.Fatalf("esperava 2 starts no máximo, obtive %d", countLines(marker))
	}
	m.Stop()
}

// freePort retorna um porta livre para uso em testes.
func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("falha ao encontrar porta livre: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// countLines conta as linhas de um arquivo.
func countLines(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "\n")
}

// waitFor espera até que a condição seja true ou o timeout expire.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}


