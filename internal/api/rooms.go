package api

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/dgfront/12/internal/auth"
	"github.com/dgfront/12/internal/store"
)

type roomResponse struct {
	*store.Room
	URL string `json:"url"`
}

func (s *Server) roomURL(room *store.Room) roomResponse {
	return roomResponse{Room: room, URL: s.cfg.PublicURL + "/r/" + room.Slug}
}

func (s *Server) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || utf8.RuneCountInString(req.Name) > 100 {
		writeError(w, http.StatusBadRequest, "название комнаты: от 1 до 100 символов")
		return
	}
	u := currentUser(r)
	for range 3 { // retry on the astronomically unlikely slug collision
		room, err := s.store.CreateRoom(r.Context(), u.ID, req.Name, auth.GenerateSlug(), MaxRoomParticipants)
		if errors.Is(err, store.ErrDuplicate) {
			continue
		}
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		s.audit(r, "room.create", "room", room.ID, map[string]string{"name": room.Name})
		writeJSON(w, http.StatusCreated, s.roomURL(room))
		return
	}
	s.internalError(w, r, errors.New("could not generate unique room slug"))
}

func (s *Server) handleMyRooms(w http.ResponseWriter, r *http.Request) {
	rooms, err := s.store.RoomsForUser(r.Context(), currentUser(r).ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]roomResponse, len(rooms))
	for i := range rooms {
		out[i] = s.roomURL(&rooms[i])
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRoomPublic returns what the invite page needs; it works without login for guests.
func (s *Server) handleRoomPublic(w http.ResponseWriter, r *http.Request) {
	room, err := s.store.RoomBySlug(r.Context(), chi.URLParam(r, "slug"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "комната не найдена")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":             room.Name,
		"status":           room.Status,
		"host_name":        room.HostName,
		"max_participants": room.MaxParticipants,
		"online":           room.Online,
	})
}

// handleEndRoom lets the current host finish the conference for everyone.
func (s *Server) handleEndRoom(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	room, err := s.store.RoomBySlug(r.Context(), chi.URLParam(r, "slug"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "комната не найдена")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if room.HostID == nil || *room.HostID != u.ID {
		writeError(w, http.StatusForbidden, "завершить комнату может только её хост")
		return
	}
	if err := s.store.FinishRoom(r.Context(), room.ID, store.RoomEnded, u.ID); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusConflict, "комната уже завершена")
		return
	} else if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "room.end", "room", room.ID, nil)
	w.WriteHeader(http.StatusNoContent)
}
