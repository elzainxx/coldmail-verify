# The consolidated go-live runbook — one ordered path from local build to live

**Date:** 2026-07-19 · **Status:** the single ordered go-live path for the whole system. The seven phases are built + fail-verified locally; this is the sequence that turns that machine on, safely, in order. **Every step is config/flag or an owner action — zero brain code changes** (the Door-5 payoff, GAP-21/31). Each stage is independently reversible.

> **QUALIFIED 2026-08-07 — "independently reversible" is about FLAGS, and the migration layer is the exception.** Every stage below really is a config flip, and the vendor and feed layers really do turn off by unsetting a variable. But the schema is **forward-only with no down-migration mechanism at all** (measured: zero `.down.sql`, no tracking table), and `git revert` of a migration deletes the file without touching any database that already ran it — leaving one tree and two divergent schemas with nothing recording which is which. Read [`go-live-rollback-posture.md`](go-live-rollback-posture.md) **before the second environment exists**; its item 1 (adopt version tracking) is cheap now and an archaeology exercise later.

> **Status as of 2026-11-07 (after the post-audit fixes).** Two things changed that this runbook's original
> author could not have known, and both are Stage 0 items below:
> 1. **A date-triggered defect was found and fixed** — the raw-sidecar table `verification_raw` had partitions
>    for 2026-07..09 only, so from 2026-10-01 every SMTP verdict write failed (SQLSTATE 23514). It was invisible
>    until the suite was re-run three months later. **Migration `0014` + the maintenance loop
>    ([ADR-0017](adr/ADR-0017-raw-partition-maintenance.md)) close it; `0014` must be applied before any real verdict is written.**
> 2. **`govulncheck` is clean** on go1.26.10, `x/net` v0.62.0, `x/text` v0.43.0 (it was **not** before: 17 live-call-path
>    advisories). The advisory database moves, so this is a *re-run before the pilot*, not a one-time tick.
>
> Unchanged: all **20** rows in [`pending-ledger.md`](pending-ledger.md) are real-world-gated (a real port-25 box, a
> vendor, a sender) and genuinely unclosable here, and the control-plane is still **not packaged** (no Dockerfile or
> compose file for it; fix-plan item 15).

**The order is safety-critical and the stages are gated:** you cannot meaningfully do Stage 2 (vendor) before Stage 1 (a real box) is earning verdicts, and Stage 3 (the outcome feed) needs a system that has been sending long enough to have outcomes — and a sender that does not yet exist. Do them in order.

**Two kinds of gate appear below:** ⚙️ **code/config** (mechanical, in this repo) and 🔴 **owner/non-code** (procurement, legal, a product decision, or a cross-project deliverable). The owner gates are consolidated in §4 — several can and should be worked **in parallel** with the local build.

Companion: the deferred-proof detail is in [`pending-ledger.md`](pending-ledger.md); source runbooks are [`phase-3-cutover-runbook.md`](phase-3-cutover-runbook.md) (the authoritative box detail) and [`phase-4-provisioning-script-design.md`](phase-4-provisioning-script-design.md).

---

## Stage 0 — before anything ships anywhere non-local (owner gates that gate the *repo*, not a phase)

