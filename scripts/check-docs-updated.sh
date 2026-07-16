#!/usr/bin/env bash
# Docs-updated staleness gate (PROJECT_RULES.md §8): a BEHAVIOR change (code under brain/
# or box/) that does NOT also touch a doc/ADR is a FAILING check, not a nag.
#
# Compares the changed file set (against a base ref, or the working tree) and fails if it
# contains code changes but no doc/ADR/spec/PROJECT_RULES change. Config-only, test-only,
# and CI/script-only diffs are exempt (they don't change externally-observable behavior on
# their own). This is intentionally simple + language-agnostic so CI runs it anywhere.
#
# Usage:
#   check-docs-updated.sh [BASE_REF]      # BASE_REF defaults to origin/main, then HEAD~1
#   (in CI, pass the PR base; locally, run with no args against the last commit)

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$HERE"

base="${1:-}"
if [ -z "$base" ]; then
  if git rev-parse --verify -q origin/main >/dev/null; then base="origin/main"
  elif git rev-parse --verify -q HEAD~1 >/dev/null; then base="HEAD~1"
  else
    echo "docs-updated: no base ref (initial commit) — skipping." ; exit 0
  fi
fi

changed="$(git diff --name-only "$base"...HEAD 2>/dev/null || git diff --name-only "$base" 2>/dev/null || true)"
# Include staged + unstaged for local runs.
changed="$changed"$'\n'"$(git diff --name-only HEAD 2>/dev/null || true)"$'\n'"$(git diff --cached --name-only 2>/dev/null || true)"
changed="$(echo "$changed" | sed '/^$/d' | sort -u)"

[ -z "$changed" ] && { echo "docs-updated: no changes — ok."; exit 0; }

# Does the diff contain a BEHAVIOR change? (source under brain/ or box/, excluding tests,
# config, and the fork's own docs which are provenance not behavior.)
code_changed=false
docs_changed=false
while IFS= read -r f; do
  case "$f" in
    # Behavior surfaces:
    brain/*.go|brain/**/*.go)
      case "$f" in
        *_test.go) : ;;                       # tests are not behavior
        brain/internal/config/*) : ;;          # config surface is exempt (dials, not behavior)
        *) code_changed=true ;;
      esac ;;
    box/truemail-rack/app/*|box/truemail-rack/config/*|box/truemail-rack/lib/*)
      code_changed=true ;;
    # Doc surfaces:
    *.md|Docs/*|Docs/**/*|PROJECT_RULES.md|box/truemail-rack/FORK.md)
      docs_changed=true ;;
  esac
done <<< "$changed"

if $code_changed && ! $docs_changed; then
  echo "FAIL (docs-updated): behavior changed under brain/ or box/ but no doc/ADR/spec was touched." >&2
  echo "Changed files:" >&2
  echo "$changed" | sed 's/^/  - /' >&2
  echo "Fix: update the relevant Docs/ spec or add/append an ADR in the SAME change (PROJECT_RULES.md §8)." >&2
  exit 1
fi

echo "docs-updated: ok (code_changed=$code_changed docs_changed=$docs_changed)."
