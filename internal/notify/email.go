package notify

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jagduvi1/freeride-watcher/internal/config"
)

// Emailer sends transactional emails via the Mailgun HTTP API.
// No SDK required — a single POST to the Messages API is all it takes.
type Emailer struct {
	cfg    *config.Config
	client *http.Client
}

// NewEmailer creates an Emailer from cfg.
func NewEmailer(cfg *config.Config) *Emailer {
	return &Emailer{
		cfg:    cfg,
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

// Send posts a plain-text email through Mailgun.
// Returns nil immediately when Mailgun is not configured.
func (e *Emailer) Send(to, subject, body string) error {
	if !e.cfg.MailgunConfigured() {
		return nil
	}

	from := e.cfg.MailgunFrom
	if from == "" {
		from = "Freerider Watcher <noreply@" + e.cfg.MailgunDomain + ">"
	}

	apiBase := mailgunBase(e.cfg.MailgunRegion)
	endpoint := fmt.Sprintf("%s/v3/%s/messages", apiBase, e.cfg.MailgunDomain)

	form := url.Values{}
	form.Set("from", from)
	form.Set("to", to)
	form.Set("subject", subject)
	form.Set("text", body)

	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("mailgun: build request: %w", err)
	}
	req.SetBasicAuth("api", e.cfg.MailgunAPIKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("mailgun: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("mailgun: status %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	return nil
}

// mailgunBase returns the Mailgun API base URL for the given region.
func mailgunBase(region string) string {
	if strings.EqualFold(region, "eu") {
		return "https://api.eu.mailgun.net"
	}
	return "https://api.mailgun.net"
}
