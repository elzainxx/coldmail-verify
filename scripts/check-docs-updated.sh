#!/usr/bin/env bash
# Docs-updated staleness gate (PROJECT_RULES.md §8): a BEHAVIOR change (code under control-plane/
# or verifier-node/) that does NOT also touch a doc/ADR is a FAILING check, not a nag.
#
# Compares the changed file set and fails if it contains code changes but no doc/ADR/spec/
# PROJECT_RULES change. Config-only, test-only, and CI/script-only diffs are exempt (they don't
# change externally-observable behavior on their own). Intentionally language-agnostic so CI runs
# it anywhere.
#
# Usage:
#   check-docs-updated.sh [BASE_REF]
#     BASE_REF given      -> RANGE mode: judge BASE_REF...HEAD, plus anything uncommitted on top.
#     no BASE_REF, remote -> RANGE mode against origin/main (the whole branch, deliberately).
#     no BASE_REF, none   -> WORKTREE mode: judge ONLY what is uncommitted. See below.
#
# ============================================================================================
# THE VERDICT USED TO BE INHERITED. Fixed 2026-08-07 (N2). MEASURED, not suspected.
#
# The old fallback chain was `origin/main`, then `HEAD~1`, and the changed set was the range
# `$base...HEAD` UNIONED with the working tree. This repo has NO remote, so every local run
# silently landed on HEAD~1 and dragged the PREVIOUS COMMIT'S diff into the verdict for the
# change in front of you. Measured on a clone, HEAD~1 = d481c97 (which touched
# Docs/api-contract.md), working tree = one .go behaviour file and no doc:
#
#     docs-updated: ok (code_changed=true docs_changed=true).   rc=0
#
# The gate's whole purpose is to fail exactly that input. `docs_changed=true` was TRUE — it just
# was not true OF THIS CHANGE. A union of two questions answers neither: the range supplied the
# doc, the working tree supplied the code, and no single change ever satisfied the rule. That
# green had been reported for weeks and was reported for the commit that added it.
#
# TWO MORE HOLES FOUND WHILE MEASURING THE FIRST, both also confirmed by running them:
#
#   * AN UNRESOLVABLE BASE REF WENT QUIET. `git diff --name-only "$base"...HEAD 2>/dev/null ||
#     ... || true` swallowed the error, the changed set came back empty, and an empty set is
#     reported as "no changes — ok", exit 0. A bad `--base` argument, or a PR base sha missing
#     from a shallow checkout, therefore turned a blocking gate into a no-op that announced
#     success. `2>/dev/null || true` on the ONE command that produces the gate's subject is how a
#     gate stops having a subject.
#
#   * AN UNTRACKED FILE WAS INVISIBLE. `git diff` does not report untracked paths, so a WHOLE NEW
#     behaviour surface — `control-plane/internal/api/brand_new_surface.go`, never added — did not
#     appear in the changed set at all. Measured: `git status` showed `?? ...brand_new_surface.go`
#     while `git diff --name-only HEAD` printed nothing. That is the worst direction for this
#     particular gate: a brand-new endpoint or handler is precisely the change most likely to need
#     a doc, and it was the one shape the gate could not see.
#
# WHY MODE IS PRINTED. WORKTREE mode judges what you are ABOUT to commit; RANGE mode judges what
# you DID. CI passes a base explicitly (ci.yml:155 — the PR base sha, or HEAD~1 on a push), so the
# commit is still judged there; that is the enforcement point and it is unchanged by this fix. What
# is no longer allowed is the two silently MIXING. And a clean tree in WORKTREE mode now says it
# judged NO COMMIT, because "nothing to check" and "everything checked out" must never print the
# same verdict.
# ============================================================================================

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$HERE"

# --- Resolve the subject -----------------------------------------------------------------------
base="${1:-}"
mode=""
if [ -n "$base" ]; then
  mode="RANGE"
elif git rev-parse --verify -q origin/main >/dev/null; then
  base="origin/main"
  mode="RANGE"
else
  mode="WORKTREE"
