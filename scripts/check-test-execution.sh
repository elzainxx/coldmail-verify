#!/usr/bin/env bash
# check-test-execution — BLOCKING CI gate. Every DECLARED test must actually EXECUTE.
#
# The module-1 counterpart of the count assertion in coldmail-send-managed /
# coldmail-send-byo's tests/harness/run.sh (go_suite). The PROPERTY is the same; the RULES are not,
# and copying theirs here would have produced a gate that fails on a correct tree — see the two-tier
# rule below. This is the third time M1 has needed its own rules for a control the senders share
# (check-test-enumeration.sh guards discovery PROPERTIES where they enumerate a package LIST, and its
# rule 6 exists only because M1 has a script-less required job). A fork's divergence compounds; a
# cross-repo port is new work, not a copy.
#
# ---------------------------------------------------------------------------------------------------
# WHY THIS EXISTS, AND WHY check-test-enumeration.sh IS NOT ENOUGH
# ---------------------------------------------------------------------------------------------------
#
# That gate proves a test is DISCOVERED. It cannot prove the test RAN. The gap between those two is
# where this repo's worst defect to date lived (audit D13): TestPGSendOutcome_RuntimeCannotDelete and
# TestPGRateBudget_DeleteDeniedForRuntime were written as "attempt the forbidden DELETE; if it
# SUCCEEDS, t.Skip()". CI connects as superuser, so the DELETE always succeeded, so both tests
# ALWAYS SKIPPED — while ci.yml's comment claimed they proved append-only "on every push". Discovered,
# compiled, vetted, green, and never once executed.
#
# A SKIP IS NOT A PASS, and Go reports it with neither. `go test` exits 0 on an all-skipped package
# and prints `ok`. Nothing in `go build`, `go vet`, or the verdict line distinguishes 381 tests that
# ran from 350 that ran and 31 that quietly did not.
#
# Measured on this tree 2026-08-06, which is what makes the exposure concrete rather than theoretical:
#   * with TM_TEST_DATABASE_URL SET (CI's configuration): 376 executed, 5 skipped, 381 declared.
#   * with it UNSET (an ordinary local run, or a CI secret that silently became empty):
#     345 executed, 36 SKIPPED, 381 declared — and `go test ./...` still exits 0, still prints `ok`
#     for every package, and any harness reading the verdict line calls it green.
# Thirty-one tests, including EVERY grant-enforcement test that D13 exists to keep honest, vanish on
# one empty environment variable. That is the same defect D13 was, reachable by a typo.
#
# ---------------------------------------------------------------------------------------------------
# WHY M1'S RULE IS TWO-TIERED AND THE SENDERS' IS NOT
# ---------------------------------------------------------------------------------------------------
#
# The senders' rule is absolute: any skip is a shortfall, full stop. That is right THERE — every skip
# in those repos is a database-gated one, so a skip always means the environment failed.
#
# Ported verbatim to M1 it would fail on a correct tree. control-plane/test/gaps holds five
# REAL-SHAPED, DELIBERATELY EMPTY harness stubs, one per named GAP the specs call load-bearing. Each
# enumerates the scenarios it will assert and t.Skip()s with a reason naming the PHASE that fills it
# (Docs/test-strategy.md §2). They exist so the scenario lists cannot drift and so the behavioral
# layer is visibly scaffolded-not-proven. Banning their skip would delete the honest signal and, worse,
# would create pressure for an exemption list — and module 3's history is three separate instances of
# an exemption quietly becoming permanent.
#
# So the tiers are drawn by PACKAGE, structurally, with no list to maintain:
#
#   TIER 1 — every package EXCEPT control-plane/test/gaps: executed MUST equal declared. Zero skips
#            tolerated. This is the senders' rule, and it is what catches the 31-test DSN failure.
#   TIER 2 — control-plane/test/gaps ONLY: executed + skipped must equal declared (so a TRUNCATED or
#            panicking run is still caught), and every skip must carry a reason matching
#            `PENDING P<digit>` — naming the phase that discharges it. A skip with no phase named is
#            not scaffolding, it is an exemption, and it fails here.
#
# THE TIER BOUNDARY IS DERIVED, NOT DECLARED. The sanctioned-skip set is "whatever is in the gaps
# package", read from `go test -list` at run time. Nothing here names GAP-37 or counts five. Adding a
# sixth stub needs no edit to this file; moving a database-gated test INTO that package to silence
# this gate is a visible, reviewable act rather than a one-line list edit; and deleting a stub cannot
# leave a stale entry behind, because there are no entries.
#
# ---------------------------------------------------------------------------------------------------
# WHY THE ROSTER COMES FROM `go test -list` AND NOT FROM A NUMBER IN THIS FILE
# ---------------------------------------------------------------------------------------------------
#
# A hardcoded expected count is an instance of the drift this project keeps finding: a doc's gate count
# had drifted below what CI ran, and architecture-overview.md claimed 53 config keys where the database
# held 89. Worse, a count low enough to be safe to hardcode is vacuous by construction — a floor of 5
# against internal/store's 60 lets 55 tests disappear. `go test -list '.*'` is the compiler's own view
# of the roster: it is self-maintaining, it cannot drift, and it cannot be quietly lowered to match a
# broken run. It is also why this gate needs no maintenance when a test is added.
#
# ---------------------------------------------------------------------------------------------------
# WHY IT RUNS THE SUITE PER PACKAGE INSTEAD OF READING CI'S `go test ./...` OUTPUT
# ---------------------------------------------------------------------------------------------------
#
# Two reasons, one of which is a constraint this repo imposes on itself:
#
#   (a) ATTRIBUTION. `go test -v ./...` prints each result line with no package prefix; a reader must
#       infer the owning package from grouping. Measured here, grouping did hold for all 24 packages —
#       but a control resting on an undocumented buffering property is the kind of assumption this
#       project has been burned by. One package per invocation makes attribution structural.
#   (b) rule 1 OF check-test-enumeration.sh. That gate requires the WORKFLOW'S OWN EXECUTED COMMANDS
#       to contain `go test ./...`, because M1's automatic discovery depends on the glob. Piping CI's
#       test step through `tee` for this gate to read would work, but making this gate the CI test
#       RUNNER would move the glob out of the workflow and turn rule 1 red — correctly, since M1 would
#       then have acquired the senders' hand-listed exposure. So CI keeps its glob step, and this gate
#       is a SECOND, INDEPENDENT observation of the same suite. The suite costs ~3s; running it twice
#       is cheaper than an assumption.
#
# The package list is DERIVED from `go list ./...`, never hand-listed here — a hand-listed loop in this
# file would reintroduce exactly the exposure rule 1 guards, inside the gate meant to reinforce it.
#
# ---------------------------------------------------------------------------------------------------
# THE TWO MODES, AND WHY THE DEGRADED ONE CANNOT BE MISTAKEN FOR THE STRICT ONE
# ---------------------------------------------------------------------------------------------------
#
# TM_TEST_DATABASE_URL is what 31 tests are gated on. With it set, tier 1 is fully enforced. Without
# it, tier 1 cannot be enforced at all — and a gate that hard-fails on a developer laptop with no
# Postgres is how exemption lists get born (this repo's own words, in check-test-enumeration.sh).
#
# So: DSN set -> STRICT. DSN unset -> DEGRADED, which still catches truncation and still enforces
# tier 2, prints exactly which tests it could not vouch for, and says in one line that it did NOT
# assert the property.
#
# A degraded run must never be able to stand in for a strict one, and it takes TWO rules, because the
# first one alone was not enough — found by breaking it rather than by reasoning about it:
#   rule 0  reads the WORKFLOW TEXT and fails unless TM_TEST_DATABASE_URL is given a value there.
#   rule 0b reads the RUN-TIME environment and fails if we are in CI with that value empty.
# Rule 0 is satisfied by `TM_TEST_DATABASE_URL: ${{ secrets.PGDSN }}`, which is a perfectly ordinary
# way to configure it — and a secret that is missing or emptied expands to "" at run time, which no
# amount of reading the file can detect. Break-verified: with only rule 0 in place, that exact
# configuration produced `ok [DEGRADED]` and EXIT 0 while 31 tests did not run. The gate written to
# stop a control from going quiet at the moment the property fails did precisely that, one layer up.
# Both rules are kept: rule 0 catches the workflow being edited, rule 0b catches the secret being
# empty, and neither subsumes the other.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$HERE"

