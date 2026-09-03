package moderation

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/storage"
	"papo/internal/utils"
	"papo/internal/websocket"
)

// moderableMimeTypes são os MIMEs de imagem sujeitos à moderação (mesmo
// conjunto dos thumbnails; o worker Python usa PIL e só lê estes formatos).
var moderableMimeTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
	"image/gif":  true,
}

// runWorker é o loop de um worker: consome a fila e processa os attachments
// (a concorrência de inferência é limitada por MODERATION_CONCURRENCY).
func (s *Service) runWorker() {
	for {
		select {
		case <-s.stopCh:
			return
		case job := <-s.queue:
			s.process(job)
		}
	}
}

// process trata um attachment: lê o registro, marca como processing, faz a
// inferência, aplica a política e persiste o resultado. Falhas são logadas e
// registradas — o job nunca trava a fila.
func (s *Service) process(job Job) {
	defer s.forget(job.AttachmentID)

	ctx := s.ctx

	attachment, err := storage.GetAttachmentByID(ctx, job.AttachmentID)
	if errors.Is(err, storage.ErrNotFound) {
		return // excluído durante o processamento (exclusão de mensagem, GC de órfãos)
	}
	if err != nil {
		utils.Warnf("moderação: falha ao ler attachment %s: %v", job.AttachmentID, err)
		return
	}

	// Não-imagem: nada a inferir.
	if !moderableMimeTypes[attachment.MimeType] {
		if err := storage.FinishAttachmentModeration(ctx, attachment.ID, string(StatusClean), nil, nil, nil, nil); err != nil {
			utils.Warnf("moderação: falha ao marcar attachment %s como clean: %v", attachment.ID, err)
		}
		return
	}

	if !s.sup.Ready() {
		// Worker não pronto: permanece 'pending' (o reconciler tenta de novo).
		return
	}

	claimed, err := storage.ClaimAttachmentForModeration(ctx, attachment.ID, staleProcessing)
	if err != nil {
		utils.Warnf("moderação: falha ao marcar attachment %s como processing: %v", attachment.ID, err)
		return
	}
	if !claimed {
		return // estado mudou (ex.: já finalizado por outra rota)
	}

	// Resolve o blob no disco (content-addressable). O worker Python só
	// aceita caminhos absolutos dentro da pasta de mídia (containment).
	_, blobPath, err := services.GetMedia(ctx, attachment.MediaShaHash)
	if err != nil {
		s.handleInferenceFailure(attachment, err)
		return
	}
	if blobPath, err = filepath.Abs(blobPath); err != nil {
		s.handleInferenceFailure(attachment, err)
		return
	}

	result, err := s.client.Classify(ctx, blobPath, attachment.MimeType)
	if err != nil {
		s.handleInferenceFailure(attachment, err)
		return
	}

	status, reason := s.policy.Evaluate(result)
	sfw, nudity, gore := result.SFW, result.Nudity, result.Gore
	modelVersion := result.Model
	if err := storage.FinishAttachmentModeration(ctx, attachment.ID, string(status), &modelVersion, &sfw, &nudity, &gore); err != nil {
		utils.Warnf("moderação: falha ao persistir o resultado do attachment %s: %v", attachment.ID, err)
		return
	}

	switch status {
	case StatusBlocked:
		s.handleBlocked(attachment, reason, result)
	case StatusSensitive:
		s.broadcastModerationUpdate(attachment)
	}
}

// handleInferenceFailure registra uma tentativa frustrada: volta a 'pending'
// (o reconciler tenta de novo) ou 'failed' quando as tentativas se esgotam.
func (s *Service) handleInferenceFailure(attachment models.Attachments, cause error) {
	attempt := attachment.ModerationAttempts + 1
	utils.Warnf("moderação: inferência falhou para o attachment %s (tentativa %d/%d): %v",
		attachment.ID, attempt, maxModerationAttempts, cause)

	next := string(StatusPending)
	if attempt >= maxModerationAttempts {
		next = string(StatusFailed)
		utils.Errorf("moderação: attachment %s marcado como failed (tentativas esgotadas)", attachment.ID)
	}
	if err := storage.FailAttachmentModeration(s.ctx, attachment.ID, next); err != nil {
		utils.Warnf("moderação: falha ao registrar a falha do attachment %s: %v", attachment.ID, err)
	}
}

