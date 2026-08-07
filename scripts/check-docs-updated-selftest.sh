#!/usr/bin/env bash
# check-docs-updated-selftest — the PAIRED CONTROL for check-docs-updated.sh.
#
# WHY IT EXISTS AT ALL, WHICH IS THE WHOLE POINT. check-docs-updated had been printing
# `ok (code_changed=true docs_changed=true)` for weeks, INCLUDING on changes it was written to
# reject. Its verdict came from a union of two different questions: the commit range `HEAD~1...HEAD`
# supplied `docs_changed`, the working tree supplied `code_changed`, and no single change ever had to
# satisfy the rule. Measured on a clone with the working tree holding one .go behaviour file and no
# doc at all: exit 0, "docs_changed=true". Two more holes surfaced while measuring that one — an
# unresolvable base ref degrading to an empty diff reported as success, and untracked files being
# invisible to `git diff` so a brand-new behaviour surface could not be seen.
#
# Three holes in an eighty-line script, none of which any amount of reading had found, all three
# trivially visible the moment the property was made false and the gate re-run. That is what this
# file automates: every case below makes the property false in one specific way and asserts the gate
# goes red FOR THE RIGHT REASON, plus the accept cases without which a gate broken to reject
# everything would pass the reject cases and be called correct.
#
# HOW, AND WHY NOT ENV OVERRIDES. Making the real gate's paths overridable would hand anyone a way
# to point a blocking gate at empty fixtures. Instead this builds a THROWAWAY GIT REPOSITORY with
# the same relative layout, copies the REAL gate in, and md5-verifies the copy so a refactor cannot
# leave this control exercising a stale file. Same convention as the two existing selftest pairs.
#
# THE FIXTURE'S LAST COMMIT DELIBERATELY TOUCHES A DOC. That is the exact precondition that made N2
# invisible, so every WORKTREE case below is simultaneously an inheritance test: if the gate ever
# starts consulting HEAD~1 again, the reject cases go green and this file goes red.
#
# CASES:
#   0. both halves of the pair are named in the workflow (neither can assert its own presence)
#   1. code + doc, uncommitted                       -> ok. The accept case that makes 2-4 mean something.
#   2. code only, uncommitted                        -> FAIL. THE N2 CASE: HEAD~1 touched a doc.
#   3. an UNTRACKED behaviour file, no doc            -> FAIL. Invisible to `git diff` before the fix.
#   4. an untracked behaviour file + an untracked doc -> ok. Keeps case 3 from being "untracked = fail".
#   5. STAGED code only                               -> FAIL. The index is part of the subject.
#   6. a test-only change                             -> ok. Tests are not behavior.
#   7. a config-only change                           -> ok. Dials are not behavior.
#   8. a doc-only change                              -> ok. Nothing to be stale against.
#   9. RANGE mode, range has code + doc               -> ok, and says [RANGE].
#  10. RANGE mode, range has code only                -> FAIL. CI's enforcement point still bites.
#  11. an UNRESOLVABLE base ref, dirty tree           -> FAIL, loudly, naming the ref.
#  11b. an UNRESOLVABLE base ref, CLEAN tree           -> FAIL. CI's shape: the empty range was the
#      whole subject, so this printed "no changes — ok", exit 0 — success on an unexamined commit.
#  12. a clean tree in WORKTREE mode                  -> exit 0, but it must SAY no commit was
#      examined. "nothing to check" and "everything checked out" must not print the same verdict;
#      a bare "ok" here is what let a scoping change look like a passing gate.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$HERE"

GATE="scripts/check-docs-updated.sh"
SELF="scripts/check-docs-updated-selftest.sh"
WORKFLOW=".github/workflows/ci.yml"

# --- 0) The pair must both be wired. Neither can assert its own presence. --------------------------
for s in "$GATE" "$SELF"; do
  if ! grep -qF "$s" "$WORKFLOW"; then
    echo "FAIL (docs-updated-selftest): $s is not named in $WORKFLOW."
    echo "  A gate cannot detect its own absence from CI — that is this control's job, and the reason"
    echo "  the two are wired as a pair in the same commit."
    exit 1
  fi
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/scripts" "$TMP/.github/workflows"
cp "$HERE/$GATE" "$TMP/$GATE"
if [[ "$(md5sum <"$HERE/$GATE" | cut -d' ' -f1)" != "$(md5sum <"$TMP/$GATE" | cut -d' ' -f1)" ]]; then
  echo "FAIL (docs-updated-selftest): the copied gate does not match $GATE — this control would be testing a stale file"
  exit 1
fi
cp "$HERE/$WORKFLOW" "$TMP/$WORKFLOW"

fail=0

