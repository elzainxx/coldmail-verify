# Phase 0 — status: what shipped, what's verified, what's honestly pending

**Date:** 2026-07-17 · **Commit:** `d582ab4` (the Phase-0 spine).
**What Phase 0 was:** set up the ground the whole build stands on — **scaffold + contracts +
living docs**, no feature logic. This doc is the honest accounting against the phase's
"done when" list, including the two things this environment could **not** execute (flagged,
not faked).

---

## Done-criteria scorecard

| # | Phase-0 "done when" | Status | Evidence |
|---|---|---|---|
| 1 | Repo scaffolds for **both deployables** exist and build empty | ✅ **met** | `control-plane/` `go build ./...` clean; `verifier-node/truemail-rack/` forked + Dockerized |
| 2 | truemail gem **pinned** (ADR recorded) | ✅ **met** | `Gemfile` `truemail '3.3.1'` exact; [ADR-0003](adr/ADR-0003-truemail-engine-pin.md); Door-5 check passes |
| 2b | truemail-rack **forked & running stock** | ⚠️ **partial** | Forked + Dockerfile + compose written; **live `docker build`/run PENDING** (no Docker daemon + no host Ruby + blocked rubygems here). Reproducible command in ground-truth §0. |
| 2c | **truemail-ground-truth.md** reconciles real vs specs | ⚠️ **partial** | Source-read reconciliation **complete** (endpoints, JSON shape, config surface, versions — GAP-1/2/3/4/8 confirmed from source); **live-run rows PENDING** (smtp_debug runtime, host_audit, GAP-14 accept-all). Flagged, not fabricated. |
| 3 | Config manifest **loadable**; secrets scaffold works | ✅ **met** | `internal/config` loads; `/readyz` shows the ladder; secrets presence-only, no values |
| 3b | The **cheap-layers-only kill-switch rung actually works** (only rung reachable without SMTP) | ✅ **met** | `config.Defaults()` → `CheapLayersOnly()==true`; live `/readyz` shows `ladder=cheap-layers-only(Rung3)`; `TestDefaultsAreCheapLayersOnly` |
| 3c | Nothing hardcoded that the manifest says is a flag | ✅ **met** | Kill-switch rungs, shadow-modes, dials all readable config; unset dials read `UNSET(owner-OQ)`, never a guessed number |
| 4 | **PROJECT_RULES.md**, the **ADR log**, living-docs enforcement in place | ✅ **met** | [PROJECT_RULES.md](../PROJECT_RULES.md); [adr/](adr/) (4 ADRs + index); [Docs/README.md](README.md); docs-updated gate proven to fail on a code-only change |
| 5 | The **test harness runs** (empty but real-shaped); code-reviewer + docs-updated gate active in CI | ✅ **met** | `test/gaps/` runs as SKIP-with-PENDING (never fake-pass); [.github/workflows/ci.yml](../.github/workflows/ci.yml) runs build/test/vet + Door-5 + docs-updated + config-fresh + code-reviewer |
| 6 | A dev can understand the doors + ladder from PROJECT_RULES + /docs without all seven specs | ✅ **met** | [PROJECT_RULES.md](../PROJECT_RULES.md) §§1–9 |

**Net:** 8 of 10 fully met; **2 partial** (2b/2c) — both blocked only by *this environment's* inability to run Docker/Ruby, both **ready-and-reproducible**, both flagged rather than faked.

---

## The two honest gaps (flagged, not faked)

### G-1 · Live verifier-node build/run is PENDING (criterion 2b)
- **Why:** Docker daemon not running in this sandbox; Ruby not on the host; rubygems.org HTTPS
  blocked (Windows schannel revocation error). The verifier-node builds/runs **only** via Docker.
- **What IS done:** the fork is vendored at v0.9.0, de-gitted, provenance in
  [FORK.md](../verifier-node/truemail-rack/FORK.md); gem pinned exactly to 3.3.1; the forced `net-smtp`
  bump applied; `Dockerfile` + `docker-compose.yml` written; the Door-5 gem-unmodified check
  **passes**.
- **To close:** run `docker compose -f verifier-node/docker-compose.yml build && … up` on a host with a
  Docker daemon; confirm `bundle list truemail` == 3.3.1. Exact commands in
  [truemail-ground-truth.md §0](truemail-ground-truth.md).

