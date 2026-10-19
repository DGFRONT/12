package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dgfront/12/internal/auth"
	"github.com/dgfront/12/internal/hub"
	"github.com/dgfront/12/internal/store"
)

type wsMsg struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type wsClient struct {
	t    *testing.T
	name string
	conn *websocket.Conn
	msgs chan wsMsg
	// hook, if set, sees every message first and consumes it by returning true.
	hook func(wsMsg) bool
}

func dialRoom(t *testing.T, base, slug, token, name string) *wsClient {
	t.Helper()
	return dialRoomWith(t, base, slug, token, name, nil)
}

func dialRoomWith(t *testing.T, base, slug, token, name string, hook func(*wsClient, wsMsg) bool) *wsClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws/rooms/"+slug, nil)
	if err != nil {
		t.Fatalf("%s: dial: %v", name, err)
	}
	c := &wsClient{t: t, name: name, conn: conn, msgs: make(chan wsMsg, 256)}
	if hook != nil {
		c.hook = func(m wsMsg) bool { return hook(c, m) }
	}
	go func() {
		defer close(c.msgs)
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var m wsMsg
			json.Unmarshal(data, &m)
			if c.hook != nil && c.hook(m) {
				continue
			}
			c.msgs <- m
		}
	}()
	t.Cleanup(func() { conn.CloseNow() })
	c.send("auth", map[string]string{"token": token})
	return c
}

func (c *wsClient) send(typ string, data any) {
	c.t.Helper()
	if err := c.trySend(typ, data); err != nil {
		c.t.Fatalf("%s: write %s: %v", c.name, typ, err)
	}
}

// trySend is safe to call from other goroutines.
func (c *wsClient) trySend(typ string, data any) error {
	b, _ := json.Marshal(map[string]any{"type": typ, "data": data})
	return c.conn.Write(context.Background(), websocket.MessageText, b)
}

// waitFor skips other messages until one of the given type arrives and decodes it into out.
func (c *wsClient) waitFor(typ string, out any) {
	c.t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m, ok := <-c.msgs:
			if !ok {
				c.t.Fatalf("%s: connection closed while waiting for %s", c.name, typ)
			}
			if m.Type == typ {
				if out != nil {
					if err := json.Unmarshal(m.Data, out); err != nil {
						c.t.Fatalf("%s: decode %s: %v", c.name, typ, err)
					}
				}
				return
			}
		case <-deadline:
			c.t.Fatalf("%s: timeout waiting for %s", c.name, typ)
		}
	}
}

// waitParticipants waits for a participants update satisfying ok.
func (c *wsClient) waitParticipants(ok func([]hub.ParticipantView) bool) []hub.ParticipantView {
	c.t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m, open := <-c.msgs:
			if !open {
				c.t.Fatalf("%s: closed while waiting for participants", c.name)
			}
			var list []hub.ParticipantView
			if m.Type == "participants" && json.Unmarshal(m.Data, &list) == nil && ok(list) {
				return list
			}
			if m.Type == "admitted" {
				var a struct {
					Participants []hub.ParticipantView `json:"participants"`
				}
				if json.Unmarshal(m.Data, &a) == nil && ok(a.Participants) {
					return a.Participants
				}
			}
		case <-deadline:
			c.t.Fatalf("%s: timeout waiting for participants", c.name)
		}
	}
}

func (c *wsClient) waitClosed() {
	c.t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-c.msgs:
			if !ok {
				return
			}
		case <-deadline:
			c.t.Fatalf("%s: connection not closed", c.name)
		}
	}
}

func (c *wsClient) expectError(contains string) {
	c.t.Helper()
	var e struct {
		Message string `json:"message"`
	}
	c.waitFor("error", &e)
	if !strings.Contains(e.Message, contains) {
		c.t.Fatalf("%s: error %q does not contain %q", c.name, e.Message, contains)
	}
}

type hello struct {
	ParticipantID string `json:"participant_id"`
	Kind          string `json:"kind"`
	Name          string `json:"name"`
}

func newUser(t *testing.T, st *store.Store, base, username string) *client {
	t.Helper()
	hash, _ := auth.HashPassword("password123")
	if _, err := st.CreateUser(context.Background(), store.NewUser{Username: username, DisplayName: strings.ToUpper(username[:1]) + username[1:], PasswordHash: hash, Role: store.RoleUser}); err != nil {
		t.Fatal(err)
	}
	return login(t, base, username, "password123")
}

func guestToken(t *testing.T, base, slug, name string) string {
	var g struct {
		Token       string `json:"token"`
		DisplayName string `json:"display_name"`
	}
	(&client{t: t, base: base}).expect(200, "POST", "/api/rooms/"+slug+"/guest", map[string]string{"display_name": name}, &g)
	return g.Token
}

