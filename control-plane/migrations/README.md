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

**Apply order:** Run sequentially `0001` → `0002` → `0003` → `0004` → `0005`.

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
| `cp_runtime` | Brain's operational credential (maps to Supabase `service_role`) | **INSERT, SELECT only** (NO UPDATE, NO DELETE) | Full read/write on `cache`; read-only on lists/policies |
| `cp_admin` | DDL owner / grant-admin (break-glass, admin-audited) | ALL (for migrations, partitions) | ALL |

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

**Ready to apply.** Run migrations `0001` → `0005` in order against your PostgreSQL 14+ / Supabase instance.
