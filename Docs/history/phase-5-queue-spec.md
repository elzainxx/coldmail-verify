# Truemail Verification System — Phase 5 (The queue) Design Specification

**Status:** APPROVED (v1.0) — adversarial architecture + backend-architect + architect-review pass folded in (Appendix C: PB1/GAP-37 blocker + PM1/PM2 resolved inline; the two-job platform-UI boundary audited and PASSED); **all 12 OQ-P5 signed off** (Appendix D), the three flagged items (OQ-P5-1/4/9) affirmed with reasoning, OQ-P5-9 strengthened (transactional purge) · **Scope:** Phase 5 only · **Type:** Planning deliverable (no code, no implementation)
**Sources:** `truemail-build-plan.md` (§ PHASE 5), `truemail-architecture-detailed.svg` (§④ SMTP hardening / greylist, §⑥ throughput / bulk, §⑧ topology), and the **approved** `phase-1-foundations-spec.md`, `phase-2-smtp-mocks-spec.md`, `phase-3-first-real-box-spec.md`, `phase-4-resilience-pool-spec.md` (frozen contracts inherited), plus upstream ground truth from the `truemail` gem / `truemail-rack` (Appendix A).
**Inherits (frozen, not redesigned):** the five one-way doors — `/verify` response shape, LOG schema, email hashing, `status`/`reason`/`recommendation` vocabularies, node-list/never-fork. Phase 5 is **additive-only** against all five. Phase 5 is where the **synchronous-v1 assumption is deliberately relaxed**: bulk work becomes asynchronous, greylisted checks acquire a real retry loop, and per-domain pacing gains a time dimension — all **behind** the Phase-4 router, **through** the same gates, **into** the same LOG.

---

## 0. About this document

### 0.1 Purpose
Expand **Phase 5 — The queue** of the build plan into a complete, buildable design specification, at the same rigor as the Phase 1–4 specs. Phase 5's goal, stated by the build plan:

> **Unblock the things synchronous v1 can't do.** Async job queue behind the router (same registry, same strategies); greylist retry / backoff (real 450 handling); bulk `/verify/bulk` returns a job handle; group work by domain, not upload order.

Phases 1–4 built a synchronous request path: a caller asks about one address, the brain runs the layers, routes through the pool, and returns a verdict **now** — and if SMTP can't run cleanly (greylist, rate-cap, saturation) the check **degrades honestly to `unknown`/cheap** rather than waiting. That was correct for every prior phase. But three things are **structurally impossible** synchronously: (a) a greylist `450` means *"try again in a few minutes"* — you cannot honor it inside one HTTP request; (b) pacing a domain's checks *over time* needs somewhere to hold work *between* the paces; (c) a bulk upload of 100k addresses cannot block one HTTP connection for an hour. Phase 5 adds the **one primitive** that unlocks all three: a **durable async job queue behind the Phase-4 router**. Because Phase 5 is about **work scheduling, retry correctness, and idempotency**, the capabilities carrying the most weight are **backend-architect** (queue model, job lifecycle, idempotency keys, state placement) and **architect-review** (the adversarial pass on the retry loop and the two-"job" boundary risk).

### 0.2 Scope discipline
- **In scope:** Everything under build-plan § PHASE 5 — (a) the **async job queue behind the router** (same P1 node registry, same P4 strategies — sticky-by-domain + least-inflight — now fed from a queue as well as from synchronous calls); (b) the **greylist retry / backoff loop** — the real `450` handling every prior spec deferred, consuming the `retry_eligible`/`retry_after_hint_s` metadata P2 §6.5 left in `signals`; (c) **bulk `/v1/verify/bulk` returns a job handle** — the async submission→handle→progress→results lifecycle, the seam shaped since P1 §3.1.1; (d) **group work by domain, not upload order** — queue ordering/spreading that only became possible with the queue + P4 sticky routing + rate limiter.
- **Out of scope:** Phases 6–7. Where a Phase 5 decision touches a later phase, this doc marks the **`SEAM →`** and stops. In particular: **no third-party fallback** (Phase 6 — a check that exhausts its retries settles honestly as `unknown/greylisted`; it does **not** call an external verifier here, §4.7); **no bounce calibration / provider auto-promotion / adaptive backoff-from-outcomes** (Phase 7); **no multi-brain queue coordination** (a single brain is assumed, inheriting P4 GAP-25); and — the boundary the owner flagged — **no platform-facing job UI** (the platform's user-facing `job_items` / live-status / resume / CSV / credits layer is a *separate project*; Phase 5 builds only the **brain's internal verification-work scheduler**, §7.6).
- **No implementation:** no Go, no Ruby, no SQL/DDL, no queue-broker config. Job/queue *shapes*, state placement, idempotency-key derivation, backoff-schedule dials, lifecycle state machines, and ordering semantics only. The "queue" is a **model + contract**, not a runnable system.
- **Additive-only against Phases 1–4:** Phase 5 adds **new operational tables** (a job/work-item store) only where they touch **no frozen schema**; **consumes** the P2 `retry_eligible`/`retry_after_hint_s` signals and the P4 `rate_limited` backpressure without changing their shapes; **reuses** the unchanged P4 router + rate limiter + eligibility gate for every queued check; and adds **at most** additive `signals` keys for retry provenance (§10). It changes **nothing** already frozen, and — critically — it adds **no** new `status`/`reason`/`recommendation` value: a check that exhausts retries settles as the **already-reserved** `unknown/greylisted` (P1 §3.4.3).

### 0.3 How to read the tagging (identical to Phases 1–4)

| Tag | Meaning |
|---|---|
| **`LOCKED`** | A one-way door (inherited) or an already-locked build-plan decision. Additive-only afterwards. |
| **`DIAL`** | A build-time / runtime value tunable later without breaking history or consumers (backoff steps, max-attempts, batch sizes, pacing windows, TTLs). |
| **`DECISION REQUIRED`** | An open choice surfaced with a recommendation but **not** assumed. Needs sign-off before build. |
| **`SEAM →`** | A Phase 5 design point that deliberately leaves room for a later phase; noted, not built. |
| **`GAP`** | A contradiction/omission found across the build plan, the SVG, the P1–P4 specs, and/or upstream truemail reality — flagged, not silently resolved. |

> **Priority rule (governs this doc):** the **greylist retry / backoff loop (§4)** and **idempotency (§5)** are specified in full — they are the two highest-rigor items (the prompt names both; idempotency is "the correctness heart of the phase"). **Group-by-domain pacing (§6)** and the **bulk job lifecycle (§7)** are specified to backend-architect rigor. The **queue architecture (§3)** and **the never-bypass-a-gate invariant (§8)** frame both. Everything else is specified well enough to build and no further.

### 0.4 The three principles Phase 5 must not violate
All inherited, restated because every Phase-5 decision descends from them:

