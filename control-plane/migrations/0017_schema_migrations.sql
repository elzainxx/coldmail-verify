-- Migration 0017: the migration tracker, verify.schema_migrations (owner ruling: "M1 migration tracker: add one so migrations run exactly once and in
-- order. Running the full chain twice must be a no-op, and a fresh install must end in the same state as an upgraded one.", Docs/future-work.md FW-3).
--
-- WHAT THIS IS. One row per migration file the runner (control-plane/cmd/migrate, internal/migrate) has applied or adopted:
--   version    the four-digit file number, '0017'
--   checksum   sha256 (hex) of the file's bytes after CRLF -> LF; the runner REFUSES to continue if a recorded file later differs on disk
--   how        'applied' = the runner executed the file; 'adopted' = the database had already run it under the old psql loop and the runner only recorded it
--   applied_at / applied_by   when and by which role (volatile: no two databases agree on them, so the runner's fingerprint tests ignore them)
-- Nothing in this file is read by the control plane at runtime: verify_app gets NO privilege on it. verify_admin may read it (break-glass inspection).
--
-- THE CHICKEN AND THE EGG. Versions 0001..0016 ran BEFORE this table existed, so the runner records them in the SAME transaction that applies this file
-- (as 'applied' on a database it built from nothing, as 'adopted' on one the old loop had already taken to 0016). That is runner logic, not SQL: this file
-- only creates the table. See control-plane/migrations/README.md "The runner and the tracker".
--
-- HARMLESS UNDER THE OLD LOOP. CI and the platform harness still apply migrations/NNNN_*.sql with one psql run per file, on an EMPTY database. This file
-- is written for that too: every statement is idempotent (running it twice changes nothing) and it fails loudly, naming the missing piece, if 0016 has
-- not run. A database the old loop took through this file has an EMPTY tracker; the runner adopts such a database (it records 0001..0017 as 'adopted').
--
-- OWNERSHIP follows 0016: the table is created by the migrating role and handed to verify_owner (ALTER ... OWNER TO needs the migrator to be able to
-- SET ROLE to verify_owner and inherit it, which 0016 guaranteed: a superuser always can, Supabase's postgres was granted it).

DO $$
BEGIN
    IF to_regnamespace('verify') IS NULL
       OR NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'verify_owner')
       OR NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'verify_admin') THEN
        RAISE EXCEPTION '0017: schema verify and the roles verify_owner / verify_admin do not exist; migration 0016 must run first'
            USING ERRCODE = '55000';
    END IF;
END
$$;

CREATE TABLE IF NOT EXISTS verify.schema_migrations (
    version    text        NOT NULL,
    checksum   text        NOT NULL,
    how        text        NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now(),
    applied_by text        NOT NULL DEFAULT current_user,

    CONSTRAINT schema_migrations_pkey            PRIMARY KEY (version),
    CONSTRAINT schema_migrations_version_format  CHECK (version ~ '^[0-9]{4}$'),
    CONSTRAINT schema_migrations_checksum_format CHECK (checksum ~ '^[0-9a-f]{64}$'),
    CONSTRAINT schema_migrations_how_known       CHECK (how IN ('applied', 'adopted'))
);

ALTER TABLE verify.schema_migrations OWNER TO verify_owner;

-- Privileges: a new table starts with none for PUBLIC (and 0016 already revoked PUBLIC's USAGE on the schema), so there is nothing to revoke; verify_app is
-- granted NOTHING; the break-glass role may read. TestTracker_Privileges pins all three, and the fingerprint tests fail if a PUBLIC entry ever appears.
GRANT SELECT ON verify.schema_migrations TO verify_admin;

COMMENT ON TABLE verify.schema_migrations IS
    'Migration tracker (0017, FW-3): one row per control-plane/migrations file the runner applied or adopted. Written only by the migrating role through cmd/migrate; never read by the runtime (verify_app has no privilege here).';
COMMENT ON COLUMN verify.schema_migrations.version IS 'Four-digit file number, e.g. 0017. Primary key: a version is recorded at most once.';
COMMENT ON COLUMN verify.schema_migrations.checksum IS 'sha256 (hex) of the migration file with CRLF normalised to LF. The runner refuses to run if a recorded file differs on disk.';
COMMENT ON COLUMN verify.schema_migrations.how IS 'applied = the runner executed the file; adopted = the database had already run it (old psql loop) and the runner only recorded it.';
COMMENT ON COLUMN verify.schema_migrations.applied_at IS 'When the row was recorded. Volatile by design (ignored by the fresh-equals-upgraded fingerprint).';
COMMENT ON COLUMN verify.schema_migrations.applied_by IS 'current_user of the migrating session. Volatile by design (ignored by the fresh-equals-upgraded fingerprint).';
