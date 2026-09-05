package webrtc

import (
	"sync"

	"papo/internal/models"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// trackRole classifica as m-lines de vídeo/áudio do offer do cliente. A
// primeira m-line de vídeo é a câmera; a segunda (quando existe) é o screen
// share (seção 5.6 — determinístico, sem depender de label).
type trackRole int

const (
	roleAudio trackRole = iota
	roleCamera
	roleScreen
)

// Peer é a PeerConnection de um usuário em uma sala (D10: 1 peer por usuário
// por sala, não por conexão WS). A PC só existe após o primeiro voice_offer.
//
// Disciplina de lock: p.mu protege o estado do peer (tracks, slots, estado de
// voz, pc). NUNCA segurar p.mu durante chamadas de PC do pion
// (SetRemoteDescription/CreateAnswer/SetLocalDescription/AddTransceiverFromTrack)
// nem durante chamadas do signaler (SendToUser/BroadcastToUsers) — essas são
// feitas com p.mu solto. O worker de renegociação é sequencial por PC (D4), o
// que serializa o signaling sem p.mu.
type Peer struct {
	m      *Manager
	room   *Room
	userID string

	mu     sync.Mutex
	pc     *webrtc.PeerConnection
	closed bool

	// signalingClient é o clientID da conexão que originou o signaling da PC
	// (definido pelo voice_offer). voice_answer/ICE/erros de reneg vão só para
	// ela — nunca para todas as conexões do usuário (outras abas/dispositivos).
	signalingClient string

	// tracks recebidas (cliente → servidor)
	audioTrack  *webrtc.TrackRemote
	videoTrack  *webrtc.TrackRemote // câmera
	screenTrack *webrtc.TrackRemote // screen share
	audioSSRC   uint32              // SSRC da track de áudio (registro p/ active speaker)

	// fanouts: 1 reader por track recebida (kind → fanout). Os subscribers
	// forwardam via o fanout do publisher (nunca chamam ReadRTP diretamente).
	fanouts map[string]*fanout

	// Identidade persistente do MID. Uma vez que um MID vira camera/screen/audio,
	// ele mantém esse papel durante toda a vida da PeerConnection.
	midRole map[string]trackRole

	// MIDs que estão efetivamente publicando no offer atual.
	activeMidRole map[string]trackRole

	// MID ao qual cada TrackRemote armazenada pertence.
	audioMID  string
	videoMID  string
	screenMID string

	// slots de envio (servidor → cliente): N vídeo + K áudio (D5)
	videoSlots []*slot
	audioSlots []*slot

	// estado de voz (o que a UI precisa)
	muted         bool
	cameraOn      bool
	screenSharing bool
	audioSet      []string // top-K de áudio atual (p/ detecção de mudança)

	// fila de renegociação (D4): worker sequencial por PC
	renegQueue chan func() error
	renegStop  chan struct{}
}

// newPeer cria o peer sem PeerConnection (a PC nasce no primeiro
// voice_offer) e inicia o worker de renegociação.
func newPeer(m *Manager, room *Room, userID, clientID string) *Peer {
	p := &Peer{
		m:               m,
		room:            room,
		userID:          userID,
		signalingClient: clientID,
		fanouts:         make(map[string]*fanout),
		midRole:         make(map[string]trackRole),
		activeMidRole:   make(map[string]trackRole),
		renegQueue:      make(chan func() error, 64),
		renegStop:       make(chan struct{}),
	}
	go p.renegWorker()
	return p
}

// renegWorker processa a fila de renegociação sequencialmente (D4 — pion não
// serializa CreateOffer/SetLocalDescription/SetRemoteDescription).
func (p *Peer) renegWorker() {
	for {
		select {
		case <-p.renegStop:
			return
		case fn := <-p.renegQueue:
			if err := fn(); err != nil {
				// Renegociação falhou: o cliente vê o erro e pode tentar de novo.
				// Vai à conexão dona do signaling (p.mu solto durante o send).
				p.mu.Lock()
				clientID := p.signalingClient
				p.mu.Unlock()
				p.m.sendErrorToClient(clientID, err)
			}
		}
	}
}

// enqueue adiciona uma operação de signaling à fila do peer (não bloqueia).
func (p *Peer) enqueue(fn func() error) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrVoiceRoomClosed
	}
	p.mu.Unlock()
	select {
	case p.renegQueue <- fn:
		return nil
	default:
		return ErrVoiceRateLimited
	}
}

// PC retorna a PeerConnection (nil antes do primeiro offer).
func (p *Peer) PC() *webrtc.PeerConnection {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pc
}

