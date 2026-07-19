# Phase 4 readiness assessment — buildable-now vs box-gated

**Date:** 2026-07-18 · **By:** P3→P4 readiness pass · **Verdict up front: Phase 4 is READY to build,
and it should NOT block on a VPS.** Almost the entire phase — sticky routing, the rate limiter, the
health-watcher, the block-rate control loop, the state machine, observability, the provisioning-script
*design*, and the "node #2 = +1 config line" proof — is **buildable locally against simulated nodes**,
exactly as Phase 3 was. Only the *final real-IP proofs* (real per-provider blocks, real 2-box
reputation behavior) are box-gated, and they gate **acceptance**, not the build.
**Authorities:** [`phase-4-resilience-pool-spec.md`](phase-4-resilience-pool-spec.md) (the build), [`phase-3-status.md`](phase-3-status.md), [`phase-3-cutover-runbook.md`](phase-3-cutover-runbook.md), [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) §3.5/§3.6, [`DECISIONS.md`](DECISIONS.md) DR-2/DR-3, [ADR-0009](adr/ADR-0009-gap24-egress-ip-invariant-runtime-enforcement.md).

---

## 0. Phase 3 is genuinely closed (this pass re-verified it)

| Check | Result |
|---|---|
| `go build` / `go vet` / `go test` (14 packages) | ✅ all pass |
| PG-gated store tests vs **real Postgres 16** (migration 0007 applied) | ✅ **9/9** (incl. node state/audit, history accumulation, warmup atomicity, both PG↔Mem parity tests) |
| Append-only grants live as `cp_runtime` | ✅ DELETE **denied** on `verifications`, `node_audits`, `node_warmup`, `nodes`; INSERT/UPDATE allowed where designed |
| **GAP-24** (egress≠audit-IP) re-proven | ✅ neutering the conjunct fires **3 tests** (audit rule, router, orchestrator) — restored green |
| **GAP-15** (audit-green but port25-closed) re-proven | ✅ neutering the conjunct fires **3 tests** (audit rule, router) — restored green |
| Door 5 gem-unmodified assertion | ✅ `truemail` pinned exactly 3.3.1, no vendored engine source, ADR-0003 agrees |
| `verifier-node/` changed since Phase 2 (`d1184c4`) | ✅ **empty diff** — zero verifier-node change (P3 was brain + infra only) |
| Additivity (Door 1/2/3/4/5) | ✅ 1 signals key, 2 new tables, reserved columns populated, `vocab.go`/`hash/` byte-identical, gem byte-identical — **no frozen door moved** |
| docs-updated / config-reference fresh | ✅ both pass |

**The two irreversible surfaces (GAP-24, GAP-15) still hold** — enforced in the pure rule
(`audit.Eligibility`) and the router (`SMTPRoute`); GAP-24 additionally has a dedicated orchestrator
degrade test, while GAP-15's orchestrator coverage is transitive (a port25-closed node degrades
through the identical `audit_red` path proven by `TestSMTPStage_RedNode_DegradesToCheapNotInvalid`).
Phase 3 is genuinely complete.

---

## 1. PENDING-REAL-BOX honesty — audited, and one wiring gap flagged loudly

**Every real-IP-dependent check is honestly marked pending with an exact command, and NONE is faked
green.** The eligibility gate **fails closed**: in the local build `port25_open=false` (hardcoded at
boot) and `TM_NODE_PUBLIC_EGRESS_IP` is empty, so **no node is ever `:smtp`-eligible** — the honest
state for a box with no real egress-25 or PTR. Verified in code: the only production path that can
**set `port25_open` true** does not exist — boot registration writes a literal `false`, and the
re-audit scheduler only *reads it back and rewrites it unchanged* (`scheduler.go` preserves, never
introduces, `true`); every `Port25Open:true`/seeded egress IP lives in **test files** (the simulated
audit surface).

