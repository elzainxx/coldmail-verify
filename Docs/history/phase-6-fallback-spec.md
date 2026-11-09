# Phase 6 — Third-party fallback (rent the moat) — Design Specification

**Status:** APPROVED (v1.0) — owner sign-off recorded (Appendix D). OQ-P6-3 (asymmetric vendor-invalid) and OQ-P6-6 (disclose + opt-out) decided; the anti-laundering rule (§3.4) and spend-once (§5.4) affirmed as the two load-bearing correctness items.
**Phase:** 6 of 7 · *Coverage on hard residue*
**Trigger (build-plan):** you need to be *usable* on consumer/catch-all lists **now**, before you have your own bounce history — so you rent coverage on the residue you structurally can't answer yourself.
**Depends on:** P1 (contracts, policy engine, layer/gate model, reserved `external_verified`), P2 (SMTP verdicts + the `catch_all`/`accept_all` residue), P4 (the give-up `rate_limited` degrade), P5 (the retry-exhausted `unknown/greylisted` residue + the queue that produces it).
**Hands to:** P7 (the flywheel weans off the fallback per-provider — P6 builds the *provenance* that makes weaning measurable, not the weaning).

---

## 0. About this document

### 0.1 Purpose
Phase 6 adds **one new layer** to the existing policy/layer model: an **external verifier** (MillionVerifier or similar) that is called **only** for the residue every prior phase deliberately labeled-and-left — the addresses the system *structurally cannot* answer with its own SMTP evidence (catch-all domains, `:mx`-routed big providers, retry-exhausted greylisted, gave-up rate-limited). The fallback makes the system usable on real-world consumer lists immediately, at bounded cost, while the Phase-7 data moat grows underneath it.

This is a **planning deliverable — a written spec, not code.** It defines the fallback layer's contract, where it sits in the layer order, the residue-routing rule, the external-spend budget/circuit-breaker, the provenance-logging shape that P7 will calibrate against, and the data-egress/privacy posture (the first time real addresses leave the system to a third party). No Go, no Ruby, no SQL/DDL, no vendor SDK — **shapes, contracts, gates, and dials only.**

### 0.2 Scope discipline
- **In scope:** the external-verifier layer as a *policy layer* (§3); the residue-routing rule — *what* is escalated and *what never is* (§4); external-spend budget + circuit-breaker (§5, highest rigor); provenance logging so P7 can measure vendor accuracy (§6); privacy / data-egress posture (§7, highest rigor); vendor abstraction so a provider is swappable (§8).
- **Out of scope / `SEAM →` P7:** the flywheel, bounce-join calibration, per-provider **weaning** off the fallback. P6 designs the *provenance that enables* weaning; it does not build weaning logic. Marked `SEAM →` wherever P6 touches P7.
- **Out of scope entirely:** the platform's user-facing job UI, credits, CSV, list storage (a separate project — the boundary re-affirmed in §7.5); writing code; integrating a live vendor.
- **The one-way doors are frozen.** P6 is **additive-only**: it uses the **already-reserved** `reason=external_verified` (P1 §3.4.3), adds the external verifier as **one entry** in a policy's `order` + a `gates`/`flags` rule (the extension point P1 §7.2 explicitly reserved), and adds new `signals` keys + a provenance table that touch **no frozen schema**. It adds **zero** new `status` values and **exactly one** new `reason` value that was *already reserved a phase-ago*.

### 0.3 Tagging convention (identical to P1–P5)
- **`LOCKED`** — a decision frozen this phase; downstream phases treat it as ground truth.
- **`DIAL`** — a knob whose *existence* is locked but whose *value* is tunable (budget ceilings, confidence caps, batch sizes).
- **`DECISION REQUIRED`** — a genuine fork needing owner sign-off; enumerated as an OQ in §13.
- **`SEAM →`** — a deliberate boundary where P6 stops and a later phase (almost always P7) continues; designed-for, not built.
- **`GAP-n`** — a contradiction, omission, or trap found across the seven source docs or against truemail/vendor reality; logged in §11, never silently resolved.

### 0.4 Three principles this phase is built on
1. **The fallback is a layer, never a gate that fabricates certainty.** An external "valid" is *someone else's verdict about someone else's evidence*. It is recorded with **external provenance and its own (capped) confidence** — it is **never laundered into an SMTP-grade `valid`** that the system earned itself. This is not fussiness: P7 must be able to tell *your* verdicts from *rented* ones, or it will train the flywheel on the vendor's errors (§6, GAP-41).
2. **`unknown` stays first-class — the fallback is best-effort, not a guarantee.** If the vendor also returns unknown, is down, times out, or the external budget is exhausted, the answer **stays `unknown`** (the same residue verdict it already had). The fallback can only ever *improve* a residue verdict; it can never *degrade* `unknown` into a fabricated `invalid`, and its absence never makes the system worse than P5 left it.
3. **Only-residue routing is the entire cost model.** The fallback is called for the small, structurally-unanswerable tail — **never whole lists, never anything the cheap layers or the box already settled.** The routing rule (§4) *is* the budget's first line of defense; the circuit-breaker (§5) is the second.

---

## 1. Phase 6 at a glance

### 1.1 What Phase 6 delivers (three outcomes)
1. **An external-verifier layer** wired into the existing policy `order`/`gates` model — not a special case bolted on beside the router, but *another layer the policy engine evaluates*, subject to the same "which layers run, which gate, which flag" discipline as every cheap layer (§3).
2. **Residue-only routing** — the external layer runs **only** when a check has reached one of the four structurally-unanswerable residue states (§4.1) and nothing cheaper settled it. Whole lists, cache hits, clean `250` valids, and hard `invalid`s never reach the vendor. This keeps spend bounded by *the size of the tail*, not the size of the list (§4, §5).
3. **External-provenance logging** — every fallback answer is logged alongside the system's own verdicts, tagged `external_verified`, carrying *which vendor*, *their raw response*, *their claimed status*, and *a capped confidence* — rich enough that P7 can later measure the vendor's real accuracy per provider and wean off it (§6). `SEAM →` P7 for the weaning itself.

### 1.2 What Phase 6 consumes — the residue seams left by P1–P5
Every prior spec deliberately labeled-and-left exactly the residue P6 now routes. P6 **consumes these definitions; it does not redefine them.**

| Source seam | Residue left | Where P6 routes it |
|---|---|---|
| **P2 §5.5 / §7.2** | `risky/catch_all` + `risky/accept_all` — existence structurally unprovable via SMTP on a catch-all domain. "the residue Phase 6 (fallback) address" (P2 §5.5). | §4.1 residue class **(a)** catch-all |
| **P2 §5.5 / P1 §6.9** | `:mx`-routed big providers (Gmail/Yahoo/O365) → `risky/provider_no_smtp` — never reach SMTP by policy; their "250s" are pure risk. | §4.1 residue class **(b)** provider-no-SMTP |
| **P5 §4.5 / §4.7** | Retry-exhausted greylisted → settles `unknown/greylisted` at `max_attempts`. "the exact residue P6 routes to an external verifier" (P5 §13). | §4.1 residue class **(c)** retry-exhausted |
| **P4 §4.6 / P5 §4.6** | Gave-up `rate_limited` — every eligible node capped and (in P5) the retry budget bounded out by `max_retry_age`. | §4.1 residue class **(d)** gave-up-capacity |
| **P1 §3.4.3 / §7.2** | `reason=external_verified` **reserved**; policy `order`+`gates` explicitly reserved an external-layer extension point. | §3.2 the layer plugs into that exact point |

### 1.3 Frozen inheritance — the five one-way doors, and how P6 stays inside them
| Door | P6's additive move — nothing frozen is touched |
|---|---|
| **Door 1** — `/verify` response shape, versioned & additive-only | Adds new **`signals`** keys (`external.*` provenance bag) + populates the reserved `raw` sidecar with the vendor payload. **No** new top-level field. `meta.node_id`/`verifier_ip` are **`null`** on a fallback row (no box ran SMTP) — same fields, a provenance-honest value. |
| **Door 2** — LOG append-only | A fallback verdict is **one** append-only `verifications` row like any other, plus (reusing P5's pattern) at most one `work_item_attempts`-style external-call transcript. No mutation of prior rows. |
| **Door 3** — email hashing (`norm/v1` + `hmac-sha256/v1`, global pepper) | The LOG still stores only `email_hash`. **But the vendor call requires the plaintext address** — this is a new, bounded egress of plaintext to a third party (the P5 §3.3 plaintext-at-rest exception now *leaves the building*). Governed in §7. **GAP-40.** |
| **Door 4** — `status`/`reason`/`recommendation` enums additive-only | Uses the **already-reserved** `reason=external_verified` (P1 §3.4.3). Adds **zero** new `status`. The external status still resolves into the *existing* `valid`/`risky`/`unknown` vocabulary at a **capped** confidence (§3.4) — it never mints a new verdict grade. |
| **Door 5** — never fork the gem; node-list discipline | The vendor is an **HTTP API the brain calls**, entirely outside the gem and the box. The box never learns a fallback exists (it is a brain-side policy layer). Zero gem/box surface — the cleanest door-5 posture of any phase. |

### 1.4 Make-or-break for this phase
- **The fallback must not become a gate that fabricates certainty.** The single most dangerous failure is laundering a vendor's "valid" into an SMTP-grade `valid` — it would poison P7's calibration with the vendor's errors and destroy the moat you're renting to buy time for. Enforced by §3.4 (capped confidence, `external_verified` reason, external provenance) + GAP-41.
- **Cost discipline is the whole point.** Only-residue routing (§4) + an external-spend circuit-breaker (§5) are the reason P6 is *rent the moat* and not *bleed money*. A routing bug that escalates non-residue = the failure mode that matters most for spend.
- **Privacy is a genuine posture change.** P6 is the first time real addresses leave the system to a third party. It interacts with the platform's "admin can't even see list contents" promise (§7.5). This is a security/trust review item, not a footnote — treated at highest rigor (§7).

---

## 2. System context — where the fallback layer sits

### 2.1 The one change to the picture
Every prior phase kept all evidence-gathering **inside the system's own infrastructure**: cheap layers in the brain, SMTP on the boxes. Phase 6 adds **one outbound arrow to a third party** — and it fires **only** at the end of the layer chain, for the residue nothing internal could settle. The fallback sits *after* the box (and after the P5 queue's retry exhaustion), never before it: the system always spends its own (cheap, then SMTP, then retry) evidence first, and rents evidence only for what remains.

