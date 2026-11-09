# Shadow Mode — Recommend-Only vs Auto-Apply (Cross-Phase)

**Status:** DRAFT v0.1 — cross-phase consolidation pass (planning, not code)
**Purpose:** define one consistent mode discipline for every design point that autonomously mutates **routing, confidence, provider eligibility, or spend**. The rule: **compute + alert, never auto-change routing or confidence, until explicitly promoted to auto.**
**Companions:** [`config-manifest.md`](../config-manifest.md) · [`kill-switch-ladder.md`](kill-switch-ladder.md) · [`cross-phase-consistency-report.md`](cross-phase-consistency-report.md)

> **Additive, no door reopened.** This introduces one new per-mechanism dial (`mode`) and reuses infrastructure the specs already define (the versioned P7 calibration store as the review surface; the existing alert/brake pipeline as the human channel). It changes no frozen contract, mints no status/reason value, and mutates no schema beyond an additive `mode` column/config key.

---

## 0. Why this exists (the cross-phase finding)

No single phase looks dangerous. Assembled, the seven phases contain **four automated mechanisms that change what a caller sees — verdict confidence, provider routing, node eligibility, spend — with no human in the per-decision loop, on day one.** A single-phase review can't see the pattern because each phase adds one mutator and treats "operator does X" as out of scope.

The specs *already* ship the correct antidote for the two *systemic* cases:
- **P4 §5.5** fleet-wide block-spike → **alert, do not cascade-quarantine** (GAP-28; owner: "err toward alert").
- **P7 §6.4** systemic calibration swing → **hold for human confirmation, do not auto-apply** (GAP-50).

Shadow mode is the **generalization of that already-blessed pattern** from "systemic swing" down to "any autonomous verdict/routing/spend mutation, gated per-decision." It is not a new philosophy — it extends one the owner already chose twice.

---

## 1. The mode dial

Every mechanism in scope carries a mode:

| mode | Computes? | Emits recommendation/alert? | Mutates live state? |
|---|---|---|---|
| **`shadow`** | yes | yes (logged, dashboards only) | **no** — pure observation; used to build confidence in the mechanism itself |
| **`recommend-only`** (DEFAULT for the four) | yes | yes (actionable: a human can approve/apply) | **no** until a human approves a specific proposal |
| **`auto`** | yes | yes (post-hoc, for audit) | **yes**, within the mechanism's own guards |

- **Default for all four in-scope mutators: `recommend-only`.** Promotion to `auto` is a deliberate owner action per-mechanism, per-scope (e.g. per-provider), never a global flip.
- **`shadow` is the bring-up rung:** run the mechanism live-but-inert first to prove its *own* outputs are sane (does the classifier distribution look right? does promotion fire only where expected?) before even recommend-only proposals are trusted.
- The mode is itself a control-plane change → it belongs in the admin-audit log (see cross-phase-consistency-report §Security F-11): who promoted mechanism M to auto, when, for which scope.

---

## 2. The four mechanisms that MUST default to recommend-only

Sourced from the auto-mutate audit (A1/A6/A8/A9). Each autonomously changes caller-visible state.

