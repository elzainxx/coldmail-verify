# Test strategy — real tests, tied to the GAPs (not syntax checks)

**Status:** LIVE — Phase-0 testing foundation. Scaffold + strategy; the harnesses are
**real-shaped but empty** (they run, they assert nothing false, and each is wired to the
named GAP it will guard).
**Principle:** the tests that matter here are **behavioral / integration tests tied to the
named GAPs**, not unit/lint alone. A test earns its place only if it would **actually fail if
the GAP regressed.** (PROJECT_RULES.md §7.)

> **Honesty rule (non-negotiable, from the prompt):** two truths are only provable against
> **real providers at rollout** — **greylist retry-then-accept** and **vendor accuracy**. The
> mock/stub substrate can prove *loop mechanics* and *classification*, never *real provider
> behavior*. We **never fake green** on those; they are marked `PROVABLE-ONLY-AT-ROLLOUT` and
> the harness asserts the *mechanism*, with the *behavioral* truth explicitly deferred.

---

## 1. The layers of testing (and what each is allowed to claim)

| Layer | Substrate | Proves | Cannot prove |
|---|---|---|---|
| **Unit** | in-process (Go `testing`, Ruby RSpec) | frozen contracts: Door-4 vocab, Door-3 hashing, config defaults, Door-1 response shape | anything about a real node or provider |
| **Behavioral / integration** | socket-level mocks (`smtp_mock`/`dns_mock`), DB, in-process CAS harness | wire behavior, idempotency mechanics, the audit cross-check, exclusion discipline | real-provider behavior (greylist, vendor accuracy) |
| **Rollout (real)** | a real node, real providers, real bounces | greylist retry-then-accept; vendor per-provider accuracy | — (this is the only layer that can) |

The unit layer exists **today** (Phase-0 control-plane: `vocab`, `hash`, `config`, `api` tests pass).
The behavioral layer is **scaffolded** below. The rollout layer is **named, not faked**.

---

## 2. The GAP-tied harnesses (the heart of the strategy)

Each harness targets a specific named GAP from the specs. Phase 0 ships each as a **compiled,
runnable, empty-but-real-shaped** stub (a Go test file under `test/` that skips with an explicit
`PENDING` reason, or a table with the scenarios enumerated), so the scenario list can never
silently drift and each has a home the owning phase fills.

| Harness | Named GAP | What it must eventually assert | Phase | Substrate |
|---|---|---|---|---|
| **SMTP-mock substrate** | P2 GAP-11/14 | the socket-level `smtp_mock`+`dns_mock` exercise the real SMTP path; the `valid` path needs a **seeded** probe (accept-all mock) | P2 | `smtp_mock`/`dns_mock` (Docker) |
| **Greylist round-trip** | **P5 GAP-35** (un-mockable) | the retry *loop mechanics* (backoff schedule, requeue, ceiling→settle) via a **by-attempt stub** (450 then 250 by attempt count); the retry-then-accept *behavior* is `PROVABLE-ONLY-AT-ROLLOUT` | P5 | by-attempt stub (not `smtp_mock` — it has no stateful greylist, GAP-12) |
| **Idempotency / CAS exactly-once** | **P5 GAP-37** | the sharpest catch: a *slow (not dead)* lease-holder + a TTL reclaim must NOT double-settle/double-run — only the CAS-on-lease-holder commits; a lost attempt discards its result | P5 | in-process concurrency harness against a real (or emulated) transactional store |
| **Multi-homed audit cross-check** | **P3 GAP-24** | `host_audit.current_host_ip` == A-record == PTR == egress IP; a green audit on the *private* IP with an unaudited public egress IP must be **caught**, not passed | P3 | `dns_mock` PTR/A fixtures + a two-IP fixture |
| **Bounce-classifier exclusion** | **P7 GAP-49** | `mailbox_full`→valid-signal; blocked/policy/transient→**zero-information** (excluded); the majority-exclusion discipline; a `zero-information` bounce never creates a historical-bounce record | P7 | outcome-event fixtures (the P7 Appendix B truth table) |

Two of these — **GAP-37** (idempotency) and **GAP-49** (classifier) — are called out in the specs
as the "correctness heart" of their phases. They get the most adversarial scenarios.

