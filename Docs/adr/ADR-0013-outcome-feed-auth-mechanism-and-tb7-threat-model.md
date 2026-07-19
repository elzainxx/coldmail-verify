# ADR-0013 — The Phase-7 outcome-feed authentication MECHANISM + the TB-7 malicious-sender threat model

- **Status:** ACCEPTED (mechanism decided 2026-07-19, on the owner's "finish all of it" authorization for the Phase-7 build; recommendation-led, and structurally reversible — see §Reversibility)
- **Kind:** door-touching — resolves the **OQ-P7-1 *mechanism* sub-decision** that [ADR-0001](ADR-0001-outcome-feed-authentication.md) deliberately left open and [`DECISIONS.md`](../DECISIONS.md) §5 parked. It **discharges the pre-P7 hard gate** ([pre-takeoff-checklist](../pre-takeoff-checklist.md) §3.4): *"no P7 code until TB-7 authentication + threat model are designed."* Does **not** reopen any of the five frozen doors.
- **Deciders:** owner (P7-build authorization) — the P7-time choice ADR-0001 §Consequences reserved.
- **Extends / builds on:** [ADR-0001](ADR-0001-outcome-feed-authentication.md) (*that* the feed is authenticated + threat-modelled — the principle, already ACCEPTED 2026-07-17). ADR-0001 fixed the **what**; this ADR fixes the **how** (the mechanism) and produces the **threat model** ADR-0001 required.
- **Source finding:** cross-phase consistency report §0/§1 **F-15** (feed poisoning) · **DR-1** · **PN-2** · the P6→P7 [`phase-7-readiness-assessment.md`](../phase-7-readiness-assessment.md) §2.1 (which named this the single hard door blocking live P7).

---

## Context

ADR-0001 elevated the P7 outcome feed (**TB-7**) from a transport dial to a hard architectural gate: it **must** be an authenticated channel with the sender as an identified principal, and **must** be threat-modelled for a *malicious* sender — because P7 reuses P4's control-loop guards (sample-gate, hysteresis, cascade-brake), which defend against **noise**, not an **adversary**. ADR-0001 explicitly left two things to P7-time:

1. the **mechanism** — *signed webhooks* vs *mutually-authenticated pull*;
2. the **TB-7 malicious-sender threat model** itself (forged-outcome poisoning of A6/A8/A9/A10).

Phase 7 cannot ship a single line of ingestion code until both are done (the gate). This ADR does both. It is written **before** the P7 ingestion code and governs it.

**One reality that shapes the choice (GAP-48):** the sender is a *separate project* ("Feature 2") that **does not exist yet**. P7 defines the contract; the sender implements its half later. So the mechanism must (a) be specifiable unilaterally by P7 now, (b) minimize the crown-jewel brain's new attack surface (the brain holds the pepper in memory — PN-12), and (c) survive the sender not existing during local build (a simulated feed must stand in for it behind the *same* interface).

---

## Part A — The TB-7 malicious-sender threat model (the deliverable ADR-0001 required)

**Trust boundary TB-7:** the seam where an external, cross-project `send_outcome` enters the brain and drives *automated mutation* of confidence (A6), routing (A8), spend (A9), and a verdict-affecting gate (A10, historical-bounce). **The adversary is a principal who can write to the feed** — a compromised sender, a sender-project insider, or an attacker who obtained the sender's feed credential. P4's guards assume outcomes are *honest-but-noisy*; this model assumes they are *chosen by an adversary who knows the guards*.

### A.1 Assets an attacker wants to move

| Asset | Mechanism abused | Attacker's goal |
|---|---|---|
| A6 bucket `confidence` | forged `mailbox_not_found` at scale, gate-clearing volume | push a `(reason×provider)` bucket's confidence **down** → more residue → more paid fallback spend (financial DoS), OR push it **up** via forged clean deliveries → mass false-`valid` |
| A8 provider promotion | forged clean deliveries for a provider | promote a provider to `:mx`-only on poisoned "good" outcomes → the system **stops SMTP-probing** it → mass false-`valid` for that provider (the F-16 amplifier) |
| A9 fallback weaning | forged own-accuracy-beats-vendor signal | wean off the paid vendor on poisoned data → drop coverage on exactly the hard residue the vendor exists to answer |
| A10 historical-bounce gate | a single (or few) forged `mailbox_not_found` for a target address | **blacklist a real address** to `invalid/historical_bounce` for the TTL window (6–12 months) — a targeted denial-of-verification |
| ingestion integrity | forged `verification_id` / `sent_at` contradictions / replay | attribute a bounce to the wrong verdict, or double-count one outcome to move a bucket faster |

### A.2 Attack techniques a *statistical* guard does NOT stop (why authentication is mandatory)

1. **Gate-clearing patient poisoning.** An attacker who knows `min_bucket_samples` (~500) and `max_step` (~10–15) simply supplies >500 consistent forged outcomes moving a bucket one bounded step per cycle. Every P4 guard *passes* — the data is high-volume, low-variance, within-step. Statistics cannot tell adversarial-consistent from honest-consistent. **Only authenticating the writer closes this.**
2. **Staggered systemic drift** (the P4 lesson, twice-defective). An attacker moves many buckets a little, spread across cycles, so no single cycle looks systemic → the cascade brake's *single-cycle snapshot* misses it. → the brake here is redesigned to watch a **rolling multi-cycle window**, not one snapshot (§6.4 of the spec, enforced by test).
3. **Targeted single-address blacklist** via A10. A `min_bucket_samples` gate does nothing for a *per-address* gate. → **F-17 / PN-11: require ≥N *independent* invalid-signal outcomes** before A10 fires (defense-in-depth *behind* authentication, not instead of it).
4. **Replay.** Re-delivering one real bounce N times to amplify it. → idempotency on `(message_id, event)` (§3.4) + a transport-level watermark/nonce (below).
5. **Integrity contradiction.** A `verification_id` that doesn't exist, or `sent_at` before the verdict's `created_at`. → reject-and-flag at ingestion (§3.4/GAP-51), never calibrated.

### A.3 The layered defense (defense-in-depth — no single layer is trusted alone)

| Layer | Defends | Kind |
|---|---|---|
| **1. Authenticated channel (this ADR)** | forged writer (techniques 1, 3) — the root defense | **hard door** |
| **2. Ingestion integrity** (reject-and-flag: unknown `verification_id`, `sent_at`<`created_at`, malformed) | integrity contradiction (technique 5) | LOCKED |
| **3. Idempotency `(message_id, event)` + watermark** | replay (technique 4) | LOCKED |
| **4. Classifier exclusion discipline** (zero-information excluded) | sender-reputation noise masquerading as address-invalidity | LOCKED (§4.3) |
| **5. F-17 ≥N-independent gate on A10** | targeted single-address blacklist (technique 3) | DIAL (PN-11) |
| **6. Recommend-only (DR-2) on A6/A8/A9** | blast-radius reduction — a human sees the proposal before routing/confidence/spend change | LOCKED default |
| **7. Cascade brake over a rolling window** | staggered systemic drift (technique 2) | LOCKED (§6.4) |
| **8. Per-provider ingestion rate-limit + classifier-distribution monitor as a SECURITY signal** | flood; a sudden `invalid-signal` share shift = a poisoning alarm | LOCKED (ADR-0001 pt 4) |

**Recommend-only (layer 6) is a mitigation, not a substitute for layer 1** — ADR-0001 §Decision.5, restated. A human approving a *plausible, gate-clearing, authenticated-looking* proposal still approves poison; authentication is what makes "authenticated-looking" true.

---

## Part B — The mechanism decision

**DECIDED: mutually-authenticated *pull* (mTLS). P7 is the authenticated client; the sender exposes an authenticated, append-only, cursor-paginated outcome feed; P7 pulls it on the recalibration cadence.** Signed-webhooks is the documented, *swappable* alternative (§Reversibility), not the chosen default.

### B.1 Why pull over signed webhooks (for *this* system specifically)

| Factor | Mutually-authenticated pull (CHOSEN) | Signed webhooks (alternative) |
|---|---|---|
| **Brain attack surface (PN-12 — the brain holds the pepper)** | ✅ P7 opens **no** new inbound public endpoint; it is an outbound mTLS client. The crown-jewel host exposes nothing new to the internet. | ✗ P7 must expose a public ingestion endpoint — new inbound surface on the exact host that holds the pepper. |
| **Flood / DoS control (technique A.2 pt 8)** | ✅ P7 controls *when* and *how fast* it reads → backpressure is native; an attacker can't push faster than P7 pulls. | ✗ An attacker pushes at will; P7 must absorb + rate-limit inbound, and a push flood hits the brain directly. |
| **Batch-cadence fit (OQ-P7-7: daily/weekly rollup)** | ✅ Pull-then-recalibrate is one flow; the pull *is* the batch window. | ✗ Push is real-time/streaming; P7 must buffer-and-batch anyway, re-deriving what pull gives for free. |
| **Two-writers hazard (GAP-37-adjacent, spec §3.2)** | ✅ The sender stays the sole source of truth for its events; P7 is a pure consumer. No shared-table write contention. | ~ Also single-writer, but P7 becomes a write endpoint for an external principal. |
| **Mutual identification (ADR-0001: "sender as identified principal")** | ✅ mTLS identifies **both** ends — the sender to P7 *and* P7 to the sender (so a spoofed feed host is rejected by P7 too). | ~ HMAC/signature identifies the sender to P7, but P7 authenticating the source requires extra pinning. |
| **Replay** | ✅ Cursor/watermark is monotonic; re-reading is idempotent by construction, on top of `(message_id,event)`. | ~ Needs an explicit timestamp+nonce replay window in addition to idempotency. |
| **Cost to the (not-yet-built) sender** | ✗ Sender must build a durable, authenticated, cursor-paginated feed (more work). | ✅ Sender just POSTs signed payloads (less work). |

The one cost — more work for the sender — is acceptable because (a) the sender is being built fresh anyway (no legacy push to retrofit), and (b) it buys the biggest wins: **no new inbound surface on the pepper-holding brain**, **native backpressure**, and **clean batch-cadence alignment**. The spec's own OQ-P7-1 recommendation ("sender-owned append feed P7 ingests… avoids two writers on one table") already leaned here; this ADR commits it and adds the mutual-auth transport requirement ADR-0001 demands.

### B.2 The concrete contract

1. **Transport:** mutual TLS (mTLS). The sender presents a certificate P7 pins/validates against a configured trust anchor; P7 presents a client certificate the sender validates. **Both are identified principals.** No unauthenticated read is possible — an unauthenticated or bad-cert pull is refused at the transport, before any application code.
2. **Feed shape:** the sender exposes an **append-only, cursor-paginated** feed of `send_outcome` records in the P7 §3.2 LOCKED shape (`verification_id, message_id, event, sent_at, occurred_at, raw` + optional rollup dims). P7 tracks a durable **watermark** (last-consumed cursor) so a pull resumes without re-reading and a replay is a no-op.
3. **Cadence:** P7 pulls on the recalibration cadence (OQ-P7-7 `DIAL`, conservative — daily/weekly), not per-outcome. The pull window *is* the batch.
4. **Per-provider ingestion rate-limit** (ADR-0001 pt 4): P7 shapes how much it ingests per provider per window; the classifier-distribution monitor (§10) is a **security** signal — an anomalous `invalid-signal` share is a possible poisoning event, alerted, not silently absorbed.
5. **Fail-closed:** no trust anchor / no client credential / bad cert / unreachable feed → P7 ingests **nothing** (the flywheel stays cold-start on the P1 baseline) rather than ingesting unauthenticated data. This mirrors buildExternal's "any switch missing ⇒ off."

### B.3 Reversibility — the choice is an adapter, not a foundation

P7 ingestion is built behind an **`OutcomeSource` interface** (the Door-5 "interfaces at the seams" discipline, exactly as the router `Strategy`, node `Client`, and `external.Vendor` are swappable). The classifier, recalibration, guards, promotion, and weaning consume *classified outcomes*, never the transport. Therefore:

- **The mutually-authenticated-pull decision is a `PullSource` adapter.** Switching to signed webhooks later is a **new adapter** (`WebhookSource`) behind the same interface — **no change to the classifier or the calibration loop.**
- Local build uses a **`SimulatedSource`** adapter (an in-memory authenticated-feed stand-in) so the entire flywheel is buildable and fail-verifiable now, with the real transport wired when the sender exists. The simulated source is treated as *authenticated by construction* (it is in-process test data), and a test asserts an *unauthenticated/failed* source ingests nothing (the fail-closed property).

This is why deciding the mechanism now does **not** over-commit: the owner can veto pull for webhooks with a one-adapter change, and nothing downstream moves.

---

## Why this is door-touching but reopens no frozen door

- It **resolves** the OQ-P7-1 mechanism sub-decision (parked in DECISIONS §5) and **discharges** the pre-P7 gate — a one-way architectural commitment about P7's input trust model. That is why it earns an ADR (extending ADR-0001).
- It **touches none of the five frozen doors.** Door 1 (`/verify` shape), Door 2 (append-only LOG), Door 3 (hashing / no-plaintext), Door 4 (`status`/`reason`/`recommendation` vocab), Door 5 (never-fork / node-list) are all unchanged. TB-7 is a *trust boundary*, not one of the five doors; authenticating it is purely additive.
- The `OutcomeSource` interface + the ingestion path add **new brain-side code and new tables** (`send_outcomes` is filled — it was created empty in P1 §4.7 for exactly this; a new `calibration` store). **Zero** frozen `verifications` column is touched; **zero** new `status`/`reason`/`recommendation` is minted (the historical-bounce gate uses the P1-reserved `historical_bounce`). Door 4 intact.

---

## Consequences

- **Positive.** The pre-P7 hard gate is discharged with a real threat model and a concrete, defensible mechanism. The flywheel's most dangerous input gains an integrity boundary that closes the poisoning class (F-15/16/17) *before* any calibration trusts a byte. The brain opens **no** new inbound attack surface. The choice is reversible (adapter).
- **Cost.** Live P7 still cannot run until the **sender ("Feature 2") exists and implements its half** of the mTLS feed (a cross-project dependency, GAP-48). This is deliberate and was already true — this ADR does not remove that gate, it removes the *design* gate. Local build proceeds now against `SimulatedSource`.
- **Left as DIALs (owner/P7-time, not blockers):** the per-provider ingestion rate-limit values; the pull cadence (OQ-P7-7); the F-17 **N** value (PN-11); the mTLS trust-anchor + credential provisioning (an operational secret, mirrors node-token / vendor-key custody — a new secret class loaded like the others, never committed).
- **Recorded:** this ADR; the ADR index (ADR-0013); [`DECISIONS.md`](../DECISIONS.md) §5 (the parked mechanism line is now RESOLVED with a pointer here) + §6 register; [pre-takeoff-checklist](../pre-takeoff-checklist.md) §3.4 (the gate is ticked "designed", with the live-sender dependency still open).

## Compliance / how it's checked

- **Design gate (this ADR):** the mechanism is decided and TB-7 is threat-modelled (Part A). ✅ discharged.
- **Behavioral test (P7 build, against the simulated source):** an **unauthenticated / failed** `OutcomeSource` ingests **zero** outcomes (nothing reaches the classifier) — the fail-closed property. A **replayed** outcome (same `(message_id, event)`) is a no-op (no double-count). A forged-but-well-formed outcome from an authenticated principal is still subject to the ≥N gate (A10) and the rolling-window cascade brake. Regression = any unauthenticated outcome influencing a calibration bucket, or a replay double-counting.
- **Live gate (unchanged, still holds):** no *live* P7 ingestion until the sender implements the mTLS feed and the trust anchor + client credential are provisioned. Enforced fail-closed in wiring (no credential ⇒ no live source ⇒ cold-start on the P1 baseline) — the P7 analogue of PENDING-REAL-VENDOR, honestly **PENDING-REAL-SENDER**.
