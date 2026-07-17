-- Migration 0002: cache
-- Implements: Phase-1 spec §4.2
-- Composite-key cache: (email_hash, config_fingerprint) for correctness.
-- Cache hits are still logged to verifications with from_cache=true.

-- =============================================================================
-- cache — verdict cache with composite key
-- =============================================================================

CREATE TABLE IF NOT EXISTS cache (
    -- Composite primary key (email_hash, config_fingerprint)
    -- Correctness fix (review B1): verdict is a function of (address × policy × config)
    email_hash bytea NOT NULL,  -- Never NULL here (unhashable rejects are not cached)
    config_fingerprint text NOT NULL,  -- Always computable in P1 (§3.2.5)

    -- Supporting columns
    domain text NOT NULL,  -- For domain-scoped invalidation/analytics
    mx_provider text,  -- Mirror of promoted column for provider analytics
    hash_scheme text NOT NULL,  -- Which scheme produced the key

    -- Cached verdict
    status text NOT NULL,
    reason text NOT NULL,
    confidence smallint NOT NULL,
    recommendation text NOT NULL,
    flags jsonb NOT NULL,
    signals jsonb NOT NULL,

    -- TTL metadata
    cached_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,  -- NOT NULL: verdicts go stale, TTL value is a dial

    PRIMARY KEY (email_hash, config_fingerprint),

    -- Integrity constraints (mirror verifications)
    CONSTRAINT valid_confidence CHECK (confidence BETWEEN 0 AND 100),
    CONSTRAINT valid_status CHECK (status IN (
        'valid', 'invalid', 'risky', 'unknown'
    )),
    CONSTRAINT valid_recommendation CHECK (recommendation IN (
        'send', 'review', 'skip'
    )),
    CONSTRAINT valid_reason CHECK (reason IN (
        'invalid_syntax', 'invalid_domain', 'no_mx', 'null_mx', 'dns_error',
        'disposable', 'blacklisted_domain', 'blacklisted_mx', 'spam_trap',
        'role_account', 'whitelisted', 'provider_no_smtp', 'plausible',
        'accepted', 'mailbox_not_found', 'mailbox_full', 'catch_all', 'accept_all',
        'greylisted', 'smtp_blocked', 'smtp_timeout', 'smtp_error', 'connection_failed',
        'external_verified', 'historical_bounce', 'unknown_error'
    )),
    CONSTRAINT domain_normalized CHECK (domain = lower(domain))
);

COMMENT ON TABLE cache IS 'Verdict cache with composite key (email_hash, config_fingerprint). Cache hits still logged with from_cache=true. TTL value is a dial (§4.2).';
COMMENT ON COLUMN cache.email_hash IS 'Cache key part 1: HMAC-SHA256 of normalized email (bytea). Always NOT NULL here.';
COMMENT ON COLUMN cache.config_fingerprint IS 'Cache key part 2: fingerprint of engine+layer+policy. Ensures verdict matches config.';
COMMENT ON COLUMN cache.expires_at IS 'TTL expiration time. NOT NULL — verdicts go stale. Value is a dial (~90d recommendation).';

-- Index for TTL sweeps
CREATE INDEX IF NOT EXISTS idx_cache_expires_at ON cache (expires_at);
