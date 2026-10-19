package hub

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pion/webrtc/v4"

	"github.com/dgfront/12/internal/sfu"
	"github.com/dgfront/12/internal/store"
)

const (
	KindHost   = "host"
	KindMember = "member"
	KindGuest  = "guest"
	KindAdmin  = "admin"

	historyLimit = 200
)

var errRoomFull = errors.New("комната заполнена")

// Room is the live state of one active conference.
type Room struct {
	hub  *Hub
	ID   string
	Slug string
	Name string
	Max  int

	mu        sync.Mutex
	hostID    string
	clients   map[string]*Client   // by participant id, waiting and admitted
	recent    map[string]time.Time // identity key -> deadline to rejoin without the queue
	hostTimer *time.Timer
	seq       int64
	closed    bool

	media *sfu.Room // nil when media is disabled
}

// ParticipantView describes someone inside the room.
type ParticipantView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Audio      bool   `json:"audio"`
	Video      bool   `json:"video"`
	AllowAudio bool   `json:"allow_audio"`
	AllowVideo bool   `json:"allow_video"`
}

// QueueView describes someone waiting to be let in.
type QueueView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Registered bool   `json:"registered"`
}

type helloData struct {
	ParticipantID string `json:"participant_id"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	RoomName      string `json:"room_name"`
	Max           int    `json:"max"`
}

type admittedData struct {
	Participants []ParticipantView `json:"participants"`
	History      []store.Message   `json:"history"`
	Media        bool              `json:"media"`
	ICEServers   []sfu.ICEServer   `json:"ice_servers"`
}

type reasonData struct {
	Reason string `json:"reason"`
}

type targetData struct {
	ParticipantID string `json:"participant_id"`
}

// ---- membership ----

func (r *Room) join(c *Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		c.finishLocked("room.closed", reasonData{ReasonEnded})
		return
	}

	// One connection per identity: a new tab replaces the old one without losing the seat.
	for _, old := range r.clients {
		if old.id.Key == c.id.Key {
			if old.admitted {
				r.recent[c.id.Key] = time.Now().Add(r.hub.RejoinGrace)
			}
			r.removeLocked(old, ReasonReplaced)
		}
	}

	switch {
	case c.id.UserID != "" && c.id.UserID == r.hostID:
		c.kind = KindHost
	case c.id.IsAdmin:
		c.kind = KindAdmin
	case c.id.UserID != "":
		c.kind = KindMember
	default:
		c.kind = KindGuest
	}
	// Guests start muted and need the host's permission; everybody else may publish.
	c.allowAudio = c.kind != KindGuest
	c.allowVideo = c.kind != KindGuest

	ctx, cancel := r.hub.dbCtx()
	defer cancel()
	pid, err := r.hub.store.CreateParticipant(ctx, r.ID, c.id.UserID, c.id.Name, c.kind, c.id.IP)
	if err != nil {
		r.hub.log.Error("create participant", "room", r.ID, "err", err)
		c.finishLocked("error", map[string]string{"message": "внутренняя ошибка сервера"})
		return
	}
	c.pid = pid
	r.seq++
	c.seq = r.seq
	r.clients[pid] = c
	c.enqueueLocked("hello", helloData{ParticipantID: pid, Name: c.id.Name, Kind: c.kind, RoomName: r.Name, Max: r.Max})

	if c.kind == KindHost && r.hostTimer != nil {
		r.hostTimer.Stop()
		r.hostTimer = nil
	}

	rejoin := r.recent[c.id.Key].After(time.Now())
	delete(r.recent, c.id.Key)
	if c.kind == KindHost || c.kind == KindAdmin || rejoin {
		if err := r.admitLocked(c, ""); err == nil {
			return
		}
	}
	c.enqueueLocked("waiting", nil)
	r.broadcastQueueLocked()
}

// seatsTakenLocked counts admitted guests and members. One seat is always kept for the host
// and admins do not take seats.
func (r *Room) seatsTakenLocked() int {
	n := 0
	for _, c := range r.clients {
		if c.admitted && (c.kind == KindMember || c.kind == KindGuest) {
			n++
		}
	}
	return n
}

func (r *Room) admitLocked(c *Client, by string) error {
	if (c.kind == KindMember || c.kind == KindGuest) && r.seatsTakenLocked() >= r.Max-1 {
		return errRoomFull
	}
	ctx, cancel := r.hub.dbCtx()
	defer cancel()
	if err := r.hub.store.AdmitParticipant(ctx, c.pid, by); err != nil {
		r.hub.log.Error("admit participant", "room", r.ID, "err", err)
		return err
	}
	history, err := r.hub.store.ChatHistory(ctx, r.ID, historyLimit)
	if err != nil {
		r.hub.log.Error("chat history", "room", r.ID, "err", err)
		history = []store.Message{}
	}
	c.admitted = true
	data := admittedData{Participants: r.participantsLocked(), History: history, ICEServers: []sfu.ICEServer{}}
	if r.media != nil {
		data.Media = true
		data.ICEServers = r.hub.sfu.ICEServers(c.pid)
	}
	c.enqueueLocked("admitted", data)
	r.startMediaLocked(c)
	r.broadcastStateLocked()
	return nil
}

// startMediaLocked opens the participant's WebRTC session; the server sends the first offer.
func (r *Room) startMediaLocked(c *Client) {
	if r.media == nil {
		return
	}
	signal := func(typ string, data any) {
		r.mu.Lock()
		defer r.mu.Unlock()
		c.enqueueLocked(typ, data)
	}
	if err := r.media.AddPeer(c.pid, signal); err != nil {
		r.hub.log.Error("start media", "room", r.ID, "err", err)
	}
}

// leaveLocked forgets the client and records why it left.
func (r *Room) leaveLocked(c *Client, reason string) {
	if r.clients[c.pid] != c {
		return
	}
	delete(r.clients, c.pid)
	if r.media != nil {
		r.media.RemovePeer(c.pid)
	}
	ctx, cancel := r.hub.dbCtx()
	defer cancel()
	if err := r.hub.store.LeaveParticipant(ctx, c.pid, reason); err != nil {
		r.hub.log.Error("leave participant", "room", r.ID, "err", err)
	}
	if c.admitted && reason == "disconnected" {
		r.recent[c.id.Key] = time.Now().Add(r.hub.RejoinGrace)
	}
	if c.kind == KindHost {
		r.startHostTimerLocked()
	}
	r.broadcastStateLocked()
}

// removeLocked takes a client out of the room on the server's initiative and tells it why.
func (r *Room) removeLocked(c *Client, reason string) {
	r.leaveLocked(c, reason)
	c.finishLocked("removed", reasonData{reason})
}

// disconnected is called when the connection is gone.
func (r *Room) disconnected(c *Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leaveLocked(c, "disconnected")
	c.finishLocked("", nil)
}

func (r *Room) removeUser(userID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.clients {
		if c.id.UserID == userID {
			r.removeLocked(c, ReasonAccountDisabled)
		}
	}
}

func (r *Room) shutdown(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.hostTimer != nil {
		r.hostTimer.Stop()
		r.hostTimer = nil
	}
	if r.media != nil {
		r.media.Close()
	}
	// Participants rows were already closed by store.FinishRoom.
	for pid, c := range r.clients {
		delete(r.clients, pid)
		c.finishLocked("room.closed", reasonData{reason})
	}
}

func (r *Room) hostClientLocked() *Client {
	for _, c := range r.clients {
		if c.kind == KindHost && c.admitted {
			return c
		}
	}
	return nil
}

// startHostTimerLocked ends the room if the host stays away for HostGrace.
func (r *Room) startHostTimerLocked() {
	if r.closed || r.hostTimer != nil || r.hostClientLocked() != nil {
		return
	}
	r.hostTimer = time.AfterFunc(r.hub.HostGrace, func() {
		r.mu.Lock()
		hostBack := r.hostClientLocked() != nil
		r.hostTimer = nil
		r.mu.Unlock()
		if hostBack {
			return
		}
		ctx, cancel := r.hub.dbCtx()
		defer cancel()
		err := r.hub.store.FinishRoom(ctx, r.ID, store.RoomEnded, "")
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			r.hub.log.Error("end room after host left", "room", r.ID, "err", err)
			return
		}
		r.hub.Shutdown(r.ID, ReasonHostLeft)
	})
}

// ---- views and broadcasts ----

func (r *Room) sortedLocked(admitted bool) []*Client {
	list := make([]*Client, 0, len(r.clients))
	for _, c := range r.clients {
		if c.admitted == admitted {
			list = append(list, c)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].seq < list[j].seq })
	return list
}

func (r *Room) participantsLocked() []ParticipantView {
	list := []ParticipantView{}
	for _, c := range r.sortedLocked(true) {
		list = append(list, ParticipantView{
			ID: c.pid, Name: c.id.Name, Kind: c.kind,
			Audio: c.audio, Video: c.video, AllowAudio: c.allowAudio, AllowVideo: c.allowVideo,
		})
	}
	return list
}

func (r *Room) queueLocked() []QueueView {
	list := []QueueView{}
	for _, c := range r.sortedLocked(false) {
		list = append(list, QueueView{ID: c.pid, Name: c.id.Name, Registered: c.id.UserID != ""})
	}
	return list
}

func isManager(c *Client) bool {
	return c.admitted && (c.kind == KindHost || c.kind == KindAdmin)
}

func (r *Room) broadcastStateLocked() {
	participants := r.participantsLocked()
	for _, c := range r.clients {
		if c.admitted {
			c.enqueueLocked("participants", participants)
			// Media is forwarded only while it is switched on and allowed.
			if r.media != nil {
				r.media.SetGate(c.pid, c.audio && c.allowAudio, c.video && c.allowVideo)
			}
		}
	}
	r.broadcastQueueLocked()
}

func (r *Room) broadcastQueueLocked() {
	queue := r.queueLocked()
	for _, c := range r.clients {
		if isManager(c) {
			c.enqueueLocked("queue", queue)
		}
	}
}

func (r *Room) broadcastLocked(typ string, data any) {
	for _, c := range r.clients {
		if c.admitted {
			c.enqueueLocked(typ, data)
		}
	}
}

func (r *Room) sendError(c *Client, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c.enqueueLocked("error", map[string]string{"message": msg})
}

// ---- commands ----

func (r *Room) handle(c *Client, env envelope) {
	if env.Type == "room.end" {
		r.handleEnd(c)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.clients[c.pid] != c {
		return // already removed
	}
	fail := func(msg string) { c.enqueueLocked("error", map[string]string{"message": msg}) }
	decode := func(v any) bool {
		if err := json.Unmarshal(env.Data, v); err != nil {
			fail("некорректные данные")
			return false
		}
		return true
	}
	// target finds the participant a manager acts upon.
	target := func() *Client {
		var d targetData
		if !decode(&d) {
			return nil
		}
		t := r.clients[d.ParticipantID]
		if t == nil {
			fail("участник не найден")
		}
		return t
	}

	if env.Type == "leave" {
		r.leaveLocked(c, "left")
		c.finishLocked("", nil)
		return
	}
	if !c.admitted {
		fail("дождитесь, пока вас впустят")
		return
	}

	switch env.Type {
	case "rtc.answer":
		var d webrtc.SessionDescription
		if !decode(&d) || r.media == nil {
			return
		}
		if err := r.media.Answer(c.pid, d); err != nil {
			r.hub.log.Warn("apply answer", "participant", c.pid, "err", err)
		}

	case "rtc.candidate":
		var d webrtc.ICECandidateInit
		if !decode(&d) || r.media == nil {
			return
		}
		if err := r.media.Candidate(c.pid, d); err != nil {
			r.hub.log.Debug("add candidate", "participant", c.pid, "err", err)
		}

	case "rtc.restart":
		// The browser lost its media connection: start a fresh one.
		if r.media != nil {
			r.media.RemovePeer(c.pid)
			r.startMediaLocked(c)
			r.broadcastStateLocked()
		}

	case "chat.send":
		var d struct {
			Text string `json:"text"`
		}
		if !decode(&d) {
			return
		}
		text := strings.TrimSpace(d.Text)
		if text == "" || utf8.RuneCountInString(text) > maxChatRune {
			fail("сообщение должно быть от 1 до 2000 символов")
			return
		}
		if !c.allowChatLocked(time.Now()) {
			fail("слишком часто, подождите секунду")
			return
		}
		ctx, cancel := r.hub.dbCtx()
		defer cancel()
		m, err := r.hub.store.CreateMessage(ctx, r.ID, c.pid, text)
		if err != nil {
			r.hub.log.Error("create message", "room", r.ID, "err", err)
			fail("не удалось отправить сообщение")
			return
		}
		r.broadcastLocked("chat.message", m)

	case "media.toggle":
		var d struct {
			Kind    string `json:"kind"`
			Enabled bool   `json:"enabled"`
		}
		if !decode(&d) {
			return
		}
		switch {
		case d.Kind == "audio" && (!d.Enabled || c.allowAudio):
			c.audio = d.Enabled
		case d.Kind == "video" && (!d.Enabled || c.allowVideo):
			c.video = d.Enabled
		case d.Kind == "audio" || d.Kind == "video":
			fail("организатор ещё не разрешил вам включать микрофон или камеру")
			return
		default:
			fail("неизвестный тип медиа")
			return
		}
		r.broadcastStateLocked()

	default:
		if !isManager(c) {
			fail("доступно только организатору")
			return
		}
		r.handleManagerLocked(c, env, fail, decode, target)
	}
}

func (r *Room) handleManagerLocked(c *Client, env envelope, fail func(string), decode func(any) bool, target func() *Client) {
	notice := func(t *Client, text string) { t.enqueueLocked("notice", map[string]string{"text": text}) }
	// The host may not act on an administrator.
	protected := func(t *Client) bool {
		if c.kind == KindHost && t.kind == KindAdmin {
			fail("нельзя применить к администратору")
			return true
		}
		return false
	}

	switch env.Type {
	case "queue.admit":
		t := target()
		if t == nil {
			return
		}
		if t.admitted {
			fail("участник уже в комнате")
			return
		}
		if err := r.admitLocked(t, c.id.UserID); err != nil {
			fail("не удалось впустить: " + err.Error())
		}

	case "queue.admit_all":
		for _, t := range r.sortedLocked(false) {
			if err := r.admitLocked(t, c.id.UserID); err != nil {
				fail("впущены не все: " + err.Error())
				return
			}
		}

	case "queue.reject":
		t := target()
		if t == nil {
			return
		}
		if t.admitted {
			fail("участник уже в комнате")
			return
		}
		r.removeLocked(t, ReasonRejected)

	case "participant.kick":
		t := target()
		if t == nil || protected(t) {
			return
		}
		if t == c || t.kind == KindHost {
			fail("нельзя выгнать организатора")
			return
		}
		r.removeLocked(t, ReasonKicked)

	case "media.mute":
		var d struct {
			ParticipantID string `json:"participant_id"`
			Kind          string `json:"kind"`
		}
		if !decode(&d) {
			return
		}
		t := r.clients[d.ParticipantID]
		if t == nil || !t.admitted {
			fail("участник не найден")
			return
		}
		if protected(t) {
			return
		}
		switch d.Kind {
		case "audio":
			t.audio = false
			notice(t, "Организатор выключил ваш микрофон")
		case "video":
			t.video = false
			notice(t, "Организатор выключил вашу камеру")
		default:
			fail("неизвестный тип медиа")
			return
		}
		r.broadcastStateLocked()

	case "media.permission":
		var d struct {
			ParticipantID string `json:"participant_id"`
			Audio         *bool  `json:"audio"`
			Video         *bool  `json:"video"`
		}
		if !decode(&d) {
			return
		}
		t := r.clients[d.ParticipantID]
		if t == nil || !t.admitted {
			fail("участник не найден")
			return
		}
		if t.kind != KindGuest {
			fail("разрешения настраиваются только для гостей")
			return
		}
		if d.Audio != nil {
			t.allowAudio = *d.Audio
			t.audio = t.audio && t.allowAudio
		}
		if d.Video != nil {
			t.allowVideo = *d.Video
			t.video = t.video && t.allowVideo
		}
		// The guest's client turns on newly allowed devices by itself; the browser asks for consent.
		t.enqueueLocked("permission", map[string]any{"audio": d.Audio, "video": d.Video})
		r.broadcastStateLocked()

	case "chat.delete":
		var d struct {
			ID int64 `json:"id"`
		}
		if !decode(&d) {
			return
		}
		ctx, cancel := r.hub.dbCtx()
		defer cancel()
		if err := r.hub.store.DeleteMessage(ctx, r.ID, d.ID); err != nil {
			fail("сообщение не найдено")
			return
		}
		r.broadcastLocked("chat.deleted", map[string]int64{"id": d.ID})

	case "host.transfer":
		t := target()
		if t == nil {
			return
		}
		if !t.admitted || t.kind != KindMember {
			fail("права можно передать только зарегистрированному участнику в комнате")
			return
		}
		ctx, cancel := r.hub.dbCtx()
		defer cancel()
		if err := r.hub.store.SetRoomHost(ctx, r.ID, t.id.UserID); err != nil {
			r.hub.log.Error("set room host", "room", r.ID, "err", err)
			fail("не удалось передать права")
			return
		}
		if old := r.hostClientLocked(); old != nil {
			old.kind = KindMember
			if err := r.hub.store.SetParticipantKind(ctx, old.pid, KindMember); err != nil {
				r.hub.log.Error("set participant kind", "err", err)
			}
		}
		t.kind = KindHost
		if err := r.hub.store.SetParticipantKind(ctx, t.pid, KindHost); err != nil {
			r.hub.log.Error("set participant kind", "err", err)
		}
		r.hostID = t.id.UserID
		if r.hostTimer != nil {
			r.hostTimer.Stop()
			r.hostTimer = nil
		}
		notice(t, "Вы стали организатором комнаты")
		r.broadcastStateLocked()

	default:
		fail("неизвестная команда " + env.Type)
	}
}

func (r *Room) handleEnd(c *Client) {
	r.mu.Lock()
	allowed := r.clients[c.pid] == c && isManager(c)
	r.mu.Unlock()
	if !allowed {
		r.sendError(c, "доступно только организатору")
		return
	}
	ctx, cancel := r.hub.dbCtx()
	defer cancel()
	if err := r.hub.EndRoom(ctx, r.ID, store.RoomEnded, c.id.UserID); err != nil && !errors.Is(err, store.ErrNotFound) {
		r.hub.log.Error("end room", "room", r.ID, "err", err)
		r.sendError(c, "не удалось завершить комнату")
	}
}
