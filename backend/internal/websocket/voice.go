package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"papo/internal/services"
	"papo/internal/utils"
	"papo/internal/webrtc"
)

// voiceCheckTimeout é o tempo máximo da checagem de permissão de voz no join
// (mesmo padrão de handleTyping).
const voiceCheckTimeout = 5 * time.Second

// voiceManagerOrError retorna o manager global de voz ou envia o erro de
// indisponibilidade (manager nil) e retorna nil.
func (c *Client) voiceManagerOrError() *webrtc.Manager {
	if m := webrtc.GetManager(); m != nil {
		return m
	}
	c.sendEvent(ErrorOutbound{Type: EventTypeError, Message: "voz indisponível"})
	return nil
}

// sendVoiceError envia um evento `error` com o código de voz do erro de
// domínio do manager (seção 7 do plano).
func (c *Client) sendVoiceError(err error) {
	code := webrtc.ErrorCode(err)
	if code == "" {
		code = webrtc.CodeVoiceNotFound
	}
	c.sendEvent(ErrorOutbound{Type: EventTypeError, Message: err.Error(), Code: &code})
}

// handleVoiceJoin pede a entrada do usuário na sala de voz do canal. A
// permissão connect_voice é checada antes (fail-closed, D13); os demais
// eventos de voz dependem apenas de membership (verificado pelo manager).
func (c *Client) handleVoiceJoin(raw []byte) {
	var event VoiceJoinInbound
	if err := json.Unmarshal(raw, &event); err != nil || event.ChannelID == "" {
		c.sendEvent(ErrorOutbound{Type: EventTypeError, Message: "evento inválido"})
		return
	}
	m := c.voiceManagerOrError()
	if m == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), voiceCheckTimeout)
	defer cancel()

	switch err := services.CanConnectVoice(ctx, event.ChannelID, c.userID); {
	case errors.Is(err, services.ErrChannelNotFound):
		c.sendEvent(ErrorOutbound{Type: EventTypeError, Message: "canal não encontrado"})
		return
	case errors.Is(err, services.ErrInvalidChannelType), errors.Is(err, services.ErrPermissionDenied):
		c.sendVoiceError(webrtc.ErrVoiceForbidden)
		return
	case err != nil:
		utils.Errorf("websocket: falha ao verificar permissão de voz (user=%s, channel=%s): %v",
			c.userID, event.ChannelID, err)
		return
	}

	if err := m.Join(event.ChannelID, c.userID, c.clientID); err != nil {
		c.sendVoiceError(err)
	}
}

// handleVoiceLeave pede a saída explícita do usuário da sala de voz.
func (c *Client) handleVoiceLeave(raw []byte) {
	var event VoiceLeaveInbound
	if err := json.Unmarshal(raw, &event); err != nil || event.ChannelID == "" {
		c.sendEvent(ErrorOutbound{Type: EventTypeError, Message: "evento inválido"})
		return
	}
	m := c.voiceManagerOrError()
	if m == nil {
		return
	}
	if err := m.Leave(event.ChannelID, c.userID, c.clientID); err != nil {
		c.sendVoiceError(err)
	}
}

// handleVoiceOffer processa a oferta SDP do cliente (join, screen share, mais
// slots) e responde com voice_answer + trickle ICE (via manager).
func (c *Client) handleVoiceOffer(raw []byte) {
	var event VoiceOfferInbound
	if err := json.Unmarshal(raw, &event); err != nil || event.ChannelID == "" || event.SDP == "" {
		c.sendEvent(ErrorOutbound{Type: EventTypeError, Message: "evento inválido"})
		return
	}
	m := c.voiceManagerOrError()
	if m == nil {
		return
	}
	if err := m.ClientOffer(event.ChannelID, c.userID, c.clientID, event.SDP); err != nil {
		c.sendVoiceError(err)
	}
}

// handleVoiceAnswer processa a resposta SDP do cliente a uma renegociação
// iniciada pelo servidor.
func (c *Client) handleVoiceAnswer(raw []byte) {
	var event VoiceAnswerInbound
	if err := json.Unmarshal(raw, &event); err != nil || event.ChannelID == "" || event.SDP == "" {
		c.sendEvent(ErrorOutbound{Type: EventTypeError, Message: "evento inválido"})
		return
	}
	m := c.voiceManagerOrError()
	if m == nil {
		return
	}
	if err := m.ClientAnswer(event.ChannelID, c.userID, event.SDP, c.clientID); err != nil {
		c.sendVoiceError(err)
	}
}