// setSignalingClient define a conexão dona do signaling da PC (chamado pelo
// handleOffer). Usado também pelos testes para isolar o roteamento de erros.
func (p *Peer) ownsClient(clientID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return clientID != "" && p.signalingClient == clientID
}

// ensurePC cria a PeerConnection na primeira vez (o worker de renegociação é
// sequencial, então não há corrida na criação). Retorna false se o peer
// fechou.
func (p *Peer) ensurePC() bool {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return false
	}
	existing := p.pc
	p.mu.Unlock()
	if existing != nil {
		return true
	}

	pc, err := p.m.api.NewPeerConnection(webrtc.Configuration{
		ICEServers: p.m.iceServers(),
	})
	if err != nil {
		return false
	}
	p.setupPCEvents(pc)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = pc.Close()
		return false
	}
	p.pc = pc
	return true
}

// handleOffer processa a oferta SDP do cliente (join, screen share ou mais
// slots) e responde com voice_answer + trickle ICE. clientID é a conexão que
// enviou a oferta: ela passa a ser a "dona" do signaling da PC.
func (p *Peer) handleOffer(clientID, sdp string) error {
	if !p.ownsClient(clientID) {
		return ErrVoiceNotFound
	}
	if !p.ensurePC() {
		return ErrVoiceRoomClosed
	}
	pc := p.PC()
	if pc == nil {
		return ErrVoiceNotFound
	}

	// Atualiza o mapa mid → papel ANTES do SetRemoteDescription (OnTrack só
	// dispara após ICE/DTLS, mas o mapa já deve estar correto). A conexão que
	// enviou a oferta torna-se a dona do signaling (answer/ICE/erros vão a ela).
	p.mu.Lock()

	previousRoles := cloneTrackRoles(p.midRole)
	previousActive := cloneTrackRoles(p.activeMidRole)

	cameraOn := p.cameraOn
	screenOn := p.screenSharing

	firstOffer := len(p.videoSlots) == 0 && len(p.audioSlots) == 0

	p.mu.Unlock()

	roles, activeRoles, err := parseMidRoles(
		sdp,
		previousRoles,
		cameraOn,
		screenOn,
	)
	if err != nil {
		return err
	}

	p.mu.Lock()

	p.midRole = roles
	p.activeMidRole = activeRoles

	cameraStopped := hasRole(previousActive, roleCamera) &&
		!hasRole(activeRoles, roleCamera)

	screenStopped := hasRole(previousActive, roleScreen) &&
		!hasRole(activeRoles, roleScreen)

	p.mu.Unlock()

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdp,
	}); err != nil {
		// Não deixe um SDP rejeitado contaminar o estado.
		p.mu.Lock()
		p.midRole = previousRoles
		p.activeMidRole = previousActive
		p.mu.Unlock()

		return err
	}

	if firstOffer {
		if err := p.allocateSlots(); err != nil {
			return err
		}
	}

	if cameraStopped {
		p.room.releasePublishedKind(p, "video")
	}

	if screenStopped {
		p.room.releasePublishedKind(p, "screen")
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return err
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return err
	}

	// voice_answer (unicast à conexão dona do signaling) — p.mu solto.
	if p.m.signaler.SendToClient != nil {
		p.m.signaler.SendToClient(clientID, VoiceAnswer{
			Type:      EventTypeVoiceAnswer,
			ChannelID: p.room.channelID,
			SDP:       answer.SDP,
		})
	}
	return nil
}

// handleAnswer processa a resposta SDP do cliente a uma renegociação
// iniciada pelo servidor.
func (p *Peer) handleAnswer(sdp string) error {
	pc := p.PC()
	if pc == nil {
		return ErrVoiceNotFound
	}
	return pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdp,
	})
}

// addICECandidate entrega um candidate trickle de ICE do cliente à PC.
func (p *Peer) addICECandidate(candidate, sdpMid string, sdpMLineIndex int) error {
	pc := p.PC()
	if pc == nil {
		return ErrVoiceNotFound
	}
	return pc.AddICECandidate(toICECandidateInit(candidate, sdpMid, sdpMLineIndex))
}

