package scheduler

import (
	"context"
	"testing"
)

// TestMaybeSampleSizeOncePerDay verifies the 24h cadence: two calls in quick
// succession (no fake restic client exists in this package, so this reuses
// the real-local-repo harness from verify_test.go) must record exactly one
// sample, not two.
func TestMaybeSampleSizeOncePerDay(t *testing.T) {
	s := newVerifyScheduler(t) // hot repo has one snapshot; no cold configured
	ctx := context.Background()

	s.maybeSampleSize(ctx)
	s.maybeSampleSize(ctx)

	samples := s.store.SizeSamplesSnapshot()
	if len(samples) != 1 {
		t.Fatalf("expected exactly 1 sample after two calls within 24h, got %d: %+v", len(samples), samples)
	}
	if samples[0].HotStored <= 0 {
		t.Fatalf("expected positive hot size, got %+v", samples[0])
	}
	if samples[0].ColdStored != 0 {
		t.Fatalf("expected cold=0 (unconfigured), got %+v", samples[0])
	}
}
