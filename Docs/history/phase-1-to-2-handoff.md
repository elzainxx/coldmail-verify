# Phase 1 → Phase 2 — handoff

**Date:** 2026-07-17 · **From:** Phase 1 (Foundations), confirmed complete · **To:** Phase 2 (SMTP against mocks)
**Purpose:** one page an engineer reads before starting Phase 2 — what Phase 1 **locked**, what Phase 2
**inherits**, the exact **`SEAM → P2`** items Phase 1 deferred, and the one **open owner item** (LICENSE).
**Authorities:** [`phase-1-foundations-spec.md`](phase-1-foundations-spec.md) (frozen), [`phase-2-smtp-mocks-spec.md`](phase-2-smtp-mocks-spec.md) (the build), [`phase-1-status.md`](phase-1-status.md) (what shipped), [`truemail-ground-truth.md`](truemail-ground-truth.md) (real engine behavior), [`DECISIONS.md`](../open-decisions.md) + [`adr/`](../adr).

---

## 1. Phase 1 is genuinely closed (verified, not just claimed)

Re-run in this readiness pass — **every gate green**:

| Gate | Result |
|---|---|
| `go build` / `go vet` / `go test` (control-plane) | ✅ all pass |
| Door-5 gem-unmodified assertion | ✅ truemail pinned **3.3.1**, no vendored engine source |
| **Door-2 append-only on real Postgres 16** | ✅ **exercised THIS pass** against a throwaway PG 16 container (migrations 0001–0005 applied): as `cp_runtime`, INSERT ok, **UPDATE/DELETE → permission denied**; the gated `TestPG*` store tests ran with `TM_TEST_DATABASE_URL` set and passed. (A plain `go test` with no DSN *skips* these — this row means they were genuinely run, not skipped-and-assumed.) |
| config-reference freshness | ✅ fresh (regenerated from manifest) |
| docs-updated staleness | ✅ (this pass keeps code+docs in step) |
| The 4 demonstrated verdicts reproduce | ✅ gmail→`risky/provider_no_smtp`, mailinator→`risky/disposable+skip` (list-seeded), garbage→`invalid/invalid_syntax` (NULL hash), NXDOMAIN→`invalid/no_mx` — all logged |
| The 4 caught bugs each locked by a regression test | ✅ NFC split (`TestNonASCIILocalPartNFCEqual`+golden vector), UUID (`TestVerifyIDsAreValidUUIDs`), null-MX ordering (`TestInvalidNeverSaysSend/null_MX`), cache aliasing (`TestMemCacheMapsAreCopiedNotAliased`, **fail-verified**) |

**One honest caveat surfaced:** the `disposable` verdict is **data-dependent** — with an empty `disposable_domains`
table a disposable address returns `risky/plausible` (still honest, just less specific). The list is a DIAL;
seed it to get `risky/disposable`. Not a regression.

---

## 2. What Phase 1 LOCKED (Phase 2 may only add to these)

- **Door 1** — `POST /v1/verify` response shape; `invalid ⇒ recommendation=skip` (invalid+send is structurally
  impossible via `repairComposability`); `confidence` int 0–100; consumers tolerate unknown keys. `signals` is
  an **open bag** → P2 adds keys freely.
- **Door 2** — `verifications` is append-only, **DB-grant-enforced** (proven live). The raw engine blob rides
  the partitioned `verification_raw` sidecar (retention = `DROP PARTITION`). Every result is logged before
  responding, including cache hits (`from_cache=true`) and `invalid_syntax` rejects (NULL hash).
- **Door 3** — `norm/v1 + hmac-sha256/v1`, `bytea` key, global pepper. **Fully wired + golden-vector pinned**
  (`8eef019d…`); NFC+IDNA locked; case-fold edge documented ([ADR-0006](../adr/ADR-0006-door3-nonascii-localpart-refusal.md),
  status ACCEPTED · RESOLVED at P1). **The store is empty now — this was the last free moment for a hashing
  change, and it is closed.** Phase 2 changes nothing here.
