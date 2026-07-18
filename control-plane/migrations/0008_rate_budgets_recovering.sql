-- Migration 0008: rate_budgets (durable per-provider rate-limit spend) + the `recovering`
-- lifecycle sub-state on `nodes.status`.
-- Implements: Phase-4 spec §4.4 (durable rate-budget state, token-bucket), §5.4/GAP-30 (the
--             `recovering` canary sub-state so block-rate quarantine is NOT a one-way door).
--
-- WHY THESE ARE NOT FROZEN-DOOR CHANGES (additive-only against all five doors — P4 §9):
--   * rate_budgets is a NEW operational table. It touches NO frozen schema (verifications is the
--     append-only LOG, Door 2, untouched; cache/hashing untouched). It mirrors the P3 node_warmup
--     DURABILITY discipline (§5.2, PM4): the CONSUMED allowance must survive a brain restart, or a
--     freshly-restarted brain would let the whole pool sprint at every provider at once (a
--     reputation hazard masquerading as a reputation control — §4.4). In-flight counts are the
--     OPPOSITE decision (ephemeral, correct-at-zero post-restart, §4.3/GAP-26) and are NOT stored.
--   * `nodes.status` gains one enum value: 'recovering'. This is an additive value on the NODE
--     LIFECYCLE state machine (§4.5/§5.4 SVG-⑦), NOT the verdict vocabulary. Door 4 freezes the
--     `status`/`reason`/`recommendation` VERDICT enums (control-plane/internal/vocab); the node
--     lifecycle is a separate, internal operational state (audit.go documents this distinction:
--     "disabled is NOT here … the router requires Enabled AND status==active"). Adding a lifecycle
--     value is exactly the additive migration the P3 note anticipated. A quarantined node that goes
--     audit-green + live moves to 'recovering' (a sub-state of quarantined for CANDIDACY: it is NOT
--     a general :smtp candidate — only a metered canary trickle reaches it, §5.4). The four-way
--     GAP-24 identity, the eligibility gate, and the verdict contract are all unchanged.

-- =============================================================================
-- nodes.status — add the 'recovering' lifecycle value (§5.4/GAP-30, additive)
-- =============================================================================
-- The CHECK is REPLACED with a superset (adds 'recovering'); every previously-valid value stays
-- valid, so this is a strict widening (additive — never a removal/rename). Idempotent: drop-if-
-- exists then add. A node in 'recovering' is a quarantined node earning its way back via canary.

ALTER TABLE nodes DROP CONSTRAINT IF EXISTS valid_status;
ALTER TABLE nodes ADD CONSTRAINT valid_status
    CHECK (status IN ('provisioning', 'active', 'quarantined', 'recovering'));

COMMENT ON COLUMN nodes.status IS 'Node lifecycle (SVG-⑦): provisioning|active|quarantined|recovering. P4 adds ''recovering'' (§5.4/GAP-30): a block-quarantined node that regained audit-green+liveness earns its way back via a metered canary trickle before promotion to active. NOT the verdict vocab (Door 4) — an operational lifecycle value. The operator on/off switch is the config descriptor''s Enabled flag, orthogonal to this.';

-- =============================================================================
-- rate_budgets — DURABLE per-(node × provider) rate-limit spend (P4 §4.4, LOCKED durable)
-- =============================================================================
-- Token-bucket with a durable last_refill_at + tokens per (node, provider_bucket) — OQ-P4-3:
-- one row per key, restart-safe (tokens are reconstructed from last_refill_at on read, so the
-- allowance is NEVER refilled by a restart), smooth, and cheap. The bucket parameters (capacity,
-- refill rate) are DIALs supplied by the policy `rate_limits` key (§4.2/§4.7); only the SPEND
-- state lives here. provider_bucket is the brain's mx_provider class plus a coarse MX-domain
-- bucket for `other` (OQ-P4-2: registrable domain of the MX host), so distinct corporate providers
-- aren't lumped into one budget. The big consumer providers (google/o365/yahoo) are :mx-routed and
-- never SMTP'd (§4.1), so the limiter's live work is honest-corporate `:smtp` traffic only.
--
-- Durability rationale (§4.4, LOCKED, mirrors node_warmup PM4): the consumed allowance is a FACT
-- about this window ("how many session-starts have we spent"); it must persist so a brain
-- restart/deploy/crash cannot reset it to full. Contrast node_warmup (the node-global ceiling) and
-- the ephemeral in-flight gauge (§4.3, in-memory, correct-at-zero post-restart — NOT stored).

CREATE TABLE IF NOT EXISTS rate_budgets (
    node_id text NOT NULL,               -- FK-less (mirrors verifications.node_id / node_warmup)
    provider_bucket text NOT NULL,       -- mx_provider class or 'other:<registrable-mx-domain>' (OQ-P4-2)
    tokens double precision NOT NULL,    -- current whole/fractional tokens available (token-bucket)
    capacity double precision NOT NULL,  -- bucket ceiling (max burst); a DIAL from policy rate_limits
    refill_per_sec double precision NOT NULL, -- steady-state refill rate; a DIAL from policy rate_limits
    last_refill_at timestamptz NOT NULL, -- anchor: tokens are reconstructed from this on read (no refill-on-restart)
    updated_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (node_id, provider_bucket),
    CONSTRAINT rate_tokens_nonneg CHECK (tokens >= 0),
    CONSTRAINT rate_capacity_pos CHECK (capacity > 0),
    CONSTRAINT rate_refill_nonneg CHECK (refill_per_sec >= 0)
);

COMMENT ON TABLE rate_budgets IS 'DURABLE per-(node × provider) rate-limit spend (P4 §4.4, OQ-P4-3 token-bucket). tokens reconstructed from last_refill_at on read → a brain restart NEVER refills the allowance (mirrors node_warmup PM4). In-flight counts are the OPPOSITE decision (ephemeral, §4.3/GAP-26) and are NOT stored here. cp_runtime: SELECT+INSERT+UPDATE (admit decrements); DELETE stays cp_admin.';
COMMENT ON COLUMN rate_budgets.provider_bucket IS 'mx_provider class (google|o365|yahoo|self|other) or ''other:<registrable-mx-domain>'' (OQ-P4-2 — shared corp platforms paced as one provider). google/o365/yahoo are :mx-routed & never SMTP''d, so the live buckets are honest-corporate ''other:*''.';
COMMENT ON COLUMN rate_budgets.last_refill_at IS 'Token-bucket refill anchor. On read, tokens += refill_per_sec × (now − last_refill_at), capped at capacity, then last_refill_at = now. Because tokens are DERIVED from elapsed wall-clock, a brain restart cannot over-grant (§4.4 restart-safe).';

-- =============================================================================
-- Grants — cp_runtime reads/writes the rate budget; DELETE stays admin (mirrors node_warmup)
-- =============================================================================

-- rate_budgets: the limiter needs SELECT+INSERT+UPDATE; DELETE stays cp_admin (a purge is break-glass).
GRANT SELECT, INSERT, UPDATE ON rate_budgets TO cp_runtime;
REVOKE DELETE ON rate_budgets FROM cp_runtime;
GRANT ALL PRIVILEGES ON rate_budgets TO cp_admin;
