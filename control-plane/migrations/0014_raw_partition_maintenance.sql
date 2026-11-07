-- Migration 0014: verification_raw partition maintenance (closes the 2026-10-01 partition cliff)
--
-- THE DEFECT. 0001 created exactly three monthly partitions (2026-07 .. 2026-09) "as examples", with
-- the note "Production: automate partition creation ahead of time". Nothing ever did. From
-- 2026-10-01 every INSERT into verification_raw failed with SQLSTATE 23514 "no partition of relation
-- verification_raw found for row" -- and that table is the raw sidecar of every SMTP verdict, which
-- must insert atomically with the LOG row (§6.4), so every SMTP verdict write failed. The suite was
-- green in August and red in November purely because the date moved.
--
-- THE FIX, and why it is shaped this way (ADR-0017):
--   * Partition creation is DDL, and PROJECT_RULES Door 2 keeps DDL OFF the runtime role (cp_runtime
--     is INSERT+SELECT only; DDL is the separate break-glass cp_admin credential, PN-9/DR-3). The
--     fix must not widen cp_runtime. So the runtime gets ONE narrow door: a SECURITY DEFINER wrapper
--     that can only ever create the partitions for "this month .. N months from now", where "now" is
--     the database clock and cannot be supplied by the caller. It cannot drop, alter or read anything.
--   * The wrapper delegates to an _at() core that takes the reference time as a parameter, so tests
--     (and an admin) can exercise it at any date. The core is NOT granted to cp_runtime.
--   * All bounds are computed in UTC (SET timezone = 'UTC' is part of the function), not in whatever
--     time zone the calling session happens to use -- created_at is timestamptz, so a bound is an
--     instant, and a session-zone-dependent bound would silently leave a gap or overlap at a month edge.
--   * Naming matches 0001 (verification_raw_YYYY_MM), so the three legacy partitions are found by name
--     and left alone. New partitions get the same grants 0005 gave the legacy ones.
--   * Concurrent callers (two control-plane instances, or the loop racing this migration) are
--     serialised by a transaction-scoped advisory lock; the function is idempotent.
--
-- N (how many future months to keep) is NOT decided here. It is the TM_RAW_PARTITION_MONTHS_AHEAD dial
-- (Docs/config-manifest.md), applied by the control-plane's maintenance loop. This migration only
-- bootstraps the CURRENT month, so a freshly migrated database accepts today's rows immediately.
--
-- Retention (DROP PARTITION after ~90 days, §3.2.6) is still an admin action and is NOT automated here.

CREATE OR REPLACE FUNCTION ensure_verification_raw_partitions_at(p_from timestamptz, p_months_ahead integer)
RETURNS integer
LANGUAGE plpgsql
SET search_path = pg_catalog, public
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
        IF to_regclass(format('public.%I', part_name)) IS NULL THEN
            EXECUTE format(
                'CREATE TABLE public.%I PARTITION OF public.verification_raw FOR VALUES FROM (%L) TO (%L)',
                part_name, m_start, m_start + interval '1 month');
            -- Same grants 0005 gave the legacy partitions: runtime INSERT+SELECT only, admin everything.
            EXECUTE format('GRANT SELECT, INSERT ON public.%I TO cp_runtime', part_name);
            EXECUTE format('GRANT ALL PRIVILEGES ON public.%I TO cp_admin', part_name);
            EXECUTE format('COMMENT ON TABLE public.%I IS %L', part_name,
                'Monthly partition, auto-created by ensure_verification_raw_partitions (0014). Retention: DROP PARTITION (admin).');
            created := created + 1;
        END IF;
    END LOOP;
    RETURN created;
END
$fn$;

COMMENT ON FUNCTION ensure_verification_raw_partitions_at(timestamptz, integer) IS
    'Creates verification_raw monthly partitions for [month(p_from) .. month(p_from)+p_months_ahead], bounds in UTC. Idempotent. Admin/test core: NOT granted to cp_runtime (ADR-0017).';

-- The runtime-callable door: the reference time is fixed to the database clock.
CREATE OR REPLACE FUNCTION ensure_verification_raw_partitions(p_months_ahead integer)
RETURNS integer
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $fn$
    SELECT public.ensure_verification_raw_partitions_at(now(), p_months_ahead)
$fn$;

COMMENT ON FUNCTION ensure_verification_raw_partitions(integer) IS
    'Runtime door for partition maintenance (ADR-0017): SECURITY DEFINER, reference time = now() (not caller-controlled), creates only this-month..N-months-ahead partitions. Returns how many it created.';

REVOKE ALL ON FUNCTION ensure_verification_raw_partitions_at(timestamptz, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION ensure_verification_raw_partitions(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ensure_verification_raw_partitions_at(timestamptz, integer) TO cp_admin;
GRANT EXECUTE ON FUNCTION ensure_verification_raw_partitions(integer) TO cp_runtime, cp_admin;

-- Bootstrap: make sure the CURRENT month has a partition, so a freshly migrated database can take a
-- row dated today before any control-plane has started. Future headroom is the maintenance loop's job.
SELECT ensure_verification_raw_partitions_at(now(), 0);
