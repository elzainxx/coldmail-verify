# Operator runbook — running the system

This is the operator's guide to a system that is already installed: pulling the kill-switches,
promoting self-tuning mechanisms from recommend-only to automatic, reading what the system is telling
you, routine maintenance, and what "rollback" does and does not mean here. To install or deploy it, see
[`install-and-deploy.md`](install-and-deploy.md); for how it is built see [`architecture.md`](architecture.md); for
the ordered, status-marked path to live see [`go-live-checklist.md`](go-live-checklist.md).
*Verified against the code 2026-11-09; the section numbers below are cited from elsewhere, so they are stable.*

---

## 1. What you run

Two programs: the **control-plane** (Go, `:8080` unless `TM_LISTEN_ADDR` says otherwise — holds all
state, needs Postgres and secrets) and the **verifier-node** (Ruby, containerised, `:3000` — the SMTP
probe, needs a port-25-capable IP). The control-plane runs cheap-verification-only with just a database
and a pepper; SMTP probing, the paid fallback and the calibration feed are each switched on when their
prerequisite is in place. **Turning capability on is always config or an operator action — never a code
change or redeploy** (the payoff of the two-deployable split).

## 2. Deployment prerequisites

Moved to [`install-and-deploy.md`](install-and-deploy.md) §3 (this section number is kept so existing
citations resolve). In one table — each tier, what turns it on, and how you turn it back off:

| Tier | Turned on by | Turned off by |
|---|---|---|
| Cheap verification | `TM_DATABASE_URL` + `TM_PEPPER` (+ `TM_FRONTDOOR_TOKEN`; install §3.1). `/v1/verify` serves with just these, but `/readyz` reports `not_ready` until `TM_QUEUE_ADDR_KEY` is also set (§6.1) | — (it is the floor) |
| Bulk / async | `TM_QUEUE_ADDR_KEY` + per-caller keys | `TM_FRONTDOOR_MODE=accept_and_defer` or `reject` (§3) |
| SMTP verification | a vetted port-25 host, `TM_NODE_*`, `TM_SMTP_MODE=enabled` (install §3.2) | `TM_SMTP_MODE=disabled` or `TM_NODE_ENABLED=false` |
| Paid fallback | a ZeroBounce key + DPA + the three switches + a daily spend cap above 0 (install §3.3; the adapter ships disabled) | `TM_FALLBACK_MODE=off` |
| Calibration | an authenticated outcome feed (install §3.4) | leave the mutators `recommend-only`; unset the opt-in flags |

---

## 3. The kill-switch ladder — safe degradation

The system degrades in **rungs**. Every rung returns a real, correctly-labelled verdict from
fewer layers — degradation lowers a verdict's *confidence*, never its *honesty*. Nothing on this
ladder ever returns a fabricated `valid`/`invalid`. Pull a rung by config; no redeploy.

| Rung | State | Pull it when | How |
|---|---|---|---|
| **0** | Full — cheap + SMTP + queue + fallback + (promoted) self-tuning | steady state | — |
| **1** | SMTP-off | IP reputation emergency, a compromised/blocked box | `TM_SMTP_MODE=disabled`, or `TM_NODE_ENABLED=false` per node, or route a provider to `:mx` |
| **2** | Fallback-off | vendor bill runaway, vendor outage, a privacy/DPA event | `TM_FALLBACK_MODE=off` (global panic button) |
| **3** | Cheap-layers-only | total SMTP loss *and* fallback off; or a cost freeze | Rung 1 + Rung 2 together (this is the baseline steady state) |
| **4** | Paused | data-integrity emergency, DB outage, maintenance | `TM_FRONTDOOR_MODE=accept_and_defer` (bulk still accepted, drainer stopped) or `=reject` (nothing accepted) |

At **Rung 1**, addresses that *would* have earned `valid` via SMTP settle as `risky` instead —
**never `invalid`** for lack of a probe. At **Rung 2**, residue stays exactly as the queue left
it (`unknown`/`risky`) — never worse. At **Rung 4 accept-and-defer**, the front door returns a
job handle and work sits durably in the queue; restarting resumes with zero loss.

**Pulling Rung 4 in practice.** `TM_FRONTDOOR_MODE` is one dial that moves both halves — the
front door and the drainer — so you cannot pause one and leave the other running:

