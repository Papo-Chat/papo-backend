package services

import (
	"errors"
	"testing"

	"papo/internal/models"
	"papo/internal/storage"
)

// newVoiceTestUser registra um usuário novo (evita conflitar com o actor
// compartilhado e com o dono do servidor nos testes de permissão).
func newVoiceTestUser(t *testing.T) models.User {
	t.Helper()
	user, err := Register(testCtx(), newRandomUsername(), newRandomPassword(), newRandomIP())
	if err != nil {
		t.Fatalf("falha ao criar usuário: %v", err)
	}
	return user
}

// createVoiceRole cria uma role vazia (as permissões de canal são definidas
// em UpdateChannelPermissions).
func createVoiceRole(t *testing.T) models.Role {
	t.Helper()
	role, err := storage.CreateRole(testCtx(), newRandomRoleName(), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role: %v", err)
	}
	return role
}

// setupVoiceServer limpa o estado e cria um servidor com dono + canal de voz.
func setupVoiceServer(t *testing.T) (owner models.User, voiceID string) {
	t.Helper()
	cleanServers(testCtx())
	owner = newVoiceTestUser(t)
	if _, err := CreateServer(testCtx(), newRandomServerName(), &owner.ID); err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	ch, err := CreateChannel(testCtx(), owner.ID, newRandomChannelName(), "voice", "")
	if err != nil {
		t.Fatalf("falha ao criar canal de voz: %v", err)
	}
	return owner, ch.ID
}

func TestCanConnectVoiceNotFound(t *testing.T) {
	if err := CanConnectVoice(testCtx(), randUUID(), testActorID()); !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("esperava ErrChannelNotFound, obtive %v", err)
	}
}

func TestCanConnectVoiceEmptyChannelID(t *testing.T) {
	if err := CanConnectVoice(testCtx(), "", testActorID()); !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("esperava ErrChannelNotFound, obtive %v", err)
	}
}

func TestCanConnectVoiceWrongType(t *testing.T) {
	cleanServers(testCtx())
	owner := newVoiceTestUser(t)
	if _, err := CreateServer(testCtx(), newRandomServerName(), &owner.ID); err != nil {
		t.Fatalf("falha ao criar servidor: %v", err)
	}
	text, err := CreateChannel(testCtx(), owner.ID, newRandomChannelName(), "text", "")
	if err != nil {
		t.Fatalf("falha ao criar canal: %v", err)
	}
	if err := CanConnectVoice(testCtx(), text.ID, owner.ID); !errors.Is(err, ErrInvalidChannelType) {
		t.Fatalf("esperava ErrInvalidChannelType, obtive %v", err)
	}
}

func TestCanConnectVoiceOpenChannel(t *testing.T) {
	_, voiceID := setupVoiceServer(t)
	// canal aberto (sem roles): qualquer usuário pode entrar
	outsider := newVoiceTestUser(t)
	if err := CanConnectVoice(testCtx(), voiceID, outsider.ID); err != nil {
		t.Fatalf("esperava nil (canal aberto), obtive %v", err)
	}
}

func TestCanConnectVoiceOwnerAlwaysAllowed(t *testing.T) {
	owner, voiceID := setupVoiceServer(t)
	// canal com roles (não aberto) e sem ConnectVoice em nenhuma role
	role := createVoiceRole(t)
	if _, err := UpdateChannelPermissions(testCtx(), owner.ID, voiceID, role.ID, models.ChannelPermission{ConnectVoice: false}); err != nil {
		t.Fatalf("falha ao atualizar permissões: %v", err)
	}
	// dono sempre pode, mesmo sem role
	if err := CanConnectVoice(testCtx(), voiceID, owner.ID); err != nil {
		t.Fatalf("esperava nil (dono), obtive %v", err)
	}
}

func TestCanConnectVoiceRoleWithPermission(t *testing.T) {
	owner, voiceID := setupVoiceServer(t)
	member := newVoiceTestUser(t)
	role := createVoiceRole(t)
	if _, err := AssignUserRole(testCtx(), owner.ID, member.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}
	if _, err := UpdateChannelPermissions(testCtx(), owner.ID, voiceID, role.ID, models.ChannelPermission{ConnectVoice: true}); err != nil {
		t.Fatalf("falha ao atualizar permissões: %v", err)
	}
	if err := CanConnectVoice(testCtx(), voiceID, member.ID); err != nil {
		t.Fatalf("esperava nil (role com connect_voice), obtive %v", err)
	}
}

func TestCanConnectVoiceRoleWithoutPermission(t *testing.T) {
	owner, voiceID := setupVoiceServer(t)
	member := newVoiceTestUser(t)
	role := createVoiceRole(t)
	if _, err := AssignUserRole(testCtx(), owner.ID, member.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}
	if _, err := UpdateChannelPermissions(testCtx(), owner.ID, voiceID, role.ID, models.ChannelPermission{ConnectVoice: false, ReadChannel: true}); err != nil {
		t.Fatalf("falha ao atualizar permissões: %v", err)
	}
	if err := CanConnectVoice(testCtx(), voiceID, member.ID); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("esperava ErrPermissionDenied, obtive %v", err)
	}
}

func TestVoiceConnectorsNotFound(t *testing.T) {
	_, err := VoiceConnectors(testCtx(), randUUID(), []string{testActorID()})
	if !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("esperava ErrChannelNotFound, obtive %v", err)
	}
}

func TestVoiceConnectorsEmptyUserList(t *testing.T) {
	allowed, err := VoiceConnectors(testCtx(), randUUID(), nil)
	if err != nil {
		t.Fatalf("esperava nil, obtive %v", err)
	}
	if len(allowed) != 0 {
		t.Fatalf("esperava mapa vazio, obtive %v", allowed)
	}
}

func TestVoiceConnectorsOpenChannel(t *testing.T) {
	_, voiceID := setupVoiceServer(t)
	u1 := newVoiceTestUser(t)
	u2 := newVoiceTestUser(t)
	allowed, err := VoiceConnectors(testCtx(), voiceID, []string{u1.ID, u2.ID})
	if err != nil {
		t.Fatalf("esperava nil, obtive %v", err)
	}
	if !allowed[u1.ID] || !allowed[u2.ID] {
		t.Fatalf("canal aberto: esperava ambos allowed, obtive %v", allowed)
	}
}

func TestVoiceConnectorsRoleAndOwner(t *testing.T) {
	owner, voiceID := setupVoiceServer(t)
	voiceMember := newVoiceTestUser(t)
	other := newVoiceTestUser(t)
	role := createVoiceRole(t)
	if _, err := AssignUserRole(testCtx(), owner.ID, voiceMember.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir role: %v", err)
	}
	if _, err := UpdateChannelPermissions(testCtx(), owner.ID, voiceID, role.ID, models.ChannelPermission{ConnectVoice: true}); err != nil {
		t.Fatalf("falha ao atualizar permissões: %v", err)
	}

	allowed, err := VoiceConnectors(testCtx(), voiceID, []string{owner.ID, voiceMember.ID, other.ID})
	if err != nil {
		t.Fatalf("esperava nil, obtive %v", err)
	}
	if !allowed[owner.ID] {
		t.Errorf("dono deveria estar allowed")
	}
	if !allowed[voiceMember.ID] {
		t.Errorf("membro com role connect_voice deveria estar allowed")
	}
	if allowed[other.ID] {
		t.Errorf("usuário sem permissão não deveria estar allowed")
	}
}
