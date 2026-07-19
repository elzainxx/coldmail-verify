# Docs — living documentation index

The single map of the project's documentation. **Living, not aspirational:** every behavior
change updates the relevant doc/ADR in the same change, and CI fails a behavior change that
doesn't ([`../PROJECT_RULES.md`](../PROJECT_RULES.md) §8). Start with the constitution, then
the specs for depth.

## Read-first

| Doc | What it is |
|---|---|
| [`../PROJECT_RULES.md`](../PROJECT_RULES.md) | **The constitution.** Five doors, first-class `unknown`, two vocabularies, shadow-mode, kill-switch — enough to work without reading all seven specs. |
| [`../README.md`](../README.md) | Repo overview: the two deployables, Door 5, build/run. |
| [`DECISIONS.md`](DECISIONS.md) | Binding cross-phase rulings (DR-1…DR-5) + owner-OQ register (§6) + parked sub-decisions (§5, still open). |
| [`config-reference.md`](config-reference.md) | **Generated** from the manifest — the always-current list of what's toggleable. |

## The seven phase specs (design authorities)

| Phase | Spec | Delivers |
|---|---|---|
| 1 | [`phase-1-foundations-spec.md`](phase-1-foundations-spec.md) | Contracts (the 5 doors), data model, control-plane skeleton, cheap layers, policy engine |
| 2 | [`phase-2-smtp-mocks-spec.md`](phase-2-smtp-mocks-spec.md) | Full SMTP path against mocks; catch-all probe; wrapper endpoints |
| 3 | [`phase-3-first-real-box-spec.md`](phase-3-first-real-box-spec.md) | One real audited verifier-node ("box" in the design docs); the `/audit` gate; selective go-live |
| 4 | [`phase-4-resilience-pool-spec.md`](phase-4-resilience-pool-spec.md) | Health-watcher, sticky routing, rate limiter, node #2 = +1 config line |
| 5 | [`phase-5-queue-spec.md`](phase-5-queue-spec.md) | Async queue, greylist retry, idempotency (CAS), bulk job handle |
| 6 | [`phase-6-fallback-spec.md`](phase-6-fallback-spec.md) | Third-party fallback on residue; anti-laundering; spend breaker |
| 7 | [`phase-7-flywheel-spec.md`](phase-7-flywheel-spec.md) | Close the loop: bounce classifier, recalibration, promotion, weaning |

Plus [`truemail-build-plan.md`](truemail-build-plan.md) (the phased plan) and
`truemail-architecture-detailed.svg` (the architecture diagram).

## Cross-phase revision-pass docs (register of record)

| Doc | What it is |
|---|---|
| [`config-manifest.md`](config-manifest.md) | Exhaustive registry of every toggle/flag/secret/dial (the source `config-reference.md` is generated from). |
| [`kill-switch-ladder.md`](kill-switch-ladder.md) | The degradation hierarchy (full → SMTP-off → fallback-off → cheap-only → paused). |
| [`shadow-mode.md`](shadow-mode.md) | Recommend-only vs auto discipline for data-driven mutators. |
| [`cross-phase-consistency-report.md`](cross-phase-consistency-report.md) | The five cross-phase lenses; origin of the findings/PNs. |
| [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) | What must be true/decided/spiked before each phase's code. |
| [`consolidation-application-report.md`](consolidation-application-report.md) | Audit trail of the accepted-recommendation application pass. |

## ADR log — one-way-door & owner-OQ rulings

See [`adr/README.md`](adr/README.md) for the index and format. ADRs are the record for anything
touching a one-way door or elevating/gating a cross-phase decision.

## Phase-0 artifacts (this scaffolding pass)

| Doc | What it is |
|---|---|
| [`truemail-ground-truth.md`](truemail-ground-truth.md) | Real engine output vs. spec assumptions (source-complete; live-run rows flagged pending). |
| [`test-strategy.md`](test-strategy.md) | The behavioral/integration test discipline tied to named GAPs. |
| [`phase-0-status.md`](phase-0-status.md) | What Phase 0 delivered, what's verified, and what's honestly pending. |
| [`phase-0-signoff-sheet.md`](phase-0-signoff-sheet.md) | The owner sign-off sheet — RESOLVED 2026-07-17 (GT-1…GT-6 accepted, Door-3 fix blessed, LICENSE deferred; recorded in [DECISIONS.md](DECISIONS.md) §6.1). |

