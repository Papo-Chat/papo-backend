# WEBRTC_PAPO.md — Plano de implementação: voz/câmera (SFU)

Plano autocontido para implementar voz/câmera/screenshare no Papo.

**Escopo:** só backend (este repo). O frontend é outro repo — o contrato de
eventos WebSocket e do endpoint REST fica especificado aqui (seções 7 e 8) para
ele consumir.

**Estado:** plano — nada implementado ainda.

---

## 1. Visão geral

Canais de tipo `voice` com SFU (Selective Forwarding Unit) embutido no backend
via **pion/webrtc** (única dependência nova). Cada usuário na call tem uma
`PeerConnection` só com o servidor. O servidor:

* **não** forwarda vídeo automaticamente — só com `track_subscribe` explícito
  do cliente (grid visível na UI);
* forwarda **áudio automaticamente** para o top-K de active speakers
  (detecção por audio-level RTP, RFC 6464);
* troca **quem** ocupa um slot pré-alocado via `RTPSender.ReplaceTrack()` —
  **sem** renegociação de SDP. Renegociação só quando o **número** de slots
  muda (raro) ou um publisher publica track nova (screen share).

Sinalização roda no WebSocket existente (`GET /ws`, JWT do cookie já validado,
origin já checada). Estado de sala é **efêmero em memória** (1 backend = 1
servidor; nada de voz no banco, mesma filosofia da presença atual).

Limitação conhecida e aceita (MVP): multi-instância exigiria sticky routing por
`channel_id`. **Não** adicionar Redis/broker/lock distribuído para isso.

---

## 2. Decisões de arquitetura

| # | Decisão | Racional |
| - | --- | --- |
| D1 | SFU single-instance, estado em memória | Simplicidade; 1 backend é decisão do projeto |
| D2 | Sinalização no WS existente | Zero superfície de auth nova (JWT + origin já cobertos) |
| D3 | Client-offers-first + renegociação **bidirecional** (qualquer lado envia `voice_offer`, o outro responde `voice_answer`) | Cobre os dois iniciadores (join pelo cliente; mais slots/screen share) sem segundo par de eventos |
| D4 | Fila de renegociação **por PeerConnection** (worker sequencial) | Pion não serializa `CreateOffer`/`SetLocalDescription`/`SetRemoteDescription`; eventos concorrentes quebram o signaling state |
| D5 | Slots pré-alocados: N vídeo (default 6) + K áudio (default 4) por subscriber, recvonly na answer | Troca de conteúdo = `ReplaceTrack`, sem round-trip de SDP |
| D6 | **Codec único de vídeo por sala** (default `vp8`) | `ReplaceTrack` exige mesmo codec na m-line; sem isso o design de slots quebra em cada swap. Enforcement: a answer só inclui o codec da sala; offer sem interseção → join rejeitado (`voice-codec-unsupported`). Áudio: opus only. `vp8` = denominador comum de send+receive em Chrome/Firefox/Safari (Safari não faz encode VP9 — revalidar antes de expor `vp9` como default) |
| D7 | **PLI em toda troca de conteúdo de slot de vídeo** (não só join/leave) | Evita corrupção de imagem até o próximo keyframe natural |
| D8 | Áudio por top-K de active speakers (RFC 6464), com decaimento + threshold | Evita microfone ruidoso monopolizando slots; mudo = cliente para de enviar frames → cai do top-K naturalmente |
| D9 | Interceptors: NACK/RTX (ambos sentidos), TWCC (subscriber→servidor), GCC por PC de subscriber; **REMB descartado** | Confiabilidade + estimativa de banda por downlink (GCC já limita o bitrate enviado a cada subscriber na fase 1) |
| D10 | **1 peer por usuário por sala** (não por conexão WS) | Multi-device: usuário com 2 abas recebe mídia nas duas conexões (`SendToUser` do hub já faz isso) e tem 1 estado na sala; peer sai quando a **última** conexão cai |
| D11 | Sala vazia → grace period (default 30s) → destruição | Reconexão rápida (wifi) é comum; não destruir na hora |
| D12 | Simulcast (RID low/med/high + seletor de camada por par) = **fase 2** | Fase 1 já elimina os maiores custos (vídeo não subscribed não é forwardado; GCC adapta bitrate). Simulcast é complexo e o ganho é marginal p/ salas 2–10 típicas. A arquitetura D5+D6 já é compatível (troca de camada = `ReplaceTrack` p/ outra RID, mesmo codec) |
| D13 | Permissão nova `connect_voice` em permissões de canal (JSONB) | Voz é mais sensível que texto: ler o canal não implica entrar na call. Fail-closed; canal aberto = livre (mesmo padrão de `read_channel`); dono sempre pode |
| D14 | TURN com credenciais efêmeras RFC 5389 por usuário | Auditável/revogável; TTL curto limita a janela |
| D15 | IPs privados **não** bloqueados em ICE candidates | Self-hosted/LAN é o caso principal — candidato privado é o caminho desejado (direto, sem TURN). Só rejeitar loopback/unspecified |
| D16 | Sem audit log de eventos de voz | Volume alto; audit é para ações administrativas |

