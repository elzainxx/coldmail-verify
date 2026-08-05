#!/usr/bin/env bash
# check-test-enumeration-selftest — the PAIRED CONTROL for check-test-enumeration.sh.
#
# WHY. On the real tree that gate reports ok, which means every one of its failure branches is
# unexercised there. A gate whose failure path never runs is a gate nobody has checked: it could be
# broken to accept everything — a typo in a grep, a `find` that matches nothing, an `if` inverted —
# and the real tree would still print ok on every run, forever. That is the same vacuity class as the
# two always-skipping append-only tests this repo already shipped (D13), and the same remedy.
#
# It also closes the one hole the gate structurally cannot cover: its rule 5 checks that check
# scripts are wired, but it cannot detect its OWN absence from the workflow. This control fails if
# either script is missing from CI, so the pair covers each other.
#
# HOW, AND WHY NOT ENV OVERRIDES. Making the real gate's paths overridable by environment variables
# would hand anyone a way to point a blocking gate at empty fixtures and have it pass having checked
# nothing. Instead this builds a THROWAWAY REPO SKELETON with the same relative layout, copies the
# REAL gate in, and md5-verifies the copy so a future refactor cannot leave this control exercising a
# stale file.
#
# CASES — one per rule, plus the accept case that makes the rest meaningful. Each asserts an EXIT
# CODE and a MESSAGE, because a gate that fails for the WRONG REASON is not a control:
#   1. a fully-conformant skeleton                     -> ok
#   2. CI names Go packages instead of ./...           -> FAIL (rule 1)
#   3. a second go.mod                                 -> FAIL (rule 2), names it
#   4. a _test.go behind //go:build                    -> FAIL (rule 3), names it
#   5. a _test.go with the older `// +build` form       -> FAIL (rule 3) — both syntaxes, since Go
#      honours the legacy form too and a gate that missed it would be trivially bypassed
#   6. .rspec --pattern                                -> FAIL (rule 4)
#   7. spec_helper config.pattern =                     -> FAIL (rule 4), the layer below .rspec
#   8. an unwired check-*.sh                            -> FAIL (rule 5), names it
#   9. //go:build appearing DEEP in a test file          -> ok. Constraints are header-only; flagging a
#      match anywhere would fail on test DATA or a comment quoting one, and someone would then add an
#      exemption list — which is how three config keys in module 3 became permanently exempt.
#  10. gen-config-reference.sh unwired                   -> ok. It is the GENERATOR, exercised in CI
#      through check-config-reference-fresh.sh, which is itself enumerated. This case pins that the
#      check-*.sh glob is intentional and not an oversight to be "tidied" into scripts/*.sh later.
#  11. hand-listed packages WITH a comment mentioning the glob -> FAIL (rule 1). A regression case,
#      not a hypothetical: rule 1 originally matched the glob string anywhere in the workflow, and
#      ci.yml's own comment about this gate contains it, so the first break-verification on the real
#      tree came back GREEN. See the anchor comment in the gate.
#  12. a gate step DELETED with a comment naming its path left behind -> FAIL (rule 5). The SAME
#      defect as case 11, in the neighbouring rule, found 2026-08-05 — and found only because rule 5
#      was break-tested instead of trusted: the gate's own comment asserted rule 5 "had the anchor
#      from the start," which was false. Deleting check-no-secret-leak.sh's step on the real tree and
#      leaving a `# TODO: re-enable bash scripts/check-no-secret-leak.sh` behind left the gate GREEN.
#  13. `go test ./...` inside a `run: |` BLOCK SCALAR -> ok (rule 1). Load-bearing: the first fix for
#      case 11 was a regex anchored to the `run:` LINE, which would have REJECTED this correct
#      workflow. A gate that fails on valid input is how exemption lists get born.
#  14. a check script named only in a step NAME -> FAIL (rule 5). Narrows the fix: the rule is "a run
#      command invokes it," not "the text survives comment-stripping." A `name:` is prose too.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$HERE"

GATE="scripts/check-test-enumeration.sh"
SELF="scripts/check-test-enumeration-selftest.sh"
WORKFLOW=".github/workflows/ci.yml"

