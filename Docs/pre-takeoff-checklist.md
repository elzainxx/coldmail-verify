# Pre-Takeoff Checklist — Before Phase 1 Code Starts

**Status:** DRAFT v0.1 — cross-phase consolidation pass (planning, not code)
**Purpose:** everything that must be **true, decided, or spiked** before implementation begins — separated into what gates *Phase 1 code* vs what gates *a specific later phase*. Sourced from all seven specs + the five cross-phase lenses.
**Companions:** [`config-manifest.md`](config-manifest.md) · [`kill-switch-ladder.md`](kill-switch-ladder.md) · [`shadow-mode.md`](shadow-mode.md) · [`cross-phase-consistency-report.md`](cross-phase-consistency-report.md)

> **The founding rule this checklist protects:** the one-way doors (response shape, LOG, hashing, vocabulary, never-fork) must be right *before* they accrue history or a consumer. Everything on the "hard gate" list below either *is* a one-way door or *protects* one. Dials are deliberately **not** here — they can be turned later.

---

## 1. HARD GATES — must be resolved before Phase 1 code

These block the first line of implementation because they are one-way doors or protect irrecoverable data.

### 1.1 The pepper-custody runbook (protects Door 3) — **BLOCKS P1**
- [ ] **Generate the global HMAC pepper** and write the custody runbook (P1 §3.3.3, OQ-1 ELEVATED — the single most fragile artifact in the system).
- [ ] Runbook covers: generated once, stored **outside** the DB; redundant + **geographically-separate** + **access-audited** backups; rotation posture is **additive-only, "assume never rotated."**
- [ ] **Recovery is tested before P1 ships** (a DR drill, not a paper claim) — lose-pepper = every historical `email_hash` de-joins forever; leak-pepper = low-entropy addresses brute-forceable.
- [ ] **[Cross-phase, security F-9/F-19, PN-12 accepted]** Note the two leak vectors no phase closes: the **brain holds the pepper in memory** to compute HMACs (a core-dump is a leak) → include brain host-hardening + core-dump suppression in the runbook. The pepper's read-surface **grows** as P5 (`idempotency_key`) and P7 (`send_outcomes`) add `email_hash`-derived stores — the runbook is revisited at those phases, not just P1. *Add (PN-12):* an explicit **brain-compromise threat model** — the single brain holds the pepper (in memory), the sole service-role cred, all rate-limiter state, the drainer, and the recalibration writer; no phase threat-models "brain compromised." Brain host-hardening is as load-bearing as pepper custody (see §4).

### 1.2 Freeze the four contract doors on paper (Doors 1–4) — **BLOCKS P1**
- [ ] `/verify` response schema written + versioned `/v1/` (Door 1).
- [ ] LOG record schema written; append-only enforced by revoked UPDATE/DELETE grants (Door 2). **[Security F-13, PN-9 accepted]** *add* a separate DDL/grant-admin credential (break-glass, admin-audited per §3.6) so the service role can't silently rewrite history; **decide** whether a tamper-evidence checkpoint (hash-chain to external WORM) is in scope, or accept grant-based immutability with a documented residual (the decision stays owner's — flagged in [`DECISIONS.md`](DECISIONS.md) §5). Cross-ref P1 §3.2.1 note.
- [ ] Email-hashing scheme + normalization frozen: `norm/v1 + hmac-sha256/v1`, `bytea`, local-part lowercased, dots/plus rules (Door 3, OQ-3). Confirm `bytea` everywhere (verifications, send_outcomes, work_items, idempotency_key).
- [ ] status / reason / recommendation vocabularies fixed as enums, additive-only (Door 4). Confirm the reserved values later phases depend on exist now: `historical_bounce` (P7), `external_verified` (P6), the `smtp_skipped_reason` signal enum (P3/P4).

### 1.3 Node-list discipline (Door 5) — **BLOCKS P1**
- [ ] Router built against a **node LIST** (even length 1) behind a pluggable Strategy interface — never a hardcoded endpoint. This is a Phase-1 architecture property that Phase 4's "node #2 = +1 config line" depends on (GAP-31).
- [ ] CI gate that asserts the pinned `truemail` gem is unmodified (Door 5) — wired from the first fork.

### 1.4 The one deliberate schema decision that shapes the join forever — **BLOCKS P1**
- [ ] Create `send_outcomes` + the join table **empty now** (P1 §4.7), anchored on `verifications.id` (verdict-level, **not** address-level `email_hash` fan-out). Getting this wrong is a one-way door P7 cannot undo. **[Schema PN-14]** while sketching, add the `calibration` table column sketch and confirm the `send_outcomes(message_id, event)` dedup constraint's home, so P7's guarantees have a shape.

