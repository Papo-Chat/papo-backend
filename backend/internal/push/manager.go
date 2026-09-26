// Package push supervisiona o processo papo-push (o único responsável pela
// entrega de push). O backend não envia push; ele apenas inicia, monitora e
// encerra o worker via os/exec, com health/liveness, restart com backoff e
// graceful shutdown. Modelado a partir de internal/moderation.
package push

import (
	"context"
	"os/exec"
	"sync"
	"time"

	"papo/internal/config"
	"papo/internal/utils"
)

type State string

const (
	StateStarting State = "starting"
	StateReady    State = "ready"
	StateDead     State = "dead"
	StateBackoff  State = "backoff"
)

// restartBackoff é a espera entre reinícios do worker (crescente, teto 30s).
var restartBackoff = []time.Duration{
	time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second,
}

// stableRunThreshold: o worker que ficou pronto por mais que isso zera o
// contador de restart (crash não é um loop de falha).
const stableRunThreshold = time.Minute

var (
	instance     *Manager
	instanceOnce sync.Once
)

// Manager inicia e supervisiona o papo-push.
type Manager struct {
	cfg      *config.Config
	ctx      context.Context
	mu       sync.Mutex
	cmd      *exec.Cmd
	state    State
	stopCh   chan struct{}
	stopOnce sync.Once
	lastStart time.Time
}

// Init cria e inicia o supervisor do papo-push. No-op quando
// USE_FCM_RELAY=false (push desabilitado). Nunca falha: a degradação é
// logada (o chat não pode cair por causa do push).
func Init(cfg *config.Config, ctx context.Context) {
	instanceOnce.Do(func() {
		if !cfg.UseFCMRelay {
			utils.Info("push desativado (USE_FCM_RELAY=false): nenhum papo-push iniciado")
			return
		}
		instance = &Manager{
			cfg:    cfg,
			ctx:    ctx,
			stopCh: make(chan struct{}),
		}
		instance.Start()
		utils.Info("supervisor do papo-push iniciado")
	})
}

// Shutdown encerra o supervisor (idempotente). O encerramento do próprio
// worker acontece no Run (terminate) quando o stopCh é fechado.
func Shutdown() {
	if instance != nil {
		instance.Stop()
	}
}

// State retorna o estado atual do worker.
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Ready indica se o worker está de pé e saudável.
func (m *Manager) Ready() bool {
	return m.State() == StateReady
}

// Start inicia o loop de supervisão em uma goroutine.
func (m *Manager) Start() {
	go m.Run(m.ctx)
}

// Stop encerra o loop de supervisão (idempotente).
func (m *Manager) Stop() {
	m.stopOnce.Do(func() {
		close(m.stopCh)
	})
}

// Run é o loop de supervisão: garantir o worker → iniciar → aguardar pronto →
// vigiar → em caso de morte/falha, backoff e reinício. Sai quando o ctx ou o
// stopCh são cancelados (encerrando o worker com o processo). Se o limite de
// restart (FCM_RELAY_RESTART_MAX) for excedido, desiste: o backend continua
// rodando, o chat funciona, o push fica indisponível e o erro fica claro no log.
func (m *Manager) Run(ctx context.Context) {
	restarts := 0
	for {
		if m.stopped(ctx) {
			return
		}

		if restarts >= m.cfg.FCMRelayRestartMax {
			utils.Errorf("push: limite de reinícios (%d) atingido; papo-push não será reiniciado; push indisponível até a reinicialização do backend",
				m.cfg.FCMRelayRestartMax)
			return
		}

		m.setState(StateStarting)
		m.lastStart = time.Now()
		cmd := m.newCmd()
		if cmd == nil {
			return
		}

		if err := cmd.Start(); err != nil {
			utils.Errorf("push: falha ao iniciar o papo-push (%s): %v", m.cfg.FCMRelayBinary, err)
			m.setCmd(nil)
			m.setState(StateDead)
			restarts++
			if !m.backoffWait(ctx, restarts) {
				return
			}
			continue
		}

		m.setCmd(cmd)

		// Wait() imediatamente após o Start(): a saída do processo fica
		// observável durante a readiness (um worker que morre no startup
		// é detectado na hora, e não só após o timeout).
		exited := make(chan struct{})
		exitErr := make(chan error, 1)
		go func() {
			exitErr <- cmd.Wait()
			close(exited)
		}()

		if m.waitHealthy(ctx, exited) {
			m.setState(StateReady)
			utils.Infof("push: papo-push pronto (health em 127.0.0.1:%d)", m.cfg.PushHealthPort)

			select {
			case <-exited:
				// Worker morreu em execução.
			case <-ctx.Done():
				m.terminate(cmd, exited)
				m.setCmd(nil)
				m.setState(StateDead)
				return
			case <-m.stopCh:
				m.terminate(cmd, exited)
				m.setCmd(nil)
				m.setState(StateDead)
				return
			}

			m.setCmd(nil)
			m.setState(StateDead)
			utils.Warnf("push: papo-push saiu: %v", <-exitErr)
			// Execução estável (mais de 1 minuto) zera o contador de restart;
			// crash em execução curta conta como restart (evita loop de falha).
			if time.Since(m.lastStart) > stableRunThreshold {
				restarts = 0
			} else {
				restarts++
			}
		} else {
			// Worker não ficou pronto (morreu, readiness expirou ou shutdown
			// em curso): encerra o que ainda estiver vivo.
			m.terminate(cmd, exited)
			m.setCmd(nil)
			m.setState(StateDead)
			utils.Warnf("push: papo-push não ficou pronto: %v", <-exitErr)
			if time.Since(m.lastStart) > stableRunThreshold {
				restarts = 0
			} else {
				restarts++
			}
			if m.stopped(ctx) {
				return
			}
		}

		if m.stopped(ctx) {
			return
		}

		if !m.backoffWait(ctx, restarts) {
			return
		}
	}
}

// backoffWait espera o delay de backoff (incrementando até o teto) e retorna
// false quando o shutdown foi solicitado durante a espera.
func (m *Manager) backoffWait(ctx context.Context, restarts int) bool {
	delay := restartBackoff[0]
	if restarts > 0 {
		i := restarts
		if i >= len(restartBackoff) {
			i = len(restartBackoff) - 1
		}
		delay = restartBackoff[i]
	}
	m.setState(StateBackoff)
	utils.Infof("push: reiniciando o papo-push em %s (tentativa %d/%d)", delay, restarts, m.cfg.FCMRelayRestartMax)
	select {
	case <-ctx.Done():
		return false
	case <-m.stopCh:
		return false
	case <-time.After(delay):
		return true
	}
}

func (m *Manager) stopped(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-m.stopCh:
		return true
	default:
		return false
	}
}

func (m *Manager) setState(state State) {
	m.mu.Lock()
	m.state = state
	m.mu.Unlock()
}

func (m *Manager) setCmd(cmd *exec.Cmd) {
	m.mu.Lock()
	m.cmd = cmd
	m.mu.Unlock()
}
