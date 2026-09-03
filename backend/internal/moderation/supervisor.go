package moderation

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"papo/internal/config"
	"papo/internal/services"
	"papo/internal/utils"
)

// SupervisorState é o estado do worker Python.
type SupervisorState string

const (
	StateStarting SupervisorState = "starting"
	StateReady    SupervisorState = "ready"
	StateDead     SupervisorState = "dead"
	StateBackoff  SupervisorState = "backoff"
)

// backoffDelays é a espera entre reinícios do worker (crescente, teto 30s).
var backoffDelays = []time.Duration{
	time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
	16 * time.Second, 30 * time.Second,
}

// readyTimeout é o tempo máximo para o worker ficar pronto (carregamento do
// modelo + criação do socket).
const readyTimeout = 120 * time.Second

// terminateGrace é a espera entre o SIGTERM e o SIGKILL do worker.
const terminateGrace = 5 * time.Second

// Supervisor inicia e supervisiona o worker Python (o único processo de
// inferência): se ele morrer, é reiniciado com backoff. O chat nunca depende
// da disponibilidade do worker (os jobs permanecem 'pending' no banco).
type Supervisor struct {
	cfg      *config.Config
	client   *Client
	mu       sync.Mutex
	cmd      *exec.Cmd
	state    SupervisorState
	models   map[string]string
	mediaDir string
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewSupervisor cria o supervisor do worker Python.
func NewSupervisor(cfg *config.Config) *Supervisor {
	// Pasta absoluta onde os blobs de mídia vivem: o worker Python só pode
	// ler arquivos dentro dela (containment de path).
	mediaDir, err := filepath.Abs(services.MediaBaseDir())
	if err != nil {
		mediaDir = services.MediaBaseDir()
	}

	return &Supervisor{
		cfg:      cfg,
		client:   NewClient(cfg.ModerationSocketPath, 5*time.Second),
		mediaDir: mediaDir,
		stopCh:   make(chan struct{}),
	}
}

// SetModels define os caminhos dos modelos passados ao worker.
func (s *Supervisor) SetModels(models map[string]string) {
	s.mu.Lock()
	s.models = models
	s.mu.Unlock()
}

// State retorna o estado atual.
func (s *Supervisor) State() SupervisorState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Ready indica se o worker está de pé e pode processar requisições.
func (s *Supervisor) Ready() bool {
	return s.State() == StateReady
}

// Run é o loop de supervisão: iniciar → aguardar pronto → vigiar → em caso de
// morte, backoff e reinício. Sai quando o ctx ou o stopCh são cancelados
// (encerrando o worker com o processo).
func (s *Supervisor) Run(ctx context.Context) {
	backoff := 0
	for {
		if s.stopped(ctx) {
			return
		}

		startedAt := time.Now()
		s.setState(StateStarting)
		cmd := exec.Command(s.cfg.ModerationWorkerCommand,
			s.cfg.ModerationWorkerPath,
			"--safety-model", s.modelPath(),
			"--socket", s.cfg.ModerationSocketPath,
			"--media-dir", s.mediaDir,
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		// Grupo de processo próprio: o encerramento atinge worker + filhos.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		if err := cmd.Start(); err != nil {
			utils.Errorf("moderação: falha ao iniciar o worker Python (%s %s): %v",
				s.cfg.ModerationWorkerCommand, s.cfg.ModerationWorkerPath, err)
		} else {
			s.setCmd(cmd)

			if s.waitReady(ctx, cmd) {
				s.setState(StateReady)
				utils.Info("moderação: worker Python pronto")
			}

			waitDone := make(chan error, 1)
			go func() {
				waitDone <- cmd.Wait()
			}()

			select {
			case err := <-waitDone:
				s.setCmd(nil)
				s.setState(StateDead)
				utils.Warnf("moderação: worker Python saiu: %v", err)
				// Execução estável (mais de 1 minuto) zera o backoff: o
				// crash não é um loop de falha.
				if time.Since(startedAt) > time.Minute {
					backoff = 0
				}
			case <-ctx.Done():
				s.terminate(cmd)
				s.setCmd(nil)
				return
			case <-s.stopCh:
				s.terminate(cmd)
				s.setCmd(nil)
				return
			}
		}

		if s.stopped(ctx) {
			return
		}

		delay := backoffDelays[backoff]
		if backoff < len(backoffDelays)-1 {
			backoff++
		}
		s.setState(StateBackoff)
		utils.Infof("moderação: reiniciando o worker Python em %s", delay)
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-time.After(delay):
		}
	}
}

// modelPath retorna o caminho do modelo primário (Fase 1: um único modelo).
func (s *Supervisor) modelPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.models["safety-xs-v1"]
}

func (s *Supervisor) setState(state SupervisorState) {
	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
}

func (s *Supervisor) setCmd(cmd *exec.Cmd) {
	s.mu.Lock()
	s.cmd = cmd
	s.mu.Unlock()
}

func (s *Supervisor) stopped(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-s.stopCh:
		return true
	default:
		return false
	}
}

// waitReady consulta o health do worker até ele responder ok, o processo
// morrer ou o readyTimeout expirar.
func (s *Supervisor) waitReady(ctx context.Context, cmd *exec.Cmd) bool {
	deadline := time.Now().Add(readyTimeout)
	for {
		if err := s.client.Health(ctx); err == nil {
			return true
		}
		if s.processExited(cmd) {
			return false
		}
		if time.Now().After(deadline) {
			utils.Warnf("moderação: worker Python não ficou pronto em %s", readyTimeout)
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-s.stopCh:
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *Supervisor) processExited(cmd *exec.Cmd) bool {
	return cmd.ProcessState != nil
}

// terminate encerra o worker (SIGTERM ao grupo; SIGKILL após a graça).
func (s *Supervisor) terminate(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		return // já encerrado
	}
	select {
	case <-time.After(terminateGrace):
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// Stop encerra o loop de supervisão (idempotente). O encerramento do próprio
// worker Python acontece no Run (terminate) quando o stopCh é fechado.
func (s *Supervisor) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
}
