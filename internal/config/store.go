package config

import (
	"encoding/json"
	"os"
	"sync/atomic"
)

// Store holds the live server config behind an atomic pointer so the scheduler
// (reader) and the API (writer) never race. Updates swap in a new config and
// persist it back to the config file — with secrets blanked, since those come
// from the environment.
type Store struct {
	p    atomic.Pointer[Config]
	path string
}

// NewStore wraps an initial config; path is the file updates are saved to
// (empty disables persistence).
func NewStore(c *Config, path string) *Store {
	s := &Store{path: path}
	s.p.Store(c)
	return s
}

// Load returns the current config (do not mutate the returned value).
func (s *Store) Load() *Config { return s.p.Load() }

// Update applies fn to a copy of the current config, swaps it in atomically,
// and persists it.
func (s *Store) Update(fn func(*Config)) error {
	cur := s.Load()
	nc := *cur
	fn(&nc)
	s.p.Store(&nc)
	return s.save(&nc)
}

// save writes the config to disk with secrets removed (they live in env).
func (s *Store) save(c *Config) error {
	if s.path == "" {
		return nil
	}
	out := *c
	out.Hot.Password = ""
	out.Cold.Password = ""
	out.Cold.S3AccessKeyID = ""
	out.Cold.S3SecretAccessKey = ""
	raw, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
