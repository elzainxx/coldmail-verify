-- Migration 0013: jobs.owner_user_id — scope every bulk-job read to the authenticated caller.
-- Implements: the alignment fix plan's Tier-0 item 1 (finding D1), canonical-module-contract §1.4,
--             and the A6 ruling (M1 adopts the PLATFORM's user id, not its own caller_id text space).
--
-- WHAT WAS WRONG (D1 — the audit's only FAIL grade):
--   The front door authenticates a per-caller key and resolves a caller_id (0010_api_callers.sql),
--   then DISCARDS it (`_ = callerID`) and reads jobs by id alone. All three read paths —
--   GetJob, JobCompletion, ListJobResults — scoped on job_id with NO owner predicate. So any
--   authenticated caller could read any other caller's bulk job status and full verification
--   results by job_id. The exposure is verdicts/domains/flags/counts — real PII — though NOT raw
--   addresses (a bulk result blanks Email; the plaintext is purged on settle, Door 3).
--
--   It was a prerequisite gate rather than a live breach only because no second untrusted caller
--   exists yet: api_callers INSERT is reserved to cp_admin (0010:43) and there is no issuance route
--   or CLI anywhere in the tree. That is exactly why this lands BEFORE multi-tenant key issuance,
--   not after. RLS is deliberately absent (0005_grants.sql: "no RLS (single client = brain)"), so
--   there was no backstop underneath the missing predicate either.
--
-- WHY THIS IS NOT A FROZEN-DOOR CHANGE (additive-only against all five doors):
--   * Door 1 (the /verify response shape) is BYTE-UNCHANGED. This adds no field to any response.
--     The bulk result objects are the same verdict objects; a job simply becomes invisible to a
--     caller that does not own it. No field removed, renamed, retyped, or re-meant.
--   * Door 2 (the append-only LOG) is UNTOUCHED — not one column on `verifications`, not one row
--     mutated. `jobs` is a P5 OPERATIONAL table (0009), never the LOG.
--   * Door 3 (hashing) unchanged: owner_user_id is a platform user id, not an address, and is
--     never hashed, never derived from an address, and never reaches the pepper.
--   * Door 4 (the status/reason/recommendation vocabulary) unchanged — this mints no vocabulary.
--   * Door 5 (the pinned gem / the node) unchanged — entirely control-plane side.
--   ADDITIVE COLUMN, NULLABLE, no default: an ALTER TABLE ADD COLUMN that cannot fail on existing
--   rows and cannot change the meaning of any column that already exists.
--
-- WHY NULLABLE, AND WHY THAT IS SAFE (the backfill decision, stated out loud):
--   Pre-0013 jobs have no recorded owner and there is NO WAY to invent one — the information was
--   never captured. Three options existed: (a) NOT NULL + backfill to a sentinel owner, (b) delete
--   pre-existing rows, (c) NULLABLE, meaning "submitted before ownership was recorded".
--   (a) is a lie (it asserts an owner the system never knew, and one sentinel owner would make
--   every legacy job readable by whoever holds that id). (b) destroys operational history — and
--   `jobs` DELETE is cp_admin-only precisely because history is not the runtime's to destroy.
--   (c) is honest. The read predicate is written so a NULL owner matches NO caller (see below),
--   which makes the legacy rows READ-CLOSED rather than read-open: the fail-safe direction. A
--   legacy job becomes invisible over the bulk API — operationally visible only to cp_admin via
--   SQL — which is the correct trade for a row whose owner is genuinely unknown.
--
-- WHY `owner_user_id uuid` AND NOT `caller_id text` (the A6 ruling):
--   Two tenant identity spaces coexisted across the three modules: the senders key on the
--   platform's public.users.id (uuid); M1 kept its own api_callers.caller_id (text) and stored it
--   on no row. Ruling: M1 adopts THE PLATFORM'S user id. So the column is `uuid`, matching the
--   senders' user_id columns, and a cross-module "everything for this user" query needs no mapping
--   layer. api_callers.caller_id remains M1's AUTHENTICATION principal (the thing a key resolves
--   to); owner_user_id is the AUTHORIZATION subject stored on the row. They are different jobs and
--   both are needed — see 0010's own caller_id-vs-caller_ref note for the same distinction.
--
-- THE TRUST BOUNDARY IS UNCHANGED — THIS IS THE LOAD-BEARING PART:
--   owner_user_id is populated from the platform user id CARRIED BY THE AUTHENTICATED KEY
--   (api_callers.user_id, added below), NEVER from the request body. A caller cannot assert its own
--   owner. This is the "credential-based" side of contract §1.4 clause 6 — the stronger of the two
--   coherent options, and the one M1's api_callers already made possible. It is deliberately NOT
--   the senders' selector-based model (where the caller supplies user_id and the credential
--   authorises the platform): M1 authenticates the CALLER, so taking identity from the body would
--   let any key read any tenant — reintroducing D1 through the front door it was just closed at.
--
-- NO FOREIGN KEY TO public.users, DELIBERATELY:
--   M1 has no users table. It is a standalone verification service with its own database (the
--   senders share the platform DB; M1 does not). A cross-database FK is not expressible, and
--   inventing a local users table would import a platform concept the §7.6/GAP-36 boundary
--   explicitly fences out. So owner_user_id is an FK-LESS uuid — the same posture as
--   verifications.node_id ("FK-less text — logged node must survive node deletion"). The uuid TYPE
--   still gives the A6 alignment; the FK would only give referential integrity M1 cannot host.

