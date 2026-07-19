# Phase 7 — readiness assessment (the honest one)

**Date:** 2026-07-18 · **Pass:** P6→P7 readiness (review/verification, no new features) · **Spec:** [`phase-7-flywheel-spec.md`](phase-7-flywheel-spec.md) (APPROVED v1.0, the final phase)
**Verdict headline:** **Phase 7 is substantially buildable NOW against a simulated outcome feed — but it must not go LIVE until ADR-0001's outcome-feed authentication is designed AND the sender (a separate system, "Feature 2") exists with an agreed contract.** One hard door (ADR-0001) gates live P7; its *principle* is decided, its *mechanism* is a deliberate owner/P7-time choice. Nothing here re-decides it.

This pass did **not** start Phase 7. It closed Phase 6 out (re-verified) and assessed what P7 needs.

---

## 0. Phase 6 re-verified closed (the pass's verification half)

| Check | Result |
|---|---|
| Anti-laundering (§3.4) fail-verified on **real Postgres** | ✅ neuter the `valid→risky` mapping → `TestResolve_VendorValid_IsNeverValid` breaks ("laundered into status:valid"); restore → green |
| Spend-once (§5.4/GAP-42) fail-verified on **real Postgres** | ✅ neuter the CAS → stale attempt double-settles + a 2nd `external_calls` row → `TestPGWorkItem_CASSettlementExactlyOnce` breaks; restore → green |
| Policy-gate defense-in-depth agrees independently | ✅ cold-email `external:valid` = `risky:external_verified:stop` (never `valid`) — a second, data-level guard beside `external.Resolve` |
| Every `runExternal` entry point capped (HIGH-fix has no siblings) | ✅ **exactly two** call sites (compute() inline for catch_all/provider_no_smtp; `EscalateExhausted` for retry-exhausted/gave-up-capacity); the enforcement gauntlet (global-kill → policy opt-in → budget+breaker `Decide`) is INSIDE `runExternal` as guard clauses before the vendor call — un-bypassable. Both sites thread the real `jobID` → per-job cap holds on both |
| Settable-but-ignored dial sweep | ✅ found + fixed one P6-introduced footgun: `TM_EXTERNAL_CONFIDENCE_CAP` was parsed but never read (runtime uses the per-policy `external.confidence_cap`). Removed the dial; the cap's single correct home is the policy JSON. Pre-existing 0-consumer dials (`AuditIntervalSeconds` superseded by the P4 watcher; `RawBlobRetentionDays` deferred retention sweep) are known-deferred, out of scope, not regressions |
| Accidental-enablement resistance (§7.4) | ✅ egress requires the AND of `TM_FALLBACK_MODE=enabled` + `TM_EXTERNAL_DPA_CONFIRMED=true` + a `TM_VENDOR_KEYS` entry; any missing fails closed; `Fallback==off` (global-kill) overrides; DPA flips only on exact `"true"`; `buildExternal` returns nil this phase (PENDING-REAL-VENDOR) = fail-closed; `runExternal`'s top guard is an independent backstop |
| Additivity | ✅ `vocab.go` byte-identical since Phase 0; `external_verified` reused (zero new vocab); gem unmodified (Door 5); `external_calls` buckets rented verdicts by `(residue_class × vendor)` + `email_hash` (the P7-weaning provenance) |
| Gate | ✅ 21 packages green on **real Postgres 16**, shuffle-stable; fresh migrations 0001–0011; vet clean; config-reference fresh; docs-updated |

**Phase 6 is closed.** The layer is built, DISABLED, fail-verified; no re-verification failed.

---

## 1. What Phase 7 is, and its one structural dependency

P7 closes the loop: a `send_outcome` from the sender joins back to `verifications.id`, is classified for what it *proves about the address* (§4 bounce classifier — the correctness heart), and — only for real-signal outcomes — recalibrates confidence per `(reason × provider)`, auto-promotes providers, measures true accuracy, and weans off the P6 fallback per-provider. It is **entirely brain-side and additive** (moves `confidence` numbers, mints zero vocab, uses the reserved `historical_bounce`, writes a new `calibration` store, never mutates the LOG).

**The one structural dependency P7 cannot satisfy alone:** the **sender is a separate system** (build-plan "your sender comes online"; GAP-48). P7 defines the *shape* of an arriving `send_outcome` and the *interface*; it does not build the mail-transfer agent, warmup, or bounce-webhook plumbing. **Per the owner: the sender is being built as "Feature 2" and does not exist yet.** This is not a P7 defect — it is the phase's premise. But it means P7's *live* value is gated on a cross-project deliverable, and its most load-bearing input (the feed) is gated on a hard authentication door.

---

## 2. Per-gate readiness — ready / not-ready / needs-owner-action

