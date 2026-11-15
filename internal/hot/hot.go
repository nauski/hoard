// Package hot presents the server's hot storage as one thing even though it
// can be several restic repositories: the shared repo every client used to
// push into, plus one private repository per enrolled client.
package hot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/restic"
)

// Shared is the key of the shared hot repo; every other key is a client name.
const Shared = ""

// Set resolves hosts and snapshot IDs to the repository that holds them.
type Set struct {
	bin      string
	cfg      *config.Store
	lockWait string
	log      *slog.Logger

	mu     sync.Mutex
	bySnap map[string]string // full snapshot ID -> repo key
	byHost map[string]string // snapshot hostname -> repo key
}

func New(bin string, cfg *config.Store, lockWait string, log *slog.Logger) *Set {
	return &Set{bin: bin, cfg: cfg, lockWait: lockWait, log: log}
}

// Keys lists the repositories: the shared repo first (left out when clients
// have their own repos and the shared one was never created), then clients.
func (h *Set) Keys() []string {
	c := h.cfg.Load()
	var keys []string
	if len(c.Clients) == 0 || sharedExists(c) {
		keys = append(keys, Shared)
	}
	names := make([]string, 0, len(c.Clients))
	for n := range c.Clients {
		names = append(names, n)
	}
	slices.Sort(names)
	return append(keys, names...)
}

func sharedExists(c *config.Config) bool {
	if strings.Contains(c.Hot.Repository, ":") {
		return true // not a plain path; cannot tell, so assume it is there
	}
	_, err := os.Stat(filepath.Join(c.Hot.Repository, "config"))
	return err == nil
}

// RepoConfig returns the repository settings behind key.
func (h *Set) RepoConfig(key string) config.Repo {
	c := h.cfg.Load()
	if key == Shared {
		return c.Hot
	}
	return c.ClientRepo(key)
}

// Repo returns a restic client for key.
func (h *Set) Repo(key string) *restic.Client {
	return restic.New(h.bin, h.RepoConfig(key)).WithRetryLock(h.lockWait)
}

// Label names a repository in job messages.
func Label(key string) string {
	if key == Shared {
		return "the shared hot repo"
	}
	return key + "'s repo"
}

// Snapshots lists every repository's snapshots, oldest first, and refreshes
// the lookup tables. A repository that cannot be read is logged and skipped;
// it is an error only if none could be read.
func (h *Set) Snapshots(ctx context.Context) ([]restic.Snapshot, error) {
	var all []restic.Snapshot
	bySnap, byHost := map[string]string{}, map[string]string{}
	var firstErr error
	ok := 0
	for _, k := range h.Keys() {
		snaps, err := h.Repo(k).Snapshots(ctx)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			h.log.Warn("cannot list snapshots", "repo", Label(k), "err", err)
			continue
		}
		ok++
		for _, s := range snaps {
			bySnap[s.ID] = k
			byHost[s.Hostname] = k
		}
		all = append(all, snaps...)
	}
	if ok == 0 && firstErr != nil {
		return nil, firstErr
	}
	// A client's own repo wins over a same-named host in the shared repo.
	for n := range h.cfg.Load().Clients {
		byHost[n] = n
	}
	h.mu.Lock()
	h.bySnap, h.byHost = bySnap, byHost
	h.mu.Unlock()
	slices.SortFunc(all, func(a, b restic.Snapshot) int { return a.Time.Compare(b.Time) })
	return all, nil
}

func (h *Set) lookupSnap(id string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for full, k := range h.bySnap {
		if strings.HasPrefix(full, id) {
			return k, true
		}
	}
	return "", false
}

// KeyForSnapshot returns the repository holding snapshot id (short or full).
// IDs change when a snapshot is tagged, so a miss triggers one re-listing.
func (h *Set) KeyForSnapshot(ctx context.Context, id string) (string, error) {
	id, _, _ = strings.Cut(id, ":") // tolerate "id:subfolder"
	if id == "" {
		return "", errors.New("missing snapshot id")
	}
	// With a single repository there is nothing to look up (and restic
	// keywords such as "latest" keep working).
	if keys := h.Keys(); len(keys) == 1 {
		return keys[0], nil
	}
	if k, ok := h.lookupSnap(id); ok {
		return k, nil
	}
	if _, err := h.Snapshots(ctx); err != nil {
		return "", err
	}
	if k, ok := h.lookupSnap(id); ok {
		return k, nil
	}
	return "", fmt.Errorf("snapshot %s not found in any repository", id)
}

// ForSnapshot returns a client for the repository holding snapshot id.
func (h *Set) ForSnapshot(ctx context.Context, id string) (*restic.Client, error) {
	k, err := h.KeyForSnapshot(ctx, id)
	if err != nil {
		return nil, err
	}
	return h.Repo(k), nil
}

// KeyForHost returns the repository that holds host's snapshots: the client's
// own repo if it has one, else wherever its snapshots were last seen, else the
// shared repo.
func (h *Set) KeyForHost(ctx context.Context, host string) string {
	if _, ok := h.cfg.Load().Clients[host]; ok {
		return host
	}
	h.mu.Lock()
	k, ok := h.byHost[host]
	known := h.byHost != nil
	h.mu.Unlock()
	if ok {
		return k
	}
	if !known {
		if _, err := h.Snapshots(ctx); err == nil {
			h.mu.Lock()
			k = h.byHost[host]
			h.mu.Unlock()
			return k
		}
	}
	return Shared
}

// ForHost returns a client for the repository holding host's snapshots.
func (h *Set) ForHost(ctx context.Context, host string) *restic.Client {
	return h.Repo(h.KeyForHost(ctx, host))
}
