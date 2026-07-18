-- Migration 0011: external_calls — the Phase-6 third-party-fallback call log (spend + provenance).
-- Implements: Phase-6 spec §6.1 (the external-call transcript sidecar), §6.2 (the provenance the P7
--             flywheel weans on), §5 (the durable per-vendor spend the budget/breaker reads),
--             §5.4/GAP-42 (spend-once — a queued escalation's row is inserted INSIDE the settlement
--             CAS tx, so a lost-lease attempt's charge rolls back with its discarded settlement),
--             PN-10 (queryable by email_hash for SAR/deletion).
--
-- WHY THIS IS NOT A FROZEN-DOOR CHANGE (additive-only against all five doors — P6 §10):
--   * external_calls is a NEW operational table (the P5 work_item_attempts pattern). It touches NO
--     frozen schema: verifications (the append-only LOG, Door 2) is unchanged — a fallback verdict is
--     ONE ordinary verifications row tagged reason='external_verified' (a value RESERVED in P1, Door 4:
--     ZERO new status/reason/recommendation). Hashing (Door 3) unchanged — this table stores only
--     email_hash, NEVER plaintext (the address egresses to the vendor transiently, reusing the P5
--     queue plaintext, purged on settle; nothing plaintext is persisted by P6, §7.3).
--   * APPEND-ONLY, mirroring work_item_attempts / node_audits / the LOG: cp_runtime gets SELECT+INSERT
--     only; UPDATE/DELETE are revoked (a spend record, once written, is history — the flywheel + the
--     SAR audit both depend on it). DELETE (retention) stays cp_admin (DR-3 break-glass).
--   * The per-vendor budget/breaker (§5) reads windowed SUM(cost_units)/COUNT off this table — durable,
--     so a brain restart cannot reset the day's spend and sprint at the vendor (the MONEY analogue of
--     the P4 restart-safe rate budget). It is a DISTINCT control from the P4 IP-reputation limiter
--     (GAP-44): different resource, different table, never conflated.

CREATE TABLE IF NOT EXISTS external_calls (
    id uuid PRIMARY KEY,                    -- App-generated UUIDv7 (time-ordered)

    -- FK-less pointers (must survive retention purges of the pointed-to rows). NULL where N/A.
    verification_id uuid,                   -- the settled verdict this call produced (join the rented population); NULL if none
    work_item_id uuid,                      -- the queue item (queued escalation); NULL for a sync /verify call
    job_id uuid,                            -- the owning bulk job (per_job budget, §5.2); NULL for a singular/sync check

    vendor text NOT NULL,                   -- which adapter answered — the per-vendor budget/breaker key (§5.3)
    residue_class text,                     -- which of the 4 §4.1 classes triggered escalation (§6.2, P7 grouping)

    -- Door-3 identity ONLY — never plaintext. Nullable so a normalize-failure path (shouldn't reach
    -- here) can't wedge the insert; in practice every escalated address has a hash.
    email_hash bytea,                       -- for SAR/deletion queryability (PN-10) + P7 join

    vendor_status_raw text,                 -- the vendor's own status string, verbatim (§6.2 — P7 measures their grade)
    vendor_confidence_raw int,              -- the vendor's own confidence/score if any (§6.2)
    request_id text,                        -- vendor call id — idempotency + audit trace to a billed call (§5.4)
    cost_units int NOT NULL DEFAULT 0,      -- what this call cost (spend accounting §5); 0 for an errored call
    outcome text NOT NULL,                  -- 'ok' | 'error' — feeds the error-rate breaker condition (§5.3)
    latency_ms int,                         -- vendor round-trip (§6.2)
    called_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT external_call_outcome CHECK (outcome IN ('ok', 'error')),
    CONSTRAINT external_call_cost_nonneg CHECK (cost_units >= 0)
);

COMMENT ON TABLE external_calls IS 'Phase-6 third-party-fallback call log (P6 §6.1): the spend record AND the P7 weaning provenance. Append-only (mirrors work_item_attempts). Stores email_hash ONLY, never plaintext (§7.3). A queued escalation''s row is inserted INSIDE the settlement CAS tx (spend-once, §5.4/GAP-42) — a lost-lease attempt''s charge rolls back with its discarded settlement. The per-vendor budget/breaker (§5) reads windowed cost_units off this table (durable; a restart can''t reset the day''s spend). Distinct from the P4 rate limiter (GAP-44). cp_runtime: SELECT+INSERT only; DELETE stays cp_admin (retention/break-glass, DR-3).';
COMMENT ON COLUMN external_calls.cost_units IS 'Cost of this vendor call (§5/§6.2). Summed (ok calls only) over rolling windows to drive the spend breaker (§5.3). 0 for an errored call (no verdict produced, no charge).';
COMMENT ON COLUMN external_calls.outcome IS '''ok'' (a verdict returned) | ''error'' (vendor-down/timeout/rate-limited). All calls count toward the error-rate breaker denominator; only ok calls count toward spend (§5.3).';
COMMENT ON COLUMN external_calls.email_hash IS 'Door-3 identity ONLY (norm/v1+hmac-sha256/v1). NEVER plaintext (the address egresses transiently to the vendor, reusing the P5 queue plaintext, purged on settle — §7.3). Queryable for SAR/deletion (PN-10): "which addresses (by hash) were sent to which vendor, when".';

-- The per-vendor windowed spend + error-rate reads (§5.2/§5.3): vendor + called_at is the hot path.
CREATE INDEX IF NOT EXISTS idx_external_calls_vendor_time ON external_calls (vendor, called_at);
-- The per-job spend read (per_job_cap, §5.2/§4.4).
CREATE INDEX IF NOT EXISTS idx_external_calls_job ON external_calls (job_id) WHERE job_id IS NOT NULL;
-- SAR/deletion + P7 join by address identity (PN-10, §6.2).
CREATE INDEX IF NOT EXISTS idx_external_calls_email_hash ON external_calls (email_hash) WHERE email_hash IS NOT NULL;

-- =============================================================================
-- Grants — APPEND-ONLY (SELECT+INSERT), mirroring work_item_attempts / node_audits / the LOG.
-- =============================================================================
GRANT SELECT, INSERT ON external_calls TO cp_runtime;
REVOKE UPDATE, DELETE ON external_calls FROM cp_runtime;
GRANT ALL PRIVILEGES ON external_calls TO cp_admin;
