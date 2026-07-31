package state

import (
	"testing"
	"time"
)

func TestAppendSizeSampleCapAndSnapshot(t *testing.T) {
	s, err := Load(t.TempDir() + "/state.json")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 450; i++ {
		s.AppendSizeSample(SizeSample{At: base.Add(time.Duration(i) * time.Hour), HotStored: int64(i), ColdStored: int64(2 * i)})
	}
	snap := s.SizeSamplesSnapshot()
	if len(snap) != 400 {
		t.Fatalf("cap: got %d want 400", len(snap))
	}
	if snap[0].HotStored != 50 { // oldest 50 dropped (0..49)
		t.Fatalf("oldest kept: got %d want 50", snap[0].HotStored)
	}
	if snap[len(snap)-1].HotStored != 449 {
		t.Fatalf("newest: got %d want 449", snap[len(snap)-1].HotStored)
	}
	// snapshot is a copy: mutating it must not affect the store
	snap[0].HotStored = -1
	if s.SizeSamplesSnapshot()[0].HotStored != 50 {
		t.Fatal("snapshot must be a copy")
	}
}

func TestLastSizeSampleAtAndRoundTrip(t *testing.T) {
	path := t.TempDir() + "/state.json"
	s, _ := Load(path)
	if !s.LastSizeSampleAt().IsZero() {
		t.Fatal("empty store must report zero last-sample time")
	}
	at := time.Date(2026, 2, 3, 4, 5, 0, 0, time.UTC)
	s.AppendSizeSample(SizeSample{At: at, HotStored: 10, ColdStored: 20})
	// reload from disk
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.LastSizeSampleAt().Equal(at) {
		t.Fatalf("round-trip last: got %v want %v", s2.LastSizeSampleAt(), at)
	}
	if len(s2.SizeSamplesSnapshot()) != 1 {
		t.Fatal("round-trip sample count")
	}
}
