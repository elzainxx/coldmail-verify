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
| `0009_work_items_jobs.sql` | P5 §3–§10 (the durable queue) | `work_items` (UNIQUE idempotency key; `addr_ciphertext` AES-256-GCM, the one bounded plaintext exception; lease `locked_by`/`locked_at`), `jobs`, `work_item_attempts` (APPEND-ONLY transcripts). `cp_runtime` SELECT+INSERT+UPDATE on the first two (DELETE stays `cp_admin`); INSERT+SELECT only on the attempts. |
| `0010_api_callers.sql` | P5 §7.5 / PN-13 (per-caller front-door keys + rotation) | `api_callers` (per-caller key, stored `key_sha256` HASHED, validity window `[not_before, not_after)` for zero-downtime rotation). Grants `cp_runtime` **SELECT only** (hot auth read); INSERT/UPDATE/DELETE stay `cp_admin` (key issuance + rotation are admin-audited, DR-3). Additive — new table; auth is a header check, `caller_id` is key-derived (never from the body), so **no Door-1/Door-4 change**. |
| `0011_external_calls.sql` … `0013_job_owner.sql` | P6 external calls, P7 flywheel/calibration, job owner | see each file header (not re-indexed here) |
| `0014_raw_partition_maintenance.sql` | ADR-0017 (partition cliff fix) | `ensure_verification_raw_partitions(n)` (SECURITY DEFINER, granted to `cp_runtime`) + `ensure_verification_raw_partitions_at(ts, n)` (admin/test core, NOT granted to `cp_runtime`); bootstraps the current month. Future headroom is the `TM_RAW_PARTITION_MONTHS_AHEAD` dial |
| `0015_non_public_mx_reason.sql` | ADR-0021 (additive reason `non_public_mx`) | widens the `valid_reason` CHECK on `verifications` and `cache` (NOT VALID + VALIDATE) |

| `0016_verify_schema.sql` | Platform Phase 1 (one schema per module) | Moves all 21 tables, every `verification_raw` partition and both partition functions from `public` into schema **`verify`**; renames `cp_runtime` → **`verify_app`** (now `LOGIN`) and `cp_admin` → **`verify_admin`**; creates **`verify_owner`** (owns the schema and everything in it); rebuilds the two partition functions for `verify`. Adds exactly two privileges: `INSERT` and `UPDATE (is_default)` on `verify.policies` for `verify_app` (startup `EnsurePolicy` needs them), plus `USAGE` on the schema. See "Since 0016" below. |

Rows `0005`–`0015` describe those files as they were written, so they still say `cp_runtime` / `cp_admin` and unqualified table names; `0016` is what renames the roles and moves the tables.

> **Migration `0009_work_items_jobs.sql` (P5 queue tables) is added by task #84** — it applies before `0010` in lexical order. `0010` (auth) is independent of `0009` and lands first in the P5 gate sequence (PN-13 before the bulk endpoint).

**Apply order:** Run sequentially in the zero-padded filename order (`0001` → … → `0010` → … → `0016`). CI applies every `migrations/NNNN_*.sql` in `sort` order, so a new migration is picked up automatically.

---

## Since 0016: schema `verify` and three roles

After `0016`, **nothing of module 1 is left in `public`** (that schema belongs to the platform; modules 2 and 3 use `send_managed` and `send_byo`). Every object is in schema `verify`, owned by `verify_owner`, and the control plane writes every SQL statement `verify.`-qualified (`verify.cache`, `verify.ensure_verification_raw_partitions(n)`) — no `search_path` setting, no `SET ROLE`, no pool configuration — exactly as modules 2 and 3 write `send_managed.config`.

| Role | Login | Holds |
|---|---|---|
| `verify_app` | **yes** — the control plane's runtime identity (`TM_DATABASE_URL`); formerly `cp_runtime` | exactly the table privileges `cp_runtime` had (carried over by the rename, proven by `TestVerifySchema_PrivilegesCarriedOver`), plus `INSERT` and `UPDATE (is_default)` on `verify.policies` and `USAGE` on the schema. No `CREATE`, owns nothing. |
| `verify_admin` | no — break-glass; formerly `cp_admin` | the same table privileges `cp_admin` had (`ALL` on every table, `EXECUTE` on both partition functions), plus `USAGE` on the schema. Table DML only: it owns nothing, so it cannot run DDL. |
| `verify_owner` | no | owns the schema, every table, partition, index and both functions. DDL (a new partition by hand, `DROP` of an aged partition, `ALTER`) runs as this role: `SET ROLE verify_owner;` first. `0016` grants it to the migrating role when that role is not already a member (a superuser always is). |

