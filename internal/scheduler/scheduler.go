// Package scheduler runs hoard's recurring jobs — mirror, check, prune, and the
// freshness refresh that drives staleness alerting. It deliberately avoids a
// cron dependency: jobs fire "daily at HH:MM" (optionally on a given weekday),
// which is all the cadence a backup hub needs, and it keeps the binary dep-free.
package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/restic"
	"github.com/nauski/hoard/internal/state"
)

// Notifier sends a failure/staleness alert. Implemented by internal/api's
// webhook sender; nil disables alerts.
type Notifier interface {
	Notify(ctx context.Context, title, body string)
}

// Scheduler owns the background job loop and can also run jobs on demand
// (triggered from the API).
type Scheduler struct {
	cfg   *config.Config
	hot   *restic.Client
	cold  *restic.Client
	store *state.Store
	log   *slog.Logger
	note  Notifier

	mu      sync.Mutex // serializes restic operations; only one runs at a time
	running string     // name of the currently running job, "" if idle
}

func New(cfg *config.Config, hot, cold *restic.Client, store *state.Store, log *slog.Logger) *Scheduler {
	return &Scheduler{cfg: cfg, hot: hot, cold: cold, store: store, log: log}
}

// SetNotifier wires the alert sink after construction. This breaks the
// construction cycle between the scheduler and the API server (which is itself
// the Notifier). Call before Run.
func (s *Scheduler) SetNotifier(n Notifier) { s.note = n }

// Running reports the name of the in-flight job, or "" when idle.
func (s *Scheduler) Running() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Run starts the scheduling loop until ctx is cancelled. It ticks once a minute
// and fires jobs whose configured time matches the current minute.
func (s *Scheduler) Run(ctx context.Context) {
	// Refresh freshness immediately so the dashboard isn't empty on boot.
	s.RefreshClients(ctx)

	t := time.NewTicker(time.Minute)
	defer t.Stop()
	var lastFired string // guards against double-firing within the same minute
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			hhmm := now.Format("15:04")
			stamp := now.Format("2006-01-02 15:04")
			if stamp == lastFired {
				continue
			}
			// Always refresh freshness each tick (cheap: one snapshots call).
			s.RefreshClients(ctx)

			if s.cfg.Schedule.Mirror == hhmm {
				lastFired = stamp
				go s.Mirror(ctx)
			}
			if s.cfg.Schedule.Check == hhmm && s.weekdayOK(now) {
				lastFired = stamp
				go s.Check(ctx)
			}
		}
	}
}

func (s *Scheduler) weekdayOK(now time.Time) bool {
	if s.cfg.Schedule.CheckWeekday == nil {
		return true
	}
	return int(now.Weekday()) == *s.cfg.Schedule.CheckWeekday
}

// Mirror copies new snapshots hot -> cold, then applies retention/prune on cold.
func (s *Scheduler) Mirror(ctx context.Context) {
	if !s.acquire("mirror") {
		return
	}
	defer s.release()

	start := time.Now()
	out, err := s.cold.CopyFrom(ctx, s.cfg.Hot)
	res := state.JobResult{Job: "mirror", StartedAt: start, EndedAt: time.Now(), Output: out}
	if err != nil {
		res.OK = false
		res.Message = err.Error()
		s.log.Error("mirror failed", "err", err)
		s.store.RecordJob(res)
		s.alert("mirror failed", err.Error())
		return
	}
	res.OK = true
	res.Message = "snapshots copied to e2"
	s.store.RecordJob(res)
	s.log.Info("mirror ok")

	// Retention runs on the cold repo right after a successful copy.
	s.prune(ctx)
}

func (s *Scheduler) prune(ctx context.Context) {
	start := time.Now()
	out, err := s.cold.ForgetPrune(ctx, s.cfg.Retention)
	res := state.JobResult{Job: "prune", StartedAt: start, EndedAt: time.Now(), Output: out}
	if err != nil {
		res.OK = false
		res.Message = err.Error()
		s.log.Error("prune failed", "err", err)
		s.store.RecordJob(res)
		s.alert("prune failed", err.Error())
		return
	}
	res.OK = true
	res.Message = "retention applied to e2"
	s.store.RecordJob(res)
}

// Check verifies cold-repo integrity with a sampled data read.
func (s *Scheduler) Check(ctx context.Context) {
	if !s.acquire("check") {
		return
	}
	defer s.release()

	start := time.Now()
	out, err := s.cold.Check(ctx, "5%")
	res := state.JobResult{Job: "check", StartedAt: start, EndedAt: time.Now(), Output: out}
	if err != nil {
		res.OK = false
		res.Message = err.Error()
		s.log.Error("check failed", "err", err)
		s.store.RecordJob(res)
		s.alert("integrity check FAILED", err.Error())
		return
	}
	res.OK = true
	res.Message = "e2 repo integrity verified"
	s.store.RecordJob(res)
	s.log.Info("check ok")
}

// RefreshClients rebuilds per-host freshness from the hot repo's snapshots and
// fires staleness alerts for hosts past the StaleAfter window.
func (s *Scheduler) RefreshClients(ctx context.Context) {
	snaps, err := s.hot.Snapshots(ctx)
	if err != nil {
		s.log.Warn("refresh clients: snapshots failed", "err", err)
		return
	}
	latest := map[string]state.Client{}
	for _, sn := range snaps {
		c, ok := latest[sn.Hostname]
		if !ok || sn.Time.After(c.LastSnapshot) {
			latest[sn.Hostname] = state.Client{
				Hostname:     sn.Hostname,
				LastSnapshot: sn.Time,
				SnapshotID:   sn.ShortID,
				Paths:        sn.Paths,
			}
		}
	}

	stale := s.cfg.Schedule.StaleAfter.Std()
	prev := s.store.Snapshot()
	for host, c := range latest {
		if stale > 0 && time.Since(c.LastSnapshot) > stale {
			c.Stale = true
			// Only alert on the transition into staleness to avoid spamming.
			if s.cfg.Alert.OnStale && !prev.Clients[host].Stale {
				s.alert("client backup is stale",
					host+" has not backed up since "+c.LastSnapshot.Format(time.RFC3339))
			}
		}
		latest[host] = c
	}
	s.store.SetClients(latest)
}

func (s *Scheduler) acquire(job string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running != "" {
		s.log.Warn("skip job, another running", "want", job, "running", s.running)
		return false
	}
	s.running = job
	return true
}

func (s *Scheduler) release() {
	s.mu.Lock()
	s.running = ""
	s.mu.Unlock()
}

func (s *Scheduler) alert(title, body string) {
	if s.note == nil {
		return
	}
	s.note.Notify(context.Background(), title, body)
}
