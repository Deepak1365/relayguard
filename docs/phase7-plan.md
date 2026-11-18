# Phase 7 plan: dashboard

APPROVED by the user 2026-11-18, all defaults (Q1-Q7 below). Planner: Opus; written to this file by the lead.

Goal: an operator today runs CLI commands per event id to understand why a customer's events failed and to
preview/replay/undo. Phase 7 adds a localhost dashboard showing the same facts and invoking the same audited
code paths.

## Design

**Architecture.** New subcommand `admin serve -addr 127.0.0.1:8090` in cmd/admin/main.go (reuses its
config/db setup, :123-141). HTTP handlers in a new package internal/adminhttp (testable with httptest).
Rejected: a separate cmd/dashboard binary (duplicates startup, second admin binary to explain).

**Routes** (Go 1.26 ServeMux method patterns, JSON):
- `GET /api/events?status=dead|stale|failed|pending|done|skipped|undone&before=<id>&limit=` ("failed" =
  `status='pending' AND last_error IS NOT NULL`, still retrying)
- `GET /api/events/{id}` -> admin.Log
- `GET /api/customers/{stripe_id}/timeline?before=&limit=`
- `GET /api/actions?before=&limit=` (audit log)
- `POST /api/events/{id}/preview` -> admin.Preview
- `POST /api/events/{id}/replay`, `POST /api/events/{id}/undo`, body `{actor, reason}` -> admin.Replay / admin.Undo
- `GET /` static UI

**UI.** Plain HTML/CSS/vanilla JS embedded via embed.FS; no JS build toolchain. Alternative: server-rendered
html/template; rejected because the roadmap asks for a JSON admin API tested on its own, and vanilla JS
keeps the UI a thin client. All untrusted text (payloads, actor, reason, last_error) rendered via
textContent only; every response sets CSP `default-src 'self'`.

**Customer timeline.** inbox has no customer column (migrations/00002_create_inbox.sql). The key is derived
from the payload the same way internal/app/apply.go does (:57-114): data.object.id for customer.created,
data.object.customer for invoices/subscriptions. Sequential scan is acceptable at fixture/chaos scale; no
migration (Q3). Ordered by inbox id (arrival) with the payload `created` shown, so arrival vs event order
is visible. Events whose payload does not decode (poison) are shown as "unattributed", not hidden.

**Reads.** New internal/admin/list.go: ListEvents, CustomerTimeline, ListActions. Each is a single
statement, hence one snapshot. Keyset pagination on id DESC with a `before` cursor; default 50, max 200.
OFFSET rejected (rows shift while the worker runs).

**Finding: admin.Log reads from separate snapshots.** It runs separate pool queries without a transaction
(internal/admin/log.go:51-67), so a dashboard refresh during worker activity could show a status
inconsistent with its change rows. Fix (Q5): one REPEATABLE READ READ ONLY transaction, as 6b did for the
checker. Behaviour change, so it gets a test that fails without the transaction.

**Reuse, not duplication.** Move replayReportable/undoReportable (cmd/admin/main.go:103-111) into
internal/admin as exported functions so the CLI and HTTP share the "audited outcome, safe to report"
decision. Handlers only decode, call admin, and map the error.

## Safety

- Bind: `serve` refuses any non-loopback address (0.0.0.0, `:8090`, hostnames). Unit tested.
- DNS rebinding: Host header allow-list (`127.0.0.1:<port>`, `localhost:<port>`), else 403. Origin checks
  alone do not stop rebinding (the attacker page becomes same-origin).
- CSRF: http.CrossOriginProtection (stdlib, Go 1.25+, Sec-Fetch-Site/Origin) on all POSTs; POST also
  requires `Content-Type: application/json` (else 415), which forces a CORS preflight the server never
  approves; bodies capped at 16 KiB.
- Preview is POST (Q6): it is not audited but takes row locks; a browser prefetch must not trigger it.
- Context: admin calls run on context.WithoutCancel(r.Context()) with a 30s timeout (as the CLI), so a
  closed tab cannot cancel mid-flight and leave the operator unsure. The UI re-reads the event log after any
  error.
- Double click: button disabled while in flight; a second request is safe anyway (replay refused+audited;
  undo protected by admin_actions_undo_once).
- Out of scope (stated in the docs): authentication, multi-user, TLS, remote access. `actor` is
  self-declared, not an identity.

## Error mapping

Body always `{error, message, result?}`; message is the admin error text (already operator-facing,
preview.go:24-36, replay.go:19-22, undo.go:20-24).