✅ **LICENSE — RESOLVED (2026-07-19, arc closeout).** The project's own license is now **proprietary / all-rights-reserved** — top-level [`LICENSE`](../LICENSE), recorded in DECISIONS §6.1 + README "License posture". It covers the owner's own code only and grants no rights over the MIT third-party components (respected in [`NOTICE`](../NOTICE), retained at `verifier-node/truemail-rack/LICENSE.txt`, gem byte-identical — Door 5). This closes the last Phase-0 deferral and clears the gate that blocked pushing the repo (which contains the `truemail-rack` fork) anywhere non-local. *(Deferred since Phase 0; chosen over MIT/Apache-2.0 because the brain's mechanics + data moat are deliberately non-public — reversible toward openness later, publishing is not.)*

🔴 **DR-3 admin-audit log (gates P3 go-live; buildable now, in parallel).** Before any real go-live, every manual control-plane action (node disable, global-fallback-kill, pause, shadow-mode promotion, budget change, breaker/brake confirm, privacy opt-out flip) must write to an **append-only admin-audit log with per-actor identity**, distinct from the verdict LOG (reusing the `node_audits`/`work_item_attempts` pattern). **Its first slice is the `port25_open` egress-test-writer** (see Stage 1 step 1) — the highest-value parallel build, and the thing that resolves the one known wiring gap.


🔴 **Migration `0014` applied, and the partition headroom verified (ADR-0017).** Without it, every SMTP verdict write fails once the last hard-coded partition's month ends (it already has). Apply `control-plane/migrations/0014_raw_partition_maintenance.sql`, start the control-plane (it creates `TM_RAW_PARTITION_MONTHS_AHEAD` future months at startup and every `TM_RAW_PARTITION_MAINTAIN_INTERVAL_SECONDS`), then confirm: `SELECT relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = 'verification_raw'::regclass ORDER BY 1;` lists the current month plus the configured number ahead. **Watch the log for `raw-partitions: maintenance FAILED`** — a failing loop has about `TM_RAW_PARTITION_MONTHS_AHEAD` months of warning before a write fails. Retention (`DROP PARTITION`) is still a manual `cp_admin` action. See [`OPERATIONS.md`](OPERATIONS.md) §8.

🔴 **Re-run `govulncheck ./...` in `control-plane/` immediately before the pilot.** Clean on 2026-11-07; any live call path blocks. Use a `golang:1.26` image at or above the patched floor (1.26.10 on that date) — CI's `setup-go '1.26'` tracks the latest patch, a pinned local image does not.
---

## Stage 1 — the real box (SMTP verification goes live)

**Gate 1 (🔴 owner procurement — the wall):** a host that passes **V1–V5** on a *refundable trial* before you pay. This is the real work; the narrow port-25∧settable-PTR∧clean-IP market is the wall, not the design.

**V1–V5 host vetting** (run FROM the trial box unless noted; each is pass/fail, a HARD fail disqualifies the host):

| V | Requirement | Proof command | PENDING until |
|---|---|---|---|
| **V1** | Outbound TCP/25 genuinely open (GAP-15) | `nc -vz <a-real-external-mx> 25` establishes AND a `220` banner is seen (`swaks --server <mx>:25 --quit-after CONNECT`) — **not** the provider's claim | a banner is received |
| **V2** | Settable custom PTR/rDNS | set PTR for the box IP → the verifier FQDN; `dig -x <box-ip> +short` returns it (allow propagation) | PTR resolves to the FQDN |
| **V3** | Clean IP (no spam history) | check `<box-ip>` vs Spamhaus/Barracuda/passive history **before** warmup | IP is not pre-poisoned |
| **V4** | Dedicated / stable IP | provider confirms the IP is yours for the box's life (not shared/rotating) | provider confirms |
| **V5** | Egress == PTR == audited IP (GAP-24) | `curl -s ifconfig.me` == the PTR'd IP == the verifier A-record target == `host_audit.current_host_ip` — **all four identical** | all four match |

*(V6–V8 — ToS, uptime, region — are procurement judgment gates, not automatable.)*

**The ordered config-only flip (audit BEFORE traffic — the order is safety-critical):**

1. **Egress-25 smoke test → set the node's `port25_open=true`.** ⚠️ **Known wiring gap:** there is no operator/env command to flip this yet (no `TM_NODE_PORT25_OPEN`). Interim: a `cp_admin` break-glass `UPDATE nodes SET port25_open=true WHERE id=…`. Permanent home: the **DR-3 admin-audited egress-test-writer** (Stage 0). Until `port25_open` is true, the node is **not** `:smtp`-eligible.
2. **Point the box at real config** (`smtp_port:25`, real `dns`) → `POST /v1/config/reload` → a new `config_fingerprint` (naturally invalidates the mock-era cache).
3. **Set the brain node descriptor** (env / config-list): `TM_NODE_BASE_URL=<private-net address>`, `TM_NODE_PUBLIC_EGRESS_IP=<public egress IP>` (**must equal V5**), `TM_NODE_ENABLED=true`, `TM_SMTP_MODE=enabled`.
4. **First audit → require green.** The node promotes `provisioning→active` **only** if green ∧ `port25_open` ∧ (GAP-24) `current_host_ip == TM_NODE_PUBLIC_EGRESS_IP`. A green on the wrong IP will **not** promote (ADR-0009) — check for the `egress-IP != audited current_host_ip (GAP-24)` warning.
5. **Start the warmup cap low** — `TM_WARMUP_DAILY_CAP=50` (day 1); step up only on prior-day low `smtp_blocked` + green audit (OQ-P3-10). The cap counts *verifications*; a catch-all check is ~2 connections (GAP-13) — size against connections.
6. **Confirm real verdicts flow** with non-null `verifier_ip` (== egress IP), real `node_id`/`engine_version`/`config_fingerprint`.

**Go-live acceptance (the real-IP proofs the local build could only simulate):** egress smoke → `port25_open`; `/v1/audit` green; break PTR → auto-quarantine + degrade-to-cheap (never invalid) → restore PTR → N greens → recover; honest-corporate address + random-probe-rejected → `valid/accepted`; Gmail/O365/Yahoo → `risky/provider_no_smtp` (SMTP never called); warmup-cap-exceeded → cheap + `smtp_skipped_reason=warmup_cap`; external port scan → wrapper port closed; the four-way IP identity holds; provider-routing drift alarm silent at boot.

**Reversible:** `TM_NODE_ENABLED=false` (or `TM_SMTP_MODE=disabled`) → cheap verdicts, no redeploy, no lost history.

⚙️ **Live-HTTP verification (PENDING-LIVE-HTTP, do this FIRST on the Linux host — code/config, no real-world input needed).** The full test plan's endpoint tiers ([`TEST-PLAN.md`](TEST-PLAN.md) §5/§6/§10) were proven **in-process** on the dev host (Go `httptest` + the fork's RSpec request specs — identical handler code) because a long-lived HTTP server could not be run/killed reliably on Windows. That does not exercise port binding, real socket serialization, production middleware ordering, or TLS. On this first Linux host, **run [`TEST-PLAN.md`](TEST-PLAN.md) §5.4** against a live server (start it on a real port; re-run the §5.1 verdict matrix, §5.3 auth, §6 row inspection, and the §10 fork endpoints over a real socket + TLS if terminated at the app) and confirm the results **match** the in-process ones. *Expectation: almost certainly a Windows-only limitation that just works on Linux — but it is verified here, not assumed.* See [`pending-ledger.md`](pending-ledger.md) §3.5.