```bash
# Soft pause: keep taking bulk work, process none of it.
TM_FRONTDOOR_MODE=accept_and_defer   # then restart (config is read at boot; no reload/SIGHUP)
# Hard pause: take nothing at all.
TM_FRONTDOOR_MODE=reject
# Release:
TM_FRONTDOOR_MODE=serving
```

What you will see, and what to tell a caller:

- `POST /v1/verify` returns **503 with `error.code = "paused"` at BOTH sub-modes.** A synchronous
  call has no deferred form — no handle to return, nowhere to defer to — and answering it would
  either defeat the pause or fabricate a verdict. **Accept-and-defer only helps bulk callers.**
  The two sub-modes are indistinguishable to the caller apart from the per-request `request_id`.

  **`paused` is a DIFFERENT code from `unavailable`, over the same 503** (fix-plan item 18). If you
  are triaging and see `unavailable`, the pause is **not** what you are looking at — that code means
  the process is not wired (no pepper or no reachable DB) and someone should be paged. `paused` means
  an operator pulled this rung deliberately; a caller should retry later, and nobody needs waking.
  When a caller reports a failure, **ask for the `request_id`** and grep it in the control-plane log:
  every error logs one line as `api error: code=… status=… request_id=… reason="…"`, where `reason`
  is the internal detail that is deliberately NOT in the response body.
- `POST /v1/verify/bulk` at `accept_and_defer` returns **202 with a `job_id` and
  `"processing_paused": true`**; the addresses are stored encrypted in `work_items` as `ready`.
  `GET /v1/jobs/{id}` keeps working and repeats `processing_paused: true` with `settled: 0`, so a
  caller polling the handle learns *why* progress is zero instead of diagnosing a stall.
- The drainer logs `PAUSED (kill-switch Rung 4 accept-and-defer)` once on entry and
  `RESUMED (Rung 4 released) — draining the work that accumulated while paused` on release. Only
  transitions are logged, so the incident's real signal is not buried.
- **Confirm the rung is actually pulled** before you rely on it — `/readyz` and the boot line show
  `frontdoor=paused-accept-and-defer(Rung4)` or `paused-reject(Rung4)`:

```bash
curl -s localhost:8080/readyz | grep -o 'frontdoor=[^ "]*'
```

While paused the drainer also skips the lease-reclaim sweep. That is deliberate: it takes no new
lease, so the only `in_flight` rows are ones a pre-pause attempt owns, and re-readying work nothing
may process would be churn. The sweep catches them on the first tick after release.

Backlog while paused is bounded only by `work_items` growth and the §7.5 per-submission cap — a
long soft pause under load accumulates rows. Watch the `ready` count and drop to `reject` if the
queue is growing faster than you intend to drain it. Full per-surface matrix and the reasoning:
[`history/kill-switch-ladder.md`](history/kill-switch-ladder.md) Rung 4 and [`adr/ADR-0015-rung4-pause-enforcement.md`](adr/ADR-0015-rung4-pause-enforcement.md).

Two **brakes** are the ladder's safety interlock — they refuse to let an *automated* action
cascade into a self-inflicted outage:

- **Fleet brake:** if a majority of active nodes trip at once, the cause is systemic → the system
  **alerts and holds** instead of auto-quarantining the whole pool (which would empty it).
- **Cascade brake:** if a systemic confidence swing appears across many calibration buckets, the
  system **holds for human confirmation** instead of auto-applying a mass recalibration.

Both convert a would-be automatic global change into a human decision. There is also an absolute
**active-floor backstop**: statistical quarantine may never drop the pool below the serving floor
— the last healthy node is never auto-quarantined.

See [`history/kill-switch-ladder.md`](history/kill-switch-ladder.md) for the full ladder with every scope.

---

## 4. Shadow-mode — promoting a mechanism to automatic

Four mechanisms can change what callers see — verdict confidence, node routing, provider
eligibility, spend — without a human in the loop. On a fresh system **all four default to
recommend-only**: they compute and *recommend*, but do not mutate live state until you promote
them. This is the starting posture; promotion is deliberate and per-mechanism.

| Mechanism | Mutates | Default | Promote when |
|---|---|---|---|
| **A1** block-rate quarantine | node routing | recommend-only | you trust the statistical trigger; its blast radius (one node, reversible) is the smallest — the most defensible early promotion |
| **A6** confidence recalibration | confidence numbers | recommend-only | a recalibration version has proven sane over a real window and you approve it |
| **A8** provider promotion | routing/eligibility | recommend-only | a provider's measured accuracy over a real sample supports dropping SMTP for it |
| **A9** fallback weaning | spend/eligibility | recommend-only | your own measured accuracy beats the vendor's by a safety margin on that bucket |

