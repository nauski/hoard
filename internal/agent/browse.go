package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// BrowseEntry is one directory in a browse listing.
type BrowseEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
}

// BrowseResult is the directory listing returned to the GUI folder picker.
type BrowseResult struct {
	Path    string        `json:"path"`    // absolute path being listed
	Parent  string        `json:"parent"`  // parent dir, "" if at filesystem root
	Home    string        `json:"home"`    // user's home dir (a convenient start)
	Entries []BrowseEntry `json:"entries"` // subdirectories, sorted
}

// browse lists the subdirectories of path (defaulting to the user's home). It
// only ever exposes what the running user can already read — the agent binds to
// localhost, so this is a single-user, same-privilege convenience, not a remote
// filesystem API. Only directories are returned (symlinks to dirs included),
// since backup targets are folders; individual files can still be typed by hand.
func browse(path string) (BrowseResult, error) {
	home, _ := os.UserHomeDir()
	if strings.TrimSpace(path) == "" {
		path = home
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return BrowseResult{}, err
	}
	abs = filepath.Clean(abs)

	// If handed a file, list its containing directory instead.
	if info, err := os.Stat(abs); err == nil && !info.IsDir() {
		abs = filepath.Dir(abs)
	}

	des, err := os.ReadDir(abs)
	if err != nil {
		return BrowseResult{}, err
	}

	entries := make([]BrowseEntry, 0, len(des))
	for _, de := range des {
		isDir := de.IsDir()
		if !isDir && de.Type()&os.ModeSymlink != 0 {
			// Resolve symlinks so links pointing at directories still show up.
			if ti, err := os.Stat(filepath.Join(abs, de.Name())); err == nil && ti.IsDir() {
				isDir = true
			}
		}
		if !isDir {
			continue
		}
		entries = append(entries, BrowseEntry{
			Name:  de.Name(),
			Path:  filepath.Join(abs, de.Name()),
			IsDir: true,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})

	parent := filepath.Dir(abs)
	if parent == abs { // reached filesystem root
		parent = ""
	}
	return BrowseResult{Path: abs, Parent: parent, Home: home, Entries: entries}, nil
}

// makeDir creates a single new directory named name inside parent (defaulting to
// the user's home) and returns its absolute path. name must be a plain folder
// name — no path separators, "." or ".." — so it can only create a child of
// parent, never escape it. An already-existing directory is treated as success
// so "New folder" is idempotent. Same single-user, same-privilege model as
// browse: the agent binds localhost and acts as the running user.
func makeDir(parent, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("folder name required")
	}
	if name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("invalid folder name %q", name)
	}
	if strings.TrimSpace(parent) == "" {
		if home, _ := os.UserHomeDir(); home != "" {
			parent = home
		}
	}
	abs, err := filepath.Abs(filepath.Join(parent, name))
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(abs, 0o755); err != nil && !os.IsExist(err) {
		return "", err
	}
	return abs, nil
}