| Real-IP check | Faked green? | Exact verification command (recorded) |
|---|---|---|
| Outbound port-25 works (GAP-15) | **No** — `port25_open=false` locally → ineligible | `nc -vz <external-mx> 25` / `swaks --server <mx>:25 --quit-after CONNECT` → banner |
| Real PTR / rDNS | **No** — proven vs `dns_mock`+sim; real is deferred | `dig -x <public-egress-ip> +short` == verifier FQDN; then `GET /v1/audit`→`warnings:{}` |
| Egress-IP == audited-IP (GAP-24) | **No** — four-way cross-check built + fail-verified | `curl ifconfig.me` == `dig +short <fqdn>` == `dig -x` PTR == `host_audit.current_host_ip` == `TM_NODE_PUBLIC_EGRESS_IP` |
| Clean IP | **No** — procurement, pre-warmup | Spamhaus/passive check before warmup |
| SPF published (GAP-17) | **No** — out-of-band, not audit-gating | `dig TXT <verifier-domain>` shows SPF authorizing the egress IP |
| Real greylist round-trip | **No** — rollout-only (un-mockable, GAP-35) | observe real 4xx→retry (P5 retries; P3 marks eligible only) |

**⚠ Wiring gap flagged (honest, not faked — but not yet operator-executable):** the cutover step
"set `port25_open=true`" names an internal function (`UpsertNodeState`), but **there is no
operator/env/admin path to flip `port25_open` in a running system** — no `TM_NODE_PORT25_OPEN` env,
no admin endpoint, no egress-test writer. So today a node **cannot be made `:smtp`-eligible without a
`cp_admin` DB write** (`UPDATE nodes SET port25_open=true`). This is honest (the gate fails closed,
nothing is faked), but the cutover step is **not mechanically executable as written**. The runbook now
says so explicitly (§4 step 1). **This is not a Phase-3 regression — it is the natural first slice of
the DR-3 admin-audit-log gate** (flipping `port25_open` is exactly the kind of manual control-plane
mutation DR-3 requires be audited with per-actor identity). Recorded as parallel work below, not built
here (building it is a feature; this is a review pass).

**The cutover runbook is a complete, mechanical path:** vet (V1–V5) → provision DNS/PTR/SPF → deploy
the P2 fork byte-identical → the ordered config-only flip (egress-test → reload → set descriptor →
audit-green → warmup-low → confirm `verifier_ip`) → the go-live acceptance proofs → the
what-doesn't-change verification. Every step has its command; the order is safety-critical (audit
before traffic); every step is reversible by config. The one gap above is the only non-executable step.

---

## 2. What Phase 4 builds, mapped: BUILDABLE-NOW vs BOX-GATED

Phase 4 is **distributed-system behavior** — and, like Phase 3, nearly all of it is provable against
**simulated/mock-backed nodes with a controllable verdict stream** (the P3 `SimAuditSurface` + the
MemStore + fake node clients already demonstrate the pattern). The genuinely box-gated part is small
and gates **acceptance**, not the build.

### 2.1 Buildable NOW (locally, against simulated nodes — the bulk of the phase)

