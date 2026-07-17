# Phase 3 — status: first real box (local build scorecard)

**Date:** 2026-07-17 · **Phase:** 3 (first real box) · **Spec:** [`phase-3-first-real-box-spec.md`](phase-3-first-real-box-spec.md) (the authority)
**Status: ✅ COMPLETE (local build)** — the full first-real-box machinery is built and test-proven
against a simulated/local box. **The system is one config flip + the cutover checklist away from a
real box** ([`phase-3-cutover-runbook.md`](phase-3-cutover-runbook.md)). Every real-IP-dependent
check is honestly marked **PENDING-REAL-BOX** with its verification command — never faked green.

**What Phase 3 is:** provisioning design + the `/audit` go/no-go gate + the GAP-24 four-way IP
invariant + selective go-live routing + the warmup ramp + the append-only `node_audits` history +
a minimal scheduled re-audit + the mock→real config-only seam. **No brain verdict logic changed**
(P2 proved the SMTP path is config-flip-ready); Phase 3 adds a router candidacy filter, a re-audit
scheduler, and infrastructure.

**Prerequisite gate (checked before starting):** ✅ Phase 2 committed green (`d1184c4`); ADR-0008
ratified (DECISIONS §6.2). Host procurement + the admin-audit-log go-live gate are **cutover gates,
not build prerequisites** (they gate the real-box cutover, not this local build — §"Cutover gates").

---

## The one thing Phase 3 could NOT change (and didn't): the verifier-node

Phase 3 is **the least Door-5-risky phase** — it deploys the **P2 fork + gem byte-identical** and
adds only brain-side code + infrastructure. The wrapper endpoints Phase 3 *consumes* — `GET
/v1/audit` (host_audit) and `GET /v1/config` (which already exposes `validation_type_for` as
`validation_type_by_domain`) — were **built in Phase 2**. **Zero verifier-node changes were needed.**
The gem-unmodified CI check still passes; `FORK.md` is unchanged.

---

## Build increments (all local, all test-proven)

