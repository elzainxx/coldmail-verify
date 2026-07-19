# The arc closes — seven phases, honestly

**Date:** 2026-07-19 · **Status:** the closing document for the seven-phase build. Phase 7 is committed (`268a10f`); all nine irreversible surfaces were re-fail-verified in one pass this closeout; the fresh-system invariant holds. This document states plainly what is done, what is pending real-world inputs, what is explicitly future, and the one truth the whole arc was built to keep honest.

> **The one-line close:** *P1–P6 built a system that produces honest verdicts; P7 added the loop that measures and calibrates them against reality — inside five frozen doors, additive throughout. The machine is finished and fail-verified. The moat is years of running it. The spec closed the arc by refusing to conflate the two, and this build honors that.*

---

## 1. What is genuinely done — the machine, fail-verified on real Postgres

A two-deployable email-verification system: a Go **control-plane** (the "brain" — all state, all decisions) and a pinned, forked Ruby **verifier-node** (the "box" — a dumb, stateless SMTP probe). Everything below is built, tested, and — for the load-bearing pieces — **fail-verified** (neuter the guard → the named test breaks → restore → green), on the in-memory fake AND real Postgres 16 with fresh migrations 0001–0012.

**The five frozen doors held for all seven phases** (`vocab.go` byte-identical since Phase 0 `d3e56d8`):
1. `/verify` response shape — versioned, additive-only.
2. The LOG — append-only, enforced by revoked grants (not just convention).
3. Email hashing — `norm/v1 + hmac-sha256/v1`, one frozen scheme; raw email never persisted (one bounded, encrypted, purged P5 exception).
4. `status`/`reason`/`recommendation` — additive-only vocab; P7 minted **zero** new values (reused the reserved `historical_bounce`).
5. Never fork the gem — pinned `3.3.1`, CI-asserted byte-identical; only the rack wrapper is forked.

**The nine irreversible surfaces — all re-fail-verified this closeout, in one pass:**

| Surface | Phase | The guarantee |
|---|---|---|
| Door-3 hashing (NFC/case-fold closure) | P1 | one address → one hash, forever; a scheme change de-joins all history |
| Door-2 append-only LOG | P1 | a verdict is never mutated; corrections are new rows |
| GAP-24 four-way egress-IP invariant | P3 | a multi-homed box can't pass audit-green on the wrong IP |
| P4 fleet cascade brake | P4 | a systemic block-spike alerts + holds, never auto-drains the pool — **and now catches STAGGERED drift + validates its own config range** (it failed three times across P4/P7; it is now defended in depth and every defense is independently proven) |
| GAP-37 CAS exactly-once settlement | P5 | at-least-once attempts, exactly-once settle — a reclaimed lease discards, never double-writes |
| P6 anti-laundering | P6 | a rented "valid" can **never** become SMTP-grade valid — capped, separable |
| P6 spend-once | P6 | the vendor charge commits inside the settle CAS; a reclaimed attempt records no charge |
| P7 classifier zero-information exclusion | P7 | a sender-fault bounce (blocked/greylisted) **never** counts against the address |
| P7 recalibration CAS | P7 | recalibrate-once — a slow cycle's writes are discarded, not double-applied |

**The invariant that carried across three phases:** settle-once (P5) → spend-once (P6) → recalibrate-once (P7). One CAS-on-lease-holder discipline, proven three times.

**The fresh-system invariant** (verified this closeout): on a clean install, every auto-mutator (A1/A6/A8/A9) sits at recommend-only, the external layer is disabled, both P7 live-path touches are off, the kill-switch is cheap-layers-only — **zero live mutation, behavior identical to end-of-P6.** No mechanism silently mutates state at mode-default.

---

## 2. What is pending real-world inputs (and what each needs)

The full detail — with exact verification commands — is in [`pending-ledger.md`](pending-ledger.md); the ordered enablement path is in [`go-live-runbook.md`](go-live-runbook.md). In brief, three real-world things gate live value, and **none was ever faked green:**

- **PENDING-REAL-BOX (P3):** a host that genuinely allows outbound :25 with a settable PTR on a clean IP (V1–V5). The pool/queue/audit logic is built + tested; only real-IP reputation behavior and the proof values are deferred. The greylist reject-then-accept round-trip (GAP-35) is un-mockable — rollout observation only.
- **PENDING-REAL-VENDOR (P6):** a selected vendor + a signed DPA. The layer is built DISABLED behind a three-switch AND-gate (fail-closed); vendor *accuracy* on the residue is un-mockable until the P7 feed measures it.
- **PENDING-REAL-SENDER (P7):** the sender ("Feature 2", GAP-48) does not exist yet. The flywheel is built + fail-verified against a simulated feed; the mTLS mechanism is decided (ADR-0013) and the design gate discharged. Real accuracy, live authentication, and any mutator firing all need the real sender + volume.

**Owner gates that are not code** (consolidated in the runbook §4): host procurement, the DPA, the DR-3 admin-audit log (the highest-value parallel build — it also unblocks the one known wiring gap, the `port25_open` flip), the OQ-P6-6 and OQ-P7-8 disclosure decisions, and the **LICENSE** — deferred since Phase 0 (ruled 2026-07-17), to be resolved *"before any external distribution"* (DECISIONS §6.1; phase-0-signoff-sheet §C). Because the repo contains the `truemail-rack` fork, the conservative operational reading is: **resolve it before the fork is pushed anywhere non-local** — the first push is the practical trigger for the "external distribution" the ruling names.

