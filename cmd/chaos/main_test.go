package main

import "testing"

// The results file must say whether the change_log check ran: "0
// violations" means something different with the check off.
func TestChangeLogCheckLine(t *testing.T) {
	cases := []struct {
		target  string
		enabled bool
		want    string
	}{
		{"relayguard", true, "- Change log check: on (changelog_* violation kinds)"},
		{"relayguard", false, "- Change log check: off (flag -changelog-check=false)"},
		{"naive", true, "- Change log check: n/a (the naive receiver writes no change_log)"},
		{"naive", false, "- Change log check: n/a (the naive receiver writes no change_log)"},
	}
	for _, c := range cases {
		if got := changeLogCheckLine(c.target, c.enabled); got != c.want {
			t.Errorf("changeLogCheckLine(%q, %v) = %q, want %q", c.target, c.enabled, got, c.want)
		}
	}
}
