-- Migration 0012: the Phase-7 flywheel — activate send_outcomes + the versioned calibration store,
-- the recalibration lease/CAS record, and the durable outcome-ingest watermark.
-- Implements: Phase-7 spec §3 (join activation + ingestion integrity), §3.4 (idempotency/GAP-51),
--             §4 (the bounce classifier's calibration_class), §5.4/PN-14 (the calibration store
--             column sketch + versioning), §5.5/GAP-53 (the recalibration CAS-on-lease-holder writer),
--             §6.4 (versioned rows make the cascade brake's rollback possible), ADR-0013 §B.2 (the
--             mutually-authenticated-pull watermark).
--
-- WHY THIS IS NOT A FROZEN-DOOR CHANGE (additive-only against all five doors — P7 §11):
--   * verifications (the append-only LOG, Door 2) is UNCHANGED — not one column added, not one row
--     mutated. Recalibration writes the SEPARATE calibration store; the verdict path READS it. The
--     log records what confidence WAS assigned; the store records the CURRENT model — they differ by
--     design (that difference is the recalibration audit trail, P7 §5.4 / Appendix C.4), never drift.
--   * send_outcomes + verification_outcomes were created EMPTY in P1 (migration 0004, §4.7) for
--     exactly this. P7 FILLS them and adds the columns P7 owns (calibration_class, ingest_error,
--     the (message_id,event) dedup key) — the P1 sketch's structural fields are untouched.
--   * ZERO new verdict vocabulary (Door 4): the historical-bounce gate uses the P1-RESERVED
--     reason='historical_bounce' (vocab.go, byte-unchanged since Phase 0). calibration_class is a
--     P7-INTERNAL taxonomy (invalid-signal|valid-signal|zero-information) — it is NOT a status/reason/
--     recommendation and never reaches the /verify response (P7 §4.1). It classifies what an outcome
--     COUNTS AS when calibrating; it mints no user-facing value.
--   * Hashing (Door 3) unchanged — the join runs on verifications.id (primary) + email_hash (rollup),
--     both already locked; P7 adds no new hashing. send_outcomes stores email_hash (rollup) + domain;
--     the raw provider event lives in the existing `raw` jsonb.
--   * Door 5 untouched — this is entirely brain-side; the gem/box never learn an outcome came back.
--
-- Recommend-only is nearly free here (shadow-mode §2.1): a recalibration version is written but only
-- MARKED EFFECTIVE (effective_from set, superseding the prior) when APPROVED. An unapproved version is
-- computed + stored + alertable, but the verdict path never reads it → zero live mutation at default
-- mode (the fresh-system invariant, DR-2 / pre-takeoff §3.5).

-- =============================================================================
-- send_outcomes — P7 activation: the columns P7 owns (P1 created the table empty, 0004)
-- =============================================================================
-- P1 §4.7 shape (verification_id, email_hash, domain, mx_provider, message_id, event, sent_at,
-- occurred_at, raw, …) is untouched. P7 adds the classifier's decision + the integrity marker + the
-- idempotency guard whose HOME P1 deliberately left to P7 (PN-14a).

ALTER TABLE send_outcomes ADD COLUMN IF NOT EXISTS calibration_class text;
ALTER TABLE send_outcomes ADD COLUMN IF NOT EXISTS ingest_error text;
ALTER TABLE send_outcomes ADD COLUMN IF NOT EXISTS ingested_at timestamptz;
-- external_verified provenance: when the joined verdict was a rented (P6) verdict, the outcome is
-- bucketed SEPARATELY (P7 §4.4.3/§5.2, GAP-54 — never merged into SMTP-earned buckets). These are
-- backfilled from the joined verifications row at ingest so weaning can measure the vendor's grade.
ALTER TABLE send_outcomes ADD COLUMN IF NOT EXISTS is_external_verified boolean NOT NULL DEFAULT false;
ALTER TABLE send_outcomes ADD COLUMN IF NOT EXISTS residue_class text;
ALTER TABLE send_outcomes ADD COLUMN IF NOT EXISTS vendor text;
ALTER TABLE send_outcomes ADD COLUMN IF NOT EXISTS from_cache boolean NOT NULL DEFAULT false;

-- calibration_class is the P7 §4.1 three-class taxonomy (or NULL until classified). CHECK keeps it a
-- closed internal set — NOT the frozen verdict vocab. 'excluded' covers the §4.4 disciplines
-- (from_cache/override) that are recorded-but-not-calibrated; ingest_error rows carry NULL class.
ALTER TABLE send_outcomes DROP CONSTRAINT IF EXISTS send_outcome_calibration_class;
ALTER TABLE send_outcomes ADD CONSTRAINT send_outcome_calibration_class
    CHECK (calibration_class IS NULL OR calibration_class IN
        ('invalid_signal', 'valid_signal', 'zero_information', 'excluded'));

