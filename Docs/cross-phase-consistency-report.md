# Cross-Phase Consistency Report — Truemail (Phases 1–7)

**Status:** DRAFT v0.1 — the review that only makes sense with all seven specs in hand (planning, not code)
**Lenses run:** architect-review + backend-architect (SEAMs, one-way doors, auto-mutate) · database-architect (schema coherence) · senior-security + vulnerability-scanner (trust boundaries, egress) · senior-qa (behavioral test design) · code-reviewer (against described contracts).
**Companions:** [`config-manifest.md`](config-manifest.md) · [`kill-switch-ladder.md`](kill-switch-ladder.md) · [`shadow-mode.md`](shadow-mode.md) · [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md)

> **Rules honored:** flag, don't silently fix. Every contradiction is logged with the two phases that disagree. No frozen spec is rewritten. Additive suggestions are labelled **PROPOSED NOTE (owner sign-off)**. Owner OQs are not resolved here.

---

## 0. Headline — what no single-phase review could see

Four findings emerge *only* when all seven specs are held together. In severity order:

1. **[SEC-HIGH] The Phase-7 feedback loop imports an external, cross-project, *unauthenticated* signal into an automated decision loop whose only guards were designed for noise, not adversaries.** P4's sample-gate / hysteresis / cascade-brake were built against a *self-generated* signal (a box's own verdict stream). P7 reuses them wholesale against the sender's outcome feed (OQ-P7-1, transport undecided). Statistical guards defend against *noise*; they do not *authenticate*. A patient attacker feeding gate-clearing, bounded-step poison defeats every guard. → §1 (F-15/16/17).

2. **[SEC-HIGH] The admin/control surface is death-by-a-thousand-toggles.** Each phase adds one or two operator controls (kill-switches, policy versions, vendor budget, provider promotion, privacy opt-out, safety-brake overrides) and scopes auth/audit out. Assembled, it's a broad control plane gated by *one* Supabase service-role credential, with no per-actor identity and no admin-audit log — while the celebrated append-only LOG audits *verdicts*, not *control-plane mutations*. → §1 (F-11).

3. **[SEC-HIGH] The one plaintext-at-rest store (P5 `work_items`) is safe in the transaction path but silent on DB backups/snapshots.** P1 froze "raw email never persisted"; P5 opened a bounded exception (purge-on-settle); the **backup-exposure gap only appears holding P1's promise and P5's exception together** — a snapshot copies live plaintext no purge transaction ever touches. → §1 (F-7).

4. **[ARCH] Four mechanisms auto-mutate routing/confidence/spend on day one; the specs already ship the correct antidote for the two *systemic* ones but not the four *per-decision* ones.** Resolved by [`shadow-mode.md`](shadow-mode.md) (recommend-only defaults for A1/A6/A8/A9). → §4.

**The reassuring headline:** the *structure* is sound. All five one-way doors held additive across all seven phases (§2). Every SEAM promised by P1–P6 is consumed or consciously deferred — no silent drops (§3). The schema is coherent across all seven — zero contradictions (§5). The load-bearing lease+CAS invariant is propagated correctly to all three write paths (§1 F-18). The emergent risks are about **operational seams** (auth, audit, egress custody, feedback-loop integrity), not about the design skeleton.

---

## 1. Security & vulnerability findings (senior-security + vulnerability-scanner)

Full trust-boundary map and 21 findings. Severity: **HIGH** (3), **MED** (11), **LOW** (3), **INFO/affirming** (4).

### 1.1 Trust boundaries & the three genuine PII-egress points
Ten boundaries (TB-1…TB-10). The three points where real (non-hashed) addresses exist:
- **TB-3** — address on the SMTP wire to a provider MX (internal infra, P3 §6.3).
- **TB-6** — address to a third-party vendor (leaves the building, P6 §7).
- **TB-4 / `work_items`** — plaintext at rest in the queue (P5 §3.3).

Every *other* durable store is hash-only (Door 3). This concentration is a feature; the HIGH findings are about the seams between these points.

