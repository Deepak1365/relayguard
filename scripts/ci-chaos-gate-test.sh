#!/usr/bin/env bash
# Self-test for scripts/ci-chaos-gate.sh on canned logs (scripts/testdata/ci-gate,
# all SYNTHETIC). A gate that cannot fail proves nothing, so every FAIL case is
# asserted too. Exits non-zero if any expectation is wrong.
set -u
cd "$(dirname "$0")/.." || exit 2
gate=scripts/ci-chaos-gate.sh
d=scripts/testdata/ci-gate
bad=0

# expect <pass|fail|usage> <description> <gate args...>  (usage = exit 2)
expect() {
  want=$1 desc=$2; shift 2
  out=$(bash "$gate" "$@" 2>&1); rc=$?
  if [ "$want" = pass ] && [ "$rc" -eq 0 ]; then
    echo "ok   $desc"
  elif [ "$want" = fail ] && [ "$rc" -eq 1 ]; then
    echo "ok   $desc (rejected: ${out##*FAIL})"
  elif [ "$want" = usage ] && [ "$rc" -eq 2 ]; then
    echo "ok   $desc (usage error: $out)"
  else
    echo "BAD  $desc: wanted $want, gate exit $rc: $out"; bad=1
  fi
}

for f in "$d"/*.txt; do
  head -n 1 "$f" | grep -q '^SYNTHETIC' || { echo "BAD  $f lacks the SYNTHETIC first line"; bad=1; }
done

expect pass "clean run passes"                      clean 3 0 "$d/clean-pass.txt"
expect fail "violations under clean"                clean 3 1 "$d/clean-violations.txt"
expect fail "violations in the log even with exit 0" clean 3 0 "$d/clean-violations.txt"
expect fail "clean log but exit 1"                  clean 3 1 "$d/clean-pass.txt"
expect fail "seeds mismatch"                        clean 3 0 "$d/seeds-mismatch.txt"
expect fail "two RESULT lines"                      clean 3 0 "$d/two-results.txt"
expect fail "no RESULT, exit 0"                     clean 3 0 "$d/fatal-no-result.txt"
expect fail "missing RESULT with exit 1 (log.Fatal) under violations" violations 3 1 "$d/fatal-no-result.txt"
expect fail "naive with violations=0"               violations 3 1 "$d/naive-zero.txt"
expect pass "valid naive control"                   violations 3 1 "$d/naive-valid.txt"
expect fail "violations mode with exit 2"           violations 3 2 "$d/naive-valid.txt"
expect fail "violations mode with exit 0"           violations 3 0 "$d/naive-valid.txt"
expect fail "relayguard RESULT under violations mode" violations 3 1 "$d/clean-pass.txt"
expect fail "missing log file"                      clean 3 0 "$d/does-not-exist.txt"
expect fail "duplicate violations= field (last would win)" clean 3 0 "$d/dup-field.txt"
expect usage "non-numeric exit_code"                clean 3 x "$d/clean-pass.txt"
expect usage "non-numeric expected_seeds"           clean three 0 "$d/clean-pass.txt"
expect usage "expected_seeds 0"                     clean 0 0 "$d/clean-pass.txt"
expect usage "unknown mode"                         dirty 3 0 "$d/clean-pass.txt"
expect usage "wrong argument count"                 clean 3 0

if [ "$bad" -ne 0 ]; then echo "gate self-test FAILED"; exit 1; fi
echo "gate self-test passed"
