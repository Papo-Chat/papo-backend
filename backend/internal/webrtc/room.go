package webrtc

import (
	"sort"
	"sync"
	"time"

	"papo/internal/models"
	"papo/internal/utils"

	"github.com/pion/webrtc/v4"
)

// Parâmetros do active speaker (D8): decaimento do score por tick (1s) e
// threshold mínimo de nível (dBFS) para permanecer no top-K. Score é o
// nível mais alto visto na janela (RFC 6464, dBFS = -level).
const (
	scoreDecayPerTick = 15.0 // dB por tick (1s): quem para de falar cai do top-K
	scoreThreshold    = -95.0
	activeSpeakerTick = time.Second
)

// Room é uma sala de voz efêmera em memória: peers (1 por usuário — D10),
// scores de active speaker (D8) e o grace period de destruição (D11).
type Room struct {
	m         *Manager
	channelID string

	mu     sync.Mutex
	peers  map[string]*Peer
	closed bool

	// active speakers (D8): score por usuário + top-K da sala (p/ broadcast).
	scores   map[string]float64
	lastTopK []string

	tickerStop chan struct{}
	cleanup    *time.Timer
}

func newRoom(m *Manager, channelID string) *Room {
	r := &Room{
		m:          m,
		channelID:  channelID,
		peers:      make(map[string]*Peer),
		scores:     make(map[string]float64),
		tickerStop: make(chan struct{}),
	}
	go r.ticker()
	return r
}

// ticker decaia os scores e sincroniza os slots de áudio dos subscribers a
// cada segundo (D8). Roda em goroutine própria por sala.
func (r *Room) ticker() {
	t := time.NewTicker(activeSpeakerTick)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.tick()
		case <-r.tickerStop:
			return
		}
	}
}

// tick decaia os scores, recalcula o top-K da sala e o top-K de cada
// subscriber (excluindo si mesmo) e sincroniza os slots de áudio.
func (r *Room) tick() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}

	for id, s := range r.scores {
		s -= scoreDecayPerTick
		if s < scoreThreshold {
			delete(r.scores, id)
		} else {
			r.scores[id] = s
		}
	}

	roomTopK := r.topKLocked("")
	type syncPair struct {
		peer   *Peer
		set    []string
		owners []*Peer
		tracks []*webrtc.TrackRemote
	}
	var syncs []syncPair
	for _, p := range r.peers {
		set := r.topKLocked(p.userID)
		owners, tracks := r.resolveAudioLocked(set)
		syncs = append(syncs, syncPair{peer: p, set: set, owners: owners, tracks: tracks})
	}
	topKChanged := !sameStringSlice(roomTopK, r.lastTopK)
	if topKChanged {
		r.lastTopK = roomTopK
	}
	r.mu.Unlock()

	for _, s := range syncs {
		s.peer.setAudioSet(s.set, s.owners, s.tracks)
	}
	if topKChanged {
		r.m.broadcastVoice(r.channelID, ActiveSpeakerUpdate{
			Type:      EventTypeActiveSpeakerUpdate,
			ChannelID: r.channelID,
			UserIDs:   roomTopK,
		})
	}
}

// resolveAudioLocked resolve os owners e as tracks de áudio de um top-K
// (chamado com r.mu segurado — r.mu → owner.p.mu é a ordem correta).
func (r *Room) resolveAudioLocked(set []string) ([]*Peer, []*webrtc.TrackRemote) {
	owners := make([]*Peer, len(set))
	tracks := make([]*webrtc.TrackRemote, len(set))
	for i, id := range set {
		owners[i] = r.peers[id]
		if owners[i] != nil {
			tracks[i] = owners[i].AudioTrack()
		}
	}
	return owners, tracks
}

// topKLocked retorna os K maiores scores da sala (excluindo excludeID),
// ordenados por nível (mais alto primeiro) com tiebreak determinístico por
// userID. Deve ser chamado com r.mu segurado.
func (r *Room) topKLocked(excludeID string) []string {
	type entry struct {
		id    string
		score float64
	}
	entries := make([]entry, 0, len(r.scores))
	for id, s := range r.scores {
		if id == excludeID {
			continue
		}
		entries = append(entries, entry{id: id, score: s})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].score != entries[j].score {
			return entries[i].score > entries[j].score
		}
		return entries[i].id < entries[j].id
	})
	k := r.m.cfg.VoiceAudioSlots
	if len(entries) > k {
		entries = entries[:k]
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.id)
	}
	return out
}

