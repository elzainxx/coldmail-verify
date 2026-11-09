# Consolidation Application Report — 2026-07-17

**Status:** COMPLETE — the audit trail for the accepted-recommendation application pass (planning/consolidation, not code).
**What this is:** every accepted item (4 cross-phase findings + PN-1…PN-16), its **home kind** (DECIDE-NOW / SPEC-NOTE / PHASE-GATE), and the **exact file + section** it landed in — so nothing was dropped or silently over-applied.
**Companions:** [`DECISIONS.md`](../open-decisions.md) · [`adr/ADR-0001`](../adr/ADR-0001-outcome-feed-authentication.md) · [`adr/ADR-0002`](../adr/ADR-0002-queue-plaintext-backup-scope.md) · [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) · [`shadow-mode.md`](shadow-mode.md) · [`kill-switch-ladder.md`](kill-switch-ladder.md)

> **Discipline applied throughout:** additive-only; flag-don't-fix; no frozen door/contract/schema/vocabulary rewritten; no future-phase mechanism built (feed-auth, admin-audit, backup-protection recorded as gates only). Every SPEC-NOTE is a delimited `[CONSOLIDATION NOTE … accepted]` block placed *after* the frozen text it annotates — never an in-place edit of locked content.

---

## A. The four cross-phase findings

| # | Finding | Home kind(s) | Landed in (file · section) |
|---|---|---|---|
| **1** | P7 outcome-feed authentication (OQ-P7-1) | DECIDE-NOW + ADR + PHASE-GATE + SPEC-NOTE | `DECISIONS.md` **DR-1**; `adr/ADR-0001-outcome-feed-authentication.md`; `pre-takeoff-checklist.md` **§3.4** + §6 summary; `phase-7-flywheel-spec.md` **§3.2** note |
| **2** | Admin identity + control-plane audit log | DECIDE-NOW (gate) + PHASE-GATE + SPEC-NOTE | `DECISIONS.md` **DR-3**; `pre-takeoff-checklist.md` **§3.6** + §4 + §6; `phase-3-first-real-box-spec.md` **§6.4** note; `kill-switch-ladder.md` **§4** |
| **3** | Queue plaintext in DB backups | DECIDE-NOW (gate) + ADR + PHASE-GATE + SPEC-NOTE | `DECISIONS.md` **DR-4**; `adr/ADR-0002-queue-plaintext-backup-scope.md`; `pre-takeoff-checklist.md` **§3.7** + §6; `phase-5-queue-spec.md` **§3.3** note (x-ref P1 §3.3/§3.2.1) |
| **4** | Four day-one auto-mutators | DECIDE-NOW + PHASE-GATE | `DECISIONS.md` **DR-2**; `shadow-mode.md` **§7** acceptance; `pre-takeoff-checklist.md` **§3.5** regression gate |

**Finding 1 sub-parts:** the F-17 companion (≥N invalid-signal outcomes before the historical-bounce gate) landed as **PN-11** on `phase-7-flywheel-spec.md` §4.6.

---

## B. The sixteen proposed notes

| PN | Home kind | Landed in (file · section) | Additive? |
|---|---|---|---|
| **PN-1** | PHASE-GATE + SPEC-NOTE | = Finding 2. `DECISIONS.md` DR-3; checklist §3.6; P3 §6.4 note; kill-switch §4 | ✅ new operational table, gated |
| **PN-2** | DECIDE-NOW + ADR + PHASE-GATE + SPEC-NOTE | = Finding 1. DR-1; ADR-0001; checklist §3.4; P7 §3.2 note | ✅ new TB-7 gate; 5 doors untouched |
| **PN-3** | PHASE-GATE + ADR + SPEC-NOTE | = Finding 3. DR-4; ADR-0002; checklist §3.7; P5 §3.3 note | ✅ affirms Door 3 + P5 exception |
| **PN-4** | DECIDE-NOW + PHASE-GATE | = Finding 4. DR-2; shadow-mode §7; checklist §3.5 | ✅ `mode` dial, Door 1 value not type |
| **PN-5** | SPEC-NOTE | `phase-3-first-real-box-spec.md` §6.4 note (combined w/ PN-1) | ✅ ops runbook |
| **PN-6** | SPEC-NOTE (integration assertion) | `phase-6-fallback-spec.md` §7.5 note | ✅ integration test; OQ-P6-6 untouched |
| **PN-7** | SPEC-NOTE + PHASE-GATE ref | `phase-6-fallback-spec.md` §7.4 note; checklist §3.3 | ✅ metric + process |
| **PN-8** | SPEC-NOTE (both brakes) | `phase-4-resilience-pool-spec.md` §5.5 note; `phase-7-flywheel-spec.md` §6.4 note; kill-switch §4 | ✅ spec addendum; value flagged |
| **PN-9** | SPEC-NOTE (Door-2 enforcement) | `phase-1-foundations-spec.md` §3.2.1 note; checklist §1.2 | ✅ hardens enforcement, not contract |
| **PN-10** | SPEC-NOTE | `phase-6-fallback-spec.md` §6.1 note | ✅ retention/index config |
| **PN-11** | SPEC-NOTE (F-17 companion) | `phase-7-flywheel-spec.md` §4.6 note | ✅ DIAL on reserved gate; Door 4 intact |
| **PN-12** | SPEC-NOTE | checklist §1.1 + §4 | ✅ threat model + runbook |
| **PN-13** | PHASE-GATE + SPEC-NOTE | checklist §3.8; `phase-5-queue-spec.md` §7.5 note | ✅ auth strength; same contract |
| **PN-14** | SPEC-NOTE (2 sub-decisions flagged) | `phase-7-flywheel-spec.md` §5.4 note; `phase-1-foundations-spec.md` §4.7 note | ✅ sketch on *new* table; enforcement flagged |
| **PN-15** | SPEC-NOTE (resolution flagged) | `phase-1-foundations-spec.md` §3.1.3 note (FLAG-A); `phase-2-smtp-mocks-spec.md` §5.6 note (FLAG-C) | ✅ names owner *or* Phase-8+; key unchanged |
| **PN-16** | SPEC-NOTE | `kill-switch-ladder.md` §Global A | ✅ config + audit; rides DR-3 |

