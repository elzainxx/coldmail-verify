# ADR-0003 — The truemail engine is pinned at 3.3.1, never forked

**Status:** ACCEPTED — 2026-07-17 (Phase 0)
**Door:** Door 5 (never fork the engine; node-list discipline)
**Deciders:** owner (build discipline); recorded by the Phase-0 scaffolding pass
**Companions:** [`../../PROJECT_RULES.md`](../../PROJECT_RULES.md) · [`../../box/truemail-rack/FORK.md`](../../box/truemail-rack/FORK.md) · [`../truemail-ground-truth.md`](../truemail-ground-truth.md) · [`DECISIONS.md`](../DECISIONS.md)

---

## Context

Door 5 (build plan §0, PROJECT_RULES.md) is absolute: **the `truemail` validation engine is a
pinned dependency and is never forked or patched; only `truemail-rack` (the HTTP server) is
forked.** Phase 0 must (a) pin the engine to an *exact* version — not a range — and (b) record
that pin as a one-way-door decision, because the engine's emitted shape is what the five frozen
contracts were written against.

The prompt asked to **confirm whether `v3.3.1` is the correct/current version** and pin explicitly.

## Decision

**Pin `truemail` to exactly `3.3.1`.** The fork's `Gemfile` reads `gem 'truemail', '3.3.1'`
(exact, no `~>`/`>=`). CI asserts this (`box/truemail-rack/bin/assert-gem-unmodified.sh` + an
in-Docker `bundle list truemail` check). The engine is **never** forked; any behavioral gap is
closed brain-side or in the rack wrapper (Phase 2+), never by editing the gem.

### Why 3.3.1 (confirmed, not assumed)

- The latest `truemail` release tag on GitHub is **`v3.3.1`** (verified 2026-07-17 via
  `git ls-remote --tags`); its `lib/truemail/version.rb` reads `VERSION = '3.3.1'`. So the
  prompt's "confirm v3.3.1 is correct/current" resolves **yes** — 3.3.1 is current, pin it.
- The phase specs' appendices cite `gem@3.3.0` / `rack@0.10.0` as **illustrative example
  strings** (README captures), not frozen contracts. The real fork base is **rack `v0.9.0`**
  (no `0.10.0` tag exists) embedding **gem `3.3.1`**. Logged as ground-truth deltas
  ([`../truemail-ground-truth.md`](../truemail-ground-truth.md)); they touch no frozen door.

### The engine stays box-side (not pulled into the brain)

Per the prompt: the brain **may not need the gem in Phase 1** (cheap layers are brain-native Go —
P1 OQ-6). So the gem dependency lives **only in `box/truemail-rack/`** (Ruby); the Go brain does
**not** take a truemail dependency. Pin it in the repo (the fork), keep it out of the brain.

### One forced consequence (flagged, not silent)

Pinning 3.3.1 exactly **forces a second, rack-side edit**: `truemail 3.3.1` declares
`net-smtp '~> 0.5.0'` (Ruby ≥ 3.1; we pin Ruby 3.2), while upstream rack v0.9.0 pinned
`net-smtp '~> 0.4.0'` — a **disjoint range, a hard resolver conflict**. Resolution: bump the
**fork's** `net-smtp` to `~> 0.5.0` (a rack-side dependency edit — legitimate, the rack is what
we fork; **not** a gem patch). Recorded in `FORK.md` modification #2. The stale `Gemfile.lock`
(which pinned `truemail 3.1.0`) is deleted and regenerated inside Docker.

## Consequences

- **Positive:** the engine's emitted shape is frozen against a known version; `engine_version`
  provenance (`gem@3.3.1`) is exact; Door 5 is CI-enforced from the first commit.
- **Cost:** upgrading the engine is now a deliberate, ADR-amending, ground-truth-re-capturing act
  (see `FORK.md` "How to update the fork base") — by design.
- **Open (not decided here):** whether/when to move to a future `truemail` release is a future
  owner decision; a bump re-runs ground-truth reconciliation because a new version *could* change
  an emitted field a frozen door depends on. That is exactly why the pin is exact.

## Compliance / enforcement

- `box/truemail-rack/Gemfile` — exact pin (asserted by the Door-5 check).
- `box/truemail-rack/bin/assert-gem-unmodified.sh` — host/CI check: exact pin, no vendored engine
  source, ADR agreement.
- In Docker: `bundle list truemail` must report exactly `3.3.1`.
- `box/truemail-rack/FORK.md` — the exhaustive rack-side modification list.
