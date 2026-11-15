-- Migration 0016: move every module-1 object out of schema public into its own schema, verify (platform Phase 1, chunk A2).
--
-- WHY. The platform shares one database with the three modules. Modules 2 and 3 each live in their own schema (send_managed, send_byo) and
-- write every SQL statement schema-qualified. Module 1 alone created 21 tables, three partition families and two functions UNQUALIFIED, i.e. in
-- public -- and public is where the platform's own users and job_items must live (module 2 and 3 read them). Two owners in one schema is how a
-- name clash ("jobs", "cache", "nodes", "policies" are all generic) or an over-broad grant becomes a production incident. This migration ends
-- that: after it, public holds nothing of module 1's, and the platform's schema gate fails if it ever does again.
--
-- FORWARD-ONLY. Migrations 0001..0015 are not edited: they still create their objects in public (they are unqualified and run before this one).
-- This file then moves what they made. There is no migration tracker and no down file (M1 has neither); this file is safe on an EMPTY
-- database (0001..0015 just ran) and on a database already at 0015. Re-running 0001..0015 on a database that already ran 0016 is NOT supported:
-- it would re-create empty tables in public (every CREATE is IF NOT EXISTS) and this file would then stop with a message naming them.
--
-- ROLES (the same trio as send_managed / send_byo):
--   verify_owner  NOLOGIN  owns the schema and every object in it. Used only by a human or the migrator through SET ROLE.
--   verify_app    LOGIN    the runtime identity of the control plane. Its password is set OUT OF BAND (ALTER ROLE ... PASSWORD), never here.
--   verify_admin  NOLOGIN  break-glass; the same privileges cp_admin held. DDL needs verify_owner, not this role (see the runbook).
-- verify_app and verify_admin are the roles migration 0005 created as cp_runtime and cp_admin, RENAMED, so every grant they held (on all 21 tables,
-- the partitions and the partition function) carries over unchanged by construction (section 6b does the same by copying when a second database in
-- one cluster finds the roles already renamed). A test compares the privileges before and after.
-- Privileges ADDED, each justified by a statement in the code: INSERT and UPDATE(is_default) on policies (EnsurePolicy runs at startup: PG:419-434);
-- USAGE on schema verify (public's implicit USAGE no longer applies). Nothing else is widened and nothing unused is pruned (cache DELETE etc. stay;
-- tightening them is a separate, deliberate change).

-- 1. Roles ------------------------------------------------------------------------------------------------------------------------------
DO $$
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'cp_runtime') AND NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'verify_app') THEN
        ALTER ROLE cp_runtime RENAME TO verify_app;
    END IF;
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'cp_admin') AND NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'verify_admin') THEN
        ALTER ROLE cp_admin RENAME TO verify_admin;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'verify_owner') THEN
        CREATE ROLE verify_owner NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'verify_app') THEN
        CREATE ROLE verify_app NOLOGIN;      -- unreachable after 0005; kept so the file is self-sufficient
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'verify_admin') THEN
        CREATE ROLE verify_admin NOLOGIN;
    END IF;
    -- ALTER ... OWNER TO needs the migrating role to be able to SET ROLE to the new owner. A superuser always can; on Supabase the migrator is `postgres`
    -- (not a superuser), which created verify_owner above and so may grant it to itself.
    IF NOT pg_has_role(current_user, 'verify_owner', 'MEMBER') THEN
        EXECUTE format('GRANT verify_owner TO %I', current_user);
    END IF;
END
$$;

ALTER ROLE verify_app LOGIN;

COMMENT ON ROLE verify_owner IS 'Owns schema verify and every object in it (module 1). NOLOGIN: used through SET ROLE by the migrator or a person, never by the service.';
COMMENT ON ROLE verify_app   IS 'Runtime login of the module-1 control plane (formerly cp_runtime). Not a superuser, owns nothing, no DDL. Password is set out of band.';
COMMENT ON ROLE verify_admin IS 'Break-glass grant-admin (formerly cp_admin): table privileges only. NOLOGIN. DDL needs verify_owner (PN-9/DR-3).';

