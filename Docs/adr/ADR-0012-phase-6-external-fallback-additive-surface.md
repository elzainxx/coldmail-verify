# ADR-0012 — Phase-6 external-fallback additive surface (anti-laundering, spend-once, hard-to-enable)

**Status:** ACCEPTED
**Doors touched:** Door-1/4 additivity + **Door 3** (the plaintext egress) + **Door 2** operational state
**Reopens a frozen door?** **No** — it *enforces* them: reuses the P1-reserved `external_verified` reason (zero new vocab), egresses plaintext transiently without a new at-rest store, adds a new append-only table, and keeps the vendor a brain-side call (never a node).
**Phase:** 6 · Depends on: P1 (reserved reason + policy extension point), P2/P4/P5 (the four residue classes), the GAP-37 CAS.

---

## Context

Phase 6 adds **one new policy layer** — an external verifier (MillionVerifier-class) called **only**
for the residue every prior phase structurally could not answer (catch-all, `:mx`-routed big
providers, retry-exhausted greylisted, gave-up rate-limited). It makes the system usable on
consumer/catch-all lists immediately, at bounded cost, while the P7 data moat grows underneath.

This is the **first deliberate egress of plaintext PII to a third party**, and it carries **two
irreversible correctness surfaces** the design spec (history/phase-6-fallback-spec.md, APPROVED v1.0) marks
as load-bearing: **anti-laundering** (§3.4) and **spend-once** (§5.4/GAP-42). The build is local-only
against a **mock vendor**; the real vendor is not enabled this phase (the DPA gates enablement, not
the build). OQ-P6-3 (asymmetric vendor-invalid) and OQ-P6-6 (disclose + opt-out) were DECIDED
pre-build (open-decisions §6).

## Decision

**1. Anti-laundering (§3.4, the correctness heart) — a rented "valid" is never SMTP-grade.**
`internal/external.Resolve` is the single place a vendor result becomes a system verdict, and it
**cannot** produce `status:valid` by construction: a vendor "valid" maps to `risky` /
`external_verified` at a confidence **capped** at `confidence_cap` (60) — below any SMTP-earned
`valid` (baseline 90). This keeps the rented population *separately measurable* so P7 can measure &
wean the vendor (GAP-41); laundering it into our own `valid` would poison calibration with the
vendor's errors. The policy gate strings agree (defense in depth): `external:valid` →
`risky:external_verified:stop`, **not** valid. Fail-verified: removing the valid→risky mapping breaks
`TestResolve_VendorValid_IsNeverValid`.

**2. OQ-P6-3 asymmetry (DECIDED) — trust the vendor's "no" only where "no" is possible.** A vendor
"invalid" is accepted as `invalid/external_verified` **only** on `retry_exhausted_greylisted` /
`gave_up_rate_limited` (where an independent non-existence is structurally plausible), and is
**downgraded to `risky`** on `catch_all` / `provider_no_smtp` (an accept-all domain accepts
everything, so a vendor "invalid" there is a guess — and a false "invalid" irreversibly discards a
real address). Encoded in `Resolve`, keyed on the residue class.

**3. Spend-once (§5.4/GAP-42) — the GAP-37 CAS extended from settle-once to spend-once.** The paid
vendor call's cost/provenance record (`external_calls`) commits **inside the same conditional
compare-and-set** as the P5 settlement (`SettleWorkItem` gained a `*ExternalCallRow` argument, step
5 of the CAS tx). A reclaimed/lost-lease attempt matches zero rows → the whole tx rolls back → **no
charge is recorded** for the discarded settle. The vendor is called before the CAS (an at-most-once
side effect of an at-least-once attempt), with a vendor idempotency key (`email_hash ‖ vendor`,
OQ-P6-4) so even a genuinely duplicated *call* returns the same billed result; the bounded worst case
is one extra read-only lookup, alertable. Fail-verified: neutering the CAS guard makes a stale
attempt double-charge, breaking `TestSpendOnce_UnderSlowDrainerReclaim`.

