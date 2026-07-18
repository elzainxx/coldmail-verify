# Phase 4 — status: resilience & pool (local build scorecard)

**Date:** 2026-07-18 · **Phase:** 4 (resilience & pool) · **Spec:** [`phase-4-resilience-pool-spec.md`](phase-4-resilience-pool-spec.md) (the authority)
**Status: ✅ COMPLETE (local build)** — the full pool-resilience machinery is built and test-proven
against a **simulated multi-node pool** (N≥3, controllable per-node audit / liveness / block-rate),
exactly as Phase 3 built the audit gate locally. Every real-multi-box check is honestly marked
**PENDING-REAL-BOX** with its verification command — never faked green.

**What Phase 4 is:** the continuous **health-watcher** (audit + liveness + block-rate → auto-quarantine)
+ **router strategies** (sticky-by-domain default + least-inflight fallback) + a **per-provider rate
limiter + jitter** + the **provisioning-script design** + **node #2 = +1 config line** + **observability**.
**No brain verdict logic changed** — two new `Strategy` implementations behind the unchanged P1
interface, a rate-limiter admission stage, and a background control loop.

**Prerequisite gate (checked before starting):** ✅ Phase 3 committed green (`fb46642` + readiness
`1e6cf2c`); GAP-24/GAP-15 re-proven; the PENDING-REAL-BOX set honestly tracked. Host procurement is
**NOT** a prerequisite — Phase 4 builds against simulated nodes and does not block on a VPS.

---

## The two things Phase 4 could NOT change (and didn't)

- **The verifier-node** — byte-identical to P2/P3 (Door 5). The endpoints the watcher consumes
  (`GET /healthcheck`, `GET /v1/stats`, `GET /v1/audit`) were built in P1/P2. Zero verifier-node change.
- **The `Strategy` interface** (P1 §5.4) — unchanged. Sticky-by-domain + least-inflight are new
  IMPLEMENTATIONS behind it (the Door-5 payoff). Adding node #2 is a config-list edit, zero brain code.

---

## Build increments (all local, all test-proven)

| # | Increment | Status | Evidence |
|---|---|---|---|
| 1 | **Migration 0008** — `rate_budgets` (durable token-bucket, §4.4) + the `recovering` lifecycle value (§5.4/GAP-30) | ✅ | `migrations/0008_rate_budgets_recovering.sql`; applied to real PG 16; grants proven live. |
| 2 | **Store layer** — `ReserveRateToken`/`PeekRateBudget` (durable, restart-safe), `BlockRateByVerifierIP` (LOG, PB1-filtered), `SetNodeStatus`, `ResetWarmup`, `MetricRowsSince` + PG + Mem (shared `applyRefill` math) | ✅ | `internal/store/{store,mem,postgres}.go`; 7 new PG-gated tests pass against real PG 16. |
| 3 | **`internal/ratelimit`** — per-(node×provider) durable token-bucket + jitter + ephemeral in-flight (GAP-26) + provider bucketing (OQ-P4-2) | ✅ | `ratelimit.go`/`jitter.go` + tests; two-caps AND, release lifecycle, per-node/provider isolation, restart-survives-budget. |
| 4 | **Router strategies** (§3, highest rigor) — sticky-by-domain (HRW, OQ-P4-1) + least-inflight fallback, behind the UNCHANGED interface | ✅ | `router/sticky.go` + tests; anti-multiplication, HRW minimal reshuffle, sticky-first ordering all proven. |
| 5 | **Orchestrator** — `rate_limited` degrade (§4.6, PB1 non-cacheable, never invalid/queued), jitter + admit + release lifecycle | ✅ | `orchestrator/{smtp_stage,ratelimit_stage_test}.go`; degrade + release-on-every-path + real-limiter e2e. |
| 6 | **`internal/health`** (§5, highest rigor) — the continuous watcher, block-rate quarantine + the three guards, the fleet-wide cascade brake, canary recovery | ✅ | `health/{decide,watcher}.go` + tests; **fleet brake fail-verified** (pool does NOT self-empty), full canary cycle. |
| 7 | **`internal/metrics`** — block-rate/unknown-rate/latency/cache-hit + pool-health + rate-pressure from LOG columns; pool-eligible-low + rate-saturation alerts | ✅ | `metrics/{metrics,alerts}.go` + tests; PB1 split (cache excluded from block-rate, counted in cache-hit). |
| 8 | **Config dials + main wiring** — the §5 watcher dials, the rate limiter + sticky strategy wired, the watcher REPLACES the P3 minimal ticker, node #2 = numbered env | ✅ | `config/config.go`, `cmd/control-plane/main.go`; `TestLoad_NodeTwoIsConfigOnly_GAP31`; config-reference regenerated. |
| 9 | **Docs + ADR-0010 + this scorecard + the provisioning-script design + handoff** | ✅ | ADR-0010, this doc, the §6 design, the P4→P5 handoff, config-manifest §, migrations/README, CI applies 0008. |

