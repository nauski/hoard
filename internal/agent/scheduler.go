package agent

import (
	"context"
	"time"
)

// RunScheduler fires a daily backup at the agent's configured "HH:MM" time.
// It re-reads the schedule each tick so GUI changes take effect without a
// restart. Blocks until ctx is cancelled.
func (a *Agent) RunScheduler(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	var lastFired string
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			want := a.ScheduleTime()
			if want == "" {
				continue
			}
			stamp := now.Format("2006-01-02 15:04")
			if now.Format("15:04") == want && stamp != lastFired {
				lastFired = stamp
				go func() {
					if err := a.Backup(ctx); err != nil {
						a.log.Error("scheduled backup failed", "err", err)
					}
				}()
			}
		}
	}
}