**4. Residue-only routing (§4, the cost firewall) — a closed set, escalate-once at mint.**
`external.Classify` returns exactly the four residue classes; hard-invalid, SMTP-earned valid, cache
hits, and disposable-risky **never** escalate. Escalation is decided at **verdict-mint** — the
`compute()` cache-hit path (`computeFromCache`) never reaches the external stage, so a re-uploaded
list returns the cached `external_verified` verdict with **no second vendor call** (GAP-43). Terminal
residue (catch_all/provider_no_smtp) escalates inline in `compute()`; retry-exhausted/gave-up
capacity escalates only at the drainer's give-up point (`EscalateExhausted`) — preserving the P5
retry-first invariant.

**5. `unknown` stays first-class (principle 2).** Vendor unknown / down / over-budget / breaker-open
→ `keep_residue`: the residue verdict stands **unchanged**, never fabricated, never worse than P5.
The per-vendor budget + circuit breaker (`internal/external.Decide`) is a **brake, not a cascade**
(alert-and-degrade), and is a **distinct control** from the P4 IP-reputation rate limiter (GAP-44) —
different resource (money vs reputation), different table.

**6. Hard-to-enable (§7.4) — the layer ships DISABLED.** Egress requires **three independent
affirmative switches**: the kill-switch `Fallback==enabled` (default off), `ExternalDPAConfirmed`
(default false), and a loaded vendor key. `Config.ExternalFallbackEnabled()` is the AND of the first
two; `buildExternal` also requires the key. The **global-fallback-kill** (`Fallback==off`) overrides
all of them. Enabling pre-DPA is unlawful sub-processing, so no single switch — and no code default —
turns it on. **PENDING-REAL-VENDOR:** no real HTTP adapter ships this phase; `buildExternal` refuses
to wire a live-looking stage against a non-existent vendor (never faking green).

**7. Additive surface.** New: `internal/external` package; `signals.external.*` provenance bag (Door
1 additive) + the vendor JSON in the reserved `raw` sidecar; the `external` open policy key (P1 §7.2
extension point); `external_calls` (a new **append-only** table, migration 0011; `cp_runtime`
SELECT+INSERT only, DELETE `cp_admin`). **Zero** new `status`/`reason`/`recommendation` values
(`external_verified` was reserved in P1 — Door 4). **No** plaintext persisted by P6 (`external_calls`
stores `email_hash` only; the address egresses transiently, reusing the P5 queue plaintext, purged on
settle — Door 3). The vendor is never a node/gem (Door 5).

## Consequences

- P7 can isolate the rented population (`WHERE reason='external_verified'`) and group by
  `(residue_class × mx_provider × vendor)` with the vendor's own claimed status + confidence + cost —
  the exact join the weaning decision runs on. **P6 builds the provenance; the weaning is SEAM → P7.**
- The `external_calls` table is queryable by `email_hash` for SAR/deletion (PN-10). A standing
  cumulative-egressed metric + periodic vendor-deletion (PN-7/F-5) are §7.4 checklist items, not built
  here.
- Vendor **accuracy** on this exact residue is untestable pre-integration (GAP-46/47): the mechanics
  (routing, anti-laundering, spend-once, breaker, cache-no-rebill, global-kill, privacy) are
  fail-verified against the mock; accuracy is PENDING-REAL-VENDOR and measured by P7. The claim is
  **coverage is bounded and honest, not accurate.**
- The master invariant (§8) holds: the shared `compute()` gate path is byte-identical to P5; the
  external stage is an additive settle-path step reached identically by sync and queue.

## Does this reopen a frozen door?

**No.** Door 1: additive `signals`/`raw` only, no top-level field, no type change. Door 2:
`verifications` untouched; `external_calls` is a new append-only sidecar. Door 3: no new plaintext
store — the address egresses transiently (the P5 exception, purged on settle) and only `email_hash`
is persisted by P6. Door 4: zero new vocab (`external_verified` reserved in P1); the anti-laundering
rule means the external status still resolves into the *existing* `valid`/`risky`/`unknown` at a
capped confidence. Door 5: the vendor is a brain-side HTTP call, never a node, never in the gem — the
cleanest Door-5 posture of any phase.