# --- 0) The pair must both be wired. The gate cannot assert its own presence. ---------------------
for s in "$GATE" "$SELF"; do
  if ! grep -qF "$s" "$WORKFLOW"; then
    echo "FAIL (test-enumeration-selftest): $s is not named in $WORKFLOW."
    echo "  The gate cannot detect its own absence from CI — that is this check's job, and the reason"
    echo "  the two scripts are wired as a pair in the same commit."
    exit 1
  fi
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/scripts" "$TMP/.github/workflows"
cp "$HERE/$GATE" "$TMP/$GATE"
if [[ "$(md5sum <"$HERE/$GATE" | cut -d' ' -f1)" != "$(md5sum <"$TMP/$GATE" | cut -d' ' -f1)" ]]; then
  echo "FAIL (test-enumeration-selftest): the copied gate does not match $GATE — this control would be testing a stale file"
  exit 1
fi

fail=0

# reset_fixture — a minimal skeleton the real gate should call ok: one module, one globbing CI step,
# one untagged test file, an unnarrowed .rspec, and one check script named in the workflow.
reset_fixture() {
  rm -rf "$TMP/control-plane" "$TMP/verifier-node" "$TMP/extra"
  # Remove any orphan check script a previous case dropped in. Without this the cases LEAK and a
  # later case fails for an EARLIER rule while claiming to test its own — a control reporting the
  # wrong reason, which is the exact thing the message assertions exist to catch. (It happened in
  # module 3's equivalent on the first run.)
  find "$TMP/scripts" -name '*.sh' ! -name "$(basename "$GATE")" -delete
  mkdir -p "$TMP/control-plane/internal/store" "$TMP/verifier-node/truemail-rack/spec"
  : >"$TMP/control-plane/go.mod"
  cat >"$TMP/control-plane/internal/store/store_test.go" <<'EOF'
package store

import "testing"

func TestNothing(t *testing.T) {}
EOF
  : >"$TMP/verifier-node/truemail-rack/spec/thing_spec.rb"
  cat >"$TMP/verifier-node/truemail-rack/.rspec" <<'EOF'
--require spec_helper
--format documentation
EOF
  cat >"$TMP/verifier-node/truemail-rack/spec/spec_helper.rb" <<'EOF'
RSpec.configure do |config|
  config.disable_monkey_patching!
end
EOF
  : >"$TMP/scripts/check-thing.sh"
  cat >"$TMP/$WORKFLOW" <<'EOF'
jobs:
  control-plane:
    defaults:
      run:
        working-directory: control-plane
    steps:
      - run: go test -v ./...
      - run: bash scripts/check-thing.sh
      - run: bash scripts/check-test-enumeration.sh
      - run: bash scripts/check-test-enumeration-selftest.sh
EOF
}

# expect <case-name> <want-rc> <substring...>  (every substring must appear)
expect() {
  local name="$1" want_rc="$2"; shift 2
  local out rc
  set +e
  out="$(cd "$TMP" && bash "$GATE" 2>&1)"
  rc=$?
  set -e
  if [[ "$rc" -ne "$want_rc" ]]; then
    echo "FAIL (test-enumeration-selftest) [$name]: exit $rc, want $want_rc"
    echo "--- gate output ---"; echo "$out"
    fail=1
    return
  fi
  local want
  for want in "$@"; do
    if ! grep -qF -- "$want" <<<"$out"; then
      echo "FAIL (test-enumeration-selftest) [$name]: exit code was right ($rc) but the message did not"
      echo "  contain: $want"
      echo "  A gate that fails for the WRONG REASON is not a control. Full output:"
      echo "--- gate output ---"; echo "$out"
      fail=1
      return
    fi
  done
  echo "  ok: $name"
}

echo "test-enumeration-selftest: exercising the failure branches the real tree never reaches"

# 1. Conformant. Without this case, a gate broken to reject EVERYTHING would pass every case below
#    and this script would call that correct.
reset_fixture
expect "a conformant skeleton passes" 0 "test-enumeration: ok"