### 2.1 ADR-0001 — outcome-feed authentication (the hard pre-P7 door)  🔴 **NEEDS-OWNER-ACTION — this gates live P7**

**Status: the door is DECIDED in principle; the mechanism is deliberately open; the design work is not done → it BLOCKS live P7 (not local build).**

- **DECIDED (recorded, ACCEPTED 2026-07-17):** the outcome feed (TB-7) **is an authenticated channel** with the sender as an **identified principal**, and **TB-7 MUST be threat-modelled for a *malicious* sender** — not merely noisy. This is a hard architectural gate elevated from a dial (DR-1 / ADR-0001 / pre-takeoff §3.4). The reasoning is load-bearing and correct: **P4's statistical guards (sample-gate, hysteresis, cascade-brake) defend against *noise*, not an *adversary*.** A patient attacker who can write to the feed can craft consistent, gate-clearing, bounded-step forged outcomes that poison recalibration (A6), force auto-promotion → mass false-`valid` (A8), or blacklist real addresses via the historical-bounce gate (A10). Recommend-only (DR-2) reduces blast radius but **does not authenticate** — both are required.
- **OPEN (by design — owner / P7-time, NOT a blocker to *decide* now):** the **mechanism** — *signed webhooks* (sender signs each delivery) **vs.** *mutually-authenticated pull* (P7 pulls a sender-owned feed over a mutually-authenticated transport). ADR-0001 §Decision.3 explicitly leaves this open; DECISIONS §5 carries it as a parked sub-decision. **The kickoff asks "confirm the mechanism is decided" — it is NOT, and by ADR-0001's design it does not need to be decided *now*: what is decided is *that* the feed is authenticated + threat-modelled. The mechanism is chosen when P7 is built, alongside the sender's transport.**

**Why this blocks live P7 but not local build:** ADR-0001 is explicit — *"no P7 code until TB-7 authentication + threat model are designed."* But (a) the authentication lives at the *ingestion boundary*, which is exactly the seam a simulated feed stands in for, and (b) the sender must be able to authenticate on its side — a cross-project dependency (GAP-48) that cannot be satisfied until the sender exists. **So: the ingestion path + classifier + calibration are buildable against a simulated authenticated feed now; flipping to the real feed is gated on (1) the mechanism decision and (2) the sender implementing its half.**

**Owner action required before live P7:** (1) choose the mechanism (signed webhooks vs mutually-authenticated pull); (2) produce the TB-7 malicious-sender threat model; (3) coordinate with the sender project so both halves authenticate. Until (1)+(2) are designed, **no live P7 code ships** (the gate holds). This is the single most important line of the assessment: **P7's readiness is blocked at this door, honestly.**

### 2.2 The sender's existence + the `send_outcome` contract  🔴 **NOT-READY (cross-project, owner) — gates live P7**

- The sender is **"Feature 2," not yet built.** P7 §3.2 LOCKS the *shape* P7 consumes (`verification_id`, `event`, `message_id`, `sent_at`, `occurred_at`, `raw`; the rest derivable from the joined verdict) — so P7 can define and build against it unilaterally. But the **contract must be *agreed*** with the sender project: the sender must supply `verification_id` (it held the verdict when it decided to send) and a normalizable `event`/DSN. **GAP-48** governs this seam — P7 normalizes the sender's *raw* event (it does not trust a pre-classified label), so classification quality is bounded by DSN fidelity (the P6-GAP-47-shaped "untestable-until-integrated" honesty).
- **Needs-owner-action:** agree the `send_outcome` contract with the sender project (shape is P7's to fix; the *supply* is the sender's commitment). Not a code blocker for local build (a simulated feed emits the locked shape); a hard blocker for live value.

### 2.3 OQ-P7-2 — historical-bounce TTL + reversibility  ✅ **DECIDED + RECORDED (confirmed this pass)**

Confirmed in [`DECISIONS.md`](DECISIONS.md) §6 and P7 §4.6 / Appendix D.3: **TTL-windowed hard gate (6–12 months, `DIAL`) on a confirmed `mailbox_not_found` only, degrading to a confidence penalty after the window** — so a deleted-then-recreated mailbox recovers ("recovery is not a one-way door," the P4-canary instinct). The gate fires only on `invalid-signal`, never on a `zero-information` bounce. **No action — it is ruled and recorded.** (The TTL *value* within 6–12mo is a `DIAL`, owner-set at build time.)

### 2.4 OQ-P7-8 — platform "your sends train our accuracy" disclosure  🟡 **DECIDED (owner posture); integration-gated (cross-project)**