WORKFLOW=".github/workflows/ci.yml"
MODULE_DIR="control-plane"
# The one package whose tests are allowed to skip. A PATH, resolved to an import path below — never a
# list of test names.
GAPS_REL="./test/gaps"

fail=0

# --- 0) CI must invoke this gate in STRICT mode ---------------------------------------------------
# Without this, the whole control degrades to "it depends how CI happens to be configured" — and the
# defect that produced this gate (D13) was precisely a CI configuration that made tests vacuous while
# a comment claimed otherwise. Checked against the workflow TEXT rather than the environment, because
# the property is about how CI is wired, and this script cannot see CI's env from a laptop.
#
# The pattern is deliberately loose about placement (a step `env:`, a job `env:`, or an inline
# assignment all satisfy it) and strict about the variable being NAMED AND GIVEN A VALUE: a bare
# `TM_TEST_DATABASE_URL:` with nothing after it is the empty-secret failure this gate exists to catch.
if ! grep -qE 'TM_TEST_DATABASE_URL[:=][[:space:]]*[^[:space:]]' "$WORKFLOW"; then
  echo "FAIL (test-execution): $WORKFLOW does not give TM_TEST_DATABASE_URL a value."
  echo "  31 of this module's tests are gated on that DSN and SKIP silently without it, with go test"
  echo "  still exiting 0 and still printing 'ok' — including every grant-enforcement test that audit"
  echo "  D13 exists to keep honest. If CI does not set it, CI proves 345 of 381 tests and reports"
  echo "  green. Fix: set it on the control-plane job (or the step that runs this gate); do not"
  echo "  relax this rule."
  fail=1