# 2. Rule 1 — CI hand-lists packages instead of globbing. This is M1 acquiring M2/M3's exposure.
reset_fixture
cat >"$TMP/$WORKFLOW" <<'EOF'
jobs:
  control-plane:
    steps:
      - run: go test -v ./internal/store ./internal/httpapi
      - run: bash scripts/check-thing.sh
      - run: bash scripts/check-test-enumeration.sh
      - run: bash scripts/check-test-enumeration-selftest.sh
EOF
expect "CI naming packages instead of ./... fails" 1 \
  "no longer runs 'go test ./...'"

# 3. Rule 2 — a second module. `go test ./...` walks one module; the other's tests vanish silently.
reset_fixture
mkdir -p "$TMP/extra"
: >"$TMP/extra/go.mod"
expect "a second go.mod fails" 1 "expected exactly 1 go.mod, found 2" "extra/go.mod"

# 4. Rule 3 — the modern build-constraint form.
reset_fixture
cat >"$TMP/control-plane/internal/store/tagged_test.go" <<'EOF'
//go:build integration

package store

import "testing"

func TestTagged(t *testing.T) {}
EOF
expect "a //go:build test file fails" 1 \
  "carry a build constraint" "control-plane/internal/store/tagged_test.go"

# 5. Rule 3 — the LEGACY form. Go still honours `// +build`, so a gate that only knew the modern
#    syntax would be bypassed by one line of older-style code.
reset_fixture
cat >"$TMP/control-plane/internal/store/legacy_test.go" <<'EOF'
// +build integration

package store

import "testing"

func TestLegacy(t *testing.T) {}
EOF
expect "the legacy // +build form also fails" 1 \
  "carry a build constraint" "control-plane/internal/store/legacy_test.go"

# 6. Rule 4 — .rspec narrowing.
reset_fixture
cat >>"$TMP/verifier-node/truemail-rack/.rspec" <<'EOF'
--pattern spec/unit/**/*_spec.rb
EOF
expect "an .rspec --pattern fails" 1 "narrows RSpec's file discovery" "--pattern"

# 7. Rule 4 — the spec_helper layer, which an .rspec-only check would miss entirely.
reset_fixture
cat >"$TMP/verifier-node/truemail-rack/spec/spec_helper.rb" <<'EOF'
RSpec.configure do |config|
  config.pattern = "spec/unit/**/*_spec.rb"
end
EOF
expect "a spec_helper config.pattern fails" 1 \
  "narrows RSpec's file discovery" "spec_helper.rb"

# 8. Rule 5 — an unwired gate. Module 3's config_consumption_selftest defect.
reset_fixture
: >"$TMP/scripts/check-orphan.sh"
expect "an unwired check script fails" 1 "named nowhere in" "scripts/check-orphan.sh"

# 9. A //go:build string DEEP in a file is NOT a constraint — Go only honours it in the header.
#    Without this case the gate would flag test data and comments, someone would add an exemption
#    list, and the exemption would become permanent. Load-bearing accept case.
reset_fixture
cat >"$TMP/control-plane/internal/store/deep_test.go" <<'EOF'
package store

import "testing"

// A test that asserts on constraint parsing would legitimately contain the string below.
const sample = `//go:build integration`

func TestDeep(t *testing.T) { _ = sample }
EOF
expect "a //go:build string below the header is correctly ignored" 0 "test-enumeration: ok"

# 10. The check-*.sh glob is intentional: a GENERATOR is not a gate, and it is exercised in CI
#     through its checker. Pins the scope so a later "tidy this to scripts/*.sh" cannot land quietly.
reset_fixture
: >"$TMP/scripts/gen-thing.sh"
expect "an unwired GENERATOR is correctly ignored" 0 "test-enumeration: ok"

