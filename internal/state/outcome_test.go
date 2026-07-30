package state

import "testing"

func TestOutcomeRoundTripAndIsolation(t *testing.T) {
	s, _ := Load("")
	if o := s.OutcomeFor("h"); o.OK || o.ConsecutiveFailures != 0 {
		t.Fatalf("expected zero outcome, got %+v", o)
	}
	s.SetOutcome("h", Outcome{OK: false, Message: "boom", ConsecutiveFailures: 2})
	if o := s.OutcomeFor("h"); o.OK || o.ConsecutiveFailures != 2 || o.Message != "boom" {
		t.Fatalf("unexpected outcome: %+v", o)
	}
	// Snapshot is a deep copy.
	if v := s.Snapshot().ClientOutcomes["h"]; v.ConsecutiveFailures != 2 {
		t.Fatalf("snapshot missing outcome: %+v", v)
	}
	// SetClients (what RefreshClients calls) must NOT clear outcomes.
	s.SetClients(map[string]Client{"h": {Hostname: "h"}})
	if o := s.OutcomeFor("h"); o.ConsecutiveFailures != 2 {
		t.Fatalf("SetClients clobbered the outcome: %+v", o)
	}
}
