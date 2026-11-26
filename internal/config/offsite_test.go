package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNewOffsiteID(t *testing.T) {
	cases := []struct {
		label string
		taken []string
		want  string
	}{
		{"Hetzner box", nil, "hetzner-box"},
		{"IDrive e2", []string{"idrive-e2"}, "idrive-e2-2"},
		{"IDrive e2", []string{"idrive-e2", "idrive-e2-2"}, "idrive-e2-3"},
		{"  ÄÖ!! ", nil, "storage"},
		{"B2 (EU)", nil, "b2-eu"},
	}
	for _, c := range cases {
		if got := NewOffsiteID(c.label, c.taken); got != c.want {
			t.Errorf("NewOffsiteID(%q, %v) = %q, want %q", c.label, c.taken, got, c.want)
		}
	}
}

func TestOffsiteFor(t *testing.T) {
	c := &Config{
		Offsite: []Offsite{{ID: "e2", Label: "e2", Default: true}, {ID: "box", Label: "box"}, {ID: "b2", Label: "b2", Default: true}},
		Routing: map[string][]string{"nas": {"box"}, "lab": {}},
	}
	cases := []struct {
		host string
		wish []string
		want []string
	}{
		{"laptop", nil, []string{"e2", "b2"}},                  // no routing entry: defaults
		{"nas", nil, []string{"box"}},                          // explicit
		{"lab", nil, []string{}},                               // explicit empty: no offsite copy
		{"lab", []string{"b2"}, []string{"b2"}},                // client adds one
		{"nas", []string{"e2", "gone"}, []string{"e2", "box"}}, // union, config order, unknown dropped
	}
	for _, tc := range cases {
		got := c.OffsiteFor(tc.host, tc.wish)
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("OffsiteFor(%q, %v) = %v, want %v", tc.host, tc.wish, got, tc.want)
		}
	}
}

func TestMigrateColdToOffsite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hoard.json")
	os.WriteFile(p, []byte(`{"hot":{"repository":"/data/hot","password":"h"},
	  "cold":{"repository":"s3:https://x/b","password":"c","s3_access_key_id":"k","s3_secret_access_key":"s","limit_upload_kibps":500},
	  "mirror":{"exclude_hosts":["lab"]}}`), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Offsite) != 1 {
		t.Fatalf("offsite = %+v", c.Offsite)
	}
	o := c.Offsite[0]
	if o.ID != "e2" || o.Label != "IDrive e2" || !o.Default || o.Repository != "s3:https://x/b" ||
		o.Password != "c" || o.S3AccessKeyID != "k" || o.S3SecretAccessKey != "s" || o.LimitUploadKiBps != 500 {
		t.Errorf("migrated storage = %+v", o)
	}
	if r, ok := c.Routing["lab"]; !ok || len(r) != 0 {
		t.Errorf("routing[lab] = %v, %v; want empty list present", r, ok)
	}
	if got := c.OffsiteFor("lab", nil); len(got) != 0 {
		t.Errorf("lab should have no offsite copy, got %v", got)
	}
}

func TestMigrateSeedsFromEnv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hoard.json")
	os.WriteFile(p, []byte(`{"hot":{"repository":"/data/hot","password":"h"},"cold":{"repository":"s3:https://x/b"}}`), 0o600)
	t.Setenv("HOARD_COLD_PASSWORD", "envpw")
	t.Setenv("HOARD_COLD_S3_ACCESS_KEY_ID", "envkey")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Offsite[0].Password != "envpw" || c.Offsite[0].S3AccessKeyID != "envkey" {
		t.Errorf("env not seeded into first storage: %+v", c.Offsite[0])
	}
}

func TestNoOffsiteIsValid(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hoard.json")
	os.WriteFile(p, []byte(`{"hot":{"repository":"/data/hot","password":"h"}}`), 0o600)
	if _, err := Load(p); err != nil {
		t.Fatalf("a server without offsite storage must load: %v", err)
	}
}

func TestDuplicateStorageRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hoard.json")
	os.WriteFile(p, []byte(`{"hot":{"repository":"/data/hot","password":"h"},
	  "offsite":[{"id":"a","label":"A","repository":"/x"},{"id":"a","label":"B","repository":"/y"}]}`), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("duplicate storage id must be rejected")
	}
}
