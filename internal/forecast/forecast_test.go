package forecast

import (
	"testing"
	"time"

	"github.com/nauski/hoard/internal/state"
)

func day(n int) time.Time { return time.Date(2026, 1, 1+n, 0, 0, 0, 0, time.UTC) }

func TestProjectLinearGrowth(t *testing.T) {
	// hot grows 1 GiB/day for 10 days; cold grows 2 GiB/day.
	var s []state.SizeSample
	for i := 0; i <= 10; i++ {
		s = append(s, state.SizeSample{At: day(i), HotStored: int64(i) << 30, ColdStored: int64(2*i) << 30})
	}
	p := Project(s, 90*24*time.Hour, day(10))
	if !p.HaveData {
		t.Fatal("want HaveData")
	}
	// slope ≈ 1 GiB/day hot, 2 GiB/day cold (allow ±1%)
	if d := p.HotPerDay - (1 << 30); d > (1<<30)/100 || d < -(1<<30)/100 {
		t.Fatalf("hot/day: got %d", p.HotPerDay)
	}
	if d := p.ColdPerDay - (2 << 30); d > (2<<30)/100 || d < -(2<<30)/100 {
		t.Fatalf("cold/day: got %d", p.ColdPerDay)
	}
	// +90d from last (10 GiB) ≈ 100 GiB hot
	want := int64(100) << 30
	if d := p.Hot90d - want; d > want/50 || d < -want/50 {
		t.Fatalf("hot 90d: got %d want ~%d", p.Hot90d, want)
	}
	if !p.ProjectedAt.Equal(day(100)) {
		t.Fatalf("projected at: got %v", p.ProjectedAt)
	}
}

func TestProjectFlatAndNegative(t *testing.T) {
	var flat []state.SizeSample
	for i := 0; i <= 5; i++ {
		flat = append(flat, state.SizeSample{At: day(i), HotStored: 100 << 30})
	}
	if p := Project(flat, 90*24*time.Hour, day(5)); p.HotPerDay != 0 || p.Hot90d != 100<<30 {
		t.Fatalf("flat: perDay=%d 90d=%d", p.HotPerDay, p.Hot90d)
	}
	// shrinking hot must clamp projection at >= 0, never negative
	var shrink []state.SizeSample
	for i := 0; i <= 5; i++ {
		shrink = append(shrink, state.SizeSample{At: day(i), HotStored: int64(50-10*i) << 30})
	}
	p := Project(shrink, 90*24*time.Hour, day(5))
	if p.HotPerDay >= 0 {
		t.Fatalf("want negative slope, got %d", p.HotPerDay)
	}
	if p.Hot90d < 0 {
		t.Fatalf("projection must clamp >=0, got %d", p.Hot90d)
	}
}

func TestProjectInsufficientData(t *testing.T) {
	if p := Project(nil, 90*24*time.Hour, day(0)); p.HaveData {
		t.Fatal("nil -> no data")
	}
	one := []state.SizeSample{{At: day(0), HotStored: 1 << 30}}
	if p := Project(one, 90*24*time.Hour, day(0)); p.HaveData {
		t.Fatal("single sample -> no data")
	}
	// two samples < 24h apart -> not enough span
	close2 := []state.SizeSample{{At: day(0), HotStored: 1 << 30}, {At: day(0).Add(time.Hour), HotStored: 2 << 30}}
	if p := Project(close2, 90*24*time.Hour, day(0)); p.HaveData {
		t.Fatal("span < 24h -> no data")
	}
}

func TestColdIgnoresLeadingZeros(t *testing.T) {
	// cold configured only from day 3 onward (0 before) — fit only positive cold points
	s := []state.SizeSample{
		{At: day(0), HotStored: 1 << 30, ColdStored: 0},
		{At: day(1), HotStored: 2 << 30, ColdStored: 0},
		{At: day(3), HotStored: 4 << 30, ColdStored: 10 << 30},
		{At: day(5), HotStored: 6 << 30, ColdStored: 14 << 30},
	}
	p := Project(s, 90*24*time.Hour, day(5))
	if p.ColdPerDay != (2 << 30) { // (14-10)/(5-3) = 2 GiB/day
		t.Fatalf("cold/day ignoring zeros: got %d want %d", p.ColdPerDay, 2<<30)
	}
}

func TestDueForSample(t *testing.T) {
	now := day(2)
	if !DueForSample(time.Time{}, now, 24*time.Hour) {
		t.Fatal("zero last -> due")
	}
	if DueForSample(now.Add(-23*time.Hour), now, 24*time.Hour) {
		t.Fatal("23h -> not due")
	}
	if !DueForSample(now.Add(-25*time.Hour), now, 24*time.Hour) {
		t.Fatal("25h -> due")
	}
}
