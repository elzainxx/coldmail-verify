# Kill-Switch Ladder — Degradation Hierarchy (Cross-Phase)

**Status:** DRAFT v0.1 — cross-phase consolidation pass (planning, not code)
**Purpose:** the documented degradation ladder from full operation down to paused. For each rung: what it disables, what the system still returns, and what triggers pulling it. Every rung obeys the founding rule — **degrade toward `unknown`, never fabricate `invalid`/`valid`.**
**Companions:** [`config-manifest.md`](config-manifest.md) · [`shadow-mode.md`](shadow-mode.md) · [`cross-phase-consistency-report.md`](cross-phase-consistency-report.md)

> **The governing invariant.** The whole system is built so that *removing capability degrades the answer's confidence, never its honesty.* Every rung below returns a **real, logged, correctly-labelled verdict** — just from fewer layers. Nothing on this ladder ever makes the system return a *wrong* verdict; it makes it return a *humbler* one. This is why the ladder is safe to pull under pressure.

---

## 0. How to read the ladder

- The ladder is **layered, not linear** — you can pull a lower rung (e.g. fallback-off) without pulling a higher one (SMTP-off), and several are **per-scope** (per-node, per-provider, per-policy) rather than global. The rungs are ordered by *how much capability they remove*, from most (full) to least (paused).
- **Two global master switches** sit outside the graceful rungs: the **global-fallback-kill** (§Global A) and **shadow-mode** (§Global B, see [`shadow-mode.md`](shadow-mode.md)). These are cross-cutting, not a single rung.
- **"Still returns"** is always a valid response envelope (Door 1) with a correctly-labelled `status`/`reason` — degradation shows up as more `unknown`/`risky` and lower `confidence`, plus honest `signals` (`smtp_skipped_reason`, `from_cache`, etc.).
- Each rung cites the config-manifest knobs that implement it.

---

## 1. The ladder

### Rung 0 — FULL OPERATION
- **Enabled:** cheap layers + real SMTP on eligible nodes + queue/retry + paid fallback on residue + (once promoted) auto-recalibration/promotion/weaning.
- **Returns:** the full verdict space — `valid` (SMTP-earned or probe-confirmed), `invalid` (address-fault proven), `risky`, `unknown`, plus `external_verified` on residue.
- **This is the target steady state**, reached only after Phase 6+ and after the operator promotes shadow-mode mechanisms to `auto` per-scope.

### Rung 1 — SMTP-OFF (per-node → per-pool → global)
- **Disables:** SMTP probing. Achieved at three scopes:
  - **per-node:** `nodes.enabled=false` (P1 §4.5) or auto-quarantine (P3 §4.5 audit-red / P4 §5.4 block-rate).
  - **per-provider:** route a provider to `:mx` (P1 `provider_routing`, P3 §5.1) — no SMTP for that provider.
  - **global:** flip the default policy's SMTP off / all nodes disabled → the P2/P3 "config-flip" in reverse.
- **Still returns:** cheap-layer verdicts + paid fallback (if enabled). Honest corporate addresses that *would* have earned `valid` now settle **`risky/provider_no_smtp`** (or escalate to fallback). Gmail/O365/Yahoo are unaffected (already `:mx`). **No address is called `invalid` for lack of SMTP** (harden-toward-unknown, P3 OQ-P3-6).
- **Triggers:** IP reputation emergency (mass blocks), a compromised box (P3 §6.4 — kill + re-spin), an audit-red or block-rate quarantine, or a deliberate "cool the whole pool" call during a reputation incident.
- **Config:** `nodes.enabled`, `audit_status`, `QUAR_THRESH`, `provider_routing`, the `:smtp`-eligibility rule (`green AND port25_open`).

### Rung 2 — FALLBACK-OFF (per-policy → per-vendor → global)
- **Disables:** the paid external vendor (P6). Achieved at three scopes:
  - **per-policy:** a policy without the `external` block (P6 §7.5) — *this is also the privacy opt-out lever*.
  - **per-vendor / spend:** the circuit-breaker trips (P6 §5.3 — budget/error/vendor-down) → `keep_residue`.
  - **global:** the **global-fallback-kill** (§Global A) — one switch, all policies, immediately.
