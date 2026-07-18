-- Migration 0009: work_items (durable async queue) + jobs (bulk handles) + work_item_attempts
-- Implements: Phase-5 spec §3.3 (queue), §7.2 (jobs), §5 (idempotency/GAP-37/33), §7.3 (GAP-38),
--             OQ-P5-8 (per-attempt transcripts), OQ-P5-9 + DR-4/ADR-0002 (plaintext-at-rest).
--
-- WHY THESE ARE NOT FROZEN-DOOR CHANGES (additive-only against all five doors — P5 §10):
--   * work_items / jobs / work_item_attempts are NEW operational tables. They touch NO frozen
--     schema: verifications (the append-only LOG, Door 2) is unchanged; cache + hashing (Door 3)
--     unchanged; the verdict vocabulary (Door 4) unchanged — a retry-exhausted item settles as the
--     already-reserved unknown/greylisted (NO new status/reason/recommendation). The queue is a
--     scheduler in front of the UNCHANGED P4 router (§8 master invariant): it changes WHEN a check
--     runs, never WHETHER the safeguards apply.
--   * work_items holds the plaintext address ONLY for work-not-yet-done (§3.3, OQ-P5-9) — the ONE
--     deliberate, bounded exception to Door 3's hash-everything discipline, because SMTP
--     structurally needs the real address and cannot run from a hash. It is bounded TWO ways:
--     (1) purged transactionally on settle (addr_ciphertext=NULL as step (4) of the settlement CAS
--     tx — never a later sweep, so a crash can't strand it), and (2) ENCRYPTED at rest (DR-4:
--     AES-256-GCM under a key SEPARATE from the pepper — internal/queue/addrcipher.go), so a DB
--     backup/PITR snapshot the purge never reaches holds only ciphertext (closes the ADR-0002 gap).
--   * The idempotency_key is derived from email_hash + policy_key + policy_version +
--     config_fingerprint (§5.1) — the SAME composite the cache keys on — reusing the Door-3
--     artifact, so NO new hashing and NO new pepper exposure.

-- =============================================================================
-- work_items — the durable async queue (P5 §3.3, §5.2/GAP-37 idempotency)
-- =============================================================================
-- One row per pending verification work-item. Lifecycle (§3.5):
--   ready -> in_flight -> {settled | waiting_retry -> ready | dead}
-- GAP-37 (the correctness heart): the drainer LEASES a ready item (locked_by/locked_at) via
-- FOR UPDATE SKIP LOCKED, and EVERY settle/requeue is a conditional compare-and-set on
-- (id, locked_by=<me>, status='in_flight'). A lost/expired lease matches ZERO rows and discards
-- its result. The lease TTL must EXCEED the brain's max SMTP deadline so a *live* attempt never
-- loses its lease (a reclaim then provably means dead). The CAS, not the lease, makes settlement
-- exactly-once (at-least-once attempts, exactly-once settlement).

CREATE TABLE IF NOT EXISTS work_items (
    id uuid PRIMARY KEY,                 -- App-generated UUIDv7 (time-ordered; also the AEAD AAD)

    -- Dedup / settle-once key (§5.1) = sha256(email_hash || policy_key || policy_version ||
    -- config_fingerprint). UNIQUE so enqueue is atomic dedup (ON CONFLICT DO NOTHING). Never raw email.
    idempotency_key bytea NOT NULL UNIQUE,

    job_id uuid,                         -- owning bulk job (§7.2); NULL for a singular queued check
    domain text NOT NULL,                -- in-clear (Door 3) — group-by-domain ordering + P4 sticky key
    email_hash bytea NOT NULL,           -- Door-3 identity for cache lookup + LOG join
    policy_key text NOT NULL,            -- part of the idempotency key
    policy_version int NOT NULL,         -- part of the idempotency key
    config_fingerprint text NOT NULL,    -- part of the idempotency key (cache semantics mirrored)

    status text NOT NULL DEFAULT 'ready',
    attempt int NOT NULL DEFAULT 0,      -- retry attempt count (0-based); drives backoff + max_attempts
    not_before timestamptz NOT NULL DEFAULT now(), -- earliest dequeue time (backoff due-time, §4.2)

    -- Drainer lease (§5.2/GAP-37). The settlement CAS is WHERE locked_by=<me> AND status='in_flight'.
    locked_by text,
    locked_at timestamptz,

    last_reason text,                    -- last attempt's reason (greylisted|rate_limited|...) — operational
    verification_id uuid,                -- FK-less; set atomically at settle (GAP-33). A re-attempt finding it set is a no-op.

    -- DR-4 (ADR-0002): the plaintext address, AES-256-GCM ciphertext at rest (nonce||ct||tag; key
    -- SEPARATE from the pepper; AAD = id). NULLed as step (4) of the settlement CAS tx (§5.3) so a
    -- crash never strands plaintext on a terminal row. The one bounded Door-3 exception, encrypted.
    addr_ciphertext bytea,

    created_at timestamptz NOT NULL DEFAULT now(),
    settled_at timestamptz,

    CONSTRAINT valid_work_item_status
        CHECK (status IN ('ready', 'in_flight', 'waiting_retry', 'settled', 'dead')),
    CONSTRAINT work_item_attempt_nonneg CHECK (attempt >= 0)
);

