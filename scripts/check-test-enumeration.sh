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
#   6. Every JOB that PROJECT_RULES.md §7 requires still exists in the workflow. Rule 5 covers gates
#      that live on disk as a file; it is structurally blind to a required job with no script behind
#      it. `code-review` is exactly that: PROJECT_RULES.md §7 makes it mandatory on every change, and
#      it is a bare `run: echo` placeholder whose own comment claimed "a placeholder that FAILS if
#      removed keeps the gate from silently disappearing." That claim was FALSE, break-verified
#      2026-08-05: deleting the entire job left all five gates rc=0 and nothing else noticed. The
#      comment was the only thing "keeping" the gate — a warning comment is not a control, in the
#      file whose header says so. Rule 6 is the mechanism.
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

# --- WHAT THE WORKFLOW ACTUALLY EXECUTES (shared by rules 1 and 5) ------------------------------
# Emit ONLY the shell text the workflow EXECUTES: the value of every `run:` key, including the body
# of `run: |` block scalars, with comments stripped. Everything else in the YAML — a `name:`, a
# full-line comment, a trailing comment — is prose ABOUT the workflow, not the workflow.
#
# Both rules below read this, deliberately, rather than each grepping the file its own way. Rules 1
# and 5 were checking the same property with two different mechanisms; one was fixed on 2026-08-05
# and the other was left broken while a comment claimed it was fine (see rule 1's CORRECTION note).
# One reading means they cannot diverge like that again.
#
# A `#` begins a comment at start-of-line or after whitespace (YAML's rule, and close enough to the
# shell's inside a run block: a shell comment in a run block invokes nothing either). Verified
# against this workflow before adopting: its only trailing comment sits on `fetch-depth: 0`, not on a
# `run:` line, so nothing real is truncated. NOTE `defaults: run:` at ci.yml:21 is a MAPPING, not a
# command — it yields an empty line here (no pattern can match it), and its nested
# `working-directory:` key is never read as a command, because a block scalar is entered only when
# the value literally begins with `|` or `>`. Block-scalar membership is "indented deeper than the
# `run` key itself", measured from `run` rather than from the line's leading whitespace, so the
# `- run: |` list-item form cannot swallow its sibling keys.
workflow_commands() {
  awk '
    function strip(s) { sub(/(^|[ \t])#.*$/, "", s); return s }
    {
      if (inblock) {
        if ($0 ~ /^[ \t]*$/) { next }
        match($0, /^[ \t]*/)
        if (RLENGTH > blockind) { print strip($0); next }
        inblock = 0
      }
      if ($0 ~ /^[ \t]*(-[ \t]+)?run:/) {
        blockind = index($0, "run:") - 1
        rest = $0
        sub(/^[ \t]*(-[ \t]+)?run:[ \t]*/, "", rest)
        if (rest ~ /^[|>]/) { inblock = 1; next }
        print strip(rest)
      }
    }
  ' "$1"
}

# `|| true`: a workflow with no `run:` key yields nothing, and an empty result must still reach both
# rules and fail them LOUDLY rather than killing this script here with no output at all — the same
# reasoning that fixed config_consumption's silent death on an emptied ledger, found the same day.
CMDS="$(workflow_commands "$WORKFLOW" || true)"

# --- 1) CI still runs `go test ./...`, not a hand-listed subset ---------------------------------
# Reading EXECUTED COMMANDS is load-bearing and was NOT in the first version of this check. It grepped
# the whole file, so the pattern matched the explanatory COMMENT in ci.yml that mentions
# `go test ./...` while describing this very gate — and break-verification on the real tree (replace
# the real command with a hand-listed package subset) left the gate GREEN. A gate satisfied by prose
# about itself is the purest form of the vacuity this file exists to prevent. Selftest case 11 pins it.
#
# CORRECTION (2026-08-05, same day). The first fix was a `run:`-anchored regex here, and this comment
# then read "Rule 5 had the anchor from the start; rule 1 did not, and only breaking it on the real
# tree showed the difference." The first clause was FALSE — rule 5 matched the script path ANYWHERE in
# the workflow, comments included, and a break proved it green on a deleted step. The claim is the
# instructive part: it asserted a neighbouring rule was immune WITHOUT breaking it, in a file whose
# whole subject is that unverified claims of immunity are worthless.
#
# The anchored regex was also too narrow in the other direction: it required the command to sit on the
# `run:` line itself, so moving `go test ./...` into a perfectly ordinary `run: |` block would have
# failed this rule on a correct workflow — and a gate that rejects valid input is how exemption lists
# get born. Reading workflow_commands() fixes both directions at once, and both rules now share it.
if ! grep -qE 'go test( +-[^ ]+)* +\./\.\.\.' <<<"$CMDS"; then
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
#
# THE MATCH IS AGAINST EXECUTED COMMANDS, NOT THE WORKFLOW FILE (fixed 2026-08-05). Rule 1's header
# above says "Rule 5 had the anchor from the start." That was WRONG — rule 5 matched the script's
# PATH anywhere in the file, and the comment right here claimed that using the path rather than the
# bare filename made a comment unable to satisfy it. Break-verified on this tree: deleting the
# check-no-secret-leak.sh step outright and leaving behind
#     # TODO: re-enable bash scripts/check-no-secret-leak.sh — the canary boot is slow on the runner
# left this gate GREEN, reporting "all 5 check script(s) wired in .github/workflows/ci.yml" about a
# workflow that ran four. So rule 1's fix was incomplete in the very same file, and the incorrect
# claim about rule 5 is precisely what would have stopped anyone re-checking it. A comment asserting
# a gate is immune is worth nothing; only the break tells you.
#
# Third instance of one shape in a single day: rule 1 matched `go test ./...` inside a comment about
# itself, both send-managed/send-byo config_consumption gates accepted a TODO naming a config key as
# that key's consumer, and now this. A commented-out step is the LIKELIEST way to reach it — it is
# what someone writes while disabling a gate they mean to restore, which is exactly when a blocking
# gate silently stops blocking. $CMDS is the shared reading defined near the top of this file.
unwired=""
for g in scripts/check-*.sh; do
  if ! grep -qF "$g" <<<"$CMDS"; then
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

# --- 6) Every REQUIRED JOB still exists in the workflow ------------------------------------------
# Rule 5's blind spot, found 2026-08-05 by break-testing the OTHER claims in ci.yml (fix-plan item 3:
# "grant tests as the runtime role, or delete the false CI claim"). Rule 5 can only see a gate that
# exists as a check-*.sh on disk. A required job with no script behind it is invisible to it.
#
# `code-review` is exactly that job. PROJECT_RULES.md §7 makes the code-reviewer gate mandatory on
# EVERY change, doc changes included, and until integration it is a bare `run: echo` placeholder whose
# own comment read: "a placeholder that FAILS if removed keeps the gate from silently disappearing."
# Break-verified: deleting the ENTIRE job left all five gates rc=0 and nothing anywhere noticed. The
# comment was the only thing keeping the gate — in a file whose header states that a warning comment
# is not a control. That claim is now deleted from ci.yml and replaced by this rule.
#
# Job names, not step text: what §7 requires is that the JOB runs, and a job is what a branch
# protection rule can be a required status check on. Matched as `^  <name>:` at job indentation, on a
# comment-stripped read of the file — a `#`-commented job header is a disabled job, exactly like a
# commented-out step in rule 5.
#
# LIMIT, stated rather than assumed away: this checks the job EXISTS, not that its body does anything.
# Gutting the job's steps while keeping the `code-review:` key would pass. That is not a regression —
# the body is already only an `echo` until integration — and it is not worth a step-content rule that
# would have to be rewritten the moment the real reviewer is wired. What rule 6 protects is the JOB
# NAME, which is what a branch-protection required status check binds to. Written down because an
# unverified claim of immunity is the defect that produced this rule in the first place.
REQUIRED_JOBS=(code-review)
missingjobs=""
jobkeys="$(sed -e 's|\(^\|[[:space:]]\)#.*$||' "$WORKFLOW" | grep -oE '^  [A-Za-z0-9_-]+:' | tr -d ' :' | sort -u || true)"
for j in "${REQUIRED_JOBS[@]}"; do
  if ! grep -qxF -- "$j" <<<"$jobkeys"; then
    missingjobs+="$j"$'\n'
  fi
done

if [[ -n "${missingjobs//[$'\n']/}" ]]; then
  echo "FAIL (test-enumeration): job(s) required by PROJECT_RULES.md §7 are absent from $WORKFLOW:"
  printf '%s' "$missingjobs" | sed '/^$/d;s/^/    - /'
  echo "  §7 makes the code-reviewer gate mandatory on every change, doc changes included. Until it is"
  echo "  wired to the org reviewer it is a placeholder job — and a placeholder that can be deleted"
  echo "  silently is not a gate. Deleting it is a rules violation, not a cleanup."
  echo "  Fix: restore the job. If §7 changed, change §7 first and update REQUIRED_JOBS here."
  fail=1
fi

if [[ "$fail" -eq 0 ]]; then
  ntests="$(find . -name '*_test.go' -not -path './.git/*' -not -path '*/vendor/*' | wc -l | tr -d ' ')"
  nspecs="$(find . -name '*_spec.rb' -not -path './.git/*' | wc -l | tr -d ' ')"
  ngates="$(ls scripts/check-*.sh | wc -l | tr -d ' ')"
  echo "test-enumeration: ok — go test ./... intact over 1 module ($ntests test files, 0 build-tagged),"
  echo "  RSpec discovery unnarrowed ($nspecs spec files), all $ngates check script(s) wired in $WORKFLOW,"
  echo "  and all ${#REQUIRED_JOBS[@]} PROJECT_RULES §7 job(s) present."
fi
exit "$fail"
