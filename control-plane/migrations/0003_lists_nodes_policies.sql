-- Migration 0003: domain lists, nodes, policies
-- Implements: Phase-1 spec §4.3–§4.6
-- Lists: catchall_domains, disposable_domains, spam_trap_domains
-- Registry: nodes
-- Policy engine: policies

-- =============================================================================
-- catchall_domains — per-domain accept-all state (Phase 2 seam)
-- =============================================================================

CREATE TABLE IF NOT EXISTS catchall_domains (
    domain text PRIMARY KEY,  -- Normalized domain (plain text, norm/v1)
    is_catchall boolean,  -- Tri-state via NULL: true/false/unknown (not yet probed)
    detected_at timestamptz,
    method text,  -- 'probe', 'seed', 'manual'
    expires_at timestamptz,  -- Re-probe cadence (Phase 2)

    CONSTRAINT domain_normalized CHECK (domain = lower(domain))
);

COMMENT ON TABLE catchall_domains IS 'Per-domain accept-all state. Tri-state is_catchall (NULL=unknown). Phase 2 probe populates; P1 seeds/reads. Analytics read this table, not the flattened flag.';
COMMENT ON COLUMN catchall_domains.is_catchall IS 'Tri-state: true (catch-all), false (not), NULL (unknown/not probed). Flag in response flattens to boolean.';

-- =============================================================================
-- disposable_domains — DEA/disposable domain list
-- =============================================================================

CREATE TABLE IF NOT EXISTS disposable_domains (
    domain text PRIMARY KEY,  -- Normalized domain
    source text,  -- List name/version provenance
    added_at timestamptz NOT NULL,

    CONSTRAINT domain_normalized CHECK (domain = lower(domain))
);

COMMENT ON TABLE disposable_domains IS 'Disposable/DEA domain list. Source of truth for is_disposable flag. OQ-7: disposable→risky (not invalid), policy skips.';

-- =============================================================================
-- spam_trap_domains — spam-trap/honeypot list (sibling to disposable)
-- =============================================================================

CREATE TABLE IF NOT EXISTS spam_trap_domains (
    domain text PRIMARY KEY,  -- Normalized domain
    source text,  -- List name/version provenance
    added_at timestamptz NOT NULL,

    CONSTRAINT domain_normalized CHECK (domain = lower(domain))
);

COMMENT ON TABLE spam_trap_domains IS 'Spam-trap/honeypot domain list. OQ-17: acquisition unsolved. Modeled now for completeness (§4.4 note).';

-- =============================================================================
-- nodes — node registry (mirror + health history)
-- =============================================================================

CREATE TABLE IF NOT EXISTS nodes (
    id text PRIMARY KEY,  -- Stable node id (written to verifications.node_id)
    label text,  -- Human-readable name
    base_url text NOT NULL,  -- HTTP endpoint for this node
    enabled boolean NOT NULL DEFAULT true,  -- Operator on/off switch
    status text NOT NULL,  -- 'provisioning', 'active', 'quarantined'
    port25_open boolean,  -- Capability flag; false/null in P1 (no SMTP)
    last_audit_at timestamptz,  -- Phase 3: /audit gate
    audit_status text,  -- 'green', 'red' (Phase 3)
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT unique_base_url UNIQUE (base_url),
    CONSTRAINT valid_status CHECK (status IN ('provisioning', 'active', 'quarantined'))
);

COMMENT ON TABLE nodes IS 'Node registry. Config-list is authoritative in P1 (OQ-5); this table is mirror + health history. verifications.node_id is FK-less (must survive deletion).';
COMMENT ON COLUMN nodes.id IS 'Stable node id. Matches verifications.node_id (text, FK-less).';
COMMENT ON COLUMN nodes.base_url IS 'HTTP endpoint. Unique across nodes (two rows must not point at same box).';
COMMENT ON COLUMN nodes.port25_open IS 'Capability: can this node do SMTP? false/null in P1. Part of audit-green→SMTP-eligible check (GAP-15/24).';

-- =============================================================================
-- policies — policy definitions (versioned, immutable)
-- =============================================================================

CREATE TABLE IF NOT EXISTS policies (
    key text NOT NULL,  -- Policy identifier (e.g., 'cold-email')
    version integer NOT NULL,  -- Immutable version; new version = new row
    definition jsonb NOT NULL,  -- Policy definition (§7.2: order, gates, flags, confidence, etc.)
    is_default boolean NOT NULL DEFAULT false,  -- Marks the single system default
    created_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (key, version)
);

COMMENT ON TABLE policies IS 'Policy definitions. Versioned and immutable: editing a policy = new version row. Every LOG row stamps (policy_key, policy_version).';
COMMENT ON COLUMN policies.definition IS 'Policy definition JSONB (§7.2): order, gates, flags, provider_routing, confidence, recommendation_thresholds. Open object, extensible.';
COMMENT ON COLUMN policies.is_default IS 'Single system-wide default (v1). OQ-23 seam: becomes tier-scoped when tiers arrive.';

-- Partial unique index: exactly one default row (OQ-23, review M7)
CREATE UNIQUE INDEX IF NOT EXISTS idx_policies_single_default
    ON policies (is_default) WHERE is_default = true;

COMMENT ON INDEX idx_policies_single_default IS 'Enforces exactly one system-wide default policy. OQ-23 seam: revisit when tiers arrive (default becomes tier-scoped).';
