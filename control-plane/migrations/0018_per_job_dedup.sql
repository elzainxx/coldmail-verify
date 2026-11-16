-- Migration 0018: per-job dedup and per-row identity for bulk jobs (owner rulings A1 "per-item identity on bulk" and A2 "dedup scope"; platform research
-- record m1-verify-outcomes.txt gaps M1-G1 and M1-G2).
--
-- WHAT WAS WRONG.
--   M1-G2 (dedup was global and permanent): work_items.idempotency_key (0009:40) was UNIQUE across ALL jobs and ALL owners, and the enqueue was
--   `ON CONFLICT (idempotency_key) DO NOTHING`. An address ANY tenant had ever enqueued under the same policy and config therefore yielded NO work item in a
--   later job, and so no result: the second tenant's job came back short, with the missing rows counted as "duplicate". The comment in enqueue.go that said
--   "a still-pending identical check from another job" was contradicted by the constraint (the key stayed unique after the check settled).
--   M1-G1 (no per-item identity): a bulk result carried no address (Door 3: the plaintext is purged on settle) and no row identity, and the results were ordered
--   by a random UUIDv7 work-item id, so a caller could not map a verdict back to the row it submitted.
--
-- THE RULINGS THIS FILE IMPLEMENTS.
--   A2. Identical addresses WITHIN ONE JOB are checked once and every input row of that job gets the result. ACROSS jobs, and across tenants, there is NO
--   dedup: a new job always yields a result for every row (the cache may answer, and a cache answer is still marked flags.from_cache).
--   A1. Every input row of a bulk job is recorded with its 0-based ordinal (the caller's own index in the submitted list) and an optional caller-supplied
--   item_id; a malformed row is recorded too, so every input row has an outcome.
--
-- WHAT IT DOES.
--   1. Creates verify.job_rows, one row per INPUT row of a job: (job_id, ordinal) primary key; item_id (optional, unique per job); work_item_id (the work item that
--      answers the row; several rows of one job may share it, which is how a duplicate is "checked once"); duplicate_of_ordinal (the ordinal of the FIRST row of the
--      job with the same address, set on every later row that shares its work item, so the caller can see which rows are one check); malformed (the address did not
--      normalise: no work item) with verification_id (the row's own invalid_syntax LOG row in verify.verifications, written by the control plane in the SAME
--      transaction that records the job: the sync path logs an un-addressable input as invalid/invalid_syntax with a NULL hash, §3.2.7, and so does bulk).
--   2. Backfills job_rows for every job that existed before this file, one row per work item (see the note on legacy jobs below).
--   3. Replaces the GLOBAL unique constraint on work_items.idempotency_key by uniqueness PER JOB: a partial unique index on (job_id, idempotency_key) for
--      job_id IS NOT NULL, and a partial unique index on (idempotency_key) for job_id IS NULL, so the singular queued check keeps its old semantics.
--   4. Grants verify_app exactly what the code needs on the new table: SELECT and INSERT (the control plane appends rows at submit and reads them to serve
--      results and status; nothing updates or deletes them). verify_admin gets what it has on every other table.
--
-- WHY THIS IS NOT A FROZEN-DOOR CHANGE (additive-only against all five doors):
--   Door 1 is untouched here (the verdict object is not changed; the bulk result ELEMENT gains item_id and ordinal in the API layer, additively).
--   Door 2: not one column of verifications changes and no row is mutated; the control plane only APPENDS (verify_app already holds INSERT). A malformed row's
--   verdict is a real LOG row, logged BEFORE the 202 is returned and committed with the job and its rows, and the result is served FROM it (job_rows.verification_id),
--   so "log every result before returning" holds for bulk as it does for /v1/verify. Door 3: job_rows holds NO address, plaintext or hash, only the caller's own row
--   index and the caller's own item_id (an opaque label the caller chose); the malformed row's LOG row has a NULL email_hash, exactly like the sync path's.
--   Door 4: no vocabulary is minted (a malformed row is reported with the legal reason invalid_syntax). Door 5: nothing to do with the engine. work_items and jobs
--   are operational tables, not the LOG.
--
-- LEGACY JOBS (submitted before this file). Their work items get job_rows with ordinals 0..n-1 in work-item id order (UUIDv7, so submission order), no item_id,
-- no malformed rows (those were never recorded). The caller's real input order and the rows that collapsed as duplicates under the old global rule are
-- information the old code never kept, so jobs.total_count of such a job can still exceed its row count and its pending count never reaches 0, exactly as
-- before. Their results now come back ordered by that synthetic ordinal. The backfill touches ONLY jobs that have no job_rows yet, so replaying this file is a no-op.
--
-- IDEMPOTENT, like every file here: the runner records it once, and the old psql loop (CI, the platform harness) may apply it twice. Every statement is
-- IF NOT EXISTS / IF EXISTS / ON CONFLICT DO NOTHING / CREATE-OR-REPLACE-by-nature; the file fails loudly, naming the missing piece, if 0016 has not run.
-- OWNERSHIP follows 0016/0017: the table is created by the migrating role and handed to verify_owner (which the migrating role can SET ROLE to).

DO $$
BEGIN
    IF to_regclass('verify.work_items') IS NULL
       OR to_regclass('verify.jobs') IS NULL
       OR NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'verify_owner')
       OR NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'verify_app')
       OR NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'verify_admin') THEN
        RAISE EXCEPTION '0018: verify.work_items, verify.jobs and the roles verify_owner / verify_app / verify_admin do not exist; migrations 0009 and 0016 must run first'
            USING ERRCODE = '55000';
    END IF;
