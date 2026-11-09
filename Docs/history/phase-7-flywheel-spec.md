# Phase 7 — The flywheel (close the loop) — Design Specification

**Status:** APPROVED (v1.0) — owner sign-off recorded (Appendix D). The **bounce classifier** (§4, the correctness heart) and **feedback-loop safety** (§6, the runaway guard) affirmed as the two load-bearing items; the **exclusions-are-the-discipline** framing (§4.3), the **historical-bounce TTL reversibility** (§4.6), the **down-fast/up-slow hysteresis** (§6.3), the **structural cold-start honesty** (§5.6/§7.4/§8.2/§14.2), and **first-class `unknown` surviving the flywheel** (§8.4) all explicitly endorsed. This is the final phase — the seven-phase arc is closed.
**Phase:** 7 of 7 · *Own data moat, real accuracy* — **the final phase.**
**Trigger (build-plan):** your sender comes online (a separate system). Its send/bounce signal can now be joined back to the verdict that authorized each send — so for the first time you can **measure your own accuracy** instead of asserting a confidence table, **recalibrate** from real outcomes, and **wean off** the rented Phase-6 fallback per-provider as your own history beats it.
**Depends on:** P1 (the empty `send_outcomes` + join table, `verifications.id` join anchor, promoted `mx_provider`, `from_cache`/override calibration-exclusion, reserved `historical_bounce`), P2 (SMTP verdicts + the `mailbox_full`→existence rule), P4 (the sample-size-gate / hysteresis / cascade-brake control-loop instincts), P6 (the `external_verified` separable-provenance population the weaning measures).
**Hands to:** nothing — this is the last phase. Anything genuinely beyond it is marked **`FUTURE / Phase-8+`** (§14), never `SEAM →`.

---

## 0. About this document

### 0.1 Purpose
Phase 7 **activates the dormant join table** (created empty in P1 §4.7, untouched for six phases) and closes the send → bounce → verdict loop. It consumes an **outcome signal** produced by the sender (a separate project — P7 does *not* build sending infrastructure), classifies each bounce into what it actually proves about the *address*, and uses only the outcomes that carry real signal to (a) **recalibrate** `confidence` per `(reason × provider)` bucket, (b) **auto-promote** a provider to `:mx`-only once its `valid`-labeled bounce rate is low enough to trust, (c) **measure true accuracy** ("of addresses labeled `valid`, what % hard-bounced"), and (d) **wean off** the Phase-6 fallback per-provider where the system's own history now beats the rented vendor.

This is a **planning deliverable — a written spec, not code.** It defines the outcome-ingestion contract, the bounce classifier, the recalibration rule, the feedback-loop guardrails, the promotion rule, the accuracy metric, and the weaning rule — as **shapes, contracts, gates, dials, and decision rules only.** No Go, no Ruby, no SQL/DDL, no sender code.

### 0.2 Scope discipline
- **In scope:** join activation on the P1 schema (§3); the bounce classifier + exclusion discipline (§4, highest rigor); per-`(reason × provider)` recalibration with a sample-size gate (§5); feedback-loop safety (§6, highest rigor); provider auto-promotion to `:mx`-only (§7); accuracy measurement (§8); per-provider fallback weaning (§9).
- **Consumes, does not build:** the sender. P7 defines the *shape of an arriving `send_outcome`* (§3.2) and the *interface by which outcomes arrive*, not the mail-transfer agent, warmup, or bounce-webhook plumbing. That is the sender project.
- **Out of scope entirely:** the platform's user-facing job UI, credits, CSV, list storage (a separate project); writing code; building the sender; running the flywheel live.
- **The one-way doors are frozen.** P7 is **additive-only** and — crucially — **moves numbers, not vocabulary.** `confidence` was always a `DIAL` (P1 §7.4); recalibrating it is P7's entire job. But the `status`/`reason`/`recommendation` enums are the frozen Door 4: P7 **adds or renames zero** reason/status values and uses the **already-reserved** `reason=historical_bounce` (P1 §3.4.3) for its one new gate. It fills `send_outcomes`/the join table (created empty in P1) and writes a new calibration store — touching **no frozen `verifications` column.**

### 0.3 Tagging convention (identical to P1–P6)
- **`LOCKED`** — a decision frozen this phase; ground truth for anyone building on it.
- **`DIAL`** — a knob whose *existence* is locked but whose *value* is tunable (thresholds, windows, sample minimums, promotion/wean cutoffs).
- **`DECISION REQUIRED`** — a genuine fork needing owner sign-off; enumerated as an OQ in §13.
- **`FUTURE / Phase-8+`** — explicitly beyond the seven-phase arc; named so scope stays honest (there is no later phase to `SEAM →`, so P7 names its own boundary instead).
- **`GAP-n`** — a contradiction, omission, or trap found across the eight source docs or against reality; logged in §12, never silently resolved.

### 0.4 Four principles this phase is built on
1. **A bounce is not a synonym for "invalid."** The classifier's whole job is to extract *what a bounce proves about the address* — and most bounce classes prove nothing about it. `mailbox_not_found` proves non-existence (gold); `mailbox_full` proves the mailbox **exists** (a `valid` confirmation, the counterintuitive one); `blocked`/reputation/policy/spam-reject prove something about **your sender**, not their address, and carry **zero information** — they are *excluded*, never counted. Getting this wrong permanently poisons the exact training data the flywheel exists to build (§4, GAP-49).
2. **The machine is the deliverable; the moat is years of running it.** P7 ships the *apparatus* that turns real outcomes into calibrated accuracy. That apparatus starts **empty** and only ever gets good on the providers your own send volume actually covers. Cold-start, it is weak and defers to the P1 baseline table + the P6 fallback; it compounds with volume. The spec must never read "and now you have a data moat" — the honest claim is "and now you have the machine that *builds* one" (§14).
3. **Recalibration must never run away.** Recalibration changes future verdicts, which change future sends, which change future bounce data — a closed loop that can spiral if a bad batch of outcomes moves a bucket too far, too fast. P7 borrows P4's control-loop instincts wholesale: a **sample-size gate** (no move below `min_bucket_samples`), **bounded step size + hysteresis** (a bucket can only move so far per cycle, and asymmetrically), and a **cascade brake** (a system-wide accuracy swing alerts a human, it does not auto-apply) (§6).
4. **Only self-earned, freshly-run, real-signal outcomes calibrate.** Cache hits (`from_cache=true`) didn't run fresh — excluded (P1 §3.2.1). Override verdicts (whitelist/blacklist) weren't checks — excluded (P1 OQ-19). **External-verified verdicts are bucketed *separately*** from SMTP-earned ones (P6 anti-laundering) — or the flywheel trains on the vendor's errors and can never wean. All three exclusion disciplines already exist in the schema; P7 **consumes** them, it does not invent them (§4.4, §5.3).

---

## 1. Phase 7 at a glance

