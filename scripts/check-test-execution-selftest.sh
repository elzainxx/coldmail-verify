#!/usr/bin/env bash
# check-test-execution-selftest — the PAIRED CONTROL for check-test-execution.sh.
#
# WHY. On the real tree that gate reports ok, which means every one of its failure branches is
# unexercised there. A gate whose failure path never runs is a gate nobody has checked: it could be
# broken to accept everything — a typo in a grep, an inverted `if`, a `go list` that matches nothing —
# and the real tree would print ok forever. This repo has shipped exactly that defect twice (the two
# always-skipping append-only tests of audit D13, and the `code-review` placeholder whose comment
# claimed deleting it would fail CI), so the pattern here follows its sibling
# check-test-enumeration-selftest.sh rather than inventing a second convention.
#
# It also closes the hole the gate cannot cover itself: check-test-enumeration.sh rule 5 requires every
# check-*.sh to be named in the workflow, but neither gate can detect its OWN absence. Case 0 below
# fails if either half of this pair is unwired, so the two cover each other.
#
# THIS PAIR EXISTS BECAUSE THE GATE'S FIRST VERSION HAD THE DEFECT IT GUARDS. Break-verification found
# that pointing the workflow at `TM_TEST_DATABASE_URL: ${{ secrets.PGDSN }}` and running with that
# secret empty produced `ok [DEGRADED]` and EXIT 0 while 31 tests did not execute — the control going
# quiet at exactly the moment the property failed. Rule 0b was added for it, and case 7 is that break
# turned into a permanent test. The lesson generalizes: the gate's remaining branches are asserted
# here rather than trusted.
#
# HOW, AND WHY NOT ENV OVERRIDES. Making the real gate's paths overridable would hand anyone a way to
# point a blocking gate at empty fixtures and have it pass having checked nothing. Instead this builds a
# THROWAWAY MODULE with the same relative layout, copies the REAL gate in, and md5-verifies the copy so
# a refactor cannot leave this control exercising a stale file.
#
# CASES — one per branch, plus the accept cases that make the rest meaningful:
#   1. a conformant skeleton (all tests execute)              -> ok [STRICT]
#   2. a TIER-1 test that skips                                -> FAIL, names the test. The D13 shape.
#   3. a skip in the sanctioned gaps package, no phase named    -> FAIL (an exemption, not scaffolding)
#   4. a skip in gaps naming `PENDING P5`                       -> ok. Without this case, a gate broken
#      to reject every skip would pass case 3 and delete the honest scaffolding signal.
#   5. a test that EXECUTES in the gaps package                 -> ok. Tier 2 sanctions skips; it does
#      not require them.
#   6. the workflow gives TM_TEST_DATABASE_URL no value         -> FAIL (rule 0)
#   7. CI markers set + the DSN empty at run time               -> FAIL (rule 0b). The regression case:
#      rule 0 alone passed this, because a `${{ secrets.X }}` reference IS a value in the file.
#   8. no CI markers + the DSN empty (a laptop)                 -> ok [DEGRADED], and the output must
#      say so. The affordance must survive, or a developer without Postgres cannot run the gate — and
#      "the gate is unrunnable locally" is how exemption lists get born.
#   9. a package where tests are declared but produce NO result -> FAIL. Truncation is caught in BOTH
#      tiers, which is what makes case 4 safe: a sanctioned skip cannot hide a truncated run.
#  10. the sanctioned-skip package absent entirely              -> FAIL, not a silent no-op. If the
#      tier-2 boundary vanishes, every skip in the module silently becomes a tier-1 failure; that must
#      be a loud configuration error rather than a behaviour change nobody chose.
#  11. a package with NO test files                             -> ok. The one legitimate zero, and the
#      case that keeps the floor below from being a blanket `declared==0 → fail` (this tree has two
#      such packages).
#  12. a test file that is present but does NOT COMPILE          -> FAIL. Same declared=0 arithmetic as
#      the host fault that exposed all this; the floor must distinguish "nothing to declare" from
#      "could not declare".
#  13. a module where NOTHING declares a test                    -> FAIL, never "ok — 0 of 0 EXECUTED".
#      The per-package floor cannot fire if the loop never runs.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$HERE"

GATE="scripts/check-test-execution.sh"
SELF="scripts/check-test-execution-selftest.sh"
WORKFLOW=".github/workflows/ci.yml"

# --- 0) The pair must both be wired. Neither can assert its own presence. --------------------------
for s in "$GATE" "$SELF"; do
  if ! grep -qF "$s" "$WORKFLOW"; then
    echo "FAIL (test-execution-selftest): $s is not named in $WORKFLOW."
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
  echo "FAIL (test-execution-selftest): the copied gate does not match $GATE — this control would be testing a stale file"
  exit 1