## Phase-1 artifacts (foundations build — complete)

| Doc | What it is |
|---|---|
| [`phase-1-status.md`](phase-1-status.md) | Live scorecard of the Phase-1 build against its "done-when" criteria. |
| [`phase-1-to-2-handoff.md`](phase-1-to-2-handoff.md) | The Phase-1→Phase-2 handoff: what P1 locked, what P2 inherits, the `SEAM → P2` inbox, and the open LICENSE item. Readiness pass verified. |
| [`../control-plane/migrations/`](../control-plane/migrations/) | The Supabase data model (§4) as forward-only SQL — the LOG (append-only via revoked grants), cache, lists, nodes, policies, empty P7 seam. See its `README.md`. |

## Phase-2 artifacts (SMTP against mocks — complete)

| Doc | What it is |
|---|---|
| [`phase-2-status.md`](phase-2-status.md) | Live scorecard of the Phase-2 build: the run-first GAP-13 spike, the wrapper endpoints, the SMTP scoring/classification, the three resolved seams, and the 8/8 socket-level proof. |
| [`phase-2-to-3-handoff.md`](phase-2-to-3-handoff.md) | The Phase-2→Phase-3 handoff: what P2 locked/built, what P3 inherits, the `SEAM → P3` inbox (the `/v1/audit` enforcement gate), and the open LICENSE item. |
| [`phase-3-readiness-assessment.md`](phase-3-readiness-assessment.md) | The honest P3 readiness assessment: the three P3 gates (host port25∧PTR∧clean-IP = the wall; GAP-13 2×-connection warmup factor; admin-audit-log before go-live), each ready / not-ready / needs-owner-action. |
| [`adr/ADR-0007-gap13-catchall-session-two-session.md`](adr/ADR-0007-gap13-catchall-session-two-session.md) | The GAP-13 spike ruling: the catch-all probe runs as two sessions (Option B); the gem is never patched (Door 5). |
| [`../verifier-node/truemail-rack/test/p2/`](../verifier-node/truemail-rack/test/p2/) | The socket-level proof harness (real `smtp_mock`/`dns_mock`, Appendix B matrix) + the throwaway test-image Dockerfile. A test harness, not a deployable. |

## Phase-3 artifacts (first real box — local build complete)

| Doc | What it is |
|---|---|
| [`phase-3-status.md`](phase-3-status.md) | Live scorecard of the Phase-3 local build: the `/audit` gate, the GAP-24 four-way invariant, selective routing, warmup, `node_audits` history + the minimal re-audit scheduler, the PENDING-REAL-BOX ledger, and the grants proven live against real Postgres. |
| [`phase-3-cutover-runbook.md`](phase-3-cutover-runbook.md) | The real-box cutover runbook: the V1–V5 vetting checklist, the ordered config-only flip, the PENDING-REAL-BOX proofs (each with its exact command), and the go-live acceptance list. **The system is one config flip + this runbook away from a real box.** |
| [`phase-3-to-4-handoff.md`](phase-3-to-4-handoff.md) | The Phase-3→Phase-4 handoff: what P3 locked/built, what P4 inherits (the `Strategy` seam, the `node_audits` flap history, the warmup counter, the lifecycle machine), the `SEAM → P4` inbox (continuous health-watcher, block-rate quarantine, the pool, rotation), and the cutover gates carried forward. |
| [`phase-4-readiness-assessment.md`](phase-4-readiness-assessment.md) | The P3→P4 readiness pass: Phase 3 re-verified closed (gate green on real Postgres, GAP-24/GAP-15 re-proven, additivity confirmed); PENDING-REAL-BOX honesty audited (one `port25_open` wiring gap flagged); Phase 4 mapped **buildable-now vs box-gated** (~90% local); DR-3 admin-audit-log flagged as the parallel track. **Phase 4 must not block on the VPS.** |
| [`adr/ADR-0009-gap24-egress-ip-invariant-runtime-enforcement.md`](adr/ADR-0009-gap24-egress-ip-invariant-runtime-enforcement.md) | GAP-24 four-way egress-IP invariant runtime-enforced (eligibility gate + re-audit scheduler); `verifier_ip` sourced from the brain registry; the P3 additive surface (one `signals` key, `node_audits`/`node_warmup` tables) — additive against all five doors. |
| [`../control-plane/migrations/0007_node_audits_warmup.sql`](../control-plane/migrations/0007_node_audits_warmup.sql) | `node_audits` (append-only audit history, OQ-P3-5) + `node_warmup` (durable ramp counter, §5.2) + the `nodes` INSERT/UPDATE grant. Append-only posture on `node_audits` proven live. |

