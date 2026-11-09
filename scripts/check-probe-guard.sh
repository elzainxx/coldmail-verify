#!/usr/bin/env bash
# check-probe-guard — BLOCKING CI gate (ADR-0021): the verifier node must never open an SMTP session to a non-public address.
# The guard (verifier-node/truemail-rack/app/truemail_server/wrapper/probe_guard.rb) decides which addresses are public; this gate
# proves it never ALLOWS one it should not, against an ORACLE that is not the guard's own table.
#
#   check-probe-guard.sh                  THREE phases, in digest-pinned containers:
#                                           1. Python generates the address set from ITS OWN transcription of the IANA special-purpose
#                                              registries — the first/last address of every block and their neighbours, plus ~50k
#                                              seeded random IPv4 and IPv6 addresses and IPv4-mapped forms;
#                                           2. the REAL guard module runs under the node image's own Ruby and decides every address;
#                                           3. Python judges every decision with `ipaddress.is_global` (an independent encoding of the
#                                              same registries). SOUNDNESS: wherever the oracle says "not globally routable" the guard
#                                              must say "refuse". The guard may over-refuse (multicast, 6to4, NAT64, unassigned IPv6 —
#                                              by design); it must never under-refuse.
#   check-probe-guard.sh --mutant-cgnat      the PAIRED CONTROLS (also scripts/check-probe-guard-selftest.sh). Each removes ONE range from a
#   check-probe-guard.sh --mutant-metadata   copy of the module (the mutation verified to have landed) and requires the gate to go RED
#   check-probe-guard.sh --mutant-rfc1918    with violations in exactly that range and no other. A gate that cannot fail is not a control.
#
# WHY THE TEST POINTS COME FROM THE ORACLE, NOT FROM THE MODULE. The first version generated the block edges from the guard's own table,
# so deleting a range from the guard also deleted that range's test points, and only random sampling could notice — which will almost
# never land in a /24. Edges are now generated from a list that does not depend on the code under test, and the mutants are required to be
# found by them. (Found by reading the mutant's violation count, 2, and asking why it was not 4+.)
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export MSYS_NO_PATHCONV=1   # git-bash on Windows must not rewrite the container-side paths below; a no-op on a Linux runner
hostpath() { if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf %s "$1"; fi; }

case "${1:-}" in
  "")                MODE=real ;;
  --mutant-cgnat)    MODE=mutant-cgnat ;;
  --mutant-metadata) MODE=mutant-metadata ;;
  --mutant-rfc1918)  MODE=mutant-rfc1918 ;;
  --mutant-firewall) MODE=mutant-firewall ;;
  *) echo "usage: check-probe-guard.sh [--mutant-cgnat|--mutant-metadata|--mutant-rfc1918|--mutant-firewall]" >&2; exit 2 ;;
esac
fail() { echo "check-probe-guard ($MODE) FAIL: $*" >&2; exit 1; }

RUBY_IMG="$(grep -m1 -E '^FROM ' "$HERE/verifier-node/truemail-rack/Dockerfile" | awk '{print $2}')"
case "$RUBY_IMG" in *@sha256:*) ;; *) fail "the Dockerfile's FROM ($RUBY_IMG) is not digest-pinned";; esac
PY_IMG="python:3.15.2-alpine@sha256:a0f2e268f2ce8f3709611a90054ec6a8ae31f6251f5896b8648b9bb99835e30a"   # the pin the DNS black-hole fixture uses

W="$HERE/.probe-guard-work"
rm -rf "$W"; mkdir -p "$W/m" "$W/out"
trap 'rm -rf "$W"' EXIT
MOD="$W/m/probe_guard.rb"
cp "$HERE/verifier-node/truemail-rack/app/truemail_server/wrapper/probe_guard.rb" "$MOD"

