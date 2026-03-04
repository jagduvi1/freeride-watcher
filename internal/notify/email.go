// Package notify sends push and email notifications to users.
package notify

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"

	"github.com/jagduvi1/freeride-watcher/internal/config"
)

// Emailer sends transactional emails via SMTP.
type Emailer struct {
	cfg *config.Config
}

// NewEmailer creates an Emailer from cfg.
func NewEmailer(cfg *config.Config) *Emailer {
	return &Emailer{cfg: cfg}
}

// Send sends a plain-text email. Returns nil if SMTP is not configured (skips silently).
func (e *Emailer) Send(to, subject, body string) error {
	if !e.cfg.SMTPConfigured() {
		return nil // gracefully skip when SMTP not configured
	}

	msg := buildMessage(e.cfg.SMTPFrom, to, subject, body)
	addr := fmt.Sprintf("%s:%d", e.cfg.SMTPHost, e.cfg.SMTPPort)

	var auth smtp.Auth
	if e.cfg.SMTPUser != "" {
		auth = smtp.PlainAuth("", e.cfg.SMTPUser, e.cfg.SMTPPass, e.cfg.SMTPHost)
	}

	// Use STARTTLS if port is 587, plain TLS if 465, plain if 25.
	switch e.cfg.SMTPPort {
	case 465:
		return sendTLS(addr, auth, e.cfg.SMTPFrom, to, msg)
	default:
		return smtp.SendMail(addr, auth, e.cfg.SMTPFrom, []string{to}, msg)
	}
}

func sendTLS(addr string, auth smtp.Auth, from, to string, msg []byte) error {
	host, _, _ := net.SplitHostPort(addr)
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: host})
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer c.Quit() //nolint:errcheck
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	_, err = w.Write(msg)
	if err != nil {
		return err
	}
	return w.Close()
}

func buildMessage(from, to, subject, body string) []byte {
	var sb strings.Builder
	sb.WriteString("From: " + from + "\r\n")
	sb.WriteString("To: " + to + "\r\n")
	sb.WriteString("Subject: " + subject + "\r\n")
	sb.WriteString("MIME-Version: 1.0\r\n")
	sb.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	sb.WriteString("\r\n")
	sb.WriteString(body)
	return []byte(sb.String())
}
