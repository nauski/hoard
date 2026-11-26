package scheduler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nauski/hoard/internal/state"
)

// PurgePath removes targetPath (a file or folder, and everything under it) from
// EVERY snapshot of host, in the hot repo AND every offsite storage, then
// prunes them to actually reclaim space. Running everywhere is what makes the
// deletion stick: purging only offsite would let the next mirror re-copy the
// path from hot; purging only hot would leave the data occupying the storage. Serialized against the
// scheduler's other jobs.
func (s *Scheduler) PurgePath(ctx context.Context, host, targetPath string) error {
	if strings.TrimSpace(targetPath) == "" {
		return fmt.Errorf("empty path")
	}
	if !s.acquire("purge") {
		return fmt.Errorf("busy: %s is running", s.Running())
	}
	defer s.release()

	start := time.Now()
	var b strings.Builder
	run := func(name string, fn func() (string, error)) error {
		out, err := fn()
		fmt.Fprintf(&b, "== %s ==\n%s\n", name, strings.TrimSpace(out))
		if err != nil {
			fmt.Fprintf(&b, "ERROR: %v\n", err)
		}
		return err
	}

	// Hot first (the source of truth), then each offsite storage.
	hotRepo := s.hot.ForHost(ctx, host)
	_ = hotRepo.Unlock(ctx)
	if err := run("rewrite hot", func() (string, error) { return hotRepo.RewriteExcludePath(ctx, host, targetPath) }); err != nil {
		return s.finishPurge(start, b.String(), err, "hot rewrite failed")
	}
	if err := run("prune hot", func() (string, error) { return hotRepo.Prune(ctx) }); err != nil {
		return s.finishPurge(start, b.String(), err, "hot prune failed")
	}
	for _, o := range s.cfg.Load().Offsite {
		cl := s.offsiteC(o)
		_ = cl.Unlock(ctx)
		if err := run("rewrite "+o.Label, func() (string, error) { return cl.RewriteExcludePath(ctx, host, targetPath) }); err != nil {
			return s.finishPurge(start, b.String(), err, o.Label+" rewrite failed")
		}
		if err := run("prune "+o.Label, func() (string, error) { return cl.Prune(ctx) }); err != nil {
			return s.finishPurge(start, b.String(), err, o.Label+" prune failed")
		}
	}

	err := s.finishPurge(start, b.String(), nil,
		fmt.Sprintf("purged %q from all versions of %s (hot + offsite)", targetPath, host))
	s.RefreshClients(ctx)
	return err
}

// PurgePathInVersion removes targetPath from a single version (hotSnapID) and
// its cold twin, then prunes both. Use this for "delete from this version only";
// PurgePath is the "all versions" variant.
func (s *Scheduler) PurgePathInVersion(ctx context.Context, hotSnapID, targetPath string) error {
	if strings.TrimSpace(targetPath) == "" || hotSnapID == "" {
		return fmt.Errorf("missing version or path")
	}
	if !s.acquire("purge") {
		return fmt.Errorf("busy: %s is running", s.Running())
	}
	defer s.release()

	start := time.Now()
	var b strings.Builder

	// Look up host+time to find the cold twin.
	hotSnaps, err := s.hot.Snapshots(ctx)
	if err != nil {
		return s.finishPurge(start, "", err, "list hot snapshots failed")
	}
	hotRepo, err := s.hot.ForSnapshot(ctx, hotSnapID)
	if err != nil {
		return s.finishPurge(start, "", err, "snapshot not found")
	}
	var host string
	var when time.Time
	for _, sn := range hotSnaps {
		if strings.HasPrefix(sn.ID, hotSnapID) || sn.ShortID == hotSnapID {
			host, when = sn.Hostname, sn.Time
			break
		}
	}

	_ = hotRepo.Unlock(ctx)
	out, err := hotRepo.RewriteExcludePathSnap(ctx, hotSnapID, targetPath)
	fmt.Fprintf(&b, "== rewrite hot version ==\n%s\n", strings.TrimSpace(out))
	if err != nil {
		return s.finishPurge(start, b.String(), err, "hot rewrite failed")
	}
	if _, err := hotRepo.Prune(ctx); err != nil {
		fmt.Fprintf(&b, "prune hot error: %v\n", err)
	}

	if !when.IsZero() {
		for _, st := range s.cfg.Load().Offsite {
			cl := s.offsiteC(st)
			snaps, err := cl.Snapshots(ctx)
			if err != nil {
				continue
			}
			for _, cs := range snaps {
				if cs.Hostname == host && cs.Time.Equal(when) {
					_ = cl.Unlock(ctx)
					o, e := cl.RewriteExcludePathSnap(ctx, cs.ID, targetPath)
					fmt.Fprintf(&b, "== rewrite %s ==\n%s\n", st.Label, strings.TrimSpace(o))
					if e == nil {
						_, _ = cl.Prune(ctx)
					}
					break
				}
			}
		}
	}

	err = s.finishPurge(start, b.String(), nil,
		fmt.Sprintf("removed %q from version %s (hot + offsite twins)", targetPath, hotSnapID))
	s.RefreshClients(ctx)
	return err
}

