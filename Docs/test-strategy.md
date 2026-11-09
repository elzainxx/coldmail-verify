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
1. **control-plane:** `go build ./...`, `go vet ./...`, `go test -v ./...` against a real Postgres
2. **every declared test actually EXECUTED** — a skip is a shortfall, not a pass; two-tiered so the
   PENDING GAP harness below stays legal and everything else must run
   (`scripts/check-test-execution.sh`, plus its paired self-test)
3. **verifier-node (Door 5):** `assert-gem-unmodified.sh` (host-portable) + `bundle list truemail`==3.3.1 in Docker
4. **verifier-node RSpec** — the fork's own suite (stock specs + the P2 `/v1` wrapper specs)
5. **no credential reaches a log line** — boot the real binary with canary secrets and scan the log
   (`scripts/check-no-secret-leak.sh`)
6. **docs-updated staleness check** — a behavior change without a doc/ADR touch fails
   (`scripts/check-docs-updated.sh`, plus its paired self-test
   `scripts/check-docs-updated-selftest.sh`). The self-test is not optional decoration: this gate
   reported `ok (code_changed=true docs_changed=true)` for weeks on changes it exists to reject,
   because its verdict came from a UNION of the previous commit's diff and the working tree — the
   range supplied `docs_changed`, the tree supplied `code_changed`, and no single change ever had to
   satisfy the rule. Two further holes were measured while confirming that one: untracked files were
   invisible to `git diff` (a brand-new behaviour file could not be seen at all), and an unresolvable
   base ref degraded to an empty diff, which this gate reports as success. Cases 2, 3 and 11 of the
   self-test are those three measurements; case 10 fails if the fix ever disarms CI's commit-range
   enforcement.
7. **config-reference freshness** — regenerate and diff; drift fails
   (`scripts/check-config-reference-fresh.sh`)
8. **test discovery intact + every gate wired** — the meta-gate: no second `go.mod`, no build-tagged
   test file, RSpec's glob unnarrowed, every `check-*.sh` named in the workflow, every §7 job present,
   and every gate named in THIS list (`scripts/check-test-enumeration.sh`, plus its paired self-test)
9. **the code-reviewer gate** — on every change, docs included
10. **the verifier-node base image can never again be a cache-only build** — the Dockerfile's `FROM` is
    pinned by DIGEST, names a supported Debian (bookworm/trixie, never bullseye or older), carries the
    Ruby patch `.ruby-version` names, and every `docker build` in the workflow passes `--pull --no-cache`
    (`scripts/check-base-image-pinned.sh`, which since 2026-11-09 also fails any workflow `image:` — the postgres service — without a digest; plus its paired self-test
    `scripts/check-base-image-pinned-selftest.sh`). The defect it guards was invisible for months: the
    base was `ruby:3.2.0-slim` (Debian 11), whose `debian-security` repository was removed, so
    `apt-get install` 404s — but only on a build that does not reuse a cached layer, and every local build
    did. It surfaced the first time a real GitHub runner built the image from scratch (ADR-0018). The
    self-test builds a throwaway tree that trips each assertion in turn and checks the exit code AND the
    message, so the gate cannot be quietly broken into accepting everything.
11. **a DNS failure of OURS is `unknown`, never `invalid`, on a host with a resolver search domain** — the layers'
    DNS tests run against a real-protocol, in-process DNS server inside a container whose `resolv.conf` carries a
    `search` line (`scripts/check-dns-search-domain.sh`, plus its paired self-test
    `scripts/check-dns-search-domain-selftest.sh`). The defect: Go's resolver walks the search list and returns the
    LAST name's error, so a SERVFAIL for the user's domain was overwritten by the NXDOMAIN of the suffixed name, the MX
    layer reported `invalid / no_mx`, and a DNS outage became a fact about someone's address. A dev host has no search
    domain, so no local run could see it (ADR-0019; module 3's From-gate had the same bug, found by its first GitHub
    Actions run). `TestSearchDomainCanary` makes the fixture self-proving (it asserts the unrooted lookup really does
    collapse under the search list), and the self-test builds a MUTANT with name-rooting disabled and requires the
    SERVFAIL case to go red with the collapse signature, canary green — so "reds without the fix" is checked on every
    run and not asserted once.
12. **the verifier-node image installs exactly the gems in a committed `Gemfile.lock`** — the lock is tracked; the Dockerfile
    COPYs it, runs no `bundle lock`/`bundle update`, and sets `frozen` so bundler fails instead of re-resolving; the lock's
    `truemail` and `net-smtp` equal the Gemfile's exact pins (so Door 5's pin cannot drift from what is installed); bundler is
    installed at the exact version in `BUNDLED WITH`; the lock's Ruby is the patch `.ruby-version` names; and the dev/test
    gems are in it so the RSpec job does not re-resolve (`scripts/check-gemfile-lock.sh`, plus its paired self-test
    `scripts/check-gemfile-lock-selftest.sh`, 14 cases each matched on the failure message). The defect: the Dockerfile ran
    `bundle lock` on every build, so only two gems were pinned and every transitive gem floated — a digest-pinned base
    (item 10) made the OS layer reproducible while the gems above it were not (ADR-0018 addendum).
