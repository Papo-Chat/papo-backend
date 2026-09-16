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
	scoreDecayPerTick = 15.0
	scoreThreshold    = -95.0
	activeSpeakerTick = time.Second

	// Um candidato precisa estar pelo menos 6 dB acima
	// para substituir alguém que já possui slot.
	audioSwitchMarginDB = 6.0

	// Consideramos atividade recente por 300ms.
	audioActivityWindow = 300 * time.Millisecond

	// Depois que a atividade para, preserva o slot por mais 1.5s.
	audioHangover = 1500 * time.Millisecond

	// Somente usado para detectar atividade/hangover.
	// NÃO corta nem bloqueia áudio abaixo desse valor.
	audioActivityThreshold = -60.0
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

	// Último momento em que o usuário teve áudio significativo.
	lastAudioActivity map[string]time.Time

	tickerStop chan struct{}
	cleanup    *time.Timer
}

func newRoom(m *Manager, channelID string) *Room {
	r := &Room{
		m:                 m,
		channelID:         channelID,
		peers:             make(map[string]*Peer),
		scores:            make(map[string]float64),
		lastAudioActivity: make(map[string]time.Time),
		tickerStop:        make(chan struct{}),
	}
	go r.ticker()
	return r
}

type scoredCandidate struct {
	id    string
	score float64
}

type audioPeerView struct {
	peer    *Peer
	muted   bool
	track   *webrtc.TrackRemote
	current []string
}

func (r *Room) stableSelectLocked(
	candidates []scoredCandidate,
	current []string,
	now time.Time,
	k int,
) []string {
	if k <= 0 || len(candidates) == 0 {
		return nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}

		return candidates[i].id < candidates[j].id
	})

	scoreByID := make(map[string]float64, len(candidates))

	for _, c := range candidates {
		scoreByID[c.id] = c.score
	}

	selected := make([]string, 0, k)
	selectedSet := make(map[string]struct{}, k)

	// Primeiro preserva quem já estava selecionado.
	for _, id := range current {
		if len(selected) == k {
			break
		}

		if _, valid := scoreByID[id]; !valid {
			continue
		}

		if _, duplicate := selectedSet[id]; duplicate {
			continue
		}

		selected = append(selected, id)
		selectedSet[id] = struct{}{}
	}

	// Preenche slots vazios pelos melhores candidatos.
	for _, c := range candidates {
		if len(selected) == k {
			break
		}

		if _, exists := selectedSet[c.id]; exists {
			continue
		}

		selected = append(selected, c.id)
		selectedSet[c.id] = struct{}{}
	}

	// Challengers precisam vencer o incumbent por 6 dB.
	for _, challenger := range candidates {
		if _, selectedAlready := selectedSet[challenger.id]; selectedAlready {
			continue
		}

		weakestIdx := -1
		weakestScore := 0.0

		for i, incumbent := range selected {
			// Protege durante o hangover.
			if last, ok := r.lastAudioActivity[incumbent]; ok {
				age := now.Sub(last)

				inHangover :=
					age > audioActivityWindow &&
						age <= audioActivityWindow+audioHangover

				if inHangover {
					continue
				}
			}

			score := scoreByID[incumbent]

			if weakestIdx == -1 ||
				score < weakestScore ||
				(score == weakestScore &&
					incumbent > selected[weakestIdx]) {

				weakestIdx = i
				weakestScore = score
			}
		}

		// Todos os incumbents estão protegidos.
		if weakestIdx == -1 {
			continue
		}

		if challenger.score < weakestScore+audioSwitchMarginDB {
			// Os próximos têm score <= challenger.
			break
		}

		old := selected[weakestIdx]

		delete(selectedSet, old)

		selected[weakestIdx] = challenger.id
		selectedSet[challenger.id] = struct{}{}
	}

	// Importante: NÃO ordenar novamente.
	//
	// Isso mantém a posição dos active-speakers estável também,
	// evitando broadcasts só porque dois incumbents inverteram 1 dB.
	return selected
}