// noteAudioLevel registra o nível (dBFS) de um usuário no score da sala
// (chamado pelo interceptor de audio-level — active_speaker.go).
func (r *Room) noteAudioLevel(userID string, level float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if cur, ok := r.scores[userID]; !ok || level > cur {
		r.scores[userID] = level
	}
}

// addPeer adiciona o usuário à sala (cria o peer sem PC — a PeerConnection
// só existe após o voice_offer). Enforce de VOICE_MAX_ROOM_PEERS. clientID é a
// conexão que pediu o join (o voice_joined vai só para ela).
func (r *Room) addPeer(userID, clientID string) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrVoiceRoomClosed
	}
	if _, ok := r.peers[userID]; ok {
		r.mu.Unlock()
		return ErrVoiceAlreadyInRoom
	}
	if len(r.peers) >= r.m.cfg.VoiceMaxRoomPeers {
		r.mu.Unlock()
		return ErrVoiceRoomFull
	}

	peer := newPeer(r.m, r, userID)
	r.peers[userID] = peer
	r.mu.Unlock()

	r.cancelCleanup()

	// Estado inicial ao late joiner (unicast à conexão que pediu o join) +
	// notifica os demais leitores.
	members := r.members()
	if r.m.signaler.SendToClient != nil {
		r.m.signaler.SendToClient(clientID, VoiceJoined{
			Type:           EventTypeVoiceJoined,
			ChannelID:      r.channelID,
			Members:        members,
			ActiveSpeakers: r.currentTopK(),
		})
	}
	r.m.broadcastVoice(r.channelID, VoiceStateUpdate{
		Type:          EventTypeVoiceStateUpdate,
		ChannelID:     r.channelID,
		UserID:        userID,
		Muted:         true,
		CameraOn:      false,
		ScreenSharing: false,
	})
	return nil
}

// removePeer remove o usuário da sala: libera os slots que ele ocupava nos
// demais subscribers (vídeo + áudio), fecha a PeerConnection e notifica os
// leitores (voice_leave). Se a sala fica vazia, agenda a destruição (D11).
func (r *Room) removePeer(userID string) {
	r.mu.Lock()
	peer := r.peers[userID]
	if peer == nil {
		r.mu.Unlock()
		return
	}
	delete(r.peers, userID)
	delete(r.scores, userID)
	if len(r.peers) == 0 {
		r.scheduleCleanupLocked()
	}
	type syncPair struct {
		peer   *Peer
		set    []string
		owners []*Peer
		tracks []*webrtc.TrackRemote
	}
	var syncs []syncPair
	for _, p := range r.peers {
		set := r.topKLocked(p.userID)
		owners, tracks := r.resolveAudioLocked(set)
		syncs = append(syncs, syncPair{peer: p, set: set, owners: owners, tracks: tracks})
	}
	r.mu.Unlock()

	// Limpa o tracking de salas do usuário (VOICE_MAX_ROOMS_PER_USER). É aqui
	// — e não nos callers (Leave/UserOffline) — porque removePeer é o único
	// ponto por onde um peer sai da sala (saída explícita, WS offline, falha
	// de ICE/PC). Sem isso, uma falha de PC deixa entry stale em userRooms e
	// o usuário não consegue reentrar (ErrVoiceAlreadyInRoom).
	r.m.trackUserRoom(userID, r.channelID, false)

	// Libera os slots de vídeo que o peer publicava nos demais subscribers e
	// resincroniza o áudio (o top-K já não inclui o peer que saiu).
	for _, s := range syncs {
		s.peer.releaseAllFrom(peer)
		s.peer.setAudioSet(s.set, s.owners, s.tracks)
	}
	peer.close()

	r.m.broadcastVoice(r.channelID, VoiceLeave{
		Type:      EventTypeVoiceLeave,
		ChannelID: r.channelID,
		UserID:    userID,
	})
}

// hasPeer indica se o usuário está na sala.
func (r *Room) hasPeer(userID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.peers[userID]
	return ok
}

// peer retorna o peer do usuário (nil quando não está na sala).
func (r *Room) peer(userID string) *Peer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peers[userID]
}

// peerCount retorna a quantidade de peers na sala.
func (r *Room) peerCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.peers)
}

// members retorna o estado de voz dos membros (p/ voice_joined e logs).
func (r *Room) members() []models.VoiceState {
	r.mu.Lock()
	defer r.mu.Unlock()
	states := make([]models.VoiceState, 0, len(r.peers))
	for _, p := range r.peers {
		states = append(states, p.state())
	}
	sort.Slice(states, func(i, j int) bool { return states[i].UserID < states[j].UserID })
	return states
}

