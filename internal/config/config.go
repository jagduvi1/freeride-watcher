// Package config loads application configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration.
type Config struct {
	// HTTP server
	ListenAddr string
	BaseURL    string

	// Persistence
	DBPath string

	// SMTP (optional — omit to disable email)
	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string
	SMTPFrom string

	// Web Push VAPID keys (generated and persisted in DB if not provided)
	VAPIDPublicKey  string
	VAPIDPrivateKey string
	VAPIDSubject    string // e.g. "mailto:admin@example.com"

	// Background fetcher
	FetchInterval time.Duration
	FetchJitter   time.Duration
	HertzAPIURL   string

	// Auth
	SessionMaxAge time.Duration
	ResetTokenTTL time.Duration
}

// Load reads configuration from environment variables with sane defaults.
func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr: env("LISTEN_ADDR", ":8080"),
		BaseURL:    env("BASE_URL", "http://localhost:8080"),
		DBPath:     env("DB_PATH", "/data/freeride.db"),

		SMTPHost: env("SMTP_HOST", ""),
		SMTPUser: env("SMTP_USER", ""),
		SMTPPass: env("SMTP_PASS", ""),
		SMTPFrom: env("SMTP_FROM", ""),

		VAPIDPublicKey:  env("VAPID_PUBLIC_KEY", ""),
		VAPIDPrivateKey: env("VAPID_PRIVATE_KEY", ""),
		VAPIDSubject:    env("VAPID_SUBJECT", "mailto:admin@example.com"),

		HertzAPIURL: env("HERTZ_API_URL",
			"https://www.hertzfreerider.se/api/transport-routes/?country=SWEDEN"),

		SessionMaxAge: 30 * 24 * time.Hour,
		ResetTokenTTL: 1 * time.Hour,
	}

	var err error

	cfg.SMTPPort, err = envInt("SMTP_PORT", 587)
	if err != nil {
		return nil, fmt.Errorf("SMTP_PORT: %w", err)
	}

	cfg.FetchInterval, err = envDuration("FETCH_INTERVAL", 5*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("FETCH_INTERVAL: %w", err)
	}

	cfg.FetchJitter, err = envDuration("FETCH_JITTER", 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("FETCH_JITTER: %w", err)
	}

	return cfg, nil
}

// SMTPConfigured returns true if enough SMTP settings are present to send mail.
func (c *Config) SMTPConfigured() bool {
	return c.SMTPHost != "" && c.SMTPFrom != ""
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	return strconv.Atoi(v)
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	return time.ParseDuration(v)
}
