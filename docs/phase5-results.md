# Phase 5 results: poison events (claim with lease)

Local machine. Synthetic fixtures and seeded histories. Every section below must
contain real output, pasted verbatim, with date, command and git commit.

## Before the fix (phase 4 worker)

### E1 (Before, Phase 4 worker built from 7b7e855)

Scenario: an invoice that crashes the worker every time (`WORKER_CRASH_AFTER_GRANT=1`),
followed by a later, unrelated event. Run with the Phase 4 worker built from commit
`7b7e855` in a worktree.

Date: 2026-11-11 (UTC). Main tree HEAD: `7b7e8556dcb0ff18ca6538bad6e154a1d54f6f76`
(with uncommitted Phase 5 changes; those are not in the binary under test). Receiver: the
user's already-running RelayGuard receiver on 127.0.0.1:8080 (`curl -s http://127.0.0.1:8080/healthz`
returned `ok`). No other worker was running.

Build of the Phase 4 worker:

```text
$ git worktree add C:/dev/relayguard-phase4 7b7e855
Preparing worktree (detached HEAD 7b7e855)
HEAD is now at 7b7e855 Phase 4 results summary - updated

$ (in C:/dev/relayguard-phase4) go build -o C:/dev/relayguard/bin/worker-phase4.exe ./cmd/worker

$ go version -m C:/dev/relayguard/bin/worker-phase4.exe | grep vcs
	build	vcs=git
	build	vcs.revision=7b7e8556dcb0ff18ca6538bad6e154a1d54f6f76
	build	vcs.time=2026-11-05T14:05:03Z
	build	vcs.modified=false

$ go version
go version go1.27.3 windows/amd64
```

Data setup (started 2026-11-11T23:32:01Z):

```text
### dev.ps1 reset-data
TRUNCATE TABLE
### dev.ps1 send testdata\events\customer_created.json
attempt 1 -> HTTP 200
### dev.ps1 send testdata\events\invoice_paid.json
attempt 1 -> HTTP 200
### dev.ps1 send testdata\events\subscription_active.json
attempt 1 -> HTTP 200
```

Crash loop: run from `C:\dev\relayguard` with
`powershell.exe -NoProfile -ExecutionPolicy Bypass -File <scratchpad>\crashloop.ps1`,
where the script was:

```text
Set-Location C:\dev\relayguard
$env:WORKER_CRASH_AFTER_GRANT = "1"
$log = "C:\dev\relayguard\bin\e1-crashloop.log"
"" | Set-Content $log
$deadline = (Get-Date).AddSeconds(30)
$cycle = 0
while ((Get-Date) -lt $deadline) {
    $cycle++
    "=== cycle $cycle start $((Get-Date).ToUniversalTime().ToString('o'))" | Add-Content $log
    & .\bin\worker-phase4.exe *>&1 | ForEach-Object { "$_" } | Add-Content $log
    "=== cycle $cycle exit code $LASTEXITCODE" | Add-Content $log
    Start-Sleep -Milliseconds 200
}
"=== loop stopped after $cycle cycles $((Get-Date).ToUniversalTime().ToString('o'))" | Add-Content $log
```

Result: 99 cycles, all exit code 1 (counted from the `=== cycle N exit code` lines in the log).
Afterwards no worker-phase4.exe process was running (checked with `tasklist`; that output is
not pasted here).
Excerpt from `bin\e1-crashloop.log` (first two cycles and the last line). The `===` lines are
UTC, and the worker's own log lines are local time (UTC-5):

```text
=== cycle 1 start 2026-11-11T23:32:22.5180912Z
2026/11/11 18:32:24 FAULT INJECTION ON: always at after_grant
2026/11/11 18:32:24 RelayGuard worker started
2026/11/11 18:32:24 applied evt_fx_customer_1 (customer.created)
2026/11/11 18:32:24 FAULT: crashing at after_grant
=== cycle 1 exit code 1
=== cycle 2 start 2026-11-11T23:32:24.9519335Z
2026/11/11 18:32:24 FAULT INJECTION ON: always at after_grant
2026/11/11 18:32:24 RelayGuard worker started
2026/11/11 18:32:24 FAULT: crashing at after_grant
=== cycle 2 exit code 1
...
=== loop stopped after 99 cycles 2026-11-11T23:32:52.7517526Z
```

State after the loop, captured at 2026-11-11T23:35:37Z. No worker ran and no events were sent
between the loop stopping and this capture. An earlier capture of the same two views at
2026-11-11T23:35:12Z (not pasted) had the same content.

```text
$ powershell.exe -NoProfile -ExecutionPolicy Bypass -File dev.ps1 inbox
 id |     event_id      |          event_type           | status  | attempts | last_error 
----+-------------------+-------------------------------+---------+----------+------------
  1 | evt_fx_customer_1 | customer.created              | done    |        1 | 
  2 | evt_fx_invoice_1  | invoice.paid                  | pending |        0 | 
  3 | evt_fx_sub_active | customer.subscription.updated | pending |        0 | 
(3 rows)

$ powershell.exe -NoProfile -ExecutionPolicy Bypass -File dev.ps1 state
 stripe_customer_id | credit_balance | grants | ledger_total 
--------------------+----------------+--------+--------------
 cus_fx_1           |              0 |      0 |            0
(1 row)

 stripe_subscription_id | status | last_event_id | last_event_created 
------------------------+--------+---------------+--------------------
(0 rows)
```

This matches the plan's expectation. Across 99 crash cycles the invoice stays `pending` with
`attempts` 0. Interpretation, not measured separately: the attempt counter is rolled back with each crashed transaction, so the event
never reaches the dead-letter limit. The subscription event behind it is never processed, and no
credits are granted.

The `healthz` check alone does not identify the receiver, because the naive receiver also answers
`ok` on `/healthz`. The inbox rows above are what show that the RelayGuard receiver was the one on
8080: only it writes to `inbox`, and `cmd/naive` contains no reference to `inbox`.

## After the fix

The sections marked **(evidence)** are the runs to cite. They were run on 2026-11-12 (UTC),
from 00:36:54Z (the failed E2 setup attempt) to 00:43:56Z, by one experiment-runner, with no
other worker or chaos process running. `tasklist` was checked before each task and after each
task, and no `worker.exe`, `chaos.exe` or `naive.exe` was listed. The earlier sections marked
**(previous-session run, superseded)** are kept below them unchanged, apart from their headings,
a note giving the reason each was superseded, and the rename note.

Times in this doc are UTC unless marked local. The `Date:` lines in the results files' headers
and the worker's own log lines are local time (UTC-5).

### Tree and binary state for all evidence runs

Main tree HEAD `2c708e68ffec82261c38c1c191b2333a09474c58`. `bin/worker.exe` and `bin/chaos.exe`
come from the lead's `dev.ps1 build` at 2026-11-12T00:28:20Z-00:28:23Z, and nothing was rebuilt
afterwards. `vcs.modified=true` is expected for every run in this phase. Go records it at build
time and counts untracked files. At 00:28:20Z the only change in the tree was the untracked
`docs/phase5-results.md`; the results files listed below did not exist yet. There are no
tracked changes.

Caveat: the superseded E2 section shows an earlier `bin/worker.exe` with `vcs.modified=false`
and the same `vcs.time` (`vcs.time` is the commit time, not the build time). That fits only if
that binary was built before `docs/phase5-results.md` existed. Its build time was not recorded,
so this cannot be confirmed.

Captured right after E4:

```text
$ git status --porcelain --untracked-files=all
?? docs/phase5-results.md
?? results/relayguard-crash-lease-unattended.md
?? results/relayguard-crash-lease.md
?? results/relayguard-poison.md

$ go version -m bin/worker.exe | grep vcs
	build	vcs=git
	build	vcs.revision=2c708e68ffec82261c38c1c191b2333a09474c58
	build	vcs.time=2026-11-12T00:10:49Z
	build	vcs.modified=true
```

