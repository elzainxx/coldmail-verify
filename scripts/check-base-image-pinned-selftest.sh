#!/usr/bin/env bash
# check-base-image-pinned-selftest — the PAIRED CONTROL for check-base-image-pinned.sh.
# On a green tree the gate prints `ok`, which means every one of its failure branches is unexercised: it could be broken to
# accept everything and CI would stay green forever. This builds throwaway fixtures that trip each assertion in turn and
# checks BOTH the exit code and the message (a gate that fails for the wrong reason is not a control).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GATE="$HERE/scripts/check-base-image-pinned.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
D=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa   # 64 hex

mk() { # mk <from-line> <ruby-version-file-content> <ci-docker-build-line>
  printf '%s\nRUN true\n' "$1" > "$TMP/Dockerfile"
  printf '%b' "$2" > "$TMP/.ruby-version"
  printf 'jobs:\n  x:\n    steps:\n      - run: %s\n' "$3" > "$TMP/ci.yml"
}
run() { bash "$GATE" "$TMP/Dockerfile" "$TMP/.ruby-version" "$TMP/ci.yml" 2>&1; }

pass=0; failn=0
expect_ok()   { local out rc=0; out="$(run)" || rc=$?; if [ $rc -eq 0 ]; then pass=$((pass+1)); echo "  ok   $1"; else failn=$((failn+1)); echo "  BAD  $1 — expected exit 0, got $rc: $out"; fi; }
expect_fail() { local msg="$2" out rc=0; out="$(run)" || rc=$?
  if [ $rc -ne 0 ] && echo "$out" | grep -q -- "$msg"; then pass=$((pass+1)); echo "  ok   $1 (rejected: $msg)"
  else failn=$((failn+1)); echo "  BAD  $1 — expected a failure containing '$msg', got rc=$rc: $out"; fi; }

GOOD_FROM="FROM ruby:3.2.11-slim-bookworm@sha256:$D"
GOOD_BUILD='docker build --pull --no-cache -t x:ci verifier-node/truemail-rack'

mk "$GOOD_FROM" 'ruby-3.2.11\r\n' "$GOOD_BUILD";                                             expect_ok   "a correct tree passes (also with the CRLF the fork stores)"
mk "FROM ruby:3.2.11-slim-bookworm" 'ruby-3.2.11\n' "$GOOD_BUILD";                            expect_fail "a tag-only FROM (no digest)"      "not pinned by digest"
mk "FROM ruby:3.2.11-slim-bookworm@sha256:abc" 'ruby-3.2.11\n' "$GOOD_BUILD";                 expect_fail "a truncated digest"               "not pinned by digest"
mk "FROM ruby:3.2.0-slim-bullseye@sha256:$D" 'ruby-3.2.0\n' "$GOOD_BUILD";                    expect_fail "the original EOL Debian 11 base"  "end-of-life Debian"
mk "FROM ruby:3.2.11-slim@sha256:$D" 'ruby-3.2.11\n' "$GOOD_BUILD";                           expect_fail "no Debian codename at all"        "supported Debian release"
mk "$GOOD_FROM" 'ruby-3.2.0\n' "$GOOD_BUILD";                                                 expect_fail "Dockerfile vs .ruby-version drift" "does not match"
mk "$GOOD_FROM" 'ruby-3.2.1\n' "$GOOD_BUILD";                                                 expect_fail "3.2.1 must not satisfy 3.2.11 (prefix trap)" "does not match"
mk "$GOOD_FROM" 'ruby-3.2.11\n' 'docker build -t x:ci verifier-node/truemail-rack';           expect_fail "a CI build with neither flag"     "lacks --pull --no-cache"
mk "$GOOD_FROM" 'ruby-3.2.11\n' 'docker build --no-cache -t x:ci verifier-node/truemail-rack'; expect_fail "a CI build missing --pull"     "lacks --pull --no-cache"
mk "$GOOD_FROM" 'ruby-3.2.11\n' 'docker build --pull -t x:ci verifier-node/truemail-rack';    expect_fail "a CI build missing --no-cache"    "lacks --pull --no-cache"
mk "$GOOD_FROM" 'ruby-3.2.11\n' 'echo no build here';                                         expect_fail "a CI file with no docker build (vacuity)" "assertion 4 would be vacuous"

echo "check-base-image-pinned-selftest: $pass passed, $failn failed"
[ "$failn" -eq 0 ]
