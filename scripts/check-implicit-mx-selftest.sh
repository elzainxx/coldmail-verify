#!/usr/bin/env bash
# check-implicit-mx-selftest — BLOCKING paired control for check-implicit-mx.sh (ADR-0020).
# Runs the gate against TWO mutant builds and requires each to go red in the right test with the right signature, controls green:
#   1. the address follow-up removed        → the pre-ADR-0020 behaviour: an A-only domain is `invalid / no_mx`;
#   2. any AAAA failure counted as "absent" → a SERVFAIL of ours is stated as `invalid / no_mx`.
# See the header of check-implicit-mx.sh for why this is a standing check and not a one-off demonstration.
set -euo pipefail
D="$(dirname "${BASH_SOURCE[0]}")"
bash "$D/check-implicit-mx.sh" --mutant-implicit
bash "$D/check-implicit-mx.sh" --mutant-partial
