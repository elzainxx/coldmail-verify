# ADR-0014 — Phase-7 flywheel: the additive surface + the two irreversible correctness surfaces

- **Status:** ACCEPTED (Phase-7 local build, 2026-07-19)
- **Kind:** door-touching **only** in that it fills reserved seams and adds operational state — it **reopens none of the five frozen doors**. Records the P7 build's surface so the additive claim is auditable (mirrors ADR-0010/0011/0012 for P4/P5/P6).
- **Deciders:** owner (P7 build authorization: "finish all of it")
- **Builds on:** [ADR-0013](ADR-0013-outcome-feed-auth-mechanism-and-tb7-threat-model.md) (the feed-auth mechanism + TB-7 threat model — the pre-P7 gate, discharged first). The spec authority is [`history/phase-7-flywheel-spec.md`](../history/phase-7-flywheel-spec.md) (APPROVED v1.0).

## Context

Phase 7 closes the loop: a `send_outcome` joins back to `verifications.id`, is classified for what it proves about the *address*, and — for real-signal outcomes only — recalibrates confidence, promotes providers, measures accuracy, and weans off the P6 fallback. It is entirely brain-side. The spec names two load-bearing correctness surfaces (the P7 analogues of P5's GAP-37 / P6's anti-laundering):

1. **The bounce classifier's exclusion discipline** (§4, GAP-49) — the correctness heart.
2. **Feedback-loop safety** (§6, GAP-50) — the runaway guard.

Both are irreversible in the sense that getting them wrong *silently and permanently poisons* the calibration data the flywheel exists to build. This ADR records how they shipped and proves the whole surface is additive.

## Decision — the additive surface (as built)

**New brain-side package `internal/flywheel`** (pure logic, the `internal/external` pattern):
- `classifier.go` — the deterministic `event → {invalid-signal | valid-signal | zero-information}` map (§4.2) + evidence-strength weighting (§4.5) + `NormalizeEvent` (the DSN→event DIAL). `mailbox_full→valid-signal`, blocked/policy/transient→zero-information. `calibration_class` is a **P7-internal taxonomy, NOT verdict vocab** — it never reaches `/verify`.
- `guards.go` — the four feedback-loop guards (§6): sample-size gate, bounded step, **down-fast/up-slow hysteresis**, and the **cascade brake over a ROLLING window** (catches staggered drift — the P4 lesson-a). `ValidateGuardParams` **fails at startup** on an out-of-range brake config (the P4 lesson-b — never a silent disable).
- `historical_gate.go` — the §4.6 gate: TTL-windowed hard invalid on a confirmed `mailbox_not_found`, degrading to a recoverable penalty past the TTL (OQ-P7-2), gated by **≥N independent outcomes** (F-17/PN-11).
- `promotion.go` — provider auto-promotion (§7), accuracy-with-**mandatory-coverage** (§8), and per-provider weaning (§9) — all recommend-only decision logic.
- `ingest.go` + `sources.go` — the ingestion path (join → integrity reject-and-flag → classify → append) behind the swappable **`OutcomeSource`** interface (ADR-0013): `SimulatedSource` (local build) + `PullSource` (the decided mTLS mechanism, fail-closed / PENDING-REAL-SENDER).
- `recalibrate.go` — the A6 recalibration pass tying guards → the versioned store under the exactly-once CAS, **recommend-only by default**.

