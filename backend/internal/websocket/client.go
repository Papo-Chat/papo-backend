package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"papo/internal/services"
	"papo/internal/utils"

	"github.com/gorilla/websocket"
)

const (
	sendBufferSize = 256

	// writeWait é o tempo máximo para escrever uma mensagem na conexão.
	writeWait = 10 * time.Second
	// pongWait é o tempo máximo sem atividade (pong) antes de encerrar a conexão.
	pongWait = 60 * time.Second
	// pingPeriod é o intervalo entre pings do protocolo WebSocket.
	pingPeriod = (pongWait * 9) / 10
	// maxMessageSize é o limite de tamanho de um evento inbound (JSON pequeno).
	maxMessageSize = 128 * 1024
	// typingCheckTimeout é o tempo máximo da checagem de permissão de canal
	// de um evento de typing.
	typingCheckTimeout = 5 * time.Second
)

// errSendBufferFull é retornado por Send quando o buffer do cliente está cheio.
var errSendBufferFull = errors.New("buffer de envio do cliente cheio")

// errClientClosed é retornado por Send quando a conexão do cliente foi
// encerrada (canal de envio fechado pelo Hub).
var errClientClosed = errors.New("conexão do cliente encerrada")

// Client é uma conexão WebSocket autenticada, gerenciada por um Hub.
type Client struct {
	hub             *Hub
	conn            *websocket.Conn
	userID          string
	statusMessage   *string
	nickname        *string
	persistedStatus *string
	send            chan []byte

	// mu protege closed e o fechamento do canal send, garantindo que Send
	// nunca escreva em um canal já fechado (panic de send on closed channel).
	mu     sync.Mutex
	closed bool

	// closeFrameOnce garante que o close frame do servidor seja enviado
	// no máximo uma vez (RFC 6455).
	closeFrameOnce sync.Once
}

// NewClient cria um Client com canal de envio bufferizado.
// statusMessage é a mensagem de status persistida do usuário
// (users.status_message), nickname é o nickname persistido (users.nickname)
// e persistedStatus é o status persistido (users.status: away/busy), todos
// carregados pelo handler na conexão.
func NewClient(hub *Hub, conn *websocket.Conn, userID string, statusMessage, nickname, persistedStatus *string) *Client {
	return &Client{
		hub:             hub,
		conn:            conn,
		userID:          userID,
		statusMessage:   statusMessage,
		nickname:        nickname,
		persistedStatus: persistedStatus,
		send:            make(chan []byte, sendBufferSize),
	}
}

// Connect registra a conexão autenticada no Hub e inicia os pumps de leitura
// e escrita. O upgrade HTTP deve ter sido feito pelo handler e o Hub.Run
// deve estar ativo.
func Connect(hub *Hub, conn *websocket.Conn, userID string, statusMessage, nickname, persistedStatus *string) *Client {
	client := NewClient(hub, conn, userID, statusMessage, nickname, persistedStatus)
	hub.Register(client)
	go client.WritePump()
	go client.ReadPump()
	return client
}

// UserID retorna o ID do usuário autenticado da conexão.
func (c *Client) UserID() string {
	return c.userID
}

// Send enfileira uma mensagem já serializada para envio ao cliente.
// Retorna erro sem bloquear quando a conexão foi encerrada ou o buffer do
// cliente está cheio.
func (c *Client) Send(message []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errClientClosed
	}
	select {
	case c.send <- message:
		return nil
	default:
		return errSendBufferFull
	}
}

// closeSend fecha o canal de envio do cliente exatamente uma vez. É chamado
// pelo Hub no desregistro; após o fechamento, Send retorna errClientClosed
// e a WritePump encerra a conexão.
func (c *Client) closeSend() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	close(c.send)
}

// sendCloseFrame envia o close frame do servidor (RFC 6455) exatamente uma
// vez, antes de encerrar a conexão. O close frame de resposta a um close
// frame recebido do cliente já é enviado pelo handler padrão do gorilla.
func (c *Client) sendCloseFrame() {
	c.closeFrameOnce.Do(func() {
		if err := c.conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(writeWait)); err != nil {
			utils.Warnf("websocket: falha ao enviar close frame (user=%s): %v", c.userID, err)
		}
	})
}

// sendEvent serializa o evento e enfileira para envio ao cliente.
// Se o buffer do cliente estiver cheio, a conexão é encerrada.
func (c *Client) sendEvent(event any) {
	data, err := json.Marshal(event)
	if err != nil {
		utils.Errorf("websocket: falha ao serializar evento: %v", err)
		return
	}
	c.sendRaw(data)
}

