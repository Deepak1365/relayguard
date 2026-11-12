// admin is the operator CLI for RelayGuard's inbox and change log.
//
//	admin preview <inbox-id>   what would applying this event change now? (writes nothing)
//	admin log <inbox-id>       what did this event change, and what did admins do to it?
//	admin replay <inbox-id> -actor <name> -reason "<text>"
//	                           put a dead or stale event back in the queue (audited)
//
// Exit codes: 0 when the command completed (for preview: any outcome,
// including "the apply would fail"; for replay: the replay was done), 1 when
// it was refused or failed (done event, not replayable, busy rows, changed
// meanwhile, not found, database error), 2 for usage errors (including a
// missing or too long -actor / -reason).
package main

import (
	"context"
	"errors"
	"flag"
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
  admin log <inbox-id>       show the event's change_log rows and admin actions
  admin replay <inbox-id> -actor <name> -reason "<text>"
                             replay a dead or stale event (both flags required; audited)`

// command is a parsed command line.
type command struct {
	name    string // preview, log or replay
	inboxID int64
	actor   string // replay only, trimmed
	reason  string // replay only, trimmed
}

// parseArgs validates the command line (os.Args[1:]). Pure, unit tested.
// For replay it also applies admin.NormalizeReplayRequest, so a missing or
// too long actor/reason is a usage error before settings or the database
// are touched.
func parseArgs(args []string) (command, error) {
	if len(args) < 2 {
		return command{}, errors.New("expected a command and an inbox id")
	}
	name := args[0]
	if name != "preview" && name != "log" && name != "replay" {
		return command{}, fmt.Errorf("unknown command %q", name)
	}
	id, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || id <= 0 {
		return command{}, fmt.Errorf("inbox id must be a positive integer, got %q", args[1])
	}
	cmd := command{name: name, inboxID: id}
	if name != "replay" {
		if len(args) != 2 {
			return command{}, fmt.Errorf("%s takes only an inbox id", name)
		}
		return cmd, nil
	}

	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // errors are returned and printed with the usage
	actor := fs.String("actor", "", "who is replaying (required)")
	reason := fs.String("reason", "", "why (required)")
	if err := fs.Parse(args[2:]); err != nil {
		return command{}, fmt.Errorf("replay: %w", err)
	}
	if fs.NArg() != 0 {
		return command{}, fmt.Errorf("replay: unexpected arguments %q (quote a reason that has spaces)", fs.Args())
	}
	req, err := admin.NormalizeReplayRequest(admin.ReplayRequest{InboxID: id, Actor: *actor, Reason: *reason})
	if err != nil {
		return command{}, err
	}
	cmd.actor, cmd.reason = req.Actor, req.Reason
	return cmd, nil
}

// replayReportable reports whether a Replay result may be printed as a
// report: only when it succeeded or the refusal was audited and committed.
// After any other error no audit row is guaranteed, and "refused, audit
// action #0" would be false.
func replayReportable(err error) bool {
	return err == nil || errors.Is(err, admin.ErrNotReplayable)
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
	case "replay":
		res, err := admin.Replay(ctx, pool,
			admin.ReplayRequest{InboxID: cmd.inboxID, Actor: cmd.actor, Reason: cmd.reason}, admin.Options{})
		if replayReportable(err) {
			// Done or refused-and-audited: an audit row exists; show it.
			fmt.Fprint(stdout, admin.FormatReplay(res))
		}
		if err != nil {
			fmt.Fprintf(stderr, "admin: replay refused or failed: %v\n", err)
			return 1
		}
	}
	return 0
}
