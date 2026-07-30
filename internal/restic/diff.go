package restic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// DiffChange is one changed path between two snapshots.
type DiffChange struct {
	Path     string `json:"path"`
	Modifier string `json:"modifier"` // "+" added, "-" removed, "M" modified, "T" type
}

// DiffStat summarises `restic diff a b` with a capped change list.
type DiffStat struct {
	Added        int          `json:"added"`
	Removed      int          `json:"removed"`
	Changed      int          `json:"changed"`
	AddedBytes   uint64       `json:"added_bytes"`
	RemovedBytes uint64       `json:"removed_bytes"`
	Changes      []DiffChange `json:"changes"`
}

const diffChangeCap = 200

// Diff runs `restic diff idA idB --json` and returns the statistics + a capped
// list of changed paths. Read-only.
func (c *Client) Diff(ctx context.Context, idA, idB string) (*DiffStat, error) {
	cmd := exec.CommandContext(ctx, c.bin, append(c.globalArgs(), "diff", idA, idB, "--json")...)
	cmd.Env = append(cmd.Environ(), c.env()...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("restic diff: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	d := &DiffStat{}
	sc := bufio.NewScanner(bytes.NewReader(out.Bytes()))
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var probe struct {
			MessageType string `json:"message_type"`
		}
		if json.Unmarshal(line, &probe) != nil {
			continue
		}
		switch probe.MessageType {
		case "change":
			var ch struct {
				Path     string `json:"path"`
				Modifier string `json:"modifier"`
			}
			if json.Unmarshal(line, &ch) == nil && len(d.Changes) < diffChangeCap {
				d.Changes = append(d.Changes, DiffChange{Path: ch.Path, Modifier: strings.TrimSpace(ch.Modifier)})
			}
		case "statistics":
			var st struct {
				ChangedFiles int `json:"changed_files"`
				Added        struct {
					Files int    `json:"files"`
					Bytes uint64 `json:"bytes"`
				} `json:"added"`
				Removed struct {
					Files int    `json:"files"`
					Bytes uint64 `json:"bytes"`
				} `json:"removed"`
			}
			if json.Unmarshal(line, &st) == nil {
				d.Added, d.AddedBytes = st.Added.Files, st.Added.Bytes
				d.Removed, d.RemovedBytes = st.Removed.Files, st.Removed.Bytes
				d.Changed = st.ChangedFiles
			}
		}
	}
	return d, nil
}
