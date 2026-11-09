# ADR-0011 — Phase-5 queue additive surface; DR-4 = separate-key encryption; PN-13 = per-caller keys; GAP-37 CAS

**Status:** ACCEPTED (2026-07-18) — Phase-5 (the durable async queue) local build.
**Doors touched:** the **Door-1/Door-4 additive-only** guarantees (zero new verdict-vocab values; a
new bulk sibling endpoint returning the SAME shape), **Door-2** operational state (three new tables),
and the **Door-3 bounded plaintext exception** (now encrypted at rest + transactionally purged). No
frozen door is mutated (see the reopen check).
**Sources:** [`../history/phase-5-queue-spec.md`](../history/phase-5-queue-spec.md) §3–§10, §14; [`../open-decisions.md`](../open-decisions.md) DR-4 (PN-3/ADR-0002), PN-13/F-20; [`../history/pre-takeoff-checklist.md`](../history/pre-takeoff-checklist.md) §3.7/§3.8; GAP-32/32b/33/34/35/36/37/38, OQ-P5-1…12; [[truemail-lease-cas-exactly-once-settlement]].

---

## Context

Phase 5 relaxes the synchronous-v1 assumption: bulk work becomes asynchronous, greylisted checks
acquire a real retry loop, and per-domain pacing gains a time dimension — all **behind** the
Phase-4 router, **through** the same gates, **into** the same LOG. It must stay **additive-only**
against the five frozen doors. Several things touch door territory and are worth a durable record:

1. **Three new operational tables** (§3.3/§7.2/OQ-P5-8): `work_items` (the durable queue),
   `jobs` (bulk handles), `work_item_attempts` (per-attempt transcripts). None touches frozen schema.
2. **A new request-path surface** (§7): `POST /v1/verify/bulk` + `GET /v1/jobs/{id}(/results)` — a
   *sibling* of the singular endpoint, returning the **byte-identical** `/v1/verify` verdict objects.
3. **The one bounded Door-3 exception** (§3.3/GAP-32b): `work_items` holds the **plaintext address**
   for work-not-yet-done, because SMTP structurally needs the real address and cannot run from a hash.
4. **Two P5-gating security decisions** the checklist deferred to P5-time: **DR-4** (backup-plaintext
   protection, ADR-0002) and **PN-13/F-20** (front-door auth beyond a static token).
5. **The GAP-37 lease-and-settle invariant** — "the catch of the whole five-spec series" — first
   *built* here and governing every future concurrent lease-and-settle path.

The questions this ADR settles: is the queue surface genuinely additive; how are DR-4 and PN-13
resolved (both were owner P5-time choices); and is the idempotency correctness heart (GAP-37) built
and fail-verified rather than hand-waved?

## Decision

