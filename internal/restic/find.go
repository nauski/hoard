package restic

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
)

// FindMatch is one path `restic find` matched inside a snapshot.
type FindMatch struct {
	Path  string    `json:"path"`
	Type  string    `json:"type"`
	Size  uint64    `json:"size"`
	MTime time.Time `json:"mtime"`
}

// FindHit is the matches inside one snapshot (full snapshot ID).
type FindHit struct {
	Snapshot string      `json:"snapshot"`
	Matches  []FindMatch `json:"matches"`
}

// Found is one matched path with its history across a host's snapshots: the
// newest snapshot that still has it and, if it vanished, the first one after
// that (the answer to "when did this disappear, and where do I get it back").
type Found struct {
	Path     string     `json:"path"`
	Type     string     `json:"type"`
	Size     uint64     `json:"size"`
	Versions int        `json:"versions"` // snapshots containing the path
	LastID   string     `json:"last_id"`
	LastTime time.Time  `json:"last_time"`
	GoneID   string     `json:"gone_id,omitempty"` // empty = still in the newest snapshot
	GoneTime *time.Time `json:"gone_time,omitempty"`
}

// maxFound caps a timeline so a broad pattern cannot flood the UI.
const maxFound = 200

// Find runs `restic find` over host's snapshots. pattern is a glob matched
// against file names ("wt-perf", "*.jsonl"); a pattern containing "/" is
// matched against full paths and also returns everything beneath a matching
// directory, which Timeline collapses.
func (c *Client) Find(ctx context.Context, host, pattern string) ([]FindHit, error) {
	out, err := c.run(ctx, "find", "--json", "--no-lock", "--host", host, pattern)
	if err != nil {
		return nil, err
	}
	var hits []FindHit
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(out, &hits); err != nil {
		return nil, fmt.Errorf("parse find: %w", err)
	}
	return hits, nil
}

// Timeline folds find hits into one entry per path. snaps must be the same
// host's snapshots. Paths below another matched directory are dropped (the
// directory stands for them). Vanished paths come first, most recent first.
func Timeline(hits []FindHit, snaps []Snapshot) []Found {
	snaps = slices.Clone(snaps)
	slices.SortFunc(snaps, func(a, b Snapshot) int { return a.Time.Compare(b.Time) })
	index := map[string]int{} // full ID -> position in time order
	for i, s := range snaps {
		index[s.ID] = i
	}
	byPath := map[string]*Found{}
	last := map[string]int{}
	for _, h := range hits {
		i, ok := index[h.Snapshot]
		if !ok {
			continue
		}
		dirs := map[string]bool{}
		for _, m := range h.Matches {
			if m.Type == "dir" {
				dirs[m.Path] = true
			}
		}
		for _, m := range h.Matches {
			if underAny(m.Path, dirs) {
				continue
			}
			f := byPath[m.Path]
			if f == nil {
				f = &Found{Path: m.Path}
				byPath[m.Path] = f
				last[m.Path] = -1
			}
			f.Versions++
			if i > last[m.Path] {
				last[m.Path] = i
				f.Type, f.Size = m.Type, m.Size
				f.LastID, f.LastTime = snaps[i].ShortID, snaps[i].Time
			}
		}
	}
	out := make([]Found, 0, len(byPath))
	for p, f := range byPath {
		if next := last[p] + 1; next < len(snaps) {
			t := snaps[next].Time
			f.GoneID, f.GoneTime = snaps[next].ShortID, &t
		}
		out = append(out, *f)
	}
	slices.SortFunc(out, func(a, b Found) int {
		if (a.GoneTime == nil) != (b.GoneTime == nil) {
			if a.GoneTime != nil {
				return -1
			}
			return 1
		}
		if c := b.LastTime.Compare(a.LastTime); c != 0 {
			return c
		}
		return strings.Compare(a.Path, b.Path)
	})
	if len(out) > maxFound {
		out = out[:maxFound]
	}
	return out
}

// underAny reports whether p lies strictly below one of dirs.
func underAny(p string, dirs map[string]bool) bool {
	for d := path.Dir(p); d != "/" && d != "."; d = path.Dir(d) {
		if dirs[d] {
			return true
		}
	}
	return false
}

// hostSnapshots lists host's snapshots, newest first.
func (c *Client) hostSnapshots(ctx context.Context, host string) ([]Snapshot, error) {
	all, err := c.Snapshots(ctx)
	if err != nil {
		return nil, err
	}
	var out []Snapshot
	for _, s := range all {
		if s.Hostname == host {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b Snapshot) int { return b.Time.Compare(a.Time) })
	return out, nil
}

// FindTimeline searches host's snapshots for pattern and returns each matched
// path's history.
func (c *Client) FindTimeline(ctx context.Context, host, pattern string) ([]Found, error) {
	snaps, err := c.hostSnapshots(ctx, host)
	if err != nil {
		return nil, err
	}
	hits, err := c.Find(ctx, host, pattern)
	if err != nil {
		return nil, err
	}
	return Timeline(hits, snaps), nil
}

// LastContaining returns the newest snapshot of host, taken before `before`
// (zero = no limit), that contains the exact path p, plus p's type ("dir" or
// "file"). It walks back from the newest snapshot with one cheap directory
// listing each, so a recent deletion is found in a few steps.
func (c *Client) LastContaining(ctx context.Context, host, p string, before time.Time) (*Snapshot, string, error) {
	snaps, err := c.hostSnapshots(ctx, host)
	if err != nil {
		return nil, "", err
	}
	for i := range snaps {
		if !before.IsZero() && !snaps[i].Time.Before(before) {
			continue
		}
		entries, err := c.Ls(ctx, snaps[i].ID, path.Dir(p))
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		if err != nil {
			continue // the parent directory is not in this snapshot
		}
		for _, e := range entries {
			if e.Path == p {
				return &snaps[i], e.Type, nil
			}
		}
	}
	return nil, "", fmt.Errorf("%s is not in any snapshot of %s", p, host)
}
