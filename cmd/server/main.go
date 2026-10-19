package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dgfront/12/internal/api"
	"github.com/dgfront/12/internal/auth"
	"github.com/dgfront/12/internal/config"
	"github.com/dgfront/12/internal/hub"
	"github.com/dgfront/12/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := openStore(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	if err := bootstrapAdmin(ctx, st, cfg, log); err != nil {
		return err
	}
	go maintenance(ctx, st, cfg.ChatRetention, log)

	h := hub.New(st, log)
	if err := h.Restore(ctx); err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.New(cfg, st, h, log).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr, "public_url", cfg.PublicURL)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// openStore retries while the database container is still starting.
func openStore(ctx context.Context, url string, log *slog.Logger) (*store.Store, error) {
	for attempt := 1; ; attempt++ {
		st, err := store.Open(ctx, url)
		if err == nil {
			return st, nil
		}
		if attempt == 30 {
			return nil, err
		}
		log.Warn("database not ready, retrying", "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// bootstrapAdmin creates the first administrator from ADMIN_USERNAME/ADMIN_PASSWORD when no admin exists.
func bootstrapAdmin(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	n, err := st.CountAdmins(ctx)
	if err != nil || n > 0 {
		return err
	}
	if cfg.AdminUsername == "" || len(cfg.AdminPassword) < 8 {
		return errors.New("no administrator exists: set ADMIN_USERNAME and ADMIN_PASSWORD (8+ characters) for the first start")
	}
	hash, err := auth.HashPassword(cfg.AdminPassword)
	if err != nil {
		return err
	}
	u, err := st.CreateUser(ctx, store.NewUser{
		Username: cfg.AdminUsername, DisplayName: "Администратор", PasswordHash: hash, Role: store.RoleAdmin,
	})
	if errors.Is(err, store.ErrDuplicate) {
		return errors.New("ADMIN_USERNAME is taken by a non-admin or blocked account; choose another name")
	}
	if err != nil {
		return err
	}
	log.Info("created initial administrator", "username", u.Username)
	return st.Audit(ctx, "", "admin.bootstrap", "user", u.ID, nil, "")
}

// maintenance purges expired chat archives and refresh tokens once an hour.
func maintenance(ctx context.Context, st *store.Store, retention time.Duration, log *slog.Logger) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		if n, err := st.PurgeOldChats(ctx, retention); err != nil {
			log.Error("chat purge failed", "err", err)
		} else if n > 0 {
			log.Info("purged archived chat messages", "count", n)
		}
		if err := st.DeleteExpiredTokens(ctx); err != nil {
			log.Error("token cleanup failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
