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
| [`adr/ADR-0007-gap13-catchall-session-two-session.md`](adr/ADR-0007-gap13-catchall-session-two-session.md) | The GAP-13 spike ruling: the catch-all probe runs as two sessions (Option B); the gem is never patched (Door 5). |
| [`../verifier-node/truemail-rack/test/p2/`](../verifier-node/truemail-rack/test/p2/) | The socket-level proof harness (real `smtp_mock`/`dns_mock`, Appendix B matrix) + the throwaway test-image Dockerfile. A test harness, not a deployable. |

## How the living-docs system stays honest (enforced, not asked)

- **Behavior change ⇒ doc/ADR change**, in the same commit — [`../scripts/check-docs-updated.sh`](../scripts/check-docs-updated.sh) fails CI otherwise.
- **One-way-door / owner-OQ decision ⇒ an ADR** in [`adr/`](adr/).
- **Manifest change ⇒ regenerate** [`config-reference.md`](config-reference.md) — CI checks it's current.
- **Every change ⇒ the code-reviewer gate** (CI), docs included.
