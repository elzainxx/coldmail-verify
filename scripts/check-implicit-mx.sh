#!/usr/bin/env bash
# check-implicit-mx — BLOCKING CI gate (ADR-0020): module 1 must honour RFC 5321 §5.1 against REAL DNS. A domain with no MX but
# an A/AAAA record receives its mail at that host, so it is NOT `invalid / no_mx`; `invalid` for a missing route is stated only
# when non-existence is provable (no MX, no A, no AAAA, all definitively absent), and an outage of ours at any step is `unknown`.
#
#   check-implicit-mx.sh                   runs the REAL code: the implicit-MX tests (every real DNS shape: A-only, AAAA-only,
#                                          dual-stack, alias, no-records, NXDOMAIN, null MX with and without an A record, and every
#                                          partial failure of the A/AAAA follow-up) must pass inside a container whose resolv.conf
#                                          carries a `search` domain, so the rooted-name wire assertions bite on the follow-ups too.
#   check-implicit-mx.sh --mutant-implicit the PAIRED CONTROL (also scripts/check-implicit-mx-selftest.sh). Reverts the ONE line
#                                          that follows a not-found MX with an address lookup — i.e. restores the pre-ADR-0020
#                                          behaviour — and requires the A-only rows to go RED while the CONTROL rows stay green.
#   check-implicit-mx.sh --mutant-partial  the second control: makes the A/AAAA combination treat ANY AAAA error as "absent" (the
#                                          failure mode of Go's own combined lookup, which keeps whichever error arrives last) and
#                                          requires the partial-failure row to go RED: a SERVFAIL of ours stated as `invalid`.
#
# WHY THIS EXISTS AS A GATE AND NOT A ONE-OFF. The previous implicit-MX fallback was dead code for the life of the project: it sat
# behind `LookupMX` returning an empty list with no error, a shape Go's resolver never produces (NODATA is an error, identical to
# NXDOMAIN), and the fixtures were fakes that returned exactly that impossible shape — so every test passed and no real domain ever
# reached the fallback. The only defence is a fixture that speaks the real protocol and a standing proof that it can fail.
#
# NO SERVER TO START. The DNS server is in the test binary (internal/dnsfixture), on loopback inside the container.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export MSYS_NO_PATHCONV=1   # git-bash on Windows must not rewrite the container-side paths below; a no-op on a Linux runner
# The path Docker needs for a bind mount: native on Windows (cygpath), unchanged elsewhere.
hostpath() { if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf %s "$1"; fi; }
case "${1:-}" in
  "")                 MODE=real ;;
  --mutant-implicit)  MODE=mutant-implicit ;;
  --mutant-partial)   MODE=mutant-partial ;;
  *) echo "usage: check-implicit-mx.sh [--mutant-implicit|--mutant-partial]" >&2; exit 2 ;;
esac
IMG="alpine@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf"   # alpine:3.24 (same pin as check-dns-search-domain.sh)
RUN='TestMXLayer_RealDNS_ImplicitMX|TestNetResolver_LookupIP|TestNetResolver_RootsEveryName'

# Under the checkout, not /tmp: the path must be valid for the Docker daemon on a runner and inside a CI replica alike.
W="$HERE/.implicit-mx-work"
rm -rf "$W"; mkdir -p "$W/control-plane"
trap 'rm -rf "$W"' EXIT
fail() { echo "check-implicit-mx ($MODE) FAIL: $*" >&2; exit 1; }

cp -r "$HERE/control-plane/." "$W/control-plane/"
LAYERS="$W/control-plane/internal/layers"
mutate() {  # mutate <file> <sed-expr> <marker-that-must-appear> — verifies the mutation landed instead of assuming it
  sed -i "$2" "$1"
  grep -q "$3" "$1" || fail "the mutation did not apply ($3 not found in $(basename "$1")) — the source it targets was renamed, and this control would be testing the REAL code"
}
case "$MODE" in
  mutant-implicit)
    mutate "$LAYERS/layers.go" 's|^\(\t*\)return impliedMX(ctx, r, domain) // IMPLICIT-MX$|\1return nil, mxNone // MUTANT: implicit MX disabled (the pre-ADR-0020 behaviour)|' 'MUTANT: implicit MX disabled' ;;
  mutant-partial)
    mutate "$LAYERS/resolver.go" 's|^\(\t*\)v6Absent := .*$|\1v6Absent := true // MUTANT: any AAAA failure counts as "absent"|' 'MUTANT: any AAAA failure counts as' ;;