// currentTopK retorna o top-K atual da sala (para o voice_joined).
func (r *Room) currentTopK() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.topKLocked("")
}

// setMuted atualiza o estado do mic e notifica os leitores do canal.
func (r *Room) setMuted(userID string, muted bool) {
	peer := r.peer(userID)
	if peer == nil {
		return
	}
	r.broadcastState(peer.updateState(&muted, nil, nil))
}

// setCameraOn atualiza o estado da câmera e notifica os leitores do canal.
func (r *Room) setCameraOn(userID string, on bool) {
	peer := r.peer(userID)
	if peer == nil {
		return
	}
	r.broadcastState(peer.updateState(nil, &on, nil))
}

// setScreenSharing atualiza o estado de screen share e notifica os leitores
// do canal (a track nova/removida chega na renegociação do publisher).
func (r *Room) setScreenSharing(userID string, on bool) {
	peer := r.peer(userID)
	if peer == nil {
		return
	}
	r.broadcastState(peer.updateState(nil, nil, &on))
}

// broadcastState distribui o estado de voz de um usuário aos leitores do
// canal de voz.
func (r *Room) broadcastState(state models.VoiceState) {
	r.m.broadcastVoice(r.channelID, VoiceStateUpdate{
		Type:          EventTypeVoiceStateUpdate,
		ChannelID:     r.channelID,
		UserID:        state.UserID,
		Muted:         state.Muted,
		CameraOn:      state.CameraOn,
		ScreenSharing: state.ScreenSharing,
	})
}

// destroy encerra a sala (fecha as PCs, para o ticker e o grace period) sem
// notificar — usado quando a sala está vazia ou no shutdown.
func (r *Room) destroy() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	if r.cleanup != nil {
		r.cleanup.Stop()
		r.cleanup = nil
	}
	close(r.tickerStop)
	peers := make([]*Peer, 0, len(r.peers))
	for _, p := range r.peers {
		peers = append(peers, p)
	}
	r.peers = make(map[string]*Peer)
	r.mu.Unlock()

	for _, p := range peers {
		r.m.trackUserRoom(p.userID, r.channelID, false)
		p.close()
	}
}

// destroyWithClosedNotice encerra a sala notificaando os membros (error
// voice-room-closed + voice_leave) — usado quando o canal de voz é excluído.
func (r *Room) destroyWithClosedNotice() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	if r.cleanup != nil {
		r.cleanup.Stop()
		r.cleanup = nil
	}
	close(r.tickerStop)
	peers := make([]*Peer, 0, len(r.peers))
	for _, p := range r.peers {
		peers = append(peers, p)
	}
	r.peers = make(map[string]*Peer)
	r.mu.Unlock()

	allowed := r.m.audience(r.channelID)
	for _, p := range peers {
		r.m.signaler.SendToUser(p.userID, VoiceError{
			Type:    "error",
			Message: "sala de voz encerrada",
			Code:    CodeVoiceRoomClosed,
		})
		r.m.broadcastTo(allowed, VoiceLeave{
			Type:      EventTypeVoiceLeave,
			ChannelID: r.channelID,
			UserID:    p.userID,
		})
		r.m.trackUserRoom(p.userID, r.channelID, false)
		p.close()
	}
}

// scheduleCleanup agenda a destruição da sala vazia após o grace period
// (D11). Chamado com r.mu segurado.
func (r *Room) scheduleCleanupLocked() {
	if r.cleanup != nil {
		r.cleanup.Stop()
	}
	grace := r.m.GracePeriod()
	r.cleanup = time.AfterFunc(grace, func() {
		r.m.mu.Lock()
		if cur := r.m.rooms[r.channelID]; cur != r {
			// Sala substituída (novo join) ou removida: nada a fazer.
			r.m.mu.Unlock()
			return
		}
		r.m.mu.Unlock()
		r.mu.Lock()
		empty := len(r.peers) == 0 && !r.closed
		r.mu.Unlock()
		if !empty {
			return
		}
		r.m.removeRoom(r.channelID)
		r.destroy()
		utils.Infof("webrtc: sala de voz %s destruída (vazia após %s)", r.channelID, grace)
	})
}

// cancelCleanup cancela a destruição agendada (novo join).
func (r *Room) cancelCleanup() {
	r.mu.Lock()
	if r.cleanup != nil {
		r.cleanup.Stop()
		r.cleanup = nil
	}
	r.mu.Unlock()
}

// sameStringSlice compara duas slices de strings por conteúdo e ordem.
func sameStringSlice(a, b []string) bool {
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
