package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dgfront/12/internal/auth"
	"github.com/dgfront/12/internal/config"
	"github.com/dgfront/12/internal/store"
)

// These tests need a disposable PostgreSQL database: TEST_DATABASE_URL=postgres://... go test ./...
// The public schema of that database is wiped.
func setup(t *testing.T) (*httptest.Server, *store.Store, string) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)

	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil { // idempotent
		t.Fatal(err)
	}
	hash, _ := auth.HashPassword("admin-password")
	if _, err := st.CreateUser(ctx, store.NewUser{Username: "root", DisplayName: "Root", PasswordHash: hash, Role: store.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{JWTSecret: []byte("test-secret-test-secret-test-secret"), PublicURL: "https://meet.test"}
	srv := httptest.NewServer(New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil))).Routes())
	t.Cleanup(srv.Close)
	return srv, st, srv.URL
}

type client struct {
	t       *testing.T
	base    string
	token   string
	refresh string
}

func (c *client) do(method, path string, body any) (*http.Response, []byte) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.refresh != "" {
		req.AddCookie(&http.Cookie{Name: refreshCookie, Value: c.refresh})
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	for _, ck := range res.Cookies() {
		if ck.Name == refreshCookie {
			c.refresh = ck.Value
		}
	}
	return res, data
}

func (c *client) expect(status int, method, path string, body any, out any) {
	c.t.Helper()
	res, data := c.do(method, path, body)
	if res.StatusCode != status {
		c.t.Fatalf("%s %s: status %d, want %d: %s", method, path, res.StatusCode, status, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			c.t.Fatalf("%s %s: decode: %v: %s", method, path, err, data)
		}
	}
}

func login(t *testing.T, base, username, password string) *client {
	t.Helper()
	c := &client{t: t, base: base}
	var s sessionResponse
	c.expect(200, "POST", "/api/auth/login", loginRequest{username, password}, &s)
	c.token = s.AccessToken
	return c
}