---

## 3. Banco de dados

### `migrations/009_voice_channels.sql`

```sql
-- +goose Up
ALTER TABLE channels DROP CONSTRAINT IF EXISTS channels_type_check;
ALTER TABLE channels ADD CONSTRAINT channels_type_check CHECK (type IN ('text', 'category', 'voice'));

-- +goose Down
ALTER TABLE channels DROP CONSTRAINT IF EXISTS channels_type_check;
ALTER TABLE channels ADD CONSTRAINT channels_type_check CHECK (type IN ('text', 'category'));
```

* O CHECK inline de `channels.type` (`migrations/001_base_schema.sql:52`) tem
  nome auto-gerado pelo Postgres (`channels_type_check`) — **confirmar com
  `\d channels` antes de rodar**.
* Sem nova tabela, sem nova coluna. Canais `voice` aceitam `topic` (a
  constraint `channels_topic_text_only` só proíbe topic em `category` — sem
  mudança).

---

## 4. Permissões

* `models.ChannelPermission` (`models/channel.go:6`) ganha
  `ConnectVoice bool \`json:"connect_voice"\``. JSONB backward compatible:
  entry de role existente sem o campo = `false` (fail-closed).
* `services/voice.go` (novo) — espelha `CanReadChannel`/`ChannelReaders`
  (`services/channels.go:260/292`), chamando o helper existente
  `userHasChannelPermission` (`services/messages.go:784`):

  ```go
  // ErrInvalidChannelType: canal existe mas type != "voice"
  func CanConnectVoice(ctx context.Context, channelID, userID string) error
      // → ErrChannelNotFound | ErrInvalidChannelType | ErrPermissionDenied
  func VoiceConnectors(ctx context.Context, channelID string, userIDs []string) (map[string]bool, error)
      // quem pode ver a call (audiência dos eventos de presença de voz)
  func ICEConfigForUser(ctx context.Context, userID string) ([]models.ICEServer, error)
      // monta a lista STUN + TURN com credencial efêmera do usuário
  ```

  * `CanConnectVoice`: `storage.GetChannelByID` → type deve ser `"voice"` →
    `userHasChannelPermission(ctx, channel, userID, /*freeIfOpen=*/true,
    func(p models.ChannelPermission) bool { return p.ConnectVoice })`.
* `services/channels.go`: `CreateChannel` (validação em `:57`) e
  `UpdateChannel` passam a aceitar `type: "voice"`.
* `PUT /channels/:id/permissions/:role_id` já aceita o novo campo no body
  (mesmo JSONB) — **sem nova rota**.

---

## 5. Arquivos novos

### 5.1 `backend/internal/models/voice.go`

