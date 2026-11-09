# ADR-0001 — The Phase-7 outcome feed is an authenticated channel (OQ-P7-1 elevated)

- **Status:** ACCEPTED (owner batch sign-off 2026-07-17)
- **Kind:** door-touching — creates a new one-way architectural commitment about P7's input trust model. Does **not** reopen any of the five frozen doors.
- **Deciders:** owner (cross-phase revision-pass acceptance)
- **Source finding:** cross-phase consistency report §0 headline / §1 **F-15** (+ F-16 promotion amplifier, F-17 gate poisoning) · [`open-decisions.md`](../open-decisions.md) **DR-1** · **PN-2**
- **Supersedes:** the P7 §3.2 / §13 framing of OQ-P7-1 as a `DECISION REQUIRED` *transport* choice. Transport-mechanism is still open; **authentication is no longer optional.**

## Context

Phase 7 closes the flywheel: a `send_outcome` feed drives automated **confidence recalibration** (A6), **provider auto-promotion** (A8), and **fallback weaning** (A9), plus the **historical-bounce gate** (A10) that can short-circuit a future verdict to `invalid/historical_bounce`.

Held against all seven specs at once, one thing becomes visible that no single-phase review could see:

> P4's control-loop guards — sample-size gate, hysteresis, cascade-brake — were designed against a **self-generated** signal: a box's own SMTP verdict stream, honest-but-noisy. Phase 7 reuses those same guards against an **external, cross-project** signal: the sender's outcome feed (OQ-P7-1), whose transport and authentication the P7 spec left as `DECISION REQUIRED`.

Statistical guards defend against **noise**. They do **not authenticate**. A patient adversary who can write to the feed can craft consistent, gate-clearing, bounded-step outcomes that every statistical guard accepts as honest:
- Forged `mailbox_not_found` → drives bucket confidence down, forces fallback spend up, and (via the historical-bounce gate) can **blacklist a real address** for the gate's TTL.
- Forged `delivered` / `mailbox_full` → inflates a provider's apparent trust → triggers **auto-promotion** → the system **stops running SMTP** for that provider → mass false-`valid`.

This is a "get the door right before it accrues history or a consumer" decision. Once P7 calibrates on the feed, the trust model is baked into the calibration store and the promoted-provider routing — not cheaply reversible. The feed is P7's most load-bearing input, and its authentication is a one-way architectural commitment.

## Decision

**OQ-P7-1 is elevated from a `DIAL`/transport choice to a hard architectural gate that must be satisfied before any Phase-7 code ships:**

1. The outcome feed (**TB-7**) is an **authenticated channel**. The sender is an **identified principal**, not an anonymous poster.
2. **TB-7 is threat-modelled for a *malicious* sender**, not merely a noisy or buggy one — the threat model explicitly includes forged-outcome poisoning of A6/A8/A9/A10.
3. Acceptable mechanisms: **signed webhooks** (sender signs each delivery) **or** **mutually-authenticated pull** (P7 pulls from a sender-owned feed over a mutually-authenticated transport). The choice between them is **left open** (see "Open").
4. Ingestion is **rate-limited per-provider**, and the P7 §10 classifier-distribution monitor is elevated to a **security** signal (an anomaly there is a possible poisoning event, not just a data-quality blip).
5. **Recommend-only (shadow-mode, DR-2) is a mitigation, not a substitute.** A human seeing a proposal before routing/confidence changes reduces blast radius; it does **not** authenticate the signal. Both are required; neither replaces the other.

**This is a phase-gate, not an implementation task in this pass.** No feed-auth code is written now — P7 is not being built. The gate is recorded in [`history/pre-takeoff-checklist.md`](../history/pre-takeoff-checklist.md) §3.4.

## Why this is door-touching but reopens no frozen door

- It **creates** a new one-way commitment: the trust model of P7's input. That is why it earns an ADR.
- It **touches** none of the five frozen doors. The `/verify` shape (Door 1), append-only LOG (Door 2), hashing scheme (Door 3), status/reason/recommendation vocabulary (Door 4), and never-fork/node-list (Door 5) are all unchanged. TB-7 is a *trust boundary*, not one of the five doors; authenticating it is purely additive to the frozen contracts.
- The companion **F-17 defense-in-depth** (require **≥N** independent invalid-signal outcomes before the historical-bounce gate fires) is applied as an additive `DIAL` on the *already-reserved* `historical_bounce` gate — **PN-11**, a SPEC-NOTE on P7 §4.6. It mints no new vocabulary (Door 4 intact).

## Consequences

- **Positive.** The flywheel's most dangerous input gains an integrity boundary before it is ever built. The poisoning class (F-15/16/17) is closed at the door rather than papered over with statistics. Recommend-only + authentication + ≥N-gate is layered defense.
- **Cost.** P7 cannot ship until the sender side can authenticate (a cross-project dependency — the sender is a separate project, GAP-48). This is deliberate: it is cheaper to discover the dependency now than after calibration has trusted forged data.
- **Left to P7-time / owner (flagged, not decided — see [`open-decisions.md`](../open-decisions.md) §5).**
  - Signed-webhooks **vs.** mutually-authenticated pull.
  - The value of **N** for the ≥N-invalid-signal gate, and whether ≥N is always-on or only-when-unauthenticated (with authentication now mandatory, ≥N is defense-in-depth).
  - Per-provider ingestion rate-limit values.

## Compliance / how it's checked

- **Pre-P7 gate (checklist §3.4):** no P7 code until TB-7 authentication + threat model are designed.
- **Behavioral test (senior-qa, when P7 is built):** an *unauthenticated* or *bad-signature* outcome delivery is **rejected at ingestion** (never reaches the classifier); a forged but well-formed outcome from an authenticated principal is still subject to the ≥N gate and the cascade-brake. Regression = any unauthenticated outcome influencing a calibration bucket.
