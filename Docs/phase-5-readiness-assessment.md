# Phase 5 readiness assessment — the queue is fully buildable locally

**Date:** 2026-07-18 · **By:** P4→P5 readiness pass · **Verdict up front: Phase 5 is READY to build, and
it needs NO real box at all.** Phase 5 is Postgres + Go — a durable `work_items` queue, a retry
scheduler, a bulk endpoint — all brain-side, all testable against local Postgres exactly as Phases 1–4
were. The one thing P5 genuinely *cannot* fully test locally (the greylist reject-then-accept
round-trip) is already honestly flagged un-mockable (GAP-35) and deferred to a real-provider rollout —
not a build blocker.
**Authorities:** [`phase-5-queue-spec.md`](phase-5-queue-spec.md) (the build), [`phase-4-status.md`](phase-4-status.md), [`phase-4-to-5-handoff.md`](phase-4-to-5-handoff.md), [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) §3.7/§3.8, [`DECISIONS.md`](DECISIONS.md) DR-4, [ADR-0002](adr/ADR-0002-queue-plaintext-backup-scope.md).

---

## 0. Phase 4 is genuinely closed (this pass re-verified it)

| Check | Result |
|---|---|
| `go build` / `go vet` / `go test` (18 packages) | ✅ all pass |
| Full suite on **real Postgres 16** (migrations 0001–0008 applied) | ✅ all PG-gated store tests pass |
| **Provenance of `00f2139`** | ✅ established by git forensics: linear reflog, no concurrent session, no extra worktree/stash/lock; authored by the machine's configured `Ahmed Zain` identity like every commit here — it is a normal commit made in-session, not a third-party's work |
| **Review coverage of `00f2139`** | ✅ every non-test Go file sits in a package with tests + was read by an independent code-reviewer pass — which **found two real defects on the canary path** (see the Finding-B regression row below). Coverage gap `store.MetricRowsSince` (observability read: declared + interface-wired, no caller yet) **closed this pass** with a PG-gated test (`TestPGMetricRowsSince_WindowOrderingAndTruncation`). It remains read-only + off the control path (control uses `BlockRateByVerifierIP`). |
| **Fleet brake fail-verified on all 4 properties** | ✅ each guard neutered → its named test fails, then restored clean (see §1) |
| **Finding-B REGRESSION found + fixed this pass** (the review earned its keep — TWICE) | ⚠️→✅ the independent reviewer's two dying leads each surfaced a real defect on the canary path: **(1)** recovering nodes were **strictly fallback-only** in `SMTPRoute`, so in a **healthy pool** a canary got **zero** traffic → zero samples → **stuck-forever** (Finding-B, reintroduced by its own routing fix; a test even asserted the bug). **Fixed** with a deterministic per-domain canary sample (`CanarySampleN`, ≈1/N of domains reach a recovering node even in a healthy pool; the rest route active). **(2)** RESIDUAL starvation: once a canary-sampled domain's recovering node hit its **low canary cap**, the orchestrator degraded that domain to cheap (`warmup_cap`) on **every** check for the rest of the window — even though a healthy active peer could serve it (the router's fall-through only fired on a *routing* error, not the downstream *warmup-cap* denial). **Fixed** by re-routing forcing active (`ForceActiveOnly`) on a canary-cap denial, degrading only if active also can't serve. Both fail-verified: `TestSMTPRoute_CanaryGetsMeteredTrickleInHealthyPool_FindingB` and `TestSMTPStage_CanaryAtCap_FallsBackToActive_NotDegrade` each fail against their respective pre-fix behavior. **These are production-code fixes** (router + orchestrator + config dial + main wiring), not test-only. |
| Sticky anti-multiplication / least-inflight / rate-limiter durability (restart-no-refill, cold-start race, clock-skew, in-flight-leak) / PB1 / A1-recommend-only+A2-auto / node-#2-config-only | ✅ all proven by named tests |
| Additivity (Doors 1–5) | ✅ `vocab.go` byte-identical since Phase 0; `recovering` is a node-lifecycle CHECK value (NOT verdict vocab); one new `signals` value (`rate_limited`); one new table (`rate_budgets`); gem byte-identical; `verifier-node/` empty diff since P2 — **no frozen door moved** |

**Phase 4 is closed and honest.** The fleet brake — found defective twice — is now guarded by four
independent, individually-fail-verified defenses.

---

## 1. The fleet brake, re-verified with maximum scrutiny (it was wrong twice)

Each of the four required properties has a named test that **fails if its guard is removed** (verified
by neutering the guard, running the test, and restoring clean — no residue):

| Property | Guard(s) | Named test(s) — fail when neutered |
|---|---|---|
| **(a) a STAGGERED drain triggers the brake** (nodes tripping one-at-a-time over cycles, not just a simultaneous spike) | the ELEVATED-signal count (`blockElevated`, ≥60% of the trip = "climbing") **+** cross-cycle cascade memory (`RecentlyBlockQuarantined`) | `TestEvaluate_StaggeredSpike_ElevatedGuardBrakes_FindingA`, `TestEvaluate_CascadeMemory_BrakesOnStaggeredDrain_FindingA` |
| **(b) `TM_FLEET_WIDE_PCT` outside 1–99 is rejected at startup** (≥100 would make the fraction-brake mathematically unable to fire) | the `Validate()` range guard | `TestLoad_FleetWidePctUpperBound_FindingD` |
| **(c) a systemic cause ALERTS and does NOT auto-quarantine** | the `if fleetWide { hold }` suppression **+** the active-floor backstop (block-rate quarantine can never drop the pool below the serving floor) | `TestEvaluate_FleetWideSpike_DoesNotEmptyPool_GAP28`, `TestWatcher_FleetWideSpike_PoolDoesNotSelfEmpty_GAP28`, `TestEvaluate_ActiveFloorBackstop_NeverSelfEmpties_FindingA` |
| **(d) a quarantined node recovers via a metered canary** (and is neither stuck-forever nor promoted on zero samples) | the `MinCanarySamples` gate **+** the quarantined→recovering transition | `TestEvaluate_Recovering_ZeroSamplesDoesNotPromote_FindingB`, `TestEvaluate_QuarantinedToRecovering`, `TestWatcher_CanaryRecoveryCycle_NotAOneWayDoor_GAP30` |

**This is Phase 4's irreversible surface, and it is now the most heavily-guarded code in the system** —
four defenses, each independently fail-verified. The clock-skew guard in the shared token-bucket math
gained an explicit test this pass (`TestApplyRefill_ClockSkewNeverDrainsOrGrants`).

---

## 2. Phase 5 is buildable locally — the whole phase, no box

Phase 5 (the queue) is **Postgres + Go, entirely brain-side** (P5 spec §1.3 Door 5: "Phase 5 adds no
box-side behavior at all"). Every component is local-testable exactly as Phases 1–4 were:

| P5 component (spec §) | Why buildable locally | How proven without a box |
|---|---|---|
| **`work_items` durable queue** (§3.3) | a Supabase table + `FOR UPDATE SKIP LOCKED` dequeue — pure Postgres | PG-gated tests: enqueue/dequeue/lease/reclaim against real Postgres 16 (same harness as P3/P4 store tests) |
| **The drainer loop** (§3) | calls the **unchanged** P4 router; a dequeued check is a normal routed check | drive it against the P4 sim node surface + MemStore (the P4 pattern) |
| **The greylist retry / backoff scheduler** (§4) | pure time-domain logic (`not_before = now + backoff(attempt)`) — deterministic with an injected clock | unit tests over the backoff schedule; **the loop mechanics** (requeue, ceiling→settle, hint-seeding, jitter) tested with a by-attempt-count stub (`450` then `250`) — GAP-35 |
| **Idempotency (settle-once/log-once/spend-once)** (§5) | the GAP-37 lease-TTL + compare-and-set on `locked_by` — all in one Postgres transaction | PG-gated tests: crash-reclaim, slow-drainer double-attempt, dedup — all simulatable by manipulating `work_items` rows + a controllable clock |
| **Group-by-domain pacing** (§6) | ordering over the ready-set + the P4 rate limiter (already built) | unit tests: domain-spread + within-domain spacing against a controllable ready-set |
| **Bulk `/v1/verify/bulk` + `/v1/jobs/{id}`** (§7) | a new brain endpoint returning a job handle + counts; results are the **identical** `/v1/verify` verdict objects | HTTP handler tests + the `jobs` table against real Postgres |
| **Observability** (§9) | queue-depth / retry-rate / settle-latency from the new operational tables | metric-derivation tests over seeded `work_items`/`jobs` |

**The ONE genuinely un-mockable thing (honestly flagged, not a blocker):** the greylist
**reject-then-accept round-trip** — `smtp_mock` cannot do stateful `450`-then-`250` (P2 GAP-12 →
GAP-35). Phase 5 tests the **loop mechanics** against a by-attempt-count stub locally, and validates
the true reject-then-accept behavior against a **real greylisting provider at rollout** — the same
honesty posture P2/P3 used for real-IP proofs. **This gates the acceptance demo, not the build.**

**The VPS is still not a blocker.** P5 adds no box-side behavior; a queued check hits the box's
unchanged `/v1/verify` exactly as a synchronous one does (§3.2). Everything P5 builds runs against
local Postgres + the sim node surface.

---

## 3. The P5-gating checklist items — owner/build obligations before P5 ships

Two accepted phase-gates from the pre-takeoff checklist gate **P5**, plus the DR-2 regression that P4
already satisfied for A1:

| Gate | State | What it requires | Blocks |
|---|---|---|---|
| **DR-4 — backup-plaintext protection for `work_items`** ([ADR-0002](adr/ADR-0002-queue-plaintext-backup-scope.md), checklist §3.7) | 🔴 **decide + wire before P5** | The queue holds the **plaintext address** for work-not-yet-done (the ONE bounded Door-3 exception, purged transactionally on settle — P5 §3.3/§5.3). A DB backup/PITR snapshot copies live plaintext that **no purge transaction touches**. Mechanism is a P5-time choice: snapshot exclusion **or** separate-key at-rest encryption (key ≠ pepper) **or** short snapshot-retention. Door 3 is NOT reopened — this makes the bounded exception backup-aware. | P5 (the queue's plaintext column) |
| **PN-13 / F-20 — front-door auth beyond a static token** (checklist §3.8) | 🔴 **before P5 bulk** | By P5 the front door is the **bulk-PII submission surface**, still guarded by a single static shared bearer. Upgrade to **per-caller keys + rotation** — an auth-strength change behind the *same* `/v1/verify(/bulk)` contract (no Door-1/vocab change). | P5 bulk endpoint |
| **DR-2 — shadow-mode fresh-system regression** (checklist §3.5) | 🟢 **satisfied for A1 in P4** | A fresh system performs zero live mutation at mode-default. A1 (block-rate quarantine) ships recommend-only and the regression is wired (`TestWatcher_BlockRate_RecommendOnly_ZeroMutation_DR2`). P5 adds no new day-one mutator, so this gate is already met for everything P5 ships. | (already met) |

**DR-4 and PN-13 are the two owner actions to line up before P5 code lands.** Both are buildable now
in parallel; neither needs a box. DR-4 is the more architectural of the two (it shapes how the queue's
one plaintext column is protected at rest) and should be decided first.

---

## 4. The SEAM → P5 items P2/P3/P4 deferred — all inventoried, all consumed by P5

Every prior spec left explicit `SEAM → P5` markers; the P5 spec §1.2 collects them. Confirmed present
and cleanly resolvable:

| Deferred by | The seam | P5 consumes it |
|---|---|---|
| **P2 §6.5** | `greylisted → unknown` **with** `retry_eligible` / `retry_after_hint_s` / `smtp_code:"450"` in `signals` — "the actual retry loop is P5" | the backoff scheduler seeds from `retry_after_hint_s`, gates on `retry_eligible`, ceilings at `max_attempts` (§4) |
| **P4 §4.6** | a rate-capped check **degrades cheap** with `smtp_skipped_reason:"rate_limited"` — "queuing it is P5" | a **queued** item that hits `rate_limited` goes `waiting_retry` (waits for capacity) instead of degrading — the limiter *creates* backpressure, the queue *absorbs* it (§4.6). Sync still degrades (unchanged). |
| **P2 OQ-P2-6 / P4 §3.6** | "only settled verdicts are cached; transient `unknown`s never are" | load-bearing for P5: a greylisted item is never satisfied by a stale non-answer, so it always actually retries (§5.5) |
| **P2 §6.3 / P4 §3.5** | 2-RCPT session reuse proven; many-RCPT-per-session domain batching is "P4/P5" | group-by-domain *sets it up*; P5 **recommends NOT building** multi-RCPT batching (reopens GAP-13/never-fork) — a `SEAM →` contingent on the GAP-13 spike (§6.4, OQ-P5-4) |
| **P1 §3.1.1** | singular `/v1/verify` shaped so `/v1/verify/bulk` (job handle) slots beside it | build the bulk sibling; the singular endpoint is untouched (§7) |
| **P4 §8** | `rate_limited`-rate + pool-health metrics = "the P5 queue trigger" | those metrics become the "add a node" capacity signal; P5 adds queue-depth/retry-rate/settle-latency beside them (§9) |

**The rate_limited backpressure P4 creates is the cleanest seam-closure:** the P4 rate limiter already
degrades a synchronous check and returns `rate_limited` (proven this pass); P5's queue is exactly the
mechanism that holds a *queued* such check until capacity frees. Nothing in P4 needs to change for P5
to consume it — the seam is already the right shape.

---

## 5. The load-bearing carry-forward the owner flagged (GAP-37)

The P5 spec's Appendix D names the **GAP-37 lease-and-settle invariant** as "the catch of the whole
five-spec series" and a **system-wide** principle, not a P5 detail: *lease TTL > max operation
deadline, and settle only via a compare-and-set on the lease-holder* (at-least-once attempts,
exactly-once settlement). It is already recorded as a memory ([[truemail-lease-cas-exactly-once-settlement]]).
Phase 5 is where it is first *built*; it then governs the P7 calibration writes and the platform's job
runner. Flagged here so the P5 build treats the idempotency design (§5) as the correctness heart it is,
not boilerplate.

---

## 6. Rules honored in this pass

- **Review/readiness only — no new features, but two real canary-path defects fixed.** The independent
  code-review surfaced two Finding-B-class defects (canary starves in a healthy pool → stuck-forever;
  and a residual cap-denial degrade that starved a canary-sampled domain even with a healthy active
  peer). Fixing them required **production-code changes** (the `CanarySampleN` metered-canary routing +
  the `ForceActiveOnly` cap-denial fallback in `router.go`/`smtp_stage.go` + a config dial + main
  wiring), plus two coverage tests (clock-skew `TestApplyRefill_ClockSkewNeverDrainsOrGrants`;
  observability-read `TestPGMetricRowsSince_WindowOrderingAndTruncation`). These are **bug fixes within
  the review's mandate** ("re-verify the irreversible surface; if it fails, fix it"), not new features.
  No door touched (all routing changes are behind the unchanged `Strategy` interface, Door 5); nothing
  in DECISIONS.md re-decided; the additive surface is unchanged (the new dial is config, not a frozen
  contract). Every fix is fail-verified (neuter → its named test fails → restore).
- **Additive-only; no door mutated** — re-confirmed on every axis (§0).
- **If a criterion was unmet or the brake failed, say so** — the brake passed all four; the one honest
  finding (the untested `MetricRowsSince` read) is named, not hidden.

---

## 7. Summary

| Question | Answer |
|---|---|
| Is `00f2139`'s provenance established? | **Yes** — git forensics prove a linear, single-session history; the "Ahmed Zain" author line is the machine's git identity on every commit here, not a third party. |
| Is `00f2139` fully review-covered? | **Now yes — but the review found a real bug.** The independent pass surfaced a Finding-B regression (canary starved in a healthy pool → stuck-forever), now fixed + fail-verified. The one coverage gap (`store.MetricRowsSince`) was closed with a PG-gated test. Everything on the irreversible surface is fail-verified. |
| Is the fleet brake re-verified? | **Yes** — all four properties, each with a named test that fails when its guard is removed. |
| Is Phase 4 gate-green + additive? | **Yes** — 18 packages on real Postgres 16; no frozen door moved. |
| Is Phase 5 buildable locally? | **Yes — the whole phase.** Postgres + Go, brain-side; no box. Only the greylist reject-then-accept round-trip is un-mockable (GAP-35) — gates the acceptance demo, not the build. |
| What gates P5? | **DR-4** (backup-plaintext for `work_items`) + **PN-13** (front-door per-caller auth) — both buildable now, no box; DR-2 already satisfied for A1. |
| Should Phase 5 block on the VPS? | **No.** P5 adds no box-side behavior; it builds and tests entirely locally. |

**Bottom line:** Phase 4 is closed, honest, and its irreversible surface is the most-guarded code in
the system. Phase 5 is design-complete and fully local-buildable — **start it against local Postgres
whenever ready**; line up DR-4 and PN-13 first. The host remains a rollout-acceptance concern
(real greylist behavior), never a build blocker.

*Also still open (unchanged, non-blocking): LICENSE (DECISIONS §6.1 — pick before external distribution). **[RESOLVED 2026-07-19 at the arc closeout: proprietary — see DECISIONS §6.1 + top-level LICENSE.]***
