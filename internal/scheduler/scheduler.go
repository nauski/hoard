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

	job liveJob // live view of the in-flight server job; own mutex, never nested with mu
}

// ServerJobView is a point-in-time snapshot of the currently running server
// job (mirror/prune/check), for the dashboard's live progress display.
type ServerJobView struct {
	Name       string    `json:"name"`
	StartedAt  time.Time `json:"started_at"`
	Percent    float64   `json:"percent"`
	PacksDone  int       `json:"packs_done"`
	PacksTotal int       `json:"packs_total"`
	Tail       []string  `json:"tail"`
}

// liveJob holds the mutable state behind ServerJob/CancelServerJob. It has
// its own mutex, deliberately separate from Scheduler.mu (which only guards
// the acquire/release "one job at a time" lock): hooks fire from inside the
// running restic command and must never risk lock ordering against acquire.
type liveJob struct {
	mu     sync.Mutex
	active bool
	view   ServerJobView
	cancel context.CancelFunc
}

// beginJob starts tracking a new live job (e.g. "mirror") and stores its
// cancel func so CancelServerJob can abort it.
func (s *Scheduler) beginJob(name string, cancel context.CancelFunc) {
	s.job.mu.Lock()
	defer s.job.mu.Unlock()
	s.job.active = true
	s.job.cancel = cancel
	s.job.view = ServerJobView{Name: name, StartedAt: time.Now(), Percent: -1}
}

// phaseJob switches the live view to a new phase of the same job (e.g.
// mirror -> prune) without touching the stored cancel func.
func (s *Scheduler) phaseJob(name string) {
	s.job.mu.Lock()
	defer s.job.mu.Unlock()
	s.job.view = ServerJobView{Name: name, StartedAt: time.Now(), Percent: -1}
}

// jobProgress is a restic.StreamHooks.OnProgress callback that updates the
// live view's percent/pack counters.
func (s *Scheduler) jobProgress(pct float64, done, total int) {
	s.job.mu.Lock()
	defer s.job.mu.Unlock()
	s.job.view.Percent = pct
	s.job.view.PacksDone = done
	s.job.view.PacksTotal = total
}

// jobLine is a restic.StreamHooks.OnActivity callback that appends to the
// live tail, capped at 200 lines.
func (s *Scheduler) jobLine(line string) {
	s.job.mu.Lock()
	defer s.job.mu.Unlock()
	s.job.view.Tail = append(s.job.view.Tail, line)
	if len(s.job.view.Tail) > 200 {
		s.job.view.Tail = s.job.view.Tail[len(s.job.view.Tail)-200:]
	}
}

// endJob clears the live view once the job (all phases) has finished.
func (s *Scheduler) endJob() {
	s.job.mu.Lock()
	defer s.job.mu.Unlock()
	s.job.active = false
	s.job.cancel = nil
}

// ServerJob returns a snapshot of the currently running server job, or nil
// if none is running. The Tail slice is copied so a concurrent jobLine
// append can't race the caller reading it.
func (s *Scheduler) ServerJob() *ServerJobView {
	s.job.mu.Lock()
	defer s.job.mu.Unlock()
	if !s.job.active {
		return nil
	}
	v := s.job.view
	v.Tail = append([]string(nil), s.job.view.Tail...)
	return &v
}