END
$$;

-- 1. job_rows ----------------------------------------------------------------------------------------------------------------------------
-- No foreign keys, deliberately: the same posture as work_item_attempts and verifications.node_id (a reference must survive the retention purge of the row it
-- points at; DELETE on work_items is a verify_admin action). A row whose work item is gone simply stops appearing in results.
--
-- The CHECKs say IS NULL / IS NOT NULL on the nullable column and read the NOT NULL boolean directly, so no branch of either can evaluate to NULL (a CHECK that
-- evaluates to NULL PASSES, which is how a constraint stops constraining).
--
-- job_rows_outcome_shape: a MALFORMED row is answered by its own LOG row (verification_id) and has no work item; every other row is answered by a work item and has
-- no verification_id of its own (its verdict is its work item's). job_rows_duplicate_of_shape: duplicate_of_ordinal, when set, is an EARLIER ordinal, and a malformed
-- row is never a duplicate (a repeated malformed address is not "one check": each malformed row is its own row with its own LOG row).
CREATE TABLE IF NOT EXISTS verify.job_rows (
    job_id               uuid    NOT NULL,
    ordinal              integer NOT NULL,
    item_id              text,
    work_item_id         uuid,
    malformed            boolean NOT NULL DEFAULT false,
    verification_id      uuid,
    duplicate_of_ordinal integer,

    CONSTRAINT job_rows_pkey PRIMARY KEY (job_id, ordinal),
    CONSTRAINT job_rows_ordinal_nonneg CHECK (ordinal >= 0),
    CONSTRAINT job_rows_outcome_shape CHECK (
        (malformed AND work_item_id IS NULL AND verification_id IS NOT NULL)
        OR (NOT malformed AND work_item_id IS NOT NULL AND verification_id IS NULL)),
    CONSTRAINT job_rows_item_id_shape CHECK (item_id IS NULL OR octet_length(item_id) BETWEEN 1 AND 200),
    CONSTRAINT job_rows_duplicate_of_shape CHECK (
        duplicate_of_ordinal IS NULL OR (NOT malformed AND duplicate_of_ordinal >= 0 AND duplicate_of_ordinal < ordinal))
);

-- 2. Backfill the jobs that predate this file. Only jobs with no job_rows yet, so a replay inserts nothing.
INSERT INTO verify.job_rows (job_id, ordinal, work_item_id)
SELECT w.job_id, (row_number() OVER (PARTITION BY w.job_id ORDER BY w.id) - 1)::integer, w.id
FROM verify.work_items w
WHERE w.job_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM verify.job_rows jr WHERE jr.job_id = w.job_id)
ON CONFLICT DO NOTHING;

-- item_id is unique within a job (NULL = the row had none, and any number of those may share a job).
CREATE UNIQUE INDEX IF NOT EXISTS uq_job_rows_item_id ON verify.job_rows (job_id, item_id) WHERE item_id IS NOT NULL;
-- The results and status reads join job_rows to work_items by this id, and "which rows does this work item answer" (retention, debugging) goes the other way.
CREATE INDEX IF NOT EXISTS idx_job_rows_work_item ON verify.job_rows (work_item_id);

ALTER TABLE verify.job_rows OWNER TO verify_owner;

