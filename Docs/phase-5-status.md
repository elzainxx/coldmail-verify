# Phase 5 — status: the queue (local build scorecard)

**Date:** 2026-07-18 · **Phase:** 5 (the durable async queue) · **Spec:** [`phase-5-queue-spec.md`](phase-5-queue-spec.md) (the authority)
**Status: ✅ COMPLETE (local build)** — the durable async queue, greylist retry/backoff, idempotency
(the GAP-37 CAS), group-by-domain pacing, and the bulk job lifecycle are built and test-proven
against **real Postgres 16** + the P4 sim-node surface, exactly as Phases 1–4 built locally. The one
genuinely un-mockable behavior (the greylist reject-then-accept round-trip, GAP-35) is honestly
deferred to a real-provider rollout — the loop mechanics are proven on a by-attempt stub, never faked.

**What Phase 5 is:** a **scheduler in front of the UNCHANGED Phase-4 router** (§8 master invariant —
the queue changes WHEN a check runs, never WHETHER the safeguards apply). New brain-side components:
`internal/queue` (drainer loop + backoff + enqueuer + the DR-4 cipher), three operational tables, the
GAP-37 CAS settlement in the store, and the `/v1/verify/bulk` + `/v1/jobs/{id}` endpoints. **No box
change** (Door 5), **zero new verdict vocab** (Door 4), **no gem patch**.

**Prerequisite gate (checked before starting, per the kickoff's stop-if-unmet):** ✅ Phase 4 closed
green (`525d1c0`); `00f2139` provenance resolved; fleet brake re-verified on all four properties;
**DR-4 decided** (separate-key encryption — the gate that introduces plaintext-at-rest); **PN-13
addressed** before bulk. Both gates were 🔴 at the readiness pass and are now built + fail-verified
(§3.7/§3.8 ticked). Host procurement is **NOT** a prerequisite — P5 is Postgres + Go, no VPS.

---

## The gates-first sequence (owner-ruled 2026-07-18)

Both P5-blocking gates were built and **fail-verified BEFORE** the queue, per the owner's sequencing:

| Gate | What shipped | Fail-verified |
|---|---|---|
| **DR-4** (backup-plaintext, ADR-0002 → ADR-0011) | `internal/queue/addrcipher.go` — AES-256-GCM, key SEPARATE from the pepper (`TM_QUEUE_ADDR_KEY`), AAD-bound to the row id, two-key rotation overlap, fail-closed key length | neuter `Seal` → `TestAddrCipher_CiphertextContainsNoPlaintext` fails; the PG e2e dumps the row and asserts ciphertext-only, then NULL after settle |
| **PN-13/F-20** (per-caller front-door auth) | migration 0010 `api_callers` (hashed `key_sha256`, validity window for rotation), `store.LookupCaller`, `api.authenticate` (sha256 + `crypto/subtle`), bulk REQUIRES a per-caller key | neuter the validity window → rotated-out key authenticates (fails); neuter the bulk gate → static token accepted on bulk (fails); both restored green |

---

## Build increments (all local, all test-proven)

| # | Increment | Status | Evidence |
|---|---|---|---|
| 1 | **DR-4 cipher** — separate-key AES-256-GCM, AAD-bound, rotation, fail-closed | ✅ | `internal/queue/addrcipher.go` + 7 tests; neuter→fail→restore proven |
| 2 | **Secrets** — `TM_QUEUE_ADDR_KEY` (+`_PREV`) loaded distinctly from the pepper, redacted | ✅ | `internal/secrets/secrets.go` + `secrets_test.go`; example env documented |
| 3 | **PN-13 auth** — `api_callers` (0010) + `LookupCaller` + `authenticate` upgrade | ✅ | migration 0010 (grants proven live); `auth_test.go`; PG-gated `TestPGLookupCaller_WindowEnforced` |
| 4 | **Migration 0009** — `work_items` + `jobs` + `work_item_attempts` (+ grants) | ✅ | applied to real PG 16; cp_runtime INSERT/UPDATE proven, DELETE denied |
| 5 | **Store layer** — Enqueue/Dequeue/Settle(CAS)/Requeue/Reclaim/JobCompletion/ListResults + Mem parity | ✅ | `store/{store,postgres,mem}.go`; GAP-37/33/38 tests on Mem + real PG |
| 6 | **compute/settle split** — `Verify` = `compute()`+`settleSync()`; `ComputeForQueue` (log-free) | ✅ | `orchestrator.go`; sync path byte-identical (orchestrator/api tests unchanged) |
| 7 | **Drainer + backoff** (§3/§4, highest rigor) — loop, retry scheduler, GAP-37 CAS transitions | ✅ | `queue/{drainer,backoff}.go` + tests; greylist→unknown, rate_limited waits, lease-TTL guard |
| 8 | **Bulk endpoint** (§7) — `/v1/verify/bulk` + `/v1/jobs/{id}(/results)`, enqueuer | ✅ | `queue/enqueue.go`, `api` handlers + `bulk_test.go`; PN-13-gated, results = Door-1 objects |
| 9 | **Behavioral pass + PG e2e** — GAP-32 affinity, master-invariant e2e on real PG | ✅ | `queue/{affinity,pg_e2e}_test.go`; **caught a real ListJobResults cursor bug** (Mem hid it) |
| 10 | **Docs + ADR-0011** — this scorecard, the additive-surface/DR-4/PN-13/GAP-37 ADR, checklist ticks | ✅ | ADR-0011, this doc, migrations/README, config/secrets.example.env, pre-takeoff §3.7/§3.8 |

---

## The correctness heart — GAP-37, fail-verified on Mem AND real Postgres

> **RULE (LOCKED):** `FOR UPDATE SKIP LOCKED` alone is insufficient. It stops two workers grabbing the
> same *ready* row, but does NOTHING when a slow-not-dead drainer is TTL-reclaimed — two live attempts
> then race the item's next state (a settled 250 resurrected into retry, or a retry dropped —
> poisoning the P7 training data).