```go
// Estado efêmero de um usuário dentro de uma sala de voz (o que a UI precisa).
type VoiceState struct {
    UserID         string `json:"user_id"`
    Muted          bool   `json:"muted"`
    CameraOn       bool   `json:"camera_on"`
    ScreenSharing  bool   `json:"screen_sharing"`
}

// Formato do browser (RTCIceServer) — o frontend passa direto p/ RTCPeerConnection.
type ICEServer struct {
    URLs       []string `json:"urls"`
    Username   string   `json:"username,omitempty"`
    Credential string   `json:"credential,omitempty"`
}
```

Sem `ServerID` (1 backend = 1 servidor singleton).

### 5.2 `backend/internal/utils/turn.go`

```go
// RFC 5389: username = "<ttl_unix>:<user_id>", credential = hex(HMAC-SHA1(secret, username)).
func GenerateTurnCredential(secret []byte, userID string, ttl time.Duration) (username, credential string)
```

* `user_id` no username: credencial auditável e revogável por usuário.
* Nunca logar credential.

### 5.3 `backend/internal/handlers/voice.go`

```go
// GET /voice/ice-servers — JWTMiddleware (cookie Auth) + rate limit global já aplicados.
func ICEServersHandler(baseURL string, c echo.Context) error
```

* 200: `{ "ice_servers": [ { "urls": [...] }, ... ] }` (shape da seção 8).
* 401 via middleware; 500 em falha interna (RFC 7807, padrão do projeto).

### 5.4 `backend/internal/webrtc/manager.go`

```go
// Signaler: funções concretas injetadas pelo main.go a partir do hub
// (evita ciclo de import — webrtc NÃO importa websocket).
type Signaler struct {
    SendToUser       func(userID string, event any)
    BroadcastToUsers func(allowed map[string]bool, event any)
}

type Manager struct {
    // rooms map[channelID]*Room + sync.RWMutex
    // limiters map[userID]*userLimiters (token bucket, golang.org/x/time/rate — já é dependência)
    ...
}

func NewManager(cfg *config.Config, s Signaler) *Manager
func GetManager() *Manager   // global, mesmo padrão do hub (websocket/hub.go:30)

// Chamados pelo websocket/client.go (eventos inbound):
func (m *Manager) Join(channelID, userID string) error          // ErrVoiceRoomFull | ErrVoiceAlreadyInRoom | ...
func (m *Manager) Leave(channelID, userID string)
func (m *Manager) ClientOffer(channelID, userID, sdp string) error   // → voice_answer unicast
func (m *Manager) ClientAnswer(channelID, userID, sdp string) error  // renegociação iniciada pelo servidor
func (m *Manager) AddICECandidate(channelID, userID, candidate, sdpMid string, sdpMLineIndex int) error
func (m *Manager) Subscribe(channelID, subscriberID, publisherID, kind string) error // kind: "video"|"screen"
func (m *Manager) Unsubscribe(channelID, subscriberID, publisherID, kind string) error
func (m *Manager) SetMuted(channelID, userID string, muted bool)      // estado + broadcast voice_state_update
func (m *Manager) SetCameraOn(channelID, userID string, on bool)
func (m *Manager) StartScreenShare(channelID, userID string)          // estado + broadcast (a track chega na renegociação)
func (m *Manager) StopScreenShare(channelID, userID string)

// Chamados pelo hub (gancho) e pelo services:
func (m *Manager) UserOffline(userID string)     // remove o peer de TODAS as salas (última conexão caiu)
func (m *Manager) DestroyRoom(channelID string)  // canal voice excluído (services.DeleteChannel)
func (m *Manager) Shutdown()                     // fecha todas as PCs (close frame) e para o manager
```

* Get-or-create de sala; sala vazia → `time.AfterFunc(grace)` → destrói se
  ainda vazia (D11).
* Enforce `VOICE_MAX_ROOM_PEERS` no Join; `VOICE_MAX_ROOMS_PER_USER` (peers do
  usuário em salas distintas).
* **Autorização de membership em toda chamada**: o peer deve estar na sala e o
  `channel_id` do evento deve bater com a sala do peer (o check de permissão
  de canal é feito antes, no client.go — padrão do codebase, ver
  `handleTyping` em `websocket/client.go:210`).
