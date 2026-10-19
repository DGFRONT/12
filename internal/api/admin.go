package api

import (
	"archive/zip"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/dgfront/12/internal/auth"
	"github.com/dgfront/12/internal/store"
)

var (
	usernameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]{3,32}$`)
	uuidRe     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

const minPasswordLen = 8

func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context(), strings.TrimSpace(r.URL.Query().Get("q")))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, users)
}

type createUserRequest struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
	Role        string `json:"role"`
}

// handleAdminCreateUser registers a new account. If no password is given, one is generated
// and returned once in the response so the admin can hand it over.
func (s *Server) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	if req.Role == "" {
		req.Role = store.RoleUser
	}
	if msg := validateUser(req.Username, req.DisplayName, req.Role); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	generated := ""
	if req.Password == "" {
		generated = auth.GeneratePassword(14)
		req.Password = generated
	} else if utf8.RuneCountInString(req.Password) < minPasswordLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("пароль должен быть не короче %d символов", minPasswordLen))
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	admin := currentUser(r)
	u, err := s.store.CreateUser(r.Context(), store.NewUser{
		Username: req.Username, DisplayName: req.DisplayName, PasswordHash: hash, Role: req.Role, CreatedBy: &admin.ID,
	})
	if errors.Is(err, store.ErrDuplicate) {
		writeError(w, http.StatusConflict, "логин уже занят")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "user.create", "user", u.ID, map[string]string{"username": u.Username, "role": u.Role})
	writeJSON(w, http.StatusCreated, map[string]any{"user": u, "generated_password": generated})
}

func validateUser(username, displayName, role string) string {
	if !usernameRe.MatchString(username) {
		return "логин: 3–32 символа, латиница, цифры, точка, дефис, подчёркивание"
	}
	if n := utf8.RuneCountInString(displayName); n < 1 || n > 64 {
		return "отображаемое имя: от 1 до 64 символов"
	}
	if role != store.RoleAdmin && role != store.RoleUser {
		return "роль должна быть admin или user"
	}
	return ""
}

type updateUserRequest struct {
	DisplayName   *string `json:"display_name"`
	Password      *string `json:"password"`
	ResetPassword bool    `json:"reset_password"`
	Role          *string `json:"role"`
	IsBlocked     *bool   `json:"is_blocked"`
}

func (s *Server) handleAdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	var req updateUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id := chi.URLParam(r, "id")
	target, ok := s.loadUser(w, r, id)
	if !ok {
		return
	}
	admin := currentUser(r)

	var patch store.UserPatch
	changes := map[string]any{}
	if req.DisplayName != nil {
		name := strings.TrimSpace(*req.DisplayName)
		if n := utf8.RuneCountInString(name); n < 1 || n > 64 {
			writeError(w, http.StatusBadRequest, "отображаемое имя: от 1 до 64 символов")
			return
		}
		patch.DisplayName = &name
		changes["display_name"] = name
	}
	if req.Role != nil {
		if *req.Role != store.RoleAdmin && *req.Role != store.RoleUser {
			writeError(w, http.StatusBadRequest, "роль должна быть admin или user")
			return
		}
		patch.Role = req.Role
		changes["role"] = *req.Role
	}
	if req.IsBlocked != nil {
		patch.IsBlocked = req.IsBlocked
		changes["is_blocked"] = *req.IsBlocked
	}
	demoting := target.Role == store.RoleAdmin &&
		((req.Role != nil && *req.Role != store.RoleAdmin) || (req.IsBlocked != nil && *req.IsBlocked))
	if demoting {
		if target.ID == admin.ID {
			writeError(w, http.StatusBadRequest, "нельзя снять права или заблокировать самого себя")
			return
		}
		if !s.otherAdminsRemain(w, r, target) {
			return
		}
	}

	generated := ""
	if req.ResetPassword {
		generated = auth.GeneratePassword(14)
		req.Password = &generated
	}
	if req.Password != nil {
		if utf8.RuneCountInString(*req.Password) < minPasswordLen {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("пароль должен быть не короче %d символов", minPasswordLen))
			return
		}
		hash, err := auth.HashPassword(*req.Password)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		patch.PasswordHash = &hash
		changes["password"] = "changed"
	}

	u, err := s.store.UpdateUser(r.Context(), id, patch)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	// Force re-login after a password change or a block.
	if patch.PasswordHash != nil || (patch.IsBlocked != nil && *patch.IsBlocked) {
		if err := s.store.RevokeUserTokens(r.Context(), id); err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	s.audit(r, "user.update", "user", id, changes)
	writeJSON(w, http.StatusOK, map[string]any{"user": u, "generated_password": generated})
}

func (s *Server) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	target, ok := s.loadUser(w, r, id)
	if !ok {
		return
	}
	admin := currentUser(r)
	if target.ID == admin.ID {
		writeError(w, http.StatusBadRequest, "нельзя удалить самого себя")
		return
	}
	if target.Role == store.RoleAdmin && !s.otherAdminsRemain(w, r, target) {
		return
	}
	if err := s.store.DeleteUser(r.Context(), id, admin.ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "user.delete", "user", id, map[string]string{"username": target.Username})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) loadUser(w http.ResponseWriter, r *http.Request, id string) (*store.User, bool) {
	var u *store.User
	err := store.ErrNotFound
	if uuidRe.MatchString(id) {
		u, err = s.store.UserByID(r.Context(), id)
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "пользователь не найден")
		return nil, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return nil, false
	}
	return u, true
}

// otherAdminsRemain makes sure removing admin rights from target keeps at least one active admin.
func (s *Server) otherAdminsRemain(w http.ResponseWriter, r *http.Request, target *store.User) bool {
	n, err := s.store.CountAdmins(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return false
	}
	if !target.IsBlocked {
		n--
	}
	if n < 1 {
		writeError(w, http.StatusBadRequest, "нельзя оставить систему без администратора")
		return false
	}
	return true
}

func (s *Server) handleAdminListRooms(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status != "" && status != store.RoomActive && status != store.RoomEnded && status != store.RoomClosedByAdmin {
		writeError(w, http.StatusBadRequest, "неизвестный статус")
		return
	}
	rooms, err := s.store.ListRooms(r.Context(), status, queryInt(r, "limit", 100, 1, 500), queryInt(r, "offset", 0, 0, 1<<30))
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

// handleAdminRoom returns a room with its participants and chat archive.
func (s *Server) handleAdminRoom(w http.ResponseWriter, r *http.Request) {
	room, ok := s.loadRoom(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	participants, err := s.store.RoomParticipants(r.Context(), room.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	messages, err := s.store.RoomMessages(r.Context(), room.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"room": s.roomURL(room), "participants": participants, "messages": messages,
	})
}

func (s *Server) handleAdminCloseRoom(w http.ResponseWriter, r *http.Request) {
	room, ok := s.loadRoom(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	err := s.store.FinishRoom(r.Context(), room.ID, store.RoomClosedByAdmin, currentUser(r).ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusConflict, "комната уже завершена")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "room.close", "room", room.ID, map[string]string{"name": room.Name})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) loadRoom(w http.ResponseWriter, r *http.Request, id string) (*store.Room, bool) {
	var room *store.Room
	err := store.ErrNotFound
	if uuidRe.MatchString(id) {
		room, err = s.store.RoomByID(r.Context(), id)
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "комната не найдена")
		return nil, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return nil, false
	}
	return room, true
}

func (s *Server) handleAdminTables(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, store.TableNames())
}

func (s *Server) handleAdminTable(w http.ResponseWriter, r *http.Request) {
	page, err := s.store.BrowseTable(r.Context(), chi.URLParam(r, "name"),
		queryInt(r, "limit", 50, 1, 500), queryInt(r, "offset", 0, 0, 1<<30))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "таблица недоступна")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	to := time.Now()
	from := to.AddDate(0, 0, -30)
	if v := r.URL.Query().Get("from"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "from: формат ГГГГ-ММ-ДД")
			return
		}
		from = t
	}
	if v := r.URL.Query().Get("to"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "to: формат ГГГГ-ММ-ДД")
			return
		}
		to = t.AddDate(0, 0, 1) // inclusive end date
	}
	if !from.Before(to) || to.Sub(from) > 366*24*time.Hour {
		writeError(w, http.StatusBadRequest, "период должен быть от 1 до 366 дней")
		return
	}
	st, err := s.store.Stats(r.Context(), from, to)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListAudit(r.Context(), queryInt(r, "limit", 100, 1, 500), queryInt(r, "offset", 0, 0, 1<<30))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleAdminExport re-checks the admin's password and streams a ZIP with every browsable table as JSON.
func (s *Server) handleAdminExport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	admin := currentUser(r)
	key := "export|" + admin.ID
	if !s.limiter.Allowed(key) {
		writeError(w, http.StatusTooManyRequests, "слишком много неудачных попыток, попробуйте через 15 минут")
		return
	}
	ok, err := auth.CheckPassword(admin.PasswordHash, req.Password)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !ok {
		s.limiter.Fail(key)
		s.audit(r, "data.export.denied", "", "", nil)
		writeError(w, http.StatusForbidden, "неверный пароль")
		return
	}
	s.limiter.Reset(key)
	s.audit(r, "data.export", "", "", map[string]any{"tables": store.TableNames()})

	name := "export-" + time.Now().UTC().Format("20060102-150405") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	zw := zip.NewWriter(w)
	for _, table := range store.TableNames() {
		f, err := zw.Create(table + ".json")
		if err == nil {
			err = s.store.ExportTable(r.Context(), table, f)
		}
		if err != nil {
			// Headers are already sent; log and abort, the client gets a truncated archive.
			s.log.Error("export failed", "table", table, "err", err)
			return
		}
	}
	if err := zw.Close(); err != nil {
		s.log.Error("export failed", "err", err)
	}
}
