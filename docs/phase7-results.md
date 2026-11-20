# Phase 7 results: admin dashboard (step 7d evidence)

Date: 2026-11-19. Commit: 0b42f07aa514e740e56a4f7e9b8d3823dd4c3880. Evidence run by the experiment-runner.
Full transcript: [results/phase7-dashboard-smoke.md](../results/phase7-dashboard-smoke.md). Screenshots: [docs/img/](img/) (embedded in that file).
Data is synthetic (testdata/events fixtures, event ids `evt_fx_*`).

## What was run

1. Clean tree (vcs.modified=false on all three binaries). `dev.ps1 build` for bin/admin.exe and bin/worker.exe, and a receiver built to the scratchpad. All three
   report `vcs.revision=0b42f07aa514e740e56a4f7e9b8d3823dd4c3880` and `vcs.modified=false` (verbatim lines in the smoke file).
2. `dev.ps1 reset-data`; receiver started in the background (startup line "RelayGuard receiver listening on 127.0.0.1:8080").
3. `dev.ps1 send` for customer_created, invoice_paid (sent twice), subscription_active, subscription_canceled, poison. The inbox showed ids 1, 2, 4, 5, 6:
   the duplicate invoice_paid was not stored twice (id 3 is missing, consistent with the receiver's `ON CONFLICT DO NOTHING` using up a
   sequence value, `cmd/receiver/main.go:92`).
4. `bin/worker.exe` with env FAULT_POISON=1 and WORKER_LEASE=1s, restarted by a loop until the poison event was dead; then receiver and worker were stopped by PID.
5. `bin/admin.exe serve`; HTTP transcript with curl.exe; each action cross-checked with `bin/admin.exe log <id>` and `dev.ps1 state`.
6. Seven screenshots with headless Edge over the DevTools protocol; admin serve, Edge and the helper processes stopped by PID; `dev.ps1 reset-data`.

## Measured poison-to-dead time

From the loop's log (verbatim in the smoke file): 9 worker starts, and the dead status was observed after 11.7971117 s wall time on the loop's stopwatch
(first worker started at 1.286156 s on that stopwatch; 200 ms polling; WORKER_LEASE=1s). The worker's own log shows POISON attempt 8 at 10:59:48
and "DEAD inbox row 6 after 8 attempts" at 10:59:49. This is one run, not a distribution.
The restart gaps (about 1.25 s) are set by WORKER_LEASE=1s. The worker default is 10 s (`cmd/worker/main.go:45`), so with the default lease
the time would be much longer; that was not measured.

## What the transcript shows (each point is a response in the smoke file)

- After the worker: inbox rows 1, 2, 4, 5 done with attempts 1; row 6 dead with attempts 8. State: cus_fx_1 credit_balance 20, 1 grant, ledger_total 20; sub_fx_1 canceled by evt_fx_sub_canceled.
  This matches the fixtures README.
- Lists: dead returns only #6; all events returns five rows; the cus_fx_1 timeline returns #5, #4, #2, #1; unattributed returns #6; actions was empty.
- Preview of dead #6: HTTP 200, outcome "skip", reason "unhandled event type", no changes; `admin log 6` shows 0 change_log and 0 admin_actions rows.
- Replay of done #2: HTTP 409 `not_replayable`, audited as action #1 (refused); state unchanged.
- Undo of #5: HTTP 200 outcome done, action #2; subscription went back to active / evt_fx_sub_active; `admin log 5` shows the undo change_log entry; inbox status undone.
- Second undo of #5: HTTP 409 `not_undoable` ("already undone (admin action #2)"), audited as action #3.
- Undo of #1 (customer_created): HTTP 409 `conflict` with 3 conflicts (one later_event, two referenced), audited as action #4; state unchanged.
- Every response matched the expectations in docs/phase7-plan.md step 7d; no result differed from them.

## Screenshots

Seven PNGs in docs/img/ (`dashboard-*.png`), each captioned in the smoke file as "fixture data from testdata/events, 2026-11-19, 0b42f07".
Each was opened and checked to show the intended page (not blank, not "Loading"). Audit action #5 (refused undo of #1, actor `ui-runner`) was written by the
screenshot run, not by the curl transcript. The dead-event Preview click wrote nothing.
The browser console logged two failed resource loads: a 404 whose URL was not recorded, and the expected 409 from the undo conflict.
index.html declares no icon, so a default /favicon.ico request (answered by the JSON no_route 404) is a plausible source of the 404. Not verified.

## Limitations

One run, one fixture set (5 inbox rows), one operator. The worker was stopped before `admin serve`, so no read or action ran during worker
activity. This is not a load, concurrency or security test; those properties are covered by the 7a/7b DB tests.

## Deviations from the plan

- curl.exe instead of Invoke-RestMethod (PowerShell 5.1 throws on 4xx).
- The worker restart loop polls the DB through `docker exec ... psql` (scratchpad script); wall time includes that polling overhead (200 ms sleep plus the query).
- Screenshots were all taken through the DevTools protocol (not `msedge --screenshot`), so the preview click and conflict notice could use the same method.
- `dev.ps1 reset-data` was run once more before writing these docs (after the cleanup), not only at the end.

## Cleanup

Receiver, worker processes, `bin/admin.exe serve` and the Edge processes (found by user-data-dir path via Get-CimInstance, stopped by PID) were stopped; no admin, receiver or worker process remained. Database reset with `dev.ps1 reset-data`.
