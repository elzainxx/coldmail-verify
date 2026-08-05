#!/usr/bin/env bash
# check-test-enumeration — BLOCKING CI gate. The module-1 counterpart of
# coldmail-send-byo / coldmail-send-managed's tests/harness/checks/test_enumeration.sh.
#
# WHY THIS EXISTS. Four times across this project, work that was written, correct, and passing was
# executed by NOTHING — and every indicator was green in each case:
#
#   * module 2: internal/httpapi's first test file, then internal/workerlink + internal/workerproto.
#   * module 3: config_consumption_selftest.sh, a BLOCKING control the workflow never named, plus
#     four more Go packages (31 tests) found by the audit that followed.
#
# A prose warning was added after the first incident and three more packages accumulated behind it.
# A warning comment is not a control. The fix for "someone might forget X" is never a note reminding
# them; it is a mechanism that fails when X is missing.
#
# WHY M1'S RULES ARE NOT M2/M3'S. Those repos enumerate each Go package by name on a `go_suite` line,
# so a new package is invisible until someone edits the list — that is what their gate checks. M1
# does NOT have that exposure: CI runs `go test -v ./...` over a single module, and RSpec uses its
# default `spec/**/*_spec.rb` glob with no pattern/exclude/default_path override. Both discover new
# tests automatically. Copying M2/M3's rule 1 here would be a gate that can never fail — the vacuity
# this project keeps hitting.
#
# So this gate guards the DISCOVERY PROPERTIES THEMSELVES, which are what M1 can silently lose:
#
#   1. CI still runs `go test ./...` — not a hand-listed subset. The moment someone replaces that
#      with named packages, M1 acquires M2/M3's exposure and this gate must be rewritten, not
#      quietly satisfied.
#   2. There is exactly ONE go.mod. `go test ./...` only walks the module it is run in, so a second
#      module anywhere under the repo would have its tests silently excluded by a green CI run.
#   3. No test file is excluded by a BUILD CONSTRAINT. A `//go:build` line on a _test.go file makes
#      it invisible to `go test ./...` while it still compiles under `go vet` with that tag — the
#      exact "green signal, no coverage" shape, and the one way rule 1's automatic discovery can be
#      defeated without touching CI.
#   4. RSpec's discovery is not narrowed. A `--pattern`, `--exclude-pattern`, or `--default-path` in
#      .rspec, or a `config.pattern` in spec_helper.rb, can drop spec files from the run while the
#      suite still reports green.
#   5. Every scripts/check-*.sh is named in the CI workflow. This is the one rule that transfers
#      verbatim: a gate on disk the workflow never names is indistinguishable from a gate that
#      passes. That is exactly how module 3's config_consumption_selftest.sh landed unwired — and I
#      "verified" it there by globbing the checks directory, which is not what CI does.
#
# Rule 5 covers this file too, but it cannot detect its own absence — hence it is wired in the same
# commit that creates it, and check-test-enumeration-selftest.sh asserts BOTH are named in CI.
#
# NO EXEMPTION LIST, DELIBERATELY. Module 3's history is three separate instances of an exemption
# quietly becoming permanent. A test that genuinely cannot run in CI should not be a test here.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$HERE"

WORKFLOW=".github/workflows/ci.yml"
RSPEC_DOTFILE="verifier-node/truemail-rack/.rspec"
SPEC_HELPER="verifier-node/truemail-rack/spec/spec_helper.rb"

fail=0

# --- 1) CI still runs `go test ./...`, not a hand-listed subset ---------------------------------
# The `run:` anchor is load-bearing and was NOT in the first version of this check. Without it the
# pattern matched the explanatory COMMENT in ci.yml that mentions `go test ./...` while describing
# this very gate — so break-verification on the real tree (replace the real command with a
# hand-listed package subset) left the gate GREEN. A gate satisfied by prose about itself is the
# purest form of the vacuity this file exists to prevent. Rule 5 had the anchor from the start; rule
# 1 did not, and only breaking it on the real tree showed the difference. Selftest case 11 pins it.
if ! grep -qE '^[[:space:]]*(-[[:space:]]+)?run:.*go test( +-[^ ]+)* +\./\.\.\.' "$WORKFLOW"; then
  echo "FAIL (test-enumeration): $WORKFLOW no longer runs 'go test ./...' over the whole module."
  echo "  M1's Go tests are discovered automatically ONLY because CI globs the module. If CI now names"
  echo "  packages individually, M1 has acquired module 2/3's exposure: a new package runs nowhere and"
  echo "  nothing says so. Fix: restore the glob, or port M2/M3's go_suite enumeration rule here —"
  echo "  do not simply delete this check."
  fail=1
fi

# --- 2) Exactly one go.mod -----------------------------------------------------------------------
# `go test ./...` walks only the module it runs in. A second module's tests would be silently
# excluded from a green run — no error, no skip, no mention.
mods="$(find . -name go.mod -not -path './.git/*' -not -path '*/vendor/*' | sed 's|^\./||' | sort)"
nmods="$(echo "$mods" | grep -c . || true)"
if [[ "$nmods" -ne 1 ]]; then
  echo "FAIL (test-enumeration): expected exactly 1 go.mod, found $nmods:"
  printf '%s\n' "$mods" | sed '/^$/d;s/^/    - /'
  echo "  CI runs 'go test ./...' with working-directory: control-plane, which walks THAT module only."
  echo "  Any other module's tests do not run and CI stays green. Fix: add a CI step for the new"
  echo "  module (and extend this check to expect it), or fold the code into control-plane."
  fail=1