fi

fail=0

# The fixture module. Deliberately dependency-free so it resolves with GOPROXY=off and needs no
# network, no Postgres, and no module cache beyond the standard library.
reset_fixture() {
  rm -rf "$TMP/control-plane"
  mkdir -p "$TMP/control-plane/internal/thing" "$TMP/control-plane/test/gaps"
  cat >"$TMP/control-plane/go.mod" <<'EOF'
module example.test/fixture

go 1.21
EOF
  cat >"$TMP/control-plane/internal/thing/thing_test.go" <<'EOF'
package thing

import "testing"

func TestAlpha(t *testing.T) {}
func TestBeta(t *testing.T)  {}
EOF
  # The gaps package must EXIST in every case except case 10 — the gate resolves its tier-2 boundary
  # from it, so an accidentally-empty one would silently change what the other cases are testing.
  cat >"$TMP/control-plane/test/gaps/gaps_test.go" <<'EOF'
package gaps

import "testing"

func TestGAPScaffold(t *testing.T) {
	t.Skip("PENDING P5: needs the work_items store; harness real-shaped, assertions land in Phase 5.")
}
EOF
  write_workflow
}

# The conformant workflow: a DSN with an actual value, plus both halves of this pair named, since the
# gate's rule 0 reads the file and case 0 above reads it too.
write_workflow() {
  cat >"$TMP/$WORKFLOW" <<'EOF'
jobs:
  control-plane:
    steps:
      - run: go test -v ./...
      - name: test-execution gate
        env:
          TM_TEST_DATABASE_URL: postgres://postgres:ci@localhost:5432/cptest
        run: bash scripts/check-test-execution.sh
      - run: bash scripts/check-test-execution-selftest.sh
EOF
}

# expect <case-name> <want-rc> <substring...>
#
# The gate is invoked with a DUMMY non-empty DSN by default: the fixture's tests touch no database, so
# what is being exercised is the gate's STRICT-vs-DEGRADED branch, not connectivity. Cases 7 and 8
# override the environment themselves.
#
# `env -u GITHUB_ACTIONS -u CI` on every case is load-bearing: this selftest RUNS in CI, where those
# markers are set, so without unsetting them every fixture would inherit rule 0b's CI strictness and
# case 8 could never test the laptop affordance it exists to pin.
expect() {
  local name="$1" want_rc="$2"; shift 2
  local out rc
  set +e
  out="$(cd "$TMP" && env -u GITHUB_ACTIONS -u CI TM_TEST_DATABASE_URL="postgres://fixture/not-connected" GOTOOLCHAIN=local GOFLAGS= bash "$GATE" 2>&1)"
  rc=$?
  set -e
  assert_result "$name" "$rc" "$want_rc" "$out" "$@"
}

# expect_env — same, but the caller supplies the environment. Used by the two cases whose whole subject
# IS the environment.
expect_env() {
  local name="$1" want_rc="$2" envspec="$3"; shift 3
  local out rc
  set +e
  out="$(cd "$TMP" && eval "env $envspec GOTOOLCHAIN=local GOFLAGS= bash \"$GATE\"" 2>&1)"
  rc=$?
  set -e
  assert_result "$name" "$rc" "$want_rc" "$out" "$@"
}

assert_result() {
  local name="$1" rc="$2" want_rc="$3" out="$4"; shift 4
  if [[ "$rc" -ne "$want_rc" ]]; then
    echo "FAIL (test-execution-selftest) [$name]: exit $rc, want $want_rc"
    echo "--- gate output ---"; echo "$out"
    fail=1
    return
  fi
  local want
  for want in "$@"; do
    if ! grep -qF -- "$want" <<<"$out"; then
      echo "FAIL (test-execution-selftest) [$name]: exit code was right ($rc) but the message did not"
      echo "  contain: $want"
      echo "  A gate that fails for the WRONG REASON is not a control. Full output:"
      echo "--- gate output ---"; echo "$out"
      fail=1
      return
    fi
  done
  echo "  ok: $name"
}

echo "test-execution-selftest: exercising the failure branches the real tree never reaches"

# 1. Conformant. Without this, a gate broken to reject EVERYTHING would pass every case below and this
#    script would call that correct.
reset_fixture
expect "a conformant skeleton passes" 0 "test-execution: ok [STRICT]"

# 2. Rule tier-1 — the D13 shape: a test that is declared, compiled, vetted, and never executed.
reset_fixture
cat >>"$TMP/control-plane/internal/thing/thing_test.go" <<'EOF'

