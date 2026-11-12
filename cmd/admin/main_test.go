package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	good := []struct {
		args []string
		want command
	}{
		{[]string{"preview", "12"}, command{"preview", 12}},
		{[]string{"log", "1"}, command{"log", 1}},
	}
	for _, c := range good {
		got, err := parseArgs(c.args)
		if err != nil || got != c.want {
			t.Errorf("parseArgs(%q) = %+v, %v; want %+v", c.args, got, err, c.want)
		}
	}
	bad := [][]string{
		nil,
		{"preview"},
		{"preview", "12", "extra"},
		{"replay", "12"}, // not in step 4
		{"preview", "abc"},
		{"preview", "0"},
		{"preview", "-3"},
		{"log", "1.5"},
	}
	for _, args := range bad {
		if got, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs(%q) = %+v, want an error", args, got)
		}
	}
}

// Usage errors exit 2 and print the usage, before touching .env or the DB.
func TestRunUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"nope"}, &out, &errOut); code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if out.Len() != 0 || !strings.Contains(errOut.String(), "usage:") {
		t.Fatalf("stdout %q, stderr %q: want usage on stderr only", out.String(), errOut.String())
	}
}