### 1.2 HIGH findings
- **F-7 · Queue plaintext + DB backups · cross-phase.** `work_items` holds raw addresses for work-not-yet-done (P5 §3.3, the one bounded Door-3 exception, OQ-P5-9). Safety hinges on the purge being *inside the settlement CAS transaction* — which the spec gets right. But a greylisted item lives in plaintext up to `max_retry_age` (~6h), a 100k bulk job holds a large plaintext working set, and **no phase addresses plaintext in DB backups/PITR snapshots** — a snapshot is a copy of `work_items` no purge ever touches. *PROPOSED NOTE:* treat backups/snapshots as containing live plaintext for the retention window (encrypt + access-control); consider encrypting the `work_items` plaintext column at rest with a key separate from the pepper. (P1 §3.3 froze "no plaintext"; P5 §3.3 opened the exception — the gap is only visible holding both.)
- **F-11 · Unauthenticated/unaudited admin surface · cross-phase.** The control surface accretes across P3–P7 (node enable, `/config/reload`, quarantine, policy `rate_limits`, per-caller limits, `external` enable = the GDPR opt-out lever, budget dials, breaker, provider promotion, weaning, historical-bounce gate, cascade-brake confirmation). All gated by one service-role credential (P1 OQ-20, RLS deferred), no per-actor identity, no audit trail of *which human* changed what. Several are security-critical (opt-out flip = privacy event; budget raise = financial; brake-confirm = the safety valve). *PROPOSED NOTE:* before P3 go-live, define admin authn/authz + an append-only admin-audit log (who/what/when) distinct from the verdict LOG — reuse the `node_audits`/`work_item_attempts` append-only pattern. Additive, touches no door.
- **F-15 · Outcome-ingestion poisoning + undecided auth · cross-phase.** (The §0 headline.) TB-7 recalibrates confidence + drives promotion/weaning from a separate project over an undecided, unspecified-auth transport. Forgeable `mailbox_not_found` → drive confidence down / force fallback spend up / permanently blacklist real addresses via the historical-bounce gate; forgeable `delivered`/`mailbox_full` → inflate trust → trigger auto-promotion → system *stops verifying* that provider → mass false-`valid`. The guards are for stability, not integrity. *PROPOSED NOTE (pre-P7):* decide OQ-P7-1 as an **authenticated** channel (signed webhooks or mutually-authenticated pull, sender as identified principal); threat model TB-7 to include a malicious sender; elevate the P7 §10 classifier-distribution monitor to a *security* signal; rate-limit ingestion per-provider.