```
                 ┌───────────────────────────── BRAIN (control plane) ─────────────────────────────┐
   POST /verify  │                                                                                   │
  ──────────────▶│  policy engine — evaluates layers in `order`:                                     │
   (sync)        │                                                                                   │
                 │   syntax → mx → null_mx → disposable → spam_trap → provider_classify → role/typo   │
                 │        │           (cheap layers — P1, no port 25)                                 │
                 │        ▼                                                                            │
                 │   [gate?] ── hard invalid / risky-disposable ─▶ SETTLE (never reaches fallback)     │
                 │        │                                                                            │
                 │        ▼  (SMTP-eligible, not :mx-routed)                                           │
                 │   router ─────────▶ NODE (box) ──RCPT──▶ provider MX     (P2/P3/P4 — real SMTP)     │
                 │        │                                                                            │
                 │        ▼  (greylisted 450 → P5 queue → retry/backoff → maybe exhausts)              │
                 │   [box verdict]                                                                     │
                 │        │                                                                            │
                 │        ▼                                                                            │
                 │   ┌──────────────── residue classifier (§4.1) ────────────────┐                    │
                 │   │ is this verdict one of the 4 structurally-unanswerable     │                    │
                 │   │ residue classes? (catch_all / provider_no_smtp /           │                    │
                 │   │ retry-exhausted unknown / gave-up rate_limited)            │                    │
                 │   └───────────────┬───────────────────────────┬───────────────┘                    │
                 │              NO ──▶ SETTLE as-is          YES ──▶ external layer (§3)                │
                 │                                                     │                               │
                 │                                    [budget/breaker open? §5] ── no ─▶ SETTLE as-is   │
                 │                                                     │ yes                           │
                 │                                                     ▼                               │
                 │                                          fallback client (§8) ─────────────────────┼──▶  EXTERNAL
                 │                                                     │                               │     VERIFIER
                 │                                                     ▼                               │   (MillionVerifier
                 │                                     resolve → capped confidence,                     │     / swappable)
                 │                                     reason=external_verified, external.* provenance  │◀──  (HTTP API)
                 │                                                     │                               │
                 │                                                     ▼                               │
                 │                                              SETTLE + LOG (§6)                       │
                 └───────────────────────────────────────────────────────────────────────────────────┘
```

### 2.2 Sync and queued both reach the fallback through the *same* residue classifier `LOCKED`
The fallback is **not** a new entry path. Both the synchronous `/verify` handler and the P5 drainer produce a verdict, and **both** run it past the same §4.1 residue classifier before settling. This preserves the P5 master invariant (the queue changes *when* a check runs, never *whether* a safeguard applies): the fallback is one more layer in the *settle* path, reached identically whether the check came in sync or through the queue.
- **Sync residue** (e.g. a catch-all domain detected on a live `/verify`): the classifier fires inline, calls the fallback within the request deadline (or, if the vendor is slow, degrades to the residue verdict + `SEAM →` an optional async fallback-requeue, OQ-P6-7).
- **Queued residue** (e.g. retry-exhausted greylisted from the P5 drainer): the classifier fires at settlement; the fallback call is just another step before the exactly-once settle (P5 §5.3), inside the same lease/compare-and-set discipline — **the fallback call is an at-most-once side effect of an at-least-once attempt, so it must be idempotency-guarded (GAP-42).**

### 2.3 The box still cannot tell any of this happened `LOCKED` — Door 5 preserved
The fallback lives entirely brain-side. The box runs its RCPT probe and returns exactly as in P2/P3; it never learns that a verdict it couldn't settle was later escalated to a vendor. No gem surface, no box surface, no node-list change. The external verifier is not a "node" — it never enters the node registry, never gets a `verifier_ip`, is never health-watched or rate-limited by the P4 pool machinery (it has its *own* budget/breaker, §5).

---

## 3. The fallback layer

### 3.1 What it is — a policy layer, not a special case `LOCKED`
The external verifier is **the same kind of thing as every cheap layer**: an entry in a policy's `order`, with a `gates`/`flags` rule that says what its result does. P1 §7.2 reserved this exact extension point — *"P6 external fallback → the external verifier is just another entry in `order` + a `gates`/`flags` rule; `reason=external_verified` is already reserved."* P6 spends that reservation and nothing more.

- **Layer id:** `external` (a new layer id, additive to the `order` array — not a new grammar).
- **Position in `order`:** **last** — after the SMTP layer (and, for queued checks, after retry exhaustion). It is the cheapest-sufficient principle inverted for cost: it is the *most expensive* layer, so it runs *only* when everything cheaper has failed to settle. `LOCKED`: the external layer is never earlier than last.
- **It is conditional, not unconditional.** Unlike cheap layers (which run for nearly every address), the `external` layer's *entry condition* is the residue predicate (§4.1). The policy engine evaluates it only when the accumulated verdict is one of the four residue classes **and** the budget/breaker is open (§5). Otherwise the layer is skipped and the residue verdict settles as-is.

### 3.2 The gate rule — how the layer plugs into the P1 grammar `LOCKED`
Using the P1 §7.2 locked action grammar `"<status>:<reason>:<stop|continue>"`, the `cold-email` policy (and any policy that opts into fallback) gains:

```jsonc
{
  "order": [ "...cheap layers...", "smtp", "external" ],   // external is LAST
  "external": {                                            // NEW policy key (open-object, P1 OQ-25)
    "enabled": true,
    "route_when": ["catch_all", "provider_no_smtp",        // §4.1 residue classes — the entry predicate
                   "retry_exhausted_greylisted", "gave_up_rate_limited"],
    "vendor": "millionverifier",                           // abstraction handle, §8 — swappable
    "confidence_cap": 60,                                  // DIAL — external valid never scores above this
    "on_vendor_unknown": "keep_residue",                   // unknown stays unknown (principle 2)
    "on_budget_exhausted": "keep_residue",                 // breaker open → settle as-is (§5)
    "on_vendor_error": "keep_residue"                      // down/timeout → settle as-is
  },
  "gates": {
    "external:valid":   "risky:external_verified:stop",    // NOTE: risky, NOT valid — §3.4, never SMTP-grade
    "external:invalid": "invalid:external_verified:stop",  // a vendor "invalid" is accepted as invalid (see §3.4 caveat + OQ-P6-3)
    "external:unknown": "unknown:external_verified:stop",  // vendor couldn't answer either → honest unknown
    "external:risky":   "risky:external_verified:stop"
  }
}
```
- **`external` is a new open-object policy key** — tolerated by the evaluator exactly as P1 OQ-25 required (unknown keys tolerated, not a closed struct). Adding it breaks no existing policy: a policy without an `external` block simply never runs the layer.
- **The `route_when` list is the residue predicate expressed as data**, not code — so *which* residue classes a policy escalates is a `DIAL` per policy (a cheap-list policy might escalate all four; a cost-sensitive one might escalate only catch-all).

