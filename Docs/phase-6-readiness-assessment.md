# Phase 6 readiness assessment — the fallback is buildable locally; the DPA gates ENABLEMENT, not implementation

**Date:** 2026-07-18 · **By:** the P5→P6 readiness/review pass · **Verdict up front: Phase 6 is READY to build locally against a mock vendor with the layer DISABLED.** Phase 6 is one new brain-side policy layer (an external verifier called only on the four structurally-unanswerable residue classes), a per-vendor spend budget + circuit-breaker, and external-provenance logging — all Postgres + Go, testable against a **stubbed adapter** exactly as P1–P5 built locally. The one thing that genuinely gates going live — a signed vendor DPA (§7.4) — gates **enablement**, not the build: **the external layer ships DISABLED and stays disabled until the checklist passes; enabling it pre-DPA is unlawful sub-processing.**
**Authorities:** [`phase-6-fallback-spec.md`](phase-6-fallback-spec.md) (the build, APPROVED v1.0), [`phase-5-status.md`](phase-5-status.md), [`DECISIONS.md`](DECISIONS.md) (OQ-P6-3, OQ-P6-6, DR-*), [`config-manifest.md`](config-manifest.md) §6, [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) §3.3, [`kill-switch-ladder.md`](kill-switch-ladder.md), [ADR-0011](adr/ADR-0011-phase-5-queue-additive-surface.md).

---

## 0. Phase 5 is genuinely closed (this pass re-verified it)

