# 0003: Undo uses conflict rule B plus a chain check, not strict row-version equality or compensating events

Status: Accepted (Phase 6, step 6, commits 14ae77f (6a) and b4c9405 (6b checker))
Date: 2026-11-12 (step 6 plan approved by the user, docs/phase6-plan.md:306-308; commits dated 2026-11-13)

## Context
Undo must not silently destroy data that changed after the event being undone. The user's first answer was
"strict row-version undo: refuse if the row changed since" (plan :3). Row versions come from a BEFORE UPDATE
trigger (0001) that sets `row_version = OLD.row_version + 1` on every UPDATE.

## Options considered
**Rule A, literal strict row-version.** Undo only if the row's current row_version equals the version the event
left. Pro: simple, and cheap to explain. Con, found by the planner: it makes newest-first undo impossible for
any row touched twice. Example from the plan: customer.created leaves v1, invoice.paid v2, undoing the invoice
is an UPDATE so the trigger makes v3, and undoing customer.created then expects v1 and sees v3, refused forever
(plan :310-313). The undo-all experiment (E4) would fail by design.

**Rule B (chosen).** An event can be undone only if every later change to its rows is accounted for in
change_log and is itself undone (plan :314-315). Equally strict about unknown writes, but undo's own trigger
bump is explained by its change_log row.

**Compensating events or compensating rows.** The plan weighed this only for the ledger: Q3, "delete the
credit_grants row vs a compensating negative grant", answered "delete" (plan :307, :361). A general
"undo as a new forward event" design was not written up as an option in the plan; what follows is my reasoning.
Pro: history is append-only and nothing is deleted. Con: the app's rules (customers, ledger,
subscription `last_event_created` ordering) would need new event semantics, and a compensating subscription
change would collide with the ordering rule. Deleting and restoring from before images fits the existing log.

## Decision
Rule B, enforced per affected row, with a refusal audited and explained (internal/admin/undo.go:33-42,
:148-151). The five rules are: row missing; a later apply entry whose event is not undone (and "undone" must be
paired with an undo row on that row, else "unpaired"); current row_version equals the version of the last log
entry; content equals that entry's image (defense in depth); a customer to be deleted has no referencing rows
(plan :323-328 and refinements :370-378).

**The chain check and the bug it closes.** Review round 1 found a blocking hole (plan :399-405). Rules 3 and 4
only compared the row with the LAST logged change. Sequence: customer.created; invoice I1; an outside UPDATE of
the email; invoice I2; undo I2 (restores the image that contains the outside email); undo I1 then passed and
wiped the outside edit. Every later before/after image carries the outside write forward, so a comparison at
the end cannot see it. Fix: every link from the event's own first entry onward must satisfy
`before_image == previous after_image`, including row_version, otherwise the conflict kind is `chain_break`
(internal/admin/undo.go:40, :459-469). Test: TestDBUndoConflictUnloggedWrite (the content subtest failed
before the fix, plan :403-405), with a no-op UPDATE variant that only bumps row_version.

Lock order: inbox row FOR NO KEY UPDATE first, then app rows in reverse seq. This is deadlock-free only because
each apply writes at most one pre-existing row and writes it last; This is an argument, not a test result: TestDBApplyWritesExistingRowLast pins the assumption it rests on; no concurrent undo-vs-worker deadlock test exists
(internal/admin/undo.go:153-169; plan :408-411).

## Consequences
- The harness checks undo rows too (checkUndoRows, commit b4c9405), and a property run shows rule B allows
  full unwinding on these synthetic seeds (generation 0 only: E4 never replays events, docs/phase6-results.md:347-348). E4a (undo newest first, seeds 1-20): `RESULT target=relayguard seeds=20 failing_seeds=0
  violations=0 restarts=277 hard_kills=40 undone=1534 undo_refusals=0 undo_skipped=0`
  (docs/phase6-results.md:177). E4b (random order with retries): `... undone=1530 undo_refusals=814 ...`
  (:229); E4c (poison, newest first, seeds 1-5): `... undone=383 undo_refusals=0 ...` (:254). In E4b every
  refusal is a legitimate "undo the later event first" or "referenced" answer, and every done event was still undone
  eventually (per-seed undone = done, docs/phase6-results.md:189-228); refusal counts vary run to run
  (:232-235, :334-337).
- Under rule A, E4a's 0 refusals would not have been achievable by construction; I take that from the plan's
  example, not from a run of rule A (no such run exists).
- Limitations: after a chain break the older event can never be undone (plan :433-434, by design).
  credit_grants.customer_id and subscriptions.customer_id have no index, so rule 5 scans both tables while
  the customer row is locked (plan :421-422). Image comparison relies on every session being UTC
  (plan :435-436). Undo was not crash-injected and not run concurrently with live processing
  (docs/phase6-results.md:345-346). The check that deleting a customer is safe only covers the references the
  fixed `referencedBy` map lists.
- Mutation gap: removing the `break` in the delete-entry loop is not caught, because the app never deletes
  (plan :420).
- Revisit: a retention policy for change_log (undo needs the history), an index for rule 5, and compensating
  events if the app ever gains side effects outside the database that deletion cannot reverse.
