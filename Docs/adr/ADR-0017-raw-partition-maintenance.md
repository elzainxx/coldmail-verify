# ADR-0017 — `verification_raw` partitions are created ahead of time by a narrow runtime door, not by widening the runtime role

- **Status:** ACCEPTED (2026-11-07) — found by re-running the suite three months after it last ran.
- **Kind:** door-referencing — the **enforcement surface** of Door 2 (grants), extended by one narrowly
  scoped function. No contract, vocabulary, schema column or response shape changes.
- **Deciders:** owner (fix-and-publish brief, chunk C1 item 2).
- **Governs:** `control-plane/migrations/0014_raw_partition_maintenance.sql` ·
  `control-plane/internal/store/partitions.go` · `cmd/control-plane/main.go` (`startRawPartitionMaintenance`)
  · the `TM_RAW_PARTITION_MONTHS_AHEAD` / `TM_RAW_PARTITION_MAINTAIN_INTERVAL_SECONDS` dials.

---

## Context

`verification_raw` is RANGE-partitioned by month on `created_at` so retention can be an instant
`DROP PARTITION` (P1 §3.2.6; Door 2 keeps `verifications` itself append-only). Migration `0001` created
**three** partitions — 2026-07, 2026-08, 2026-09 — "as examples", with the note *"Production: automate
partition creation ahead of time"*. Nothing ever did.

A row dated in a month with no partition is **rejected** (`SQLSTATE 23514`, *no partition of relation
"verification_raw" found for row*). The raw sidecar is written atomically with the LOG row of every SMTP
verdict (§6.4), so **from 2026-10-01 every SMTP verdict write failed**. It was invisible in August: the
suite was green, and only turned red when `TestPGAppendWithRawSidecar` was re-run in November and `now()`
was no longer inside the hard-coded window. `Docs/history/pre-takeoff-checklist.md` carried the gap as an
unchecked line — *"`verification_raw` partition cadence"* — which is a warning comment, not a control.

## Decision

1. **The control-plane keeps N future months created**, where N is the dial
   `TM_RAW_PARTITION_MONTHS_AHEAD` (default 3, range 1..120; **0 is rejected at startup** because zero
   headroom *is* the cliff). A loop (`store.RunRawPartitionMaintenance`) ticks at startup — so a process
   that comes up after a long outage heals within seconds of coming up (it does not block serving) — and every
   `TM_RAW_PARTITION_MAINTAIN_INTERVAL_SECONDS` (default 6h). A failed tick is logged with its
   consequence and retried; it never takes the process down.
2. **The runtime role does not gain DDL.** `cp_runtime` is INSERT+SELECT only and DDL is the separate
   break-glass `cp_admin` credential (PN-9/DR-3, Door 2 hardening). Instead migration `0014` adds a
   `SECURITY DEFINER` function, `ensure_verification_raw_partitions(n)`, granted to `cp_runtime`. It can
   only create `verification_raw_YYYY_MM` partitions for *this month .. n months ahead*, where "now" is the
   **database clock** and cannot be supplied by the caller. It cannot drop, alter, read or write anything,
   and `n` is bounded 0..120.
3. The logic lives in a core, `ensure_verification_raw_partitions_at(p_from, n)`, that takes the reference
   time as a parameter so tests can exercise it at any date. The core is **not** granted to `cp_runtime`.
4. **Bounds are UTC.** `created_at` is `timestamptz`, so a bound is an instant; the function pins
   `SET timezone = 'UTC'`, so a session in another zone cannot create a partition that leaves a gap or an
   overlap at a month edge. New partitions are named like the legacy ones (so those are found and left
   alone) and get the grants `0005` gave them.
5. Concurrent callers are serialised by a transaction-scoped advisory lock; the function is idempotent.
6. The migration only **bootstraps the current month**. Future headroom is the dial's job, so there is
   exactly one place N is decided.

## Alternatives considered