- **Door 4** — `status`/`reason`/`recommendation` enums in one place (`internal/vocab`); storage CHECK
  constraints mirror them **exactly (26/26 reasons)**. **All 10 SMTP reasons P2 emits are already reserved**
  (`accepted, mailbox_not_found, mailbox_full, catch_all, accept_all, greylisted, smtp_blocked, smtp_timeout,
  smtp_error, connection_failed`) → **P2 adds zero new enum values.**
- **Door 5** — the truemail **gem is never forked** (only `truemail-rack`); CI-asserted. Brain talks to a node
  **list** via the `Strategy` interface.

**Open-question sweep (explicit):** **no frozen door has an open question Phase 2 is tempted to touch.** The
only genuine P2 pre-build unknown is **GAP-13 / OQ-P2-11** — can the fork *wrapper* drive a same-session 2nd
RCPT (catch-all probe) without patching the gem — and that is a Door-5-**protecting** spike (the spec already
forbids the gem patch; fall back to two-session Option B if not), not a door with an open question.

---

## 3. What Phase 2 INHERITS (built and ready to build on)

- **The control-plane skeleton** — front door, orchestrator lifecycle (cache → policy → layers → score →
  assemble → LOG → cache → respond), the single verdict **assembler** (`internal/verdict/assemble.go`) that
  builds the response and the LOG row from one source so they can't drift.
- **The node seam, dormant** — `internal/node` (pure-function `email → raw engine JSON`, returns
  `ErrNotImplemented` today), `internal/router` (node-list + `Strategy`). P2 lights these up against mocks.
- **The data model** — all §4 tables exist; `catchall_domains` is created **empty** and P2 **writes** it
  (brain-side only — boxes never touch the DB). `nodes.audit_status`/`last_audit_at`/`port25_open` reserved.
- **The policy engine** — table-driven, open-object (OQ-25); P2 adds an `smtp` entry to `order`+`gates` **as
  data**, using the existing `status:reason:stop|continue` grammar. No grammar/schema change.
- **Real engine ground truth** — [`truemail-ground-truth.md`](truemail-ground-truth.md), now including the
  **live smtp_mock capture** (see §5 below). Use it over any spec assumption where they differ.

---

## 4. `SEAM → P2` items Phase 1 deferred (Phase 2's explicit inbox)

| Seam | Where P1 left it | What P2 does with it |
|---|---|---|
| **Reserved SMTP `reason` values** | Door 4 reserves all 10 now (emitted by no P1 path) | P2 populates them via SMTP scoring (§7.2). Zero new enum values. |
| **Reserved `signals` keys** | `smtp_code`, `greylisted` reserved in P1 §3.1.3 | P2 populates them + adds `smtp_enhanced_code, retry_after_hint_s, retry_eligible, smtp_safe_pass, smtp_debug_ref, dns_divergence, brain_mx_provider, engine_mx_hosts, catchall_conclusion` (all additive, P2 §8 ledger). |
| **Wrapper endpoints (NOT built in P1)** | node client targets stock `GET /?email=` | P2 forks the rack wrapper: `/v1/verify, /v1/audit, /v1/catchall/:domain, /v1/stats, /v1/config/reload, /v1/config` (P2 §4) — in the wrapper, never the gem. |
| **config_fingerprint engine coverage (GAP-3/OQ-11)** | P1 fingerprint marks engine inputs `unconfigured` | P2 `GET /v1/config` exposes full effective config; brain fingerprints it (P2 §4.6). Retires the P1 `unconfigured` placeholder. |
| **smtp_code parsing (GAP-4)** | P1 reserved `signals.smtp_code`, no parser | P2 parses the numeric code from the raw `smtp_debug` strings (P2 §6.2). |
| **DNS reconciliation (GAP-10/OQ-24)** | P1 flagged brain-DNS vs engine-DNS divergence | P2 rule: engine-MX authoritative for the SMTP verdict, brain-MX advisory for routing, divergence logged (P2 §6.6). |
| **Catch-all probe** | `flags.is_catchall` reads the table; P1 never probes | P2 builds the same-session 2nd-RCPT probe; writes `catchall_domains` tri-state (P2 §5). **Gated by the GAP-13 spike.** |
| **Cache "settled-only" predicate** | P1 caches cheap verdicts | P2 must NOT cache transient/your-fault `unknown`s (greylist/timeout/blocked) — caching a transient suppresses the retry that resolves it (P2 §6.5, OQ-P2-6). |
| **`verification_raw` sensitivity** | P1 runbook governs the pepper + raw blob | P2 transcript adds verifier identity + probe addresses → one-line update to the P1 access-control runbook (P2 §6.4, OQ-P2-5). |
| **Store test-double JSON fidelity** | `MemStore` preserves Go types; PG round-trips `signals` through JSON | P2 adds **numeric** signals (`retry_after_hint_s` int, §8). PG returns numbers as `float64` and nil maps as `{}`; the mem-fake doesn't. P2 must round-trip the fake through JSON, or not assert on `signals` value *types* in mem-fake tests (note in `postgres.go`). |