1. **DR-4 mechanism = SEPARATE-KEY at-rest encryption** (owner ruling 2026-07-18; ADR-0002 left the
   mechanism a P5-time choice among snapshot-exclusion / separate-key-encryption / short-retention).
   The `work_items.addr_ciphertext` column stores the plaintext address as **AES-256-GCM** ciphertext
   (`internal/queue/addrcipher.go`, stdlib only — no new dependency) under a key **deliberately
   SEPARATE from the pepper** (`TM_QUEUE_ADDR_KEY`, loaded distinctly in `secrets.go`). The AAD is the
   row `id` (anti-replay: a sealed blob can't be lifted into another row). Rationale for this option
   over the other two: it is the **only one that is code + locally fail-verifiable** (ADR-0002's
   restore-and-assert becomes a `dump-the-row → only-ciphertext` test); snapshot-exclusion and
   short-retention are backup-*infrastructure* choices untestable on local Postgres. Failure-domain
   separation (F-9/F-19): losing this key makes only in-flight work undecryptable (recoverable —
   re-enqueue), unlike losing the pepper (all history de-joins forever); a snapshot leak ≠ a pepper
   leak. The exception is bounded TWO ways now: **encrypted at rest** (this) AND **purged in the
   settlement transaction** (`addr_ciphertext=NULL` as step 4 of the GAP-37 CAS tx — never a later
   sweep, so a crash can't strand plaintext). **Door 3 is affirmed, not reopened** — the raw address
   is never in a durable store in clear, and the pepper/hash scheme are unchanged.

2. **PN-13/F-20 = per-caller keys + rotation** (before the bulk-PII surface ships). The `api_callers`
   table (migration 0010) stores `key_sha256` — the key HASHED, never in clear — with a validity
   window `[not_before, not_after)` for zero-downtime rotation (two live keys overlap). Auth
   (`api.authenticate`) is a **header check only**: `sha256(bearer)` → `LookupCaller` (constant-time
   via `crypto/subtle`) → the caller_id is **derived from the key**, stashed in a private request
   context, **never read from the body** — so **Door 1/Door 4 are untouched** (no field/vocab change).
   sha256 not a slow KDF: keys are brain-issued high-entropy (no dictionary to slow), and a per-request
   KDF on this hot surface is a DoS amplifier. The **bulk endpoints REQUIRE a per-caller key**; the
   sync `/v1/verify` additionally accepts the legacy static token during transition (backward-compat —
   `TestAuthEnforcedInProduction` still passes). `cp_runtime` gets SELECT-only on `api_callers`;
   issuance/rotation are `cp_admin` (admin-audited, DR-3). **`caller_id` (key-derived, trusted) is kept
   DISTINCT from the body-supplied `caller_ref` (untrusted, GAP-36)** — merging them would let a caller
   forge its identity.

3. **The queue surface is additive-only (§10):**
   - **Door 1 / Door 4:** ZERO new `status`/`reason`/`recommendation` values — a retry-exhausted check
     settles as the **already-reserved** `unknown/greylisted`; a capacity give-up settles the existing
     cheap verdict. `vocab.go` is **byte-unchanged**. The bulk endpoint is a NEW sibling; its results
     are the **byte-identical** `/v1/verify` verdict objects (a consumer parses them unchanged, §7.4).
     Retry-provenance is additive `signals` keys only (OQ-P5-10, open jsonb).
   - **Door 2:** three **new** operational tables touching no frozen schema (`verifications`/`cache`/
     hashing unchanged). The settled verdict is INSERTed into the append-only LOG exactly once (GAP-33);
     `work_items`/`jobs` grant posture is cp_runtime SELECT+INSERT+UPDATE, **DELETE revoked** (retention
     is cp_admin), proven live; `work_item_attempts` is append-only (INSERT+SELECT), mirroring
     `node_audits`.
   - **Door 3:** the one bounded exception (item 1), encrypted + purged; the idempotency key derives
     from the **existing `email_hash`** (`sha256(email_hash‖policy‖version‖config_fingerprint)`, §5.1) —
     no new hashing, no new pepper exposure. Group-by-domain routes on `domain` (already in clear).
   - **Door 5:** the verifier-node fork + gem are **byte-identical to P2–P4** — Phase 5 adds NO box-side
     behavior (a queued check hits the box's unchanged `/v1/verify` exactly as a sync one does, §3.2).
     The retry path routes through the **UNCHANGED** P4 `Strategy` interface (a compile guard,
     `TestStrategyInterfaceUnchanged_Door5`, ensures no method was added for retry affinity).

4. **The master invariant is enforced by construction (§8).** The drainer's ONLY path to a provider is
   `orchestrator.ComputeForQueue`, which runs the **identical** P4 gate path (eligibility → sticky →
   rate-limit → warmup) as the sync `Verify` — they share `compute()`. There is no drain-mode branch
   that skips a gate. The compute/settle split is a pure statement-move: `Verify` = `compute()` +
   `settleSync()` (the three pre-existing log calls, unchanged — the sync path is byte-identical, proven
   by the unchanged orchestrator/api tests); the drainer calls the log-FREE `ComputeForQueue` and
   settles via the CAS, so an intermediate greylist writes **no** LOG row (§5.3) while Door 2 ("the log
   is the point") holds — the LOG write lives in the settlement the drainer bypasses until terminal.

5. **The GAP-37 idempotency heart is BUILT and FAIL-VERIFIED, not hand-waved** — "the catch of the
   whole five-spec series." A naive `FOR UPDATE SKIP LOCKED` + TTL lease is subtly wrong: a *slow* (not
   dead) drainer whose lease is TTL-reclaimed runs a **second live attempt** that races the item's next
   state (last-writer-wins → a settled `250` resurrected into retry, or a retry dropped — poisoning the
   P7 training data). Two-part fix (LOCKED):
   - **(a) Lease TTL > the brain's max SMTP deadline** so a *live* attempt never loses its lease; a
     reclaim then provably means dead. `NewDrainer` **fails fast** if `LeaseTTL <= MaxSMTPDeadline`
     (`ErrLeaseTTLTooShort`, `TestNewDrainer_RejectsShortLease`).
   - **(b) Every settle/requeue is a conditional compare-and-set** on `WHERE id=? AND locked_by=<me>
     AND status='in_flight'`. A lost attempt matches **zero rows**, rolls back (no LOG row — GAP-33),
     and discards its result (`store.ErrLeaseLost`). **The CAS, not the lease, makes settlement
     exactly-once.** Posture: at-least-once attempts, exactly-once settlement (a duplicate RCPT probe is
     read-only at the provider — a connection, not a delivered mail). Fail-verified on BOTH the Mem fake
     and real Postgres: `TestSettle_ExactlyOnceUnderSlowDrainerReclaim` (neuter the CAS guard → two LOG
     rows → fails; restored green) and `TestPGWorkItem_CASSettlementExactlyOnce` (the SQL CAS returns
     zero rows for the stale lease; the lost attempt's verdict is never in the LOG).
   - **GAP-38:** job completion is judged AUTHORITATIVELY by `NOT EXISTS(work_items WHERE job_id=? AND
     status NOT IN (settled,dead))`, never the denormalized `settled_count` (which can drift under the
     reclaim/no-op path) — `TestJobCompletion_AuthoritativeNotCounter` (drift the counter low → still
     reports complete).

6. **Greylist retry settles unknown, never invalid (§4.5).** A `450` requeues on a hint-seeded,
   exponential, jittered backoff up to `max_attempts`/`max_retry_age` (whichever trips first), then
   settles the reserved `unknown/greylisted` — retrying buys more chances to observe a settled answer,
   never manufactures one. A **queued** `rate_limited` WAITS (short backoff, no attempt bump —
   §4.6/OQ-P5-7); the **sync** path still degrades cheap (P4 unchanged) — the limiter creates
   backpressure, the queue absorbs it. Same-node retry affinity (GAP-32) is delivered free by P4 sticky
   HRW (a pure function of domain) — the drainer stores no last-node routing input.
   Fail-verified: `TestDrainer_GreylistRetriesThenSettlesUnknownNeverInvalid`,
   `TestDrainer_QueuedRateLimitedWaitsNotDegrades`, `TestRetryAffinity_SameDomainSameNode_GAP32`.
   The real reject-then-accept round-trip is **un-mockable** (GAP-35, P2 GAP-12) — the loop MECHANICS
   are tested on a by-attempt stub; true validation is deferred to a real greylisting provider at
   rollout (honestly not overclaimed).

7. **The two-job boundary holds (§7.6/GAP-36).** The brain's `jobs` table holds verification-work state
   only (counts, status, policy) + an OPAQUE `caller_ref` it never interprets — no user id, credits,
   CSV, filename, or per-row UI. Malformed bulk input is counted + reported, never a work-item (the
   brain doesn't parse CSVs). Bulk results carry `email=""` (the plaintext was purged on settle — a
   bulk result cannot echo the raw address, Door 3); correlation is a platform seam.

## Consequences

- Phase 5 is fully local-buildable (Postgres + Go); every acceptance criterion except the un-mockable
  greylist round-trip (GAP-35) is proven against real Postgres 16. No box/VPS needed.
- The queue subsystem **refuses to start without `TM_QUEUE_ADDR_KEY`** (won't store the plaintext
  exception unencrypted) — a queue-enabled brain with no key is honestly not-ready (bulk → 503).
- The DR-4 key is a NEW secret with its own custody (separate failure domain from the pepper — noted
  in the queue custody comment + the P5 data-handling runbook); it CAN rotate (two-key overlap), unlike
  the pepper.
- Honest residuals (stated in the custody note): backups hold the *encrypted* column (safe iff the key
  isn't in the same snapshot); a brain compromise / core dump exposes key + live plaintext together
  (the F-9/F-19 concentration — encryption defends the at-rest/backup surface, not a compromised brain;
  core-dump suppression + host-hardening, PN-12, is the mitigation).
- Carry-forward: the GAP-37 lease-TTL-> deadline + CAS-on-lease-holder invariant governs the P7
  calibration writes and any future concurrent lease-and-settle path ([[truemail-lease-cas-exactly-once-settlement]]).

## Does this reopen a frozen door?

**No.** Zero new verdict-vocab values (`vocab.go` byte-unchanged); three new operational tables
touching no frozen schema; the bulk endpoint returns the SAME verdict shape (Door 1); the gem +
verifier-node fork are byte-identical (Door 5); the LOG stays append-only, written exactly once per
logical check (Door 2, GAP-33). Door 3's "raw email never persisted" stands — the one bounded
exception is now encrypted at rest (DR-4) AND transactionally purged, and the idempotency key reuses
the existing `email_hash` with no new pepper exposure. PN-13 changes auth *strength* behind the *same*
contract. This ADR **enforces** the doors; it does not mutate any of them.