### 1.1 What Phase 7 delivers (six outcomes)
1. **Join activation** — the empty `send_outcomes` table + `verification_outcomes` join (P1 §4.7) begin to be written, linking each real send outcome to `verifications.id` — the *exact verdict that authorized that send*, verdict-level, never fanned out across an address's history (§3).
2. **A bounce classifier** — the build-plan rule set, specified as a deterministic mapping from a provider's bounce event → `{invalid-signal | valid-signal | zero-information}`, with the exclusion discipline that a bounce about *your sender* never counts against the *address* (§4).
3. **Per-`(reason × provider)` recalibration** — `confidence` for each bucket is recomputed from observed hard-bounce rates, gated on a minimum sample (~200–500 sends/bucket, mirroring P4's sample-size instinct) and bounded in how far it can move per cycle (§5, §6).
4. **Provider auto-promotion (#4)** — a provider auto-promotes to `:mx`-only once its `valid`-label bounce rate crosses a trust threshold with adequate samples — the original build-plan "#4," now answerable from data instead of guesswork (§7).
5. **Accuracy measurement (#5)** — "of addresses labeled `valid`, what % hard-bounced with `mailbox_not_found`" ≤ target: the system's first *measured* accuracy number, per provider (§8).
6. **Per-provider fallback weaning** — the P6 fallback is dropped for a `(residue_class × provider)` bucket once the system's own verdict for that bucket demonstrably beats the vendor's — the SEAM → P7 that P6 built the separable provenance for (§9).

### 1.2 What Phase 7 consumes — the seams left by P1–P6
Every prior spec left P7 a specific, already-shaped hook. P7 **fills these; it does not redesign them.**

| Source seam | What was left | Where P7 uses it |
|---|---|---|
| **P1 §4.7** | `send_outcomes` (+ `verification_id` FK, `email_hash`, `domain`, `mx_provider`, `hash_scheme`, `sent_at`, `event`, `raw`) + `verification_outcomes` join, **created empty**; `verifications.id` = **authoritative verdict-level join anchor**; `email_hash` = rollup key only. | §3 join activation — anchored on this schema, not redesigned. |
| **P1 §3.2.2** | `mx_provider` **promoted** to a typed column from row #1, expressly as "the permanent grouping dimension of the Phase-7 flywheel." | §5 recalibration bucket, §7 promotion, §8 accuracy — all group by it. |
| **P1 §3.4.3 / §7.2** | `reason=historical_bounce` (invalid, P7) **reserved**; a historical-bounce gate reserved as "a new layer id in `order` + a `gates` rule (`historical_bounce:pass → invalid:historical_bounce:stop`)." | §4.6 the historical-bounce gate — uses the reserved reason + reserved gate shape. |
| **P1 §3.2.1 / OQ-19** | `from_cache=true` **calibration-excluded**; whitelist/blacklist override verdicts **calibration-excluded** (marked override-derived). | §4.4 / §5.3 exclusion discipline — consumed, not re-decided. |
| **P1 OQ-7** | disposable kept `risky`, never `invalid`, expressly "so bounce data stays clean." | §4.4 the `invalid` label means exactly one thing when calibrating. |
| **P2 §5.5 / §7.4** | catch-all/accept-all → `risky` (existence unprovable); "per-provider trust calibration = P7." | §4.5 delivered-on-catch-all is near-worthless evidence; §7 per-provider trust. |
| **P4 §5.3 / §5.5** | sample-size gate, hysteresis, cascade brake — the control-loop guards against small-N noise and runaway auto-remediation. | §6 feedback-loop safety — the same instincts applied to recalibration. |
| **P6 §3.4 / §6 / §8.4** | `external_verified` = a **separately measurable** rented population (own provenance, capped confidence, per `residue_class × provider × vendor`); the weaning query pre-written (P6 §6.3). | §5.3 bucket external separately; §9 run the weaning query P6 defined. |

### 1.3 Frozen inheritance — the five one-way doors, and how P7 stays inside them
| Door | P7's additive move — nothing frozen is touched |
|---|---|
| **Door 1** — `/verify` response shape, versioned & additive-only | A recalibrated `confidence` is a *different number in the same field* — the field's type/range (int 0–100) is unchanged. The historical-bounce gate may set `reason=historical_bounce` + a `flags.historical_bounce` marker — a reserved reason + an additive flag. **No** new top-level field. |
| **Door 2** — LOG append-only | P7 **never mutates a `verifications` row.** Recalibration writes a **separate calibration store** (§5.4); the historical-bounce gate reads it. Outcomes are appended to `send_outcomes`. The append-only log stays append-only. |
| **Door 3** — email hashing (`norm/v1` + `hmac-sha256/v1`, global pepper) | The join runs on `verifications.id` (primary) and `email_hash` (rollup) — both already locked. P7 adds **no** new hashing. `hash_scheme` on the outcome disambiguates if the scheme ever additively migrates. |
| **Door 4** — `status`/`reason`/`recommendation` enums additive-only | **The load-bearing door for P7.** Confidence numbers were always a `DIAL`; the vocabulary was the frozen door. P7 moves numbers freely and adds **zero** new `status`/`reason`/`recommendation` values, using only the **already-reserved** `historical_bounce`. Verified in §4.6 and the door-check (Appendix C). |
| **Door 5** — never fork the gem; node-list discipline | P7 is **entirely brain-side** (ingestion, classification, calibration store, gate). The gem never changes; the box never learns the flywheel exists; the sender is a separate system feeding an inbound signal. Clean door-5 posture. |

### 1.4 Make-or-break for this phase
- **The bounce classifier is the correctness heart** (the analogue of GAP-37 for P5). A misclassified bounce trains the model *wrong, permanently* — and unlike a bad verdict, a poisoned calibration bucket silently degrades every future verdict in that bucket. The `mailbox_full`→`valid` and the blocked→`zero-information` rules are the two that a naive "bounce = bad" implementation gets wrong (§4, GAP-49).
- **Feedback-loop safety is the runaway guard.** Because recalibration feeds the verdicts that feed the next batch of outcomes, a bad move can compound. The sample-size gate + bounded step + cascade brake are what keep the loop convergent instead of divergent (§6, GAP-50).
- **Honesty about cold-start is the trust item.** The flywheel is real and it is the endgame — but it is empty on day one and only ever covers the providers your volume touches. A spec that conflates "the machine that builds a moat" with "a moat" is selling the same illusion incumbent marketing sells. The done-vs-future boundary (§14) exists to keep this honest.

---

## 2. System context — where the flywheel sits

### 2.1 The one change to the picture
Every prior phase produced a verdict and *stopped*. The loop was open: the system asserted `valid`/`invalid`/`risky`/`unknown` and never learned whether it was right. Phase 7 closes it. When the sender (a separate system) sends to an address, its **outcome** (delivered / bounced / complaint) flows **back in** as a `send_outcome`, joins to the `verifications.id` that authorized the send, is classified for what it proves about the address, and — if it carries real signal — updates a **calibration store** that future verdicts read from. The verdict path itself is unchanged in shape; it simply reads calibrated numbers instead of a fixed P1 table, and gains one new optional gate (historical-bounce, §4.6).

```
                        ┌───────────────────────── THE OPEN LOOP (P1–P6) ───────────────────────┐
                        │                                                                        │
  email ──▶ /verify ──▶ [cheap layers ▸ SMTP box ▸ P5 queue ▸ P6 fallback] ──▶ verdict ──▶ LOG   │
                        │                                        (verifications row, id = anchor) │
                        └────────────────────────────────────────────────────────────────────────┘
                                                                          │
                                                                          │ (verdict authorizes a send —
                                                                          │  in the SEPARATE sender project)
                                                                          ▼
                        ┌───────────────────────── THE CLOSING LOOP (P7) ───────────────────────┐
                        │                                                                        │
   sender (separate) ──▶  send_outcome  ──▶ [§3 ingest] ──▶ join on verification_id              │
                        │  {event, message_id,               (verdict-level, NOT address fan-out) │
                        │   sent_at, occurred_at, raw}                    │                        │
                        │                                                 ▼                        │
                        │                                    [§4 bounce classifier]                │
                        │                              invalid-signal │ valid-signal │ ZERO-INFO   │
                        │                                    (exclude zero-info + §4.4 excludes)    │
                        │                                                 ▼                        │
                        │                        [§5 recalibration]  per (reason × provider)       │
                        │                    gated by §6 (sample-size · bounded step · brake)      │
                        │                                                 ▼                        │
                        │              ┌──────────────────┬───────────────────────┬────────────┐  │
                        │              ▼                  ▼                       ▼            ▼  │
                        │      calibration store   §7 provider promote     §8 accuracy    §9 wean │
                        │     (read by verdicts)   (→ :mx-only)            (of-valids-%)   fallback│
                        └────────────────────────────────────────────────────────────────────────┘
                                     │
                                     └────▶ feeds the NEXT verdict's confidence (the flywheel)
```

### 2.2 What P7 adds to the runtime, and what it doesn't
- **Adds (brain-side only):** an **outcome-ingestion path** (§3.2) that accepts `send_outcome` records from the sender; the **bounce classifier** (§4); a **recalibration job** that runs on a cadence (`DIAL`) over accumulated outcomes (§5); a **calibration store** the verdict path reads (§5.4); an optional **historical-bounce gate** (§4.6). All stateless-brain / all-state-in-Postgres, consistent with P1.
- **Does NOT add:** any sending capability, warmup logic, bounce-webhook parsing specific to an ESP, or the decision *to* send. The sender owns all of that and hands P7 a normalized `send_outcome`. **GAP-48** governs the shape-contract seam.

### 2.3 Door 5 preserved
The box remains a pure function `email → raw engine facts`; it never learns an outcome came back. The gem is untouched. The sender is a *separate project* feeding an inbound signal, exactly as the P6 vendor was a separate service feeding an inbound answer — P7 adds an ingestion surface in the brain, not a new box capability.

---

## 3. Join activation — filling the table P1 created empty

### 3.1 Anchor: the join is verdict-level, and that is not re-litigated `LOCKED`
P1 §4.7 deliberately chose the join anchor to be **`verifications.id`** — the specific verdict row that authorized a specific send — **not** `email_hash` (the address). P7 **honors this without redesign.** The reasoning P1 recorded (and P7 restates as the governing invariant):

> The append-only log has **many rows per address over time** (different dates, policies, configs, and — post-P7 — different calibrated confidences). The build-plan requirement is to join a bounce to *"the verdict that allowed the send,"* a single row. Anchoring on `email_hash` would fan one bounce out across every historical verdict for that address and make **per-verdict accuracy uncomputable** — you could never say "the verdict that said `valid` on 2026-08-01 under policy X was wrong," only "this address bounced sometime." That destroys the calibration signal.

- **`LOCKED`:** the primary join is `send_outcomes.verification_id → verifications.id`. `email_hash` is used **only** for address-level rollups (e.g. "has this address *ever* bounced" for the §4.6 historical-bounce gate) — never as the accuracy-attribution key.
- **`FUTURE / Phase-8+`:** attributing a bounce to a *blend* of prior verdicts, or address-level reputation scoring across verdicts, is explicitly out — it would reopen the P1 decision. Named, not built.

### 3.2 The `send_outcome` ingestion contract — the sender interface `LOCKED` (shape) / `DECISION REQUIRED` (transport)
P7 consumes an outcome; it does not build the sender. The **shape** of what arrives is locked to the P1 §4.7 `send_outcomes` sketch (P7 fills it, adds nothing structural). What the sender must supply per outcome:

| Field (from P1 §4.7) | Who supplies it | P7's use |
|---|---|---|
| `verification_id` | sender (it received the verdict when it decided to send) | **the authoritative join** — which verdict authorized this send |
| `message_id` | sender | idempotency + dedup of repeated webhook deliveries (§3.4) |
| `event` | sender (raw ESP/MTA event) | **input to the classifier (§4)** — normalized, not trusted as-is |
| `sent_at` | sender | temporal attribution: which verdict/config was *current at send time* (§3.3) |
| `occurred_at` | sender | when the bounce/delivery happened; ordering + late-arrival handling |
| `raw` | sender | untouched provider event → `raw` jsonb; the fidelity copy the classifier's exclusions can be re-audited against |
| `email_hash`, `domain`, `mx_provider`, `hash_scheme`, `provider_canonical_hash` | sender **or** backfilled from the joined `verifications` row | rollup dimensions; if the sender omits them, P7 derives them from the joined verdict (they were captured at verify time) |

- **`LOCKED` (shape):** the `send_outcome` record shape is P1 §4.7 — P7 adds no structural field. The sender's contract is "give me a `verification_id`, an `event`, a `message_id`, and two timestamps; the rest I can derive from the verdict."
- **`DECISION REQUIRED` (transport):** *how* outcomes arrive — push webhook from the sender vs. P7 polls a sender-owned outcome feed vs. a shared outcomes table both write/read — is **OQ-P7-1**. Recommendation: a **sender-owned append feed P7 ingests** (push or pull), so the sender stays the source of truth for its own events and P7 stays a consumer; avoids two writers on one table (a GAP-37-adjacent hazard). Either way the *shape* above is fixed.

> **[CONSOLIDATION NOTE — 2026-07-17 · additive · does not alter the frozen text above] `[Finding-1 / OQ-P7-1, accepted]`**
> Cross-phase review (consistency report §0/§1 F-15) **elevated OQ-P7-1 from a transport `DECISION REQUIRED` to a hard pre-P7 door.** The `transport` sub-question (push vs. pull) remains open, but the feed **MUST be authenticated with the sender as an identified principal, and TB-7 MUST be threat-modelled for a *malicious* sender** — because P7 reuses P4's guards, which were built against a *self-generated, honest-but-noisy* stream, against this *external, cross-project* feed. Statistical guards defend against noise, not an adversary. Recorded as [`DECISIONS.md`](../open-decisions.md) **DR-1** / **[ADR-0001](../adr/ADR-0001-outcome-feed-authentication.md)**; gated in [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) §3.4. Recommend-only defaults (shadow-mode DR-2) mitigate but do **not** authenticate. **Not built here** — P7 is not being implemented; this note gates it. Companion belt-and-suspenders at §4.6 (PN-11).

### 3.3 Temporal attribution — join to the verdict that was current at send time `LOCKED`
An address can be verified many times. The outcome must attribute to the verdict **the sender actually acted on**, which is why `verification_id` is supplied by the sender (it *had* the verdict in hand) rather than re-derived by P7 looking up "the latest verdict for this address." `sent_at` is the tiebreaker/validator: if the supplied `verification_id`'s `created_at` is *after* `sent_at`, that is a data-integrity contradiction (a send attributed to a verdict that didn't exist yet) — **rejected and flagged** (§3.4, GAP-51), never silently calibrated.

### 3.4 Idempotency + integrity of ingestion `LOCKED`
- **Idempotent on `message_id` + `event`:** a webhook re-delivery (ESPs retry) must not double-count a bounce into a calibration bucket. Dedup key `LOCKED` = `(message_id, event)`; a duplicate is a no-op append-guard, mirroring P5's exactly-once *settlement* discipline (the [[truemail-lease-cas-exactly-once-settlement]] invariant applies here too — see §5.5).
- **Reject, don't guess, on integrity failure:** unknown `verification_id` (no such verdict), `sent_at` after the verdict's `created_at` (§3.3), or a malformed event → the outcome is stored `raw` with an `ingest_error` marker and **excluded from calibration**, and surfaced on the ingestion-health metric (§10). A poisoned join is worse than a missing one.
- **`from_cache` outcomes:** if the joined verdict has `from_cache=true`, the outcome is recorded (it's a real send) but **excluded from calibration** (§4.4) — the cache hit didn't run a fresh check, so its accuracy isn't *this system's* fresh-check accuracy.

---

## 4. The bounce classifier — the correctness heart `LOCKED` (rules) / highest rigor

> **The single rule everything else defends: a bounce tells you about the *address* only when it is about the *address*.** Most bounce classes are about your sender, the receiving policy, or transient conditions — they prove *nothing* about whether the mailbox exists, and must be **excluded**, not counted as evidence either way. A classifier that treats every bounce as "invalid" poisons the training data the flywheel exists to build (GAP-49). This is to P7 what GAP-37 was to P5: the one place getting it wrong corrupts data silently and permanently.

### 4.1 The three verdict-classes a bounce maps to `LOCKED`
The classifier maps a normalized send outcome to exactly one of three **calibration classes** — *not* to the frozen verdict vocabulary (it never mints a `reason`; it decides what an outcome *counts as* when calibrating):

| Calibration class | Meaning | Effect on calibration |
|---|---|---|
| **`invalid-signal`** | the outcome is **positive proof the address does not exist / cannot receive** | counts as "the verdict was wrong if it said `valid`" — the gold negative signal |
| **`valid-signal`** | the outcome is **positive proof the mailbox exists** (delivered cleanly, or bounced *because the mailbox is full*) | counts as "the address is real" — confirms a `valid`, weakens an `invalid` |
| **`zero-information`** | the outcome says something about **your sender / the receiving policy / a transient**, **nothing** about the address | **excluded** — never counted for or against; logged for sender-health, not calibration |

### 4.2 The build-plan rule set — bounce event → calibration class `LOCKED`
Anchored on the build plan's stated rules (§178) and the P2 SMTP semantics. This is the mapping, not an invented taxonomy:

| Bounce / outcome event | Calibration class | Rationale |
|---|---|---|
| **`mailbox_not_found`** (hard 550 no-such-user, "user unknown," "no such recipient") | **`invalid-signal`** | the gold signal — the receiving server authoritatively denies the mailbox exists. This is the one outcome that proves `invalid`. |
| **`mailbox_full`** (452/552 over-quota, "mailbox full," "quota exceeded") | **`valid-signal`** | **the counterintuitive one** — a full mailbox is a mailbox that **exists**. It bounced, but it proves existence. Consistent with the P2/[[truemail-552-mailbox-full-valid]] ruling that `552` mailbox-full is positive existence proof. Counting this as `invalid` would be the classic poisoning error. |
| **delivered, no bounce** | **weak `valid-signal`** (down-weighted, §4.5) | evidence of `valid`, but *weak* — silent drops, spam-folder delivery, and accept-then-discard all look like "delivered." Not proof; a down-weighted positive. |
| **delivered on a known catch-all domain** | **near-`zero-information`** (§4.5) | a catch-all accepts everything; "delivered" there proves almost nothing about the specific mailbox (P1/P2 already flagged this). Down-weighted to near-zero. |
| **`blocked` / reputation / IP-blocklisted** | **`zero-information`** | about **your sender's IP/reputation**, not their address. Excluded. |
| **policy-reject / content / spam-reject (`spam_reject`)** | **`zero-information`** | the receiver's filtering policy rejected *your message*; the mailbox may well exist. Excluded. |
| **greylisted / deferred / transient 4xx (non-quota)** | **`zero-information`** | transient; a retry would likely deliver. Excluded (consistent with P2/P4 never counting `greylisted` as address-fault). |
| **complaint (spam report)** | **`zero-information`** for validity (`valid`-adjacent — a human read it) | proves a human exists behind it, but is a *sender-reputation* signal; not counted as validity evidence to avoid conflating "real person" calibration with "don't send" policy. Logged for sender-health. |
| **unclassifiable / ambiguous DSN** | **`zero-information`** | harden toward exclusion, never toward `invalid` — the same "harden toward `unknown`" instinct as P2 §Priority. |

### 4.3 Why the exclusions are the discipline, not the leftovers `LOCKED`
The `zero-information` bucket is **the majority of real-world bounces** and is the classifier's most important output, not its residue. A real send stream is full of blocks, deferrals, and policy rejects that have nothing to do with address validity. Counting even a *fraction* of them as `invalid-signal` would systematically bias every calibration bucket toward "worse than reality" — and because low confidence → fewer sends → fewer outcomes, a bucket could get *stuck* looking bad. **The exclusion of sender-fault outcomes is what keeps calibration measuring the address, not the sender.**

- **`LOCKED`:** the numerator of any accuracy/bounce-rate calculation counts **only `invalid-signal`** hard bounces classified `mailbox_not_found`. `zero-information` outcomes are **never** in the numerator or the denominator of an address-accuracy metric (they *are* counted in sender-health metrics, §10 — a different denominator).
- **`DIAL`:** the exact DSN-code → event mapping (which 5xx strings mean `mailbox_not_found` vs. policy-reject) is tunable and provider-specific; the *three-class taxonomy* and *which class each event maps to* is `LOCKED`.

### 4.4 The inherited exclusion disciplines — consumed, not re-decided `LOCKED`
Three exclusions already exist in the schema (P1/P6); P7 enforces them at calibration time:
1. **`from_cache=true` → excluded** (P1 §3.2.1) — served from cache, not a fresh check; its accuracy isn't this system's fresh-check accuracy.
2. **override verdicts (whitelist/blacklist) → excluded** (P1 OQ-19) — configuration overrides, not verifications; teaching the model from them teaches it the operator's config, not reality.
3. **`external_verified` → bucketed *separately*, never blended** (P6 §3.4, [[truemail-rented-verdict-provenance-separable]]) — a rented verdict's accuracy is the *vendor's*, not yours; blending them means you train on the vendor's errors and can never wean (§9). External verdicts get their *own* calibration buckets keyed by `vendor × residue_class × provider`.
4. **disposable stays `risky`, never `invalid`** (P1 OQ-7) — so the `invalid` label means exactly one thing (positive non-deliverability proof) when it appears in a calibration numerator.

### 4.5 Evidence-strength weighting — causation honesty `LOCKED` (the principle) / `DIAL` (the weights)
The build plan is explicit that not all positive outcomes are equal. P7 encodes **evidence strength**, not a naive `delivered = valid`:

| Outcome | Evidence strength toward `valid` | Why |
|---|---|---|
| `mailbox_full` bounce | **strong** | the server explicitly named an existing-but-full mailbox — near-proof of existence |
| delivered on a **non-catch-all** domain | **moderate** | real delivery to a discriminating server is decent evidence, but silent drops/spam-folder mean it's not proof |
| delivered on a **catch-all** domain | **near-zero** | the domain 250s everything; delivery proves the domain accepts mail, not that *this* mailbox exists (P2 §5.5) |
| `mailbox_not_found` bounce | **strong negative** | positive proof of non-existence |

- **`LOCKED`:** delivered-no-bounce is **weak** positive evidence and delivered-on-catch-all is **near-worthless** — never treated as equal to a `mailbox_full` existence proof or a clean discriminating-server delivery. Catch-all state is read from `catchall_domains` (P1 §4.3, written by the P2 probe).
- **`DIAL`:** the numeric weights themselves (how much a delivered-non-catch-all nudges a bucket vs. a `mailbox_full`) are tunable; their *ordering* (`mailbox_full` ≥ discriminating-delivery > catch-all-delivery ≈ 0) is `LOCKED`.

### 4.6 The historical-bounce gate — the one new gate, using the reserved reason `LOCKED`
Once an address has a confirmed `invalid-signal` (`mailbox_not_found`) bounce on record, re-verifying it should short-circuit to `invalid` — you have *ground-truth* proof it doesn't exist, stronger than any SMTP probe. This is the gate P1 §7.2 reserved:

- **Layer/gate shape (reserved in P1, now filled):** a new layer id in the policy `order` + the reserved gate rule `historical_bounce:pass → invalid:historical_bounce:stop`. It reads the address-level rollup (§3.1 — this is the *one* place `email_hash` rollup is the right key: "has this address *ever* hard-bounced").
- **Uses the reserved `reason=historical_bounce`** (invalid, P1 §3.4.3) — **zero new vocabulary.** This is the only place P7 emits a verdict-affecting value, and it was reserved a phase ago (Door 4 honored).
- **`DECISION REQUIRED` (OQ-P7-2):** does a historical bounce *expire*? A mailbox can be deleted then re-created; a person can reclaim a username. **Recommendation:** the gate fires only within a TTL window (`DIAL`, e.g. 6–12 months) and on a **confirmed `mailbox_not_found`** only (never on a `zero-information` bounce) — after the window, it degrades to a strong *confidence penalty* rather than a hard gate, so a re-created mailbox can recover. This keeps the gate from becoming a permanent, un-revisitable `invalid` — the same "recovery is not a one-way door" instinct as P4's canary (§5.4/GAP-30).
- **Anti-poisoning:** the gate fires **only** on `invalid-signal`. A `zero-information` bounce (blocked, greylisted) **never** creates a historical-bounce record — otherwise a sender-reputation problem would permanently mislabel real addresses as `invalid`. This is the classifier's exclusion discipline protecting the gate.

> **[CONSOLIDATION NOTE — 2026-07-17 · additive DIAL on the already-reserved gate · Door 4 intact] `[PN-11 / F-17, accepted]`**
> Cross-phase security review (F-17): a *single* forged/mislabeled `mailbox_not_found` can suppress a real address for up to the TTL. As defense-in-depth behind the DR-1 feed-authentication door (§3.2 note), the gate should require **≥N independent `invalid-signal` outcomes** before it fires, not one. **N is a `DIAL`** (owner/P7-time; see [`DECISIONS.md`](../open-decisions.md) §5) — this note adds a threshold to the *existing* reserved `historical_bounce` gate; it mints **no** new vocabulary and changes **no** frozen rule. With authentication now mandatory (DR-1), ≥N is layered protection, not the sole guard. **Not built here.**

---

## 5. Recalibration — moving the numbers Door 4 always let move `LOCKED` (mechanism) / `DIAL` (values)

### 5.1 What recalibration is, and what it is not
P1 §7.4 shipped a **fixed baseline-by-reason confidence table** and called it "explicitly a placeholder for" P7. Recalibration replaces the *placeholder values* with numbers observed from real outcomes — **within the frozen vocabulary.** It does not add reasons, does not change what `valid`/`invalid`/`risky`/`unknown` mean, and does not touch the response shape. It changes *how confident the system is* in a `(reason × provider)` bucket, based on how often that bucket was actually right.

- **`LOCKED`:** recalibration writes **only** `confidence` (an int 0–100, always a `DIAL` per Door 1) and only via the separate calibration store (§5.4). It **never** rewrites a `verifications` row (Door 2 append-only) and **never** mints a new `reason`/`status` (Door 4).

### 5.2 The bucket: `(reason × provider)` — the P1-promoted dimension `LOCKED`
The calibration unit is the `(reason × mx_provider)` pair — exactly why P1 promoted `mx_provider` to a typed column "from row #1." Examples: `(accepted × google)`, `(catch_all × o365)`, `(provider_no_smtp × yahoo)`. Each bucket accumulates: sends, `invalid-signal` bounces, `valid-signal` confirmations (weighted, §4.5), with `zero-information` outcomes excluded from both numerator and denominator (§4.3).

- **`LOCKED`:** the bucket key is `(reason, mx_provider)`. External-verified verdicts get a **parallel, separate** bucket space keyed by `(vendor, residue_class, mx_provider)` (§4.4.3) — never merged into the SMTP-earned buckets.
- **`DIAL`:** finer sub-bucketing (e.g. by policy, by `config_fingerprint`) is available if a bucket proves heterogeneous, but the base dimension is `(reason × provider)`.

### 5.3 The exclusions applied at recalibration `LOCKED`
Before an outcome enters a bucket's counts, it must pass every exclusion from §4.4: not `from_cache`, not an override verdict, correctly separated if `external_verified`, and classified `invalid-signal` or `valid-signal` (not `zero-information`). **These are consumed disciplines, already in the schema — §5 enforces them; it does not re-decide them.**

### 5.4 The calibration store — a separate, versioned, append-friendly store `LOCKED`
- **`LOCKED`:** recalibration output lives in a **new calibration store** (a `calibration` table keyed by `(reason, mx_provider, [vendor, residue_class])`), **not** in `verifications`. The verdict path reads the current calibrated confidence for a bucket at scoring time; if a bucket has no calibrated entry yet (cold-start), it falls back to the P1 baseline table (§5.6). This keeps Door 2 (append-only log) inviolate — the log records what confidence *was assigned*, the calibration store records the *current model*.
- **Versioned, not overwritten in place:** each recalibration cycle writes a new versioned row per bucket (effective-dated), so the confidence a verdict used is always reconstructable and a bad cycle is **revertible** (§6.4). This mirrors the append-only instinct and is the mechanism that makes the cascade brake's rollback possible.

> **[CONSOLIDATION NOTE — 2026-07-17 · additive · this table is new in P7, so a sketch adds no frozen change] `[PN-14, accepted]`**
> Schema review (consistency report §5, note 1): §5.4 names the calibration store's *key* and *versioning* but gives no column list (unlike P1 §4.7's `send_outcomes` sketch). Proposed sketch so the "revertible" guarantee has an explicit shape: `(reason text, mx_provider text, vendor text NULL, residue_class text NULL, confidence smallint 0–100, version int, effective_from timestamptz, effective_to timestamptz NULL, sample_n int, created_at timestamptz)`. **Two sub-decisions are surfaced, NOT decided** ([`DECISIONS.md`](../open-decisions.md) §5): (a) the *home* of the `(message_id, event)` dedup guard — DB `UNIQUE` on `send_outcomes` vs. app-level append-guard vs. staging table (P7 §3.4 locks the dedup *key*, not its enforcement site; see the companion note at P1 §4.7); (b) whether `residue_class` is promoted to a typed column or stays in `signals.external.residue_class` jsonb (report §5 note 3). Deciding either would be a new schema commitment — left for the owner. **Sketch only; no table is built here.**

### 5.5 Exactly-once, no-double-count writes `LOCKED`
The recalibration job is a lease-and-settle path: it reads a window of outcomes, computes bucket updates, and commits them. The [[truemail-lease-cas-exactly-once-settlement]] invariant applies — **the compare-and-set on the lease-holder, not the lease, is the guarantee.** A recalibration cycle that overran its lease (slow, not dead) must **discard its write** (match zero rows) rather than double-apply a bucket update on top of the cycle that reclaimed it. Single transactional store (Postgres), no external broker — the same reasoning as P5's queue. Ingestion dedup (§3.4) prevents the *input* double-count; the CAS prevents the *output* double-apply.

### 5.6 Cold-start behavior — the honesty gate `LOCKED`
A bucket with fewer than `min_bucket_samples` outcomes (§6.2) is **not calibrated** — the verdict path uses the P1 baseline table for it, unchanged. The flywheel improves only the buckets your volume covers; everything else behaves exactly as it did in P1–P6. **This is the mechanism that makes "empty on day one, compounds with volume" literally true in the code**, not just a caveat in prose (§14).

---

## 6. Feedback-loop safety — the runaway guard `LOCKED` / highest rigor

> **Recalibration is a closed control loop:** new confidence → different verdicts → different sends → different outcomes → new confidence. Uncontrolled, a bad batch can push a bucket the wrong way, which changes sending, which produces outcomes that *confirm* the bad push — a divergent spiral. P7 borrows P4's control-loop guards wholesale (P4 §5.3/§5.5), because the failure mode is identical in shape: a naive rule ("observe rate → set confidence") without the control theory oscillates or runs away (GAP-50).

### 6.1 The four guards `LOCKED` (the set) / `DIAL` (the values)
| Guard | What it prevents | Inherited from |
|---|---|---|
| **Sample-size gate** | small-N noise moving a bucket on a handful of lucky/unlucky sends | P4 §5.3 guard 1 (GAP-27) |
| **Bounded step size** | one cycle slamming a bucket from 90→10 on a bad window | new (P7) — the recalibration analogue of hysteresis |
| **Hysteresis (asymmetric move)** | a bucket flapping around a threshold cycle-to-cycle | P4 §5.4 |
| **Cascade brake** | a system-wide swing auto-applying instead of alerting a human | P4 §5.5 (GAP-28), [[truemail-fleet-brake-no-cascade]] |

### 6.2 Sample-size gate `LOCKED`
A bucket's confidence is **not recalibrated** until it has accumulated ≥ `min_bucket_samples` qualifying outcomes (§5.3). Below that, it keeps the P1 baseline (§5.6).
- **`DIAL`:** `min_bucket_samples` — the build plan's own guidance is **~200–500 sends per bucket** before trusting a recalibration; start at the high end (500) and lower only with evidence. Mirrors P4's `min_samples` instinct exactly, at flywheel scale.
- **Why:** a bucket that has seen 8 sends and 1 bounce is *not* a 12.5%-bounce bucket; it's noise. Recalibrating on it would be the GAP-27 error at the calibration layer.

### 6.3 Bounded step + hysteresis `LOCKED`
- **Bounded step (`LOCKED`, `DIAL` value):** a single recalibration cycle may move a bucket's confidence by at most `max_step` points (`DIAL`, e.g. ≤ 10–15 per cycle). A bucket that "should" move 90→20 gets there over several cycles, each re-confirmed by fresh outcomes — so a single poisoned window can never fully capture a bucket before the next cycle (and the §6.4 brake) catches it.
- **Hysteresis (`LOCKED`):** moving confidence **down** (toward less trust, after observed bounces) is allowed **faster/easier** than moving it **up** (toward more trust) — asymmetric, mirroring P4's "quick to quarantine, slow to trust." Rationale: a false-high confidence sends bad mail (costly, reputation); a false-low confidence just over-defers to fallback/`risky` (safe). **Trust is earned slowly, doubt is applied quickly** — the same asymmetry as [[truemail-rented-verdict-provenance-separable]]'s "trust a no more than a yes."

### 6.4 Cascade brake — a system-wide swing alerts, it does not auto-apply `LOCKED`
Mirroring P4's fleet-wide guard (OQ-P4-9, [[truemail-fleet-brake-no-cascade]]): if a recalibration cycle would move **many buckets at once** or **a high-volume bucket by a large amount** — the signature of a systemic cause (a sender IP reputation collapse misread as address invalidity, a classifier bug, a bad outcome feed) — the cycle **does not auto-apply.** It **raises a critical alert, holds the current calibration, and requires human confirmation.**
- **`LOCKED`:** the brake errs toward **alert, not auto-apply** — a bad global recalibration is far more dangerous than a delayed good one, exactly as a fleet-wide auto-quarantine was more dangerous than a delayed one in P4.
- **Revertibility:** because the calibration store is versioned/effective-dated (§5.4), a cycle that slipped through and proved bad is **rolled back** to the prior version, not hand-repaired. Recovery is not a one-way door.
- **`DIAL`:** the "system-wide swing" definition (e.g. > N% of high-volume buckets moving > `max_step` in one cycle) — start conservative.

> **[CONSOLIDATION NOTE — 2026-07-17 · additive · applies to both brakes] `[PN-8 / F-12, accepted]`**
> Cross-phase review: the cascade brake (here) and P4's fleet-brake (§5.5) both **hold for a human** but no phase specifies a **brake-hold timeout / default-on-timeout behavior** or an **authenticated confirm**. An alerting-pipeline outage silently neutralizes both safety valves. Proposed additions: (1) specify default-on-timeout behavior (stay held vs. auto-release) — the *value/behavior* is owner/phase-time and **flagged, not decided** ([`DECISIONS.md`](../open-decisions.md) §5); (2) the human confirm is an **authenticated, admin-audited** action (rides the DR-3 admin-audit gate); (3) monitor alert-pipeline health so a dead channel is itself alerted. Additive to the existing `LOCKED` brake; changes no rule. Companion note on P4 §5.5. **Not built here.**

### 6.5 The loop cannot fabricate its own confirmation `LOCKED`
The deepest safety property: a bad confidence push changes *which addresses get sent to*, so a runaway could in principle manufacture outcomes that confirm itself. Three properties break that spiral:
1. **`zero-information` exclusion (§4.3)** — the majority of outcomes a reputation problem generates (blocks, deferrals) are excluded, so a sender-reputation collapse cannot masquerade as mass address-invalidity.
2. **Down-only-fast, up-only-slow (§6.3)** — the loop is biased toward the *safe* error (over-deferring), so drift lands on `risky`/fallback, not on confidently sending bad mail.
3. **Human-in-the-loop brake (§6.4)** on the systemic signature — the one pattern automation can't safely resolve alone gets a human, never an auto-apply.

---

## 7. Provider auto-promotion to `:mx`-only — the original "#4," now answerable `LOCKED` (rule) / `DIAL` (thresholds)

### 7.1 What "#4" was, and why it was un-answerable until now
The big providers (Gmail/Yahoo/O365) are routed to `:mx` — the system never SMTP-probes them, so it can never earn an SMTP-grade `valid` for them; they land `risky/provider_no_smtp` and (pre-P7) go to the P6 fallback (residue class b). The build-plan "#4" asks: *when can I trust a provider enough to promote it to `:mx`-only* — i.e. treat "the domain routes to this provider and passed cheap layers" as good enough, without SMTP or fallback? Pre-P7 this was a guess. With real bounce data per provider, it's a measurement.

### 7.2 The promotion rule `LOCKED`
A provider auto-promotes to `:mx`-only when its **`valid`-label bounce rate** — of the verdicts this system labeled `valid` (or the promotion-candidate label) for that provider, the fraction that hard-bounced `mailbox_not_found` — is **below `promote_threshold`**, with **≥ `min_promote_samples`** qualifying outcomes.
- **`LOCKED`:** promotion requires *both* a low measured bounce rate *and* adequate samples — never one without the other (the §6.2 sample-size discipline applies; a provider with 10 clean sends is not "trusted," it's untested).
- **`LOCKED`:** promotion is **reversible** and subject to the same hysteresis as recalibration (§6.3) — a promoted provider whose bounce rate later climbs back above a (higher, hysteretic) `demote_threshold` is **demoted** back to fallback/`risky`. Quick to demote, slow to promote.
- **`DIAL`:** `promote_threshold` (e.g. ≤ 1–2% `mailbox_not_found` on `valid`-labels), `min_promote_samples`, and the hysteretic `demote_threshold`.

### 7.3 What promotion changes, within the frozen doors `LOCKED`
Promotion is a **routing/policy** change (which layers run for that provider), plus a calibrated **confidence** for the `(provider_no_smtp × provider)` or the promoted-label bucket — **not** a new vocabulary value. A promoted provider's verdict still uses existing statuses/reasons; it just earns a higher calibrated confidence and stops being routed to the fallback (§9 — promotion and weaning are two views of the same "your own data now beats renting" event). The `mx_provider` promoted column (P1) already carries the dimension; P7 adds a promotion flag/state to the provider-routing config (a `DIAL`/config surface, not a Door-4 change).

### 7.4 The honest limit `LOCKED`
Auto-promotion only ever fires for providers your **own send volume covers** to `min_promote_samples`. A provider you rarely send to never accumulates the evidence and stays on the cautious (fallback/`risky`) path indefinitely — correctly. Promotion is *earned per provider by volume*, never global. **This is the flywheel's coverage limit made concrete** (§14).

---

## 8. Accuracy measurement — the original "#5," the system's first true number `LOCKED`

### 8.1 The metric `LOCKED`
The build-plan "#5": **of the addresses this system labeled `valid`, what percentage hard-bounced with `mailbox_not_found`.** That percentage — for the first time in seven phases — is a **measured accuracy**, not an asserted confidence.
- **Numerator (`LOCKED`):** `valid`-labeled verdicts whose joined outcome classified `invalid-signal` (`mailbox_not_found`) — a genuine false-`valid`.
- **Denominator (`LOCKED`):** `valid`-labeled verdicts that got **any calibratable outcome** (i.e. excluding `zero-information`-only sends, `from_cache`, overrides — §4.3/§4.4). A `valid` that only ever got a `blocked` bounce is *not* in the denominator; you don't know if it was right.
- **Target:** "≤ target" (`DIAL`) — the accuracy bar the system holds itself to per provider.

### 8.2 Reported per provider, never as one global vanity number `LOCKED`
Accuracy is reported **per `mx_provider`** (and optionally per `reason`), because a single blended number hides exactly what matters: you may be 99.5% accurate on the providers you SMTP-probe and unmeasured on the ones you route to `:mx`. **`LOCKED`: no single global accuracy figure is published without its per-provider breakdown and its coverage (what fraction of `valid`-labels even got a calibratable outcome).** Reporting "99% accurate" while 80% of `valid`-labels never got a measurable outcome would be the overclaim §14 exists to prevent.

### 8.3 Accuracy is also the input to promotion and weaning `LOCKED`
The §8.1 metric *is* the signal §7 (promotion) and §9 (weaning) threshold against. They are three consumers of one measurement: measure the truth (§8), trust a provider once the truth is good enough (§7), stop renting once your truth beats the vendor's (§9).

### 8.4 The `valid`-label caveat is `unknown`-honest `LOCKED`
Accuracy is measured on `valid`-labels because those are the falsifiable claims (a `valid` that bounces `mailbox_not_found` is provably wrong). `unknown` and `risky` are **not** accuracy-scored the same way — an `unknown` was never a claim to be right or wrong about (that's the whole point of first-class `unknown`). P7 does **not** retroactively pressure the system to convert `unknown`→`valid` to look more "accurate"; doing so would relitigate the founding discipline. `unknown` staying first-class survives the flywheel intact.

---

## 9. Per-provider fallback weaning — spending the SEAM P6 left `LOCKED` (rule) / `DIAL` (thresholds)

### 9.1 The weaning query P6 already wrote
P6 §6.3 pre-defined the exact query weaning runs, and guaranteed the provenance to answer it ([[truemail-rented-verdict-provenance-separable]]):

> *"For `(residue_class × mx_provider × vendor)`, what fraction of the vendor's `valid` claims actually bounced? Once my own verdict for that bucket has a lower bounce rate than the vendor's, drop the fallback for that bucket."*

P7 **runs** it. Because P6 kept the rented population separable (`WHERE reason='external_verified'`, bucketed by vendor × residue_class × provider) and capped its confidence below any self-earned grade, P7 can measure the vendor's real accuracy **independently** of its own — the precondition for weaning that P6 built and this spec now consumes.

### 9.2 The weaning rule `LOCKED`
For a `(residue_class × provider)` bucket, **drop the P6 fallback when the system's own verdict for that bucket has a *measured* `mailbox_not_found` bounce rate at least as low as the vendor's**, with adequate samples on **both** populations (§6.2 gate on each).
- **`LOCKED`:** weaning is **per-bucket**, never global — you may beat the vendor on `catch_all × google` long before `provider_no_smtp × yahoo`, exactly as P6 §6.2 anticipated ("you may beat the vendor on catch-all before provider-no-SMTP").
- **`LOCKED`:** weaning requires the system's own bucket to be **measured** (not baseline-guessed) — you can only stop renting once you've *earned* the evidence that you're better, not merely hoped it.
- **`LOCKED` (reversible):** a weaned bucket whose own accuracy later degrades **re-enables** the fallback (hysteresis, §6.3) — weaning is not a one-way door either.
- **`DIAL`:** the margin by which own-accuracy must beat the vendor before dropping (recommend a small *safety margin*, not mere parity, so you don't wean on noise), and the sample minimums on both populations.

### 9.3 What weaning is not `LOCKED`
Weaning is **not** turning the fallback off wholesale, and **not** a cost decision made in the abstract — it is a *measured, per-provider* retirement of rented coverage as owned coverage overtakes it. The P6 external-spend budget/circuit-breaker (P6 §5) still governs spend on the buckets not yet weaned; weaning simply shrinks, over time and by evidence, the set of buckets that still reach the vendor. **The endgame is that the fallback's footprint asymptotically shrinks to the providers your volume never covers — which may never be zero** (§14).

### 9.4 The compounding, stated honestly `LOCKED`
Weaning is the visible face of the flywheel compounding: every send grows the owned-accuracy evidence, which retires more rented coverage, which... The spec states plainly that this is a **multi-year, volume-gated process**, not a switch — see §14. On day one, **zero** buckets are weaned (none are measured yet); the fallback carries full residue coverage exactly as P6 shipped it.

---

## 10. Observability — measuring the machine that measures `LOCKED` (the set) / `DIAL` (thresholds)

Consistent with P4 §8: the metrics/alerts *set* is `LOCKED`; thresholds/channels are `DIAL`. New P7 signals:

| Metric | What it watches | Why |
|---|---|---|
| **ingestion health** | rate of `ingest_error` (unknown `verification_id`, `sent_at` vs. verdict integrity, malformed events) (§3.4) | a broken outcome feed silently starves the flywheel or poisons it; must be visible |
| **classifier distribution** | share of outcomes landing `invalid-signal` / `valid-signal` / `zero-information` | a sudden shift toward `invalid-signal` is the signature of a sender-reputation problem masquerading as address-invalidity (the §4.3 poisoning risk) |
| **recalibration deltas** | how far buckets moved this cycle; count of buckets hitting `max_step` | the §6.4 cascade-brake input; a wide swing = the systemic-cause alert |
| **per-provider accuracy (#5)** | of-valids-%-bounced, per `mx_provider`, **with coverage** (§8.2) | the headline honesty metric; never reported without coverage |
| **coverage / cold-start** | fraction of buckets still on the P1 baseline (uncalibrated) vs. calibrated | makes "empty on day one, compounds with volume" observable, not just asserted |
| **fallback footprint** | share of residue still routed to the vendor vs. weaned buckets (§9) | the flywheel's progress bar — shrinking footprint = compounding working |
| **brake events** | recalibration cycles held for human confirmation (§6.4) | a spike means something systemic is being (correctly) caught |

- **Alerts (`LOCKED` set):** ingestion-error spike (feed broken); classifier-distribution shift (poisoning signature); **cascade-brake fired** (critical — a systemic recalibration was held, mirrors P4's fleet-wide critical); accuracy regression on a promoted provider (a promotion going bad → auto-demote candidate, §7.2).
- **`SEAM →` nothing:** this is the last phase; there is no later observability seam. `FUTURE / Phase-8+`: automated remediation beyond the auto-demote/auto-re-enable-fallback the spec already defines (e.g. auto-tuning `max_step` from convergence behavior) is named-not-built.

---

## 11. The additive ledger — everything P7 touches, and proof it's additive `LOCKED`

| Surface | P7's change | Additive proof |
|---|---|---|
| `verifications` (Door 2) | **read-only** at calibration; the historical-bounce gate reads a rollup | no column added, no row mutated — append-only intact |
| `confidence` field (Door 1) | recalibrated to different values | type/range unchanged (int 0–100); was always a `DIAL` |
| `reason`/`status` (Door 4) | uses reserved `historical_bounce`; adds **zero** new values | reserved a phase ago (P1 §3.4.3); door honored |
| `send_outcomes` + join (P1 §4.7) | **begins to be written** (was created empty) | filling a table designed for exactly this; no structural change |
| `calibration` store | **new table** | net-new, keyed by `(reason, mx_provider[, vendor, residue_class])`; touches no frozen schema |
| policy `order`/`gates` | adds the reserved historical-bounce gate + a provider-promotion flag | uses the P1 §7.2 reserved extension point; a new layer id + reserved gate rule |
| `flags` (Door 1) | optional `flags.historical_bounce` marker | additive jsonb flag, no top-level field |
| routing/policy config | provider promotion/demotion state; per-bucket fallback enable | config-surface dials, not vocabulary |

---

## 12. GAPs — contradictions/traps across the eight source docs `LOCKED` (logged, never silently resolved)

| GAP | Where it surfaces | The trap | Resolution / stance |
|---|---|---|---|
| **GAP-48** | build-plan "your sender" / P1 §4.7 `send_outcomes.event` | The sender is a **separate project**; its outcome/event vocabulary and delivery mechanism are **not specified anywhere in the eight docs.** P7 can't classify what it can't parse. | **Contract, not assumption (§3.2):** P7 defines the *required shape* of an arriving `send_outcome` (verification_id, event, message_id, two timestamps, raw) and normalizes the sender's raw event at ingestion. Transport is **OQ-P7-1**. The seam is made explicit, not papered over. |
| **GAP-49** | build-plan §178 "mailbox_full = valid" vs. naive "bounce = invalid" | **The correctness heart.** A bounce is intuitively "bad," so a naive implementation counts *every* bounce (including `mailbox_full` and sender-fault blocks) as `invalid` — permanently poisoning calibration. | **§4 classifier** — three calibration classes; `mailbox_full`→`valid-signal`; blocked/policy/transient→`zero-information` (excluded). The exclusions are the discipline (§4.3). Highest rigor. |
| **GAP-50** | recalibration as a closed loop (no prior doc addresses loop stability) | Recalibration feeds the verdicts that feed the next outcomes — a control loop that can **run away or oscillate** if driven naively (the build plan says "recalibrate" without the control theory, exactly as P4's build-plan line said "auto-quarantine" without it — GAP-27/28). | **§6 feedback-loop safety** — sample-size gate + bounded step + hysteresis + cascade brake, borrowed wholesale from P4; plus the §6.5 self-confirmation break. |
| **GAP-51** | P1 §4.7 `verification_id` nullable "if a send ever occurs with no prior verdict" + `sent_at` | A send with no prior verdict, or an outcome whose `sent_at` **precedes** the verdict's `created_at`, is a **join/temporal-integrity contradiction** that would attribute a bounce to the wrong (or no) verdict. | **§3.3/§3.4** — reject-and-flag on integrity failure; store `raw` + `ingest_error`, exclude from calibration, surface on ingestion-health (§10). A poisoned join is worse than a missing one. |
| **GAP-52** | build-plan "rival the incumbents" vs. cold-start reality | The endgame framing invites the **overclaim** that shipping P7 = having a data moat. In truth the machine ships **empty** and only covers volume-touched providers. | **§14 done-vs-future boundary + §5.6 cold-start in code** — the machine is the deliverable; the moat is years of running it. Made observable via the coverage metric (§10), not just prose. |
| **GAP-53** (inherited, now active) | [[truemail-lease-cas-exactly-once-settlement]] — GAP-37 | Recalibration is a new lease-and-settle path; a slow (not dead) cycle could **double-apply** a bucket update. The queue solved this once; a new writer reintroduces the risk. | **§5.5** — CAS-on-lease-holder, single transactional store, no broker. The invariant transfers to the calibration writer exactly as it did to P6's spend-once. |
| **GAP-54** | P6 anti-laundering vs. P7 blending temptation | When calibrating, it is tempting to pool external-verified outcomes with SMTP-earned ones for "more samples." Doing so trains on the vendor's errors and destroys weaning (P6 §3.4). | **§4.4.3 / §5.2** — external buckets are **parallel and separate**, never merged. Consumed P6 discipline, enforced at the calibration layer. |

> **No GAP silently resolved.** GAP-49 (classifier) and GAP-50 (loop stability) are the sharpest — both are the build plan stating an outcome ("classify bounces," "recalibrate") without the correctness/control theory, and both are handled by explicit mechanism, not by trusting the naive rule. GAP-52 (cold-start overclaim) is a *framing* trap and is handled by the honesty boundary (§14), not a mechanism — flagged as such.

---

## 13. Open questions / decisions required

| OQ | Question | Recommendation | Where |
|---|---|---|---|
| **OQ-P7-1** | Outcome **transport**: sender push-webhook, P7 pull from a sender feed, or shared table? | **Sender-owned append feed P7 ingests** (push or pull) — sender stays source of truth for its events; avoids two writers on one table (GAP-37-adjacent). Shape is fixed regardless (§3.2). | §3.2 · `DECISION REQUIRED` |
| **OQ-P7-2** | Does a **historical bounce expire**? (deleted-then-recreated mailbox) | **TTL-windowed hard gate (6–12 mo, `DIAL`) on confirmed `mailbox_not_found` only**, degrading to a confidence penalty after the window — so a re-created mailbox recovers. Recovery is not a one-way door. | §4.6 · `DECISION REQUIRED` |
| **OQ-P7-3** | `min_bucket_samples` value | **Start at 500** (build plan's upper "~200–500"), lower only with evidence. Mirrors P4 conservative-first. | §6.2 · `DIAL` |
| **OQ-P7-4** | `promote_threshold` / `demote_threshold` for provider auto-promotion (#4) | `promote ≤ ~1–2%` `mailbox_not_found` on `valid`-labels with `min_promote_samples`; hysteretic `demote` higher. Quick to demote, slow to promote. | §7.2 · `DIAL` |
| **OQ-P7-5** | Accuracy **target** for #5 ("of valids, % bounced ≤ target") | Owner-set per provider; **publish with coverage, never as a bare global number** (§8.2). The number is a bar you hold yourself to, not a marketing figure. | §8.1 · `DECISION REQUIRED` |
| **OQ-P7-6** | Weaning **margin**: parity or safety margin before dropping the vendor per bucket? | **Safety margin, not parity** — don't stop renting on noise; require own-accuracy to beat vendor by a `DIAL` margin with samples on both. | §9.2 · `DIAL` |
| **OQ-P7-7** | Recalibration **cadence** (how often the job runs) | Conservative (e.g. daily/weekly rollup), not per-outcome — batch windows dampen twitch and make the cascade brake's "per-cycle" bound meaningful. | §5 · `DIAL` |
| **OQ-P7-8** | Does the platform surface **"your sends now train our accuracy"** to users? (the P6 §7.5 privacy-seam analogue) | **Surface it** — outcome data derived from user sends feeds calibration. Same disclose-don't-ship-silent posture as [[truemail-platform-privacy-egress-contradiction]]. Brain exposes the seam; platform decides terms. | §14 · `DECISION REQUIRED` |

---

## 14. Done vs. Phase-8/future — closing the seven-phase arc without overclaiming

> This is the final phase, so it names its own boundary rather than passing a `SEAM →` forward. The purpose of this section is the one thing the owner flagged as the real risk of P7: **honesty about what shipping this actually gets you.**

### 14.1 What is genuinely done at the end of P7 `LOCKED`
- **The loop is closed.** A verdict can be checked against reality; the system can, for the first time, be **wrong in a measurable way** instead of merely confident.
- **The machine that builds a moat exists and runs:** ingestion, a correct bounce classifier, gated recalibration, provider auto-promotion, a measured accuracy number, and per-provider weaning — all inside the frozen doors, all additive.
- **The founding disciplines survived the flywheel:** `unknown` stays first-class (§8.4), the `invalid` label still means exactly one thing (§4.4), the log stays append-only (§5.4), the vocabulary door never opened (§4.6), and rented verdicts stayed separable end-to-end (§9).

### 14.2 What is explicitly NOT done — the cold-start truth `LOCKED`
- **On day one the machine is empty.** Every bucket is uncalibrated and falls back to the P1 baseline table (§5.6); **zero** providers are promoted (§7); **zero** buckets are weaned (§9). The system behaves *exactly* as it did at the end of P6 until real volume accumulates. This is not a caveat — it is literally how §5.6/§7.4/§9.4 are specified.
- **It only ever gets good where your volume goes.** Accuracy, promotion, and weaning are **per-provider, sample-gated.** A provider you rarely send to is never calibrated and correctly stays on the cautious path forever. The flywheel's coverage is bounded by your sending footprint — **it does not, and cannot, make you accurate on providers you don't send to.**
- **The moat is years of running this, not a feature you shipped.** The deliverable is the *apparatus*; the moat is the *accumulated, volume-gated, per-provider accuracy* that apparatus produces over time. **A spec (or a pitch) that says "and now you have a data moat" is selling the illusion incumbent marketing sells.** The honest sentence is: *"and now you have the machine that builds one, which starts empty and compounds with every send."*

### 14.3 What is `FUTURE / Phase-8+` (named, not built)
- **Cross-verdict / address-level reputation** (blending many verdicts per address) — deliberately excluded to protect the P1 verdict-level join (§3.1).
- **Automated control-loop tuning** (auto-adjusting `max_step`/`min_bucket_samples` from observed convergence) — P7 ships fixed, human-set dials + the cascade brake; adaptive control is later.
- **Multi-vendor comparison/weighting** as an active strategy (P6 §8.4 built the *possibility*; P7 weans off *a* vendor per bucket but does not run a live multi-vendor tournament).
- **Sharing/monetizing the accuracy data** or any cross-tenant model — a product/privacy decision far beyond the brain (touches OQ-P7-8 and the [[truemail-platform-privacy-egress-contradiction]] posture).

### 14.4 The arc, honestly summarized `LOCKED`
P1–P6 built a system that produces **honest verdicts** — it never guesses to avoid `unknown`, never launders a rented answer, never fabricates certainty. P7 adds the one thing that turns honest-but-unmeasured into **honest-and-measured**: the loop that checks verdicts against reality and calibrates from it. That is a genuinely different and more valuable system than a truemail wrapper — **but its edge over an incumbent exists only on the providers your own sending has covered enough to measure.** Shipping P7 is finishing the machine. Building the moat is using it, for years, at volume. The spec closes the seven-phase arc by refusing to conflate the two.

---

## Appendix A — Upstream ground truth (what P7 anchors on, verbatim intent)

| Anchor | Source | What P7 must not redesign |
|---|---|---|
| Verdict-level join on `verifications.id`; `email_hash` = rollup only | P1 §4.7, resolved M1/M2 | §3.1 — the join is verdict-level; address fan-out is a poisoning error |
| `send_outcomes` shape (verification_id, email_hash, domain, mx_provider, hash_scheme, sent_at, event, raw) + `verification_outcomes` join, **created empty** | P1 §4.7 | §3.2 — P7 fills it; adds no structural field |
| `mx_provider` **promoted to typed column** "the permanent grouping dimension of the Phase-7 flywheel" | P1 §3.2.2, M3 | §5.2 — the calibration bucket dimension |
| `reason=historical_bounce` (invalid, P7) **reserved**; gate `historical_bounce:pass → invalid:historical_bounce:stop` reserved | P1 §3.4.3, §7.2, §7.9/703 | §4.6 — the one new gate, reserved reason, zero new vocab |
| `from_cache=true` calibration-excluded; whitelist/blacklist override-derived calibration-excluded | P1 §3.2.1, OQ-19 | §4.4 — consumed exclusion disciplines |
| disposable kept `risky`, never `invalid`, "so bounce data stays clean" | P1 OQ-7 | §4.4 — `invalid` means exactly one thing in a numerator |
| `confidence` is a `DIAL`; the P1 table is "explicitly a placeholder for" P7 | P1 §7.4 | §5.1 — recalibration replaces the placeholder within the frozen vocab |
| `552`/`mailbox_full` = positive existence proof → valid | P2 §5.5, [[truemail-552-mailbox-full-valid]] | §4.2 — `mailbox_full`→`valid-signal`, the counterintuitive rule |
| catch-all → existence unprovable; "per-provider trust calibration = P7" | P2 §5.5, §7.4 | §4.5 delivered-on-catch-all ≈ zero; §7 per-provider trust |
| sample-size gate + hysteresis + cascade brake | P4 §5.3/§5.4/§5.5 | §6 — the loop-safety guards, borrowed wholesale |
| lease + CAS-on-holder = exactly-once settlement | P5, [[truemail-lease-cas-exactly-once-settlement]] | §5.5 — the calibration writer is a new lease-and-settle path |
| `external_verified` separable population, capped confidence, weaning query pre-written | P6 §3.4/§6.3/§8.4, [[truemail-rented-verdict-provenance-separable]] | §5.3 bucket separately; §9 run the query |

---

## Appendix B — Bounce-classification proof matrix

Each row: an arriving outcome → the calibration class it must map to → what would break if misclassified. This is the classifier's test-of-record (the P7 analogue of P2's SMTP truth table / P6's residue matrix).

| # | Arriving outcome | Joined verdict | Class (§4.2) | Calibration effect | If misclassified → |
|---|---|---|---|---|---|
| 1 | hard 550 "user unknown" | `valid/accepted × google` | **invalid-signal** | false-`valid` recorded; bucket confidence pressured down | (correct) — the gold signal |
| 2 | 552 "mailbox full" | `valid/accepted × o365` | **valid-signal** (strong) | confirms existence; bucket confidence supported | **counting as invalid** → GAP-49 poisoning: a real mailbox teaches "invalid" |
| 3 | 550 IP-blocklisted | `valid × yahoo` | **zero-information** | **excluded** | counting as invalid → a sender-reputation problem masquerades as mass address-invalidity (§6.5) |
| 4 | spam-folder (delivered, no bounce) | `valid × google` | **weak valid-signal** | small positive nudge | treating as *strong* valid → over-trusts a silent-drop-prone channel |
| 5 | delivered on catch-all domain | `risky/catch_all × other` | **near-zero-info** | negligible | treating as strong valid → the exact catch-all trap P2 warned of |
| 6 | greylist/deferred 4xx | any | **zero-information** | excluded | counting as invalid → transient condition mislabels a real address |
| 7 | 550 "user unknown", verdict `from_cache=true` | cached `valid` | invalid-signal but **excluded** (§4.4) | not calibrated | calibrating on it → measuring cache staleness as fresh-check accuracy |
| 8 | 550 "user unknown", verdict `external_verified` | vendor `valid × catch_all × MV` | invalid-signal, **separate vendor bucket** (§5.2) | calibrates the *vendor*, not own | merging into own bucket → GAP-54: train on vendor errors, can't wean |
| 9 | outcome, `verification_id` unknown | — | **ingest_error** (§3.4) | rejected, flagged | silently dropping → invisible feed breakage; silently guessing → poisoned join |
| 10 | outcome, `sent_at` < verdict `created_at` | temporal contradiction | **ingest_error** (§3.3) | rejected, flagged | attributing → bounce charged to a verdict that didn't exist at send |
| 11 | complaint (spam report) | `valid × google` | **zero-information** for validity | excluded from accuracy; logged sender-health | counting as invalid → conflates "don't want" with "doesn't exist" (the OQ-7 error, at outcome layer) |
| 12 | re-check of an address with a prior confirmed `mailbox_not_found` | new `/verify` | **historical-bounce gate → invalid/historical_bounce** (§4.6) | short-circuits to invalid within TTL | firing on a prior *zero-info* bounce → permanent mislabel of a real address |

---

## Appendix C — Adversarial architect-review pass

A deliberate attempt to break the spec before the owner does. Findings logged; none left silent.

### C.1 — PB1: "The classifier trusts the sender's event labels — garbage in, garbage out."
**Probe:** §4 classifies on the sender's `event`. If the sender mislabels a policy-reject as `mailbox_not_found`, P7 poisons a bucket faithfully.
**Resolution — holds, with a named dependency.** §3.2 normalizes the sender's *raw* event at ingestion rather than trusting a pre-classified label, and §4.2 maps on DSN-code/text semantics with the untouched `raw` retained (§3.2) so any mapping can be re-audited. But the spec is honest that **classification quality is bounded by DSN fidelity** — a truly mislabeled upstream DSN is the GAP-48 seam. This is the same shape as P6's GAP-47 (untestable-until-integrated): the mechanism is correct; its accuracy depends on a signal from a separate system. Flagged, not hidden.

### C.2 — PB2: "Recalibration + the historical-bounce gate is a hidden feedback amplifier."
**Probe:** the gate turns a past bounce into a future `invalid`, which suppresses a send, which prevents the outcome that could clear a stale record → a self-reinforcing permanent `invalid`.
**Resolution — caught, guarded by OQ-P7-2.** The gate is **TTL-windowed** and degrades to a *confidence penalty* (not a hard gate) after the window (§4.6), so a re-created mailbox gets re-sent-to and can clear. This is the "recovery is not a one-way door" instinct (P4 GAP-30) applied to the gate. Without the TTL, PB2 would be a real trap — hence it is a `DECISION REQUIRED`, not a silent default.

### C.3 — PM1: minor — "Delivered-no-bounce as valid-signal risks over-counting on high-deliverability providers."
Down-weighted to **weak** (§4.5) and never equal to a `mailbox_full`/discriminating-delivery proof; the `DIAL` weights let this be tuned toward zero if silent-drop rates prove high. Acceptable.

### C.4 — PM2: minor — "The calibration store is a second source of truth for confidence; could drift from the logged value."
By design (§5.4): the **log records what confidence was assigned** (append-only, Door 2); the **store holds the current model**. They are *meant* to differ over time — that difference is the audit trail of recalibration. Versioned/effective-dated so any historical verdict's basis is reconstructable. Not drift; provenance.

### C.5 — Door check `PASS`
- **Door 1:** confidence values change; field type/range unchanged; only additive `flags.historical_bounce`. ✔
- **Door 2:** `verifications` never mutated; recalibration writes a separate store; outcomes appended. ✔
- **Door 3:** join on existing `verifications.id` + `email_hash`; no new hashing. ✔
- **Door 4:** **zero** new `status`/`reason`/`recommendation`; only the reserved `historical_bounce`. ✔ (the load-bearing check)
- **Door 5:** entirely brain-side; gem untouched; box unaware; sender is a separate feed. ✔

### C.6 — Boundary check `PASS` (the cold-start honesty audit)
The section the owner flagged as the real risk. Verified: §5.6 makes cold-start *the code's* behavior (uncalibrated buckets use the P1 baseline), §7.4/§9.4 make promotion/weaning literally start at zero, §8.2 forbids a bare global accuracy number, §10 makes coverage an observable metric, and §14 refuses to conflate "the machine" with "the moat." **No sentence in the spec claims a data moat exists at ship time.** The overclaim GAP-52 warned of is structurally prevented, not merely disclaimed.

### C.7 — Residue/exclusion check `PASS`
Every calibration-exclusion the eight docs established is enforced at the calibration layer: `from_cache` (row 7), override verdicts (§4.4.2), `external_verified` separate-bucketing (row 8, §5.2), disposable-stays-risky (§4.4.4), and the `zero-information` majority (§4.3). Nothing that "wasn't a real fresh self-check" trains the model.

---

## Appendix D — Owner decision log

**D.1 — Verdict: APPROVED (v1.0), owner sign-off 2026-07-16.** Phase 7 is accepted as designed; the seven-phase arc is closed.

**D.2 — The two highest-rigor items affirmed.**
- **§4 bounce classifier — the correctness heart, endorsed.** The three-class map (invalid-signal / valid-signal / zero-information) with **§4.3's discipline that the exclusions are the point, not the leftovers** is called out as *exactly correct*. The load-bearing line explicitly affirmed: *the zero-information bucket is the majority of real-world bounces; counting even a fraction of it as invalid-signal biases every bucket toward "worse than reality," and because low confidence → fewer sends → fewer outcomes, a bucket gets **stuck looking bad**.* That stuck-bucket feedback trap is the subtle poisoning the whole flywheel exists to avoid. `mailbox_full → valid-signal` confirmed consistent with the P2 `552` ruling ([[truemail-552-mailbox-full-valid]] carried through correctly).
- **§6 feedback-loop safety — endorsed.** P4's guards borrowed correctly; the **asymmetric hysteresis (down-fast, up-slow — trust earned slowly, doubt applied quickly)** singled out as the right bias for a system whose failure mode is sending to dead addresses.

**D.3 — §4.6 historical-bounce gate reversibility affirmed.** A confirmed `mailbox_not_found` short-circuits future checks to `invalid`, but within a **TTL window degrading to a confidence penalty** after — so a re-created/reclaimed mailbox recovers. Same "recovery is not a one-way door" instinct as the P4 canary. Owner confirms: without the TTL, one bounce would permanently blacklist an address someone later reclaims. **OQ-P7-2 DECIDED: TTL-windowed hard gate (6–12 mo `DIAL`) on confirmed `mailbox_not_found` only, degrading to a penalty after.**

**D.4 — Cold-start honesty affirmed as structural, not disclaimed.** The one thing the owner was worried would slip, and didn't: §5.6 makes "empty on day one → P1 baseline" the *actual behavior*; §7.4 makes promotion start at zero; §8.2 forbids a global accuracy number without its coverage; §14.2 states it plainly ("the machine is the deliverable; the moat is years of running it"). **No sentence claims a moat exists at ship time.** GAP-52 structurally prevented.

**D.5 — §8.4 the quiet triumph.** The flywheel **does not** retroactively pressure `unknown → valid` to inflate the accuracy metric — it explicitly refused the easy erosion of first-class `unknown`. The founding discipline (never guess to avoid `unknown`) holds intact across all seven phases.

**D.6 — Remaining OQs.** OQ-P7-1 (transport: sender-owned append feed recommended), OQ-P7-3/4/6/7 (`DIAL` values), OQ-P7-5 (accuracy target — publish-with-coverage), OQ-P7-8 (the "your sends train our accuracy" disclosure — **surface it**, same disclose-don't-hide posture as P6) are accepted as owner/build-time calls; none block approval.

**D.7 — Closing assessment of the whole arc (owner).** Seven specs, each catching at least one production-damaging failure: P1 froze the five doors against *actual* truemail behavior (not the README's); P2 the accept-all-mock/untestable-valid-path realization + catch-all valid invariant; P3 the multi-homed box going green on the wrong IP (GAP-24) and audit-green ≠ can-send (GAP-15); P4 auto-heal-becoming-auto-outage (cascade brake) + block-quarantine-as-one-way-door (canary); P5 the SKIP-LOCKED-isn't-enough idempotency race (GAP-37, the sharpest catch — corrupts verdicts only under load); P6 anti-laundering keeping the rented population separable + spend-once inheriting GAP-37; P7 exclusion-is-the-discipline classifier + structural cold-start honesty. **Two process wins recorded:** (1) the GAP-37 memory did genuine cross-phase work — settle-once (P5) → spend-once (P6) → idempotent-ingestion (P7); saving architectural invariants as memory paid off exactly as intended. (2) Every cross-project seam surfaced rather than hid — the two-job boundary (P5), the vendor-privacy tension (P6), the "your sends train our accuracy" disclosure (P7); the planning exercise did its real job: finding the assumptions that would have bitten silently.

---

## 15. Definition of done + acceptance criteria

Phase 7 is **done** (as a design) when:
1. The join is verdict-level on `verifications.id`, anchored on P1 §4.7, never fanned out to addresses (§3.1). ✔ specified
2. The `send_outcome` ingestion **contract** is defined as a shape P7 consumes, not a sender P7 builds (§3.2, GAP-48). ✔
3. Ingestion is idempotent on `(message_id, event)` and rejects-and-flags integrity failures (§3.4, GAP-51). ✔
4. The bounce classifier maps every outcome to one of three calibration classes with `mailbox_full`→valid-signal and blocked/policy/transient→zero-information (§4, GAP-49). ✔ highest-rigor
5. Every inherited exclusion (`from_cache`, override, `external_verified`-separate, disposable-risky, `zero-information`) is enforced at calibration (§4.4, §5.3). ✔
6. Recalibration writes only `confidence`, only to a separate versioned store, never mutating the log, never minting vocab (§5, Door 2 + Door 4). ✔
7. Feedback-loop safety = sample-size gate + bounded step + hysteresis + cascade brake + self-confirmation break (§6, GAP-50). ✔ highest-rigor
8. The recalibration writer honors the CAS-on-lease-holder exactly-once discipline (§5.5, GAP-53). ✔
9. Provider auto-promotion (#4) is data-driven, sample-gated, and reversible with hysteresis (§7). ✔
10. Accuracy (#5) is measured per provider, published only with coverage, never as a bare global number (§8). ✔
11. Weaning runs the P6-defined query per `(residue_class × provider)`, requires own-bucket to be *measured*, and is reversible (§9). ✔
12. Every GAP (48–54) is logged, not silently resolved; every OQ has a recommendation (§12, §13). ✔
13. The done-vs-future boundary closes the arc without claiming a moat exists at ship time (§14, GAP-52). ✔

**Carry-forward:** none — this is the final phase. Open items are the four `DECISION REQUIRED` OQs (transport, historical-bounce TTL, accuracy target, platform disclosure) and the `DIAL` values, all owner calls at build time. The seven-phase arc is structurally complete: a system that produces honest verdicts (P1–P6) and now measures and calibrates them against reality (P7), inside five frozen doors, additive throughout.
