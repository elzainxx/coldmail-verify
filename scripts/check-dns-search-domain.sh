#!/usr/bin/env bash
# check-dns-search-domain — BLOCKING CI gate (ADR-0019): module 1's DNS must behave the same on a host whose
# /etc/resolv.conf has a `search` domain as on one that has none. A DNS failure of OURS is `unknown`, never `invalid`.
#
#   check-dns-search-domain.sh            runs the REAL code: the layers' DNS tests must pass inside a container whose
#                                         resolv.conf carries a search domain.
#   check-dns-search-domain.sh --mutant   the PAIRED CONTROL (also scripts/check-dns-search-domain-selftest.sh). Builds the
#                                         same package with name-rooting DISABLED, runs the same container, and requires it
#                                         to go RED — the SERVFAIL case failing with the collapse signature, the canary still
#                                         green. A fixture that cannot fail is not a control; this keeps "reds without the
#                                         fix" true on every run and not only on the day it was written.
#
# THE DEFECT. Go's resolver, given a name without a trailing dot, walks the resolv.conf search list and returns the error of
# the LAST name it tried, so a SERVFAIL for the real domain is overwritten by the NXDOMAIN of the suffixed one. asDNSNotFound
# then reads "this domain does not exist", resolveMX returns mxNone, and the MX layer reports Fail: `invalid / no_mx` for an
# address whose only fault is that OUR resolution failed. The dev host has no search domain; a cloud VM does — so nothing in a
# local run could ever see it. (Module 3's From-gate had the same bug, found by its first GitHub Actions run.)
#
# NO SERVER TO START. The DNS server is in the test binary (internal/layers/dns_realdns_test.go, golang.org/x/net/dnsmessage),
# on loopback inside the container; its zone answers NXDOMAIN for everything it does not list, so a search-suffixed name
# (servfail.test.search-domain.test.) is NXDOMAIN with no internet and no flakiness. Needs only Go and Docker.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export MSYS_NO_PATHCONV=1   # git-bash on Windows must not rewrite the container-side paths below; a no-op on a Linux runner
# The path Docker needs for a bind mount: native on Windows (cygpath), unchanged elsewhere.
hostpath() { if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf %s "$1"; fi; }
MODE=real; [ "${1:-}" = "--mutant" ] && MODE=mutant
IMG="alpine@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf"   # alpine:3.24
RUN='TestMXLayer_RealDNS|TestNetResolver|TestFQDN|TestAsDNSNotFound|TestSearchDomainCanary'

# Under the checkout, not /tmp: the path must be valid for the Docker daemon on a runner and inside a CI replica alike.
W="$HERE/.dns-fixture-work"
rm -rf "$W"; mkdir -p "$W/control-plane"
trap 'rm -rf "$W"' EXIT
fail() { echo "check-dns-search-domain ($MODE) FAIL: $*" >&2; exit 1; }

cp -r "$HERE/control-plane/." "$W/control-plane/"
if [ "$MODE" = mutant ]; then
  # Disable the ONE line that roots a name. If it is renamed the mutation silently does nothing and the "mutant" would be the
  # real code — so the mutation is verified to have landed, not assumed.
  sed -i 's|^\treturn name + "\."$|\treturn name // MUTANT: name-rooting disabled|' "$W/control-plane/internal/layers/resolver.go"
  grep -q 'MUTANT: name-rooting disabled' "$W/control-plane/internal/layers/resolver.go" \
    || fail "the mutation did not apply (resolver.go's fqdn() no longer ends in 'return name + \".\"'?) — this control would be testing the real code"
fi
( cd "$W/control-plane" && CGO_ENABLED=0 GOTOOLCHAIN=local go test -c -o "$W/layers.test" ./internal/layers ) \
  || fail "could not build the layers test binary"

# Listed inside the container too: the binary is a Linux binary, so this works on any host that can run Docker.
listing="$(docker run --rm -v "$(hostpath "$W")/layers.test:/layers.test:ro" "$IMG" /layers.test -test.list "$RUN")" \
  || fail "could not list the tests in the built binary"
declared="$(grep -c "^Test" <<<"$listing" || true)"
[ "${declared:-0}" -gt 0 ] || fail "the test binary declares no DNS tests matching $RUN"