### G-2 · Ground-truth live-run rows are PENDING (criterion 2c)
- **Why:** same as G-1 — capturing *runtime* engine output needs the running verifier-node + mock substrate.
- **What IS done:** everything **source-readable** is reconciled from the fork's actual source +
  the gem's real gemspec + upstream's documented output — endpoints (no `/validate`, GAP-1), the
  lean JSON body (no domain/mail_servers, GAP-2/9), raw-string smtp_debug (GAP-4), subset config
  (GAP-3), local-tz date (GAP-8), and the real versions (gem 3.3.1, rack 0.9.0 — GT-1/GT-2). **No
  frozen door is contradicted.**
- **Still PENDING (do not freeze on these until captured live):** runtime `smtp_debug` array on a
  real reject; `host_audit` live output (a gem primitive, not exposed by the stock rack — needs the
  P2 wrapper or a direct in-container call); `smtp_mock` accept-all behavior (GAP-14). All five
  deltas are in [truemail-ground-truth.md §5](truemail-ground-truth.md), tagged for owner sign-off.

Neither gap blocks Phase 1 *planning*; both must be closed before Phase 2/3 code treats the
pending shapes as reality-verified.

---

## Spec-vs-reality mismatches surfaced for owner sign-off (never silently fixed)

From [truemail-ground-truth.md §5](truemail-ground-truth.md): **GT-1** gem `3.3.0`→`3.3.1`
(illustrative spec string; pin 3.3.1), **GT-2** rack `0.10.0`→`0.9.0` (no 0.10.0 tag exists),
**GT-3** `net-smtp` forced `0.4.0`→`0.5.0` (a hard resolver conflict the exact pin creates — a
rack-side edit, not a gem patch), **GT-4** `host_audit` live shape pending, **GT-5** `smtp_mock`
accept-all pending. None mutates a frozen door.

## Owner decisions still parked (unchanged by Phase 0)

Phase 0 **decided nothing** that was owner-open. The parked sub-decisions in
[DECISIONS.md §5](DECISIONS.md) remain parked; the [§6 owner-OQ register](DECISIONS.md) added
this phase only *points* to where already-decided OQs live (P6-3, P6-6, P7-2, P7-8, and the P1
mock-vs-live ruling → [ADR-0004](adr/ADR-0004-cheap-layers-control-plane-native.md)). The project's own
**LICENSE is still an owner choice** (README.md "License posture") — third-party MIT respected in
[NOTICE](../NOTICE), the project's own license deliberately not asserted.

## Code-review gate — outcome

The required review gate ran on the Phase-0 diff. The background reviewer **stalled** (0-byte
output, ~37 min idle — the same failure this project recorded in P3 Appendix C), but not before
surfacing one genuine lead, which was then confirmed and completed by an **inline adversarial pass**
(the documented fallback):

- **MAJOR (Door 3) — CONFIRMED & FIXED.** `hash.Normalize` guarded `isASCII` on the domain but
  **not the local part**, so a non-ASCII local part was hashed without NFC — composed vs decomposed
  byte forms of the same address produced **different `email_hash` values**, splitting one identity
  in the frozen store. Empirically confirmed with a failing test, then fixed by refusing non-ASCII
  on **both** sides until NFC/IDNA is wired at P1. Locked with a permanent regression test
  (`TestNonASCIILocalPartRefused`). The original suite missed it by only exercising the domain path.
- **Inline sweep of the remaining high-risk surfaces — CLEAN:** no secret value reaches any
  log/print path (`secrets.String()` is presence-only via `redact()`); the assembler emits only
  frozen vocabulary constants and guards with `Response.Valid()`; Door-2 append-only holds by
  interface shape; Door-5 pin is exact + CI-enforced.

Full control-plane suite green after the fix (build + vet + all unit tests; GAP harness SKIPs as designed).

## What Phase 0 deliberately did NOT build (correct scope)

No cheap-layer logic, no SMTP, no wrapper endpoints, no queue, no fallback, no calibration, no real
DB — all later-phase. The control-plane returns an honest `unknown`/`plausible` (never a guessed verdict);
the node client returns `ErrNotImplemented` (never a fake verdict); the store offers no
verdict-mutation method (Door 2 by shape). Stubs respect contracts; features are the phases that own them.