func find(list []hub.ParticipantView, id string) *hub.ParticipantView {
	for i := range list {
		if list[i].ID == id {
			return &list[i]
		}
	}
	return nil
}

func TestLiveRoom(t *testing.T) {
	_, st, base := setup(t)
	ctx := context.Background()
	admin := login(t, base, "root", "admin-password")
	anna := newUser(t, st, base, "anna")
	boris := newUser(t, st, base, "boris")
	vera := newUser(t, st, base, "vera")

	var room roomResponse
	anna.expect(201, "POST", "/api/rooms", map[string]string{"name": "Созвон"}, &room)

	// Host enters without the queue.
	host := dialRoom(t, base, room.Slug, anna.token, "host")
	var h hello
	host.waitFor("hello", &h)
	if h.Kind != hub.KindHost {
		t.Fatalf("host kind %q", h.Kind)
	}
	host.waitFor("admitted", nil)

	// Guest token is bound to one room and the name is normalized.
	var g struct {
		Token       string `json:"token"`
		DisplayName string `json:"display_name"`
	}
	(&client{t: t, base: base}).expect(200, "POST", "/api/rooms/"+room.Slug+"/guest", map[string]string{"display_name": "  Петя   Гость "}, &g)
	if g.DisplayName != "Петя Гость" {
		t.Fatalf("guest name %q", g.DisplayName)
	}
	var other roomResponse
	boris.expect(201, "POST", "/api/rooms", map[string]string{"name": "Другая"}, &other)
	wrong := dialRoom(t, base, other.Slug, g.Token, "wrong-room")
	wrong.expectError("другой комнаты")
	wrong.waitClosed()

	// Guest and a registered user both wait in the queue.
	guest := dialRoom(t, base, room.Slug, g.Token, "guest")
	var gh hello
	guest.waitFor("hello", &gh)
	guest.waitFor("waiting", nil)
	guest.send("chat.send", map[string]string{"text": "можно войти?"})
	guest.expectError("дождитесь")

	bor := dialRoom(t, base, room.Slug, boris.token, "boris")
	var bh hello
	bor.waitFor("hello", &bh)
	if bh.Kind != hub.KindMember {
		t.Fatalf("boris kind %q", bh.Kind)
	}
	bor.waitFor("waiting", nil)
	var queue []hub.QueueView
	for len(queue) < 2 {
		host.waitFor("queue", &queue)
	}
	if queue[0].Name != "Петя Гость" || queue[0].Registered || !queue[1].Registered {
		t.Fatalf("queue %+v", queue)
	}

	// Only managers manage the queue.
	host.send("queue.admit", map[string]string{"participant_id": gh.ParticipantID})
	guest.waitFor("admitted", nil)
	guest.send("queue.admit", map[string]string{"participant_id": bh.ParticipantID})
	guest.expectError("только организатору")

	host.send("queue.reject", map[string]string{"participant_id": bh.ParticipantID})
	var removed struct{ Reason string }
	bor.waitFor("removed", &removed)
	if removed.Reason != hub.ReasonRejected {
		t.Fatalf("boris removed with %q", removed.Reason)
	}
	bor.waitClosed()

	// Chat goes to everyone inside.
	host.send("chat.send", map[string]string{"text": "Привет всем"})
	var m store.Message
	guest.waitFor("chat.message", &m)
	if m.Body != "Привет всем" || m.AuthorKind != hub.KindHost {
		t.Fatalf("chat %+v", m)
	}
	guest.send("chat.send", map[string]string{"text": "Здравствуйте"})
	host.waitFor("chat.message", &m) // the host's own message comes back first
	host.waitFor("chat.message", &m)
	if m.Author != "Петя Гость" || m.AuthorKind != hub.KindGuest {
		t.Fatalf("chat author %+v", m)
	}
	host.send("chat.send", map[string]string{"text": strings.Repeat("я", 2001)})
	host.expectError("2000")

	// Guests cannot turn on devices until the host allows it.
	guest.send("media.toggle", map[string]any{"kind": "audio", "enabled": true})
	guest.expectError("не разрешил")
	host.send("media.permission", map[string]any{"participant_id": gh.ParticipantID, "audio": true})
	guest.waitFor("permission", nil)
	guest.send("media.toggle", map[string]any{"kind": "audio", "enabled": true})
	host.waitParticipants(func(l []hub.ParticipantView) bool {
		p := find(l, gh.ParticipantID)
		return p != nil && p.Audio && p.AllowAudio && !p.AllowVideo
	})
	// ...and may always turn them off themselves.
	guest.send("media.toggle", map[string]any{"kind": "audio", "enabled": false})
	host.waitParticipants(func(l []hub.ParticipantView) bool { p := find(l, gh.ParticipantID); return p != nil && !p.Audio })
	guest.send("media.toggle", map[string]any{"kind": "audio", "enabled": true})
	host.waitParticipants(func(l []hub.ParticipantView) bool { p := find(l, gh.ParticipantID); return p != nil && p.Audio })
	host.send("media.mute", map[string]any{"participant_id": gh.ParticipantID, "kind": "audio"})
	guest.waitFor("notice", nil)
	host.waitParticipants(func(l []hub.ParticipantView) bool { p := find(l, gh.ParticipantID); return p != nil && !p.Audio })
	host.send("media.permission", map[string]any{"participant_id": gh.ParticipantID, "audio": false})
	guest.waitFor("permission", nil)
	guest.send("media.toggle", map[string]any{"kind": "audio", "enabled": true})
	guest.expectError("не разрешил")

	// The admin enters without the queue, is visible, and the host cannot act on them.
	adm := dialRoom(t, base, room.Slug, admin.token, "admin")
	var ah hello
	adm.waitFor("hello", &ah)
	if ah.Kind != hub.KindAdmin {
		t.Fatalf("admin kind %q", ah.Kind)
	}
	adm.waitFor("admitted", nil)
	host.waitParticipants(func(l []hub.ParticipantView) bool {
		p := find(l, ah.ParticipantID)
		return p != nil && p.Kind == hub.KindAdmin
	})
	host.send("participant.kick", map[string]string{"participant_id": ah.ParticipantID})
	host.expectError("администратору")

	// The admin panel sees the live state without joining.
	var detail struct {
		Live hub.Snapshot `json:"live"`
	}
	admin.expect(200, "GET", "/api/admin/rooms/"+room.ID, nil, &detail)
	if !detail.Live.HostConnected || len(detail.Live.Participants) != 3 {
		t.Fatalf("live snapshot %+v", detail.Live)
	}

	// A late participant gets the chat history.
	ver := dialRoom(t, base, room.Slug, vera.token, "vera")
	var vh hello
	ver.waitFor("hello", &vh)
	ver.waitFor("waiting", nil)
	adm.send("queue.admit", map[string]string{"participant_id": vh.ParticipantID})
	var adm2 struct {
		History []store.Message `json:"history"`
	}
	ver.waitFor("admitted", &adm2)
	if len(adm2.History) != 2 || adm2.History[0].Body != "Привет всем" {
		t.Fatalf("history %+v", adm2.History)
	}

	// Host hands over to a registered participant; guests cannot become hosts.
	host.send("host.transfer", map[string]string{"participant_id": gh.ParticipantID})
	host.expectError("зарегистрированному")
	host.send("host.transfer", map[string]string{"participant_id": vh.ParticipantID})
	ver.waitFor("notice", nil)
	host.waitParticipants(func(l []hub.ParticipantView) bool {
		return find(l, vh.ParticipantID).Kind == hub.KindHost && find(l, h.ParticipantID).Kind == hub.KindMember
	})
	veraUser, _ := st.UserByUsername(ctx, "vera")
	r, _ := st.RoomByID(ctx, room.ID)
	if r.HostID == nil || *r.HostID != veraUser.ID {
		t.Fatalf("host not stored: %+v", r)
	}
	host.send("room.end", nil)
	host.expectError("только организатору")

	// Kicked participants leave.
	ver.send("participant.kick", map[string]string{"participant_id": gh.ParticipantID})
	guest.waitFor("removed", &removed)
	if removed.Reason != hub.ReasonKicked {
		t.Fatalf("guest removed with %q", removed.Reason)
	}

	// New host ends the conference for everyone.
	ver.send("room.end", nil)
	for _, c := range []*wsClient{host, adm, ver} {
		var closed struct{ Reason string }
		c.waitFor("room.closed", &closed)
		if closed.Reason != hub.ReasonEnded {
			t.Fatalf("%s: closed with %q", c.name, closed.Reason)
		}
		c.waitClosed()
	}
	r, _ = st.RoomByID(ctx, room.ID)
	if r.Status != store.RoomEnded || r.Online != 0 {
		t.Fatalf("room after end: %+v", r)
	}
	// The link is dead now.
	(&client{t: t, base: base}).expect(410, "POST", "/api/rooms/"+room.Slug+"/guest", map[string]string{"display_name": "x"}, nil)
	late := dialRoom(t, base, room.Slug, anna.token, "late")
	late.waitFor("room.closed", nil)
}

