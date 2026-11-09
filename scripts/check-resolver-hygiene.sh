#!/usr/bin/env bash
# check-resolver-hygiene — BLOCKING CI gate (ADR-0019 addendum): the verifier node's host audit goes RED when its resolver is not
# clean, so the brain routes no SMTP to it. "A documented deployment rule is not a control" — this is the control.
#
#   check-resolver-hygiene.sh                  runs the REAL fork code (verifier-node/truemail-rack/app/.../resolver_hygiene.rb) under
#                                              the REAL Ruby of the node image (the digest-pinned base from the Dockerfile), in
#                                              containers whose /etc/resolv.conf and hostname are the fixture's. Two assertions:
#                                                1. every case's verdict is the expected one (clean vs red), and
#                                                2. SOUNDNESS against an ORACLE: wherever Ruby's own Resolv::DNS would actually use a
#                                                   search domain (in either of truemail's two modes — resolv.conf, or `dns:` servers
#                                                   with a hostname-derived search), the check must be red. The rule may over-approximate
#                                                   (ndots, a search line when `dns:` is set); it must never MISS a search domain.
#   check-resolver-hygiene.sh --mutant-search      the PAIRED CONTROLS (also scripts/check-resolver-hygiene-selftest.sh). Each disables ONE
#   check-resolver-hygiene.sh --mutant-hostname    rule in a copy of the module (the mutation is verified to have landed) and requires the
#   check-resolver-hygiene.sh --mutant-ndots       gate to go RED for that rule, and ONLY the cases that rule governs to fail. A fixture
#                                                  that cannot fail is not a control; this keeps "reds without the check" true on every run.
#
# WHY THE RSPEC DOES NOT DO THIS. The specs (spec/truemail_server/wrapper/resolver_hygiene_spec.rb) inject their inputs and must: a
# GitHub runner's container has a `search` line, so a spec that read the machine would be red there and green on a developer's PC.
# This gate is the other half: the real file, the real hostname, the real resolver.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export MSYS_NO_PATHCONV=1   # git-bash on Windows must not rewrite the container-side paths below; a no-op on a Linux runner
hostpath() { if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf %s "$1"; fi; }

case "${1:-}" in
  "")                 MODE=real ;;
  --mutant-search)    MODE=mutant-search ;;
  --mutant-hostname)  MODE=mutant-hostname ;;
  --mutant-ndots)     MODE=mutant-ndots ;;
  *) echo "usage: check-resolver-hygiene.sh [--mutant-search|--mutant-hostname|--mutant-ndots]" >&2; exit 2 ;;
esac
fail() { echo "check-resolver-hygiene ($MODE) FAIL: $*" >&2; exit 1; }

# The node image's own Ruby: the FROM line of the Dockerfile (check-base-image-pinned.sh keeps it digest-pinned and supported).
IMG="$(grep -m1 -E '^FROM ' "$HERE/verifier-node/truemail-rack/Dockerfile" | awk '{print $2}')"
case "$IMG" in *@sha256:*) ;; *) fail "the Dockerfile's FROM ($IMG) is not digest-pinned — check-base-image-pinned.sh should have caught this";; esac

# Under the checkout, not /tmp: the path must be valid for the Docker daemon on a runner and inside a CI replica alike.
W="$HERE/.resolver-hygiene-work"
rm -rf "$W"; mkdir -p "$W/m"
trap 'rm -rf "$W"' EXIT
MOD="$W/m/resolver_hygiene.rb"
cp "$HERE/verifier-node/truemail-rack/app/truemail_server/wrapper/resolver_hygiene.rb" "$MOD"

mutate() {  # mutate <sed-expr> <marker-that-must-appear> — verifies the mutation landed instead of assuming it
  sed -i "$1" "$MOD"
  grep -q "$2" "$MOD" || fail "the mutation did not apply ($2 not found) — resolver_hygiene.rb was reshaped, and this control would be testing the REAL code"
}
case "$MODE" in
  mutant-search)   mutate "s|^\( *\)when 'search' then .*\$|\1when 'search' then nil # MUTANT: search line ignored|" 'MUTANT: search line ignored' ;;
  mutant-hostname) mutate "s|if host.to_s.include?('.')\$|if false # MUTANT: dotted hostname ignored|" 'MUTANT: dotted hostname ignored' ;;
  mutant-ndots)    mutate "s|if m\[1\].to_i > 1\$|if false # MUTANT: ndots ignored|" 'MUTANT: ndots ignored' ;;
esac

# The probe: the check as the node runs it (real file, real hostname), plus the ORACLE — Ruby's own resolver, asked which names it
# would try for a relative name, in both of truemail's modes (Truemail::Dns::Worker passes nil for no `dns:`, else a Hash).
cat >"$W/m/probe.rb" <<'RUBY'
require 'resolv'
require_relative 'resolver_hygiene'
def search_in_effect?(info)
  cfg = Resolv::DNS::Config.new(info)
  cfg.lazy_initialize
  cfg.generate_candidates('probe.invalid').map(&:to_s).uniq.size > 1
