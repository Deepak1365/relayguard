---
description: Run the RelayGuard team loop for a roadmap item until it is done (plan, build, test, review, experiment, record)
argument-hint: [optional focus, e.g. "finish phase 5"]
---
Act as the team lead for RelayGuard. Focus: $ARGUMENTS (if empty, the first unchecked item in docs/ROADMAP.md).

## Stop and wait for me ONLY at these checkpoints
A. After the planner's plan: show it and wait for my approval.
B. Before git commit or git push: show a summary, `git status`, and a proposed commit message.
C. Before anything irreversible or listed in CLAUDE.md rule 6.
D. When genuinely blocked: information only I have, a decision that changes the scope, or the same problem
   still failing after 3 fix attempts.

Everywhere else, KEEP GOING. Do not end your turn just to report progress. For choices with a sensible default,
pick the option that best follows CLAUDE.md, note the choice in the final summary, and continue.

## Loop (after the plan is approved)
1. implementer implements the next part of the plan.
2. Build and test: `dev.ps1 test`, `dev.ps1 build`, and `go vet ./...`. If you can read IDE diagnostics,
   check them too. Any failure or warning in new code goes back to the implementer; repeat step 2.
3. reviewer reviews the code. Blocking items go back to the implementer, then step 2 again.
   Up to 4 review rounds; after that, checkpoint D.
4. experiment-runner runs the planned experiments.
   - Results as expected: continue.
   - Unexpected result (any RelayGuard violation or a mismatch with the plan): do NOT just rerun until it passes.
     Keep the failing output in the results doc, have the implementer find the root cause and fix it, go back to
     step 2, then rerun the FULL experiment set. The results doc shows both runs as "bug found by the harness".
     If the root cause is unclear after 2 attempts: checkpoint D.
5. reviewer checks the results doc against the raw outputs; fix the doc if needed.
6. interview-coach updates docs/interview-notes.md.
7. Tick the finished items in docs/ROADMAP.md.
8. Checkpoint B.
