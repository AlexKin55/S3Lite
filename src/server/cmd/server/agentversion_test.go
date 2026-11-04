package main

import "testing"

func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.0.4", "0.0.4", false},
		{"0.0.4", "0.0.5", true},
		{"0.0.10", "0.0.9", false}, // numeric, not lexicographic
		{"0.1.0", "0.0.9", false},
		{"1.0.0", "0.9.9", false},
		{"0.0.3", "0.0.10", true},
		{"", "0.0.1", true},
		{"0.0.1", "", false},
		{"0.0.4-rc1", "0.0.5", true},  // suffix ignored
		{"0.0", "0.0.1", true},        // missing segment counts as 0
		{"0.0.4", "0.0.4-rc1", false}, // suffix ignored on both sides
	}
	for _, c := range cases {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Errorf("versionLess(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestObserveAgentVersion(t *testing.T) {
	s := &server{}
	s.observeAgentVersion("0.0.4")
	if got := s.newestAgentVersion(); got != "0.0.4" {
		t.Fatalf("newestAgentVersion() = %q, want 0.0.4", got)
	}
	s.observeAgentVersion("0.0.3") // older — must not move the marker
	if got := s.newestAgentVersion(); got != "0.0.4" {
		t.Fatalf("after older report: newestAgentVersion() = %q, want 0.0.4", got)
	}
	s.observeAgentVersion("0.1.0") // newer — must replace it
	if got := s.newestAgentVersion(); got != "0.1.0" {
		t.Fatalf("after newer report: newestAgentVersion() = %q, want 0.1.0", got)
	}
	s.observeAgentVersion("") // empty — ignored
	if got := s.newestAgentVersion(); got != "0.1.0" {
		t.Fatalf("after empty report: newestAgentVersion() = %q, want 0.1.0", got)
	}
}
