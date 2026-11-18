-- Migration 0019: an idempotency key on bulk jobs (partial Tier 4, owner ruling OD-6/OD-8) and the columns of node self-promotion (owner ruling OD-4).
-- Both are ADDITIVE: nullable columns, one partial unique index, one NULL-safe CHECK. No existing row, column, constraint or privilege changes, and a row
-- that sets none of the new columns reads and behaves exactly as before.
--
-- 1. verify.jobs: idem_caller_id, idempotency_key, request_fingerprint.
--    A caller of POST /v1/verify/bulk may send an `Idempotency-Key` header. The key is scoped to the AUTHENTICATED caller (api_callers.caller_id, derived
--    from the bearer key and never from the body) and is unique per (caller, key): the partial unique index below is what makes two concurrent requests
--    with one key create exactly one job (the second INSERT waits for the first transaction and then finds the key taken). request_fingerprint is a
--    32-byte digest of the canonical request (policy, caller_ref, owner and, per row, the item_id and the KEYED email hash; never an address, never an
--    unkeyed hash of one: Door 3), so a repeat with the same key and another body is told apart from a true repeat without storing anything the job
--    does not already hold. A job with no key has all three columns NULL: every job that exists today, and every job created without the header.
--    The 202 body of the first answer (counts, malformed rows) is reproducible from verify.job_rows, which is append-only, so nothing more is stored.
--
-- 2. verify.nodes: registered_at, promoted_by_audit_id.
--    The control plane re-registers each node as `provisioning` (port25_open false) at every boot. registered_at is the database's clock at that
--    registration. A node is promoted to `active` ONLY by an audit row recorded AFTER it (node_audits.created_at > nodes.registered_at), in one
--    statement that also sets port25_open from that audit and writes the audit row's id to promoted_by_audit_id, so a stale audit (an earlier boot's)
--    can never promote a freshly registered node and a promotion always names the audit that caused it. Both columns are NULL on a node that has never
--    been registered by this code (and registered_at NULL promotes nothing).
--
-- IDEMPOTENT, like every file here: the runner records it once and the old psql loop may apply it twice. Fails loudly, naming the missing piece, if
-- 0009/0016/0018 have not run. OWNERSHIP follows 0018: the migrating role alters the tables, which verify_owner owns (the role is a member of it).

DO $$
BEGIN
    IF to_regclass('verify.jobs') IS NULL OR to_regclass('verify.nodes') IS NULL OR to_regclass('verify.node_audits') IS NULL
       OR to_regclass('verify.job_rows') IS NULL THEN
        RAISE EXCEPTION '0019: verify.jobs, verify.nodes, verify.node_audits and verify.job_rows do not exist; migrations 0009, 0016 and 0018 must run first'
            USING ERRCODE = '55000';
    END IF;
END
$$;

-- 1. jobs -------------------------------------------------------------------------------------------------------------------------------
ALTER TABLE verify.jobs ADD COLUMN IF NOT EXISTS idem_caller_id text;
ALTER TABLE verify.jobs ADD COLUMN IF NOT EXISTS idempotency_key text;
ALTER TABLE verify.jobs ADD COLUMN IF NOT EXISTS request_fingerprint bytea;

-- All three or none, and the key's shape is the API's (1..128 characters of [A-Za-z0-9._:-]). Every branch is a boolean: a CHECK that evaluates to NULL
-- passes, so each nullable column is tested with IS NULL / IS NOT NULL and nothing reads a NULL operand.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'jobs_idempotency_shape' AND conrelid = 'verify.jobs'::regclass) THEN
        ALTER TABLE verify.jobs ADD CONSTRAINT jobs_idempotency_shape CHECK (
            (idempotency_key IS NULL AND idem_caller_id IS NULL AND request_fingerprint IS NULL)
            OR (idempotency_key IS NOT NULL AND idem_caller_id IS NOT NULL AND request_fingerprint IS NOT NULL
                AND idempotency_key ~ '^[A-Za-z0-9._:-]{1,128}$'
                AND octet_length(request_fingerprint) = 32));
    END IF;
END
$$;

-- Unique per (caller, key). Partial, so the (all) keyless jobs take no index entry and never conflict.
CREATE UNIQUE INDEX IF NOT EXISTS uq_jobs_caller_idempotency_key ON verify.jobs (idem_caller_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

COMMENT ON COLUMN verify.jobs.idem_caller_id IS
    'The authenticated caller (api_callers.caller_id) the Idempotency-Key of this job is scoped to (0019). NULL on a job created without the header.';
COMMENT ON COLUMN verify.jobs.idempotency_key IS
    'The caller-supplied Idempotency-Key of POST /v1/verify/bulk (0019): 1..128 characters of [A-Za-z0-9._:-], unique per (idem_caller_id, idempotency_key). A repeat with the same key and the same canonical request returns THIS job and creates nothing; the same key with another request is refused (409). NULL on a job created without the header.';
COMMENT ON COLUMN verify.jobs.request_fingerprint IS
    'sha256 over the canonical request of a keyed job (0019): shape, effective policy, caller_ref, owner and per row the item_id and the KEYED email hash (Door 3: no address and no unkeyed hash of one). Compared on a repeat of the key. NULL on a job created without the header.';

-- 2. nodes ------------------------------------------------------------------------------------------------------------------------------
ALTER TABLE verify.nodes ADD COLUMN IF NOT EXISTS registered_at timestamptz;
ALTER TABLE verify.nodes ADD COLUMN IF NOT EXISTS promoted_by_audit_id uuid;

COMMENT ON COLUMN verify.nodes.registered_at IS
    'The database clock at this control-plane boot''s registration of the node (0019: RegisterNode sets provisioning, port25_open false and this). Only an audit row recorded AFTER it (node_audits.created_at > registered_at) may promote the node. NULL on a node never registered by this code.';
COMMENT ON COLUMN verify.nodes.promoted_by_audit_id IS
    'The node_audits row that promoted the node provisioning -> active (0019), written in the same statement as the status and port25_open (which is taken from that audit). Cleared at every registration. NULL on a node that has not been promoted since its last registration.';

-- 3. Self-check ---------------------------------------------------------------------------------------------------------------------------
DO $$
DECLARE
    missing text;
BEGIN
    SELECT string_agg(c, ', ') INTO missing FROM (VALUES
        ('verify.jobs.idem_caller_id'), ('verify.jobs.idempotency_key'), ('verify.jobs.request_fingerprint'),
        ('verify.nodes.registered_at'), ('verify.nodes.promoted_by_audit_id')) AS want(c)
    WHERE NOT EXISTS (
        SELECT 1 FROM pg_attribute a
        WHERE a.attrelid = (split_part(want.c, '.', 1) || '.' || split_part(want.c, '.', 2))::regclass
          AND a.attname = split_part(want.c, '.', 3) AND NOT a.attisdropped);
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION '0019: columns missing after the migration: %', missing USING ERRCODE = '55000';
    END IF;
    IF to_regclass('verify.uq_jobs_caller_idempotency_key') IS NULL THEN
        RAISE EXCEPTION '0019: the unique index uq_jobs_caller_idempotency_key is missing' USING ERRCODE = '55000';
    END IF;
END
$$;
