package webrtc

import (
	"fmt"
	"sync"
	"time"

	"papo/internal/models"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// trackRole classifica as m-lines de vídeo/áudio do offer do cliente. A
// primeira m-line de vídeo é a câmera; a segunda (quando existe) é o screen
// share (seção 5.6 — determinístico, sem depender de label). O mesmo vale
// para áudio: a primeira publicação é o microfone e a segunda, durante a
// transmissão, é o áudio da tela.
type trackRole int

const (
	roleAudio trackRole = iota
	roleCamera
	roleScreen
	roleScreenAudio
)

// isVideoKind indica os kinds que carregam vídeo (precisam de keyframe/PLI).
func isVideoKind(kind string) bool {
	return kind == "video" || kind == "screen"
}

// Peer é a PeerConnection de um usuário em uma sala (D10: 1 peer por usuário
// por sala, não por conexão WS). A PC só existe após o primeiro voice_offer.
//
// Disciplina de lock: p.mu protege o estado do peer (tracks, slots, estado de
// voz, pc). NUNCA segurar p.mu durante chamadas de PC do pion
// (SetRemoteDescription/CreateAnswer/SetLocalDescription/AddTransceiverFromTrack)
// nem durante chamadas do signaler (SendToUser/BroadcastToUsers) — essas são
// feitas com p.mu solto. O worker de renegociação é sequencial por PC (D4), o
// que serializa o signaling sem p.mu.
type pendingVideoSubscription struct {
	pub  *Peer
	kind string
}

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

	// Áudio da tela: faixa própria, separada do microfone. Não entra no
	// active speaker (o SSRC não é registrado) e não é afetada pelo mute.
	screenAudioTrack *webrtc.TrackRemote

	// fanouts: 1 reader por track recebida (kind → fanout). Os subscribers
	// forwardam via o fanout do publisher (nunca chamam ReadRTP diretamente).
	fanouts map[string]*fanout

	// Identidade persistente do MID. Uma vez que um MID vira camera/screen/audio,
	// ele mantém esse papel durante toda a vida da PeerConnection.
	midRole map[string]trackRole

	// MIDs que estão efetivamente publicando no offer atual.
	activeMidRole map[string]trackRole

	// MID ao qual cada TrackRemote armazenada pertence.
	audioMID       string
	videoMID       string
	screenMID      string
	screenAudioMID string

	// slots de envio (servidor → cliente): N vídeo + K áudio (D5) + S áudio
	// de tela. Os slots de áudio de tela só são ocupados por assinatura
	// explícita (track_subscribe kind=screen_audio), como os de vídeo.
	videoSlots       []*slot
	audioSlots       []*slot
	screenAudioSlots []*slot

	// slotsReady vira true quando allocateSlots publicou os slots. Não dá
	// para inferir isso pelo tamanho dos pools: um pool pode ter tamanho 0.
	slotsReady bool

	// Subscribes podem chegar entre voice_joined e o primeiro voice_answer,
	// antes de allocateSlots publicar os slots. Mantemos esses pedidos
	// pendentes para eliminar a corrida de late join.
	pendingVideoSubs map[string]pendingVideoSubscription

	// Estado publicado para a UI. cameraOn/screenSharing só viram true quando
	// a TrackRemote correspondente já existe no SFU; isso impede subscribers
	// de correrem na frente da renegociação e receberem voice-not-found.
	muted         bool
	cameraOn      bool
	screenSharing bool
	screenAudio   bool

	// Intenção enviada pelo cliente ANTES do voice_offer. É separada do estado
	// publicado para que o backend consiga classificar os MIDs sem anunciar
	// mídia que ainda não chegou ao SFU.
	cameraIntent bool
	screenIntent bool

	audioSet []string // top-K de áudio atual (p/ detecção de mudança)

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

		muted: true,

		fanouts:          make(map[string]*fanout),
		pendingVideoSubs: make(map[string]pendingVideoSubscription),
		midRole:          make(map[string]trackRole),
		activeMidRole: make(map[string]trackRole),
		renegQueue:    make(chan func() error, 64),
		renegStop:     make(chan struct{}),
	}
	go p.renegWorker()
	return p
}

func (p *Peer) isMuted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.muted
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