**Also deferred past P2 (do not build in P2):** the queue/greylist-retry loop (P5), real IP/PTR/warmup + the
`/audit` *enforcement* gate (P3), health-watcher/rate-limit/pool (P4). P2 designs the requeue *shape* only.

---

## 5. New this pass: live `smtp_mock` ground truth (so P2 builds on verified behavior)

Captured against the real `smtpmock` **v2.5.4** server (the one the `smtp_mock` gem wraps), verbatim RCPT TO:

```text
RANDOM  <hex>@example.test        -> 250 Received       # accept-all by default (GAP-14 CONFIRMED)
SEEDED  nx-seeded@example.test    -> 550 User not found  # a pre-listed addr really gets 550
BLOCKED blocked@example.test      -> 421 Service not available   # blacklisted rcptto (see delta)
SAME-SESSION (multipleRcptto): target -> 250 Received ; probe nx-seeded -> 550 User not found
```

- ✅ **GAP-14 confirmed:** random probe → `250` (accept-all), so the `valid` path needs a **seeded** probe; a
  seeded address really returns `550`; the **same-session two-RCPT catch-all shape works**.
- ⚠ **New non-door delta for the P2 build:** a blacklisted RCPT returns **`421` (4xx transient)**, not the
  spec's illustrative **`554` (5xx your-fault)**. Both map to `unknown` (harden-toward-unknown intact — no door
  touched), but the **reason differs** (`421`→transient vs the intended `smtp_blocked`). **P2 §6.1 must
  reconcile:** craft a 5xx block via `-msgRcpttoBlacklistedEmail` to exercise the `smtp_blocked` path, or accept
  that the stock blacklist knob yields a 4xx. Recorded in [`truemail-ground-truth.md`](truemail-ground-truth.md) §3.4.

---

## 6. The one open owner item (does NOT block Phase 2)

- **LICENSE** — still **deferred** (DECISIONS.md §6.1, Phase-0 sign-off). Third-party MIT is respected in
  [`NOTICE`](../../NOTICE); the project's own license is deliberately unasserted. Blocks nothing local; **revisit
  before any external distribution.** No Phase-2 dependency.

*(All other Phase-0/1 owner items are resolved: GT-1…GT-6 accepted, the Door-3 fix blessed → ADR-0006. The
DECISIONS.md §5 parked items are all P4/P5/P7-scoped — none blocks Phase 2.)*

---

## 7. Verdict

**Phase 1 is genuinely complete.** All done-criteria verified against real infrastructure, all four caught
bugs regression-locked, all five doors frozen with no open question P2 would touch, and the one previously
deferred ground-truth row (smtp_mock accept-all) is now captured from the real server. Phase 2 has a clean,
verified foundation and an explicit seam inbox. **The GAP-13 spike (wrapper same-session RCPT vs gem) is the
one thing P2 should run first.** LICENSE remains the only open owner item, and it blocks nothing here.
