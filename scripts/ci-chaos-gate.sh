#!/usr/bin/env bash
# CI gate for chaos runs. Usage:
#   bash scripts/ci-chaos-gate.sh <clean|violations> <expected_seeds> <exit_code> <log>
#
# Why a gate script and not just the chaos exit code: chaos exits 1 both for
# "violations found" and for log.Fatal (bad flag, missing secret, DB down), so
# exit 1 alone cannot prove the checker saw anything. The RESULT line (printed
# only after every seed ran) is the proof; this script requires both.
#
#   clean:      exit 0 AND exactly one "RESULT target=relayguard" line with
#               seeds=<n> failing_seeds=0 violations=0.
#   violations: exit 1 AND exactly one "RESULT target=naive" line with
#               seeds=<n> and violations >= 1 (negative control: proves the
#               checker still detects the naive receiver's flaws).
#
# Reads only the given log file (chaos console output), never CI's own log.
set -euo pipefail

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <clean|violations> <expected_seeds> <exit_code> <log>" >&2
  exit 2
fi
mode=$1 expected=$2 code=$3 log=$4

fail() { echo "GATE FAIL ($mode): $*"; exit 1; }

case "$mode" in
  clean) target=relayguard ;;
  violations) target=naive ;;
  *) echo "unknown mode '$mode' (want clean or violations)" >&2; exit 2 ;;
esac
case "$expected" in ''|*[!0-9]*) echo "expected_seeds must be a number, got '$expected'" >&2; exit 2 ;; esac
[ "$expected" -ge 1 ] || { echo "expected_seeds must be >= 1, got '$expected'" >&2; exit 2; }
case "$code" in ''|*[!0-9]*) echo "exit_code must be a number, got '$code'" >&2; exit 2 ;; esac
[ -f "$log" ] || fail "log file $log not found"

want_code=0
[ "$mode" = violations ] && want_code=1
[ "$code" -eq "$want_code" ] || fail "chaos exit code was $code, expected $want_code"

# Match the line start and the target as a whole word, so target=naive2 or a
# RESULT quoted mid-line would not count.
lines=$(grep -E "^RESULT target=${target}( |\$)" "$log" || true)
count=0
[ -n "$lines" ] && count=$(printf '%s\n' "$lines" | wc -l | tr -d ' ')
[ "$count" -eq 1 ] || fail "expected exactly one 'RESULT target=$target' line in $log, found $count"

# Parse key=value fields by name; field order and extra fields do not matter.
# A repeated key fails: otherwise "violations=5 ... violations=0" would pass
# because the last value wins. read -a splits without glob expansion.
seeds="" failing="" violations_n=""
read -r -a fields <<< "$lines"
for field in "${fields[@]}"; do
  case "$field" in
    seeds=*) [ -z "$seeds" ] || fail "duplicate seeds= field: $lines"
      seeds=${field#seeds=} ;;
    failing_seeds=*) [ -z "$failing" ] || fail "duplicate failing_seeds= field: $lines"
      failing=${field#failing_seeds=} ;;
    violations=*) [ -z "$violations_n" ] || fail "duplicate violations= field: $lines"
      violations_n=${field#violations=} ;;
  esac
done
isnum() { case "$1" in ''|*[!0-9]*) return 1 ;; *) return 0 ;; esac; }
isnum "$seeds" || fail "RESULT line has no numeric seeds= field: $lines"
isnum "$failing" || fail "RESULT line has no numeric failing_seeds= field: $lines"
isnum "$violations_n" || fail "RESULT line has no numeric violations= field: $lines"
[ "$seeds" -eq "$expected" ] || fail "seeds=$seeds, expected $expected (a run that skipped seeds proves less)"

if [ "$mode" = clean ]; then
  [ "$failing" -eq 0 ] || fail "failing_seeds=$failing, expected 0"
  [ "$violations_n" -eq 0 ] || fail "violations=$violations_n, expected 0"
  echo "GATE PASS (clean): exit 0, seeds=$seeds failing_seeds=0 violations=0"
else
  [ "$violations_n" -ge 1 ] || fail "naive control found violations=$violations_n; the checker may be blind"
  echo "GATE PASS (violations): exit 1, naive seeds=$seeds violations=$violations_n (checker still sees the flaws)"
fi
