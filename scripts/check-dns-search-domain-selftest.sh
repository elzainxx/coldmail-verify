#!/usr/bin/env bash
# check-dns-search-domain-selftest — BLOCKING paired control for check-dns-search-domain.sh (ADR-0019).
# Runs the gate against a MUTANT build (name-rooting disabled) and requires it to go red on the SERVFAIL case, canary green.
# See the header of check-dns-search-domain.sh for why this is a standing check and not a one-off demonstration.
set -euo pipefail
exec bash "$(dirname "${BASH_SOURCE[0]}")/check-dns-search-domain.sh" --mutant