---

## 3. Per-phase acceptance tests — one that fails if the phase's "done when" regresses

For each phase's build-plan "done when", a **real acceptance test** (not a smoke check) that
would fail if the defining GAP regressed. Phase 0 enumerates them as the acceptance matrix each
phase must satisfy; the owning phase implements the assertions.

| Phase | "done when" (build plan) | The acceptance test that guards it |
|---|---|---|
| **0** | both deployables build empty; manifest loadable; cheap-only rung works; harness runs empty-but-real | **implemented now:** `go build/test/vet` green; `config.CheapLayersOnly()` true by default; gem-unmodified check passes; this harness compiles and runs |
| **1** | POST an address → scored, logged verdict, cheap layers only, no port 25 | a request produces a valid Door-1 response **and** exactly one append-only LOG row (incl. `from_cache`/`invalid_syntax` NULL-hash cases) |
| **2** | full pipeline on mocks; mock→real is one config value | the §3.5 "Prove:" matrix on socket mocks (`valid` via seeded probe; `invalid`; `unknown/blocked`; `catch_all`; `timeout`) + a config-only mock→real diff |
| **3** | one audited verifier-node, real verdicts, big providers `:mx` | `:smtp`-eligible **iff** `green AND port25_open`; forced-red → quarantine → degrade to cheap (never `invalid`); the GAP-24 four-way IP identity holds |
| **4** | a blocked verifier-node auto-quarantines, traffic re-routes, +1 config line | quarantine node #1 (present node #2) → traffic flows to #2, not degrade; fleet-wide spike → **alert, not cascade** (GAP-28); node #2 = config-only diff |
| **5** | greylist retried on backoff & settles; bulk job handle; group-by-domain; no gate bypassed | GAP-37 no-double-settle under reclaim; GAP-35 loop mechanics; every queued check passes the identical router+gates (master invariant) |
| **6** | residue escalates to a stubbed vendor; never non-residue; spend-once | vendor `valid`→`risky` (anti-laundering); hard-invalid/valid/cache-hit never escalated; GAP-42 no double-charge under reclaim |
| **7** | loop closed; recalibration gated; accuracy per provider; weaning per bucket | GAP-49 classifier truth table; recalibration sample-gated + bounded-step + cascade-brake; `unknown` never pressured to `valid` |

---

## 4. What we will NOT fake green (rollout-only truths)

- **Greylist retry-then-accept** (P5 GAP-35 / GAP-12): `smtp_mock` has **no stateful
  reject-then-accept** mode. We test the loop with a by-attempt stub and validate the true
  round-trip **only against a real greylisting provider at rollout.** The harness that would
  "prove greylisting" against a mock is deliberately **absent** — its presence would be a lie.
- **Vendor per-provider accuracy** (P6 GAP-47): the vendor's real accuracy on the exact residue
  we send is **untestable pre-integration.** We test routing/budget/breaker/idempotency against a
  **stubbed adapter**; accuracy is measured by P7 against real bounces. We claim "coverage exists
  and is bounded/honest," never "coverage is accurate."
- **Real IP reputation / warmup / real-provider blocks** (P3): only a real clean IP talking to
  real MX proves these. Mocks prove **code-path identity**, not behavioral identity — stated in
  P2 §3.5 and honored here.

CI surfaces these as **explicit `PROVABLE-ONLY-AT-ROLLOUT` markers**, not skipped-and-forgotten
— a green CI never implies these are proven.

---

## 5. CI shape

Per PROJECT_RULES.md §7, every change runs (see [`../.github/workflows/ci.yml`](../.github/workflows/ci.yml)):
1. **control-plane:** `go build ./...`, `go vet ./...`, `go test ./...`
2. **verifier-node (Door 5):** `assert-gem-unmodified.sh` (host-portable) + `bundle list truemail`==3.3.1 in Docker
3. **docs-updated staleness check** — a behavior change without a doc/ADR touch fails
4. **config-reference freshness** — regenerate and diff; drift fails
5. **the code-reviewer gate** — on every change, docs included

CI **must not fake green**: the rollout-only truths (§4) are reported as pending markers, and the
verifier-node Docker build is the real (not stubbed) resolution of the pinned gem.
