package moderation

import "papo/internal/utils"

// Job é a unidade de trabalho da fila de moderação: um attachment a
// classificar. A imagem NUNCA fica em memória na fila — o worker lê o blob do
// disco (caminho content-addressable derivado do hash) no momento do
// processamento.
type Job struct {
	AttachmentID string
}

// enqueue coloca o job na fila de forma não bloqueante. Se a fila estiver
// cheia, o attachment permanece 'pending' no banco (o reconciler tenta de
// novo). Deduplicação in-flight: o mesmo attachment é processado no máximo
// uma vez por vez.
func (s *Service) enqueue(id string) {
	if id == "" {
		return
	}

	s.mu.Lock()
	if _, ok := s.inflight[id]; ok {
		s.mu.Unlock()
		return
	}
	s.inflight[id] = struct{}{}
	s.mu.Unlock()

	select {
	case s.queue <- Job{AttachmentID: id}:
	default:
		s.forget(id)
		utils.Infof("moderação: fila cheia, attachment %s permanece pending (o reconciler tenta de novo)", id)
	}
}

// forget remove o attachment do conjunto in-flight (após processamento ou
// quando o enqueue não conseguiu entrar na fila).
func (s *Service) forget(id string) {
	s.mu.Lock()
	delete(s.inflight, id)
	s.mu.Unlock()
}