mutate() {  # mutate <sed-expr> <marker-that-must-appear>
  sed -i "$1" "$MOD"
  grep -q "$2" "$MOD" || fail "the mutation did not apply ($2 not found) — probe_guard.rb was reshaped, and this control would be testing the REAL code"
}
# The removed line becomes a comment ("|" is the sed delimiter, so the replacement text must not contain one).
case "$MODE" in
  mutant-cgnat)    mutate "s|^ *'100\.64\.0\.0/10' => 'cgnat',\$|        # MUTANT: cgnat range removed|" 'MUTANT: cgnat range removed' ;;
  mutant-metadata) mutate "s|^ *'169\.254\.0\.0/16' => 'link-local/cloud-metadata',\$|        # MUTANT: link-local range removed|" 'MUTANT: link-local range removed' ;;
  mutant-rfc1918)  mutate "s|^ *'172\.16\.0\.0/12' => 'private-rfc1918',\$|        # MUTANT: 172.16/12 range removed|" 'MUTANT: 172.16/12 range removed' ;;
esac

# The shipped egress ruleset (defence in depth: the layer DNS rebinding cannot race). A copy, so the firewall mutant never touches the repo.
NFT="$W/m/verifier_egress.nft"
cp "$HERE/verifier-node/egress/verifier_egress.nft" "$NFT"
if [ "$MODE" = mutant-firewall ]; then
  sed -i 's|100\.64\.0\.0/10, ||' "$NFT"
  ! grep -qF '100.64.0.0/10' "$NFT" || fail "the firewall mutation did not apply (100.64.0.0/10 is still in the ruleset)"
fi

