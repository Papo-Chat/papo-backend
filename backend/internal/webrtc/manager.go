package webrtc

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"papo/internal/config"
	"papo/internal/utils"

	"golang.org/x/time/rate"
)

// Erros de domínio dos eventos de voz. O websocket/client.go mapeia cada um
// para o código de erro do evento `error` (seção 7 do plano).
var (
	ErrVoiceNotFound         = errors.New("usuário não está na sala de voz")
	ErrVoiceForbidden        = errors.New("sem permissão para a sala de voz")
	ErrVoiceRoomFull         = errors.New("sala de voz cheia")
	ErrVoiceAlreadyInRoom    = errors.New("usuário já está na sala de voz")
	ErrVoiceCodecUnsupported = errors.New("codec de vídeo não suportado na sala")
	ErrVoiceInvalidSDP       = errors.New("SDP inválido")
	ErrVoiceRateLimited      = errors.New("muitos eventos de voz")
	ErrVoiceRoomClosed       = errors.New("sala de voz encerrada")
)

// voiceErrorCode mapeia um erro de domínio para o código do evento `error`.
func voiceErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrVoiceNotFound):
		return CodeVoiceNotFound
	case errors.Is(err, ErrVoiceForbidden):
		return CodeVoiceForbidden
	case errors.Is(err, ErrVoiceRoomFull):
		return CodeVoiceRoomFull
	case errors.Is(err, ErrVoiceAlreadyInRoom):
		return CodeVoiceAlreadyInRoom
	case errors.Is(err, ErrVoiceCodecUnsupported):
		return CodeVoiceCodecUnsupported
	case errors.Is(err, ErrVoiceInvalidSDP):
		return CodeVoiceInvalidSDP
	case errors.Is(err, ErrVoiceRateLimited):
		return CodeVoiceRateLimited
	case errors.Is(err, ErrVoiceRoomClosed):
		return CodeVoiceRoomClosed
	default:
		return ""
	}
}

// ErrorCode expõe o mapeamento erro de domínio -> código do evento `error`
// para a camada de transporte (websocket), que envia o evento de erro.
func ErrorCode(err error) string {
	return voiceErrorCode(err)
}

// Signaler é o conjunto de funções concretas injetado pelo main.go a partir
// do hub WebSocket (evita ciclo de import — webrtc não importa websocket).
type Signaler struct {
	// SendToUser envia em unicast a todas as conexões do usuário.
	SendToUser func(userID string, event any)
	// SendToClient envia em unicast a UMA conexão (clientID). Usado para o
	// signaling WebRTC (answer/ICE/erros), que pertence à conexão que originou
	// — não pode vazar para outras abas/dispositivos do mesmo usuário.
	SendToClient func(clientID string, event any)
	// BroadcastToUsers envia somente aos clientes cujo usuário está em allowed.
	BroadcastToUsers func(allowed map[string]bool, event any)
	// VoiceAudience retorna os usuários online autorizados a escutar os
	// eventos de voz do canal (permissão connect_voice, via
	// services.VoiceConnectors). Nil/erro vira mapa vazio (ninguém recebe).
	VoiceAudience func(channelID string) map[string]bool
}

// Manager é o SFU embutido: estado efêmero de salas de voz em memória
// (1 backend = 1 servidor). Salas são criadas sob demanda (get-or-create)
// e destruídas após um grace period vazias.
type Manager struct {
	cfg      *config.Config
	signaler Signaler

	api *webrtcAPI // MediaEngine/interceptors compartilhados (codec da sala)

	mu    sync.RWMutex
	rooms map[string]*Room

	limiterMu sync.Mutex
	limiters  map[string]*userLimiters

	userMu    sync.Mutex
	userRooms map[string]map[string]struct{} // userID -> canalIDs (VOICE_MAX_ROOMS_PER_USER)

	ssrcMu    sync.Mutex
	ssrcOwner map[uint32]ssrcOwner // SSRC da track de áudio -> (sala, usuário)

	stopped atomic.Bool
}

// ssrcOwner associa o SSRC de uma track de áudio recebida à sala e ao usuário
// que a publica (o interceptor de audio-level usa para reportar o nível).
type ssrcOwner struct {
	room   *Room
	userID string
}

type userLimiters struct {
	signal    *rate.Limiter
	subscribe *rate.Limiter
}

