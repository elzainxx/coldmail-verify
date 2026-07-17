# Phase 3 → Phase 4 — handoff

**Date:** 2026-07-17 · **From:** Phase 3 (first real box), local build complete · **To:** Phase 4 (resilience & pool)
**Purpose:** one page an engineer reads before Phase 4 — what P3 **locked/built**, what P4 **inherits**,
the **`SEAM → P4`** items, and the cutover gates that carry forward.
**Authorities:** [`phase-3-first-real-box-spec.md`](phase-3-first-real-box-spec.md), [`phase-3-status.md`](phase-3-status.md), [`phase-4-resilience-pool-spec.md`](phase-4-resilience-pool-spec.md), [`phase-3-cutover-runbook.md`](phase-3-cutover-runbook.md), [`DECISIONS.md`](DECISIONS.md) + [`adr/`](adr/) (esp. ADR-0009).

---

## 1. Phase 3 (local) is genuinely closed (verified)

| Gate | Result |
|---|---|
| `go build` / `go vet` / `go test` (control-plane) | ✅ all pass (incl. the new `audit`/`router`/`provrouting` packages) |
| PG-gated store tests against real Postgres 16 (migration 0007 applied) | ✅ 7/7 (incl. node state/audit, history accumulation, warmup atomicity) |
| Append-only grant enforcement (as `cp_runtime`) | ✅ `node_audits`/`node_warmup`/`nodes` DELETE **denied**; INSERT/UPDATE allowed |
| GAP-24 + GAP-15 + PB1 failing-cases | ✅ **fail-verified** (neuter → test fires, restore → green) |
| Door-5 gem-unmodified + verifier-node byte-identical to P2 | ✅ zero verifier-node change (P3 is brain + infra only) |
| config-reference freshness / docs-updated | ✅ regenerated (235 knobs) |

**The system is one config flip + the cutover runbook away from a real box.** On the local machine
no node is `:smtp`-eligible (no real egress-25, no real PTR — `port25_open=false`,
`PublicEgressIP` empty), which is the correct honest state; the eligibility LOGIC is fully built +
tested. Real-IP proofs are PENDING-REAL-BOX with recorded commands ([`phase-3-cutover-runbook.md`](phase-3-cutover-runbook.md)).

---

## 2. What Phase 3 LOCKED / built (additive-only against the five frozen doors)

- **The `/audit` gate** (`internal/audit` + the router `SMTPRoute` candidacy filter): `:smtp`-
  eligible IFF `active ∧ green ∧ port25_open ∧ fresh ∧ (GAP-24) egress-IP-matched`. The `Strategy`
  interface is **unchanged** (Door 5 — P4 adds sticky/least-inflight as new impls, no interface churn).
- **The GAP-24 four-way invariant, runtime-enforced** (ADR-0009): the gate AND the scheduler refuse a
  green whose `current_host_ip` ≠ the expected public egress IP. `verifier_ip` sourced from the brain
  registry (OQ-P3-11), never box self-report.
- **Selective routing** (`internal/provrouting`): one policy source (`provider_routing`) renders the
  engine's `validation_type_for` backstop; a drift check logs an alarm (GAP-19/OQ-P3-8). O365 → `:mx`
  (OQ-P3-9).
- **The warmup ramp** (`node_warmup`, durable per-node/per-window cap): over-cap degrades to cheap,
  `smtp_skipped_reason=warmup_cap`, non-cacheable (PB1). Counts verifications (catch-all ≈ 2
  connections — GAP-13/§5.2).
- **`node_audits` append-only history** + the **minimal scheduled re-audit + auto-quarantine** actor
  (audit-driven only; first-red-quarantine / N-green-recover, OQ-P3-7).
- **Additivity:** one new `signals` key (`smtp_skipped_reason`); reserved `nodes`/`verifier_ip`
  columns populated; two new tables; **zero new vocab** (Door 4); gem byte-identical (Door 5).

---

## 3. What Phase 4 INHERITS (built and ready to build on)

- **The node LIST + `Strategy` interface** — P4 adds sticky-by-domain + least-inflight as **new
  Strategy implementations**; the `SMTPRoute` eligibility filter already produces the candidate set.
- **`node_audits` flap history** — P4's continuous health-watcher reads this (green→red→green history
  is exactly what it wants; it was written append-only for this reason, OQ-P3-5).
- **The durable warmup counter** — P4's recovery resets it to a low value (PM3); the per-window row
  shape already supports that.
- **The lifecycle state machine** (`provisioning|active|quarantined`) — P4 adds block-rate-driven
  quarantine as a **second trigger** alongside the audit-driven one built here.
- **`smtp_skipped_reason`** — P4 reuses the token vocabulary; the eligibility filter's per-node
  reasons feed the watcher's diagnosis.

---

## 4. `SEAM → P4` items (Phase 4's explicit inbox)

| Seam | Where P3 left it | What P4 does |
|---|---|---|
| **Continuous health-watcher** | P3 built a minimal audit-only re-audit ticker (§4.5) | the daemon that polls `/audit` + `/health` + **block-rate per node**, correlates, manages the pool. |
| **Block-rate-driven quarantine** (GAP-18) | P3 does **audit-driven** quarantine only; a getting-blocked-but-green box shows as rising `unknown/smtp_blocked` in the LOG (operator-visible) | the statistical block-rate trigger (A1, recommend-only by DR-2). |
| **The pool / node #2** | list-shaped + `Strategy` interface locked; `SMTPRoute` filters N candidates | sticky/least-inflight strategies; "add VPS = +1 config line" (GAP-31); topology choice (SVG ⑧). |
| **Per-provider rate + jitter** | P3's warmup = ONE ascending cap per node | the distributed per-provider rate limiter. |
| **HELO/verifier rotation** (GAP-22) | single fixed verifier identity | rotation (the first P4 reputation feature a real box needs at volume). |
| **Recovery canary trickle** (GAP-30) | quarantine→active on N greens (audit) | canary-trickle recovery under block-quarantine (block-quarantine is a one-way door without it). |

**Deferred past P4 (do not build in P4):** the queue/greylist-retry loop (P5), third-party fallback
(P6), the flywheel (P7).

---

## 5. Cutover gates carried forward (owner action — gate the real-box go-live, not P4 code)

| Gate | State | Note |
|---|---|---|
| **1. Host** (port25 ∧ PTR ∧ clean ∧ dedicated ∧ egress==PTR'd IP) | 🔴 procurement — **the wall** | Vet V1–V5 on a refundable trial before paying ([runbook §1](phase-3-cutover-runbook.md)). |
| **2. GAP-13 reputation cost** | 🟡 known input | Size the warmup cap against **connections** (~2× for catch-all). |
| **3. Admin-audit log (DR-3)** | 🔴 before go-live | Append-only, per-actor identity; buildable now in parallel; still not built. |

---

## 6. The open owner item (does NOT block Phase 4)

- **LICENSE** — still deferred (DECISIONS §6.1). Revisit before external distribution.

---

## 7. Verdict

**Phase 3 (local) is genuinely complete.** The full first-real-box machinery — the `/audit` gate,
the GAP-24 runtime invariant, selective routing, the warmup ramp, the `node_audits` history, and the
minimal re-audit scheduler — is built and test-proven against a simulated box; the grants are proven
live against real Postgres; and the mock→real cutover is a documented config-only seam. Every
real-IP-dependent check is honestly PENDING-REAL-BOX with its verification command. **Phase 4's first
move is the continuous health-watcher + block-rate quarantine on the audit signal + history P3
already produces** — and a second box is a config-list edit, not a code change.
