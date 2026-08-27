package websocket

import (
	"sort"
	"sync"
)

// Statuses de presença. online/offline são efêmeros (conexão ativa);
// away/busy são persistidos pelo usuário (users.status) e valem enquanto o
// usuário está online.
const (
	StatusOnline  = "online"
	StatusOffline = "offline"
	StatusAway    = "away"
	StatusBusy    = "busy"
)

// PresenceMember é uma entrada da lista de membros online enviada no
// presence_sync.
type PresenceMember struct {
	UserID        string  `json:"user_id"`
	Status        string  `json:"status"`
	StatusMessage *string `json:"status_message,omitempty"`
}

// PresenceStore é o estado efêmero dos usuários online, mantido apenas em
// memória. Um usuário é considerado online enquanto possuir pelo menos uma
// conexão ativa: múltiplas conexões do mesmo usuário são suportadas e o
// usuário só fica offline quando a última conexão é encerrada.
type PresenceStore struct {
	mu    sync.RWMutex
	users map[string]*presenceEntry
}

// presenceEntry agrega as conexões ativas de um usuário online.
type presenceEntry struct {
	connections   int
	statusMessage *string
	nickname      *string
	persisted     *string
}

// NewPresenceStore cria um PresenceStore vazio.
func NewPresenceStore() *PresenceStore {
	return &PresenceStore{users: make(map[string]*presenceEntry)}
}

// AddConnection registra uma conexão ativa do usuário.
// statusMessage, nickname e persisted (status away/busy persistido) são
// usados apenas na primeira conexão (criação da entrada); as demais conexões
// não as alteram (a atualização em runtime é feita por SetStatusMessage,
// SetNickname e SetPersistedStatus).
// Retorna true quando o usuário transiciona de offline para online.
func (p *PresenceStore) AddConnection(userID string, statusMessage, nickname, persisted *string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry, ok := p.users[userID]
	if !ok {
		p.users[userID] = &presenceEntry{connections: 1, statusMessage: statusMessage, nickname: nickname, persisted: persisted}
		return true
	}
	entry.connections++
	return false
}

// RemoveConnection libera uma conexão do usuário.
// Retorna true quando o usuário transiciona de online para offline
// (última conexão encerrada).
func (p *PresenceStore) RemoveConnection(userID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry, ok := p.users[userID]
	if !ok {
		return false
	}
	entry.connections--
	if entry.connections > 0 {
		return false
	}
	delete(p.users, userID)
	return true
}

// SetStatusMessage atualiza a mensagem de status de um usuário online.
// Retorna false quando o usuário está offline.
func (p *PresenceStore) SetStatusMessage(userID string, statusMessage *string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry, ok := p.users[userID]
	if !ok {
		return false
	}
	entry.statusMessage = statusMessage
	return true
}

// SetNickname atualiza o nickname de um usuário online.
// Retorna false quando o usuário está offline.
func (p *PresenceStore) SetNickname(userID string, nickname *string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry, ok := p.users[userID]
	if !ok {
		return false
	}
	entry.nickname = nickname
	return true
}

// SetPersistedStatus atualiza o status persistido (away/busy; nil remove) de
// um usuário online. Retorna false quando o usuário está offline.
func (p *PresenceStore) SetPersistedStatus(userID string, persisted *string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry, ok := p.users[userID]
	if !ok {
		return false
	}
	entry.persisted = persisted
	return true
}

// EffectiveStatus retorna o status efetivo do usuário: offline quando não
// há conexão ativa; away/busy quando há status persistido; online nos demais
// casos.
func (p *PresenceStore) EffectiveStatus(userID string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	entry, ok := p.users[userID]
	if !ok {
		return StatusOffline
	}
	if entry.persisted != nil {
		return *entry.persisted
	}
	return StatusOnline
}

// StatusMessage retorna a mensagem de status de um usuário online, ou nil
// quando o usuário está offline ou não tem mensagem.
func (p *PresenceStore) StatusMessage(userID string) *string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if entry, ok := p.users[userID]; ok {
		return entry.statusMessage
	}
	return nil
}

// Nickname retorna o nickname de um usuário online, ou nil quando o usuário
// está offline ou não tem nickname.
func (p *PresenceStore) Nickname(userID string) *string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if entry, ok := p.users[userID]; ok {
		return entry.nickname
	}
	return nil
}

// OnlineMembers retorna um snapshot dos usuários online, ordenado por id de
// usuário (ordem determinística para o cliente).
func (p *PresenceStore) OnlineMembers() []PresenceMember {
	p.mu.RLock()
	defer p.mu.RUnlock()

	members := make([]PresenceMember, 0, len(p.users))
	for userID, entry := range p.users {
		status := StatusOnline
		if entry.persisted != nil {
			status = *entry.persisted
		}
		members = append(members, PresenceMember{
			UserID:        userID,
			Status:        status,
			StatusMessage: entry.statusMessage,
		})
	}
	sort.Slice(members, func(i, j int) bool {
		return members[i].UserID < members[j].UserID
	})
	return members
}
