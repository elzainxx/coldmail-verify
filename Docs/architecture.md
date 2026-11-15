# Architecture — how the system is built

**Status:** LIVE. Every count, name and route below was checked against the code on 2026-11-13 (migrations and roles brought up to `0016` on 2026-11-15)
(24 control-plane packages, 6 control-plane routes, 16 migrations, 21 ADRs, 11 CI jobs). If you change
the system, change this file in the same commit ([`../PROJECT_RULES.md`](../PROJECT_RULES.md) §8).
This is the map; the design authority for any one capability is its ADR ([`adr/`](adr/)) and, for depth
and history, the phase specs under [`history/`](history/).

---

## 1. What the system is

An **email-verification system**: given an address, it returns an honest verdict about whether the
address can receive mail — `valid`, `invalid`, `risky` or `unknown` — with a 0–100 confidence and a
recommended action. One principle governs everything:

> **It reports what it can prove, and says `unknown` when it can't — it never guesses a verdict.**

It verifies through progressively more expensive layers (cheap syntax/DNS checks first, a real SMTP
probe next, an optional paid third-party fallback last), learns from real send outcomes to calibrate its
own confidence, and degrades safely: removing a capability lowers a verdict's *confidence*, never its
*honesty*. `invalid` means exactly one thing — positive proof of non-deliverability. Blocked, limited,
greylisted, timed-out, ambiguous and "our DNS failed" are all `unknown` (or `risky`), never `invalid`.

## 2. Two deployables, never merged

```
   caller ──HTTP──▶  ┌───────────────────┐        ┌──────────────────────┐
                     │  control-plane    │──HTTP─▶ │  verifier-node       │
                     │  (Go, "the brain")│        │  (Ruby, "the box")   │
                     │  :8080            │        │  :3000               │
                     │  all state        │        │  stateless SMTP probe│
                     │  all decisions    │        │  forked truemail-rack│
                     │  the LOG          │        │  + pinned, unmodified│
                     │  policy, calib.   │        │  truemail 3.3.1      │
                     └───────────────────┘        └──────────────────────┘
                        Postgres 16                   port 25 outbound
```

- **`control-plane/` — the brain (Go 1.26).** Holds *all* state (Postgres) and makes *all* decisions:
  policy, the append-only verdict LOG, the cheap layers, node routing and rate limiting, the async
  queue, the paid-fallback logic and the calibration flywheel. It never speaks SMTP and has no port-25
  dependency. It listens on `:8080` unless `TM_LISTEN_ADDR` says otherwise.
- **`verifier-node/` — the box (Ruby 3.4.12, containerised).** A dumb, stateless SMTP probe: a fork of
  `truemail-rack` wrapping the **pinned, unmodified `truemail 3.3.1`** engine. It attempts an SMTP
  conversation from a reputation-bearing IP and reports raw facts; it renders no verdict. It holds no
  database credential and no pepper. It listens on `:3000`.

Why the split: the box lives on a scarce, reputation-sensitive port-25 IP; the brain holds the secrets
and accumulates the data. A stateless box means node #2 is a config line and a compromised box leaks
nothing but itself. The naming ("brain"/"box" in the design docs) is fixed by
[ADR-0005](adr/ADR-0005-deployable-naming.md).

## 3. The request path

`POST /v1/verify` runs `orchestrator.Verify` (`control-plane/internal/orchestrator/orchestrator.go`):

1. **Normalize + hash** (Door 3). An unparseable address is rejected here; the raw address is never
   stored — only `email_hash` and `domain`.
2. **Historical-bounce gate** (P7, opt-in OFF by default): ≥N independent confirmed
   `mailbox_not_found` outcomes inside a TTL window can short-circuit to `historical_bounce`.
3. **Cache lookup** on the composite key `(email_hash, config_fingerprint)`; a TTL is only used if the
   operator set `TM_CACHE_TTL_SECONDS` (unset ⇒ no caching).
4. **Cheap layers under the policy** — syntax, domain/MX/DNS (rooted names, [ADR-0019](adr/ADR-0019-dns-names-are-rooted-and-failure-is-unknown.md);
   implicit MX for A-only domains, [ADR-0020](adr/ADR-0020-implicit-mx-rfc5321.md)), disposable, role, typo,
   lists — stopping at the cheapest layer that can answer.