---

## The two highest-rigor items (the phase's spine)

### Sticky-by-domain — the anti-multiplication property (§3.1)

> **RULE (LOCKED):** a domain (hence largely a provider) concentrates on ONE node. Without it, a
> provider's checks scatter across all N nodes → N× provider-perceived load → every cap is theatre.

- **Where it lives:** `router.StickyStrategy` (HRW/rendezvous over the *eligible* set) with a
  least-inflight fallback when the sticky home lacks rate/warmup headroom (§3.3/§3.4). Behind the
  UNCHANGED `Strategy` interface — headroom/inflight oracles injected at construction (Door 5).
- **Proven:** `TestSticky_DomainConcentratesOnOneNode` (a domain never scatters), `_HRWDistributesAcrossPool`
  (balance), `_MinimalReshuffleOnMembershipChange` (only the departed node's domains rehash — the
  re-route-on-quarantine property), `_FallsBackToLeastInflightWhenHomeCapped` (sticky-first ordering).
- **"Sticky concentrates; the limiter paces"** — one mechanism in two parts (§4.1). The rate limiter
  bounds what the concentrated provider sees.

### The health-watcher + block-rate quarantine — the control loop with three guards (§5)

> **RULE (LOCKED):** block-rate is a NOISY, LAGGING, STATISTICAL signal — it needs the three guards
> or "auto-heal" becomes "auto-outage."

- **The three guards (§5.3):** the **sample-size gate** (GAP-27 — inert below `min_samples`), **hysteresis**
  (quick to quarantine, slow to trust), and the **fleet-wide cascade brake** (GAP-28).
- **The fleet-wide cascade brake — Phase 4's irreversible surface (§5.5/GAP-28), FAIL-VERIFIED +
  HARDENED:** auto-quarantine redistributes a blocked node's load onto peers, cascading until the pool
  empties on a *shared* cause. **Four defenses** (all err-toward-alert): (a) the brake counts
  **ELEVATED** nodes (climbing toward the trip), not just hard-tripped, so a **staggered** subnet
  spike is caught while peers climb; (b) **cross-cycle cascade memory** (a recent block-quarantine + a
  fresh trip = cascade underway → brake); (c) the ≥2 rule; (d) an **active-floor backstop** (block-rate
  quarantine can NEVER drop the pool below the serving floor). When braked, a **critical** `fleet_wide`
  alert fires. **Proven the pool does NOT self-empty:** `TestWatcher_FleetWideSpike_PoolDoesNotSelfEmpty_GAP28`
  (e2e) + `TestEvaluate_FleetWideSpike_DoesNotEmptyPool_GAP28` (pure) + the per-defense
  `FindingA` regression tests (staggered/cascade-memory/active-floor). A single outlier IS quarantined
  (`TestEvaluate_SingleOutlier_IsQuarantined`) — only the systemic/cascading case is suppressed.
