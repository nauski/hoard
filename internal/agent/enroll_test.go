package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestEnroll(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/enroll/redeem" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"repository":"rest:http://h:8000/hot","password":"s3cret","server_url":"http://h:8080"}`))
	}))
	defer ts.Close()

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	a := &Agent{cfgPath: cfgPath, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	if err := a.Enroll(context.Background(), ts.URL, "tok"); err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	c := a.GetConfig()
	if c.Repository != "rest:http://h:8000/hot" {
		t.Errorf("Repository = %q, want rest:http://h:8000/hot", c.Repository)
	}
	if c.ServerURL != "http://h:8080" {
		t.Errorf("ServerURL = %q, want http://h:8080", c.ServerURL)
	}
	if c.PasswordFile == "" {
		t.Fatal("PasswordFile not set")
	}

	info, err := os.Stat(c.PasswordFile)
	if err != nil {
		t.Fatalf("stat password file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("password file mode = %o, want 600", perm)
	}

	got, err := os.ReadFile(c.PasswordFile)
	if err != nil {
		t.Fatalf("read password file: %v", err)
	}
	if string(got) != "s3cret" {
		t.Errorf("password file contents = %q, want s3cret", got)
	}
}
