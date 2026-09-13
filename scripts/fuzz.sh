#!/usr/bin/env bash
# Runs every Fuzz* target in the module for the given time each.
set -euo pipefail
fuzztime=${1:-30s}
found=0
while IFS= read -r pkg; do
  for target in $(go test -list '^Fuzz' "$pkg" | grep '^Fuzz' || true); do
    found=1
    echo "== $pkg $target"
    go test -run '^$' -fuzz "^${target}\$" -fuzztime "$fuzztime" "$pkg"
  done
done < <(go list ./...)
[[ $found == 1 ]] || echo "no fuzz targets yet"