// setupPCEvents liga os callbacks da PeerConnection. Os callbacks podem ser
// disparados por goroutines do pion; eles adquirem p.mu (nunca o inverso).
func (p *Peer) setupPCEvents(pc *webrtc.PeerConnection) {
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		mid := ""
		if tc := receiver.RTPTransceiver(); tc != nil {
			mid = tc.Mid()
		}
		p.onIncomingTrack(track, mid)
	})

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		init := candidate.ToJSON()
		// ICE do servidor vai à conexão dona do signaling (p.mu para ler o
		// clientID; o send é feito fora do lock).
		p.mu.Lock()
		clientID := p.signalingClient
		p.mu.Unlock()
		if clientID != "" && p.m.signaler.SendToClient != nil {
			p.m.signaler.SendToClient(clientID, VoiceICECandidate{
				Type:          EventTypeVoiceICECandidate,
				ChannelID:     p.room.channelID,
				Candidate:     init.Candidate,
				SDPMid:        init.SDPMid,
				SDPMLineIndex: intFromU16Ptr(init.SDPMLineIndex),
			})
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			// A última conexão caiu: remove o peer da sala (libera slots,
			// notifica os leitores). disconnected → aguarda recuperação.
			p.room.removePeer(p)
		}
	})
}

// onIncomingTrack classifica a track recebida (pelo mid) e a armazena. O
// registro do SSRC (active speaker) é feito SEM p.mu para não criar o ciclo
// de lock p.mu → m.ssrcMu → r.mu → p.mu.
func (p *Peer) onIncomingTrack(track *webrtc.TrackRemote, mid string) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}

	role, ok := p.activeMidRole[mid]
	if !ok {
		p.mu.Unlock()
		return
	}

	var newSSRC, oldSSRC uint32
	var oldFanout *fanout
	switch role {
	case roleAudio:
		oldFanout = p.fanouts["audio"]

		p.audioTrack = track
		p.audioMID = mid

		newSSRC = uint32(track.SSRC())
		oldSSRC = p.audioSSRC
		p.audioSSRC = newSSRC

	case roleCamera:
		oldFanout = p.fanouts["video"]

		p.videoTrack = track
		p.videoMID = mid

	case roleScreen:
		oldFanout = p.fanouts["screen"]

		p.screenTrack = track
		p.screenMID = mid
	}
	p.mu.Unlock()

	if oldFanout != nil && oldFanout.track != track {
		oldFanout.destroy()
	}

	if newSSRC != 0 {
		// Registra antes de iniciar ReadRTP para não perder os primeiros levels.
		p.m.registerSSRC(newSSRC, p.room, p.userID)
		if oldSSRC != 0 && oldSSRC != newSSRC {
			p.m.unregisterSSRC(oldSSRC)
		}
	}

	switch role {
	case roleAudio:
		// P0: o active-speaker depende de ReadRTP. O reader precisa existir mesmo
		// sem subscribers, senão scores/top-K nunca nascem.
		_ = p.fanoutFor("audio", track)
	case roleCamera:
		p.room.rebindPublishedTrack(p, "video", track)
	case roleScreen:
		p.room.rebindPublishedTrack(p, "screen", track)
	}
}

func (p *Peer) rebindVideoSlot(pub *Peer, kind string, track *webrtc.TrackRemote) {
	p.mu.Lock()
	hasSlot := false
	for _, s := range p.videoSlots {
		if s.owner == pub && s.kind == kind {
			hasSlot = true
			break
		}
	}
	p.mu.Unlock()
	if !hasSlot {
		return
	}

	fanout := pub.fanoutFor(kind, track)
	if fanout == nil {
		return
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	rebound := false
	for _, s := range p.videoSlots {
		if s.owner == pub && s.kind == kind && s.src != track {
			s.assignWithFanout(pub, kind, track, fanout)
			rebound = true
		}
	}
	p.mu.Unlock()

	if rebound {
		sendPLI(pub.PC(), track)
	}
}

// allocateSlots cria os slots de envio (N vídeo + K áudio) com tracks
// placeholder (D5). As chamadas AddTransceiverFromTrack são feitas SEM p.mu;
// as slices de slots são publicadas sob p.mu ao final.
func (p *Peer) allocateSlots() error {
	pc := p.PC()
	if pc == nil {
		return ErrVoiceNotFound
	}
	n := p.m.cfg.VoiceVideoSlots
	k := p.m.cfg.VoiceAudioSlots
	videoCodec := p.m.api.videoCodec.RTPCodecCapability
	audioCodec := webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	}

	videoSlots := make([]*slot, 0, n)
	for i := 0; i < n; i++ {
		track, err := webrtc.NewTrackLocalStaticRTP(videoCodec, "papo", "papo-video")
		if err != nil {
			return err
		}
		t, err := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionSendonly,
		})
		if err != nil {
			return err
		}
		videoSlots = append(videoSlots, &slot{peer: p, sender: t.Sender(), local: track})
	}

	audioSlots := make([]*slot, 0, k)
	for i := 0; i < k; i++ {
		track, err := webrtc.NewTrackLocalStaticRTP(audioCodec, "papo", "papo-audio")
		if err != nil {
			return err
		}
		t, err := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionSendonly,
		})
		if err != nil {
			return err
		}
		audioSlots = append(audioSlots, &slot{peer: p, sender: t.Sender(), local: track})
	}

	p.mu.Lock()
	p.videoSlots = videoSlots
	p.audioSlots = audioSlots
	p.mu.Unlock()
	return nil
}

