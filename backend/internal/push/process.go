package push

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"

	"papo/internal/utils"
)

// healthInterval é a cadência do polling de liveness (healthz).
const healthInterval = 2 * time.Second

// healthTimeout é o tempo máximo para o worker ficar pronto.
const healthTimeout = 30 * time.Second

// terminateGrace é a espera entre o SIGTERM e o SIGKILL do worker.
const terminateGrace = 5 * time.Second

// healthClient é o cliente HTTP para o healthz local (timeout curto).
var healthClient = &http.Client{
	Timeout: 1 * time.Second,
}

// newCmd monta a comanda do papo-push: Setpgid (encerramento em grupo),
// Stdout/Stderr herdados (logs estruturados do worker no stdout do backend).
func (m *Manager) newCmd() *exec.Cmd {
	cmd := exec.Command(m.cfg.FCMRelayBinary)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Grupo de processo próprio: o encerramento atinge o worker + filhos.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// checkHealth consulta o healthz local do worker.
func (m *Manager) checkHealth(ctx context.Context) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", m.cfg.PushHealthPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := healthClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health status %d", resp.StatusCode)
	}
	return nil
}

// waitHealthy consulta o healthz do worker até ele responder ok, o processo
// sair (exited), o ctx/stopCh ser cancelado ou o healthTimeout expirar.
func (m *Manager) waitHealthy(ctx context.Context, exited <-chan struct{}) bool {
	deadline := time.Now().Add(healthTimeout)
	for {
		if err := m.checkHealth(ctx); err == nil {
			return true
		}
		select {
		case <-exited:
			return false
		case <-ctx.Done():
			return false
		case <-m.stopCh:
			return false
		case <-time.After(healthInterval):
		}
		if time.Now().After(deadline) {
			utils.Warnf("push: papo-push não ficou pronto em %s", healthTimeout)
			return false
		}
	}
}

// terminate encerra o worker (SIGTERM ao grupo; SIGKILL se a saída não
// acontecer dentro da graça). Se o processo já saiu, retorna na hora.
func (m *Manager) terminate(cmd *exec.Cmd, exited <-chan struct{}) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-exited:
		return
	case <-time.After(terminateGrace):
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
