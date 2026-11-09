# Phase 1 — status: foundations build (live scorecard)

**Date:** 2026-07-17 · **Phase:** 1 (Foundations) · **Spec:** [`phase-1-foundations-spec.md`](phase-1-foundations-spec.md)
**Status: ✅ COMPLETE** — all 7 increments done; the done-when is demonstrated against real
Postgres + real DNS (see increment 6). Pending owner review of the phase.
**What Phase 1 is:** make the five doors *real* (Phase 0 encoded their shapes), stand up the Supabase
data model, build the control-plane skeleton + cheap layers + one policy — so you can **POST an address
and get a scored, logged verdict using cheap layers only, no port 25 anywhere.**

**Prerequisite gate (checked before starting):** ✅ all four conditions met — deployables renamed,
verifier-node ran for real, ground-truth fully real, and the Phase-0 sign-off is **resolved in
[DECISIONS.md](../open-decisions.md) §6.1** (GT-1…GT-6 accepted, Door-3 fix blessed → [ADR-0006](../adr/ADR-0006-door3-nonascii-localpart-refusal.md), LICENSE deferred).

---

## Build increments (in dependency order)

| # | Increment | Status | Evidence |
|---|---|---|---|
| 1 | **Door 3** — wire the frozen `norm/v1` NFC + IDNA pipeline (discharge ADR-0006); `provider_canonical_hash` (OQ-2) | ✅ **done** | `internal/hash/hash.go`; 17 tests incl. a **golden HMAC vector**, composed==decomposed, punycode==unicode, trailing-dot/CJK collapse; code-reviewer BLOCK cleared (1 CRITICAL + 2 MAJOR + 3 MINOR all fixed & empirically verified) |
| 2 | **Supabase data model** — §4 tables as forward-only SQL; Door-2 append-only via revoked grants | ✅ **done** | `control-plane/migrations/0001…0005` + README; **applied to real Postgres 16** — all migrations exit 0; append-only PROVEN (as `cp_runtime`: INSERT ok, UPDATE/DELETE → permission denied); CHECK + partial-unique guards bite |
| 3 | **Store (pgx)** — real Postgres client implementing the append-only `store.Store`; in-memory fake | ✅ **done** | `internal/store/{postgres,mem}.go` + `internal/idgen` (UUIDv7). Unit tests (mem: append-only accumulation, composite-key + TTL cache, tri-state lists) + **gated PG integration tests proven against real Postgres 16** (append, cache round-trip, wrong-config miss, NULL-hash invalid_syntax insert). Skips honestly without `TM_TEST_DATABASE_URL` (never faked). |
| 4 | **Cheap layers** — 9 brain-native layers (syntax, typo, role, free/corp, MX/DNS, null-MX, disposable, spam-trap, provider-classify) | ✅ **done** | `internal/layers/{layers,resolver,cheap_layers,dns_layers}.go`. Each emits a `LayerOutput`, none gates alone. DNS layers share ONE memoized lookup via injectable `Resolver`; **harden-toward-unknown proven** (transient DNS → Unknown, only NXDOMAIN → Fail). OQ-7 disposable = Flag (never invalid). Full fixture-resolver tests, no network. |
| 5 | **Policy engine + `cold-email`** — table-driven, action grammar, cheapest-sufficient, confidence baseline | ✅ **done** | `internal/policy/{policy,coldemail}.go`. Open-object definition (OQ-25), `status:reason:stop\|continue` grammar, stop-early, baseline-by-reason confidence, thresholds. `repairComposability` makes invalid+send structurally impossible. Tests prove: invalid⇒never-send (Door 1), stop-early skips DNS, disposable⇒risky+skip (OQ-7), transient⇒unknown, provider⇒risky, clean⇒plausible-not-valid. **Dial note** below. |
| 6 | **Orchestrator + real `/v1/verify` + LOG** — the full request lifecycle; every result logged | ✅ **done** | `internal/orchestrator` + `internal/verdict/assemble.go` (single-source response+LOG) + api wired. **Proven against real PG+DNS**: 4 POSTs → gmail `risky/provider_no_smtp`, mailinator `risky/disposable+skip`, garbage `invalid/invalid_syntax` (NULL hash), NXDOMAIN `invalid/no_mx` — all logged (Door 2), cache re-logs `from_cache=true`, reject not cached. A real UUID bug (req-prefixed request_id rejected by PG) was caught here and regression-pinned. |
| 7 | **Config flags, docs, CI, final review** — every P1 manifest flag wired; PG in CI; final gate | ✅ **done** | `TM_DATABASE_URL` + `TM_CACHE_TTL_SECONDS` wired + in manifest/example-env; config-reference regenerated + fresh; CI runs a real Postgres service + applies migrations so the Door-2 append-only + store tests RUN on every push (not skip); final code-reviewer pass over the whole phase found + fixed one MINOR (MemStore cache-map aliasing — the fake returned/stored `Flags`/`Signals` by reference; PG path unaffected; fixed by copying the maps to match PG's no-aliasing; locked by `TestMemCacheMapsAreCopiedNotAliased`). |

---

## Door-by-door: what "real" means at Phase 1 (§3, PROJECT_RULES)

- **Door 1** (`/v1/verify` shape) — the versioned, additive-only response is served for real by the
  orchestrator (increment 6); the `invalid ⇒ never send` composability invariant is enforced by
  `vocab.ComposabilityOK` + `verdict.Valid()`.
- **Door 2** (append-only LOG) — DB-enforced: `verifications` grants revoke UPDATE/DELETE from the
  runtime role (proven live, increment 2). The raw sidecar is partitioned; retention = DROP PARTITION.
- **Door 3** (hashing) — the frozen `norm/v1+hmac-sha256/v1` scheme is fully wired and golden-vector
  pinned (increment 1). The Phase-0 non-ASCII refusal (ADR-0006) is lifted; NFC/IDNA now normalize.
- **Door 4** (vocab) — the frozen enums live in `internal/vocab`; storage-layer CHECK constraints
  mirror them (proven to reject a bogus value live).
- **Door 5** (node-list + pinned gem) — the node LIST + `Strategy` interface exist from Phase 0; the
  gem-unmodified CI assertion holds. Phase 1 is control-plane-side only (no verifier-node wrapper).

---

## Implementation note: one policy `order` dial refined (not a spec change)

The shipped `cold-email` policy runs **`null_mx` before `mx`**, where §7.2's *illustrative*
`order` lists `mx` first. With `mx` first, the general `mx:fail` gate (→ `no_mx`) fires on a
null-MX domain and **masks** the more specific `null_mx:pass` gate, so every RFC-7505 null-MX
domain would be logged with reason `no_mx` instead of `null_mx`. The policy `order` is an
explicit **DIAL** (config-manifest §1.3: "LOCKED shape / DIAL contents"), so ordering the
most-specific detector first is *implementing* the dial correctly, not redesigning the spec.
Caught by `TestInvalidNeverSaysSend/null_MX`. No frozen door or grammar changed.

## What Phase 1 does NOT build (correct scope — SEAM → stop)

No SMTP, no verifier-node wrapper logic, no queue, no fallback, no flywheel. The cheap layers are
brain-native (OQ-6); the engine stays verifier-node-side and is not consulted in Phase 1. The
cheap-layers-only kill-switch rung (Rung 3) is Phase 1's steady state.