func TestGammaAlwaysSkips(t *testing.T) {
	t.Skip("pretending an environment precondition was missing")
}
EOF
expect "a tier-1 skip fails and is named" 1 \
  "EXECUTED 2 OF 3 DECLARED TESTS" "TestGammaAlwaysSkips"

# 3. Tier 2 is a sanctioned exception, not a blanket exemption.
reset_fixture
cat >>"$TMP/control-plane/test/gaps/gaps_test.go" <<'EOF'

func TestGAPNoPhase(t *testing.T) {
	t.Skip("not doing this one for now")
}
EOF
expect "a gaps skip naming no phase fails" 1 "do not name a discharging phase"

# 4. The accept case that keeps case 3 from being satisfied by a gate that rejects all skips. Deleting
#    the honest scaffolding signal would be a REGRESSION, not extra strictness.
reset_fixture
cat >>"$TMP/control-plane/test/gaps/gaps_test.go" <<'EOF'

func TestGAPSecondScaffold(t *testing.T) {
	t.Skip("PENDING P7: needs the classifier; truth table real-shaped.")
}
EOF
expect "a gaps skip naming a phase passes" 0 "test-execution: ok [STRICT]"

# 5. Tier 2 SANCTIONS skips; it does not require them. A gate that demanded a skip there would fail the
#    day a GAP stub is filled in — which is the day it is supposed to start passing.
reset_fixture
cat >>"$TMP/control-plane/test/gaps/gaps_test.go" <<'EOF'

func TestGAPNowImplemented(t *testing.T) {}
EOF
expect "a gaps test that EXECUTES passes" 0 "test-execution: ok [STRICT]"

# 6. Rule 0 — the workflow stops giving the DSN a value. A bare key is the empty-secret shape, and it
#    must fail even though the ENVIRONMENT here has a perfectly good DSN in it.
reset_fixture
cat >"$TMP/$WORKFLOW" <<'EOF'
jobs:
  control-plane:
    steps:
      - run: go test -v ./...
      - name: test-execution gate
        env:
          TM_TEST_DATABASE_URL:
        run: bash scripts/check-test-execution.sh
      - run: bash scripts/check-test-execution-selftest.sh
EOF
expect "a workflow with no DSN value fails rule 0" 1 "does not give TM_TEST_DATABASE_URL a value"

# 7. Rule 0b — THE REGRESSION CASE. A secret reference satisfies rule 0 (it IS a value in the file) and
#    expands to "" at run time. With rule 0 alone this configuration returned `ok [DEGRADED]`, exit 0,
#    with 31 real tests unexecuted.
reset_fixture
cat >"$TMP/$WORKFLOW" <<'EOF'
jobs:
  control-plane:
    steps:
      - run: go test -v ./...
      - name: test-execution gate
        env:
          TM_TEST_DATABASE_URL: ${{ secrets.PGDSN }}
        run: bash scripts/check-test-execution.sh
      - run: bash scripts/check-test-execution-selftest.sh
EOF
expect_env "an emptied secret in CI fails rule 0b" 1 \
  "-u TM_TEST_DATABASE_URL GITHUB_ACTIONS=true" \
  "running in CI with TM_TEST_DATABASE_URL EMPTY"

# 8. The laptop affordance, and the assertion that it cannot be MISTAKEN for a strict run. Both halves
#    matter: a degraded run that printed the same success line as a strict one would be worse than no
#    degraded mode at all.
reset_fixture
expect_env "no DSN outside CI degrades, and says so" 0 \
  "-u TM_TEST_DATABASE_URL -u GITHUB_ACTIONS -u CI" \
  "ok [DEGRADED]" "was NOT asserted"

# 9. Truncation, caught in the SANCTIONED package — the case that keeps tier 2 from becoming a hole.
#    A panic kills the test binary, so TestGAPTruncated never produces a result line at all: declared=3,
#    one SKIP, one FAIL, and one test simply absent from the output. That is the timeout shape without
#    needing to wait for a timeout, and it is why the truncation branch sits BEFORE the tier split.
#
#    NOT done with a `TestMain` that exits early, which was the first attempt: `go test -list` RUNS
#    TestMain, so that fixture declares ZERO tests and the gate correctly goes inert — the case would
#    have passed while testing nothing, which is the defect this whole file is about.
reset_fixture
cat >>"$TMP/control-plane/test/gaps/gaps_test.go" <<'EOF'

func TestGAPPanics(t *testing.T)   { panic("simulating a truncated run") }
func TestGAPTruncated(t *testing.T) {}
EOF
expect "a truncated run in the sanctioned package still fails" 1 "produced NO RESULT AT ALL"