### 1.3 MED findings (condensed)
- **F-1 · Node-token rotation has no lifecycle owner.** Each phase hands token discipline forward as "already handled"; no phase specifies rotation cadence, revocation-propagation, or revoke-vs-drain ordering at queue scale. *NOTE:* rotation triggers `/config/reload`; revocation atomic with quarantine; router excludes a revoked-token node immediately.
- **F-2 · Network isolation is load-bearing and provider-heterogeneous.** Isolation is primary, token secondary (P3 §6.2) — correct — but the mechanism is chosen per box-provider (WireGuard vs VLAN), and at N boxes the mesh is the whole control-plane perimeter with no fleet-wide posture check. *NOTE:* fleet-wide continuous external-scan assertion (extend P4 §8).
- **F-4 · First third-party PII egress; disclosure decision is cross-project.** P6 handles the mechanism well (address-only, residue-only, DPA-gated, TLS); the residual is that the *honoring* of opt-out lives in the out-of-scope platform (GAP-45, OQ-P6-6 decided disclose+opt-out). *NOTE:* contractually assert platform opt-out ↔ policy-without-`external` maps 1:1, verified at integration.
- **F-5 · Vendor retention is an uncontrolled downstream store.** The §7.4 checklist *records* vendor retention but there's no mechanical control/audit; over years the cumulative egressed set is unbounded and un-deletable by us. *NOTE:* add a cumulative-distinct-addresses-egressed metric + periodic vendor-deletion process.
- **F-10 · Pepper *loss* silently de-joins the flywheel.** Beyond the known cache/dedup catastrophe, pepper loss/rotation silently breaks the P7 historical-bounce gate's ability to recognize a known-bad address — a data-integrity finding, not just availability.
- **F-12 · Human-in-the-loop valves assume a reachable, authorized human.** A5/A7 refuse to act and require a human, but no phase specifies on-call ownership, confirm-authz, or brake-hold timeout. An alerting outage neutralizes both safety valves. *NOTE:* specify brake-hold timeout/default + authenticated confirm; monitor alert-pipeline health.
- **F-13 · Append-only is a grant, not WORM.** Immutability is `REVOKE UPDATE/DELETE` on one service role (P1 §3.2.1) — whoever holds the Supabase owner cred can rewrite history (including calibration ground-truth) with no tamper-evidence. *NOTE:* separate DDL/grant-admin cred with break-glass audit; consider a periodic hash-chain checkpoint to external WORM.
- **F-14 · LOG audits verdicts, not control-plane or egress.** No queryable audit of *which addresses egressed to the vendor when* (needed for SAR/deletion). *NOTE:* confirm the P6 §6.1 external-call transcript is retained + queryable by `email_hash` for data-subject requests.
- **F-16 · Auto-promotion self-disables verification** (see shadow-mode A8). *NOTE:* gate behind human confirmation.
- **F-17 · Historical-bounce gate can blacklist a real address from one poisoned outcome** (ties to F-15). *NOTE:* require ≥N independent invalid-signal outcomes before the gate fires.
- **F-19 · Single brain = crown-jewel concentration.** Holds pepper (in memory), the sole service-role cred, all rate-limiter state, the drainer, the recalibration writer. No phase threat-models "brain compromised." *NOTE:* explicit brain-compromise threat model; brain host hardening as critical as pepper custody.

### 1.4 LOW & affirming
- **F-6 (LOW)** vendor API key has no rotation story (leak = budget-DoS + billed enumeration oracle). **F-8 (LOW)** confirm no `raw`/transcript blob persists the plaintext *target* address (probe fakes + verifier identity are accepted per OQ-P2-5). **F-20 (LOW)** static front-door bearer token guards the bulk-PII endpoint by P5; upgrade to per-caller keys + rotation before bulk ships.
- **AFFIRMING:** **F-3** compromised-box blast radius is genuinely small and the design earns it (box holds no DB creds, no pepper, no queue/pool state). **F-9** pepper custody is handled with unusual rigor. **F-18** the lease+CAS exactly-once invariant is propagated correctly to queue (P5)/spend (P6)/calibration (P7). **F-21** `verifier_ip` integrity (GAP-24) is the security-load-bearing invariant for failure isolation, and P3 correctly makes the brain registry authoritative + cross-checked.

---

## 2. One-way-door additivity check (architect-review) — ALL FIVE HELD

Verdict: **no phase reopened any of the five frozen doors.** Every phase is additive-only; the only PII exceptions (plaintext in queue / to vendor) are *governed*, not door changes.

| Door | Phases that touched it | Additive? | Evidence |
|---|---|---|---|
| **1 — /verify response shape** | P2, P3, P4, P5, P6, P7 | **YES** | Only new `signals` keys; zero field removal/rename/type change. P6 adds `signals.external.*` + first populates reserved `raw` (no new top-level field; `meta.node_id`/`verifier_ip`=`null` = honest existing-field value). P7 confidence = different *value*, same int 0–100 *type*. Bulk results byte-identical verdict objects (P5 §7.4). |
| **2 — LOG append-only** | P2, P3, P4, P5, P6, P7 | **YES** | Transcripts → `verification_raw` sidecar (P2); settlement writes one row, reclaim no-ops (P5 GAP-33); P7 recalibration writes a *separate* versioned `calibration` store, `verifications` read-only. |
| **3 — email hashing** | scheme untouched; P5 bounded plaintext, P6 vendor egress | **YES (scheme intact; exceptions governed)** | Algorithm/pepper/bytea key never changed. P5 §3.3 plaintext purged transactionally on settle; P6 §7.3 reuses it, adds no new store. GAP-40/32b flag these as the *only* plaintext surfaces. |
| **4 — status/reason/recommendation vocab** | P2–P7 | **YES — the critical confirmation** | P2/P3: zero new values (all reserved in P1 §3.4.3). P4: `rate_limited` is a value on the open `smtp_skipped_reason` *signals* enum, **not** verdict vocab. P5: `complete_with_unknowns` is a *job-status* string, separate namespace. P6: already-reserved `external_verified`. **P7: uses ONLY the reserved `reason=historical_bounce`; adds/renames ZERO** — its three calibration classes are an internal namespace, never minted as verdict vocab. |
| **5 — never-fork gem + node-list** | P2 (fork rack only), P3–P7 | **YES** | CI-asserted-unmodified gem; node #2 = +1 config line (P4 §7); vendor = brain-side HTTP, never a node (P6 §2.3); P7 entirely brain-side. GAP-13 remains an *open pre-provisioning spike*, resolved by Door 5 outranking elegance — never a door reopening. |

