package agent

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func TestNextRun(t *testing.T) {
	at := func(d, h, m int) time.Time { return time.Date(2026, 11, d, h, m, 0, 0, time.Local) }
	var zero time.Time
	cases := []struct {
		name            string
		sched           string
		last, boot, now time.Time
		want            time.Time
		wantErr         bool
	}{
		{"interval not yet due", "1h", at(15, 10, 0), zero, at(15, 10, 30), at(15, 11, 0), false},
		{"interval missed while asleep", "1h", at(15, 10, 0), zero, at(15, 18, 0), at(15, 11, 0), false},
		{"interval, no state: run now", "30m", zero, at(15, 9, 0), at(15, 10, 0), at(15, 10, 0), false},
		{"daily, last run before today's slot", "21:00", at(15, 9, 0), zero, at(15, 20, 0), at(15, 21, 0), false},
		{"daily, last run after today's slot", "21:00", at(15, 21, 0), zero, at(15, 22, 0), at(16, 21, 0), false},
		{"daily missed overnight", "02:30", at(14, 2, 30), zero, at(15, 8, 0), at(15, 2, 30), false},
		{"daily, no state: next slot after boot", "21:00", zero, at(15, 22, 0), at(15, 22, 5), at(16, 21, 0), false},
		{"interval under a minute", "10s", zero, zero, at(15, 10, 0), zero, true},
		{"garbage", "hourly", zero, zero, at(15, 10, 0), zero, true},
		{"bad clock time", "25:00", zero, zero, at(15, 10, 0), zero, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := nextRun(c.sched, c.last, c.boot, c.now)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if !c.wantErr && !got.Equal(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestLastStartPersists(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := Load(cfg, "restic", "", log)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	a.saveLastStart(when)

	b, err := Load(cfg, "restic", "", log)
	if err != nil {
		t.Fatal(err)
	}
	if !b.lastStart.Equal(when) {
		t.Fatalf("lastStart = %v, want %v", b.lastStart, when)
	}
}

func TestSetConfigRejectsBadSchedule(t *testing.T) {
	a, err := Load(filepath.Join(t.TempDir(), "config.json"), "restic", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c := a.GetConfig()
	c.Schedule = "hourly"
	if err := a.SetConfig(c); err == nil {
		t.Fatal("SetConfig accepted an invalid schedule")
	}
	c.Schedule = "1h"
	if err := a.SetConfig(c); err != nil {
		t.Fatalf("SetConfig rejected 1h: %v", err)
	}
}
