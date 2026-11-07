# Phase 1 Database Migrations

**Status:** Forward-only SQL DDL translating the approved Phase-1 spec (§3.2–§4.7).  
**Target:** PostgreSQL 14+ via Supabase.  
**Principle:** Append-only log + Door 2 grant enforcement.

---

## Migration Files (apply in order)

| File | Implements | Key Tables |
|---|---|---|
| `0001_verifications.sql` | §3.2 (Door 2), §4.1 | `verifications` (THE LOG), `verification_raw` (sidecar, partitioned) |
| `0002_cache.sql` | §4.2 | `cache` (composite key: `email_hash`, `config_fingerprint`) |
| `0003_lists_nodes_policies.sql` | §4.3–§4.6 | `catchall_domains`, `disposable_domains`, `spam_trap_domains`, `nodes`, `policies` |
| `0004_outcomes_seam.sql` | §4.7 (Phase 7 seam) | `send_outcomes`, `verification_outcomes` (empty in P1) |
| `0005_grants.sql` | §3.2.1 (PN-9, Door 2 enforcement) | Role grants: `cp_runtime` (INSERT+SELECT only), `cp_admin` (DDL owner) |
| `0006_catchall_write.sql` | P2 §5.6 (brain writes the catch-all probe result) | Grants `cp_runtime` INSERT+UPDATE on `catchall_domains` (DELETE stays `cp_admin`). Additive — NOT the append-only LOG. |
| `0007_node_audits_warmup.sql` | P3 §4.3/§4.5 (audit history + gate), §5.2 (warmup) | `node_audits` (APPEND-ONLY audit history, OQ-P3-5), `node_warmup` (durable per-node/per-window ramp counter, §5.2). Grants `cp_runtime` INSERT+SELECT on `node_audits` (DELETE revoked — LOG posture), SELECT+INSERT+UPDATE on `node_warmup`, and INSERT+UPDATE on `nodes` (DELETE stays `cp_admin`). Additive — new tables, reserved `nodes` columns populated, no frozen-schema change. |
| `0008_rate_budgets_recovering.sql` | P4 §4.4 (durable rate budget), §5.4/GAP-30 (canary sub-state) | `rate_budgets` (durable per-(node×provider) token-bucket spend — restart-safe, §4.4). Grants `cp_runtime` SELECT+INSERT+UPDATE on `rate_budgets` (DELETE revoked — counter posture, mirrors `node_warmup`). Widens the `nodes.status` CHECK to add `recovering` (the canary sub-state — a NODE-LIFECYCLE value, NOT verdict vocab; ADR-0010). Additive — new table + a strict CHECK widening, no frozen-schema change. |
| `0010_api_callers.sql` | P5 §7.5 / PN-13 (per-caller front-door keys + rotation) | `api_callers` (per-caller key, stored `key_sha256` HASHED, validity window `[not_before, not_after)` for zero-downtime rotation). Grants `cp_runtime` **SELECT only** (hot auth read); INSERT/UPDATE/DELETE stay `cp_admin` (key issuance + rotation are admin-audited, DR-3). Additive — new table; auth is a header check, `caller_id` is key-derived (never from the body), so **no Door-1/Door-4 change**. |
| `0011_external_calls.sql` … `0013_job_owner.sql` | P6 external calls, P7 flywheel/calibration, job owner | see each file header (not re-indexed here) |
| `0014_raw_partition_maintenance.sql` | ADR-0017 (partition cliff fix) | `ensure_verification_raw_partitions(n)` (SECURITY DEFINER, granted to `cp_runtime`) + `ensure_verification_raw_partitions_at(ts, n)` (admin/test core, NOT granted to `cp_runtime`); bootstraps the current month. Future headroom is the `TM_RAW_PARTITION_MONTHS_AHEAD` dial |

> **Migration `0009_work_items_jobs.sql` (P5 queue tables) is added by task #84** — it applies before `0010` in lexical order. `0010` (auth) is independent of `0009` and lands first in the P5 gate sequence (PN-13 before the bulk endpoint).

**Apply order:** Run sequentially in the zero-padded filename order (`0001` → … → `0008` → `0009` → `0010`). CI applies every `migrations/NNNN_*.sql` in `sort` order, so a new migration is picked up automatically.

---

## How to Apply

### Local Docker PostgreSQL

```bash
# Start a local Postgres 14+ container
docker run --name truemail-pg -e POSTGRES_PASSWORD=localdev -p 5432:5432 -d postgres:14

# Apply migrations
for f in control-plane/migrations/*.sql; do
  psql -h localhost -U postgres -d postgres -f "$f"
done
```

### Supabase

1. **Via Supabase Dashboard:**
   - Go to SQL Editor
   - Paste each migration file in order
   - Execute

2. **Via Supabase CLI:**
   ```bash
   # Link project (one-time)
   supabase link --project-ref <your-project-ref>

   # Apply migrations
   supabase db push
   ```