// trackOfKind retorna a track recebida do peer para o kind (video/screen/audio).
func (p *Peer) trackOfKind(kind string) *webrtc.TrackRemote {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.trackOfKindLocked(kind)
}

func (p *Peer) trackOfKindLocked(kind string) *webrtc.TrackRemote {
	switch kind {
	case "video":
		if p.videoTrack == nil {
			return nil
		}
		if p.activeMidRole[p.videoMID] != roleCamera {
			return nil
		}
		return p.videoTrack

	case "screen":
		if p.screenTrack == nil {
			return nil
		}
		if p.activeMidRole[p.screenMID] != roleScreen {
			return nil
		}
		return p.screenTrack

	case "audio":
		if p.audioTrack == nil {
			return nil
		}
		if p.activeMidRole[p.audioMID] != roleAudio {
			return nil
		}
		return p.audioTrack
	}

	return nil
}

// AudioTrack retorna a track de áudio (mic) do peer.
func (p *Peer) AudioTrack() *webrtc.TrackRemote {
	return p.trackOfKind("audio")
}

// hasActiveTrack indica se o peer está publicando a track do kind.
func (p *Peer) hasActiveTrack(kind string) bool {
	return p.trackOfKind(kind) != nil
}

// fanoutFor retorna o fanout (reader único) da track do kind, criando sob
// demanda. Retorna nil quando a track é nil. O caller NÃO pode estar segurando
// p.mu (adquire internamente) — os callers resolvem o fanout ANTES de segurar
// o lock do subscriber (evita subscriber.mu → publisher.mu).
//
// Se a track do kind mudou (renegociação), o fanout antigo é destruído e um
// novo é criado para a track nova.
func (p *Peer) fanoutFor(kind string, track *webrtc.TrackRemote) *fanout {
	if track == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if f := p.fanouts[kind]; f != nil && f.track == track {
		return f
	}
	if old := p.fanouts[kind]; old != nil {
		old.destroy()
	}
	f := newFanout(track)
	p.fanouts[kind] = f
	f.start()
	return f
}

// stopAllFanouts destrói todos os fanouts do peer (chamado em close).
func (p *Peer) stopAllFanouts() {
	p.mu.Lock()
	fanouts := make([]*fanout, 0, len(p.fanouts))
	for _, f := range p.fanouts {
		fanouts = append(fanouts, f)
	}
	p.fanouts = make(map[string]*fanout)
	p.mu.Unlock()
	for _, f := range fanouts {
		f.destroy()
	}
}

// assignVideoSlot faz o subscriber começar a receber a track de vídeo/screen
// do publisher em um slot de vídeo (D5). Sem slot livre → ErrVoiceRoomFull
// (o caminho de renegociação para adicionar slot é raro/fase 2).
func (p *Peer) assignVideoSlot(pub *Peer, kind string) error {
	track := pub.trackOfKind(kind)
	if track == nil {
		return ErrVoiceNotFound
	}

	pubPC := pub.PC()

	fanout := pub.fanoutFor(kind, track)
	if fanout == nil {
		return ErrVoiceNotFound
	}

	p.mu.Lock()

	if p.closed {
		p.mu.Unlock()
		return ErrVoiceRoomClosed
	}

	slot := p.findVideoSlotFor(pub, kind)
	if slot == nil {
		p.mu.Unlock()
		return ErrVoiceRoomFull
	}

	slot.assignWithFanout(pub, kind, track, fanout)

	p.mu.Unlock()

	// Pion chamada com p.mu solto.
	sendPLI(pubPC, track)

	return nil
}

// findVideoSlotFor retorna o slot para (owner, kind): o slot já ocupado por
// (owner, kind) [reassign] ou um slot livre. Nil quando não há (o caller
// segura peer.mu).
func (p *Peer) findVideoSlotFor(owner *Peer, kind string) *slot {
	for _, s := range p.videoSlots {
		if s.owner == owner && s.kind == kind {
			return s
		}
	}
	for _, s := range p.videoSlots {
		if s.owner == nil {
			return s
		}
	}
	return nil
}