printf 'nameserver 127.0.0.1\nsearch search-domain.test\noptions ndots:1\n' >"$W/resolv.conf"

rc=0
out="$(docker run --rm -e LAYERS_EXPECT_SEARCH_DOMAIN=1 \
  -v "$(hostpath "$W")/resolv.conf:/etc/resolv.conf:ro" -v "$(hostpath "$W")/layers.test:/layers.test:ro" \
  "$IMG" /layers.test -test.v -test.count=1 -test.timeout=120s -test.run "$RUN" 2>&1)" || rc=$?

# Verdicts read $out through here-strings, never `echo "$out" | grep -q` (an early-exit pipe under pipefail).
passed="$(grep -c '^--- PASS: ' <<<"$out" || true)"
failed="$(grep -c '^--- FAIL: ' <<<"$out" || true)"
skipped="$(grep -c '^--- SKIP: ' <<<"$out" || true)"
echo "check-dns-search-domain ($MODE): search domain injected; declared=$declared passed=$passed failed=$failed skipped=$skipped rc=$rc"
grep -E 'canary:|^--- (FAIL|SKIP)' <<<"$out" | cut -c1-230 || true

if [ "$MODE" = real ]; then
  [ "$rc" -eq 0 ]               || { grep -E '_test.go:[0-9]+:' <<<"$out" | head -20 >&2 || true; fail "a DNS test failed under a resolv.conf search domain (rc=$rc): module 1's DNS handling depends on the host's search list"; }
  [ "$failed" -eq 0 ]           || fail "$failed test(s) failed under a search domain"
  [ "$skipped" -eq 0 ]          || fail "$skipped test(s) skipped — a skip is a shortfall, not a pass"
  [ "$passed" -eq "$declared" ] || fail "only $passed of $declared declared tests passed"
  grep -q 'canary: search list .* is live' <<<"$out" || fail "the canary did not confirm a live search list — the fixture's proof that it exercises the hazard is missing"
  echo "check-dns-search-domain: ok — all $declared DNS tests pass under a resolv.conf search domain, and the canary confirms the hazard is live there"
else
  # The mutant must go red, and for the RIGHT reason: the SERVFAIL case failing (the layer reported a verdict-bearing
  # Fail / the resolver asked a suffixed name), while the canary — which asserts on Go's raw resolver — still passes,
  # proving the fixture itself is sound.
  [ "$rc" -ne 0 ] || fail "the mutant (name-rooting disabled) PASSED under a search domain: this fixture cannot detect the defect it exists for"
  grep -q -- '--- FAIL: TestMXLayer_RealDNS_FailureIsUnknownNeverInvalid_AndOnlyTheRootedNameIsAsked' <<<"$out" \
    || fail "the mutant went red, but not in the MX-layer DNS test — it failed for some other reason, which proves nothing about this defect"
  # Since ADR-0020 the address follow-up masks the collapse for a plain SERVFAIL (Go prefers the ORIGINAL name's error for address
  # lookups), so the verdict-level signature lives on the shape that still collapses: an MX SERVFAIL on a name with no address
  # records (zone entry mxservfail-noaddr.test.). The plain SERVFAIL row must still go red, by its wire assertion.
  grep -q 'MX query that SERVFAILs on a name with no address is unknown: MXLayer result="fail"' <<<"$out" \
    || fail "the MX-SERVFAIL-with-no-address case failed without the collapse signature (MXLayer reporting Fail for a SERVFAIL)"
  grep -q 'SERVFAIL is unknown, never invalid: the resolver asked "[A-Z]* servfail.test.search-domain.test."' <<<"$out" \
    || fail "the SERVFAIL case did not show the search-suffixed query that is the defect on the wire"
  grep -q -- '--- PASS: TestSearchDomainCanary' <<<"$out" \
    || fail "the canary did not pass under the mutant — the fixture itself is unsound, so its red means nothing"
  echo "check-dns-search-domain --mutant: ok — with name-rooting disabled the fixture goes RED (SERVFAIL read as invalid; declared=$declared failed=$failed), canary green — the fixture detects the defect"
fi