# Phase 1 (Python): the address set, from the registry transcription below and a fixed seed (a red is reproducible).
cat >"$W/m/generate.py" <<'PY'
import ipaddress, random
V4 = """0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 169.254.0.0/16 172.16.0.0/12 192.0.0.0/24 192.0.0.0/29 192.0.0.8/32
192.0.0.9/32 192.0.0.10/32 192.0.0.170/31 192.0.2.0/24 192.31.196.0/24 192.52.193.0/24 192.88.99.0/24 192.168.0.0/16
192.175.48.0/24 198.18.0.0/15 198.51.100.0/24 203.0.113.0/24 224.0.0.0/4 240.0.0.0/4 255.255.255.255/32""".split()
V6 = """::/128 ::1/128 ::ffff:0:0/96 64:ff9b::/96 64:ff9b:1::/48 100::/64 2001::/23 2001:1::1/128 2001:1::2/128 2001:3::/32
2001:4:112::/48 2001:20::/28 2001:30::/28 2001:db8::/32 2002::/16 3fff::/20 5f00::/16 fc00::/7 fe80::/10 fec0::/10 ff00::/8
2000::/3""".split()
rng = random.Random(20261109)
out = []
def edges(cidr):
    n = ipaddress.ip_network(cidr)
    bits = 32 if n.version == 4 else 128
    lo, hi = int(n.network_address), int(n.broadcast_address)
    for x in (lo - 1, lo, (lo + hi) // 2, hi, hi + 1):
        if 0 <= x < 2 ** bits:
            out.append(str(ipaddress.ip_address(x) if n.version == 6 else ipaddress.IPv4Address(x)))
for c in V4 + V6:
    edges(c)
for _ in range(30000):
    out.append(str(ipaddress.IPv4Address(rng.getrandbits(32))))
for _ in range(20000):
    out.append(str(ipaddress.IPv6Address((rng.getrandbits(16) << 112) | rng.getrandbits(112))))
v4s = [a for a in out if '.' in a]
for a in v4s[::40]:
    out.append('::ffff:' + a)
for a in dict.fromkeys(out):
    print(a)
PY

# Phase 2 (Ruby): the REAL guard decides every address.  addr <TAB> label | "public"
cat >"$W/m/classify.rb" <<'RUBY'
require 'ipaddr'
require_relative 'probe_guard'
G = TruemailServer::Wrapper::ProbeGuard
n = 0
File.open('/out/decisions.tsv', 'w') do |f|
  File.foreach('/out/addrs.txt') { |line| a = line.strip; next if a.empty?; f.puts "#{a}\t#{G.non_public_range(a) || 'public'}"; n += 1 }
end
puts "decided #{n}"
# every range the guard refuses by CIDR (IPv6 outside 2000::/3 is "not global unicast" there, and `ip6 daddr != 2000::/3` in the firewall)
File.write('/out/guard_cidrs.txt', (G::V4_RANGES.keys + G::V6_SPECIAL_INSIDE_GLOBAL.keys).join("\n") + "\n")
RUBY

# Phase 3 (Python): judge every decision with the stdlib's registry encoding.
cat >"$W/m/oracle.py" <<'PY'
import ipaddress, sys
unsound, over, total = [], 0, 0
for line in open('/out/decisions.tsv'):
    addr, label = line.rstrip('\n').split('\t')
    ip = ipaddress.ip_address(addr)
    if ip.version == 6 and ip.ipv4_mapped is not None:   # judged as the IPv4 address it will connect to
        ip = ip.ipv4_mapped
    non_global = not ip.is_global
    total += 1
    if non_global and label == 'public':
        unsound.append((addr, str(ip)))
    if (not non_global) and label != 'public':
        over += 1
with open('/out/unsound.txt', 'w') as fh:
    for a, _ in unsound:
        fh.write(a + chr(10))
print(f"oracle: {total} decisions; UNSOUND (guard allows a non-global address) = {len(unsound)}; over-refusals (allowed, by design) = {over}")
for addr, shown in unsound[:12]:
    print(f"  UNSOUND {addr} (oracle: {shown} is not globally routable)")
sys.exit(1 if unsound else 0)
PY

docker run --rm -v "$(hostpath "$W")/m:/m:ro" -v "$(hostpath "$W")/out:/out" "$PY_IMG" sh -c 'python /m/generate.py >/out/addrs.txt' \
  || fail "the address generator failed"
gen="$(wc -l <"$W/out/addrs.txt" | tr -d ' ')"
[ "${gen:-0}" -gt 40000 ] || fail "only ${gen:-0} addresses were generated — the oracle comparison would be thin"
docker run --rm -v "$(hostpath "$W")/m:/m:ro" -v "$(hostpath "$W")/out:/out" "$RUBY_IMG" ruby /m/classify.rb >"$W/ruby.log" 2>&1 \
  || { cat "$W/ruby.log" >&2; fail "the guard module crashed under the node's Ruby"; }
decided="$(grep -o 'decided [0-9]*' "$W/ruby.log" | awk '{print $2}')"
[ "${decided:-0}" = "$gen" ] || fail "the guard decided ${decided:-0} of $gen addresses"


rc=0
out="$(docker run --rm -v "$(hostpath "$W")/m:/m:ro" -v "$(hostpath "$W")/out:/out" "$PY_IMG" python /m/oracle.py 2>&1)" || rc=$?
echo "check-probe-guard ($MODE): $gen addresses under $RUBY_IMG"
echo "$out" | head -16 | cut -c1-200

# FIREWALL DRIFT: every range the guard refuses must be in the shipped egress ruleset (exact CIDR tokens — a substring test would let
# 10.0.0.0/8 satisfy 0.0.0.0/8), IPv6 must keep its allow-list rule, and the file must be valid nftables.
grep -oE '[0-9a-fA-F:.]+/[0-9]+' "$NFT" | sort -u >"$W/out/nft_cidrs.txt"
missing="$(while IFS= read -r c; do [ -n "$c" ] && { grep -Fxq -- "$c" "$W/out/nft_cidrs.txt" || echo "$c"; }; done <"$W/out/guard_cidrs.txt")"
v6rule=ok; grep -Eq 'ip6 daddr != 2000::/3 .*reject' "$NFT" || v6rule=MISSING
nftrc=0
for attempt in 1 2 3; do   # apk fetches from a mirror; a transient network blip must not turn a gate red
  nftout="$(docker run --rm --cap-add NET_ADMIN -v "$(hostpath "$W")/m:/m:ro" "alpine@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf" \
            sh -c 'apk add --no-cache nftables >/dev/null 2>&1 && nft -c -f /m/verifier_egress.nft && echo NFT-SYNTAX-OK' 2>&1)" && nftrc=0 && break || nftrc=$?
done
echo "firewall: ranges missing from verifier_egress.nft: [${missing:-none}]; IPv6 allow-list rule: $v6rule; nft -c: $(grep -q NFT-SYNTAX-OK <<<"$nftout" && echo ok || echo "FAILED rc=$nftrc")"

if [ "$MODE" = mutant-firewall ]; then
  [ "$rc" -eq 0 ] || fail "the firewall mutant also broke the GUARD oracle — the fixture is unsound"
  [ "$missing" = "100.64.0.0/10" ] || fail "the firewall mutant (CGNAT removed from the ruleset) was not detected as exactly '100.64.0.0/10' missing (got: '${missing:-nothing}')"
  echo "check-probe-guard --mutant-firewall: ok — a range the guard refuses but the firewall ruleset lacks (100.64.0.0/10) is detected; the guard itself is untouched"
elif [ "$MODE" = real ]; then
  [ "$rc" -eq 0 ] || fail "the guard ALLOWS an address the independent oracle says is not globally routable — the node could be steered at an internal host"
  [ -z "$missing" ] || fail "the egress ruleset verifier_egress.nft lacks ranges the guard refuses: $(echo $missing) — the firewall layer has drifted from the guard"
  [ "$v6rule" = ok ] || fail "verifier_egress.nft lost its IPv6 allow-list rule (ip6 daddr != 2000::/3 ... reject)"
  grep -q NFT-SYNTAX-OK <<<"$nftout" || { echo "$nftout" | tail -3 >&2; fail "verifier_egress.nft is not valid nftables (nft -c)"; }
  echo "check-probe-guard: ok — over $gen addresses (every special-block edge, midpoint and neighbour from the registry list, ~50k random IPv4/IPv6, IPv4-mapped forms) the guard never allows a non-global address"
else
  want="${MODE#mutant-}"
  [ "$rc" -ne 0 ] || fail "the $MODE mutant PASSED the oracle: this gate cannot detect the range it exists to enforce"
  # every unsound address the oracle found is in $W/out/unsound.txt, one per line
  case "$want" in
    cgnat)    in_range='^100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.'; edge_first='^100\.64\.0\.0$';  edge_last='^100\.127\.255\.255$' ;;
    metadata) in_range='^169\.254\.';                                    edge_first='^169\.254\.0\.0$'; edge_last='^169\.254\.255\.255$' ;;
    rfc1918)  in_range='^172\.(1[6-9]|2[0-9]|3[01])\.';                  edge_first='^172\.16\.0\.0$';  edge_last='^172\.31\.255\.255$' ;;
  esac
  [ -s "$W/out/unsound.txt" ] || fail "the oracle reported failure but wrote no violations"
  grep -Eq -- "$in_range" "$W/out/unsound.txt" || fail "the $MODE mutant went red, but not with violations in the range it removed — it failed for some other reason"
  # the registry-derived EDGES of the removed range must be among the violations (random sampling alone cannot find a small block)
  grep -Eq -- "$edge_first" "$W/out/unsound.txt" && grep -Eq -- "$edge_last" "$W/out/unsound.txt" \
    || fail "the $MODE mutant was not caught AT THE EDGES of the removed range (first and last address) — the test points are not independent of the code"
  # nothing OUTSIDE the removed range may be unsound (the other ranges are intact)
  other="$(grep -Ev -- "$in_range" "$W/out/unsound.txt" || true)"
  [ -z "$other" ] || { echo "$other" | head -3 >&2; fail "the $MODE mutant produced violations OUTSIDE the removed range — the fixture is unsound, so its red means nothing"; }
  echo "check-probe-guard --$MODE: ok — with the $want range removed the oracle finds it, at the registry-derived edges as well as at random ($(wc -l <"$W/out/unsound.txt" | tr -d ' ') violations, all inside that range)"
fi
