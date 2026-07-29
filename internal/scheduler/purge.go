package scheduler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nauski/hoard/internal/state"
)

// PurgePath removes targetPath (a file or folder, and everything under it) from
// EVERY snapshot of host, in BOTH the hot and cold (e2) repos, then prunes both
// to actually reclaim space. Running on both repos is what makes the deletion
// stick: purging only cold would let the next mirror re-copy the path from hot;
// purging only hot would leave the data occupying e2. Serialized against the
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

	// Hot first (the source of truth), then cold (e2).
	_ = s.hot.Unlock(ctx)
	if err := run("rewrite hot", func() (string, error) { return s.hot.RewriteExcludePath(ctx, host, targetPath) }); err != nil {
		return s.finishPurge(start, b.String(), err, "hot rewrite failed")
	}
	if err := run("prune hot", func() (string, error) { return s.hot.Prune(ctx) }); err != nil {
		return s.finishPurge(start, b.String(), err, "hot prune failed")
	}
	_ = s.cold.Unlock(ctx)
	if err := run("rewrite cold(e2)", func() (string, error) { return s.cold.RewriteExcludePath(ctx, host, targetPath) }); err != nil {
		return s.finishPurge(start, b.String(), err, "cold rewrite failed")
	}
	if err := run("prune cold(e2)", func() (string, error) { return s.cold.Prune(ctx) }); err != nil {
		return s.finishPurge(start, b.String(), err, "cold prune failed")
	}

	err := s.finishPurge(start, b.String(), nil,
		fmt.Sprintf("purged %q from all versions of %s (hot + e2)", targetPath, host))
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
	var host string
	var when time.Time
	for _, sn := range hotSnaps {
		if strings.HasPrefix(sn.ID, hotSnapID) || sn.ShortID == hotSnapID {
			host, when = sn.Hostname, sn.Time
			break
		}
	}

	_ = s.hot.Unlock(ctx)
	out, err := s.hot.RewriteExcludePathSnap(ctx, hotSnapID, targetPath)
	fmt.Fprintf(&b, "== rewrite hot version ==\n%s\n", strings.TrimSpace(out))
	if err != nil {
		return s.finishPurge(start, b.String(), err, "hot rewrite failed")
	}
	if _, err := s.hot.Prune(ctx); err != nil {
		fmt.Fprintf(&b, "prune hot error: %v\n", err)
	}

	if !when.IsZero() {
		if coldSnaps, err := s.cold.Snapshots(ctx); err == nil {
			for _, cs := range coldSnaps {
				if cs.Hostname == host && cs.Time.Equal(when) {
					_ = s.cold.Unlock(ctx)
					o, e := s.cold.RewriteExcludePathSnap(ctx, cs.ID, targetPath)
					fmt.Fprintf(&b, "== rewrite cold twin %s ==\n%s\n", cs.ShortID, strings.TrimSpace(o))
					if e == nil {
						_, _ = s.cold.Prune(ctx)
					}
					break
				}
			}
		}
	}

	err = s.finishPurge(start, b.String(), nil,
		fmt.Sprintf("removed %q from version %s (hot + e2 twin)", targetPath, hotSnapID))
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

	_ = s.hot.Unlock(ctx)
	out, err := s.hot.ForgetSnapshot(ctx, hotSnapID)
	fmt.Fprintf(&b, "== forget hot ==\n%s\n", strings.TrimSpace(out))
	if err != nil {
		return s.finishDelete(start, b.String(), err, "forget hot failed")
	}
	if _, err := s.hot.Prune(ctx); err != nil {
		fmt.Fprintf(&b, "prune hot error: %v\n", err)
	}

	// Best-effort: forget the cold twin.
	if target != nil {
		if coldSnaps, err := s.cold.Snapshots(ctx); err == nil {
			for _, cs := range coldSnaps {
				if cs.Hostname == target.host && cs.Time.Equal(target.t) {
					_ = s.cold.Unlock(ctx)
					o, e := s.cold.ForgetSnapshot(ctx, cs.ID)
					fmt.Fprintf(&b, "== forget cold twin %s ==\n%s\n", cs.ShortID, strings.TrimSpace(o))
					if e == nil {
						_, _ = s.cold.Prune(ctx)
					}
					break
				}
			}
		}
	}

	err = s.finishDelete(start, b.String(), nil, "deleted version "+hotSnapID+" (hot + e2 twin)")
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
