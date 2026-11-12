// admin is the operator CLI for RelayGuard's inbox and change log.
//
//	admin preview <inbox-id>   what would applying this event change now? (writes nothing)
//	admin log <inbox-id>       what did this event change, and what did admins do to it?
//
// Exit codes: 0 when the command completed (for preview: any outcome,
// including "the apply would fail"), 1 when it was refused or failed (done
// event, busy rows, changed meanwhile, not found, database error), 2 for
// usage errors.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/Deepak1365/relayguard/internal/admin"
	"github.com/Deepak1365/relayguard/internal/config"
	"github.com/Deepak1365/relayguard/internal/db"
)

const usage = `usage:
  admin preview <inbox-id>   show what applying the event now would change (rolled back, writes nothing)
  admin log <inbox-id>       show the event's change_log rows and admin actions`

// command is a parsed command line.
type command struct {
	name    string // preview or log
	inboxID int64
}

// parseArgs validates the command line (os.Args[1:]). Pure, unit tested.
func parseArgs(args []string) (command, error) {
	if len(args) != 2 {
		return command{}, errors.New("expected a command and an inbox id")
	}
	name := args[0]
	if name != "preview" && name != "log" {
		return command{}, fmt.Errorf("unknown command %q", name)
	}
	id, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || id <= 0 {
		return command{}, fmt.Errorf("inbox id must be a positive integer, got %q", args[1])
	}
	return command{name: name, inboxID: id}, nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	cmd, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "admin: %v\n%s\n", err, usage)
		return 2
	}
	// Same as the worker: settings from .env, real environment wins. Values
	// are never printed.
	if err := config.LoadDotEnv(".env"); err != nil {
		fmt.Fprintf(stderr, "admin: loading .env: %v\n", err)
		return 1
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		fmt.Fprintln(stderr, "admin: DATABASE_URL is not set")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		fmt.Fprintf(stderr, "admin: database: %v\n", err)
		return 1
	}
	defer pool.Close()

	switch cmd.name {
	case "preview":
		res, err := admin.Preview(ctx, pool, cmd.inboxID, admin.Options{})
		if err != nil {
			fmt.Fprintf(stderr, "admin: preview refused or failed: %v\n", err)
			return 1
		}
		fmt.Fprint(stdout, admin.FormatPreview(res))
	case "log":
		res, err := admin.Log(ctx, pool, cmd.inboxID)
		if err != nil {
			fmt.Fprintf(stderr, "admin: log failed: %v\n", err)
			return 1
		}
		fmt.Fprint(stdout, admin.FormatLog(res))
	}
	return 0
}
