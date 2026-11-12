---
description: Audit the RelayGuard setup (project health, Claude Code config, guardrail enforcement, rule compliance). Changes nothing.
---
Run a full audit of this project and its Claude Code setup. Do NOT change, create or delete any files,
and do not kill any processes. Ask me before anything that needs my input.

Report a table: # | Check | PASS / FAIL / SKIP | Evidence (the command you ran and the key output).
Then list every FAIL with a suggested fix, give an overall verdict, and wait. Fix nothing without my approval.

## A. Project health
1. `dev.ps1 test`, `go vet ./...`, and `gofmt -l .` (gofmt must list no files).
2. `dev.ps1 build` succeeds.
3. `dev.ps1 migrate-status`: every migration applied.
4. CI: the latest GitHub Actions run on main is green (use `gh run list -L 1` if gh is installed; otherwise SKIP and ask me to check the Actions tab).
5. Ask me whether the RelayGuard receiver is running. If yes, run
   `bin/chaos.exe -target relayguard -seeds 1-3 -dup 0.2 -reorder 10 -supervise bin/worker.exe -crash-rate 0.1 -kill-interval 2s`
   and expect 0 violations. If no, SKIP.

## B. Claude Code configuration
6. .claude/settings.json is valid JSON. Report the number of allow / ask / deny rules and any hooks.
7. The deny list contains: Read(./.env), Edit(./.env), the dev.ps1 reset rule, docker compose down -v, git push --force / -f,
   git reset --hard, git clean, and the two canary rules from section C.
8. Every ALLOW rule is read-only, build or test. FAIL any allow rule that could delete files, kill processes,
   run docker, push, or touch files outside the repo.
9. The five subagents and the /next-phase, /verify and /audit commands are available.

## C. Enforcement, tested with harmless canaries only
Never test with the real destructive commands, and never try to read the real .env.
10. Run `echo relayguard-deny-canary`. It must be BLOCKED by the deny rule `Bash(echo relayguard-deny-canary:*)`.
11. Try to read docs/deny-canary.txt. It must be BLOCKED by `Read(./docs/deny-canary.txt)`.
    If you can read its content, report FAIL loudly: the Read deny mechanism is not working.
12. If a hook guards .env in shell commands, run `echo .env-hook-canary`. It must be BLOCKED by the hook. Otherwise SKIP and say no hook exists.

## D. Rule compliance, checked from git and files
13. `git ls-files .env` prints nothing.
14. Secret scan of all history, printing ONLY a count, never the matches:
    `git log -p --all | grep -cE "whsec_[A-Za-z0-9]{16,}"` must be 0.
15. Naive baseline frozen: `git log --oneline -- cmd/naive` and the diffs show only the original code plus fault-injection points.
16. Applied migrations never edited: each migrations/*.sql file has exactly one commit in `git log --oneline -- <file>`.
17. Results provenance: every results/*.md file referenced from docs/ has a commit stamp WITHOUT -dirty, and that
    commit exists in `git log`. List any exceptions.
18. Numbers: every RelayGuard and naive number in the summary tables of docs/phase*-results.md matches the
    referenced results/*.md file or the pasted raw output. List each mismatch.
19. CLAUDE.md "Status" and docs/ROADMAP.md checkboxes agree with git history.
20. Leftovers: `tasklist` for worker.exe, naive.exe, worker-phase4.exe and chaos.exe, and `git worktree list`.
    Report anything still running or left over (do not kill or remove it).