## Phase-4 artifacts (resilience & pool — local build complete)

| Doc | What it is |
|---|---|
| [`phase-4-status.md`](phase-4-status.md) | Live scorecard of the Phase-4 local build: sticky-by-domain (HRW) + least-inflight routing, the per-provider rate limiter, the continuous health-watcher, block-rate quarantine + the three guards, the **fleet-wide cascade brake (fail-verified)**, canary recovery, observability, node #2 = +1 config line, and the PENDING-REAL-BOX ledger. Grants proven live on real Postgres. |
| [`phase-4-provisioning-script-design.md`](phase-4-provisioning-script-design.md) | The §6 provisioning-script DESIGN: the ordered, idempotent, fail-closed stage contract (spin → install → PTR → audit → register → warm up), one level more concrete than P3's checklist — design, not runnable code. Resolves the P3 `port25_open` wiring gap (stage 4 = the egress-test writer, DR-3 first slice). |
| [`phase-4-to-5-handoff.md`](phase-4-to-5-handoff.md) | The Phase-4→Phase-5 handoff: what P4 locked/built, what P5 inherits (the `rate_limited` backpressure the queue absorbs, sticky-by-domain setting up domain-batched session reuse, the durable-counter discipline), the `SEAM → P5` inbox (the queue, greylist retry, CAS idempotency), and the cutover gates carried forward (incl. DR-4 queue-plaintext-backup). |
| [`adr/ADR-0010-phase-4-pool-additive-surface.md`](adr/ADR-0010-phase-4-pool-additive-surface.md) | The Phase-4 additive surface: one `signals` value (`rate_limited`), one table (`rate_budgets`), the `recovering` node-lifecycle value (NOT verdict vocab); block-rate quarantine (A1) defaults recommend-only (DR-2); the fleet-wide cascade brake (GAP-28) + canary recovery (GAP-30) built + fail-verified — additive against all five doors. |
| [`../control-plane/migrations/0008_rate_budgets_recovering.sql`](../control-plane/migrations/0008_rate_budgets_recovering.sql) | `rate_budgets` (durable per-(node×provider) token-bucket spend, restart-safe, §4.4) + the `recovering` `nodes.status` value (§5.4/GAP-30). `cp_runtime` SELECT+INSERT+UPDATE, DELETE revoked (counter posture, proven live). |

## Phase-5 artifacts (the queue — local build complete)

| Doc | What it is |
|---|---|
| [`phase-5-status.md`](phase-5-status.md) | Live scorecard of the Phase-5 local build: the durable `work_items` queue, greylist retry/backoff, the **GAP-37 CAS idempotency heart (fail-verified on Mem AND real Postgres)**, group-by-domain pacing, the bulk job lifecycle, the two gates-first (DR-4 encryption + PN-13 per-caller auth), and the PENDING-REAL-BOX/rollout ledger (the un-mockable greylist round-trip, GAP-35). |
| [`phase-5-readiness-assessment.md`](phase-5-readiness-assessment.md) | The P4→P5 readiness pass: Phase 4 re-verified closed, the fleet brake re-verified on all four properties, and Phase 5 mapped fully local-buildable (no VPS) with DR-4 + PN-13 as the two gates to line up first. |
| [`phase-6-readiness-assessment.md`](phase-6-readiness-assessment.md) | The P5→P6 readiness pass: Phase 5 re-verified closed (GAP-37/38 re-fail-verified on real Postgres; DR-4 containment + GAP-36 platform boundary audited **0 findings**; queue-dial drift closed); Phase 6 mapped fully local-buildable against a **mock vendor with the layer DISABLED** — the **DPA gates enablement, not implementation**. Per-gate ready/not-ready: vendor pick + DPA + platform disclosure are owner actions; OQ-P6-3 decided; global-fallback-kill buildable now. |
| [`adr/ADR-0011-phase-5-queue-additive-surface.md`](adr/ADR-0011-phase-5-queue-additive-surface.md) | The Phase-5 additive surface + the two door-touching gate resolutions: **DR-4 = separate-key at-rest encryption** of the queue plaintext (key ≠ pepper); **PN-13 = per-caller keys + rotation**; the **GAP-37** lease-TTL + compare-and-set exactly-once settlement — additive against all five doors, zero new verdict vocab. |
| [`../control-plane/migrations/0009_work_items_jobs.sql`](../control-plane/migrations/0009_work_items_jobs.sql) | `work_items` (durable queue; UNIQUE idempotency_key; `addr_ciphertext` encrypted plaintext; lease `locked_by`/`locked_at`; `verification_id` FK-less) + `jobs` (counts/status/opaque caller_ref) + `work_item_attempts` (append-only transcripts). Partial indexes for the group-by-domain dequeue, the GAP-38 completion query, and the lease-reclaim sweep. cp_runtime SELECT+INSERT+UPDATE, DELETE cp_admin. |
| [`../control-plane/migrations/0010_api_callers.sql`](../control-plane/migrations/0010_api_callers.sql) | `api_callers` (PN-13): per-caller key stored `key_sha256` HASHED, validity window `[not_before, not_after)` for zero-downtime rotation. cp_runtime SELECT-only (hot auth read); issuance/rotation cp_admin (DR-3 admin-audited). |