| Phase-4 component (spec §) | Why buildable locally | How it's proven without real boxes |
|---|---|---|
| **Sticky-by-domain + least-inflight strategies** (§3) | Pure functions behind the unchanged P1 `Strategy` interface; HRW is a hash of `(domain, eligible-set)` — no network | Unit tests: a domain sticks to one node; N=2 splits ~balanced; on membership change only affected domains rehash (HRW minimal reshuffle); sticky-first/least-inflight-fallback ordering |
| **The eligibility filter extension** (§3.3) | Adds conjuncts (`not rate-capped`, `warmup headroom`) to the P3 filter already built + tested | Same pattern as the P3 gate tests (seeded `NodeHealth` + rate/warmup state) |
| **Per-provider rate limiter + jitter** (§4) | Concurrency cap + rate cap + jitter are brain-side admission logic; **durable budget in Supabase** mirrors the P3 `node_warmup` discipline already built | PG-gated tests (real Postgres) for durable budget: survives a "restart" (new store handle) without refilling; in-flight ephemeral resets to zero; token-bucket refill-from-`last_refill_at` |
| **In-flight accounting (ephemeral)** (§4.3) | Per-process counters — correct-at-zero post-restart by design | In-memory counter tests |
| **The health-watcher control loop** (§5) | Reads `/v1/audit`+`/health`+block-rate; block-rate is a **LOG query** (`GROUP BY verifier_ip`) — all feedable from a seeded LOG + the sim audit surface | The whole control loop tested against a **controllable verdict stream**: seed `verifications` rows with chosen `verifier_ip`/`status` → assert quarantine/recover transitions |
| **Block-rate quarantine + the three guards** (§5.3) | Sample-size gate, hysteresis, fleet-wide brake are pure decision logic over the block-rate query | The GAP-27 (small-sample), GAP-28 (fleet-wide cascade), hysteresis tests — **all simulatable** (Appendix B rows 4,5,6 explicitly note "controllable verdict stream") |
| **The `recovering` canary sub-state + warmup reset** (§5.4, GAP-30) | State-machine logic + the durable warmup-counter reset (P3 counter exists) | Transition tests: audit-green quarantined node → recovering → canary clean → active with warmup reset |
| **Failure isolation via `verifier_ip`** (§5.6) | The `GROUP BY verifier_ip` query — the P3 invariant that makes it measurable is **built + fail-verified** | Seed two nodes' LOG rows; assert dirty node's block-rate rises without moving the clean node's |
| **`smtp_skipped_reason:"rate_limited"`** (§4.6) | One additive enum value on the existing open P3 signal key | Orchestrator test: saturated cap → cheap degrade, non-cacheable, never invalid/queued (mirrors the P3 `warmup_cap` test) |
| **The provisioning script** (§6) | The spec explicitly says **design, not runnable code** ("ordered stage design + contract", one level more concrete than P3's checklist) | It's a document + gated-stage contract; no box needed to write it |
| **Observability metrics + alerts** (§8) | All derived from **existing LOG columns** (no schema change) + watcher/limiter counters | Metric-derivation tests over a seeded LOG; alert-threshold logic unit-tested |
| **Node #2 = +1 config line (the Door-5 proof)** (§7) | Two **simulated** nodes in the config-list prove the router/watcher/limiter are N-agnostic — a **config-only diff, zero brain code change** | Same as P3's "zero brain code diff mock→real" proof, now "zero brain code diff N=1→N=2" against mock-backed nodes |

**This is ~90% of the phase.** It is buildable and test-provable locally today, using exactly the
simulated-substrate discipline Phase 3 established (seed the store, drive the sim audit surface, feed
a controllable verdict stream). The spec's own Appendix B says several rows "are simulatable against
P2/P3 mock-backed nodes."

### 2.2 BOX-GATED (needs real boxes — gates ACCEPTANCE, not the build)

| Item | Why it needs real boxes | Gates |
|---|---|---|
| **Real per-provider block behavior** | Only real MX greylist/tarpit/block a real IP; `smtp_mock` can't reproduce reputation dynamics (P2 §3.5 honest limit, GAP-35) | Final acceptance of the block-rate quarantine loop against *real* blocks |
| **2-box reputation isolation in the wild** | One dirty real IP vs one clean real IP — the failure-isolation guarantee's real-world proof | Acceptance criterion 6 (§13) |
| **The `valid`-path + greylist round-trip at N>1** | Inherited P3 real-box items, now across a pool | Rollout observation |
| **Node #2 as a *real* second box** | The config-line proof is local; a *real* #2 needs the §6 provisioning (a second host on the narrow port-25∧PTR market) | Acceptance of "traffic re-routes to a real peer" |

**Crucially: these gate the phase's *acceptance demo*, not its construction.** The build proceeds
locally to completion; the real-box proofs are the same "flip a config value + observe" cutover Phase 3
already made mechanical — now for a second node. **Phase 4 must NOT wait for a VPS to start.**

---

## 3. The three cutover gates — owner actions, and what runs in parallel

These gate the **real-box go-live**, not the Phase-4 build. Two are procurement/owner; one is
buildable-now parallel work.

| Gate | State | Blocks | Runs in parallel with P4 build? |
|---|---|---|---|
| **1. Host** (port25 ∧ PTR ∧ clean ∧ dedicated ∧ egress==PTR'd IP) | 🔴 owner procurement — **the wall** | Real-box **acceptance** of P3 *and* P4 (needs ≥1 real box; the P4 2-node demo needs 2) | Yes — the P4 build doesn't need it; procurement can proceed alongside. Phase 4 §6 turns the P3 V1–V5 vetting into a repeatable script *design* (buildable now). |
| **2. GAP-13 / warmup-connection sizing** | 🟡 known input | nothing to *start*; shapes the warmup + rate-limiter caps | Yes — the rate limiter is built to size caps against **connections** (catch-all ≈ 2, ADR-0007); the number folds in at go-live. |
| **3. Admin identity + control-plane audit log (DR-3)** | 🔴 required before **P3/P4 go-live**; **buildable now** | Real go-live of any manual control action (node disable, quarantine confirm, **`port25_open` flip**, shadow-mode promotion) | **Yes — this is the parallel work to prioritize.** It is buildable now against the existing append-only pattern (the `node_audits` posture), gates go-live, and its **first concrete slice is the `port25_open` egress-test-writer + its admin path** (the §1 wiring gap). Building it unblocks the cutover step that is currently not operator-executable. |

**DR-3 flagged as the parallel track:** per [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md)
§3.6 and DECISIONS DR-3, every manual control-plane action must write to an append-only admin-audit
log with per-actor identity **before P3 go-live**. It is **not built** (this was a review pass). It
is the one gate that is buildable now, gates real go-live, and directly resolves the `port25_open`
wiring gap — so it is the highest-value parallel work to run alongside the Phase-4 build.

**Also note (DR-2 / pre-takeoff §3.5, a P4 build-time gate, not a cutover gate):** Phase 4 ships the
**first data-driven mutator, A1 (block-rate quarantine)** — which **defaults to `recommend-only`**
(compute + alert, never auto-quarantine until a human promotes it). The **fresh-system regression**
(a fresh system performs *zero* live mutation at mode-default) must be wired as a regression test when
A1 ships. This is a Phase-4 build obligation, buildable locally, flagged so it isn't missed.

---

## 4. Rules honored in this pass

- **Additive-only; no door mutated** — confirmed on every axis (§0); `vocab.go`/`hash/`/gem
  byte-identical, `verifications`/`cache` schema untouched.
- **Nothing in DECISIONS.md re-decided** — DR-2/DR-3 are read and honored, not re-opened; no new
  ruling made. The `port25_open` finding is *flagged*, not resolved by a new decision.
- **No new features built** — this pass re-ran the gate, re-proved the invariants, audited honesty,
  and documented findings + this assessment. The only edits are documentation (the runbook wiring-gap
  flag + this assessment).

---

## 5. Summary

| Question | Answer |
|---|---|
| Is Phase 3 genuinely complete? | **Yes** — gate green on real Postgres, GAP-24/GAP-15 re-proven at 3 layers, additivity confirmed, no door moved. |
| Is the deferred real-box work honestly tracked? | **Yes** — every real-IP check is PENDING-REAL-BOX with an exact command, none faked green (the gate fails closed). **One wiring gap flagged:** no operator path to flip `port25_open` yet — honest, but not executable; it's the first slice of DR-3. |
| Is Phase 4 ready to build? | **Yes.** ~90% is buildable locally against simulated nodes (sticky routing, rate limiter, health-watcher, block-rate loop + guards, canary recovery, observability, provisioning-script design, the N=1→N=2 config-only proof). |
| What's box-gated? | Only the **real-IP acceptance proofs** (real per-provider blocks, real 2-box isolation) — they gate the acceptance demo, not construction. |
| Should Phase 4 block on the VPS? | **No.** Build locally to completion; the real-box proofs are a config-flip cutover away, same as Phase 3. |
| Parallel owner work? | **DR-3 admin-audit-log** (buildable now, gates go-live, resolves the `port25_open` gap) + host procurement (the wall) + fold GAP-13 connection-cost into cap sizing. |

**Bottom line:** Phase 3 is closed and honest. Phase 4 is design-complete and locally buildable —
**start it now against simulated nodes**; do not wait for a host. Run the **DR-3 admin-audit-log** in
parallel (it gates real go-live and unblocks the one non-executable cutover step). The host remains
the procurement wall for *acceptance*, not for the build.

*Also still open (unchanged, non-blocking): LICENSE (DECISIONS §6.1 — pick before external distribution). **[RESOLVED 2026-07-19 at the arc closeout: proprietary — see DECISIONS §6.1 + top-level LICENSE.]***
