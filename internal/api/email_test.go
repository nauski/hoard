package api

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/scheduler"
	"github.com/nauski/hoard/internal/state"
)

func TestBuildEmail(t *testing.T) {
	msg := string(buildEmail("a@x.com", "b@y.com", "Subj", "hello body", time.Unix(0, 0).UTC()))
	for _, want := range []string{"From: a@x.com\r\n", "To: b@y.com\r\n", "Subject: Subj\r\n", "Date: ", "\r\n\r\nhello body"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("email missing %q\n---\n%s", want, msg)
		}
	}
}

// safeBuf is a mutex-guarded buffer the mock writes and the test reads.
type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *safeBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// startMockSMTP is a minimal SMTP server for tests. It advertises AUTH (but NOT
// STARTTLS, so net/smtp.SendMail skips TLS), and — addressed as host "localhost"
// — lets PlainAuth send PLAIN over the plaintext test connection. It captures the
// DATA payload into the returned buffer.
func startMockSMTP(t *testing.T) (string, string, *safeBuf, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	captured := &safeBuf{}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		w := func(s string) { _, _ = conn.Write([]byte(s)) }
		w("220 mock ESMTP\r\n")
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					w("250 ok\r\n")
					continue
				}
				_, _ = captured.Write([]byte(line))
				continue
			}
			up := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(up, "EHLO"):
				w("250-mock\r\n250 AUTH PLAIN\r\n")
			case strings.HasPrefix(up, "HELO"):
				w("250 mock\r\n")
			case strings.HasPrefix(up, "AUTH"):
				w("235 ok\r\n")
			case strings.HasPrefix(up, "MAIL"):
				w("250 ok\r\n")
			case strings.HasPrefix(up, "RCPT"):
				w("250 ok\r\n")
			case strings.HasPrefix(up, "DATA"):
				w("354 go\r\n")
				inData = true
			case strings.HasPrefix(up, "QUIT"):
				w("221 bye\r\n")
				return
			default:
				w("250 ok\r\n")
			}
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return "localhost", port, captured, func() { _ = ln.Close() }
}

func TestSendEmail(t *testing.T) {
	host, port, captured, closeFn := startMockSMTP(t)
	defer closeFn()
	sm := config.SMTP{Host: host, Port: port, Username: "u", Password: "p", From: "a@x.com", To: "b@y.com"}
	if err := sendEmail(sm, "Hello Subject", "the body text"); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}
	// Give the mock a moment to flush the DATA.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(captured.String(), "the body text") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := captured.String()
	if !strings.Contains(got, "Subject: Hello Subject") || !strings.Contains(got, "the body text") {
		t.Fatalf("mock did not receive the message:\n%s", got)
	}
}

func TestSendEmailNotConfigured(t *testing.T) {
	if err := sendEmail(config.SMTP{}, "s", "b"); err == nil {
		t.Fatal("expected error for unconfigured SMTP")
	}
}

func TestNotifyFansOutToBoth(t *testing.T) {
	var webhookHit int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&webhookHit, 1)
	}))
	defer hook.Close()
	host, port, captured, closeFn := startMockSMTP(t)
	defer closeFn()

	cfg := config.NewStore(&config.Config{
		Hot: config.Repo{Repository: "/h"}, Cold: config.Repo{Repository: "s3:x"},
		Alert: config.Alert{WebhookURL: hook.URL},
		SMTP:  config.SMTP{Host: host, Port: port, From: "a@x.com", To: "b@y.com"},
	}, "")
	st, _ := state.Load("")
	srv := New(cfg, "restic", scheduler.New(cfg, "restic", st, testLogger()), st, testLogger(), nil)

	srv.Notify(context.Background(), "title here", "body here")

	if atomic.LoadInt32(&webhookHit) != 1 {
		t.Fatalf("webhook not hit: %d", atomic.LoadInt32(&webhookHit))
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(captured.String(), "body here") {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(captured.String(), "title here") {
		t.Fatalf("email not sent:\n%s", captured.String())
	}
}
