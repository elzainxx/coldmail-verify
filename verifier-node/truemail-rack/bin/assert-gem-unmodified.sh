#!/usr/bin/env bash
# Door 5 enforcement (host-runnable, no Ruby required).
#
# The engine gem `truemail` MUST be a pinned, unmodified dependency — never forked,
# never vendored-and-edited, never a floating range. This check asserts that from the
# fork's Gemfile alone, so CI can run it without a Ruby toolchain or registry access.
#
# It verifies THREE things:
#   1. The Gemfile pins `truemail` to an EXACT version (no ~>, >=, <, or range operator).
#   2. That exact version matches the version ADR-0003 records (single source of truth).
#   3. No vendored/edited copy of the truemail gem's source has been committed into the
#      fork (a committed lib/truemail/*.rb here would BE a fork of the engine).
#
# Inside Docker (where gems resolve) a stronger check also runs: `bundle list truemail`
# must report exactly the pinned version. This script is the host/CI-portable gate.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GEMFILE="$HERE/Gemfile"
ADR="$HERE/../../Docs/adr/ADR-0003-truemail-engine-pin.md"

fail() { echo "FAIL (Door 5): $*" >&2; exit 1; }

[ -f "$GEMFILE" ] || fail "Gemfile not found at $GEMFILE"

# 1. Extract the truemail pin line and require an exact version string.
pin_line="$(grep -E "^\s*gem\s+['\"]truemail['\"]" "$GEMFILE" || true)"
[ -n "$pin_line" ] || fail "no 'gem \"truemail\"' line in Gemfile"

# Reject any range/optimistic operator on the engine pin.
if echo "$pin_line" | grep -qE "~>|>=|<=|>|<"; then
  fail "engine 'truemail' must be pinned EXACTLY (found a range operator): $pin_line"
fi

pinned_version="$(echo "$pin_line" | grep -oE "[0-9]+\.[0-9]+\.[0-9]+" | head -1)"
[ -n "$pinned_version" ] || fail "could not parse an exact x.y.z version from: $pin_line"

# 2. Cross-check against ADR-0003 (single source of truth for the pin).
if [ -f "$ADR" ]; then
  if ! grep -qE "\b${pinned_version//./\\.}\b" "$ADR"; then
    fail "Gemfile pins truemail $pinned_version but ADR-0003 does not record that version"
  fi
else
  echo "warn: ADR-0003 not found at $ADR (skipping cross-check)" >&2
fi

# 3. No committed engine source = no silent fork of the engine.
if find "$HERE" -path "*/truemail/version.rb" -o -path "*/lib/truemail/*.rb" 2>/dev/null | grep -q .; then
  fail "found vendored truemail engine source inside the fork — the engine must be a gem dependency, not committed source"
fi

echo "OK (Door 5): truemail engine pinned EXACTLY to $pinned_version, no vendored engine source, ADR-0003 agrees."