COMMENT ON TABLE work_items IS 'Durable async queue (P5 §3.3). One row per verification work-item; lifecycle ready->in_flight->{settled|waiting_retry|dead} (§3.5). GAP-37: settle/requeue is a conditional CAS on (id, locked_by=<me>, status=''in_flight''); a lost lease matches zero rows and discards. GAP-33: verification_id stamped in the SAME tx as the LOG insert; a re-attempt finding it set is a no-op (Door-2 append-only preserved). addr_ciphertext holds the plaintext ENCRYPTED (DR-4, key != pepper) ONLY for work-not-yet-done, NULLed in the settlement tx (§5.3 step 4). Drained through the UNCHANGED P4 router (§8). cp_runtime: SELECT+INSERT+UPDATE; DELETE stays cp_admin (retention purge is admin).';
COMMENT ON COLUMN work_items.idempotency_key IS 'Dedup key (§5.1) = sha256(email_hash || policy_key || policy_version || config_fingerprint). Reuses the Door-3 artifact (email_hash); no new pepper exposure. UNIQUE => enqueue ON CONFLICT DO NOTHING is atomic dedup (§5.5).';
COMMENT ON COLUMN work_items.addr_ciphertext IS 'Plaintext address encrypted at rest (DR-4/ADR-0002: AES-256-GCM, key SEPARATE from pepper, AAD=id). Held ONLY for work-not-yet-done; NULLed as step (4) of the settlement CAS tx (§5.3/OQ-P5-9) so a crash never strands it. The one bounded Door-3 exception, now backup-safe (a snapshot holds only ciphertext).';
COMMENT ON COLUMN work_items.locked_by IS 'Drainer lease-holder (§5.2/GAP-37). Settlement CAS: WHERE locked_by=<me> AND status=''in_flight''. Lease TTL must exceed the brain max SMTP deadline so a live attempt never loses its lease (a reclaim then means genuinely dead).';
COMMENT ON COLUMN work_items.verification_id IS 'FK-less pointer to the settled verifications row (§5.3/GAP-33). Set atomically in the settlement tx; a re-attempt finding it set is a no-op (no duplicate LOG row). Append-only (Door 2) preserved.';

-- Dequeue (§3.1/§6.2): ready|waiting_retry AND now>=not_before, group-by-domain spread. Partial
-- index excludes terminal rows so the hot path scans only pending work.
CREATE INDEX IF NOT EXISTS idx_work_items_dequeue ON work_items (domain, not_before)
    WHERE status IN ('ready', 'waiting_retry');

-- Authoritative job-completion query (§7.3/GAP-38): NOT EXISTS(... status NOT IN (settled,dead)).
CREATE INDEX IF NOT EXISTS idx_work_items_job_status ON work_items (job_id, status)
    WHERE job_id IS NOT NULL;

-- Lease-reclaim sweep (§5.2): find in_flight items whose lease is older than the TTL.
CREATE INDEX IF NOT EXISTS idx_work_items_reclaim ON work_items (locked_at)
    WHERE status = 'in_flight';

-- =============================================================================
-- jobs — bulk job handles (P5 §7.2)
-- =============================================================================
-- The BRAIN's notion of a bulk verification job (NOT the platform's user-facing job — §7.6/GAP-36).
-- Holds verification-work state ONLY: counts, status, policy, and an OPAQUE caller_ref the brain
-- never interprets. NO user id, credits, CSV, filename, or per-row UI state.

CREATE TABLE IF NOT EXISTS jobs (
    id uuid PRIMARY KEY,                 -- the handle returned to the caller (UUIDv7)
    policy_key text NOT NULL,
    policy_version int NOT NULL,
    status text NOT NULL DEFAULT 'submitted',
    total_count int NOT NULL,            -- items in the job (post-dedup)
    -- Denormalized progress counter (§7.3/GAP-38): incremented in the settlement CAS tx so it moves
    -- iff the item truly settled. Used ONLY for cheap progress display — completion is judged
    -- AUTHORITATIVELY by NOT EXISTS(work_items WHERE job_id=? AND status NOT IN (settled,dead)),
    -- never by settled_count == total_count (the counter can drift under the reclaim/no-op path).
    settled_count int NOT NULL DEFAULT 0,
    submitted_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    caller_ref text,                     -- OPAQUE caller correlation (§7.6); echoed, never interpreted

    CONSTRAINT valid_job_status
        CHECK (status IN ('submitted', 'running', 'complete', 'complete_with_unknowns')),
    CONSTRAINT job_total_nonneg CHECK (total_count >= 0),
    CONSTRAINT job_settled_nonneg CHECK (settled_count >= 0)
);

