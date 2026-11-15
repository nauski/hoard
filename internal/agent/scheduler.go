package agent

import (
	"context"
	"fmt"
	"time"
)

// nextRun returns when the next scheduled backup is due. schedule is either an
// interval ("1h", "30m") or a daily local "HH:MM". last is when the previous
// backup started (zero = never). boot is when this agent process started; it
// stands in for last on a daily schedule with no history, so an upgrade does
// not trigger an immediate backup. A result at or before now means "due".
func nextRun(schedule string, last, boot, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(schedule); err == nil {
		if d < time.Minute {
			return time.Time{}, fmt.Errorf("schedule interval %q is shorter than 1m", schedule)
		}
		if last.IsZero() {
			return now, nil
		}
		return last.Add(d), nil
	}
	hm, err := time.Parse("15:04", schedule)
	if err != nil {
		return time.Time{}, fmt.Errorf("schedule %q is neither HH:MM nor an interval like 1h", schedule)
	}
	from := last
	if from.IsZero() {
		from = boot
	}
	from = from.In(now.Location())
	next := time.Date(from.Year(), from.Month(), from.Day(), hm.Hour(), hm.Minute(), 0, 0, now.Location())
	if !next.After(from) {
		next = next.AddDate(0, 0, 1)
	}
	return next, nil
}

// NextRun returns when the next scheduled backup is due, or the zero time if
// no schedule is set.
func (a *Agent) NextRun(now time.Time) (time.Time, error) {
	a.mu.Lock()
	eff, _ := a.cfg.effective()
	sched, last, boot := eff.Schedule, a.lastStart, a.boot
	a.mu.Unlock()
	if sched == "" {
		return time.Time{}, nil
	}
	return nextRun(sched, last, boot, now)
}

// RunScheduler starts a backup whenever one is due. It checks once at start
// (catching up a slot missed while the agent was stopped) and then every
// minute (catching up after sleep). The schedule is re-read on each check, so
// GUI changes take effect without a restart. Blocks until ctx is cancelled.
func (a *Agent) RunScheduler(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	var skipped time.Time // the due slot we already logged a skip for
	for {
		// Check in first, so settings changed on the server dashboard apply
		// before this minute's schedule decision.
		requested := a.CheckIn(ctx, true)
		now := time.Now()
		next, err := a.NextRun(now)
		switch {
		case requested:
			// a backup was just started on the server's request
		case err != nil:
			a.log.Warn("schedule ignored", "err", err)
		case next.IsZero() || now.Before(next):
			// nothing due
		default:
			if _, running := a.LastRun(); running {
				if !skipped.Equal(next) {
					skipped = next
					a.log.Info("scheduled backup skipped, previous run still going")
				}
			} else {
				go func() {
					if err := a.Backup(ctx); err != nil {
						a.log.Error("scheduled backup failed", "err", err)
					}
				}()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
