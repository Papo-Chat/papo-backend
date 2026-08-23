package websocket

import (
	"encoding/json"
	"sync"
	"time"

	"papo/internal/utils"
)

// Hub mantém os clientes WebSocket ativos em memória e o estado efêmero de
// presença (usuários online). Não persiste nada.
type Hub struct {
	mu         sync.RWMutex
	clients    map[*Client]struct{}
	presence   *PresenceStore
	register   chan *Client
	unregister chan *Client
	stop       chan struct{}

	// shuttingDown é marcado pelo Shutdown para rejeitar novos registros
	// e permitir o encerramento ordenado das conexões.
	shuttingDown bool
	stopOnce     sync.Once
}

// hub é o hub global de conexões WebSocket da aplicação.
var hub = NewHub()

// GetHub retorna o hub global de conexões WebSocket.
// Run deve estar ativo antes de aceitar conexões.
func GetHub() *Hub {
	return hub
}

// NewHub cria um Hub. Run deve ser iniciado em uma goroutine dedicada.
func NewHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]struct{}),
		presence:   NewPresenceStore(),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		stop:       make(chan struct{}),
	}
}

// Run processa o registro e o desregistro de clientes e mantém o estado de
// presença atualizado. Deve ser executado em sua própria goroutine e termina
// quando Shutdown fecha o canal stop.
func (h *Hub) Run() {
	for {
		select {
		case c := <-h.register:
			h.mu.Lock()
			if h.shuttingDown {
				h.mu.Unlock()
				// Conexão chegou durante o shutdown: encerra sem registrar.
				c.sendCloseFrame()
				c.conn.Close()
				continue
			}
			h.clients[c] = struct{}{}
			h.mu.Unlock()
			h.presenceOnline(c)
		case c := <-h.unregister:
			h.mu.Lock()
			removed := false
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				removed = true
			}
			h.mu.Unlock()
			// O fechamento do canal de envio e a liberação da presença
			// acontecem somente quando o cliente foi efetivamente
			// desregistrado (unregister duplicado é ignorado).
			if removed {
				c.closeSend()
				h.presenceOffline(c)
			}
		case <-h.stop:
			return
		}
	}
}

// Shutdown encerra todas as conexões ativas (com close frame) e espera o
// desregistro de cada uma antes de parar o Hub. Deve ser chamado uma única
// vez, no encerramento da aplicação.
func (h *Hub) Shutdown() {
	h.mu.Lock()
	h.shuttingDown = true
	clients := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()

	for _, c := range clients {
		c.sendCloseFrame()
		c.conn.Close()
	}

	// Cada ReadPump percebe a conexão fechada e desregistra o cliente;
	// espera o mapa esvaziar (com tempo máximo, para não travar o shutdown).
	deadline := time.Now().Add(5 * time.Second)
	for h.clientCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}

	h.stopOnce.Do(func() { close(h.stop) })
}

// clientCount retorna a quantidade de clientes registrados.
func (h *Hub) clientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Register enfileira um cliente para registro no Hub.
// Bloqueia até o Hub processar.
func (h *Hub) Register(c *Client) {
	h.register <- c
}

// Unregister enfileira um cliente para desregistro no Hub.
// O Hub fecha o canal de envio do cliente ao processar.
// Bloqueia até o Hub processar.
func (h *Hub) Unregister(c *Client) {
	h.unregister <- c
}

// Clients retorna uma snapshot dos clientes atualmente registrados.
func (h *Hub) Clients() map[*Client]struct{} {
	h.mu.RLock()
	defer h.mu.RUnlock()
	snapshot := make(map[*Client]struct{}, len(h.clients))
	for c := range h.clients {
		snapshot[c] = struct{}{}
	}
	return snapshot
}

// Broadcast serializa o evento uma única vez e o envia a todos os clientes
// conectados (eventos globais do backend: presença e canais). Para eventos
// privados por canal, use BroadcastToUsers com a autorização pré-computada.
// Se o buffer de envio de um cliente está cheio, a conexão dele é encerrada
// (mesma regra de Client.sendEvent).
func (h *Hub) Broadcast(event any) {
	data, err := json.Marshal(event)
	if err != nil {
		utils.Errorf("websocket: falha ao serializar evento de broadcast: %v", err)
		return
	}

	for c := range h.Clients() {
		c.sendRaw(data)
	}
}

// OnlineUserIDs retorna um snapshot com os ids de usuário distintos dos
// clientes registrados.
func (h *Hub) OnlineUserIDs() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	seen := make(map[string]struct{}, len(h.clients))
	for c := range h.clients {
		seen[c.userID] = struct{}{}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	return ids
}

// BroadcastToUsers serializa o evento uma única vez e o envia somente aos
// clientes cujo usuário está no conjunto allowed (autorização para escutar o
// evento). Os demais clientes não recebem o evento. Se o buffer de envio de
// um cliente está cheio, a conexão dele é encerrada (mesma regra de
// Client.sendEvent).
func (h *Hub) BroadcastToUsers(event any, allowed map[string]bool) {
	data, err := json.Marshal(event)
	if err != nil {
		utils.Errorf("websocket: falha ao serializar evento de broadcast: %v", err)
		return
	}

	for c := range h.Clients() {
		if !allowed[c.userID] {
			continue
		}
		c.sendRaw(data)
	}
}

// broadcastExcept serializa o evento uma única vez e o envia a todos os
// clientes registrados, exceto o informado.
func (h *Hub) broadcastExcept(event any, exclude *Client) {
	data, err := json.Marshal(event)
	if err != nil {
		utils.Errorf("websocket: falha ao serializar evento de broadcast: %v", err)
		return
	}

	for c := range h.Clients() {
		if c == exclude {
			continue
		}
		c.sendRaw(data)
	}
}

// presenceOnline registra a conexão do cliente na presença e envia a lista
// de membros online (presence_sync) ao cliente. Quando esta é a primeira
// conexão do usuário, também notifica os demais clientes (presence_update
// online), exceto o próprio cliente em conexão, que já recebe o estado
// completo no presence_sync.
func (h *Hub) presenceOnline(c *Client) {
	becameOnline := h.presence.AddConnection(c.userID, c.statusMessage)
	if becameOnline {
		h.broadcastExcept(PresenceUpdateOutbound{
			Type:          EventTypePresenceUpdate,
			UserID:        c.userID,
			Status:        StatusOnline,
			StatusMessage: h.presence.StatusMessage(c.userID),
		}, c)
	}

	c.sendEvent(PresenceSyncOutbound{
		Type:    EventTypePresenceSync,
		Members: h.presence.OnlineMembers(),
	})
}

// presenceOffline libera a conexão do cliente na presença. Quando esta era a
// última conexão do usuário, notifica os clientes (presence_update offline).
func (h *Hub) presenceOffline(c *Client) {
	if !h.presence.RemoveConnection(c.userID) {
		return
	}

	h.Broadcast(PresenceUpdateOutbound{
		Type:   EventTypePresenceUpdate,
		UserID: c.userID,
		Status: StatusOffline,
	})
}

// UpdateStatusMessage atualiza a mensagem de status de um usuário online e
// notifica os clientes (presence_update).
// Retorna false quando o usuário está offline (nada a atualizar ou notificar).
func (h *Hub) UpdateStatusMessage(userID string, statusMessage *string) bool {
	if !h.presence.SetStatusMessage(userID, statusMessage) {
		return false
	}

	h.Broadcast(PresenceUpdateOutbound{
		Type:          EventTypePresenceUpdate,
		UserID:        userID,
		Status:        StatusOnline,
		StatusMessage: h.presence.StatusMessage(userID),
	})
	return true
}
