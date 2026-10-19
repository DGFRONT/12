package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/dgfront/12/internal/auth"
	"github.com/dgfront/12/internal/hub"
	"github.com/dgfront/12/internal/store"
)

// handleGuestToken lets a visitor without an account enter a display name and get a token
// valid for this room only. The guest still has to wait in the queue.
func (s *Server) handleGuestToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DisplayName string `json:"display_name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.Join(strings.Fields(req.DisplayName), " ")
	if n := utf8.RuneCountInString(name); n < 1 || n > 64 {
		writeError(w, http.StatusBadRequest, "имя: от 1 до 64 символов")
		return
	}
	slug := chi.URLParam(r, "slug")
	room, err := s.store.RoomBySlug(r.Context(), slug)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "комната не найдена")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if room.Status != store.RoomActive {
		writeError(w, http.StatusGone, "конференция уже завершена")
		return
	}
	token, err := s.tokens.IssueGuest(auth.NewGuestID(), name, slug)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": token, "display_name": name})
}

// handleWS upgrades to WebSocket. The first message must be {"type":"auth","data":{"token":"..."}}
// carrying either a user access token or a guest token for this room.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// websocket.Accept rejects cross-origin requests by default.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()

	slug := chi.URLParam(r, "slug")
	authCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	_, data, err := conn.Read(authCtx)
	cancel()
	if err != nil {
		return
	}
	var msg struct {
		Type string `json:"type"`
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.Type != "auth" {
		wsFail(conn, "unauthorized", "сначала нужно представиться")
		return
	}

	id, failMsg := s.identify(r, msg.Data.Token, slug)
	if failMsg != "" {
		wsFail(conn, "unauthorized", failMsg)
		return
	}
	room, err := s.store.RoomBySlug(r.Context(), slug)
	if errors.Is(err, store.ErrNotFound) {
		wsFail(conn, "not_found", "комната не найдена")
		return
	}
	if err != nil {
		s.log.Error("load room", "err", err)
		wsFail(conn, "error", "внутренняя ошибка сервера")
		return
	}
	if room.Status != store.RoomActive {
		writeWS(conn, "room.closed", map[string]string{"reason": room.Status})
		conn.Close(websocket.StatusNormalClosure, "")
		return
	}
	s.hub.Serve(r.Context(), conn, room, id)
}

func (s *Server) identify(r *http.Request, token, slug string) (hub.Identity, string) {
	if claims, err := s.tokens.ParseAccess(token); err == nil {
		u, err := s.store.UserByID(r.Context(), claims.Subject)
		if err != nil || u.IsBlocked {
			return hub.Identity{}, "учётная запись недоступна"
		}
		return hub.Identity{
			Key: "u:" + u.ID, UserID: u.ID, Name: u.DisplayName, IsAdmin: u.Role == store.RoleAdmin, IP: clientIP(r),
		}, ""
	}
	if claims, err := s.tokens.ParseGuest(token); err == nil {
		if claims.Room != slug {
			return hub.Identity{}, "приглашение выдано для другой комнаты"
		}
		return hub.Identity{Key: "g:" + claims.Subject, Name: claims.Name, IP: clientIP(r)}, ""
	}
	return hub.Identity{}, "сессия истекла, войдите заново"
}

func writeWS(conn *websocket.Conn, typ string, data any) {
	b, _ := json.Marshal(map[string]any{"type": typ, "data": data})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn.Write(ctx, websocket.MessageText, b)
}

func wsFail(conn *websocket.Conn, code, message string) {
	writeWS(conn, "error", map[string]string{"code": code, "message": message})
	conn.Close(websocket.StatusPolicyViolation, code)
}
