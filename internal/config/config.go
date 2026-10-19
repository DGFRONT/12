// Package config loads service settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr      string
	DatabaseURL   string
	JWTSecret     []byte
	PublicURL     string
	CookieSecure  bool
	AdminUsername string
	AdminPassword string
	ChatRetention time.Duration
}

func Load() (Config, error) {
	c := Config{
		HTTPAddr:      env("HTTP_ADDR", ":8080"),
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		JWTSecret:     []byte(os.Getenv("JWT_SECRET")),
		PublicURL:     strings.TrimRight(env("PUBLIC_URL", "http://localhost:8080"), "/"),
		AdminUsername: os.Getenv("ADMIN_USERNAME"),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"),
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if len(c.JWTSecret) < 32 {
		return c, errors.New("JWT_SECRET must be at least 32 characters")
	}

	c.CookieSecure = strings.HasPrefix(c.PublicURL, "https://")
	if v := os.Getenv("COOKIE_SECURE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return c, fmt.Errorf("COOKIE_SECURE: %w", err)
		}
		c.CookieSecure = b
	}

	days, err := strconv.Atoi(env("CHAT_RETENTION_DAYS", "60"))
	if err != nil || days < 1 {
		return c, errors.New("CHAT_RETENTION_DAYS must be a positive integer")
	}
	c.ChatRetention = time.Duration(days) * 24 * time.Hour
	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
