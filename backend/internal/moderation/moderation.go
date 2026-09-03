// Package moderation implementa a moderação assíncrona de imagens (nudez e
// gore) dos attachments. O processo Go mantém uma fila limitada em memória e
// um reconciler; a inferência roda em um worker Python persistente
// supervisionado por este processo (socket Unix, protocolo NDJSON) e nunca
// bloqueia o envio de mensagem. O resultado (clean/sensitive/blocked) é
// persistido no attachment (attachments.moderation_status) e notificado ao
// cliente via WebSocket.
package moderation

import (
	"context"
	"os"
	"sync"
	"time"

	"papo/internal/config"
	"papo/internal/utils"
)

// Status é o estado de moderação de um attachment (coluna
// attachments.moderation_status).
type Status string

const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusClean      Status = "clean"
	StatusSensitive  Status = "sensitive"
	StatusBlocked    Status = "blocked"
	StatusFailed     Status = "failed"
)

// maxModerationAttempts é o número de tentativas de inferência antes do
// attachment ficar 'failed' (requer intervenção manual).
const maxModerationAttempts = 3

// reconcileInterval é a cadência do reconciler (recoloca pendentes na fila,
// incluindo 'processing' órfãos de um crash).
const reconcileInterval = 30 * time.Second

// staleProcessing é a idade que um 'processing' precisa ter para ser
// considerado órfão (crash do processo em pleno processamento).
const staleProcessing = 5 * time.Minute

// reconcileBatchSize é o número máximo de pendentes por passada do reconciler.
const reconcileBatchSize = 100

// Result é o resultado da inferência (probabilidades por classe).
type Result struct {
	SFW    float64
	Nudity float64
	Gore   float64
	Model  string
}

// Classifier faz a inferência de uma imagem no disco. Implementado pelo
// Client (worker Python); é uma interface para os testes alimentarem o fluxo
// com resultados sintéticos, sem worker real nem imagem sensível.
type Classifier interface {
	Classify(ctx context.Context, path, mime string) (Result, error)
}

// Service orquestra a fila, os workers, o supervisor do worker Python e o
// reconciler.
type Service struct {
	cfg         *config.Config
	ctx         context.Context
	queue       chan Job
	inflight    map[string]struct{}
	mu          sync.Mutex
	policy      Policy
	client      Classifier
	sup         *Supervisor
	concurrency int
	stopCh      chan struct{}
	stopOnce    sync.Once
	wg          sync.WaitGroup
}

var (
	instance     *Service
	instanceOnce sync.Once
)

// Init cria e inicia o serviço de moderação (no-op quando
// MODERATION_ENABLED=false). Nunca falha: a degradação é logada (o chat não
// pode cair por causa da moderação).
func Init(cfg *config.Config, ctx context.Context) {
	instanceOnce.Do(func() {
		if !cfg.ModerationEnabled {
			utils.Info("moderação de imagens desativada (MODERATION_ENABLED=false)")
			return
		}
		instance = New(cfg, ctx)
		instance.Start()
	})
}

// Shutdown encerra o serviço (no-op quando ele nunca foi iniciado).
func Shutdown() {
	if instance != nil {
		instance.Stop()
	}
}

// Enqueue coloca um attachment na fila de moderação (não bloqueante; no-op
// quando a moderação está desativada). Se a fila estiver cheia, o attachment
// permanece 'pending' no banco e o reconciler tenta de novo.
func Enqueue(attachmentID string) {
	if instance != nil {
		instance.enqueue(attachmentID)
	}
}

// New cria o serviço de moderação.
func New(cfg *config.Config, ctx context.Context) *Service {
	queueSize := cfg.ModerationQueueSize
	if queueSize <= 0 {
		queueSize = 256
	}
	concurrency := cfg.ModerationConcurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	// O worker Python atende as conexões sequencialmente: workers Go
	// adicionais só enfileirariam no socket sem paralelizar a inferência.
	if concurrency > 1 {
		utils.Infof("moderação: MODERATION_CONCURRENCY=%d ignorado (worker Python é sequencial), usando 1", concurrency)
		concurrency = 1
	}

	return &Service{
		cfg:         cfg,
		ctx:         ctx,
		queue:       make(chan Job, queueSize),
		inflight:    make(map[string]struct{}),
		policy:      NewPolicy(cfg),
		client:      NewClient(cfg.ModerationSocketPath, cfg.ModerationTimeout),
		sup:         NewSupervisor(cfg),
		concurrency: concurrency,
		stopCh:      make(chan struct{}),
	}
}

// Start inicia o supervisor (que garante o bootstrap dos modelos e o worker
// Python, com retry), os workers da fila e o reconciler. Nunca falha: a
// degradação é logada.
func (s *Service) Start() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.sup.Run(s.ctx)
	}()

	for i := 0; i < s.concurrency; i++ {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.runWorker()
		}()
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runReconciler()
	}()

	utils.Info("moderação de imagens iniciada")
}

// Stop encerra workers, reconciler e o worker Python (idempotente). Sinaliza
// todos os componentes ANTES de esperar (o supervisor só sai quando recebe o
// próprio stop; sem isso o wg.Wait() deadlocks).
func (s *Service) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		s.sup.Stop()
		s.wg.Wait()
		os.Remove(s.cfg.ModerationSocketPath)
		utils.Info("moderação de imagens encerrada")
	})
}
