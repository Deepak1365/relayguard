package main

import "testing"

func TestParseServeArgs(t *testing.T) {
	ok := map[string][]string{
		"127.0.0.1:8090": nil,
		"127.0.0.1:9000": {"-addr", "127.0.0.1:9000"},
	}
	for want, args := range ok {
		if got, err := parseServeArgs(args); err != nil || got != want {
			t.Errorf("parseServeArgs(%v) = %q, %v; want %q", args, got, err, want)
		}
	}
	bad := [][]string{
		{"-addr", "0.0.0.0:8090"},
		{"-addr", ":8090"},
		{"-addr", "localhost:8090"},
		{"-addr", "127.0.0.1"},
		{"extra"},
		{"-nope"},
	}
	for _, args := range bad {
		if got, err := parseServeArgs(args); err == nil {
			t.Errorf("parseServeArgs(%v) = %q, nil; want an error", args, got)
		}
	}
}