// sendRaw enfileira uma mensagem já serializada para envio ao cliente.
// Se o buffer do cliente estiver cheio, a conexão é encerrada; se a conexão
// já foi encerrada, a mensagem é descartada.
func (c *Client) sendRaw(data []byte) {
	if err := c.Send(data); err != nil {
		if errors.Is(err, errClientClosed) {
			return
		}
		utils.Warnf("websocket: buffer de envio cheio, encerrando conexão (user=%s)", c.userID)
		c.sendCloseFrame()
		c.conn.Close()
	}
}

// ReadPump lê e valida os eventos recebidos da conexão. Deve rodar em
// goroutine própria (Connect cuida disso). Ao terminar, desregistra o
// cliente no Hub e encerra a conexão.
func (c *Client) ReadPump() {
	defer func() {
		c.hub.Unregister(c)
		c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				utils.Warnf("websocket: erro de leitura (user=%s): %v", c.userID, err)
			}
			return
		}
		c.handle(message)
	}
}

// handle valida o envelope do evento recebido, responde com erro quando o
// tipo é desconhecido ou o JSON é inválido e despacha o evento.
func (c *Client) handle(raw []byte) {
	var envelope struct {
		Type EventType `json:"type"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || !envelope.Type.IsInbound() {
		c.sendEvent(ErrorOutbound{Type: EventTypeError, Message: "evento inválido"})
		return
	}

	switch envelope.Type {
	case EventTypeHeartbeat:
		c.sendEvent(HeartbeatAckOutbound{Type: EventTypeHeartbeatAck})
	case EventTypeTyping:
		c.handleTyping(raw)
	case EventTypeVoiceJoin:
		c.handleVoiceJoin(raw)
	case EventTypeVoiceLeave:
		c.handleVoiceLeave(raw)
	case EventTypeVoiceOffer:
		c.handleVoiceOffer(raw)
	case EventTypeVoiceAnswer:
		c.handleVoiceAnswer(raw)
	case EventTypeVoiceICECandidate:
		c.handleVoiceICECandidate(raw)
	case EventTypeTrackSubscribe:
		c.handleTrackSubscribe(raw)
	case EventTypeTrackUnsubscribe:
		c.handleTrackUnsubscribe(raw)
	case EventTypeVoiceMute:
		c.handleVoiceMute(raw)
	case EventTypeVoiceCamera:
		c.handleVoiceCamera(raw)
	case EventTypeScreenShareStart:
		c.handleScreenShareStart(raw)
	case EventTypeScreenShareStop:
		c.handleScreenShareStop(raw)
	}
}

// handleTyping valida o evento de typing e o distribui somente aos clientes
// cujo usuário pode ler o canal (read_channel). O usuário precisa da
// permissão read_channel do canal; em caso de canal inexistente ou sem
// permissão, o cliente recebe um evento de erro.
func (c *Client) handleTyping(raw []byte) {
	var event TypingInbound
	if err := json.Unmarshal(raw, &event); err != nil || event.ChannelID == "" {
		c.sendEvent(ErrorOutbound{Type: EventTypeError, Message: "evento inválido"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), typingCheckTimeout)
	defer cancel()

	switch err := services.CanReadChannel(ctx, event.ChannelID, c.userID); {
	case errors.Is(err, services.ErrChannelNotFound):
		c.sendEvent(ErrorOutbound{Type: EventTypeError, Message: "canal não encontrado"})
		return
	case errors.Is(err, services.ErrPermissionDenied):
		c.sendEvent(ErrorOutbound{Type: EventTypeError, Message: "sem permissão para o canal"})
		return
	case err != nil:
		utils.Errorf("websocket: falha ao verificar permissão de typing (user=%s, channel=%s): %v",
			c.userID, event.ChannelID, err)
		return
	}

	allowed, err := services.ChannelReaders(ctx, event.ChannelID, c.hub.OnlineUserIDs())
	if err != nil {
		utils.Errorf("websocket: falha ao autorizar o broadcast de typing (user=%s, channel=%s): %v",
			c.userID, event.ChannelID, err)
		return
	}

	c.hub.BroadcastToUsers(TypingOutbound{
		Type:      EventTypeTyping,
		ChannelID: event.ChannelID,
		UserID:    c.userID,
		IsTyping:  true,
	}, allowed)
}

// WritePump escreve as mensagens enfileiradas na conexão e envia pings do
// protocolo para mantê-la viva. Deve rodar em goroutine própria (Connect
// cuida disso). Quando o Hub fecha o canal de envio, encerra a conexão.
func (c *Client) WritePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			if !ok {
				// O Hub fechou o canal de envio: encerra a conexão com o
				// close frame do servidor.
				c.sendCloseFrame()
				return
			}
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