// manager é o manager global (mesmo padrão do hub: websocket/hub.go).
// Nil até o main.go criar o manager; GetManager retorna nil nesse caso e os
// callers tratam (evento `error` genérico).
var manager *Manager

// NewManager cria o Manager global com a configuração e o Signaler do hub.
func NewManager(cfg *config.Config, s Signaler) *Manager {
	m := &Manager{
		cfg:       cfg,
		signaler:  s,
		rooms:     make(map[string]*Room),
		limiters:  make(map[string]*userLimiters),
		userRooms: make(map[string]map[string]struct{}),
		ssrcOwner: make(map[uint32]ssrcOwner),
	}
	api, err := newSharedAPI(cfg, m)
	if err != nil {
		// Codec inválido em VOICE_VIDEO_CODEC: o servidor não inicia
		// (mesma política de falha de configuração do boot).
		utils.Fatal("falha ao configurar o codec de vídeo de voz: " + err.Error())
	}
	m.api = api
	manager = m
	return m
}

// GetManager retorna o manager global (nil se o main.go ainda não o criou).
func GetManager() *Manager {
	return manager
}

// sendErrorToClient envia um evento `error` de voz a UMA conexão (clientID) —
// usado pelos erros de renegociação, que pertencem à conexão que originou o
// signaling (sem fallback para SendToUser: clientID vazio não envia).
func (m *Manager) sendErrorToClient(clientID string, err error) {
	if clientID == "" || m.signaler.SendToClient == nil {
		return
	}
	code := voiceErrorCode(err)
	if code == "" {
		code = CodeVoiceNotFound
	}
	m.signaler.SendToClient(clientID, VoiceError{
		Type:    "error",
		Message: err.Error(),
		Code:    code,
	})
}

// audience retorna a audiência autorizada do canal (usuários online com
// permissão connect_voice — mesma regra de quem pode entrar na call).
func (m *Manager) audience(channelID string) map[string]bool {
	if m.signaler.VoiceAudience == nil {
		return map[string]bool{}
	}
	return m.signaler.VoiceAudience(channelID)
}

// broadcastVoice envia um evento de voz aos leitores autorizados do canal.
func (m *Manager) broadcastVoice(channelID string, event any) {
	m.broadcastTo(m.audience(channelID), event)
}

// broadcastTo envia um evento de voz a uma audiência já computada.
func (m *Manager) broadcastTo(allowed map[string]bool, event any) {
	m.signaler.BroadcastToUsers(allowed, event)
}

// registerSSRC associa o SSRC de uma track de áudio recebida à sala/usuário
// (chamado quando a track de áudio do peer é estabelecida).
func (m *Manager) registerSSRC(ssrc uint32, room *Room, userID string) {
	m.ssrcMu.Lock()
	m.ssrcOwner[ssrc] = ssrcOwner{room: room, userID: userID}
	m.ssrcMu.Unlock()
}

// unregisterSSRC remove o SSRC do registro (peer fechado ou track substituída).
func (m *Manager) unregisterSSRC(ssrc uint32) {
	if ssrc == 0 {
		return
	}
	m.ssrcMu.Lock()
	delete(m.ssrcOwner, ssrc)
	m.ssrcMu.Unlock()
}

// reportAudioLevel encaminha o nível (dBFS) reportado pelo interceptor para a
// sala do SSRC (silencioso quando o SSRC não está registrado).
func (m *Manager) reportAudioLevel(ssrc uint32, level float64) {
	m.ssrcMu.Lock()
	o, ok := m.ssrcOwner[ssrc]
	m.ssrcMu.Unlock()
	if !ok {
		return
	}
	o.room.noteAudioLevel(o.userID, level)
}

// limiterFor retorna os token buckets do usuário (cria sob demanda).
func (m *Manager) limiterFor(userID string) *userLimiters {
	m.limiterMu.Lock()
	defer m.limiterMu.Unlock()
	l, ok := m.limiters[userID]
	if !ok {
		l = &userLimiters{
			signal:    rate.NewLimiter(rate.Limit(m.cfg.VoiceSignalRateLimit), m.cfg.VoiceSignalRateBurst),
			subscribe: rate.NewLimiter(rate.Limit(m.cfg.VoiceSubscribeRateLimit), m.cfg.VoiceSubscribeRateBurst),
		}
		m.limiters[userID] = l
	}
	return l
}