| Error | Status | UI shows |
|---|---|---|
| ErrInvalidInput | 400 | message (no audit row) |
| ErrNotFound | 404 | message |
| ErrNotPreviewable, ErrUndone | 409 | message (points to the log view) |
| ErrNotReplayable, ErrNotUndoable | 409 | refusal reason + audit action id (from result) |
| ErrConflict | 409 | every Conflict.Explanation verbatim + kind/table/row/inbox links |
| ErrChanged | 409 | "changed during preview; run it again" |
| ErrBusy | 503 + Retry-After: 1 | "rows are locked; try again" |
| other | 500 | generic text; details logged server-side |

`result` is included only when the shared Reportable function says the outcome was audited, so the UI
never shows "refused, action #0".

## Steps (one commit each, checkpoint B before each commit and push)

### 7a: read API (implementer sonnet)
Files: internal/admin/list.go, Log snapshot fix, exported Reportable functions, CLI switched to them.
Tests: DB tests per status filter; keyset pages with no duplicates or gaps; limit cap; timeline for all
three payload shapes; negative control (another customer's events excluded). Log snapshot: use a
beforeReread-style hook to commit a status change between reads and assert a consistent view; must fail
without the transaction.
Acceptance: tests pass; existing cmd/admin tests green.

### 7b: HTTP API + `admin serve` (implementer sonnet; design fixed above, no new locking; Opus reviewer checks the safety layer)
Files: internal/adminhttp/{server,handlers,errors}.go, internal/adminhttp/*_db_test.go, cmd/admin/main.go.
Tests (httptest against dbtest.Pool + Truncate): replay 200 + audit row exists; replay refused 409 and its
action_id exists in admin_actions; invalid input 400 with Count(admin_actions) unchanged; 404; GET on a POST
route 405; cross-site POST 403 with zero audit rows, plus a same-origin control that succeeds; wrong Host
403 plus control; wrong content type 415; undo conflict 409 with kinds matching and explanations non-empty
and identical to the audit detail's conflicts; ErrBusy 503 by holding a row lock with a short
Options.LockTimeout (repeat 20x, timing-dependent).
Acceptance: all pass; full DB test count reported; never run while an experiment is running.

### 7c: UI (implementer sonnet)
Files: internal/adminhttp/static/{index.html,app.js,style.css}.
Views: failed/dead/stale lists, customer timeline, event detail (log), audit log, preview/replay/undo with
an actor+reason form (actor never pre-filled, Q7).
Tests: GET / returns HTML with the CSP header; embedded files present; a source test that app.js contains
no innerHTML/eval (same pattern as TestPreviewSourceHasNoCommit).
Acceptance: manual click-through on the dev DB (scratchpad notes, not evidence).

### 7d: evidence (experiment-runner sonnet; screenshots per Q2)
Clean tree (vcs.modified=false) build; reset-data; receiver + worker with FAULT_POISON; send testdata/events
fixtures incl. a duplicate invoice and poison.json until dead; `admin serve`. Scripted HTTP transcript
(Invoke-RestMethod; JSON saved verbatim to results/phase7-dashboard-smoke.md): lists, preview of the dead
event, replay refused, undo done then second undo refused, an undo conflict; each cross-checked with
`admin log <id>` and `dev.ps1 state`. Screenshots of the same data in docs/img/, captioned "fixture data
from testdata/events, <date>, <commit>". docs/phase7-results.md, README section,
docs/decisions/0006-dashboard-localhost-security.md. Then interview notes (interview-coach).
Risk: poison.json to dead takes 8 attempts with backoff; the runner measures and reports the real time.

## Risks
- Timeline: payload-derived customer keys miss undecodable (poison) events; shown as "unattributed".
- ErrBusy test is timing-dependent: use a lock held for a known duration and repeat 20 times.
- Poison dead time may make 7d slow; reported as measured.

## Open questions: approved defaults (user, 2026-11-18)
- Q1: `serve` is a subcommand of cmd/admin. Yes.
- Q2: screenshots by the runner with headless Edge (msedge --headless --screenshot), reviewed by the user.
  GIF optional, recorded by the user, not required for acceptance.
- Q3: no expression index for the timeline; a migration only if measured slow.
- Q4: port 8090.
- Q5: fix admin.Log to one snapshot in 7a. Yes.
- Q6: preview is POST. Yes.
- Q7: actor never pre-filled; required every time (as the CLI).

## Likely interview questions
Why check Host if you already check Origin (DNS rebinding)? Why keyset not OFFSET? How do you avoid showing
an audit id that was never committed? What happens if the browser disconnects mid-undo? Why is preview POST
if it changes nothing? Why no auth, and what changes before deploying beyond localhost? Why no SPA framework?
