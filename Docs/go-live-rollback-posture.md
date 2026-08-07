# M1's rollback posture at go-live — what "reversible" does and does not mean here

**Date:** 2026-08-07 · **Status:** a go-live CHECKLIST item, not a defect report. Nothing below is a bug
in the build. It records a property of the migration layer that an operator must know **before** the
first non-local deploy, because the moment two databases exist the cheap fix stops being cheap.

**Why this file exists.** [`go-live-runbook.md`](go-live-runbook.md) opens with "Each stage is
independently reversible", and for what that sentence is about — flags, config, and env-gated layers —
it is true and was designed that way (Door 5, GAP-21/31: every stage is a config flip, zero code
changes). Stage 2's vendor layer really does turn off by unsetting a variable. Stage 3's feed really
does fail closed. **The migration layer is the exception, and it is the one layer where reversal
means touching data.** A reader who takes the headline sentence to cover schema changes will reach for
`git revert` and get a result that is worse than either doing nothing or rolling forward.

---

## 1. The measured facts

Measured 2026-08-07 against a real PostgreSQL 16, applying `control-plane/migrations/*.sql` in lexical
order to throwaway databases — the same way [`ci.yml`](../.github/workflows/ci.yml) does it. Not
inferred from reading the files.

| Fact | Measured value |
|---|---|
| `.down.sql` files in the repo | **0** (of 13 migrations) |
| Any other down/rollback mechanism | **none** — no tool, no `-down` flag, no reverse script |
| A `schema_migrations` / version-tracking table | **none exists** |
| How migrations are applied | `for f in $(ls migrations/[0-9][0-9][0-9][0-9]_*.sql | sort); do psql -f "$f"; done` |
| `REVOKE` statements | **15**, across 8 migrations |
| `ALTER TABLE … DROP CONSTRAINT` | **2** (`0008` on `nodes.valid_status`, `0012` on `send_outcomes`) |

This is **forward-only by design**, and the design is recorded: `migrations/README.md` §"No Down
Migrations" says down-migrations "would violate the append-only contract" of the LOG. That rationale is
sound *for the LOG*. §3 below is about where it stops applying.

## 2. What `git revert` of a migration actually does

Measured on two databases built from the same repo, differing only in whether `0013_job_owner.sql` ran:

| | ran 0013 | 0013 reverted / never ran |
|---|---|---|
| `api_callers.user_id` column | present | **absent** |
| `jobs.owner_user_id` column | present | **absent** |
| `idx_jobs_owner` index | present | **absent** |

`git revert` deletes the migration **file**. It executes nothing against a database that already ran
it. So after a revert:

- a **new** environment, built from the reverted tree, has the 0012 schema;
- an **existing** environment that already applied 0013 keeps the columns and the index;
- **nothing in either database records which of the two it is** — there is no tracking table to ask.

Two environments, one tree, divergent schemas, and no way to tell them apart short of introspecting
`information_schema`. That is the hazard, and it is not specific to 0013 — it is how every migration
in this repo behaves.

## 3. Where the append-only rationale does NOT reach

The README's justification is about the LOG: you never roll back historical verdict data. Correct. But
migrations here also carry **grants** and **constraints**, and neither is append-only LOG data:

- **15 REVOKEs.** Every one was checked, and **all 15 revoke privileges granted by their own
  migration** — the pattern is `GRANT SELECT, INSERT ON t TO cp_runtime; REVOKE UPDATE, DELETE ON t
  FROM cp_runtime;`, tightening a grant the same file just issued. That is deliberate least-privilege
  hardening, and it means **no REVOKE in this repo silently strips a privilege an earlier migration
  depended on.** `0013:110`'s `REVOKE INSERT, UPDATE, DELETE ON api_callers FROM cp_runtime` is a
  re-assertion of `0010:59`'s identical line (its own comment says so), and it was measured to be a
  **no-op**: `cp_runtime` holds exactly `SELECT` on `api_callers` both with and without 0013.
  > This corrects an assumption worth stating plainly, because acting on it would have produced a
  > phantom fix: 0013's REVOKE was believed to be a privilege drop that `git revert` could not undo.
  > It is not a privilege drop at all. The irreversibility in 0013 is the two **columns** and the
  > **index** in §2, which is a different problem with a different remedy.
- **2 dropped constraints.** `0008` and `0012` each `ALTER TABLE … DROP CONSTRAINT IF EXISTS` before
  recreating a widened version. Reverting the file leaves the **widened** constraint in place. The
  reverted code then believes a narrower invariant holds than the database enforces — the direction
  that admits rows the old code cannot handle, which is worse than the reverse.

So the honest scope of the append-only argument: it explains why the **LOG** has no down-migrations. It
does not explain, and was never meant to explain, why a *grant* or a *constraint* change has no reverse
path. Those simply have none.

## 4. What to do at go-live — the checklist

Ordered. The first item is the only one that must happen before the second environment exists.

1. **Adopt a migration tool with version tracking before the second environment exists.**
   `migrations/README.md` §Idempotency already recommends this ("For production: track applied
   migrations in a `schema_migrations` table or use a migration tool") — but it is **prose, and nothing
   enforces it**, which is why there is still no such table. With one database the gap is invisible;
   with two it is the difference between "which migrations has this box had" being a query and being an
   archaeology exercise. Retrofitting after divergence means hand-reconciling live schemas.
2. **Treat `git revert` of a migration as NOT a rollback.** Reverting the file de-syncs the tree from
   every database that already ran it. To undo applied DDL, write a **new forward migration** that
   reverses it explicitly (`ALTER TABLE … DROP COLUMN`, `DROP INDEX`, re-`GRANT`) — reviewed like any
   other migration, and applied through the same path. Fix forward is already this repo's stated
   policy; this item is only about making sure the policy is followed *instead of* a revert, not after
   one.
3. **Before deploying a migration, know its reverse.** Not necessarily write it — know it, and know
   whether it is data-losing. `ADD COLUMN` reverses cleanly (`DROP COLUMN` loses only that column's
   data). `DROP CONSTRAINT` + recreate-widened does **not** reverse cleanly once rows exist that only
   the widened form permits: narrowing it again fails while those rows are present, so the reverse is a
   data decision, not a DDL one.
4. **The LOG genuinely does not roll back.** `verifications`, `verification_raw`, `send_outcomes`,
   `node_audits`, `work_item_attempts` are append-only by contract (ADR/§3.2.1) and `cp_runtime` has no
   `UPDATE`/`DELETE` on them by construction. Recovery for those is **restore**, not rollback — which is
   exactly what [`pending-ledger.md`](pending-ledger.md) **B8** (the DR-4 backup/PITR restore-into-
   isolation drill) is for. B8 stays open, and this file is a reason to actually run it rather than
   defer it again: it is the only recovery path the LOG has.

## 5. What this does not claim

- It does **not** claim a migration is broken. All 13 apply cleanly, in order, on a fresh database, and
  the CI job that does so is green.
- It does **not** claim forward-only is the wrong design. For an append-only verdict LOG it is the right
  one; the point is that the *reason* covers less ground than the *rule*.
- It does **not** propose writing 13 `.down.sql` files. Most would be either trivial or dishonest (a
  down-migration for a partitioned append-only table is a data-loss script wearing a rollback costume).
  The actionable item is item 1: **tracking**, so you always know what a given database has run.

---

*Companion docs: deferred proofs are in [`pending-ledger.md`](pending-ledger.md); the ordered go-live
path is [`go-live-runbook.md`](go-live-runbook.md); the migration conventions are in
[`../control-plane/migrations/README.md`](../control-plane/migrations/README.md).*
