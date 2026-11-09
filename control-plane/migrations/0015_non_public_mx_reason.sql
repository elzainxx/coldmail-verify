-- Migration 0015: the additive verdict reason `non_public_mx` (ADR-0021)
--
-- WHY. The verifier node now refuses to open an SMTP session to a non-public address (loopback, RFC 1918, link-local / cloud
-- metadata, CGNAT, ...) and reports that as a fact; the control plane turns it into status=risky, reason=non_public_mx. The
-- reason vocabulary is frozen-and-additive (Door 4): a value is never renamed or removed, only added -- here, and in
-- internal/vocab/vocab.go, in the same change. Storage mirrors the vocabulary with a CHECK on both tables that carry a reason
-- (`verifications`, the log; `cache`), so both are widened. TestVocabMirrorsTheDatabaseCheckConstraints fails if the two ever drift.
--
-- HOW. Replace the constraint with the old list plus the new value. NOT VALID + VALIDATE keeps the validating scan of the (large,
-- append-only) log out of the ACCESS EXCLUSIVE lock: ADD ... NOT VALID is instant and enforces the rule for new rows at once;
-- VALIDATE then scans under SHARE UPDATE EXCLUSIVE, which does not block inserts.
--
-- Door 2: this is DDL, run by the migration owner (cp_admin), never by cp_runtime.

ALTER TABLE verifications DROP CONSTRAINT IF EXISTS valid_reason;
ALTER TABLE verifications ADD CONSTRAINT valid_reason CHECK (reason IN (
    'invalid_syntax', 'invalid_domain', 'no_mx', 'null_mx', 'dns_error',
    'disposable', 'blacklisted_domain', 'blacklisted_mx', 'spam_trap',
    'role_account', 'whitelisted', 'provider_no_smtp', 'plausible',
    'accepted', 'mailbox_not_found', 'mailbox_full', 'catch_all', 'accept_all',
    'greylisted', 'smtp_blocked', 'smtp_timeout', 'smtp_error', 'connection_failed',
    'external_verified', 'historical_bounce', 'unknown_error',
    'non_public_mx'
)) NOT VALID;
ALTER TABLE verifications VALIDATE CONSTRAINT valid_reason;

ALTER TABLE cache DROP CONSTRAINT IF EXISTS valid_reason;
ALTER TABLE cache ADD CONSTRAINT valid_reason CHECK (reason IN (
    'invalid_syntax', 'invalid_domain', 'no_mx', 'null_mx', 'dns_error',
    'disposable', 'blacklisted_domain', 'blacklisted_mx', 'spam_trap',
    'role_account', 'whitelisted', 'provider_no_smtp', 'plausible',
    'accepted', 'mailbox_not_found', 'mailbox_full', 'catch_all', 'accept_all',
    'greylisted', 'smtp_blocked', 'smtp_timeout', 'smtp_error', 'connection_failed',
    'external_verified', 'historical_bounce', 'unknown_error',
    'non_public_mx'
)) NOT VALID;
ALTER TABLE cache VALIDATE CONSTRAINT valid_reason;