- **Canary recovery — block-quarantine is NOT a one-way door (§5.4/GAP-30):** quarantined + audit-green
  + live → `recovering`, which receives a **metered canary trickle of REAL checks** (the router admits
  it as a fallback candidate — active preferred — metered by the low canary cap). It promotes to active
  only after ≥`MinCanarySamples` **real** clean canary attempts (never on zero samples) → active,
  **re-warming gently** (warmup reset, PM3). `TestWatcher_CanaryRecoveryCycle_NotAOneWayDoor_GAP30` +
  `FindingB` regression tests.
- **Shadow-mode (DR-2):** block-rate quarantine (A1, statistical) **defaults to recommend-only** —
  ZERO live block-rate mutation until an owner promotes A1 to auto. Audit-red (A2) + canary (A3) stay
  auto. `TestWatcher_BlockRate_RecommendOnly_ZeroMutation_DR2` (the fresh-system regression gate).
- **Single writer (§5.1):** the watcher is the ONLY writer of the audit/block-driven `nodes.status`;
  the router only READS it. The watcher REPLACES the P3 minimal re-audit ticker (§1.2) — it uses the
  P3 scheduler's new `Observe` (fetch + record, no transition), so no two writers race.

---

## Failure isolation (§5.6) — the GAP-24 payoff, now load-bearing

Per-node block-rate is `GROUP BY verifier_ip` over the LOG. It is measurable **only because** P3 made
`verifier_ip` accurate and enforced egress==PTR==verifier_ip==audit-IP (GAP-24). A dirty node #1 shows
a high block-rate on its own `verifier_ip` without moving the clean peers' stats
(`TestWatcher_SingleDirtyNode_QuarantinedCleanPeersIsolated`). **The owner directive stands: GAP-24 is
the load-bearing dependency of the pool's whole failure-isolation guarantee — don't let it rot.**

---

## Block-rate excludes cache hits + degrades (PB1)

The block-rate query counts ONLY node-attributable SMTP attempts (`from_cache=false ∧ verifier_ip NOT
NULL`) — a cache hit ran no SMTP, and a cheap-degrade has `verifier_ip=null` (§4.6). Counting either
would quarantine a node for verdicts it never produced. Proven:
`TestPGBlockRate_ExcludesCacheAndDegrades_PB1` (real PG) + the Mem parity test +
`TestWatcher_BlockRate_ExcludesCacheAndDegrades_PB1` (e2e).

---

## Additivity (§9) — proven additive against all five frozen doors

- **Door 1** — one new `signals` value (`smtp_skipped_reason:"rate_limited"`) on the already-open P3
  key; no field/type/shape change.
- **Door 2** — one new table (`rate_budgets`, DELETE-revoked grant posture proven live); metrics derive
  from EXISTING LOG columns; `node_warmup`/`node_audits` read/reused, not altered; **no frozen-schema change**.
- **Door 3** — hashing untouched; sticky routing keys on `domain` (in clear), never `email_hash`.
- **Door 4** — **zero new `status`/`reason`/`recommendation`**; `vocab.go` byte-unchanged. The
  `recovering` value is a NODE-LIFECYCLE value (`nodes.status` CHECK widening), NOT verdict vocab (ADR-0010).
- **Door 5** — verifier-node fork + gem **byte-identical to P2/P3**; the `Strategy` interface unchanged;
  node #2 = +1 config line, zero brain code diff.

---

## Storage grants — proven live against real Postgres 16 (Door-2 posture)

Migration 0008 applied to a throwaway PG 16; connected AS `cp_runtime`:
- **`rate_budgets`:** INSERT+UPDATE ✅ (the token bucket), **DELETE denied** (counter posture, mirrors `node_warmup`).
- **`nodes.status`:** the `recovering` value accepted by the widened CHECK; `SetNodeStatus` UPDATE ✅.

All 15 PG-gated store tests pass (the P1/P2/P3 set + the 7 new P4 ones); 1 honest skip (the DELETE-denied
test skips when connected as a privileged role — never faked).

---

## PENDING-REAL-BOX (never faked green — the honest ledger)

The pool LOGIC is built + tested against simulated nodes; only **real-multi-box reputation behavior**
is deferred (it gates the acceptance demo, not the build — §2.2/§13).

