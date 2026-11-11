---
description: Quick health check of RelayGuard (tests, build, short chaos regression)
---
Run a quick verification and report results without changing any code:
1. powershell.exe -NoProfile -ExecutionPolicy Bypass -File dev.ps1 test
2. powershell.exe -NoProfile -ExecutionPolicy Bypass -File dev.ps1 build
3. If the RelayGuard receiver is reachable on 127.0.0.1:8080 (check /healthz, and confirm with me that it is the
   RelayGuard one), run: bin/chaos.exe -target relayguard -seeds 1-3 -dup 0.2 -reorder 10 -supervise bin/worker.exe -crash-rate 0.1 -kill-interval 2s
4. Summarise: tests, build, the RESULT line, and anything unexpected. Do not write results files.