func (p *Peer) audioRoutingSnapshot() (
	muted bool,
	track *webrtc.TrackRemote,
	set []string,
) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return true, nil, nil
	}

	// O áudio da tela tem faixa própria; o mute vale para o microfone mesmo
	// durante a transmissão.
	return p.muted,
		p.trackOfKindLocked("audio"),
		append([]string(nil), p.audioSet...)
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
func (p *Peer) handleOffer(
	clientID,
	sdp string,
	cameraOn,
	screenOn bool,
) error {
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

	firstOffer := !p.slotsReady

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

	screenAudioStopped := hasRole(previousActive, roleScreenAudio) &&
		!hasRole(activeRoles, roleScreenAudio)

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
		p.room.setPublishedKind(p, "video", false)
	}

	if screenStopped {
		p.room.releasePublishedKind(p, "screen")
		p.room.setPublishedKind(p, "screen", false)
	}

	if screenAudioStopped {
		p.room.releasePublishedKind(p, "screen_audio")
		p.room.setPublishedKind(p, "screen_audio", false)
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

	case roleScreenAudio:
		oldFanout = p.fanouts["screen_audio"]

		// O SSRC NÃO é registrado no active speaker: som da transmissão não
		// conta como o usuário falando.
		p.screenAudioTrack = track
		p.screenAudioMID = mid
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
		// Só anuncia camera_on depois que a track está armazenada e portanto
		// Subscribe(video) já pode passar em hasActiveTrack.
		p.room.setPublishedKind(p, "video", true)
		p.room.rebindPublishedTrack(p, "video", track)
	case roleScreen:
		// Mesmo contrato para screen share: o evento de estado passa a ser um
		// sinal de "track pronta", não apenas intenção de publicar.
		p.room.setPublishedKind(p, "screen", true)
		p.room.rebindPublishedTrack(p, "screen", track)
	case roleScreenAudio:
		// O reader começa já: sem ele os pacotes ficariam acumulando no
		// receiver enquanto ninguém assina o áudio da transmissão.
		_ = p.fanoutFor("screen_audio", track)
		p.room.setPublishedKind(p, "screen_audio", true)
		p.room.rebindPublishedTrack(p, "screen_audio", track)
	}
}

// slotPoolLocked retorna o pool de slots usado para o kind assinável: vídeo e
// screen dividem os slots de vídeo; o áudio da tela tem os seus. O caller
// segura p.mu.
func (p *Peer) slotPoolLocked(kind string) []*slot {
	if kind == "screen_audio" {
		return p.screenAudioSlots
	}
	return p.videoSlots
}

// rebindSlot troca a track de origem dos slots que já forwardam (pub, kind),
// quando o publisher republica a track em uma renegociação.
func (p *Peer) rebindSlot(pub *Peer, kind string, track *webrtc.TrackRemote) {
	p.mu.Lock()
	hasSlot := false
	for _, s := range p.slotPoolLocked(kind) {
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
	for _, s := range p.slotPoolLocked(kind) {
		if s.owner == pub && s.kind == kind && s.src != track {
			s.assignWithFanout(pub, kind, track, fanout)
			rebound = true
		}
	}
	p.mu.Unlock()

	if rebound && isVideoKind(kind) {
		sendPLIBurst(pub.PC(), track)
	}
}

// allocateSlots cria os slots de envio (N vídeo + K áudio + S áudio de tela)
// com tracks placeholder (D5). A ordem importa: o AddTrack do pion reaproveita
// os transceivers na ordem das m-lines do offer, então os K primeiros
// transceivers de áudio do cliente ficam com o microfone dos participantes e
// os S seguintes com o áudio das transmissões. Cliente que não oferece os S
// extras entra normalmente e apenas não recebe esse áudio. As chamadas AddTransceiverFromTrack são feitas SEM p.mu;
// as slices de slots são publicadas sob p.mu ao final.
func (p *Peer) allocateSlots() error {
	pc := p.PC()
	if pc == nil {
		return ErrVoiceNotFound
	}
	n := p.m.cfg.VoiceVideoSlots
	k := p.m.cfg.VoiceAudioSlots
	sa := p.m.cfg.VoiceScreenAudioSlots
	videoCodec := p.m.api.videoCodec.RTPCodecCapability
	audioCodec := webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	}

	videoSlots := make([]*slot, 0, n)
	for i := 0; i < n; i++ {
		track, err := webrtc.NewTrackLocalStaticRTP(
			videoCodec,
			fmt.Sprintf("papo-video-%d", i),
			"papo-video",
		)
		if err != nil {
			return err
		}

		sender, err := pc.AddTrack(track)
		if err != nil {
			return err
		}

		s := &slot{
			peer:   p,
			sender: sender,
			local:  track,
		}
		s.startRTCPFeedback()
		videoSlots = append(videoSlots, s)
	}

	audioSlots := make([]*slot, 0, k)
	for i := 0; i < k; i++ {
		track, err := webrtc.NewTrackLocalStaticRTP(
			audioCodec,
			fmt.Sprintf("papo-audio-%d", i),
			"papo-audio",
		)
		if err != nil {
			return err
		}

		sender, err := pc.AddTrack(track)
		if err != nil {
			return err
		}

		audioSlots = append(audioSlots, &slot{
			peer:   p,
			sender: sender,
			local:  track,
		})
	}

	screenAudioSlots := make([]*slot, 0, sa)
	for i := 0; i < sa; i++ {
		track, err := webrtc.NewTrackLocalStaticRTP(
			audioCodec,
			fmt.Sprintf("papo-screen-audio-%d", i),
			"papo-screen-audio",
		)
		if err != nil {
			return err
		}

		sender, err := pc.AddTrack(track)
		if err != nil {
			return err
		}

		screenAudioSlots = append(screenAudioSlots, &slot{
			peer:   p,
			sender: sender,
			local:  track,
		})
	}

	p.mu.Lock()
	p.videoSlots = videoSlots
	p.audioSlots = audioSlots
	p.screenAudioSlots = screenAudioSlots
	p.slotsReady = true

	pending := make([]pendingVideoSubscription, 0, len(p.pendingVideoSubs))
	for _, sub := range p.pendingVideoSubs {
		pending = append(pending, sub)
	}
	p.pendingVideoSubs = make(map[string]pendingVideoSubscription)
	p.mu.Unlock()

	// O primeiro answer já pode ser emitido depois disso; processar aqui garante
	// que subscribes recebidos cedo não sejam perdidos.
	for _, sub := range pending {
		if err := p.assignSlot(sub.pub, sub.kind); err != nil {
			p.mu.Lock()
			clientID := p.signalingClient
			p.mu.Unlock()
			p.m.sendErrorToClient(clientID, err)
		}
	}

	return nil
}

