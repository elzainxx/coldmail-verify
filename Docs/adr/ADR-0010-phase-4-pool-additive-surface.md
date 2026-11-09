# ADR-0010 — Phase-4 resilience-pool additive surface; the `recovering` lifecycle value; A1 recommend-only

**Status:** ACCEPTED (2026-07-18) — Phase-4 (resilience & pool) local build.
**Door touched:** the **Door-1/Door-4 additive-only** guarantees (one new `signals` enum value) and
**Door-2** operational state (one new table). No frozen door is mutated (see the reopen check).
**Sources:** [`../history/phase-4-resilience-pool-spec.md`](../history/phase-4-resilience-pool-spec.md) §3–§9, §13; [`../open-decisions.md`](../open-decisions.md) DR-2; [`../history/shadow-mode.md`](../history/shadow-mode.md) §2.4/§7; GAP-27/28/30, OQ-P4-1/3/6/7/9.

---

## Context

Phase 4 turns the one audited box (P3) into a **managed pool of N boxes** with a continuous control
loop. It builds new brain-side behavior — sticky-by-domain + least-inflight routing, a per-provider
rate limiter, the health-watcher, block-rate quarantine, canary recovery, observability — but it must
stay **additive-only** against the five frozen doors. Four things touch door territory and are worth a
durable record:

1. **One new `signals` enum value:** `smtp_skipped_reason:"rate_limited"` (§4.6) — a rate-capped
   check degrades to cheap, carrying this token. It is added to the P3 open string enum
   (`no_eligible_node|audit_red|provider_mx_routed|warmup_cap`).

2. **One new operational table:** `rate_budgets` (§4.4) — the durable per-(node × provider)
   token-bucket spend. It exists because the *consumed rate allowance* must survive a brain restart,
   or a freshly-restarted brain would let the whole pool sprint at every provider at once (the PM4
   reputation hazard, mirrored from the P3 warmup counter). In-flight counts are the **opposite**
   decision — ephemeral, correct-at-zero post-restart (§4.3/GAP-26) — and are deliberately NOT stored.

3. **One new node-lifecycle value:** `recovering` on `nodes.status` (§5.4/GAP-30). A block-quarantined
   node gets no traffic → no samples → can't prove recovery. The `recovering` canary sub-state is the
   resolution: audit-green + live moves a quarantined node to `recovering`, which receives a metered
   canary trickle; a clean canary over the window promotes it to active (re-warming gently). Without
   it, block-rate quarantine is a **one-way door to permanent quarantine**.

4. **A shadow-mode default:** the block-rate (statistical, A1) quarantine trigger **defaults to
   recommend-only** (DR-2) — it emits a recommendation, never an auto-mutation, until an owner
   promotes it. Audit-red (A2, deterministic) and the canary (A3, conservative) stay auto.

The question this ADR settles: is `recovering` a Door-4 vocabulary change (it is not), is the additive
surface genuinely additive, and are the phase's two irreversible surfaces (the fleet-wide cascade
brake, the canary) built and fail-verified rather than deferred?

## Decision

