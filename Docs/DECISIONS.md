# DECISIONS — Truemail (rulings of record)

**Status:** LIVE — the binding decision log for the Truemail control-plane. Rulings here are for-build.
(Deployable names: the design docs' "brain" is the `control-plane`, "box" is the `verifier-node` — [ADR-0005](adr/ADR-0005-deployable-naming.md). Ruling text below that quotes the consistency report keeps its original "brain"/"box" vocabulary.)
**Scope of this file:** owner rulings that are *cross-phase* or that *elevate/gate* something a single phase couldn't decide alone. Per-phase sign-offs remain in each spec's Appendix D; this file records only what the phases could not.
**Companions:** [`adr/`](adr/) (door-touching decisions) · [`consolidation-application-report.md`](consolidation-application-report.md) · [`cross-phase-consistency-report.md`](cross-phase-consistency-report.md) · [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) · [`shadow-mode.md`](shadow-mode.md) · [`kill-switch-ladder.md`](kill-switch-ladder.md) · [`config-manifest.md`](config-manifest.md)

> **How to read a ruling.** Each ruling names its **kind** — `DECIDE-NOW` (a binding ruling, and an ADR if it touches a one-way door), `PHASE-GATE` (a condition a named phase must satisfy before its code ships; recorded in [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md), not implemented here), `SPEC-NOTE` (an additive note applied to a phase/revision doc). Findings that gate or annotate are **not** implemented in this pass — recording a defense is not building it.

> **⚠ Missing input flagged (not invented).** The task named `PROJECT_RULES.md` as a "read first" input. **No such file exists in the repo** (only the seven phase specs, the build plan, the SVG, and the five revision docs). This log and the application report were produced against the frozen specs + revision docs, which encode the governing rules (the five one-way doors, additive-only, harden-toward-`unknown`). If `PROJECT_RULES.md` is meant to exist, it is an outstanding artifact — surfaced here, **not** authored, since it was not on the produce list.

---

## 0. What this pass did (and did not do)

On **2026-07-17** the owner accepted, in one batch, the output of the cross-phase revision pass: the **four cross-phase findings**, the **OQ-P7-1 elevation**, the **shadow-mode recommend-only defaults**, and **all sixteen proposed notes (PN-1…PN-16)**. This log records those acceptances and routes each to its correct home. 

- **Additive-only held.** Every ruling below annotates, gates, or affirms. **No frozen door, contract, schema, or vocabulary was rewritten** (verified item-by-item in §4 and the application report). Two rulings *touch* a one-way door without mutating it — each has an ADR (§3).
- **Nothing new was resolved.** Where applying an accepted item surfaced a sub-decision the docs never pinned, it is **flagged in §5**, not decided.
- **No future-phase mechanism was built.** Feed-auth, the admin-audit log, and backup-plaintext protection are recorded as **phase-gates**, not implemented.

---

## 1. DECIDE-NOW rulings

### DR-1 — OQ-P7-1 is ELEVATED: the outcome feed is a hard pre-P7 door `DECIDE-NOW` · door-touching → **[ADR-0001](adr/ADR-0001-outcome-feed-authentication.md)**
- **Ruling.** OQ-P7-1 is reclassified from a `DIAL`/transport-choice to a **hard architectural gate that must be satisfied before any Phase-7 code ships.** The Phase-7 outcome feed (TB-7) **MUST** be an **authenticated channel** with the sender as an **identified principal**, and TB-7 **MUST** be threat-modelled for a *malicious* sender — not merely a noisy one.
- **Rationale (the §0 headline of the consistency report, F-15).** P4's control-loop guards (sample-gate, hysteresis, cascade-brake) were designed against a *self-generated* signal (a box's own verdict stream). P7 reuses them wholesale against an *external, cross-project* feed. Statistical guards defend against **noise**; they do not **authenticate**. A patient attacker feeding gate-clearing, bounded-step poison defeats every guard. This is the classic "get the door right before it accrues history or a consumer" logic — the feed is the input to the flywheel, and its trust model, once P7 calibrates on it, is not cheaply reversible.
- **What is decided vs. left open.** *Decided:* the feed is authenticated, sender-as-principal, TB-7 threat-modelled — this is now a door, not a dial. *Left open (owner / P7-time, see §5):* the specific mechanism — **signed webhooks vs. mutually-authenticated pull** — and the per-provider ingestion rate-limit values.
- **Applied to:** [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) §3.4 (PHASE-GATE before P7); SPEC-NOTE on P7 §3.2 (`[Finding-1 / OQ-P7-1, accepted]`); companion **PN-11** (F-17 belt-and-suspenders, routed under DR-5). Elevation recorded in **ADR-0001**.
- **Door check:** does **not** reopen Doors 1–5. It adds a *new* pre-P7 gate on TB-7 (a trust boundary), leaving the five frozen doors untouched. It is ADR-worthy because it *creates* a one-way architectural commitment about P7's input trust model.