| # | Increment | Status | Evidence |
|---|---|---|---|
| 1 | **Migration 0007** — `node_audits` (append-only history, OQ-P3-5) + `node_warmup` (durable counter, §5.2) + `nodes` UPDATE grant | ✅ | `migrations/0007_node_audits_warmup.sql`; applied to real PG 16; grants proven live (below). |
| 2 | **Store layer** — `NodeAuditRow`/`NodeHealth`/`WarmupDecision` + PG + Mem (audit history, health r/w, atomic warmup reserve) | ✅ | `internal/store/{store,mem,postgres}.go`; 3 new PG-gated tests pass against real PG 16. |
| 3 | **`internal/audit`** — green-derive, GAP-24 cross-check, the LOCKED eligibility rule, the lifecycle state machine | ✅ | `audit.go` + `audit_test.go`; GAP-24 + GAP-15 failing-cases **fail-verified** (neuter → test fires). |
| 4 | **`node.AuditClient`** — real `GET /v1/audit` + the simulated audit surface (green/ptr/dns/multi-homed) | ✅ | `node/audit_client.go`; `SimAuditSurface` is the local "controllable host_audit inputs". |
| 5 | **Router candidacy filter** (§4.4, the phase's spine) — `SMTPRoute` layers eligibility on the enabled filter; `Strategy` interface UNCHANGED (Door 5) | ✅ | `router/router.go` + `router_test.go`; red / port25-closed / egress-mismatch / no-eligible-node all proven. |
| 6 | **Orchestrator** — `smtp_skipped_reason` on all skip paths, the PB1 non-cacheable rule, `verifier_ip` from the registry, warmup reserve | ✅ | `orchestrator/{orchestrator,smtp_stage}.go` + tests; PB1 **fail-verified**; the P2 behavioral matrix still green under the new gate. |
| 7 | **`validation_type_for` render + drift check** (§5.1, GAP-19/OQ-P3-8) — one policy source, generated engine backstop, equality alarm | ✅ | `internal/provrouting/` + tests; wired into `buildSMTP` (logged drift alarm). |
| 8 | **Minimal scheduled re-audit + auto-quarantine** (§4.5, NOT the P4 watcher) | ✅ | `audit/scheduler.go` + `scheduler_test.go`; full green→red→green transition cycle proven on the sim surface. |
| 9 | **Config dials + main wiring** — `TM_NODE_PUBLIC_EGRESS_IP`, warmup/cadence/recover dials, node register-at-boot, re-audit ticker | ✅ | `config/config.go`, `cmd/control-plane/main.go`; config-reference regenerated (235 knobs). |
| 10 | **Docs + ADR-0009 + this scorecard + cutover runbook** | ✅ | ADR-0009, this doc, the runbook, config-manifest §3.5, ground-truth (host_audit anchor GT-4). |

---

## The `/audit` gate — the phase's spine (highest rigor, §4.4)

> **RULE (LOCKED, §4.4): a node is `:smtp`-eligible IFF `status==active` AND `audit_status==green`
> AND `port25_open` AND audit-fresh AND (GAP-24) the audit ran on the node's expected public egress
> IP.** A red / stale / egress-mismatched / port25-closed node is excluded from `:smtp` candidacy.

- **Where it lives:** `internal/audit.Eligibility` (the pure rule) → consumed by the router's
  `SMTPRoute` candidacy filter (per configured node) → consumed by the orchestrator SMTP stage.
- **GAP-15 built:** `audit_status==green` and `port25_open` are **separate** eligibility inputs. A
  green box with egress-25 blocked is **NOT eligible** (`TestEligibility_Port25ClosedNotEligible_GAP15`).
- **GAP-24 built + runtime-enforced (ADR-0009):** the eligibility gate AND the scheduler both refuse
  a green whose `current_host_ip` ≠ the expected public egress IP. A multi-homed box that audited its
  private interface never serves `:smtp` and never promotes. Fail-verified.
- **OQ-P3-6:** no eligible node → the address **degrades to the cheap verdict** (`risky/plausible` /
  `provider_no_smtp`), **never `invalid`**, `verifier_ip=null`, with an additive `smtp_skipped_reason`.
- **PB1:** a capacity-gap fallback (no eligible node / audit red / warmup cap) is **provisional →
  non-cacheable**; `provider_mx_routed` (settled routing) stays cacheable. Fail-verified.

---

## Simulated audit surface — the local "real box" (no real IP)

The kickoff mandates: construct controllable `host_audit`-shaped inputs so every gate + invariant is
exercised without a real IP. `node.SimAuditSurface` + the `Sim*` constructors provide exactly that:

| Constructor | Models | Proves |
|---|---|---|
| `SimGreen(ip)` | `warnings=={}` on `ip` | promotes provisioning→active (when `ip`==expected egress ∧ port25) |
| `SimPTRWarning` | `warnings:{ptr:…}` | red → quarantine on first red (§4.5) |
| `SimDNSWarning` | `warnings:{dns:…}` | red → quarantine; A-record misalignment |
| `SimMultiHomedMismatch(privateIP)` | green on the **private** IP | **GAP-24** — does NOT promote (the silent-poisoning trap) |
| `SetError(url, …)` | unreachable box | fetch failure records nothing, leaves state (not a fabricated green) |

---

## PENDING-REAL-BOX (never faked green — the honest ledger)

Each item is **built, wired, and tested** in logic; only the real-IP *value/proof* is deferred. The
exact verification command is recorded so the cutover is mechanical. See the runbook for the full list.

| Item | What's built now | PENDING-REAL-BOX proof (the command) |
|---|---|---|
| **Outbound port-25 works** (GAP-15) | `port25_open` eligibility input + the egress-smoke-test capability shape; false in the local build → no node eligible | On the trial box: `nc -vz <external-mx> 25` (or a `swaks --server <mx>:25 --quit-after banner`) establishes → set `TM_… port25_open=true` |
| **Real PTR / rDNS** | the `/audit` gate reads `warnings` (green iff `{}`); proven vs `dns_mock` + the sim surface | `dig -x <public-egress-ip> +short` == verifier FQDN; then `GET /v1/audit` → `warnings:{}` |
| **Real egress IP == audited IP** (GAP-24) | the four-way cross-check (gate + scheduler), fail-verified | `curl ifconfig.me` (egress) == `dig +short <verifier-fqdn>` (A) == `dig -x` PTR == `host_audit.current_host_ip` == `TM_NODE_PUBLIC_EGRESS_IP` |
| **Clean IP** | (procurement) — the audit doesn't check blocklists (GAP-17 sibling) | Spamhaus/passive check **before** warmup; documented in the runbook |
| **SPF published** (GAP-17) | out-of-band checklist item (host_audit does NOT check it) | `dig TXT <verifier-domain>` shows an SPF authorizing the egress IP |
| **Real greylist round-trip** | `greylisted`→unknown + `retry_eligible` (P2, un-mockable — GAP-35) | rollout-only; observe real 4xx→retry (P5 retries; P3 marks eligible, doesn't retry) |

---

## Storage grants — proven live against real Postgres 16 (Door-2 posture)

Migration 0007 applied to a throwaway PG 16; connected AS `cp_runtime`:

- **`node_audits`:** INSERT ✅, **DELETE denied** (append-only history — the LOG's own posture).
- **`node_warmup`:** INSERT+UPDATE ✅ (the counter), **DELETE denied**.
- **`nodes`:** UPDATE ✅ (health/lifecycle writes), **DELETE denied** (runtime never destroys a node record).

All 7 PG-gated store tests pass (`TestPGNodeStateAndAudit`, `TestPGNodeAuditHistoryAccumulates`,
`TestPGWarmupReserveAtomicCap` + the 4 P1/P2 ones).

---

## Additivity (§8) — proven additive against all five frozen doors

- **Door 1** — one new `signals` key (`smtp_skipped_reason`); no field/type/invariant changed.
- **Door 2** — `verifier_ip` + `nodes.audit_status/last_audit_at/port25_open` **populated** (reserved
  in P1); `node_audits`/`node_warmup` are **new tables** (no frozen-schema change); `node_audits` is
  append-only, grant-enforced.
- **Door 3** — hashing untouched; the ADR-0008 `verification_raw` redaction carries through verbatim.
- **Door 4** — **zero new `status`/`reason`/`recommendation`**; `vocab.go` byte-unchanged.
- **Door 5** — verifier-node fork + gem **byte-identical to P2**; only brain code + infra added.

---

## Config-flip (§5.3): mock → real is config-only, reversible

Per the ordered checklist ([`phase-3-cutover-runbook.md`](phase-3-cutover-runbook.md) §"The flip"):
the ONLY things that change mock→real are **config values across two locations** (the brain node
descriptor's `base_url`/`PublicEgressIP`/`enabled`, and the box's `smtp_port:25`/`dns` via
`/v1/config/reload`) — **zero brain code diff** (GAP-21: honest as "zero-code, config-only,
reversible", not a literal one key). Every step is reversible by config (`enabled=false` / repoint /
reload mock) with no redeploy.

---

## Cutover gates (NOT build prerequisites — they gate the real-box go-live)

Per the readiness assessment ([`phase-3-readiness-assessment.md`](phase-3-readiness-assessment.md)):
- **Gate 1 — the host** (port25 ∧ settable PTR ∧ clean IP): 🔴 owner procurement. **The wall.** The
  local build is done; the host is not acquired. `PublicEgressIP`/`port25_open` stay empty/false
  until a trial box passes V1–V5.
- **Gate 2 — GAP-13 reputation cost:** 🟡 a known input — the warmup cap counts **verifications**;
  a catch-all-eligible check is ~2 real connections (Option B). Size the ramp against connections.
- **Gate 3 — admin-audit log (DR-3):** 🔴 required before go-live, buildable now in parallel; **not**
  built here (it gates the cutover, not this local build).

---

## Adversarial review — 2 test-double-fidelity findings caught + fixed (both low, both locked)

A background multi-dimension adversarial review ran (4 dimensions: the GAP-24 gate, the orchestrator
PB1/additivity wiring, the store/warmup/grants, and the provrouting/scheduler scope) with an
adversarial verify pass (verifiers told to REFUTE). Depth matched reversibility, as directed: the
review ran long (the verify stage on Opus is thorough) so — consistent with the owner's "don't run a
47-min block, salvage and adjudicate inline" guidance — it was stopped once the confirmed findings
had landed and the rest were re-confirming the same dimensions. **The GAP-24 invariant and the audit
hard-block (the two irreversible surfaces) were fail-verified by neuter BEFORE the review** and held.

Two **CONFIRMED** findings, both **low severity**, both in the **MemStore↔PG test-double fidelity**
(the mem fake exists precisely to not mask real bugs, so both were fixed):
- **Warmup deny-path cap divergence** (`mem.go` `ReserveWarmupSlot`): on a deny with a *lower* caller
  cap, Mem mutated + returned the lowered cap while PG returns the stored (higher) cap unchanged.
  **Fixed** — Mem no longer touches the stored cap on deny (mirrors PG's ON-CONFLICT-SET-not-applied);
  locked by `TestMemReserveWarmup_DenyPathDoesNotLowerStoredCap` + `TestPGReserveWarmup_DenyReportsStoredCap`
  (fail-verified). *(Not a production bug — the sole live caller passes a constant config dial that
  doesn't change within a warmup window; the safety-critical deny/allow decision was always correct.)*
- **Unregistered-node audit divergence** (`mem.go` `AppendNodeAudit`/`GetNodeHealth`): an audit for a
  node with no `nodes` row made Mem's `GetNodeHealth` return `known=true`, while PG returns
  `known=false` (its `UPDATE nodes WHERE id=$1` matches 0 rows). **Fixed** — Mem now tracks a
  `registered` set so `known` reflects `nodes`-row existence exactly as PG does; the audit still
  records history in both. Locked by `TestMemAppendNodeAudit_UnregisteredNodeNotKnown` +
  `TestPGAppendNodeAudit_UnregisteredNodeNotKnown`. *(Both stores already **fail-closed identically** —
  the router excludes an unknown-health node from `:smtp` either way — so no verdict was ever at risk.)*

No correctness hole was found in the gate, the GAP-24 invariant, the PB1 cache rule, the additivity,
or the scope boundary (no P4 health-watcher / block-rate / pool crept in).

## What Phase 3 does NOT build (correct scope — SEAM → later)

No continuous health-watcher / block-rate quarantine (P4 — GAP-18; P3 is audit-driven only). No
pool / 2nd box / sticky routing (P4). No per-provider rate-limit + jitter (P4 — P3's warmup is one
ascending cap). No HELO/verifier rotation (P4 — GAP-22). No queue / greylist retry (P5). No fallback
(P6). No flywheel (P7). The gem is never forked.