func TestRoomCapacityAndReconnect(t *testing.T) {
	_, st, base := setup(t)
	ctx := context.Background()
	admin := login(t, base, "root", "admin-password")
	anna := newUser(t, st, base, "anna")
	annaUser, _ := st.UserByUsername(ctx, "anna")
	// A room for 3: the host plus two seats.
	room, err := st.CreateRoom(ctx, annaUser.ID, "Маленькая", "smallroom123", 3)
	if err != nil {
		t.Fatal(err)
	}

	host := dialRoom(t, base, room.Slug, anna.token, "host")
	host.waitFor("admitted", nil)

	tokens := []string{guestToken(t, base, room.Slug, "Гость 1"), guestToken(t, base, room.Slug, "Гость 2"), guestToken(t, base, room.Slug, "Гость 3")}
	var guests []*wsClient
	var ids []string
	for i, tok := range tokens {
		c := dialRoom(t, base, room.Slug, tok, "guest"+string(rune('1'+i)))
		var gh hello
		c.waitFor("hello", &gh)
		c.waitFor("waiting", nil)
		guests = append(guests, c)
		ids = append(ids, gh.ParticipantID)
	}
	host.send("queue.admit_all", nil)
	guests[0].waitFor("admitted", nil)
	guests[1].waitFor("admitted", nil)
	host.expectError("заполнена")

	// Admins do not need a seat.
	adm := dialRoom(t, base, room.Slug, admin.token, "admin")
	adm.waitFor("admitted", nil)

	// A dropped connection may come back without the queue; a second tab replaces the first.
	guests[0].conn.CloseNow()
	host.waitParticipants(func(l []hub.ParticipantView) bool { return find(l, ids[0]) == nil })
	again := dialRoom(t, base, room.Slug, tokens[0], "guest1-again")
	again.waitFor("admitted", nil)
	tab2 := dialRoom(t, base, room.Slug, tokens[0], "guest1-tab2")
	tab2.waitFor("admitted", nil)
	var removed struct{ Reason string }
	again.waitFor("removed", &removed)
	if removed.Reason != hub.ReasonReplaced {
		t.Fatalf("old tab removed with %q", removed.Reason)
	}

	// Blocking an account disconnects it at once.
	borisClient := newUser(t, st, base, "boris")
	boris, _ := st.UserByUsername(ctx, "boris")
	bor := dialRoom(t, base, room.Slug, borisClient.token, "boris")
	bor.waitFor("waiting", nil)
	admin.expect(200, "PATCH", "/api/admin/users/"+boris.ID, map[string]any{"is_blocked": true}, nil)
	bor.waitFor("removed", &removed)
	if removed.Reason != hub.ReasonAccountDisabled {
		t.Fatalf("blocked user removed with %q", removed.Reason)
	}

	// Closing from the admin panel disconnects everyone.
	admin.expect(204, "POST", "/api/admin/rooms/"+room.ID+"/close", nil, nil)
	var closed struct{ Reason string }
	host.waitFor("room.closed", &closed)
	if closed.Reason != hub.ReasonClosedByAdmin {
		t.Fatalf("closed with %q", closed.Reason)
	}
	guests[2].waitFor("room.closed", nil)
}