- **Still returns:** everything Rung 0 does *except* vendor answers on residue. Residue (`catch_all`, `provider_no_smtp`, retry-exhausted, gave-up-capacity) settles as P5 left it — **`unknown`/`risky`, never worse, never fabricated** (P6 `keep_residue`).
- **Triggers:** runaway vendor bill (spend cap), vendor outage/degradation (error-rate breaker), a DPA/compliance event (pull egress immediately), or a privacy incident requiring the third party be cut off.
- **Config:** `external` layer enable, `daily_cap`/`monthly_cap`/`per_job_cap`, circuit-breaker conditions, `global-fallback-kill`.

### Rung 3 — CHEAP-LAYERS-ONLY
- **Disables:** SMTP (Rung 1 global) **and** fallback (Rung 2 global) together. Only syntax/typo/role/free-corp, MX/DNS/null-MX, disposable, spam-trap, provider-classification run.
- **Still returns:** provable negatives at full confidence (`invalid/invalid_syntax`, `invalid/no_mx`, `invalid/null_mx`, `invalid/spam_trap`), `risky/disposable`, and for everything that would need SMTP/fallback → **`risky/plausible` or `unknown`** at bounded-low confidence. **The system never guesses `valid` here** — a clean address it can't probe is `risky`, not `valid`.
- **Triggers:** total SMTP-infra loss (all nodes down/blocked) *and* fallback unavailable/killed; or a deliberate "cheapest-sufficient only" cost-freeze; or the earliest phases (this rung *is* Phase 1's steady state).
- **Config:** the union of Rung 1 + Rung 2 globals. Note: this is not a special mode — it is simply the pipeline with its expensive layers removed, which is exactly what Phase 1 shipped. The system is *designed* to be correct at this rung.

### Rung 4 — PAUSED (accept-and-defer, or reject)
- **Disables:** verdict *production*. Two sub-modes:
  - **accept-and-defer (queue-only):** the front door accepts submissions and returns a **job handle** (P5 §7), but the drainer is stopped — work sits durably in `work_items`, nothing is processed. Restart resumes with zero loss (durable queue, P5).
  - **reject:** the front door returns a service-unavailable envelope; no new work accepted.
- **Still returns:** either a job handle + "processing paused" job status (accept-and-defer), or an explicit unavailable response (reject). **It never returns a fabricated verdict to look "up."**
- **Triggers:** a data-integrity emergency (suspected calibration poisoning, a pepper-custody incident, an append-only-LOG tamper suspicion), a Supabase outage, or a controlled maintenance stop. Accept-and-defer is preferred when the queue and DB are healthy but processing must halt; reject when the DB itself is compromised.
- **Config:** drainer stop (P5), front-door mode.

---

## 2. The two global master switches (cross-cutting, not rungs)

### Global A — GLOBAL-FALLBACK-KILL
- **What:** one switch that disables the P6 external vendor across **all** policies immediately, regardless of per-policy `external` blocks or breaker state.
- **Why separate from Rung 2:** Rung 2's per-policy/per-vendor mechanisms are *graceful and scoped*; the global-fallback-kill is the **panic button** — a single lever the operator can pull without editing every policy, for a billing runaway or a privacy/DPA incident where *no* address may egress. It is the fastest path to "no PII leaves the building."
- **Effect:** identical to Rung 2 global (residue settles `keep_residue`), but reachable in one action and independent of breaker logic.
- **`[PN-16, accepted 2026-07-17]`** this switch and its authz/audit are not named in any single phase — it is the natural consolidation of P6 §5.5 (breaker = brake) + §7.5 (opt-out lever). **Accepted:** define it as **one operator-flippable, admin-audited flag** — its audit trail rides the DR-3 admin-audit gate (F-11). Recorded in [`DECISIONS.md`](DECISIONS.md) DR-5 (PN-16); the admin-audit log itself is gated before P3 go-live ([`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) §3.6). Flag defined here; **not built in this pass.**

### Global B — SHADOW-MODE (per-mechanism `mode` dial)
- **What:** the [`shadow-mode.md`](shadow-mode.md) dial. Setting a mechanism to `shadow` or `recommend-only` disables its **autonomous mutation** of routing/confidence/spend while keeping it computing + alerting.
- **Why on the ladder:** it is a *capability* kill orthogonal to the layer rungs — it turns off the system's ability to *change itself* (recalibration A6, promotion A8, weaning A9, block-rate quarantine A1) without touching its ability to *answer*. In an incident where you suspect the feedback loop is misbehaving (e.g. poisoned outcomes, F-15), you drop these to recommend-only **without** taking SMTP or fallback offline.
- **Default:** the four in-scope mutators ship at `recommend-only` (see shadow-mode §2), so the "self-modification off" posture is the *starting* state, promoted to `auto` deliberately — the ladder's self-modification rung is pulled *up*, not down.

---

## 3. Interaction with the auto-mutate mechanisms

The auto-mutate audit's mechanisms map onto the ladder as follows — this is how the system pulls its *own* rungs:

| mechanism | pulls which rung | scope | reversible? |
|---|---|---|---|
| audit-red quarantine (A2) | Rung 1 SMTP-off | per-node | yes (N-green recovery) |
| block-rate quarantine (A1) | Rung 1 SMTP-off | per-node | yes (canary recovery, GAP-30) |
| rate-limiter degrade (A4) | partial Rung 1 (paces, doesn't remove) | per node×provider | yes (window-roll) |
| spend circuit-breaker (A11) | Rung 2 fallback-off | per-vendor | yes (window-roll) |
| provider promotion (A8) | Rung 1 SMTP-off | per-provider | yes (hysteretic demote) |
| fleet-brake (A5) | **refuses** to pull Rung 1 fleet-wide | pool | alerts human instead |
| cascade-brake (A7) | **refuses** to auto-apply recalibration | calibration | alerts human instead |

**The two brakes are the ladder's safety interlock:** they exist specifically to stop an *automated* rung-pull from cascading into a self-inflicted outage (auto-heal → auto-outage, GAP-28) or a mass mis-calibration (GAP-50). They convert a would-be automatic global degradation into a human decision.

---

## 4. What the ladder guarantees (and what it doesn't)

**Guarantees:**
- At every rung, a response is either a correctly-labelled verdict or an honest unavailable/paused signal — never a fabricated `valid`/`invalid`.
- Every rung is reachable by config/flag, not code (Door 5 discipline).
- Every downward rung is reversible (quarantine → recovery, breaker → window-roll, pause → resume with durable queue).

**Does NOT guarantee (flagged for owner):**
- **Authz/audit on who pulls a rung.** The manual rungs (node disable, global-fallback-kill, pause, shadow-mode promotion) have **no specified operator identity or audit trail** in any phase (security F-11). **`[PN-1, accepted 2026-07-17 → DR-3]`** every manual rung-pull writes to an **append-only admin-audit log** (who/what/when), reusing the `node_audits`/`work_item_attempts` pattern — **gated before P3 go-live** ([`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) §3.6). Not built here.
- **A defined "brake-hold timeout."** The two brakes hold for a human — but no phase specifies what happens if no human responds (does the pool stay held? does the flywheel starve?) (security F-12). **`[PN-8, accepted 2026-07-17]`** specify default-on-timeout behavior for A5/A7 + an authenticated, admin-audited confirm + alert-pipeline-health monitoring (notes on P4 §5.5 and P7 §6.4). The **timeout value/behavior is flagged, not decided** ([`DECISIONS.md`](DECISIONS.md) §5). Not built here.

---

## 5. One-glance ladder

```
Rung 0  FULL           cheap + SMTP + queue + fallback + (auto self-tuning)
   │
Rung 1  SMTP-OFF       cheap + queue + fallback           (per-node/provider/global)
   │                   would-be valid → risky/provider_no_smtp; never invalid
Rung 2  FALLBACK-OFF   cheap + SMTP + queue               (per-policy/vendor/global)
   │                   residue → keep_residue (unknown/risky); never worse
Rung 3  CHEAP-ONLY     cheap layers only                  (= Phase-1 steady state)
   │                   provable invalids full-conf; unprobed → risky/unknown; never valid
Rung 4  PAUSED         accept-and-defer (durable queue) OR reject
                       job handle / unavailable; never a fabricated verdict

Global A  FALLBACK-KILL   panic button: no PII egress, all policies, one action
Global B  SHADOW-MODE     self-modification off (recalibration/promotion/weaning/quarantine)
                          → default state; promoted to auto deliberately
```