fi

# --- 3) No _test.go is hidden behind a build constraint ------------------------------------------
# A //go:build tag on a test file removes it from `go test ./...` while `go vet` with that tag still
# type-checks it clean — the same "green signal, zero coverage" shape as an unwired package.
tagged=""
while IFS= read -r f; do
  [[ -z "$f" ]] && continue
  # Constraints are only honoured in the header, before any non-comment/non-blank line. Scanning the
  # first 20 lines is generous and avoids flagging the literal string inside test data or a comment
  # further down the file.
  if head -20 "$f" | grep -qE '^//go:build|^// \+build'; then
    tagged+="${f#./}"$'\n'
  fi
done <<<"$(find . -name '*_test.go' -not -path './.git/*' -not -path '*/vendor/*')"

if [[ -n "${tagged//[$'\n']/}" ]]; then
  echo "FAIL (test-enumeration): test file(s) carry a build constraint, so 'go test ./...' skips them:"
  printf '%s' "$tagged" | sed '/^$/d;s/^/    - /'
  echo "  A tagged test file still compiles and vets clean under its tag, so it is indistinguishable"
  echo "  from a test that runs. This is the one way M1's automatic discovery can be defeated without"
  echo "  touching CI. Fix: remove the constraint and gate at RUNTIME instead (t.Skip with a reason,"
  echo "  as the store's TM_TEST_DATABASE_URL tests do) — a skip is VISIBLE in 'go test -v' output;"
  echo "  an excluded file is not."
  fail=1
fi

# --- 4) RSpec discovery is not narrowed ---------------------------------------------------------
# The suite runs `bundle exec rspec` with no file arguments, so it relies entirely on the default
# spec/**/*_spec.rb glob. Any of these narrows it while the run still reports green.
if [[ -f "$RSPEC_DOTFILE" ]]; then
  if narrowing="$(grep -nE '^\s*--(pattern|exclude-pattern|default-path)' "$RSPEC_DOTFILE")"; then
    echo "FAIL (test-enumeration): $RSPEC_DOTFILE narrows RSpec's file discovery:"
    printf '%s\n' "$narrowing" | sed 's/^/    /'
    echo "  CI runs 'bundle exec rspec' with no file arguments, so spec files outside the resulting"
    echo "  glob do not run and the suite still reports green. Fix: remove the override, or tag the"
    echo "  specs and filter by tag — a filtered spec is reported as excluded; an unglobbed file is not."
    fail=1
  fi
fi
if [[ -f "$SPEC_HELPER" ]]; then
  if narrowing="$(grep -nE '^\s*config\.(pattern|exclude_pattern)\s*=' "$SPEC_HELPER")"; then
    echo "FAIL (test-enumeration): $SPEC_HELPER narrows RSpec's file discovery:"
    printf '%s\n' "$narrowing" | sed 's/^/    /'
    echo "  Same failure as an .rspec --pattern, one layer down. Fix: remove it, or filter by tag."
    fail=1
  fi
fi

# --- 5) Every check script is named in the CI workflow ------------------------------------------
# The transferable rule. gen-config-reference.sh is deliberately NOT covered: it is the GENERATOR,
# and check-config-reference-fresh.sh (which IS enumerated) invokes it — so it is exercised in CI
# through its checker. The glob is check-*.sh precisely so a generator cannot be mistaken for a gate.
unwired=""
for g in scripts/check-*.sh; do
  # Match the path as it appears in a workflow `run:` line, not the bare filename, so a passing
  # mention inside a comment cannot satisfy this.
  if ! grep -qF "$g" "$WORKFLOW"; then
    unwired+="$g"$'\n'
  fi
done

if [[ -n "${unwired//[$'\n']/}" ]]; then
  echo "FAIL (test-enumeration): check script(s) exist but are named nowhere in $WORKFLOW:"
  printf '%s' "$unwired" | sed '/^$/d;s/^/    - /'
  echo "  The workflow enumerates each gate as its own step; it does NOT glob. An unenumerated gate"
  echo "  never runs in CI, so it is a green signal that proves nothing — this is exactly how module"
  echo "  3's config_consumption_selftest.sh landed as a BLOCKING control that never ran."
  echo "  Fix: add a step to $WORKFLOW."
  fail=1
fi

if [[ "$fail" -eq 0 ]]; then
  ntests="$(find . -name '*_test.go' -not -path './.git/*' -not -path '*/vendor/*' | wc -l | tr -d ' ')"
  nspecs="$(find . -name '*_spec.rb' -not -path './.git/*' | wc -l | tr -d ' ')"
  ngates="$(ls scripts/check-*.sh | wc -l | tr -d ' ')"
  echo "test-enumeration: ok — go test ./... intact over 1 module ($ntests test files, 0 build-tagged),"
  echo "  RSpec discovery unnarrowed ($nspecs spec files), all $ngates check script(s) wired in $WORKFLOW."
fi
exit "$fail"