**Two requested confirmations:** (1) **P7 moved a dial, not the vocabulary — CONFIRMED** (cleanest Door-4 posture in the arc). (2) **P6 fallback did NOT add response-shape fields non-additively — CONFIRMED** (`signals.external.*` under the open bag; no new top-level field).

---

## 3. SEAM consumption ledger (architect-review) — no silent drops

Every `SEAM →` promised by P1–P6 traces to a consumer or a conscious deferral. **No consumer references a seam that was never promised.** Full ledger abbreviated to the notable rows + the four flags.

**Consumed (representative):** P1 bulk-handle → P5 §7 · P1 `send_outcomes`/join → P7 §3 (on `verifications.id`) · P1 `Strategy` interface → P4 §3 (two impls, unchanged interface) · P1 confidence-recalibration → P7 §5 · P1 policy open-object keys → P4 `rate_limits` + P6 `external` + P7 historical-bounce gate (all three) · P2 `/audit` → P3 §4.4 gate · P2 greylist requeue shape → P5 §4 · P3 `node_audits` → P4 §5.4 hysteresis · P4 `rate_limited` backpressure → P5 §4.6 · P5 retry-exhausted residue → P6 §4.1 · P6 `external_verified` separable provenance + pre-written weaning query → P7 §9.

**The four flags (flagged, not fixed):**
- **FLAG-A (true dangling):** `signals.platform` (P1 §3.1.3, reserved "→ later Type-2 layers") is **never populated by any phase 1–7**. Harmless (open `signals` bag tolerates absence) but it's the one reserved response-key with zero downstream owner. *PROPOSED NOTE:* name the owning phase or mark it explicitly Phase-8+.
- **FLAG-B (intended dangling):** engine-swap boundary (P1 §3.5.2) and tier-scoped default (P1 §4.6/OQ-23) are forward seams with no numbered consumer in the arc. Correctly future.
- **FLAG-C (partial):** catch-all re-probe cadence (P2 §5.6) → "relates to P4 pacing" is never given an explicit scheduler. `catchall_domains.expires_at` exists but no phase specifies the re-probe loop that reads it. *PROPOSED NOTE:* assign re-probe ownership (P4 health-watcher or P5 queue) or mark Phase-8+.
- **FLAG-D (deferred-by-decision, most notable):** **domain-batched many-RCPT session reuse** is promised in P2 §6.3, re-promised in P4 §3.5, then **explicitly declined in P5 §6.4/OQ-P5-4** (contingent on the open GAP-13 spike). Not a silent drop — a conscious deferral gated on a spike — but a reader tracing "session reuse" should know it terminates in a *decision not to build*. Ties to the GAP-13 pre-takeoff gate.

---

## 4. Auto-mutate audit (architect-review + backend-architect) → shadow-mode

Eleven autonomous mechanisms (A1–A11). **Four auto-mutate caller-visible state (routing/confidence/eligibility/spend) on day one with no human in the per-decision loop:** A1 (P4 block-rate quarantine), A6 (P7 confidence recalibration — the sharpest, directly rewrites the numbers external systems code against), A8 (P7 provider auto-promotion), A9 (P7 fallback weaning). Full treatment and the recommend-only defaults are in [`shadow-mode.md`](shadow-mode.md).

