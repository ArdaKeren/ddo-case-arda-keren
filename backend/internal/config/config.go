// Package config loads runtime settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// DefaultSeedAdminEmail is the first user created in a fresh system. The
// .invalid TLD (RFC 2606) guarantees it can never receive real mail.
const DefaultSeedAdminEmail = "seed.dalga@ddo-case.invalid"

type Config struct {
	DatabaseURL string
	HTTPAddr    string

	JWTSecret  []byte
	JWTIssuer  string
	AccessTTL  time.Duration
	RefreshTTL time.Duration

	// IANA zone that defines the business day for ticket numbering.
	QueueTimezone string

	// Host patterns allowed to open cross-origin WebSockets, e.g. "localhost:5173".
	AllowedOrigins []string
	// How long queue events stay replayable for reconnecting clients.
	EventRetention time.Duration

	// First-admin bootstrap. The email defaults to the seed user; nothing is
	// created unless ADMIN_PASSWORD is also set.
	AdminEmail    string
	AdminPassword string
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL:   getenv("DATABASE_URL", "postgres://qa:qa_secret@localhost:5432/qa?sslmode=disable"),
		HTTPAddr:      getenv("HTTP_ADDR", ":8080"),
		JWTSecret:     []byte(os.Getenv("JWT_SECRET")),
		JWTIssuer:     getenv("JWT_ISSUER", "qa-backend"),
		QueueTimezone: getenv("QUEUE_TIMEZONE", "Europe/Istanbul"),
		AdminEmail:    getenv("ADMIN_EMAIL", DefaultSeedAdminEmail),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"),
	}

	if len(c.JWTSecret) < 32 {
		return c, errors.New("JWT_SECRET must be set and at least 32 bytes long")
	}

	if _, err := time.LoadLocation(c.QueueTimezone); err != nil {
		return c, fmt.Errorf("QUEUE_TIMEZONE: %w", err)
	}

	var err error
	if c.AccessTTL, err = duration("ACCESS_TOKEN_TTL", 15*time.Minute); err != nil {
		return c, err
	}
	if c.RefreshTTL, err = duration("REFRESH_TOKEN_TTL", 30*24*time.Hour); err != nil {
		return c, err
	}
	if c.EventRetention, err = duration("EVENT_RETENTION", 48*time.Hour); err != nil {
		return c, err
	}
	for _, o := range strings.Split(getenv("ALLOWED_ORIGINS", "localhost:5173,127.0.0.1:5173"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.AllowedOrigins = append(c.AllowedOrigins, o)
		}
	}
	return c, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func duration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}
