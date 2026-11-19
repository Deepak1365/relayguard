package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/Deepak1365/relayguard/internal/admin"
	"github.com/Deepak1365/relayguard/internal/adminhttp"
)

// parseServeArgs reads serve's flags and validates the address (loopback
// only) before settings or the database are touched. Pure, unit tested.
func parseServeArgs(args []string) (addr string, err error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	a := fs.String("addr", "127.0.0.1:8090", "listen address (127.0.0.1 only)")
	if err := fs.Parse(args); err != nil {
		return "", fmt.Errorf("serve: %w", err)
	}
	if fs.NArg() != 0 {
		return "", fmt.Errorf("serve: unexpected arguments %q", fs.Args())
	}
	if _, _, err := adminhttp.ParseAddr(*a); err != nil {
		return "", fmt.Errorf("serve: %w", err)
	}
	return *a, nil
}

// runServe starts the dashboard API and blocks until interrupted.
func runServe(args []string, stderr io.Writer) int {
	addr, err := parseServeArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "admin: %v\n%s\n", err, usage)
		return 2
	}
	pool, code := openPool(stderr)
	if pool == nil {
		return code
	}
	defer pool.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	announce := func(bound string) {
		fmt.Fprintf(stderr, "admin: dashboard API listening on http://%s (no authentication; loopback only)\n", bound)
	}
	if err := adminhttp.Serve(ctx, pool, addr, admin.Options{}, announce); err != nil {
		fmt.Fprintf(stderr, "admin: %v\n", err)
		return 1
	}
	return 0
}
