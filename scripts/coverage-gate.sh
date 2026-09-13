#!/usr/bin/env bash
# Fails when a gated package's statement coverage is below its floor.
# Gates live in scripts/coverage-gates.txt as "<package path> <percent>".
set -euo pipefail
profile=${1:?usage: coverage-gate.sh cover.out}
gates="$(dirname "$0")/coverage-gates.txt"
module=$(go list -m)
fail=0
while read -r pkg floor; do
  [[ -z "$pkg" || "$pkg" == \#* ]] && continue
  pct=$(awk -v p="$module/$pkg/" '
    NR > 1 {
      split($1, a, ":"); file = a[1]
      dir = file; sub(/[^\/]+$/, "", dir)
      if (dir == p) { total += $2; if ($3 > 0) covered += $2 }
    }
    END { if (total == 0) print "none"; else printf "%.1f", 100 * covered / total }' "$profile")
  if [[ "$pct" == none ]]; then
    echo "FAIL $pkg: no coverage data (package missing or untested)"; fail=1; continue
  fi
  if awk -v a="$pct" -v b="$floor" 'BEGIN { exit !(a < b) }'; then
    echo "FAIL $pkg: $pct% < $floor%"; fail=1
  else
    echo "ok   $pkg: $pct% (floor $floor%)"
  fi
done < "$gates"
exit $fail