---

## C. Files touched

**Created (4):**
- `DECISIONS.md` — the cross-phase rulings of record (DR-1…DR-5), phase-gate summary, ADR index, five-door standing check, and the "explicitly NOT decided" register.
- `adr/ADR-0001-outcome-feed-authentication.md` — OQ-P7-1 elevation (door-touching).
- `adr/ADR-0002-queue-plaintext-backup-scope.md` — P5 backup-plaintext gate (affirms Door 3).
- `consolidation-application-report.md` — this file.

**Updated — revision docs (3):**
- `pre-takeoff-checklist.md` — §3 re-headed (spikes **+ phase-gates**); new §3.4–3.8; §1.1/§1.2/§4 PN pointers; §6 one-glance summary refreshed.
- `shadow-mode.md` — new §7 acceptance (DR-2).
- `kill-switch-ladder.md` — §Global A (PN-16 accepted) + §4 (PN-1/PN-8 accepted).

**Updated — phase specs, additive notes only (7):**
- `phase-1-foundations-spec.md` — §3.2.1 (PN-9), §3.1.3 (PN-15/FLAG-A), §4.7 (PN-14 companion).
- `phase-2-smtp-mocks-spec.md` — §5.6 (PN-15/FLAG-C).
- `phase-3-first-real-box-spec.md` — §6.4 (Finding 2/PN-1 + PN-5).
- `phase-4-resilience-pool-spec.md` — §5.5 (PN-8).
- `phase-5-queue-spec.md` — §3.3 (Finding 3/PN-3), §7.5 (PN-13).
- `phase-6-fallback-spec.md` — §6.1 (PN-10), §7.4 (PN-7), §7.5 (PN-6).
- `phase-7-flywheel-spec.md` — §3.2 (Finding 1/PN-2), §4.6 (PN-11), §5.4 (PN-14), §6.4 (PN-8).

**Not touched:** `config-manifest.md`, `cross-phase-consistency-report.md` (the source registers — left as the origin of record), `truemail-build-plan.md`, the SVG. No frozen contract text, schema DDL, or vocabulary enum was edited in any spec — every spec change is an appended `[CONSOLIDATION NOTE]` block.

---

## D. Flags raised (surfaced, per the rules — NOT resolved in this pass)

Recorded in full in [`DECISIONS.md`](../open-decisions.md) §5. Summary:

1. **⚠ `PROJECT_RULES.md` does not exist** — named as a "read first" input but absent from the repo. Not authored (it was not on the produce list). Surfaced in `DECISIONS.md` header. **If it is meant to exist, it is an outstanding artifact for the owner.**
2. **OQ-P7-1 mechanism** — signed webhooks vs. mutually-authenticated pull (DR-1 fixes *that* it's authenticated, not *how*).
3. **PN-11 / F-17** — the value of **N**, and whether ≥N is always-on or only-when-unauthenticated.
4. **PN-14a** — the *home* of the `(message_id, event)` dedup guard (DB UNIQUE vs. app-guard vs. staging).
5. **PN-14b** — `residue_class` typed column vs. jsonb.
6. **PN-15** — for `signals.platform` (FLAG-A) and catch-all re-probe cadence (FLAG-C): name an owning phase vs. mark Phase-8+.
7. **PN-8 / F-12** — brake-hold timeout value + default-on-timeout behavior.
8. **PN-9 / F-13** — hash-chain-to-WORM tamper-evidence in scope, or accept grant-based immutability with a documented residual.
9. **DR-4 mechanism** — backup-protection choice (exclusion / separate-key encryption / short-retention), a P5-time call.

---

## E. Additivity attestation

Every one of the 4 findings + 16 PNs was re-checked at apply time against the test *"does this alter frozen content, or only annotate/gate it?"* — **all stayed additive.** The three that came closest, and why each is still additive:

- **PN-2 / DR-1 (feed auth):** creates a *new* gate on TB-7 (a trust boundary), leaving Doors 1–5 untouched. Door-touching → ADR-0001, but reopens nothing.
- **PN-3 / DR-4 (backup plaintext):** *affirms* Door 3 and the P5 bounded exception; adds a backup-scope gate. Door-referencing → ADR-0002, but P1's "no plaintext" and P5's purge stand verbatim.
- **PN-9 (grant-admin cred):** hardens Door-2 *enforcement* (who holds the grant), not the append-only *contract*. The revoked-grant design and sidecar split are unchanged.

**No accepted recommendation turned out to be non-additive.** Had one required mutating a frozen door/contract, it would have been stopped and flagged per the rules rather than applied — none did.
