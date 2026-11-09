# Phase 4 → Phase 5 — handoff

**Date:** 2026-07-18 · **From:** Phase 4 (resilience & pool), local build complete · **To:** Phase 5 (async queue, greylist retry, idempotency)
**Purpose:** one page an engineer reads before Phase 5 — what P4 **locked/built**, what P5 **inherits**,
the **`SEAM → P5`** items, and the cutover gates that carry forward.
**Authorities:** [`phase-4-resilience-pool-spec.md`](phase-4-resilience-pool-spec.md), [`phase-4-status.md`](phase-4-status.md), [`phase-5-queue-spec.md`](phase-5-queue-spec.md), [`DECISIONS.md`](../open-decisions.md) + [`adr/`](../adr) (esp. ADR-0010, ADR-0002/DR-4).

---

## 1. Phase 4 (local) is genuinely closed (verified)

| Gate | Result |
|---|---|
| `go build` / `go vet` / `go test` (control-plane, 18 packages) | ✅ all pass |
| PG-gated store tests against real Postgres 16 (migration 0008 applied) | ✅ 15/15 (7 new P4: durable rate budget across restart, refill-by-elapsed, peek-no-consume, block-rate PB1, recovering status, warmup reset; 1 honest skip for the runtime-role DELETE grant) |
| Append-only grants live as `cp_runtime` | ✅ `rate_budgets` DELETE **denied**; INSERT/UPDATE allowed; `recovering` accepted by the widened `nodes.status` CHECK |
| **GAP-28** (fleet-wide cascade brake) fail-verified | ✅ the pool does NOT self-empty on a simulated fleet-wide spike (pure + e2e); a single outlier IS quarantined |
| **GAP-30** (canary recovery) proven | ✅ full quarantine→recovering→active cycle with warmup reset (PM3) |
| **PB1** (block-rate excludes cache/degrades) | ✅ real PG + Mem parity + e2e |
| **DR-2** (A1 recommend-only, fresh-system zero mutation) | ✅ `TestWatcher_BlockRate_RecommendOnly_ZeroMutation_DR2` |
| Door 5 gem-unmodified + verifier-node byte-identical to P2/P3 | ✅ zero verifier-node change (P4 is brain + infra only) |
| Additivity (Door 1/2/3/4/5) | ✅ 1 signals value, 1 new table, 1 node-lifecycle value, unchanged Strategy interface, gem byte-identical — **no frozen door moved** |
| config-reference freshness / docs-updated | ✅ regenerated |

**The system is still one config flip + the cutover away from real boxes.** On the local machine no
node is `:smtp`-eligible (no real egress-25, no real PTR — inherited P3 honest state); the pool LOGIC
is fully built + tested. Real-multi-box proofs are PENDING-REAL-BOX with recorded commands
([`phase-4-status.md`](phase-4-status.md), [`phase-4-provisioning-script-design.md`](phase-4-provisioning-script-design.md) §4).

---

## 2. What Phase 4 LOCKED / built (additive-only against the five frozen doors)

- **Router strategies** (`router/sticky.go`): sticky-by-domain (HRW, OQ-P4-1) default + least-inflight
  fallback (§3.4), BEHIND the unchanged `Strategy` interface (Door 5). Headroom/inflight oracles
  injected at construction. Anti-multiplication + HRW minimal-reshuffle proven.
- **The per-provider rate limiter** (`internal/ratelimit`): durable token-bucket budget in Supabase
  (§4.4, restart-safe — the PM4 discipline) + ephemeral in-flight (§4.3/GAP-26, correct-at-zero
  post-restart) + jitter (§4.2) + provider bucketing (`other:<domain>`, OQ-P4-2). "Sticky concentrates;
  the limiter paces" (§4.1).
