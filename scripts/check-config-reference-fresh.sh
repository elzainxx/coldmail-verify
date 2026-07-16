#!/usr/bin/env bash
# Config-reference freshness gate: regenerate Docs/config-reference.md from the manifest
# and fail if it differs from what's checked in. Keeps "what's toggleable" from drifting.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$HERE/Docs/config-reference.md"

[ -f "$OUT" ] || { echo "FAIL: $OUT missing — run scripts/gen-config-reference.sh" >&2; exit 1; }

before="$(cat "$OUT")"
bash "$HERE/scripts/gen-config-reference.sh" >/dev/null
after="$(cat "$OUT")"

if [ "$before" != "$after" ]; then
  echo "FAIL (config-reference-fresh): Docs/config-reference.md is stale vs config-manifest.md." >&2
  echo "Fix: run scripts/gen-config-reference.sh and commit the result." >&2
  # Restore the committed version so the check itself doesn't leave a dirty tree in CI.
  printf '%s' "$before" > "$OUT"
  exit 1
fi
echo "config-reference-fresh: ok."