* Rate limit por usuário (seção 9): violação → evento `error`
  `code: "voice-rate-limited"` (não corta a conexão).

### 5.5 `backend/internal/webrtc/room.go`

```go
type Room struct {
    channelID string
    peers     map[string]*Peer   // key: userID (D10)
    mu        sync.Mutex
    // active speakers: scores map[userID]float64 + topK atual (D8)
    ...
}
```

* `addPeer`/`removePeer` (removePeer: libera os slots que o peer ocupava nos
  subscribers — "parar de escrever" no slot, sem renegociação — e recalcula
  top-K).
* **Áudio:** para o subscriber X, o top-K é calculado **excluindo o próprio X**
  (o browser dele já toca o áudio local; não se recebe a própria voz).
* `syncAudioSlots(sub *Peer, topK []string)`: preenche os K slots de áudio do
  subscriber com os publishers do top-K via `ReplaceTrack` (sem SDP).
* `forEachSubscriber(fn)` com snapshot.

### 5.6 `backend/internal/webrtc/peer.go`

```go
type Peer struct {
    userID string
    pc     *webrtc.PeerConnection
    // slots de envio (servidor→cliente): N videoSenders + K audioSenders
    // (*webrtc.RTPSender, cada um numa TrackLocalStaticRTP)
    // ocupação: videoSlots[K] → *Peer (publisher) + kind; audioSlots[K] → *Peer
    // tracks recebidas (cliente→servidor): audio, video, screen (*webrtc.TrackRemote)
    renegQueue chan func() error  // D4: worker sequencial por PC
    ...
}
```

* **Criação** (`newPeer`):
  * ICE servers da config (STUN/TURN sem credential — o credential não vai no
    SDP; TURN aqui só como URL; a credential efêmera é do cliente via
    `/voice/ice-servers`).
  * MediaEngine: opus + codec de vídeo da sala (D6).
  * InterceptorRegistry: NACK (send+recv), TWCC header extension + TWCC sender
    nas m-lines de envio, GCC (D9).
  * Fluxo do join: cliente envia offer (m-lines sendonly: áudio, vídeo,
    opcionalmente screen) → `SetRemoteDescription` → `AddTransceiver`
    (recvonly) para cada slot de vídeo (N) e de áudio (K) → `CreateAnswer` →
    `SetLocalDescription` → `voice_answer` + trickle.
  * **Codec enforcement (D6):** na answer, aceitar só o codec da sala nas
    m-lines de vídeo; sem interseção → erro `voice-codec-unsupported`.
* **Identificação da track de screen share:** é a m-line de vídeo **nova** na
  renegociação (o servidor conhece a contagem anterior de m-lines — ele
  controla a answer). Determinístico, sem depender de label.
* **Fila de renegociação (D4):** todo `CreateOffer`/`SetLocalDescription`/
  `SetRemoteDescription` passa por `renegQueue` processada por um worker
  sequencial (goroutine por PC).
* `OnICECandidate` → `Signaler.SendToUser(voice_ice_candidate)`.
* `OnConnectionStateChange`: `failed`/`closed` → remove o peer da sala;
  `disconnected` → aguarda recuperação (pion tenta reconectar).
* **PLI (D7):** `RTPReceiver.WriteRTCP` com `rtcp.PictureLossIndication` para
  o SSRC do publisher, disparado em **toda** troca de conteúdo de slot de
  vídeo. (Validar API exata nos docs do pion na implementação.)
* **Mute/câmera:** o cliente para de enviar frames (track segue conectada); o
  servidor só atualiza estado + broadcast. Slots que recebem esse publisher
  simplesmente param de receber pacotes (sem ação).
  **Push-to-talk é concern do client** (toggling rápido desse estado) e já é
  suportado: quem segura o botão é o único gerando audio-level → entra no
  top-K sozinho; ao soltar, o score decai. Sem mudança de servidor.

### 5.7 `backend/internal/webrtc/sfu.go`

Forwarding por slot:

