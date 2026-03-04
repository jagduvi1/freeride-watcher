package notify

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/jagduvi1/freeride-watcher/internal/config"
	"github.com/jagduvi1/freeride-watcher/internal/db"
)

// PushPayload is the JSON body delivered to the service worker.
type PushPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
}

// Pusher sends Web Push notifications via VAPID.
type Pusher struct {
	cfg *config.Config
}

// NewPusher creates a Pusher from cfg.
func NewPusher(cfg *config.Config) *Pusher {
	return &Pusher{cfg: cfg}
}

// Configured returns true when VAPID keys are set.
func (p *Pusher) Configured() bool {
	return p.cfg.VAPIDPublicKey != "" && p.cfg.VAPIDPrivateKey != ""
}

// Send delivers a push notification to a single subscription.
// It returns a non-nil error only for transient failures.
// On 410 Gone (subscription expired) it returns ErrGone so the caller can delete the record.
func (p *Pusher) Send(sub db.PushSubscription, payload PushPayload) error {
	if !p.Configured() {
		return nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	resp, err := webpush.SendNotification(data, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys: webpush.Keys{
			P256dh: sub.P256dh,
			Auth:   sub.AuthKey,
		},
	}, &webpush.Options{
		Subscriber:      p.cfg.VAPIDSubject,
		VAPIDPublicKey:  p.cfg.VAPIDPublicKey,
		VAPIDPrivateKey: p.cfg.VAPIDPrivateKey,
		TTL:             86400, // 24 h
	})
	if err != nil {
		return fmt.Errorf("send push: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusGone:
		return ErrGone
	default:
		slog.Warn("push unexpected status", "status", resp.StatusCode, "endpoint", sub.Endpoint)
		return nil
	}
}

// ErrGone is returned when the push endpoint no longer exists (HTTP 410).
var ErrGone = fmt.Errorf("push subscription gone (410)")

// GenerateVAPIDKeys generates a new VAPID key pair and returns (privateKey, publicKey).
func GenerateVAPIDKeys() (private, public string, err error) {
	return webpush.GenerateVAPIDKeys()
}