# 11. REGRESSION, found by break-verifying rule 1 on the real tree. The first version of rule 1
#     grepped for `go test ./...` anywhere in the workflow — and ci.yml's own comment describing this
#     gate contains that string. So replacing the real command with a hand-listed package subset left
#     the gate GREEN: it was satisfied by prose ABOUT itself. This case reproduces that exactly — the
#     command is hand-listed AND a comment mentions the glob — so the anchor to a `run:` line cannot
#     be dropped again.
reset_fixture
cat >"$TMP/$WORKFLOW" <<'EOF'
jobs:
  control-plane:
    steps:
      # This job used to run `go test ./...`; the note survives, the command does not.
      - run: go test -v ./internal/store
      - run: bash scripts/check-thing.sh
      - run: bash scripts/check-test-enumeration.sh
      - run: bash scripts/check-test-enumeration-selftest.sh
EOF
expect "a COMMENT mentioning go test ./... does not satisfy rule 1" 1 \
  "no longer runs 'go test ./...'"

# --- RULE 5 HAS THE SAME DEFECT (cases 12-14, added 2026-08-05) ----------------------------------
# Case 11 fixed rule 1 and the gate then claimed, in a comment, that rule 5 "had the anchor from the
# start." It did not — rule 5 grepped for the script's PATH anywhere in the workflow. Break-verified
# on the real tree: deleting the check-no-secret-leak.sh step and leaving
# `# TODO: re-enable bash scripts/check-no-secret-leak.sh — the canary boot is slow on the runner`
# behind left the gate GREEN, reporting "all 5 check script(s) wired" about a workflow running four.
# The unverified claim of immunity is what would have kept anyone from re-checking, in a file whose
# whole subject is that such claims are worthless.
#
# 12. The defect. The orphan gate is REAL and on disk; only its step is gone.
reset_fixture
: >"$TMP/scripts/check-disabled.sh"
cat >"$TMP/$WORKFLOW" <<'EOF'
jobs:
  control-plane:
    steps:
      - run: go test -v ./...
      - run: bash scripts/check-thing.sh
      # TODO: re-enable bash scripts/check-disabled.sh — the canary boot is slow on the runner
      - run: bash scripts/check-test-enumeration.sh
      - run: bash scripts/check-test-enumeration-selftest.sh
EOF
expect "a COMMENTED-OUT gate step does not satisfy rule 5" 1 \
  "named nowhere in" "scripts/check-disabled.sh"

# 13. A `run: |` block scalar must still count, for BOTH rules. Load-bearing in the strict sense: the
#     regex that first fixed case 11 required the command on the `run:` line itself, so this correct
#     workflow would have FAILED rule 1 — and this repo's real ci.yml already uses the block form for
#     three of its steps, so that narrowing was one refactor away from a false red.
reset_fixture
: >"$TMP/scripts/check-block.sh"
cat >"$TMP/$WORKFLOW" <<'EOF'
jobs:
  control-plane:
    steps:
      - name: go suite and gates
        run: |
          go test -v ./...
          bash scripts/check-thing.sh
          bash scripts/check-block.sh
          bash scripts/check-test-enumeration.sh
          bash scripts/check-test-enumeration-selftest.sh
EOF
expect "commands inside a 'run: |' block satisfy both rules" 0 "test-enumeration: ok"

# 14. Narrowing: stripping comments is not the rule — the text must be a RUN command. A step name
#     naming the script is prose, exactly like a comment, and must wire nothing.
reset_fixture
: >"$TMP/scripts/check-named-only.sh"
cat >"$TMP/$WORKFLOW" <<'EOF'
jobs:
  control-plane:
    steps:
      - run: go test -v ./...
      - run: bash scripts/check-thing.sh
      - name: formerly ran scripts/check-named-only.sh
        run: echo skipped
      - run: bash scripts/check-test-enumeration.sh
      - run: bash scripts/check-test-enumeration-selftest.sh
EOF
expect "a gate named only in a step NAME does not satisfy rule 5" 1 \
  "named nowhere in" "scripts/check-named-only.sh"

if [[ "$fail" -eq 0 ]]; then
  echo "test-enumeration-selftest: ok — 14 cases (4 accept, 10 reject), all 5 rules exercised, and"
  echo "  both scripts confirmed wired in $WORKFLOW. Cases 11-14 pin the prose-satisfaction defect in"
  echo "  BOTH rule 1 and rule 5, plus the block-scalar form the narrow fix would have rejected."
fi
exit "$fail"
