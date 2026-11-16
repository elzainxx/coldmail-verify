# Docs — the one-page map

Living documentation for module 1 (`coldmail-verify`: the Go control-plane and the Ruby verifier-node). **Living, not
aspirational:** every behaviour change updates the relevant doc or ADR in the same change, and CI fails a behaviour
change that does not ([`../PROJECT_RULES.md`](../PROJECT_RULES.md) §8). Every count and name in the living set was
checked against the code on 2026-11-13.

## Start here — by what you are doing

| If you are… | Read | In that order |
|---|---|---|
| **a developer / maintainer** | [`architecture.md`](architecture.md) → [`install-and-deploy.md`](install-and-deploy.md) | what it is and how it is built; then how to run, test and review it |
| **an operator** | [`operator-runbook.md`](operator-runbook.md) → [`go-live-checklist.md`](go-live-checklist.md) | pulling kill-switches, reading alerts, maintenance, rollback; then the status-marked path to live |
| **a caller / integrator** | [`api-contract.md`](api-contract.md) | the six routes, auth, errors — and Appendix A, what each verdict field means |
| **reviewing the security model** | [`security-posture.md`](security-posture.md) | what protects what, and the honest list of what is not built |
| **deciding what to do next** | [`future-work.md`](future-work.md) → [`open-decisions.md`](open-decisions.md) | everything not done yet; then the rulings of record (zero open) |

## The living set (all in `Docs/`)

| File | What it is |
|---|---|
| [`architecture.md`](architecture.md) | The system: two deployables, the request path, the packages, the data model, the five one-way doors, the nine irreversible surfaces and their fail-verify tests. |
| [`install-and-deploy.md`](install-and-deploy.md) | From a clean checkout to a running system: local setup, the capability tiers and their env vars, the real-box cutover, live-HTTP verification. |
| [`operator-runbook.md`](operator-runbook.md) | Running it: the kill-switch ladder, shadow-mode promotion, health and recovery, alerts, partition maintenance, schema rollback and restore. |
| [`config-reference.md`](config-reference.md) | **Generated** from [`config-manifest.md`](config-manifest.md) — every flag, dial and secret, its default and its blast radius. CI fails if it is stale. |
| [`vendor-zerobounce.md`](vendor-zerobounce.md) | The ZeroBounce external-fallback adapter (ships **disabled**): the (status, sub_status) → M1 mapping table with its evidence, "not honestly mappable", what is UNVERIFIED, the gating order, the four dials, the stand-in and the enablement runbook. |
| [`bulk-item-identity.md`](bulk-item-identity.md) | Bulk jobs, row by row (owner rulings A1/A2, migration `0018`): the `items` request shape and the `item_id` rules, `ordinal` / `duplicate_of` on every result, results in ordinal order, malformed rows logged, dedup unique per job (never across jobs or tenants), what is stored, and the tests that prove it. |
| [`failure-origin.md`](failure-origin.md) | `signals.failure_origin` (`own` / `remote` / `undetermined`) on every `unknown` result: who failed when M1 could not answer, the one place it is computed, the mapping table (one row per code path that yields `unknown`), the cases that cannot be classified honestly, and the tests that hold it. |
| [`api-contract.md`](api-contract.md) | The served HTTP surface, verified against the code on every `go test`; Appendix A reads a verdict in plain language. |
| [`security-posture.md`](security-posture.md) | Assets, secrets and custody, data protection, authentication, the verifier node's four layered controls, supply chain, and the unbuilt-controls list. |
| [`go-live-checklist.md`](go-live-checklist.md) | One ordered path to live; 44 items, each ✅ done (with evidence), ⏳ owner-only, or 🔧 not built yet. |
| [`future-work.md`](future-work.md) | The 20 real-world-gated rows, the in-repo build tasks (FW-1…5), the sub-decisions that still need an owner (FW-6…9), and what has closed since. |
| [`open-decisions.md`](open-decisions.md) | Rulings of record: DR-1…DR-5, the owner-OQ register, the post-arc rulings (ADR-0015…0021). **Zero open items.** |
| [`test-strategy.md`](test-strategy.md) | The test discipline and the CI shape; rule 7 of the enumeration gate requires every `scripts/check-*.sh` to be named here. |
| [`adr/`](adr/) | 21 architecture decision records (ADR-0001…0021; next is 0022). Anything that touches a one-way door, or gates a cross-phase decision, is an ADR. Index: [`adr/README.md`](adr/README.md). |
| `truemail-architecture-detailed.svg` | The architecture diagram (a design-time picture; [`architecture.md`](architecture.md) is current). |
| [`../PROJECT_RULES.md`](../PROJECT_RULES.md) | **The constitution** — five doors, first-class `unknown`, two vocabularies, shadow-mode, the kill-switch ladder. Stays at the repo root. |

