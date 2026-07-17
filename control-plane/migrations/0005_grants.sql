-- Migration 0005: grants (Door 2 append-only enforcement)
-- Implements: Phase-1 spec §3.2.1 (PN-9/DR-3)
-- Two-role model: cp_runtime (INSERT+SELECT only on verifications) and cp_admin (DDL owner).
-- This is the mechanism that makes THE LOG physically append-only.

-- =============================================================================
-- Role definitions
-- =============================================================================

-- Runtime service role: brain's operational credential
-- Gets INSERT + SELECT only on verifications (NO UPDATE, NO DELETE)
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cp_runtime') THEN
        CREATE ROLE cp_runtime NOLOGIN;
    END IF;
END
$$;

COMMENT ON ROLE cp_runtime IS 'Runtime service role for the control-plane. Maps to Supabase service_role concept. Gets INSERT+SELECT only on verifications (append-only enforcement).';

-- Grant-admin / DDL owner: break-glass, admin-audited access (PN-9/DR-3)
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cp_admin') THEN
        CREATE ROLE cp_admin NOLOGIN;
    END IF;
END
$$;

COMMENT ON ROLE cp_admin IS 'Grant-admin / DDL owner. Separate break-glass, admin-audited credential (distinct from runtime). PN-9: prevents runtime from rewriting history.';

-- =============================================================================
-- Grant privileges: verifications (THE LOG, append-only enforcement)
-- =============================================================================

-- cp_runtime gets INSERT + SELECT only (NO UPDATE, NO DELETE)
GRANT SELECT, INSERT ON verifications TO cp_runtime;
REVOKE UPDATE, DELETE ON verifications FROM cp_runtime;

COMMENT ON TABLE verifications IS 'THE LOG: append-only enforcement via grants. cp_runtime has INSERT+SELECT only. Corrections are new rows, never mutations. PN-9: cp_admin holds grant-admin separately.';

-- cp_admin gets full table ownership for DDL/grants
GRANT ALL PRIVILEGES ON verifications TO cp_admin;

-- =============================================================================
-- Grant privileges: verification_raw (retention via DROP PARTITION only)
-- =============================================================================

-- cp_runtime gets INSERT + SELECT
GRANT SELECT, INSERT ON verification_raw TO cp_runtime;
GRANT SELECT, INSERT ON verification_raw_2026_07 TO cp_runtime;
GRANT SELECT, INSERT ON verification_raw_2026_08 TO cp_runtime;
GRANT SELECT, INSERT ON verification_raw_2026_09 TO cp_runtime;

-- Retention is DROP PARTITION (admin action), never UPDATE/DELETE on rows
REVOKE UPDATE, DELETE ON verification_raw FROM cp_runtime;

-- cp_admin for partition management
GRANT ALL PRIVILEGES ON verification_raw TO cp_admin;
GRANT ALL PRIVILEGES ON verification_raw_2026_07 TO cp_admin;
GRANT ALL PRIVILEGES ON verification_raw_2026_08 TO cp_admin;
GRANT ALL PRIVILEGES ON verification_raw_2026_09 TO cp_admin;

-- =============================================================================
-- Grant privileges: cache (read/write)
-- =============================================================================

GRANT SELECT, INSERT, UPDATE, DELETE ON cache TO cp_runtime;
GRANT ALL PRIVILEGES ON cache TO cp_admin;

-- =============================================================================
-- Grant privileges: lists, nodes, policies (read-mostly for runtime)
-- =============================================================================

-- Lists: runtime reads; admin writes
GRANT SELECT ON catchall_domains TO cp_runtime;
GRANT SELECT ON disposable_domains TO cp_runtime;
GRANT SELECT ON spam_trap_domains TO cp_runtime;

GRANT ALL PRIVILEGES ON catchall_domains TO cp_admin;
GRANT ALL PRIVILEGES ON disposable_domains TO cp_admin;
GRANT ALL PRIVILEGES ON spam_trap_domains TO cp_admin;

-- Nodes: runtime reads (registry mirror); admin writes
GRANT SELECT ON nodes TO cp_runtime;
GRANT ALL PRIVILEGES ON nodes TO cp_admin;

-- Policies: runtime reads; admin writes (immutable versioned rows)
GRANT SELECT ON policies TO cp_runtime;
GRANT ALL PRIVILEGES ON policies TO cp_admin;

-- =============================================================================
-- Grant privileges: send_outcomes + join table (Phase 7 seam, empty in P1)
-- =============================================================================

-- Runtime will eventually write outcomes; empty in P1
GRANT SELECT, INSERT ON send_outcomes TO cp_runtime;
GRANT SELECT, INSERT ON verification_outcomes TO cp_runtime;

GRANT ALL PRIVILEGES ON send_outcomes TO cp_admin;
GRANT ALL PRIVILEGES ON verification_outcomes TO cp_admin;

-- =============================================================================
-- Supabase mapping note
-- =============================================================================

-- In Supabase:
-- - cp_runtime maps to the service_role key (used by the control-plane at runtime)
-- - cp_admin maps to the postgres/owner role (break-glass DDL access)
--
-- Phase 1 uses service-role key only (OQ-20); no RLS (single client = brain).
-- RLS designed if tenant identities appear later.
