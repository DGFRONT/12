// Package api exposes the REST API and serves the web frontend.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/dgfront/12/internal/auth"
	"github.com/dgfront/12/internal/config"
	"github.com/dgfront/12/internal/hub"
	"github.com/dgfront/12/internal/store"
	"github.com/dgfront/12/web"
)

const MaxRoomParticipants = 10

type Server struct {
	cfg     config.Config
	store   *store.Store
	hub     *hub.Hub
	tokens  *auth.Tokens
	limiter *auth.LoginLimiter
	log     *slog.Logger
}

func New(cfg config.Config, st *store.Store, h *hub.Hub, log *slog.Logger) *Server {
	return &Server{
		cfg:     cfg,
		store:   st,
		hub:     h,
		tokens:  auth.NewTokens(cfg.JWTSecret),
		limiter: auth.NewLoginLimiter(5, 15*time.Minute),
		log:     log,
	}
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RealIP, middleware.Recoverer, securityHeaders)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.Ping(r.Context()); err != nil {
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})

	r.Route("/api", func(r chi.Router) {
		r.Use(middleware.NoCache)
		r.Post("/auth/login", s.handleLogin)
		r.Post("/auth/refresh", s.handleRefresh)
		r.Post("/auth/logout", s.handleLogout)
		r.Get("/rooms/{slug}", s.handleRoomPublic)
		r.Post("/rooms/{slug}/guest", s.handleGuestToken)

		r.Group(func(r chi.Router) {
			r.Use(s.requireUser)
			r.Get("/me", s.handleMe)
			r.Get("/rooms", s.handleMyRooms)
			r.Post("/rooms", s.handleCreateRoom)
			r.Post("/rooms/{slug}/end", s.handleEndRoom)
		})

		r.Route("/admin", func(r chi.Router) {
			r.Use(s.requireUser, requireAdmin)
			r.Get("/users", s.handleAdminListUsers)
			r.Post("/users", s.handleAdminCreateUser)
			r.Patch("/users/{id}", s.handleAdminUpdateUser)
			r.Delete("/users/{id}", s.handleAdminDeleteUser)
			r.Get("/rooms", s.handleAdminListRooms)
			r.Get("/rooms/{id}", s.handleAdminRoom)
			r.Post("/rooms/{id}/close", s.handleAdminCloseRoom)
			r.Get("/tables", s.handleAdminTables)
			r.Get("/tables/{name}", s.handleAdminTable)
			r.Get("/stats", s.handleAdminStats)
			r.Get("/audit", s.handleAdminAudit)
			r.Post("/export", s.handleAdminExport)
		})
	})

	r.Get("/ws/rooms/{slug}", s.handleWS)
	r.Handle("/static/*", web.Static())
	r.Get("/", web.Page("login.html"))
	r.Get("/app", web.Page("app.html"))
	r.Get("/admin", web.Page("admin.html"))
	r.Get("/r/{slug}", web.Page("room.html"))
	return r
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

type ctxKey int

const userKey ctxKey = 0

// requireUser authenticates the bearer access token and loads the current user from the database,
// so blocking, deletion or a role change takes effect immediately.
func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			writeError(w, http.StatusUnauthorized, "требуется вход")
			return
		}
		claims, err := s.tokens.ParseAccess(token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "сессия истекла")
			return
		}
		u, err := s.store.UserByID(r.Context(), claims.Subject)
		if errors.Is(err, store.ErrNotFound) || (err == nil && u.IsBlocked) {
			writeError(w, http.StatusUnauthorized, "учётная запись недоступна")
			return
		}
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	})
}

func requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if currentUser(r).Role != store.RoleAdmin {
			writeError(w, http.StatusForbidden, "доступно только администратору")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func currentUser(r *http.Request) *store.User {
	return r.Context().Value(userKey).(*store.User)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if net.ParseIP(host) == nil {
		return ""
	}
	return host
}

func (s *Server) audit(r *http.Request, action, targetType, targetID string, meta any) {
	actor := ""
	if u, ok := r.Context().Value(userKey).(*store.User); ok {
		actor = u.ID
	}
	if err := s.store.Audit(r.Context(), actor, action, targetType, targetID, meta, clientIP(r)); err != nil {
		s.log.Error("audit log write failed", "action", action, "err", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "внутренняя ошибка сервера")
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return false
	}
	return true
}

func queryInt(r *http.Request, key string, def, min, max int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return def
	}
	return clamp(v, min, max)
}

func clamp(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
