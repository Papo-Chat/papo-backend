package websocket

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/storage"
	"papo/internal/webrtc"

	ws "github.com/gorilla/websocket"
)

// setupVoiceManager cria o manager global de voz apontando para o hub de teste
// (mesmo wiring do main). O manager é o único estado global do pacote webrtc;
// cada teste de voz cria o seu e o derruba no cleanup.
func setupVoiceManager(t *testing.T, hub *Hub) *webrtc.Manager {
	t.Helper()
	cfg := &config.Config{
		VoiceVideoCodec:         "vp8",
		VoiceVideoSlots:         6,
		VoiceAudioSlots:         4,
		VoiceMaxRoomPeers:       25,
		VoiceMaxRoomsPerUser:    1,
		VoiceRoomCleanupGrace:   time.Second,
		VoiceSignalRateLimit:    100,
		VoiceSignalRateBurst:    100,
		VoiceSubscribeRateLimit: 100,
		VoiceSubscribeRateBurst: 100,
	}
	m := webrtc.NewManager(cfg, webrtc.Signaler{
		SendToUser: hub.SendToUser,
		SendToClient: hub.SendToClient,
		BroadcastToUsers: func(allowed map[string]bool, event any) {
			hub.BroadcastToUsers(event, allowed)
		},
		VoiceAudience: func(channelID string) map[string]bool {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			allowed, err := services.VoiceConnectors(ctx, channelID, hub.OnlineUserIDs())
			if err != nil {
				return map[string]bool{}
			}
			return allowed
		},
	})
	t.Cleanup(m.Shutdown)
	return m
}

// readVoiceEvent lê eventos até encontrar um do tipo esperado, ignorando
// presence_sync (que chega na conexão e pode intercalar com a resposta de voz).
func readVoiceEvent(t *testing.T, conn *ws.Conn, expectedType string) wsEvent {
	t.Helper()
	deadline := time.Now().Add(3 * wsReadTimeout)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("falha ao ler evento websocket: %v", err)
		}
		var event wsEvent
		if err := json.Unmarshal(data, &event); err != nil {
			t.Fatalf("falha ao decodificar o evento %s: %v", data, err)
		}
		if event.Type == expectedType {
			return event
		}
	}
	t.Fatalf("não recebeu o evento %s a tempo", expectedType)
	return wsEvent{}
}

// newVoiceTestServerChannel cria um servidor + canal de VOZ (tipo "voice").
func newVoiceTestServerChannel(t *testing.T, ownerID *string) (models.Server, models.ChannelSummary) {
	t.Helper()
	removeAllServersTest(t)
	server, err := services.CreateServer(testCtx(), "server_"+randHex(8), ownerID)
	if err != nil {
		t.Fatalf("falha ao criar servidor de teste: %v", err)
	}
	actorID := ""
	if ownerID != nil {
		actorID = *ownerID
	}
	channel, err := services.CreateChannel(testCtx(), actorID, "chan_"+randHex(8), "voice", "")
	if err != nil {
		t.Fatalf("falha ao criar canal de voz de teste: %v", err)
	}
	return server, channel
}

// grantVoiceReadPermission torna o canal não-aberto (tem permissões) e dá ao
// usuário apenas read_channel (sem connect_voice), para o caso de negação.
func grantVoiceReadPermission(t *testing.T, server models.Server, channel models.ChannelSummary, user models.User) {
	t.Helper()
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(8), nil, models.RolePermissions{})
	if err != nil {
		t.Fatalf("falha ao criar role de teste: %v", err)
	}
	actorID := ""
	if server.OwnerID != nil {
		actorID = *server.OwnerID
	}
	if _, err := services.UpdateChannelPermissions(testCtx(), actorID, channel.ID, role.ID, models.ChannelPermission{ReadChannel: true}); err != nil {
		t.Fatalf("falha ao definir permissão no canal: %v", err)
	}
	if _, err := storage.AssignUserRole(testCtx(), user.ID, role.ID); err != nil {
		t.Fatalf("falha ao atribuir a role ao usuário: %v", err)
	}
}

// TestVoiceJoinInvalidEvent: voice_join sem channel_id → "evento inválido".
func TestVoiceJoinInvalidEvent(t *testing.T) {
	hub := newWSHub(t)
	srv := newWSTestServer(t, hub, nil)
	setupVoiceManager(t, hub)
	conn := srv.dial(t, "user-invalid")

	sendRaw(t, conn, `{"type":"voice_join"}`)
	event := readVoiceEvent(t, conn, "error")
	if event.Message != "evento inválido" {
		t.Errorf("esperava erro 'evento inválido', obtive %+v", event)
	}
}

// TestVoiceJoinNotFound: voice_join em canal inexistente → "canal não encontrado".
func TestVoiceJoinNotFound(t *testing.T) {
	hub := newWSHub(t)
	srv := newWSTestServer(t, hub, nil)
	setupVoiceManager(t, hub)
	conn := srv.dial(t, "user-notfound")

	sendRaw(t, conn, `{"type":"voice_join","channel_id":"`+randUUID()+`"}`)
	event := readVoiceEvent(t, conn, "error")
	if event.Message != "canal não encontrado" {
		t.Errorf("esperava erro 'canal não encontrado', obtive %+v", event)
	}
}

// TestVoiceJoinForbidden: canal de voz não-aberto, usuário sem connect_voice →
// erro com código voice-forbidden (D13, fail-closed no backend).
func TestVoiceJoinForbidden(t *testing.T) {
	owner := newTestUser(t)
	server, channel := newVoiceTestServerChannel(t, &owner.ID)
	user := newTestUser(t)
	grantVoiceReadPermission(t, server, channel, user)

	hub := newWSHub(t)
	srv := newWSTestServer(t, hub, nil)
	setupVoiceManager(t, hub)
	conn := srv.dial(t, user.ID)

	sendRaw(t, conn, `{"type":"voice_join","channel_id":"`+channel.ID+`"}`)
	event := readVoiceEvent(t, conn, "error")
	if event.Code == nil || *event.Code != "voice-forbidden" {
		t.Errorf("esperava código 'voice-forbidden', obtive %+v", event)
	}
}

// TestVoiceJoinSuccess: canal de voz aberto → voice_joined para o usuário.
func TestVoiceJoinSuccess(t *testing.T) {
	owner := newTestUser(t)
	_, channel := newVoiceTestServerChannel(t, &owner.ID)
	user := newTestUser(t)

	hub := newWSHub(t)
	srv := newWSTestServer(t, hub, nil)
	setupVoiceManager(t, hub)
	conn := srv.dial(t, user.ID)

	sendRaw(t, conn, `{"type":"voice_join","channel_id":"`+channel.ID+`"}`)
	event := readVoiceEvent(t, conn, "voice_joined")
	found := false
	for _, m := range event.Members {
		if m.UserID == user.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("voice_joined não contém o usuário %s: %+v", user.ID, event.Members)
	}
}