1. **The queue changes *when* a check runs, never *whether* the safeguards apply (`LOCKED` — the phase's master invariant, §8).** A queued or retried check goes through the **identical** eligibility filter (P3 §4.4 / P4 §3.3), sticky routing (P4 §3.2), rate limiter (P4 §4), and warmup caps (P3 §5.2) as a synchronous check. The queue is a *scheduler in front of* the Phase-4 router, never a *bypass around* it.
2. **`unknown` stays first-class; retrying does not manufacture certainty (`LOCKED`, P1 §7.5 / P2 §0.4).** A greylist retry that still fails after `max_attempts` settles as **`unknown/greylisted`**, never `invalid` and never a fabricated `valid`. Retrying buys *more chances to observe a settled answer*; it never *invents* one. Exhausting retries is itself an honest, terminal `unknown`.
3. **Idempotency: a retried or re-queued check must not double-log, double-count a rate budget, or produce two verdicts (`LOCKED`, the correctness heart, §5).** Because retry state is durable (§4.4) and a brain can restart mid-flight, the same logical check may be *attempted* more than once — but it must *settle* exactly once, *log* its settled verdict exactly once (Door 2 append-only is not a licence to append duplicates), and *spend* rate budget once per real SMTP attempt (not once per requeue).

---

## 1. Phase 5 at a glance

### 1.1 What Phase 5 delivers (from the build plan)
> **Phase 5 done when:** greylisted checks are actually retried on a backoff and settle correctly; a bulk submission returns a job handle and completes asynchronously; work is paced by domain over time; and none of it bypasses a single Phase-4 safeguard.

Four provable outcomes, in the build plan's own words:
1. **Async queue behind the router:** a check can be *enqueued* (not just called synchronously) and later *drained* through the **same** P4 router/strategies/rate-limiter/gates (§3, §8).
2. **Greylist retry / backoff:** a `450`/transient result is **retried on a backoff schedule** up to a ceiling, then settles as `unknown/greylisted` — the real `450` handling P2 §6.5 designed the metadata for but explicitly did not build (§4).
3. **Bulk `/v1/verify/bulk` → job handle:** a bulk request returns **immediately** with a job handle; results are fetched by polling the handle; the seam P1 §3.1.1 shaped is cashed in (§7).
4. **Group by domain, not upload order:** the queue **orders and spreads** work so one domain's checks don't burst against its provider — pacing that needs the queue *plus* P4 sticky routing *plus* the P4 rate limiter to be meaningful (§6).

### 1.2 What Phase 5 consumes from Phases 1–4 (the seams now cashed in)
Phase 5 is largely an **exercise in collecting deferred seams**. Every prior spec left an explicit `SEAM → P5`; this phase resolves each:

| Consumed from | What it is | Phase 5 action |
|---|---|---|
| P1 §3.1.1 / §7.9 | Singular `/v1/verify` shaped so `/v1/verify/bulk` (job handle) slots beside it — no contract change to the singular endpoint | Build the **bulk sibling** returning a job handle; the singular synchronous endpoint stays exactly as-is (§7). |
| P1 §5.4 / P4 §3 | `Strategy` interface + the two P4 implementations (sticky-by-domain, least-inflight) | The queue **feeds the same router**; a dequeued check calls `Strategy.Pick` identically to a synchronous one — **no strategy change, no interface change** (§3, §8). |
| P2 §6.5 | Greylist → `unknown/greylisted` **with requeue metadata** in `signals` (`retry_eligible`, `retry_after_hint_s`, `smtp_code:"450"`) — "the actual backoff/requeue/retry loop is P5" | Build the **loop that consumes** that metadata: backoff schedule seeded by `retry_after_hint_s`, gated by `retry_eligible`, ceilinged by `max_attempts` (§4). |
| P2 OQ-P2-6 / P3 PB1 / P4 §3.6 | **Transient/your-fault `unknown`s are never cached; only settled verdicts are** | The retry loop **depends on** this: a greylisted result must *not* be cached (or a retry would be short-circuited by a stale non-answer). Phase 5 reaffirms and consumes the rule (§5.4). |
| P4 §3.4 / §4.6 | A rate-capped check **degrades to cheap** with `smtp_skipped_reason:"rate_limited"`, non-cacheable, **"queuing it is P5"** | The queue is the *right* home for `rate_limited` backpressure: instead of degrading, a **bulk/queued** check can *wait for capacity* (§4.6, §6). The synchronous path still degrades (§8.3). |
| P2 §6.3 / P4 §3.5 | 2-RCPT (target+probe) session reuse proven; **domain-batching many targets into one session is "P4/P5"** | Group-by-domain (§6) *sets up* domain-batched session reuse; whether P5 actually batches many RCPTs per session is scoped in §6.4 (`DECISION REQUIRED`). |
| P4 §8 (metrics) / §5 | `rate_limited` rate + pool-health metrics flagged as the "P5 queue trigger" capacity signal | Those metrics are the **trigger** to *turn the queue on*; Phase 5 adds queue-depth/retry-rate/settle-latency metrics beside them (§9). |

### 1.3 The frozen inheritance (context, not re-litigated)
Carried verbatim from the approved P1–P4 specs — **none may change; Phase 5 only activates, consumes, or adds behind the frozen interfaces:**

- **Door 1** — `/verify` response shape versioned & additive-only. **The bulk endpoint does NOT change the per-address verdict shape** — a bulk job's *results* are a collection of the **identical** `/v1/verify` verdict objects (§7.4). Any retry-provenance fields are additive `signals` keys (§10). The singular endpoint is untouched.
- **Door 2** — the LOG (`verifications`) is append-only. **A retried check logs its *settled* verdict once (§5.3);** intermediate greylisted attempts are either not logged as settled verdicts or logged as explicitly-provisional attempt rows in an operational table (not the verdict LOG) — the append-only LOG is never made to hold two verdicts for one logical check (§5.3, GAP-33).
- **Door 3** — `norm/v1 + hmac-sha256/v1`, `bytea`, global pepper. The **idempotency key** and the **queue's dedup** derive from `email_hash` (+ policy/config) — the *same* hash the cache uses — **no new hashing, no new pepper exposure** (§5.1). Group-by-domain routes on `domain` (in clear, P1 §3.2.2), exactly as P4 sticky routing does.
- **Door 4** — `status`/`reason`/`recommendation` enums additive-only. **Phase 5 emits zero new `status`/`reason`/`recommendation` values.** A retry-exhausted check settles as the already-reserved `unknown/greylisted`; a job that can't run settles cheaply exactly as P4 degrades (§4.7, §8.3).
- **Door 5** — the **truemail gem is never forked**; only **truemail-rack** is. Phase 5 adds **no** box-side behavior at all: the queue, retry loop, and job store are **entirely brain-side**; a queued check calls the box's unchanged `/v1/verify` exactly as a synchronous check does. The box has no idea whether its caller was a queue or a live request (§3.2). The gem stays pinned + CI-asserted-unmodified.
- Boxes are dumb, stateless, disposable; **the brain holds all state in Supabase**. In P5 this is load-bearing: the **queue and all job/retry state are brain-side durable** — a box never holds a job, never retries, never knows a retry happened. This is what lets a box be killed/re-spun (P4) mid-bulk-job without losing queued work (§3.3).

### 1.4 The make-or-break, named up front
Three facts dominate everything below:

- **The queue must not become a gate-bypass (§8, the master invariant).** The single most dangerous failure mode in Phase 5 is a queue that, in the name of "draining faster" or "finishing the bulk job," routes a check around the eligibility filter, the rate limiter, or the warmup cap. If that happens, every reputation guarantee Phases 3–4 built evaporates — a bulk job becomes a way to sprint the whole pool at every provider. The queue is a **producer of checks that enter the same front of the router**; it is architecturally *upstream* of the gates, never *around* them.
- **Idempotency is the correctness heart, and it is subtle because three things must each happen once (§5).** Settle-once (one verdict), log-once (one LOG row, Door 2), spend-once (one rate-budget decrement per real attempt). These are *different* once-nesses with *different* failure modes: a crash between "SMTP succeeded" and "verdict logged" risks a re-attempt that double-spends budget; a naive requeue risks two workers draining the same item; a retry that re-reads a cached greylist risks never retrying at all. Each needs its own guard (§5.2–§5.5).
- **There are two "job" concepts and they must never merge (§7.6, the boundary risk the prompt flagged).** The **brain's queue** schedules *verification work* — it knows about addresses, domains, retries, and rate budgets, and it is ignorant of users, credits, CSVs, and dashboards. The **platform's job** (a separate project) is user-facing — uploads, `job_items`, live status, resume, billing. Phase 5 builds the former and hands results back through the **same `/v1/verify` verdict contract**. If this spec starts designing progress bars, per-user quotas, or CSV parsing, it has crossed the line and must stop (§7.6).

---

## 2. System context for Phase 5

Phases 1–4 built a **synchronous** request path: one address in, one verdict out, degrade honestly if SMTP can't run. Phase 5 adds a **second way in** — a durable queue — that feeds the **same** router. The synchronous path is untouched (P1 §3.1.1 promised the singular endpoint would not change); the queue is a new *producer* sitting in front of the existing router, plus a new **worker loop** (drainer) that pulls from the queue and calls the exact same routing/rate-limiting/gating code the synchronous handler calls. The genuinely new components are a **durable job/work-item store**, a **drainer loop**, and a **retry scheduler** — all brain-side, all in Supabase.

```
                    ┌─────────────────────────────────────────────────────────────────────┐
   caller ─POST────▶│  BRAIN (Go, port 25 CLOSED)                                           │
   /v1/verify       │                                                                        │
   (sync, UNCHANGED)│   SYNC PATH (P1–P4, unchanged): front door → cache → policy →          │
        │           │      cheap layers → provider routing → ROUTER → rate-limiter → SMTP    │
        │           │      → SCORING → LOG → response NOW  (greylist/cap → degrade, §8.3)    │
        │           │                          ▲                                             │
   caller ─POST────▶│   BULK PATH (NEW, §7):    │  same router, same gates (§8)              │
   /v1/verify/bulk  │      submit → enqueue N work-items → return JOB HANDLE immediately     │
        │           │                          │                                             │
        │  poll      │   ┌──────────────────────┼──────────────────────────────────────┐    │
        └───────────▶│   │  QUEUE + DRAINER (NEW) §3/§6                                  │    │
   GET /jobs/{id}    │   │   work_items (durable, Supabase) ── group-by-domain ordering  │    │
                    │   │   drainer loop: dequeue eligible item → [SAME router+gates] ──┘    │
                    │   │        ├─ settled verdict → LOG once (§5.3) → mark item done        │
                    │   │        └─ greylist/transient → RETRY SCHEDULER (§4)                 │
                    │   │   ┌──────────────────────────────────────────────────────────┐    │
                    │   │   │  RETRY SCHEDULER (NEW) §4: due-time = now + backoff(attempt)│    │
                    │   │   │   attempt<max → requeue at due-time; attempt==max → settle  │    │
                    │   │   │   unknown/greylisted (§4.5). Idempotency keys guard §5      │    │
                    │   │   └──────────────────────────────────────────────────────────┘    │
                    │   └────────────────────────────────────────────────────────────────┘  │
                    │                                                                        │
                    │   HEALTH-WATCHER (P4 §5, unchanged) — quarantine still removes nodes    │
                    └───────┬────────────────────────────────────────────────┬───────────────┘
      reads/writes ALL      │                                                 │ HTTPS (per-node token)
      queue + pool state    ▼                                                 ▼
              ┌───────────────────────────────────┐        ┌──────────────────────────────────┐
              │  Supabase (all state)             │        │  POOL — N × P4 box (UNCHANGED)     │
              │   work_items (§3.3, NEW)          │        │   node-1 … node-N                  │
              │   jobs (§7.2, NEW)                │        │   (queue-driven or sync-driven —   │
              │   rate_budgets (P4 §4.4)          │◀──────▶│    the box cannot tell the diff)   │
              │   node_warmup, nodes, node_audits │        └──────────────┬─────────────────────┘
              │   verifications (LOG, unchanged)  │                       ▼ OUTBOUND 25 (paced by domain, §6)
              └───────────────────────────────────┘              honest corporate MX
```

**Phase 5 truth (the queue is a scheduler, not a shortcut):** the box, the router, the rate limiter, the eligibility gate, and the LOG are **byte-for-byte the Phase-4 versions**. What is new lives entirely in the space *between* "a check exists to be run" and "the router runs it": a **durable place to hold** the check (`work_items`), an **ordering** over held checks (group-by-domain, §6), a **retry scheduler** that re-enqueues transient failures on a backoff (§4), and a **bulk submission API** that returns a handle instead of blocking (§7). If any queued check reaches a provider without passing through the identical P4 router+gates, the master invariant (§8) is violated — a `GAP`, not a tuning knob.

**What is genuinely new in Phase 5, by layer:**
- **Brain `internal/queue` (new):** the durable `work_items` store + the **drainer loop** that dequeues and hands each item to the unchanged router (§3).
- **Brain `internal/retry` (new):** the **backoff scheduler** — computes due-time from attempt count + `retry_after_hint_s`, requeues transient failures, settles at the ceiling (§4).
- **Brain `internal/queue` ordering (new):** **group-by-domain** ordering/spreading over the ready set (§6), leaning on P4 sticky routing (same domain → same node → warm path) and the P4 rate limiter (the actual pacing enforcement).
- **Brain — bulk endpoint (`POST /v1/verify/bulk`, `GET /v1/jobs/{id}`, new):** submit → job handle → progress → results (§7). The **only** new request-path surface.
- **Idempotency machinery (§5):** an idempotency key per logical check + a settle-once/log-once/spend-once discipline threaded through the drainer and retry scheduler. Not a component so much as a **correctness invariant enforced at three points**.
- **Supabase:** `work_items` (§3.3) + `jobs` (§7.2), both new operational tables touching **no frozen schema**. `verifications` (LOG), `cache`, hashing, `rate_budgets`, `nodes` — all unchanged.

**What is explicitly NOT new (unchanged from P4):** the router + both strategies, the rate limiter + durable budgets, the eligibility gate, the health-watcher + quarantine, the box/wrapper/gem, the singular `/v1/verify` endpoint and its verdict shape, and the entire LOG/cache/hashing substrate.

---

## 3. The async job queue behind the router

### 3.1 What the queue is (and, more importantly, what it is not) `LOCKED` (the model)
The queue is a **durable, brain-owned store of pending verification work-items**, drained by a background loop that hands each ready item to the **unchanged Phase-4 router**. It is deliberately modest:

- **It is a work list, not a message bus.** A work-item is *"verify this (email, policy) — attempt K, due at time T."* The queue's whole job is to answer *"what should the drainer run next, respecting domain pacing and retry due-times?"* It is **not** a general pub/sub, not a fan-out event system, not a workflow engine.
- **It is a producer in front of the router, never a bypass around it (§8).** The drainer's *only* way to run a check is to call the same routing→gate→rate-limiter→SMTP path the synchronous handler calls. The queue decides *when* an item is offered to the router; the router (with its gates) decides *whether and where* it runs — exactly as for a synchronous request.
- **`DECISION REQUIRED` (OQ-P5-1) — build the queue in Postgres/Supabase (`SELECT … FOR UPDATE SKIP LOCKED`) or bring an external broker (Redis/SQS/RabbitMQ)?** **Recommendation: Supabase-backed `work_items` table with `FOR UPDATE SKIP LOCKED` dequeue.** Rationale: (a) *all* other state is already durable in Supabase (P1 §4, P4 §4.4) — one datastore, one transaction boundary, which is exactly what idempotency (§5) needs (settle + spend + mark-done in one transaction); (b) Phase-5 volumes (a small pool doing honest-corporate `:smtp` — big providers are `:mx`, never queued for SMTP) are modest; (c) an external broker adds a second source of truth that can disagree with the DB, reintroducing the double-run hazard idempotency exists to kill. A broker is a **`SEAM →`** scaling path if queue depth genuinely outgrows Postgres — flagged (GAP-34), not built.

### 3.2 The box cannot tell a queued check from a synchronous one `LOCKED` — Door 5 preserved
A dequeued check calls the box's unchanged `POST /v1/verify` with the identical request the synchronous handler would send. **The box has no `queued` flag, no `attempt` header, no retry awareness** — retry/queue state is entirely brain-side (§1.3, Door 5). This is not an accident of convenience; it is what keeps the box a **pure function** (`email → raw engine JSON`, P1 §5.5) and disposable (P4): a box can be quarantined and re-spun mid-job and the brain simply routes the next attempt to a different node. The queue's existence is invisible below the router.

### 3.3 `work_items` — the durable queue table (shape, not DDL) `LOCKED` (that it's durable) / `DIAL` (fields)
The one new queue table. Brain-owned; boxes never read/write it (P1). Field/type/purpose + locked-vs-dial:

| Field | Type | Null? | Locked/Dial | Purpose |
|---|---|---|---|---|
| `id` | uuid | no | LOCKED | Work-item identity (the unit the drainer locks). |
| `idempotency_key` | bytea | no | LOCKED | The settle-once/dedup key — derived from `email_hash` + `policy_key` + `policy_version` + `config_fingerprint` (§5.1). **Not** the raw email (Door 3). |
| `job_id` | uuid | yes | LOCKED | Owning bulk job (§7.2); `NULL` for a queued *singular* check (if any — §3.4). |
| `domain` | text | no | LOCKED | In-clear domain (P1 §3.2.2) — the group-by-domain ordering key (§6) and the sticky-routing key (P4 §3.2). |
| `email_hash` | bytea | no | LOCKED | Address identity for cache lookup + LOG join (Door 3). The raw email is held only as long as needed to run the check — see OQ-P5-9. |
| `policy_key` / `policy_version` | text | no | LOCKED | Which policy governs this check (P1). Part of the idempotency key. |
| `status` | text | no | DIAL | `ready \| in_flight \| waiting_retry \| settled \| dead` (§3.5 lifecycle). |
| `attempt` | int | no | DIAL | Retry attempt count (0-based); drives backoff (§4.2) and the `max_attempts` ceiling (§4.5). |
| `not_before` | timestamptz | no | DIAL | Earliest time the item may be dequeued — how backoff *due-time* is expressed (§4.2). Ready iff `now ≥ not_before`. |
| `locked_by` / `locked_at` | text / timestamptz | yes | DIAL | Drainer lease (which worker/loop holds it + when) — the crash-recovery reclaim handle (§5.2). |
| `last_reason` | text | yes | DIAL | Last attempt's `reason` (e.g. `greylisted`, `rate_limited`) — operational visibility, not a verdict. |
| `verification_id` | uuid | yes | LOCKED | FK to the settled `verifications` row once done (§5.3) — the single authoritative verdict. |
| `created_at` / `settled_at` | timestamptz | no/yes | DIAL | Provenance + settle-latency metric (§9). |

- **`LOCKED`:** the item is **durable** the moment it is enqueued — a brain restart loses **no** queued work (contrast the ephemeral in-flight counter, P4 §4.3, which is *supposed* to reset). This mirrors the P3 warmup / P4 budget durability discipline: work you've promised to do must survive a crash.
- **The raw email problem (`DECISION REQUIRED`, OQ-P5-9 — owner-approved with a strengthening):** to actually run SMTP the drainer needs the *plaintext* address, but the LOG/cache deliberately hold only `email_hash` (Door 3). **Decision:** hold the plaintext in `work_items` **only for the life of the item**, and **purge it the moment the item reaches a terminal state — as part of the same settlement transaction (§5.3), NOT a later sweep.** Nulling the plaintext is step (4) of the conditional settlement/dead transaction, so a crash can never leave plaintext lingering on a settled/dead row: either the item is still live (plaintext legitimately present, work not yet done) or it is terminal (plaintext already gone, atomically). A background sweep is a **backstop for orphaned/expired rows only**, never the primary purge. This keeps the durable-at-rest plaintext window bounded to "work not yet done."
- **Data-handling posture (`LOCKED`, the one deliberate exception):** *the queue holds plaintext addresses for **work-not-yet-done only**, purged transactionally on settle.* This is the **single, deliberate, bounded exception** to the system's hash-everything discipline (Door 3): every other durable store — the LOG, the cache, `send_outcomes` — holds only `email_hash`. The exception exists because SMTP structurally requires the real address at attempt time and cannot be run from a hash. It must be called out in the data-handling runbook (alongside the P2 §6.4 verifier-identity note) so the exception is *known and bounded*, not discovered later.

> **[CONSOLIDATION NOTE — 2026-07-17 · additive · Door 3 NOT reopened; this affirms the exception] `[Finding-3 / PN-3, accepted]`**
> The in-transaction purge above is correct for the **live table**, but is **backup-unaware**: a DB backup / PITR snapshot is a second copy of `work_items` that includes whatever plaintext was live at snapshot time, and **no purge transaction ever runs against a snapshot** (consistency report §0/§1 F-7). The gap is only visible holding **this exception together with P1's frozen "raw email never persisted"** (P1 §3.3.1 / §3.2.1) — neither phase is wrong alone. **Accepted gate (before P5):** the plaintext column must be excluded from / protected in backups; **mechanism is a P5-time choice** — snapshot exclusion, separate-key at-rest encryption (key ≠ pepper), or short snapshot-retention. Treat backups as containing live plaintext for the retention window. Recorded as [`DECISIONS.md`](../open-decisions.md) **DR-4** / **[ADR-0002](../adr/ADR-0002-queue-plaintext-backup-scope.md)**; gated in [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) §3.7. **P1's Door 3 stands verbatim; nothing here changes the hashing scheme or what any table stores. Mechanism not chosen/built here.**

### 3.4 Singular checks: still synchronous by default `LOCKED` (the boundary) / `SEAM →`
The build plan is precise: **bulk becomes async; singular `/v1/verify` "may remain" synchronous** (P1 §3.1.1). Phase 5 keeps the singular endpoint **synchronous** — a caller asking about one address still gets a verdict *now* (degrading per §8.3 if SMTP can't run). The queue is **fed by bulk submissions** (§7) and by the **retry scheduler** (§4) re-enqueuing transient failures. Whether a *synchronous* greylist should *also* spawn a background retry work-item (so a later re-`/verify` of the same address finds a settled answer) is a genuine question — see §4.6 and OQ-P5-5. **Default: no** — a synchronous greylist degrades to `unknown/greylisted` and is *not* auto-queued unless the caller used bulk; auto-queuing every synchronous transient turns the sync path into a queue producer with no caller asking for async, which muddies the two-path model. `SEAM →` a "retry-and-notify" mode for singular checks is a later, opt-in feature.

### 3.5 Work-item lifecycle (state machine) `LOCKED` (states) / `DIAL` (transitions)
```
   enqueue                dequeue (now≥not_before,          settled verdict
   ─────────▶ [ready] ───── eligible node, budget) ─────▶ [in_flight] ────────▶ [settled] ─(FK verification_id)
                 ▲                                             │
                 │ requeue at not_before = now+backoff         │ greylist/transient  &  attempt<max (§4.5)
                 └──────────────── [waiting_retry] ◀───────────┤
                                       │                       │ attempt==max  OR  non-retryable
                                       │ now≥not_before        ▼
                                       └──────────────▶ ... [dead]  =  settle as unknown/greylisted (§4.5, §4.7)
```
- `ready → in_flight`: the drainer leases the item (`locked_by`/`locked_at`) **only if** `now ≥ not_before`, the domain isn't paced-out (§6), and the router reports an eligible node with rate headroom. Otherwise it stays `ready`/`waiting_retry` (no wasted SMTP attempt — the gate is respected *before* leasing, §8).
- `in_flight → settled`: a settled verdict (any of `valid`/`invalid`/`risky`, or a **terminal** `unknown` that isn't retryable) — log once (§5.3), record `verification_id`, done.
- `in_flight → waiting_retry`: a **retryable transient** (`greylisted`, or — for a *queued* item only — `rate_limited`, §4.6) with `attempt < max_attempts`: bump `attempt`, set `not_before = now + backoff(attempt)` (§4.2), release the lease.
- `in_flight → dead → settled(unknown)`: `attempt == max_attempts` **or** a non-retryable failure — settle honestly as `unknown/greylisted` (§4.5). `dead` is an internal marker; the *verdict* is a normal terminal `unknown` (Door 4 — no new status).
- **Crash recovery:** an `in_flight` item whose `locked_at` is older than the lease TTL is **reclaimed** to `ready` by the drainer (§5.2) — the crash-safety hinge for idempotency.

---

## 4. The greylist retry / backoff loop — HIGHEST RIGOR

The prompt names this the highest-rigor item. Every prior spec deferred real `450` handling to here (P1 §3.4.3 reserved `greylisted`; P2 §6.5 classified it, marked it `retry_eligible`, and stubbed the loop; GAP-12 noted the mock can't even simulate retry-then-accept). Phase 5 builds the loop.

### 4.1 What greylisting actually is, and why it *requires* a queue `LOCKED` (the rationale)
Greylisting is a provider anti-spam tactic: on first contact from an unknown (IP, sender, recipient) triple, the server returns a **transient `4xx` (usually `450`)** — *"I don't know you; come back in a few minutes and I'll probably accept."* A legitimate mailserver retries after a delay and succeeds; naive spam cannons often don't. The critical facts for our design:

- **The correct response is defined in the time domain: wait, then retry.** You cannot honor "come back in N minutes" inside a single synchronous HTTP request — the caller is waiting *now*. This is the structural reason the build plan puts greylist retry in the *queue* phase: **the queue is the only place that can hold a check across the required delay** (§1.1). Synchronously, all we can do is classify it `unknown/greylisted` and degrade (P2 §6.5) — which is exactly what P2 did.
- **A greylist is not evidence about the address.** The mailbox may well exist; the provider simply hasn't decided to trust *us* yet. So `greylisted → unknown`, never `invalid` (P2 §0.4, principle §0.4.2). Retrying does not "prove" the address — it gives the provider the second contact it asked for, after which we observe a *real* (settled) answer (`250`/`550`/etc.).
- **Retrying too fast defeats the purpose.** Retrying before the provider's greylist window elapses just earns another `450` (or worse, looks like the spam behaviour greylisting screens for). So the backoff's *first* step should respect the provider's hint when present (§4.2). This is where greylist retry and P4 per-provider pacing (§6) reinforce each other.

### 4.2 The backoff schedule `DIAL` (values) / `LOCKED` (shape)
The retry scheduler computes, on each transient failure, a **due-time** `not_before = now + backoff(attempt)`. The schedule:

- **Seed from the provider's hint when present:** `retry_after_hint_s` (P2 §6.5, parsed from the `4xx` body if the server offered one) sets the **floor** for attempt 0's delay. Absent a hint, use the policy default first step (`DIAL`, e.g. 5 min — greylist windows are typically minutes, not seconds).
- **Exponential with a cap and jitter (`LOCKED` shape, `DIAL` numbers):** `backoff(attempt) = min(base · 2^attempt, ceiling) ± jitter`. Illustrative defaults (`DIAL`): base 5 min, so ~5m → 10m → 20m → 40m, capped at e.g. 60 min, with ±10–20% **jitter** so a whole domain-batch that greylisted together doesn't retry in a synchronized thundering-herd (the same anti-fingerprint reasoning as P4 §4.2 jitter, now in the *retry* time domain).
- **Why jitter here specifically:** if a bulk job hits a domain that greylists all its addresses at once, every one of those work-items would otherwise become due at the *identical* instant and burst the provider — reproducing the exact scatter P4 fought, just displaced in time. Per-item jitter spreads the retry wave. `LOCKED` that we jitter; the spread is a `DIAL`.

### 4.3 Where retry state lives: durable, no exceptions `LOCKED` — mirrors P3 warmup / P4 budget
Retry state (`attempt`, `not_before`, `status`) lives in the durable `work_items` row (§3.3). The prompt states the hazard exactly: *"a retry that resets on brain restart either loses work or double-runs."* Both failure modes are unacceptable:

- **If retry state were in-memory only:** a brain restart between attempts would either **lose** the pending retry (the greylisted check silently never completes — a bulk job with a permanent hole) **or**, if the item were also somewhere else, **double-run** it (two attempts, double budget spend, maybe double log). 
- **The fix is the same one P3 used for the warmup counter and P4 used for the rate budget (§1.3, P4 §4.4):** the source of truth is **durable in Supabase**. A restarted brain reads `work_items` and resumes exactly where it left off — pending retries are just rows with a future `not_before`. This is the third instance of the identical durability discipline; it is not a new idea, it is a *consistently applied* one.
- **Contrast (deliberate asymmetry, inherited):** the P4 **in-flight counter** is ephemeral *on purpose* (correct-at-zero post-crash, P4 §4.3 / GAP-26). Retry state is the **opposite**: it must survive. The discriminator is the same as P4's — *promised future work* is durable; *momentary liveness* is ephemeral. Phase 5 adds a row to the "durable" side of that ledger.

### 4.4 How a retry re-enters routing: the same gates, from the top `LOCKED` (§8 master invariant applied)
A due retry is **not** a "resume" of the prior attempt — it is a **fresh trip through the entire Phase-4 router**, because the pool may have changed since the last attempt (the node that greylisted might now be quarantined; a different node might be the sticky home; the rate budget refills over the retry window):

- The retry item, when `now ≥ not_before`, re-enters exactly as a new check: **eligibility filter → sticky routing → rate-limiter admit → SMTP** (§8). It may land on a **different node** than the attempt that greylisted — which is fine and often desirable (a fresh (IP, recipient) triple, though see GAP-32 on greylist-per-IP).
- **`GAP-32` (new) — greylisting is keyed on the sending IP, but sticky routing may move the retry to a different IP.** A provider greylists the *(sender-IP, from, rcpt)* triple; it expects the *same IP* to retry. If the retry routes to a different node (because the sticky home is now capped/quarantined and least-inflight picked another), the provider sees a *new* triple and re-greylists — the retry "restarts the clock" instead of satisfying it. **Sticky-by-domain (P4 §3.2) largely mitigates this** (same domain → same node → same IP across attempts, which is *exactly* what greylist retry needs — an unplanned second payoff of stickiness). The residual risk is only when the sticky home is unavailable at retry time. **Resolution:** prefer the **same node** for a retry when it's still eligible (a sticky-with-affinity-to-last-attempt bias); fall back to least-inflight only if the sticky home is truly unavailable, accepting a re-greylist as an honest `unknown` on that attempt. Flagged, recommended, `DECISION REQUIRED` (OQ-P5-6).

### 4.5 The max-attempts ceiling and honest settlement `LOCKED` (that it settles unknown)
- **`max_attempts` (`DIAL`, e.g. 3–5)** is the ceiling. When `attempt == max_attempts` and the check is *still* transient, the work-item settles as **`unknown/greylisted`** — a normal terminal verdict (Door 4, no new value), logged once (§5.3). This is principle §0.4.2 made concrete: **retrying buys more chances to observe a settled answer; exhausting them is itself an honest `unknown`, not a failure to be papered over.**
- **`DIAL` — total retry budget in time, not just count:** a companion `max_retry_age` (e.g. "give up after 6 hours") prevents an item with a long backoff from lingering for days. Whichever ceiling (`max_attempts` or `max_retry_age`) trips first settles the item. Recommended, both `DIAL`.
- **What is retryable vs terminal (`LOCKED` direction, `DIAL` set):** only **transient** signals are retryable — `greylisted` (primary), and optionally `smtp_timeout`/`connection_failed` (network-transient, secondary, separately dialled — mirrors P4 OQ-P4-6's "primary vs secondary" split for the block-rate numerator). A settled `250`/`550`/`catch_all`/`552`-mailbox-full is **terminal on the first observation** — never retried (there is nothing to wait for). `smtp_blocked` (your-IP-blocked) is **not** retried by the greylist loop — that's a *reputation* problem the health-watcher handles by quarantine (P4 §5), not something a retry fixes; retrying a blocked IP just confirms the block. Flagged so "retry all transients" isn't over-read.

### 4.6 The `rate_limited` case: the seam P4 left, now closed `LOCKED` (for queued items) / `SEAM →` honored
P4 §4.6 said: when every eligible node is at its per-provider cap, a check **degrades to cheap** with `smtp_skipped_reason:"rate_limited"`, and *"queuing that check for later is the Phase-5 queue."* Phase 5 closes it — **but only for checks that are already in the queue:**

- **A queued (bulk/retry) item that hits `rate_limited` does NOT settle cheap — it goes `waiting_retry`** with a **short** backoff (capacity, unlike greylisting, frees on the order of the rate window, not provider-decision minutes — so a distinct, shorter `DIAL` step, e.g. seconds-to-a-minute). The queue is precisely the mechanism to "wait for capacity," which is what P4 said it should be. This is the cleanest seam-closure in the phase: **the rate limiter *creates* backpressure (P4); the queue *absorbs* it (P5).**
- **A synchronous check that hits `rate_limited` still degrades cheap, as in P4 (§8.3)** — the sync caller is waiting *now* and did not ask for async; auto-queuing it is the OQ-P5-5 question (default: no). So `rate_limited` has **two** correct behaviours depending on entry path: *sync → degrade* (P4, unchanged), *queued → wait-and-retry* (P5, new). Both honest; neither fabricates a verdict.
- **`rate_limited` retry does NOT count toward `max_attempts`** (`DIAL`, recommended): it is a *capacity* wait, not a *provider-transient* wait — conflating them would let a busy pool prematurely exhaust a greylisted item's retry budget. Track a separate (uncapped-but-`max_retry_age`-bounded) capacity-wait, or count it toward `max_retry_age` only. Flagged (OQ-P5-7).

### 4.7 A retry-exhausted check does NOT call a third party `LOCKED` / `SEAM →` P6
When retries exhaust, the item settles `unknown/greylisted` and **stops**. It does **not** escalate to an external verifier — that is the **Phase-6 fallback**, explicitly out of scope (§0.2). The residue Phase 5 produces (retry-exhausted `unknown`, `catch_all`, `rate_limited`-that-eventually-gave-up) is *exactly* the residue P6 will route to a fallback — but Phase 5 **labels it honestly and leaves it**; `reason=external_verified` stays reserved (P1). `SEAM →` P6 routes this residue; P5 does not.

---

## 5. Idempotency — the correctness heart — HIGHEST RIGOR

The prompt: *"a retried or re-queued check must not double-log, double-count against rate budgets, or produce two verdicts."* Because retry state is durable (§4.3) and a brain can crash at any instant, the same logical check **will** occasionally be *attempted* more than once. The design must guarantee it *settles*, *logs*, and *spends* exactly once. These are **three different once-nesses** with three different guards.

### 5.1 The idempotency key — one logical check, one identity `LOCKED`
Every work-item carries an `idempotency_key` (§3.3) derived from the **same identity the cache uses**: `hash(email_hash ‖ policy_key ‖ policy_version ‖ config_fingerprint)` (P1 §4.2 cache key + policy). Properties:

- **Same address + same policy + same engine/layer config ⇒ same key.** A duplicate submission (same address twice in one bulk upload, or the same address in two jobs) maps to the same key — enabling **dedup** (§5.5) and making "settle once" well-defined.
- **Derived from `email_hash`, never the raw email (Door 3).** No new hashing, no pepper exposure to the queue — the queue reuses the artifact Door 3 already produced. `config_fingerprint` in the key means a config change (which *should* invalidate a prior answer, P2 §4.5) yields a *new* logical check — correct, and consistent with cache semantics.
- **The key is the unit of "once."** Settle-once, log-once, spend-once are all defined *per idempotency key* (with spend-once qualified per *real attempt* — §5.4).

### 5.2 Settle-once + the crash-in-the-middle problem `LOCKED` — the lease + reclaim
The dangerous window: the drainer leases an item (`in_flight`), calls the box, SMTP **succeeds**, and the brain **crashes before writing the verdict**. On restart the item is still `in_flight` with a stale lease. Guards:

- **Lease + TTL reclaim:** `in_flight` items carry `locked_by`/`locked_at` (§3.3). A `ready` item is only dequeuable by *one* drainer at a time (`FOR UPDATE SKIP LOCKED`, §3.1) — so two workers never grab the same *ready* row (this kills the naive "two workers, same item" double-run). An `in_flight` item whose `locked_at` exceeds the lease TTL is presumed crashed and **reclaimed to `ready`** — so a crash never strands an item, at the cost of a *possible* re-attempt (which the spend-once guard, §5.4, must tolerate).
- **`GAP-37` (new, from review PB1) — TTL-reclaim can run two *live* attempts concurrently, and `SKIP LOCKED` does NOT prevent it.** `SKIP LOCKED` only stops two workers grabbing the same *ready* row at the same instant. It does **nothing** for a TTL-reclaim: if the original drainer is not dead but merely **slow** (a long SMTP `response_timeout`, a GC pause, a stalled provider), the reclaimer re-readies the row and a **second** attempt runs *while the first is still open* — two real SMTP sessions **and**, worse, a **race on the work-item's next state** (attempt A greylists → wants `waiting_retry`; attempt B gets `250` → wants `settled`; last writer wins, so a settled verdict could be resurrected into retry, or a retry silently dropped). This is the sharpest correctness hazard in the phase and the naive lease is not enough. **Resolution (two-part, `LOCKED`):** (1) **the lease TTL must exceed the brain's own maximum SMTP client deadline** (P2 §6.5 — the brain already bounds how long one `/v1/verify` can take), so a *live* attempt's lease never expires before the attempt itself times out; a reclaim then means the holder is genuinely dead, not slow. (2) **Every state-changing settlement/requeue is a conditional compare-and-set** — `UPDATE work_items SET … WHERE id=? AND locked_by=<me> AND status='in_flight'`. An attempt that lost its lease (was reclaimed) finds **zero rows updated** and **discards its result without touching state or the LOG** — it neither settles nor requeues. Exactly one attempt (the current lease-holder) can transition the item. The lost attempt's SMTP connection still happened (budget spent, §5.4 — accepted), but it can never corrupt the item's state or double-log.
- **The unavoidable re-attempt, made safe:** because SMTP is a side-effecting network call to a third party, we **cannot** get exactly-once *delivery to the provider* — a crash after "SMTP succeeded, verdict unwritten" means we *will* re-attempt. Phase 5 accepts **at-least-once *attempts*, exactly-once *settlement*** (the standard, honest distributed-systems posture). The re-attempt is safe because: (a) an SMTP RCPT probe is **read-only at the provider** (it never sends DATA / delivers mail — P2 §5.7), so re-probing has no external side effect beyond one extra connection (budgeted, §5.4); and (b) **settlement is guarded by the compare-and-set above (GAP-37)** so only the lease-holder's result is ever committed. Settlement is made exactly-once by §5.3.
- **`GAP-33` (new) — "append-only LOG" (Door 2) vs "exactly-once verdict."** Door 2 says the LOG is append-only; idempotency says one verdict per logical check. These are in tension only if a re-attempt naively appends a *second* verdict row. **Resolution:** settlement writes the `verifications` row and stamps `work_items.verification_id` **in one transaction** (§5.3); a reclaimed re-attempt that finds `verification_id` already set **does not write again** — it is a no-op settle. Append-only is preserved (we never UPDATE a verdict); duplicate-append is prevented (we never append a *second* verdict for a key that already settled). The two doors are compatible; the guard is transactional, not schema.

### 5.3 Log-once — the settlement transaction `LOCKED`
Settlement of a work-item is a **single Supabase transaction**, guarded by the §5.2 compare-and-set (only the current lease-holder commits), that: (1) inserts the one `verifications` LOG row (Door 2 append), (2) sets `work_items.status='settled'` + `verification_id` (the FK proving *this* item produced *that* verdict) **conditional on `locked_by=<me> AND status='in_flight'`**, (3) if part of a job, increments the job's settled-counter (§7.2), and (4) **nulls the plaintext address** (§3.3 purge-on-settle — plaintext gone the instant the item is terminal, atomically, so a crash never strands it). The identical transaction shape applies to a `dead` (retry-exhausted) settlement. Because it's one conditional transaction keyed by the item id + lease:

- A crash *before* commit → item stays `in_flight` → lease expires → reclaimed → re-attempted → settles later (no LOG row was written, so no duplicate).
- A crash *after* commit → item is `settled` with `verification_id` set → a reclaim sees it's already settled (not `in_flight`) → the conditional update matches **zero rows** → **no-op** (GAP-33/GAP-37).
- **A slow (not dead) attempt whose lease was reclaimed** → its settlement's `WHERE locked_by=<me>` matches zero rows → it **discards its result**, logs nothing, touches no state (GAP-37).
- **Intermediate greylisted attempts are NOT settled verdicts and are NOT logged to `verifications`.** A `450` on attempt 1 updates `work_items` (`attempt++`, `not_before`, `last_reason='greylisted'`) but writes **no** LOG row — the LOG gets exactly one row, at final settlement (whether that's a `250`, a `550`, or a retry-exhausted `unknown/greylisted`). **`DECISION REQUIRED` (OQ-P5-8):** whether per-attempt SMTP transcripts (useful for debugging/P7) are captured in an operational `work_item_attempts` table (not the verdict LOG). **Recommendation:** yes, a lightweight append-only attempts table for observability — it keeps the verdict LOG clean (one settled verdict) while preserving the per-attempt transcript the flywheel may later want. Distinct from `verifications`; never conflated.

### 5.4 Spend-once (per real attempt) — the rate-budget interaction `LOCKED`
This is the subtlest once-ness, because it is **not** once-per-key — it is **once per real SMTP attempt**:

- **Each *real* SMTP attempt legitimately spends one rate-budget unit** (P4 §4.4) — that's correct: the provider really did see that connection. A greylist retry that opens a *new* SMTP session *should* decrement the budget again, because it's a real second connection the provider observes. So retries **do** consume budget across attempts — by design (this is why `rate_limited` and greylist retry interact, §4.6).
- **What must NOT double-count is a *requeue that never reached SMTP*** — e.g. the drainer leased an item, found no eligible node / no rate headroom, and released it back to `ready` (§3.5). That path **spends nothing** (no SMTP happened) — the budget is decremented **only at the point of actual admission to a real SMTP call** (P4's admission point), never at dequeue. Enqueue/dequeue/requeue are free; only a real provider connection costs budget.
- **The crash-after-SMTP re-attempt (§5.2) does double-spend by one** — the crashed attempt spent a unit (real connection happened), and the re-attempt spends another. This is **accepted and correct**: the provider genuinely saw two connections. The budget accounting reflects reality (two connections = two units), which is the *conservative* direction — it slightly *under*-utilizes the budget after a crash, never *over*-connects the provider. Erring toward fewer provider connections is the right side of the reputation asymmetry (P4 §0.4.2). Flagged so it's a conscious accepted cost, not a hidden bug.

### 5.5 Dedup — the same address submitted twice `LOCKED` (that we dedup at settle) / `DIAL` (in-flight collapse)
- **Within a bulk job**, the same (address, policy) appearing twice is the **same idempotency key** — dedup collapses it to **one** work-item (or one settled verdict fanned out to both input positions). The build plan and P1 §0.2 are explicit that **list-level dedup is a platform concern, not the engine's** — so Phase 5 does **not** deduplicate the *caller's input list*; it dedups *its own work* (two identical keys ⇒ one SMTP attempt) as an efficiency + spend-once measure, and lets the platform decide how to present duplicate input rows. This respects the P1 boundary while not wastefully verifying the same address twice.
- **Cache short-circuit (inherited, reaffirmed):** before enqueuing *or* before dequeuing→SMTP, the same **cache lookup** the synchronous path uses applies: a settled cached verdict (P2 OQ-P2-6 — *only* settled, never transient) **satisfies the item without an SMTP attempt**. This is the biggest efficiency lever in bulk (a re-uploaded list of known addresses costs near-zero SMTP) and it's free — the cache already exists (P1 §4.2). **Transient `unknown`s are not cached (P2 OQ-P2-6 / P4 §3.6), so a greylisted item is never satisfied by a stale non-answer** — the exact property §4 needs to actually retry. This is why P2's "cache only settled" rule was load-bearing for P5.
- **`DIAL` — in-flight dedup:** if key K is already `in_flight` and K is submitted again, collapse to the existing item vs enqueue a duplicate that the settle-once guard (§5.2) will later no-op. Recommend collapse-on-enqueue when cheap; the settle-once guard is the backstop either way.

---

## 6. Group work by domain, not upload order

### 6.1 Why upload order is the wrong order `LOCKED` (the rationale)
A raw bulk upload is typically **sorted by nothing useful** — or worse, sorted in a way that clusters one domain (a company exporting its CRM: hundreds of `@acme.com` in a row). Draining in upload order means: (a) a run of same-domain addresses **bursts that one provider** back-to-back (the exact reputation hazard P4's rate limiter and jitter fight — now arriving *pre-clustered* by the upload); and (b) conversely, interleaved domains waste the warm sticky session (P4 §3.2) by hopping providers every item. **Neither the reputation goal nor the efficiency goal is served by upload order.** The queue must impose its own order.

### 6.2 What group-by-domain does `LOCKED` (intent) / `DIAL` (mechanics)
The drainer selects the next item to run not by `created_at` but by a **domain-aware spread**: it round-robins *across* domains while pacing *within* each domain. Concretely, the ready-set is partitioned by `domain`, and the drainer picks from domains that are **not currently paced-out** (i.e., whose provider has rate headroom, per the P4 limiter, and whose last start respects the jitter/spacing):

- **Across domains → fairness + parallelism:** two different domains (hence, largely, two different providers on two different sticky nodes, P4 §3.2) can be in-flight simultaneously without either bursting. A 100k-address job touching 5k domains drains *broadly*, not one-domain-at-a-time.
- **Within a domain → pacing over time:** a single domain's many addresses are **spaced out** according to the P4 per-provider rate cap + jitter — the queue simply *doesn't offer* the next `@acme.com` item until acme's provider has headroom. This is pacing *over time*, which is **only possible because the queue can hold the rest of acme's work** while it waits (§1.1) — the synchronous path had nowhere to hold it.
- **`LOCKED` division of labour:** the **queue orders** (which domain's item is offered next); the **P4 rate limiter enforces** (whether the offered item is admitted to SMTP). Group-by-domain is not a *second* rate limiter — it is the **scheduler that feeds** the one P4 limiter a well-spread stream so the limiter rarely has to say "no." When it does say no (`rate_limited`), the item waits (§4.6). Ordering + limiting are, again, one mechanism in two parts (echoing P4 §3.1).

### 6.3 The three-way interlock: queue + sticky + rate-limiter `LOCKED` (the synthesis)
Group-by-domain is the capstone of a mechanism assembled across three phases, and it only works because all three parts exist:
- **P4 sticky-by-domain** ensures a domain's checks *concentrate on one node/IP* (so per-provider pacing is meaningful — P4 §3.1).
- **P4 rate limiter** *bounds* how fast that one IP approaches that provider (concurrency + rate + jitter — P4 §4).
- **P5 group-by-domain + queue** *feeds* the limiter a domain-spread, time-spaceable stream and *holds* the overflow — turning "degrade when capped" (P4, synchronous) into "wait and drain when capacity frees" (P5, queued).
**Synthesis (`LOCKED`):** *sticky concentrates, the rate limiter paces, the queue holds and spreads over time.* Remove any one and bulk verification either bursts providers (no pacing), caps out and degrades everything (no queue), or paces the wrong denominator (no stickiness). This is the single most important design statement in Phase 5, and it is the direct continuation of P4 §3.1's synthesis.

### 6.4 Domain-batched session reuse — the P2/P4 seam, scoped `DECISION REQUIRED` (OQ-P5-4) / `SEAM →`
P2 §6.3 proved 2-RCPT (target+probe) session reuse; P4 §3.5 said batching *many targets for one domain into one SMTP session* is a "P4/P5 optimization." Group-by-domain *sets it up* (same domain → same node → same warm connection → many ready items). But whether P5 actually holds one SMTP session open for N RCPTs is a **real decision with a Door-5 tension** (the pinned gem validates one address per `Truemail.validate` — the same GAP-13 constraint that shadowed the 2-RCPT probe):

- **Recommendation: do NOT build many-RCPT-per-session batching in P5.** Group-by-domain delivers most of the reputation benefit (spacing + concentration) *without* holding long multi-RCPT sessions, and multi-RCPT-per-session reopens the GAP-13 never-fork-the-gem tension (P2 OQ-P2-11) at pool scale. Ship group-by-domain *ordering/pacing* now; leave *session-level batching* as a `SEAM →` optimization contingent on the GAP-13 spike (P2 Appendix D standing directive) and real throughput data. Flagged, recommended, not assumed.
- **`SEAM →`:** if measured connection-setup cost or provider connection-rate limits later justify it, domain-batched multi-RCPT sessions are the optimization — but only via a wrapper-side session (never a gem patch, Door 5), and only after the GAP-13 spike resolves.

### 6.5 Fairness across jobs `DECISION REQUIRED` (OQ-P5-3)
Two bulk jobs submitted close together shouldn't let a huge first job starve a small second one. **Recommendation:** the drainer's domain-spread selection is **job-agnostic at the domain level** (it spreads across *domains*, which naturally interleaves jobs that touch different domains) plus a **`DIAL` per-job concurrency ceiling** (no single job monopolizes more than X% of in-flight capacity) so a 1M-address job and a 100-address job both make progress. Full weighted-fair-queuing / priority tiers are a `SEAM →` (ties to the platform's tier concept — but the *scheduling* is brain-side; the *policy* of who's premium is platform-side, §7.6). `DECISION REQUIRED` on whether P5 needs more than the simple ceiling now — recommend the ceiling, defer WFQ.

### 6.6 What group-by-domain does NOT do (scope fence)
- **It does not change routing** — sticky-by-domain (P4) still picks the node; group-by-domain only picks *which ready item is offered next*. Ordering ≠ routing.
- **It does not enforce rate** — the P4 limiter does; the queue just avoids offering doomed-to-be-capped items (§6.2). If the queue and limiter ever disagree, the **limiter wins** (it's the gate, §8).
- **It does not reorder for verdict correctness** — order affects *when*/*reputation*, never *what* a verdict is. Any two orderings produce the same per-address verdicts (idempotency, §5).

---

## 7. The bulk job lifecycle — `/v1/verify/bulk` returns a job handle

### 7.1 The lifecycle, end to end `LOCKED` (shape)
The seam P1 §3.1.1 shaped: bulk submission is **async**, returning a handle immediately; results are fetched by polling.

```
  POST /v1/verify/bulk {addresses:[…], policy?}     ─────▶  202 Accepted { job_id, status:"accepted", count:N }
       │  (brain: dedup+cache-check §5.5, enqueue M work-items, create jobs row §7.2)
       │
  GET  /v1/jobs/{job_id}                             ─────▶  200 { job_id, status, counts:{total,settled,pending,…} }
       │  (poll for progress; status ∈ submitted|running|complete|complete_with_unknowns)
       │
  GET  /v1/jobs/{job_id}/results?cursor=…            ─────▶  200 { results:[<verdict>, …], next_cursor }
       │  (paginated; each <verdict> is the IDENTICAL /v1/verify object — Door 1)
```

- **Submit → handle (`202`):** the brain validates the request, runs the **cheap** dedup + cache short-circuit (§5.5) so already-known addresses may settle instantly, enqueues the rest as `work_items` (§3.3), creates the `jobs` row (§7.2), and returns the `job_id` **without waiting for any SMTP**. `LOCKED`: the response is a *handle*, never inline verdicts (that's the whole point — P1 §3.1.1).
- **Progress:** `GET /v1/jobs/{id}` returns counts derived from `work_items` for that `job_id` (total/settled/pending/waiting_retry). This is a **coarse brain-side progress**, not a live per-row feed (that's the platform's job, §7.6).
- **Results:** `GET /v1/jobs/{id}/results` returns the settled verdicts, **paginated by cursor**, each verdict the **byte-identical `/v1/verify` object** (Door 1). A job with retry-exhausted items returns those as `unknown/greylisted` verdicts — honest, terminal (§4.5).

### 7.2 `jobs` — the bulk-job table (shape, not DDL) `LOCKED` (that it's brain-side) / `DIAL` (fields)
The second new operational table. Brain-owned; the **brain's** notion of a bulk verification job — *not* the platform's user-facing job (§7.6):

| Field | Type | Null? | Locked/Dial | Purpose |
|---|---|---|---|---|
| `id` (`job_id`) | uuid | no | LOCKED | The handle returned to the caller. |
| `policy_key` / `policy_version` | text | no | LOCKED | Policy governing all items in the job (P1). |
| `status` | text | no | DIAL | `submitted \| running \| complete \| complete_with_unknowns` (§7.3). |
| `total_count` | int | no | DIAL | Items in the job (post-dedup). |
| `settled_count` | int | no | DIAL | Incremented in the settle transaction (§5.3) — the progress numerator. |
| `submitted_at` / `completed_at` | timestamptz | no/yes | DIAL | Provenance + job-latency metric (§9). |
| `caller_ref` | text | yes | DIAL | **Opaque** caller-supplied correlation string echoed back — the brain does not interpret it (§7.6 boundary: this is how the platform correlates *its* job to *ours* without the brain knowing about users). |

- **`LOCKED`:** this table holds **verification-work** state only — counts, status, policy. It holds **no** user id, no credit balance, no CSV, no filename, no per-row UI state. The single concession to the platform is the **opaque `caller_ref`** (§7.6).
- **Retention (`DIAL`):** completed jobs + their `work_items` are retained for a results-fetch window (e.g. days), then the `work_items` are purged (verdicts live forever in the LOG; the *queue* rows are transient operational state). Mirrors P1 §3.2.6 retention discipline — the LOG is permanent, operational tables are not.

### 7.3 Job completion semantics `LOCKED` (that unknowns don't block completion)
- A job is **`complete`** when every work-item is `settled` (a terminal verdict, any of `valid/invalid/risky/unknown`). Retry-exhausted `unknown/greylisted` items **count as settled** (§4.5) — **a job is not held open forever waiting for a greylist that will never clear.** `max_retry_age` (§4.5) bounds the longest an item — hence a job — can stay pending.
- **`GAP-38` (new, from review PM1) — the `settled_count` is a denormalized cache and can drift; completion must not trust it blindly.** §5.3 increments `jobs.settled_count` in the settlement transaction — fine on the happy path. But a denormalized counter is exactly the kind of state that drifts under the reclaim/compare-and-set path (a lost attempt that no-ops must **not** increment; a crash between LOG-insert and counter-increment in a badly-composed transaction could skew it). If completion is judged purely by `settled_count == total_count`, a single missed increment leaves a finished job **forever "running."** **Resolution (`LOCKED`):** (1) `settled_count` is incremented **inside the same conditional settlement transaction** as the `work_items` row (§5.3) — so it moves iff the item truly settled (atomic; a no-op attempt increments nothing); (2) completion is **authoritatively** `NOT EXISTS(work_items WHERE job_id=? AND status NOT IN (settled,dead))`, with `settled_count` used only as a **cheap progress display**, never as the completion oracle. The count can be lazily reconciled from the authoritative query. Denormalized progress, authoritative completion — never conflated.
- **`complete_with_unknowns`** (`DIAL`, recommended) is a convenience status distinguishing "all settled, some are honest `unknown`s" from "all settled cleanly" — purely informational; the per-verdict truth is always in the results. This is *not* a new `status`/`reason` enum value (Door 4) — it's a **job**-status string, a different namespace from the **verdict** vocabulary. Flagged so the two aren't confused.

### 7.4 Results are the same verdict objects — Door 1 preserved `LOCKED`
Each result element is the **identical `/v1/verify` response object** (P1 §3.1) — same `status`/`reason`/`recommendation`/`signals`/`meta`, including `meta.node_id`/`verifier_ip` and any retry-provenance `signals` (§10). **A consumer that can parse a synchronous verdict can parse a bulk result with zero changes** — that is the Door-1 payoff and the reason bulk was shaped as a *sibling* of the singular endpoint (P1 §3.1.1), not a divergent API.

### 7.5 Auth, limits, backpressure `DIAL` / `SEAM →`
- **Auth:** the same bearer token on the front door (P1 §3.5.4). Per-caller quotas / tiers are **platform-side** (§7.6) — the brain enforces only pool-protective limits.

> **[CONSOLIDATION NOTE — 2026-07-17 · additive · auth upgrade, no contract change] `[PN-13 / F-20, accepted]`**
> Cross-phase security review (F-20): by P5 this front door is the **bulk-PII submission surface**, yet it is guarded by a single **static shared bearer token**. Accepted gate (before P5 bulk ships): upgrade to **per-caller keys + rotation**. This is an auth-strength change behind the *same* `/v1/verify(/bulk)` contract — no Door-1 field or vocabulary changes. Gated in [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) §3.8. **Not built here.**
- **Submission backpressure (`DIAL`):** a max addresses-per-submission and a max in-flight-jobs guard (protect the brain + Supabase from an unbounded upload). Over-limit → `429`/`413` with a clear message, not a silent truncation.
- **`SEAM →`:** streaming/webhook result delivery (push instead of poll) is a later convenience; P5 ships **poll** (simplest, statelessly correct). Flagged.

### 7.6 The two-"job" boundary — the line Phase 5 must not cross `LOCKED` (the prompt's flagged risk)
There are **two distinct "job" concepts**, at two layers, and conflating them is the specific failure the prompt warned about:

| | **Brain's queue job (THIS phase)** | **Platform's job (separate project — NOT this phase)** |
|---|---|---|
| **Knows about** | addresses, domains, policies, work-items, retries, rate budgets, verdicts | users, accounts, credits/billing, CSV files, filenames, upload UI, live row-status, resume, export |
| **Identity** | `job_id` (opaque handle) | platform `job_items` rows tied to a user + upload |
| **API** | `POST /v1/verify/bulk` → handle; `GET /v1/jobs/{id}` → counts; results = `/v1/verify` verdicts | the platform's own UI/API over its own DB |
| **Correlation** | echoes an **opaque `caller_ref`** it never interprets | stores our `job_id` + maps rows to its `job_items` |

- **`LOCKED` boundary rules:** (1) the brain's queue is **ignorant of users, credits, and CSVs** — it schedules verification work and hands back verdicts through the **same `/v1/verify` contract**; (2) it exposes **counts**, not a live per-row UI feed — the platform builds any progress bar from polling + its own `job_items`; (3) the **only** coupling is the opaque `caller_ref` (brain → platform correlation) and the platform storing our `job_id` (platform → brain correlation); (4) **credits, quotas, resume-after-close, and CSV parsing are platform concerns** — if this spec ever specifies them, it has crossed the line.
- **Why the separation matters (not just tidiness):** the brain is the *verification engine* — reusable by any caller, testable without a UI, and free of the platform's release cadence. Bleeding user/credit/CSV concepts into the brain would (a) couple the engine to one product, (b) put PII (filenames, user ids) into the verification substrate, and (c) duplicate the platform's own `job_items` state, creating a two-writer consistency problem. The clean contract — **submit work, poll counts, fetch verdicts** — is what keeps the brain a pure verification service. **This is a `LOCKED` architectural boundary; the review pass (Appendix C) explicitly checks that nothing in §7 designed the platform's UI.**

---

## 8. The master invariant: the queue changes *when*, never *whether* — `LOCKED`

This is the phase's most important safety property, stated once, explicitly, because everything above depends on it.

### 8.1 The invariant `LOCKED`
> **Every check that reaches a provider — synchronous, queued, or retried — passes through the identical Phase-4 router: eligibility filter (P3 §4.4 / P4 §3.3) → sticky routing (P4 §3.2) → rate-limiter admission (P4 §4) → warmup cap (P3 §5.2). The queue determines *when* a check is offered to that router; it never determines *whether* the safeguards apply, and it never routes around them.**

The queue is architecturally **upstream** of the router — a producer of checks that enter at the same front door a synchronous request does. There is **no** "drain mode," "catch-up mode," or "finish-the-job" path that skips a gate. A bulk job that is behind schedule **stays behind schedule**; it never buys speed by bypassing pacing.

### 8.2 Why this is the make-or-break `LOCKED`
The whole reputation apparatus of Phases 3–4 — audit gate, per-provider caps, warmup, quarantine — exists to protect IP reputation. A queue's natural pressure is *throughput* ("drain 100k items fast"). If that pressure were ever allowed to override the gates, **a single bulk upload would sprint the entire pool at every provider simultaneously** — the precise catastrophe P4 §3.1 and §4 were built to prevent, now triggerable by any large job. So the invariant isn't a nicety; it's the reason the queue is *behind* the router and not *instead of* it. The build plan's own words: *"same registry, same strategies"* — Phase 5 adds a feeder, not a fast lane.

### 8.3 The two entry paths, one router `LOCKED`
| | Synchronous `/v1/verify` (P1–P4) | Queued (bulk / retry, P5) |
|---|---|---|
| Eligibility filter | applies | **applies (identical)** |
| Sticky routing | applies | **applies (identical)** |
| Rate-limiter admit | applies | **applies (identical)** |
| Warmup cap | applies | **applies (identical)** |
| On `rate_limited` | **degrade to cheap** (P4 §4.6, waiting caller) | **wait + retry** (§4.6, queue can hold) |
| On greylist `450` | **degrade to `unknown/greylisted`** (P2, waiting caller) | **retry on backoff** (§4, queue can hold) |
| Verdict shape | `/v1/verify` object | **identical `/v1/verify` object** (Door 1) |

The **only** differences are in the right column's two "wait" rows — and those are about *holding work across time*, which the queue can do and the synchronous path cannot. The gates are identical; the difference is exclusively temporal. That is the entire thesis of Phase 5.

### 8.4 What the queue does NOT do (scope fence)
- **No gate bypass** (§8.1) — the master invariant.
- **No box-side change** — the box never learns it was called by a queue (§3.2, Door 5).
- **No new verdict vocabulary** — retry-exhaustion settles as reserved `unknown/greylisted` (Door 4).
- **No third-party escalation** — retry-exhausted residue is left honest for P6 (§4.7).
- **No multi-brain coordination** — single brain assumed, inheriting P4 GAP-25 (the queue's `FOR UPDATE SKIP LOCKED` is single-brain-correct; multi-brain drainer coordination is `SEAM →`, GAP-34).
- **No platform UI** — counts + verdicts only (§7.6).

---

## 9. Observability (additive to P4 §8)

Phase 5 adds queue-specific metrics beside the P4 pool/reputation metrics; **all derive from the new operational tables + the unchanged LOG** — no schema change to `verifications`.

### 9.1 Metrics `DIAL` (thresholds)
| Metric | Source | Why it matters |
|---|---|---|
| **queue depth** | `count(work_items WHERE status IN (ready,waiting_retry))` | backlog size; sustained growth = pool too small for demand (the P4 §8 "add a node" signal, now measured directly) |
| **retry rate** | fraction of items entering `waiting_retry` (greylist vs rate_limited, split) | high greylist-retry = provider distrust of the pool; high rate_limited-retry = capacity pressure |
| **settle latency** | `settled_at − created_at`, percentiles | how long bulk work takes end-to-end; the retry backoff dominates the tail |
| **retry-exhaustion rate** | items settling `unknown/greylisted` at `max_attempts` | how often retries *fail* to reach a settled answer — feeds the P6 fallback business case |
| **job completion time** | `jobs.completed_at − submitted_at` | the caller-facing SLA for bulk |
| **dedup/cache-hit on enqueue** | items satisfied by cache/dedup without SMTP (§5.5) | the biggest bulk-cost lever; a re-uploaded known list should show near-100% |

### 9.2 Alerts `DIAL`
| Alert | Trigger | Severity |
|---|---|---|
| **queue-backlog-growing** | queue depth rising monotonically over a window / above a floor | warn → high | 
| **retry-exhaustion-spike** | retry-exhaustion rate above baseline | warn (provider-distrust or capacity signal) |
| **job-stuck** | a job's `settled_count` unchanged for longer than expected given `max_retry_age` | high (possible drainer wedge / crash-reclaim failure) |
| **drainer-liveness** | drainer loop hasn't advanced (no dequeue) while `ready` items exist | high (the queue's equivalent of P4's health-watcher liveness) |

- **`LOCKED` linkage:** queue-backlog + rate_limited-retry are the **direct capacity signal** the P4 §8 spec pointed at ("add a node or, later, queue"). Now the queue *is* here, so the signal becomes "add a node" (the queue absorbs bursts but can't create provider capacity — a permanently-growing backlog means the *paced* throughput genuinely can't meet demand, which only more IPs fix). `SEAM →` auto-scaling the pool from queue depth is later (P4 §6.3 already fenced auto-provisioning out).

### 9.3 What observability does NOT do here (scope fence)
- **No per-user/per-job-owner analytics** — that's platform-side (§7.6); the brain reports work-scheduling health.
- **No bounce/outcome correlation** — that's the P7 flywheel; P5 reports *operational* queue health, not verdict *accuracy*.

---

## 10. Additive-only ledger (what Phase 5 adds to the frozen contracts)

| Additive item | Type | Set when | Source § |
|---|---|---|---|
| `work_items` | new **operational table** | on enqueue (bulk/retry) | §3.3 |
| `jobs` | new **operational table** | on bulk submit | §7.2 |
| `work_item_attempts` (recommended, OQ-P5-8) | new **operational table** | per SMTP attempt (transcripts) | §5.3 |
| `signals.attempt` / `signals.retry_of` (candidate) | additive `signals` keys on the settled verdict | when a verdict settled after ≥1 retry — provenance for P7 | §10 note |
| `job_id` / `caller_ref` echo | fields on the bulk response object (additive, bulk-only) | bulk submit/results | §7.1/§7.2 |

- **`signals` retry-provenance keys (`DECISION REQUIRED`, OQ-P5-10):** whether to stamp `signals.attempt` (how many tries it took) / `signals.retry_settled=true` on a verdict that settled after retries. **Recommendation: yes, additive `signals` keys** — they cost nothing (open jsonb, Door-1-additive per P1 §3.1.3), and the flywheel (P7) will want "did retrying change the outcome?" as calibration input. Additive-only; **no** new `status`/`reason`/`recommendation`/`flags`/`verifications`-column.
- **Zero frozen-contract change (the ledger's headline):** Phase 5 adds **two (or three) operational tables** and **additive `signals` keys**, and introduces **one new request-path surface** (`/v1/verify/bulk` + `/v1/jobs/{id}`) that returns the **same verdict objects**. It adds **zero** new `status`/`reason`/`recommendation`/`flags` values, **zero** columns to `verifications`, **zero** changes to the singular endpoint, cache, hashing, router, rate limiter, or box. A diff against the frozen Door-1/Door-4 contracts shows no removal, rename, or type change.

---

## 11. GAPs (contradictions / omissions found, flagged not silently resolved)

Continuing the numbering from P4 (GAP-31 was the last). New GAPs are P5-era; inherited GAPs are re-listed where P5 re-touches them.

| GAP | Where | The contradiction / omission | Resolution in this spec |
|---|---|---|---|
| **GAP-32** (new) | greylist RFC reality vs P4 sticky routing | Greylisting is keyed on the **sending IP**; it expects the *same IP* to retry. But a retry re-enters routing (§4.4) and could land on a *different* node/IP if the sticky home is unavailable — the provider then sees a new triple and **re-greylists**, so the retry restarts the clock instead of satisfying it. | Sticky-by-domain (P4 §3.2) **already** sends a domain's retries to the same node/IP — an unplanned second payoff of stickiness. Add a **same-node affinity bias** for retries (prefer the last-attempt node when still eligible; least-inflight only if it's gone). `DECISION REQUIRED` OQ-P5-6. Residual re-greylist on an honest fallback settles as `unknown` — acceptable. |
| **GAP-33** (new) | Door 2 (LOG append-only) vs idempotency (exactly-once verdict) | If a crash-reclaimed re-attempt naively appends a verdict, the append-only LOG holds **two** verdicts for one logical check. | Settlement writes the LOG row + stamps `work_items.verification_id` in **one transaction** (§5.3); a re-attempt that finds `verification_id` set is a **no-op** (§5.2). Append-only preserved (never UPDATE a verdict); duplicate-append prevented. Transactional guard, not a schema change. |
| **GAP-37** (new, review PB1 — the sharpest) | naive lease + `SKIP LOCKED` vs a *slow* (not dead) drainer | `SKIP LOCKED` stops two workers grabbing the same *ready* row, but a **TTL reclaim of a merely-slow attempt** runs a **second live attempt concurrently** → two SMTP sessions **and** a last-writer-wins race on the item's next state (a settled verdict resurrected into retry, or a retry dropped). The naive lease is insufficient. | **(1) Lease TTL > the brain's max SMTP client deadline** (P2 §6.5) so a live attempt never loses its lease; a reclaim then means genuinely-dead. **(2) Conditional compare-and-set** on every settle/requeue (`WHERE locked_by=<me> AND status='in_flight'`) — a lost attempt matches zero rows and **discards its result** (§5.2). Exactly one attempt transitions the item. |
| **GAP-38** (new, review PM1) | denormalized `jobs.settled_count` vs job completion | A drifted counter (missed/duplicated increment under the reclaim path) leaves a finished job **forever "running"** if completion trusts `settled_count == total_count`. | Increment `settled_count` **inside** the conditional settlement transaction (moves iff truly settled); judge completion **authoritatively** by `NOT EXISTS(work_items WHERE job_id=? AND status NOT IN (settled,dead))`; use the counter only as cheap progress (§7.3). |
| **GAP-34** (new) | build-plan "async job queue" vs single datastore | The build plan says "queue" without a substrate; a naive external broker (Redis/SQS) becomes a **second source of truth** that can disagree with Supabase, reintroducing the double-run hazard idempotency (§5) exists to kill (broker says "delivered," DB says "not settled," or vice-versa). | **Supabase-backed `work_items` with `FOR UPDATE SKIP LOCKED`** (§3.1, OQ-P5-1) — one datastore, one transaction boundary, so settle+spend+mark-done are atomic. External broker is a **`SEAM →`** scaling path if depth outgrows Postgres — flagged, not built. |
| **GAP-35** (new) | build-plan "greylist retry" vs GAP-12 (mock can't do reject-then-accept) | P2 flagged the `smtp_mock` **cannot** simulate stateful reject-then-accept (GAP-12), so the *retry-then-succeed* round-trip **cannot be end-to-end tested on the mock** — exactly the loop P5 builds. | P5 tests the **loop mechanics** (backoff scheduling, requeue, ceiling→settle, idempotency) against a mock/stub that returns `450` then `250` **by attempt count** (a P5 test harness, not the P2 `smtp_mock`); the true reject-then-accept behaviour is validated against a **real greylisting provider** during rollout. Explicitly not overclaimed — same honesty posture as P2 GAP-12. |
| **GAP-36** (new) | the two-"job" boundary (prompt-flagged) | Two "job" concepts (brain queue vs platform user-job) risk **conflation** — the brain accreting user/credit/CSV concepts, or duplicating the platform's `job_items`. | §7.6 `LOCKED` boundary: brain queue is user/credit/CSV-**ignorant**, exposes counts+verdicts, couples only via opaque `caller_ref`. Appendix C review pass explicitly checks §7 didn't design platform UI. |
| **GAP-32b / plaintext-at-rest** (new) | Door 3 (LOG/cache hold only `email_hash`) vs the queue needing plaintext to run SMTP | A queued check must hold the **plaintext address** at rest (unlike the hash-only LOG) long enough to run — a new PII-at-rest surface Door 3 otherwise avoids. | Hold plaintext in `work_items` **only for the item's life**; **purge on settle** (§3.3), keeping only `email_hash` thereafter. Bounded window (work-not-yet-done), never the permanent LOG. `DECISION REQUIRED` OQ-P5-9. |
| GAP-12 (inherited, P2) | `smtp_mock` has no stateful greylist mode | Re-surfaces as GAP-35 — the retry loop is the thing that can't be faithfully mocked. | See GAP-35. |
| GAP-13 (inherited, P2/P4) | catch-all/session-reuse vs never-fork-gem | Re-surfaces if P5 tries many-RCPT-per-session domain batching (§6.4). | **Not re-solved**: §6.4 recommends **not** building session batching in P5; if pursued, only wrapper-side + after the GAP-13 spike, never a gem patch. Cross-phase dependency. |
| GAP-25 (inherited, P4) | single-brain assumption for pool state | The queue's `SKIP LOCKED` dequeue + in-flight are single-brain-correct; multi-brain drainer coordination is unbuilt. | Single brain assumed (inherited); multi-brain drainer/leader-election is `SEAM →` (GAP-34, §8.4). |

---

## 12. Open questions / decisions required (with recommendations)

Each surfaced with a recommendation; **none silently assumed.**

| # | Decision | Recommendation | § |
|---|---|---|---|
| **OQ-P5-1** | Queue substrate: Postgres/Supabase (`SKIP LOCKED`) vs external broker | **Supabase-backed `work_items`** — one datastore, atomic settle+spend, no second source of truth (GAP-34). Broker is a `SEAM →` scaling path. | §3.1 |
| **OQ-P5-2** | Backoff schedule numbers (base, factor, cap, `max_attempts`, `max_retry_age`) | **Exponential, hint-seeded, jittered** (`LOCKED` shape); start base 5 min, ×2, cap 60 min, `max_attempts` 3–5, `max_retry_age` ~6 h (all `DIAL`, start conservative). | §4.2/§4.5 |
| **OQ-P5-3** | Cross-job fairness | **Domain-spread (job-agnostic) + per-job in-flight ceiling** (`DIAL`); full weighted-fair-queuing/priority is `SEAM →` (ties to platform tiers, §7.6). | §6.5 |
| **OQ-P5-4** | Domain-batched multi-RCPT session reuse in P5? | **No** — group-by-domain gives most of the benefit; multi-RCPT-per-session reopens GAP-13. `SEAM →` optimization contingent on the GAP-13 spike + throughput data. | §6.4 |
| **OQ-P5-5** | Should a *synchronous* greylist/rate_limited auto-spawn a background retry work-item? | **No (default)** — sync degrades as in P2/P4; auto-queuing every sync transient blurs the two-path model. `SEAM →` opt-in "retry-and-notify" for singular checks later. | §3.4/§4.6 |
| **OQ-P5-6** | Retry node affinity (GAP-32) | **Prefer the same node** for a retry when still eligible (satisfies greylist's same-IP expectation); least-inflight fallback only if the sticky home is gone, accepting an honest re-greylist. | §4.4/GAP-32 |
| **OQ-P5-7** | Does a `rate_limited` requeue count toward `max_attempts`? | **No** — it's a capacity wait, not a provider-transient; bound it by `max_retry_age` only, so a busy pool doesn't prematurely burn a greylisted item's retry budget. | §4.6 |
| **OQ-P5-8** | Per-attempt transcript capture (`work_item_attempts`)? | **Yes** — a lightweight append-only attempts table (distinct from the verdict LOG) preserves per-attempt transcripts for debugging/P7 while keeping the LOG at one settled verdict. | §5.3 |
| **OQ-P5-9** | Plaintext address at rest in the queue (GAP-32b) | **APPROVED + strengthened (owner):** hold in `work_items` for the item's life; **purge transactionally as step (4) of settlement** (§5.3), never a later sweep, so a crash can't strand plaintext. The **one deliberate, bounded exception** to hash-everything (Door 3); documented in the data-handling posture (§3.3) + runbook. | §3.3/§5.3 |
| **OQ-P5-10** | Retry-provenance `signals` keys on the settled verdict | **Yes, additive** (`signals.attempt` / `retry_settled`) — free (open jsonb, Door-1-additive), useful to the P7 flywheel ("did retrying change the outcome?"). | §10 |
| **OQ-P5-11** | Result delivery: poll-only vs webhook/stream | **Poll-only in P5** (`GET /v1/jobs/{id}/results`, cursor-paginated) — simplest, statelessly correct. Push delivery is a `SEAM →`. | §7.1/§7.5 |
| **OQ-P5-12** | Which transients are retryable beyond `greylisted` | **`greylisted` primary; `smtp_timeout`/`connection_failed` secondary + separately dialled** (may be network); **`smtp_blocked` NOT retried** (reputation → quarantine, not retry). Mirrors P4 OQ-P4-6's primary/secondary split. | §4.5 |

---

## 13. Phase seams (what Phase 5 deliberately leaves for later)

| Later phase | Seam left in / honored by Phase 5 | Not built now |
|---|---|---|
| **P6** fallback | Retry-exhausted `unknown/greylisted`, `catch_all`, and give-up `rate_limited` residue is **cleanly labeled and left** — the exact residue P6 routes to an external verifier; `reason=external_verified` still reserved (P1). | Fallback API integration; routing the retry-exhausted/catch-all residue to a third party (a P6 policy call). §4.7. |
| **P7** flywheel | Every settled verdict logs once with full provenance; **retry-provenance `signals`** (attempt count, retry-settled — OQ-P5-10) + the `work_item_attempts` transcripts (OQ-P5-8) are the P5-specific join-gold: *"did retrying change the outcome?"* / *"which providers greylist us most?"* | Bounce classification, per-(reason×provider) recalibration, **adaptive backoff/max-attempts tuned from real retry-success rates**, provider auto-promotion. §4.2 backoff stays fixed/dial, not learned. |
| **Multi-brain HA** (cross-phase) | Queue dequeue is `FOR UPDATE SKIP LOCKED` (single-brain-correct, shareable substrate); retry state durable in Supabase (shareable). | Multi-brain drainer coordination, leader election for the drainer loop, external-broker migration if depth outgrows Postgres (GAP-34). §8.4. |
| **Auto-scale the pool** (later P4-flavored) | Queue-depth + rate_limited-retry metrics (§9) are the **direct capacity signal** ("add a node") — measured, alerted. | Auto-provisioning a node from queue depth (P4 §6.3 already fenced this out); the §6 provisioning script stays operator-invoked. |
| **Session batching** (optimization) | Group-by-domain concentrates a domain's ready items on one warm node (P4 §3.2) — *sets up* multi-RCPT batching. | Many-RCPT-per-session batching itself (§6.4, OQ-P5-4) — contingent on the GAP-13 spike; never a gem patch (Door 5). |
| **Platform job UI** (separate project) | Brain exposes `job_id` handle + counts + verdicts through the `/v1/verify` contract; opaque `caller_ref` for correlation. | Users, credits, CSV parsing, live per-row status, resume-after-close, export — **all platform-side** (§7.6, GAP-36). |

---

## 14. Phase 5 done — definition + acceptance criteria

**Phase 5 is done when** (build plan): *greylisted checks are retried on a backoff and settle correctly; a bulk submission returns a job handle and completes asynchronously; work is paced by domain over time; and none of it bypasses a Phase-4 safeguard.*

Acceptance criteria (each maps to a section; each is a provable behaviour):

1. **Async queue behind the router (§3):** a check can be enqueued and later drained through the **unchanged** P4 router; the box cannot distinguish a queued from a synchronous call (§3.2). Queued work is **durable** — a brain restart loses no queued items (§3.3).
2. **Greylist retry works (§4):** a `450` is retried on a **hint-seeded, exponential, jittered** backoff (§4.2) up to `max_attempts`/`max_retry_age`, then settles **`unknown/greylisted`** — never `invalid`, never a fabricated `valid` (§4.5, principle §0.4.2). Retry state is **durable in Supabase** (survives restart without loss or double-run, §4.3 — the P3/P4 durability discipline's third instance).
3. **A retry re-enters the same gates (§4.4/§8):** a due retry makes a **fresh** trip through eligibility → sticky → rate-limiter → warmup; it never resumes around a gate. Same-node affinity satisfies greylist's same-IP expectation (GAP-32).
4. **Idempotency holds — including against a *slow* (not dead) drainer (§5, GAP-37):** settlement is a **conditional compare-and-set on the lease-holder** (`locked_by=<me> AND status='in_flight'`), with lease TTL > the brain's max SMTP deadline; a crash-reclaimed or lost-lease re-attempt **settles once** (one verdict), **logs once** (Door 2 preserved via the zero-row no-op, GAP-33), **spends budget only per real SMTP attempt** (§5.4), and **cannot race the item's state**. Duplicate submissions dedup to one work-item (§5.5). *The compare-and-set, not the lease, is what makes settlement exactly-once.*
5. **Cache/transient discipline consumed (§5.5):** settled verdicts short-circuit SMTP (huge bulk savings); **transient `unknown`s are never cached** (P2 OQ-P2-6), so a greylisted item is never satisfied by a stale non-answer and always actually retries.
6. **Group-by-domain pacing (§6):** bulk work drains **spread across domains** and **spaced within a domain** (not upload order); the queue *feeds* the P4 rate limiter a well-spread stream; when capacity is short, queued items **wait** rather than degrade (§4.6). The three-way interlock (sticky + limiter + queue) holds (§6.3).
7. **Bulk job lifecycle (§7):** `POST /v1/verify/bulk` returns a **`202` + job handle immediately** (no inline verdicts); `GET /v1/jobs/{id}` reports counts; results are **byte-identical `/v1/verify` objects** (Door 1, §7.4); a job **completes even with honest `unknown`s** (§7.3), bounded by `max_retry_age`.
8. **The master invariant holds (§8):** a large bulk job **cannot** buy throughput by bypassing any gate — sync and queued paths differ **only** in the two "wait vs degrade" rows (§8.3); everything else is identical.
9. **The two-job boundary is respected (§7.6, GAP-36):** the brain's queue is user/credit/CSV-**ignorant**, exposes counts+verdicts only, and couples to the platform solely via the opaque `caller_ref`. The review pass (Appendix C) confirms no platform-UI concepts leaked in.
10. **`rate_limited` seam closed (§4.6):** the P4 backpressure a **queued** item hits becomes a short wait-and-retry (queue absorbs it); a **synchronous** check still degrades (P4 unchanged) — the cleanest seam-closure in the phase.
11. **Additivity (§10):** the entire surface is additive — 2–3 new operational tables, additive `signals` keys, one new bulk endpoint returning the same verdict shape — and **zero** new `status`/`reason`/`recommendation`/`flags`/`verifications`-column, **zero** change to the singular endpoint/cache/hashing/router/limiter/box.
12. **Door 5 intact:** no box-side change; the gem stays pinned + CI-asserted-unmodified; the retry-then-accept round-trip is honestly acknowledged as un-mockable (GAP-35) and validated against a real greylisting provider at rollout.
13. **Every OQ-P5 (§12) is signed off or explicitly deferred with an owner** — none silently assumed. GAP-32/33/34/35/36/37/38 carry explicit owner-visible stances (Appendix D).

---

*Phase 5 spec — v1.0, APPROVED. The load-bearing carry-forward beyond this phase: the **GAP-37 lease-and-settle invariant** (lease TTL > max operation deadline; settle only via a compare-and-set on the lease-holder; at-least-once attempts, exactly-once settlement) is a **system-wide** correctness principle, not a Phase-5 detail — it governs the Phase-7 calibration writes, the platform's job runner, and any future concurrent lease-and-settle path. The queue is a scheduler in front of the P4 router (changes *when*, never *whether*), and it holds plaintext for work-not-yet-done only, purged transactionally on settle — the one deliberate, bounded exception to hash-everything.*

## Appendix A — Upstream ground truth Phase 5 depends on

### A.1 truemail / truemail-rack (inherited, unchanged)
- **`Truemail.validate` verifies one address per call** (P1 §5.5, P2 A.1) — the queue calls it once per work-item attempt; no batch API in the gem. The "many RCPT per session" tension (§6.4 / GAP-13) stems from this.
- **`smtp_mock` has no stateful greylist mode** (P2 A.3 / GAP-12) — reject-then-accept can't be faithfully simulated; hence GAP-35's attempt-count test harness + real-provider validation.
- **The box is a pure function** `email → raw engine JSON` (P1 §5.5) — it holds no queue/retry/job state; all such state is brain-side (Door 5, §3.2).
- **Cache key = `(email_hash, config_fingerprint)`** (P1 §4.2); **only settled verdicts are cached** (P2 OQ-P2-6, P4 §3.6) — the idempotency key (§5.1) and the retry-not-short-circuited property (§5.5) both build on this.

### A.2 Greylisting (industry ground truth, not truemail-specific)
- **Greylisting returns a transient `4xx` (usually `450`) on first contact from an unknown (IP, sender, rcpt) triple, expecting a delayed retry from the same IP** — the basis for §4.1's "retry needs a queue" and GAP-32's same-IP expectation.
- **Retry timing matters:** retrying before the greylist window elapses earns another `450`; a legitimate MTA waits minutes. Basis for the hint-seeded backoff (§4.2).
- **A greylist is evidence about the *sender's trust*, not the *recipient's existence*** — basis for `greylisted → unknown, never invalid` (§0.4.2).

### A.3 Distributed-systems ground truth Phase 5 depends on
- **Exactly-once *delivery* to a third party is impossible; exactly-once *effect* is achieved via at-least-once + idempotency** — the basis for §5.2's "at-least-once attempts, exactly-once settlement" posture.
- **A queue with a durable lease + TTL reclaim (`FOR UPDATE SKIP LOCKED`) gives single-consumer-per-item without a broker** — the basis for §3.1 / §5.2.
- **RCPT probing is read-only at the provider (no DATA sent, no mail delivered)** (P2 §5.7) — the basis for §5.2's "re-attempt is safe": a duplicate attempt costs one extra connection (budgeted, §5.4), never a duplicate email.

---

## Appendix B — Behaviour proof matrix

Each row is a scenario the Phase-5 design must handle correctly; the acceptance tests (§14) exercise these.

| # | Scenario | Expected behaviour | Guards |
|---|---|---|---|
| 1 | Bulk submit of 100k addresses | `202` + job handle immediately; M work-items enqueued (post dedup/cache); no inline verdicts | §7.1 |
| 2 | Address already in cache (settled) | satisfied without an SMTP attempt at enqueue/dequeue | §5.5 |
| 3 | Same address twice in one upload | dedup to one work-item / one SMTP attempt; both input positions get the verdict | §5.5 |
| 4 | Greylist `450` with `retry_after_hint_s:300` | `waiting_retry`, `not_before=now+~300s±jitter`, `attempt++`; no LOG row yet | §4.2/§5.3 |
| 5 | Greylist clears on retry 2 (`250`) | settles `valid/accepted`, logged once, `verification_id` set | §4/§5.3 |
| 6 | Greylist never clears, hits `max_attempts` | settles `unknown/greylisted` (terminal, Door 4), logged once | §4.5 |
| 7 | Brain crashes after SMTP `250`, before verdict write | item reclaimed via lease TTL; re-attempt; settles once (LOG has one row); budget spent twice (two real connections — accepted) | §5.2/§5.4/GAP-33 |
| 8 | Brain restarts with pending retries | retries resume from durable `work_items` (future `not_before`); none lost, none double-run | §4.3 |
| 9 | Queued item hits `rate_limited` (pool capped) | `waiting_retry` with short backoff (waits for capacity); does NOT degrade cheap; does NOT count toward `max_attempts` | §4.6 |
| 10 | Synchronous check hits `rate_limited` | degrades cheap with `smtp_skipped_reason:"rate_limited"` (P4 unchanged) — NOT queued | §4.6/§8.3 |
| 11 | Bulk job spans 5k domains | drains spread across domains, spaced within each; no domain bursts its provider | §6.2 |
| 12 | Retry's sticky home is quarantined at retry time | prefers last-attempt node if eligible (GAP-32); else least-inflight, accepting an honest re-greylist | §4.4/GAP-32 |
| 13 | Two jobs submitted close together | domain-spread interleaves them; per-job in-flight ceiling stops the big one starving the small | §6.5 |
| 14 | `smtp_blocked` (your-IP-blocked) result | NOT retried by the greylist loop (reputation → health-watcher quarantine, P4 §5); settles `unknown/smtp_blocked` | §4.5 |
| 15 | Job with some retry-exhausted items | completes (`complete_with_unknowns`); results include honest `unknown/greylisted` verdicts | §7.3 |
| 16 | Queue depth grows monotonically | backlog alert fires → "add a node" capacity signal (queue absorbs bursts, not permanent under-capacity) | §9 |
| 17 | Attempt to route a queued check around a gate | impossible by construction — drainer's only path to SMTP is the P4 router+gates | §8 |
| 18 | **Slow (not dead) drainer** — attempt A stalls in a long SMTP call, TTL reclaims to attempt B | lease TTL > max SMTP deadline makes this rare; if it happens, A's settle/requeue matches zero rows (`locked_by` lost) and is discarded — only B transitions the item; no double-settle, no state race | §5.2/GAP-37 |
| 19 | Attempt B settles while attempt A also finishes | exactly one (the current lease-holder) commits; the other's conditional update is a no-op; LOG has one row; `settled_count` moves once | §5.2/§5.3/GAP-37/38 |
| 20 | All items settled but `settled_count` drifted low | job still reports `complete` via the authoritative `NOT EXISTS(... NOT IN (settled,dead))` query; counter reconciled lazily | §7.3/GAP-38 |

---

## Appendix C — Review log (adversarial architecture + backend-architect + architect-review pass)

This spec was drafted, then critiqued by an adversarial **architecture + backend-architect + architect-review** pass against the frozen P1–P4 contracts, distributed-systems reality, and the prompt's explicit boundary checks, before finalizing (mirroring P1 Appendix B, P2/P3/P4 Appendix C). The pass was run **inline** (consistent with the owner's "don't run long sessions, split the work" guidance and the P4 process note — inline was faster and effective). Findings folded in:

**Blocker (resolved):**
- **PB1 — the lease was not enough; a *slow* (not dead) drainer double-runs and races on state.** The draft's idempotency rested on `FOR UPDATE SKIP LOCKED` + a TTL reclaim. But `SKIP LOCKED` only prevents two workers grabbing the same *ready* row — it does nothing when a TTL reclaim re-readies an item whose original holder is merely slow (long SMTP timeout, GC pause). Two live attempts then run concurrently, and — the real damage — they **race on the work-item's next state** (a `250` settle vs a `450` requeue, last-writer-wins), which can resurrect a settled verdict into retry or silently drop a retry. This is the sharpest correctness hazard in the phase; the naive lease hid it. **Resolved:** §5.2 GAP-37 — (1) lease TTL must exceed the brain's own max SMTP client deadline (so a *live* attempt never loses its lease and a reclaim genuinely means dead), and (2) every settle/requeue is a **conditional compare-and-set** on `locked_by=<me> AND status='in_flight'`; a lost attempt matches zero rows and discards its result. Exactly one attempt can transition the item. §5.3 and the proof matrix (rows 18–19) updated to match.

**Majors (all resolved):**
- **PM1 — job completion trusted a denormalized counter.** §7.3 judged a job `complete` by `settled_count == total_count`, but `settled_count` is a denormalized cache that drifts exactly under the new reclaim/no-op path — one missed increment leaves a finished job "running" forever. **Resolved:** §7.3 GAP-38 — increment inside the conditional settlement transaction (moves iff truly settled) and judge completion **authoritatively** by `NOT EXISTS(work_items WHERE job_id=? AND status NOT IN (settled,dead))`; the counter is cheap progress only.
- **PM2 — the retry-then-accept loop is not end-to-end testable on the P2 mock.** The draft built the greylist loop but the P2 `smtp_mock` cannot do stateful reject-then-accept (GAP-12) — the retry-then-succeed round-trip, the loop's whole point, can't be mocked faithfully. Left implicit risks overclaiming test coverage. **Resolved:** promoted to GAP-35 — loop *mechanics* tested against a by-attempt-count stub (P5 harness), true reject-then-accept validated against a real greylisting provider at rollout; explicitly not overclaimed (same honesty posture as P2 GAP-12).

**Boundary check (the prompt's explicit instruction) — PASS:** §7 was audited for platform-UI leakage. The brain's `jobs` table holds **only** verification-work state (counts, status, policy); it carries **no** user id, credit balance, CSV, filename, or per-row UI state; the sole platform coupling is the **opaque `caller_ref`** it never interprets. Progress is **counts, not a live per-row feed**. Credits/quotas/resume/CSV are named as platform-side and fenced out (§7.6, GAP-36). The spec did **not** start designing the platform's job UI. Confirmed clean.

**Minors folded in:** the plaintext-at-rest window (queue must hold the raw address to run SMTP, unlike the hash-only LOG) given its own gap-flag (GAP-32b) with a purge-on-settle resolution (§3.3, OQ-P5-9); the greylist same-IP-vs-sticky-retry tension flagged as GAP-32 with a same-node-affinity recommendation (§4.4, OQ-P5-6); `rate_limited`-retry explicitly excluded from the `max_attempts` count so a busy pool can't burn a greylisted item's retry budget (§4.6, OQ-P5-7); the double-spend-by-one after a crash confirmed as an accepted conservative cost, not a bug (§5.4).

**Net:** the phase's two highest-rigor items (retry loop §4, idempotency §5) survived the pass with one blocker (PB1/GAP-37) that materially changed the idempotency design — the compare-and-set is now the load-bearing correctness mechanism, not the lease. The two-job boundary held.

---

## Appendix D — Owner decision log

**Verdict: APPROVED (v1.0).** The owner reviewed the correctness heart (§4–§5) and signed off. *"GAP-37 alone justifies the entire planning exercise — the sharpest single catch in all five specs."*

### D.1 The catch of the whole series — GAP-37 (owner's named strongest point)
The owner affirmed GAP-37 as **the catch of the whole five-spec series** and confirmed the reasoning is correct, not hand-waved:
- The naive design (`FOR UPDATE SKIP LOCKED` + TTL lease) is *"what 90% of engineers would ship, and it's subtly wrong in a way that only bites under load."* `SKIP LOCKED` stops two workers grabbing the same *ready* row — but does **nothing** when a drainer is **slow, not dead** (long SMTP timeout, GC pause): the reclaimer re-readies the row, a second attempt runs while the first is still open, and a **last-writer-wins race on the item's next state** either resurrects a settled `250` into retry or silently drops a retry.
- Why this is *the* catch: it is a verdict-corruption bug that **(a)** only appears under concurrency, **(b)** is nearly impossible to reproduce in testing, and **(c)** *"poisons the exact data your Phase 7 flywheel will train on."* The owner named the Phase-7 training-data contamination explicitly as what makes it series-defining.
- The fix is affirmed correct: **(1)** lease TTL must exceed the brain's own max SMTP deadline (so a reclaim *provably* means dead, not slow); **(2)** every settlement is a **conditional compare-and-set on `locked_by=<me>`** — a lost attempt matches zero rows and discards its result. *"The compare-and-set, not the lease, is what makes settlement exactly-once."* The posture — **at-least-once attempts, exactly-once settlement** — is the right one, and the justification for tolerating duplicate attempts (an SMTP RCPT probe is read-only at the provider — no DATA, no mail sent, so a duplicate costs one connection, not a delivered email) is *"the right reason, not a hand-wave."*
- **GAP-38 affirmed as the correct downstream catch:** job completion must be judged by the authoritative `NOT EXISTS(... NOT IN (settled,dead))` query, never the denormalized `settled_count` (which drifts under the same reclaim path — a trusted counter would make a job report "done" while work remained).

### D.2 The three flagged OQs — owner's calls
- **OQ-P5-1 (Postgres `SKIP LOCKED` vs external broker) → APPROVED: Postgres.** The reason is the load-bearing part: *"one source of truth for idempotency."* An external broker (Redis/SQS) would split settlement state across two systems, and **the GAP-37 compare-and-set only works if the lease and the verdict live in the same transactional store** — a broker would reintroduce the exact race just closed. Standing directive: **"don't let anyone 'scale it up' to a broker without re-solving idempotency."** For this scale (single brain, thousands of domains, not millions of msgs/sec) Postgres is more than enough and keeps correctness in one place.
- **OQ-P5-4 (multi-RCPT session batching) → APPROVED: defer.** Group-by-domain delivers most of the reputation benefit (spacing + concentration) without long multi-RCPT sessions; batching reopens the GAP-13 never-fork tension at pool scale. Ship the ordering now; make session batching a `SEAM →` contingent on the GAP-13 spike + real throughput data. *"Don't build the optimization before the data says you need it."*
- **OQ-P5-9 (plaintext-at-rest) → APPROVED with an active strengthening (not a rubber stamp).** The owner flagged this as *"your one plaintext exception"* and required: **the purge must be part of the settlement transaction, not a later sweep** — so a crash can't leave plaintext lingering. Folded in as §5.3 step (4) + the §3.3 data-handling posture line: *"the queue holds plaintext for work-not-yet-done only, purged on settle."* This is the single, deliberate, bounded exception to the hash-everything discipline (Door 3), now called out in the data-handling runbook alongside the P2 §6.4 verifier-identity note.

### D.3 The boundary check — owner-confirmed clean
The owner affirmed the §7.6/GAP-36 audit: the brain's `jobs` table holds **only** verification-work state (counts, status, policy) — no user id, credits, CSV, filename, or per-row UI — with the sole coupling an opaque `caller_ref` it never interprets; progress is counts, not a live feed. *"The separation you insisted on held."*

### D.4 Seam-closures the owner called out as noteworthy
- **`rate_limited` resolves cleanly with no new machinery:** sync → degrade cheap (P4 unchanged); queued → wait-and-retry (P5). *"The limiter creates backpressure, the queue absorbs it — the cleanest seam in the phase."*
- **GAP-32 — an unplanned payoff:** greylisting is keyed on the sending IP, and P4 sticky routing already sends a domain's retries to the same node/IP — so the retry lands on the same IP that got greylisted, which is exactly what a greylist wants. *"Sticky routing paid off twice."*
- **GAP-35 — honest about the untestable part:** the retry-then-accept round-trip can't be faithfully mocked (P2 GAP-12); loop mechanics tested on a by-attempt stub, true validation deferred to a real greylisting provider at rollout. *"It didn't overclaim."*

### D.5 Batch-accepted (no separate discussion, all recommendations sound)
OQ-P5-2 (backoff numbers), -3 (cross-job fairness: domain-spread + per-job ceiling, WFQ deferred), -5 (no auto-queue of sync transients), -6 (retry node affinity, GAP-32), -7 (`rate_limited` retry excluded from `max_attempts`), -8 (`work_item_attempts` transcript table), -10 (retry-provenance `signals` keys), -11 (poll-only results), -12 (retryable-transient set) — all accepted as recommended.

### D.6 Carry-forward directive (owner, load-bearing beyond Phase 5)
**The GAP-37 principle — *lease TTL > max operation deadline, and settle only via a compare-and-set on the lease-holder* — is a system-wide architectural invariant, not a Phase-5 detail.** It governs **any** future lease-and-settle or concurrent-write path in the system: the **Phase-7 calibration writes**, the **platform's job runner**, and anything else that leases work and commits a result. Recorded as a memory ([[truemail-lease-cas-exactly-once-settlement]]) so it survives into those phases. Related standing directives: [[truemail-never-fork-engine-door5]] (the queue adds no box-side behavior), [[truemail-fleet-brake-no-cascade]] (the queue never bypasses the P4 gates the brake protects), [[truemail-audit-green-not-ready]] (per-node `verifier_ip` isolation still governs every queued check).

**Net:** approve Phase 5. It caught the trap that would have quietly corrupted the training data, kept the platform boundary clean, and resolved every deferred `SEAM → P5` from four prior specs.

---