---

## 2. OWNER OPEN QUESTIONS — decisions required (not resolved here)

The prompt named four as outstanding. Each is surfaced with its blocking phase; **none is resolved in this pass.**

| OQ | Question | Blocks | Recommendation (owner decides) |
|---|---|---|---|
| **OQ-P6-3** | Vendor `invalid` acceptance asymmetry — accept as `invalid` only on retry-exhausted/gave-up-capacity; downgrade to `risky` on catch-all/`:mx`? | P6 | **Affirm the asymmetry.** A vendor "invalid" on an accept-all domain is itself a guess; accepting it irreversibly discards a real address. The spec's asymmetric default is the safe direction. |
| **OQ-P6-6** | Platform disclosure posture for vendor egress — disclose + opt-out? | P6 (+ cross-project platform) | **Disclose + opt-out (the spec's decision).** *Add* the F-4 integration assertion: the platform opt-out flag must map 1:1 to a policy-without-`external` in the brain, verified at integration — else the opt-out is honored in the brain but the platform could still route through a fallback-enabled policy. |
| **OQ-P7-2** | Historical-bounce gate TTL + reversibility | P7 | **Already DECIDED in the P7 sign-off:** hard-gate to `invalid/historical_bounce` only on confirmed `mailbox_not_found`, TTL-windowed 6–12 months, then degrade to a confidence penalty (a re-created mailbox recovers). **[Security F-17, PN-11 accepted]** *add:* require ≥N independent invalid-signal outcomes as **defense-in-depth behind the DR-1 authentication door** (whether ≥N is always-on or only-when-unauthenticated is flagged open in [`DECISIONS.md`](DECISIONS.md) §5). |
| **OQ-P7-8** | Platform "your sends train our accuracy" disclosure | P7 (+ cross-project) | **Surface it, don't bury it** (the spec's disclose-don't-hide posture). This is the second cross-project privacy seam (after OQ-P6-6); both are honored mechanically per-policy. |

**One elevated cross-phase decision the lenses add to this list:**
- **OQ-P7-1 (transport/auth of the outcome feed) — ELEVATE to a hard gate before *any* P7 code.** [Security F-15, the §0 headline] The feed drives automated recalibration/promotion/weaning; it must be an **authenticated** channel (signed webhooks or mutually-authenticated pull, sender as an identified principal), with TB-7 threat-modelled for a malicious sender. The statistical guards are necessary but not sufficient against adversarial input. *This is not a dial; it is a pre-P7 door.*

---

## 3. SPIKES & PHASE-GATES — resolved before the phase they gate

*§3.1–3.3 are **spikes** (investigations that must run). §3.4–3.8 are **phase-gates** accepted in the 2026-07-17 consolidation pass ([`DECISIONS.md`](DECISIONS.md)) — design conditions a named later phase must satisfy before its code ships. Neither is implemented now; recording a gate is not building the mechanism.*

### Spikes (investigations)

### 3.1 GAP-13 catch-all session-reuse spike — **now gates P3** (run pre-provisioning)
- [ ] **Run the catch-all-session spike before provisioning the real box** (memory: GAP-13 spike-before-real-box). Question: can the 2nd-RCPT catch-all probe be done in one session **wrapper-side, without patching the pinned gem**?
- [ ] If **yes** → Option A (same-session probe, stronger signal).
- [ ] If **no** → Option B (two sessions, documented weaker signal, 2× connections). **Door 5 outranks design elegance** — never patch the gem to get Option A.
- [ ] On a real IP this is also a **reputation-cost decision** (a 2nd RCPT per check costs connections/reputation), tracked against warmup. Ties to the P2 §5.4 / P5 §6.4 "session reuse" seam that terminates in this decision (SEAM FLAG-D).

### 3.2 Host-selection research (P3) — **gates P3, the real wall**
- [ ] **Pre-provisioning market research (OQ-P3-1, GAP-16):** find a VPS host that genuinely allows **outbound port 25** AND **settable PTR**, on a **clean IP**, at acceptable cost. Reclassified from a design item to a research gate — *"the narrow market is the wall, not the design."*
- [ ] Vet **before paying** (V1–V5 behaviorally checkable: egress-25 smoke test, PTR propagation, clean-IP on blocklists, egress==PTR). V6–V8 (ToS, uptime, region) are procurement judgment gates, not automatable.
- [ ] Decide the private-network mechanism per host (VLAN if co-located, else WireGuard, OQ-P3-12) — this becomes the control-plane perimeter at N boxes (security F-2).

### 3.3 Vendor DPA / due-diligence (P6) — **gates P6 GO-LIVE (not the build)**
> **Build status (2026-07-18):** the `external` layer is **BUILT and fail-verified locally against a
> mock vendor, DISABLED by default** (ADR-0012, [`phase-6-status.md`](phase-6-status.md)). This
> checklist now gates **enabling** it (turning a real vendor on), **not** the code — enabling is
> guarded in code by three independent switches (`TM_FALLBACK_MODE=enabled` AND
> `TM_EXTERNAL_DPA_CONFIRMED=true` AND a `TM_VENDOR_KEYS` entry), and a real vendor adapter is still
> PENDING-REAL-VENDOR (no live HTTP client ships this phase).
- [ ] The `external` layer **ships DISABLED until this §7.4 checklist passes** (DPA signed, retention understood, TLS, key custody). Enabling pre-DPA = unlawful sub-processing. **Setting `TM_EXTERNAL_DPA_CONFIRMED=true` is the operator's affirmation this checklist passed** — do not set it before.
- [ ] **[Security F-5, PN-7 accepted]** the checklist records vendor retention; *add* a cumulative-distinct-addresses-egressed metric + a periodic vendor-deletion process (retention is a standing exposure, not a one-time tick). The `external_calls` table is queryable by `email_hash` for SAR/deletion (PN-10). Cross-ref P6 §7.4 note.
- [ ] Implement + wire a real `external.Vendor` adapter (one at launch, OQ-P6-8) in `buildExternal`; set the budget ceilings (`TM_EXTERNAL_DAILY_CAP`/`_MONTHLY_CAP`/`_PER_JOB_CAP`/`_RATE_CEILING`) conservatively from observed residue-tail volume before enabling (OQ-P6-5).

### Phase-gates (design conditions accepted 2026-07-17 — [`DECISIONS.md`](DECISIONS.md))

### 3.4 Outcome-feed AUTHENTICATION — **gates P7** (hard pre-P7 door) [DR-1 / PN-2 / F-15 / ADR-0001 / ADR-0013] ✅ DESIGN DISCHARGED (P7, 2026-07-19); LIVE still gated on the sender
- [x] **Before any P7 code:** the outcome feed (TB-7) is an **authenticated channel** with the **sender as an identified principal**, and **TB-7 is threat-modelled for a *malicious* sender** (forged-outcome poisoning of recalibration / promotion / weaning / historical-bounce gate). Elevated from a dial to a door — see [ADR-0001](adr/ADR-0001-outcome-feed-authentication.md). **Discharged in [ADR-0013](adr/ADR-0013-outcome-feed-auth-mechanism-and-tb7-threat-model.md): the TB-7 threat model (Part A) + the mechanism decision (Part B).**
- [x] Ingestion is **rate-limited per-provider**; the P7 §10 classifier-distribution monitor is treated as a **security** signal. (Recorded in ADR-0013 §A.3 layer 8; the per-provider rate-limit *values* are DIALs.)
- [x] **Mechanism DECIDED (was flagged open):** **mutually-authenticated pull (mTLS)** — P7 is the authenticated client (no new inbound surface on the pepper-holding brain), native backpressure, cursor+watermark replay-safety. Built behind a swappable `OutcomeSource` interface (reversible to signed-webhooks by one adapter); `SimulatedSource` for local build, fail-closed (no credential ⇒ cold-start). The ≥N value (PN-11) remains a DIAL. Recommend-only (DR-2) mitigates but does **not** authenticate — both required (ADR-0013 §A.3 layer 6).
- [ ] **LIVE gate (still open, cross-project):** the sender ("Feature 2") does not exist yet (GAP-48) — no *live* ingestion until it implements its half of the mTLS feed + the trust anchor/client credential are provisioned. Honestly **PENDING-REAL-SENDER**; local build proceeds against `SimulatedSource`.

### 3.5 Shadow-mode fresh-system REGRESSION — **gates the first phase to ship each mutator** [DR-2 / PN-4]
- [ ] On a **fresh system**, assert every in-scope mutator sits at its **default mode** and performs **zero live mutation**: zero confidence buckets applied (A6), zero providers promoted (A8), zero buckets weaned (A9), block-rate quarantine emitting **recommendations only** (A1). Behavior identical to end-of-P6 until a human promotes a mechanism to `auto`. (See [`shadow-mode.md`](shadow-mode.md) §5.)
- [ ] Wire this as a **regression test** in whichever phase first ships each mutator (**A1 → P4**; **A6/A8/A9 → P7**). Regression = any mechanism silently mutating live state at mode-default.

### 3.6 Admin identity + control-plane AUDIT LOG — **gates P3 go-live** [DR-3 / PN-1 / F-11]
- [ ] **Before P3 go-live:** every manual control-plane action (node disable, global-fallback-kill, pause, shadow-mode promotion, budget change, breaker/brake confirm, privacy opt-out flip) writes to an **append-only admin-audit log** with **per-actor identity** (who/what/when), distinct from the verdict LOG — reusing the `node_audits` / `work_item_attempts` append-only pattern.
- [ ] **Do NOT build it now** — it gates P3, not P1. (Cross-ref P3 §6.4 note, kill-switch §4, and the global-fallback-kill definition PN-16.)

### 3.7 Backup-plaintext protection for `work_items` — **gates P5** [DR-4 / PN-3 / F-7 / ADR-0002] ✅ RESOLVED (P5, 2026-07-18)
- [x] **Before P5:** the queue's plaintext column must be excluded from / protected in backup & PITR snapshots (a snapshot copies live plaintext no purge transaction touches). Treat backups as containing live plaintext for the retention window.
- [x] **Mechanism chosen (owner, 2026-07-18): SEPARATE-KEY at-rest encryption (key ≠ pepper)** — the only locally fail-verifiable option (ADR-0002's restore-and-assert, done as a `dump-the-row → only-ciphertext` test). Built as `internal/queue/addrcipher.go` (AES-256-GCM, AAD-bound to the row id, rotation overlap, fail-closed key length) + a separate secret `TM_QUEUE_ADDR_KEY` loaded distinctly from the pepper. Fail-verified: neuter `Seal` → `TestAddrCipher_CiphertextContainsNoPlaintext` fails; restored green. **Door 3 not reopened** — the bounded exception is now (1) transactionally purged on settle AND (2) encrypted at rest.
- [x] The `work_items.addr_ciphertext` column holds only ciphertext; the plaintext is purged in the settlement transaction (§5.3 step 4) AND encrypted at rest for any backup taken while an item is live. Residuals stated honestly in the queue custody note (backups hold the encrypted column, safe iff the key isn't in the same snapshot; key-holder == plaintext-holder on brain compromise — F-9/F-19/PN-12).

### 3.8 Front-door auth beyond a static token — **gates P5 bulk** [PN-13 / F-20] ✅ RESOLVED (P5, 2026-07-18)
- [x] **Before P5 bulk ships:** upgrade the front-door bearer from a single static shared token to **per-caller keys + rotation** — the same endpoint becomes the bulk-PII submission surface at P5. (Cross-ref P5 §7.5 note.)
- [x] Built as the `api_callers` table (migration 0010: `key_sha256` stored HASHED, validity window `[not_before, not_after)` for zero-downtime rotation overlap; `cp_runtime` SELECT-only, issuance/rotation `cp_admin`/DR-3) + `store.LookupCaller` + the `api.authenticate` upgrade (sha256 lookup + `crypto/subtle` constant-time static compare). The bulk-PII surface REQUIRES a per-caller key; the sync `/v1/verify` also accepts the legacy static token during transition (backward-compat — the existing `TestAuthEnforcedInProduction` still passes). `caller_id` is key-derived (trusted) and kept distinct from the body-supplied `caller_ref` (untrusted, GAP-36) — auth is header-only so **no Door-1/Door-4 change**. Fail-verified: neuter the validity window → the rotated-out-key case fails; neuter the bulk gate → the static-token-on-bulk case fails; both restored green.

---

## 4. PRE-CODE SETUP (not doors, but needed before P1 runs)

- [ ] Supabase project created; tables `verifications`, `cache`, `catchall_domains`, `disposable_domains`, `nodes`, `policies` (+ empty `send_outcomes`/join per §1.4). Access model: service-role only in P1, RLS deferred (OQ-20) — **[Security F-11, PN-1 accepted → §3.6]** the admin authn/authz + admin-audit-log model is an **accepted phase-gate before P3 go-live** (§3.6), since the control surface grows every phase. Also stage the brain-host-hardening runbook (PN-12, §1.1/§4).
- [ ] Fork `truemail-rack`, pin the `truemail` gem, get the stock container hitting `/validate` locally (baseline).
- [ ] Brain skeleton: token-auth front door, `/v1/verify` stub, node registry from config list, Strategy router, Supabase client.
- [ ] Ship **one** policy (`cold-email`, cheapest-sufficient), structured as an open object so P4/P6/P7 keys slot in (OQ-25).
- [x] `verification_raw` partition **creation** is automated (ADR-0017, migration 0014; dial `TM_RAW_PARTITION_MONTHS_AHEAD`). Found 2026-11-07: until then only 2026-07..09 existed and every SMTP verdict write failed from 2026-10-01.
- [ ] Retention plan for the LOG (biggest data grower) + `verification_raw` partition **drop** (retention). Creation is automated; `DROP PARTITION` after ~90 days is still a manual admin action.

---

## 5. Decisions that are SAFE to defer (explicitly NOT pre-takeoff)

To keep scope honest — these are dials or later-phase choices, deliberately *not* blocking P1:
- All numeric dials with no default (config-manifest §8.2): rate limits, thresholds, windows, sample-gates, caps, TTLs, cadences. Tune with data.
- Queue substrate details (P5), fallback vendor selection (P6, one at launch), multi-vendor tournament (Phase-8+).
- Shadow-mode → auto promotion per-mechanism (starts recommend-only by default; promote later).
- Multi-brain HA (single-brain assumed, GAP-25/OQ-P4-8), engine-swap (Door-5-preserving future), tier-scoped policies — all named Phase-8+ or future seams (SEAM FLAG-B).

---

## 6. One-glance gate summary

```
BEFORE P1 CODE (hard doors):
  ☐ Pepper generated + custody runbook + recovery drill        (Door 3, F-9)
  ☐ Response / LOG / hashing / vocabulary schemas frozen        (Doors 1–4)
  ☐ Node-list + Strategy router; gem-unmodified CI gate         (Door 5, GAP-31)
  ☐ send_outcomes + join created empty on verifications.id      (§4.7 one-way)

DECISIONS REQUIRED (owner OQs):
  ☐ OQ-P6-3 vendor-invalid asymmetry   ☐ OQ-P6-6 disclose+opt-out
  ☐ OQ-P7-2 historical-bounce TTL (decided; +F-17 ≥N-outcome gate, PN-11)
  ☐ OQ-P7-8 sends-train-accuracy disclosure
  ✅ OQ-P7-1 outcome-feed AUTHENTICATION  ← ELEVATED to a hard pre-P7 door (DR-1, ADR-0001) → gate §3.4

SPIKES (before the phase they gate):
  ☐ GAP-13 catch-all session-reuse spike     → gates P3 (pre-provisioning)
  ☐ Host-selection research (port25 ∧ PTR ∧ clean) → gates P3 (the wall)
  ☐ Vendor DPA / due-diligence checklist      → gates P6

PHASE-GATES (accepted 2026-07-17, DECISIONS.md — record only, not built):
  ◐ Outcome-feed AUTHENTICATION (TB-7 threat-modelled) → DESIGN DISCHARGED P7 2026-07-19 §3.4 (DR-1/ADR-0001/ADR-0013: mTLS pull); LIVE still gated on the sender (PENDING-REAL-SENDER)
  ☐ Shadow-mode fresh-system regression (zero live mutation) → A1→P4, A6/8/9→P7  §3.5 (DR-2)
  ☐ Admin identity + control-plane audit log   → before P3 go-live    §3.6 (DR-3, PN-1/F-11)
  ✅ Backup-plaintext protection for work_items → RESOLVED P5 2026-07-18 §3.7 (separate-key encryption, DR-4/ADR-0002)
  ✅ Front-door auth beyond static token        → RESOLVED P5 2026-07-18 §3.8 (per-caller keys+rotation, PN-13/F-20)
```

*This checklist enumerates gates and resolves none of the owner OQs. It rewrites no frozen spec: the phase-gates in §3.4–3.8 were **accepted** in the 2026-07-17 consolidation ([`DECISIONS.md`](DECISIONS.md)) and are recorded here as conditions, **not implemented**. The 16 proposed notes (PN-1…PN-16), all additive, are accepted and routed to their homes — full placement in [`consolidation-application-report.md`](consolidation-application-report.md); origin in [`cross-phase-consistency-report.md`](cross-phase-consistency-report.md) §7.*