| Check | Result |
|---|---|
| **GAP-37 CAS exactly-once** re-fail-verified on **real Postgres** | ✅ neuter the settle CAS guard (postgres.go:666) → the stale-lease attempt double-settles (`ErrLeaseLost`→`<nil>`) → `TestPGWorkItem_CASSettlementExactlyOnce` fails; restored → passes |
| **Lease-TTL > max-SMTP-deadline fail-fast** re-fail-verified | ✅ neuter the `NewDrainer` guard → a too-short lease is accepted → `TestNewDrainer_RejectsShortLease` fails; restored → passes |
| **GAP-38 authoritative completion** (not a counter) | ✅ the query is `NOT EXISTS(work_items WHERE job_id=? AND status NOT IN ('settled','dead'))` (postgres.go:824); neuter Mem to counter-trust → a drifted-low counter reports not-complete → `TestJobCompletion_AuthoritativeNotCounter` fails; restored → passes |
| **DR-4 containment** (independent security audit) | ✅ **0 findings.** Plaintext purge is atomic in the settlement CAS tx (no separate sweep — a crash can't strand plaintext); the lease-reclaim sweep deliberately does NOT touch `addr_ciphertext` (a reclaimed item still needs it); AES key is a distinct field/env, never derived from or stored with the pepper, redacted in `String()`; fail-closed holds (32-byte key required; bulk 503 without it); **no other plaintext-at-rest or plaintext-in-logs path** — the ADR-0008 redaction runs on the SHARED `compute()` path so the queue can't bypass it, and `work_item_attempts.signals` is left nil |
| **Platform boundary GAP-36** (independent architecture audit) | ✅ **0 findings.** `caller_id` is key-derived and never read from the body (explicitly discarded at the one confusable point — `_ = callerID`); `caller_ref` is a pure opaque echo, never interpreted; the two are never conflated; **no user/credit/CSV/filename/per-row-UI in any brain table** (full column inventory of migrations 0009/0010); the bulk endpoint returns counts+verdicts only |
| **Queue-dial drift closed** (the one authorized change) | ✅ backoff/attempt/age/lease/batch/idle dials promoted from `buildQueue` code-defaults to `config.Dials` (readable, env-overridable, PROJECT_RULES §6); defaults unchanged (no behavior change); the §7.5 submission-backpressure dials read **UNSET** (owner-OQ, never a guessed number); config-reference regenerated + fresh; a `Validate()` guard keeps the GAP-37 lease-margin > 0 |
| **Master invariant + additivity** | ✅ `ComputeForQueue` and sync `Verify` still share the byte-identical `compute()` gate path (untouched this pass); zero new vocab (`vocab.go` byte-identical since Phase 0 `d3e56d8`); gem + verifier-node byte-identical; no frozen door moved |
| **Full gate + fresh-migration proof** | ✅ 20 packages green on real Postgres 16; all 10 migrations (0001–0010) apply clean from scratch → gate green; go vet clean |

**Phase 5 is closed and honest.** The irreversible surface (GAP-37 exactly-once settlement) is re-fail-verified on real Postgres; the two security-sensitive audits (DR-4 plaintext containment, GAP-36 platform boundary) came back with zero findings.

---

## 1. What Phase 6 is (and why it's buildable locally)

Phase 6 adds **one policy layer** — `external` — that the policy engine runs **last**, and **only** on the four residue classes P1–P5 deliberately labeled-and-left (catch-all, `:mx`-provider-no-SMTP, retry-exhausted greylisted, gave-up rate-limited). It resolves the vendor's answer into the *existing* vocabulary at a **capped** confidence (`reason=external_verified`, already reserved P1), logs external provenance for P7, and bounds spend with a per-vendor budget + circuit-breaker. It is **additive-only** (ADR-0011-style): zero new `status`, exactly one `reason` reserved a phase ago, new `signals.external.*` keys, a provenance sidecar + spend counter touching no frozen schema. It adds **no box/gem surface** (the cleanest Door-5 posture of any phase — the vendor is a brain-side HTTP call, never a node).

| P6 component (spec §) | Why buildable locally | How proven without a live vendor |
|---|---|---|
| **The `external` layer in the policy `order`/`gates`** (§3) | the P1 §7.2 extension point reserved exactly this; it's a new `order` entry + gate rules | policy-engine tests with the layer wired + a stubbed adapter |
| **Residue-only routing** (§4) — the four classes, and what's NEVER escalated | pure classifier over the accumulated verdict; the residue definitions are inherited unchanged | table tests: each of the 4 residue classes escalates; hard-invalid / SMTP-valid / cache-hit / disposable NEVER do (the cost firewall) |
| **Anti-laundering** (§3.4) — vendor `valid` → `risky` at ≤ `confidence_cap`, never SMTP-grade | pure mapping | the headline correctness test: a vendor `valid` never yields `status:valid` |
| **Per-vendor spend budget + circuit-breaker** (§5, highest rigor) | a durable Supabase counter + a 3-state breaker — same transactional store as the queue | budget-exhaustion → breaker open → `keep_residue`; `per_job_cap` bounds a pathological list; all deterministic |
| **Spend-once under P5 reclaim** (§5.4, GAP-42) | the SAME GAP-37 compare-and-set on the lease-holder, now guarding a paid side effect | slow-drainer reclaim → the lost attempt discards its result and does NOT double-charge (fail-verify by removing the CAS, exactly as P5's GAP-37) |
| **External-provenance logging** (§6) | `signals.external.*` + a `work_item_attempts`-style transcript sidecar | a P7 query can isolate `reason='external_verified'` and group by (residue_class × provider × vendor) |
| **Vendor abstraction** (§8) — swappable adapter + normalization table | a conceptual interface with a stub adapter behind it | stub adapter + normalization tests; unrecognized vendor status → `unknown`, never a guess |

**The ONE genuinely un-mockable thing (honestly flagged, not a build blocker):** the vendor's **real accuracy on the exact residue we send** (catch-all, big-provider — where vendors are *weakest*, GAP-46/47). Like P2's greylist (GAP-12/35) and P5's reject-then-accept, this can't be faithfully mocked: the spec tests the **mechanics** (routing, gates, budget, breaker, idempotency, normalization) against a stubbed adapter and defers *accuracy* validation to live integration — where **P7** measures the vendor against real bounces. The claim P6 can honestly make is *"coverage exists and is bounded,"* not *"coverage is accurate."*

**The VPS is not a blocker** (same as P4/P5): P6 is brain-side; it needs a vendor HTTP endpoint (stubbed locally), not a box.

---

## 2. The go-live gates — ready / not-ready / needs-owner-action

Phase 6 is the **first deliberate egress of real, plaintext personal data to a third party** (§7). That makes the go-live gates categorically different from a build gate — several are **owner/legal actions that gate ENABLEMENT, not implementation.** Per the spec's `LOCKED` posture: **the external layer ships DISABLED and is enabled only after the §7.4 checklist passes; enabling pre-DPA is unlawful sub-processing.**

| Gate | State | What it is | Who / what unblocks it |
|---|---|---|---|
| **Vendor selection** | 🟡 **NEEDS-OWNER-ACTION** (does not block the build) | which adapter ships (`external.vendor`; OQ-P6-8 recommends **one** at launch — `millionverifier` — swappable) | **Owner** picks the launch vendor. The build proceeds against a stubbed adapter regardless; the concrete adapter is a config + adapter add (no layer-contract change). |
| **DPA + due-diligence (§7.4)** | 🔴 **NOT-READY — gates ENABLEMENT, not the build** | DPA / sub-processor agreement signed; vendor retention + deletion terms recorded; TLS enforced; API key in secrets with rotation; vendor SOC2/GDPR posture reviewed. **Enabling the layer pre-DPA = unlawful sub-processing.** | **Owner / legal.** This is a contract + operational checklist, NOT code. The layer is built DISABLED; the checklist flips it on. Recorded: pre-takeoff §3.3; PN-7/F-5 added a *cumulative-addresses-egressed* metric + a *periodic vendor-deletion* process (standing controls, not a one-time tick). |
| **OQ-P6-6 platform disclosure posture** (cross-project) | 🟡 **NEEDS-OWNER-ACTION — DECIDED, integration-gated** | **DECIDED: disclose + opt-out** (DECISIONS.md §6, P6 §7.5) — the platform's terms disclose third-party-partner use for residue, AND offer a per-user "never use third-party fallback" toggle. Mechanically FREE in the brain: fallback is per-policy, so opt-out = a policy without the `external` block. | **Owner + platform.** The brain's obligations (minimize egress to address-only/residue-only; make egress auditable by `email_hash`; supply the per-policy lever) are buildable now. PN-6/F-4 requires an **integration assertion**: platform-opt-out ↔ policy-without-`external` maps **1:1**, verified at integration (an integration test, not a brain feature) — else a user opts out on the platform yet is still routed through a fallback-enabled policy (silent egress of exactly the data they declined). |
| **OQ-P6-3 vendor-invalid asymmetry** | ✅ **READY — DECIDED, in DECISIONS.md** | Accept a vendor `invalid` as `invalid` **only** on retry-exhausted / gave-up-capacity residue (independent non-existence plausible); **downgrade to `risky`** on catch-all/`:mx` (a vendor "invalid" there is itself a guess). *Trust the vendor's "no" more than their "yes," and only where "no" is structurally possible.* | Confirmed recorded: **DECISIONS.md §6 OQ-P6-3** ("DECIDED (asymmetric)") + P6 §3.4/Appendix D.2 + config-manifest §6.2. No owner action outstanding; the build implements the ruling. |
| **Global-fallback-kill switch** | ✅ **READY — buildable now, primitive already exists** | the "no PII egress in one action" panic button (Global A; PN-16, rides DR-3 audit). | Buildable now. The config primitive **already exists** from Phase 0: `KillSwitch.Fallback = FallbackOff` (config.go:51, `TM_FALLBACK_MODE=off`). P6 wires the `external` layer to honor it (fallback-off ⇒ the layer never runs ⇒ zero egress) + the admin-audited action under DR-3. No new door; a kill-switch rung made real for the vendor. |

**The one-line summary of the gate posture:** everything needed to *build and test* Phase 6 is ready now (against a stubbed adapter, layer disabled). What is **not** ready — and correctly so — is *turning the vendor on*: that waits on a signed DPA (owner/legal), a picked vendor (owner), and the platform's disclosure/opt-out wiring (owner + platform, decided, integration-gated). **The DPA gates enablement, not implementation** — build the layer disabled, flip it on when the checklist passes.

---

## 3. The correctness carry-forward (GAP-42 = GAP-37 extended to money)

The load-bearing item P6 inherits is **GAP-42**: the vendor call is a **paid** side effect inside the P5 at-least-once attempt, so the GAP-37 tolerance ("a duplicate RCPT probe is free") no longer holds — **a duplicate attempt now costs money.** The spec's resolution is the GAP-37 invariant transferred to a new failure domain: **the vendor call + its cost-decrement + the settle all commit under the SAME conditional compare-and-set on the lease-holder** — a lost/reclaimed attempt discards its (already-fetched) vendor result and does not settle or charge-record. Prefer the vendor's idempotency key (`email_hash ‖ vendor`) so even a duplicated *call* returns the same billed result; worst case without one is one extra read-only lookup, bounded by the breaker and alerted.

This is exactly the [[truemail-lease-cas-exactly-once-settlement]] memory doing the cross-phase work it was saved for. **P6's spend-once is fail-verifiable locally the same way P5's settle-once was** (remove the CAS → a reclaim double-charges the stubbed vendor). It is the phase's second-highest-rigor item after the anti-laundering rule.

---

## 4. The SEAM → P6 items P1–P5 left — all inventoried, all consumed by P6

| Deferred by | The residue seam | P6 consumes it |
|---|---|---|
| **P2 §5.5** | `risky/catch_all` + `accept_all` — existence structurally unprovable on a catch-all domain | residue class (a) — escalate to the vendor |
| **P1 §6.9 / P2 §5.5** | `:mx`-routed big providers → `risky/provider_no_smtp` | residue class (b) |
| **P5 §4.5** | retry-exhausted greylisted → `unknown/greylisted` at `max_attempts`/`max_retry_age` | residue class (c) — "the exact residue P6 routes" (P5 §13) |
| **P4 §4.6 / P5 §4.6** | gave-up `rate_limited` (capped past the retry budget) | residue class (d) |
| **P1 §3.4.3 / §7.2** | `reason=external_verified` reserved; policy `order`+`gates` external-layer extension point reserved | §3.2 — the layer plugs into that exact point, spends the reservation and nothing more |
| **P5 §3.3 (DR-4)** | the transient queue plaintext, purged on settle | §7.3 — the vendor call reuses the SAME transient plaintext; adds NO new plaintext store, inherits the purge |

**The retry-exhausted residue P5 produces is the cleanest seam-closure:** P5 labels it `unknown/greylisted` and stops (§4.7 — "does NOT call a third party"); P6 is exactly the mechanism that routes that residue to the vendor. Nothing in P5 changes for P6 to consume it.

---

## 5. Rules honored in this pass

- **Review/readiness only — one authorized change.** The only code change is the queue-dial promotion (explicitly authorized: "close the queue-dial drift"). It is a no-behavior-change-at-default config lift; the GAP-37 lease-margin guard was added to `Validate()`; the owner-OQ backpressure dials read UNSET. Every other task was verification (fail-verify, audit, gate).
- **Nothing re-decided.** OQ-P6-3 and OQ-P6-6 are confirmed already-recorded in DECISIONS.md — this assessment reports their state, it does not re-decide them.
- **Additive-only; no door mutated** — re-confirmed on every axis (§0): vocab.go byte-identical since Phase 0, gem/verifier-node byte-identical, no frozen migration touched.
- **If a re-verification failed, say so and stop** — none failed. All three (GAP-37 CAS, lease-TTL, GAP-38) re-fail-verified; both security audits returned zero findings.
- **code-reviewer on the change** — the dial-promotion diff runs the code-reviewer gate (PROJECT_RULES §7).

---

## 6. Summary

| Question | Answer |
|---|---|
| Is GAP-37/38 re-fail-verified on real Postgres? | **Yes** — CAS neuter → double-settle fails; lease-TTL neuter → accepts short lease fails; GAP-38 uses the authoritative NOT EXISTS query. All restored green. |
| Is DR-4 containment re-proven? | **Yes** — 0 findings: purge is atomic in the settlement tx (no sweep), key is separate from the pepper, fail-closed holds, no other plaintext-at-rest/in-logs path. |
| Is the platform boundary clean? | **Yes** — 0 findings: caller_id (key-derived) strictly distinct from caller_ref (opaque echo); no user/credit/CSV/filename in any brain table. |
| Is the queue-dial drift closed? | **Yes** — promoted to config, defaults unchanged, UNSET dials read UNSET, config-reference fresh, GAP-37 margin guarded. |
| Is additivity confirmed + the gate green? | **Yes** — no frozen door moved; 20 packages green on real Postgres 16; fresh-migration (0001–0010) proof passes. |
| Is Phase 6 buildable locally? | **Yes — the whole phase**, against a stubbed vendor adapter with the layer DISABLED. Only vendor *accuracy* is un-mockable (GAP-47), deferred to live integration + P7 measurement. |
| What gates Phase 6 go-live? | **The DPA (§7.4) gates ENABLEMENT, not the build.** Owner actions: pick a vendor (OQ-P6-8), sign the DPA + due-diligence (§7.4), wire the platform disclosure/opt-out (OQ-P6-6, decided, integration-gated). OQ-P6-3 is decided + recorded; the global-fallback-kill primitive already exists. |
| Should Phase 6 block on the VPS? | **No.** P6 adds no box-side behavior; it builds and tests entirely locally against a stub. |

**Bottom line:** Phase 5 is closed, its irreversible surface re-fail-verified, and both security-sensitive surfaces audited clean. Phase 6 is design-complete (APPROVED v1.0) and fully local-buildable **against a mock vendor with the layer disabled** — build it now; **the DPA gates enablement, not implementation.** Line up the vendor pick, the signed DPA, and the platform disclosure/opt-out wiring before flipping `external.enabled` on.

*Also still open (unchanged, non-blocking): LICENSE (DECISIONS §6.1 — pick before external distribution); DR-3 admin-audit log (gates P3 go-live, and the global-fallback-kill / vendor-enablement admin actions ride it).*