fi

# --- The declared roster, per package, from the compiler -----------------------------------------
cd "$MODULE_DIR"

# `go list` rather than a literal list in this file: a hand-listed loop here would recreate the
# hand-listed-subset exposure that check-test-enumeration.sh rule 1 exists to prevent, inside the gate
# meant to reinforce it.
if ! PKGS="$(go list ./... 2>&1)"; then
  echo "FAIL (test-execution): 'go list ./...' failed — cannot determine the package set:"
  printf '%s\n' "$PKGS" | sed 's/^/    /'
  exit 1
fi

if ! GAPS_PKG="$(go list "$GAPS_REL" 2>/dev/null)"; then
  echo "FAIL (test-execution): the sanctioned-skip package $GAPS_REL does not exist."
  echo "  This gate's tier-2 boundary is DERIVED from that package rather than from a list of test"
  echo "  names, so its absence is not a no-op: every skip in the module would become a tier-1"
  echo "  failure. If the GAP harness moved, update GAPS_REL. If it was deleted, the five PENDING"
  echo "  stubs and the scenario lists they pin went with it — that is a docs/test-strategy change,"
  echo "  not a cleanup."
  exit 1
fi

STRICT=1
if [[ -z "${TM_TEST_DATABASE_URL:-}" ]]; then
  STRICT=0
fi

# --- 0b) IN CI, DEGRADED MODE IS ITSELF A FAILURE -------------------------------------------------
# Rule 0 above reads the workflow TEXT and is satisfied by `TM_TEST_DATABASE_URL: ${{ secrets.X }}`,
# which is a legitimate way to set it. But a secret that is MISSING or EMPTIED expands to the empty
# string at run time, and text cannot see that. Break-verified on this tree 2026-08-06: pointing the
# workflow at a secret reference and running with the variable empty gave `ok [DEGRADED]` and EXIT 0
# — 31 tests unexecuted, gate green. That is this project's signature defect reproduced inside the
# gate written to close it: the control goes quiet at exactly the moment the property fails, and
# because rule 0 still passed, the workflow would have looked correctly configured.
#
# So degradation is a LAPTOP affordance and never a CI one. In CI the absence of the DSN is the
# failure, not a reason to check less. Keyed on the standard CI markers rather than on the workflow
# file, because this is a RUN-TIME condition and only the run can see it.
if [[ "$STRICT" -eq 0 && ( -n "${GITHUB_ACTIONS:-}" || -n "${CI:-}" ) ]]; then
  echo "FAIL (test-execution): running in CI with TM_TEST_DATABASE_URL EMPTY."
  echo "  A missing or emptied secret expands to \"\" here, and 31 of this module's tests — every"
  echo "  grant-enforcement test audit D13 exists to keep honest — then SKIP while 'go test ./...'"
  echo "  exits 0 and prints 'ok'. Degraded mode exists so a laptop without Postgres can still run"
  echo "  this gate; accepting it in CI would make the strict property theoretical, which is the"
  echo "  defect this gate exists to remove. Fix: give the secret a value. Do NOT relax this rule."
  fail=1
