# Phase 6 — status: the third-party fallback (local build scorecard)

**Date:** 2026-07-18 · **Phase:** 6 (rent the moat — external fallback on the residue) · **Spec:** [`phase-6-fallback-spec.md`](phase-6-fallback-spec.md) (the authority)
**Status: ✅ COMPLETE (local build, layer DISABLED)** — the external-verifier layer, residue-only
routing (a closed 4-class set), the anti-laundering rule, per-vendor budget + circuit-breaker, the
global-fallback-kill, and the P7-weaning provenance are built and test-proven against a **controllable
mock vendor**, exactly as Phases 1–5 built locally (Postgres + Go, no VPS). The layer ships **DISABLED
by default** and refuses to egress until the §7.4 DPA checklist passes — enabling pre-DPA is unlawful
sub-processing. The one genuinely un-testable behavior (vendor **accuracy** on this exact residue,
GAP-46/47) is honestly **PENDING-REAL-VENDOR** — the mechanics are fail-verified, accuracy is not faked.

**What Phase 6 is:** **one new policy layer** the engine evaluates LAST, an additive settle-path step
reached identically by sync `/verify` and the P5 drainer (§8 master invariant preserved — the shared
`compute()` gate path is byte-identical to P5). New brain-side components: `internal/external` (the
Vendor interface, the residue classifier, the anti-laundering resolver, the per-vendor budget +
breaker, the mock adapter), the external stage in the orchestrator, the `external_calls` append-only
table, and the spend-once extension of the P5 CAS. **No box change** (Door 5), **zero new verdict
vocab** (Door 4 — reuses the P1-reserved `external_verified`), **no gem patch**, **no new plaintext
store** (Door 3 — the address egresses transiently, only `email_hash` is persisted).