# The fixture repository. NO REMOTE — that is what puts the real gate on its no-upstream path, which
# is the path N2 lived on. Two commits, and the SECOND one touches a doc: see the header.
reset_fixture() {
  rm -rf "$TMP/repo"
  mkdir -p "$TMP/repo/scripts" "$TMP/repo/.github/workflows" \
           "$TMP/repo/control-plane/internal/api" "$TMP/repo/control-plane/internal/config" \
           "$TMP/repo/Docs"
  cp "$TMP/$GATE" "$TMP/repo/$GATE"
  cp "$TMP/$WORKFLOW" "$TMP/repo/$WORKFLOW"
  cat >"$TMP/repo/control-plane/internal/api/api.go" <<'EOF'
package api

func Serve() string { return "v1" }
EOF
  cat >"$TMP/repo/control-plane/internal/api/api_test.go" <<'EOF'
package api

import "testing"

func TestServe(t *testing.T) { _ = Serve() }
EOF
  cat >"$TMP/repo/control-plane/internal/config/config.go" <<'EOF'
package config

var Timeout = 30
EOF
  cat >"$TMP/repo/Docs/spec.md" <<'EOF'
# Spec

The front door answers /v1.
EOF
  (
    cd "$TMP/repo"
    git init -q .
    git config user.email selftest@example.invalid
    git config user.name  selftest
    git config commit.gpgsign false
    git add -A
    git commit -qm "initial"
    # COMMIT 2 TOUCHES A DOC AND CODE TOGETHER. This is the inheritance precondition.
    printf '\n// second commit\nfunc Health() bool { return true }\n' >>control-plane/internal/api/api.go
    printf '\nHealth is documented here.\n' >>Docs/spec.md
    git add -A
    git commit -qm "second: code and doc together"
  )
}

# run <base-ref-or-empty> -> sets $out and $rc
run_gate() {
  local baseref="${1:-}"
  set +e
  if [[ -n "$baseref" ]]; then
    out="$(cd "$TMP/repo" && bash "$GATE" "$baseref" 2>&1)"
  else
    out="$(cd "$TMP/repo" && bash "$GATE" 2>&1)"
  fi
  rc=$?
  set -e
}

# expect <case-name> <want-rc> <base-ref-or-""> <substring...>
expect() {
  local name="$1" want_rc="$2" baseref="$3"; shift 3
  local out rc
  run_gate "$baseref"
  assert_result "$name" "$rc" "$want_rc" "$out" "$@"
}

assert_result() {
  local name="$1" rc="$2" want_rc="$3" out="$4"; shift 4
  if [[ "$rc" -ne "$want_rc" ]]; then
    echo "FAIL (docs-updated-selftest) [$name]: exit $rc, want $want_rc"
    echo "--- gate output ---"; echo "$out"
    fail=1
    return
  fi
  local want
  for want in "$@"; do
    if ! grep -qF -- "$want" <<<"$out"; then
      echo "FAIL (docs-updated-selftest) [$name]: exit code was right ($rc) but the message did not"
      echo "  contain: $want"
      echo "  A gate that fails for the WRONG REASON is not a control. Full output:"
      echo "--- gate output ---"; echo "$out"
      fail=1
      return
    fi
  done
  echo "  ok: $name"
}

echo "docs-updated-selftest: exercising the branches the real tree never reaches"

# 1. THE ACCEPT CASE. Without it, a gate broken to reject everything passes cases 2, 3, 5, 10 and 11
#    and this file would call that correct.
reset_fixture
printf '\nfunc Ping() bool { return true }\n' >>"$TMP/repo/control-plane/internal/api/api.go"
printf '\nPing is documented.\n' >>"$TMP/repo/Docs/spec.md"
expect "code plus a doc passes" 0 "" "docs-updated: ok [WORKTREE]" "code_changed=true docs_changed=true"

# 2. THE N2 CASE, and the reason this whole file exists. The working tree holds a behaviour change and
#    NO doc. HEAD~1 in this fixture touched Docs/spec.md, so any gate that consults the commit range
#    will report docs_changed=true and pass — which is precisely what the real gate did, for weeks, on
#    the real repo, including for the commit that introduced it.
reset_fixture
printf '\nfunc Ping() bool { return true }\n' >>"$TMP/repo/control-plane/internal/api/api.go"
expect "code with NO doc fails, and does not inherit HEAD~1's doc" 1 "" \
  "FAIL (docs-updated)" "uncommitted work only" "control-plane/internal/api/api.go"

# 3. UNTRACKED. `git diff` does not list untracked paths, so before the fix a whole new behaviour
#    surface was absent from the changed set entirely — the shape MOST likely to need a doc was the
#    one shape the gate could not see.
reset_fixture
cat >"$TMP/repo/control-plane/internal/api/brand_new_surface.go" <<'EOF'
package api

func BrandNewSurface() string { return "shipped" }
EOF
expect "an UNTRACKED behaviour file with no doc fails" 1 "" \
  "FAIL (docs-updated)" "brand_new_surface.go"

# 4. The accept half of case 3. A fix implemented as "untracked files are suspicious" would pass case
#    3 for the wrong reason and then reject legitimate new-file-plus-new-doc work, which is how a gate
#    earns an exemption list.
reset_fixture
cat >"$TMP/repo/control-plane/internal/api/brand_new_surface.go" <<'EOF'
package api

func BrandNewSurface() string { return "shipped" }
EOF
cat >"$TMP/repo/Docs/brand-new-surface.md" <<'EOF'
# BrandNewSurface

