# Phase 4 — the repeatable provisioning-script design (§6)

**Date:** 2026-07-18 · **Status:** DESIGN (an ordered, idempotent, fail-closed stage contract — NOT
runnable code, per spec §0.2). One level more concrete than P3's vetting checklist
([`phase-3-cutover-runbook.md`](phase-3-cutover-runbook.md) §1), still design-only.
**Authority:** [`phase-4-resilience-pool-spec.md`](phase-4-resilience-pool-spec.md) §6; anchors in
[`phase-3-first-real-box-spec.md`](phase-3-first-real-box-spec.md) §3 (V1–V8), §3.4, §4, §5.3.1, §6.

> **Why this exists.** "Adding a box is a config edit" (§7) is only true if standing the box up is a
> **push-button, repeatable** operation, not a bespoke manual ritual. This turns P3's vetting
> checklist into an **ordered stage design with hard gates**. It is a *contract*, not a shell script —
> the mechanism of each stage (which cloud API, which config tool) is a `DIAL`; the **order and the
> pass/fail gate are `LOCKED`** (the order is safety-critical: audit before enable, exactly as P3
> §5.3.1). **Never fake a real-IP check green** — each stage names the exact proof.

---

## 0. Design principles (§6.1) `LOCKED`

- **Idempotent.** Re-running a stage that already succeeded is a no-op (re-run-safe after a partial
  failure). Provisioning touches DNS/PTR/firewall — a half-applied run must be resumable, not a landmine.
- **Fail-closed at every gate.** A stage that cannot verify its own success **stops the pipeline and
  leaves the node `provisioning`/`disabled`** — it never advances a half-ready box toward `active`. The
  P3 gates (audit-green, port-25 egress proof, the GAP-24 identity) are **hard stops**, not warnings.
- **Cattle, not a pet.** Everything is reproducible from config + the node's identity, so kill+re-spin
  (P3 §6, SVG-⑦) re-runs the same script. Nothing hand-tuned survives only on the box.
- **No secrets to the box beyond P3's allowance.** Per-node token from the secrets manager (P3 §6.4);
  **no DB creds, no pepper** (the box holds no state).
- **Honesty about manual gates is a feature** (§6.2/OQ-P4-10). On the narrow port-25∧PTR host market
  (P3 GAP-16), some stages can't be fully automated uniformly; the script **pauses at a gate** for the
  operator's provider-ticket step, then resumes and verifies. Pretending it's all one-click would lie.

---

## 1. The ordered stages (§6.2) — each a fail-closed gate

Maps the build-plan's *spin → install → PTR → audit → register → warm up* to gated stages. The
**order and gate are `LOCKED`**; the mechanism is a `DIAL`. The pipeline **cannot reach stage 7
(enable)** without stages 1–6 all passing.

| # | Stage | Does | Gate (fail-closed) | Automated? (OQ-P4-10) | Anchor |
|---|---|---|---|---|---|
| 1 | **Spin** | provision the VPS on a **pre-vetted** provider (P3 V1–V8 already passed for the provider) with a dedicated, clean IP | IP reputation pre-check (P3 V3): a pre-poisoned IP is **rejected here**, before anything else | **script-with-manual-fallback** — attempt the provider API; pause if it needs a ticket | P3 §3.2 |
| 2 | **Install** | deploy the **P2/P3 forked rack + wrapper container, byte-identical** (Door 5 — gem pinned, CI-asserted); apply the firewall (ingress brain-only, egress 25+DNS only) | container health + firewall verified (external scan shows the wrapper port **closed** to the public internet — P3 §6.7) | **fully scripted** (idempotent, uniform) | P3 §3.5/§6 |
| 3 | **PTR + DNS + SPF** | set PTR (rDNS) on the **public egress IP** → verifier FQDN; A-record verifier-domain → **public egress IP**; publish SPF (GAP-17) | all three set **and propagated**; **the GAP-24 identity checked**: PTR-IP == A-record-IP == public-egress-IP | **script-with-manual-fallback** — many providers only offer PTR via a support ticket; pause-at-gate | P3 §3.3/§6.6.1 |
| 4 | **Egress proof** | one-time outbound TCP/25 smoke test to a known MX (envelope only, no DATA) | a banner is received → set `port25_open=true`; **fail ⇒ stop** (audit-green alone is insufficient — GAP-15) | **fully scripted** | P3 §3.4 |
| 5 | **Audit** | call `GET /v1/audit`; require green (`warnings=={}`) **on the public egress IP** | green **AND** `host_audit.current_host_ip == public egress IP` (the GAP-24 runtime cross-check); red ⇒ stop, node stays `provisioning` | **fully scripted** | P3 §4 |
| 6 | **Register** | add the node to the **config-list** (authoritative, P1 §5.3 — the `TM_NODE_<i>_*` block, §7); seed the `nodes` row `enabled=false`, `status='provisioning'`; per-node token in the secrets manager | node appears in the registry; the health-watcher sees it; still **ineligible** (the §3.3 filter) | **fully scripted** | P1 §5.3, §7 |
| 7 | **Warm up** | set `enabled=true`; the node enters `active` under a **low warmup cap** (P3 §5.2 durable counter, from a low start) + the per-provider limiter (§4) | first real verdicts flow, logged with the correct `verifier_ip`; block-rate watched (§5) | **fully scripted** | P3 §5.2, §4, §5 |