-- =============================================================================
-- api_callers.user_id — the platform user a key belongs to (the authorization subject)
-- =============================================================================
-- The key already resolves to a caller_id. This adds the PLATFORM user id the key acts for, so the
-- front door can stamp an owner on a job without ever trusting the body. NULLABLE: a key issued
-- before this migration (and the dev/legacy static-token path) has no platform user, and a NULL
-- here must NOT silently become a readable owner — see the predicate note below.
ALTER TABLE api_callers ADD COLUMN IF NOT EXISTS user_id uuid;

COMMENT ON COLUMN api_callers.user_id IS 'The PLATFORM user (public.users.id in the platform''s own DB) this key acts for — the AUTHORIZATION subject, stamped onto jobs.owner_user_id at submit (A6 ruling; fix-plan item 1 / D1). FK-LESS by design: M1 has its own database and no users table (§7.6/GAP-36 fences platform concepts out), so the uuid TYPE carries the cross-module alignment while referential integrity stays with the platform. DISTINCT from caller_id (the authentication principal the key resolves to) and from jobs.caller_ref (untrusted, body-supplied). NULL = a key issued before ownership was recorded; such a key can submit but its jobs are read-closed, never read-open.';

-- =============================================================================
-- jobs.owner_user_id — the owner predicate every read path now carries
-- =============================================================================
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS owner_user_id uuid;

COMMENT ON COLUMN jobs.owner_user_id IS 'The PLATFORM user that owns this bulk job (fix-plan item 1 / D1; contract §1.4). Populated at submit from the AUTHENTICATED key''s api_callers.user_id — NEVER from the request body, so a caller cannot assert ownership. Every read path (GetJob, JobCompletion, ListJobResults) predicates on it and returns 404, never 403: a job owned by someone else is indistinguishable from a job that does not exist (no cross-tenant existence disclosure, matching the senders'' posture). NULL = submitted before ownership was recorded; the predicate is IS NOT DISTINCT FROM-free and NULL-matches-nobody, so legacy rows are read-CLOSED (fail-safe), reachable only by cp_admin via SQL.';

-- The read predicate is (id = $1 AND owner_user_id = $2). Under SQL''s three-valued logic
-- NULL = <anything> is NULL, not true — so a legacy NULL-owner row matches no caller, which is the
-- fail-safe direction we want and the reason no COALESCE/sentinel appears anywhere here.
-- This index serves that exact two-column equality. Not UNIQUE: one owner has many jobs.
CREATE INDEX IF NOT EXISTS idx_jobs_owner ON jobs (owner_user_id, id);

-- Restate the table comment: the original said "NO user id" as a DESIGN STATEMENT, and that premise
-- (the platform owns the user-facing job; the module holds only work state) is still correct and is
-- NOT being reversed. An OWNER column is not user-facing job state — no credits, no CSV, no
-- filename, no per-row UI, no PII beyond an opaque uuid. The omission rested on a "single client"
-- assumption that 0010's multi-caller auth had ALREADY falsified; this closes that gap without
-- moving the §7.6/GAP-36 boundary one inch.
COMMENT ON TABLE jobs IS 'Bulk job handles (P5 §7.2) — the BRAIN''s verification-work scheduler, NOT the platform''s user-facing job (§7.6/GAP-36). Holds counts/status/policy + an OPAQUE caller_ref + the owning platform user id (owner_user_id, 0013/D1); NO credits, CSV, filename, or per-row UI state. owner_user_id is an AUTHORIZATION scope, not user-facing job state — the boundary is unchanged. Completion is authoritative (NOT EXISTS work_items NOT IN (settled,dead)); settled_count is cheap progress only (GAP-38). cp_runtime: SELECT+INSERT+UPDATE; DELETE stays cp_admin.';

-- =============================================================================
-- Grants — nothing new is needed, stated explicitly so the absence is not a question
-- =============================================================================
-- 0009 already grants cp_runtime SELECT+INSERT+UPDATE on jobs and 0010 grants SELECT on
-- api_callers; a column added to an already-granted table inherits the table grant. cp_runtime
-- must NOT gain INSERT/UPDATE on api_callers — issuing a key, or repointing a key's user_id, stays
-- cp_admin (DR-3, admin-audited): a runtime that could set user_id could reassign ownership, which
-- is the same privilege-escalation surface 0010 revoked INSERT to prevent.
REVOKE INSERT, UPDATE, DELETE ON api_callers FROM cp_runtime;