5. **SMTP stage**, only if the kill-switch allows it *and* a node is `:smtp`-eligible (status `active`,
   audit green, `port25_open`, and the four-way egress-IP invariant holds — [ADR-0009](adr/ADR-0009-gap24-egress-ip-invariant-runtime-enforcement.md)).
   The brain routes sticky-by-domain, paces per provider, and sends `POST /v1/verify` to the node.
   A destination the node refuses (non-public address, [ADR-0021](adr/ADR-0021-smtp-probe-destination-guard.md))
   comes back as `risky` / `non_public_mx`.
6. **External fallback** (P6, ships disabled): only for the residue, only with all three switches set;
   a vendor `valid` is capped to `risky` / `external_verified` (anti-laundering).
7. **Assemble, log, return.** The verdict is appended to the LOG (`verifications` + the raw sidecar)
   *before* it is returned; cache hits are logged too.

`POST /v1/verify/bulk` takes the same path asynchronously through the durable queue (§5). The
control-plane serves exactly six routes — `POST /v1/verify`, `POST /v1/verify/bulk`, `GET /v1/jobs/{id}`,
`GET /v1/jobs/{id}/results`, `GET /healthz`, `GET /readyz` — and [`api-contract.md`](api-contract.md) is
checked against that set by `TestRoutes_*` on every push.

**The verdict contract** (frozen, additive-only — Doors 1 and 4): `status ∈ {valid, invalid, risky,
unknown}`; `recommendation ∈ {send, review, skip}`; `reason` is one of **27** values in
`internal/vocab/vocab.go` (syntax, MX, SMTP, vendor and calibration causes — `non_public_mx` is the most
recent). `invalid ⇒ skip`; `valid ⇒ {send, review}`; `risky`/`unknown ⇒ {review, skip}`
(`vocab.ComposabilityOK`). `reason` is internal mechanics; the caller is never shown cost structure or
which node was blocked.

## 4. The control-plane packages (`control-plane/internal/`)

| package | role |
|---|---|
| `vocab` | the frozen `status`/`reason`/`recommendation` enums (Door 4) — the only place they live |
| `hash` | the frozen email normalization + HMAC hashing scheme (Door 3, `norm/v1` + `hmac-sha256/v1`) |
| `verdict` | the `/verify` response shape (Door 1) and its assembler |
| `store` | Postgres and an in-memory fake; the append-only LOG (Door 2), CAS/lease queue operations |
| `policy` | the policy engine — which layers run, gates, thresholds |
| `layers` | the cheap layers (syntax, MX/DNS, disposable, role, …) |
| `orchestrator` | the verify lifecycle (§3) |
| `api` | the HTTP front door: routes, auth (`api_callers` per-caller keys; legacy static token for sync only), errors, `/readyz` |
| `router` · `node` · `ratelimit` · `health` · `provrouting` | the node pool: routing strategy, the HTTP client, per-provider pacing, the health-watcher control loop, the provider-routing single source |
| `smtp` | brain-side SMTP surface: wrapper-response types, transcript parser, 5xx classifier, signal→verdict scoring, `probe_refused` |
| `audit` | the node audit / eligibility gate (egress-IP invariant, staleness, the `resolver` warning) |
| `queue` | the async bulk queue: lease/CAS idempotency, retry/backoff, the drainer, the at-rest address cipher |
| `external` | the P6 third-party fallback: anti-laundering, spend breaker, `Vendor` interface (no real adapter yet) |
| `flywheel` | the P7 loop: bounce classifier, recalibration + guards, promotion, weaning, `OutcomeSource` (`PullSource` fail-closed) |
| `metrics` | pure derivation of block-rate, unknown-rate, latency and pool health from the LOG |
| `secrets` · `config` | secret loading (redacted in any `String()`) and the readable config surface (defaults → env, validated at startup) |
| `idgen` | time-ordered UUIDv7 for the LOG's primary key |
| `dnsfixture` · `testpg` | test infrastructure only: an in-process authoritative DNS server, and a throwaway migrated Postgres |

The single entrypoint is `cmd/control-plane/main.go`; read `buildAPI`, `buildSMTP`, `buildQueue`,
`buildExternal` and `buildFlywheel` to see exactly which switch gates each capability. Background
loops: the health-watcher, the queue drainer, and the `verification_raw` partition maintenance loop
([ADR-0017](adr/ADR-0017-raw-partition-maintenance.md)).