3. **Via `psql` to Supabase connection string:**
   ```bash
   # Get connection string from Supabase dashboard (Database → Connection string)
   export SUPABASE_URI="postgresql://postgres:[PASSWORD]@db.[PROJECT].supabase.co:5432/postgres"

   for f in control-plane/migrations/*.sql; do
     psql "$SUPABASE_URI" -f "$f"
   done
   ```

---

## Append-Only Grant Model (Door 2 Enforcement)

The `0005_grants.sql` migration implements two roles:

| Role | Purpose | Verifications Grants | Other Tables |
|---|---|---|---|
| `cp_runtime` | Brain's operational credential (maps to Supabase `service_role`) | **INSERT, SELECT only** (NO UPDATE, NO DELETE) | Full read/write on `cache`; INSERT+UPDATE on `catchall_domains` (0006); INSERT+SELECT on `node_audits` (append-only, 0007); SELECT+INSERT+UPDATE on `node_warmup` + INSERT+UPDATE on `nodes` (0007); SELECT+INSERT+UPDATE on `rate_budgets` (counter, 0008); read-only on other lists/policies |
| `cp_admin` | DDL owner / grant-admin (break-glass, admin-audited) | ALL (for migrations, partitions) | ALL (incl. the only DELETE on `node_audits`/`node_warmup`/`nodes`/`rate_budgets`) |

**P3 append-only note (0007):** `node_audits` carries the LOG's own posture — `cp_runtime` gets
INSERT+SELECT, DELETE is revoked, so audit *history* (a green→red→green flap record P4 needs) can
never be destroyed by the runtime. Proven live: as `cp_runtime`, `INSERT node_audits` succeeds and
`DELETE FROM node_audits` raises `permission denied` (same for `node_warmup`/`nodes` DELETE).

**P4 counter-posture note (0008):** `rate_budgets` mirrors `node_warmup` — `cp_runtime` gets
SELECT+INSERT+UPDATE (the durable token-bucket spend the limiter increments), DELETE revoked. Proven
live: as `cp_runtime`, `INSERT`/`UPDATE rate_budgets` succeed and `DELETE FROM rate_budgets` raises
`permission denied`. The `recovering` value is accepted by the widened `nodes.status` CHECK.

**Why:** The LOG (`verifications`) is append-only by **revoked grants**, not convention. The runtime service role *cannot* update or delete historical rows — corrections are new rows. PN-9 hardens this: `cp_admin` is a separate, audited credential (not held by the runtime process), so the runtime cannot silently rewrite history.

**Retention note:** `verification_raw` is RANGE-partitioned by month. Aging out old engine blobs is `DROP PARTITION` (admin action), never `UPDATE`/`DELETE` on rows. This keeps `verifications` truly append-only (§3.2.6).

---

## Schema Key Points (from the spec)

### verifications (THE LOG)
- **Append-only, indefinite retention.** Every result logged before responding.
- `id`: App-generated **UUIDv7** (time-ordered for insert locality).
- `email_hash`: **bytea (32 bytes)**, NULLABLE by design (invalid_syntax/unparseable → NULL, §3.2.7).
- `hash_scheme`: Always set when `email_hash` is set; NULL otherwise (CHECK constraint enforces linkage).
- `domain`: Plain **text**, not `citext` (already normalized by `norm/v1` to lowercase/punycode).
- `mx_provider`: **Promoted typed column** (was jsonb in early draft) — permanent flywheel grouping dimension (review M3).
- `config_fingerprint`: **NOT NULL** — always computable in P1; part of cache key (review M8).
- `from_cache`: Calibration excludes rows with `from_cache=true`.
- `node_id`: **FK-less text** — logged node must survive node deletion from `nodes` table.
- Vocabulary CHECK constraints: `status`, `reason`, `recommendation` are enum-guarded at storage layer (Door 4). Adding a value = additive migration (ALTER CHECK).

### verification_raw (sidecar)
- **1:1 with verifications**, RANGE-partitioned by `created_at` (monthly).
- **The ONLY retention-managed store.** Retention = `DROP PARTITION` after ~90 days (§3.2.6, OQ-10).
- Keeps `verifications` truly append-only (never UPDATEd to null a column).

### cache
- **Composite PK: `(email_hash, config_fingerprint)`** (review B1 correctness fix).
- Cache key includes config so a policy/config change is naturally cache-invalidating.
- `expires_at`: **NOT NULL** — verdicts go stale; TTL value is a dial (~90d).
- Cache hits are still **logged** to `verifications` with `from_cache=true` (§4.2, OQ-9).

### catchall_domains
- `is_catchall`: **Tri-state via NULL** (true/false/unknown). Collapse to boolean in response flag is safe; analytics read this table for the tri-state.

### policies
- **Composite PK: `(key, version)`.** Policies are versioned and immutable (edit = new version row).
- `is_default`: Partial unique index `WHERE is_default = true` — exactly one system-wide default (OQ-23). **Seam:** becomes tier-scoped when tiers arrive (§4.6).