func TestRoomEndsWhenHostGone(t *testing.T) {
	_, st, base := setup(t)
	ctx := context.Background()
	anna := newUser(t, st, base, "anna")
	var room roomResponse
	anna.expect(201, "POST", "/api/rooms", map[string]string{"name": "Ушла"}, &room)

	host := dialRoom(t, base, room.Slug, anna.token, "host")
	host.waitFor("admitted", nil)
	guest := dialRoom(t, base, room.Slug, guestToken(t, base, room.Slug, "Гость"), "guest")
	var gh hello
	guest.waitFor("hello", &gh)
	host.send("queue.admit", map[string]string{"participant_id": gh.ParticipantID})
	guest.waitFor("admitted", nil)

	// Host comes back within the grace period: nothing happens.
	host.conn.CloseNow()
	host = dialRoom(t, base, room.Slug, anna.token, "host-again")
	host.waitFor("admitted", nil)
	time.Sleep(400 * time.Millisecond) // HostGrace in tests is 300ms
	if r, _ := st.RoomByID(ctx, room.ID); r.Status != store.RoomActive {
		t.Fatalf("room ended although host returned: %s", r.Status)
	}

	// Host leaves for good: the room ends after the grace period.
	host.send("leave", nil)
	var closed struct{ Reason string }
	guest.waitFor("room.closed", &closed)
	if closed.Reason != hub.ReasonHostLeft {
		t.Fatalf("closed with %q", closed.Reason)
	}
	r, _ := st.RoomByID(ctx, room.ID)
	if r.Status != store.RoomEnded || r.EndedBy != nil {
		t.Fatalf("room after host left: %+v", r)
	}
}
