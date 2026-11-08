#!/usr/bin/env bash
# check-base-image-pinned — BLOCKING CI gate. The verifier-node image's base must be (1) pinned by DIGEST, (2) on a supported
# Debian release, (3) at the Ruby patch the fork's .ruby-version names — and every `docker build` in ci.yml must pass
# `--pull --no-cache`.
#
# THE DEFECT THIS GUARDS (found 2026-11-08, the first time GitHub Actions built the image from scratch): the Dockerfile's
# base was `ruby:3.2.0-slim`, a Debian 11 (bullseye) image. Debian removed bullseye's `debian-security` repository, so
# `apt-get install` returned 404 — but only for a build that did not reuse a cached layer. Every local build reused one,
# so the image looked fine for months while being impossible to build. A dead base image is invisible exactly as long as a
# cache is warm; it is a date-triggered failure, like the verification_raw partition cliff (ADR-0017).
#
# WHAT IT ASSERTS, and why each:
#   1. FROM carries `@sha256:<64 hex>`  — a tag is a moving target, a digest is a content address; without it "the same
#      image" is a claim about a tag.
#   2. FROM names no end-of-life Debian codename (bullseye/buster/stretch/jessie) and DOES name a supported one
#      (bookworm/trixie) — so the pin is auditable at a glance and a downgrade is a one-line red.
#   3. FROM's Ruby tag equals the patch in the fork's .ruby-version — the Gemfile reads that file and pins Ruby EXACTLY, so
#      the two drifting apart is a build failure waiting for the next from-scratch build.
#   4. every `docker build` in ci.yml uses `--pull --no-cache` — the actual control: CI on a real runner starts with no cache,
#      and this keeps it that way even if someone "optimises" the workflow.
# Paired control: scripts/check-base-image-pinned-selftest.sh proves each assertion can FAIL.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DOCKERFILE="${1:-$HERE/verifier-node/truemail-rack/Dockerfile}"
RUBYVER="${2:-$HERE/verifier-node/truemail-rack/.ruby-version}"
CI="${3:-$HERE/.github/workflows/ci.yml}"

fail=0
bad() { echo "FAIL (base-image-pinned): $*" >&2; fail=1; }

from="$(grep -E '^FROM[[:space:]]' "$DOCKERFILE" | head -1 | tr -d '\r' || true)"
[ -n "$from" ] || bad "no FROM line in $DOCKERFILE"

# 1. digest
echo "$from" | grep -Eq '@sha256:[0-9a-f]{64}([[:space:]]|$)' || bad "FROM is not pinned by digest (@sha256:<64 hex>): $from"

# 2. supported Debian
if echo "$from" | grep -Eqi '(bullseye|buster|stretch|jessie)'; then bad "FROM names an end-of-life Debian release: $from"; fi
echo "$from" | grep -Eq '(bookworm|trixie)' || bad "FROM does not name a supported Debian release (bookworm or trixie), so the pin cannot be audited: $from"

# 3. Ruby patch == .ruby-version
want="$(tr -d '\r\n' < "$RUBYVER" | sed -n 's/^ruby-//p')"
[ -n "$want" ] || bad "could not read a version from $RUBYVER"
esc="${want//./\\.}"
echo "$from" | grep -Eq "ruby:${esc}-" || bad "FROM's Ruby tag does not match $RUBYVER (wants ruby-${want:-?}): $from"

# 4. CI builds without a cache
n=0
while IFS= read -r l; do
  n=$((n+1))
  if ! { echo "$l" | grep -q -- '--pull' && echo "$l" | grep -q -- '--no-cache'; }; then
    bad "a 'docker build' in $(basename "$CI") lacks --pull --no-cache (a warm cache can hide a dead base image): $(echo "$l" | sed 's/^[[:space:]]*//')"
  fi
done < <(grep -E 'docker build ' "$CI" | grep -v -E '^[[:space:]]*#' || true)
[ "$n" -gt 0 ] || bad "found no 'docker build' line in $CI — assertion 4 would be vacuous"

[ "$fail" -eq 0 ] || exit 1
echo "base-image-pinned: ok — $(echo "$from" | sed 's/@sha256:\(.\{12\}\).*/@sha256:\1…/'); Ruby ${want} matches .ruby-version; $n CI docker build(s) use --pull --no-cache."