### 3.3 The layer's contract (conceptual — Layer/Signal interface, not code)
The `external` layer conforms to the same conceptual layer contract every P1 layer does — *given an address + accumulated context, return a result the policy engine maps through gates.* Its result carries:
- **`result`**: one of `valid | invalid | risky | unknown` — the vendor's answer **normalized** into the system's existing vocabulary (§8.2 normalization table; the vendor's own strings never leak into `status`).
- **`signals.external`** (new `signals` sub-object, additive under Door 1): `{ vendor, vendor_status_raw, vendor_confidence_raw, request_id, latency_ms, cost_units, called_at }` — the provenance bag (§6).
- **`raw`**: the untouched vendor JSON, nested in the reserved `raw` sidecar (P1 §3.2 `raw` field — `null` until now for cheap-layer stops; the fallback finally populates it for a non-SMTP layer).

### 3.4 The anti-laundering rule — an external "valid" is `risky`, never SMTP-grade `valid` `LOCKED`
This is the correctness heart of the phase, and the strongest `LOCKED` in it.

> **A vendor's `valid` maps to `status: risky`, `reason: external_verified`, at a confidence *capped* below any SMTP-earned `valid` (`confidence_cap`, `DIAL`, recommend 60).** The system **never** emits `status: valid` on the strength of a third party's say-so. `valid` remains reserved for the P2 `LOCKED` invariant — a clean `250` on a probe-confirmed non-catch-all mailbox — evidence *the system gathered itself*.

Rationale, and why it is non-negotiable:
1. **Provenance integrity for the flywheel (GAP-41).** P7 recalibrates confidence *per (reason × provider)* from real bounce outcomes. If a rented "valid" were indistinguishable from an SMTP-earned `valid`, P7 would train on a blend of *your* accuracy and *the vendor's* accuracy — and could never measure the vendor to wean off it. Tagging every fallback verdict `external_verified` with `signals.external.vendor` is what makes the vendor a *separately measurable* population. **This is the mechanism that makes weaning (P7) possible at all.**
2. **A vendor "valid" is a weaker claim than a `250`.** Most vendors themselves *guess* on catch-all and big-provider addresses (the exact residue we send them) — they often return "valid" or "catch-all/unknown/risky" with their own low confidence. Inheriting that as our top-grade `valid` would be laundering their uncertainty into our certainty. Capping it `risky` is *honest about what was actually proven*: nothing, by us.
3. **Consistency with harden-toward-`unknown` (P1 §7.5 / P2 §0.4).** The system's whole verdict philosophy is that confidence is *earned*, and absence of proof is not proof. A rented answer is not earned proof; `risky` (deliverable-but-unconfirmed) is the truthful home for "a third party thinks this is fine but we didn't verify it."

**The one asymmetry — a vendor `invalid` (`DECISION REQUIRED`, OQ-P6-3).** A vendor claiming *invalid* is a different risk than a vendor claiming valid: a false "invalid" wrongly discards a real address (a suppression error), a false "valid" wrongly sends (a bounce). The recommendation (§13) is to accept a vendor `invalid` as `status: invalid` **only** for residue classes where an independent invalid is plausible (retry-exhausted, gave-up-capacity — where the address may genuinely not exist), and to **downgrade a vendor `invalid` to `risky` on catch-all/`:mx` residue** (where *nobody*, including the vendor, can prove non-existence — a catch-all domain accepts everything, so a vendor "invalid" there is itself a guess). Flagged for owner sign-off, not silently resolved.

---

## 4. Residue-only routing — what is escalated, and what never is

### 4.1 The four residue classes — the entire universe of what may be escalated `LOCKED`
The fallback is called for exactly these four verdict states, each **defined by a prior phase** and consumed unchanged here (§1.2). Nothing else is ever eligible.

| # | Residue class | Verdict that triggers it | Defined by | Why it is structurally unanswerable by us |
|---|---|---|---|---|
| **(a)** | catch-all | `risky` / `catch_all` \| `accept_all` | P2 §5.5 | The domain accepts every RCPT; SMTP *cannot* prove a specific mailbox exists. |
| **(b)** | provider-no-SMTP | `risky` / `provider_no_smtp` | P1 §6.9, P2 §5.5 | Gmail/Yahoo/O365 are `:mx`-routed and never reach SMTP; their 250s are pure risk by policy. |
| **(c)** | retry-exhausted greylisted | `unknown` / `greylisted` (settled at `max_attempts`/`max_retry_age`) | P5 §4.5 | The pool retried through backoff and the provider never resolved to a clean answer. |
| **(d)** | gave-up capacity | `risky`/`plausible` (or `provider_no_smtp`) with `signals.smtp_skipped_reason` in {`rate_limited`} that a P5 retry could not clear within `max_retry_age` | P4 §4.6, P5 §4.6 | Every eligible node stayed capped past the retry budget; we never got to run SMTP at all. |

- **`LOCKED`: this table is the closed set.** A policy's `route_when` (§3.2) may escalate a *subset* of these four; it may **never** add a fifth. Adding a residue class is a new one-way-door consideration, flagged as such, never slipped into a policy.

### 4.2 What is NEVER escalated — the cost firewall `LOCKED`
The routing rule is the budget's first line of defense (§0.4 principle 3). The following **never** reach the vendor, no matter what:

| Never escalated | Why |
|---|---|
| **Hard `invalid`** (syntax, no-MX, null-MX, spam-trap) — settled by a cheap gate | Proven non-deliverable by us; renting a second opinion is pure waste and would *weaken* a clean invalid. |
| **SMTP-earned `valid`** (clean `250`, probe-confirmed non-catch-all) | We proved it ourselves; the vendor adds nothing and costs money. |
| **Cache hits** (`flags.from_cache=true`) | Already settled; re-escalating a cached residue verdict would multiply spend on re-uploaded lists. The escalation decision is made **once**, when the verdict is first *minted*, never on a cache read (GAP-43). |
| **`risky/disposable`** (settled by the disposable gate → `skip`) | The policy already refuses to send; deliverability is not the question, so a vendor check buys nothing. |
| **Anything a cheap layer stopped early** | By the cheapest-sufficient principle, if a cheap gate fired, no expensive layer runs — least of all the most expensive one. |
| **Whole lists / bulk uploads wholesale** | The fallback is applied per-residue-address inside the normal per-check path (sync or P5 queue), never as a batch "send the whole file to the vendor." Bulk still runs the full internal pipeline first; only each file's *residue tail* is eligible (§4.4). |

### 4.3 The routing decision is made once, at verdict-mint time `LOCKED`
The residue classifier (§2.2) runs at the moment a verdict is *first settled* — not on every read, not on cache hits. This is the idempotency anchor for cost: **an address is escalated at most once per (email_hash × policy × config) identity** — the same identity the cache and the P5 idempotency key use (P5 §5.1). If the same address is verified again and hits cache, the cached `external_verified` verdict is returned with **no second vendor call and no second charge** (GAP-43). A genuine re-verification (cache miss, e.g. config changed) may re-escalate — but that is a new identity, correctly a new (bounded) spend.

### 4.4 Bulk interaction — residue tail only, never the file `LOCKED`
For a P5 bulk job, the fallback changes nothing about how the job runs: every address flows through cheap layers → SMTP → (greylist retry). Only the **residue tail** — the fraction that settles into one of the four §4.1 classes — is eligible for escalation, address by address, through the same classifier. On a typical cold-email list this tail is a *minority* (most addresses settle `valid`/`invalid`/`risky-disposable` internally), which is precisely why only-residue routing keeps spend bounded. **Observability (§9) must report the escalation *fraction* per job** so a runaway tail (a list that is 90% catch-all) is visible before it is expensive — and the circuit-breaker (§5) is the backstop when it is.

### 4.5 `SEAM →` P7 — the residue is where the moat gets built
Every address escalated to the fallback is, by definition, an address the system *couldn't* answer itself — which is exactly the population P7's flywheel most needs to learn. P6 logs each escalation richly (§6) so that when P7's own send→bounce data arrives, it can compare *the vendor's answer* against *the real outcome*, per provider, and begin weaning (dropping the fallback where its own history now beats the rented answer). **P6 builds this provenance; it does not build the weaning — `SEAM →` P7.**

---

## 5. External-spend budget + circuit-breaker (HIGHEST RIGOR)

### 5.1 Why this is a *different, legitimate* budget than anything before
The v1 build dropped the internal cost-control component — internal SMTP checks cost the system nothing per-check (own boxes, own bandwidth). **The fallback is the first component that spends real external money per call.** A vendor charges per verification; a routing bug, a pathological all-catch-all list, or a retry storm could run up a bill silently. This is not the P4 rate limiter (which protects *IP reputation* by pacing SMTP) — it is a **spend ceiling** protecting *money*. Both are backpressure mechanisms; they guard different resources and must not be conflated (GAP-44).

- `LOCKED`: the external-spend budget is a **distinct** control from the P4 per-provider rate limiter. The P4 limiter never governs vendor calls; the vendor breaker never governs SMTP.

### 5.2 The budget shape — a spend ceiling with windows `DIAL`
A durable, brain-side spend counter (same transactional store as the queue — Supabase, single source of truth, per the P5 GAP-34 discipline), governing:
- **`daily_cap` / `monthly_cap`** (`DIAL`) — hard ceilings on vendor calls (or cost-units) per rolling window. Recommend starting conservative and raising from observed residue-tail volume (§9), never the reverse.
- **`per_job_cap`** (`DIAL`) — a ceiling on escalations from a single bulk job, so one pathological list (90% catch-all) cannot exhaust the global budget. When hit, the rest of that job's residue settles as-is (`keep_residue`), and the job completes honestly with an elevated `unknown`/`risky` count — never blocked, never failed.
- **`rate_ceiling`** (`DIAL`) — a max vendor-calls-per-minute, so a burst can't spike cost or trip the vendor's own rate limits.

### 5.3 The circuit-breaker — three trip conditions `LOCKED` (states) / `DIAL` (thresholds)
A breaker in front of the vendor client, with the standard three states, tripping on **any** of:
1. **Budget exhaustion** — a window cap (§5.2) reached → breaker **open** until the window rolls.
2. **Vendor error rate** — sustained failures/timeouts above a threshold (`DIAL`) → **open** for a cooldown, then **half-open** (probe with a single call) → **closed** on success. Protects against paying for (or waiting on) a degraded vendor.
3. **Vendor-down** — connection failures → **open** immediately for a cooldown.

- **`LOCKED` breaker-open behavior:** when the breaker is open, the `external` layer is **skipped** and the residue verdict **settles as-is** (`on_budget_exhausted`/`on_vendor_error` = `keep_residue`, §3.2). **The breaker never fabricates a verdict and never blocks a check** — a residue address with the breaker open settles exactly as P5 would have left it (`unknown`/`risky`), no worse. This is principle 2 (unknown stays first-class) made operational: *the fallback's absence never degrades the system below its internal honest answer.*
- **`LOCKED`: breaker state is per-vendor** (§8) — if a second vendor is ever configured (A/B or failover), each has its own breaker and budget. One vendor tripping does not trip another.

### 5.4 Idempotency of spend — do not double-charge (GAP-42) `LOCKED`
The fallback call is an **at-most-once side effect inside an at-least-once attempt** (§2.2). Under the P5 slow-drainer/reclaim scenario (the GAP-37 trap), a residue check could be attempted twice — and a naive design would call (and pay) the vendor twice. The invariant, inherited directly from the GAP-37 memory (*"the compare-and-set, not the lease, is the guarantee"*):

> **The vendor call, its cost-decrement, and the settlement must commit under the same conditional compare-and-set on the lease-holder** (`WHERE ... AND locked_by=<me> AND status='in_flight'`, P5 §5.3). A reclaimed/lost attempt that already called the vendor **discards its result and does not settle**; the winning attempt either reuses a recorded vendor `request_id` (idempotency key against the vendor, if the vendor supports it, §8.3) or accepts one duplicate read-only lookup as the bounded worst case.

- Unlike an RCPT probe (read-only, free — P2 §5.7), **a duplicate vendor call costs money.** So the spend-once guarantee is *stronger* here than P5's attempt-safety: we prefer the vendor's own idempotency key (`request_id` keyed on `email_hash ‖ vendor`) so a retried attempt returns the same billed result, not a new charge (OQ-P6-4). Where the vendor offers no idempotency key, the compare-and-set still guarantees **settle-once and cost-record-once**; at most one *extra* lookup may be billed under a genuine reclaim, logged and alertable (§9), and bounded by the breaker.
- **Cost is decremented once, in the settlement transaction** — never optimistically before the call (which would over-count on failure) and never in a later sweep (which could be lost on crash). Same discipline as the P5 §5.3 plaintext-purge: *the side effect and its accounting commit together or not at all.*

### 5.5 The breaker is a brake, not a cascade (inherited posture)
Consistent with the fleet-brake memory (don't auto-cascade on a fleet-wide signal): a tripped vendor breaker **alerts and degrades to `keep_residue`** — it does **not** quarantine the vendor permanently, disable the policy, or fail jobs. Recovery is automatic (window roll / half-open probe). A *sustained* trip is an operational signal ("residue volume exceeds budget — raise the cap or improve internal coverage"), surfaced to the operator, not a self-inflicted outage.

---

## 6. Provenance logging — the flywheel's raw material

### 6.1 Two things must be logged, and kept distinguishable `LOCKED`
1. **The verdict**, in the normal `verifications` LOG (Door 2, append-only), tagged `reason=external_verified` — so it is queryable alongside every other verdict and **provenance is visible in the verdict itself**.
2. **The external-call transcript**, in a dedicated append-only table (reusing the P5 `work_item_attempts` pattern — a sidecar to the LOG, not the LOG) — so P7 has the vendor's raw claim, confidence, and cost per call, without bloating the verdict row.

The `LOCKED` requirement across both: **a P7 query must be able to isolate the rented population** (`WHERE reason='external_verified'`) and, within it, group by vendor and by the vendor's own claimed status — because that is the join it needs to measure vendor accuracy against real bounces and decide weaning.

> **[CONSOLIDATION NOTE — 2026-07-17 · additive · retention/queryability, no contract change] `[PN-10, accepted]`**
> Cross-phase security review (F-14): the LOG audits *verdicts*, not *egress*. For a data-subject access/deletion request (SAR) the operator needs to answer "**which addresses were sent to which vendor, when**" — confirm the external-call transcript (this section's dedicated append-only sidecar) is **retained and queryable by `email_hash`** for SAR/deletion. Additive retention/index config on an already-specified table; changes no frozen contract. **Not built here.**

### 6.2 The `signals.external` provenance bag — what every fallback verdict carries `LOCKED`
Added under the P1 `signals` open object (additive, Door 1). Present iff the fallback layer ran:

| Key | Meaning | Why P7 needs it |
|---|---|---|
| `external.vendor` | which vendor answered (`millionverifier`, ...) | per-vendor accuracy; weaning is per-vendor **and** per-provider |
| `external.vendor_status_raw` | the vendor's own status string, verbatim | measure the vendor's *own grade* against outcome (their "valid" vs real bounce) |
| `external.vendor_confidence_raw` | the vendor's own confidence/score, if any | calibrate: is the vendor's high-confidence better than its low? |
| `external.residue_class` | which of the four §4.1 classes triggered escalation | weaning is *per residue class* — you may beat the vendor on catch-all before provider-no-SMTP |
| `external.mx_provider` | the classified provider of the address (from P1 promoted column) | *the* grouping dimension of the flywheel ("recalibrate per (reason × provider)") |
| `external.request_id` | vendor call id (idempotency + audit) | trace a verdict to a specific billed call (§5.4) |
| `external.cost_units` | what the call cost | spend accounting + cost-per-provider economics for the wean decision |
| `external.latency_ms` | vendor round-trip | operational; also the sync-deadline pressure signal (OQ-P6-7) |
| `external.called_at` | timestamp of the call | provenance ordering; passed in (no wall-clock in-spec) |

- The **untouched vendor JSON** goes in the reserved `raw` sidecar (P1 §3.2). `signals.external` is the *parsed, queryable* projection; `raw` is the *fidelity* copy.

### 6.3 The provenance is what makes weaning *measurable* — `SEAM →` P7 `LOCKED`
This is the load-bearing reason the anti-laundering rule (§3.4) and this logging exist. When P7's send→bounce loop closes, the weaning decision is a query the P6 provenance makes answerable:

> *"For (residue_class × mx_provider × vendor), what fraction of the vendor's `valid` claims actually bounced? Once my own SMTP/history-based verdict for that bucket has a lower bounce rate than the vendor's, drop the fallback for that bucket."*

- P6 **guarantees this query is possible** (the tags, the separable population, the per-provider dimension). P6 does **not** run it, set the threshold, or implement the drop — that is the P7 flywheel. `SEAM →` P7.
- **GAP-41 closed by this section:** because `external_verified` is a distinct reason with vendor provenance, P7 never trains on a blend of own+rented verdicts — it can always subtract the rented population, measure it separately, and calibrate each independently.

### 6.4 Provenance vs the plaintext-egress log are different concerns `LOCKED`
Provenance logging (this section) stores **no plaintext** — it stores `email_hash` (Door 3) plus the vendor's metadata. The fact that plaintext *was sent to the vendor* is a **data-egress** event governed by §7 (audit, retention, DPA), not by the verdict provenance. Keep them mentally separate: §6 is *what the vendor said* (kept, for the flywheel); §7 is *what we sent them and under what terms* (minimized, audited, and — like the P5 queue plaintext — never persisted longer than the call needs).

---

## 7. Privacy & data-egress posture (HIGHEST RIGOR)

### 7.1 The posture change, stated plainly `LOCKED`
Every phase P1–P5 kept personal data (email addresses) **inside the system's own infrastructure** — hashed at rest in the LOG (Door 3), plaintext only transiently in the P5 queue for work-not-yet-done (P5 §3.3), never leaving to a third party. **Phase 6 is the first deliberate egress of real, plaintext personal data to an external processor.** This is a genuine privacy/trust posture change and is treated as a security-review item, not an integration detail.

- `LOCKED`: sending an address to the vendor is a **processing-by-a-sub-processor** event. It requires the vendor to be a contracted data sub-processor (DPA in place, §7.4) before a single real address is sent. No DPA → no egress (the layer stays disabled).

### 7.2 What is sent, to whom, and the minimization rule `LOCKED`
- **What is sent:** the **plaintext email address only** — the minimum the vendor needs to verify. **Never** sent: the requesting user's identity, the `caller_ref`, the list/file it came from, the platform account, any other list member, or any internal id. The vendor receives an address, not a customer or a list. (GAP-45 — minimization is the mitigation for the platform-promise tension, §7.5.)
- **To whom:** exactly one contracted vendor per policy (`external.vendor`, §3.2), over TLS, authenticated with a vendor API key held brain-side in secrets (never on a box, never in the gem).
- **Only residue is sent (§4).** The minimization is structural, not just contractual: the routing rule guarantees the *vast majority* of addresses (all cleanly settled internally) **never leave the system at all**. Only the structurally-unanswerable tail is exposed — the smallest surface that still delivers coverage.

### 7.3 Plaintext lifecycle for the egress path `LOCKED`
- The plaintext used for the vendor call is the **same transient plaintext the P5 queue already holds** for the residue check (P5 §3.3) — the fallback does not introduce a *new* plaintext store. It reads the address, calls the vendor, and the P5 settlement transaction purges the plaintext exactly as it already does (P5 §5.3 step 4). **The fallback adds no plaintext-at-rest beyond the P5 exception, and inherits its transactional purge-on-settle.**
- **Nothing plaintext is logged in P6.** The transcript (§6) and the LOG (Door 3) are hash-only. The vendor's `request_id` traces the call without storing the address.
- `LOCKED`: **the address is never persisted by us for the purpose of the vendor call** — it is passed through from the already-held queue plaintext and purged on settle. A crash mid-call leaves no P6-specific plaintext residue (the P5 purge invariant covers it).

### 7.4 Vendor due-diligence checklist (operational gate, `DIAL` on specifics) `LOCKED`
Before enabling the layer against a live vendor, the operator confirms (a provisioning-checklist item, like the P3 box audit): DPA / sub-processor agreement signed; vendor's data-retention and deletion terms recorded (does the vendor retain the addresses we send? for how long? — this bounds *our* exposure); TLS enforced; API key in secrets with rotation; vendor's own security posture (SOC2 / GDPR processor terms) reviewed. **`LOCKED`: the layer ships disabled and is enabled only after this checklist passes** — the same discipline as `port25_open` gating `:smtp`-eligibility (a green audit ≠ ready; a configured vendor ≠ cleared to send).

> **[CONSOLIDATION NOTE — 2026-07-17 · additive · turns a one-time tick into a standing control] `[PN-7 / F-5, accepted]`**
> Cross-phase review (F-5): the checklist *records* vendor retention once, but the **cumulative set of addresses egressed grows unbounded** over time and is un-deletable by us without a process. Add to this checklist: a **cumulative-distinct-addresses-egressed metric** (standing exposure visibility) + a **periodic vendor-deletion process** (exercise the DPA's deletion right on a cadence, not just at offboarding). Additive to the operational gate; changes no frozen rule. Also gated in [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) §3.3. **Not built here.**

### 7.5 The platform-promise tension — surface it, don't bury it (`DECISION REQUIRED`, GAP-45) `LOCKED`
The platform (separate project) may promise users *"admin can't even see your list contents."* Phase 6 means the **brain quietly ships the hard residue of a user's list to MillionVerifier.** Even though (a) admin still can't see contents, and (b) only the residue tail leaves and only as bare addresses, **this is a real change to where a user's data goes, and users deserve to know.** The two projects' privacy stories touch here, and this is the one place they do.

- `LOCKED` (this spec's obligation): P6 **flags** this as a cross-project disclosure item. The brain's job is to (1) minimize egress (§7.2), (2) make egress auditable (which addresses were sent to which vendor, by `email_hash` — §6), and (3) **not silently resolve the platform's terms.**
- `DECISION REQUIRED` (OQ-P6-6, owner + platform): the platform's terms/privacy policy must disclose sub-processor use (a named third-party verifier) for the residue, OR the platform must offer users an opt-out (a policy without an `external` block — §3.2 — which cleanly disables fallback for that user's checks, at the cost of more `unknown`/`risky` residue). This is a business/legal/product decision the brain cannot make alone; P6 **surfaces it and makes both options mechanically supported** (fallback is per-policy, so opt-out is just a policy without the `external` block).
- **This is the boundary check for P6** (the analogue of P5's two-job boundary): P6 must **not** decide the platform's privacy terms — it must expose the seam honestly and give the platform the mechanical levers (per-policy enable, per-vendor audit) to honor whatever it decides. Confirmed in Appendix C.

> **[CONSOLIDATION NOTE — 2026-07-17 · additive · integration assertion, does NOT resolve OQ-P6-6] `[PN-6 / F-4, accepted]`**
> Cross-phase review (F-4): the opt-out is honored *in the brain* as "a policy without an `external` block," but the *decision* to opt a user out lives in the out-of-scope **platform** (GAP-45). If the platform's opt-out flag does not map **1:1** to a policy-without-`external`, a user could opt out on the platform yet still be routed through a fallback-enabled policy — a silent egress of exactly the data they declined. **Accepted additive assertion:** contractually require **platform opt-out ↔ policy-without-`external` maps 1:1, verified at integration** (an integration test, not a brain feature). This **annotates** the seam; it does **not** resolve **OQ-P6-6** (disclose + opt-out remains the owner's standing decision, unchanged; see [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) §2). **Not built here.**

---

## 8. Vendor abstraction — swappable, never hardwired

### 8.1 The fallback is an interface, not a vendor `LOCKED`
Like the layer contract (P1) and the node client (P2/P3), the fallback is a **conceptual interface** the brain calls, with a concrete vendor adapter behind it. `external.vendor` (§3.2) names which adapter runs. This is not premature abstraction — it is required because: (a) vendors differ in accuracy per provider, so A/B or failover is a realistic P7-adjacent need; (b) vendor terms/pricing change; (c) the weaning `SEAM →` P7 is *per-vendor*, so the system must be able to run more than one and compare.

- **Conceptual interface:** `Verify(ctx, plaintext_address) → {result, vendor_status_raw, vendor_confidence_raw, request_id, cost_units, latency_ms}` — mirrors the P1 layer-result shape so the policy engine maps it through gates identically.
- `LOCKED`: **no vendor-specific field ever reaches `status`/`reason`.** Vendor strings live only in `signals.external.vendor_status_raw` and `raw`; they are normalized to the system vocabulary at the adapter boundary (§8.2).

### 8.2 Normalization table — vendor vocabulary → system vocabulary `LOCKED` (shape) / `DIAL` (mappings)
Each adapter owns a small, explicit mapping from the vendor's status strings to `{valid, invalid, risky, unknown}`. Example shape (MillionVerifier-like — mappings are `DIAL`, confirmed per vendor):

| Vendor status (raw) | System result | Note |
|---|---|---|
| `ok` / `valid` | `valid` → resolved to **`risky`** by §3.4 anti-laundering (capped) | never SMTP-grade |
| `invalid` / `bad` | `invalid` (or `risky` on catch-all/`:mx` residue — OQ-P6-3) | the §3.4 asymmetry |
| `catch_all` / `accept_all` | `risky` | vendor also sees catch-all — honest risky |
| `unknown` / `error` / `timeout` | `unknown` | vendor couldn't answer → residue stays honest |
| `disposable` / `role` | `risky` (+ set corresponding P1 flag if not already) | consistent with P1 flag semantics |

- **`LOCKED`: an unrecognized vendor status maps to `unknown`, never to a guess.** A vendor adding a new status string must never silently become `valid` — unknown-tolerance in the *safe* direction (harden-toward-unknown, P1 §7.5).

### 8.3 Adapter responsibilities `LOCKED`
Each adapter: (1) holds its own API key (secrets, §7.4); (2) enforces the vendor's request/idempotency contract — passing an idempotency key (`email_hash ‖ vendor`) where the vendor supports it, so a P5-reclaim retry returns the same billed result (§5.4, OQ-P6-4); (3) owns its own breaker + budget state (§5.3 — per-vendor); (4) normalizes per §8.2; (5) reports `cost_units` for spend accounting (§5, §6). Adding a second vendor is a new adapter + a policy `external.vendor` change — **no change to the layer contract, the gates, or the LOG.**

### 8.4 `SEAM →` P7 — multi-vendor comparison and weaning
Running two vendors to compare accuracy, weighting them per provider, or dropping one per-bucket is the P7 flywheel's job. P6 makes it **possible** (interface + per-vendor provenance + per-vendor breaker) but builds **none** of it. `SEAM →` P7.

---

## 9. Observability

| Metric | Definition | Why it matters |
|---|---|---|
| **escalation rate** | fraction of settled verdicts routed to the fallback (overall + per residue class §4.1) | the core cost driver; a rising rate = internal coverage shrinking or a bad list mix |
| **escalation fraction per job** | residue-tail size / job size (§4.4) | catches a pathological list (90% catch-all) *before* it drains the budget |
| **external spend** | cost-units consumed vs `daily_cap`/`monthly_cap`/`per_job_cap` (§5.2) | the money view; the primary budget-health signal |
| **breaker state** | per-vendor open/half-open/closed + trip reason (§5.3) | is the fallback currently serving or degrading-to-residue |
| **vendor latency** | `external.latency_ms` percentiles | sync-deadline pressure (OQ-P6-7); vendor-health |
| **vendor error/timeout rate** | failed vendor calls / total | feeds breaker condition 2; vendor SLA tracking |
| **duplicate-call rate** | extra vendor lookups under P5 reclaim (§5.4) | should be ~0; a spike = idempotency/lease problem (GAP-42) |
| **residue-kept rate** | residue settled as-is because breaker open / budget exhausted / vendor unknown | how often the fallback *couldn't* help — the honest coverage gap; feeds the P7 build-case |
| **per-(residue_class × provider × vendor) volume** | escalation counts by bucket (§6.2) | the pre-flywheel view; the buckets P7 will later calibrate & wean |

**Alerts:** budget approaching cap (warn) / exhausted (page-if-sustained, per §5.5 — alert not cascade); breaker open sustained; escalation rate step-change; duplicate-call rate > ~0; a per-job escalation fraction over a threshold (a bad list). All **alert-and-degrade**, never auto-disable the policy (fleet-brake posture, §5.5).

---

## 10. Additive change ledger (what P6 adds, all backward-compatible)

| Surface | Addition | Door / rule it stays inside |
|---|---|---|
| `reason` enum | **uses** `external_verified` (already reserved P1 §3.4.3) — adds nothing | Door 4 — zero new values |
| `status` enum | none — external results resolve to existing `valid`/`invalid`/`risky`/`unknown` | Door 4 — zero new values |
| `signals` | new `external.*` sub-object (§6.2) + `external` provenance keys | Door 1 — additive `signals` keys |
| `raw` | now populated on a non-SMTP layer (vendor JSON) — was reserved/`null` | P1 §3.2 — reserved field, first use |
| policy `definition` | new open key `external` (§3.2) — tolerated per P1 OQ-25 | P1 §7.2 reserved extension point |
| policy `order`/`gates` | `external` layer id + its gate rules | P1 §7.2 locked grammar, additive |
| new tables | external-call transcript sidecar (P5 `work_item_attempts` pattern); external-spend counter | touch no frozen schema |
| new operational control | per-vendor budget + circuit-breaker (§5) | distinct from P4 rate limiter (GAP-44) |

---

## 11. GAPs — contradictions, omissions, traps found this phase

| GAP | Where | The contradiction / omission | Resolution in this spec |
|---|---|---|---|
| **GAP-40** | Door 3 vs the vendor call | The hash-everything discipline requires plaintext to leave the system for the vendor — the first egress of PII to a third party. | §7 — bounded, minimized (address-only, residue-only), DPA-gated, transient plaintext reused from the P5 queue and purged on settle. The one deliberate egress, governed not hidden. |
| **GAP-41** | flywheel provenance | If a rented verdict were indistinguishable from an SMTP-earned one, P7 would train on a blend and could never measure or wean the vendor. | §3.4 anti-laundering + §6 provenance — every fallback verdict is `external_verified` with per-vendor tags; the rented population is always separable. |
| **GAP-42** | P5 GAP-37 × vendor spend | A slow-drainer reclaim could run a residue attempt twice → **two paid vendor calls** + last-writer settle race. | §5.4 — vendor call + cost-decrement + settle commit under the same compare-and-set on the lease-holder; prefer vendor idempotency key; bounded worst case = one extra read-only lookup, alerted. |
| **GAP-43** | cache × escalation | Re-escalating a cached residue verdict on every read would multiply spend on re-uploaded lists. | §4.2/§4.3 — escalation decided **once at verdict-mint**, never on a cache hit; cache returns the stored `external_verified` verdict with no new call. |
| **GAP-44** | P4 rate limiter vs vendor budget | Two backpressure mechanisms guarding different resources (IP reputation vs money) risk conflation. | §5.1 — `LOCKED` distinct controls; the P4 limiter never governs vendor calls and the vendor breaker never governs SMTP. |
| **GAP-45** | platform "admin can't see contents" promise | The brain shipping a user's residue to a vendor is a real data-egress the platform's privacy story must account for. | §7.5 — surfaced as a cross-project `DECISION REQUIRED` (disclose sub-processor OR per-user opt-out); per-policy fallback makes both options mechanical; P6 does not decide platform terms. |
| **GAP-46** | vendor accuracy on the exact residue we send | The residue (catch-all, big-provider) is precisely where vendors are *weakest* — we may be renting low-quality answers. | §3.4 caps external `valid` to `risky`; §6 logs vendor confidence so P7 can measure whether the rent is even worth it per bucket, and wean fast where it isn't. Honest, not overclaimed. |
| **GAP-47** (test) | no live vendor in dev | Like P2's GAP-12/GAP-35 (no stateful greylist to test against), the vendor's real behavior can't be faithfully mocked pre-integration. | §14 — loop/routing/budget/breaker/idempotency mechanics tested against a **stubbed adapter**; true vendor-accuracy validation deferred to live integration (honestly acknowledged, not overclaimed). |
| *inherited* **GAP-37** | P5 lease/CAS | Applies directly to the paid vendor call (§5.4). | Carried forward as the governing invariant for spend-once. |
| *inherited* **GAP-13** | catch-all session spike | The catch-all residue P6 rents coverage for is the same residue the GAP-13 spike measures the internal cost of — informs *whether* to rent vs improve internal coverage. | Noted; the rent-vs-build tradeoff is a P7 economics `SEAM →`. |

---

## 12. Contradictions flagged across the seven source docs

1. **"Log fallback answers alongside your own (tagged external)" (build-plan §6) vs Door 3 hash-only LOG.** Not a true contradiction — the *verdict* logs fine as `external_verified`; the *plaintext egress* is the new surface, governed in §7. Flagged so the two aren't conflated. **Resolved, not silent.**
2. **Build-plan drops internal cost-control (v1) but P6 needs a spend control.** These are different budgets (§5.1, GAP-44) — dropping internal per-check accounting (own boxes, free) does not remove the need for external per-call accounting (real money). No contradiction once the two are named distinctly.
3. **P1 §7.2 shows `external` as an entry in `order` with a `gates` rule; the build-plan calls it "integrate a fallback API as a layer."** Consistent — this spec makes the layer literal (§3). Confirmed the P1 reservation and the build-plan wording agree.
4. **P2 §5.5 "the residue Phase 6 / Phase 7 address" pairs P6 and P7 on catch-all.** No conflict: P6 *rents* an answer on catch-all now; P7 *earns* one later from send history and weans off the rent. The residue is shared; the mechanism differs by phase. `SEAM →` P7 kept clean.

---

## 13. Open questions / decisions required

| # | Decision | Recommendation | § |
|---|---|---|---|
| **OQ-P6-1** | External `valid` → `risky` (capped), never SMTP-grade `valid`? | **Yes — `LOCKED` as recommended.** The anti-laundering rule is the phase's correctness heart; it is what makes P7 weaning possible and keeps confidence honest. Flagged as needing explicit affirmation (like P5's OQ-P5-1). | §3.4 |
| **OQ-P6-2** | Which residue classes does the shipped `cold-email` policy escalate? | **All four (§4.1) at launch**, so the system is maximally usable immediately; tighten via `route_when` per policy once §9 spend data shows which buckets are worth the rent. `DIAL`. | §3.2/§4.1 |
| **OQ-P6-3** | Accept a vendor `invalid` as `status: invalid`? | **DECIDED — asymmetric, `LOCKED`.** Accept as `invalid` only for retry-exhausted/gave-up-capacity residue (independent non-existence plausible); **downgrade to `risky`** on catch-all/`:mx` residue (a vendor "invalid" there is itself a guess — the domain accepts everything). Owner: *trust the vendor's "no" more than their "yes," and only where "no" is structurally possible* — the famous-provider "trust their no more than their yes" logic applied to the vendor. A false vendor-invalid discards a real address **irreversibly** (you never send, never learn); this is the one place a rented verdict does permanent harm, so it is bounded to where non-existence is provable. | §3.4 |
| **OQ-P6-4** | Use the vendor's idempotency key for spend-once? | **Yes where supported** (`email_hash ‖ vendor`), so a P5 reclaim returns the same billed result. Where unsupported, the compare-and-set still guarantees settle-once/cost-record-once; bound the extra lookup by the breaker and alert on it. | §5.4/§8.3 |
| **OQ-P6-5** | Budget ceiling values (`daily`/`monthly`/`per_job`/`rate_ceiling`)? | **Start conservative, raise from observed residue-tail volume** (§9) — never loosen blind. Set `per_job_cap` low enough that one pathological list can't drain the global budget. All `DIAL`. | §5.2 |
| **OQ-P6-6** | Platform disclosure: name the sub-processor OR offer per-user opt-out? | **DECIDED — disclose + opt-out (options 1+2 together), `LOCKED`.** The platform's terms disclose third-party-partner sub-processor use for residue it can't verify itself (honest, industry-norm), **and** offer a "never use third-party fallback" opt-out (per user/job) — mechanically free, since fallback is per-policy (a policy without the `external` block; opted-out users get more `unknown` residue, full privacy). Owner note: this is a *positioning* call — if "your list never leaves our infrastructure" becomes central to selling the platform, revisit toward opt-in-by-default and accept worse residue coverage. The brain supports all three postures mechanically; the platform chooses. **This is the first real cross-project contradiction — surfaced, not shipped silent.** | §7.5 |
| **OQ-P6-7** | Sync `/verify` on residue — call the vendor inline, or degrade + async-requeue? | **Inline within the request deadline if the vendor is fast; if it exceeds the deadline, degrade to the residue verdict and (opt-in) enqueue an async fallback-requeue** (P5 queue), mirroring OQ-P5-5's "don't auto-queue every sync transient." Default: inline with a tight timeout, `keep_residue` on timeout. | §2.2 |
| **OQ-P6-8** | One vendor at launch, or two for comparison? | **One at launch** (cost + simplicity); the interface (§8) makes a second a config+adapter change. Multi-vendor comparison/weighting is `SEAM →` P7. Don't build A/B before the flywheel needs it (same discipline as P5 OQ-P5-4). | §8.4 |
| **OQ-P6-9** | Cache the `external_verified` verdict like any other? | **Yes — it caches under the same (email_hash × policy × config) key**, so a re-uploaded list pays once (GAP-43). Note: it is a *settled* verdict, so it is cacheable (unlike transient degrades — P4 §4.6); confirm it is excluded from P7 calibration when `from_cache` (P1 calibration-exclusion rule already covers this). | §4.3 |

---

## 14. Phase seams — what P6 leaves for later, designed-for not built

| Later phase | Seam left in / honored by P6 | Not built now |
|---|---|---|
| **P7** weaning | Every fallback verdict is `external_verified` with per-(residue_class × provider × vendor) provenance + vendor confidence + cost (§6). The exact query that decides "drop the fallback for this bucket" is *answerable* — but not run. | The bounce-join, per-bucket calibration, the wean threshold, and the drop logic (§6.3, §8.4). |
| **P7** rent-vs-build economics | `external.cost_units` per bucket (§6.2) + the GAP-13 catch-all internal-cost signal — together they price "rent the answer vs improve internal coverage." | The economic decision + any auto-tuning of `route_when` from it. |
| **P7** multi-vendor | The vendor interface (§8) + per-vendor breaker/budget/provenance make A/B and per-provider vendor selection mechanical. | Running two vendors, comparing, weighting, per-provider selection. |
| **Platform** privacy terms | §7.5 surfaces the disclosure/opt-out decision and makes both mechanical (per-policy `external` block). | The platform's actual terms/opt-out UX (a platform-project decision). |
| **Sync async-fallback** | OQ-P6-7 leaves a clean opt-in seam to enqueue a slow sync-residue fallback into the P5 queue rather than block the request. | The async fallback-requeue path itself (default is inline+timeout). |

---

## 15. Definition of done + acceptance criteria

**Phase 6 done when:** a residue verdict (one of the four §4.1 classes) is escalated to a stubbed external verifier through the policy `order`/`gates` model, resolved into the existing vocabulary at capped confidence with `reason=external_verified` and full `external.*` provenance, logged append-only, spend-accounted once under the lease/compare-and-set, and bounded by a per-vendor budget + circuit-breaker — with non-residue verdicts provably never escalated and the plaintext egress governed by §7.

Acceptance criteria:
1. **A catch-all `risky/catch_all` verdict escalates** to the stubbed vendor and settles `risky/external_verified` with `signals.external.*` populated and vendor JSON in `raw`. (§3, §4.1a, §6)
2. **A vendor `valid` never produces `status: valid`** — it settles `risky` at ≤ `confidence_cap`. (§3.4 — the anti-laundering `LOCKED`, headline correctness demo)
3. **A hard `invalid`, an SMTP-earned `valid`, and a cache hit are provably never escalated** — no vendor call, no charge. (§4.2/§4.3 — the cost firewall)
4. **A P5-reclaimed residue attempt does not double-charge** — vendor call + cost + settle commit under the compare-and-set; a lost attempt discards its result. (§5.4, GAP-42, inherits GAP-37)
5. **Breaker open / budget exhausted / vendor unknown all `keep_residue`** — the check settles exactly as P5 left it, never worse, never fabricated. (§3.2, §5.3 — principle 2)
6. **`per_job_cap` bounds a pathological list** — a 90%-catch-all job stops escalating at the cap and completes honestly with elevated residue counts. (§5.2, §4.4)
7. **Provenance is P7-queryable** — the rented population is isolable (`reason='external_verified'`) and groupable by (residue_class × provider × vendor). (§6.3, GAP-41)
8. **Only the address is sent** — no user id, caller_ref, list, or other member leaves; egress is address-only, residue-only, DPA-gated, over TLS. (§7.2/§7.4)
9. **Plaintext egress leaves no new residue** — the address is the transient P5 queue plaintext, purged on settle; nothing plaintext is logged by P6. (§7.3)
10. **A policy without an `external` block runs unchanged** — fallback fully disabled, residue settles internally (the opt-out lever). (§3.2, §7.5)
11. **An unrecognized vendor status maps to `unknown`, never `valid`.** (§8.2 — harden-toward-unknown)
12. **The box is unaware throughout** — no gem/box/node-registry change; the vendor is never a node. (§2.3, Door 5)
13. **The platform-promise egress is surfaced, not buried** — §7.5 flags the disclosure/opt-out decision explicitly. (GAP-45, boundary check)

**Carry-forward to P7:** the `external_verified` provenance (per residue_class × provider × vendor, with vendor confidence + cost) is the pre-flywheel data the weaning decision runs on; the GAP-37 lease/compare-and-set invariant now governs paid side effects, not just settlement; the rent-vs-build economics await real bounce data.

---

## Appendix A — Upstream ground truth Phase 6 depends on

**A.1 The residue definitions (consumed, not redefined)**
- P2 §5.5 `LOCKED`: `valid` requires target-250 AND probe-explicitly-rejected; every other accepted path is `risky` — catch-all is `risky/catch_all`, existence structurally unprovable via SMTP. "the residue Phase 6 (fallback) address."
- P1 §6.9 / P2 §5.5: Gmail/Yahoo/O365 are `:mx`-routed, never reach SMTP → `risky/provider_no_smtp`.
- P5 §4.5: retry-exhausted greylisted settles `unknown/greylisted` at `max_attempts`/`max_retry_age`. P5 §13: "the exact residue P6 routes to an external verifier."
- P4 §4.6 / P5 §4.6: capped-out `rate_limited` degrades to cheap; a P5 retry that can't clear it within `max_retry_age` is gave-up capacity.

**A.2 The layer/policy model P6 extends**
- P1 §7.2 `LOCKED` action grammar `"<status>:<reason>:<stop|continue>"`; policy is table-driven JSONB; unknown keys tolerated (OQ-25). Explicitly reserved: "P6 external fallback → the external verifier is just another entry in `order` + a `gates`/`flags` rule; `reason=external_verified` is already reserved."
- P1 §3.4.3: `external_verified` reserved, status "(varies)", "Verdict came from the third-party fallback (tagged external)."
- P1 §3.2: `raw` field reserved, `null` until an engine/external source is consulted.

**A.3 The correctness invariants P6 inherits**
- Harden toward `unknown`, never `invalid` (P1 §7.5, P2 §0.4) — absence of proof is not proof.
- GAP-37 (P5): lease TTL > max op deadline + compare-and-set on lease-holder = exactly-once settlement; the CAS, not the lease, is the guarantee; one transactional store, no broker.
- P5 §3.3: the queue holds plaintext for work-not-yet-done only, purged transactionally on settle — the one bounded exception to Door 3, which P6's egress reuses.
- Fleet-brake posture: alert-and-degrade on a resource-pressure signal, never auto-cascade.

**A.4 Vendor reality (why the residue is the hard part)**
- The residue P6 rents coverage on (catch-all, big-provider) is exactly where third-party verifiers are *weakest* — they guess too. This is why external `valid` is capped to `risky` (§3.4) and why the vendor's own confidence is logged (§6) so P7 can measure whether the rent is worth it (GAP-46).

---

## Appendix B — Proof matrix (residue → routing → verdict)

| # | Input verdict (pre-fallback) | Residue class? | Fallback runs? | Vendor says | Settled verdict | Note |
|---|---|---|---|---|---|---|
| 1 | `invalid/invalid_syntax` (cheap gate) | no | **no** | — | `invalid/invalid_syntax` | cost firewall — never escalated (§4.2) |
| 2 | `valid/accepted` (clean 250, probe-rejected) | no | **no** | — | `valid/accepted` | SMTP-earned; vendor adds nothing (§4.2) |
| 3 | `risky/disposable` (skip) | no | **no** | — | `risky/disposable` | policy already skips (§4.2) |
| 4 | `risky/catch_all` | yes (a) | yes | valid | `risky/external_verified` (≤ cap) | **never `valid`** — anti-laundering (§3.4) |
| 5 | `risky/catch_all` | yes (a) | yes | invalid | `risky/external_verified` | catch-all: vendor "invalid" is a guess → downgrade (OQ-P6-3) |
| 6 | `risky/provider_no_smtp` (Gmail) | yes (b) | yes | valid | `risky/external_verified` (≤ cap) | big-provider residue rented (§4.1b) |
| 7 | `unknown/greylisted` (retry-exhausted) | yes (c) | yes | invalid | `invalid/external_verified` | independent invalid plausible here (OQ-P6-3) |
| 8 | `unknown/greylisted` (retry-exhausted) | yes (c) | yes | unknown | `unknown/greylisted` | `keep_residue` — unknown stays first-class (§3.2) |
| 9 | `risky/plausible` (gave-up rate_limited) | yes (d) | yes | valid | `risky/external_verified` (≤ cap) | capacity residue rented (§4.1d) |
| 10 | `risky/catch_all` | yes (a) | **no** (breaker open) | — | `risky/catch_all` | budget/breaker → settle as-is, no worse (§5.3) |
| 11 | `risky/catch_all` | yes (a) | **no** (`per_job_cap` hit) | — | `risky/catch_all` | pathological list bounded (§5.2) |
| 12 | `risky/catch_all` (cache hit) | yes but cached | **no** | — | `risky/external_verified` (from cache) | escalate once at mint, not on read (§4.3, GAP-43) |
| 13 | `risky/catch_all` | yes (a) | yes, vendor down | — | `risky/catch_all` | vendor error → `keep_residue` (§5.3) |
| 14 | `unknown/greylisted` | yes (c) | yes, unrecognized vendor status | `"weird_new"` | `unknown/external_verified` | unknown-tolerance safe direction (§8.2) |
| 15 | `risky/catch_all` (P5 reclaim, dup attempt) | yes (a) | yes (once) | valid | `risky/external_verified` | lost attempt discards; charged once (§5.4, GAP-42) |

---

## Appendix C — Adversarial architect-review pass (log)

Reviewed against all seven source docs and the frozen doors. Blockers (PB) had to be resolved before this draft; medium findings (PM) are noted with resolutions; the boundary check is explicit.

### C.1 PB1 (blocker, caught & resolved) — the apparent P5 invariant violation: retry-exhausted `unknown` → external `invalid`
**The trap.** P5 §4.5 `LOCKED`: a retry-exhausted check settles `unknown/greylisted`, **never `invalid`** — exhaustion is not proof of non-existence. But P6 row-7 lets a retry-exhausted residue escalate to the vendor and settle `invalid/external_verified`. On its face, P6 breaks a P5 one-way lock — the sharpest apparent contradiction in the phase.

**Why it is not actually a violation (resolution).** P5's invariant forbids the *retry mechanism* from fabricating `invalid` **out of exhaustion itself** — running out of retries is not evidence. P6 is not doing that. The `invalid` here comes from **new, independent evidence** (a separate vendor check), and it is tagged with a **different reason** (`external_verified`, not `greylisted`). The verdict is not "exhaustion ⇒ invalid"; it is "a new layer gathered new evidence ⇒ invalid, provenance external." The two invariants coexist precisely because P6 never reuses the `greylisted` reason to mean invalid. **Guardrail added:** OQ-P6-3's asymmetry — a vendor `invalid` is only accepted as `invalid` on residue classes where an *independent* invalid is plausible (retry-exhausted, gave-up-capacity), and downgraded to `risky` on catch-all/`:mx` (where non-existence is unprovable by anyone). So the one place P6 can turn an `unknown` into `invalid`, it does so only on new evidence, with new provenance, under an explicit owner decision — not by laundering exhaustion. **Resolved; the coexistence is now stated in §3.4 and OQ-P6-3.**

### C.2 PB2 (blocker, caught & resolved) — paid side effect under the GAP-37 reclaim
**The trap.** P5's GAP-37 established at-least-once *attempts*, exactly-once *settlement*, tolerable because an RCPT probe is free (read-only, no DATA). P6 puts a **paid** side effect (the vendor call) inside that same at-least-once attempt — so the GAP-37 tolerance ("a dup attempt costs one connection") no longer holds: a dup attempt now costs **money**. Naively wiring the vendor call into the drainer would double-charge under a slow-drainer reclaim.
**Resolution (§5.4).** The vendor call, its cost-decrement, and the settle all commit under the **same conditional compare-and-set on the lease-holder** — a lost attempt discards its (already-fetched) vendor result and does not settle or charge-record. Prefer the vendor's idempotency key (`email_hash ‖ vendor`) so even a genuinely duplicated *call* returns the same billed result. Worst case without a vendor key: one extra read-only lookup, bounded by the breaker and alerted (§9 duplicate-call rate ~0). This is the GAP-37 invariant correctly extended from "settle-once" to "spend-once" — the memory'd principle doing exactly the cross-phase work it was saved for.

### C.3 PM1 (medium, resolved) — cache ordering: never cache the intermediate residue verdict
**Finding.** If the pre-fallback `risky/catch_all` were written to cache/LOG before the fallback ran, a concurrent read could serve the intermediate verdict, and the escalation-once rule (§4.3) would be ambiguous.
**Resolution.** Escalation is defined to happen **at verdict-mint, before settle** (§4.3) — the fallback layer is part of producing the verdict, not a post-settle amendment. Only the **final** verdict (`external_verified`, or `keep_residue`) is ever written to cache/LOG. There is no window where the intermediate residue verdict is durable. Stated in §4.3 and acceptance criterion 12-adjacent (§4.3).

### C.4 PM2 (medium, resolved) — two budgets could be conflated
**Finding.** P4 has a per-provider rate limiter (backpressure for IP reputation); P6 adds a spend breaker (backpressure for money). Reusing P4 machinery would wrongly couple SMTP pacing to vendor spend.
**Resolution (§5.1, GAP-44).** `LOCKED` as **distinct** controls in different resource domains; neither governs the other. Named explicitly so no future refactor merges them.

### C.5 PM3 (medium, acknowledged) — vendor accuracy is untestable pre-integration
**Finding.** Like P2's GAP-12/GAP-35 (no stateful greylist to test against), the vendor's *real* accuracy on the residue can't be faithfully mocked. Over-claiming coverage quality would be dishonest.
**Resolution (GAP-47, §14).** Mechanics (routing, gates, budget, breaker, idempotency, normalization) are tested against a **stubbed adapter**; the vendor's real per-provider accuracy is validated only at live integration and *measured* by P7. The spec claims *coverage exists and is bounded/honest*, not *coverage is accurate* — the latter is P7's to prove. Not overclaimed.

### C.6 Boundary check — did P6 design the platform's privacy terms or job UI? **PASS**
Per the owner's explicit instruction (the P6 analogue of P5's two-job boundary): the spec was audited for scope creep into the platform project.
- §7.5 **surfaces** the platform-promise egress tension and **makes both resolutions mechanical** (per-policy `external` block for opt-out; per-vendor audit for disclosure) — but **does not write the platform's terms, opt-out UX, or consent flow.** It states plainly that the disclose-vs-opt-out choice is a legal/product decision the brain cannot make alone (OQ-P6-6).
- No user id, credits, CSV, filename, or list storage entered any P6 table or the vendor call (§7.2). The vendor receives an address, never a customer.
- **Verdict: clean.** P6 exposes the seam honestly and hands the platform the levers, exactly as instructed — it does not blur the line.

### C.7 Residue-definition check — did P6 redefine any inherited residue? **PASS**
The four §4.1 classes are quoted from P2/P4/P5 with citations; none is redefined. The `route_when` list can only escalate a *subset*; adding a fifth class is `LOCKED`-forbidden without a new door consideration.

### C.8 Door check — all five doors intact? **PASS**
Door 1 (additive `signals`/`raw`, no top-level field); Door 2 (append-only, sidecar transcript); Door 3 (hash-only LOG; the one bounded plaintext egress governed by §7, reusing the P5 exception — no *new* plaintext store); Door 4 (zero new `status`, reuses reserved `external_verified`); Door 5 (vendor is a brain-side HTTP call, never a node, never in the gem — the cleanest door-5 posture of any phase).

---

## Appendix D — Owner decision log

**Verdict: APPROVED (v1.0).** Both flagged correctness items resolve correctly; the two OQs needing real judgment (P6-3, P6-6) are decided below. The GAP-37 memory proved its worth by transferring cleanly to spend-once.

### D.1 The two load-bearing correctness items — affirmed
- **Anti-laundering (§3.4, OQ-P6-1) — correct and correctly reasoned.** A vendor "valid" → `risky/external_verified` at capped confidence, never SMTP-grade `valid`. The load-bearing insight: this keeps the rented population *separately measurable*, so P7 trains on your own bounces without the vendor's errors contaminating calibration. If a rented "valid" were indistinguishable from a 250-earned valid, P7 would train on a blend of your accuracy and the vendor's and **could never wean off.** This is the mechanism that makes weaning possible at all — affirmed.
- **Spend-once (§5.4) — the GAP-37 memory did exactly its job.** It extended the P5 compare-and-set invariant from "settle-once" to "spend-once": the paid vendor call commits under the same compare-and-set on the lease-holder, so a slow-drainer reclaim can't double-charge. *A queue that can re-attempt is a queue that can double-pay a vendor, and the same guard that prevents double-settlement prevents double-billing.* That the invariant transferred cleanly to a **new failure domain (money instead of state)** validates the "save the architectural invariant" instinct. Affirmed.
- **Residue-only routing (§4) — the cost firewall is tight.** Four residue classes = closed set, each defined by a prior phase and consumed unchanged; the never-escalated table is exhaustive; §4.3 nails cost-idempotency (escalate once at verdict-mint, never on a cache read → re-uploading a list doesn't re-bill, GAP-43). Affirmed.

### D.2 OQ-P6-3 — the vendor "invalid" asymmetry — DECIDED (asymmetric)
Approved the asymmetric handling, with eyes open on what it means. The stakes: a false vendor "valid" costs one bounce (recoverable — the sender takes one hit); a **false vendor "invalid" wrongly discards a real address forever** — a suppression error you may never detect, because you'll never send to it to find out. Resolution locked: accept a vendor `invalid` as `invalid` **only** where independent non-existence is plausible (retry-exhausted, gave-up-capacity); **downgrade to `risky` on catch-all/`:mx`** (a catch-all domain accepts everything, so a vendor "invalid" there is itself a guess, and letting a guess discard a real address is the one place a rented verdict does irreversible harm). **What's being approved: trust the vendor's "no" more than their "yes," and only where "no" is even structurally possible** — the same "trust their no more than their yes" logic from the P2 famous-provider table, now applied to the vendor.

### D.3 OQ-P6-6 — the platform-privacy tension — DECIDED (disclose + opt-out), owner's call
Genuinely the owner's decision, not the brain's — and the spec handled its half correctly: it **surfaced** the tension rather than silently resolving it, made both resolutions mechanical (fallback is per-policy → opt-out is a policy without the `external` block), and **refused to write the platform's terms.** Decision: **disclose + opt-out (options 1+2).** The platform's terms disclose third-party-partner use for unverifiable residue (honest, industry-norm, costs a line of marketing purity) **and** offer a "never use third-party fallback" toggle (mechanically free). Noted as a **positioning call**: if "your list never leaves our infrastructure" becomes central to the platform's pitch, revisit toward opt-in-by-default and accept worse residue coverage — the brain supports all three postures; the platform chooses. **This is the first real contradiction between the two projects, and the spec caught it rather than letting it ship silently — exactly what the planning exercise was for: the seams between the verification system and the platform are where unexamined assumptions hide.**

### D.4 The honest limitation — GAP-47, affirmed
Vendor accuracy on this exact residue is **untestable before integration** (same shape as P2's un-mockable greylist). The correct claim: coverage is **bounded and honest, not accurate** — you can prove you only spend on the tail and only rent on the residue, but you cannot prove the vendor is *right* until P7 measures it against real bounces. Directive carried forward: **don't let "we have fallback coverage now" harden into "we're accurate on Gmail now."** You're renting coverage; only P7 turns that into measured accuracy.

### D.5 Boundary check — clean (the P5 two-job analogue held again)
No user/credit/CSV/list entered any P6 table or the vendor call. The vendor receives an address, never a customer. The separation held.

### D.6 Batch-accepted OQs (recommendations sound, no separate read needed)
OQ-P6-2 (escalate all four classes at launch, tighten from data), OQ-P6-4 (vendor idempotency key where supported; CAS guarantees settle/cost-once regardless), OQ-P6-5 (conservative budget ceilings, raise from observed volume), OQ-P6-7 (inline with tight timeout, `keep_residue` on timeout; async-requeue an opt-in `SEAM →`), OQ-P6-8 (one vendor at launch; multi-vendor is `SEAM →` P7), OQ-P6-9 (cache `external_verified` like any settled verdict; excluded from P7 calibration when `from_cache`).

### D.7 Carry-forward to Phase 7 (the endgame)
- The `external_verified` provenance (per residue_class × provider × vendor, with vendor confidence + cost) is the **pre-flywheel data the weaning decision runs on** — P7 measures the vendor against real bounces and drops the rent per-bucket where its own history wins.
- The GAP-37 lease/compare-and-set invariant now governs **paid side effects**, not just settlement — it will govern P7's calibration writes too (the memory's stated purpose).
- The dormant join table (empty since P1) **finally activates** in P7 when the sender comes online and bounces flow back.
- The rent-vs-build economics (`external.cost_units` per bucket + the GAP-13 internal-cost signal) await real bounce data — a P7 decision.