When the evidence runs started, only `docs/phase5-results.md` and
`results/relayguard-crash-lease-unattended.md` were untracked. The other two results files were
written by E3 and E4.

Build evidence: there is no build log. The binaries' timestamps match the lead's `dev.ps1 build`
(local time, UTC-5):

```text
$ ls -la --time-style=full-iso bin/worker.exe bin/chaos.exe
-rwxr-xr-x 1 adabs 197609 17109504 2026-11-11 19:28:23.213184500 -0500 bin/chaos.exe
-rwxr-xr-x 1 adabs 197609 14875648 2026-11-11 19:28:20.117892800 -0500 bin/worker.exe
```

Unit tests, run 2026-11-12T02:26:31Z at HEAD `2c708e68ffec82261c38c1c191b2333a09474c58`, after
the evidence runs, with the same tree as above. `db-test` was not run. The `(cached)` results
come from Go's test cache, which was valid for this source tree. Exit status 0.

```text
$ powershell.exe -NoProfile -ExecutionPolicy Bypass -File dev.ps1 test
?   	github.com/Deepak1365/relayguard/cmd/chaos	[no test files]
?   	github.com/Deepak1365/relayguard/cmd/migrate	[no test files]
?   	github.com/Deepak1365/relayguard/cmd/naive	[no test files]
?   	github.com/Deepak1365/relayguard/cmd/receiver	[no test files]
?   	github.com/Deepak1365/relayguard/cmd/sendevent	[no test files]
ok  	github.com/Deepak1365/relayguard/cmd/worker	(cached)
?   	github.com/Deepak1365/relayguard/internal/app	[no test files]
?   	github.com/Deepak1365/relayguard/internal/config	[no test files]
?   	github.com/Deepak1365/relayguard/internal/db	[no test files]
?   	github.com/Deepak1365/relayguard/internal/fault	[no test files]
ok  	github.com/Deepak1365/relayguard/internal/sim	(cached)
?   	github.com/Deepak1365/relayguard/internal/stripeevent	[no test files]
ok  	github.com/Deepak1365/relayguard/internal/stripesig	(cached)
?   	github.com/Deepak1365/relayguard/migrations	[no test files]
```

Receiver: the user's RelayGuard receiver on 127.0.0.1:8080. `curl -s http://127.0.0.1:8080/healthz`
returned `ok`. The process listening on 8080 was
`C:\Users\adabs\AppData\Local\Temp\go-build1909373459\b001\exe\receiver.exe` (PID 24428). Its
parent (PID 2668) is `"C:\Program Files\Go\bin\go.exe" run ./cmd/receiver`, started
2026-11-11 18:00:14 local. That makes it the RelayGuard receiver, not `cmd/naive`. Its
"listening" startup line was not seen.

### E2 (evidence): After, Phase 5 worker, rerun

Scenario: same as E1 (`WORKER_CRASH_AFTER_GRANT=1`, so the invoice crashes the worker every
time), with the Phase 5 worker and `WORKER_LEASE=1s`.

Data setup (started 2026-11-12T00:38:00Z). An attempt at 00:36:54Z ran `reset-data`, but its
three `send` calls failed because of a shell quoting mistake (`read testdata\events$f.json: ...
cannot find the file`), so nothing reached the receiver. The setup was then repeated in full:

```text
### dev.ps1 reset-data
TRUNCATE TABLE
### dev.ps1 send testdata/events/customer_created.json
attempt 1 -> HTTP 200
### dev.ps1 send testdata/events/invoice_paid.json
attempt 1 -> HTTP 200
### dev.ps1 send testdata/events/subscription_active.json
attempt 1 -> HTTP 200
### dev.ps1 inbox (before the loop)
 id |     event_id      |          event_type           | status  | attempts | last_error 
----+-------------------+-------------------------------+---------+----------+------------
  1 | evt_fx_customer_1 | customer.created              | pending |        0 | 
  2 | evt_fx_invoice_1  | invoice.paid                  | pending |        0 | 
  3 | evt_fx_sub_active | customer.subscription.updated | pending |        0 | 
(3 rows)
```

Crash loop: run from `C:\dev\relayguard` at 2026-11-12T00:38:36Z with
`powershell.exe -NoProfile -ExecutionPolicy Bypass -File <scratchpad>\e2-crashloop.ps1`.
The loop works like E1's, with three differences:
- Each cycle starts the worker with `Start-Process -PassThru` and keeps its `Handle`, so
  `ExitCode` can be read after the process exits.
- While a worker runs, the script polls the invoice's status every 500 ms.
- When the invoice is `dead`, the script waits 3 s so the event behind it can be processed,
  then stops the worker. Once the invoice is dead the worker no longer crashes and would
  otherwise idle forever. The deadline is 180 s.

The script was:

```text
Set-Location C:\dev\relayguard
$env:WORKER_CRASH_AFTER_GRANT = "1"
$env:WORKER_LEASE = "1s"
$log = "C:\dev\relayguard\bin\e2-crashloop.log"
$out = "C:\dev\relayguard\bin\e2-cycle.out"
$err = "C:\dev\relayguard\bin\e2-cycle.err"
"" | Set-Content $log
function Get-InvoiceStatus {
    $s = & docker compose exec -T db psql -U relayguard -d relayguard -tA -c "SELECT status FROM inbox WHERE event_id = 'evt_fx_invoice_1';"
    return "$s".Trim()
}
$deadline = (Get-Date).AddSeconds(180)
$cycle = 0
$dead = $false
while ((Get-Date) -lt $deadline -and -not $dead) {
    $cycle++
    "=== cycle $cycle start $((Get-Date).ToUniversalTime().ToString('o'))" | Add-Content $log
    $p = Start-Process -FilePath .\bin\worker.exe -NoNewWindow -PassThru -RedirectStandardOutput $out -RedirectStandardError $err
    $null = $p.Handle   # keep the handle so ExitCode is available after exit
    $stoppedByScript = $false
    while (-not $p.HasExited) {
        Start-Sleep -Milliseconds 500
        if ((Get-InvoiceStatus) -eq "dead") {
            $dead = $true
            Start-Sleep -Seconds 3   # let the worker process the event behind the invoice
            if (-not $p.HasExited) { Stop-Process -Id $p.Id -Force; $stoppedByScript = $true }
            break
        }
        if ((Get-Date) -ge $deadline) {
            if (-not $p.HasExited) { Stop-Process -Id $p.Id -Force; $stoppedByScript = $true }
            break
        }
    }
    $p.WaitForExit()
    Get-Content $out, $err -ErrorAction SilentlyContinue | Add-Content $log
    if ($stoppedByScript) {
        "=== cycle $cycle worker stopped by script (Stop-Process), exit code $($p.ExitCode)" | Add-Content $log
    } else {
        "=== cycle $cycle exit code $($p.ExitCode)" | Add-Content $log
    }
    if (-not $dead -and (Get-InvoiceStatus) -eq "dead") { $dead = $true }
    Start-Sleep -Milliseconds 200
}
"=== loop stopped after $cycle cycles $((Get-Date).ToUniversalTime().ToString('o')) invoice_dead=$dead" | Add-Content $log
Remove-Item $out, $err -ErrorAction SilentlyContinue
```

Result: 9 cycles.
- Cycles 1-8 each ended with `FAULT: crashing at after_grant` and `exit code 1`.
- In cycle 9 the worker marked the invoice dead and then applied the subscription. The script
  then stopped it. The `-1` exit code comes from `Stop-Process`, not from the worker.

Starts of consecutive cycles were 1.26-1.34 s apart, measured from the `=== cycle N start` lines.
The loop ended at 00:38:51Z, and `tasklist` afterwards showed no `worker.exe`. The `===` lines
are UTC, and the worker's own lines are local time (UTC-5). Full `bin\e2-crashloop.log` (its
first line is empty):