fi

# count_executed / count_skipped read `go test -v` output for ONE package.
#
# The `^` anchor is load-bearing: Go indents SUBTEST result lines, and a subtest is not a declared
# top-level test — `go test -list` does not list them, so counting them would inflate `executed` above
# `declared` and make a shortfall arithmetically invisible. Measured here: 58 subtest results across
# the module, all indented, none listed.
count_executed() { grep -cE '^(--- (PASS|FAIL)): ' || true; }
count_skipped()  { grep -cE '^--- SKIP: ' || true; }

total_declared=0
total_executed=0
total_skipped=0
gaps_skipped=0
shortfalls=""
unvouched=""

for pkg in $PKGS; do
  # THE NONZERO FLOOR. `declared=0` used to `continue` here with the comment "no test files, or a
  # package that failed to build — the test step reports the latter", and that disjunction was the
  # hole: the two cases are indistinguishable at this line, only ONE of them is legitimate, and the
  # arithmetic below compares 0 against 0, MATCHES, and reports success having checked nothing.
  #
  # MEASURED, not theorised (2026-08-07). On this Windows host a freshly-linked Go test binary is
  # sometimes silently never executed — not a hang, not an error, just no run. `go test -list` then
  # reports nothing, `declared` is 0, this `continue` fired, and the package vanished from the gate's
  # own output. Three branches of the paired self-test passed that way: `exit 0, want 1`, with the
  # fixture package the case had just written absent from the report entirely. A gate that computes
  # "declared vs executed" cannot see a shortfall in a roster it never obtained, and 0-of-0 is the
  # one input on which its central comparison is vacuously true.
  #
  # So `declared` is now DERIVED TWICE and the two are cross-checked: the roster from `go test -list`,
  # and — independently — whether the package HAS test files at all, from `go list`. Zero declarations
  # is legitimate if and only if there are no test files to declare from. Measured on this tree: two
  # packages are legitimately zero (cmd/control-plane, internal/verdict — 0 TestGoFiles, 0
  # XTestGoFiles), so a blanket floor would false-fail; and a package with a test file that does not
  # COMPILE also yields declared=0 (probed: `go test -list` exits 1 and prints "[build failed]",
  # which the old `2>/dev/null` discarded). The stderr is captured now rather than swallowed, because
  # "the test step reports the latter" is an assumption about a DIFFERENT step, and this gate's whole
  # premise is that a green from another step is not evidence.
  # `if ! x="$(cmd)"` rather than `x="$(cmd)"; rc=$?`. The second form is what this fix was FIRST
  # written as, and under `set -e` (line 47) a failing command substitution in a plain assignment
  # ABORTS THE SCRIPT before the next statement runs — so `list_rc` was never read and the whole floor
  # below was unreachable on the one input it exists to catch. Break-verified: selftest case 12 caught
  # it as "exit code was right (1) but the message did not contain: DECLARED ZERO TESTS", with EMPTY
  # gate output. An unreachable guard whose absence still produced the right exit code is this
  # project's own failure family — the assertion on the MESSAGE, not just the status, is what saw it.
  list_rc=0
  if ! list_out="$(go test -list '.*' "$pkg" 2>&1)"; then
    list_rc=1
  fi
  declared="$(printf '%s\n' "$list_out" | grep -cE '^(Test|Example|Fuzz)' || true)"
  testfiles="$(go list -f '{{len .TestGoFiles}} {{len .XTestGoFiles}}' "$pkg" 2>/dev/null || echo "? ?")"
  has_tests=1
  [[ "$testfiles" == "0 0" ]] && has_tests=0

  if [[ "$declared" -eq 0 ]]; then
    if [[ "$has_tests" -eq 0 && "$list_rc" -eq 0 ]]; then
      # The ONLY sanctioned zero: nothing to declare, and the roster command agreed.
      echo "  $pkg: declared=0 (no test files — nothing to execute)"
      continue
    fi
    echo "FAIL (test-execution): $pkg DECLARED ZERO TESTS, and that is not legitimate here."
    echo "  test files present: TestGoFiles+XTestGoFiles = $testfiles; 'go test -list' exit = $list_rc"
    echo "  A zero roster is the one input this gate's declared-vs-executed comparison cannot judge:"
    echo "  0 executed of 0 declared MATCHES, so without this floor the package is skipped silently"
    echo "  and the gate reports success. Either the test binary did not build, or it did not RUN."
    echo "  'go test -list' said:"
    printf '%s\n' "$list_out" | sed 's/^/      /'
    fail=1
    continue
  fi

  # A nonzero roster obtained from a command that FAILED is not trustworthy either.
  if [[ "$list_rc" -ne 0 ]]; then
    echo "FAIL (test-execution): $pkg — 'go test -list' exited $list_rc while still naming $declared test(s)."
    echo "  A partially-obtained roster understates 'declared', which makes any shortfall below it"
    echo "  arithmetically invisible. Fix the build/list failure; do not proceed on a partial roster."
    printf '%s\n' "$list_out" | sed 's/^/      /'
    fail=1
    continue
  fi

  out="$(go test -count=1 -v "$pkg" 2>&1 || true)"
  executed="$(printf '%s\n' "$out" | count_executed)"
  skipped="$(printf '%s\n' "$out" | count_skipped)"

  total_declared=$((total_declared + declared))
  total_executed=$((total_executed + executed))
  total_skipped=$((total_skipped + skipped))

  # A truncated or panicking run is caught in BOTH tiers: executed+skipped must account for the whole
  # roster regardless of which tier the package is in.
  if [[ $((executed + skipped)) -lt "$declared" ]]; then
    shortfalls+="$pkg: $((declared - executed - skipped)) of $declared declared test(s) produced NO RESULT AT ALL (executed=$executed skipped=$skipped) — a truncated run, a panic, or a -run filter"$'\n'
    fail=1
    continue
  fi

  if [[ "$pkg" == "$GAPS_PKG" ]]; then
    # TIER 2. Skips are sanctioned scaffolding, but only as scaffolding: each must name the phase that
    # discharges it. A reason with no phase is an exemption, and exemptions here become permanent.
    gaps_skipped=$((gaps_skipped + skipped))
    if [[ "$skipped" -gt 0 ]]; then
      pending="$(printf '%s\n' "$out" | grep -cE 'PENDING P[0-9]' || true)"
      if [[ "$pending" -lt "$skipped" ]]; then
        shortfalls+="$pkg: $((skipped - pending)) of $skipped skip(s) do not name a discharging phase (want a reason matching 'PENDING P<n>'). A skip without a phase is an exemption, not scaffolding."$'\n'
        fail=1
      fi
    fi
    echo "  $pkg: declared=$declared executed=$executed skipped=$skipped (tier 2 — PENDING GAP harness, skips sanctioned)"
    continue
  fi

  # TIER 1. Zero skips tolerated.
  if [[ "$executed" -lt "$declared" ]]; then
    names="$(printf '%s\n' "$out" | grep -E '^--- SKIP: ' | sed 's/^--- SKIP: /      - /;s/ ([0-9.]*s)$//')"
    if [[ "$STRICT" -eq 1 ]]; then
      shortfalls+="$pkg: EXECUTED $executed OF $declared DECLARED TESTS ($skipped skipped)"$'\n'"$names"$'\n'
      fail=1
    else
      unvouched+="$pkg: $skipped of $declared not executed"$'\n'"$names"$'\n'
    fi
  fi
  echo "  $pkg: declared=$declared executed=$executed skipped=$skipped"