**PENDING-REAL-BOX after this stage:** the greylist reject-then-accept round-trip (GAP-35) is rollout-observation-only (un-mockable); the DR-4 backup/PITR restore drill runs against go-live infra. See [`pending-ledger.md`](pending-ledger.md) §1.

---

## Stage 2 — the paid vendor fallback (enable the P6 layer)

**Gate (🔴 owner/legal — the DPA):** the `external` layer ships **DISABLED**; enabling it pre-DPA is **unlawful sub-processing**. It stays off until the §7.4 checklist passes: **DPA signed, retention understood, TLS confirmed, key custody established.** Setting the flag is the operator's affirmation the checklist passed — do not set it before.

**The three-switch enablement (AND — all three, or no egress; global-kill overrides):**
1. Implement + wire a real `external.Vendor` adapter (one at launch, OQ-P6-8) into `buildExternal` — this phase ships none (fail-closed).
2. Set conservative budget ceilings from observed residue-tail volume: `TM_EXTERNAL_DAILY_CAP` / `_MONTHLY_CAP` / `_PER_JOB_CAP` / `_RATE_CEILING` (OQ-P6-5 — start low, raise from data, never loosen blind).
3. Flip, in order: `TM_EXTERNAL_DPA_CONFIRMED=true` (flips only on exact `"true"`) + `TM_FALLBACK_MODE=enabled` + a `TM_VENDOR_KEYS` entry for the named vendor.

Any switch missing ⇒ the layer stays off. `TM_FALLBACK_MODE=off` (global-fallback-kill) forces it off regardless — the one-action panic button (PN-16, rides DR-3).