-- 2. Schema -----------------------------------------------------------------------------------------------------------------------------
CREATE SCHEMA IF NOT EXISTS verify AUTHORIZATION verify_owner;
REVOKE ALL ON SCHEMA verify FROM PUBLIC;
GRANT USAGE ON SCHEMA verify TO verify_app, verify_admin;

-- 3. Move the tables (and, for the partitioned table, every partition) ------------------------------------------------------------------
-- An explicit list of module 1's 21 tables, not "everything in public": public is shared with the platform and must never be swept.
DO $$
DECLARE
    t    text;
    part record;
    m1_tables CONSTANT text[] := ARRAY[
        'verifications', 'verification_raw', 'cache', 'catchall_domains', 'disposable_domains', 'spam_trap_domains',
        'nodes', 'policies', 'send_outcomes', 'verification_outcomes', 'node_audits', 'node_warmup', 'rate_budgets',
        'work_items', 'jobs', 'work_item_attempts', 'api_callers', 'external_calls', 'calibration', 'recalibration_runs',
        'outcome_ingest_cursor'];
BEGIN
    -- A name present in BOTH schemas means 0001..0015 were re-run on a migrated database (see the header). Refuse before moving anything.
    FOREACH t IN ARRAY m1_tables LOOP
        IF to_regclass(format('public.%I', t)) IS NOT NULL AND to_regclass(format('verify.%I', t)) IS NOT NULL THEN
            RAISE EXCEPTION '0016: table % exists in BOTH public and verify; 0001..0015 were re-run on a migrated database. Drop the empty public.% (check it is empty first) and re-run 0016.', t, t
                USING ERRCODE = '42P07';
        END IF;
    END LOOP;

    -- Partitions of verification_raw first (they are separate relations and do NOT follow the parent).
    IF to_regclass('public.verification_raw') IS NOT NULL THEN
        FOR part IN
            SELECT c.relname
            FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
            WHERE i.inhparent = 'public.verification_raw'::regclass
            ORDER BY c.relname
        LOOP
            EXECUTE format('ALTER TABLE public.%I SET SCHEMA verify', part.relname);
        END LOOP;
    END IF;

    FOREACH t IN ARRAY m1_tables LOOP
        IF to_regclass(format('public.%I', t)) IS NOT NULL THEN
            EXECUTE format('ALTER TABLE public.%I SET SCHEMA verify', t);
        END IF;
    END LOOP;
END
$$;

-- 4. Ownership: every table and partition belongs to verify_owner (an index follows its table) -------------------------------------------
DO $$
DECLARE r record;
BEGIN
    FOR r IN
        SELECT c.relname FROM pg_class c
        WHERE c.relnamespace = 'verify'::regnamespace AND c.relkind IN ('r', 'p') AND pg_get_userbyid(c.relowner) <> 'verify_owner'
        ORDER BY c.relname
    LOOP
        EXECUTE format('ALTER TABLE verify.%I OWNER TO verify_owner', r.relname);
    END LOOP;
END
$$;

-- 5. Privileges ADDED (everything else carried over through the rename) -----------------------------------------------------------------
-- EnsurePolicy (control-plane startup, store/postgres.go) INSERTs a policy row and flips is_default. Before this migration the runtime role held
-- only SELECT on policies, so a login holding exactly the old grants failed at startup (42501) and never served. UPDATE is column-level: the rows
-- stay immutable except for the default flag.
GRANT INSERT ON verify.policies TO verify_app;
GRANT UPDATE (is_default) ON verify.policies TO verify_app;