The bring-up sequence for each: run it in **shadow** (compute + log, no proposals) to confirm its
own outputs look sane, then **recommend-only** (it emits approvable proposals), then **auto** (it
acts within its guards). Promotion to `auto` is an admin-audited operator action, per mechanism,
per scope — never a global flip. The audit-red node quarantine, canary recovery, rate-limiter
pacing, the historical-bounce gate, and the spend circuit-breaker stay automatic by default —
they act on deterministic signals and degrade only in the honest direction.

Recommend-only is a *mitigation*, not a substitute for authenticating the outcome feed — keep the
calibration mutators recommend-only until the feed is authenticated **and** the mechanism's shadow
outputs have proven sane. See [`history/shadow-mode.md`](history/shadow-mode.md).

---

## 5. Health, quarantine, and recovery

The control-plane runs a continuous **health-watcher** — the single writer of node status. It:

- **audits** each node on a cadence (`TM_WATCHER_CADENCE_SECONDS`), recording green/red;
- **polls liveness**, quarantining a node after `TM_LIVENESS_MISSES` consecutive failures;
- **watches block-rate** — if a node's SMTP blocks exceed `TM_BLOCK_QUAR_THRESHOLD_PCT` over a
  window (with at least `TM_BLOCK_MIN_SAMPLES` attempts), it's quarantine-eligible.

Quarantine is **quick, recovery is slow** (asymmetric, on purpose — reputation is easy to lose,
hard to rebuild):

- A quarantined node recovers only via a **canary**: a small metered trickle of real traffic
  (`TM_CANARY_WARMUP_CAP`), and only after `TM_CANARY_RECOVER_GREENS` consecutive clean cycles
  with at least `TM_CANARY_MIN_SAMPLES` real attempts. A node never re-enters the pool at full
  volume on zero evidence.
- On recovery its warmup counter resets low — a recovered node re-warms, it doesn't resume at
  full send rate.

Startup validates the control-loop dials: an out-of-range value that would silently disable a
brake (e.g. a fleet-brake fraction ≥ 100%) is a **startup error**, never a silent no-op.

---

## 6. What the alerts mean