## 5. The verifier-node

`verifier-node/truemail-rack/` is a vendored fork; [`FORK.md`](../verifier-node/truemail-rack/FORK.md) lists
every change (18 rows) and is the authority. The stock rack endpoints (`/`, `/version`, `/healthcheck`)
are untouched. The wrapper, mounted at `/v1`, adds:

| Endpoint | Purpose |
|---|---|
| `POST /v1/verify` | one SMTP probe, returning the engine's raw facts plus the full per-host transcript (and `probe_refused` if the destination guard refused) |
| `GET /v1/audit` | the host audit (`host_audit` from the gem, plus the wrapper's `resolver` warning); green iff `warnings` is empty |
| `GET /v1/catchall/:domain` | the catch-all probe — a *second* `Truemail.validate` for a nonexistent local part ([ADR-0007](adr/ADR-0007-gap13-catchall-session-two-session.md)) |
| `GET /v1/stats` | process-local counters (incl. `probe_refused`) |
| `POST /v1/config/reload` · `GET /v1/config` | re-apply / read the effective engine config, with an advisory fingerprint |

Door 5: the gem is a pinned dependency, never patched — `bin/assert-gem-unmodified.sh` fails CI if it
differs. Everything the wrapper adds uses the gem's public API. Image and gems: a digest-pinned
`ruby:3.4.12-slim-bookworm` base, built without a cache in CI, installing a committed `Gemfile.lock`
frozen ([ADR-0018](adr/ADR-0018-verifier-node-base-image.md)). The node's own safety controls are in
[`security-posture.md`](security-posture.md) §5.

## 6. Data model

Postgres 16, 16 forward-only migrations (`control-plane/migrations/0001…0016`, no down-migrations —
[`operator-runbook.md`](operator-runbook.md) §9). Everything lives in schema `verify` (since `0016`; nothing of module 1 is in `public`). Three roles: `verify_owner` (owns every object; no login), `verify_app` (the service; the only login) and `verify_admin`
(break-glass table privileges, no login; DDL needs `verify_owner`).

| Group | Tables | Posture |
|---|---|---|
| **The LOG** | `verifications`, `verification_raw` (monthly partitions, ADR-0017) | append-only: `verify_app` has INSERT+SELECT only (Door 2) |
| **Cache, lists & policy** | `cache`, `disposable_domains`, `spam_trap_domains`, `catchall_domains`, `policies` | runtime-writable where the layer needs it |
| **Pool** | `nodes`, `node_audits` (append-only), `node_warmup`, `rate_budgets` | the health-watcher is the single writer of node status |
| **Queue** | `work_items` (the one bounded plaintext exception, encrypted), `jobs`, `work_item_attempts` (append-only) | lease/CAS-guarded |
| **Auth** | `api_callers` (key hashes, validity windows) | runtime SELECT only |
| **Fallback** | `external_calls` | append-only; `email_hash` only |
| **Flywheel** | `send_outcomes` (append-only), `verification_outcomes`, `calibration`, `recalibration_runs`, `outcome_ingest_cursor` | versioned / CAS-guarded |

Storage CHECK constraints mirror `vocab`; adding a vocabulary value is an additive migration (0015
added `non_public_mx`).

## 7. The five one-way doors (frozen, additive-only)

Some decisions become historical data or a contract other systems code against. They may be
*extended* but never renamed, removed, repurposed or re-schemed.

1. **`/verify` response shape** (`internal/verdict`) — versioned; a field never changes meaning;
   `confidence` is always an int 0–100.
2. **The LOG is append-only** (`internal/store`, `0005_grants.sql`) — enforced by revoked
   `UPDATE`/`DELETE` grants on the runtime role, not by convention. Corrections are new rows.
3. **Email hashing** (`internal/hash`) — one frozen scheme; the raw address is never persisted. The one
   bounded exception (the queue's in-flight plaintext) is encrypted at rest and purged in the
   settlement transaction.
4. **The `status`/`reason`/`recommendation` vocabulary** (`internal/vocab`) — additive-only; the
   calibration flywheel buckets on `reason`.
5. **Never fork the engine** — `truemail` is pinned at 3.3.1 and unmodified; only the rack *server* is
   forked; the node list is a list (node #2 is a config edit, never a code change).

If a task seems to need a door touched, stop and reconsider the design.

## 8. The nine irreversible surfaces (and the test that breaks when each is neutered)

Beyond the doors, nine guards protect properties that are expensive or impossible to undo. Each has
a named **fail-verify test**: neuter the guard, watch the test go red, restore *from a copy* (not
`git checkout`, which can mask an uncommitted state), watch it go green. A guard whose test survives
neutering is not load-bearing — that is a finding. All ten test names below were confirmed to exist on
2026-11-13.

| # | Surface | Guard (file) | Test |
|---|---|---|---|
| 1 | Door-3 hashing NFC/case-fold closure | `internal/hash/hash.go` | `TestLocalCaseFoldFrozenEdge`, `TestGoldenVectorFrozen` |
| 2 | Door-2 append-only LOG | `internal/store/mem.go` + `0005_grants.sql` | `TestMemAppendOnlyAccumulates` |
| 3 | GAP-24 four-way egress-IP invariant | `internal/audit/audit.go` | `TestEligibility_EgressMismatchFailsGate_GAP24` |
| 4 | P4 fleet cascade brake | `internal/health/decide.go` | `TestEvaluate_FleetWideSpike_DoesNotEmptyPool_GAP28` |
| 5 | GAP-37 CAS exactly-once settlement | `internal/store/mem.go` + `postgres.go` | `TestSettle_ExactlyOnceUnderSlowDrainerReclaim` |
| 6 | P6 anti-laundering (rented ≠ SMTP-grade valid) | `internal/external/external.go` | `TestResolve_VendorValid_IsNeverValid` |
| 7 | P6 spend-once | `internal/store/mem.go` + `postgres.go` | `TestSpendOnce_UnderSlowDrainerReclaim` |
| 8 | P7 classifier zero-information exclusion | `internal/flywheel/classifier.go` | `TestClassify_BlockedNeverInvalidNumerator` |
| 9 | P7 recalibration CAS | `internal/store/mem_flywheel.go` + `postgres_flywheel.go` | `TestRecalibrate_CASExactlyOnce` |

Surfaces 5, 7 and 9 are the **same CAS-on-lease-holder discipline** at three layers (settle once →
spend once → recalibrate once): a transition commits only if the worker still holds the lease; a
reclaimed or slow attempt matches zero rows and discards. If you touch the queue, the fallback spend
path or the calibration writer, preserve it.

## 9. Safe degradation and self-modification

- **The kill-switch ladder** (full → SMTP-off → fallback-off → cheap-only → paused) is pulled by
  config (`TM_SMTP_MODE`, `TM_FALLBACK_MODE`, `TM_FRONTDOOR_MODE`, per-node `TM_NODE_ENABLED`) with no
  redeploy. Every rung returns a real, correctly labelled verdict. Operating it: [`operator-runbook.md`](operator-runbook.md) §3.
- **Shadow-mode.** Anything that mutates routing, confidence, eligibility or spend from data — A1
  block-rate quarantine, A6 recalibration, A8 provider promotion, A9 fallback weaning — defaults to
  `recommend-only` and is promoted per mechanism by an owner. The two systemic brakes (fleet, cascade)
  alert and hold. Operating it: [`operator-runbook.md`](operator-runbook.md) §4.
- **Dials vs flags.** Everything the specs marked LOCKED is a rule or a type; everything marked DIAL
  is a readable config entry, validated at startup (an out-of-range brake dial is a startup error).
  The full list is [`config-reference.md`](config-reference.md), generated from
  [`config-manifest.md`](config-manifest.md) and checked fresh in CI.

## 10. How the architecture is kept honest

Eleven CI jobs run on every push (`.github/workflows/ci.yml`): the Go build/vet/test against real
Postgres, the gem-unmodified assertion, the base-image and Gemfile.lock gates, the real-DNS fixtures
(search-domain, implicit MX), the node's resolver-hygiene and probe-guard gates, the node RSpec suite,
the boot-log secret scan, the docs/config-freshness gates, and the code-reviewer gate. Every gate has
a paired self-test that proves it goes red on a mutant. The test strategy and the rule that every gate
must be named there is [`test-strategy.md`](test-strategy.md).

Where to read next: the rules ([`../PROJECT_RULES.md`](../PROJECT_RULES.md)), the decisions
([`open-decisions.md`](open-decisions.md), [`adr/`](adr/)), the security model
([`security-posture.md`](security-posture.md)), and what is not done ([`future-work.md`](future-work.md)).