esac
( cd "$W/control-plane" && CGO_ENABLED=0 GOTOOLCHAIN=local go test -c -o "$W/layers.test" ./internal/layers ) \
  || fail "could not build the layers test binary"

# Listed inside the container too: the binary is a Linux binary, so this works on any host that can run Docker.
listing="$(docker run --rm -v "$(hostpath "$W")/layers.test:/layers.test:ro" "$IMG" /layers.test -test.list "$RUN")" \
  || fail "could not list the tests in the built binary"
declared="$(grep -c '^Test' <<<"$listing" || true)"
[ "${declared:-0}" -ge 4 ] || fail "the test binary declares only ${declared:-0} tests matching $RUN (expected the implicit-MX matrix, the LookupIP combination table and the rooting pin)"

printf 'nameserver 127.0.0.1\nsearch search-domain.test\noptions ndots:1\n' >"$W/resolv.conf"

rc=0
out="$(docker run --rm -v "$(hostpath "$W")/resolv.conf:/etc/resolv.conf:ro" -v "$(hostpath "$W")/layers.test:/layers.test:ro" \
  "$IMG" /layers.test -test.v -test.count=1 -test.timeout=120s -test.run "$RUN" 2>&1)" || rc=$?

# The same mutant, END TO END: the whole Verify path (cheap layers → policy → SMTP stage → log/cache) over the real DNS protocol.
# (No container needed: these tests have no search-domain dependency; the wire is already covered above.)
E2E='TestImplicitMX'
e2e_declared="$( cd "$W/control-plane" && CGO_ENABLED=0 GOTOOLCHAIN=local go test -list "$E2E" ./internal/orchestrator | grep -c '^TestImplicitMX' || true )"
[ "${e2e_declared:-0}" -ge 5 ] || fail "the orchestrator package declares only ${e2e_declared:-0} $E2E tests (expected the five end-to-end cases)"
e2e_rc=0
e2e="$( cd "$W/control-plane" && CGO_ENABLED=0 GOTOOLCHAIN=local go test -count=1 -v -run "$E2E" ./internal/orchestrator 2>&1 )" || e2e_rc=$?
e2e_passed="$(grep -c '^--- PASS: TestImplicitMX' <<<"$e2e" || true)"
e2e_failed="$(grep -c '^--- FAIL: TestImplicitMX' <<<"$e2e" || true)"
e2e_skipped="$(grep -c '^--- SKIP: TestImplicitMX' <<<"$e2e" || true)"
echo "check-implicit-mx ($MODE): end-to-end declared=$e2e_declared passed=$e2e_passed failed=$e2e_failed skipped=$e2e_skipped rc=$e2e_rc"
grep -E '^--- (FAIL|SKIP)' <<<"$e2e" | cut -c1-200 || true

# Verdicts read $out through here-strings, never `echo "$out" | grep -q` (an early-exit pipe under pipefail).
passed="$(grep -c '^--- PASS: ' <<<"$out" || true)"
failed="$(grep -c '^--- FAIL: ' <<<"$out" || true)"
skipped="$(grep -c '^--- SKIP: ' <<<"$out" || true)"
echo "check-implicit-mx ($MODE): search domain injected; declared=$declared passed=$passed failed=$failed skipped=$skipped rc=$rc"
grep -E '^--- (FAIL|SKIP)' <<<"$out" | cut -c1-200 || true

