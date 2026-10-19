// Package hub keeps the live state of conference rooms: who is waiting, who is inside,
// media permissions and chat. Clients talk to it over WebSocket with JSON messages
// of the form {"type": "...", "data": {...}}.
package hub

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/dgfront/12/internal/sfu"
	"github.com/dgfront/12/internal/store"
)

// Reasons sent with "room.closed" and "removed" messages.
const (
	ReasonEnded           = "ended"
	ReasonClosedByAdmin   = "closed_by_admin"
	ReasonHostLeft        = "host_left"
	ReasonKicked          = "kicked"
	ReasonRejected        = "rejected"
	ReasonReplaced        = "replaced"
	ReasonAccountDisabled = "account_disabled"
)

// Identity is an authenticated visitor of a room.
type Identity struct {
	Key     string // "u:<user id>" or "g:<guest id>", stable across reconnects
	UserID  string // empty for guests
	Name    string
	IsAdmin bool
	IP      string
}

type Hub struct {
	store *store.Store
	log   *slog.Logger

	// HostGrace is how long a room waits for a disconnected host before ending.
	HostGrace time.Duration
	// RejoinGrace lets a participant whose connection dropped come back without the queue.
	RejoinGrace time.Duration

	// sfu forwards audio and video; nil disables media (tests of room logic only).
	sfu *sfu.SFU

	mu    sync.Mutex
	rooms map[string]*Room
}

// SetSFU enables audio and video forwarding.
func (h *Hub) SetSFU(s *sfu.SFU) { h.sfu = s }

func New(st *store.Store, log *slog.Logger) *Hub {
	return &Hub{
		store:       st,
		log:         log,
		HostGrace:   5 * time.Minute,
		RejoinGrace: 2 * time.Minute,
		rooms:       map[string]*Room{},
	}
}

// Restore prepares the hub after a process start: connections did not survive, so
// open participations are closed and every active room starts waiting for its host.
func (h *Hub) Restore(ctx context.Context) error {
	if err := h.store.CloseDanglingParticipants(ctx); err != nil {
		return err
	}
	rooms, err := h.store.ListRooms(ctx, store.RoomActive, 100000, 0)
	if err != nil {
		return err
	}
	for i := range rooms {
		h.Track(&rooms[i])
	}
	return nil
}

// Track returns the live room for an active database room, creating it if needed.
func (h *Hub) Track(sr *store.Room) *Room {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r, ok := h.rooms[sr.ID]; ok {
		return r
	}
	r := &Room{
		hub:     h,
		ID:      sr.ID,
		Slug:    sr.Slug,
		Name:    sr.Name,
		Max:     sr.MaxParticipants,
		clients: map[string]*Client{},
		recent:  map[string]time.Time{},
	}
	if sr.HostID != nil {
		r.hostID = *sr.HostID
	}
	if h.sfu != nil {
		r.media = h.sfu.NewRoom()
	}
	r.mu.Lock()
	r.startHostTimerLocked()
	r.mu.Unlock()
	h.rooms[sr.ID] = r
	return r
}

func (h *Hub) room(id string) *Room {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rooms[id]
}

// EndRoom finishes the room in the database and disconnects everybody.
// It returns store.ErrNotFound if the room was not active.
func (h *Hub) EndRoom(ctx context.Context, roomID, status, actorID string) error {
	err := h.store.FinishRoom(ctx, roomID, status, actorID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	reason := ReasonEnded
	if status == store.RoomClosedByAdmin {
		reason = ReasonClosedByAdmin
	}
	h.Shutdown(roomID, reason)
	return err
}

// Shutdown disconnects everybody from a room that is already finished in the database.
func (h *Hub) Shutdown(roomID, reason string) {
	h.mu.Lock()
	r := h.rooms[roomID]
	delete(h.rooms, roomID)
	h.mu.Unlock()
	if r != nil {
		r.shutdown(reason)
	}
}

// DisconnectUser removes a blocked or deleted user from every room.
func (h *Hub) DisconnectUser(userID string) {
	h.mu.Lock()
	rooms := make([]*Room, 0, len(h.rooms))
	for _, r := range h.rooms {
		rooms = append(rooms, r)
	}
	h.mu.Unlock()
	for _, r := range rooms {
		r.removeUser(userID)
	}
}

// Snapshot is the live view of a room for the admin panel.
type Snapshot struct {
	HostConnected bool              `json:"host_connected"`
	Participants  []ParticipantView `json:"participants"`
	Queue         []QueueView       `json:"queue"`
}

// Snapshot returns the live state of a room, or nil if nobody has opened it since start.
func (h *Hub) Snapshot(roomID string) *Snapshot {
	r := h.room(roomID)
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return &Snapshot{HostConnected: r.hostClientLocked() != nil, Participants: r.participantsLocked(), Queue: r.queueLocked()}
}

// Serve runs an authenticated WebSocket connection until it closes.
func (h *Hub) Serve(ctx context.Context, conn *websocket.Conn, sr *store.Room, id Identity) {
	r := h.Track(sr)
	c := newClient(r, conn, id)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.writeLoop(ctx)
	r.join(c)
	c.readLoop(ctx)
	r.disconnected(c)
}

func (h *Hub) dbCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}