// releaseVideoSlot para de forwardar a track do publisher (o slot fica vazio).
func (p *Peer) releaseVideoSlot(pubID, kind string) {
	pub := p.room.peer(pubID) // resolve ANTES de p.mu (evita p.mu → r.mu)

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.videoSlots {
		if s.owner == pub && s.kind == kind {
			s.release()
		}
	}
}

// releaseAllFrom libera todos os slots do subscriber que forwardam do
// publisher (vídeo + áudio) — usado quando o publisher sai da sala.
func (p *Peer) releaseAllFrom(pub *Peer) {
	if pub == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.videoSlots {
		if s.owner == pub {
			s.release()
		}
	}
	for _, s := range p.audioSlots {
		if s.owner == pub {
			s.release()
		}
	}
}

func (p *Peer) releaseFrom(pub *Peer, kind string) {
	if pub == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	for _, s := range p.videoSlots {
		if s.owner == pub && s.kind == kind {
			s.release()
		}
	}
}

// setAudioSet atualiza o top-K de áudio do subscriber e sincroniza os slots.
// owners e tracks já resolvidos pelo caller (room.tick, sob o lock da sala) —
// evita p.mu → r.mu e p.mu → other.p.mu.
func (p *Peer) setAudioSet(set []string, owners []*Peer, tracks []*webrtc.TrackRemote) {
	// Resolve os fanouts dos publishers ANTES de segurar p.mu (evita
	// p.mu → owner.mu). fanout[i] é nil quando owner[i]/track[i] é nil.
	fanouts := make([]*fanout, len(owners))
	for i := range owners {
		if owners[i] != nil && tracks[i] != nil {
			fanouts[i] = owners[i].fanoutFor("audio", tracks[i])
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.audioSet = set
	for i, s := range p.audioSlots {
		var owner *Peer
		var track *webrtc.TrackRemote
		if i < len(owners) {
			owner = owners[i]
			track = tracks[i]
		}
		if s.owner == owner && s.src == track {
			continue
		}
		if owner == nil || track == nil || fanouts[i] == nil {
			s.release()
			continue
		}
		s.assignWithFanout(owner, "audio", track, fanouts[i])
	}
}

// sendPLI pede um keyframe ao publisher na troca de conteúdo de slot de vídeo
// (D7). O PLI vai na PC do publisher, para o SSRC da track.
func sendPLI(pc *webrtc.PeerConnection, track *webrtc.TrackRemote) {
	if pc == nil || track == nil {
		return
	}
	_ = pc.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())},
	})
}

// state retorna o estado de voz do peer (p/ voice_joined e voice_state_update).
func (p *Peer) state() models.VoiceState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return models.VoiceState{
		UserID:        p.userID,
		Muted:         p.muted,
		CameraOn:      p.cameraOn,
		ScreenSharing: p.screenSharing,
	}
}

// updateState atualiza o estado de voz (campos nil não mudam) e retorna o
// estado resultante.
func (p *Peer) updateState(muted, cameraOn, screenSharing *bool) models.VoiceState {
	p.mu.Lock()
	defer p.mu.Unlock()
	if muted != nil {
		p.muted = *muted
	}
	if cameraOn != nil {
		p.cameraOn = *cameraOn
	}
	if screenSharing != nil {
		p.screenSharing = *screenSharing
	}
	return models.VoiceState{
		UserID:        p.userID,
		Muted:         p.muted,
		CameraOn:      p.cameraOn,
		ScreenSharing: p.screenSharing,
	}
}

// close fecha a PeerConnection e para o worker de renegociação (idempotente).
func (p *Peer) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.renegStop)
	pc := p.pc
	p.pc = nil
	audioSSRC := p.audioSSRC
	p.audioSSRC = 0
	p.mu.Unlock()

	// Encerra os fanouts (readers únicos das tracks) ANTES de fechar a PC:
	// destrói os forwarders e libera os readers.
	p.stopAllFanouts()

	if audioSSRC != 0 {
		p.m.unregisterSSRC(audioSSRC)
	}
	if pc != nil {
		_ = pc.Close()
	}
}

// hasRole indica se o mapa mid→papel contém o papel.
func hasRole(roles map[string]trackRole, role trackRole) bool {
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

// intFromU16Ptr converte *uint16 → *int (para o shape do voice_ice_candidate).
func intFromU16Ptr(v *uint16) *int {
	if v == nil {
		return nil
	}
	i := int(*v)
	return &i
}

func cloneTrackRoles(src map[string]trackRole) map[string]trackRole {
	dst := make(map[string]trackRole, len(src))

	for mid, role := range src {
		dst[mid] = role
	}

	return dst
}