fi

# A base ref that does not resolve is a LOUD configuration error. It must never degrade to an empty
# diff: an empty diff is reported as success, so the failure mode of a typo'd or unfetched ref was a
# gate that passed having examined nothing.
if [ "$mode" = "RANGE" ] && ! git rev-parse --verify -q "$base^{commit}" >/dev/null; then
  echo "FAIL (docs-updated): base ref '$base' does not resolve to a commit." >&2
  echo "  This is fatal on purpose. The previous version swallowed the error and continued with an" >&2
  echo "  EMPTY changed set, which this gate reports as 'no changes — ok' with exit 0 — a blocking" >&2
  echo "  gate silently turned into a no-op that announced success." >&2
  echo "  In CI this usually means the base commit is not in the checkout: actions/checkout needs" >&2
  echo "  fetch-depth: 0 (ci.yml sets it) for the PR base sha to be reachable." >&2
  exit 1
fi

# Uncommitted work, in all three forms. `git diff` alone misses the third.
worktree="$(git diff --name-only HEAD)"
staged="$(git diff --cached --name-only)"
untracked="$(git ls-files --others --exclude-standard)"   # respects .gitignore

if [ "$mode" = "RANGE" ]; then
  # No `|| true`: if this fails the gate must not proceed on a partial subject.
  ranged="$(git diff --name-only "$base...HEAD")"
  changed="$ranged"$'\n'"$worktree"$'\n'"$staged"$'\n'"$untracked"
  subject="$base...HEAD plus uncommitted work"
else
  # The fix for N2: NO commit range. The previous commit is not part of this question.
  ranged=""
  changed="$worktree"$'\n'"$staged"$'\n'"$untracked"
  subject="uncommitted work only (no base ref, no origin/main)"
fi
changed="$(printf '%s\n' "$changed" | sed '/^$/d' | sort -u)"

if [ -z "$changed" ]; then
  if [ "$mode" = "WORKTREE" ]; then
    echo "docs-updated: nothing to judge — the working tree, the index and the untracked set are all"
    echo "  empty, and NO COMMIT WAS EXAMINED. This is not a statement about HEAD: in WORKTREE mode"
    echo "  the last commit is out of scope by design (it is judged in CI, where a base ref is passed"
    echo "  explicitly). Run with a base ref to judge a commit range from here."
    exit 0
  fi
  echo "docs-updated: no changes in $subject — ok."
  exit 0
fi

# Does the subject contain a BEHAVIOR change? (source under control-plane/ or verifier-node/,
# excluding tests, config, and the fork's own docs which are provenance not behavior.)
code_changed=false
docs_changed=false
while IFS= read -r f; do
  case "$f" in
    # Behavior surfaces:
    control-plane/*.go|control-plane/**/*.go)
      case "$f" in
        *_test.go) : ;;                                # tests are not behavior
        control-plane/internal/config/*) : ;;          # config surface is exempt (dials, not behavior)
        *) code_changed=true ;;
      esac ;;
    verifier-node/truemail-rack/app/*|verifier-node/truemail-rack/config/*|verifier-node/truemail-rack/lib/*)
      code_changed=true ;;
    # Doc surfaces:
    *.md|Docs/*|Docs/**/*|PROJECT_RULES.md|verifier-node/truemail-rack/FORK.md)
      docs_changed=true ;;
  esac
done <<< "$changed"

if $code_changed && ! $docs_changed; then
  echo "FAIL (docs-updated): behavior changed under control-plane/ or verifier-node/ but no doc/ADR/spec was touched." >&2
  echo "Subject: $subject" >&2
  echo "Changed files:" >&2
  printf '%s\n' "$changed" | sed 's/^/  - /' >&2
  echo "Fix: update the relevant Docs/ spec or add/append an ADR in the SAME change (PROJECT_RULES.md §8)." >&2
  exit 1
fi

echo "docs-updated: ok [$mode] (code_changed=$code_changed docs_changed=$docs_changed)."
echo "  Subject: $subject"
