# Open decisions — Truemail (rulings of record) · **0 open**

> **OWNER: MODULE 1 (`coldmail-verify`).** Every ruling below is module 1's. Modules 2 and 3 (`coldmail-send-managed`, `coldmail-send-byo`) rule in their own `Docs/open-decisions.md` files and are not bound by anything here — and note that module 3's repo contains TWO files named `open-decisions.md` (its own living one, and module 2's carried copy under `Docs/history/`), each now carrying its own ownership header. Module 1's companion register of everything not done yet — real-world-gated work, in-repo build tasks, and the sub-decisions that still need an owner — is [`future-work.md`](future-work.md).

**Status:** LIVE — the binding decision log for the Truemail control-plane. Rulings here are for-build.
**Open items in this file: 0** (verified 2026-11-13 against the code and migrations 0001–0015). Every item that used to be parked here (old §5) now has a disposition in §5: **closed** with evidence, or **moved** to [`future-work.md`](future-work.md) §5 with the owner decision it needs named — nothing is decided here that was not already ruled, and nothing is left open here.
(Deployable names: the design docs' "brain" is the `control-plane`, "box" is the `verifier-node` — [ADR-0005](adr/ADR-0005-deployable-naming.md). Ruling text below that quotes the consistency report keeps its original "brain"/"box" vocabulary.)
**Scope of this file:** owner rulings that are *cross-phase* or that *elevate/gate* something a single phase couldn't decide alone. Per-phase sign-offs remain in each spec's Appendix D; this file records only what the phases could not.
**Companions:** [`adr/`](adr/) (door-touching decisions) · [`history/consolidation-application-report.md`](history/consolidation-application-report.md) · [`history/cross-phase-consistency-report.md`](history/cross-phase-consistency-report.md) · [`history/pre-takeoff-checklist.md`](history/pre-takeoff-checklist.md) · [`history/shadow-mode.md`](history/shadow-mode.md) · [`history/kill-switch-ladder.md`](history/kill-switch-ladder.md) · [`config-manifest.md`](config-manifest.md)

> **How to read a ruling.** Each ruling names its **kind** — `DECIDE-NOW` (a binding ruling, and an ADR if it touches a one-way door), `PHASE-GATE` (a condition a named phase must satisfy before its code ships; recorded in [`history/pre-takeoff-checklist.md`](history/pre-takeoff-checklist.md), not implemented here), `SPEC-NOTE` (an additive note applied to a phase/revision doc). Findings that gate or annotate are **not** implemented in this pass — recording a defense is not building it.

> **Closed — `PROJECT_RULES.md` (flagged missing on 2026-07-17).** The first pass of this log was written before the constitution existed and flagged its absence rather than inventing it. [`../PROJECT_RULES.md`](../PROJECT_RULES.md) was authored in Phase 0 and is at the repo root; this log is consistent with it.

---

## 0. What this pass did (and did not do)

On **2026-07-17** the owner accepted, in one batch, the output of the cross-phase revision pass: the **four cross-phase findings**, the **OQ-P7-1 elevation**, the **shadow-mode recommend-only defaults**, and **all sixteen proposed notes (PN-1…PN-16)**. This log records those acceptances and routes each to its correct home. 

- **Additive-only held.** Every ruling below annotates, gates, or affirms. **No frozen door, contract, schema, or vocabulary was rewritten** (verified item-by-item in §4 and the application report). Two rulings *touch* a one-way door without mutating it — each has an ADR (§3).
- **Nothing new was resolved.** Where applying an accepted item surfaced a sub-decision the docs never pinned, it was flagged rather than decided. Those flags are what §5 now dispositions.
- **No future-phase mechanism was built.** Feed-auth, the admin-audit log, and backup-plaintext protection are recorded as **phase-gates**, not implemented.

---

## 1. DECIDE-NOW rulings

### DR-1 — OQ-P7-1 is ELEVATED: the outcome feed is a hard pre-P7 door `DECIDE-NOW` · door-touching → **[ADR-0001](adr/ADR-0001-outcome-feed-authentication.md)**
- **Ruling.** OQ-P7-1 is reclassified from a `DIAL`/transport-choice to a **hard architectural gate that must be satisfied before any Phase-7 code ships.** The Phase-7 outcome feed (TB-7) **MUST** be an **authenticated channel** with the sender as an **identified principal**, and TB-7 **MUST** be threat-modelled for a *malicious* sender — not merely a noisy one.
- **Rationale (the §0 headline of the consistency report, F-15).** P4's control-loop guards (sample-gate, hysteresis, cascade-brake) were designed against a *self-generated* signal (a box's own verdict stream). P7 reuses them wholesale against an *external, cross-project* feed. Statistical guards defend against **noise**; they do not **authenticate**. A patient attacker feeding gate-clearing, bounded-step poison defeats every guard. This is the classic "get the door right before it accrues history or a consumer" logic — the feed is the input to the flywheel, and its trust model, once P7 calibrates on it, is not cheaply reversible.
- **What is decided vs. left open.** *Decided:* the feed is authenticated, sender-as-principal, TB-7 threat-modelled — this is now a door, not a dial. *Left open (owner / P7-time, see §5):* the specific mechanism — **signed webhooks vs. mutually-authenticated pull** — and the per-provider ingestion rate-limit values.
- **Applied to:** [`history/pre-takeoff-checklist.md`](history/pre-takeoff-checklist.md) §3.4 (PHASE-GATE before P7); SPEC-NOTE on P7 §3.2 (`[Finding-1 / OQ-P7-1, accepted]`); companion **PN-11** (F-17 belt-and-suspenders, routed under DR-5). Elevation recorded in **ADR-0001**.
- **Door check:** does **not** reopen Doors 1–5. It adds a *new* pre-P7 gate on TB-7 (a trust boundary), leaving the five frozen doors untouched. It is ADR-worthy because it *creates* a one-way architectural commitment about P7's input trust model.

### DR-2 — Shadow-mode recommend-only defaults are ACCEPTED (Finding 4) `DECIDE-NOW` · additive, no door
- **Ruling.** The `mode ∈ {shadow, recommend-only, auto}` dial ([`history/shadow-mode.md`](history/shadow-mode.md)) is adopted. The four day-one auto-mutators — **A1** (P4 block-rate quarantine, statistical trigger), **A6** (P7 confidence recalibration), **A8** (P7 provider auto-promotion), **A9** (P7 fallback weaning) — **default to `recommend-only`.** Promotion to `auto` is a deliberate, per-mechanism, per-scope owner action, logged to the admin-audit trail (DR-4). The two systemic brakes (A5 fleet, A7 cascade) already alert-and-hold and are unchanged — they are the blessed pattern being generalized.
- **Rationale.** Assembled, the seven phases auto-mutate caller-visible state (routing / confidence / eligibility / spend) on day one with no human in the per-decision loop. A6 is the sharpest: it rewrites the `confidence` *value* external systems code against. Recommend-only makes "the machine started changing itself" an explicit, approvable event, not a silent threshold crossing.
- **Door check.** Additive — the `mode` dial is a new config key/column; A6 still writes only `confidence` as an int 0–100 (Door 1's *value*, never its *type*); no `status`/`reason`/`recommendation` value is minted (Door 4 intact); the LOG stays append-only (Door 2 — recalibration writes the separate versioned calibration store). **No door reopened → no ADR.**
- **Applied to:** [`history/shadow-mode.md`](history/shadow-mode.md) (accepted marker, §7) ; [`history/pre-takeoff-checklist.md`](history/pre-takeoff-checklist.md) §3.5 (the fresh-system regression PHASE-GATE); [`config-manifest.md`](config-manifest.md) P7 header already reflects recommend-only.

### DR-3 — Admin identity + control-plane audit log is a PHASE-GATE before P3 go-live (Finding 2 / PN-1) `DECIDE-NOW` (gate accepted) · additive, no door
- **Ruling.** Before **P3 go-live**, every *manual control-plane action* — node disable, global-fallback-kill, pause, shadow-mode promotion, budget change, breaker/brake confirmation, privacy opt-out flip — **MUST** write to an **append-only admin-audit log** carrying **per-actor identity** (who / what / when), distinct from the verdict LOG. It reuses the existing `node_audits` / `work_item_attempts` append-only pattern.
- **Rationale (F-11).** The control surface accretes across P3–P7 and is today gated by *one* Supabase service-role credential with no per-actor identity and no audit of *which human* changed what — while the celebrated append-only LOG audits *verdicts*, not *control-plane mutations*. Several controls are security-critical (opt-out flip = a privacy event; budget raise = financial; brake-confirm = the safety valve).
- **Scope guard.** This pass records the **gate**, not the mechanism. The admin-audit table/authz is **not** built here — it gates P3, not P1.
- **Applied to:** [`history/pre-takeoff-checklist.md`](history/pre-takeoff-checklist.md) §3.6 (PHASE-GATE before P3 go-live); SPEC-NOTE on P3 §6.4 and on [`history/kill-switch-ladder.md`](history/kill-switch-ladder.md) §4 (`[Finding-2 / PN-1, accepted]`).

### DR-4 — Queue plaintext must be backup-aware: a PHASE-GATE before P5 (Finding 3 / PN-3) `DECIDE-NOW` (gate accepted) · door-touching (affirms Door 3) → **[ADR-0002](adr/ADR-0002-queue-plaintext-backup-scope.md)**
- **Ruling.** Before **P5**, the queue's plaintext-at-rest column (`work_items`, the one bounded Door-3 exception) **MUST** be excluded from, or protected in, backup/PITR snapshots. The **mechanism is a P5-time decision** among: snapshot exclusion, separate-key at-rest encryption, or short snapshot-retention. Backups/PITR are to be **treated as containing live plaintext** for the retention window.
- **Rationale (F-7).** P1 froze "raw email never persisted" (Door 3); P5 opened a *bounded* exception with a correct in-transaction purge-on-settle. The gap is invisible to either phase alone: a snapshot copies live `work_items` plaintext that **no purge transaction ever touches**. The exception was designed backup-*unaware*.
- **Door check.** **Door 3 is NOT reopened.** P1's "no plaintext" promise and P5's bounded exception both stand verbatim; this ruling only records that the exception's *reasoning must reach the backup surface too*. ADR-0002 documents the tension at the seam where both promises meet, and affirms Door 3 unchanged.
- **Scope guard.** The protection mechanism is **not** built or chosen here — that is P5's call.
- **Applied to:** [`history/pre-takeoff-checklist.md`](history/pre-takeoff-checklist.md) §3.7 (PHASE-GATE before P5); SPEC-NOTE on P5 §3.3 cross-referencing P1 §3.3 / §3.2.1 (`[Finding-3 / PN-3, accepted]`).

### DR-5 — PN-1…PN-16 are ACCEPTED as a batch; each routed to its home `DECIDE-NOW`
- **Ruling.** All sixteen proposed notes are accepted as recommended and applied to their correct homes, each tagged `[PN-n, accepted]`. PN-1…PN-4 are the four findings above (PN-1 = DR-3, PN-2 = DR-1, PN-3 = DR-4, PN-4 = DR-2). PN-5…PN-16 are applied per the table below.
- **Additivity re-verified on application.** Each PN was re-checked at apply time against "does this *alter* a frozen door/contract, or only *annotate/gate* it?" **All sixteen stayed additive.** The two that came closest are flagged in §5 (PN-14's dedup-constraint home and `residue_class` typing; PN-9's Door-2 *enforcement* hardening) — neither mutates locked content.

| PN | Home kind | Landed in |
|---|---|---|
| **PN-1** | PHASE-GATE + SPEC-NOTE | = **DR-3**. checklist §3.6; P3 §6.4 note; kill-switch §4 note |
| **PN-2** | DECIDE-NOW + PHASE-GATE + SPEC-NOTE | = **DR-1** (ADR-0001). checklist §3.4; P7 §3.2 note |
| **PN-3** | PHASE-GATE + SPEC-NOTE | = **DR-4** (ADR-0002). checklist §3.7; P5 §3.3 note |
| **PN-4** | DECIDE-NOW + PHASE-GATE | = **DR-2**. shadow-mode §7; checklist §3.5 |
| **PN-5** | SPEC-NOTE | P3 §6.4 (node-token rotation runbook); relates to pre-existing config-manifest §8.3 node-token custody entry (not edited this pass) |
| **PN-6** | SPEC-NOTE (+ P6 integration gate) | P6 §7.5 (platform opt-out ↔ policy-without-`external` 1:1, verified at integration) |
| **PN-7** | SPEC-NOTE (+ P6 DPA gate) | P6 §7.4 (cumulative-egressed metric + periodic vendor-deletion) |
| **PN-8** | SPEC-NOTE | P4 §5.5 + P7 §6.4 (brake-hold timeout/default + authenticated confirm + alert-pipeline health); kill-switch §4 marked accepted |
| **PN-9** | SPEC-NOTE (Door-2 enforcement, additive) | P1 §3.2.1 (separate DDL/grant-admin cred; optional hash-chain checkpoint); checklist §1.2 pointer |
| **PN-10** | SPEC-NOTE | P6 §6.1 (external-call transcript retained + queryable by `email_hash` for SAR/deletion) |
| **PN-11** | SPEC-NOTE (companion to DR-1 / F-17) | P7 §4.6 (≥N independent invalid-signal outcomes before the historical-bounce gate fires) |
| **PN-12** | SPEC-NOTE | checklist §1.1 + §4 (explicit brain-compromise threat model; brain host-hardening runbook) |
| **PN-13** | PHASE-GATE + SPEC-NOTE | checklist §3.8 (before P5 bulk); P5 §7.5 (per-caller keys + rotation beyond static token) |
| **PN-14** | SPEC-NOTE (2 sub-decisions flagged, §5) | P7 §5.4 (calibration column sketch) + P1 §4.7 (dedup-constraint home *flagged*) |
| **PN-15** | SPEC-NOTE (choice flagged, §5) | P1 §3.1.3 (`signals.platform` owner) + P2 §5.6 (catch-all re-probe cadence owner) |
| **PN-16** | SPEC-NOTE | kill-switch §Global A (define the global-fallback-kill flag + its authz/audit; rides DR-3's audit gate) |

---

## 2. Phase-gate summary (created by this pass)

Created by the 2026-07-17 pass and recorded in [`history/pre-takeoff-checklist.md`](history/pre-takeoff-checklist.md). **Status as of 2026-11-13** (the gates were not implemented by that pass; the phases that owned them have since built or not built them):

| Gate | Blocks | Source | Status 2026-11-13 |
|---|---|---|---|
| Outcome-feed **authenticated** (sender-as-principal, TB-7 threat-modelled) | before **P7** code | DR-1 / PN-2 / F-15 | ✅ **Discharged.** Mechanism = mutually-authenticated pull (mTLS) and the TB-7 threat model: [ADR-0013](adr/ADR-0013-outcome-feed-auth-mechanism-and-tb7-threat-model.md). `PullSource` is fail-closed (`Authenticated()` is false with no client). The real mTLS client waits for the sender: [`future-work.md`](future-work.md) S2/S3. |
| **Admin-audit log** + per-actor identity for every control-plane action | before **P3** go-live | DR-3 / PN-1 / F-11 | 🔧 **Ruling stands; not built.** No admin-audit table exists in migrations 0001–0015. It gates go-live: [`go-live-checklist.md`](go-live-checklist.md) G0.2, build task [`future-work.md`](future-work.md) §4 FW-1. |
| **Backup-plaintext** protection for `work_items` (mechanism = P5 choice) | before **P5** | DR-4 / PN-3 / F-7 | ✅ **Built.** Separate-key AES-256-GCM at rest + purge in the settlement transaction ([ADR-0011](adr/ADR-0011-phase-5-queue-additive-surface.md), §6.3 below; `TestAddrCipher_CiphertextContainsNoPlaintext`). The restore drill is real-world-gated: [`future-work.md`](future-work.md) B8. |
| **Shadow-mode regression**: fresh system = every in-scope mutator at default mode, zero live mutation | before whichever phase first ships each mutator (A1→P4, A6/A8/A9→P7) | DR-2 / PN-4 | ✅ **Built.** `config.ShadowModes` defaults to recommend-only; `TestFreshSystem_FlywheelWiredButInert` ([ADR-0014](adr/ADR-0014-phase-7-flywheel-additive-surface.md)); A1 default in [ADR-0010](adr/ADR-0010-phase-4-pool-additive-surface.md). |
| Front-door auth beyond static token (per-caller keys + rotation) | before **P5** bulk | PN-13 / F-20 | ✅ **Built.** `api_callers` (migration 0010), keys stored hashed, validity-window rotation, bulk requires a per-caller key ([ADR-0011](adr/ADR-0011-phase-5-queue-additive-surface.md), §6.3 below). |

---

## 3. ADR index (door-touching decisions)

There are **21 ADRs** (ADR-0001 … ADR-0021; the next number is ADR-0022). The full index — one row per ADR with the door it touches and whether it reopens it (none does) — is [`adr/README.md`](adr/README.md); it is the single place that list is maintained. The two this pass created are ADR-0001 (the outcome feed is an authenticated door) and ADR-0002 (queue plaintext must be backup-aware); ADR-0013 later discharged ADR-0001. The rulings after this pass (ADR-0015 … ADR-0021) are summarised in §6.4.

---

## 4. Five-door standing check (re-affirmed this pass)

No ruling in this log reopens any frozen door. Restated for record (full evidence: consistency report §2):

1. **/verify response shape (Door 1)** — untouched. A6 changes a `confidence` *value*, never its int-0–100 *type*. Additive only.
2. **LOG append-only (Door 2)** — untouched. PN-9 *hardens enforcement* (separate grant-admin cred) without changing the log contract; recalibration writes the separate versioned calibration store.
3. **Email hashing / no-plaintext (Door 3)** — untouched. DR-4 affirms the P5 bounded exception and gates its backup scope; the hashing scheme, pepper, and `bytea` key are unchanged.
4. **status / reason / recommendation vocabulary (Door 4)** — untouched. Zero values minted this pass; PN-11 adds only a DIAL (N) to the *already-reserved* `historical_bounce` gate.
5. **never-fork gem + node-list (Door 5)** — untouched. Nothing here patches the gem or hardcodes an endpoint.

---

## 5. Parked sub-decisions — dispositions (verified 2026-11-13)

This section used to be "Explicitly NOT decided": eight sub-decisions (nine rows below, because PN-15 covers two seams) the 2026-07-17 pass surfaced and
deliberately left to the owner or to the phase that built the thing. Every one now has a disposition, so
**nothing is open in this file**. Two rules were applied: a sub-decision the build later *made* is **closed**
and cites the code or migration that made it; a sub-decision still needing an owner call with **no recorded
recommendation** is **moved** to [`future-work.md`](future-work.md) §5 (and listed for the owner there) —
no ruling is invented here.

| Parked item | Disposition | Evidence / where it lives now |
|---|---|---|
| **OQ-P7-1 mechanism** — signed webhooks vs mutually-authenticated pull | **CLOSED** (ruled 2026-07-19, P7 build): mutually-authenticated pull (mTLS). P7 is the authenticated client, so it opens no inbound surface on the pepper-holding control-plane; built behind a swappable `OutcomeSource` interface so it is reversible by one adapter. The TB-7 malicious-sender threat model DR-1 required is produced in the same record. | [ADR-0013](adr/ADR-0013-outcome-feed-auth-mechanism-and-tb7-threat-model.md); §6 row OQ-P7-1. Live ingestion waits for the sender: [`future-work.md`](future-work.md) S2/S3. |
| **PN-11 / F-17** — the value of N, and whether the ≥N guard is always on | **CLOSED — built as a dial.** `TM_HISTORICAL_BOUNCE_MIN_N`, default **2** (`config.Dials.HistoricalBounceMinN`), validated at startup. The guard is always on: `EvaluateHistoricalGate` never fires below N independent confirmed outcomes, authenticated feed or not — defence in depth behind DR-1. An owner may tune N; no further decision is pending. | `control-plane/internal/config/config.go` (`HistoricalBounceMinN`), `internal/flywheel/historical_gate.go`; [ADR-0014](adr/ADR-0014-phase-7-flywheel-additive-surface.md); §6 row OQ-P7-2. |
| **PN-14a** — where the `(message_id, event)` dedup guard lives | **CLOSED:** a database partial `UNIQUE` index, `uq_send_outcomes_message_event` on `send_outcomes (message_id, event) WHERE message_id IS NOT NULL AND event IS NOT NULL`. | `control-plane/migrations/0012_flywheel_calibration.sql` (the "PN-14a RESOLVED" block). |
| **PN-14b** — `residue_class` as a typed column vs jsonb only | **CLOSED:** a typed nullable column, `send_outcomes.residue_class text`. | `control-plane/migrations/0012_flywheel_calibration.sql`. |
| **PN-15 / FLAG-A** — who owns `signals.platform`, or mark it Phase-8+ | **MOVED** → [`future-work.md`](future-work.md) FW-6 and the owner list. No code in the tree populates `signals.platform`; the reserved key stays reserved. Which resolution to take (assign a phase, or declare it Phase-8+) is a product-scope call and no recommendation is recorded. | `Docs/history/cross-phase-consistency-report.md` (FLAG-A); P1 §3.1.3 note. |
| **PN-15 / FLAG-C** — who owns the catch-all re-probe loop | **MOVED** → [`future-work.md`](future-work.md) FW-7 and the owner list. The state is written (`catchall_domains.expires_at`, `store/postgres.go`) but nothing re-probes on it; the spec offers "P4 health-watcher or P5 queue or Phase-8+" and records no choice. | `control-plane/internal/orchestrator/smtp_stage.go` (the FLAG-C comment); P2 §5.6 note. |
| **PN-8 / F-12** — brake-hold timeout and default-on-timeout (stay-held vs auto-release) | **MOVED** → [`future-work.md`](future-work.md) FW-8 and the owner list. What is built is *stay-held*: both brakes alert and hold for a human and no timeout exists (`flywheel/guards.go`: "requires human confirmation … alert, never auto-apply"). Adding a timeout, or making the confirm an audited action, is a new decision that also depends on DR-3 (FW-1). | `control-plane/internal/flywheel/guards.go`, `internal/health/decide.go`; P4 §5.5 and P7 §6.4 notes. |
| **PN-9 / F-13** — hash-chain-to-WORM checkpoint in scope, or accept grant-based immutability with a documented residual | **MOVED** → [`future-work.md`](future-work.md) FW-9 and the owner list. The grant-based part is built and the residual is now written down: the runtime role cannot rewrite the LOG, but whoever holds `cp_admin` can (see [`security-posture.md`](security-posture.md) §3). Whether to add an external tamper-evidence checkpoint is an owner call with no recorded recommendation beyond "optional". | `control-plane/migrations/0005_grants.sql`; checklist §1.2 in `Docs/history/pre-takeoff-checklist.md`. |
| **DR-4 mechanism** — backup-protection mechanism (exclusion / separate-key encryption / short retention) | **CLOSED** (ruled 2026-07-18, P5 build): separate-key at-rest encryption of `work_items.addr_ciphertext` plus purge in the settlement transaction. | §6.3 below; [ADR-0011](adr/ADR-0011-phase-5-queue-additive-surface.md); [ADR-0002](adr/ADR-0002-queue-plaintext-backup-scope.md). |

**Count:** 9 rows (8 parked sub-decisions; PN-15 is two) → **5 closed** (OQ-P7-1 mechanism, PN-11, PN-14a, PN-14b, DR-4 mechanism), **4 moved** (FLAG-A, FLAG-C,
PN-8, PN-9). **Open in this file: 0.** The four moved items are decisions, not work: each is listed with the question the owner must
answer in [`future-work.md`](future-work.md) §5.

---

## 6. Owner-OQ register (added Phase 0 — pointers, not re-decisions)

Phase 0 was asked to capture the outstanding owner open-questions and *their resolutions as they land*.
The named OQs below were **already decided** in each phase's Appendix D sign-off; this register makes
them **discoverable in one place** and links each to its owning ruling. **Nothing is re-decided here**;
the §5 parked sub-decisions were dispositioned on 2026-11-09 (closed, or moved to `future-work.md`) — see §5.

| OQ | Question | Status | Resolved / recorded in |
|---|---|---|---|
| **OQ-P6-3** | Vendor `invalid` acceptance asymmetry | **DECIDED** (asymmetric): accept as `invalid` only on retry-exhausted / gave-up-capacity; downgrade to `risky` on catch-all/`:mx`. | P6 §3.4 + Appendix D.2; [`config-manifest.md`](config-manifest.md) §6.2; pre-takeoff-checklist §2 |
| **OQ-P6-6** | Platform disclosure for vendor egress | **DECIDED**: disclose + opt-out (per-policy `external` block = the opt-out lever); PN-6 asserts platform-opt-out ↔ policy-without-`external` maps 1:1 at integration. Positioning call flagged. | P6 §7.5 + Appendix D.3; |
| **OQ-P7-1** | Outcome-feed authentication **mechanism** (signed webhooks vs mutually-authenticated pull) | **DECIDED (2026-07-19, P7 build): mutually-authenticated pull (mTLS)** — discharges the ADR-0001 pre-P7 gate together with the TB-7 malicious-sender threat model; built behind a swappable `OutcomeSource` interface (reversible to signed-webhooks). Live still gated on the sender (PENDING-REAL-SENDER, GAP-48). | **[ADR-0013](adr/ADR-0013-outcome-feed-auth-mechanism-and-tb7-threat-model.md)**; §5 (parked line resolved) |
| **OQ-P7-2** | Historical-bounce gate TTL + reversibility | **DECIDED**: TTL-windowed hard gate (6–12 mo, DIAL) on confirmed `mailbox_not_found` only, degrading to a confidence penalty after. PN-11 adds a ≥N defense-in-depth DIAL behind DR-1. **Built (P7): `TM_HISTORICAL_BOUNCE_TTL_DAYS`=270, `_MIN_N`=2 — [ADR-0014](adr/ADR-0014-phase-7-flywheel-additive-surface.md).** | P7 §4.6 + Appendix D.3 |
| **OQ-P7-8** | "Your sends train our accuracy" disclosure | **DECIDED**: surface it (disclose-don't-hide); brain exposes the seam, platform decides terms. | P7 §14 + Appendix D.6 |
| **P1 cheap-layers mock-vs-live** | Do the cheap layers call a mocked engine, or run brain-native? | **DECIDED**: brain-native for syntax + MX/DNS/null-MX (the cheap path); the engine is used only for the SMTP path (P2). → the Go brain takes **no** truemail dependency. Recorded as **ADR-0004** (a structural, hard-to-reverse choice). | P1 §6 / OQ-6; **[ADR-0004](adr/ADR-0004-cheap-layers-brain-native.md)** |

The four remaining P1-named OQs the specs still hold as *decided-with-standing-caveat* (OQ-1 pepper
custody, OQ-7 disposable-risky, OQ-3 local-part case, OQ-23 single-default) are recorded in P1 Appendix
C and restated as rules in [`../PROJECT_RULES.md`](../PROJECT_RULES.md); they are not re-opened.

### 6.1 Phase-0 sign-off resolutions (owner ruling 2026-07-17)

The Phase-0 closeout produced [`history/phase-0-signoff-sheet.md`](history/phase-0-signoff-sheet.md) — the six
spec-vs-reality deltas (GT-1…GT-6), the Door-3 local-part fix, and the LICENSE. On **2026-07-17**
the owner ruled **"accept all + record"** on the deltas and the fix, and **deferred** LICENSE.
Recorded here so the Phase-1 prerequisite ("sign-off resolved in open-decisions.md") is satisfied; the
sheet's boxes are checked to match. **Two of these touch door territory** and are flagged loudest.

| Item | Touches a door? | Ruling (2026-07-17) | Recorded in |
|---|---|---|---|
| **GT-1** engine `3.3.0`→`3.3.1` | No (illustrative spec string; provenance only) | **ACCEPTED** — pin 3.3.1 | [ADR-0003](adr/ADR-0003-truemail-engine-pin.md) |
| **GT-2** rack `0.10.0`→`0.9.0` | No (no `0.10.0` tag exists; provenance only) | **ACCEPTED** — fork base `v0.9.0` | [FORK.md](../verifier-node/truemail-rack/FORK.md) |
| **GT-3** `net-smtp` `~>0.4.0`→`0.5.1` (exact) | **Door 5 territory** — fork-side dep edit, gem byte-for-byte unmodified | **ACCEPTED** — forced by the exact engine pin; rack-side edit, not a gem patch. **Phase-8: tightened from the range `~> 0.5.0` to EXACTLY `0.5.1`** (the resolved version) for rebuild determinism — engine unchanged (Door 5). | [ADR-0003](adr/ADR-0003-truemail-engine-pin.md) §"One forced consequence"; FORK.md #2 |
| **GT-4** `host_audit` live shape | No (informs P3 gate; gate design frozen) | **ACCEPTED** — gate anchor (`current_host_ip`+`warnings`) confirmed live | [truemail-ground-truth.md](history/truemail-ground-truth.md) §3.3 |
| **GT-5** `smtp_mock` accept-all (P2 GAP-14) | No (P2 dev-substrate property) | **ACCEPTED** — scope call: real-MX path proven at P0; seeded-probe fixture is a P2 verification | history/truemail-ground-truth.md §3.4 |
| **GT-6** `host_audit` extra `date`/`configuration` keys | No (additive; gate reads only `warnings`) | **ACCEPTED** — additive/ignorable, record only | history/truemail-ground-truth.md §3.3 |
| **Door-3 fix** — refuse non-ASCII local part until NFC (P1) | **Door 3** — a *tightening* that affirms the frozen scheme (refuse > mis-hash) | **BLESSED** — safe direction, locked by `TestNonASCIILocalPartRefused`, no scheme change; P1 wires NFC/IDNA and may relax both guards *together* | **[ADR-0006](adr/ADR-0006-door3-nonascii-localpart-refusal.md)** |
| **LICENSE** (project's own) | No (legal/distribution posture; no code impact) | **RESOLVED (2026-07-19, arc closeout): PROPRIETARY / all-rights-reserved** — added as the top-level [`LICENSE`](../LICENSE). Covers the owner's own code only; explicitly grants no rights over the MIT third-party components (respected in [NOTICE](../NOTICE), retained at `verifier-node/truemail-rack/LICENSE.txt`, gem byte-identical). Chosen over MIT/Apache-2.0 because the brain's mechanics + data moat are deliberately non-public; reversible toward openness later, publishing is not. **SUPERSEDED (repos public by owner decision, 2026-11-17)** — the "deliberately non-public" rationale above no longer holds: the owner has made the repositories public, with the source visible for transparency only and all rights still reserved (the top-level [`LICENSE`](../LICENSE) is now a single paragraph saying so; the text in this row is kept as the 2026-07-19 record). Was DEFERRED 2026-07-17 (Phase 0). | [phase-0-signoff-sheet.md](history/phase-0-signoff-sheet.md) §C; [`../LICENSE`](../LICENSE); [`../README.md`](../README.md) "License posture" |

**Door standing after these rulings:** unchanged. GT-3 is a fork-side dependency resolution (the gem
is unmodified — Door 5 intact); the Door-3 blessing is a refuse-not-mint tightening that leaves the
frozen `norm/v1+hmac-sha256/v1` scheme verbatim (Door 3 intact). No frozen door is reopened. The **§5
parked sub-decisions were untouched by this ruling** (their 2026-11-09 dispositions are in §5).

### 6.2 Phase-2 sign-off resolutions (owner ruling 2026-07-17, P2→P3 readiness pass)

The Phase-2 build produced two door-referencing ADRs. On **2026-07-17** the owner, in the P2→P3
readiness pass, explicitly authorized ratifying **ADR-0008** and applying its spec-text amendment
(and no other re-decision). Recorded here so the ratification is discoverable in one place.

| Item | Touches a door? | Ruling (2026-07-17) | Recorded in |
|---|---|---|---|
| **ADR-0007** — catch-all probe = two sessions (Option B); gem never patched | **Door 5 territory** — affirms never-fork; the gem is byte-identical | **ACCEPTED** (carries the P2 Appendix-D owner sign-off; the GAP-13 spike confirmed the gem exposes no reusable same-session) | [ADR-0007](adr/ADR-0007-gap13-catchall-session-two-session.md) |
| **ADR-0008** — `verification_raw` redacts the plaintext address; the P2 "store verbatim" text is narrowed | **Door 3** — a conformance/tightening that AFFIRMS the frozen scheme (the door governs a spec instruction that contradicted it) | **RATIFIED** — the P2 spec (§4.1/§6.4/OQ-P2-5 + Appendix D) is amended to "verbatim except the redacted address identity" so spec and code agree; redaction confirmed at the persistence boundary on all paths (email field, 5xx bodies, short local-parts, case-variant echoes, nested fields), case-insensitive, fail-verified by four regression tests | [ADR-0008](adr/ADR-0008-door3-raw-blob-address-redaction.md) |

**Door standing after these rulings:** unchanged. ADR-0007 keeps the gem byte-identical (Door 5
intact, CI-asserted). ADR-0008 is a refuse-to-persist-plaintext tightening that leaves the frozen
`norm/v1+hmac-sha256/v1` scheme, the `bytea` key, and the pepper verbatim (Door 3 intact) — the raw
full email is now genuinely never persisted in a durable store. No frozen door is reopened; the §5
parked sub-decisions were untouched (dispositions in §5). This is the only re-decision the owner authorized in this pass.

### 6.3 Phase-5 gate resolutions (owner ruling 2026-07-18, Phase-5 build)

The Phase-5 build had to resolve the two P5-blocking phase-gates whose *mechanism* the earlier
passes deliberately parked ("P5-time"). On **2026-07-18** the owner ruled on both; recorded here so
the resolution is discoverable in one place. Both are captured door-side in **[ADR-0011](adr/ADR-0011-phase-5-queue-additive-surface.md)**.

| Item | Touches a door? | Ruling (2026-07-18) | Recorded in |
|---|---|---|---|
| **DR-4 mechanism** (§5 parked: "backup-protection mechanism — P5-time") | **Door 3** — affirms + makes the bounded exception backup-aware | **SEPARATE-KEY at-rest encryption** of `work_items.addr_ciphertext` (AES-256-GCM, key ≠ pepper, `TM_QUEUE_ADDR_KEY`). Chosen over snapshot-exclusion / short-retention as the only **locally fail-verifiable** option (dump-the-row → ciphertext-only). Bounded two ways now: encrypted at rest AND purged in the settlement tx. | [ADR-0011](adr/ADR-0011-phase-5-queue-additive-surface.md); [ADR-0002](adr/ADR-0002-queue-plaintext-backup-scope.md); pre-takeoff §3.7 (ticked) |
| **PN-13 / F-20** (per-caller keys + rotation before P5 bulk) | Auth-strength behind the **same** contract — no Door-1/vocab change | **Per-caller keys stored HASHED** (`api_callers`, migration 0010) + rotation via overlapping validity windows; bulk REQUIRES a per-caller key, sync keeps the legacy static token during transition. `caller_id` is key-derived (trusted), distinct from the opaque body-supplied `caller_ref` (GAP-36). | [ADR-0011](adr/ADR-0011-phase-5-queue-additive-surface.md); pre-takeoff §3.8 (ticked) |

**Door standing after these rulings:** unchanged. DR-4 keeps Door 3's "raw email never persisted"
verbatim — the one bounded exception is now encrypted at rest AND transactionally purged (the raw
address is never in a durable store in clear). PN-13 changes auth *strength* behind the *same*
`/v1/verify(/bulk)` contract (Door 1/4 untouched — auth is header-only, `caller_id` key-derived).
Phase 5 mints **zero** new verdict-vocab values (Door 4; `vocab.go` byte-unchanged) and adds no
box-side behavior (Door 5). The GAP-37 lease-CAS invariant is now built. The §5 sub-decisions unrelated to P5 were dispositioned on 2026-11-09.

### 6.4 Rulings since the arc closed (owner, 2026-08-04 → 2026-11-09)

After the seven-phase arc closed, audits and the first real GitHub-Actions runs found defects, and the owner ruled on each. Every one is an ADR (each lists `Deciders: owner`); none reopens a door (`adr/README.md` index, "Reopens it?" column). Listed here so the log of record is complete in one place — the ADR is the authority for each.

| ADR | Ruling | Door standing |
|---|---|---|
| [ADR-0015](adr/ADR-0015-rung4-pause-enforcement.md) (2026-08-04) | Kill-switch Rung 4 is *enforced*: `TM_FRONTDOOR_MODE=accept_and_defer` stops the drainer and pauses the sync endpoint; `reject` takes nothing. | Door 1 additive (one boolean on two bulk envelopes); Door 4 untouched. |
| [ADR-0016](adr/ADR-0016-no-credential-in-a-log-line.md) (2026-08-04) | No credential reaches a log line (pgx's parse error embeds the DSN); gated by `check-no-secret-leak.sh`. | Strengthens Door 3. |
| [ADR-0017](adr/ADR-0017-raw-partition-maintenance.md) (2026-11-07) | `verification_raw` partitions are created ahead of time by a narrow `SECURITY DEFINER` function + a maintenance loop (migration 0014). | No door touched. |
| [ADR-0018](adr/ADR-0018-verifier-node-base-image.md) (2026-11-08; addenda to 2026-11-09) | The node base image is digest-pinned on a supported Debian and built without a cache; gems installed from a committed `Gemfile.lock`; **addendum 2: Ruby 3.2 → 3.4.12** (a bounded attempt, ruled by the owner), the EOL closed. | Door 5 intact — truemail stays exactly 3.3.1, unmodified. |
| [ADR-0019](adr/ADR-0019-dns-names-are-rooted-and-failure-is-unknown.md) (2026-11-08; addendum 2026-11-09) | Every DNS name is looked up rooted; a DNS failure of ours is `unknown`, never `invalid`; the node's resolver hygiene is enforced by its host audit. | Door 5 intact. |
| [ADR-0020](adr/ADR-0020-implicit-mx-rfc5321.md) (2026-11-09) | **A-only domains:** a domain with no MX but an A/AAAA record receives mail at that host (RFC 5321 §5.1) — it is not `invalid`; if the probe cannot run it is `risky`. Null MX and "no MX and no A/AAAA" stay `invalid`. | Door 4 untouched (no new values). |
| [ADR-0021](adr/ADR-0021-smtp-probe-destination-guard.md) (2026-11-09) | The SMTP probe never connects to a non-public address (pre-flight guard + post-check + egress firewall); the brain returns `risky` / `non_public_mx`, never `invalid`, never cached. | Door 4 additive: one new `reason` value (migration 0015). |

Anything these ADRs left to do (installing the egress firewall on the real box, a short hostname on it) is real-world-gated and is tracked in [`go-live-checklist.md`](go-live-checklist.md), not here.

### 6.5 Rulings of the Phase 3 close-out (owner, 2026-11-18)

The platform's Phase 3 ran module 1 for the first time as a real caller and ruled on what it found (`coldmail-platform/Docs/phase-3-closeout-spec.md`). The module-1 half, **all additive, no door reopened**:

| Ruling | What was decided and where it landed | Status |
|---|---|---|
| **OD-4 — node promotion (docs versus code)** | The install docs said the first green audit promotes `provisioning → active`; **no running code did that, and every boot reset the node to `provisioning` with `port25_open=false`**, so a person had to `UPDATE` the row after every restart. Ruled: a node becomes `active` **only** after a green audit row recorded after this boot's registration, with `port25_open` taken from that audit, in one atomic step that records the audit row's id; a red audit leaves it `provisioning`; a restart re-registers, re-audits and re-promotes and **never trusts a stale `active`**. Built in migration `0019` (`nodes.registered_at`, `nodes.promoted_by_audit_id`), `Store.RegisterNode` / `Store.SettleProvisioningNode`, `health.Evaluate` (a provisioning node is promoted by a green, egress-confirmed, live audit and is **not** quarantined by a red one) and the watcher; documented in [`install-and-deploy.md`](install-and-deploy.md) §3.2 d.4 and [`operator-runbook.md`](operator-runbook.md) §5.1-5.2 (with the lease-recovery wait and its dial). **One thing is decided by the ruling and flagged here, not hidden:** a green audit does not probe outbound port 25, so `port25_open` no longer carries an independent egress proof; that is [`future-work.md`](future-work.md) FW-2 (now narrowed to a dedicated egress test). | **RESOLVED 2026-11-18** |
| **OD-6 / OD-8 — a partial Tier 4** | `GET /v1/describe`, an idempotent bulk create (`Idempotency-Key`; `409 idempotency_key_reused`; `Idempotent-Replayed: true`) and `totals` on the job status, with the listing declared stable once `complete` is true. **No `quote` / `run` / `report`** (what is charged depends on the answer, so module 1 has nothing to quote). Built in migration `0019` (`jobs.idem_caller_id`, `jobs.idempotency_key`, `jobs.request_fingerprint`, one partial unique index) and `internal/api`, `internal/queue`, `internal/store`; contract in [`api-contract.md`](api-contract.md). Closes [`future-work.md`](future-work.md) FW-10. | **RESOLVED 2026-11-18** |
| **OD-9 — one syntax rule** | The platform pre-validates with `norm/v1` plus module 1's syntax layer exactly as module 1 implements it. The shared vector file is now format `/2`: every non-malformed vector carries `syntax_ok` (the real `layers.SyntaxLayer` over the parts `hash.SplitNormalized` returns, the split the orchestrator uses) and 31 new `syntax-*` vectors pin every boundary on both sides. **M1-10 is unchanged**: module 1 still settles such an address as `invalid` / `invalid_syntax` and the platform's charge for it is the platform's rule. | **RESOLVED (module-1 half) 2026-11-18** |
| **A3 — bad input** | A job id that is not a UUID is `400 validation_failed` (field `id`) on both routes that take one, never a `500`, never the database. The results cursor was already a `400` and stays one. | **RESOLVED 2026-11-18** |

---

*This log records acceptances, routes, and (Phase 0) an owner-OQ pointer register; it builds nothing
and reopens nothing. Line-by-line placement is in [`history/consolidation-application-report.md`](history/consolidation-application-report.md).*