| Option | Why not |
|---|---|
| Pre-create 10 years of partitions in a migration | Hides the problem for a decade, then reproduces it; a fixed horizon is exactly what `0001` did. |
| A `DEFAULT` partition to catch strays | Rows that land in it defeat `DROP PARTITION` retention, and it makes the failure *silent* instead of loud. |
| Grant `cp_runtime` the right to create tables | Breaks Door 2's "runtime cannot rewrite the schema" separation for a housekeeping need. |
| `pg_cron` / `pg_partman` / an external cron | A second moving part with its own credential and no tie to the application's lifecycle; unavailable on some hosts. |
| Make the caller pass the date | A compromised runtime credential could then mint partitions at arbitrary dates. |

## Consequences

- A control-plane that is up creates its own headroom; a control-plane that is down for longer than N
  months still hits the cliff — but it heals within seconds of startup (tick 0 runs immediately, in the
  background, and does NOT block serving, so a write that arrives before it completes can still fail), and
  the loop logs every failure.
- **Assumption, stated:** the three legacy partitions were created from `'2026-07-01'` etc. under the
  migration session's zone. The new partitions are exact UTC months. On a database whose default zone was
  not UTC when `0001` ran, the *first* new partition (2026-10) would not abut `2026_09` exactly (a gap or an
  overlap at that one edge). **This is NOT tested here**: the tests assert that every partition the function
  creates is an exact UTC calendar month (`TestPGRawPartitions_BoundsAreUTCMonthAligned`, and the headroom
  guard checks the exact-month shape for the months it covers), but not abutment with the legacy partitions
  on a non-UTC-default database. `0001` is unchanged. If a deployment ever ran `0001` in another zone, check
  `pg_get_expr(relpartbound, oid)` for `verification_raw_2026_09` before relying on the first new month.
- **Not automated, still:** retention (`DROP PARTITION` after ~90 days). Creation and retention are
  separate concerns; this ADR closes the one that breaks writes.
- Not done, deliberately: a `/readyz` check on partition headroom. It would make the cliff visible to a
  load balancer, but changes the readiness envelope; it is recorded as a follow-up, not smuggled in here.

## Verification (what makes the guard a control, not a comment)

- `TestPGRawPartitions_LegacyCliffIsReal` — reproduces the defect on the pre-`0014` schema with a
  date-independent probe (so it holds on any day), with a control row inside the legacy window.
- `TestPGRawPartitions_MaintenanceKeepsNMonthsAhead` — an *aged* table fails the headroom guard, one real
  tick makes it pass, a second tick is a no-op, and the headroom is exactly N (last covered instant
  inserts, first uncovered one is rejected).
- `TestPGRawPartitions_NoopMaintenanceLeavesTheCliff` — the standing paired control: with a tick that does
  nothing, the same guard **fails**.
- `TestPGRawPartitions_RuntimeDoorIsNarrow` — `cp_runtime` can call the wrapper and nothing else: the
  caller-time core, `CREATE`/`DROP` of a partition, `DELETE`, `UPDATE` and an out-of-range `n` are all
  refused, and new partitions carry INSERT+SELECT only.
- `TestPGRawPartitions_ConcurrentTicksDoNotCollide` — four simultaneous ticks create exactly N between
  them. Break-verified: with the advisory lock removed it fails with `relation ... already exists`.
- `TestBuildAPI_StartsPartitionMaintenance` — the **wiring**, end to end through `buildAPI` against a real
  database. Break-verified: with the single `startRawPartitionMaintenance` call removed it fails after 20 s
  while every store test stays green — which is why the wiring needs its own test.
- Config: `TestRawPartitionDialsRejectTheCliffAndGarbage` (0, negatives, garbage and >120 are refused).

- **Recorded:** this ADR; the ADR index; `config-manifest.md` §9; `operator-runbook.md` §8;
  `history/pre-takeoff-checklist.md`; `migrations/README.md`.
