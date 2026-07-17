-- Migration 0004: send_outcomes + join table (Phase 7 seam)
-- Implements: Phase-1 spec §4.7
-- Created EMPTY in Phase 1 so the shape exists. No P1 code writes here.
-- The join must be verdict-level (verifications.id), not address-level (email_hash).

-- =============================================================================
-- send_outcomes — bounce/delivery events (Phase 7)
-- =============================================================================

CREATE TABLE IF NOT EXISTS send_outcomes (
    -- Identity
    id uuid PRIMARY KEY,  -- Time-ordered (UUIDv7 recommended)

    -- Join anchors
    verification_id uuid,  -- FK to verifications(id) — the verdict that permitted the send
    email_hash bytea,  -- Address-rollup convenience (NOT the primary join)
    provider_canonical_hash bytea,  -- For alias-aware rollups (§3.3.5)
    hash_scheme text,  -- Disambiguates email_hash if scheme is ever migrated

    -- Flywheel dimensions (captured at outcome time, not joined)
    domain text,  -- For per-domain/per-provider rollups without forcing address join
    mx_provider text,  -- Provider dimension for auto-promotion (P7)

    -- Outcome metadata
    message_id text,  -- Sender's message id
    event text,  -- Bounce/complaint/delivered class (classified in P7)
    sent_at timestamptz,  -- When the message was sent (temporal attribution)
    occurred_at timestamptz,  -- When the outcome happened
    raw jsonb,  -- Untouched provider event

    FOREIGN KEY (verification_id) REFERENCES verifications(id) ON DELETE SET NULL,
    CONSTRAINT domain_normalized CHECK (domain IS NULL OR domain = lower(domain))
);

COMMENT ON TABLE send_outcomes IS 'Phase 7 seam: bounce/delivery outcomes. EMPTY in P1. verification_id is the authoritative link to the exact verdict (§4.7). email_hash is rollup convenience only.';
COMMENT ON COLUMN send_outcomes.verification_id IS 'FK to verifications(id): the authoritative link to the verdict that permitted this send. Nullable only if send occurred with no prior verdict.';
COMMENT ON COLUMN send_outcomes.email_hash IS 'Address-rollup convenience. NOT the primary join (would fan out to all historical verdicts per address). Same bytea type as verifications for Phase-7 joins.';
COMMENT ON COLUMN send_outcomes.sent_at IS 'When the message was sent. Required for temporal attribution (which verdict was current at send time).';
COMMENT ON COLUMN send_outcomes.domain IS 'Captured at outcome time. Needed for flywheel per-domain/per-provider rollups without forcing address join.';
COMMENT ON COLUMN send_outcomes.mx_provider IS 'Provider dimension captured at outcome time. Needed for auto-promotion logic (P7).';

-- Note: P7 §3.4 locks ingestion dedup key = (message_id, event).
-- PN-14 flags: enforcement site (DB UNIQUE, app-level guard, or staging table) is a Phase-7 decision.
-- No constraint added here; flagged for P7 design.

-- Indexes (Phase 7 analytics)
CREATE INDEX IF NOT EXISTS idx_send_outcomes_verification_id ON send_outcomes (verification_id) WHERE verification_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_send_outcomes_email_hash ON send_outcomes (email_hash) WHERE email_hash IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_send_outcomes_domain ON send_outcomes (domain) WHERE domain IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_send_outcomes_occurred_at ON send_outcomes (occurred_at);

-- =============================================================================
-- verification_outcomes — many-to-many join table
-- =============================================================================

CREATE TABLE IF NOT EXISTS verification_outcomes (
    verification_id uuid NOT NULL,
    send_outcome_id uuid NOT NULL,

    PRIMARY KEY (verification_id, send_outcome_id),
    FOREIGN KEY (verification_id) REFERENCES verifications(id) ON DELETE CASCADE,
    FOREIGN KEY (send_outcome_id) REFERENCES send_outcomes(id) ON DELETE CASCADE
);

COMMENT ON TABLE verification_outcomes IS 'Many-to-many join: verdicts ↔ outcomes. EMPTY in P1. Allows linking one verdict to multiple later outcomes without altering verifications (append-only).';