done

# --- THE MODULE-WIDE FLOOR ------------------------------------------------------------------------
# The per-package floor above cannot fire if the LOOP NEVER RUNS. `go list ./...` succeeding with an
# empty or test-less package set would leave every total at 0, every comparison vacuously true, and
# this gate printing "ok [STRICT] — 0 of 0 declared test(s) EXECUTED" with exit 0. That sentence is
# grammatically a success and semantically nothing, and it is precisely the shape the paired self-test
# case 1 ("a conformant skeleton passes") would still accept, because its fixture DOES declare tests.
#
# The floor is deliberately a bare "more than zero" rather than a pinned number. A hardcoded count
# (`-lt 392`) would be a second roster to maintain by hand — the exposure check-test-enumeration.sh
# rule 1 exists to prevent, reintroduced inside this gate. The property worth asserting is not "this
# module has 392 tests"; it is "this gate actually measured something".
if [[ "$total_declared" -eq 0 ]]; then
  echo "FAIL (test-execution): the module declared ZERO tests in total."
  echo "  'go list ./...' returned $(printf '%s\n' $PKGS | grep -c . || true) package(s) and not one"
  echo "  declared a test, so every comparison this gate makes was vacuously satisfied and it was"
  echo "  about to report success. A gate that measured nothing must never be green: 0 executed of 0"
  echo "  declared MATCHES. Causes seen in practice: a toolchain that does not execute freshly-built"
  echo "  test binaries (see the per-package floor above), a wrong MODULE_DIR, or a build break."
  fail=1