**New tables (migration 0012)** — all operational, none frozen:
- `send_outcomes` **filled** (created empty in P1 §4.7): P7 adds `calibration_class`, `ingest_error`, `is_external_verified`, `residue_class`, `vendor`, `from_cache`, `ingested_at`, and the **`(message_id, event)` partial UNIQUE** dedup index (PN-14a resolved: the guard's home is a DB constraint).
- `calibration` — the versioned, effective-dated confidence store (PN-14 sketch). Bucket `(reason × mx_provider)`; external-verified verdicts get a **parallel bucket space** keyed by `(vendor, residue_class, mx_provider)` — a CHECK keeps them cleanly separable (GAP-54).
- `recalibration_runs` — the exactly-once recalibration writer's lease/CAS record (GAP-53).
- `outcome_ingest_cursor` — the durable pull watermark (ADR-0013 §B.2).

**Two LIVE-PATH verdict touches** (`orchestrator/flywheel_stage.go`), both nil-safe + opt-in-OFF by default:
- The §4.6 gate short-circuits to `invalid/historical_bounce` (the **P1-reserved** reason — zero new vocab).
- The §5.4 read replaces the P1 baseline confidence with the current **effective** calibrated version (recommend-only proposals are inert until approved), re-deriving the recommendation honoring composability.

## The two irreversible surfaces — fail-verified (neuter → break → restore → green)

| Surface | What shipped | Fail-verified |
|---|---|---|
| **Classifier exclusion** (§4.3, GAP-49) | `Classify` maps blocked/policy/spam/greylist/deferred/complaint/unknown → `zero-information`, NEVER invalid-signal; enforced again at the aggregate read (`BucketAggregatesSince` WHERE clause) | neuter (map `blocked`→invalid-signal) → `TestClassify_BlockedNeverInvalidNumerator` **and** `TestIngest_BlockedNeverCalibrates` break; restore → green |
| **Recalibration CAS** (§5.5, GAP-53) | `CommitRecalibration` stages calibration versions + flips the run `leased→committed` under ONE compare-and-set on the lease-holder; a reclaimed lease matches zero rows → discards all writes | neuter (drop the `status='leased'` guard) → `TestRecalibrate_CASExactlyOnce` breaks; restore → green. Proven on **real Postgres** too (`TestPGRecalibration_CASExactlyOnce`) |

Both proven on the Mem fake AND real Postgres 16 (fresh migrations 0001–0012). The `(message_id, event)` dedup is DB-enforced (`TestPGSendOutcome_DedupNoOp`). The staggered-drift + startup-config-validation regressions are locked by `TestCascadeBrake_CatchesStaggeredDrift` + `TestValidateGuardParams_RejectsSilentDisable`.

## Why this reopens no frozen door

| Door | P7's additive move — nothing frozen touched |
|---|---|
| **Door 1** (response shape) | a recalibrated `confidence` is a different int in the same field (type/range unchanged); one additive `flags.historical_bounce` + `signals.calibrated`/`historical_bounce_penalty` keys; no new top-level field |
| **Door 2** (append-only LOG) | `verifications` is **read-only** at calibration — not one column added, not one row mutated. Recalibration writes the SEPARATE `calibration` store; outcomes append to `send_outcomes`. The log stays append-only |
| **Door 3** (hashing, no plaintext) | the join runs on `verifications.id` + `email_hash` (both locked); P7 adds no hashing; `send_outcomes` stores `email_hash` (rollup) + `domain`, never plaintext |
| **Door 4** (vocab) | **zero** new `status`/`reason`/`recommendation` — the gate uses the P1-reserved `historical_bounce`; `calibration_class` is P7-internal, never in `/verify`. `vocab.go` byte-identical since Phase 0 (`d3e56d8`) |
| **Door 5** (never fork the gem) | entirely brain-side; the gem/box never learn an outcome came back; the sender is a separate system feeding an inbound signal (ADR-0013 mTLS pull) — the cleanest Door-5 posture alongside P6 |

## Consequences

- **Positive.** The loop is closed: verdicts can be measured against reality and calibrated, inside the frozen doors, additive throughout. Both irreversible surfaces are fail-verified. Recommend-only defaults + the fresh-system invariant mean a fresh P7 system performs **zero live mutation** (behavior identical to end-of-P6). The transport is a swappable adapter (reversible mechanism choice).
- **Cold-start honesty is structural, not disclaimed** (§14.2 / GAP-52): uncalibrated buckets use the P1 baseline (`GetEffectiveCalibration` cold-start), promotion/weaning start at zero (sample-gated), and accuracy is unreportable without coverage (`Accuracy.Reportable`). No code path claims a moat at ship time.
- **PENDING-REAL-SENDER (never faked green):** real accuracy numbers, live authentication, the agreed `send_outcome` contract, and any promotion/weaning firing all require the real sender ("Feature 2", GAP-48) — un-mockable pre-integration, exactly as P6's vendor accuracy was PENDING-REAL-VENDOR. The apparatus is fail-verified; the accuracy is not asserted.
- **Recorded:** this ADR; the ADR index (ADR-0014); `history/phase-7-status.md`; the manifest §7.5 concrete env rows + regenerated `config-reference.md`; open-decisions §6 register (OQ-P7-1 mechanism resolved via ADR-0013).

## Compliance / how it's checked

- 21+ Go packages green on **real Postgres 16** (fresh 0001–0012), shuffle-stable; `go vet` clean; gem-unmodified (Door 5); config-reference fresh; docs-updated.
- Both irreversible surfaces fail-verified (neuter → the named test breaks → restore → green), on Mem and (for the CAS + dedup) real Postgres.
- Fresh-system invariant: `TestFreshSystem_FlywheelWiredButInert` — a wired-but-fresh flywheel yields a byte-identical verdict to the flywheel-off orchestrator (DR-2 / pre-takeoff §3.5).
