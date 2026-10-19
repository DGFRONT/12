package hub

import (
	"context"
	"encoding/json"
	"time"

	"github.com/coder/websocket"
)

const (
	sendBuffer   = 64
	writeTimeout = 10 * time.Second
	pingInterval = 25 * time.Second
	maxMessage   = 16 << 10

	chatBurst   = 5
	chatPerSec  = 5.0
	maxChatRune = 2000
)

type envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Client is one WebSocket connection. All fields except conn and send are guarded by room.mu.
type Client struct {
	room *Room
	conn *websocket.Conn
	send chan []byte
	id   Identity

	pid      string // participants.id of this visit
	kind     string // host, member, guest, admin
	seq      int64  // join order
	admitted bool
	finished bool // send is closed, no more messages

	audio, video           bool
	allowAudio, allowVideo bool

	chatTokens float64
	chatLast   time.Time
}

func newClient(r *Room, conn *websocket.Conn, id Identity) *Client {
	conn.SetReadLimit(maxMessage)
	return &Client{room: r, conn: conn, send: make(chan []byte, sendBuffer), id: id, chatTokens: chatBurst}
}

// enqueueLocked queues a message; a client that cannot keep up is disconnected. Caller holds room.mu.
func (c *Client) enqueueLocked(typ string, data any) {
	if c.finished {
		return
	}
	msg, err := encode(typ, data)
	if err != nil {
		c.room.hub.log.Error("encode message", "type", typ, "err", err)
		return
	}
	select {
	case c.send <- msg:
	default:
		c.room.hub.log.Warn("client too slow, disconnecting", "participant", c.pid)
		c.finished = true
		close(c.send)
	}
}

// finishLocked sends a last message (if typ is not empty) and closes the connection after it is written.
func (c *Client) finishLocked(typ string, data any) {
	if c.finished {
		return
	}
	if typ != "" {
		c.enqueueLocked(typ, data)
	}
	if !c.finished {
		c.finished = true
		close(c.send)
	}
}

func encode(typ string, data any) ([]byte, error) {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return json.Marshal(envelope{Type: typ, Data: raw})
}

func (c *Client) writeLoop(ctx context.Context) {
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				c.conn.Close(websocket.StatusNormalClosure, "")
				return
			}
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.conn.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				c.conn.CloseNow()
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.conn.Ping(pctx)
			cancel()
			if err != nil {
				c.conn.CloseNow()
				return
			}
		case <-ctx.Done():
			c.conn.CloseNow()
			return
		}
	}
}

func (c *Client) readLoop(ctx context.Context) {
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil || env.Type == "" {
			c.room.sendError(c, "некорректное сообщение")
			continue
		}
		c.room.handle(c, env)
	}
}

// allowChat applies a token-bucket limit of chatPerSec messages per second. Caller holds room.mu.
func (c *Client) allowChatLocked(now time.Time) bool {
	if !c.chatLast.IsZero() {
		c.chatTokens += now.Sub(c.chatLast).Seconds() * chatPerSec
		if c.chatTokens > chatBurst {
			c.chatTokens = chatBurst
		}
	}
	c.chatLast = now
	if c.chatTokens < 1 {
		return false
	}
	c.chatTokens--
	return true
}