---

## 3. What is explicitly Phase-8 / future (named, not built)

These are deliberately out of the seven-phase arc — named so the scope stays honest, never half-built:
- **Multi-vendor tournament** — P6 built the *possibility* (one vendor behind a swappable interface); P7 weans off *a* vendor per bucket. A live multi-vendor A/B is future.
- **Multi-brain HA** — the system assumes a single brain (it holds the pepper, the sole service-role cred, the rate-limiter state, the drainer, the recalibration writer). Horizontal brain HA is future (GAP-25/OQ-P4-8).
- **Engine-swap** — replacing the `truemail` engine behind the Door-5 boundary without a fork; the boundary is drawn for it, the swap is future.
- **Session batching / same-session catch-all probe** — the GAP-13 decision took two sessions (Door 5 outranked the elegance); session reuse is a future optimization.
- **Tier-scoped policies** — one default policy ships; the policy object is open for per-tier policies, built when a tier model exists.
- **Automated control-loop tuning** — auto-adjusting `max_step`/`min_bucket_samples` from observed convergence; P7 ships fixed, human-set dials + the cascade brake.
- **Cross-verdict / address-level reputation** — deliberately excluded to protect the P1 verdict-level join.
- **Sharing/monetizing the accuracy data** — a product/privacy decision far beyond the brain.

---

## 4. The cold-start truth, stated plainly

**The machine is the deliverable. The moat is years of running it.**

On day one the flywheel is **empty**: every calibration bucket is uncalibrated and falls back to the P1 baseline; **zero** providers are promoted; **zero** buckets are weaned; the system behaves *exactly* as it did at end-of-P6 until real volume accrues. This is not a caveat bolted on — it is how the code is written (`GetEffectiveCalibration` cold-start → baseline; promotion/weaning sample-gated to "hold"; accuracy unreportable without coverage). It only ever gets good on the providers your own sending volume covers enough to measure; a provider you rarely send to stays on the cautious path forever, correctly.

**Verified this closeout — no overclaim exists:** a scan of every doc and code path found the phrase "data moat" **never as a claim that a moat exists at ship time** — every occurrence is a phase goal/title ("Own data moat, real accuracy" — the *aim* of Phase 7), a future-tense reference ("grows underneath", "grow your own data moat"), or a security-scope mention (P3 blast-radius). Phase 7 §14 states the honest sentence directly (`phase-7-flywheel-spec.md:439`): *"you have the machine that builds one, which starts empty and compounds with every send"* — not *"you have a moat."* `phase-7-status.md` echoes the same discipline in its own words (the claim-discipline + cold-start lines). The overclaim GAP-52 warned of is structurally prevented (cold-start is the code's behavior, accuracy is unreportable without coverage), not merely disclaimed.

---

## 5. The arc, in one table

| Phase | Delivered | The catch it made |
|---|---|---|
| 1 | Contracts (5 doors), data model, cheap layers, policy engine | froze the doors against *actual* truemail behavior, not the README's |
| 2 | Full SMTP path vs mocks; catch-all probe | the accept-all-mock / untestable-valid-path realization; `552`→valid invariant |
| 3 | One real audited box; the `/audit` gate; selective go-live | the multi-homed box going green on the wrong IP (GAP-24); audit-green ≠ can-send (GAP-15) |
| 4 | Health-watcher, sticky routing, rate limiter, the pool | auto-heal becoming auto-outage (the cascade brake); block-quarantine-as-one-way-door (the canary) |
| 5 | Async queue, greylist retry, CAS idempotency, bulk jobs | the SKIP-LOCKED-isn't-enough idempotency race (GAP-37 — corrupts verdicts only under load) |
| 6 | Third-party fallback on the residue; anti-laundering; spend breaker | keeping the rented population separable + spend-once inheriting GAP-37 |
| 7 | The flywheel: classifier, recalibration, promotion, accuracy, weaning | exclusion-is-the-discipline classifier + structural cold-start honesty + the mTLS feed-auth gate |

Seven specs, each catching at least one production-damaging failure a single-phase view would have missed. The GAP-37 memory did genuine cross-phase work; every cross-project seam was surfaced rather than hidden (the two-job boundary, the vendor-privacy tension, the "your sends train our accuracy" disclosure).

---

**The arc is structurally complete:** a system that produces honest verdicts (P1–P6) and now measures and calibrates them against reality (P7), inside five frozen doors, additive throughout. What remains is not more building — it is the real-world inputs (a box, a vendor+DPA, a sender) and the owner decisions (LICENSE, disclosures, the admin-audit log) that turn the finished machine on. Those are enumerated, ordered, and honest. Nothing here overclaims.

*Companions: [`pending-ledger.md`](pending-ledger.md) · [`go-live-runbook.md`](go-live-runbook.md) · [`phase-7-status.md`](phase-7-status.md) · [`DECISIONS.md`](DECISIONS.md) · [`PROJECT_RULES.md`](../PROJECT_RULES.md).*
