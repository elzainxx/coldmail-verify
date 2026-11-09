#!/usr/bin/env bash
# check-probe-guard-selftest — BLOCKING paired control for check-probe-guard.sh (ADR-0021).
# Runs the gate against FOUR mutants (three of the guard, one of the shipped egress firewall ruleset) — CGNAT (the Alibaba-metadata block), link-local (169.254.169.254, AWS/GCP/Azure
# metadata) and RFC 1918's 172.16/12 (the one block whose edges are not a whole octet) — each removed in turn, and requires every one
# to go red with violations in exactly that range and no other (the firewall mutant: CGNAT missing from the nft ruleset, detected as exactly that).
# See the header of check-probe-guard.sh for why this is a standing check and not a one-off demonstration.
set -euo pipefail
D="$(dirname "${BASH_SOURCE[0]}")"
bash "$D/check-probe-guard.sh" --mutant-cgnat
bash "$D/check-probe-guard.sh" --mutant-metadata
bash "$D/check-probe-guard.sh" --mutant-rfc1918
bash "$D/check-probe-guard.sh" --mutant-firewall