-- Privileges: a new table starts with none for PUBLIC (0016 already revoked PUBLIC's USAGE on the schema), so there is nothing to revoke. The control plane only
-- ever appends rows and reads them: SELECT + INSERT, no UPDATE and no DELETE (a row is a fact about the submission, never edited). verify_admin: what it holds
-- on every other table (0009: GRANT ALL PRIVILEGES), for retention.
GRANT SELECT, INSERT ON verify.job_rows TO verify_app;
GRANT ALL PRIVILEGES ON verify.job_rows TO verify_admin;

COMMENT ON TABLE verify.job_rows IS
    'One row per INPUT row of a bulk job (0018; owner rulings A1/A2). (job_id, ordinal): ordinal is the caller''s own 0-based index in the submitted list, counting malformed rows. item_id: the caller''s optional per-row label, unique within the job. work_item_id: the work item that answers the row (rows of one job with the same normalised address share one: checked once). duplicate_of_ordinal: the ordinal of the FIRST row of the job with the same address (set on each later row that shares its work item; NULL on a first row and on every malformed row). malformed: the address did not normalise (norm/v1): no work item, and the row is answered by its own invalid_syntax LOG row (verification_id). Holds NO address and NO hash (Door 3). verify_app: SELECT + INSERT only.';
COMMENT ON COLUMN verify.job_rows.ordinal IS 'The row''s 0-based position in the caller''s submitted list (malformed rows count). The results are ordered by it and the results cursor is derived from it.';
COMMENT ON COLUMN verify.job_rows.item_id IS 'The caller-supplied label for this row (request shape items[].item_id), 1..200 bytes, unique within the job; NULL for the addresses[] shape. Echoed on every result for the row; never interpreted.';
COMMENT ON COLUMN verify.job_rows.work_item_id IS 'FK-less pointer to the work item that answers this row; several rows of one job may share it (identical addresses in one job are checked once). NULL iff malformed.';
COMMENT ON COLUMN verify.job_rows.malformed IS 'true when the address did not normalise (Door 3 norm/v1): no work item exists for the row, and it is answered by its own invalid/invalid_syntax LOG row (verification_id).';
COMMENT ON COLUMN verify.job_rows.verification_id IS 'FK-less pointer to the verify.verifications LOG row of a MALFORMED row (the invalid_syntax verdict with a NULL email_hash that the sync path also writes for an un-addressable input, §3.2.7), appended in the same transaction as the job and its rows so the verdict is logged before it is returned (Door 2). NULL on every other row: their verdict is their work item''s verification_id.';
COMMENT ON COLUMN verify.job_rows.duplicate_of_ordinal IS 'The ordinal of the FIRST row of the job with the same normalised address; set on each later row that shares that row''s work item, so a caller (the platform, which charges a duplicated address once) can see which rows are one check. Always below the row''s own ordinal; NULL on a first row and on every malformed row.';

-- 3. work_items: the idempotency key is unique PER JOB, not globally ------------------------------------------------------------------------
-- The new indexes first, then the old constraint goes. 0009 declared `idempotency_key bytea NOT NULL UNIQUE`, whose automatic name is
-- work_items_idempotency_key_key (table_column_key). The partial index for job_id IS NOT NULL is what `ON CONFLICT (job_id, idempotency_key) WHERE job_id IS NOT
-- NULL` infers; the one for job_id IS NULL keeps a singular queued check (no job) deduplicated by its key alone, as before.
CREATE UNIQUE INDEX IF NOT EXISTS uq_work_items_job_idem ON verify.work_items (job_id, idempotency_key) WHERE job_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_work_items_unjobbed_idem ON verify.work_items (idempotency_key) WHERE job_id IS NULL;

ALTER TABLE verify.work_items DROP CONSTRAINT IF EXISTS work_items_idempotency_key_key;

COMMENT ON COLUMN verify.work_items.idempotency_key IS
    'Dedup key (§5.1) = sha256(email_hash || policy_key || policy_version || config_fingerprint). Reuses the Door-3 artifact (email_hash); no new pepper exposure. UNIQUE PER JOB since 0018 (uq_work_items_job_idem; uq_work_items_unjobbed_idem for a check with no job): identical addresses in one job are checked once, and a different job (any owner) always gets its own work item and result.';
COMMENT ON COLUMN verify.jobs.total_count IS
    'Input rows of the job (0018): every row of the submission, malformed and duplicate rows included, so total = enqueued + duplicate + malformed. Jobs submitted before 0018 hold the old count (rows that normalised, duplicates included).';
COMMENT ON COLUMN verify.jobs.settled_count IS
    'Denormalized progress counter, one per WORK ITEM settled (§7.3/GAP-38), bumped in the settlement CAS tx. Cheap display only and no longer what the status route reports: GET /v1/jobs/{id} counts ROWS (job_rows) whose work item is terminal, plus malformed rows, so pending reaches 0 when the job is complete. Completion is never this counter.';

-- 4. Self-check: refuse to finish if a GLOBAL unique index on idempotency_key survived, or the per-job ones are missing --------------------------
DO $$
DECLARE
    stray text;
BEGIN
    SELECT string_agg(i.indexrelid::regclass::text, ', ') INTO stray
    FROM pg_index i
    WHERE i.indrelid = 'verify.work_items'::regclass
      AND i.indisunique
      AND i.indpred IS NULL
      AND i.indnatts = 1
      AND i.indkey[0] = (SELECT a.attnum FROM pg_attribute a WHERE a.attrelid = 'verify.work_items'::regclass AND a.attname = 'idempotency_key');
    IF stray IS NOT NULL THEN
        RAISE EXCEPTION '0018: a global unique index on work_items.idempotency_key still exists: %', stray USING ERRCODE = '55000';
    END IF;

    IF to_regclass('verify.uq_work_items_job_idem') IS NULL OR to_regclass('verify.uq_work_items_unjobbed_idem') IS NULL THEN
        RAISE EXCEPTION '0018: the per-job unique indexes on work_items are missing' USING ERRCODE = '55000';
    END IF;
END
$$;