- **The continuous health-watcher** (`internal/health`): audit (via the P3 scheduler's new `Observe`)
  + liveness (`/healthcheck`) + block-rate (LOG). The pure `Evaluate` owns the state machine + the
  three guards + the fleet brake + the canary. Single writer of `nodes.status` (§5.1).
- **`rate_limited` degrade** (orchestrator): §4.6 — degrade to cheap, PB1 non-cacheable, never
  invalid/queued. One additive `signals` value.
- **The `recovering` lifecycle value** (`nodes.status`, migration 0008): the canary sub-state (GAP-30).
- **Observability** (`internal/metrics`): the four build-plan metrics + pool-health + rate-pressure
  from LOG columns; the two metric-threshold alerts (pool-eligible-low, rate-saturation).
- **Node #2 = +1 config line** (`config`): numbered `TM_NODE_<i>_*` env; the GAP-31 proof.
- **Additivity:** 1 `signals` value, 1 new table (`rate_budgets`), 1 node-lifecycle value; **zero new
  verdict vocab** (Door 4); gem byte-identical (Door 5).

---

## 3. What Phase 5 INHERITS (built and ready to build on)

- **The backpressure the queue absorbs.** The rate limiter CREATES the `rate_limited` degrade (§4.6);
  the P5 queue's whole reason for being is to **retry that check when capacity frees**. The
  retry-eligible shape is already in `signals` (P2 §6.5 `retry_eligible`/`retry_after_hint_s`) — P4
  leaves it set but retries nothing.
- **Sticky-by-domain sets up domain-batched session reuse.** Same domain → same node → same warm
  connection. P5's "group many target addresses for one domain into one session" batching is the
  natural next step (build-plan §5) — sticky routing is its precondition, built here.
- **The durable-counter discipline** (`node_warmup` P3 + `rate_budgets` P4): the P5 queue's
  idempotency (CAS lease-and-settle, GAP-37) reuses the same "durable state, exactly-once" reasoning.
- **The degrade-not-queue seam is clean:** every capacity-gap degrade (`no_eligible_node`,
  `warmup_cap`, `rate_limited`) is PROVISIONAL/non-cacheable (PB1) — P5 can safely re-drive them.

---

## 4. `SEAM → P5` items (Phase 5's explicit inbox)

| Seam | Where P4 left it | What P5 does |
|---|---|---|
| **The queue itself** | `rate_limited`/`no_eligible_node`/`warmup_cap` degrade synchronously (§4.6) | the async `work_items` queue + drainer; retry the degrade when capacity frees |
| **Greylist retry** | P2/P3 mark `greylisted`→unknown + retry-eligible; nothing retries | the backoff/retry-then-accept round-trip (un-mockable — GAP-35, rollout) |
| **Domain-batched session reuse** | sticky routing concentrates a domain on one node | batch many targets for one domain into one warm session (build-plan §5) |
| **Idempotency (CAS)** | the durable-counter pattern exists (`node_warmup`, `rate_budgets`) | lease + compare-and-set exactly-once settlement (GAP-37) |
| **Bulk job handle** | `/v1/verify` is synchronous | `/v1/verify/bulk` accept-and-defer + a job handle (kill-switch Rung 4) |

**Deferred past P5 (do not build in P5):** third-party fallback (P6), the flywheel (P7).

---

## 5. Cutover gates carried forward (owner action — gate the real-box go-live, not P5 code)

| Gate | State | Note |
|---|---|---|
| **1. Host** (≥1 box for P3 acceptance; 2 for the P4 pool demo) | 🔴 procurement — **the wall** | the §6 provisioning-script design makes standing each box up a repeatable gated pipeline. |
| **2. GAP-13 reputation cost** | 🟡 known input | size warmup + rate caps against **connections** (~2× for catch-all). |
| **3. Admin-audit log (DR-3)** | 🔴 before go-live; **buildable now in parallel** | its first slice is the §6 stage-4 `port25_open` egress-test-writer (resolves the P3 wiring gap). **The highest-value parallel work.** |
| **DR-4 — queue-plaintext backup scope** | 🔴 before **P5** (new for P5) | the `work_items` plaintext-at-rest exception (the one bounded Door-3 exception) must be backup-aware ([ADR-0002](../adr/ADR-0002-queue-plaintext-backup-scope.md)); mechanism is a P5-time choice. |

---

## 6. The open owner item (does NOT block Phase 5)

- **LICENSE** — still deferred (DECISIONS §6.1). Revisit before external distribution.

---

## 7. Verdict

**Phase 4 (local) is genuinely complete.** The full pool-resilience machinery — sticky routing, the
per-provider rate limiter, the continuous health-watcher, block-rate quarantine with the three guards,
the fleet-wide cascade brake (fail-verified), canary recovery, observability, and the node-#2-config-only
proof — is built and test-proven against a simulated multi-node pool; the grants are proven live against
real Postgres; and the two irreversible surfaces (the fleet brake, the canary) are built and
fail-verified, not deferred. Every real-multi-box check is honestly PENDING-REAL-BOX with its command.
**Phase 5's first move is the durable async queue that absorbs the `rate_limited` backpressure P4 creates.**