* Um goroutine por par ativo (track do publisher → slot do subscriber): lê
  `track.ReadRTP()`, escreve `slot.WriteRTP()` com **tradução de sequence
  number por par** (map de offset; prática padrão de SFU).
* `assignVideoSlot(sub, pub, kind)`: slot livre → `ReplaceTrack`; **sem slot
  livre** → renegociação pontual (nova m-line recvonly) — caminho raro.
* `releaseVideoSlot(sub, slot)`: para de escrever (slot continua existindo,
  vazio). **Slot vazio = sem pacotes** (não usar track de silêncio, não
  renegociar); novo ocupante dispara PLI.
* `replaceVideoSlot(sub, slot, pub, kind)`: `ReplaceTrack` + PLI (D7).
* Troca de camada simulcast (fase 2): `ReplaceTrack` para outra RID do mesmo
  publisher (mesmo codec) — a arquitetura já suporta.

### 5.8 `backend/internal/webrtc/active_speaker.go`

* Interceptador de audio-level (RFC 6464) alimenta `scores[userID]` da sala.
* Ticker por sala (1s): decaimento dos scores + threshold mínimo; top-K =
  `VOICE_AUDIO_SLOTS` maiores scores.
* Mudança no top-K → `room.syncAudioSlots` para cada subscriber +
  `BroadcastToUsers(active_speaker_update)` para os leitores do canal.
* (Mudo não envia frames → sem pacotes → score decai → sai do top-K sozinho.)

### 5.9 `backend/internal/webrtc/validate.go`

* `validateSDP(sdp string, expectedMlines int) error`:
  * tamanho ≤ 64 KB (teto físico do WS é 128 KB — `client.go:26`);
  * parse do SDP (pion/sdp); m-lines de vídeo devem conter o codec da sala;
  * nº de m-lines ≤ esperado (slots + tracks do peer) — contem injeção de
    m-lines extras.
* `validateCandidate(raw string) error`: máx 1 candidate por evento, ~1 KB por
  candidate, rejeitar addresses loopback/unspecified. **IPs privados permitidos
  (D15).**

---

## 6. Arquivos alterados

| Arquivo | Alteração |
| --- | --- |
| `backend/internal/models/channel.go` | `ChannelPermission` + `ConnectVoice bool` |
| `backend/internal/services/channels.go` | aceitar `type: "voice"` em `CreateChannel`/`UpdateChannel`; `DeleteChannel` (`:207`) chama `webrtc.GetManager().DestroyRoom(id)` após sucesso |
| `backend/internal/websocket/events.go` | novos `EventType` + structs (seção 7); `IsInbound()` (`:40`) aceita os inbound de voz |
| `backend/internal/websocket/client.go` | switch do `handle` (`:198`) roteia `voice_*`/`track_*`: unmarshal → permissão (`services.CanConnectVoice` p/ join, timeout de 5s como `handleTyping`) → `webrtc.GetManager()` |
| `backend/internal/websocket/hub.go` | campo `onUserOffline func(userID string)` + `SetOnUserOffline`; chamado em `presenceOffline` quando `RemoveConnection` retorna true (última conexão do usuário) |
| `backend/internal/config/config.go` | novas env vars (seção 9), padrão `getEnv*` existente |
| `backend/internal/handlers/routes.go` | `RegisterVoiceRoutes`: `GET /voice/ice-servers` com `middleware.JWTMiddleware` |
| `backend/cmd/main.go` | validar `TURN_SECRET` (obrigatório se `TURN_URLS` não vazio, ≥ 32 bytes — mesmo padrão de `validateJWTSecret`); criar manager com `Signaler{SendToUser: hub.SendToUser, BroadcastToUsers: hub.BroadcastToUsers}`; `hub.SetOnUserOffline(manager.UserOffline)`; shutdown: `e.Shutdown` → `hub.Shutdown` (corta WS, dispara `UserOffline`) → `manager.Shutdown()` (varredura final) |
| `openapi.yml` | rota `/voice/ice-servers`; enum `Channel.type` + `voice` (`:2441`); campo `connect_voice` em `ChannelPermissionEntry`; novos eventos na tabela do `/ws` (`:1857`) |

