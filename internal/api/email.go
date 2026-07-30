package api

import (
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"time"

	"github.com/nauski/hoard/internal/config"
)

// buildEmail renders a plain-text RFC 5322 message with CRLF headers. Pure.
func buildEmail(from, to, subject, body string, now time.Time) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	b.WriteString("\r\n")
	return []byte(b.String())
}

func portOr(p, def string) string {
	if strings.TrimSpace(p) == "" {
		return def
	}
	return p
}

// sendEmail sends subject/body via the configured SMTP server. net/smtp.SendMail
// negotiates STARTTLS when the server advertises it and uses PLAIN auth when a
// username is set. Bounded to ~20s so a dead server can't hang the alert path.
func sendEmail(sm config.SMTP, subject, body string) error {
	if sm.Host == "" || sm.From == "" || sm.To == "" {
		return fmt.Errorf("SMTP not configured")
	}
	addr := net.JoinHostPort(sm.Host, portOr(sm.Port, "587"))
	msg := buildEmail(sm.From, sm.To, subject, body, time.Now())
	var auth smtp.Auth
	if sm.Username != "" {
		auth = smtp.PlainAuth("", sm.Username, sm.Password, sm.Host)
	}
	done := make(chan error, 1)
	go func() { done <- smtp.SendMail(addr, auth, sm.From, []string{sm.To}, msg) }()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		return fmt.Errorf("smtp send to %s timed out", addr)
	}
}

// handleTestEmail sends a fixed test message via the current SMTP config so the
// user can verify email before relying on it. Mirrors test-cold.
func (s *Server) handleTestEmail(w http.ResponseWriter, r *http.Request) {
	sm := s.cfg.Load().SMTP
	if sm.Host == "" || sm.From == "" || sm.To == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "SMTP not configured"})
		return
	}
	if err := sendEmail(sm, "hoard test email", "This is a test alert from hoard. If you got this, email alerts work."); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
