// Package forecast projects repo-size growth from recorded size samples.
package forecast

import (
	"math"
	"time"

	"github.com/nauski/hoard/internal/state"
)

type Projection struct {
	HaveData    bool      `json:"have_data"`
	SampleCount int       `json:"sample_count"`
	FirstAt     time.Time `json:"first_at"`
	LastAt      time.Time `json:"last_at"`
	HotStored   int64     `json:"hot_stored"`
	HotPerDay   int64     `json:"hot_per_day"`
	Hot90d      int64     `json:"hot_90d"`
	ProjectedAt time.Time `json:"projected_at"`
}

// DueForSample reports whether a new sample is due (last zero, or old enough).
func DueForSample(last, now time.Time, interval time.Duration) bool {
	return last.IsZero() || now.Sub(last) >= interval
}

type pt struct {
	days float64 // days since epoch reference
	y    float64
}

// fit returns (slopePerDay, valueAt(atDays)) for the least-squares line, and ok.
func fit(pts []pt, atDays float64) (slope, valAt float64, ok bool) {
	if len(pts) < 2 {
		return 0, 0, false
	}
	var sx, sy, sxx, sxy float64
	n := float64(len(pts))
	for _, p := range pts {
		sx += p.days
		sy += p.y
		sxx += p.days * p.days
		sxy += p.days * p.y
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0, 0, false
	}
	slope = (n*sxy - sx*sy) / den
	intercept := (sy - slope*sx) / n
	return slope, intercept + slope*atDays, true
}

func Project(samples []state.SizeSample, horizon time.Duration, now time.Time) Projection {
	p := Projection{SampleCount: len(samples)}
	if len(samples) == 0 {
		return p
	}
	ref := samples[0].At
	toDays := func(t time.Time) float64 { return t.Sub(ref).Hours() / 24 }
	first, last := samples[0].At, samples[len(samples)-1].At
	p.FirstAt, p.LastAt = first, last
	p.HotStored = samples[len(samples)-1].HotStored

	var hot []pt
	for _, s := range samples {
		hot = append(hot, pt{toDays(s.At), float64(s.HotStored)})
	}
	atDays := toDays(last.Add(horizon))
	p.ProjectedAt = last.Add(horizon)

	if last.Sub(first) >= 24*time.Hour {
		if slope, val, ok := fit(hot, atDays); ok {
			p.HaveData = true
			p.HotPerDay = int64(slope)
			p.Hot90d = clampInt64(val)
		}
	}
	return p
}

// Storage is the size projection of one offsite storage.
type Storage struct {
	Stored int64 `json:"stored"`
	PerDay int64 `json:"per_day"`
	In90d  int64 `json:"in_90d"`
}

// ProjectStorage projects one offsite storage's size. Samples taken before
// per-storage sizes existed only have ColdStored; first says to count that as
// this storage's size.
func ProjectStorage(samples []state.SizeSample, id string, first bool, horizon time.Duration) Storage {
	var pts []pt
	var out Storage
	if len(samples) == 0 {
		return out
	}
	ref := samples[0].At
	toDays := func(t time.Time) float64 { return t.Sub(ref).Hours() / 24 }
	for _, s := range samples {
		v, ok := s.Offsite[id]
		if !ok && first && s.Offsite == nil {
			v = s.ColdStored
		}
		if v > 0 {
			out.Stored = v
			pts = append(pts, pt{toDays(s.At), float64(v)})
		}
	}
	if len(pts) >= 2 && pts[len(pts)-1].days-pts[0].days >= 1 {
		if slope, val, ok := fit(pts, toDays(samples[len(samples)-1].At.Add(horizon))); ok {
			out.PerDay, out.In90d = int64(slope), clampInt64(val)
		}
	}
	return out
}

func clampInt64(f float64) int64 {
	if f < 0 || math.IsNaN(f) {
		return 0
	}
	if f > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(f)
}