**Ciclo de import (resolvido):** `websocket` → `webrtc` (client.go despacha) e
`webrtc` → **nada** de `websocket` (envio via `Signaler`; offline via gancho
do hub; ambos injetados no `main.go`). `webrtc` importa só `config`, `models`,
`utils`, pion. `services` pode importar `webrtc` (DeleteChannel) sem ciclo.

---

## 7. Contrato de eventos WebSocket (para o repo do frontend)

Mesma conexão `GET /ws`, mesmo envelope `{ "type": ..., ... }`. Erros: evento
`error` existente com `code` — novos: `voice-not-found`, `voice-forbidden`,
`voice-room-full`, `voice-already-in-room`, `voice-codec-unsupported`,
`voice-invalid-sdp`, `voice-rate-limited`, `voice-room-closed`.

### Inbound (cliente → servidor)

| Event | Payload | Notas |
| --- | --- | --- |
| `voice_join` | `{ type, channel_id }` | Permissão `connect_voice`; responde `voice_joined` (unicast) |
| `voice_leave` | `{ type, channel_id }` | Saída explícita |
| `voice_offer` | `{ type, channel_id, sdp }` | Qualquer lado inicia (D3): join, screen share, mais slots |
| `voice_answer` | `{ type, channel_id, sdp }` | Resposta a um `voice_offer` |
| `voice_ice_candidate` | `{ type, channel_id, candidate, sdp_mid, sdp_mline_index }` | Trickle, 1 por evento |
| `track_subscribe` | `{ type, channel_id, publisher_id, kind: "video" \| "screen" }` | Vídeo sempre explícito |
| `track_unsubscribe` | `{ type, channel_id, publisher_id, kind }` | |
| `voice_mute` | `{ type, channel_id, muted }` | Estado do mic; cliente para de enviar frames |
| `voice_camera` | `{ type, channel_id, on }` | Estado da câmera; idem |
| `screen_share_start` | `{ type, channel_id }` | Estado; a track nova chega via renegociação |
| `screen_share_stop` | `{ type, channel_id }` | Idem, remove a track |

### Outbound (servidor → cliente)

| Event | Payload | Destinatários |
| --- | --- | --- |
| `voice_joined` | `{ type, channel_id, members: [VoiceState], active_speakers: [user_id] }` | Unicast (estado inicial p/ late joiner) |
| `voice_answer` | `{ type, channel_id, sdp }` | Unicast |
| `voice_ice_candidate` | `{ type, channel_id, candidate, sdp_mid, sdp_mline_index }` | Unicast |
| `voice_state_update` | `{ type, channel_id, user_id, muted, camera_on, screen_sharing }` | Leitores do canal de voz (inclui quem está fora da call) |
| `voice_leave` | `{ type, channel_id, user_id }` | Idem |
| `active_speaker_update` | `{ type, channel_id, user_ids: [top-K] }` | Idem (só p/ UI destacar) |

* Áudio **não** tem `track_subscribe` (automático via top-K).
* Mute/câmera off não desconectam track (evita renegociação p/ evento frequente).
* `voice_leave` chega também quando: WS caiu, sessão revogada (recheck de 30s
  do hub já desconecta), sala derrubada (`voice-room-closed`).

---

## 8. Endpoint REST novo

### `GET /voice/ice-servers` (autenticado, cookie `Auth`)

```json
{
  "ice_servers": [
    { "urls": ["stun:stun.exemplo.com:3478"] },
    {
      "urls": ["turn:turn.exemplo.com:3478?transport=udp", "turns:turn.exemplo.com:443?transport=tcp"],
      "username": "1759999999:uuid-do-usuario",
      "credential": "hex-hmac-sha1"
    }
  ]
}
```

* Shape idêntico a `RTCiceServer` do browser — o frontend passa direto para
  `new RTCPeerConnection({ iceServers })`.
