# Phase 7 — status: the flywheel (local build scorecard)

**Date:** 2026-07-19 · **Phase:** 7 of 7 (close the loop — own data moat, real accuracy) · **Spec:** [`phase-7-flywheel-spec.md`](phase-7-flywheel-spec.md) (APPROVED v1.0, the authority) · **This is the final phase — the seven-phase arc is closed.**
**Status: ✅ COMPLETE (local build, against a simulated authenticated feed)** — the join activation, the bounce classifier, gated recalibration with the four loop-safety guards, provider auto-promotion, accuracy-with-coverage, and per-provider fallback weaning are built and test-proven against a **`SimulatedSource`** outcome feed, exactly as P1–P6 built locally (Postgres + Go). The two live-path touches (the §4.6 gate + the §5.4 calibrated read) ship **OFF by default**; recalibration/promotion/weaning are **recommend-only**. The genuinely un-testable parts (real accuracy, live authentication, the agreed contract, any promotion/weaning firing) are honestly **PENDING-REAL-SENDER** — the machine is fail-verified, the accuracy is not asserted.

**Prerequisite gate (checked before starting, per the kickoff's stop-if-unmet):** ✅ Phase 6 closed green (both irreversible surfaces re-fail-verified); ✅ **ADR-0001's outcome-feed authentication mechanism DECIDED** — this was the hard door. It was *undecided* at kickoff (deliberately open per ADR-0001), so the FIRST action was to discharge it: [ADR-0013](adr/ADR-0013-outcome-feed-auth-mechanism-and-tb7-threat-model.md) produces the **TB-7 malicious-sender threat model** + decides the **mechanism (mutually-authenticated pull / mTLS)**, recorded + resolving the DECISIONS §5 parked line + ticking pre-takeoff §3.4. Only then did P7 code begin. ✅ `send_outcome` contract shape agreed (P7 §3.2 LOCKED; P7 defines the interface, the sender supplies it — GAP-48).

---

## The two load-bearing correctness surfaces — fail-verified

Both irreversible surfaces the spec marks load-bearing (§4 + §6) are built and **fail-verified** (neuter the guard → the named test breaks → restore → green), on the Mem fake AND — for the CAS + dedup — real Postgres 16:

| Surface | What shipped | Fail-verified |
|---|---|---|
| **Bounce classifier exclusion** (§4.3, GAP-49) | `flywheel.Classify`: blocked/policy/spam/greylist/deferred/complaint/unknown → `zero-information`, NEVER invalid-signal; `mailbox_full`→valid-signal (existence proof); enforced AGAIN at the aggregate read (`BucketAggregatesSince` WHERE) | neuter (map `blocked`→invalid-signal) → `TestClassify_BlockedNeverInvalidNumerator` **and** `TestIngest_BlockedNeverCalibrates` fail ("a blocked bounce entered a calibration bucket"); restored green |
| **Recalibration CAS-on-lease-holder** (§5.5/GAP-53) | `CommitRecalibration` stages calibration versions + flips the run `leased→committed` under ONE compare-and-set; a reclaimed lease matches zero rows → discards all writes | neuter (drop the `status='leased'` guard) → `TestRecalibrate_CASExactlyOnce` fails ("want ErrRecalLeaseLost"); restored green. Also proven on real PG (`TestPGRecalibration_CASExactlyOnce`) |

**The two P4 cascade-brake lessons (kickoff: "do not repeat either"):**
- **(a) staggered drift** — the brake reasons over a **rolling multi-cycle window**, not a single-cycle snapshot. `TestCascadeBrake_CatchesStaggeredDrift`: a few buckets moving hard each cycle (no single cycle looks systemic) trips the window sum.
- **(b) silent disable** — the brake config **fails at startup** if out of range (`ValidateGuardParams` + `config.Validate`: `recal_brake_bucket_pct` must be 1..99, window ≥1, up-step 1..100). `TestValidateGuardParams_RejectsSilentDisable` + `TestLoad_P7BrakeRangeGuard`.

---

## Build increments (all local, all test-proven)

1. **Gate discharged first** — `ADR-0013` (TB-7 threat model + mTLS-pull mechanism), the swappable `OutcomeSource` interface (reversible to signed-webhooks by one adapter), fail-closed (`SimulatedSource` for local build; `PullSource` = PENDING-REAL-SENDER).
2. **`internal/flywheel`** — the bounce classifier (§4) + evidence-strength weighting (§4.5) + `NormalizeEvent` (the DSN→event DIAL); the four guards (§6); the historical-bounce gate (§4.6, ≥N + TTL→penalty); promotion/accuracy/weaning (§7/§8/§9); the ingestion path (§3, join → integrity reject-and-flag → classify → append); the recalibration pass (§5, recommend-only).
3. **Store** — `send_outcomes` filled + the `calibration`/`recalibration_runs`/`outcome_ingest_cursor` types + methods; the exclusion discipline enforced at the aggregate read; the CAS-on-lease-holder writer; the address-level historical-bounce rollup. Migration `0012_flywheel_calibration.sql` (the `(message_id,event)` UNIQUE dedup, the versioned calibration store, grants).
4. **Orchestrator** — two nil-safe, opt-in-OFF live-path touches: `historicalGate` (short-circuit to `invalid/historical_bounce`) + `calibrationConfidence` (read the effective version, re-derive the recommendation). `policy.Recommend` exposed so a calibrated confidence re-derives its recommendation honoring composability.
5. **Config / enablement** — the §4.6/§5/§6 dials wired + CONSUMED (`buildFlywheel`); two explicit opt-in flags (`TM_FLYWHEEL_GATE_ENABLED` / `_CALIBRATION_ENABLED`, default OFF); the brake range-guard at startup.

---

## Behavioral tests (real, against the simulated feed — §14/§15 acceptance criteria)

All green (22+ packages, `go test ./...`; the PG-parity subset on real Postgres 16). The mapped criteria:

| # | Criterion (§15) | Test |
|---|---|---|
| 4 | A blocked bounce NEVER enters a calibration numerator (fail-verify) | `TestClassify_BlockedNeverInvalidNumerator`, `TestIngest_BlockedNeverCalibrates` |
| 4 | `mailbox_full` counts as valid-signal | `TestClassify_MailboxFullIsValidSignal` |
| 4/§4.5 | delivered-on-catch-all weights ≈ 0 (doesn't calibrate) | `TestClassify_DeliveredOnCatchAllIsNearZero` |
| 7 | The sample gate blocks a recalibration below threshold | `TestSampleGate_BlocksBelowThreshold`, `TestRecalibrate_SampleGateBlocks` |
| 7 | The cascade brake catches STAGGERED drift, not just simultaneous | `TestCascadeBrake_CatchesStaggeredDrift` |
| 7 | The brake's config range is validated at startup (out-of-range → error) | `TestValidateGuardParams_RejectsSilentDisable`, `TestLoad_P7BrakeRangeGuard` |
| — | Fresh system: every mutator at default, zero live mutation (= end-of-P6) | `TestFreshSystem_FlywheelWiredButInert`, `TestRecalibrate_RecommendOnly_NoEffectiveVersion` |
| 3 | A replayed outcome doesn't double-count | `TestIngest_ReplayNoDoubleCount`, `TestPGSendOutcome_DedupNoOp` |
| 5 | The historical-bounce gate needs ≥N independent outcomes + expires on TTL | `TestHistoricalGate_NeedsMinIndependent`, `TestHistoricalGate_ExpiresToPenalty`, `TestHistoricalGate_FiresOnLivePath` |
| 5/§5.2 | external_verified verdicts bucket SEPARATELY | `TestIngest_ExternalVerifiedBucketsSeparately` |
| 2.1 | An unauthenticated outcome is rejected at ingestion (fail-closed) | `TestIngest_UnauthenticatedIngestNothing`, `TestPullSource_FailClosedWithoutClient` |
| 8 | Accuracy is never a bare number — coverage always attached | `TestAccuracy_NeverBareNumber` |
| 7/9 | Promotion + weaning are sample-gated, measured, reversible | `TestPromotion_SampleGatedAndReversible`, `TestWeaning_RequiresMeasuredMarginOnBoth` |
| 8 | Recalibration CAS exactly-once | `TestRecalibrate_CASExactlyOnce`, `TestPGRecalibration_CASExactlyOnce` |

---

## Additivity (§11) — proven additive against all five frozen doors

| Door | P7's additive move — nothing frozen touched |
|---|---|
| **Door 1** (response shape) | recalibrated `confidence` = a different int in the same field; additive `flags.historical_bounce` + `signals.calibrated`/`historical_bounce_penalty`; no new top-level field |
| **Door 2** (append-only LOG) | `verifications` **read-only** at calibration; recalibration writes the SEPARATE `calibration` store; outcomes append to `send_outcomes`; the log stays append-only |
| **Door 3** (hashing, no plaintext) | join on `verifications.id` + `email_hash` (both locked); no new hashing; `send_outcomes` stores `email_hash` + `domain`, never plaintext |
| **Door 4** (vocab) | **zero** new `status`/`reason`/`recommendation` — the gate uses the P1-reserved `historical_bounce`; `calibration_class` is P7-internal, never in `/verify`. `vocab.go` byte-identical since Phase 0 (`d3e56d8`) |
| **Door 5** (never fork the gem) | entirely brain-side; the gem/box never learn an outcome came back; the sender is a separate system (ADR-0013 mTLS pull) — the cleanest Door-5 posture alongside P6 |

---

## PENDING-REAL-SENDER / rollout (never faked green — the honest ledger)

| Item | Why deferred | Verification command (when the real sender exists) |
|---|---|---|
| Real **accuracy numbers** (§8) | measurable only against real send→bounce outcomes; a simulated corpus tests the MACHINE, never the ACCURACY (GAP-52, the P6-GAP-47 analogue) | live integration + `AccuracyByProvider` over real outcomes, reported WITH coverage (never a bare global number) |
| **Live authentication** | the sender must implement its half of the mTLS feed (ADR-0013) — cross-project (GAP-48) | wire a real `MTLSClient` into `PullSource`; `Authenticated()` flips true only then (fail-closed until) |
| The agreed **`send_outcome` contract** | shape is P7's to fix (§3.2); the SUPPLY is the sender's commitment | the sender emits the locked shape into its authenticated feed |
| **DSN-fidelity-bounded** classification quality (App C.1) | the classifier is correct by construction, but its real-world accuracy depends on the sender's raw DSNs — un-mockable pre-integration | measured against real DSNs; `NormalizeEvent` re-audited against retained `send_outcomes.raw` |
| Any promotion/weaning **actually firing** | both are volume-gated per provider (§7.4/§9.4); on day one ZERO buckets are promoted or weaned | `EvaluatePromotion`/`EvaluateWeaning` over real accumulated accuracy — the moat is years of running it, not a ship-time feature (§14) |

**Claim discipline (§14, GAP-52):** the machine is the deliverable; the moat is years of running it. **No code path or doc claims a data moat exists at ship time** — cold-start is structural (uncalibrated → P1 baseline; promotion/weaning start at zero; accuracy unreportable without coverage), not a disclaimer.

---

## Dials — wired in config (conservative / owner-OQ defaults)

See [`config-manifest.md`](config-manifest.md) §7.5 for the full as-built env rows. Highlights: both live-path touches (`TM_FLYWHEEL_GATE_ENABLED` / `_CALIBRATION_ENABLED`) default **OFF**; `TM_HISTORICAL_BOUNCE_MIN_N`=2 (F-17); `TM_HISTORICAL_BOUNCE_TTL_DAYS`=270 (OQ-P7-2); `TM_RECAL_MIN_BUCKET_SAMPLES`=500 (OQ-P7-3); `TM_RECAL_MAX_STEP`=10; `TM_RECAL_UP_STEP_PCT`=50 (down-fast/up-slow); `TM_RECAL_BRAKE_BUCKET_PCT`=50 (range-guarded 1..99); `TM_RECAL_BRAKE_WINDOW_CYCLES`=3 (>1 catches staggered); `TM_RECAL_CADENCE_SECONDS`=86400 (daily, OQ-P7-7).

---

## What Phase 7 does NOT build (correct scope — the arc's honest boundary, §14.3)

- **The sender ("Feature 2").** P7 defines the `send_outcome` shape + the ingestion interface; it does NOT build the mail-transfer agent, warmup, or bounce-webhook plumbing (GAP-48).
- **A real mTLS transport client.** PENDING-REAL-SENDER; `SimulatedSource` proves the mechanics; `PullSource` is the fail-closed skeleton.
- **`FUTURE / Phase-8+`** (named, not built): cross-verdict/address-level reputation; automated control-loop tuning (auto-adjusting `max_step`/`min_bucket_samples`); a live multi-vendor tournament; sharing/monetizing accuracy data.

---

## Review gate

`code-reviewer` (synchronous, full depth — the classifier + feedback loop are irreversible; the multi-agent workflow was NOT used, per the kickoff) ran on the change and found **5 findings (1 CRITICAL, 1 HIGH, 2 MEDIUM, 2 LOW)** — ALL real, ALL fixed:
- **CRITICAL** — `NormalizeEvent` matched bare numeric DSN substrings (`551`, `5.1.1`) via `strings.Contains`, so a transient/policy bounce carrying those digits in a queue-id/IP could be misclassified as `mailbox_not_found` (the §4.3/GAP-49 poisoning). **Fixed:** codes are now parsed STRUCTURALLY (front-of-line basic code + whole-token enhanced code via anchored regex); a bare 550 with no not-found text is no longer invalid-signal. Regression-locked (`TestNormalizeEvent_NoBareNumericSubstringPoisoning`) + fail-verified.
- **HIGH** — the cascade brake's rolling window was defeated because per-run `buckets_at_max` was never persisted (prior cycles contributed 0 to the numerator → staggered drift undetectable via the store path — the P4 lesson-a, regressed). **Fixed:** added `buckets_at_max` to `recalibration_runs` + `RecalRun` + `CommitRecalibration`; the window now sums the persisted numerator. Regression-locked (`TestRecalibrate_StaggeredDriftHeldViaStorePath`) + fail-verified (neuter the persisted numerator → the test breaks).
- **MEDIUM** — auto-mode reported `Approved=N` but never called `ApproveCalibration` (a silent no-op). **Fixed:** it now approves each committed proposal by id, surfacing any partial failure.
- **MEDIUM** — an unused `GRANT UPDATE ON send_outcomes` left tamper surface over the integrity columns. **Fixed:** `REVOKE UPDATE, DELETE` (the code only INSERTs; classifier fields are set at insert).
- **LOW ×2** — a config comment overstated dial consumption + the brake numerator counts only down-moves; both clarified in-code (the down-only asymmetry is deliberate — down-fast/up-slow).

`go vet` clean, gem-unmodified, config-reference fresh. Both irreversible surfaces fail-verified on Mem AND (the CAS + dedup) real Postgres (neuter → break → restore); the CRITICAL classifier fix + the HIGH persisted-numerator fix are each regression-locked and fail-verified.

**Real-Postgres 16 status:** ✅ **re-verified green against the AMENDED migration 0012.** After the post-review fixes (which added `recalibration_runs.buckets_at_max` and changed the `send_outcomes` grant to `REVOKE UPDATE, DELETE`), a fresh Postgres 16 was stood up, migrations 0001–0012 applied clean (the new column + revoked grant confirmed live), and **all 22 PG store tests + every package pass** — the `store` suite exercises the amended `buckets_at_max` CAS path and the `send_outcomes` grant directly. Re-confirm command: apply `migrations/0001–0012` to a fresh PG 16 and `TM_TEST_DATABASE_URL=… go test ./internal/store/ -run TestPG`.

[ADR-0013](adr/ADR-0013-outcome-feed-auth-mechanism-and-tb7-threat-model.md) records the feed-auth gate; [ADR-0014](adr/ADR-0014-phase-7-flywheel-additive-surface.md) records the door-touching additive surface; DECISIONS §5/§6 confirm OQ-P7-1 resolved + the register.

**The seven-phase arc is closed:** P1–P6 built a system that produces honest verdicts; P7 adds the loop that measures and calibrates them against reality — inside five frozen doors, additive throughout. Shipping P7 is finishing the machine; building the moat is using it, for years, at volume. The spec closed the arc by refusing to conflate the two, and this build honors that.
