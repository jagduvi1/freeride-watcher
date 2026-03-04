// Package config loads application configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"time"
)

// Config holds all runtime configuration.
type Config struct {
	// HTTP server
	ListenAddr string
	BaseURL    string

	// Persistence
	DBPath string

	// Mailgun (optional — omit to disable email fallback)
	MailgunAPIKey string
	MailgunDomain string
	MailgunFrom   string
	MailgunRegion string // "us" (default) or "eu"

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

		MailgunAPIKey: env("MAILGUN_API_KEY", ""),
		MailgunDomain: env("MAILGUN_DOMAIN", ""),
		MailgunFrom:   env("MAILGUN_FROM", ""),
		MailgunRegion: env("MAILGUN_REGION", "us"),

		VAPIDPublicKey:  env("VAPID_PUBLIC_KEY", ""),
		VAPIDPrivateKey: env("VAPID_PRIVATE_KEY", ""),
		VAPIDSubject:    env("VAPID_SUBJECT", "mailto:admin@example.com"),

		HertzAPIURL: env("HERTZ_API_URL",
			"https://www.hertzfreerider.se/api/transport-routes/?country=SWEDEN"),

		SessionMaxAge: 30 * 24 * time.Hour,
		ResetTokenTTL: 1 * time.Hour,
	}

	var err error

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

// MailgunConfigured returns true when the minimum Mailgun settings are present.
func (c *Config) MailgunConfigured() bool {
	return c.MailgunAPIKey != "" && c.MailgunDomain != ""
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	return time.ParseDuration(v)
}