* `turns:443` só quando configurado em `TURN_URLS` (fallback p/ firewall que
  bloqueia UDP).
* STUN/TURN por default vazios: em LAN o ICE resolve direto; quem expõe na
  internet configura.

---

## 9. Configuração (novas env vars)

| Var | Default | Descrição |
| --- | --- | --- |
| `STUN_URLS` | vazio | Lista (vírgula) de URLs STUN |
| `TURN_URLS` | vazio | Lista de URLs TURN |
| `TURN_SECRET` | — (obrigatório se `TURN_URLS`) | Segredo HMAC, ≥ 32 bytes |
| `TURN_TTL_SECONDS` | 3600 | TTL da credencial efêmera |
| `VOICE_VIDEO_CODEC` | `vp8` | Codec único de vídeo da sala (`vp8`/`vp9`/`h264`) |
| `VOICE_VIDEO_SLOTS` | 6 | Slots de vídeo pré-alocados por subscriber |
| `VOICE_AUDIO_SLOTS` | 4 | Slots de áudio = top-K de active speakers |
| `VOICE_MAX_ROOM_PEERS` | 25 | Máx. peers por sala (contém CPU/banda do SFU) |
| `VOICE_MAX_ROOMS_PER_USER` | 1 | Máx. salas simultâneas por usuário |
| `VOICE_ROOM_CLEANUP_GRACE` | 30s | Grace period antes de destruir sala vazia |
| `VOICE_SIGNAL_RATE_LIMIT` / `VOICE_SIGNAL_RATE_BURST` | 10 / 20 | Por usuário: join/leave/offer/answer/ice/mute/camera/screen |
| `VOICE_SUBSCRIBE_RATE_LIMIT` / `VOICE_SUBSCRIBE_RATE_BURST` | 5 / 10 | Por usuário: track_subscribe/unsubscribe |

---

## 10. Segurança (resumo operacional)

* **AuthN:** WS existente (JWT cookie) + origin check — nada novo.
* **AuthZ em todo evento:** `connect_voice` no join (fail-closed, D13);
  membership + `channel_id` consistente em todos os demais; publisher alvo
  existe na sala e com track ativa em `track_subscribe`.
* **Abuso:** rate limit por usuário de eventos de voz (seção 9); validação de
  SDP/candidates (5.9); limites de sala (D12/D9); 1 track de screen share por
  peer; codec único (D6) limita a superfície de codecs suportados.
* **Mídia:** SRTP/DTLS (pion). **Sinalização:** autenticada via WS. **TURN:**
  efêmero por usuário (D14). **Logs:** nunca credential/SDP completo; segue a
  regra de não expor segredos.

---

## 11. Fases de implementação

**Fase 0 — fundação (sem mídia):**
1. `migrations/009_voice_channels.sql` (seção 3).
2. `models/voice.go` + `ConnectVoice` em `ChannelPermission`.
3. `services/voice.go` (`CanConnectVoice`, `VoiceConnectors`, `ICEConfigForUser`).
4. `config.go` + validação de boot de `TURN_SECRET` em `main.go`.
5. `utils/turn.go` + `handlers/voice.go` + rota em `routes.go`.
6. `openapi.yml` (rota, enum, campo de permissão, tabela de eventos).

**Fase 1 — MVP SFU:**
7. `webrtc/manager.go` + `room.go` + gancho `onUserOffline` do hub + wiring em
   `main.go` (incluindo shutdown e `DestroyRoom` em `DeleteChannel`).
8. `webrtc/peer.go`: PC por peer, slots pré-alocados, interceptors
   (NACK/TWCC/GCC), fila de renegociação, trickle ICE, codec enforcement.
9. `webrtc/sfu.go`: forwarding por slot, `ReplaceTrack`, PLI, slot vazio.
10. `webrtc/active_speaker.go`: RFC 6464, top-K com decaimento, swap de slots
    de áudio, `active_speaker_update`.