**Two-part fix, both proven:** (a) the lease TTL must EXCEED the brain's max SMTP deadline so a live
attempt never loses its lease (a reclaim provably means dead — `NewDrainer` fails fast otherwise); and
(b) every settle/requeue is a **conditional compare-and-set** on `(id, locked_by=<me>,
status='in_flight')` — a lost attempt matches zero rows, rolls back (no LOG row — GAP-33), and discards
its result (`ErrLeaseLost`). **The CAS, not the lease, makes settlement exactly-once.**

- `TestSettle_ExactlyOnceUnderSlowDrainerReclaim` (Mem): two live attempts (A reclaimed-stale, B
  current) → exactly one settles, the LOG holds **exactly one** row. **Neuter the CAS guard → both
  settle → two LOG rows → the test fails.** Restored green.
- `TestPGWorkItem_CASSettlementExactlyOnce` (real PG): the SQL CAS returns zero rows for the stale
  lease; the lost attempt's verdict is **never** in the LOG. Mem and PG agree.
- `TestPGE2E_SubmitDrainSettleResults_MasterInvariant` (real PG): the full submit→drain→settle→results
  lifecycle; the plaintext is ciphertext at rest and NULL after settle; completion is authoritative.

---

## Additivity (§10) — proven additive against all five frozen doors

- **Door 1** — a NEW sibling endpoint (`/v1/verify/bulk`) returning the **byte-identical** `/v1/verify`
  verdict objects; retry provenance is additive `signals` keys. The singular endpoint is untouched.
- **Door 2** — three new operational tables (`work_items`/`jobs`/`work_item_attempts`); the settled
  verdict is INSERTed into the append-only LOG **exactly once** (GAP-33). No frozen-schema change.
- **Door 3** — the ONE bounded plaintext exception (`work_items.addr_ciphertext`) is **encrypted at
  rest** (DR-4, key ≠ pepper) AND **purged in the settlement tx** (never a sweep); the idempotency key
  reuses the existing `email_hash` (no new hashing, no new pepper exposure). Group-by-domain routes on
  `domain` (in clear). Bulk results carry `email=""` (purged — can't echo the raw address).
- **Door 4** — **zero** new `status`/`reason`/`recommendation`; retry-exhaustion settles the reserved
  `unknown/greylisted`. `vocab.go` byte-unchanged.
- **Door 5** — verifier-node fork + gem **byte-identical to P2–P4**; the queue adds NO box-side
  behavior; the retry path routes through the UNCHANGED P4 `Strategy` interface (a compile guard
  enforces it).

---

## PENDING-REAL-BOX / rollout (never faked green — the honest ledger)

| Item | What's built now | Deferred proof |
|---|---|---|
| **Greylist reject-then-accept round-trip** (GAP-35) | the retry loop mechanics (requeue, ceiling→settle, hint-seeded backoff, idempotency) on a by-attempt-count stub | a REAL greylisting provider at rollout (the P2 `smtp_mock` can't do stateful 450-then-250) — gates the acceptance demo, not the build |
| **Real per-provider pacing under load** | the group-by-domain ordering + P4 rate limiter, deterministically tested | rollout observation on a real IP |
| **DR-4 backup restore drill** | the at-rest ciphertext + purge, proven locally (dump-the-row) | a real backup/PITR restore-into-isolation drill at go-live (ADR-0002 compliance test) |

---

## Dials (§4.2/§4.5) — wired in `main.go` buildQueue (conservative defaults)

Backoff base 5m ×2 cap 60m ±15% jitter; rate-wait 30s; `max_attempts` 4; `max_retry_age` 6h;
lease TTL = `SMTPClientDeadline + 2m` (the GAP-37 margin); batch 10; idle-poll 2s. These are code
defaults for the local build; promoting them to config dials is a follow-up (noted, not blocking —
the queue subsystem only activates when `TM_QUEUE_ADDR_KEY` is set, so no production surface changes
silently).

---

## What Phase 5 does NOT build (correct scope — SEAM → later)

No third-party fallback (P6 — a retry-exhausted item settles `unknown/greylisted` and stops; it does
not call an external verifier). No bounce calibration / adaptive backoff-from-outcomes (P7). No
multi-brain queue coordination (single brain, `SKIP LOCKED`, no broker — GAP-34/OQ-P5-1). No
many-RCPT-per-session batching (OQ-P5-4 — reopens the GAP-13 never-fork tension; group-by-domain sets
it up, session batching stays a SEAM contingent on the GAP-13 spike). No platform job UI (§7.6/GAP-36
— the brain exposes counts + verdicts + an opaque caller_ref). The gem is never forked.

---

## Review gate

Every commit ran the code-reviewer gate (PROJECT_RULES §7). The gates commit (`40ac247`) was reviewed
APPROVE-WITH-SUGGESTIONS (0 critical/high/medium, 1 low — a `.gitignore` defense-in-depth hardening,
applied in `576c56e`). The queue substrate + drainer + bulk commits carry the built-in behavioral +
PG-gated proofs above; a consolidated adversarial review of the idempotency surface is the P5→P6
readiness pass's job (mirroring the P4→P5 pattern).