### 2.1 A6 — P7 confidence recalibration (the sharpest)
- **Mutates:** `confidence` on future verdicts — the number external systems code against (Door 1's contract *value*).
- **Spec today (P7 §5–6):** auto-apply within sample-gate + bounded `max_step` + down-fast/up-slow hysteresis, per bucket.
- **Shadow-mode default:** compute the new per-bucket confidence into the **versioned calibration store** (P7 §5.4) but **the verdict path keeps reading the prior effective version** until the new version is approved (or a canary window elapses). The store *already* being versioned/effective-dated (for rollback, §6.4) means recommend-only is nearly free: an unapproved version is simply not marked effective.
- **Why sharpest:** cold-start (§5.6) hides that the mechanism is *auto once samples accrue* — the day-one emptiness makes it look inert when it is only dormant. Recommend-only makes "the machine started calibrating" an explicit, visible, approvable event, not a silent threshold crossing.

### 2.2 A8 — P7 provider auto-promotion to `:mx`-only (#4)
- **Mutates:** provider eligibility / routing — the system *stops doing SMTP verification* for a whole provider and trusts cheap-layer + routing alone.
- **Spec today (P7 §7.2):** auto-promote when `valid`-label bounce-rate < `promote_threshold` with samples; reversible hysteretic demote.
- **Shadow-mode default:** surface "**provider X is promotable** (rate R over N samples)" as a recommendation for sign-off; do not flip routing for an entire provider automatically. Promotion is rare, high-impact, and reversibility is **after-the-fact** (demotion happens *after* bad mail is sent).
- **Security amplifier (F-16):** promotion is the highest-leverage poisoning target — promote on poisoned "good" outcomes and the system blinds itself to a provider's bad addresses. A human gate on a rare, high-impact, poisonable action is proportionate.

### 2.3 A9 — P7 per-provider fallback weaning
- **Mutates:** spend + provider eligibility — drops the P6 paid vendor for a `(residue_class × provider)` bucket.
- **Spec today (P7 §9.2):** auto-wean when own measured accuracy beats vendor by a safety margin, both sample-gated; reversible.
- **Shadow-mode default:** emit a "**bucket B is wean-eligible**" recommendation; a human approves dropping paid coverage. Ties to OQ-P6-3's caution: an over-eager wean drops coverage on exactly the hard residue the vendor exists to answer.
- **Note the conflation to avoid (audit finding):** *weaning lives in P7 §9, not P6.* P6's only auto-mutator is the **spend circuit-breaker** (§5.3) — a *different* mechanism (see §3 below).

### 2.4 A1 — P4 block-rate auto-quarantine (single node)
- **Mutates:** routing — removes a node from `:smtp` candidacy on a *statistical* signal.
- **Spec today (P4 §5.4):** auto on `block-rate ≥ QUAR_THRESH` with `samples ≥ min_samples`, pool healthy; guarded by sample-gate, hysteresis, fleet-brake.
- **Shadow-mode default (recommended, softer than the others):** **recommend-only for the block-rate (statistical) trigger** — surface "node N is quarantine-eligible (rate R over N samples)" for confirmation. **The audit-red trigger (A2) stays auto** — it is a clean, deterministic, binary signal, not a statistical inference.
- **Rationale:** block-rate is an *inference* about reputation from a noisy stream; a wrong auto-quarantine on a transient block spike removes healthy capacity. This is the one in-scope mutator where the owner may reasonably choose `auto` early (fast reputation protection is valuable) — so it is flagged recommend-only *by default* but is the **most defensible early promotion to `auto`**, precisely because its blast radius (one node, reversible via canary recovery) is smaller than A6/A8/A9's.

---

## 3. Mechanisms that may stay `auto` (documented, not gated)

The audit found these are safe to leave auto — they act on deterministic signals, degrade only in the *honest* direction (`unknown`/`risky`/residue, never fabricated `valid`/`invalid`), or are reversible by TTL/window. They still appear on the [kill-switch ladder](kill-switch-ladder.md) for completeness.

| id | mechanism | phase§ | why auto is safe |
|---|---|---|---|
| A2 | audit-red quarantine | P3 §4.5 / P4 §5.4 | deterministic binary signal (`warnings=={}`), not an inference |
| A3 | canary auto-recovery | P4 §5.4 | conservative — metered trickle, resets warmup low; reversible |
| A4 | per-provider rate-limiter admission / degrade | P4 §4 | reputation-protective; degrades to cheap, never to `invalid` |
| A10 | historical-bounce gate | P7 §4.6 | fires only on confirmed `mailbox_not_found`, TTL-windowed → degrades to penalty; reversible. **See caveat.** |
| A11 | P6 spend circuit-breaker | P6 §5.3/§5.5 | alert-and-degrade to honest residue; never auto-disables a policy; reversible on window-roll |

**A10 caveat (security F-17):** the historical-bounce gate is auto-safe against *accidental* misclassification (TTL + confirmed-only), but combined with a poisoned outcome feed (§4 below) a single *forged* `mailbox_not_found` can suppress a real address for up to the TTL. The outcome feed (**OQ-P7-1**) is now a hard authenticated door (DR-1 / [ADR-0001](../adr/ADR-0001-outcome-feed-authentication.md)); as **defense-in-depth behind it**, require **≥N independent invalid-signal outcomes** before the gate fires (PN-11, a proposed note — not a mode change; always-on vs. only-when-unauthenticated flagged in [`DECISIONS.md`](../open-decisions.md) §5).

---

## 4. The precondition shadow mode does NOT solve: input authentication

Shadow mode governs *whether a mechanism acts*. It does **not** authenticate the *signal* the mechanism acts on. The cross-phase security lens (F-15) found the sharpest emergent risk:

> P4's control-loop guards (sample-gate, hysteresis, cascade-brake) were designed against a **self-generated** signal (a box's own verdict stream). P7 reuses them against an **external, cross-project, unauthenticated** signal (the sender's outcome feed, OQ-P7-1). The statistical guards defend against *noise*, not an *adversary*. A patient attacker feeding consistent, gate-clearing, bounded-step poison defeats every statistical guard because those guards assume outcomes are honest-but-noisy.

**Implication for shadow mode:** recommend-only on A6/A8/A9 is a *mitigation* (a human sees the proposal before routing/confidence changes) but **not a substitute** for authenticating the outcome feed. Both are required:
- **Authenticate TB-7** (OQ-P7-1 must be decided as a signed/mutually-authenticated channel; threat model must include a malicious sender) — *this is a pre-P7 gate, not a mode dial.*
- **Keep A6/A8/A9 recommend-only** until the feed is authenticated AND the mechanism's shadow-mode outputs have proven sane over a real window.

---

## 5. Verification: does any phase auto-mutate routing/confidence on day one?

Answer, from the audit: **yes — four do (A1, A6, A8, A9), and this doc sets all four to `recommend-only` by default.** The two systemic brakes (A5 fleet, A7 cascade) already alert-and-hold correctly and are the model. The five in §3 are auto but degrade only honestly / reversibly.

**Behavioral acceptance (from the QA lens, GAP-52 proxy):** on a fresh system, assert every in-scope mechanism is in its default mode — zero confidence buckets applied, zero providers promoted, zero buckets weaned, block-rate quarantine emitting recommendations only — i.e. **behavior identical to end-of-P6** until a human promotes a mechanism to auto. A regression is any mechanism silently mutating live state at mode-default.

---

## 6. Summary table

| id | mechanism | phase§ | mutates | spec default | **shadow-mode default** |
|---|---|---|---|---|---|
| A6 | confidence recalibration | P7 §5–6 | confidence | auto (within guards) | **recommend-only** |
| A8 | provider promotion `:mx` | P7 §7.2 | routing/eligibility | auto | **recommend-only** |
| A9 | fallback weaning | P7 §9.2 | spend/eligibility | auto | **recommend-only** |
| A1 | block-rate quarantine | P4 §5.4 | routing | auto | **recommend-only** (statistical trigger); audit-red stays auto |
| A5 | fleet-wide brake | P4 §5.5 | (would-be routing) | alert-only | unchanged — the model |
| A7 | cascade brake | P7 §6.4 | confidence (many) | alert-only | unchanged — the model |
| A2 | audit-red quarantine | P3/P4 | routing | auto | auto (deterministic) |
| A3 | canary recovery | P4 §5.4 | routing/warmup | auto | auto (conservative) |
| A4 | rate-limiter degrade | P4 §4 | pacing | auto | auto (honest degrade) |
| A10 | historical-bounce gate | P7 §4.6 | verdict | auto (TTL) | auto (+ F-17 caveat) |
| A11 | spend circuit-breaker | P6 §5.3 | spend | auto (alert+degrade) | auto (honest degrade) |

*Proposed note for owner sign-off: adopt the `mode ∈ {shadow, recommend-only, auto}` dial with recommend-only defaults for A1/A6/A8/A9, reusing the versioned calibration store and existing alert pipeline. No frozen door is touched.*

---

## 7. Acceptance (2026-07-17) `[Finding-4 / PN-4, accepted → DR-2]`

**ACCEPTED as the resolution of cross-phase Finding 4.** The owner accepted, in the 2026-07-17 consolidation batch, the `mode ∈ {shadow, recommend-only, auto}` dial with **recommend-only defaults for A1, A6, A8, A9**; the two systemic brakes (A5 fleet, A7 cascade) stay alert-and-hold and are the pattern generalized. Recorded as [`DECISIONS.md`](../open-decisions.md) **DR-2**.

- **Door check (re-affirmed):** additive — a new `mode` config key/column; A6 still writes only `confidence` as int 0–100 (Door 1 *value*, not *type*); zero `status`/`reason`/`recommendation` minted (Door 4); LOG stays append-only (Door 2, recalibration writes the separate versioned store). **No door reopened → no ADR.**
- **Regression gate wired (PHASE-GATE):** the §5 behavioral acceptance — *fresh system = every in-scope mutator at default mode, zero live mutation* — is recorded in [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) §3.5 as a regression test for the first phase to ship each mutator (**A1 → P4**; **A6/A8/A9 → P7**).
- **Promotion to `auto`** is a deliberate per-mechanism, per-scope owner action, logged to the admin-audit trail (DR-3). Not performed in this pass; day-one state is recommend-only.
- **Precondition unchanged:** shadow-mode mitigates but does **not** authenticate the outcome feed — §4's requirement is now the hard door **DR-1 / [ADR-0001](../adr/ADR-0001-outcome-feed-authentication.md)**.

*This pass records acceptance and wires the regression gate; it builds no mechanism and reopens no door.*

### 7.1 P7 build (2026-07-19): A6/A8/A9 shipped recommend-only; the fresh-system regression is now proven

The Phase-7 build ships all three P7 mutators **recommend-only by default**, and the §5 behavioral acceptance (**fresh system = every in-scope mutator at default mode, zero live mutation**) is now a passing regression test, not a plan:
- **A6 recalibration** — a recommend-only pass writes a **proposal** (an unapproved calibration version, `effective_from` NULL) that the verdict path does NOT read; the live §5.4 read (`TM_FLYWHEEL_CALIBRATION_ENABLED`) defaults OFF. Proven by `TestRecalibrate_RecommendOnly_NoEffectiveVersion` (a proposal never becomes effective without a deliberate `ApproveCalibration`) and `TestFreshSystem_FlywheelWiredButInert` (a wired-but-fresh flywheel yields a byte-identical verdict to flywheel-off).
- **A8 promotion / A9 weaning** — `EvaluatePromotion`/`EvaluateWeaning` are recommend-only decision logic; on a fresh system both return "hold" (sample gates unmet) → **zero** providers promoted, **zero** buckets weaned (`TestPromotion_SampleGatedAndReversible`, `TestWeaning_RequiresMeasuredMarginOnBoth`).
- **A10 historical-bounce gate** — stays auto (deterministic: TTL + confirmed-only + ≥N), but ships OFF by default (`TM_FLYWHEEL_GATE_ENABLED`=false) so a fresh system does not gate until an owner opts in.
- **§4 input authentication (the precondition shadow-mode does NOT solve)** — discharged: [ADR-0013](../adr/ADR-0013-outcome-feed-auth-mechanism-and-tb7-threat-model.md) decides the mechanism (mutually-authenticated pull / mTLS) + the TB-7 threat model; ingestion is **fail-closed** (an unauthenticated source ingests nothing — `TestIngest_UnauthenticatedIngestNothing`). Recommend-only + authentication are both present, neither substituting for the other. Promotion of any mechanism to `auto` remains a deliberate, admin-audited owner action (DR-3); the build performs none.
