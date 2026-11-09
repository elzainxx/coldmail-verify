#!/usr/bin/env bash
# check-resolver-hygiene-selftest — BLOCKING paired control for check-resolver-hygiene.sh (ADR-0019 addendum).
# Runs the gate against THREE mutants of the check — the `search`/`domain` rule, the dotted-hostname rule and the ndots rule, each
# disabled in turn — and requires every one to go red, for the right rule only; the search and hostname mutants must also be caught
# by the ORACLE (Ruby's own resolver), not only by the expected-verdict table.
# See the header of check-resolver-hygiene.sh for why this is a standing check and not a one-off demonstration.
set -euo pipefail
D="$(dirname "${BASH_SOURCE[0]}")"
bash "$D/check-resolver-hygiene.sh" --mutant-search
bash "$D/check-resolver-hygiene.sh" --mutant-hostname
bash "$D/check-resolver-hygiene.sh" --mutant-ndots