Confirmed DECIDED in [`DECISIONS.md`](DECISIONS.md) §6 and P7 §14 / Appendix D.6: **surface it (disclose-don't-hide)** — outcome data derived from user sends feeds calibration; the brain exposes the seam, the platform decides terms. This is the second cross-project privacy seam (after OQ-P6-6). **Needs-owner-action only at the platform layer** (the brain's obligation — expose the seam — is a P7-build item, not a blocker); the platform's actual terms/consent copy is a product decision, out of brain scope. Same posture as the P6 disclosure seam.

### 2.5 F-17 / PN-11 — ≥N independent invalid-signal outcomes before the historical-bounce gate fires  ✅ **RECORDED (defense-in-depth behind ADR-0001)**

Confirmed in [`DECISIONS.md`](DECISIONS.md) §5 (parked `DIAL`) + P7 §4.6 consolidation note. A *single* forged/mislabeled `mailbox_not_found` could suppress a real address for the TTL; PN-11 requires **≥N independent `invalid-signal` outcomes** before the gate fires. **N is a `DIAL`** (owner/P7-time), and whether ≥N is always-on or only-when-unauthenticated is parked — with ADR-0001 making authentication mandatory, ≥N is **layered defense**, not the sole guard. **No action to *decide* now; it is recorded as a build-time DIAL behind the DR-1 door.**

---

## 3. Buildable-now vs needs-the-real-sender (explicit split)

**Buildable NOW, locally, against a simulated outcome feed** (the P1–P6 pattern: Postgres + Go, no external system):
- The `calibration` store table + migration (the PN-14 sketch: `(reason, mx_provider, vendor?, residue_class?, confidence, version, effective_from/to, sample_n)`).
- The **bounce classifier** (§4) — a deterministic `event → {invalid-signal | valid-signal | zero-information}` mapping, fully testable against synthetic DSNs (the correctness heart, GAP-49; `mailbox_full→valid-signal`, blocked/policy/transient→zero-information). This is the highest-value local build.
- The **ingestion path** (§3) with idempotency on `(message_id, event)` and reject-and-flag integrity checks (§3.4/GAP-51) — behind an **authenticated boundary stub** (the boundary exists; the real credential is wired when the mechanism is chosen).
- **Recalibration** (§5) with the four feedback-loop guards (§6: sample-gate, bounded step, hysteresis, cascade brake) + the CAS-on-lease-holder exactly-once write (§5.5/GAP-53 — the same invariant proven in P5/P6, transferring a third time).
- **Provider auto-promotion (§7)**, **accuracy measurement (§8)**, **weaning (§9)** — all as mechanisms driven by a simulated outcome corpus. Cold-start behavior (§5.6: empty → P1 baseline) is literally testable.
- All of it **recommend-only by default** (DR-2 / shadow-mode) — zero live mutation on a fresh system (the fresh-system invariant, pre-takeoff §3.5).

**Genuinely needs the real sender (cannot be faked green — the honest ledger):**
- **Real accuracy numbers.** The whole point (§8) — measurable only against real send→bounce outcomes. A simulated corpus tests the *machine*, never the *accuracy* (the GAP-52 cold-start honesty; the P6-GAP-47 analogue).
- **Live authentication.** The sender must implement its half of the chosen mechanism (§2.1) — cross-project.
- **The agreed `send_outcome` contract** (§2.2) — the sender must actually supply the locked shape.
- **DSN-fidelity-bounded classification quality** (Appendix C.1) — the classifier is correct by construction, but its real-world accuracy depends on the sender's raw DSNs, un-mockable pre-integration.
- **Any promotion/weaning actually firing** — both are volume-gated per provider (§7.4/§9.4); on day one **zero** buckets are promoted or weaned. The moat is years of running it, not a ship-time feature (§14).

---

## 4. The honest headline

**Phase 7 is the most local-buildable-yet-least-live-ready of the seven.** The apparatus (classifier, recalibration, guards, promotion, weaning, the calibration store) is buildable now against a simulated feed, entirely additive, inside the frozen doors — and it should be built recommend-only so a fresh system performs zero live mutation. **But P7 delivers no real value until three cross-project things land: (1) ADR-0001's authentication mechanism is chosen and TB-7 is threat-modelled for a malicious sender [the hard door — currently principle-decided, mechanism-open, design-not-done → BLOCKS live P7]; (2) the sender ("Feature 2") exists and implements its half; (3) the `send_outcome` contract is agreed.** OQ-P7-2 (TTL) is ruled and recorded; OQ-P7-8 (disclosure) is ruled (platform-gated); F-17/PN-11 (≥N gate) is recorded as a build-time DIAL behind the door.

**Recommendation:** P7 may be built *locally, recommend-only, against a simulated authenticated feed* whenever the owner chooses — but **do not ship live P7 code until ADR-0001's mechanism is decided and threat-modelled.** The gate is real and it holds. Nothing in this assessment re-decides any OQ or door.

**Do not start Phase 7.**