// CancelServerJob cancels the currently running server job, if any. Returns
// false when idle.
func (s *Scheduler) CancelServerJob() bool {
	s.job.mu.Lock()
	c := s.job.cancel
	s.job.mu.Unlock()
	if c == nil {
		return false
	}
	c()
	return true
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

// Mirror applies retention to the hot repo (only when retention.apply_to_hot is
// set), copies snapshots hot -> cold (minus mirror.exclude_hosts), then applies
// retention/prune on cold. Hot goes first so the copy cannot re-upload
// snapshots that retention is about to drop.
func (s *Scheduler) Mirror(ctx context.Context) {
	if !s.acquire("mirror") {
		return
	}
	defer s.release()

	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.beginJob("mirror", cancel)
	defer s.endJob()

	cfg := s.cfg.Load()
	if cfg.Retention.ApplyToHot {
		if !s.prune(jctx, s.hotC(), "prune-hot", "the hot repo") {
			return // cancelled
		}
		s.phaseJob("mirror")
	}

	start := time.Now()
	// With hot-only hosts configured, copy the remaining hosts by name. If the
	// host list cannot be read, fail rather than fall back to copying everything:
	// a hot-only host must never reach the offsite repo by accident.
	var hosts []string
	var out string
	var err error
	if len(cfg.Mirror.ExcludeHosts) > 0 {
		var all []string
		all, err = s.hotC().Hostnames(jctx)
		for _, h := range all {
			if !cfg.Mirror.Excludes(h) {
				hosts = append(hosts, h)
			}
		}
		if err == nil && len(hosts) == 0 {
			s.store.RecordJob(state.JobResult{Job: "mirror", StartedAt: start, EndedAt: time.Now(), OK: true,
				Message: "nothing to mirror: every host is hot-only"})
			return
		}
	}
	if err == nil {
		hooks := restic.StreamHooks{OnProgress: s.jobProgress, OnActivity: s.jobLine}
		out, err = s.coldC().CopyFrom(jctx, cfg.Hot, hosts, hooks)
	}
	res := state.JobResult{Job: "mirror", StartedAt: start, EndedAt: time.Now(), Output: out}
	if err != nil {
		if jctx.Err() == context.Canceled {
			res.OK = false
			res.Message = "cancelled"
			s.log.Info("mirror cancelled")
			s.store.RecordJob(res)
			return // skip prune; a user-initiated cancel is not a failure
		}
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
	s.prune(jctx, s.coldC(), "prune", "e2")
}

// prune applies retention to one repo as job (history name) and reports
// whether the caller should carry on: false only when the user cancelled.
func (s *Scheduler) prune(ctx context.Context, cl *restic.Client, job, where string) bool {
	s.phaseJob(job)
	start := time.Now()
	out, err := cl.ForgetPrune(ctx, s.cfg.Load().Retention, restic.StreamHooks{OnActivity: s.jobLine})
	res := state.JobResult{Job: job, StartedAt: start, EndedAt: time.Now(), Output: out}
	if err != nil {
		if ctx.Err() == context.Canceled {
			res.OK = false
			res.Message = "cancelled"
			s.store.RecordJob(res)
			return false
		}
		res.OK = false
		res.Message = err.Error()
		s.log.Error(job+" failed", "err", err)
		s.store.RecordJob(res)
		s.alert("retention on "+where+" failed", err.Error())
		return true
	}
	res.OK = true
	res.Message = "retention applied to " + where
	if freed := parseFreed(out); freed != "" {
		res.Message += " · freed " + freed
	}
	s.store.RecordJob(res)
	return true
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

	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.beginJob("check", cancel)
	defer s.endJob()

	start := time.Now()
	hooks := restic.StreamHooks{OnProgress: s.jobProgress, OnActivity: s.jobLine}
	out, err := s.coldC().Check(jctx, "5%", hooks)
	res := state.JobResult{Job: "check", StartedAt: start, EndedAt: time.Now(), Output: out}
	if err != nil {
		if jctx.Err() == context.Canceled {
			res.OK = false
			res.Message = "cancelled"
			s.log.Info("check cancelled")
			s.store.RecordJob(res)
			return
		}
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

// RefreshClients rebuilds per-host freshness from the hot repo's snapshots,
// fires staleness alerts for hosts past their stale window, and flags a host
// whose newest backup lost a large share of its files (see checkAnomaly).
func (s *Scheduler) RefreshClients(ctx context.Context) {
	snaps, err := s.hotC().Snapshots(ctx)
	if err != nil {
		s.log.Warn("refresh clients: snapshots failed", "err", err)
		return
	}
	// Newest and second-newest snapshot per host.
	newest, second := map[string]restic.Snapshot{}, map[string]restic.Snapshot{}
	for _, sn := range snaps {
		switch n, ok := newest[sn.Hostname]; {
		case !ok || sn.Time.After(n.Time):
			if ok {
				second[sn.Hostname] = n
			}
			newest[sn.Hostname] = sn
		default:
			if p, ok := second[sn.Hostname]; !ok || sn.Time.After(p.Time) {
				second[sn.Hostname] = sn
			}
		}
	}

	cfg := s.cfg.Load()
	prev := s.store.Snapshot()
	latest := map[string]state.Client{}
	for host, sn := range newest {
		var size uint64
		if sn.Summary != nil {
			size = sn.Summary.TotalBytesProcessed
		}
		c := state.Client{
			Hostname:     host,
			LastSnapshot: sn.Time,
			SnapshotID:   sn.ShortID,
			Paths:        sn.Paths,
			Size:         size,
			HotOnly:      cfg.Mirror.Excludes(host),
			Anomaly:      prev.Clients[host].Anomaly,
		}
		if stale := cfg.Schedule.StaleFor(host); stale > 0 && time.Since(c.LastSnapshot) > stale {
			c.Stale = true
			// Only alert on the transition into staleness to avoid spamming.
			if cfg.Alert.OnStale && !prev.Clients[host].Stale {
				s.alert("client backup is stale",
					host+" has not backed up since "+c.LastSnapshot.Format(time.RFC3339))
			}
		}
		// Check each new snapshot once: only when the host was already known and
		// its newest snapshot moved forward.
		if p, known := prev.Clients[host]; known && p.SnapshotID != sn.ShortID && sn.Time.After(p.LastSnapshot) {
			if before, ok := second[host]; ok {
				if a := s.checkAnomaly(ctx, host, before, sn); a != nil {
					c.Anomaly = a
				}
			}
		}
		if c.Anomaly != nil && !c.Anomaly.Held {
			s.hold(ctx, c.Anomaly)
		}
		latest[host] = c
	}
	s.store.SetClients(latest)
}

// checkAnomaly compares a host's two newest snapshots and, when the file count
// fell past the configured thresholds, alerts and returns the anomaly record.
func (s *Scheduler) checkAnomaly(ctx context.Context, host string, before, after restic.Snapshot) *state.Anomaly {
	an := s.cfg.Load().Anomaly
	if an.Files <= 0 && an.Percent <= 0 {
		return nil
	}
	fb, bb, fa, ba, err := s.snapshotCounts(ctx, before, after)
	if err != nil {
		// Never fail silently here: an unchecked backup is an unprotected one.
		s.log.Warn("mass-deletion check skipped: cannot size snapshots", "host", host, "err", err)
		return nil
	}
	if !an.Triggered(fb, fa) {
		return nil
	}
	a := &state.Anomaly{
		At: after.Time, PrevID: before.ShortID, NewID: after.ShortID,
		FilesBefore: fb, FilesAfter: fa, BytesBefore: bb, BytesAfter: ba,
	}
	var lost uint64
	if a.BytesBefore > a.BytesAfter {
		lost = a.BytesBefore - a.BytesAfter
	}
	s.log.Warn("backup shrank sharply", "host", host, "files_before", fb, "files_after", fa)
	s.alert("files vanished from a backup", fmt.Sprintf(
		"%s: %d files / %s fewer than the previous backup (%d -> %d files). The previous snapshot %s is being held so retention cannot remove it; review and restore from it on the dashboard.",
		host, fb-fa, humanBytes(lost), fb, fa, before.ShortID))
	return a
}

// snapshotCounts returns the file count and logical size of two snapshots,
// measured the same way for both. Clients on restic 0.17+ embed a summary in
// each snapshot, which is free to read. Older clients (e.g. a distro-packaged
// restic) do not, so the server walks both snapshots with its own restic.
// That walk counts directories as well as files, which is why both snapshots
// are always measured the same way.
// ponytail: two tree walks per new snapshot for pre-0.17 clients; cache the
// previous count in state if that shows up on a large host.
func (s *Scheduler) snapshotCounts(ctx context.Context, before, after restic.Snapshot) (fb int, bb uint64, fa int, ba uint64, err error) {
	if before.Summary != nil && after.Summary != nil {
		return before.Summary.TotalFilesProcessed, before.Summary.TotalBytesProcessed,
			after.Summary.TotalFilesProcessed, after.Summary.TotalBytesProcessed, nil
	}
	hot := s.hotC()
	sb, err := hot.SnapshotStats(ctx, before.ID)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	sa, err := hot.SnapshotStats(ctx, after.ID)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return int(sb.TotalFileCount), sb.TotalSize, int(sa.TotalFileCount), sa.TotalSize, nil
}

// hold tags the anomaly's previous snapshot so retention keeps it. Tagging
// needs an exclusive lock and rewrites the snapshot under a new ID; on failure
// (e.g. a backup is running) Held stays false and the next refresh retries.
func (s *Scheduler) hold(ctx context.Context, a *state.Anomaly) {
	hot := s.hotC()
	if err := hot.Tag(ctx, a.PrevID, []string{restic.TagHold}, nil); err != nil {
		s.log.Warn("could not hold snapshot yet, will retry", "snapshot", a.PrevID, "err", err)
		return
	}
	a.Held = true
	snaps, err := hot.Snapshots(ctx)
	if err != nil {
		return
	}
	for _, sn := range snaps {
		if strings.HasPrefix(sn.Original, a.PrevID) {
			a.PrevID = sn.ShortID
		}
	}
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