// DeleteSnapshot deletes one whole version. It removes the hot snapshot by ID
// and the matching cold snapshot (same host + timestamp, which restic copy
// preserves), then prunes both.
func (s *Scheduler) DeleteSnapshot(ctx context.Context, hotSnapID string) error {
	if !s.acquire("delete-version") {
		return fmt.Errorf("busy: %s is running", s.Running())
	}
	defer s.release()

	start := time.Now()
	var b strings.Builder

	// Identify the hot snapshot so we can find its cold twin by host+time.
	hotSnaps, err := s.hot.Snapshots(ctx)
	if err != nil {
		return s.finishDelete(start, "", err, "list hot snapshots failed")
	}
	hotRepo, err := s.hot.ForSnapshot(ctx, hotSnapID)
	if err != nil {
		return s.finishDelete(start, "", err, "snapshot not found")
	}
	var target *struct {
		host string
		t    time.Time
	}
	for _, sn := range hotSnaps {
		if strings.HasPrefix(sn.ID, hotSnapID) || sn.ShortID == hotSnapID {
			target = &struct {
				host string
				t    time.Time
			}{sn.Hostname, sn.Time}
			break
		}
	}

	_ = hotRepo.Unlock(ctx)
	out, err := hotRepo.ForgetSnapshot(ctx, hotSnapID)
	fmt.Fprintf(&b, "== forget hot ==\n%s\n", strings.TrimSpace(out))
	if err != nil {
		return s.finishDelete(start, b.String(), err, "forget hot failed")
	}
	if _, err := hotRepo.Prune(ctx); err != nil {
		fmt.Fprintf(&b, "prune hot error: %v\n", err)
	}

	// Best-effort: forget the twin on every offsite storage.
	if target != nil {
		for _, st := range s.cfg.Load().Offsite {
			cl := s.offsiteC(st)
			snaps, err := cl.Snapshots(ctx)
			if err != nil {
				continue
			}
			for _, cs := range snaps {
				if cs.Hostname == target.host && cs.Time.Equal(target.t) {
					_ = cl.Unlock(ctx)
					o, e := cl.ForgetSnapshot(ctx, cs.ID)
					fmt.Fprintf(&b, "== forget twin on %s ==\n%s\n", st.Label, strings.TrimSpace(o))
					if e == nil {
						_, _ = cl.Prune(ctx)
					}
					break
				}
			}
		}
	}

	err = s.finishDelete(start, b.String(), nil, "deleted version "+hotSnapID+" (hot + offsite twins)")
	s.RefreshClients(ctx)
	return err
}

func (s *Scheduler) finishPurge(start time.Time, output string, err error, okMsg string) error {
	res := state.JobResult{Job: "purge", StartedAt: start, EndedAt: time.Now(), Output: output}
	if err != nil {
		res.OK = false
		res.Message = okMsg + ": " + err.Error()
		s.store.RecordJob(res)
		s.alert("purge failed", res.Message)
		return err
	}
	res.OK = true
	res.Message = okMsg
	s.store.RecordJob(res)
	return nil
}

func (s *Scheduler) finishDelete(start time.Time, output string, err error, okMsg string) error {
	res := state.JobResult{Job: "delete-version", StartedAt: start, EndedAt: time.Now(), Output: output}
	if err != nil {
		res.OK = false
		res.Message = okMsg + ": " + err.Error()
		s.store.RecordJob(res)
		return err
	}
	res.OK = true
	res.Message = okMsg
	s.store.RecordJob(res)
	return nil
}