Documented in the same change.
EOF
expect "an untracked behaviour file WITH an untracked doc passes" 0 "" "docs-updated: ok [WORKTREE]"

# 5. The index. A change that is `git add`ed but not committed is still the change in front of you.
reset_fixture
printf '\nfunc Ping() bool { return true }\n' >>"$TMP/repo/control-plane/internal/api/api.go"
(cd "$TMP/repo" && git add control-plane/internal/api/api.go)
expect "STAGED code with no doc fails" 1 "" "FAIL (docs-updated)" "api.go"

# 6-8. The exemptions, asserted rather than assumed. Each is a case where the gate MUST stay quiet;
#      without them, a gate tightened until everything fails would look like it was getting stricter.
reset_fixture
printf '\nfunc TestPing(t *testing.T) {}\n' >>"$TMP/repo/control-plane/internal/api/api_test.go"
expect "a test-only change is not behavior" 0 "" "code_changed=false"

reset_fixture
printf '\nvar Retries = 3\n' >>"$TMP/repo/control-plane/internal/config/config.go"
expect "a config-only change is not behavior" 0 "" "code_changed=false"

reset_fixture
printf '\nA clarification.\n' >>"$TMP/repo/Docs/spec.md"
expect "a doc-only change passes" 0 "" "code_changed=false docs_changed=true"

# 9-10. RANGE mode. This is CI's path (ci.yml passes the PR base sha, or HEAD~1 on a push), so the
#       commit-judging behaviour must be PRESERVED by the N2 fix, not removed by it. Case 10 is the
#       proof that scoping WORKTREE mode down did not disarm the enforcement point.
reset_fixture
expect "RANGE mode accepts a commit that touched code AND a doc" 0 "HEAD~1" \
  "docs-updated: ok [RANGE]" "HEAD~1...HEAD"

reset_fixture
(cd "$TMP/repo" && printf '\nfunc Undocumented() {}\n' >>control-plane/internal/api/api.go \
  && git add -A && git commit -qm "code with no doc")
expect "RANGE mode rejects a commit that touched code and no doc" 1 "HEAD~1" \
  "FAIL (docs-updated)" "HEAD~1...HEAD"

# 11. THE QUIET NO-OP. `git diff --name-only "$base"...HEAD 2>/dev/null || ... || true` turned an
#     unresolvable ref into an EMPTY changed set, and an empty set is reported as success. So a typo'd
#     base, or a PR base sha missing from a shallow checkout, made a blocking gate announce ok having
#     examined nothing. Asserted with a behaviour change present, so a gate that merely stayed silent
#     would be caught by the exit code too.
reset_fixture
printf '\nfunc Ping() bool { return true }\n' >>"$TMP/repo/control-plane/internal/api/api.go"
expect "an unresolvable base ref fails loudly instead of reporting no changes" 1 \
  "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" \
  "does not resolve to a commit" "fetch-depth: 0"

# 11b. THE SAME HOLE IN CI'S SHAPE, and the case that actually reproduces the quiet no-op. Case 11
#      above stages an uncommitted change, so with the guard removed the gate still fails — on the
#      WRONG REASON (the working-tree change), which is why case 11's message assertion is what
#      detects the regression there rather than its exit code. Break-verification made that visible:
#      removing the guard turned case 11 into "exit code was right (1) but the message did not
#      contain: does not resolve to a commit".
#
#      CI, however, runs on a CLEAN checkout and passes a base sha (ci.yml). There the empty range was
#      the ENTIRE subject, so an unfetched or typo'd base produced literally "docs-updated: no changes
#      — ok", exit 0: the gate reporting success on a commit it never looked at. That is the shape
#      worth pinning, and it needs a clean tree to stage.
reset_fixture
expect "an unresolvable base ref on a CLEAN tree fails instead of 'no changes — ok'" 1 \
  "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" \
  "does not resolve to a commit"

# 12. A clean tree must not print a verdict it did not earn. Exit 0 is right — there is nothing to
#     judge — but the message has to say NO COMMIT WAS EXAMINED, or the developer who scoped the gate
#     down reads a bare "ok" as the gate agreeing with them.
reset_fixture
expect "a clean tree says explicitly that no commit was examined" 0 "" \
  "nothing to judge" "NO COMMIT WAS EXAMINED"

if [[ "$fail" -eq 0 ]]; then
  echo "docs-updated-selftest: ok — all 13 cases of $GATE behave as documented"
  echo "  Cases 2, 3 and 11/11b are the three MEASURED false greens (N2): the inherited verdict, the"
  echo "  invisible untracked file, and the unresolvable base ref that reported success. Case 10"
  echo "  is the counterweight — it fails if the N2 fix ever disarms CI's commit-range enforcement."
  echo "  11b exists because break-verification showed 11 alone caught the regression only via its"
  echo "  MESSAGE: with a dirty tree the gate still exits 1, for the wrong reason. A clean tree is"
  echo "  what makes the empty range the entire subject, which is CI's actual shape."
fi
exit "$fail"