-- 6. The partition-maintenance functions, rebuilt in verify -----------------------------------------------------------------------------
-- 0014 hard-coded public (to_regclass, CREATE TABLE ... PARTITION OF public.verification_raw, GRANT ... public.%I, and the wrapper's call) and the
-- role names cp_runtime / cp_admin. A function body is text: moving the function would not rewrite it, so both are re-created here.
DROP FUNCTION IF EXISTS public.ensure_verification_raw_partitions(integer);
DROP FUNCTION IF EXISTS public.ensure_verification_raw_partitions_at(timestamptz, integer);

CREATE OR REPLACE FUNCTION verify.ensure_verification_raw_partitions_at(p_from timestamptz, p_months_ahead integer)
RETURNS integer
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
SET timezone = 'UTC'
AS $fn$
DECLARE
    m_start   timestamptz;
    i         integer;
    part_name text;
    created   integer := 0;
BEGIN
    IF p_months_ahead IS NULL OR p_months_ahead < 0 OR p_months_ahead > 120 THEN
        RAISE EXCEPTION 'ensure_verification_raw_partitions: months_ahead % is out of range 0..120', p_months_ahead
            USING ERRCODE = '22023';
    END IF;

    -- Serialise concurrent callers; released automatically at transaction end.
    PERFORM pg_advisory_xact_lock(hashtext('verification_raw_partition_maintenance'));

    FOR i IN 0..p_months_ahead LOOP
        m_start   := date_trunc('month', p_from) + make_interval(months => i);
        part_name := 'verification_raw_' || to_char(m_start, 'YYYY_MM');
        IF to_regclass(format('verify.%I', part_name)) IS NULL THEN
            EXECUTE format(
                'CREATE TABLE verify.%I PARTITION OF verify.verification_raw FOR VALUES FROM (%L) TO (%L)',
                part_name, m_start, m_start + interval '1 month');
            -- Same grants 0005 gave the legacy partitions: runtime INSERT+SELECT only, admin everything.
            EXECUTE format('GRANT SELECT, INSERT ON verify.%I TO verify_app', part_name);
            EXECUTE format('GRANT ALL PRIVILEGES ON verify.%I TO verify_admin', part_name);
            EXECUTE format('COMMENT ON TABLE verify.%I IS %L', part_name,
                'Monthly partition, auto-created by ensure_verification_raw_partitions (0014, rebuilt in verify by 0016). Retention: DROP PARTITION (admin).');
            created := created + 1;
        END IF;
    END LOOP;
    RETURN created;
END
$fn$;

COMMENT ON FUNCTION verify.ensure_verification_raw_partitions_at(timestamptz, integer) IS
    'Creates verify.verification_raw monthly partitions for [month(p_from) .. month(p_from)+p_months_ahead], bounds in UTC. Idempotent. Admin/test core: NOT granted to verify_app (ADR-0017).';

CREATE OR REPLACE FUNCTION verify.ensure_verification_raw_partitions(p_months_ahead integer)
RETURNS integer
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
    SELECT verify.ensure_verification_raw_partitions_at(now(), p_months_ahead)
$fn$;

COMMENT ON FUNCTION verify.ensure_verification_raw_partitions(integer) IS
    'Runtime door for partition maintenance (ADR-0017): SECURITY DEFINER (owner verify_owner), reference time = now() (not caller-controlled), creates only this-month..N-months-ahead partitions. Returns how many it created.';

ALTER FUNCTION verify.ensure_verification_raw_partitions_at(timestamptz, integer) OWNER TO verify_owner;
ALTER FUNCTION verify.ensure_verification_raw_partitions(integer) OWNER TO verify_owner;

REVOKE ALL ON FUNCTION verify.ensure_verification_raw_partitions_at(timestamptz, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION verify.ensure_verification_raw_partitions(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION verify.ensure_verification_raw_partitions_at(timestamptz, integer) TO verify_admin;
GRANT EXECUTE ON FUNCTION verify.ensure_verification_raw_partitions(integer) TO verify_app, verify_admin;

-- 6b. A second database in the same cluster -------------------------------------------------------------------------------------------------
-- Roles are CLUSTER-wide, not per database. The first database migrated in a cluster renames cp_runtime / cp_admin (section 1). A second database in
-- that cluster then runs 0005, which finds no cp_runtime / cp_admin and creates them AGAIN and grants this database's tables to THEM; section 1 sees
-- both the old and the new name and renames nothing, so verify_app / verify_admin would hold nothing here. Move this database's table privileges
-- across (same objects, same privileges) and drop the old role if no other database still depends on it. A no-op whenever the rename above ran
-- (the old role no longer exists). Production has one database, so this is the path tests and multi-database dev clusters take.
DO $$
DECLARE
    pair text[];
    r    record;
BEGIN
    FOREACH pair SLICE 1 IN ARRAY ARRAY[ARRAY['cp_runtime', 'verify_app'], ARRAY['cp_admin', 'verify_admin']] LOOP
        IF EXISTS (SELECT FROM pg_roles WHERE rolname = pair[1]) THEN
            FOR r IN
                SELECT c.relname, a.privilege_type
                FROM pg_class c CROSS JOIN LATERAL aclexplode(c.relacl) a
                WHERE c.relnamespace = 'verify'::regnamespace AND c.relkind IN ('r', 'p')
                  AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = pair[1])
                ORDER BY c.relname, a.privilege_type
            LOOP
                EXECUTE format('GRANT %s ON verify.%I TO %I', r.privilege_type, r.relname, pair[2]);
                EXECUTE format('REVOKE %s ON verify.%I FROM %I', r.privilege_type, r.relname, pair[1]);
            END LOOP;
            BEGIN
                EXECUTE format('DROP ROLE %I', pair[1]);
            EXCEPTION WHEN dependent_objects_still_exist THEN
                NULL;  -- another database of this cluster still holds grants to it; that database's own 0016 removes the role
            END;
        END IF;
    END LOOP;
END
$$;

-- 7. Self-check: refuse to finish if anything of module 1's is still in public, or anything in verify is not owned by verify_owner -----------
DO $$
DECLARE
    leftover text;
    stray    text;
BEGIN
    SELECT string_agg(c.relname, ', ' ORDER BY c.relname) INTO leftover
    FROM pg_class c
    WHERE c.relnamespace = 'public'::regnamespace
      AND (c.relname = ANY (ARRAY[
            'verifications', 'verification_raw', 'cache', 'catchall_domains', 'disposable_domains', 'spam_trap_domains',
            'nodes', 'policies', 'send_outcomes', 'verification_outcomes', 'node_audits', 'node_warmup', 'rate_budgets',
            'work_items', 'jobs', 'work_item_attempts', 'api_callers', 'external_calls', 'calibration', 'recalibration_runs',
            'outcome_ingest_cursor'])
           OR c.relname LIKE 'verification\_raw\_%');
    IF leftover IS NOT NULL THEN
        RAISE EXCEPTION '0016: module-1 objects are still in public: %', leftover USING ERRCODE = '55000';
    END IF;

    SELECT string_agg(c.relname || ' (owner ' || pg_get_userbyid(c.relowner) || ')', ', ' ORDER BY c.relname) INTO stray
    FROM pg_class c
    WHERE c.relnamespace = 'verify'::regnamespace AND c.relkind IN ('r', 'p', 'i', 'S') AND pg_get_userbyid(c.relowner) <> 'verify_owner';
    IF stray IS NOT NULL THEN
        RAISE EXCEPTION '0016: objects in verify not owned by verify_owner: %', stray USING ERRCODE = '55000';
    END IF;

    IF to_regprocedure('public.ensure_verification_raw_partitions(integer)') IS NOT NULL
       OR to_regprocedure('public.ensure_verification_raw_partitions_at(timestamptz,integer)') IS NOT NULL THEN
        RAISE EXCEPTION '0016: a partition function is still in public' USING ERRCODE = '55000';
    END IF;
END
$$;
