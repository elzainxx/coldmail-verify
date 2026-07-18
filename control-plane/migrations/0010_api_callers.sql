-- Migration 0010: api_callers (PN-13 / F-20 — per-caller front-door keys + rotation)
-- Implements: DECISIONS.md PN-13 (DR-5 batch), pre-takeoff-checklist §3.8, Phase-5 spec §7.5.
--
-- WHY THIS IS NOT A FROZEN-DOOR CHANGE (additive-only against all five doors):
--   * api_callers is a NEW operational table. It touches NO frozen schema (verifications/cache/
--     hashing/nodes/policies all unchanged). Auth is a HEADER check only — the caller_id it
--     resolves is DERIVED FROM THE KEY, never read from the request body — so Door 1 (the /verify
--     response shape) and Door 4 (the vocabulary) are byte-unchanged. This is the "auth-strength
--     change behind the SAME /v1/verify(/bulk) contract" PN-13 requires.
--   * By P5 the front door is the BULK-PII submission surface (F-20), yet P1..P4 guard it with a
--     single STATIC shared bearer. This table upgrades to per-caller keys + rotation: the bulk
--     endpoint (/v1/verify/bulk) REQUIRES a per-caller key; the sync /v1/verify additionally
--     accepts the legacy static token during transition (backward-compat, dropped by a flag later).
--
-- THE KEY IS STORED HASHED, NEVER IN CLEAR:
--   key_sha256 = sha256(raw key). NOT a slow KDF: the keys are brain-ISSUED, high-entropy (≥256-bit
--   random) — there is no dictionary to slow, so a KDF buys zero security, while a per-request slow
--   KDF on this HOT, bulk-PII surface is a DoS amplifier. sha256 (fast) for storage/lookup +
--   crypto/subtle.ConstantTimeCompare for the confirming compare is the correct choice for a
--   high-entropy secret. This sha256 is a lookup digest of a bearer key — entirely distinct from
--   the Door-3 HMAC pepper (no pepper involvement, no new pepper exposure).
--
-- caller_id vs caller_ref (GAP-36) — deliberately SEPARATE (a trust boundary):
--   caller_id  = the AUTHENTICATED principal, DERIVED FROM THE KEY (trusted). Opaque — no user PII
--                (the platform holds the user↔caller_id map). This is who is calling.
--   caller_ref = a caller-SUPPLIED correlation label in the request body (jobs.caller_ref, §7.2),
--                UNTRUSTED, echoed back but never interpreted. This is which of the caller's jobs.
--   Merging them would let a caller forge its identity via the body — so they must stay separate.

CREATE TABLE IF NOT EXISTS api_callers (
    caller_id text NOT NULL,             -- opaque authenticated principal (e.g. 'caller_7f3a'); no user PII (GAP-36)
    key_sha256 bytea NOT NULL,           -- sha256(raw key). UNIQUE — one key maps to exactly one caller. The hot auth lookup.
    status text NOT NULL DEFAULT 'active', -- active | rotating_out (informational; validity windows are authoritative)
    not_before timestamptz NOT NULL DEFAULT now(), -- key valid FROM
    not_after timestamptz,               -- key valid UNTIL (NULL = no expiry). Rotation window lives here.
    label text,                          -- opaque admin note (never PII)
    created_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (key_sha256),            -- the lookup is an indexed equality on the hash (no secret-dependent branch)
    CONSTRAINT valid_caller_status CHECK (status IN ('active', 'rotating_out'))
);

COMMENT ON TABLE api_callers IS 'Per-caller front-door keys + rotation (PN-13/F-20, P5 §7.5). key_sha256 = sha256(raw key), stored HASHED never in clear; the raw key exists only transiently in the request header + once at issuance. Auth: header key -> sha256 -> lookup -> validity window [not_before, not_after) -> caller_id on request context. NOT a KDF (keys are high-entropy brain-issued; a slow KDF on this hot bulk-PII surface is a DoS amplifier). caller_id is the AUTHENTICATED principal (trusted, key-derived); distinct from the body-supplied jobs.caller_ref (untrusted). cp_runtime: SELECT only (auth reads it hot); INSERT/UPDATE/DELETE stay cp_admin (key issuance + rotation are admin-audited, DR-3).';
COMMENT ON COLUMN api_callers.caller_id IS 'Opaque authenticated principal derived from the key. No user PII — the platform holds the user↔caller_id map (GAP-36). Distinct from the body-supplied, untrusted jobs.caller_ref.';
COMMENT ON COLUMN api_callers.key_sha256 IS 'sha256(raw bearer key). Stored hashed; compared constant-time (crypto/subtle). NOT the Door-3 pepper HMAC — a lookup digest of a high-entropy issued key.';
COMMENT ON COLUMN api_callers.not_after IS 'Key valid until (NULL = no expiry). Rotation = two active rows for one caller_id with overlapping windows: issue key B (not_after NULL), set key A not_after=now+grace; both authenticate during grace, then A expires. No redeploy, no downtime.';

-- The auth lookup filters on the validity window in SQL (expiry enforced by the query, not app code).
-- key_sha256 is the PK so the equality lookup is already indexed; this partial index speeds the
-- windowed scan for a caller's active keys during rotation.
CREATE INDEX IF NOT EXISTS idx_api_callers_caller ON api_callers (caller_id, not_after);

-- =============================================================================
-- Grants — cp_runtime reads (hot auth); INSERT/UPDATE/DELETE are cp_admin (DR-3 admin-audited)
-- =============================================================================
-- Key issuance and rotation are privileged, admin-audited actions (DR-3): the runtime only READS
-- to authenticate. A runtime that could mint keys would be a privilege-escalation surface.
GRANT SELECT ON api_callers TO cp_runtime;
REVOKE INSERT, UPDATE, DELETE ON api_callers FROM cp_runtime;
GRANT ALL PRIVILEGES ON api_callers TO cp_admin;