## Phase-6 artifacts (the third-party fallback — local build complete, layer DISABLED)

| Doc | What it is |
|---|---|
| [`phase-6-status.md`](phase-6-status.md) | Live scorecard of the Phase-6 local build: the external-verifier layer (built DISABLED against a **mock vendor**), residue-only routing (closed 4-class set), the **anti-laundering rule + spend-once (both fail-verified)**, per-vendor budget + circuit-breaker, the global-fallback-kill, the P7-weaning provenance, and the honest **PENDING-REAL-VENDOR** ledger (vendor accuracy is un-mockable, GAP-46/47). |
| [`phase-6-readiness-assessment.md`](phase-6-readiness-assessment.md) | The P5→P6 readiness pass (predecessor): Phase 5 re-verified closed; Phase 6 mapped fully local-buildable against a mock vendor with the layer DISABLED — the **DPA gates enablement, not implementation**. |
| [`adr/ADR-0012-phase-6-external-fallback-additive-surface.md`](adr/ADR-0012-phase-6-external-fallback-additive-surface.md) | The Phase-6 additive surface + the two irreversible correctness surfaces: **anti-laundering** (vendor `valid`→`risky/external_verified` capped, never SMTP-grade — §3.4); **spend-once = the GAP-37 CAS extended to money** (§5.4/GAP-42); OQ-P6-3 asymmetry; **hard-to-enable** (kill-switch AND DPA AND vendor key). Additive against all five doors, zero new vocab. |
| [`../control-plane/migrations/0011_external_calls.sql`](../control-plane/migrations/0011_external_calls.sql) | `external_calls` (P6 §6.1): the append-only external-verifier spend + P7-weaning provenance sidecar. Stores `email_hash` ONLY (never plaintext, §7.3); queryable by `email_hash` for SAR/deletion (PN-10). A queued escalation's row rides the settlement CAS (spend-once, GAP-42). cp_runtime SELECT+INSERT only; DELETE cp_admin. |
| [`phase-7-readiness-assessment.md`](phase-7-readiness-assessment.md) | The P6→P7 readiness pass: Phase 6 re-verified closed (both irreversible surfaces re-fail-verified on real Postgres; every `runExternal` path confirmed capped; a settable-but-ignored `TM_EXTERNAL_CONFIDENCE_CAP` found + removed). Phase 7 mapped **buildable-now against a simulated feed vs needs-the-real-sender**. **ADR-0001 (outcome-feed authentication) is the hard pre-P7 door** — principle DECIDED (authenticated + malicious-sender threat model), mechanism deliberately owner/P7-time; the sender ("Feature 2") does not exist yet. OQ-P7-2 (TTL) ruled+recorded; OQ-P7-8 (disclosure) ruled (platform-gated); F-17/PN-11 (≥N gate) recorded as a build-time DIAL. |

## How the living-docs system stays honest (enforced, not asked)

- **Behavior change ⇒ doc/ADR change**, in the same commit — [`../scripts/check-docs-updated.sh`](../scripts/check-docs-updated.sh) fails CI otherwise.
- **One-way-door / owner-OQ decision ⇒ an ADR** in [`adr/`](adr/).
- **Manifest change ⇒ regenerate** [`config-reference.md`](config-reference.md) — CI checks it's current.
- **Every change ⇒ the code-reviewer gate** (CI), docs included.
