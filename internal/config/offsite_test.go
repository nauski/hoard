package config

import (
	"encoding/json"
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
		{"hot", nil, "hot-2"}, // "hot" is the hot repo's key in /api/stats
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

func TestEnvOnlySeedsFirstStorage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hoard.json")
	os.WriteFile(p, []byte(`{"hot":{"repository":"/data/hot","password":"h"}}`), 0o600)
	t.Setenv("HOARD_COLD_REPOSITORY", "sftp://u@h/p")
	t.Setenv("HOARD_COLD_PASSWORD", "pw")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Offsite) != 1 || c.Offsite[0].ID != "offsite" || c.Offsite[0].Label != "Offsite" ||
		!c.Offsite[0].Default || c.Offsite[0].Repository != "sftp://u@h/p" || c.Offsite[0].Password != "pw" {
		t.Fatalf("seeded = %+v", c.Offsite)
	}
}

func TestSavedConfigHasNoLegacyKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hoard.json")
	os.WriteFile(p, []byte(`{"hot":{"repository":"/data/hot","password":"h"},
	  "cold":{"repository":"s3:https://x/b","password":"c"},"mirror":{"exclude_hosts":["lab"]}}`), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewStore(c, p).Update(func(*Config) {}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"cold", "mirror"} {
		if _, ok := m[k]; ok {
			t.Errorf("legacy key %q written: %s", k, raw)
		}
	}
}

func TestEmptyOffsiteListSurvivesWithEnv(t *testing.T) {
	p := writeCfg(t, `{"hot":{"repository":"/data/hot","password":"h"}}`)
	t.Setenv("HOARD_COLD_REPOSITORY", "sftp://u@h/p")
	c, err := Load(p)
	if err != nil || len(c.Offsite) != 1 {
		t.Fatalf("seed: %v %+v", err, c.Offsite)
	}
	st := NewStore(c, p)
	if err := st.Update(func(c *Config) { c.Offsite = []Offsite{} }); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c2.Offsite) != 0 {
		t.Fatalf("removed storage came back from env: %+v", c2.Offsite)
	}
}

func TestEnvSecretsNotOnUnrelatedStorage(t *testing.T) {
	p := writeCfg(t, `{"hot":{"repository":"/data/hot","password":"h"},
	  "offsite":[{"id":"box","label":"Box","repository":"/x"},{"id":"e2","label":"e2","repository":"s3:y"}]}`)
	t.Setenv("HOARD_COLD_PASSWORD", "envpw")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Offsite[0].Password != "" || c.Offsite[1].Password != "envpw" {
		t.Errorf("secrets: box=%q e2=%q", c.Offsite[0].Password, c.Offsite[1].Password)
	}
}

func TestEnvIDriveSeedsE2(t *testing.T) {
	p := writeCfg(t, `{"hot":{"repository":"/data/hot","password":"h"}}`)
	t.Setenv("HOARD_COLD_REPOSITORY", "s3:https://s3.eu-central-2.idrivee2.com/b")
	t.Setenv("HOARD_COLD_PASSWORD", "pw")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Offsite) != 1 || c.Offsite[0].ID != "e2" || c.Offsite[0].Label != "IDrive e2" || c.Offsite[0].Password != "pw" {
		t.Fatalf("seeded = %+v", c.Offsite)
	}
}

func TestMigrateKeepsColdFieldsWhenRepoFromEnv(t *testing.T) {
	p := writeCfg(t, `{"hot":{"repository":"/data/hot","password":"h"},"cold":{"password":"c","limit_upload_kibps":500}}`)
	t.Setenv("HOARD_COLD_REPOSITORY", "s3:https://x/b")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Offsite) != 1 || c.Offsite[0].Repository != "s3:https://x/b" || c.Offsite[0].Password != "c" || c.Offsite[0].LimitUploadKiBps != 500 {
		t.Fatalf("migrated = %+v", c.Offsite)
	}
}
