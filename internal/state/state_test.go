package state

import (
	"testing"
	"time"
)

func TestSetVerifyRoundTrips(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if v := s.Snapshot().LastVerify; v != nil {
		t.Fatalf("expected nil LastVerify initially, got %+v", v)
	}
	s.SetVerify(VerifyResult{Time: time.Now(), OK: true, Client: "host", File: "/a.txt", Bytes: 11})
	v := s.Snapshot().LastVerify
	if v == nil || !v.OK || v.Client != "host" {
		t.Fatalf("unexpected LastVerify: %+v", v)
	}
}
