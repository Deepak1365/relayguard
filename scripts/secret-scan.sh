#!/usr/bin/env bash
# Secret scan for RelayGuard. Used by CI (.github/workflows/ci.yml) and runnable
# locally: bash scripts/secret-scan.sh [repo-dir]
#
# Fails (exit 1) if any tracked file:
#   1. is a .env file (.env, .env.local, path/.env, ...); .env.example is allowed;
#   2. contains a Stripe webhook secret: whsec_ followed by 20+ token characters.
#      Placeholders like whsec_replace_me and whsec_test_secret are shorter and pass.
# Scans tracked files only (what a push would publish), not history.
set -u
cd "${1:-.}" || exit 2

fail=0

env_files=$(git ls-files | grep -E '(^|/)\.env(\.[^/]*)?$' | grep -vE '(^|/)\.env\.example$')
if [ -n "$env_files" ]; then
  echo "secret-scan: committed .env file(s) found:"
  echo "$env_files" | sed 's/^/  /'
  fail=1
fi

secrets=$(git grep -n -I -E 'whsec_[A-Za-z0-9+/=_-]{20,}')
if [ -n "$secrets" ]; then
  echo "secret-scan: possible Stripe webhook secret(s) found (values not shown):"
  echo "$secrets" | cut -d: -f1,2 | sed 's/^/  /'
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  echo "secret-scan: FAILED"
  exit 1
fi
echo "secret-scan: OK (no committed .env files, no whsec_ tokens)"