func TestAdminAndRoomsFlow(t *testing.T) {
	_, st, base := setup(t)
	ctx := context.Background()

	anon := &client{t: t, base: base}
	anon.expect(401, "POST", "/api/auth/login", loginRequest{"root", "wrong"}, nil)
	anon.expect(401, "POST", "/api/auth/login", loginRequest{"nobody", "whatever"}, nil)
	anon.expect(401, "GET", "/api/me", nil, nil)

	admin := login(t, base, "ROOT", "admin-password") // usernames are case-insensitive

	// Admin registers a user with a generated password.
	var created struct {
		User              store.User `json:"user"`
		GeneratedPassword string     `json:"generated_password"`
	}
	admin.expect(201, "POST", "/api/admin/users", createUserRequest{Username: "ivan", DisplayName: "Иван"}, &created)
	if len(created.GeneratedPassword) != 14 || created.User.Role != store.RoleUser {
		t.Fatalf("unexpected create response %+v", created)
	}
	admin.expect(409, "POST", "/api/admin/users", createUserRequest{Username: "Ivan", DisplayName: "dup", Password: "password123"}, nil)
	admin.expect(400, "POST", "/api/admin/users", createUserRequest{Username: "x", DisplayName: "short"}, nil)

	user := login(t, base, "ivan", created.GeneratedPassword)
	user.expect(403, "GET", "/api/admin/users", nil, nil)

	// Refresh rotates the cookie; the old one stops working.
	old := user.refresh
	var s sessionResponse
	user.expect(200, "POST", "/api/auth/refresh", nil, &s)
	if user.refresh == old || s.AccessToken == "" {
		t.Fatal("refresh did not rotate")
	}
	replay := &client{t: t, base: base, refresh: old}
	replay.expect(401, "POST", "/api/auth/refresh", nil, nil)

	// User creates a room; the invite page works without login.
	var room roomResponse
	user.expect(201, "POST", "/api/rooms", map[string]string{"name": "Планёрка"}, &room)
	if !strings.HasPrefix(room.URL, "https://meet.test/r/") || len(room.Slug) != 12 || room.MaxParticipants != 10 {
		t.Fatalf("unexpected room %+v", room)
	}
	var pub map[string]any
	anon.expect(200, "GET", "/api/rooms/"+room.Slug, nil, &pub)
	if pub["name"] != "Планёрка" || pub["status"] != "active" {
		t.Fatalf("unexpected public room %+v", pub)
	}
	anon.expect(404, "GET", "/api/rooms/nope", nil, nil)

	// Admin sees and closes the room without joining it.
	var active []roomResponse
	admin.expect(200, "GET", "/api/admin/rooms?status=active", nil, &active)
	if len(active) != 1 || active[0].ID != room.ID {
		t.Fatalf("active rooms %+v", active)
	}
	admin.expect(204, "POST", "/api/admin/rooms/"+room.ID+"/close", nil, nil)
	admin.expect(409, "POST", "/api/admin/rooms/"+room.ID+"/close", nil, nil)
	admin.expect(404, "POST", "/api/admin/rooms/not-a-uuid/close", nil, nil)
	user.expect(409, "POST", "/api/rooms/"+room.Slug+"/end", nil, nil)
	anon.expect(200, "GET", "/api/rooms/"+room.Slug, nil, &pub)
	if pub["status"] != store.RoomClosedByAdmin {
		t.Fatalf("room not closed: %+v", pub)
	}

	// Host ends their own room; another user cannot.
	var room2 roomResponse
	user.expect(201, "POST", "/api/rooms", map[string]string{"name": "Вторая"}, &room2)
	admin.expect(403, "POST", "/api/rooms/"+room2.Slug+"/end", nil, nil)
	user.expect(204, "POST", "/api/rooms/"+room2.Slug+"/end", nil, nil)

	// Admin safety rails.
	admin.expect(400, "PATCH", "/api/admin/users/"+adminID(t, st), map[string]any{"role": "user"}, nil)
	admin.expect(400, "DELETE", "/api/admin/users/"+adminID(t, st), nil, nil)

	// Promote ivan, then ivan may manage admins too.
	admin.expect(200, "PATCH", "/api/admin/users/"+created.User.ID, map[string]any{"role": "admin"}, nil)
	user.expect(200, "GET", "/api/admin/users", nil, nil)
	admin.expect(200, "PATCH", "/api/admin/users/"+created.User.ID, map[string]any{"role": "user"}, nil)

	// Blocking takes effect immediately, even for a still-valid access token.
	admin.expect(200, "PATCH", "/api/admin/users/"+created.User.ID, map[string]any{"is_blocked": true}, nil)
	user.expect(401, "GET", "/api/me", nil, nil)
	anon.expect(403, "POST", "/api/auth/login", loginRequest{"ivan", created.GeneratedPassword}, nil)

	// Data browser never exposes secrets.
	var page store.TablePage
	admin.expect(200, "GET", "/api/admin/tables/users", nil, &page)
	if page.Total != 2 || bytes.Contains(bytes.Join(rawRows(page), nil), []byte("password")) {
		t.Fatalf("users table page %+v", page)
	}
	admin.expect(404, "GET", "/api/admin/tables/refresh_tokens", nil, nil)

	var stats store.Stats
	admin.expect(200, "GET", "/api/admin/stats", nil, &stats)
	if stats.UsersTotal != 2 || stats.RoomsCreated != 2 || stats.UsersBlocked != 1 {
		t.Fatalf("stats %+v", stats)
	}

	// Export requires the admin's password.
	admin.expect(403, "POST", "/api/admin/export", map[string]string{"password": "nope"}, nil)
	res, data := admin.do("POST", "/api/admin/export", map[string]string{"password": "admin-password"})
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("export status %d: %s", res.StatusCode, data)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != len(store.TableNames()) {
		t.Fatalf("zip has %d files", len(zr.File))
	}
	for _, f := range zr.File {
		rc, _ := f.Open()
		var rows []map[string]any
		if err := json.NewDecoder(rc).Decode(&rows); err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		rc.Close()
		if f.Name == "users.json" && (len(rows) != 2 || rows[0]["password_hash"] != nil) {
			t.Fatalf("users export %+v", rows)
		}
	}

	var log []store.AuditEntry
	admin.expect(200, "GET", "/api/admin/audit", nil, &log)
	actions := map[string]bool{}
	for _, e := range log {
		actions[e.Action] = true
	}
	for _, a := range []string{"user.create", "user.update", "room.create", "room.close", "room.end", "data.export", "data.export.denied"} {
		if !actions[a] {
			t.Errorf("audit log misses %s", a)
		}
	}

	// Deleting a user closes their active rooms.
	var room3 roomResponse
	hash, _ := auth.HashPassword("password123")
	petr, _ := st.CreateUser(ctx, store.NewUser{Username: "petr", DisplayName: "Пётр", PasswordHash: hash, Role: store.RoleUser})
	p := login(t, base, "petr", "password123")
	p.expect(201, "POST", "/api/rooms", map[string]string{"name": "Третья"}, &room3)
	admin.expect(204, "DELETE", "/api/admin/users/"+petr.ID, nil, nil)
	r3, err := st.RoomByID(ctx, room3.ID)
	if err != nil || r3.Status != store.RoomClosedByAdmin || r3.OwnerID != nil {
		t.Fatalf("room of deleted user: %+v %v", r3, err)
	}
}

func TestChatRetention(t *testing.T) {
	_, st, _ := setup(t)
	ctx := context.Background()
	owner, err := st.UserByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	oldRoom, _ := st.CreateRoom(ctx, owner.ID, "old", "oldoldoldold", 10)
	newRoom, _ := st.CreateRoom(ctx, owner.ID, "new", "newnewnewnew", 10)
	for _, id := range []string{oldRoom.ID, newRoom.ID} {
		if err := st.FinishRoom(ctx, id, store.RoomEnded, owner.ID); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, id := range []string{oldRoom.ID, newRoom.ID} {
		if _, err := conn.Exec(ctx, `
			WITH p AS (INSERT INTO participants (room_id, display_name, kind, ip) VALUES ($1, 'Гость', 'guest', '10.0.0.1') RETURNING id)
			INSERT INTO messages (room_id, participant_id, body) SELECT $1, id, 'привет' FROM p`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(ctx, `UPDATE rooms SET ended_at = now() - interval '61 days' WHERE id = $1`, oldRoom.ID); err != nil {
		t.Fatal(err)
	}

	n, err := st.PurgeOldChats(ctx, 60*24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("purged %d, err %v", n, err)
	}
	if msgs, _ := st.RoomMessages(ctx, newRoom.ID); len(msgs) != 1 {
		t.Fatal("recent chat archive was purged")
	}
	var ips int
	conn.QueryRow(ctx, `SELECT count(ip) FROM participants`).Scan(&ips)
	if ips != 1 {
		t.Fatalf("expected only the recent guest IP to remain, got %d", ips)
	}
}

func adminID(t *testing.T, st *store.Store) string {
	u, err := st.UserByUsername(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func rawRows(p store.TablePage) [][]byte {
	out := make([][]byte, len(p.Rows))
	for i, r := range p.Rows {
		out[i] = r
	}
	return out
}