- **The runtime password is set out of band**, never in a migration: `ALTER ROLE verify_app PASSWORD '…'`. `0016` only makes the role `LOGIN`.
- **Why `INSERT` + `UPDATE (is_default)` on `policies`:** `EnsurePolicy` runs at startup (`cmd/control-plane/main.go`) and does `INSERT INTO verify.policies … ON CONFLICT DO NOTHING` then `UPDATE verify.policies SET is_default=true …`. Before `0016` the runtime role held `SELECT` only, so a login that held exactly the old grants failed there with SQLSTATE 42501 and the process never served. `UPDATE` is column-level: policy rows stay immutable except for the default flag. Nothing else was widened and nothing unused was pruned (e.g. `DELETE` on `cache` stays; tightening it is a separate decision).
- **Do not re-run `0001`–`0015` on a database that already ran `0016`.** They are unqualified and every `CREATE` is `IF NOT EXISTS`, so they would create empty module-1 tables in `public` again. `0016` then refuses to move anything and stops with SQLSTATE `42P07`, naming the table. Recovery: confirm the `public` copy is empty, drop it, re-run `0016`. (`0016` itself is safe on an empty database and on one already at `0015`; there is still no migration tracker, `Docs/future-work.md` FW-3.)
- **Roles are cluster-wide, tables are per database.** In a cluster with a single module-1 database (production) `0016` renames the two roles. In a cluster with several (CI, the test suite's scratch databases, a shared dev server) the first database renames them; every later database's `0005` creates `cp_runtime` / `cp_admin` again and grants its own tables to them, so `0016` (section 6b) copies that database's table privileges to `verify_app` / `verify_admin`, revokes them from the old role and drops it when no other database still depends on it. Tests that read or change role state therefore take `testpg`'s migration lock (`pg_advisory_lock`), because migrations in parallel test binaries rename, alter and drop the same roles.
- **Counts:** `0016` has 3 top-level `REVOKE` statements (`PUBLIC` on the schema and on each of the two functions; all three on objects it creates itself) and 5 top-level `GRANT` statements (schema `USAGE`, `EXECUTE` on each of the two functions, and the two `policies` privileges). The `GRANT`/`REVOKE` that section 6b issues through `EXECUTE format(…)` are not counted: they only copy privileges that `0005`–`0014` already granted. All 22 `REVOKE`s of the 16 files (19 before `0016`) are accounted for in "No Down Migrations" below.

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

`0005_grants.sql` created two roles (`cp_runtime`, `cp_admin`); `0016` renamed them, and the names below are the current ones. A third role, `verify_owner`, owns the objects (see "Since 0016" above):

| Role | Purpose | Verifications Grants | Other Tables |
|---|---|---|---|
| `verify_app` | Brain's operational credential (maps to Supabase `service_role`) | **INSERT, SELECT only** (NO UPDATE, NO DELETE) | Full read/write on `cache`; INSERT+UPDATE on `catchall_domains` (0006); INSERT+SELECT on `node_audits` (append-only, 0007); SELECT+INSERT+UPDATE on `node_warmup` + INSERT+UPDATE on `nodes` (0007); SELECT+INSERT+UPDATE on `rate_budgets` (counter, 0008); read-only on other lists; on `policies` SELECT, INSERT and UPDATE (`is_default`) only (0016) |
| `verify_admin` | grant-admin (break-glass, admin-audited); owns nothing, so DDL needs `verify_owner` | ALL table privileges (DML + TRUNCATE; no DDL) | ALL (incl. the only DELETE on `node_audits`/`node_warmup`/`nodes`/`rate_budgets`) |

**P3 append-only note (0007):** `node_audits` carries the LOG's own posture — `verify_app` gets
INSERT+SELECT, DELETE is revoked, so audit *history* (a green→red→green flap record P4 needs) can
never be destroyed by the runtime. Proven live: as `verify_app`, `INSERT node_audits` succeeds and
`DELETE FROM node_audits` raises `permission denied` (same for `node_warmup`/`nodes` DELETE).

**P4 counter-posture note (0008):** `rate_budgets` mirrors `node_warmup` — `verify_app` gets
SELECT+INSERT+UPDATE (the durable token-bucket spend the limiter increments), DELETE revoked. Proven
live: as `verify_app`, `INSERT`/`UPDATE rate_budgets` succeed and `DELETE FROM rate_budgets` raises
`permission denied`. The `recovering` value is accepted by the widened `nodes.status` CHECK.

**Why:** The LOG (`verifications`) is append-only by **revoked grants**, not convention. The runtime service role *cannot* update or delete historical rows — corrections are new rows. PN-9 hardens this: `verify_admin` is a separate, audited credential (not held by the runtime process), so the runtime cannot silently rewrite history.

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

All migrations use `CREATE TABLE IF NOT EXISTS` and `CREATE INDEX IF NOT EXISTS`. Re-running a migration is safe (no-op if already applied) **up to `0015`, and only on a database that has not run `0016`**: `0001`–`0015` create their tables unqualified, i.e. in `public`, so re-running them after `0016` re-creates empty module-1 tables there (see "Since 0016"). `0016` itself is safe on an empty database and on one already at `0015`. For production: track applied migrations in a `schema_migrations` table or use a migration tool (e.g., `golang-migrate`, `dbmate`).

---

## No Down Migrations

**Deliberate omission.** The LOG is append-only and becomes historical data; you **never roll back the LOG** (§3.2.1). If a migration fails, fix forward. Down-migrations are not provided because they would violate the append-only contract.

**The scope of that reason, stated exactly (added 2026-08-07, re-measured 2026-11-15 over 16 migrations).** It
explains the **LOG**, and it does not reach two other things these files carry. (a) **Grants** — 22 `REVOKE`
statements across 11 files (`grep -cE '^\s*REVOKE\b' *.sql`): 17 are least-privilege `REVOKE … FROM cp_runtime`
tightenings (`cp_runtime` is the role `0016` renames to `verify_app`), 2 (in `0014`) remove the default `PUBLIC` execute
on two functions, and 3 (in `0016`) remove the default `PUBLIC` access to the schema `verify` and to the two functions
it creates; the 2026-08-07 line-by-line review
found that every REVOKE then present revoked a privilege its *own* migration granted (the three in `0016` revoke the
default `PUBLIC` access on objects that same file creates), so none silently strips a
privilege an earlier migration relied on. (b) **Constraints** — 4 `DROP CONSTRAINT IF EXISTS` (`0008` `nodes.valid_status`,
`0012` `send_outcomes.send_outcome_calibration_class`, `0015` `verifications.valid_reason` and `cache.valid_reason`), each
recreating a **widened** version; reverting the file leaves the widened constraint live, so the code believes a narrower
invariant than the database enforces. Neither is append-only data, and neither has a reverse path. Also note what
`git revert` does *not* do: it deletes the file and executes nothing, so a database that already ran the migration keeps
the DDL while a fresh one built from the reverted tree does not — measured on `0013` (two columns + an index present in
one, absent in the other), with no tracking table to distinguish them (adopting one is `Docs/future-work.md` FW-3). The
go-live consequences and the checklist are in [`../../Docs/operator-runbook.md`](../../Docs/operator-runbook.md) §9 (the
original write-up is [`../../Docs/history/go-live-rollback-posture.md`](../../Docs/history/go-live-rollback-posture.md)).

---

## Adding Partitions (verification_raw)

Example partitions (2026-07 through 2026-09) are created in `0001_verifications.sql`. **Production:** the control plane keeps future partitions created itself (ADR-0017: `verify.ensure_verification_raw_partitions(n)`, every `TM_RAW_PARTITION_MAINTAIN_INTERVAL_SECONDS`); the SQL below is the by-hand equivalent. DDL needs the table owner, `verify_owner` (`verify_admin` owns nothing and cannot run it):

```sql
-- Example: add October 2026 partition, by hand
SET ROLE verify_owner;
CREATE TABLE verify.verification_raw_2026_10 PARTITION OF verify.verification_raw
    FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');

GRANT SELECT, INSERT ON verify.verification_raw_2026_10 TO verify_app;
GRANT ALL PRIVILEGES ON verify.verification_raw_2026_10 TO verify_admin;
RESET ROLE;
```

**Retention (after ~90 days):**
```sql
-- Owner action only (verify_owner); verify_app and verify_admin cannot drop a partition
SET ROLE verify_owner;
DROP TABLE verify.verification_raw_2026_07;
RESET ROLE;
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

- Phase-1 spec: `Docs/history/phase-1-foundations-spec.md`
- Project rules (Doors 1–5): `PROJECT_RULES.md`
- Decision log: `Docs/open-decisions.md`

---

**Ready to apply.** Run migrations `0001` → `0016` in zero-padded filename order against your PostgreSQL 14+ / Supabase instance, once (see "Since 0016" for what must not be re-run).
