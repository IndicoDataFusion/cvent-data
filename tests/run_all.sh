#!/usr/bin/env bash
# run_all.sh — run every test_*.sh in this folder, summarize results
set -uo pipefail
cd "$(dirname "$0")"
shopt -s nullglob
total=0; failed=0
for t in test_*.sh; do
  echo "──────────────────────────────────────────"
  if bash "$t"; then
    total=$((total+1))
  else
    failed=$((failed+1)); total=$((total+1))
  fi
done
echo "──────────────────────────────────────────"
echo "tests: $total, failed: $failed"
[ "$failed" -eq 0 ]