-- PN-14a RESOLVED (P7): the (message_id, event) dedup guard's HOME is a DB partial UNIQUE index.
-- A replayed webhook delivery (ESPs retry) is then a no-op INSERT (ON CONFLICT DO NOTHING) — the
-- exactly-once INGESTION discipline (§3.4), the input-side complement of the recalibration CAS
-- (§5.5, the output side). Partial (WHERE both NOT NULL) so a legitimately-null pair never collides.
CREATE UNIQUE INDEX IF NOT EXISTS uq_send_outcomes_message_event
    ON send_outcomes (message_id, event)
    WHERE message_id IS NOT NULL AND event IS NOT NULL;

COMMENT ON COLUMN send_outcomes.calibration_class IS 'P7 §4.1 classifier decision: invalid_signal (mailbox_not_found — the gold negative) | valid_signal (delivered / mailbox_full — existence proof) | zero_information (blocked/policy/greylist — about the SENDER, excluded from calibration) | excluded (from_cache/override — recorded, not calibrated). P7-INTERNAL taxonomy, NOT verdict vocab (never in /verify).';
COMMENT ON COLUMN send_outcomes.ingest_error IS 'P7 §3.4/GAP-51 reject-and-flag marker: unknown_verification_id | sent_before_verdict | malformed | unauthenticated. A flagged row is stored raw + EXCLUDED from calibration (a poisoned join is worse than a missing one). NULL => clean.';
COMMENT ON COLUMN send_outcomes.is_external_verified IS 'P7 §4.4.3/§5.2 (GAP-54): the joined verdict was reason=external_verified (a rented P6 verdict). Such outcomes bucket SEPARATELY (by vendor × residue_class × provider), never merged into SMTP-earned buckets — else the flywheel trains on the vendor''s errors and can never wean (§9).';

