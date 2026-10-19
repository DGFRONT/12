package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dgfront/12/internal/auth"
	"github.com/dgfront/12/internal/store"
)

const refreshCookie = "refresh_token"

var dummyHash = func() string {
	h, err := auth.HashPassword("dummy password for timing equalization")
	if err != nil {
		panic(err)
	}
	return h
}()

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type sessionResponse struct {
	AccessToken string      `json:"access_token"`
	ExpiresAt   time.Time   `json:"expires_at"`
	User        *store.User `json:"user"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	key := clientIP(r) + "|" + strings.ToLower(req.Username)
	if !s.limiter.Allowed(key) {
		writeError(w, http.StatusTooManyRequests, "слишком много неудачных попыток, попробуйте через 15 минут")
		return
	}

	u, err := s.store.UserByUsername(r.Context(), req.Username)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, err)
		return
	}
	// Hash even for unknown users so response time does not reveal which logins exist.
	hash := dummyHash
	if u != nil {
		hash = u.PasswordHash
	}
	ok, err := auth.CheckPassword(hash, req.Password)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	ok = ok && u != nil
	if !ok {
		s.limiter.Fail(key)
		writeError(w, http.StatusUnauthorized, "неверный логин или пароль")
		return
	}
	if u.IsBlocked {
		writeError(w, http.StatusForbidden, "учётная запись заблокирована")
		return
	}
	s.limiter.Reset(key)
	if err := s.store.TouchLogin(r.Context(), u.ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.startSession(w, r, u)
}

// handleRefresh exchanges the refresh cookie for a new access token and rotates the cookie.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(refreshCookie)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "требуется вход")
		return
	}
	userID, err := s.store.ConsumeRefreshToken(r.Context(), auth.HashRefreshToken(c.Value))
	if errors.Is(err, store.ErrNotFound) {
		s.clearRefreshCookie(w)
		writeError(w, http.StatusUnauthorized, "сессия истекла")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	u, err := s.store.UserByID(r.Context(), userID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && u.IsBlocked) {
		s.clearRefreshCookie(w)
		writeError(w, http.StatusUnauthorized, "учётная запись недоступна")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.startSession(w, r, u)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(refreshCookie); err == nil {
		if _, err := s.store.ConsumeRefreshToken(r.Context(), auth.HashRefreshToken(c.Value)); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, err)
			return
		}
	}
	s.clearRefreshCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, currentUser(r))
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *store.User) {
	access, exp, err := s.tokens.IssueAccess(u.ID, u.Role)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	refresh, hash := auth.NewRefreshToken()
	if err := s.store.CreateRefreshToken(r.Context(), u.ID, hash, time.Now().Add(auth.RefreshTTL)); err != nil {
		s.internalError(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookie,
		Value:    refresh,
		Path:     "/api/auth",
		MaxAge:   int(auth.RefreshTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, sessionResponse{AccessToken: access, ExpiresAt: exp, User: u})
}

func (s *Server) clearRefreshCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: refreshCookie, Value: "", Path: "/api/auth", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode,
	})
}