// checkSignal aplica o rate limit de sinalização do usuário.
func (m *Manager) checkSignal(userID string) error {
	if !m.limiterFor(userID).signal.Allow() {
		return ErrVoiceRateLimited
	}
	return nil
}

// checkSubscribe aplica o rate limit de subscribe do usuário.
func (m *Manager) checkSubscribe(userID string) error {
	if !m.limiterFor(userID).subscribe.Allow() {
		return ErrVoiceRateLimited
	}
	return nil
}

// getRoom retorna a sala do canal (nil quando não existe).
func (m *Manager) getRoom(channelID string) *Room {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.rooms[channelID]
}

// createRoom cria e registra a sala (o caller já validou os limites).
func (m *Manager) createRoom(channelID string) *Room {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.rooms[channelID]; existing != nil {
		return existing
	}
	room := newRoom(m, channelID)
	m.rooms[channelID] = room
	return room
}

// removeRoom remove a sala do mapa (idempotente).
func (m *Manager) removeRoom(channelID string) *Room {
	m.mu.Lock()
	room := m.rooms[channelID]
	delete(m.rooms, channelID)
	m.mu.Unlock()
	return room
}

// trackUserRoom registra/limpa a sala do usuário para VOICE_MAX_ROOMS_PER_USER.
func (m *Manager) trackUserRoom(userID, channelID string, add bool) {
	m.userMu.Lock()
	defer m.userMu.Unlock()
	if add {
		set := m.userRooms[userID]
		if set == nil {
			set = make(map[string]struct{})
			m.userRooms[userID] = set
		}
		set[channelID] = struct{}{}
		return
	}
	if set := m.userRooms[userID]; set != nil {
		delete(set, channelID)
		if len(set) == 0 {
			delete(m.userRooms, userID)
		}
	}
}

// userRoomCount retorna quantas salas distintas o usuário ocupa.
func (m *Manager) userRoomCount(userID string) int {
	m.userMu.Lock()
	defer m.userMu.Unlock()
	return len(m.userRooms[userID])
}

// Join adiciona o usuário à sala de voz do canal (cria o peer sem PC; a
// PeerConnection só existe após o voice_offer). Enforce de VOICE_MAX_ROOM_PEERS
// e VOICE_MAX_ROOMS_PER_USER. A permissão de canal (connect_voice) é checada
// antes, no websocket/client.go. clientID identifica a conexão que pediu o
// join (o voice_joined vai só para ela, não para todas as conexões do usuário).
func (m *Manager) Join(channelID, userID, clientID string) error {
	if m.stopped.Load() {
		return ErrVoiceRoomClosed
	}
	if err := m.checkSignal(userID); err != nil {
		return err
	}
	if err := m.reserveUserRoom(userID, channelID); err != nil {
		return err
	}

	for {
		if m.stopped.Load() {
			m.trackUserRoom(userID, channelID, false)
			return ErrVoiceRoomClosed
		}
		room := m.getRoom(channelID)
		if room == nil {
			room = m.createRoom(channelID)
		}
		err := room.addPeer(userID, clientID)
		if errors.Is(err, ErrVoiceRoomClosed) {
			// O cleanup venceu a corrida e já removeu a Room do manager.
			continue
		}
		if err != nil {
			m.trackUserRoom(userID, channelID, false)
			return err
		}
		return nil
	}
}

func (m *Manager) clientPeer(channelID, userID, clientID string) (*Room, *Peer, error) {
	room := m.getRoom(channelID)
	if room == nil {
		return nil, nil, ErrVoiceNotFound
	}
	peer := room.peer(userID)
	if peer == nil || !peer.ownsClient(clientID) {
		return nil, nil, ErrVoiceNotFound
	}
	return room, peer, nil
}

// Leave remove o usuário da sala de voz do canal (saída explícita).
func (m *Manager) Leave(channelID, userID, clientID string) error {
	if err := m.checkSignal(userID); err != nil {
		return err
	}
	room, peer, err := m.clientPeer(channelID, userID, clientID)
	if err != nil {
		return err
	}
	room.removePeer(peer)
	return nil
}

func (m *Manager) reserveUserRoom(userID, channelID string) error {
	m.userMu.Lock()
	defer m.userMu.Unlock()

	set := m.userRooms[userID]
	if set == nil {
		set = make(map[string]struct{})
		m.userRooms[userID] = set
	}
	if _, ok := set[channelID]; ok {
		return ErrVoiceAlreadyInRoom
	}
	if len(set) >= m.cfg.VoiceMaxRoomsPerUser {
		return ErrVoiceAlreadyInRoom
	}
	set[channelID] = struct{}{}
	return nil
}