- **The system already ships the correct pattern for the two *systemic* cases:** A5 (P4 fleet-brake) and A7 (P7 cascade-brake) both alert-and-hold, never auto-apply. Shadow mode generalizes this blessed pattern down to the per-decision mutators — a consistent extension, not a new philosophy.
- **Conflation to correct (for readers):** the prompt's "P6 fallback-weaning" actually lives in **P7 §9**; P6's only auto-mutator is the **spend circuit-breaker** (§5.3, A11). Weaning = A9 (P7); breaker = A11 (P6).
- **Genuinely safe autos (monitor, don't gate):** A2 (audit-red, deterministic), A3 (canary recovery, conservative), A4 (rate-limiter degrade, honest direction), A10 (historical-bounce gate, TTL-reversible — *with the F-17 poisoning caveat*), A11 (breaker, alert-and-degrade).

---

## 5. Schema coherence (database-architect) — COHERENT across all seven

**Verdict: zero contradictions.** All seven audited areas are coherent; severity across the board is **info**. The three load-bearing invariants — verdict-level join anchor, append-only-log vs separate-calibration-store, CAS-on-lease-holder — are honored uniformly downstream.

| # | Area | Verdict | Key evidence |
|---|---|---|---|
| 1 | **Verdict-level join anchor** | COHERENT | P7 §3.1 restates P1 §4.7 verbatim + LOCKED: join on `send_outcomes.verification_id → verifications.id`; `email_hash` only for address rollups, never accuracy attribution. The sole address-level use (§4.6 historical-bounce gate) is explicitly the *rollup* use; address-level scoring fenced to Phase-8+. P7 adds zero structural fields; adds a temporal-integrity guard (created_at > sent_at → rejected). |
| 2 | **Calibration store** | COHERENT | New `calibration` table keyed `(reason, mx_provider, [vendor, residue_class])`, "not in `verifications`"; versioned/effective-dated (not overwritten); never rewrites a `verifications` row (Door 2). All four key components populated upstream (`reason` frozen enum P1 §3.4; `mx_provider` typed column from row #1 P1 §3.2.2; `vendor`/`residue_class` from P6 signals). CAS on the write (GAP-53) + ingestion dedup on `(message_id,event)`. |
| 3 | **Queue tables** | COHERENT | `work_items`: `locked_by`/`locked_at`/`status`/`not_before`/`attempt`/`verification_id` FK/`idempotency_key` (bytea, not raw email — Door 3). Columns exactly support GAP-37 CAS (`WHERE id=? AND locked_by=<me> AND status='in_flight'`) and GAP-38 authoritative `NOT EXISTS(... NOT IN settled,dead)`, not the drifting `settled_count`. |
| 4 | **Append-only LOG** | COHERENT | `verifications` append-only via revoked UPDATE/DELETE; mutable raw → `verification_raw` sidecar. Every writer consistent: P5 one row/txn + reclaim no-op; P6 one append + dedicated transcript sidecar; P7 writes the separate calibration store, never the log. |
| 5 | **Email-hash representation** | COHERENT | `bytea` (32-byte HMAC), `norm/v1+hmac-sha256/v1`, scheme-tagged, *everywhere* it's stored/joined (verifications, send_outcomes, work_items, work_items.idempotency_key). Plaintext egress (P6) is the one governed exception, not a representation change. |
| 6 | **Ledger / spend** | COHERENT | Vendor call + cost-decrement + settlement commit under one CAS on the lease-holder (P6 §5.4, inherits P5 §5.3); cost decremented once in the settlement txn, never optimistically, never in a sweep. P5 spend-once (per real SMTP attempt) vs P6 spend-once (per settlement, money) are *intentionally different denominators*, documented — not a contradiction. |
| 7 | **Provenance / bucketing** | COHERENT | `external_verified` isolable (`WHERE reason='external_verified'`); P7 gives it *parallel separate* buckets keyed `(vendor, residue_class, mx_provider)`, never merged into SMTP-earned buckets (GAP-54); capped confidence (≤60) keeps rented verdicts weaker + weanable. |

**Three additive schema notes (PROPOSED NOTE, owner sign-off — no frozen change):**
1. **`calibration` table shape is described, not sketched as columns** (P7 §5.4 names the key + versioning but no column list, unlike P1 §4.7's `send_outcomes` sketch). Propose adding a sketch (`bucket-key parts, confidence smallint 0–100, version int, effective_from/effective_to timestamptz, sample_n, created_at`) so the "revertible" guarantee has an explicit shape.
2. **Confirm the dedup uniqueness constraint lives on `send_outcomes(message_id, event)`**, not on the many-to-many join table, so the "no double-count into a bucket" guard (§3.4) has a concrete home.
3. **`residue_class` is a bucket key but stays in `signals.external.residue_class` jsonb** (P6 §6.2), while `mx_provider` was promoted to a typed column for exactly this grouping use (P1 §3.2.2). Consistent with "when unsure, jsonb," but consider whether a calibration group-by dimension warrants the same promotion, or confirm jsonb-indexed access is acceptable at flywheel scale.

---

## 6. QA — behavioral test design (senior-qa)

For each phase the "done when" criteria were re-expressed as **observable input→outcome assertions** (not lint/compile/"exists on paper"). The full per-phase test map is long; below are (a) the tests that catch the named GAPs — the regression-critical set — and (b) the QA-gaps: acceptance criteria that are **not behaviorally testable as written** (flagged for owner, not rewritten).

### 6.1 One behavioral test per named GAP (the regression-critical set)
| GAP | The single assertion that fails if the guard regresses |
|---|---|
| **GAP-13** (session reuse vs never-fork) | Run the pre-provisioning spike: attempt the 2nd-RCPT probe in one session **without patching the gem**. If impossible wrapper-side → assert fallback to Option B (two sessions) and the pinned-gem CI check still passes. **Regression = a gem patch appears** (gem-hash changes). On a real box: a catch-all check opens ≤2 connections, reputation-cost tracked in warmup. |
| **GAP-15** (audit-green ≠ can-send) | Box audit-green but outbound 25 blocked → `port25_open=false`, node **NOT `:smtp`-eligible** (`green AND port25_open` both required). Regression = green-but-can't-send box gets SMTP work. |
| **GAP-24** (multi-homed audits wrong IP) | Force `host_audit` to detect the **private** IP → audit does **not** go green; the four-way cross-check (`current_host_ip == A-record == PTR == egress == verifier_ip`) flags the divergence. Regression = green audit on the unaudited public egress IP. |
| **GAP-28** (fleet-brake / no cascade) | Drive **all** active nodes over the block threshold in one window → **no auto-quarantine**, critical fleet-wide alert fires, membership held. Regression = pool auto-quarantines into total outage on a shared cause. |
| **GAP-37** (lease-CAS exactly-once settle) | Stall attempt A in a long SMTP call so the TTL reclaimer runs B; A finishes last → A's settle `UPDATE … WHERE locked_by=A AND status='in_flight'` matches **zero rows**, A discards; exactly one verdict logged; no settled-`250` resurrected into retry. Regression = two verdicts / double-spend beyond one / last-writer-wins flip. |
| **GAP-42** (spend-once) | Reproduce the reclaim on a **residue** check that calls the paid vendor → vendor call + cost decrement + settle under one CAS; lost attempt does not charge-record; duplicate-call rate ≈ 0. Regression = two paid calls for one logical check. |
| **GAP-49** (stuck-bucket / exclusion-is-discipline) | Feed a bucket 95% sender-fault blocks + a few `mailbox_not_found` + one `mailbox_full` → confidence pressured **only** by `mailbox_not_found`; `mailbox_full` **raises** existence evidence; the 95% zero-info in neither numerator nor denominator. Regression = the block flood drives the bucket "worse than reality" and low confidence → fewer sends → stuck-bad. |
| **GAP-50** (feedback-loop safety) | One poisoned window that "should" slam a bucket 90→10 → moves ≤ `max_step`; sub-`min_bucket_samples` bucket doesn't move; down-moves faster than up; a many-bucket swing **held for human confirm** + rollback-able. Regression = one bad batch captures a bucket / loop diverges. |
| **GAP-52** (cold-start honesty) | Fresh system, no outcomes → every bucket serves the P1 baseline (0% calibrated), zero promoted, zero weaned; behavior **identical to end-of-P6**. Regression = any bucket claims a calibrated confidence with zero real outcomes. |
| **GAP-53** (idempotent ingestion) | Re-deliver the same `(message_id, event)` → counted **once**; slow-cycle the recalibration writer → CAS makes its overrun write a zero-row no-op. Regression = retried webhook double-counts / slow recalibration double-applies. |
| GAP-10 | Divergent brain/dns_mock MX → `signals.dns_divergence=true`, engine-MX authoritative; routed-`:smtp`-but-engine-no-MX → `unknown/smtp_error`, not `invalid`. |
| GAP-14 | `valid`-path test uses a **seeded** rejected probe; a **random** probe on the accept-all mock always → `catch_all`, never `valid`. |
| GAP-18 | Single P3 box blocked with green audit → rising `unknown/smtp_blocked` in LOG but **no** auto-quarantine (audit-driven only in P3). |
| GAP-19 | Engine `validation_type_for` disagrees with brain `provider_routing` → logged drift alarm; brain stays authoritative. |
| GAP-26 | Restart mid-window → rate **budget** resumes from durable spend (no refill) **and** in-flight counter resets to **zero**. |
| GAP-30 | Block-quarantine, restore audit-green → enters `recovering`, canary trickle, can return to `active` (not stuck-forever with no samples). |
| GAP-32 | Greylist on node #1; retry prefers **node #1's IP** when eligible; least-inflight only if home gone. |
| GAP-33 | Crash after commit, reclaim → re-attempt finds `verification_id` set, writes **no second** verdict. |
| GAP-34 | Dequeue uses `FOR UPDATE SKIP LOCKED` on one Postgres store; no external broker holds a second copy of settlement state. |
| GAP-38 | Drift `settled_count` low while all items terminal → job reports `complete` via the authoritative `NOT EXISTS` query. |
| GAP-41 | `external_verified` verdict never `valid`, always carries `signals.external.vendor` → P7 can subtract the rented population. |
| GAP-43 | Re-POST a cached `external_verified` address → no second vendor call/charge. |
| GAP-44 | Exhaust vendor spend → SMTP pacing unaffected; saturate the P4 limiter → vendor breaker unaffected (two distinct budgets). |
| GAP-51 | Outcome with `sent_at` before verdict `created_at` → `ingest_error`, excluded from calibration, surfaced on ingestion-health. |
| GAP-54 | SMTP-earned + `external_verified` outcomes for one provider → **separate** bucket spaces, never merged. |

### 6.2 The two "counterintuitive verdict" regression tests (highest-value alongside GAP-37/49)
- **`552 mailbox_full → valid`** (P2 §6.1.1): a 552 settles `valid/mailbox_full` high-conf — **assert NOT `invalid`, NOT `risky`**. A naive implementer inverts this; the error is silent and poisons P7's numerator.
- **`disposable → risky`, not `invalid`** (P1 OQ-7): a disposable-domain address is `risky/disposable`, **assert status is NOT `invalid`**. Inverting it poisons calibration.

### 6.3 QA-gaps — criteria not behaviorally testable as written (flagged, not rewritten)
- **The "config-only / zero-code-diff" family** (P2 §12.11, P3 §12.6, P4 §13.3): the *behavioral* half (identical output mock→real→N) is testable; the *"zero diff"* half is a VCS/CI assertion. Treat consistently: one behavioral identity test + one CI diff gate everywhere.
- **"Un-mockable until integration" is a recurring, deliberately-honest class** (P2 GAP-12/35 stateful greylist round-trip; P6 GAP-47 vendor accuracy; P7 GAP-48 sender DSN fidelity). Each has mechanics testable against a stub + real behavior deferred to rollout. **Acceptance language must never read "done" for the deferred half.**
- **CI/static gates**, not behavioral: "no hardcoded endpoint" (grep), "gem unmodified" (CI hash), "tables exist on paper" (design review).
- **Governance gates**, not tests: every phase's last acceptance item ("every OQ signed off").
- **Procurement/market gates**, genuinely non-automatable, and the specs say so: P3 host-selection V6–V8 / OQ-P3-1, P4 §6 manual provisioning stages. These are the real execution risk ("the narrow market is the wall, not the design") → tracked in [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md), not the test suite.
- **Pepper custody** (P1 OQ-1): the recovery drill is a destructive DR exercise, not a per-request test → operational, owner-owned.

### 6.4 Cross-phase QA observation worth templating
The **lease+CAS "slow-not-dead holder, lost-lease write matches zero rows"** test (GAP-37) is the single most load-bearing correctness property and recurs at three sites (P5 settle-once, P6 spend-once, P7 ingestion/recalibration-write-once). Template it once, instantiate at all three. Likewise the **`from_cache`/override exclusion** discipline is asserted at three layers (P1 log, P4 block-rate control loop, P7 calibration) — a regression has *different symptoms per layer*; test all three sites.

---

## 7. Consolidated proposed-notes register (all owner sign-off; none reopens a door)

| id | proposed note | source finding | additive artifact |
|---|---|---|---|
| PN-1 | Admin authn/authz + append-only admin-audit log (who/what/when for every control-plane mutation) before P3 go-live | F-11 | new operational table |
| PN-2 | Decide OQ-P7-1 as an **authenticated** channel; threat-model TB-7 for a malicious sender; rate-limit ingestion per-provider | F-15 | pre-P7 gate + config |
| PN-3 | Treat DB backups/snapshots as live plaintext for the retention window; consider encrypting `work_items` plaintext column with a non-pepper key | F-7 | ops runbook + optional column encryption |
| PN-4 | Adopt the `mode ∈ {shadow, recommend-only, auto}` dial, recommend-only default for A1/A6/A8/A9 | auto-mutate audit | `mode` config/column |
| PN-5 | Node-token rotation runbook: rotation triggers `/config/reload`; revoke atomic with quarantine; router excludes revoked-token node immediately | F-1 | ops runbook |
| PN-6 | Contractually assert platform opt-out ↔ policy-without-`external` maps 1:1, verified at integration | F-4 | integration test |
| PN-7 | Cumulative-addresses-egressed metric + periodic vendor-deletion process in the §7.4 checklist | F-5 | metric + process |
| PN-8 | Specify brake-hold timeout/default for A5/A7 + authenticated confirm + alert-pipeline-health monitor | F-12 | spec addendum |
| PN-9 | Separate DDL/grant-admin credential (break-glass audit); consider periodic hash-chain checkpoint of `verifications` to external WORM | F-13 | ops + optional |
| PN-10 | Confirm P6 external-call transcript retained + queryable by `email_hash` for SAR/deletion | F-14 | retention config |
| PN-11 | Require ≥N independent invalid-signal outcomes before the historical-bounce gate fires | F-17 | P7 §4.6 dial |
| PN-12 | Explicit brain-compromise threat model; brain host hardening runbook | F-19 | threat model |
| PN-13 | Upgrade front-door auth beyond a static shared token (per-caller keys + rotation) before bulk (P5) ships | F-20 | auth upgrade |
| PN-14 | Add a `calibration` table column sketch; confirm `send_outcomes(message_id,event)` dedup constraint; decide `residue_class` typed-vs-jsonb | schema §5 | schema sketch |
| PN-15 | Name the owner of `signals.platform` or mark it Phase-8+; assign catch-all re-probe cadence ownership (FLAG-A, FLAG-C) | SEAM §3 | spec addendum |
| PN-16 | Define the global-fallback-kill switch + its authz/audit as one flag | kill-switch §Global A | config + audit |

*None of PN-1…PN-16 changes a frozen contract, mints a status/reason value, or reopens a one-way door. All are additive operational/config/test artifacts for owner sign-off.*