COMMENT ON TABLE jobs IS 'Bulk job handles (P5 §7.2) — the BRAIN''s verification-work scheduler, NOT the platform''s user-facing job (§7.6/GAP-36). Holds counts/status/policy + an OPAQUE caller_ref only; NO user id, credits, CSV, filename. Completion is authoritative (NOT EXISTS work_items NOT IN (settled,dead)); settled_count is cheap progress only (GAP-38). cp_runtime: SELECT+INSERT+UPDATE; DELETE stays cp_admin.';
COMMENT ON COLUMN jobs.settled_count IS 'Denormalized progress counter (§7.3/GAP-38). Incremented in the settlement CAS tx (atomic with work_items.verification_id). Cheap display ONLY — completion is authoritative via NOT EXISTS, never this counter (which can drift under reclaim/no-op).';
COMMENT ON COLUMN jobs.caller_ref IS 'OPAQUE caller correlation string (§7.6/GAP-36). The brain never interprets it — echoed back so the platform correlates its job to ours. The ONLY platform coupling. Distinct from the key-derived, trusted caller_id (api_callers). No user PII.';

-- =============================================================================
-- work_item_attempts — per-attempt transcripts (P5 OQ-P5-8)
-- =============================================================================
-- Append-only operational table capturing EVERY attempt (including intermediate greylisted tries),
-- distinct from verifications (the settled-verdict LOG). Feeds debugging + the P7 flywheel
-- ("did retrying change the outcome?", "which providers greylist us most?"). NOT a verdict.
-- Any SMTP transcript captured here must reuse the Door-3 address redaction (ADR-0008) before persist.

CREATE TABLE IF NOT EXISTS work_item_attempts (
    id uuid PRIMARY KEY,                 -- UUIDv7
    work_item_id uuid NOT NULL,          -- FK-less (must survive work_item retention purge)
    attempt int NOT NULL,
    attempted_at timestamptz NOT NULL DEFAULT now(),
    node_id text,                        -- which node ran this attempt (mirrors verifications.node_id)
    verifier_ip inet,                    -- the node's egress IP (mirrors verifications.verifier_ip)
    reason text,                         -- this attempt's outcome reason (NOT the settled verdict)
    signals jsonb,                       -- this attempt's signals (transcript already Door-3-redacted)
    latency_ms int,

    CONSTRAINT work_item_attempt_num_nonneg CHECK (attempt >= 0)
);

COMMENT ON TABLE work_item_attempts IS 'Per-SMTP-attempt transcripts (P5 OQ-P5-8). Append-only; distinct from verifications (the settled-verdict LOG). Captures EVERY attempt (intermediate greylisted tries included) for debugging + P7. Any transcript is Door-3 address-redacted (ADR-0008) before persist. cp_runtime: SELECT+INSERT only (append-only, mirrors node_audits/Door-2 posture); DELETE stays cp_admin.';

CREATE INDEX IF NOT EXISTS idx_work_item_attempts_item ON work_item_attempts (work_item_id, attempt);

-- =============================================================================
-- Grants — cp_runtime drains (SELECT+INSERT+UPDATE); DELETE stays cp_admin (retention/break-glass)
-- =============================================================================

-- work_items: the drainer enqueues, leases, settles, requeues -> SELECT+INSERT+UPDATE. DELETE is a
-- retention purge of terminal rows (admin action), mirroring the "runtime never destroys" posture.
GRANT SELECT, INSERT, UPDATE ON work_items TO cp_runtime;
REVOKE DELETE ON work_items FROM cp_runtime;
GRANT ALL PRIVILEGES ON work_items TO cp_admin;

-- jobs: submit, progress, complete -> SELECT+INSERT+UPDATE; DELETE admin-only (retention).
GRANT SELECT, INSERT, UPDATE ON jobs TO cp_runtime;
REVOKE DELETE ON jobs FROM cp_runtime;
GRANT ALL PRIVILEGES ON jobs TO cp_admin;

-- work_item_attempts: APPEND-ONLY (INSERT+SELECT, no UPDATE/DELETE) — mirrors node_audits / the LOG.
GRANT SELECT, INSERT ON work_item_attempts TO cp_runtime;
REVOKE UPDATE, DELETE ON work_item_attempts FROM cp_runtime;
GRANT ALL PRIVILEGES ON work_item_attempts TO cp_admin;
