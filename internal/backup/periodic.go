package backup

import (
	"context"
	"time"
)

// RunPeriodically calls fn every interval until ctx is done, calling it once
// immediately first iff immediate is true. Shared by every "check once, then
// keep checking on a fixed schedule" loop in this codebase (the outstanding-
// upload monitor, the stale-receiver monitor, and a job's own interval-based
// schedule), which otherwise each hand-roll the same ticker/select loop.
func RunPeriodically(ctx context.Context, interval time.Duration, immediate bool, fn func()) {
	if immediate {
		fn()
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fn()
		}
	}
}