func (m *Manager) ClientOffline(userID, clientID string) {
	m.userMu.Lock()
	channelIDs := make([]string, 0, len(m.userRooms[userID]))
	for id := range m.userRooms[userID] {
		channelIDs = append(channelIDs, id)
	}
	m.userMu.Unlock()

	for _, channelID := range channelIDs {
		room := m.getRoom(channelID)
		if room == nil {
			continue
		}
		peer := room.peer(userID)
		if peer != nil && peer.ownsClient(clientID) {
			room.removePeer(peer)
		}
	}
}

// ClientOffer processa a oferta SDP do cliente (join, screen share ou mais
// slots) e responde com voice_answer + trickle ICE. clientID é a conexão que
// enviou a oferta: ela passa a ser a "dono" do signaling da PeerConnection
// (voice_answer/ICE/erros vão só para ela).
func (m *Manager) ClientOffer(channelID, userID, clientID, sdp string) error {
	if err := m.checkSignal(userID); err != nil {
		return err
	}
	_, peer, err := m.clientPeer(channelID, userID, clientID)
	if err != nil {
		return err
	}
	if err := validateSDP(sdp, m.cfg, true); err != nil {
		return err
	}
	return peer.enqueue(func() error { return peer.handleOffer(clientID, sdp) })
}

// ClientAnswer processa a resposta SDP do cliente a uma renegociação
// iniciada pelo servidor.
func (m *Manager) ClientAnswer(channelID, userID, clientID, sdp string) error {
	if err := m.checkSignal(userID); err != nil {
		return err
	}
	_, peer, err := m.clientPeer(channelID, userID, clientID)
	if err != nil {
		return err
	}
	if err := validateSDP(sdp, m.cfg, false); err != nil {
		return err
	}
	return peer.enqueue(func() error { return peer.handleAnswer(sdp) })
}

// AddICECandidate entrega um candidate trickle de ICE do cliente.
func (m *Manager) AddICECandidate(channelID, userID, clientID, candidate, sdpMid string, sdpMLineIndex int) error {
	if err := m.checkSignal(userID); err != nil {
		return err
	}
	if err := validateCandidate(candidate); err != nil {
		return err
	}
	_, peer, err := m.clientPeer(channelID, userID, clientID)
	if err != nil {
		return err
	}
	return peer.enqueue(func() error {
		return peer.addICECandidate(candidate, sdpMid, sdpMLineIndex)
	})
}

// Subscribe faz o subscriber começar a receber a track de vídeo/screen do
// publisher em um slot de vídeo (vídeo sempre explícito, D5).
func (m *Manager) Subscribe(
	channelID,
	subscriberID,
	clientID,
	publisherID,
	kind string,
) error {
	if err := m.checkSubscribe(subscriberID); err != nil {
		return err
	}

	if kind != "video" && kind != "screen" {
		return ErrVoiceInvalidSDP
	}

	// Valida que ESTA conexão WebSocket é dona do subscriber.
	room, sub, err := m.clientPeer(channelID, subscriberID, clientID)
	if err != nil {
		return err
	}

	// Publisher é apenas o alvo da assinatura.
	pub := room.peer(publisherID)
	if pub == nil {
		return ErrVoiceNotFound
	}

	if sub == pub {
		return ErrVoiceNotFound
	}

	if kind == "video" && !pub.hasActiveTrack("video") {
		return ErrVoiceNotFound
	}

	if kind == "screen" && !pub.hasActiveTrack("screen") {
		return ErrVoiceNotFound
	}

	return sub.assignVideoSlot(pub, kind)
}

// Unsubscribe para de forwardar a track de vídeo/screen do publisher para o
// subscriber (o slot fica vazio, sem renegociação).
func (m *Manager) Unsubscribe(
	channelID,
	subscriberID,
	clientID,
	publisherID,
	kind string,
) error {
	if err := m.checkSubscribe(subscriberID); err != nil {
		return err
	}

	_, sub, err := m.clientPeer(channelID, subscriberID, clientID)
	if err != nil {
		return err
	}

	sub.releaseVideoSlot(publisherID, kind)
	return nil
}

