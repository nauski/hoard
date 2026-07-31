// Package scheduler runs hoard's recurring jobs — mirror, check, prune, and the
// freshness refresh that drives staleness alerting. It deliberately avoids a
// cron dependency: jobs fire "daily at HH:MM" (optionally on a given weekday),
// which is all the cadence a backup hub needs, and it keeps the binary dep-free.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/forecast"
	"github.com/nauski/hoard/internal/restic"
	"github.com/nauski/hoard/internal/state"
)

// errBusy is returned when a restore is requested while another job holds the lock.
var errBusy = fmt.Errorf("busy: another job is running")

// Notifier sends a failure/staleness alert. Implemented by internal/api's
// webhook sender; nil disables alerts.
type Notifier interface {
	Notify(ctx context.Context, title, body string)
}

// Scheduler owns the background job loop and can also run jobs on demand
// (triggered from the API).
type Scheduler struct {
	cfg       *config.Store
	resticBin string
	store     *state.Store
	log       *slog.Logger
	note      Notifier

	mu      sync.Mutex // serializes restic operations; only one runs at a time
	running string     // name of the currently running job, "" if idle
}

func New(cfg *config.Store, resticBin string, store *state.Store, log *slog.Logger) *Scheduler {
	return &Scheduler{cfg: cfg, resticBin: resticBin, store: store, log: log}
}

// hotC / coldC build a restic client from the CURRENT config, so changing the
// repo/endpoint/credentials in Settings takes effect on the next operation
// without a restart. restic.New just captures strings, so this is cheap.
func (s *Scheduler) hotC() *restic.Client  { return restic.New(s.resticBin, s.cfg.Load().Hot) }
func (s *Scheduler) coldC() *restic.Client { return restic.New(s.resticBin, s.cfg.Load().Cold) }

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
	s.maybeSampleSize(ctx)

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
			s.maybeSampleSize(ctx)

			if s.cfg.Load().Schedule.Mirror == hhmm {
				lastFired = stamp
				go s.Mirror(ctx)
			}
			if s.cfg.Load().Schedule.Check == hhmm && s.weekdayOK(now) {
				lastFired = stamp
				go s.Check(ctx)
			}
			if s.cfg.Load().Schedule.Verify == hhmm {
				lastFired = stamp
				go s.Verify(ctx)
			}
		}
	}
}

func (s *Scheduler) weekdayOK(now time.Time) bool {
	if s.cfg.Load().Schedule.CheckWeekday == nil {
		return true
	}
	return int(now.Weekday()) == *s.cfg.Load().Schedule.CheckWeekday
}

// Mirror copies new snapshots hot -> cold, then applies retention/prune on cold.
func (s *Scheduler) Mirror(ctx context.Context) {
	if !s.acquire("mirror") {
		return
	}
	defer s.release()

	start := time.Now()
	out, err := s.coldC().CopyFrom(ctx, s.cfg.Load().Hot)
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
	if st, e := s.coldC().Stats(ctx); e == nil {
		res.Message += " · e2 now " + humanBytes(st.TotalSize)
	}
	s.store.RecordJob(res)
	s.log.Info("mirror ok")

	// Retention runs on the cold repo right after a successful copy.
	s.prune(ctx)
}

func (s *Scheduler) prune(ctx context.Context) {
	start := time.Now()
	out, err := s.coldC().ForgetPrune(ctx, s.cfg.Load().Retention)
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
	if freed := parseFreed(out); freed != "" {
		res.Message += " · freed " + freed
	}
	s.store.RecordJob(res)
}

// parseFreed pulls restic prune's "to delete: N blobs / X" freed amount.
func parseFreed(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "to delete:") {
			if i := strings.LastIndex(line, "/"); i >= 0 {
				return strings.TrimSpace(line[i+1:])
			}
		}
	}
	return ""
}

// humanBytes formats a byte count for job messages.
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// Check verifies cold-repo integrity with a sampled data read.
func (s *Scheduler) Check(ctx context.Context) {
	if !s.acquire("check") {
		return
	}
	defer s.release()

	start := time.Now()
	out, err := s.coldC().Check(ctx, "5%")
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
	snaps, err := s.hotC().Snapshots(ctx)
	if err != nil {
		s.log.Warn("refresh clients: snapshots failed", "err", err)
		return
	}
	latest := map[string]state.Client{}
	for _, sn := range snaps {
		c, ok := latest[sn.Hostname]
		if !ok || sn.Time.After(c.LastSnapshot) {
			var size uint64
			if sn.Summary != nil {
				size = sn.Summary.TotalBytesProcessed
			}
			latest[sn.Hostname] = state.Client{
				Hostname:     sn.Hostname,
				LastSnapshot: sn.Time,
				SnapshotID:   sn.ShortID,
				Paths:        sn.Paths,
				Size:         size,
			}
		}
	}

	stale := s.cfg.Load().Schedule.StaleAfter.Std()
	prev := s.store.Snapshot()
	for host, c := range latest {
		if stale > 0 && time.Since(c.LastSnapshot) > stale {
			c.Stale = true
			// Only alert on the transition into staleness to avoid spamming.
			if s.cfg.Load().Alert.OnStale && !prev.Clients[host].Stale {
				s.alert("client backup is stale",
					host+" has not backed up since "+c.LastSnapshot.Format(time.RFC3339))
			}
		}
		latest[host] = c
	}
	s.store.SetClients(latest)
}

// maybeSampleSize records a repo-size sample at most once per 24h. Hot is
// required; cold is best-effort (0 if unconfigured/unreachable). Errors
// sampling hot are logged and skipped so the tick never wedges.
func (s *Scheduler) maybeSampleSize(ctx context.Context) {
	if !forecast.DueForSample(s.store.LastSizeSampleAt(), time.Now(), 24*time.Hour) {
		return
	}
	hot, err := s.hotC().StatsMode(ctx, "raw-data")
	if err != nil {
		s.log.Warn("size sample: hot stats failed", "err", err)
		return
	}
	sample := state.SizeSample{At: time.Now(), HotStored: int64(hot.TotalSize)}
	if s.cfg.Load().Cold.Repository != "" {
		if cold, err := s.coldC().StatsMode(ctx, "raw-data"); err != nil {
			s.log.Warn("size sample: cold stats failed (recording hot only)", "err", err)
		} else {
			sample.ColdStored = int64(cold.TotalSize)
		}
	}
	s.store.AppendSizeSample(sample)
	s.log.Info("recorded size sample", "hot", sample.HotStored, "cold", sample.ColdStored)
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
