package config

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Offsite is one labelled offsite storage that hot snapshots are mirrored to.
type Offsite struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Default bool   `json:"default"` // receives hosts that have no routing entry
	Repo
}

var nonID = regexp.MustCompile(`[^a-z0-9]+`)

// NewOffsiteID derives a storage id from its label: lowercase [a-z0-9-], with
// a numeric suffix when taken. An empty result becomes "storage".
func NewOffsiteID(label string, taken []string) string {
	base := strings.Trim(nonID.ReplaceAllString(strings.ToLower(label), "-"), "-")
	if base == "" {
		base = "storage"
	}
	id := base
	for n := 2; slices.Contains(taken, id); n++ {
		id = base + "-" + strconv.Itoa(n)
	}
	return id
}

// OffsiteByID returns the storage with the given id.
func (c *Config) OffsiteByID(id string) (Offsite, bool) {
	for _, o := range c.Offsite {
		if o.ID == id {
			return o, true
		}
	}
	return Offsite{}, false
}

// OffsiteIDs lists the storage ids in config order.
func (c *Config) OffsiteIDs() []string {
	ids := make([]string, 0, len(c.Offsite))
	for _, o := range c.Offsite {
		ids = append(ids, o.ID)
	}
	return ids
}

// OffsiteFor returns the storage ids host is mirrored to, in c.Offsite order:
// routing[host] if the key exists (even when empty), else every Default
// storage; plus every id in wish. Unknown ids are dropped.
func (c *Config) OffsiteFor(host string, wish []string) []string {
	routed, has := c.Routing[host]
	var out []string
	for _, o := range c.Offsite {
		if slices.Contains(wish, o.ID) || (has && slices.Contains(routed, o.ID)) || (!has && o.Default) {
			out = append(out, o.ID)
		}
	}
	return out
}

// ServerOffsiteFor is OffsiteFor(host, nil).
func (c *Config) ServerOffsiteFor(host string) []string { return c.OffsiteFor(host, nil) }

// migrateOffsite turns the legacy single cold repo into the first storage.
func (c *Config) migrateOffsite() {
	if len(c.Offsite) > 0 || c.Cold.Repository == "" {
		c.Cold, c.Mirror.ExcludeHosts = Repo{}, nil // never written back
		return
	}
	c.Offsite = []Offsite{{ID: "e2", Label: "IDrive e2", Default: true, Repo: c.Cold}}
	c.Cold = Repo{}
	excluded := c.Mirror.ExcludeHosts
	c.Mirror.ExcludeHosts = nil
	for _, h := range excluded {
		if _, ok := c.Routing[h]; ok {
			continue
		}
		if c.Routing == nil {
			c.Routing = map[string][]string{}
		}
		c.Routing[h] = []string{}
	}
}