```text

=== cycle 1 start 2026-11-12T00:38:37.1398749Z
2026/11/11 19:38:37 FAULT INJECTION ON: always at after_grant
2026/11/11 19:38:37 RelayGuard worker started (lease 1s, max attempts 8)
2026/11/11 19:38:37 applied evt_fx_customer_1 (customer.created)
2026/11/11 19:38:37 FAULT: crashing at after_grant
=== cycle 1 exit code 1
=== cycle 2 start 2026-11-12T00:38:38.4796532Z
2026/11/11 19:38:38 FAULT INJECTION ON: always at after_grant
2026/11/11 19:38:38 RelayGuard worker started (lease 1s, max attempts 8)
2026/11/11 19:38:38 FAULT: crashing at after_grant
=== cycle 2 exit code 1
=== cycle 3 start 2026-11-12T00:38:39.7394178Z
2026/11/11 19:38:39 FAULT INJECTION ON: always at after_grant
2026/11/11 19:38:39 RelayGuard worker started (lease 1s, max attempts 8)
2026/11/11 19:38:39 FAULT: crashing at after_grant
=== cycle 3 exit code 1
=== cycle 4 start 2026-11-12T00:38:41.0413081Z
2026/11/11 19:38:41 FAULT INJECTION ON: always at after_grant
2026/11/11 19:38:41 RelayGuard worker started (lease 1s, max attempts 8)
2026/11/11 19:38:41 FAULT: crashing at after_grant
=== cycle 4 exit code 1
=== cycle 5 start 2026-11-12T00:38:42.3147626Z
2026/11/11 19:38:42 FAULT INJECTION ON: always at after_grant
2026/11/11 19:38:42 RelayGuard worker started (lease 1s, max attempts 8)
2026/11/11 19:38:42 FAULT: crashing at after_grant
=== cycle 5 exit code 1
=== cycle 6 start 2026-11-12T00:38:43.5839502Z
2026/11/11 19:38:43 FAULT INJECTION ON: always at after_grant
2026/11/11 19:38:43 RelayGuard worker started (lease 1s, max attempts 8)
2026/11/11 19:38:43 FAULT: crashing at after_grant
=== cycle 6 exit code 1
=== cycle 7 start 2026-11-12T00:38:44.8789409Z
2026/11/11 19:38:44 FAULT INJECTION ON: always at after_grant
2026/11/11 19:38:44 RelayGuard worker started (lease 1s, max attempts 8)
2026/11/11 19:38:44 FAULT: crashing at after_grant
=== cycle 7 exit code 1
=== cycle 8 start 2026-11-12T00:38:46.1656682Z
2026/11/11 19:38:46 FAULT INJECTION ON: always at after_grant
2026/11/11 19:38:46 RelayGuard worker started (lease 1s, max attempts 8)
2026/11/11 19:38:46 FAULT: crashing at after_grant
=== cycle 8 exit code 1
=== cycle 9 start 2026-11-12T00:38:47.4699887Z
2026/11/11 19:38:47 FAULT INJECTION ON: always at after_grant
2026/11/11 19:38:47 RelayGuard worker started (lease 1s, max attempts 8)
2026/11/11 19:38:47 DEAD inbox row 2 after 8 attempts (possible poison event)
2026/11/11 19:38:47 applied evt_fx_sub_active (customer.subscription.updated)
=== cycle 9 worker stopped by script (Stop-Process), exit code -1
=== loop stopped after 9 cycles 2026-11-12T00:38:51.5537006Z invoice_dead=True
```

State after the loop, captured at 2026-11-12T00:39:22Z. No worker ran and no events were sent
between the loop stopping and this capture. `dev.ps1 inbox` truncates `last_error` to 70
characters, so the full value is also shown with a direct psql query.

```text
$ powershell.exe -NoProfile -ExecutionPolicy Bypass -File dev.ps1 inbox
 id |     event_id      |          event_type           | status | attempts |                               last_error                               
----+-------------------+-------------------------------+--------+----------+------------------------------------------------------------------------
  1 | evt_fx_customer_1 | customer.created              | done   |        1 | 
  2 | evt_fx_invoice_1  | invoice.paid                  | dead   |        8 | gave up after 8 attempts; last error: none recorded (the worker crashe
  3 | evt_fx_sub_active | customer.subscription.updated | done   |        1 | 
(3 rows)

$ powershell.exe -NoProfile -ExecutionPolicy Bypass -File dev.ps1 state
 stripe_customer_id | credit_balance | grants | ledger_total 
--------------------+----------------+--------+--------------
 cus_fx_1           |              0 |      0 |            0
(1 row)

 stripe_subscription_id | status |   last_event_id   | last_event_created 
------------------------+--------+-------------------+--------------------
 sub_fx_1               | active | evt_fx_sub_active |         1793800120
(1 row)

$ docker compose exec -T db psql -U relayguard -d relayguard -P pager=off -c "SELECT id, event_id, status, attempts, last_error FROM inbox WHERE event_id = 'evt_fx_invoice_1';"
 id |     event_id     | status | attempts |                                               last_error                                                
----+------------------+--------+----------+---------------------------------------------------------------------------------------------------------
  2 | evt_fx_invoice_1 | dead   |        8 | gave up after 8 attempts; last error: none recorded (the worker crashed or was killed while processing)
(1 row)
```

The final state matches the plan's expectation:
- Customer event: `done`, 1 attempt.
- Invoice: `dead` after 8 attempts, with `last_error` "gave up after 8 attempts; last error:
  none recorded (the worker crashed or was killed while processing)".
- Subscription event: `done`, 1 attempt.
- `cus_fx_1`: balance 0, 0 grants, ledger 0. `sub_fx_1`: active.

Compared with E1, the Phase 5 worker stops retrying the crashing invoice after 8 attempts and
then processes the event behind it. The Phase 4 worker stayed stuck on the invoice for 99
cycles with `attempts` 0.

Ordering observation, which repeats the superseded run's finding: the subscription did not
overtake the invoice while the invoice's lease was running. There is no `applied evt_fx_sub_active`
line in cycles 1-8; it appears only in cycle 9, after `DEAD inbox row 2`. The restart interval
here was 1.26-1.34 s, shorter than in the superseded run (about 1.8 s) but still longer than the
1 s lease. The claim query (`cmd/worker/main.go`) selects
`WHERE status = 'pending' AND next_attempt_at <= now() ORDER BY id LIMIT 1`. Possible reason,
not measured: every restarted worker found the invoice's lease already expired and claimed the
invoice (id 2) again before the subscription (id 3). To see the subscription overtake the invoice, the next run would
need a lease longer than the restart interval.

### E2 (previous-session run, superseded)

Superseded because the exit codes were not captured (the `exit code` lines are empty) and
`last_error` was captured only in its 70-character truncated form. Its numbers agree with the
E2 evidence run above. The evidence run overwrote `bin\e2-crashloop.log`, so the log text pasted
in this section is the only remaining copy of this run's log.

Scenario: same as E1 (`WORKER_CRASH_AFTER_GRANT=1`, so the invoice crashes the worker every
time), with the Phase 5 worker and `WORKER_LEASE=1s`.

Date: 2026-11-12 (UTC). Main tree HEAD: `2c708e68ffec82261c38c1c191b2333a09474c58`, clean apart
from the untracked `docs/phase5-results.md`. Receiver: the user's already-running RelayGuard
receiver on 127.0.0.1:8080 (`curl -s http://127.0.0.1:8080/healthz` returned `ok`). No other
worker was running (checked with `tasklist`).

Binary under test:

```text
$ go version -m bin/worker.exe | grep vcs
	build	vcs=git
	build	vcs.revision=2c708e68ffec82261c38c1c191b2333a09474c58
	build	vcs.time=2026-11-12T00:10:49Z
	build	vcs.modified=false
```

Data setup (started 2026-11-12T00:16:56Z). The first three `send` attempts failed because of
shell path escaping (nothing was sent; the receiver was never reached); they were repeated
with forward slashes:

```text
### dev.ps1 reset-data
TRUNCATE TABLE
### dev.ps1 send testdata\events\customer_created.json   (as typed through Git Bash)
2026/11/11 19:17:19 read testdataeventscustomer_created.json: open testdataeventscustomer_created.json: The system cannot find the file specified.
exit status 1
### dev.ps1 send testdata\events\invoice_paid.json   (as typed through Git Bash)
2026/11/11 19:17:23 read testdataeventsinvoice_paid.json: open testdataeventsinvoice_paid.json: The system cannot find the file specified.
exit status 1
### dev.ps1 send testdata\events\subscription_active.json   (as typed through Git Bash)
2026/11/11 19:17:26 read testdataeventssubscription_active.json: open testdataeventssubscription_active.json: The system cannot find the file specified.
exit status 1
### dev.ps1 send testdata/events/customer_created.json
attempt 1 -> HTTP 200
### dev.ps1 send testdata/events/invoice_paid.json
attempt 1 -> HTTP 200
### dev.ps1 send testdata/events/subscription_active.json
attempt 1 -> HTTP 200
### dev.ps1 inbox (before the loop)
 id |     event_id      |          event_type           | status  | attempts | last_error 
----+-------------------+-------------------------------+---------+----------+------------
  1 | evt_fx_customer_1 | customer.created              | pending |        0 | 
  2 | evt_fx_invoice_1  | invoice.paid                  | pending |        0 | 
  3 | evt_fx_sub_active | customer.subscription.updated | pending |        0 | 
(3 rows)
```

Crash loop: run from `C:\dev\relayguard` with
`powershell.exe -NoProfile -ExecutionPolicy Bypass -File <scratchpad>\e2-crashloop.ps1`.
Unlike E1's loop, this one starts the worker with `Start-Process` and polls `dev.ps1 inbox`
every 500 ms while it runs, because once the invoice is dead the worker no longer crashes
and would idle forever. The loop stops the worker as soon as the invoice shows `dead`
(60 s cap). The script was:

```text
Set-Location C:\dev\relayguard
$env:WORKER_CRASH_AFTER_GRANT = "1"
$env:WORKER_LEASE = "1s"
$log = "C:\dev\relayguard\bin\e2-crashloop.log"
$scratch = "<scratchpad>"
$outF = Join-Path $scratch "e2-worker-out.txt"
$errF = Join-Path $scratch "e2-worker-err.txt"
"" | Set-Content $log
$start = Get-Date
$deadline = $start.AddSeconds(60)
"=== loop start $($start.ToUniversalTime().ToString('o'))" | Add-Content $log
$cycle = 0
$dead = $false
while ((Get-Date) -lt $deadline -and -not $dead) {
    $cycle++
    "=== cycle $cycle start $((Get-Date).ToUniversalTime().ToString('o'))" | Add-Content $log
    $p = Start-Process -FilePath .\bin\worker.exe -NoNewWindow -PassThru -RedirectStandardOutput $outF -RedirectStandardError $errF
    $killed = $false
    while (-not $p.HasExited) {
        Start-Sleep -Milliseconds 500
        $inbox = (& powershell.exe -NoProfile -ExecutionPolicy Bypass -File dev.ps1 inbox) -join "`n"
        if ($inbox -match "evt_fx_invoice_1\s*\|[^|]*\|\s*dead\s*\|") {
            $dead = $true
            "=== invoice dead detected $((Get-Date).ToUniversalTime().ToString('o')) (elapsed $(((Get-Date) - $start).TotalSeconds)s)" | Add-Content $log
            if (-not $p.HasExited) { Stop-Process -Id $p.Id -Force; $killed = $true }
            break
        }
        if ((Get-Date) -ge $deadline) {
            if (-not $p.HasExited) { Stop-Process -Id $p.Id -Force; $killed = $true }
            break
        }
    }
    $p.WaitForExit()
    Get-Content $outF, $errF | Add-Content $log
    if ($killed) { "=== cycle $cycle worker stopped by loop" | Add-Content $log }
    else { "=== cycle $cycle exit code $($p.ExitCode)" | Add-Content $log }
    if (-not $dead) {
        $inbox = (& powershell.exe -NoProfile -ExecutionPolicy Bypass -File dev.ps1 inbox) -join "`n"
        if ($inbox -match "evt_fx_invoice_1\s*\|[^|]*\|\s*dead\s*\|") {
            $dead = $true
            "=== invoice dead detected $((Get-Date).ToUniversalTime().ToString('o')) (elapsed $(((Get-Date) - $start).TotalSeconds)s)" | Add-Content $log
        }
    }
    Start-Sleep -Milliseconds 200
}
"=== loop stopped after $cycle cycles, dead=$dead $((Get-Date).ToUniversalTime().ToString('o'))" | Add-Content $log
```

Result: 9 cycles. Cycles 1-8 each ended with `FAULT: crashing at after_grant`. In cycle 9 the
worker marked the invoice dead and then applied the subscription; the loop then stopped it.
Time from loop start to the loop detecting `dead`: 17.745287 s (as printed in the log; this
includes the 500 ms polling granularity). The `exit code` lines are empty: with
`Start-Process -PassThru`, PowerShell did not report the exit code, so exit codes were not
measured in this run. After the loop no worker.exe process was running (checked with
`tasklist`; output not pasted). The `===` lines are UTC, the worker's own lines are local
time (UTC-5).

Full `bin\e2-crashloop.log`:

```text
=== loop start 2026-11-12T00:27:20.4996377Z
=== cycle 1 start 2026-11-12T00:27:20.5222634Z
2026/11/11 19:27:22 FAULT INJECTION ON: always at after_grant
2026/11/11 19:27:22 RelayGuard worker started (lease 1s, max attempts 8)
2026/11/11 19:27:22 applied evt_fx_customer_1 (customer.created)
2026/11/11 19:27:22 FAULT: crashing at after_grant
=== cycle 1 exit code 
=== cycle 2 start 2026-11-12T00:27:24.7371404Z
2026/11/11 19:27:24 FAULT INJECTION ON: always at after_grant
2026/11/11 19:27:24 RelayGuard worker started (lease 1s, max attempts 8)
2026/11/11 19:27:24 FAULT: crashing at after_grant
=== cycle 2 exit code 
=== cycle 3 start 2026-11-12T00:27:26.5124945Z
2026/11/11 19:27:26 FAULT INJECTION ON: always at after_grant
2026/11/11 19:27:26 RelayGuard worker started (lease 1s, max attempts 8)
2026/11/11 19:27:26 FAULT: crashing at after_grant
=== cycle 3 exit code 
=== cycle 4 start 2026-11-12T00:27:28.2592109Z
2026/11/11 19:27:28 FAULT INJECTION ON: always at after_grant
2026/11/11 19:27:28 RelayGuard worker started (lease 1s, max attempts 8)
2026/11/11 19:27:28 FAULT: crashing at after_grant
=== cycle 4 exit code 
=== cycle 5 start 2026-11-12T00:27:30.0538806Z
2026/11/11 19:27:30 FAULT INJECTION ON: always at after_grant
2026/11/11 19:27:30 RelayGuard worker started (lease 1s, max attempts 8)
2026/11/11 19:27:30 FAULT: crashing at after_grant
=== cycle 5 exit code 
=== cycle 6 start 2026-11-12T00:27:31.8176372Z
2026/11/11 19:27:31 FAULT INJECTION ON: always at after_grant
2026/11/11 19:27:31 RelayGuard worker started (lease 1s, max attempts 8)
2026/11/11 19:27:31 FAULT: crashing at after_grant
=== cycle 6 exit code 
=== cycle 7 start 2026-11-12T00:27:33.5888826Z
2026/11/11 19:27:33 FAULT INJECTION ON: always at after_grant
2026/11/11 19:27:33 RelayGuard worker started (lease 1s, max attempts 8)
2026/11/11 19:27:33 FAULT: crashing at after_grant
=== cycle 7 exit code 
=== cycle 8 start 2026-11-12T00:27:35.3647078Z
2026/11/11 19:27:35 FAULT INJECTION ON: always at after_grant
2026/11/11 19:27:35 RelayGuard worker started (lease 1s, max attempts 8)
2026/11/11 19:27:35 FAULT: crashing at after_grant
=== cycle 8 exit code 
=== cycle 9 start 2026-11-12T00:27:37.1624911Z
=== invoice dead detected 2026-11-12T00:27:38.2449247Z (elapsed 17.745287s)
2026/11/11 19:27:37 FAULT INJECTION ON: always at after_grant
2026/11/11 19:27:37 RelayGuard worker started (lease 1s, max attempts 8)
2026/11/11 19:27:37 DEAD inbox row 2 after 8 attempts (possible poison event)
2026/11/11 19:27:37 applied evt_fx_sub_active (customer.subscription.updated)
=== cycle 9 worker stopped by loop
=== loop stopped after 9 cycles, dead=True 2026-11-12T00:27:38.4953008Z
```

(The worker's output is appended to the log only after the process ends, so the
`invoice dead detected` line appears before cycle 9's worker lines.)

State after the loop, captured at 2026-11-12T00:27:51Z. No worker ran and no events were sent
between the loop stopping and this capture. `dev.ps1 inbox` truncates `last_error` to 70
characters; the full text was not captured.

```text
$ powershell.exe -NoProfile -ExecutionPolicy Bypass -File dev.ps1 inbox
 id |     event_id      |          event_type           | status | attempts |                               last_error                               