11. `webrtc/validate.go` + rate limit por usuário.
12. `websocket/events.go` + roteamento em `client.go` (seção 7).
13. Screen share (renegociação do publisher + subscribe como vídeo).
14. Presença: `voice_state_update`, `voice_joined`, `voice_leave`.

**Fase 2 — otimização de banda:**
15. Simulcast RID (3 camadas) + seletor de camada por (publisher, subscriber)
    sobre o estimador GCC + tamanho de render reportado pelo cliente.
16. Métricas via log estruturado (peers por sala, bitrate por subscriber,
    renegociações) — sem nova infra.

**Fora de escopo (documentado):** multi-instância (sticky routing), E2EE,
gravação. (Push-to-talk não está aqui: é concern do client, ver 5.6.)

---

## 12. Testes (`cd backend && go test ./...`)

* **services:** `CanConnectVoice` (não encontrado, type errado, sem permissão,
  canal aberto, dono, role com `connect_voice`); `ICEConfigForUser` (shape,
  credential presente só em TURN).
* **utils/turn:** vetor de teste HMAC-SHA1 conhecido; formato do username; TTL.
* **handlers:** `/voice/ice-servers` 401 (sem cookie) / 200 (shape) / 500.
* **webrtc (PCs in-process, pion suporta ICE em loopback):**
  * join → offer/answer → conexão estabelecida; 2º peer entra e recebe o 1º via
    slot de áudio (top-K trivial);
  * `track_subscribe` → vídeo chega no slot; `track_unsubscribe` → slot vazio;
    swap de slot (2 publishers, 1 slot) → `ReplaceTrack` + PLI;
  * screen share: `screen_share_start` + renegociação (nova m-line) → track
    identificada; `track_subscribe { kind: "screen" }` → chega no slot;
    `screen_share_stop` → slots de screen liberados, câmera segue intacta;
  * leave → slots liberados nos demais; último sai → sala destruída após o
    grace period;
  * `UserOffline` remove o peer; 2 conexões do mesmo usuário = 1 peer;
  * rate limit: flood de `track_subscribe` → `voice-rate-limited`;
  * validação: SDP > 64 KB, codec errado, candidate loopback → rejeitados;
  * `DestroyRoom` (DeleteChannel) derruba os peers com `voice-room-closed`.
* **websocket:** evento de voz em canal inexistente/sem permissão → evento
  `error` com o code correto (mesmo padrão dos testes de typing existentes).

---

## 13. Riscos / validar na implementação

1. **APIs do pion** (consultar docs oficiais antes de codificar): semântica de
   `ReplaceTrack` (parâmetros de codec), `TrackLocalStaticRTP` + tradução de
   SSN, wiring dos interceptors (NACK/TWCC/GCC), `RTPReceiver.WriteRTCP` p/
   PLI, `AddTransceiver` entre offer e answer.
2. **Safari e encode VP9** — base do default `vp8`; revalidar.
3. **RFC 6464** — confirmar que Chrome/Firefox/Safari enviam audio-level no
   `getUserMedia` (expectativa: sim). Fallback documentado: sem a extensão, o
   usuário só entra no top-K quando a UI marcar "falar".
4. **SSN após `ReplaceTrack`** — jump grande de SSN pode confundir o jitter
   buffer; PLI mitiga; validar e, se necessário, reiniciar o SSN do slot no
   swap.
5. **Slot vazio sem pacotes** — comportamento do browser (track "stalled") —
   validar que não gera artefatos de áudio/vídeo.

---

## 14. Infraestrutura (decisão do owner)

* **coturn** (STUN + TURN UDP/TCP 3478 + TURN sobre TLS 443) **não existe neste
  repo** (`infra/` só tem o postgres). Deploy + TLS + definição de `TURN_SECRET`
  é trabalho de infra fora do repo. O backend funciona sem TURN em LAN; TURN é
  requisito para exposição na internet. **Decidir onde o coturn roda antes da
  fase 0** (não bloqueia as fases 0–1 em LAN).
* Se houver nginx fora do repo: rotear `turns:443` (TLS) para o coturn.