// SetMuted atualiza o estado do mic do usuário e notifica os leitores do
// canal (o cliente para de enviar frames; os slots param de receber pacotes).
func (m *Manager) SetMuted(channelID, userID, clientID string, muted bool) error {
	if err := m.checkSignal(userID); err != nil {
		return err
	}
	room, _, err := m.clientPeer(channelID, userID, clientID)
	if err != nil {
		return err
	}
	if room == nil || !room.hasPeer(userID) {
		return ErrVoiceNotFound
	}
	room.setMuted(userID, muted)
	return nil
}

// SetCameraOn atualiza o estado da câmera do usuário e notifica os leitores
// do canal (o cliente para de enviar frames; idem mic).
func (m *Manager) SetCameraOn(channelID, userID, clientID string, on bool) error {
	if err := m.checkSignal(userID); err != nil {
		return err
	}
	room, _, err := m.clientPeer(channelID, userID, clientID)
	if err != nil {
		return err
	}
	if room == nil || !room.hasPeer(userID) {
		return ErrVoiceNotFound
	}
	room.setCameraOn(userID, on)
	return nil
}

// StartScreenShare atualiza o estado de screen share do usuário e notifica
// os leitores do canal. A track nova chega na renegociação seguinte
// (voice_offer com a m-line de vídeo adicional).
func (m *Manager) StartScreenShare(channelID, userID, clientID string) error {
	if err := m.checkSignal(userID); err != nil {
		return err
	}
	room, _, err := m.clientPeer(channelID, userID, clientID)
	if err != nil {
		return err
	}
	if room == nil || !room.hasPeer(userID) {
		return ErrVoiceNotFound
	}
	room.setScreenSharing(userID, true)
	return nil
}

// StopScreenShare remove o estado de screen share do usuário (a track é
// removida na renegociação seguinte).
func (m *Manager) StopScreenShare(channelID, userID, clientID string) error {
	if err := m.checkSignal(userID); err != nil {
		return err
	}
	room, _, err := m.clientPeer(channelID, userID, clientID)
	if err != nil {
		return err
	}
	if room == nil || !room.hasPeer(userID) {
		return ErrVoiceNotFound
	}
	room.setScreenSharing(userID, false)
	return nil
}

// UserOffline remove o peer do usuário de todas as salas (a última conexão
// WebSocket dele caiu — gancho do hub). Chamado pelo hub.
func (m *Manager) UserOffline(userID string) {
	m.userMu.Lock()
	channelIDs := make([]string, 0, len(m.userRooms[userID]))
	for id := range m.userRooms[userID] {
		channelIDs = append(channelIDs, id)
	}
	m.userMu.Unlock()

	for _, channelID := range channelIDs {
		room := m.getRoom(channelID)
		if room == nil {
			continue
		}

		peer := room.peer(userID)
		if peer == nil {
			continue
		}

		// removePeer também limpa userRooms.
		room.removePeer(peer)
	}
	m.limiterMu.Lock()
	delete(m.limiters, userID)
	m.limiterMu.Unlock()
}

// DestroyRoom destrói a sala de voz do canal (canal de voz excluído): fecha
// todas as PeerConnections e notifica os membros (error voice-room-closed +
// voice_leave). Chamado por services.DeleteChannel.
func (m *Manager) DestroyRoom(channelID string) {
	room := m.removeRoom(channelID)
	if room == nil {
		return
	}
	room.destroyWithClosedNotice()
}

// Shutdown fecha todas as PeerConnections e para o manager (encerramento da
// aplicação). Chamado pelo main.go após o hub.Shutdown.
func (m *Manager) Shutdown() {
	if !m.stopped.CompareAndSwap(false, true) {
		return
	}
	m.mu.Lock()
	rooms := make([]*Room, 0, len(m.rooms))
	for _, r := range m.rooms {
		rooms = append(rooms, r)
	}
	m.rooms = make(map[string]*Room)
	m.mu.Unlock()

	for _, room := range rooms {
		room.destroy()
	}

	// Encerra o ICEUDPMux (libera a porta UDP compartilhada).
	if m.api != nil && m.api.iceMux != nil {
		_ = m.api.iceMux.Close()
	}
}

// GracePeriod retorna o grace period de destruição de salas vazias (D11).
func (m *Manager) GracePeriod() time.Duration {
	if m.cfg.VoiceRoomCleanupGrace > 0 {
		return m.cfg.VoiceRoomCleanupGrace
	}
	return 30 * time.Second
}