----+-------------------+-------------------------------+--------+----------+------------------------------------------------------------------------
  1 | evt_fx_customer_1 | customer.created              | done   |        1 | 
  2 | evt_fx_invoice_1  | invoice.paid                  | dead   |        8 | gave up after 8 attempts; last error: none recorded (the worker crashe
  3 | evt_fx_sub_active | customer.subscription.updated | done   |        1 | 
(3 rows)

$ powershell.exe -NoProfile -ExecutionPolicy Bypass -File dev.ps1 state
 stripe_customer_id | credit_balance | grants | ledger_total 
--------------------+----------------+--------+--------------
 cus_fx_1           |              0 |      0 |            0
(1 row)

 stripe_subscription_id | status |   last_event_id   | last_event_created 
------------------------+--------+-------------------+--------------------
 sub_fx_1               | active | evt_fx_sub_active |         1793800120
(1 row)
```

The final state matches the plan's expectation: customer `done`/1, invoice `dead`/8 with
`last_error` starting "gave up after 8 attempts; last error: none recorded", subscription
`done`/1; `cus_fx_1` balance 0, 0 grants, ledger 0; `sub_fx_1` active.

Difference from the plan's explanation: the plan expected the subscription to overtake the
invoice while the invoice's lease was running. In this run it did not. The log shows the
subscription applied only in cycle 9, after the invoice was marked dead. Cycles 1-8 show no
`applied evt_fx_sub_active` line. Possible reason (not measured): each restart took about
1.8 s, which is longer than the 1 s lease, so on every restart the invoice's lease had
already expired and the worker claimed the invoice again before the subscription.

## Chaos harness (regression and poison)

| Run | Flags | Seeds | Violations | Restarts (hard kills) | Inbox dead |
|---|---|---|---|---|---|
| E3 (evidence) regression: same as the phase 4 crash run ([results](../results/relayguard-crash-lease.md)) | `-dup 0.2 -reorder 10 -crash-rate 0.1 -kill-interval 2s -lease 1s` | 1-20 | 0 | 272 (30) | 0 in every seed |
| E3b crash-only regression | `-crash-rate 0.1 -kill-interval 2s -lease 1s` | 1-20 | not run | not run | not run |
| E4 (evidence) poison events ([results](../results/relayguard-poison.md)) | same as regression + `-poison 2` | 1-5 | 0 | 155 (21) | 2 in every seed |
| E5 control: phase 4 worker with poison | `-poison 2 -lease 1s -timeout 1m`, `bin/worker-phase4.exe` | 1 | not run | not run | not run |
| E3 (previous-session run, superseded) ([results](../results/relayguard-crash-lease-unattended.md)) | same as E3 | 1-20 | 0 | 273 (32) | 0 in every seed |

Results files: `results/relayguard-crash-lease.md` (E3 evidence) and `results/relayguard-poison.md`
(E4 evidence). The superseded run's file was renamed from `results/relayguard-crash-lease.md` to
`results/relayguard-crash-lease-unattended.md` (plain rename, approved by the user) so that the
evidence run could write the original name. E3b and E5 were not run.

### E3 (evidence): regression (phase 4 crash config plus the lease)

Date: 2026-11-12 (UTC), started 00:39:34Z, finished 00:41:12Z. HEAD `2c708e6`. Binaries, tree and
receiver are described in "Tree and binary state" above. Before the run, `tasklist` showed no
`worker.exe`, `chaos.exe` or `naive.exe`, and `healthz` returned `ok`. `bin\supervised.log` had
12860 lines before the run and 15344 after it, so this run is lines 12861-15344.

Command (run from `C:\dev\relayguard` in Git Bash, exit status 0):

```text
./bin/chaos.exe -target relayguard -seeds 1-20 -dup 0.2 -reorder 10 -supervise bin/worker.exe -crash-rate 0.1 -kill-interval 2s -lease 1s -out results/relayguard-crash-lease.md
```

Stdout, verbatim:

```text
target=relayguard seeds=1-20 customers=20 dup=0.20 reorder=10 poison=0 crash-rate=0.10 kill-interval=2s supervise="bin/worker.exe"
seed=1    events=84   deliveries=99   provider_retries=0   gave_up=0   inbox[done=81 stale=3] violations=0 (3.206s)
seed=2    events=83   deliveries=97   provider_retries=0   gave_up=0   inbox[done=81 stale=2] violations=0 (5.931s)
seed=3    events=80   deliveries=95   provider_retries=0   gave_up=0   inbox[done=79 stale=1] violations=0 (4.153s)
seed=4    events=78   deliveries=93   provider_retries=0   gave_up=0   inbox[done=75 stale=3] violations=0 (3.427s)
seed=5    events=78   deliveries=92   provider_retries=0   gave_up=0   inbox[done=74 stale=4] violations=0 (3.955s)
seed=6    events=79   deliveries=91   provider_retries=0   gave_up=0   inbox[done=76 stale=3] violations=0 (3.431s)
seed=7    events=70   deliveries=89   provider_retries=0   gave_up=0   inbox[done=70] violations=0 (4.66s)
seed=8    events=71   deliveries=88   provider_retries=0   gave_up=0   inbox[done=67 stale=4] violations=0 (8.701s)
seed=9    events=81   deliveries=97   provider_retries=0   gave_up=0   inbox[done=74 stale=7] violations=0 (5.435s)
seed=10   events=89   deliveries=109  provider_retries=0   gave_up=0   inbox[done=85 stale=4] violations=0 (4.474s)
seed=11   events=80   deliveries=96   provider_retries=0   gave_up=0   inbox[done=76 stale=4] violations=0 (5.178s)
seed=12   events=85   deliveries=105  provider_retries=0   gave_up=0   inbox[done=83 stale=2] violations=0 (4.455s)
seed=13   events=80   deliveries=95   provider_retries=0   gave_up=0   inbox[done=78 stale=2] violations=0 (6.191s)
seed=14   events=83   deliveries=94   provider_retries=0   gave_up=0   inbox[done=79 stale=4] violations=0 (4.98s)
seed=15   events=89   deliveries=104  provider_retries=0   gave_up=0   inbox[done=87 stale=2] violations=0 (4.949s)
seed=16   events=63   deliveries=73   provider_retries=0   gave_up=0   inbox[done=61 stale=2] violations=0 (2.907s)
seed=17   events=70   deliveries=88   provider_retries=0   gave_up=0   inbox[done=69 stale=1] violations=0 (4.683s)
seed=18   events=91   deliveries=106  provider_retries=0   gave_up=0   inbox[done=89 stale=2] violations=0 (4.985s)
seed=19   events=68   deliveries=86   provider_retries=0   gave_up=0   inbox[done=62 stale=6] violations=0 (4.939s)
seed=20   events=90   deliveries=108  provider_retries=0   gave_up=0   inbox[done=88 stale=2] violations=0 (6.475s)
---
RESULT target=relayguard seeds=20 failing_seeds=0 violations=0 restarts=272 hard_kills=30
wrote results/relayguard-crash-lease.md
```

RESULT line:

```text
RESULT target=relayguard seeds=20 failing_seeds=0 violations=0 restarts=272 hard_kills=30
```

After the run, `tasklist` showed no `worker.exe` or `chaos.exe`. No Inbox cell contains `dead` or
`pending`. Line counts in this run's part of `bin\supervised.log` (lines 12861-15344, counted
with `grep -c`):

| Pattern | Lines |
|---|---|
| "DEAD inbox row" | 0 |
| "POISON" | 0 |
| "FAULT: crashing" | 242 |
| "worker started" | 268 |
| "panic" | 0 |
| "error", case-insensitive | 0 |

Worker-start lines are fewer than restarts + 1 (the initial start) in all three chaos runs: 268
vs 273 here, 154 vs 156 in E4, and 265 vs 274 in the superseded E3. A likely reason is hard kills
that land during startup, before the worker prints `worker started`. This was not investigated.

Determinism check against `results/relayguard-crash.md` (Phase 4). The table was generated by a
script from the two results files. P4 is Phase 4 and P5 is this run. Δ = P5 minus P4:

| Seed | Events P4 | Deliveries P4 | Events P5 | Deliveries P5 | Events/Deliveries | Inbox P4 | Inbox P5 | Δ done | Δ stale |
|---|---|---|---|---|---|---|---|---|---|
| 1 | 84 | 99 | 84 | 99 | match | done=82 stale=2 | done=81 stale=3 | -1 | 1 |
| 2 | 83 | 97 | 83 | 97 | match | done=82 stale=1 | done=81 stale=2 | -1 | 1 |
| 3 | 80 | 95 | 80 | 95 | match | done=79 stale=1 | done=79 stale=1 | 0 | 0 |
| 4 | 78 | 93 | 78 | 93 | match | done=77 stale=1 | done=75 stale=3 | -2 | 2 |
| 5 | 78 | 92 | 78 | 92 | match | done=75 stale=3 | done=74 stale=4 | -1 | 1 |
| 6 | 79 | 91 | 79 | 91 | match | done=77 stale=2 | done=76 stale=3 | -1 | 1 |
| 7 | 70 | 89 | 70 | 89 | match | done=70 stale=0 | done=70 stale=0 | 0 | 0 |
| 8 | 71 | 88 | 71 | 88 | match | done=67 stale=4 | done=67 stale=4 | 0 | 0 |
| 9 | 81 | 97 | 81 | 97 | match | done=78 stale=3 | done=74 stale=7 | -4 | 4 |
| 10 | 89 | 109 | 89 | 109 | match | done=86 stale=3 | done=85 stale=4 | -1 | 1 |
| 11 | 80 | 96 | 80 | 96 | match | done=76 stale=4 | done=76 stale=4 | 0 | 0 |
| 12 | 85 | 105 | 85 | 105 | match | done=84 stale=1 | done=83 stale=2 | -1 | 1 |
| 13 | 80 | 95 | 80 | 95 | match | done=79 stale=1 | done=78 stale=2 | -1 | 1 |
| 14 | 83 | 94 | 83 | 94 | match | done=82 stale=1 | done=79 stale=4 | -3 | 3 |
| 15 | 89 | 104 | 89 | 104 | match | done=89 stale=0 | done=87 stale=2 | -2 | 2 |
| 16 | 63 | 73 | 63 | 73 | match | done=62 stale=1 | done=61 stale=2 | -1 | 1 |
| 17 | 70 | 88 | 70 | 88 | match | done=70 stale=0 | done=69 stale=1 | -1 | 1 |
| 18 | 91 | 106 | 91 | 106 | match | done=90 stale=1 | done=89 stale=2 | -1 | 1 |
| 19 | 68 | 86 | 68 | 86 | match | done=64 stale=4 | done=62 stale=6 | -2 | 2 |
| 20 | 90 | 108 | 90 | 108 | match | done=89 stale=1 | done=88 stale=2 | -1 | 1 |

Totals: Phase 4 done=1558 stale=34. This run: done=1534 stale=58.
- Events and Deliveries match for all 20 seeds.
- In every seed, done + stale equals Events in both runs.
- Stale is higher than in Phase 4 in 16 seeds, equal in 4 (3, 7, 8, 11) and lower in none.

The superseded run (below) also had more stale rows than Phase 4, and its per-seed split differs
from this run's. The done/stale split varies from run to run under crashes. This is reported as
observed: the checker found 0 violations, so a different done/stale split is not a violation by
itself.

### E4 (evidence): poison events

Date: 2026-11-12 (UTC), started 00:42:56Z, finished 00:43:56Z. HEAD `2c708e6`, same binaries as
E3. Before the run, `tasklist` showed no `worker.exe`, `chaos.exe` or `naive.exe`. This run is
lines 15345-16372 of `bin\supervised.log`.

Command (run from `C:\dev\relayguard` in Git Bash, exit status 0):

```text
./bin/chaos.exe -target relayguard -seeds 1-5 -dup 0.2 -reorder 10 -supervise bin/worker.exe -crash-rate 0.1 -kill-interval 2s -lease 1s -poison 2 -out results/relayguard-poison.md
```

Stdout, verbatim:

```text
target=relayguard seeds=1-5 customers=20 dup=0.20 reorder=10 poison=2 crash-rate=0.10 kill-interval=2s supervise="bin/worker.exe"
seed=1    events=84   deliveries=101  provider_retries=0   gave_up=0   inbox[dead=2 done=81 stale=3] violations=0 (11.509s)
seed=2    events=83   deliveries=99   provider_retries=0   gave_up=0   inbox[dead=2 done=81 stale=2] violations=0 (13.303s)
seed=3    events=80   deliveries=97   provider_retries=0   gave_up=0   inbox[dead=2 done=78 stale=2] violations=0 (10.986s)
seed=4    events=78   deliveries=95   provider_retries=0   gave_up=0   inbox[dead=2 done=75 stale=3] violations=0 (11.962s)
seed=5    events=78   deliveries=94   provider_retries=0   gave_up=0   inbox[dead=2 done=73 stale=5] violations=0 (11.857s)
---
RESULT target=relayguard seeds=5 failing_seeds=0 violations=0 restarts=155 hard_kills=21
wrote results/relayguard-poison.md
```

RESULT line:

```text
RESULT target=relayguard seeds=5 failing_seeds=0 violations=0 restarts=155 hard_kills=21
```

This matches the plan's expectation: 0 violations and dead=2 in every seed. After the run,
`tasklist` showed no `worker.exe` or `chaos.exe`.
- In every seed, dead + done + stale = Events + 2. For example, seed 1: 2 + 81 + 3 = 86 = 84 + 2.
  The two poison events appear to be counted under Deliveries but not under Events. That reading
  comes from the numbers; the harness code was not checked.
- In every seed, Deliveries are exactly 2 higher than in E3 (101 vs 99, 99 vs 97, 97 vs 95,
  95 vs 93, 94 vs 92), and Events are the same.

Line counts in this run's part of `bin\supervised.log` (lines 15345-16372):

| Pattern | Lines |
|---|---|
| "DEAD inbox row" | 9 |
| "POISON" | 80 |
| "FAULT: crashing" | 54 |
| "worker started" | 154 |
| "panic" | 0 |
| "error", case-insensitive | 0 |

80 POISON lines = 5 seeds × 2 poison events × 8 attempts. Every poison event logged attempts 1
through 8.

**Unexpected detail (not a violation): 9 DEAD lines, not 10.** Seed 4 logged only one
`DEAD inbox row` line, but the harness reported `dead=2` for seed 4. The DEAD and attempt-8 lines
for all seeds (line numbers relative to line 15345):

```text
190:2026/11/11 19:43:05 POISON: evt_s1_poison_1 crashes the worker (attempt 8)
198:2026/11/11 19:43:06 DEAD inbox row 1 after 8 attempts (possible poison event)
199:2026/11/11 19:43:06 POISON: evt_s1_poison_0 crashes the worker (attempt 8)
206:2026/11/11 19:43:08 DEAD inbox row 52 after 8 attempts (possible poison event)
409:2026/11/11 19:43:20 POISON: evt_s2_poison_1 crashes the worker (attempt 8)
413:2026/11/11 19:43:20 POISON: evt_s2_poison_0 crashes the worker (attempt 8)
417:2026/11/11 19:43:21 DEAD inbox row 81 after 8 attempts (possible poison event)
418:2026/11/11 19:43:21 DEAD inbox row 94 after 8 attempts (possible poison event)
611:2026/11/11 19:43:29 POISON: evt_s3_poison_1 crashes the worker (attempt 8)
619:2026/11/11 19:43:31 DEAD inbox row 12 after 8 attempts (possible poison event)
620:2026/11/11 19:43:31 POISON: evt_s3_poison_0 crashes the worker (attempt 8)
624:2026/11/11 19:43:32 DEAD inbox row 29 after 8 attempts (possible poison event)
808:2026/11/11 19:43:41 POISON: evt_s4_poison_1 crashes the worker (attempt 8)
819:2026/11/11 19:43:43 DEAD inbox row 29 after 8 attempts (possible poison event)
820:2026/11/11 19:43:43 POISON: evt_s4_poison_0 crashes the worker (attempt 8)
1012:2026/11/11 19:43:53 POISON: evt_s5_poison_1 crashes the worker (attempt 8)
1023:2026/11/11 19:43:54 DEAD inbox row 2 after 8 attempts (possible poison event)
1024:2026/11/11 19:43:54 POISON: evt_s5_poison_0 crashes the worker (attempt 8)
1028:2026/11/11 19:43:56 DEAD inbox row 60 after 8 attempts (possible poison event)
```

The rest of seed 4 after line 820 (relative lines 821-829, before seed 5's first `applied` line)
has only worker starts:

```text
2026/11/11 19:43:43 FAULT INJECTION ON: crash rate 0.10 at every fault point
2026/11/11 19:43:43 FAULT INJECTION ON: test.poison events crash the worker
2026/11/11 19:43:43 RelayGuard worker started (lease 1s, max attempts 8)
2026/11/11 19:43:44 FAULT INJECTION ON: crash rate 0.10 at every fault point
2026/11/11 19:43:44 FAULT INJECTION ON: test.poison events crash the worker
2026/11/11 19:43:44 RelayGuard worker started (lease 1s, max attempts 8)
2026/11/11 19:43:44 FAULT INJECTION ON: crash rate 0.10 at every fault point
2026/11/11 19:43:44 FAULT INJECTION ON: test.poison events crash the worker
2026/11/11 19:43:44 RelayGuard worker started (lease 1s, max attempts 8)
```

In `deadLetterCrashed` (`cmd/worker/main.go`, lines 139-152), the
`UPDATE ... SET status = 'dead'` runs in autocommit, and only after `Exec` returns does the worker
log `DEAD inbox row`.

The supervisor does not stop the worker between seeds. `sup.Start()` runs once, before the seed
loop, and `sup.Stop()` runs only in `exit()` (`cmd/chaos/main.go`). In the log, seed 5's first
`applied` line follows directly after relative line 829, with no stop in between.

The worker's stdout and stderr are written straight to the log file (`internal/sim/supervise.go`
sets `cmd.Stdout`/`cmd.Stderr` to the `*os.File`), so buffering cannot lose a line.

Possible explanation, not verified: one of the supervisor's random hard kills (`killRandomly` in
`internal/sim/supervise.go`) hit the worker that dead-lettered seed 4's second poison event. The
kill would have landed after the server committed the UPDATE but before the log line was written.
The workers started at relative lines 823 and 826 ended without any output, which fits this.

If that is right, the log line undercounts dead-lettering, but the inbox does not. The seed 4
inbox rows were overwritten by seed 5, so they could not be inspected afterwards.

### E3 (previous-session run, superseded): regression (phase 4 crash config plus the lease)

Superseded because the lead rebuilt the binaries (`dev.ps1 build`, 00:28:20Z-00:28:23Z) just before
this run started. This run was launched by the previous session's experiment-runner. When the
current session began, the lead believed no chaos process was running, and the current runner
found it mid-run at 2026-11-12T00:30:26Z. Its results file is now
`results/relayguard-crash-lease-unattended.md` (renamed; the paths quoted inside this section are
the original ones). Its header RESULT line, verbatim:

```text
**Seeds: 20, failing seeds: 0, violations: 0, process restarts: 273 (of which hard kills: 32)**
```

Date: 2026-11-12 (UTC); the run started after 2026-11-12T00:28:14Z, and its first supervised worker
started at 2026/11/11 19:29:13 local (UTC-5). HEAD `2c708e6`. Receiver: the user's RelayGuard
receiver (`healthz` returned `ok`). No other worker was running before the run (checked with `tasklist`).
`bin\supervised.log` had 10373 lines before the run and 12860 after it. This run is lines 10374-12860.

Command:

```text
bin/chaos.exe -target relayguard -seeds 1-20 -dup 0.2 -reorder 10 -supervise bin/worker.exe -crash-rate 0.1 -kill-interval 2s -lease 1s -out results/relayguard-crash-lease.md
```

Stdout, verbatim:

```text
target=relayguard seeds=1-20 customers=20 dup=0.20 reorder=10 poison=0 crash-rate=0.10 kill-interval=2s supervise="bin/worker.exe"
seed=1    events=84   deliveries=99   provider_retries=0   gave_up=0   inbox[done=80 stale=4] violations=0 (5.326s)
seed=2    events=83   deliveries=97   provider_retries=0   gave_up=0   inbox[done=80 stale=3] violations=0 (4.737s)
seed=3    events=80   deliveries=95   provider_retries=0   gave_up=0   inbox[done=78 stale=2] violations=0 (5.692s)
seed=4    events=78   deliveries=93   provider_retries=0   gave_up=0   inbox[done=73 stale=5] violations=0 (3.919s)
seed=5    events=78   deliveries=92   provider_retries=0   gave_up=0   inbox[done=74 stale=4] violations=0 (4.434s)
seed=6    events=79   deliveries=91   provider_retries=0   gave_up=0   inbox[done=75 stale=4] violations=0 (3.914s)
seed=7    events=70   deliveries=89   provider_retries=0   gave_up=0   inbox[done=70] violations=0 (6.155s)
seed=8    events=71   deliveries=88   provider_retries=0   gave_up=0   inbox[done=67 stale=4] violations=0 (5.455s)
seed=9    events=81   deliveries=97   provider_retries=0   gave_up=0   inbox[done=74 stale=7] violations=0 (5.052s)
seed=10   events=89   deliveries=109  provider_retries=0   gave_up=0   inbox[done=87 stale=2] violations=0 (5.949s)
seed=11   events=80   deliveries=96   provider_retries=0   gave_up=0   inbox[done=75 stale=5] violations=0 (3.989s)
seed=12   events=85   deliveries=105  provider_retries=0   gave_up=0   inbox[done=81 stale=4] violations=0 (4.957s)
seed=13   events=80   deliveries=95   provider_retries=0   gave_up=0   inbox[done=76 stale=4] violations=0 (3.796s)
seed=14   events=83   deliveries=94   provider_retries=0   gave_up=0   inbox[done=81 stale=2] violations=0 (3.918s)
seed=15   events=89   deliveries=104  provider_retries=0   gave_up=0   inbox[done=88 stale=1] violations=0 (2.92s)
seed=16   events=63   deliveries=73   provider_retries=0   gave_up=0   inbox[done=62 stale=1] violations=0 (5.413s)
seed=17   events=70   deliveries=88   provider_retries=0   gave_up=0   inbox[done=70] violations=0 (3.188s)
seed=18   events=91   deliveries=106  provider_retries=0   gave_up=0   inbox[done=89 stale=2] violations=0 (6.709s)
seed=19   events=68   deliveries=86   provider_retries=0   gave_up=0   inbox[done=63 stale=5] violations=0 (4.948s)
seed=20   events=90   deliveries=108  provider_retries=0   gave_up=0   inbox[done=88 stale=2] violations=0 (6.18s)
---
RESULT target=relayguard seeds=20 failing_seeds=0 violations=0 restarts=273 hard_kills=32
wrote results/relayguard-crash-lease.md
```

RESULT line:

```text
RESULT target=relayguard seeds=20 failing_seeds=0 violations=0 restarts=273 hard_kills=32
```

No Inbox cell contains `dead` or `pending`. In this run's part of `bin\supervised.log`:
0 lines containing "DEAD inbox row", 0 containing "POISON", 241 containing "FAULT: crashing",
265 containing "worker started", 0 containing "panic", 0 containing "error" or "ERROR".

**Provenance note (unexpected).** The results file header reads
`Worker binary: vcs.revision=2c708e68ffec82261c38c1c191b2333a09474c58 vcs.modified=true`.
Before E2, `go version -m bin/worker.exe` showed `vcs.modified=false`. After E3 it shows:

```text
$ go version -m bin/worker.exe | grep vcs
	build	vcs=git
	build	vcs.revision=2c708e68ffec82261c38c1c191b2333a09474c58
	build	vcs.time=2026-11-12T00:10:49Z
	build	vcs.modified=true

$ ls -l --time-style=full-iso bin/*.exe
-rwxr-xr-x 1 adabs 197609 17109504 2026-11-11 19:28:23.213184500 -0500 bin/chaos.exe
-rwxr-xr-x 1 adabs 197609 16251904 2026-11-11 19:28:21.604153200 -0500 bin/naive.exe
-rwxr-xr-x 1 adabs 197609 14869504 2026-11-11 18:31:50.583601000 -0500 bin/worker-phase4.exe
-rwxr-xr-x 1 adabs 197609 14875648 2026-11-11 19:28:20.117892800 -0500 bin/worker.exe
```

`bin/worker.exe`, `bin/naive.exe` and `bin/chaos.exe` were rewritten at 19:28:20-19:28:23 local
(2026-11-12T00:28:20Z-00:28:23Z), which matches the output of `dev.ps1 build`. The experiment-runner did
not run a build, and no project hook builds. At that time the only change to the tree was the
untracked `docs/phase5-results.md`. The first E3 worker started at 19:29:13 local, after the
rebuild, so all of E3 ran on the rebuilt binaries (same commit, `vcs.modified=true`). The E3
numbers are recorded as measured. Whether they count as evidence for the clean `2c708e6`
binary is for the lead to decide.

Determinism check against `results/relayguard-crash.md` (Phase 4), plus the done/stale
comparison. P4 is Phase 4 and P5 is this run. Δ = P5 minus P4:

| Seed | Events P4 | Deliveries P4 | Events P5 | Deliveries P5 | Events/Deliveries | Inbox P4 | Inbox P5 | Δ done | Δ stale |
|---|---|---|---|---|---|---|---|---|---|
| 1 | 84 | 99 | 84 | 99 | match | done=82 stale=2 | done=80 stale=4 | -2 | 2 |
| 2 | 83 | 97 | 83 | 97 | match | done=82 stale=1 | done=80 stale=3 | -2 | 2 |
| 3 | 80 | 95 | 80 | 95 | match | done=79 stale=1 | done=78 stale=2 | -1 | 1 |
| 4 | 78 | 93 | 78 | 93 | match | done=77 stale=1 | done=73 stale=5 | -4 | 4 |
| 5 | 78 | 92 | 78 | 92 | match | done=75 stale=3 | done=74 stale=4 | -1 | 1 |
| 6 | 79 | 91 | 79 | 91 | match | done=77 stale=2 | done=75 stale=4 | -2 | 2 |
| 7 | 70 | 89 | 70 | 89 | match | done=70 stale=0 | done=70 stale=0 | 0 | 0 |
| 8 | 71 | 88 | 71 | 88 | match | done=67 stale=4 | done=67 stale=4 | 0 | 0 |
| 9 | 81 | 97 | 81 | 97 | match | done=78 stale=3 | done=74 stale=7 | -4 | 4 |
| 10 | 89 | 109 | 89 | 109 | match | done=86 stale=3 | done=87 stale=2 | 1 | -1 |
| 11 | 80 | 96 | 80 | 96 | match | done=76 stale=4 | done=75 stale=5 | -1 | 1 |
| 12 | 85 | 105 | 85 | 105 | match | done=84 stale=1 | done=81 stale=4 | -3 | 3 |
| 13 | 80 | 95 | 80 | 95 | match | done=79 stale=1 | done=76 stale=4 | -3 | 3 |
| 14 | 83 | 94 | 83 | 94 | match | done=82 stale=1 | done=81 stale=2 | -1 | 1 |
| 15 | 89 | 104 | 89 | 104 | match | done=89 stale=0 | done=88 stale=1 | -1 | 1 |
| 16 | 63 | 73 | 63 | 73 | match | done=62 stale=1 | done=62 stale=1 | 0 | 0 |
| 17 | 70 | 88 | 70 | 88 | match | done=70 stale=0 | done=70 stale=0 | 0 | 0 |
| 18 | 91 | 106 | 91 | 106 | match | done=90 stale=1 | done=89 stale=2 | -1 | 1 |
| 19 | 68 | 86 | 68 | 86 | match | done=64 stale=4 | done=63 stale=5 | -1 | 1 |
| 20 | 90 | 108 | 90 | 108 | match | done=89 stale=1 | done=88 stale=2 | -1 | 1 |

Totals: Phase 4 done=1558 stale=34, this run done=1531 stale=61. Events and Deliveries match
for all 20 seeds. In every seed, done + stale equals Events in both runs. Stale counts differ
per seed and are not a constant +1. Compared with Phase 4, stale is higher in 15 seeds,
equal in 4 (7, 8, 16, 17) and lower in 1 (10). This is reported as observed: the checker found
0 violations, so the different done/stale split is not a violation by itself.

### E3b, E4, E5

Previous session: not run. It stopped after E3 because of the provenance note above.
Current session: E4 was run (see "E4 (evidence)" above). E3b and E5 were not run. A search of
`docs/` and `.claude/` found no plan that defines them. The previous session's table had the rows
"Crash-only regression" and "Control: phase 4 worker with poison" without labels. Attaching E3b
and E5 to those rows (and E4 to the poison row) is the current runner's inference.

## Limitations

- Only one worker process runs at a time, both in the restart loop and under `-supervise`. The
  fencing check under concurrent workers is not exercised by these runs. The comment above the
  fencing query in `process` (`cmd/worker/main.go`) says "Not directly tested".
- The poison sample is small: 5 seeds, 10 poison events in total.
- Poison is simulated. With `FAULT_POISON=1`, `test.poison` events make the worker exit via
  `os.Exit`. Real poison (panics, hangs, out-of-memory, handler bugs) was not tested.
- The done/stale split varies from run to run under crashes (see the E3 comparison with Phase 4
  and with the superseded run). The cause has not been explained.
- The `DEAD inbox row` log line can undercount dead-lettering (seed 4 of E4). Use the inbox as the
  source of truth, not the log.

## Tradeoffs

- One extra database round trip per event (claim, then process).
- After a crash, an event waits for its lease to expire before it is retried
  (10s by default; the harness uses 1s to keep runs short).