13. **a domain with no MX but an A/AAAA record is not `invalid` (RFC 5321 §5.1), proved against REAL DNS shapes** — the layers' MX
    tests and the orchestrator's whole `Verify` path run the production resolver over the real DNS protocol against an in-process
    authoritative server (`internal/dnsfixture`) inside a container whose `resolv.conf` has a `search` line
    (`scripts/check-implicit-mx.sh`, plus its paired self-test `scripts/check-implicit-mx-selftest.sh`). Pairs: A-only → reaches the SMTP
    probe, and is `risky` if the probe cannot run; truly no records, NXDOMAIN and null MX (with or without an A record) → still `invalid`;
    every partial failure of the A/AAAA follow-up → `unknown`. The defect: the old fallback was dead code, because Go reports NODATA as a
    not-found *error* and every fixture was a fake that returned an empty list instead (ADR-0020). The self-test builds two MUTANTS — the
    address follow-up removed, and an AAAA failure counted as "absent" (Go's own combined A+AAAA lookup keeps whichever error arrives last) —
    and requires each to go red in the right test with the right signature while the no-records/NXDOMAIN/null-MX controls stay green.

14. **a verifier node whose resolver is not clean does no SMTP** — the fork's `GET /v1/audit` adds a `resolver` warning (so the audit is red,
    the node is quarantined, and the brain routes it nothing) when `/etc/resolv.conf` has a `search` or `domain` line or `ndots` > 1, or the
    hostname contains a dot (`scripts/check-resolver-hygiene.sh`, plus its paired self-test `scripts/check-resolver-hygiene-selftest.sh`).
    The gate runs the real fork module under the node image's own digest-pinned Ruby in containers whose resolv.conf and hostname are the
    fixture's, asserts 8 expected verdicts, and checks SOUNDNESS against an oracle — wherever Ruby's own `Resolv::DNS` would use a search
    domain (in either of truemail's two modes) the check must be red. The self-test builds three mutants (search/domain rule off, hostname rule
    off, ndots rule off) and requires each to go red for that rule only. The defect: the rule "no search domain, no dotted hostname" was a
    sentence in a runbook ("a documented deployment rule is not a control"), and a resolv.conf-only check would have missed the hostname half,
    which Ruby turns into a search domain even when resolv.conf is clean (ADR-0019 addendum). Also pinned in Go on the exact wrapper JSON
    (`internal/audit/resolver_warning_test.go`: red even when the box claims green) and end to end (`internal/orchestrator/resolver_hygiene_test.go`:
    real scheduler, real store, the node is never called, the verdict is `risky/plausible`, with a clean-audit control that does run the probe).
15. **the verifier node never opens an SMTP session to a non-public address** — a pre-flight in the forked wrapper refuses loopback, RFC 1918,
    link-local/cloud-metadata, CGNAT and every other non-public range before the gem's SMTP step; a post-check discards a transcript that leaked
    through DNS rebinding; the brain turns the refusal into `risky` / `non_public_mx` (`scripts/check-probe-guard.sh`, plus its paired self-test
    `scripts/check-probe-guard-selftest.sh`). The gate has Python generate ~51k addresses from its OWN transcription of the IANA registries (every
    block's edges and midpoint, seeded random IPv4/IPv6, IPv4-mapped forms), the REAL guard decide each under the node image's Ruby, and Python's
    `ipaddress.is_global` judge each: the guard must never allow a non-global address. It also fails if the shipped egress ruleset
    (`verifier-node/egress/verifier_egress.nft`) lacks a range the guard refuses, loses its IPv6 allow-list rule, or is not valid nftables. The self-test
    builds four mutants (CGNAT, link-local, 172.16/12 removed from the guard; CGNAT removed from the firewall file) and requires each to go red at the
    removed block's edges only. The first version generated its edges from the guard's own table, so deleting a range deleted its test points — found
    by asking why a mutant produced 2 violations and not 4+ (ADR-0021). Also: RSpec (every range + its public neighbours), the socket-proof harness
    (`test/p2` rows 9–11: a counting listener sees 0 connections with the guard on and 2 with the control), and Go (golden wire fixture, end-to-end
    `Verify`, and `TestVocabMirrorsTheDatabaseCheckConstraints` against real Postgres, with a through-0014 control).

**This list is checked, not trusted.** It stood at five items while CI ran nine, which is the same
count-drift this project has already found twice (a gate count below CI's; 53 documented config keys
against 89 in the database). `check-test-enumeration.sh` rule 7 fails if any `check-*.sh` wired into
the workflow is not named here — so the mechanism that catches an unwired gate also catches this list
falling behind it. That is why each item carries its `scripts/…` path as well as the sentence saying
what it proves: the prose is for the reader, the path is what a mechanism can check. Item 1 is
intentionally not a script and is exempt; everything else must appear here, so adding a gate without
touching this file is red. What rule 7 does NOT prove is that a description here is *accurate* — a
wrong sentence about a real gate passes, and that remains the code reviewer's job (§7).

CI **must not fake green**: the rollout-only truths (§4) are reported as pending markers, and the
verifier-node Docker build is the real (not stubbed) resolution of the pinned gem. §5's own history is
why: `go test ./...` exiting 0 says nothing about whether the tests RAN (audit D13 — two grant tests
that always skipped while ci.yml claimed they proved append-only on every push), which is what item 2
exists to assert.