// handleBlocked exclui a mensagem inteira (ON DELETE CASCADE remove os
// attachments e thumbnails) e grava o log de auditoria explícito do que
// aconteceu. O evento message_delete chega ao cliente em tempo real.
func (s *Service) handleBlocked(attachment models.Attachments, reason string, result Result) {
	if attachment.MessagesID == nil {
		utils.Warnf("moderação: attachment %s blocked sem mensagem vinculada (nada a excluir)", attachment.ID)
		return
	}

	messageID := *attachment.MessagesID
	message, err := storage.GetMessageByID(s.ctx, messageID)
	if errors.Is(err, storage.ErrNotFound) {
		return // mensagem já excluída
	}
	if err != nil {
		utils.Warnf("moderação: falha ao ler a mensagem %s (blocked): %v", messageID, err)
		return
	}

	if err := storage.DeleteMessage(s.ctx, messageID); err != nil {
		utils.Errorf("moderação: falha ao excluir a mensagem %s (blocked pela moderação): %v", messageID, err)
		return
	}

	services.RecordAudit(s.ctx, services.AuditEntry{
		ActorID:      "", // ação de sistema: actor_id NULL, actor_username "sistema"
		Action:       services.ActionMessageModerationBlocked,
		EntityType:   services.EntityMessage,
		EntityID:     &messageID,
		TargetUserID: attachment.CreatedBy,
		Metadata: map[string]any{
			"channel_id":    message.ChannelID,
			"reason":        reason,
			"attachment_id": attachment.ID,
			"model_version": result.Model,
			"sfw_score":     result.SFW,
			"nudity_score":  result.Nudity,
			"gore_score":    result.Gore,
		},
	})

	utils.Warnf("moderação: mensagem %s excluída (blocked: %s, canal %s)", messageID, reason, message.ChannelID)

	broadcastChannelEvent(s.ctx, message.ChannelID, websocket.MessageDeleteOutbound{
		Type:      websocket.EventTypeMessageDelete,
		ID:        messageID,
		ChannelID: message.ChannelID,
	})
}

// broadcastModerationUpdate notifica os leitores do canal sobre a mudança de
// estado de moderação do attachment (evento attachment_moderation_update).
func (s *Service) broadcastModerationUpdate(attachment models.Attachments) {
	if attachment.MessagesID == nil {
		return
	}
	message, err := storage.GetMessageByID(s.ctx, *attachment.MessagesID)
	if err != nil {
		utils.Warnf("moderação: falha ao ler a mensagem para o evento do attachment %s: %v", attachment.ID, err)
		return
	}

	broadcastChannelEvent(s.ctx, message.ChannelID, websocket.AttachmentModerationUpdateOutbound{
		Type:         websocket.EventTypeAttachmentModerationUpdate,
		ChannelID:    message.ChannelID,
		MessageID:    message.ID,
		AttachmentID: attachment.ID,
		Status:       string(StatusSensitive),
	})
}

// runReconciler recoloca periodicamente na fila os attachments pendentes de
// moderação (incluindo 'processing' órfãos de um crash). Roda também no boot
// (catch-up da fila persistida no banco).
func (s *Service) runReconciler() {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	s.reconcile()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.reconcile()
		}
	}
}

func (s *Service) reconcile() {
	ids, err := storage.ListModerationPending(s.ctx, staleProcessing, reconcileBatchSize)
	if err != nil {
		utils.Warnf("moderação: reconciler falha ao listar os pendentes: %v", err)
		return
	}
	for _, id := range ids {
		s.enqueue(id)
	}
	if len(ids) > 0 {
		utils.Infof("moderação: reconciler recolocou %d attachment(s) pendente(s) na fila", len(ids))
	}
}

// broadcastChannelEvent envia um evento via WebSocket somente aos clientes
// cujo usuário pode ler o canal (mesma regra de ListMessages; fail closed).
func broadcastChannelEvent(ctx context.Context, channelID string, event any) {
	hub := websocket.GetHub()
	allowed, err := services.ChannelReaders(ctx, channelID, hub.OnlineUserIDs())
	if err != nil {
		utils.Warnf("moderação: falha ao autorizar o broadcast do canal %s: %v", channelID, err)
		return
	}
	hub.BroadcastToUsers(event, allowed)
}