### send_outcomes + verification_outcomes (Phase 7 seam)
- **Empty in Phase 1.** Shape exists so P7 joins cleanly.
- `verification_id` is the **authoritative join** (specific verdict). `email_hash` is rollup convenience only (would fan out to all historical verdicts per address if used as primary join).
- `domain`, `mx_provider`, `hash_scheme` captured at outcome time → flywheel rollups without forcing address join (review M2).
- PN-14 flags: ingestion dedup key `(message_id, event)` locked in P7 §3.4; enforcement site (DB UNIQUE vs app-level guard) is a P7 decision.

---

## Idempotency

All migrations use `CREATE TABLE IF NOT EXISTS` and `CREATE INDEX IF NOT EXISTS`. Re-running a migration is safe (no-op if already applied). For production: track applied migrations in a `schema_migrations` table or use a migration tool (e.g., `golang-migrate`, `dbmate`).

---

## No Down Migrations

**Deliberate omission.** The LOG is append-only and becomes historical data; you **never roll back the LOG** (§3.2.1). If a migration fails, fix forward. Down-migrations are not provided because they would violate the append-only contract.

**The scope of that reason, stated exactly (added 2026-08-07).** It explains the **LOG**, and it does
not reach two other things these files carry. (a) **Grants** — 15 `REVOKE`s across 8 migrations. All 15
revoke privileges granted by their *own* migration (least-privilege hardening in the same file), so
none silently strips a privilege an earlier migration relied on — verified statement by statement. (b)
**Constraints** — `0008` and `0012` each `DROP CONSTRAINT IF EXISTS` and recreate a widened version;
reverting the file leaves the widened constraint live, so the code believes a narrower invariant than
the database enforces. Neither is append-only data, and neither has a reverse path. Also note what
`git revert` does *not* do: it deletes the file and executes nothing, so a database that already ran
the migration keeps the DDL while a fresh one built from the reverted tree does not — measured on
`0013` (two columns + an index present in one, absent in the other), with no tracking table to
distinguish them. The go-live consequences and the checklist are in
[`../../Docs/go-live-rollback-posture.md`](../../Docs/go-live-rollback-posture.md).

---

## Adding Partitions (verification_raw)

Example partitions (2026-07 through 2026-09) are created in `0001_verifications.sql`. **Production:** automate partition creation ahead of time (e.g., cron job creating next month's partition).

```sql
-- Example: add October 2026 partition
CREATE TABLE verification_raw_2026_10 PARTITION OF verification_raw
    FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');

GRANT SELECT, INSERT ON verification_raw_2026_10 TO cp_runtime;
GRANT ALL PRIVILEGES ON verification_raw_2026_10 TO cp_admin;
```

**Retention (after ~90 days):**
```sql
-- Admin action only (cp_admin)
DROP TABLE verification_raw_2026_07;
```

---

## Phase 7 Note (Outcomes)

The `send_outcomes` and `verification_outcomes` tables are created empty. **No Phase 1 code writes to them.** They exist so the shape is locked now (§4.7 seam). When the sender comes online (Phase 7):
- Bounces link to **`verifications.id`** (specific verdict), not just `email_hash` (address).
- `sent_at` captured for temporal attribution ("which verdict was current at send time").
- Join is many-to-many via `verification_outcomes` (one verdict → multiple later outcomes, no mutation of `verifications`).

---

## Resolved Review Findings (already in the design)

All 25 open questions signed off (Appendix C). Key findings already resolved:
- **B1 (cache correctness):** Composite key `(email_hash, config_fingerprint)`.
- **B2 (join-key type):** `bytea` everywhere (32-byte HMAC).
- **B3 (null-hash):** `email_hash` nullable; `invalid_syntax` → NULL (§3.2.7).
- **M1 (P7 join):** `verifications.id` is authoritative; `email_hash` demoted to rollup.
- **M2 (outcomes missing fields):** `domain`, `mx_provider`, `hash_scheme`, `sent_at` added.
- **M3 (mx_provider buried):** Promoted to typed column from row #1.
- **M4 (append-only vs retention):** `raw` moved to partitioned sidecar; retention = DROP PARTITION.
- **M5 (citext):** Plain `text` + CHECK lowercase (redundant case-folding avoided).
- **M6 (random PK):** Time-ordered UUIDv7 (app-generated).
- **M7 (multi-default):** Partial unique index on `is_default`.
- **M8 (nullable fingerprint):** `config_fingerprint` NOT NULL.

---

## Questions / Ambiguities

**None flagged.** The spec is explicit, and all 25 open questions are signed off (§9, Appendix C). Every constraint, nullability, and enum value is locked. If a genuine ambiguity is found during implementation, surface it (do not silently resolve).

---

## References

- Phase-1 spec: `Docs/phase-1-foundations-spec.md`
- Project rules (Doors 1–5): `PROJECT_RULES.md`
- Decision log: `Docs/DECISIONS.md`

---

**Ready to apply.** Run migrations `0001` → `0010` in zero-padded filename order against your PostgreSQL 14+ / Supabase instance.
