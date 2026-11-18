# 0002: Use replay_count as a generation in the worker's fence token (ABA protection)

Status: Accepted (Phase 6, step 5a, commit 425f62d)
Date: 2026-11-12 (steps 4, 5a, 5b approved by the user, docs/phase6-plan.md:128)

## Context
Phase 5 fences a worker's write with `attempts`: the claim bumps `attempts` and the process transaction
only proceeds if the row still carries the value that claim saw. Admin replay (step 5b) resets `attempts` to 0
so a dead or stale event can run again. That reopens an ABA hole: a worker that claimed the event with
`attempts = 1` and stalled can wake after a replay and a second claim have brought `attempts` back to 1, and
pass the fence of the newer claim (cmd/worker/main.go:130-133; migrations/00005_replay.sql:1-6).

## Options considered
1. **Do not reset attempts on replay.** Not recorded in the plan as a weighed option; I list it because it
   avoids the hole. Con: the dead-letter limit (8 attempts) would apply to the replayed run with the old count
   already spent, so a replay would be dead on arrival. The plan resets attempts (plan :236).
2. **Fence on a timestamp or a random claim id.** Not in the plan. Con: needs another column and still has to be
   compared in every fenced statement; a counter that only grows is simpler to reason about.
3. **Add a monotonic `replay_count` to inbox and fence on (replay_count, attempts) (chosen).** Pro: replay_count
   only grows, so the pair never repeats. It doubles as `change_log.generation`, which lets the checker and
   undo tell apply rows of different runs apart (plan :165-171). Con: one more column and one more predicate in
   the claim, fence and dead-letter statements.

## Decision
- Migration 00005 adds `inbox.replay_count INTEGER NOT NULL DEFAULT 0` with CHECK `>= 0`, plus CHECK
  constraints on change_log generation and seq (new file; 00004 not edited).
- The claim returns `claimToken{attempts, generation}`. The fence is `WHERE id = $1 AND status = 'pending' AND
  attempts = $2 AND replay_count = $3`; the dead-letter statement also includes replay_count
  (cmd/worker/main.go:134-137, :159-169, :224-233).
- `change_log.generation` is the replay_count at the fence. Replay only starts from dead or stale events,
  which have no apply rows, so older generations are always empty (plan :169-170).
- Same commit: claim and fence switch from FOR UPDATE to FOR NO KEY UPDATE. Reason: change_log and
  admin_actions have FKs to inbox, so an admin transaction holds FOR KEY SHARE on the inbox row, and a
  FOR UPDATE fence would queue behind it (plan :96, :166). The lock stays exclusive between claim, fence and
  replay.

## Consequences
- Tests: TestDBReplayFencesOldClaim (mutation: drop replay_count from the fence fails it),
  TestDBDeadLetterFencedByGeneration, TestDBKeyShareDoesNotBlockClaimOrFence (mutations: claim/fence back to
  FOR UPDATE fail it) (plan :173-175, :195-199).
- Side effect that needed re-checking: with a weaker lock the preview no longer queues behind the worker, so it
  overlaps an apply in progress. That is bounded by lock_timeout and by ErrChanged (see 0005); the plan states
  the absence of deadlock there rests on reasoning, not on a test (plan :200-212).
- Evidence: R2 (poison, seeds 1-5) ended `RESULT target=relayguard seeds=5 failing_seeds=0 violations=0
  restarts=161 hard_kills=20` (docs/phase6-results.md:114). The chaos runs in docs/phase6-results.md do not
  replay events (the results doc says so for E4, docs/phase6-results.md:343-349), so the ABA protection is proven by the targeted
  tests above, not by a chaos experiment.
- Not measured: multiple concurrent workers. The tests simulate a stalled claim with a second connection;
  no multi-worker experiment has been run.
- Costs and limits: the worker built before this migration (bin/worker-phase4.exe) ignores replay_count and
  must not be used once replays exist (CLAUDE.md, Handoff). The "ABA" claim is about a stalled claim holder, which is
  the only fence bypass found; the plan did not enumerate others.
- Revisit: if another admin action ever resets attempts or status, it must bump the generation too.