-- =============================================================================
-- calibration — the versioned, effective-dated confidence model (P7 §5.4 / PN-14 sketch)
-- =============================================================================
-- The bucket is (reason × mx_provider), with a PARALLEL, SEPARATE bucket space for external-verified
-- verdicts keyed by (vendor, residue_class, mx_provider) — never merged (§5.2, GAP-54). Each
-- recalibration cycle writes a NEW versioned row per bucket (append-friendly, effective-dated), so the
-- confidence a verdict used is reconstructable and a bad cycle is REVERTIBLE (§6.4 — the mechanism the
-- cascade brake's rollback relies on). The verdict path reads the CURRENT effective row for a bucket;
-- a bucket with no effective row (cold-start) falls back to the P1 baseline table (§5.6).

CREATE TABLE IF NOT EXISTS calibration (
    id uuid PRIMARY KEY,                    -- App-generated UUIDv7 (time-ordered)

    -- The bucket key (PN-14 sketch). reason + mx_provider is the base dimension (§5.2). vendor +
    -- residue_class are NULL for a SMTP-earned bucket, NON-NULL for the parallel external bucket space
    -- (§5.2/GAP-54) — so the two populations are structurally impossible to merge in a GROUP BY.
    reason text NOT NULL,                   -- the frozen Door-4 reason this bucket calibrates (never minted here)
    mx_provider text NOT NULL,              -- the P1-promoted grouping dimension (§5.2); '' => provider-agnostic
    vendor text,                            -- NULL => SMTP-earned bucket; non-NULL => rented (P6) bucket
    residue_class text,                     -- NULL => SMTP-earned; non-NULL => the P6 residue class (external bucket)

    confidence smallint NOT NULL,           -- the recalibrated confidence (0..100 — Door-1 value range, unchanged)
    sample_n int NOT NULL,                  -- qualifying outcomes behind this version (the §6.2 sample-gate evidence)

    version int NOT NULL,                   -- monotonic per bucket; a new cycle writes version+1
    -- Effective-dating: a version is EFFECTIVE only when effective_from is set AND effective_to is NULL.
    -- Recommend-only (DR-2) writes effective_from=NULL (computed, alertable, NOT read by verdicts) until
    -- APPROVED; approval stamps effective_from (and effective_to on the prior version). A bad cycle is
    -- rolled back by clearing its effective_from and re-effecting the prior (§6.4). This is why
    -- recommend-only is "nearly free" (shadow-mode §2.1): an unapproved version simply isn't effective.
    mode text NOT NULL,                     -- 'shadow' | 'recommend_only' | 'auto' — the mechanism mode at write time
    approved boolean NOT NULL DEFAULT false,-- a human (or auto, when promoted) approved this version to be effective
    effective_from timestamptz,             -- NULL => not effective (proposal only); set on approval
    effective_to timestamptz,               -- NULL => still effective (when effective_from set); set when superseded
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT calibration_confidence_range CHECK (confidence >= 0 AND confidence <= 100),
    CONSTRAINT calibration_sample_nonneg CHECK (sample_n >= 0),
    CONSTRAINT calibration_mode CHECK (mode IN ('shadow', 'recommend_only', 'auto')),
    -- external bucket well-formedness: vendor and residue_class are BOTH null (SMTP) or BOTH set
    -- (rented) — never half-specified, so the two bucket spaces stay cleanly separable (§5.2/GAP-54).
    CONSTRAINT calibration_external_pair CHECK ((vendor IS NULL) = (residue_class IS NULL))
);

COMMENT ON TABLE calibration IS 'P7 §5.4 versioned confidence store — the SEPARATE model the verdict path reads (NEVER verifications, Door 2 intact). Bucket = (reason × mx_provider); external-verified verdicts get a PARALLEL bucket space keyed by (vendor, residue_class, mx_provider), never merged (§5.2/GAP-54). Versioned + effective-dated so a bad cycle is revertible (§6.4) and recommend-only is near-free (an unapproved version isn''t effective — shadow-mode §2.1). Cold-start: no effective row => the verdict path uses the P1 baseline (§5.6).';
COMMENT ON COLUMN calibration.effective_from IS 'NULL => a PROPOSAL (computed + alertable, NOT read by verdicts) — the recommend-only default (DR-2). Set on approval; the verdict path reads only rows with effective_from set AND effective_to NULL. Clearing it rolls a version back (§6.4).';
COMMENT ON COLUMN calibration.vendor IS 'NULL => a SMTP-earned bucket. Non-NULL => a rented (P6 external_verified) bucket, measured SEPARATELY so weaning (§9) can compare own-accuracy vs the vendor''s without training on the vendor''s errors (GAP-54). Paired with residue_class (both-null or both-set — CHECK).';

-- The verdict-path read: the CURRENT effective confidence for a bucket (hot path — one row).
CREATE UNIQUE INDEX IF NOT EXISTS uq_calibration_effective
    ON calibration (reason, mx_provider, COALESCE(vendor, ''), COALESCE(residue_class, ''))
    WHERE effective_from IS NOT NULL AND effective_to IS NULL;
-- The version history + the latest-proposal read per bucket (recalibration + rollback).
CREATE INDEX IF NOT EXISTS idx_calibration_bucket_version
    ON calibration (reason, mx_provider, COALESCE(vendor, ''), COALESCE(residue_class, ''), version DESC);

-- =============================================================================
-- recalibration_runs — the exactly-once recalibration writer's lease/CAS record (P7 §5.5, GAP-53)
-- =============================================================================
-- Recalibration is a NEW lease-and-settle path (like the P5 drainer / P6 spend): a cycle leases a
-- window of outcomes, computes bucket updates, and commits them under a COMPARE-AND-SET on the
-- lease-holder. The [[truemail-lease-cas-exactly-once-settlement]] invariant applies: the CAS, not the
-- lease, is the guarantee. A cycle that overran its lease (slow, not dead) matches ZERO rows on commit
-- and DISCARDS its bucket writes rather than double-applying on top of the cycle that reclaimed it.
-- Single transactional store (Postgres), no broker — the same reasoning as P5's queue, transferring a
-- THIRD time (settle-once → spend-once → recalibrate-once). Ingestion dedup (send_outcomes UNIQUE)
-- prevents the INPUT double-count; this CAS prevents the OUTPUT double-apply.

CREATE TABLE IF NOT EXISTS recalibration_runs (
    id uuid PRIMARY KEY,                    -- App-generated UUIDv7 — the lease unit
    status text NOT NULL,                   -- leased | committed | discarded (lost-lease) | failed
    leased_by text NOT NULL,                -- the worker that holds the lease (the CAS principal)
    leased_at timestamptz NOT NULL,
    window_from timestamptz NOT NULL,       -- the outcome window this cycle recalibrated over
    window_to timestamptz NOT NULL,
    buckets_touched int NOT NULL DEFAULT 0, -- how many buckets this cycle would move (cascade-brake DENOMINATOR, §6.4)
    buckets_at_max int NOT NULL DEFAULT 0,   -- how many hit the max_step ceiling (cascade-brake NUMERATOR, §6.4) — persisted so the ROLLING window (not a single snapshot) can sum across cycles and catch STAGGERED drift (the P4 lesson-a)
    brake_held boolean NOT NULL DEFAULT false, -- true => the cascade brake held this cycle for human confirm (§6.4)
    committed_at timestamptz,

    CONSTRAINT recalibration_run_status CHECK (status IN ('leased', 'committed', 'discarded', 'failed'))
);

COMMENT ON TABLE recalibration_runs IS 'P7 §5.5/GAP-53: the recalibration writer''s lease/CAS record. The CAS-on-lease-holder (not the lease) makes recalibration exactly-once — a slow cycle whose lease was reclaimed matches zero rows and discards its bucket writes (the P5 settle-once / P6 spend-once invariant, a third transfer). brake_held records a §6.4 cascade-brake hold (systemic swing → alert, never auto-apply).';

-- The active-lease + stale-lease-reclaim reads.
CREATE INDEX IF NOT EXISTS idx_recalibration_runs_status ON recalibration_runs (status, leased_at);

-- =============================================================================
-- outcome_ingest_cursor — the durable pull watermark (ADR-0013 §B.2, mutually-authenticated pull)
-- =============================================================================
-- P7 pulls the sender-owned authenticated feed on a cadence and tracks the last-consumed cursor per
-- source, so a pull resumes without re-reading and a replay is a no-op (belt-and-suspenders with the
-- send_outcomes (message_id,event) UNIQUE). One row per feed source (a source key identifies the
-- authenticated principal / feed). The cursor is opaque (the sender defines its ordering).

CREATE TABLE IF NOT EXISTS outcome_ingest_cursor (
    source_key text PRIMARY KEY,            -- identifies the authenticated feed source (the mTLS principal)
    cursor text NOT NULL,                   -- last-consumed opaque cursor/watermark
    updated_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE outcome_ingest_cursor IS 'ADR-0013 §B.2: the durable pull watermark for the mutually-authenticated (mTLS) outcome feed. P7 resumes a pull from the last-consumed cursor per authenticated source — replay-safe by construction, complementing the send_outcomes (message_id,event) dedup. Empty until a real sender feed is wired (PENDING-REAL-SENDER); the local build uses a SimulatedSource.';

-- =============================================================================
-- Grants
-- =============================================================================
-- send_outcomes / verification_outcomes: cp_runtime already has SELECT+INSERT (migration 0005). P7
-- ingestion sets the classifier fields (calibration_class/ingest_error/rollup dims) AT INSERT — the
-- join classifies the outcome BEFORE the single INSERT … ON CONFLICT DO NOTHING (AppendSendOutcome),
-- so there is NO post-hoc UPDATE path. cp_runtime therefore gets SELECT+INSERT only; UPDATE AND DELETE
-- are BOTH revoked (an outcome, once classified + written, is history — the append-only-in-spirit
-- posture the flywheel + the historical-bounce rollup depend on; a runtime UPDATE could flip
-- calibration_class or clear ingest_error to fabricate/un-flag calibration data). A genuine re-
-- classification, if ever needed, is a narrow cp_admin operation (DR-3 break-glass). verifications'
-- grants are untouched (Door 2 stays INSERT-only there too).
REVOKE UPDATE, DELETE ON send_outcomes FROM cp_runtime;

-- calibration: cp_runtime reads (verdict path) + inserts new versions + updates effective_from/to on
-- approval/supersede/rollback (the versioned model IS mutable state, like cache/rate_budgets — NOT the
-- LOG). DELETE stays cp_admin (a version is history; rollback clears effective_from, never deletes).
GRANT SELECT, INSERT, UPDATE ON calibration TO cp_runtime;
REVOKE DELETE ON calibration FROM cp_runtime;
GRANT ALL PRIVILEGES ON calibration TO cp_admin;

-- recalibration_runs: cp_runtime inserts a lease + CAS-updates its status (settle/discard). DELETE
-- cp_admin. Mirrors the work_items lease posture (SELECT+INSERT+UPDATE, no runtime DELETE).
GRANT SELECT, INSERT, UPDATE ON recalibration_runs TO cp_runtime;
REVOKE DELETE ON recalibration_runs FROM cp_runtime;
GRANT ALL PRIVILEGES ON recalibration_runs TO cp_admin;

-- outcome_ingest_cursor: cp_runtime reads + upserts the watermark (mutable operational state).
GRANT SELECT, INSERT, UPDATE ON outcome_ingest_cursor TO cp_runtime;
REVOKE DELETE ON outcome_ingest_cursor FROM cp_runtime;
GRANT ALL PRIVILEGES ON outcome_ingest_cursor TO cp_admin;