| Item | What's built now | PENDING-REAL-BOX proof |
|---|---|---|
| **Real per-provider block behavior** | the block-rate query + quarantine + guards, driven by a controllable LOG | rollout: observe real MX greylist/tarpit/block a real IP (`smtp_mock` can't reproduce reputation — GAP-35) |
| **2-box reputation isolation in the wild** | per-`verifier_ip` isolation proven on seeded LOG rows | one dirty real IP vs one clean real IP; acceptance criterion 6 (§13) |
| **Node #2 as a *real* second box** | node #2 = +1 config line proven locally (`TestLoad_NodeTwoIsConfigOnly_GAP31`) | a real #2 needs the §6 provisioning on the narrow port-25∧PTR market |
| **Real jitter/rate against a live provider** | the token-bucket + jitter proven deterministically | rollout observation |

---

## Adversarial review — 2 real findings on the irreversible surfaces, both caught + fixed

A bounded adversarial review (5 dimensions, adversarial verify pass — verifiers told to REFUTE) ran
against the two irreversible surfaces. Consistent with the owner's "don't run a 47-min block, salvage
inline" discipline, it was stopped once the two confirmed findings on the load-bearing surfaces had
landed and the remaining finders were cycling on retries. **Both findings were REAL and both were
fixed before commit** (the review earned its keep — these were genuine holes, not noise):

- **Finding A (CRITICAL, staggered cascade bypass, GAP-28):** the fleet brake was a single-cycle
  snapshot keyed on ≥2 *hard-tripped* nodes in the *same* cycle. A real subnet blocklist pushes nodes'
  30-min rolling rates over the trip at *different* times, so the pool drained **one node at a time**,
  each looking like a lone outlier, and the critical alert never fired — precisely the cascade the
  brake exists to prevent. **Fixed** with four defenses: the elevated-signal (climbing peers count),
  cross-cycle cascade memory, the ≥2 rule, and the active-floor backstop (block-rate quarantine can
  never self-empty the pool). Locked by `TestEvaluate_StaggeredSpike_ElevatedGuardBrakes_FindingA`,
  `_CascadeMemory_BrakesOnStaggeredDrain_FindingA`, `_ActiveFloorBackstop_NeverSelfEmpties_FindingA`.
- **Finding B (medium, zero-sample canary promotion, GAP-30):** a `recovering` node got **no traffic**
  (the gate excluded it; no canary routing was built), so its block-rate was trivially 0 and it
  promoted back to active on audit-green cycles alone — re-warming a possibly-still-blocklisted IP with
  no evidence. **Fixed** by (1) building the metered canary *routing* (recovering nodes are a
  fallback-only candidate, metered by the low canary cap) and (2) a `MinCanarySamples` gate requiring
  real clean canary evidence before promotion. Locked by
  `TestEvaluate_Recovering_ZeroSamplesDoesNotPromote_FindingB` + `_RealCleanSamplesPromotes_FindingB`.

A bounded **round-2** review then re-checked the two fixes + the three unfinished dimensions and found
**three more real findings**, all fixed before commit (this is why the review was worth resuming):

- **Finding C (CRITICAL — a regression from the Finding-B fix):** `CanaryWarmupCap` (5) was **below**
  `CanaryMinSamples` (10), so a metered canary could never accrue enough real attempts to promote —
  re-introducing the GAP-30 stuck-forever one-way door. **Fixed**: default cap raised to 20 (≥ the
  gate) + a `Validate()` guard that rejects any config where `canary_warmup_cap < canary_min_samples`.
  Locked by `TestLoad_P4WatcherDialsDefaultAndOverride` + the `Validate`-rejection test.
- **Finding D (medium):** rate-saturation detected via the sticky Headroom oracle returned the generic
  `ErrNoEligibleNode`, so the router mislabeled it `no_eligible_node` (or `audit_red` with a stale
  peer) instead of `rate_limited` (§4.6). **Fixed**: a distinct `ErrRateSaturated` → `SMTPRoute` emits
  `rate_limited`. Locked by `TestSMTPRoute_AllRateCapped_EmitsRateLimited` +
  `TestSMTPRoute_RateCappedWithStalePeer_StillRateLimited`.
- **Finding E (medium):** `reserveRate`'s first-touch `INSERT` had no `ON CONFLICT`, so two concurrent
  cold-start admits to a never-seen bucket raced into a `unique_violation` → spurious `rate_limited`
  degrade (failed closed, self-healed, but real). **Fixed**: an idempotent `ON CONFLICT DO NOTHING`
  pre-create before the `SELECT ... FOR UPDATE` (mirroring `ReserveWarmupSlot`). Locked by the
  PG-gated `TestPGReserveRate_ConcurrentColdStartNoUniqueViolation`.
- **Finding F (refuted):** a claim that `CanaryCap<=0` could bypass the meter — correctly refuted, the
  config layer rejects a zero/negative cap.
- **Finding G (high — post-commit, GAP-28):** the fleet-brake fraction is `TM_FLEET_WIDE_PCT/100`, and
  the loader only rejected 0/negative — a value **≥100** yields a fraction ≥1.0, so the systemic-spike
  test (`elevated ≥ fraction × active`) can **never** fire, silently disabling "the single most
  important safety line" with no startup error. (The active-floor backstop still bounds the worst case,
  but a dead fraction-brake + no `fleet_wide` alert is exactly the auto-heal→auto-outage hole.) **Fixed**:
  a `Validate()` guard requiring `fleet_wide_pct ∈ 1..99` (rejects the silent-disable at startup). Locked
  by `TestLoad_FleetWidePctUpperBound_FindingD`. This one was surfaced by the round-1 fleet-brake finder
  (it also flagged the CRITICAL staggered-cascade = Finding A) but was **not** among the five fixed
  pre-commit — it is the honest post-commit follow-up (the finder's other high-severity items on
  correlated-audit-red and the last-node edge are covered by the active-floor backstop, which holds
  regardless of the fraction dial).