### DR-2 — Shadow-mode recommend-only defaults are ACCEPTED (Finding 4) `DECIDE-NOW` · additive, no door
- **Ruling.** The `mode ∈ {shadow, recommend-only, auto}` dial ([`shadow-mode.md`](shadow-mode.md)) is adopted. The four day-one auto-mutators — **A1** (P4 block-rate quarantine, statistical trigger), **A6** (P7 confidence recalibration), **A8** (P7 provider auto-promotion), **A9** (P7 fallback weaning) — **default to `recommend-only`.** Promotion to `auto` is a deliberate, per-mechanism, per-scope owner action, logged to the admin-audit trail (DR-4). The two systemic brakes (A5 fleet, A7 cascade) already alert-and-hold and are unchanged — they are the blessed pattern being generalized.
- **Rationale.** Assembled, the seven phases auto-mutate caller-visible state (routing / confidence / eligibility / spend) on day one with no human in the per-decision loop. A6 is the sharpest: it rewrites the `confidence` *value* external systems code against. Recommend-only makes "the machine started changing itself" an explicit, approvable event, not a silent threshold crossing.
- **Door check.** Additive — the `mode` dial is a new config key/column; A6 still writes only `confidence` as an int 0–100 (Door 1's *value*, never its *type*); no `status`/`reason`/`recommendation` value is minted (Door 4 intact); the LOG stays append-only (Door 2 — recalibration writes the separate versioned calibration store). **No door reopened → no ADR.**
- **Applied to:** [`shadow-mode.md`](shadow-mode.md) (accepted marker, §7) ; [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) §3.5 (the fresh-system regression PHASE-GATE); [`config-manifest.md`](config-manifest.md) P7 header already reflects recommend-only.

### DR-3 — Admin identity + control-plane audit log is a PHASE-GATE before P3 go-live (Finding 2 / PN-1) `DECIDE-NOW` (gate accepted) · additive, no door
- **Ruling.** Before **P3 go-live**, every *manual control-plane action* — node disable, global-fallback-kill, pause, shadow-mode promotion, budget change, breaker/brake confirmation, privacy opt-out flip — **MUST** write to an **append-only admin-audit log** carrying **per-actor identity** (who / what / when), distinct from the verdict LOG. It reuses the existing `node_audits` / `work_item_attempts` append-only pattern.
- **Rationale (F-11).** The control surface accretes across P3–P7 and is today gated by *one* Supabase service-role credential with no per-actor identity and no audit of *which human* changed what — while the celebrated append-only LOG audits *verdicts*, not *control-plane mutations*. Several controls are security-critical (opt-out flip = a privacy event; budget raise = financial; brake-confirm = the safety valve).
- **Scope guard.** This pass records the **gate**, not the mechanism. The admin-audit table/authz is **not** built here — it gates P3, not P1.
- **Applied to:** [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) §3.6 (PHASE-GATE before P3 go-live); SPEC-NOTE on P3 §6.4 and on [`kill-switch-ladder.md`](kill-switch-ladder.md) §4 (`[Finding-2 / PN-1, accepted]`).

### DR-4 — Queue plaintext must be backup-aware: a PHASE-GATE before P5 (Finding 3 / PN-3) `DECIDE-NOW` (gate accepted) · door-touching (affirms Door 3) → **[ADR-0002](adr/ADR-0002-queue-plaintext-backup-scope.md)**
- **Ruling.** Before **P5**, the queue's plaintext-at-rest column (`work_items`, the one bounded Door-3 exception) **MUST** be excluded from, or protected in, backup/PITR snapshots. The **mechanism is a P5-time decision** among: snapshot exclusion, separate-key at-rest encryption, or short snapshot-retention. Backups/PITR are to be **treated as containing live plaintext** for the retention window.
- **Rationale (F-7).** P1 froze "raw email never persisted" (Door 3); P5 opened a *bounded* exception with a correct in-transaction purge-on-settle. The gap is invisible to either phase alone: a snapshot copies live `work_items` plaintext that **no purge transaction ever touches**. The exception was designed backup-*unaware*.
- **Door check.** **Door 3 is NOT reopened.** P1's "no plaintext" promise and P5's bounded exception both stand verbatim; this ruling only records that the exception's *reasoning must reach the backup surface too*. ADR-0002 documents the tension at the seam where both promises meet, and affirms Door 3 unchanged.
- **Scope guard.** The protection mechanism is **not** built or chosen here — that is P5's call.
- **Applied to:** [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) §3.7 (PHASE-GATE before P5); SPEC-NOTE on P5 §3.3 cross-referencing P1 §3.3 / §3.2.1 (`[Finding-3 / PN-3, accepted]`).

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

Recorded in [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md); **none implemented here**:

| Gate | Blocks | Source |
|---|---|---|
| Outcome-feed **authenticated** (sender-as-principal, TB-7 threat-modelled) | before **P7** code | DR-1 / PN-2 / F-15 |
| **Admin-audit log** + per-actor identity for every control-plane action | before **P3** go-live | DR-3 / PN-1 / F-11 |
| **Backup-plaintext** protection for `work_items` (mechanism = P5 choice) | before **P5** | DR-4 / PN-3 / F-7 |
| **Shadow-mode regression**: fresh system = every in-scope mutator at default mode, zero live mutation | before whichever phase first ships each mutator (A1→P4, A6/A8/A9→P7) | DR-2 / PN-4 |
| Front-door auth beyond static token (per-caller keys + rotation) | before **P5** bulk | PN-13 / F-20 |

---

## 3. ADR index (door-touching decisions)

| ADR | Decision | Door referenced | Reopens it? |
|---|---|---|---|
| [ADR-0001](adr/ADR-0001-outcome-feed-authentication.md) | OQ-P7-1 elevated to a hard pre-P7 authentication door | *new* TB-7 trust gate (P7 input) | No — new gate, five frozen doors untouched |
| [ADR-0002](adr/ADR-0002-queue-plaintext-backup-scope.md) | Queue-plaintext backup-awareness gate at P5 | **Door 3** (email hashing / no-plaintext) | **No** — affirms Door 3 + the P5 bounded exception; adds a gate |

---

## 4. Five-door standing check (re-affirmed this pass)

No ruling in this log reopens any frozen door. Restated for record (full evidence: consistency report §2):

1. **/verify response shape (Door 1)** — untouched. A6 changes a `confidence` *value*, never its int-0–100 *type*. Additive only.
2. **LOG append-only (Door 2)** — untouched. PN-9 *hardens enforcement* (separate grant-admin cred) without changing the log contract; recalibration writes the separate versioned calibration store.
3. **Email hashing / no-plaintext (Door 3)** — untouched. DR-4 affirms the P5 bounded exception and gates its backup scope; the hashing scheme, pepper, and `bytea` key are unchanged.
4. **status / reason / recommendation vocabulary (Door 4)** — untouched. Zero values minted this pass; PN-11 adds only a DIAL (N) to the *already-reserved* `historical_bounce` gate.
5. **never-fork gem + node-list (Door 5)** — untouched. Nothing here patches the gem or hardcodes an endpoint.

---

## 5. Explicitly NOT decided (surfaced, per the rules — owner / phase-time calls)

Applying the accepted items surfaced these sub-decisions the docs never pinned. **They are flagged, not resolved:**

- **[OQ-P7-1 mechanism]** signed webhooks **vs.** mutually-authenticated pull — DR-1 fixes *that* the feed is authenticated, not *how*. Owner / P7-time.
- **[PN-11 / F-17]** the value of **N** (independent invalid-signal outcomes before the historical-bounce gate fires) — a DIAL, owner-set; and whether the ≥N guard is *always on* or *only when the feed is unauthenticated* (DR-1 makes authentication mandatory, so ≥N becomes defense-in-depth rather than the sole guard). Owner/P7-time.
- **[PN-14a]** the **home of the `(message_id, event)` dedup guard** — P7 §3.4 locks the dedup *key*; it does not pin whether the guard is a DB `UNIQUE` on `send_outcomes`, an app-level append-guard, or a staging table. Recorded as a note; **not** decided (deciding it would be a new schema commitment).
- **[PN-14b]** **`residue_class` typed column vs. staying in `signals.external.residue_class` jsonb** — a calibration group-by dimension; report §5 note 3 raised it as open. Not decided.
- **[PN-15]** for `signals.platform` (FLAG-A) and the catch-all re-probe cadence (FLAG-C): **name an owning phase vs. mark Phase-8+** — PN-15 requires resolving the dangling status, but *which* resolution is a product-scope call. Not decided.
- **[PN-8 / F-12]** the **brake-hold timeout value** and its default-on-timeout behavior (hold vs. auto-release) for A5/A7 — the note records that it must be specified; the value/behavior is owner/phase-time.
- **[PN-9 / F-13]** whether the optional **hash-chain-to-external-WORM** tamper-evidence checkpoint is in scope, or grant-based immutability with a documented residual is accepted — already an owner call in checklist §1.2; unchanged.
- **[DR-4 mechanism]** the backup-protection mechanism (exclusion / separate-key encryption / short-retention) — P5-time.

---

## 6. Owner-OQ register (added Phase 0 — pointers, not re-decisions)

Phase 0 was asked to capture the outstanding owner open-questions and *their resolutions as they land*.
The named OQs below were **already decided** in each phase's Appendix D sign-off; this register makes
them **discoverable in one place** and links each to its owning ruling. **Nothing is re-decided here**;
the §5 parked sub-decisions above remain parked (they are the genuinely-open ones).

| OQ | Question | Status | Resolved / recorded in |
|---|---|---|---|
| **OQ-P6-3** | Vendor `invalid` acceptance asymmetry | **DECIDED** (asymmetric): accept as `invalid` only on retry-exhausted / gave-up-capacity; downgrade to `risky` on catch-all/`:mx`. | P6 §3.4 + Appendix D.2; [`config-manifest.md`](config-manifest.md) §6.2; pre-takeoff-checklist §2 |
| **OQ-P6-6** | Platform disclosure for vendor egress | **DECIDED**: disclose + opt-out (per-policy `external` block = the opt-out lever); PN-6 asserts platform-opt-out ↔ policy-without-`external` maps 1:1 at integration. Positioning call flagged. | P6 §7.5 + Appendix D.3; [[truemail-platform-privacy-egress-contradiction]] |
| **OQ-P7-2** | Historical-bounce gate TTL + reversibility | **DECIDED**: TTL-windowed hard gate (6–12 mo, DIAL) on confirmed `mailbox_not_found` only, degrading to a confidence penalty after. PN-11 adds a ≥N defense-in-depth DIAL behind DR-1. | P7 §4.6 + Appendix D.3 |
| **OQ-P7-8** | "Your sends train our accuracy" disclosure | **DECIDED**: surface it (disclose-don't-hide); brain exposes the seam, platform decides terms. | P7 §14 + Appendix D.6 |
| **P1 cheap-layers mock-vs-live** | Do the cheap layers call a mocked engine, or run brain-native? | **DECIDED**: brain-native for syntax + MX/DNS/null-MX (the cheap path); the engine is used only for the SMTP path (P2). → the Go brain takes **no** truemail dependency. Recorded as **ADR-0004** (a structural, hard-to-reverse choice). | P1 §6 / OQ-6; **[ADR-0004](adr/ADR-0004-cheap-layers-brain-native.md)** |

The four remaining P1-named OQs the specs still hold as *decided-with-standing-caveat* (OQ-1 pepper
custody, OQ-7 disposable-risky, OQ-3 local-part case, OQ-23 single-default) are recorded in P1 Appendix
C and restated as rules in [`../PROJECT_RULES.md`](../PROJECT_RULES.md); they are not re-opened.

### 6.1 Phase-0 sign-off resolutions (owner ruling 2026-07-17)

The Phase-0 closeout produced [`phase-0-signoff-sheet.md`](phase-0-signoff-sheet.md) — the six
spec-vs-reality deltas (GT-1…GT-6), the Door-3 local-part fix, and the LICENSE. On **2026-07-17**
the owner ruled **"accept all + record"** on the deltas and the fix, and **deferred** LICENSE.
Recorded here so the Phase-1 prerequisite ("sign-off resolved in DECISIONS.md") is satisfied; the
sheet's boxes are checked to match. **Two of these touch door territory** and are flagged loudest.

| Item | Touches a door? | Ruling (2026-07-17) | Recorded in |
|---|---|---|---|
| **GT-1** engine `3.3.0`→`3.3.1` | No (illustrative spec string; provenance only) | **ACCEPTED** — pin 3.3.1 | [ADR-0003](adr/ADR-0003-truemail-engine-pin.md) |
| **GT-2** rack `0.10.0`→`0.9.0` | No (no `0.10.0` tag exists; provenance only) | **ACCEPTED** — fork base `v0.9.0` | [FORK.md](../verifier-node/truemail-rack/FORK.md) |
| **GT-3** `net-smtp` `~>0.4.0`→`~>0.5.0` | **Door 5 territory** — fork-side dep edit, gem byte-for-byte unmodified | **ACCEPTED** — forced by the exact pin; rack-side edit, not a gem patch | [ADR-0003](adr/ADR-0003-truemail-engine-pin.md) §"One forced consequence"; FORK.md #2 |
| **GT-4** `host_audit` live shape | No (informs P3 gate; gate design frozen) | **ACCEPTED** — gate anchor (`current_host_ip`+`warnings`) confirmed live | [truemail-ground-truth.md](truemail-ground-truth.md) §3.3 |
| **GT-5** `smtp_mock` accept-all (P2 GAP-14) | No (P2 dev-substrate property) | **ACCEPTED** — scope call: real-MX path proven at P0; seeded-probe fixture is a P2 verification | truemail-ground-truth.md §3.4 |
| **GT-6** `host_audit` extra `date`/`configuration` keys | No (additive; gate reads only `warnings`) | **ACCEPTED** — additive/ignorable, record only | truemail-ground-truth.md §3.3 |
| **Door-3 fix** — refuse non-ASCII local part until NFC (P1) | **Door 3** — a *tightening* that affirms the frozen scheme (refuse > mis-hash) | **BLESSED** — safe direction, locked by `TestNonASCIILocalPartRefused`, no scheme change; P1 wires NFC/IDNA and may relax both guards *together* | **[ADR-0006](adr/ADR-0006-door3-nonascii-localpart-refusal.md)** |
| **LICENSE** (project's own) | No (legal/distribution posture; no code impact) | **DEFERRED** — stays unasserted; third-party MIT respected in [NOTICE](../NOTICE). Pick one before any external distribution. Does not block local Phase 1. | [phase-0-signoff-sheet.md](phase-0-signoff-sheet.md) §C |

**Door standing after these rulings:** unchanged. GT-3 is a fork-side dependency resolution (the gem
is unmodified — Door 5 intact); the Door-3 blessing is a refuse-not-mint tightening that leaves the
frozen `norm/v1+hmac-sha256/v1` scheme verbatim (Door 3 intact). No frozen door is reopened. The **§5
parked sub-decisions remain parked** — this ruling touches none of them.

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
parked sub-decisions remain parked. This is the only re-decision the owner authorized in this pass.

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
box-side behavior (Door 5). The GAP-37 lease-CAS invariant is now built ([[truemail-lease-cas-exactly-once-settlement]]). The §5 parked sub-decisions unrelated to P5 remain parked.

---

*This log records acceptances, routes, and (Phase 0) an owner-OQ pointer register; it builds nothing
and reopens nothing. Line-by-line placement is in [`consolidation-application-report.md`](consolidation-application-report.md).*
