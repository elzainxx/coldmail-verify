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
| 2b | truemail-rack **forked & running stock** | ✅ **met** (closeout 2026-07-17) | Built (`truemail-verifier-node:p0`, 664MB) + **ran live**; `Truemail::VERSION=3.3.1` at runtime; stock rack answered `/version`,`/healthcheck`,`/` on :3000. Fork genuinely runs. |
| 2c | **truemail-ground-truth.md** reconciles real vs specs | ✅ **met** (closeout 2026-07-17) | **All rows now `SOURCE ✓` or `LIVE ✓`; none PENDING.** GAP-1/2/3/4/8 confirmed (source + live); GT-4 host_audit confirmed live; GT-6 (extra host_audit keys) newly flagged. No door contradicted. |
| 3 | Config manifest **loadable**; secrets scaffold works | ✅ **met** | `internal/config` loads; `/readyz` shows the ladder; secrets presence-only, no values |
| 3b | The **cheap-layers-only kill-switch rung actually works** (only rung reachable without SMTP) | ✅ **met** | `config.Defaults()` → `CheapLayersOnly()==true`; live `/readyz` shows `ladder=cheap-layers-only(Rung3)`; `TestDefaultsAreCheapLayersOnly` |
| 3c | Nothing hardcoded that the manifest says is a flag | ✅ **met** | Kill-switch rungs, shadow-modes, dials all readable config; unset dials read `UNSET(owner-OQ)`, never a guessed number |
| 4 | **PROJECT_RULES.md**, the **ADR log**, living-docs enforcement in place | ✅ **met** | [PROJECT_RULES.md](../PROJECT_RULES.md); [adr/](adr/) (4 ADRs + index); [Docs/README.md](README.md); docs-updated gate proven to fail on a code-only change |
| 5 | The **test harness runs** (empty but real-shaped); code-reviewer + docs-updated gate active in CI | ✅ **met** | `test/gaps/` runs as SKIP-with-PENDING (never fake-pass); [.github/workflows/ci.yml](../.github/workflows/ci.yml) runs build/test/vet + Door-5 + docs-updated + config-fresh + code-reviewer |
| 6 | A dev can understand the doors + ladder from PROJECT_RULES + /docs without all seven specs | ✅ **met** | [PROJECT_RULES.md](../PROJECT_RULES.md) §§1–9 |

**Net:** **10 of 10 fully met** after the 2026-07-17 closeout (2b/2c were the two partials; both are now
met via the live run). The deployables are also renamed (brain→control-plane, box→verifier-node,
[ADR-0005](adr/ADR-0005-deployable-naming.md)).

---

## Closeout resolution — the two former gaps are now closed (2026-07-17)

Both gaps that the *original* pass honestly left PENDING (because that environment had no Docker
daemon, no host Ruby, and a blocked registry) are now **closed by an actual live run** on a machine
with Docker available:

### G-1 (was: live build/run PENDING) → **RESOLVED**
The fork was **built and executed**: `truemail-verifier-node:p0` (664MB) on `ruby:3.2.0-slim` *(historical: that Debian 11 base could not be rebuilt from scratch and was replaced on 2026-11-08 — [ADR-0018](adr/ADR-0018-verifier-node-base-image.md))*; `bundle`
resolved + installed **truemail 3.3.1** + **net-smtp 0.5.1** + native thin/eventmachine; the stock rack
ran and answered `/version`, `/healthcheck`, `/` on :3000. **Door 5 proven at runtime:**
`Truemail::VERSION = 3.3.1`. We are extending the fork, not rewriting it.

> **Environment hurdle, resolved the right way (recorded for reproducibility):** the host runs a
> **Norton SSL/TLS-scanning proxy** that re-signs HTTPS with a private root CA, breaking in-container
> `gem`/`bundle` cert verification. Fixed by **trusting** that CA in the image
> (`verifier-node/truemail-rack/certs/` — machine-local, gitignored — installed via
> `update-ca-certificates`); TLS verification is **never disabled**. Also: base pinned to
> `ruby:3.2.0-slim` to match the fork's `.ruby-version` (its Gemfile pins ruby exactly; *superseded 2026-11-08 by
> `ruby:3.2.11-slim-bookworm@sha256:…` — ADR-0018*). A clean CI has
> no such cert → the trust step is a no-op. Details in the Dockerfile + FORK.md.

### G-2 (was: ground-truth rows PENDING) → **RESOLVED**
**Every PENDING-RUN row in [truemail-ground-truth.md](truemail-ground-truth.md) is now filled from real
captured output** — endpoints, the lean JSON body (`domain`/`mail_servers` confirmed **dropped** live),
the `smtp_debug` array shape (real gmail.com attempt), `host_audit` structure (GT-4), `/version`, auth,
and GAP-1 (`/validate` → 404). One new minor delta surfaced (**GT-6:** `host_audit.as_json` carries
extra `date`/`configuration` keys — additive, non-door). **No frozen door is contradicted.**

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
mock-vs-live ruling → [ADR-0004](adr/ADR-0004-cheap-layers-brain-native.md)). The project's own
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