Two dimensions (additivity/doors, and part of sticky routing) returned clean; the remaining surfaces
are covered by the built-in behavioral + PG-gated tests (durable-budget-survives-restart, PB1
exclusions, HRW anti-multiplication/minimal-reshuffle). **Net: the review found 6 real defects (2
critical, 4 medium/high) — 5 fixed pre-commit, Finding G fixed in the follow-up — all locked with named
regression tests.**

## What Phase 4 does NOT build (correct scope — SEAM → later)

No queue / greylist-retry / backoff loop (P5 — a rate-capped/greylisted check DEGRADES or marks
retry-eligible, never queued here). No third-party fallback (P6). No bounce calibration / provider
auto-promotion / adaptive rate-tuning (P7). No multi-brain distributed rate limiter (single brain
assumed, GAP-25/OQ-P4-8). No topology migration (1-VPS-N-IPs ↔ N-VPS-1-IP, SVG-⑧ — flagged, not chosen).
The gem is never forked.

---

## Cutover gates carried forward (owner action — gate the real-box go-live, not P4 code)

Unchanged from the P3→P4 readiness assessment:
- **Gate 1 — host procurement:** 🔴 **the wall** for *acceptance* (the P4 2-node demo needs 2 boxes);
  the build doesn't need it. The §6 provisioning-script design turns P3's V1–V8 vetting into a
  repeatable gated pipeline (buildable now).
- **Gate 2 — GAP-13 / warmup-connection sizing:** 🟡 a known input; the rate limiter is built to size
  caps against connections (catch-all ≈ 2, ADR-0007).
- **Gate 3 — DR-3 admin-audit log:** 🔴 required before go-live, **buildable now in parallel**; its
  first slice is the `port25_open` egress-test-writer + its admin path (the §6 stage-4 writer, which
  resolves the P3 wiring gap). **The highest-value parallel work.**

**DR-2 build obligation met:** the first data-driven mutator (A1 block-rate quarantine) ships
recommend-only, and the fresh-system zero-mutation regression is wired
(`TestWatcher_BlockRate_RecommendOnly_ZeroMutation_DR2`).