end
msg = TruemailServer::Wrapper::ResolverHygiene.check
puts "VERDICT=#{msg ? 'red' : 'clean'} ORACLE_DEFAULT=#{search_in_effect?(nil)} ORACLE_DNS=#{search_in_effect?({ nameserver_port: [['127.0.0.1', 53]] })}"
puts "MESSAGE=#{msg}" if msg
RUBY

mkdir -p "$W/conf"
printf 'nameserver 127.0.0.1\n'                                   >"$W/conf/clean.conf"
printf 'nameserver 127.0.0.1\nsearch corp.example\n'              >"$W/conf/search.conf"
printf 'nameserver 127.0.0.1\ndomain corp.example\n'              >"$W/conf/domain.conf"
printf 'nameserver 127.0.0.1\noptions ndots:2\n'                  >"$W/conf/ndots2.conf"
printf 'nameserver 127.0.0.1\noptions ndots:1 timeout:2\n'        >"$W/conf/ndots1.conf"
printf '# search corp.example\n; domain corp.example\nnameserver 127.0.0.1\n' >"$W/conf/commented.conf"

# case: <name> <conf> <hostname> <expected verdict> <the rule that governs it: search|hostname|ndots|none>
CASES=(
  "clean-short            clean.conf     box                  clean none"
  "commented-short        commented.conf box                  clean none"
  "ndots1-short           ndots1.conf    box                  clean none"
  "search-short           search.conf    box                  red   search"
  "domain-short           domain.conf    box                  red   search"
  "ndots2-short           ndots2.conf    box                  red   ndots"
  "clean-dotted-hostname  clean.conf     verifier.example.com red   hostname"
  "search-dotted-hostname search.conf    verifier.example.com red   search"
)

problems=0; unsound=0; wrong_rules=""
for c in "${CASES[@]}"; do
  read -r name conf host want rule <<<"$c"
  out="$(docker run --rm --hostname "$host" -v "$(hostpath "$W")/conf/$conf:/etc/resolv.conf:ro" -v "$(hostpath "$W")/m:/m:ro" "$IMG" ruby /m/probe.rb 2>&1)" \
    || fail "case $name: the probe crashed under the node's Ruby — $(head -3 <<<"$out" | tr '\n' ' ')"
  verdict="$(grep -o 'VERDICT=[a-z]*' <<<"$out" | cut -d= -f2)"
  o_def="$(grep -o 'ORACLE_DEFAULT=[a-z]*' <<<"$out" | cut -d= -f2)"
  o_dns="$(grep -o 'ORACLE_DNS=[a-z]*' <<<"$out" | cut -d= -f2)"
  [ -n "$verdict" ] && [ -n "$o_def" ] && [ -n "$o_dns" ] || fail "case $name: could not read the probe's output: $out"
  line="  $name: verdict=$verdict (want $want) oracle[default=$o_def dns-mode=$o_dns]"
  bad=""
  if [ "$verdict" != "$want" ]; then bad="verdict $verdict, want $want"; problems=$((problems+1)); wrong_rules="$wrong_rules $rule"; fi
  if { [ "$o_def" = true ] || [ "$o_dns" = true ]; } && [ "$verdict" != red ]; then
    bad="${bad:+$bad; }UNSOUND — Ruby's resolver WOULD use a search domain here and the check says clean"; unsound=$((unsound+1)); problems=$((problems+1)); wrong_rules="$wrong_rules $rule"
  fi
  echo "$line${bad:+   <-- $bad}"
done
echo "check-resolver-hygiene ($MODE): ${#CASES[@]} cases against $IMG; problems=$problems (unsound=$unsound)"

if [ "$MODE" = real ]; then
  [ "$problems" -eq 0 ] || fail "$problems case(s) wrong — the node's host audit would not go red for a resolver that is not clean"
  echo "check-resolver-hygiene: ok — the check is clean on a clean resolver and red on a search line, a domain line, ndots > 1 and a dotted hostname, and never misses a search domain Ruby's own resolver would use"
else
  # A mutant must go red, for the RIGHT rule: every failing case is governed by the disabled rule, and at least one fails.
  want_rule="${MODE#mutant-}"
  [ "$problems" -gt 0 ] || fail "the mutant ($MODE) PASSED all cases: this gate cannot detect the rule it exists to enforce"
  for r in $wrong_rules; do
    [ "$r" = "$want_rule" ] || fail "the $MODE mutant broke a case governed by '$r', not '$want_rule' — the fixture is unsound, so its red means nothing"
  done
  if [ "$want_rule" = search ] || [ "$want_rule" = hostname ]; then
    [ "$unsound" -gt 0 ] || fail "the $MODE mutant went red but the ORACLE did not catch it — the soundness check is not doing its job"
  fi
  echo "check-resolver-hygiene --$MODE: ok — with the $want_rule rule disabled the gate goes RED ($problems case(s), $unsound caught by the oracle), and nothing else breaks"
fi