| Alert | Meaning | Action |
|---|---|---|
| **audit-red** | a node's audit went red (DNS/PTR/config problem) | the node auto-quarantines; investigate the host. Read `GET /v1/audit` → `warnings`: `ptr`/`dns` come from the engine; **`resolver`** means the host has a `search`/`domain` line, `ndots` > 1 or a dotted hostname — fix the host (short hostname, nameservers-only `resolv.conf`), not the node; a node started with `PROBE_ALLOW_NON_PUBLIC=1` is also red by design |
| **`non_public_mx`** (a verdict `reason`, not an alert) | the probe destination guard refused a mail host that resolves to a non-public address (ADR-0021); the verdict is `risky`, never cached | expected for hostile or misconfigured domains; investigate only if it appears for domains you know to be public |
| **block-spike** (single node) | one node's block-rate crossed the threshold | it quarantines (if A1 promoted) or recommends; check that IP's reputation |
| **fleet-wide** | a majority of nodes tripped at once — systemic | **the fleet brake has held the pool** — this needs a human; do not drain, find the common cause (a routing change, a shared upstream) |
| **recovering** | a quarantined node is on its canary trickle | informational; watch for clean cycles |
| **`unknown_error`** (a verdict `reason`, not an alert) with `signals.error_capped = true` | an item could not be computed — its policy could not be resolved, its address could not be decrypted (a missing or rotated-out `TM_QUEUE_ADDR_KEY`), or `ComputeForQueue` returned an error — on `TM_QUEUE_ERROR_MAX_ATTEMPTS` attempts (default 5, about 30 s apart), so the queue settled it instead of retrying forever. It is `failure_origin: own`, terminal (the item is `dead`, it appears in the job's results and the job completes) and never cached | the cause is on our side: read the drainer log (`queue drainer: addr decrypt failed …`, `item … failed N times (last: …)`), fix it (restore the key, restore the policy), and re-submit the affected addresses in a new job. Many of these at once after a key rotation means the previous key was dropped too early (`TM_QUEUE_ADDR_KEY_PREV`) |
| **`smtp_skipped_reason = smtp_unwired`** | the SMTP stage is not wired in this process (SMTP disabled, or no node or engine config could be fetched at boot): every residue address is answered by the cheap layers alone, labelled `failure_origin: own`, and not cached | expected on a deliberate cheap-layers-only deployment; otherwise find why boot logged `staying cheap-layers-only` (node list, token, `/v1/config` fetch) |
| **rate-saturation** | demand is exceeding paced capacity | add a node, or route more through the queue |
| **pool-eligible-low** | the eligible node set is near the floor | you are one quarantine from cheap-only; add capacity |

The **fleet-wide** alert is the one that must never be a mere dashboard number — it is the signal
that the brake did its job and a human decision is required.

### 6.1 `/readyz` says `not_ready` — which dependency, and what to do

`GET /readyz` is the probe an orchestrator or the platform should act on. It answers **200 `ready` only
when every check in `checks{}` is true, and 503 `not_ready` otherwise**; `ready` (bool) and `status`
(word) always agree with the HTTP code. The checks are independent, so the one that is `false` is the
one to fix:

| `checks.…` | `false` means | Fix |
|---|---|---|
| `pepper` | no `TM_PEPPER` loaded: nothing can be hashed | set it (never rotate casually, install §3.1) and restart |
| `db` | `TM_DATABASE_URL` unset, the store failed to open or seed, or the **live** 3 s ping failed | fix the database or the DSN; `db` recovers by itself once the ping succeeds |
| `queue` | the bulk/job subsystem did not come up (below) | fix what `reasons` names, **then restart**: the queue's status is decided once at boot, unlike `db` |

`queue` appears in `checks{}` only on a process whose boot reached the queue wiring, and only when it is
`false` does the body gain a top-level `reasons` array of strings (omitted entirely when empty). Every
other field of the body is unchanged. A not-ready queue says why, in these words:

| `reasons` entry | Cause | Fix |
|---|---|---|
| `queue: TM_QUEUE_ADDR_KEY is not set: the bulk and job routes answer 503 until it is` | no DR-4 at-rest key, so the queue refuses to start rather than store plaintext; `POST /v1/verify/bulk` and `GET /v1/jobs/…` answer 503 `unavailable` to an authenticated caller | set `TM_QUEUE_ADDR_KEY` (32 bytes, hex or base64, **not** the pepper: `openssl rand -hex 32`) and restart |
| `queue: DR-4 cipher init failed: …` | a key is set but it, or `TM_QUEUE_ADDR_KEY_PREV`, is not exactly 32 bytes after decoding (the text carries the length, never the key) | correct the key and restart |
| `queue: drainer init failed: …; bulk submissions would enqueue but not drain` | the key is fine but the drainer refused to be built (both retry ceilings zero, or a lease TTL that does not exceed the SMTP deadline). Startup config validation rejects those inputs first, so this is a **second line of defence**: seeing it means a config path skipped validation | fix the dial the text names and restart, and report it; until then submissions are accepted and **never processed** |

**A deployment that serves only `POST /v1/verify` and has no bulk caller still has to set
`TM_QUEUE_ADDR_KEY` to be reported ready** — there is no "no bulk wanted" setting; the key is a boot-time
precondition of the whole module being ready. **Why it is all-or-nothing:** the platform always submits bulk jobs, so a deployment without the queue key can never serve it; a verify-only mode is not offered. (Rung 4 does not change this: a paused queue is still
`ready`, because a pause is a deliberate state and not a missing dependency, §3.) A process that failed
before the queue wiring (no pepper, no `TM_DATABASE_URL`, store open failed, policy load or seed failed)
is already `not_ready` for `pepper`/`db` and carries no `queue` check. To check from a shell:

```bash
curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/readyz   # 200 or 503
curl -s localhost:8080/readyz | grep -o '"reasons":\[[^]]*\]'    # empty when ready
```

---

## 7. Operating principles

- **Every rung is reversible.** Quarantine → canary recovery; breaker → window-roll; pause →
  resume with the durable queue. You can always step back up.
- **Degrade toward `unknown`, never fabricate.** Every safe state the system falls into returns
  humbler verdicts, never wrong ones.
- **Enabling capability is deliberate.** SMTP, the vendor, and auto-calibration each take an
  explicit operator action with its prerequisite in place — nothing dangerous turns itself on.
- **The manual controls should be audited — and today they are not.** Node disables, the fallback-kill,
  pauses and mechanism promotions change what callers see, and the intended home for "who did what, when" is an
  append-only admin-audit log (DR-3). **It is not built** ([`future-work.md`](future-work.md) FW-1; go-live G0.2),
  so until it is, record these actions yourself (a change ticket, the host's own access logs).

For the full ordered bring-up with every owner gate and verification command, work through
[`go-live-checklist.md`](go-live-checklist.md).

---

## 8. Routine maintenance — `verification_raw` partitions

`verification_raw` (the raw-engine-JSON sidecar of every SMTP verdict) is partitioned by month. **A row
dated in a month with no partition is rejected** with `SQLSTATE 23514` ("no partition of relation
`verification_raw` found for row"), and because that insert is atomic with the LOG row, **every SMTP
verdict write fails.** Migration `0001` created only 2026-07..09; this is what keeps it from happening
again ([ADR-0017](adr/ADR-0017-raw-partition-maintenance.md)).

**What runs by itself.** The control-plane calls `verify.ensure_verification_raw_partitions(n)` at startup and then
every `TM_RAW_PARTITION_MAINTAIN_INTERVAL_SECONDS` (default 6 h), keeping the current month plus
`TM_RAW_PARTITION_MONTHS_AHEAD` (default 3) future months. It is idempotent. Migration `0014` must be
applied first (and `0016`, which rebuilds the function in schema `verify`): the runtime role `verify_app` gets the function from them and gains no other DDL.

**What to watch.** One log line per failure, and it names the consequence:
`raw-partitions: maintenance FAILED (...) — verification_raw will run out of partitions ...`. A failing
loop has roughly `TM_RAW_PARTITION_MONTHS_AHEAD` months before any write fails, so this is a *fix this
week* alert, not a page — unless the line appears together with `SQLSTATE 23514` write errors, in which
case it is already a page. Created partitions log `raw-partitions: created N ...`.

**If the cliff has already been hit** (writes failing with 23514): apply `0014` if it is not applied, then
restart the control-plane (tick 0 creates the partitions within seconds of startup), or call the runtime door by
hand, as `verify_app` or as any role that may `SET ROLE` to it: `SELECT verify.ensure_verification_raw_partitions(3);`
(it is `SECURITY DEFINER`, owned by `verify_owner`, so it creates the partitions with the owner's rights). The
admin core `verify.ensure_verification_raw_partitions_at(now(), 3)` is `SECURITY INVOKER`: running it **as
`verify_admin` does not work** (that role owns nothing, and creating a partition needs ownership of
`verify.verification_raw` and `CREATE` on schema `verify`, which only `verify_owner` has), so the core needs
`SET ROLE verify_owner;` first. Verdicts whose write failed during the gap were not logged
and callers should re-submit them (this document does not know what each caller received).

**What does NOT run by itself: retention.** Dropping old partitions (`DROP PARTITION` after ~90 days,
P1 §3.2.6) is still a manual action, and it needs `verify_owner` (`SET ROLE verify_owner; DROP TABLE verify.verification_raw_YYYY_MM;`): `verify_admin` owns nothing and cannot drop a partition. Nothing is dropped automatically.

**A deployment whose database default time zone was not UTC when `0001` ran** should check that
`verification_raw_2026_09` ends exactly at `2026-10-01 00:00:00+00` (`SELECT pg_get_expr(relpartbound, oid)
FROM pg_class WHERE relname = 'verification_raw_2026_09' AND relnamespace = 'verify'::regnamespace`) before trusting the first auto-created month;
see the stated assumption in ADR-0017.

---

## 9. Schema changes, rollback and restore — what "reversible" does and does not mean

Everything above is reversible by **flags and config**: unset a variable and the vendor layer is off, the feed
fails closed, a node disables, a rung is released — no redeploy, no lost history. **The migration layer is the
exception, and it is the one layer where reversal means touching data.** Anyone who takes "every stage is
reversible" to cover schema changes will reach for `git revert` and get a result worse than doing nothing or
rolling forward. (Originally written 2026-08-07 as `history/go-live-rollback-posture.md`; re-measured below on
2026-11-13.)

### 9.1 The measured facts

Measured against the tree — the migration row on 2026-11-17 over 18 migrations, the tracker row on 2026-11-16, the `REVOKE` row on 2026-11-15 (`0017` and `0018` add none), the others on 2026-11-13; not inferred:

| Fact | Value |
|---|---|
| Migrations | **18** (`0001` … `0018`), applied by the runner, `cmd/migrate up` (§9.6). CI builds its test database with the old loop (`for f in $(ls migrations/[0-9][0-9][0-9][0-9]_*.sql \| sort); do psql -f "$f"; done`, valid on an EMPTY database only) and then runs the runner over it |
| `.down.sql` files, or any other rollback mechanism | **0** — none |
| A `schema_migrations` / version-tracking table | **`verify.schema_migrations`**, created by `0017` and filled by the runner: one row per file (`version`, sha256 `checksum`, `how` = `applied` or `adopted`, `applied_at`, `applied_by`). `verify_app` holds no privilege on it; `verify_admin` may `SELECT` (FW-3, closed) |
| `REVOKE` statements | **22** in 11 files (`grep -cE '^\s*REVOKE\b' migrations/*.sql`): 17 are `REVOKE … FROM cp_runtime` tightenings (the least-privilege pattern: `GRANT SELECT, INSERT … TO cp_runtime; REVOKE UPDATE, DELETE … FROM cp_runtime;`), 2 (in `0014`) remove the default `PUBLIC` execute on two functions, and 3 (in `0016`) remove the default `PUBLIC` access to schema `verify` and to the two functions that file creates (`cp_runtime` is the role `0016` renames to `verify_app`) |
| Re-running `0001` … `0015` on a database that already ran `0016` | **never done by the runner** (it adopts those versions instead, and refuses a database that also holds `public.verifications`). By hand it is **not supported**: they are unqualified and `IF NOT EXISTS`, so they re-create empty module-1 tables in `public`; `0016` then stops with SQLSTATE `42P07` naming the table, before moving anything. Check the `public` copy is empty, drop it, re-run `0016` |
| `ALTER TABLE … DROP CONSTRAINT` | **4**: `0008` (`nodes.valid_status`), `0012` (`send_outcomes.send_outcome_calibration_class`), `0015` (`verifications.valid_reason` and `cache.valid_reason`) — each drops, then recreates a **widened** constraint |

The 2026-08-07 write-up reported "15 REVOKE across 8 migrations, 2 DROP CONSTRAINT" at 13 migrations; that figure
did not reproduce with the command above (17 in 9 files at `0013`), so treat the *command*, not the old number,
as the record. Forward-only is deliberate: `migrations/README.md` §"No Down Migrations" says a down-migration
"would violate the append-only contract" of the LOG. That is sound *for the LOG*; §9.3 is about where it stops applying.

### 9.2 `git revert` of a migration is not a rollback

Measured 2026-08-07 on two databases built from the same repo, differing only in whether `0013_job_owner.sql` ran:
the one that ran it has `api_callers.user_id`, `jobs.owner_user_id` and `idx_jobs_owner`; the other does not.
`git revert` deletes the migration **file** and executes nothing against a database that already ran it. After a
revert a *new* environment is built from the older schema while an *existing* one keeps the columns. Since `0017` the database does record what it ran,
so the divergence is no longer invisible: the existing database holds a recorded version the reverted tree has no file for, and the runner refuses
it ("the database is ahead of this checkout") instead of carrying on. It is still two environments with divergent schemas, and a refusal is not a rollback.
The hazard applies to every migration in the repo.

### 9.3 Where the append-only rationale does not reach

- **Grants.** The 2026-08-07 line-by-line review found that every REVOKE then in the tree revoked a privilege its own migration granted, so none silently strips
  a privilege an earlier migration relied on (`0013`'s `REVOKE … ON api_callers` re-asserts `0010`'s and was measured
  to be a no-op); the two added since (`0014`) only remove default `PUBLIC` execute on new functions. The irreversibility in `0013` is its two columns and its index, not a privilege.
- **Constraints.** Each of the four dropped constraints was recreated *wider* (the latest, `0015`, admits the new
  reason `non_public_mx`). Reverting the file leaves the **wider** constraint in the database while the reverted code
  believes a narrower invariant — the direction that admits rows the old code cannot handle. Narrowing it again fails
  while any row carrying the new value exists, so the reverse is a data decision, not a DDL one.

### 9.4 What to do — in order

1. **Apply migrations only with the runner** (§9.6; migration version tracking, [`future-work.md`](future-work.md) FW-3, is built and closed). With two
   databases the difference between a query (`migrate status`) and archaeology is exactly this tracker.
2. **Treat `git revert` of a migration as not a rollback.** To undo applied DDL write a **new forward migration** that
   reverses it explicitly (`DROP COLUMN`, `DROP INDEX`, re-`GRANT`), reviewed like any other and applied the same way.
3. **Before deploying a migration, know its reverse** — and whether it loses data. `ADD COLUMN` reverses cleanly;
   `DROP CONSTRAINT` + recreate-widened does not, once rows exist that only the wider form allows.
4. **The LOG genuinely does not roll back.** `verifications`, `verification_raw`, `send_outcomes`, `node_audits`,
   `work_item_attempts` and `external_calls` are append-only by grant. Recovery for those is **restore**, not
   rollback — which is what the DR-4 backup/PITR restore-into-isolation drill is for ([`future-work.md`](future-work.md)
   B8; go-live G1.13). It is the only recovery path the LOG has, so run it rather than defer it. Back the **pepper** up
   separately and test recovery: a restore without the pepper cannot re-join a single address.

### 9.5 What this does not claim

It does not claim a migration is broken — all 18 apply cleanly, in order, on a fresh database, in CI on every push (by the old loop and by the runner). It
does not claim forward-only is the wrong design. And it does not propose 18 `.down.sql` files: most would be trivial or
dishonest (a down-migration of a partitioned append-only table is a data-loss script wearing a rollback costume). The
actionable item was tracking, so you always know what a given database has run; it is now built (§9.6).

### 9.6 Applying and checking migrations: the runner

Since `0017` there is one way to apply migrations to a database you keep: `control-plane/cmd/migrate` (design and proof: [`../control-plane/migrations/README.md`](../control-plane/migrations/README.md) "The runner and the tracker"). It needs the **migrating** identity's DSN (a superuser on stock Postgres, `postgres` on Supabase, never `verify_app`) on a direct or session-mode connection, and the migrations directory:

```bash
cd control-plane
export TM_MIGRATE_DATABASE_URL='postgres://postgres:<password>@<host>:5432/<db>' TM_MIGRATIONS_DIR=migrations
go run ./cmd/migrate status      # one line per file: NNNN applied|adopted|pending <checksum8>; changes nothing, takes no lock
go run ./cmd/migrate up          # migrate: applied N, adopted M, already applied K, total T
```

Run `status` first on any database you have not looked at. **Stop the control plane for an upgrade of an old database** (`0016` moves every table to schema `verify`).

| Exit | Message | What it means and what to do |
|---|---|---|
| `0` | `migrate: applied 0, adopted 0, already applied T, total T` | nothing to do; running it again is always safe |
| `0` | `adopted M` > 0 | the database was migrated by the old loop; the runner recorded those versions without running them |
| `2` | `migration NNNN changed after it was applied: recorded <8 hex>, on disk <8 hex>` | someone edited an applied file. Revert the edit and put the change in a **new** migration. Nothing was changed |
| `2` | `migration NNNN is recorded … but there is no file for it: the database is ahead of this checkout` | this host's code is older than the database (a rollback of the code, a stale deploy). Deploy the newer checkout; do not edit the tracker. Nothing was changed |
| `2` | `migration NNNN is not applied but a later one (MMMM) is` | the history has a hole (a row deleted, or a file applied out of order by hand). Find out what happened, restore the row only if the file's effect is really present; nothing was changed |
| `2` | `verify.verifications and public.verifications both exist` | `0001`–`0015` were re-run by hand on a migrated database; check the `public` copy is empty, drop it (§9.1) |
| `2` | `no migration files`, `migration NNNN is missing`, `has 2 files`, `not named NNNN_name.sql` | the migrations directory is wrong or malformed (a numbering gap, two files with one number, a misnamed file that would never run) |
| `1` | `another migration runner holds the advisory lock …` | another `up` is running on this database (or crashed with its connection still open). Wait, or find it with `SELECT pid, application_name, state FROM pg_stat_activity WHERE application_name = 'tm-migrate'`; a dead runner releases the lock when its connection ends |
| `1` | `migration NNNN_x.sql failed and was rolled back` | that file's SQL failed; its transaction rolled back and nothing after it ran, everything before it stays recorded. Fix the cause in a new forward migration if the file is wrong, or fix the environment, and run `up` again: it repeats the same file |

A database a `psql` loop (or the Supabase SQL editor) migrated has an empty or missing tracker: the first `up` adopts it. After that, apply migrations **only** with the runner: a file applied by hand is not recorded and the next `up` would apply it again.
