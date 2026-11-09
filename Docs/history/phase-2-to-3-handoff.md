# Phase 2 → Phase 3 — handoff

**Date:** 2026-07-17 · **From:** Phase 2 (SMTP against mocks), complete · **To:** Phase 3 (first real box)
**Purpose:** one page an engineer reads before Phase 3 — what P2 **locked/built**, what P3 **inherits**,
the **`SEAM → P3`** items, and the still-open owner item (LICENSE).
**Authorities:** [`phase-2-smtp-mocks-spec.md`](phase-2-smtp-mocks-spec.md) (built), [`phase-3-first-real-box-spec.md`](phase-3-first-real-box-spec.md) (the build), [`phase-2-status.md`](phase-2-status.md) (what shipped), [`truemail-ground-truth.md`](truemail-ground-truth.md), [`DECISIONS.md`](../open-decisions.md) + [`adr/`](../adr).

---

## 1. Phase 2 is genuinely closed (verified)

| Gate | Result |
|---|---|
| `go build` / `go vet` / `go test` (control-plane) | ✅ all pass (incl. gated PG tests against real Postgres 16) |
| Fork RSpec (stock + `/v1` wrapper specs) | ✅ **71 examples, 0 failures** |
| Socket-level proof (real `smtp_mock`/`dns_mock`, Appendix B) | ✅ **8/8 rows** (`verifier-node/truemail-rack/test/p2/`) |
| Door-5 gem-unmodified (host + Docker) | ✅ `truemail (3.3.1)` resolved, no vendored source, ADR-0003 agrees |
| Door-2 append-only + catch-all grants on real PG | ✅ LOG UPDATE/DELETE denied; catch-all INSERT/UPDATE ok, DELETE denied (migration 0006) |
| config-reference freshness / docs-updated | ✅ regenerated from the manifest |

**The GAP-13 spike ran first** (mandated): the pinned gem exposes no reusable same-session →
**Option B (two sessions), gem never patched** ([ADR-0007](../adr/ADR-0007-gap13-catchall-session-two-session.md)).

---

## 2. What Phase 2 LOCKED / built (additive-only against the five frozen doors)

- **The `/v1` wrapper endpoints** (`verify, audit, catchall/:domain, stats, config, config/reload`) —
  in the forked rack, never the gem. Mounted via `config.ru` URLMap; stock endpoints byte-identical.
- **The catch-all probe** — two `Truemail.validate` calls (Option B); `valid` minted **only** on
  target-250 + probe-explicitly-rejected (§5.5 row 4).
- **The brain SMTP surface** (`control-plane/internal/smtp`) — transcript parse (GAP-4), 5xx
  classification (§6.1, incl. the real 421-vs-554 reconciliation), the §7.2 scoring map, DNS
  reconciliation (§6.6), the settled-only cache predicate (§6.5).
- **The orchestrator SMTP stage** (§7.1) — runs on the non-terminal residue only; node failure →
  unknown (never fabricated); raw blob → `verification_raw` sidecar; catch-all → `catchall_domains`.
- **The three inherited seams RESOLVED:** config_fingerprint over `/v1/config` (GAP-3), smtp_code
  parsing (GAP-4), DNS reconciliation (GAP-10).
- **Zero new vocab values** (Door 4); the `cold-email` policy gained SMTP as DATA; new `signals`
  keys only (Door 1); transcript rides the existing sidecar (Door 2); hashing untouched (Door 3).

---

## 3. What Phase 3 INHERITS (built and ready to build on)

- **The config-flip is proven config-only** — mock→real changes only the node `base_url` +
  engine `dns`/`smtp_port` (applied via `/v1/config/reload`); the brain code is identical. P3 flips
  the node to real; the policy is stable across the flip (OQ-P2-8).
- **`/v1/audit` contract built + read/logged** — `nodes.audit_status`/`last_audit_at`/`port25_open`
  reserved. P3 builds the **enforcement gate** (no-SMTP-until-green) on top; the shape is ready.
- **`GET /v1/config`** — the effective-config exposure P3's audit/eligibility can also read.
- **Harden-toward-unknown everywhere** — a red/dead/blocked box degrades to `unknown`, never invalid;
  no-eligible-node degrades to the cheap verdict (§7.1) — the exact P3 SMTP-eligibility posture.

---

## 4. `SEAM → P3` items (Phase 3's explicit inbox)

| Seam | Where P2 left it | What P3 does |
|---|---|---|
| **`/v1/audit` enforcement** | contract built; read/logged only (OQ-P2-9) | build the **gate**: `:smtp`-eligible iff `audit green AND port25_open` (GAP-15); real IP/PTR/SPF; the audit must run on the PUBLIC egress IP ([[truemail-audit-green-not-ready]]). |
| **`port25_open` egress test** | column reserved, unset | the egress smoke-test that sets it (distinct from the audit — GAP-15/24). |
| **Real-box behavioral fidelity** | mocks prove code-path identity, NOT provider behavior (§3.5 honest limit) | one real audited box; the first real Gmail/corporate SMTP responses; catch-all against a real non-catch-all peer (retires the GAP-14 seeded-probe test artifact). |
| **GAP-13 spike (pre-provisioning)** | Option B chosen against mocks | on a real IP the same-session-vs-two-session choice is a **reputation-cost** decision ([[truemail-gap13-spike-before-real-box]]); Option B stands unless real-IP evidence says otherwise — still never a gem patch. |
| **Warmup / HELO rotation** | single fixed verifier identity; noted P4 | real-IP warmup ramp + HELO/verifier rotation (reputation) — P3/P4. |

**Deferred past P3 (do not build in P3):** the queue/greylist-retry loop (P5 — requeue *shape* only
exists), health-watcher/rate-limit/pool + per-provider pacing (P4), fallback (P6), flywheel (P7).

---

## 5. `SEAM → P4/P5` carried forward (designed, not built)

- **Greylist requeue shape** — `signals.retry_eligible` + `retry_after_hint_s` are present on a
  greylist/timeout unknown; **nothing retries them in P2** (P5 queue). GAP-12: the retry-then-accept
  round-trip is un-mockable → **rollout-only** (GAP-35), never faked green.
- **`/v1/stats` counter feed** — raw counters emitted; the health-watcher/alerting is P4.
- **Re-probe cadence** — `catchall_domains.expires_at` written; the loop that reads it and re-probes
  is unowned (FLAG-C, [`DECISIONS.md`](../open-decisions.md) §5) — P4 health-watcher or P5 queue, owner's call.
- **Store test-double JSON fidelity** — P2 added a numeric signal (`retry_after_hint_s`, int). PG
  round-trips signals through JSON (int→float64); the mem-fake preserves Go types. Tests must not
  assert on signal value *types* in mem-fake tests (note in `postgres.go`). Live now.

---

## 6. The one open owner item (does NOT block Phase 3)

- **LICENSE** — still deferred (DECISIONS.md §6.1). Third-party MIT respected in [`NOTICE`](../../NOTICE);
  the project's own license is deliberately unasserted. Revisit before external distribution.

---

## 7. Verdict

**Phase 2 is genuinely complete.** The full SMTP path runs end-to-end against mocks; the catch-all
probe and 5xx/greylist classification behave per spec; the three inherited seams are resolved;
mock→real is proven config-only; real tests pass (Go + RSpec + an 8/8 socket-level proof); and none
of it required a real port-25 box. All five doors are intact and CI-asserted. **Phase 3's first move
is the `/v1/audit` enforcement gate on a real, egress-audited box** — the contract is already built.