1. **`recovering` is a NODE-LIFECYCLE value, NOT a verdict-vocabulary value — so it is not a Door-4
   change.** Door 4 freezes the *verdict* enums `status`/`reason`/`recommendation`
   (`control-plane/internal/vocab`), which callers branch on and P7 calibrates on. The node lifecycle
   (`nodes.status ∈ provisioning|active|quarantined|recovering`) is a separate, internal, operational
   state machine (SVG-⑦) that never reaches a caller and never appears in a verdict. `audit.go`
   already documents this split ("disabled is NOT here … the router requires Enabled AND
   status==active"). Adding `recovering` is a **strict widening** of the `nodes.status` CHECK
   (migration 0008): every previously-valid value stays valid — a purely additive migration, exactly
   the kind the P3 note anticipated. `vocab.go` is **byte-unchanged**; zero new
   `status`/`reason`/`recommendation` verdict values.

2. **The additive surface is additive-only (§9):**
   - **Door 1 / Door 4:** one new `signals` value (`smtp_skipped_reason:"rate_limited"`) on the
     already-open P3 string enum; **zero** new verdict-vocab values; the `/verify` response shape,
     `flags`, and `verifications` columns are unchanged. A rate-capped/pool-saturated check degrades to
     the *existing* cheap verdicts (`risky/plausible`), never a new status/reason.
   - **Door 2:** `rate_budgets` is a **new table** touching no frozen schema (`verifications`, `cache`,
     hashing untouched). It adopts the durable-counter grant posture (cp_runtime SELECT+INSERT+UPDATE,
     **DELETE revoked**, DELETE stays cp_admin), proven live as `cp_runtime`. The block-rate,
     unknown-rate, latency, and cache-hit metrics (§8) derive from **existing** LOG columns — no schema
     change. `node_warmup`/`node_audits` (P3) are **read/reused**, not altered.
   - **Door 3:** unchanged. Sticky-by-domain routes on `domain` (already in clear, P1 §3.2.2), **never**
     on `email_hash` — no new hashing, no pepper exposure to the router (§3.2).
   - **Door 5:** the verifier-node fork + gem are **byte-identical to Phase 2/3** — every new Phase-4
     behavior is brain-side (two new `Strategy` implementations behind the UNCHANGED P1 §5.4 interface,
     a rate limiter, a health-watcher) or operational (the provisioning-script design, metrics). Adding
     node #2 is a **config-list edit** (numbered `TM_NODE_2_*` env, §7/GAP-31), zero brain code change.
     The `/healthcheck` + `/v1/stats` + `/v1/audit` endpoints the watcher consumes were built in P1/P2.

3. **The two irreversible surfaces are BUILT and FAIL-VERIFIED locally, not deferred** (and HARDENED
   after an adversarial review — see the review note below):
   - **The fleet-wide cascade brake (§5.5/GAP-28 — "the single most important line in the phase"):**
     auto-quarantine redistributes a blocked node's load onto peers, whose block-rate then rises,
     cascading until the pool empties — on a *shared* cause that was no single IP's fault. The pure
     `health.Evaluate` computes a fleet-wide pre-check with **four** defenses (all err-toward-alert):
     (a) it counts **ELEVATED** nodes (rate ≥ `ElevatedFraction` × `QuarThreshold`, climbing) not just
     hard-tripped nodes, so a *staggered* subnet spike (nodes cross the rolling threshold one at a
     time) is caught while peers are still climbing; (b) **cross-cycle cascade memory** — a recent
     block-quarantine + a fresh trip = the cascade underway → brake even at one-per-cycle; (c) the ≥2
     rule (a lone tripped node in a quiet pool is a genuine per-node fault); (d) an **active-floor
     backstop** — block-rate quarantine may NEVER drop the active pool below the serving floor, so the
     pool cannot self-empty even if every other guard missed. When braked, a **critical** `fleet_wide`
     alert fires and membership holds (OQ-P4-9). Fail-verified end-to-end
     (`TestWatcher_FleetWideSpike_PoolDoesNotSelfEmpty_GAP28`) and per-defense
     (`TestEvaluate_StaggeredSpike_ElevatedGuardBrakes_FindingA`,
     `TestEvaluate_CascadeMemory_BrakesOnStaggeredDrain_FindingA`,
     `TestEvaluate_ActiveFloorBackstop_NeverSelfEmpties_FindingA`). A single outlier against a healthy
     pool IS quarantined (`TestEvaluate_SingleOutlier_IsQuarantined`) — the brake suppresses only the
     systemic/cascading case.
   - **The canary recovery (§5.4/GAP-30):** a block-quarantined node that regained audit-green + live
     enters `recovering` and receives a **metered canary trickle of REAL checks** (the router admits it
     as a fallback candidate — active nodes preferred — metered by the low canary warmup cap). It
     promotes to active only after ≥ `MinCanarySamples` **real** clean canary attempts over the recover
     streak — never on zero samples (which would trivially satisfy `BlockRate()==0` with no evidence
     the IP recovered) — then re-warms gently (warmup reset, PM3). Proven end-to-end
     (`TestWatcher_CanaryRecoveryCycle_NotAOneWayDoor_GAP30`) and per-condition
     (`TestEvaluate_Recovering_ZeroSamplesDoesNotPromote_FindingB`,
     `TestEvaluate_Recovering_RealCleanSamplesPromotes_FindingB`).

   > **Adversarial-review note (2026-07-18).** A bounded adversarial review of these two surfaces found
   > two real defects in the first build, both fixed before commit: (A) the fleet brake was a
   > single-cycle snapshot keyed on hard-tripped nodes, so a *staggered* systemic spike drained the
   > pool one node at a time with no `fleet_wide` alert — fixed by the elevated-signal + cross-cycle
   > memory + active-floor defenses above; (B) a `recovering` node received no traffic, so it promoted
   > on zero canary samples (or would stick forever) — fixed by the metered real-sample canary + the
   > `MinCanarySamples` gate. Both are locked by the named `FindingA`/`FindingB` regression tests.

4. **A1 (block-rate quarantine) defaults to recommend-only (DR-2 / shadow-mode §2.4).** The watcher
   performs **zero live block-rate mutation** at the default mode — it emits a recommendation + a
   `block_spike` alert; an owner promotes A1 to `auto` (an admin-audited action under DR-3) to enforce.
   Audit-red (A2) and the canary (A3) stay auto (deterministic / conservative). This satisfies the
   fresh-system invariant (DR-2): a fresh system performs no live statistical mutation until promoted
   (`TestWatcher_BlockRate_RecommendOnly_ZeroMutation_DR2`).

5. **Block-rate excludes cache hits and cheap-degrades (PB1).** The block-rate query counts only
   node-attributable SMTP attempts (`from_cache=false ∧ verifier_ip NOT NULL`), so a node is never
   quarantined for verdicts it never produced (`TestPGBlockRate_ExcludesCacheAndDegrades_PB1`, and the
   Mem parity test) — the same `from_cache`-excluded discipline P1 §3.2.1 locked, applied to the
   control loop. This is why `verifier_ip` (P3/GAP-24) had to be accurate: per-node block-rate is
   `GROUP BY verifier_ip`, so a wrong `verifier_ip` would smear one dirty box across the fleet's stats
   and silently break failure isolation. GAP-24 is now the **load-bearing dependency** of the pool.

## Consequences

- The auto-heal→auto-outage failure (GAP-28) cannot occur: a systemic spike alerts and holds; only a
  lone outlier against a healthy pool is quarantined. Block-quarantine is reversible (the canary,
  GAP-30); recovery re-warms gently (PM3), so a briefly-recovered node is not re-burned.
- On the local machine the whole pool machinery is built and tested but **inert for real traffic**
  (no eligible node — `port25_open=false`, `PublicEgressIP` empty, inherited from P3). Only the
  real-IP acceptance proofs (real per-provider blocks, real 2-box reputation isolation) are
  `PENDING-REAL-BOX`; they gate the acceptance demo, not the build.
- Adding node #2 is a config-list edit (proven: `TestLoad_NodeTwoIsConfigOnly_GAP31`), honestly
  bounded by GAP-31 (the brain is N-agnostic; the world still makes you provision + audit each box via
  the §6 script).
- The rate limiter's durable budget survives a brain restart without refilling
  (`TestPGRateBudget_DurableAcrossRestart`); in-flight resets to zero (correct post-crash, GAP-26).

## Does this reopen a frozen door?

**No.** Every change is additive: one `signals` enum value on an already-open key, one new operational
table (`rate_budgets`, append-only-ish grant posture), one additive node-lifecycle value
(`recovering` — the node state machine, NOT the verdict vocab), and two new `Strategy` implementations
behind the **unchanged** P1 §5.4 interface. `vocab.go` and the hashing scheme are byte-unchanged; the
gem + verifier-node fork are byte-identical (Door 5); the LOG stays append-only (Door 2), and the new
table adopts the same DELETE-revoked grant enforcement. A rate-saturated or pool-churning check
degrades toward `unknown`/cheap — **never a fabricated `valid`/`invalid`** (harden-toward-unknown).
This ADR **enforces** the doors; it does not mutate any of them.
