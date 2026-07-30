package api

import (
	"strings"
	"testing"
	"time"

	"github.com/nauski/hoard/internal/config"
)

func testKitConfig() *config.Config {
	return &config.Config{
		Hot: config.Repo{Repository: "rest:http://nas:8000/hot", Password: "hotpw"},
		Cold: config.Repo{
			Repository:        "s3:https://s3.example.com/mybucket",
			Password:          "coldpw",
			S3AccessKeyID:     "AKIAEXAMPLE",
			S3SecretAccessKey: "supersecret",
		},
	}
}

func TestBuildRecoveryKitContainsColdAndHot(t *testing.T) {
	out := buildRecoveryKit(testKitConfig(), "0.18.0", time.Unix(0, 0).UTC())
	for _, want := range []string{
		"s3:https://s3.example.com/mybucket", "coldpw", "AKIAEXAMPLE", "supersecret",
		"restic restore latest", "rest:http://nas:8000/hot", "hotpw", "0.18.0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("kit missing %q\n---\n%s", want, out)
		}
	}
}

func TestBuildRecoveryKitUnconfiguredCold(t *testing.T) {
	c := testKitConfig()
	c.Cold = config.Repo{} // nothing configured
	out := buildRecoveryKit(c, "0.18.0", time.Unix(0, 0).UTC())
	if strings.Contains(out, "supersecret") {
		t.Fatal("kit leaked a secret for an unconfigured repo")
	}
	if !strings.Contains(out, "not configured") {
		t.Fatalf("expected 'not configured' marker for empty cold repo\n---\n%s", out)
	}
}
