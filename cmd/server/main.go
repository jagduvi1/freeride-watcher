// Command server is the entry point for freeride-watcher.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jagduvi1/freeride-watcher/internal/config"
	"github.com/jagduvi1/freeride-watcher/internal/db"
	"github.com/jagduvi1/freeride-watcher/internal/fetcher"
	"github.com/jagduvi1/freeride-watcher/internal/notify"
	"github.com/jagduvi1/freeride-watcher/internal/web"
)

func main() {
	// Structured logging to stdout.
	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})))

	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	database, err := db.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer database.Close()

	// Load or generate VAPID keys (persisted in the DB so they survive container restarts).
	if err := initVAPID(cfg, database); err != nil {
		return fmt.Errorf("vapid: %w", err)
	}

	pusher := notify.NewPusher(cfg)
	emailer := notify.NewEmailer(cfg)

	f := fetcher.New(cfg, database, pusher, emailer)
	srv := web.NewServer(cfg, database, pusher, emailer, f)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start background fetcher.
	go f.Start(ctx)

	slog.Info("server starting", "addr", cfg.ListenAddr, "base_url", cfg.BaseURL)
	return srv.Start(ctx)
}

// initVAPID loads VAPID keys from env → DB → generates new ones.
// Generated keys are stored in the DB and logged so operators can persist them in .env.
func initVAPID(cfg *config.Config, database *db.DB) error {
	// 1. Env vars take priority.
	if cfg.VAPIDPublicKey != "" && cfg.VAPIDPrivateKey != "" {
		return nil
	}

	// 2. Try loading from DB.
	pub, _ := database.GetConfig("vapid_public_key")
	priv, _ := database.GetConfig("vapid_private_key")
	if pub != "" && priv != "" {
		cfg.VAPIDPublicKey = pub
		cfg.VAPIDPrivateKey = priv
		slog.Info("VAPID keys loaded from database")
		return nil
	}

	// 3. Generate and persist.
	privKey, pubKey, err := notify.GenerateVAPIDKeys()
	if err != nil {
		return fmt.Errorf("generate VAPID keys: %w", err)
	}
	if err := database.SetConfig("vapid_public_key", pubKey); err != nil {
		return err
	}
	if err := database.SetConfig("vapid_private_key", privKey); err != nil {
		return err
	}
	cfg.VAPIDPublicKey = pubKey
	cfg.VAPIDPrivateKey = privKey

	slog.Info("VAPID keys generated and saved to database")
	// Print keys directly to stderr (not via slog) so they don't appear as
	// structured fields in log aggregators. Copy them to .env to persist
	// across DB resets.
	fmt.Fprintf(os.Stderr, strings.Join([]string{
		"",
		"=== VAPID keys generated — save these to your .env ===",
		"VAPID_PUBLIC_KEY=" + pubKey,
		"VAPID_PRIVATE_KEY=" + privKey,
		"======================================================",
		"",
	}, "\n"))
	return nil
}