fi

if [[ -n "$shortfalls" ]]; then
  echo "FAIL (test-execution): declared tests did not execute:"
  printf '%s' "$shortfalls" | sed '/^$/d;s/^/    /'
  echo "  go test exits 0 and prints 'ok' for an all-skipped package, so without this gate the above"
  echo "  is reported as PASSING. This is NOT satisfied by declaring the skips intended: a skip is"
  echo "  unaccounted-for work. Either give the package what it needs (the DSN, a fixture, a role) or"
  echo "  delete the test. The one sanctioned exception is the PENDING GAP harness in $GAPS_PKG,"
  echo "  whose skips must each name the phase that discharges them."
fi

if [[ "$STRICT" -eq 0 ]]; then
  echo "test-execution: DEGRADED MODE — TM_TEST_DATABASE_URL is unset, so tier 1 was NOT asserted."
  if [[ -n "$unvouched" ]]; then
    echo "  Database-gated tests this run could not vouch for:"
    printf '%s' "$unvouched" | sed '/^$/d;s/^/    /'
  fi
  echo "  Truncation and the tier-2 phase-naming rule WERE asserted. CI runs this gate with the DSN"
  echo "  set (rule 0 above fails if the workflow stops doing so), so the strict property is asserted"
  echo "  there — this line exists so a degraded local run can never be mistaken for a strict one."
fi

if [[ "$fail" -eq 0 ]]; then
  # The success line states WHERE the skips are rather than asserting it. The first version of this
  # line read "$total_skipped skipped (all in $GAPS_PKG…)" unconditionally — and in degraded mode
  # that printed "36 skipped (all in .../test/gaps)" when only 5 were, the other 31 being the
  # database-gated tests this gate had just declined to vouch for. A summary line that states
  # something it has not checked is the same defect as a comment claiming a gate is immune.
  #
  # In STRICT mode "all in $GAPS_PKG" is a DERIVED claim: tier 1 fails on any shortfall, so reaching
  # here with fail=0 implies every skip was in the sanctioned package. It is asserted anyway, and the
  # reason is a measured one — break-verifying the paired self-test (B78) broke tier 1's comparison and
  # this line then printed "2 skipped, all in .../test/gaps" when only 1 was. The derivation was sound;
  # what it rested on had been removed. A summary that is only true while the code above it is correct
  # is exactly the shape this gate exists to find, so the arithmetic is now checked rather than trusted.
  if [[ "$STRICT" -eq 1 && "$total_skipped" -ne "$gaps_skipped" ]]; then
    echo "FAIL (test-execution): INTERNAL INCONSISTENCY — a strict run is about to report success with"
    echo "  $total_skipped skip(s) of which only $gaps_skipped are in the sanctioned package $GAPS_PKG."
    echo "  Tier 1 should already have failed on those, so reaching this line means a rule above did"
    echo "  not fire: this gate is broken, not the tree. Do not silence this by widening the tiers."
    exit 1
  fi
  mode="STRICT"; [[ "$STRICT" -eq 0 ]] && mode="DEGRADED"
  echo "test-execution: ok [$mode] — $total_executed of $total_declared declared test(s) EXECUTED,"
  if [[ "$STRICT" -eq 1 ]]; then
    echo "  $total_skipped skipped, all in $GAPS_PKG (tier 2) and each naming a discharging phase."
  else
    echo "  $total_skipped skipped: $gaps_skipped sanctioned in $GAPS_PKG (tier 2, phases named)"
    echo "  and $((total_skipped - gaps_skipped)) database-gated test(s) NOT VOUCHED FOR (see above)."
  fi
fi
exit "$fail"