**`LOCKED`:** stages 3→4→5 are the P3 eligibility trilogy (DNS/PTR identity → egress works → audit
green) in the safety-critical order. The script **cannot reach stage 7 (enable)** without all of 1–6
passing — P3 §5.3.1's ordered checklist made into a pipeline with hard gates.

---

## 2. The port25_open wiring gap this design resolves (carried from the P3→P4 readiness pass)

The P3 readiness pass flagged (loudly) that **there is no operator/env/admin path to set
`port25_open=true`** in a running system — boot registration writes a literal `false`, and the
re-audit path reads it back unchanged. Stage 4 (Egress proof) **is** that path: the egress-test writer
that flips `port25_open` on a received banner. Because flipping `port25_open` is a manual/automated
control-plane mutation, its write **rides the DR-3 admin-audit log** (per-actor identity) — the
egress-test-writer + its admin path are the **first concrete slice of the DR-3 gate**, which is
buildable now in parallel with the Phase-4 build. Until that lands, the interim remains a break-glass
`cp_admin` DB write (`UPDATE nodes SET port25_open=true WHERE id=…`), recorded in the cutover runbook.

---

## 3. Relationship to "node #2 = +1 config line" (§6.3/§7) `LOCKED`

The script produces a **provisioned, audited, registered** node; the *routing activation* is the **+1
config-list line** (the numbered `TM_NODE_<i>_*` env block — proven config-only by
`TestLoad_NodeTwoIsConfigOnly_GAP31`). They are **two distinct acts**: the script makes a box *fit to
serve*; the config line makes the brain *route to it*. Both must be true; neither alone suffices (a
registered-but-not-enabled node is inert; an enabled-but-unprovisioned node fails the eligibility
filter and never gets traffic — fail-safe). This is the honest bound of GAP-31: the **brain** treats N
as a config value; the **world** still makes you earn each clean IP.

**`SEAM →` P4-later/P5:** invoking the script **automatically** (auto-scale on load, auto-re-spin a
killed node) is beyond this phase — Phase 4 makes the script **repeatable when invoked**, not
**self-invoking** (§5.7).

---

## 4. What is PENDING-REAL-BOX (never faked green)

Every stage's LOGIC is built + tested locally (against the P3 sim audit surface + the P4 sim liveness
surface + a controllable LOG). Only the real-IP **values/proofs** are deferred, each with its command
(from [`phase-3-cutover-runbook.md`](phase-3-cutover-runbook.md)):

| Stage | PENDING-REAL-BOX proof (the exact command) |
|---|---|
| 1 Spin | Spamhaus/passive blocklist check on the allocated IP **before** warmup |
| 3 PTR/DNS/SPF | `dig -x <egress-ip> +short` == FQDN; `dig +short <fqdn>` == egress-ip; `dig TXT <domain>` shows SPF |
| 4 Egress proof | `nc -vz <external-mx> 25` / `swaks --server <mx>:25 --quit-after CONNECT` → a `220` banner |
| 5 Audit | `GET /v1/audit` → `warnings:{}` AND `current_host_ip` == egress-ip (the four-way GAP-24 identity) |
| 7 Warm up | a real honest-corporate address yields a real SMTP verdict logged with a non-null `verifier_ip` |

Real **multi-box** behavior (real per-provider block-rate under load, real 2-box reputation isolation)
is the one thing the local build cannot simulate — it is PENDING-REAL-BOX and gates the **acceptance
demo**, not the build (§2.2/§13).