func (r *Room) audioViewsLocked() map[string]audioPeerView {
	views := make(map[string]audioPeerView, len(r.peers))

	for id, p := range r.peers {
		if p == nil {
			continue
		}

		muted, track, current := p.audioRoutingSnapshot()

		views[id] = audioPeerView{
			peer:    p,
			muted:   muted,
			track:   track,
			current: current,
		}
	}

	return views
}

func resolveAudioFromViews(
	set []string,
	views map[string]audioPeerView,
) ([]*Peer, []*webrtc.TrackRemote) {
	owners := make([]*Peer, len(set))
	tracks := make([]*webrtc.TrackRemote, len(set))

	for i, id := range set {
		view, ok := views[id]
		if !ok {
			continue
		}

		owners[i] = view.peer
		tracks[i] = view.track
	}

	return owners, tracks
}

func (r *Room) activeSpeakerCandidatesLocked() []scoredCandidate {
	candidates := make([]scoredCandidate, 0, len(r.scores))

	for id, score := range r.scores {
		if _, exists := r.peers[id]; !exists {
			continue
		}

		candidates = append(candidates, scoredCandidate{
			id:    id,
			score: score,
		})
	}

	return candidates
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

	now := time.Now()

	// Um snapshot de cada Peer por tick.
	views := r.audioViewsLocked()

	// ActiveSpeakerUpdate também usa hysteresis + hangover.
	roomTopK := r.stableSelectLocked(
		r.activeSpeakerCandidatesLocked(),
		r.lastTopK,
		now,
		r.m.cfg.VoiceAudioSlots,
	)

	type syncPair struct {
		peer   *Peer
		set    []string
		owners []*Peer
		tracks []*webrtc.TrackRemote
	}

	syncs := make([]syncPair, 0, len(views))

	for id, view := range views {
		set := r.audioSetLocked(
			id,
			view.current,
			now,
			views,
		)

		owners, tracks := resolveAudioFromViews(set, views)

		syncs = append(syncs, syncPair{
			peer:   view.peer,
			set:    set,
			owners: owners,
			tracks: tracks,
		})
	}

	topKChanged := !sameStringSlice(roomTopK, r.lastTopK)

	if topKChanged {
		r.lastTopK = roomTopK
	}

	// Decay vale para o próximo tick.
	for id, score := range r.scores {
		score -= scoreDecayPerTick

		if score < scoreThreshold {
			delete(r.scores, id)
		} else {
			r.scores[id] = score
		}
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

// audioSetLocked retorna os publishers que devem ser encaminhados para um
// subscriber.
//
// Se todos os publishers couberem nos slots, todos são encaminhados,
// independentemente do nível de áudio.
//
// Quando há mais publishers que slots, o audio-level é usado apenas para
// priorizar quem ocupa os slots.
//
// Deve ser chamado com r.mu segurado.
func (r *Room) audioSetLocked(
	excludeID string,
	current []string,
	now time.Time,
	views map[string]audioPeerView,
) []string {
	k := r.m.cfg.VoiceAudioSlots

	candidates := make([]scoredCandidate, 0, len(views))

	for id, view := range views {
		if id == excludeID {
			continue
		}

		if view.muted || view.track == nil {
			continue
		}

		// Quem não tem audio-level continua elegível,
		// mas perde prioridade se houver competição.
		score := -1000.0

		if s, ok := r.scores[id]; ok {
			score = s
		}

		candidates = append(candidates, scoredCandidate{
			id:    id,
			score: score,
		})
	}

	return r.stableSelectLocked(
		candidates,
		current,
		now,
		k,
	)
}

// noteAudioLevel registra o nível (dBFS) de um usuário no score da sala
// (chamado pelo interceptor de audio-level — active_speaker.go).
func (r *Room) noteAudioLevel(userID string, level float64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return
	}

	p := r.peers[userID]
	if p == nil || p.isMuted() {
		return
	}

	if level >= audioActivityThreshold {
		r.lastAudioActivity[userID] = time.Now()
	}

	if cur, ok := r.scores[userID]; !ok || level > cur {
		r.scores[userID] = level
	}
}

func (r *Room) rebindPublishedTrack(pub *Peer, kind string, track *webrtc.TrackRemote) {
	r.mu.Lock()
	subs := make([]*Peer, 0, len(r.peers))
	for _, sub := range r.peers {
		if sub != pub {
			subs = append(subs, sub)
		}
	}
	r.mu.Unlock()

	for _, sub := range subs {
		sub.rebindVideoSlot(pub, kind, track)
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

	peer := newPeer(r.m, r, userID, clientID)
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
func (r *Room) removePeer(peer *Peer) {
	if peer == nil {
		return
	}

	r.mu.Lock()

	current := r.peers[peer.userID]

	if current != peer {
		r.mu.Unlock()
		return
	}

	delete(r.peers, peer.userID)
	delete(r.scores, peer.userID)
	delete(r.lastAudioActivity, peer.userID)
	if len(r.peers) == 0 {
		r.scheduleCleanupLocked()
	}
	now := time.Now()
	views := r.audioViewsLocked()

	type syncPair struct {
		peer   *Peer
		set    []string
		owners []*Peer
		tracks []*webrtc.TrackRemote
	}

	syncs := make([]syncPair, 0, len(views))

	for id, view := range views {
		set := r.audioSetLocked(
			id,
			view.current,
			now,
			views,
		)

		owners, tracks := resolveAudioFromViews(set, views)

		syncs = append(syncs, syncPair{
			peer:   view.peer,
			set:    set,
			owners: owners,
			tracks: tracks,
		})
	}
	r.mu.Unlock()

	// Limpa o tracking de salas do usuário (VOICE_MAX_ROOMS_PER_USER). É aqui
	// — e não nos callers (Leave/UserOffline) — porque removePeer é o único
	// ponto por onde um peer sai da sala (saída explícita, WS offline, falha
	// de ICE/PC). Sem isso, uma falha de PC deixa entry stale em userRooms e
	// o usuário não consegue reentrar (ErrVoiceAlreadyInRoom).
	r.m.trackUserRoom(peer.userID, r.channelID, false)

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
		UserID:    peer.userID,
	})
}

func (r *Room) releasePublishedKind(pub *Peer, kind string) {
	r.mu.Lock()

	subs := make([]*Peer, 0, len(r.peers))
	for _, sub := range r.peers {
		if sub != pub {
			subs = append(subs, sub)
		}
	}

	r.mu.Unlock()

	for _, sub := range subs {
		sub.releaseFrom(pub, kind)
	}
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

	out := make([]string, 0, len(r.lastTopK))

	for _, id := range r.lastTopK {
		if _, exists := r.peers[id]; !exists {
			continue
		}

		if _, hasScore := r.scores[id]; !hasScore {
			continue
		}

		out = append(out, id)
	}

	return out
}

// setMuted atualiza o estado do mic e notifica os leitores do canal.
func (r *Room) setMuted(userID string, muted bool) {
	peer := r.peer(userID)
	if peer == nil {
		return
	}

	state := peer.updateState(&muted, nil, nil)

	if muted {
		r.mu.Lock()

		delete(r.scores, userID)
		delete(r.lastAudioActivity, userID)

		r.mu.Unlock()
	}

	r.broadcastState(state)
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
		// Ordem de locks: manager -> room. Join nunca segura ambos ao mesmo tempo.
		r.m.mu.Lock()
		r.mu.Lock()

		if cur := r.m.rooms[r.channelID]; cur != r || r.closed || len(r.peers) != 0 {
			r.mu.Unlock()
			r.m.mu.Unlock()
			return
		}

		// Verificação + fechamento + remoção precisam ser uma única transição.
		r.closed = true
		r.cleanup = nil
		close(r.tickerStop)
		delete(r.m.rooms, r.channelID)

		r.mu.Unlock()
		r.m.mu.Unlock()
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