# 10. The derived tier-2 boundary must fail LOUDLY when absent, never degrade to "no sanctioned skips."
reset_fixture
rm -rf "$TMP/control-plane/test"
expect "the sanctioned-skip package missing fails loudly" 1 "sanctioned-skip package"

# --- THE NONZERO FLOOR (cases 11-13, added 2026-08-07) --------------------------------------------
#
# WHY THESE EXIST, AND WHY THIS FILE COULD NOT SEE THE DEFECT. Cases 1-10 above all stage fixtures
# that DECLARE tests, so every one of them exercises the gate on a nonzero roster. The gate's central
# comparison is `executed < declared`, and on the single input declared=0 that comparison is
# VACUOUSLY TRUE-BY-EQUALITY: 0 of 0 matches, nothing looks missing, and the old `continue` at the top
# of the loop removed the package from the report entirely. So the one input the gate cannot judge was
# also the one input this control never supplied.
#
# It was found the way these things get found — by accident, from the OUTSIDE. Run on the Windows host,
# cases 2, 3 and 9 returned `exit 0, want 1`, and the failure output showed the just-appended fixture
# package ABSENT from the gate's report while a DIFFERENT package was named. The host had silently not
# executed the freshly-linked test binary (not a hang, not an error — no run), so `go test -list`
# returned nothing and the package fell through the `continue`. Three of this file's ten branches
# passed by omission, which is exactly the class of defect it was written to catch, occurring inside
# its own fixtures.
#
# NOTE ON WHAT CASE 12 CAN AND CANNOT STAGE. A selftest cannot reproduce "the toolchain refused to run
# the binary" on a WORKING toolchain — that is an environment fault, not a fixture. What it CAN stage
# is the other producer of the same declared=0 signal: a test file that does not COMPILE. Both reach
# the identical arithmetic, so the floor is what is being tested, not the cause. Recorded plainly
# because a case that claims to reproduce the host fault would be a false claim about its own reach.
#
# 11. THE ACCEPT CASE, and it is load-bearing. A package with NO test files legitimately declares
#     zero, and this tree has two of them (cmd/control-plane, internal/verdict). Without this case a
#     floor implemented as a blanket `declared -eq 0 → fail` would pass cases 12 and 13 and be wrong
#     on the real tree — the floor would go from missing a defect to inventing one.
reset_fixture
mkdir -p "$TMP/control-plane/internal/notests"
cat >"$TMP/control-plane/internal/notests/lib.go" <<'EOF'
package notests

func Add(a, b int) int { return a + b }
EOF
expect "a package with NO test files is a legitimate zero" 0 \
  "test-execution: ok [STRICT]" "no test files — nothing to execute"

# 12. A test file that is PRESENT but does not compile. Same declared=0 arithmetic as the host fault,
#     and the case that proves the floor distinguishes "nothing to declare" from "could not declare".
reset_fixture
mkdir -p "$TMP/control-plane/internal/broken"
cat >"$TMP/control-plane/internal/broken/lib.go" <<'EOF'
package broken

func Add(a, b int) int { return a + b }
EOF
cat >"$TMP/control-plane/internal/broken/lib_test.go" <<'EOF'
package broken

import "testing"

func TestBroken(t *testing.T) { thisSymbolDoesNotExist() }
EOF
expect "a test file that does not COMPILE fails the floor" 1 \
  "DECLARED ZERO TESTS" "test files present"

# 13. THE MODULE-WIDE FLOOR. The per-package floor cannot fire if the loop never runs, so a package set
#     in which nothing declares a test must fail rather than print "ok — 0 of 0 declared EXECUTED".
#     Staged by removing every test file: `go list ./...` still succeeds and still returns packages, so
#     none of the gate's earlier guards fire and this is genuinely the last line of defence.
reset_fixture
rm -f "$TMP/control-plane/internal/thing/thing_test.go" "$TMP/control-plane/test/gaps/gaps_test.go"
cat >"$TMP/control-plane/test/gaps/lib.go" <<'EOF'
package gaps
EOF
cat >"$TMP/control-plane/internal/thing/lib.go" <<'EOF'
package thing
EOF
expect "a module that declares ZERO tests in total fails" 1 \
  "the module declared ZERO tests in total"

if [[ "$fail" -eq 0 ]]; then
  echo "test-execution-selftest: ok — all 13 branches of $GATE behave as documented"
  echo "  Cases 11-13 pin the NONZERO FLOOR, added after three of cases 1-10 passed BY OMISSION on a"
  echo "  host that silently did not execute freshly-built test binaries: declared=0 made the gate's"
  echo "  own comparison vacuously true and dropped the package from its report."
fi
exit "$fail"