**Prerequisite gate (checked before starting, per the kickoff's stop-if-unmet):** ✅ Phase 5 closed
green (`520a2a8`); GAP-37/38 re-fail-verified on real Postgres; queue dials promoted to config; DR-4
containment re-proven. Vendor selection + DPA are **NOT** build prerequisites — they gate *enabling*
the layer; it is built DISABLED.

---

## The two load-bearing correctness surfaces — fail-verified

Both irreversible surfaces the spec marks load-bearing (§3.4 + §5.4) are built and **fail-verified**
(neuter the guard → the named test breaks → restore → green):

| Surface | What shipped | Fail-verified |
|---|---|---|
| **Anti-laundering** (§3.4, OQ-P6-1) | `external.Resolve`: a vendor `valid` → `risky/external_verified` capped at `confidence_cap` (60), NEVER SMTP-grade `valid`; the policy `external:valid` gate agrees (`risky:…`, defense in depth) | neuter the valid→risky mapping in `Resolve` → `TestResolve_VendorValid_IsNeverValid` fails ("laundered into status:valid"); restored green |
| **Spend-once** (§5.4/GAP-42) | `SettleWorkItem` gained a `*ExternalCallRow` recorded **inside the same CAS tx** — the vendor call + cost-record + settle commit under the ONE compare-and-set on the lease-holder | neuter the Mem CAS guard → the stale reclaimed attempt double-settles → **two spend records** → `TestSpendOnce_UnderSlowDrainerReclaim` fails; restored green |

**OQ-P6-3 asymmetry (DECIDED, implemented):** a vendor `invalid` is accepted as `invalid` only on
`retry_exhausted_greylisted`/`gave_up_rate_limited` (independent non-existence plausible), and
**downgraded to `risky`** on `catch_all`/`provider_no_smtp` — `TestResolve_VendorInvalid_Asymmetry_OQP63`.

---

## Build increments (all local, all test-proven against the mock vendor)

1. **`internal/external`** — the swappable `Vendor` interface (§8); the controllable **mock** adapter
   (valid/invalid/unknown/error on command, counts calls + idempotency keys); `Classify` (the closed
   4-class residue set + `route_when`); `Resolve` (anti-laundering + OQ-P6-3); the per-vendor `Budget`
   + circuit `Breaker` (pure `Decide` over durable spend facts); `DeriveVendorIdempotencyKey`.
2. **Store** — `ExternalCallRow` (append-only spend + provenance); `SettleWorkItem` extended with the
   in-CAS spend record (spend-once); `AppendExternalCall` (sync path); `ExternalSpendSince` (windowed
   durable budget read); migration `0011_external_calls.sql` (append-only, `email_hash` only, grants).
3. **Orchestrator** — `runExternal` (the stage, mirrors `runSMTP`); `compute()` escalates the TERMINAL
   residue (catch_all/provider_no_smtp) inline; `EscalateExhausted` for the drainer's give-up path
   (retry-exhausted greylisted / gave-up capacity); `foldExternal` (capped `external_verified`,
   `signals.external.*` provenance bag, vendor JSON → the reserved `raw` sidecar); `keep_residue` on
   unknown/error/breaker-open. The spend record threads to the settler (sync append / queue CAS).
4. **Policy** — the `external` open block (§3.2); cold-email declares the `external` layer LAST in
   `order` + the `external:*` gates (`valid`→risky, never valid) + the `external_verified` baseline.
5. **Config / secrets / enablement** — the §6.2/6.3 dials (confidence cap, spend caps UNSET owner-OQ,
   breaker error-rate/cooldown); `TM_EXTERNAL_DPA_CONFIRMED` (default false); `ExternalFallbackEnabled`
   = kill-switch `Fallback==enabled` AND DPA confirmed; the vendor key is the third switch; the
   global-fallback-kill (`Fallback==off`) overrides. `buildExternal` wires the stage only when all
   three are affirmative (and refuses a live stage with no real adapter — PENDING-REAL-VENDOR).

---

## Behavioral tests (real, against the mock — §14 / §15 acceptance criteria)

All green (21 packages, `go test ./...`). The acceptance criteria mapped to tests:

| # | Criterion | Test |
|---|---|---|
| 1 | A residue verdict escalates + settles `external_verified` with `signals.external.*` + raw | `TestExternal_ProviderResidueEscalates_VendorValidIsNeverValid` |
| 2 | A vendor `valid` never produces `status:valid` (**headline**) | same + `TestResolve_VendorValid_IsNeverValid` (fail-verified) |
| 3 | Hard-invalid / SMTP-valid / cache-hit / disposable **never** escalate (cost firewall) | `TestExternal_NeverEscalatedSet`, `TestClassify_ClosedSet` |
| 4 | A P5-reclaimed residue attempt does not double-charge (spend-once) | `TestSpendOnce_UnderSlowDrainerReclaim` (fail-verified) + `TestDrainer_ExhaustedGreylistEscalatesToVendor_SpendOnce` |
| 5 | Breaker-open / budget-exhausted / vendor-unknown → `keep_residue` | `TestExternal_BreakerOpen_KeepsResidue`, `TestExternal_VendorUnknown_KeepsResidue`, `TestBreaker_*` |
| 7 | Provenance is P7-queryable (rented population isolable, per-vendor) | `TestExternalSpendSince_WindowsAndOutcomes` + the `external_calls` schema |
| 8/9 | Address-only egress; no plaintext persisted by P6 | `TestExternal_AddressOnlyEgress_NoPlaintextPersisted` |
| 9 | Cache re-read does not re-bill (GAP-43) | `TestExternal_CacheReReadDoesNotReBill` |
| 10 | A policy without an `external` block runs unchanged (opt-out); global-kill stops all egress | `TestExternal_OptOut_And_GlobalKill` |
| 11 | An unrecognized vendor status → `unknown`, never `valid` | `TestResolve_VendorUnknown_KeepsResidue`, mock normalization |
| 14 | Disabled by default; refuses to enable without the checklist | `TestExternalFallback_DisabledByDefault_RefusesWithoutChecklist` |

---

## Additivity (§10) — proven additive against all five frozen doors

| Door | P6's additive move — nothing frozen touched |
|---|---|
| **Door 1** (response shape) | new `signals.external.*` keys + the vendor JSON in the reserved `raw` sidecar; `meta.node_id`/`verifier_ip` null on a fallback row (no box ran); no new top-level field |
| **Door 2** (append-only LOG) | a fallback verdict is ONE ordinary `verifications` row; `external_calls` is a NEW append-only sidecar table (SELECT+INSERT for `cp_runtime`); no mutation of prior rows |
| **Door 3** (hashing, no plaintext) | the address egresses **transiently** (the P5 queue plaintext / the sync raw input), purged on settle; P6 persists **no plaintext** — `external_calls` stores `email_hash` only |
| **Door 4** (vocab) | **zero** new `status`/`reason`/`recommendation` — uses the P1-reserved `external_verified`; the external status resolves into the *existing* vocabulary at a capped confidence |
| **Door 5** (never fork the gem) | the vendor is a brain-side HTTP call, never a node, never in the gem/box, never health-watched or rate-limited by the P4 pool — the cleanest Door-5 posture of any phase |

`vocab.go` remains byte-identical since Phase 0 (`d3e56d8`). The shared `compute()` gate path is
unchanged for any non-escalated verdict (master invariant §8).

---

## PENDING-REAL-VENDOR / rollout (never faked green — the honest ledger)

| Item | Why deferred | Verification command (when a real vendor + DPA exist) |
|---|---|---|
| Vendor **accuracy** on the residue (GAP-46/47) | catch-all/big-provider is exactly where vendors are weakest; un-mockable pre-integration — like P2's un-mockable greylist (GAP-12/35) | Live integration + P7 measurement: `SELECT residue_class, vendor, vendor_status_raw, <bounced?> FROM external_calls JOIN outcomes …` — measured, never claimed |
| Real vendor HTTP adapter | no real vendor enabled this phase (kickoff); the interface + mock prove the mechanics | Implement a real `external.Vendor` adapter; wire it in `buildExternal`; the layer is one adapter + the §7.4 checklist away from live |
| The §7.4 DPA / due-diligence gate | owner/legal action; enabling pre-DPA = unlawful sub-processing | DPA signed + retention/deletion recorded + TLS + key custody → set `TM_EXTERNAL_DPA_CONFIRMED=true` + `TM_FALLBACK_MODE=enabled` + `TM_VENDOR_KEYS` |
| Budget ceiling **values** (OQ-P6-5) | owner-OQ — "start conservative, raise from observed residue-tail volume, never loosen blind" | set `TM_EXTERNAL_DAILY_CAP` / `_MONTHLY_CAP` / `_PER_JOB_CAP` / `_RATE_CEILING` from §9 observed volume |
| PN-7/F-5 standing controls | cumulative-egressed metric + periodic vendor-deletion — §7.4 checklist items, not built here | operational (the exposure grows unbounded; exercise the DPA deletion right on a cadence) |

**Claim discipline (GAP-47):** coverage is **bounded and honest, not accurate**. We can prove we only
spend on the tail and only rent on the residue; we cannot prove the vendor is *right* until P7 measures
it against real bounces. Don't let "we have fallback coverage now" harden into "we're accurate on Gmail now."

---

## Dials — wired in config (conservative / owner-OQ defaults)

| Dial | Default | Note |
|---|---|---|
| `external.confidence_cap` (per-policy JSON, **not** an env var) | 60 | §3.4 anti-laundering ceiling — a rented verdict is never SMTP-grade. Read from the policy (`external.Resolve`), per-policy by design. A global `TM_EXTERNAL_CONFIDENCE_CAP` was removed in the P6→P7 pass as settable-but-ignored. |
| `TM_EXTERNAL_DAILY_CAP` / `_MONTHLY_CAP` / `_PER_JOB_CAP` / `_RATE_CEILING` | **UNSET** (owner-OQ) | §5.2 — 0/unset ⇒ not enforced; start conservative, raise from observed volume |
| `TM_EXTERNAL_ERROR_RATE_PCT` | 0 (off) | §5.3 cond-2 — budget ceilings apply until an owner tunes the error-rate |
| `TM_EXTERNAL_ERROR_MIN_SAMPLES` | 20 | §5.3/GAP-27 sample gate (mirrors block-rate) |
| `TM_EXTERNAL_BREAKER_COOLDOWN_SECONDS` | 300 | §5.3 open duration before a half-open probe |
| `TM_EXTERNAL_DPA_CONFIRMED` | **false** | §7.4 legal enablement gate — the layer stays disabled until set |
| `TM_FALLBACK_MODE` | `off` (default) | kill-switch Rung 2 / global-fallback-kill — must be `enabled` to egress |

---

## What Phase 6 does NOT build (correct scope — SEAM → later)

- **The flywheel / weaning (P7).** P6 builds the *provenance* that makes weaning measurable (per
  `residue_class × mx_provider × vendor`, with the vendor's own status + confidence + cost); it does
  NOT run the bounce-join, calibrate, set a threshold, or drop the fallback. `SEAM → P7`.
- **A real vendor adapter / live egress.** PENDING-REAL-VENDOR; the mock proves the mechanics.
- **Multi-vendor A/B.** One vendor at launch (OQ-P6-8); the interface makes a second a config+adapter
  change. `SEAM → P7`.
- **The async sync-fallback requeue** (OQ-P6-7 opt-in seam). Default is inline with a tight timeout.
- **The platform's privacy terms / opt-out UX** (OQ-P6-6 is DECIDED disclose+opt-out; the brain makes
  the opt-out mechanical — a policy without the `external` block — but does not write platform terms).

---

## Review gate

`code-reviewer` + the adversarial multi-dimension review workflow (anti-laundering, spend-once,
routing, PII-egress, breaker, additivity) ran on the change; findings resolved. Full gate green on
real Postgres 16 (fresh migration 0001–0011), `go vet` clean, shuffle-stable, gem-unmodified,
config-reference fresh. ADR-0012 records the door-touching surface; DECISIONS §6 confirms OQ-P6-3/6.