// handleVoiceICECandidate entrega um candidate trickle de ICE do cliente.
func (c *Client) handleVoiceICECandidate(raw []byte) {
	var event VoiceICECandidateInbound
	if err := json.Unmarshal(raw, &event); err != nil || event.ChannelID == "" || event.Candidate == "" {
		c.sendEvent(ErrorOutbound{Type: EventTypeError, Message: "evento inválido"})
		return
	}
	m := c.voiceManagerOrError()
	if m == nil {
		return
	}
	sdpMLineIndex := 0
	if event.SDPMLineIndex != nil {
		sdpMLineIndex = *event.SDPMLineIndex
	}
	sdpMid := ""
	if event.SDPMid != nil {
		sdpMid = *event.SDPMid
	}
	if err := m.AddICECandidate(event.ChannelID, c.userID, c.clientID, event.Candidate, sdpMid, sdpMLineIndex); err != nil {
		c.sendVoiceError(err)
	}
}

// handleTrackSubscribe faz o subscriber começar a receber a track de
// vídeo/screen do publisher (vídeo sempre explícito, D5).
func (c *Client) handleTrackSubscribe(raw []byte) {
	var event TrackSubscribeInbound
	if err := json.Unmarshal(raw, &event); err != nil || event.ChannelID == "" ||
		event.PublisherID == "" || (event.Kind != "video" && event.Kind != "screen") {
		c.sendEvent(ErrorOutbound{Type: EventTypeError, Message: "evento inválido"})
		return
	}
	m := c.voiceManagerOrError()
	if m == nil {
		return
	}
	if err := m.Subscribe(event.ChannelID, c.userID, event.PublisherID, event.Kind, c.clientID); err != nil {
		c.sendVoiceError(err)
	}
}

// handleTrackUnsubscribe para de forwardar a track de vídeo/screen do
// publisher para o subscriber.
func (c *Client) handleTrackUnsubscribe(raw []byte) {
	var event TrackUnsubscribeInbound
	if err := json.Unmarshal(raw, &event); err != nil || event.ChannelID == "" ||
		event.PublisherID == "" || (event.Kind != "video" && event.Kind != "screen") {
		c.sendEvent(ErrorOutbound{Type: EventTypeError, Message: "evento inválido"})
		return
	}
	m := c.voiceManagerOrError()
	if m == nil {
		return
	}
	if err := m.Unsubscribe(event.ChannelID, c.userID, event.PublisherID, event.Kind, c.clientID); err != nil {
		c.sendVoiceError(err)
	}
}

// handleVoiceMute atualiza o estado do mic do usuário.
func (c *Client) handleVoiceMute(raw []byte) {
	var event VoiceMuteInbound
	if err := json.Unmarshal(raw, &event); err != nil || event.ChannelID == "" {
		c.sendEvent(ErrorOutbound{Type: EventTypeError, Message: "evento inválido"})
		return
	}
	m := c.voiceManagerOrError()
	if m == nil {
		return
	}
	if err := m.SetMuted(event.ChannelID, c.userID, c.clientID, event.Muted); err != nil {
		c.sendVoiceError(err)
	}
}

// handleVoiceCamera atualiza o estado da câmera do usuário.
func (c *Client) handleVoiceCamera(raw []byte) {
	var event VoiceCameraInbound
	if err := json.Unmarshal(raw, &event); err != nil || event.ChannelID == "" {
		c.sendEvent(ErrorOutbound{Type: EventTypeError, Message: "evento inválido"})
		return
	}
	m := c.voiceManagerOrError()
	if m == nil {
		return
	}
	if err := m.SetCameraOn(event.ChannelID, c.userID, c.clientID, event.On); err != nil {
		c.sendVoiceError(err)
	}
}

// handleScreenShareStart marca o início do screen share (a track nova chega na
// renegociação seguinte).
func (c *Client) handleScreenShareStart(raw []byte) {
	var event ScreenShareStartInbound
	if err := json.Unmarshal(raw, &event); err != nil || event.ChannelID == "" {
		c.sendEvent(ErrorOutbound{Type: EventTypeError, Message: "evento inválido"})
		return
	}
	m := c.voiceManagerOrError()
	if m == nil {
		return
	}
	if err := m.StartScreenShare(event.ChannelID, c.userID, c.clientID); err != nil {
		c.sendVoiceError(err)
	}
}

// handleScreenShareStop marca o fim do screen share.
func (c *Client) handleScreenShareStop(raw []byte) {
	var event ScreenShareStopInbound
	if err := json.Unmarshal(raw, &event); err != nil || event.ChannelID == "" {
		c.sendEvent(ErrorOutbound{Type: EventTypeError, Message: "evento inválido"})
		return
	}
	m := c.voiceManagerOrError()
	if m == nil {
		return
	}
	if err := m.StopScreenShare(event.ChannelID, c.userID, c.clientID); err != nil {
		c.sendVoiceError(err)
	}
}