if [ "$MODE" = real ]; then
  [ "$rc" -eq 0 ]               || { grep -E '_test.go:[0-9]+:' <<<"$out" | head -20 >&2 || true; fail "an implicit-MX test failed (rc=$rc): module 1 is not honouring RFC 5321 §5.1 against real DNS shapes"; }
  [ "$failed" -eq 0 ]           || fail "$failed test(s) failed"
  [ "$skipped" -eq 0 ]          || fail "$skipped test(s) skipped — a skip is a shortfall, not a pass"
  [ "$passed" -eq "$declared" ] || fail "only $passed of $declared declared tests passed"
  grep -q -- '--- PASS: TestMXLayer_RealDNS_ImplicitMX_LegacyLogicFailsTheFixture' <<<"$out" \
    || fail "the legacy-logic control did not run and pass — without it nothing shows the fixture can reproduce the old defect"
  [ "$e2e_rc" -eq 0 ]                  || { grep -E '_test.go:[0-9]+:' <<<"$e2e" | head -20 >&2 || true; fail "an end-to-end implicit-MX test failed (rc=$e2e_rc)"; }
  [ "$e2e_failed" -eq 0 ] && [ "$e2e_skipped" -eq 0 ] || fail "end-to-end: $e2e_failed failed, $e2e_skipped skipped — a skip is a shortfall, not a pass"
  [ "$e2e_passed" -eq "$e2e_declared" ] || fail "end-to-end: only $e2e_passed of $e2e_declared declared tests passed"
  echo "check-implicit-mx: ok — all $declared wire-level and $e2e_declared end-to-end implicit-MX tests pass against the real DNS protocol (A-only reaches the probe and is not invalid; if the probe cannot run it is risky; no-records, NXDOMAIN and null MX still are invalid; an outage of ours is unknown)"
else
  [ "$e2e_rc" -ne 0 ] || fail "the mutant PASSED the END-TO-END tests: the whole-path control cannot detect the defect"
  # A mutant must go red, in the right test, with the right signature, and the CONTROL rows must stay green — otherwise the red
  # means "the fixture is broken", not "the fixture detects this defect".
  [ "$rc" -ne 0 ] || fail "the mutant PASSED: this gate cannot detect the defect it exists for"
  grep -q -- '--- FAIL: TestMXLayer_RealDNS_ImplicitMX ' <<<"$out" \
    || fail "the mutant went red, but not in the implicit-MX matrix — it failed for some other reason, which proves nothing about this defect"
  # "Controls stay green" is about the VERDICT: a CONTROL row (no-records / NXDOMAIN / null MX) must still come out invalid under
  # the mutant. Its WIRE assertion legitimately differs (the mutant no longer asks for an address), so only `MXLayer result=` counts.
  bad="$(grep -E 'dns_realdns_test.go:[0-9]+: .*CONTROL.*MXLayer result=' <<<"$out" || true)"
  [ -z "$bad" ] || { echo "$bad" | head -5 >&2; fail "a CONTROL row's verdict changed under the mutant — the controls are unsound, so this red means nothing"; }
  case "$MODE" in
    mutant-implicit)
      grep -q 'A-only: NODATA for MX, an A record .*MXLayer result="fail" outcome=2, want result="pass" outcome=1' <<<"$out" \
        || fail "the A-only row did not fail with the defect's signature (A-only domain reported Fail / mxNone)"
      grep -q -- '--- FAIL: TestImplicitMX_ADomainWithOnlyAnARecord_ContinuesToTheSMTPProbe' <<<"$e2e" \
        || fail "end-to-end: the A-only → SMTP probe test did not go red under the mutant"
      grep -q -- '--- PASS: TestImplicitMX_Controls_ProvableAbsenceIsStillInvalid_AndNeverReachesTheProbe' <<<"$e2e" \
        || fail "end-to-end: the provable-absence controls did not stay green under the mutant — the whole-path fixture is unsound"
      echo "check-implicit-mx --mutant-implicit: ok — with the address follow-up removed the matrix goes RED (A-only read as invalid/no_mx; declared=$declared failed=$failed), controls green" ;;
    mutant-partial)
      grep -q 'A = NODATA, AAAA = SERVFAIL .*MXLayer result="fail" outcome=2, want result="unknown" outcome=4' <<<"$out" \
        || fail "the partial-failure row did not fail with the defect's signature (an AAAA SERVFAIL reported as invalid/no_mx)"
      grep -q -- '--- FAIL: TestNetResolver_LookupIP_CombinesTheTwoFamiliesExplicitly' <<<"$out" \
        || fail "the LookupIP combination table did not catch the mutant — its unit-level pin is missing"
      grep -q -- '--- FAIL: TestImplicitMX_AnOutageAtTheAddressFollowUp_IsUnknownNeverInvalid' <<<"$e2e" \
        || fail "end-to-end: the outage-at-the-follow-up test did not go red under the mutant (our SMTP-less DNS outage must not reach invalid)"
      echo "check-implicit-mx --mutant-partial: ok — treating an AAAA failure as 'absent' goes RED (a SERVFAIL of ours stated as invalid; declared=$declared failed=$failed), controls green" ;;
  esac
fi
