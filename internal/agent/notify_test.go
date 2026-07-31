package agent

import (
	"encoding/json"
	"testing"
)

func bptr(b bool) *bool { return &b }

func TestNotifyEnabled(t *testing.T) {
	cases := []struct {
		v    *bool
		want bool
	}{{nil, true}, {bptr(true), true}, {bptr(false), false}}
	for _, c := range cases {
		if got := (Config{NotifyDesktop: c.v}).NotifyEnabled(); got != c.want {
			t.Errorf("NotifyEnabled(%v)=%v want %v", c.v, got, c.want)
		}
	}
}

func TestNotifyDesktopRoundTrip(t *testing.T) {
	raw, _ := json.Marshal(Config{NotifyDesktop: bptr(false)})
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if c.NotifyDesktop == nil || *c.NotifyDesktop != false {
		t.Fatalf("round-trip: %v", c.NotifyDesktop)
	}
	// absent key -> nil -> enabled
	var c2 Config
	json.Unmarshal([]byte(`{"repository":"x"}`), &c2)
	if !c2.NotifyEnabled() {
		t.Fatal("absent notify_desktop must default to enabled")
	}
}

func TestNotifyArgs(t *testing.T) {
	cases := []struct {
		name    string
		rr      RunResult
		wantOK  bool
		wantSub string // a substring that must appear in args when ok
		urgency string
	}{
		{"success", RunResult{Kind: "backup", OK: true, Message: "snapshot ab: 2 files, 1 KiB added"}, true, "Backup complete", "normal"},
		{"failure", RunResult{Kind: "backup", OK: false, Message: "repo unreachable"}, true, "Backup failed", "critical"},
		{"preflight-fail", RunResult{Kind: "backup", OK: false, Message: "bad password"}, true, "Backup failed", "critical"},
		{"cancelled", RunResult{Kind: "backup", OK: false, Message: "cancelled"}, false, "", ""},
		{"restore", RunResult{Kind: "restore", OK: true, Message: "restored"}, false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args, ok := notifyArgs(c.rr)
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			joined := args
			if !contains(joined, "hoard: "+c.wantSub) {
				t.Errorf("args %v missing title %q", args, c.wantSub)
			}
			if !contains(joined, c.urgency) {
				t.Errorf("args %v missing urgency %q", args, c.urgency)
			}
			if !contains(joined, c.rr.Message) {
				t.Errorf("args %v missing body %q", args, c.rr.Message)
			}
		})
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
