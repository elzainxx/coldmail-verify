-- Migration 0007: node_audits (append-only audit history) + node_warmup (durable ramp counter)
-- Implements: Phase-3 spec §4.3 (OQ-P3-5, audit history), §5.2 (warmup counter is durable state),
--             §4.4/§4.5 (the gate + auto-quarantine write to nodes' reserved columns).
--
-- WHY THESE ARE NOT FROZEN-DOOR CHANGES (additive-only against all five doors — P3 §8):
--   * node_audits and node_warmup are NEW tables. They touch NO frozen schema (verifications is
--     the append-only LOG, Door 2, untouched here). P1 reserved the SUMMARY columns on `nodes`
--     (audit_status/last_audit_at/port25_open) but did NOT reserve an audit *history* — OQ-P3-5
--     resolves that with an append-only history table (audit results are historical data: the
--     Door-2 spirit "what you don't log now is gone forever"; a mutable column would overwrite a
--     green→red→green flap history that P4's health-watcher will want). SEAM → P4 consumes it.
--   * nodes gains cp_runtime INSERT+UPDATE (was SELECT-only) so the brain can mirror the config
--     node list and write the reserved health/lifecycle columns (audit_status, last_audit_at,
--     port25_open, status). DELETE stays cp_admin — the runtime never destroys a node record,
--     mirroring the LOG's "runtime cannot destroy history" posture (PN-9/DR-3). No table/column/
--     CHECK change to `nodes` — only the grant, and the writes target columns 0003 already created.

-- =============================================================================
-- node_audits — APPEND-ONLY audit history (P3 §4.3, OQ-P3-5)
-- =============================================================================
-- One row per /v1/audit result (initial + scheduled + on-demand). The gate (§4.4) reads the
-- LATEST row per node; the flap history (green→red→green) is what P4's health-watcher consumes.
-- current_host_ip is the audit's self-detected IP — the GAP-24 anchor (§6.6.1): the brain
-- cross-checks it against the node's expected public egress IP so a multi-homed box can't pass
-- audit-green on its PRIVATE IP while the real egress IP is unaudited.

CREATE TABLE IF NOT EXISTS node_audits (
    id uuid PRIMARY KEY,                 -- App-generated UUIDv7 (time-ordered, insert locality)
    node_id text NOT NULL,               -- FK-less (must survive node deletion — mirrors verifications.node_id)
    audited_at timestamptz NOT NULL,     -- Audit time, UTC (from the wrapper's audited_at)
    audit_status text NOT NULL,          -- 'green' (warnings=={}) | 'red' (any warning) — P3 §4.1
    warnings jsonb NOT NULL,             -- The raw host_audit warnings ({} == green); which dim failed (dns/ptr)
    current_host_ip inet,                -- host_audit.current_host_ip; the GAP-24 anchor (§6.6.1). NULL if undetectable.
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT valid_audit_status CHECK (audit_status IN ('green', 'red'))
);

COMMENT ON TABLE node_audits IS 'APPEND-ONLY audit history (P3 §4.3/OQ-P3-5). One row per /v1/audit. Gate reads latest per node; flap history feeds the P4 health-watcher. NOT the verdict LOG. cp_runtime: INSERT+SELECT only (append-only, mirrors Door-2 posture).';
COMMENT ON COLUMN node_audits.audit_status IS 'green iff host_audit warnings=={} (P3 §4.1). Any warning (dns/ptr) => red.';
COMMENT ON COLUMN node_audits.warnings IS 'Raw host_audit warnings object; {} == green. Preserved so an operator sees WHICH dimension failed (dns vs ptr).';
COMMENT ON COLUMN node_audits.current_host_ip IS 'host_audit self-detected IP — the GAP-24/§6.6.1 anchor: cross-checked vs the node''s expected public egress IP so a multi-homed box can''t pass green on its private IP.';

-- Latest-audit-per-node lookup + flap history scans.
CREATE INDEX IF NOT EXISTS idx_node_audits_node_time ON node_audits (node_id, audited_at DESC);

-- =============================================================================
-- node_warmup — DURABLE warmup ramp counter (P3 §5.2, LOCKED: not in-memory)
-- =============================================================================
-- The warmup cap is a per-node, per-window ascending ceiling on :smtp-routed verifications
-- (P3 §5.2). It MUST be durable in Supabase: an in-memory counter resets on a brain restart and
-- a fresh IP silently jumps back to full allowance mid-warmup (a reputation hazard). The box stays
-- stateless; the warmup PROGRESS is durable brain state (P1 §5.8). One row per (node, window):
-- `count` is slots reserved so far this window; `cap` is that window's ceiling (set at row creation,
-- so a ramp that raises the cap tomorrow simply opens a new window row at the higher cap).
--
-- Connection-cost note (P3 §5.2, GAP-13/ADR-0007): on a real IP a catch-all-eligible verification
-- costs ~2 outbound connections (target + probe, Option B). The cap counts VERIFICATIONS; a warmup
-- ramp sized in connections must set `cap` to ~half the intended connection budget. Recorded so the
-- ramp isn't silently 2x the intended real-MX volume (the readiness-assessment Gate-2 factor).

CREATE TABLE IF NOT EXISTS node_warmup (
    node_id text NOT NULL,               -- FK-less (mirrors verifications.node_id)
    window_start timestamptz NOT NULL,   -- Start of the ramp window (e.g. the UTC day), UTC
    count integer NOT NULL DEFAULT 0,    -- :smtp verifications reserved this window
    cap integer NOT NULL,                -- This window's ascending ceiling (DIAL; P3 §5.2)
    updated_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (node_id, window_start),
    CONSTRAINT warmup_count_nonneg CHECK (count >= 0),
    CONSTRAINT warmup_cap_nonneg CHECK (cap >= 0)
);

COMMENT ON TABLE node_warmup IS 'DURABLE warmup ramp counter (P3 §5.2, LOCKED not-in-memory). Per (node, window): count reserved so far, cap = that window''s ascending ceiling. Restart-safe so a brain restart cannot reset a fresh IP''s ramp to full allowance. cp_runtime: SELECT+INSERT+UPDATE (increments); DELETE stays cp_admin.';
COMMENT ON COLUMN node_warmup.cap IS 'This window''s ceiling on :smtp verifications (DIAL, P3 §5.2 OQ-P3-10). Set at window creation; a ramp raising the cap opens a new window row. Counts verifications, NOT connections (catch-all = ~2 connections — GAP-13/§5.2).';

-- =============================================================================
-- Grants — cp_runtime writes audit history + warmup + node health/lifecycle
-- =============================================================================

-- node_audits: APPEND-ONLY (INSERT+SELECT, NO UPDATE/DELETE) — mirrors the verdict LOG's posture.
GRANT SELECT, INSERT ON node_audits TO cp_runtime;
REVOKE UPDATE, DELETE ON node_audits FROM cp_runtime;
GRANT ALL PRIVILEGES ON node_audits TO cp_admin;

-- node_warmup: counter needs SELECT+INSERT+UPDATE; DELETE stays admin (a purge is break-glass).
GRANT SELECT, INSERT, UPDATE ON node_warmup TO cp_runtime;
REVOKE DELETE ON node_warmup FROM cp_runtime;
GRANT ALL PRIVILEGES ON node_warmup TO cp_admin;

-- nodes: was SELECT-only (0005). P3 lets the brain mirror the config node list and write the
-- reserved health/lifecycle columns (audit_status, last_audit_at, port25_open, status). DELETE
-- stays cp_admin: the runtime never destroys a node record (PN-9/DR-3 posture). No schema change.
GRANT INSERT, UPDATE ON nodes TO cp_runtime;
REVOKE DELETE ON nodes FROM cp_runtime;

COMMENT ON TABLE nodes IS 'Node registry. Config-list is authoritative (P1 OQ-5); this table mirrors + holds health history. P3 §4.3/§4.4: cp_runtime gains INSERT+UPDATE to mirror the list and write reserved health/lifecycle columns (audit_status, last_audit_at, port25_open, status); DELETE stays cp_admin. verifications.node_id is FK-less (must survive deletion).';