🔴 **OQ-P6-6 disclosure decision (owner + platform):** disclose vendor egress + offer opt-out (the spec's decision). The brain makes the opt-out mechanical (a policy without the `external` block never egresses); the platform's terms/consent copy is the product decision. **F-4 integration assertion:** verify the platform opt-out flag maps 1:1 to a policy-without-`external` at integration.

**PENDING-REAL-VENDOR after this stage:** vendor *accuracy* on the residue (GAP-46/47) — measured only once the P7 outcome feed exists; never claimed. See [`pending-ledger.md`](pending-ledger.md) §2.

---

## Stage 3 — the outcome feed (turn on the flywheel)

**Gate (🔴 cross-project — the sender must exist):** P7's live value needs the sender ("Feature 2", GAP-48), which **does not exist yet**. P7 defines the interface; the sender implements its half. The authentication *mechanism* is DECIDED — **mutually-authenticated pull (mTLS)**, ADR-0013 — and the design gate is discharged (TB-7 malicious-sender threat model produced). What remains is the sender + the credentials.

**The enablement (fail-closed until every piece lands):**
1. The sender stands up an authenticated, append-only, cursor-paginated outcome feed emitting the locked `send_outcome` shape (§3.2).
2. Provision the mTLS trust anchor + P7's client credential (a new secret class, loaded like the node tokens / vendor keys — never committed).
3. Wire a real `MTLSClient` into `PullSource`. `Authenticated()` flips true only then — until then the ingestor pulls **nothing** (cold-start on the P1 baseline), never unauthenticated data.
4. The three P7 mutators stay **recommend-only** by default (A6 recalibration, A8 promotion, A9 weaning); the two live-path touches (`TM_FLYWHEEL_GATE_ENABLED`, `TM_FLYWHEEL_CALIBRATION_ENABLED`) stay **OFF** until an owner deliberately opts in — and a recalibration proposal only becomes effective on a deliberate `ApproveCalibration`. Promotion of any mechanism to `auto` is a per-mechanism, admin-audited owner action (DR-3).

🔴 **OQ-P7-8 disclosure decision (owner + platform):** surface "your sends train our accuracy" (disclose-don't-hide — the second cross-project privacy seam after OQ-P6-6); honored mechanically per-policy.

**PENDING-REAL-SENDER after this stage:** real accuracy numbers, live authentication, and any mutator actually firing — all need the real sender + volume over time. See [`pending-ledger.md`](pending-ledger.md) §3.

---

## 4. The owner gates, consolidated (the non-code work, with ownership)

| Gate | Kind | Blocks | Can start now? |
|---|---|---|---|
| **LICENSE** | legal/business | (was: any non-local push) | ✅ **RESOLVED 2026-07-19 — proprietary/all-rights-reserved ([`LICENSE`](../LICENSE)); no longer blocks the push** |
| **Host procurement (V1–V5)** | procurement | Stage 1 (real box) | ✅ vet on a refundable trial now |
| **DR-3 admin-audit log** | build (parallel) | P3 go-live; the `port25_open` flip | ✅ **highest-value parallel build** |
| **Vendor selection + DPA (§7.4)** | legal | Stage 2 (vendor egress) | ✅ due-diligence anytime |
| **Budget ceilings (OQ-P6-5)** | ops tuning | safe Stage 2 enablement | needs observed volume |
| **OQ-P6-6 disclosure** | product | Stage 2 (platform terms) | ✅ decide anytime |
| **The sender ("Feature 2")** | cross-project build | Stage 3 (outcome feed) | separate project |
| **mTLS trust anchor + client cred** | ops/security | Stage 3 live auth | with the sender |
| **OQ-P7-8 disclosure** | product | Stage 3 (platform terms) | ✅ decide anytime |
| **Migration version tracking** | build (parallel) | nothing yet — it gates *cheaply fixing* the schema-divergence hazard | ✅ **do it before the SECOND environment exists** ([`go-live-rollback-posture.md`](go-live-rollback-posture.md) item 1) |

**The parallel track:** LICENSE, host vetting, the DR-3 audit log, and the two disclosure decisions can all proceed **before or alongside** the code work — none needs the local build finished. The DR-3 audit log is called out repeatedly as the single highest-value parallel build (it also unblocks the `port25_open` flip).

---

*Companion docs: everything deferred is in [`pending-ledger.md`](pending-ledger.md); the arc's honest close is in [`arc-closing.md`](arc-closing.md).*