// trackOfKind retorna a track recebida do peer para o kind
// (video/screen/audio/screen_audio).
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

	case "screen_audio":
		if p.screenAudioTrack == nil {
			return nil
		}
		if p.activeMidRole[p.screenAudioMID] != roleScreenAudio {
			return nil
		}
		return p.screenAudioTrack

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

	// Áudio precisa ser lido mesmo sem subscribers para alimentar active
	// speaker; o áudio da tela também começa já, para não acumular pacotes
	// enquanto ninguém assina. Vídeo/screen ficam lazy até assignWithFanout(),
	// para não consumir o keyframe inicial antes do subscriber estar conectado.
	if !isVideoKind(kind) {
		f.start()
	}

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

func videoSubscriptionKey(pub *Peer, kind string) string {
	if pub == nil {
		return ""
	}
	return pub.userID + ":" + kind
}

// subscribeTrack assina uma track explícita do publisher (video, screen ou
// screen_audio). Aceita também o período entre voice_joined e allocateSlots:
// sem isso, "slots ainda não criados" era interpretado como "sala cheia".
func (p *Peer) subscribeTrack(pub *Peer, kind string) error {
	if pub == nil {
		return ErrVoiceNotFound
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrVoiceRoomClosed
	}
	if !p.slotsReady {
		p.pendingVideoSubs[videoSubscriptionKey(pub, kind)] = pendingVideoSubscription{
			pub:  pub,
			kind: kind,
		}
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	return p.assignSlot(pub, kind)
}

// assignSlot faz o subscriber começar a receber a track do publisher em um
// slot do pool do kind (D5). Sem slot livre → ErrVoiceRoomFull.
func (p *Peer) assignSlot(pub *Peer, kind string) error {
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

	beforeRoutes := p.audioRoutesLocked()

	slot := p.findSlotFor(pub, kind)
	if slot == nil {
		p.mu.Unlock()
		return ErrVoiceRoomFull
	}

	slot.assignWithFanout(pub, kind, track, fanout)

	afterRoutes := p.audioRoutesLocked()
	clientID := p.signalingClient

	p.mu.Unlock()

	// Só o áudio da tela aparece nas rotas; para vídeo o snapshot não muda.
	if !sameAudioRoutes(beforeRoutes, afterRoutes) {
		p.sendAudioRoutes(clientID, afterRoutes)
	}

	if isVideoKind(kind) {
		// Pion chamada com p.mu solto. Um pequeno burst melhora a recuperação
		// quando o primeiro PLI coincide com a ativação do encoder/browser.
		sendPLIBurst(pubPC, track)
	}

	return nil
}

// findSlotFor retorna o slot para (owner, kind) no pool do kind: o slot já
// ocupado por (owner, kind) [reassign] ou um slot livre. Nil quando não há (o
// caller segura peer.mu).
func (p *Peer) findSlotFor(owner *Peer, kind string) *slot {
	pool := p.slotPoolLocked(kind)
	for _, s := range pool {
		if s.owner == owner && s.kind == kind {
			return s
		}
	}
	for _, s := range pool {
		if s.owner == nil {
			return s
		}
	}
	return nil
}

// releaseSlot para de forwardar a track do publisher (o slot fica vazio).
func (p *Peer) releaseSlot(pubID, kind string) {
	pub := p.room.peer(pubID) // resolve ANTES de p.mu (evita p.mu → r.mu)

	p.mu.Lock()
	before := p.audioRoutesLocked()
	if pub != nil {
		delete(p.pendingVideoSubs, videoSubscriptionKey(pub, kind))
		for _, s := range p.slotPoolLocked(kind) {
			if s.owner == pub && s.kind == kind {
				s.release()
			}
		}
	}
	after := p.audioRoutesLocked()
	clientID := p.signalingClient
	p.mu.Unlock()

	if !sameAudioRoutes(before, after) {
		p.sendAudioRoutes(clientID, after)
	}
}

// audioRoutesLocked lista as rotas de áudio do subscriber: os slots de
// microfone (kind "audio") e os de áudio de transmissão (kind
// "screen_audio"). O caller segura p.mu.
func (p *Peer) audioRoutesLocked() []VoiceAudioRoute {
	routes := make([]VoiceAudioRoute, 0, len(p.audioSlots)+len(p.screenAudioSlots))
	for _, s := range p.audioSlots {
		if s == nil || s.local == nil || s.owner == nil {
			continue
		}
		routes = append(routes, VoiceAudioRoute{
			TrackID: s.local.ID(),
			UserID:  s.owner.userID,
			Kind:    "audio",
		})
	}
	for _, s := range p.screenAudioSlots {
		if s == nil || s.local == nil || s.owner == nil {
			continue
		}
		routes = append(routes, VoiceAudioRoute{
			TrackID: s.local.ID(),
			UserID:  s.owner.userID,
			Kind:    "screen_audio",
		})
	}
	return routes
}

func sameAudioRoutes(a, b []VoiceAudioRoute) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (p *Peer) sendAudioRoutes(clientID string, routes []VoiceAudioRoute) {
	if clientID == "" || p.m.signaler.SendToClient == nil {
		return
	}
	p.m.signaler.SendToClient(clientID, VoiceAudioRoutes{
		Type:      EventTypeVoiceAudioRoutes,
		ChannelID: p.room.channelID,
		Routes:    routes,
	})
}

// releaseAllFrom libera todos os slots do subscriber que forwardam do
// publisher (vídeo + áudio) — usado quando o publisher sai da sala.
func (p *Peer) releaseAllFrom(pub *Peer) {
	if pub == nil {
		return
	}
	p.mu.Lock()
	before := p.audioRoutesLocked()
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
	for _, s := range p.screenAudioSlots {
		if s.owner == pub {
			s.release()
		}
	}
	after := p.audioRoutesLocked()
	clientID := p.signalingClient
	p.mu.Unlock()

	if !sameAudioRoutes(before, after) {
		p.sendAudioRoutes(clientID, after)
	}
}

// releaseFrom libera os slots do subscriber que forwardam (pub, kind) — usado
// quando o publisher para de publicar aquele kind.
func (p *Peer) releaseFrom(pub *Peer, kind string) {
	if pub == nil {
		return
	}

	p.mu.Lock()
	before := p.audioRoutesLocked()
	delete(p.pendingVideoSubs, videoSubscriptionKey(pub, kind))
	for _, s := range p.slotPoolLocked(kind) {
		if s.owner == pub && s.kind == kind {
			s.release()
		}
	}
	after := p.audioRoutesLocked()
	clientID := p.signalingClient
	p.mu.Unlock()

	if !sameAudioRoutes(before, after) {
		p.sendAudioRoutes(clientID, after)
	}
}

// setAudioSet atualiza o conjunto de áudio do subscriber.
//
// Publishers que continuam no conjunto permanecem no mesmo slot.
// Uma mudança na ordem do ranking não deve reassociar todos os slots.
func (p *Peer) setAudioSet(
	set []string,
	owners []*Peer,
	tracks []*webrtc.TrackRemote,
) {
	// Resolve fanouts antes de pegar p.mu para evitar:
	// subscriber.mu -> publisher.mu
	fanouts := make([]*fanout, len(owners))

	for i := range owners {
		if i >= len(tracks) {
			continue
		}

		if owners[i] == nil || tracks[i] == nil {
			continue
		}

		fanouts[i] = owners[i].fanoutFor("audio", tracks[i])
	}

	p.mu.Lock()

	if p.closed {
		p.mu.Unlock()
		return
	}
	beforeRoutes := p.audioRoutesLocked()

	// Não mantenha referência à slice do caller.
	p.audioSet = append(p.audioSet[:0], set...)

	// Publisher -> índice no conjunto desejado.
	desired := make(map[*Peer]int, len(owners))

	for i, owner := range owners {
		if owner == nil {
			continue
		}

		if i >= len(tracks) {
			continue
		}

		if tracks[i] == nil || fanouts[i] == nil {
			continue
		}

		desired[owner] = i
	}

	assigned := make(map[*Peer]bool, len(desired))

	// Primeiro preserva os slots atuais.
	for _, s := range p.audioSlots {
		if s.owner == nil {
			continue
		}

		owner := s.owner

		idx, wanted := desired[owner]
		if !wanted {
			s.release()
			continue
		}

		// Segurança contra slot duplicado para o mesmo publisher.
		if assigned[owner] {
			s.release()
			continue
		}

		assigned[owner] = true

		// Mesmo publisher e mesma track: não mexe no slot.
		if s.src == tracks[idx] {
			continue
		}

		// Publisher permanece, mas a TrackRemote mudou.
		s.assignWithFanout(
			owner,
			"audio",
			tracks[idx],
			fanouts[idx],
		)
	}

	// Agora aloca apenas publishers novos.
	for i, owner := range owners {
		if owner == nil {
			continue
		}

		if i >= len(tracks) {
			continue
		}

		if tracks[i] == nil || fanouts[i] == nil {
			continue
		}

		if assigned[owner] {
			continue
		}

		var free *slot

		for _, s := range p.audioSlots {
			if s.owner == nil {
				free = s
				break
			}
		}

		if free == nil {
			break
		}

		free.assignWithFanout(
			owner,
			"audio",
			tracks[i],
			fanouts[i],
		)

		assigned[owner] = true
	}

	afterRoutes := p.audioRoutesLocked()
	clientID := p.signalingClient
	p.mu.Unlock()

	if !sameAudioRoutes(beforeRoutes, afterRoutes) {
		p.sendAudioRoutes(clientID, afterRoutes)
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

func (p *Peer) requestKeyframe(track *webrtc.TrackRemote) {
	if p == nil || track == nil {
		return
	}

	pc := p.PC()
	if pc == nil {
		return
	}

	sendPLI(pc, track)
}

func sendPLIBurst(pc *webrtc.PeerConnection, track *webrtc.TrackRemote) {
	sendPLI(pc, track)
	if pc == nil || track == nil {
		return
	}
	go func() {
		for _, delay := range []time.Duration{250 * time.Millisecond, time.Second} {
			time.Sleep(delay)
			if pc.ConnectionState() == webrtc.PeerConnectionStateClosed ||
				pc.ConnectionState() == webrtc.PeerConnectionStateFailed {
				return
			}
			sendPLI(pc, track)
		}
	}()
}

func (p *Peer) mediaIntent() (cameraOn, screenOn bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.cameraIntent, p.screenIntent
}

func (p *Peer) updateMediaIntent(cameraOn, screenSharing *bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if cameraOn != nil {
		p.cameraIntent = *cameraOn
	}
	if screenSharing != nil {
		p.screenIntent = *screenSharing
	}
}

func (p *Peer) updatePublishedMedia(
	cameraOn,
	screenSharing,
	screenAudio *bool,
) (models.VoiceState, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	changed := false
	if cameraOn != nil && p.cameraOn != *cameraOn {
		p.cameraOn = *cameraOn
		changed = true
	}
	if screenSharing != nil && p.screenSharing != *screenSharing {
		p.screenSharing = *screenSharing
		changed = true
	}
	if screenAudio != nil && p.screenAudio != *screenAudio {
		p.screenAudio = *screenAudio
		changed = true
	}

	return p.stateLocked(), changed
}

// stateLocked monta o estado de voz do peer (o caller segura p.mu).
func (p *Peer) stateLocked() models.VoiceState {
	return models.VoiceState{
		UserID:        p.userID,
		Muted:         p.muted,
		CameraOn:      p.cameraOn,
		ScreenSharing: p.screenSharing,
		ScreenAudio:   p.screenAudio,
	}
}

// state retorna o estado de voz do peer (p/ voice_joined e voice_state_update).
func (p *Peer) state() models.VoiceState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stateLocked()
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
	return p.stateLocked()
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
