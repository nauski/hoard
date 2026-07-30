package scheduler

import (
	"context"
	"time"

	"github.com/nauski/hoard/internal/restic"
	"github.com/nauski/hoard/internal/state"
)

// Restore restores from the hot repo to target. It holds the job lock (mutually
// exclusive with mirror/check/prune/verify) and records a JobResult. Progress is
// delivered through hooks supplied by the caller (the API pushes them to the
// live dashboard).
func (s *Scheduler) Restore(ctx context.Context, snapID, subpath, target, overwrite string, hooks restic.RestoreHooks) (*restic.RestoreResult, error) {
	if !s.acquire("restore") {
		return nil, errBusy
	}
	defer s.release()

	start := time.Now()
	res, out, err := s.hotC().Restore(ctx, snapID, subpath, target, overwrite, false, hooks)
	jr := state.JobResult{Job: "restore", StartedAt: start, EndedAt: time.Now(), Output: out}
	if err != nil {
		jr.OK = false
		jr.Message = err.Error()
		s.store.RecordJob(jr)
		return res, err
	}
	jr.OK = true
	if res != nil {
		jr.Message = "restored " + humanBytes(res.BytesRestored) + " to " + target
	} else {
		jr.Message = "restored to " + target
	}
	s.store.RecordJob(jr)
	return res, nil
}