Code-adjacent docs, kept beside the code they describe: [`../control-plane/migrations/README.md`](../control-plane/migrations/README.md)
(migration conventions), [`../verifier-node/truemail-rack/FORK.md`](../verifier-node/truemail-rack/FORK.md) (every change to the fork,
18 rows), [`../control-plane/config/secrets.example.env`](../control-plane/config/secrets.example.env) (the secret and env scaffold).

## [`history/`](history/) — provenance, frozen

38 files that explain how the system came to be. They are **records of what was true when written**: cite them, do not
edit them (only path-only link fixes), and read a living doc first when the two differ.

| Group | Files |
|---|---|
| The seven phase specs (design authorities) | [`phase-1-foundations-spec`](history/phase-1-foundations-spec.md) · [`phase-2-smtp-mocks-spec`](history/phase-2-smtp-mocks-spec.md) · [`phase-3-first-real-box-spec`](history/phase-3-first-real-box-spec.md) · [`phase-4-resilience-pool-spec`](history/phase-4-resilience-pool-spec.md) · [`phase-5-queue-spec`](history/phase-5-queue-spec.md) · [`phase-6-fallback-spec`](history/phase-6-fallback-spec.md) · [`phase-7-flywheel-spec`](history/phase-7-flywheel-spec.md) |
| Phase status scorecards (0–7) | [`phase-0-status`](history/phase-0-status.md) … [`phase-7-status`](history/phase-7-status.md), and the owner sign-off [`phase-0-signoff-sheet`](history/phase-0-signoff-sheet.md) |
| Handoffs and readiness assessments | `phase-N-to-M-handoff` (1→2 … 4→5) · `phase-N-readiness-assessment` (3 … 7) |
| Cross-phase revision pass | [`cross-phase-consistency-report`](history/cross-phase-consistency-report.md) · [`consolidation-application-report`](history/consolidation-application-report.md) · [`pre-takeoff-checklist`](history/pre-takeoff-checklist.md) · [`kill-switch-ladder`](history/kill-switch-ladder.md) · [`shadow-mode`](history/shadow-mode.md) |
| The arc's close; the build plan; engine ground truth | [`arc-closing`](history/arc-closing.md) · [`truemail-build-plan`](history/truemail-build-plan.md) · [`truemail-ground-truth`](history/truemail-ground-truth.md) |
| Sources merged into a living doc (kept whole) | [`TEST-PLAN`](history/TEST-PLAN.md) (→ install-and-deploy §4) · [`USER_GUIDE`](history/USER_GUIDE.md) (→ api-contract Appendix A) · [`go-live-rollback-posture`](history/go-live-rollback-posture.md) (→ operator-runbook §9) · [`phase-3-cutover-runbook`](history/phase-3-cutover-runbook.md) and [`phase-4-provisioning-script-design`](history/phase-4-provisioning-script-design.md) (→ install-and-deploy §3.2) |

Renamed in place when this layout was adopted (git history follows with `--follow`): `DECISIONS` → `open-decisions`,
`pending-ledger` → `future-work`, `OPERATIONS` → `operator-runbook`, `ONBOARDING` → `install-and-deploy`,
`go-live-runbook` → `go-live-checklist`.

## How the living docs stay honest (enforced, not asked)

- **Behaviour change ⇒ doc/ADR change**, in the same commit — [`../scripts/check-docs-updated.sh`](../scripts/check-docs-updated.sh) fails CI otherwise.
- **One-way-door or owner-OQ decision ⇒ an ADR** in [`adr/`](adr/).
- **Manifest change ⇒ regenerate** [`config-reference.md`](config-reference.md) — [`../scripts/check-config-reference-fresh.sh`](../scripts/check-config-reference-fresh.sh) checks it is current.
- **The route table is the served set** — `TestRoutes_*` reads [`api-contract.md`](api-contract.md).
- **Every gate is named in [`test-strategy.md`](test-strategy.md)** — [`../scripts/check-test-enumeration.sh`](../scripts/check-test-enumeration.sh) rule 7.
- **Every change ⇒ the code-reviewer gate** (CI), docs included.
