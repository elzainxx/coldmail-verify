# Truemail Verification System — Phase 3 (First real box) Design Specification

**Status:** APPROVED (v1.0) — adversarial architecture + security/devops review folded in (Appendix C); **all 11 OQ-P3 signed off** (Appendix D), OQ-P3-1 (host selection) decided to be a **pre-provisioning research task**, not a design decision · **Scope:** Phase 3 only · **Type:** Planning deliverable (no code, no provisioning, no real box bought)
**Sources:** `truemail-build-plan.md` (§ PHASE 3), `truemail-architecture-detailed.svg` (§④ SMTP hardening / PTR-gate, §⑤ provider trust, §⑦ node lifecycle, §⑧ topology), the **approved** `phase-1-foundations-spec.md` and `phase-2-smtp-mocks-spec.md` (frozen contracts inherited), and upstream ground truth from the `truemail` gem `host_audit` + `truemail-rack` (captured in Appendix A).
**Inherits (frozen, not redesigned):** the five one-way doors — `/verify` response shape, LOG schema, email hashing, `status`/`reason` vocabularies, node-list/never-fork. Phase 3 is **additive-only** against all five, and adds **no new `reason`/`status`/`signals`** beyond what P1/P2 already reserved. Phase 3 is almost entirely **infrastructure + operational** — it turns already-built contracts into a live, audited, IP-clean box.

---

## 0. About this document

### 0.1 Purpose
Expand **Phase 3 — First real box (port 25 enters)** of the build plan into a complete, buildable design specification, at the same rigor as the Phase 1 and Phase 2 specs. Phase 3's goal, stated by the build plan:

> One clean, audited SMTP box in the pool… **one audited box returns real SMTP verdicts on honest domains, and big providers are safely routed to `:mx`.**

This is the phase where the whole Phase-2 promise — "switching to a real server is literally one config value" — is **cashed in and stress-tested**. Nothing in the brain's logic changes; Phase 3 is provisioning, an audit gate, and a selective go-live. Because it introduces **a real IP talking to real mail servers**, the two capabilities carrying the most weight here are **senior-devops** (can we even get a port-25 + PTR host, and deploy safely) and **senior-security** (the box is now internet-facing on egress and brain-reachable on ingress — lock it down).

### 0.2 Scope discipline
- **In scope:** Everything under build-plan § PHASE 3 — (a) **provisioning** the real box as a *vetting checklist + requirements* (outbound port 25 confirmed, settable PTR, clean IP, verifier domain + SPF, PTR matching verifier domain, forked rack+wrapper deployed, box on private network / brain-only); (b) the **`/audit` gate** — wrapping `host_audit`, the go/no-go rule (ineligible for `:smtp` until green), scheduled re-audit, auto-quarantine-on-red **as a design**; (c) **going live selectively** — the `validation_type_for` routing map, the warmup ramp, the mock→real config flip; (d) confirming real verdicts flow and log with `verifier_ip` tagged.
- **Out of scope:** Phases 4–7. Where a Phase 3 decision touches a later phase, this doc marks the **`SEAM →`** and stops. In particular: **the health-watcher is NOT built** (Phase 4 — this doc designs only the *audit signal* and the *gate*, §4.5); **no pool / 2nd box / router strategies** (Phase 4); **no queue / greylist retry** (Phase 5); **no per-provider rate-limit machinery** (Phase 4 — Phase 3's warmup ramp is a *minimal ascending cap*, explicitly not the P4 rate limiter, §5.2); **no third-party fallback** (Phase 6).
- **No implementation:** no Ruby, no Go, no Dockerfiles, no Terraform, no `dig`/`nc` scripts. Requirements, checklists, contract deltas, decision rules, and interface *shapes* only. The provisioning section is a **vetting checklist**, not a run script (build-plan §3.1 wording: "described as a design/checklist — not a run script yet").
- **Additive-only against Phases 1–2:** Phase 3 **populates** reserved `nodes` columns (`audit_status`, `last_audit_at`, `port25_open` — P1 §4.5) and the LOG's reserved `verifier_ip` (P1 §3.2.2); it **reuses** the P2 `/v1/audit`, `/v1/config`, `/v1/config/reload` contracts (P2 §4.2/§4.6/§4.5) rather than inventing new ones. It changes **nothing** already frozen.

### 0.3 How to read the tagging (identical to Phases 1–2)

| Tag | Meaning |
|---|---|
| **`LOCKED`** | A one-way door (inherited) or an already-locked build-plan decision. Additive-only afterwards. |
| **`DIAL`** | A build-time / runtime value tunable later without breaking history or consumers (warmup volumes, timeouts, cadences). |
| **`DECISION REQUIRED`** | An open choice surfaced with a recommendation but **not** assumed. Needs sign-off before build. |
| **`SEAM →`** | A Phase 3 design point that deliberately leaves room for a later phase; noted, not built. |
| **`GAP`** | A contradiction/omission found between the build plan, the SVG, the P1/P2 specs, and/or upstream truemail reality — flagged, not silently resolved. |

> **Priority rule (governs this doc):** the **`/audit` gate (§4)** is specified in full — it is the **go/no-go for the entire phase** and the one non-negotiable (SVG §④: "PTR-gated: no SMTP until `/audit` green — non-negotiable"). The **provisioning vetting (§3)** and **security posture (§6)** are specified to devops/security rigor because they are where Phase 3 actually fails in the real world. Going-live (§5) is specified to contract discipline. Everything else is specified well enough to build and no further.

### 0.4 The two principles Phase 3 must not violate
Both inherited, restated because every Phase-3 decision descends from them:

1. **Harden toward `unknown`, never `invalid`** (P1 §7.5, P2 §0.4). A real box that is blocked, greylisted, tarpitted, or rate-limited by a real provider is a **your-fault `unknown`**, never an address `invalid`. Phase 3 *increases* the surface for your-fault outcomes (real providers actively defend) — so this principle matters more, not less, once the IP is real.
2. **PTR-gated SMTP is non-negotiable** (SVG §④, build-plan §3.2). **No node runs `:smtp` until its `/audit` is green.** A red audit means real mail servers will distrust the connection — verdicts become unreliable *and* the IP's reputation burns. This is the phase's hard gate (§4.4).

---

## 1. Phase 3 at a glance

### 1.1 What Phase 3 delivers (from the build plan)
> **Phase 3 done when:** one audited box returns real SMTP verdicts on honest domains, and big providers are safely routed to `:mx`.

Phase 2 built and proved the entire SMTP path against fakes; the brain's routing, parsing, classification, scoring, and logging are identical against mock and real (P2 §2). Phase 3 introduces **exactly one new thing the mocks could not provide: a real IP with real reverse DNS talking to real mail servers.** Everything else is inherited. The deliverable is: **provision → audit-green → go-live-selectively → real verdicts logged with `verifier_ip`.**

### 1.2 What Phase 3 consumes from Phases 1–2 (the seams now cashed in)
Phase 3 builds almost nothing new in code — it **activates** what earlier phases reserved:

| Consumed from | What it is | Phase 3 action |
|---|---|---|
| P2 §4.2 | `GET /v1/audit` contract (built, read/logged, gate deferred) | **Enforce the gate** — no `:smtp` until green (§4.4). |
| P2 §4.6 | `GET /v1/config` (full effective config; `config_fingerprint` honest) | Fingerprint now covers the **real** engine config (`smtp_port:25`, real `dns`). |
| P2 §4.5 | `POST /v1/config/reload` (pull; proven) | The **mechanism of the mock→real flip** (§5.3). |
| P1 §4.5 | `nodes.audit_status`, `last_audit_at`, `port25_open` (reserved `SEAM → P3`) | **Populate** them; drive the `provisioning→active→quarantined` lifecycle (§4.5, SVG ⑦). |
| P1 §3.2.2 | `verifications.verifier_ip` (`inet`, nullable — "`null` in P1/cheap-only") | **Populate** it with the box's real SMTP egress IP for every SMTP verdict (§5.4). |
| P1 §6.9 / P2 §5.5-note | Provider routing (Gmail/O365/Yahoo → `:mx`) | Make it **real** at the engine via `validation_type_for` (§5.1). |
| P2 §7.3 (OQ-P2-8) | `cold-email` policy has `smtp` enabled, routed to the **mock** node | **Flip the node to real** — policy stays stable (the config-flip goal). |

### 1.3 The frozen inheritance (context, not re-litigated)
Carried verbatim from the approved P1/P2 specs — **none may change; Phase 3 only activates or populates:**

- **Door 1** — `/verify` response shape versioned & additive-only. P3 **populates** the reserved `meta.verifier_ip`/`meta.node_id`/`meta.engine_version` with real values; adds **no** field.
- **Door 2** — the LOG is append-only; the raw engine blob lives in `verification_raw`. P3 writes real `verifier_ip` (a reserved column) and real transcripts (existing sidecar). **No schema change.**
- **Door 3** — `norm/v1 + hmac-sha256/v1`, `bytea`, global pepper. P3 changes nothing.
- **Door 4** — `status`/`reason`/`recommendation` enums additive-only. **P3 emits zero new values** — real SMTP produces exactly the reasons P2 already made real (`accepted`, `mailbox_not_found`, `mailbox_full`, `catch_all`, `smtp_blocked`, `smtp_timeout`, `smtp_error`, `connection_failed`, `greylisted`).
- **Door 5** — the **truemail gem is never forked**; only **truemail-rack** is. Phase 3 deploys the P2 fork **unchanged** and adds only **infrastructure** (host, DNS, PTR, firewall) — the least Door-5-risky phase, since nothing in the software layer changes. CI still asserts the gem unmodified.
- Limit handling: blocked/limited/broken/timeout → **`unknown`, never `invalid`**.
- Boxes are dumb, stateless, disposable; brain holds all state; boxes never talk to each other; boxes never touch the DB. **This is now a security property** (§6): a compromised or IP-burned box loses no data and is killed + re-spun (SVG ⑦).

### 1.4 The make-or-break, named up front
Two facts dominate everything below, and both are outside our software:

- **The host market is narrow (§3.1).** A cheap VPS that **genuinely allows outbound port 25 AND lets you set PTR** is the exception, not the rule — most mainstream clouds block port 25 by default and many never grant custom rDNS. **This is the single largest execution risk of Phase 3**, and it is a procurement/vetting problem, not a design problem. The build plan flags it ("vet before paying"); this spec treats it as the gating constraint.
- **The audit is necessary but not sufficient (GAP-15, §4.6).** `host_audit` proves **IP/DNS/PTR alignment** — it does **not** prove **outbound port 25 actually works** or that **SPF is published**. A box can be **audit-green and still unable to send a single SMTP packet** because egress 25 is silently blocked. Phase 3 must verify port-25 egress **separately** from the audit. Missing this is the most likely way to declare a dead box "ready."

---

## 2. System context for Phase 3

Phase 2 lit up the SMTP path against fakes; the box was a mock-backed rack, `smtp_port`/`dns` pointed at `smtp_mock`/`dns_mock`, and `verifier_ip` was `null`. Phase 3 replaces the fake substrate with a **real host on a real IP with real reverse DNS**, gated by `/audit`. **The brain does not change.**

```
                    ┌─────────────────────────────────────────────────────┐
   caller ──POST──▶ │  BRAIN (Go, good VPS, port 25 CLOSED) — UNCHANGED     │
   /v1/verify       │  front door → cache → policy → cheap layers →         │
                    │  provider routing → router(node-list) → [SMTP call]   │
                    │  → SCORING (SMTP-aware, P2 §7) → LOG → cache → resp    │
                    └──────┬───────────────────────────────────┬───────────┘
                           │ reads/writes                       │ HTTPS  Authorization: <node-token>
                           ▼                        PRIVATE NET  │  (brain-only ingress; §6)
                    ┌───────────────┐         ┌──────────────────┴───────────────────────────┐
                    │  Supabase     │         │  REAL NODE = forked truemail-rack + wrapper   │
                    │  (all state)  │         │  ┌──────────────────────────────────────┐    │
                    │  + nodes.*    │         │  │ truemail GEM (pinned, UNFORKED)        │    │
                    │    audit_*    │         │  │  regex→mx→mx_blacklist→SMTP            │    │
                    └───────────────┘         │  └──────────────────────────────────────┘    │
                                              │        │ dns: REAL       │ smtp_port: 25       │
                                              │        ▼ resolver        ▼ (egress 25 open)    │
                                              └────────┼─────────────────┼────────────────────┘
                                                       │                 │  OUTBOUND port 25
                                                       ▼                 ▼  to REAL mail servers
                                          ┌──────────────────┐   ┌──────────────────────────┐
                                          │  real DNS +       │   │  honest corporate MX      │
                                          │  PTR (rDNS) set   │   │  (Gmail/O365/Yahoo → :mx, │
                                          │  to verifier dom  │   │   never SMTP'd — §5.1)    │
                                          └──────────────────┘   └──────────────────────────┘
```

**Phase 3 truth (the config-flip cashed in):** the *only* things that change from a Phase-2 mock node to this real node are **infrastructure and one config value per the flip** — the node descriptor's `base_url` (now the real box), the engine's `dns:` (real resolver, or empty for system default) and `smtp_port:` (real `25`), and the node's `enabled`/`audit_status` gating. **Every line of brain code — routing, parsing, classification, scoring, logging — is byte-identical to Phase 2.** If Phase 3 requires a brain code change to work against a real box, the Phase-2 config-flip promise was **false**, and that is a `GAP` to raise, not to patch around (§5.3 verifies this explicitly).

**What is genuinely new in Phase 3, by layer:**
- **Infrastructure (the bulk of the phase):** a real VPS with outbound 25 + settable PTR; a verifier domain with an A-record and **SPF**; PTR (rDNS) pointing back at the verifier domain; a private network / firewall so only the brain can reach the box (§6).
- **Brain `internal/router` + `nodes` state:** the **audit gate** — the router must exclude a node whose `audit_status ≠ green` from `:smtp` candidacy (§4.4). This is the one meaningful brain behavior added, and it is small (a candidacy filter, not new routing logic).
- **Brain orchestration:** a **scheduled re-audit** call and an **auto-quarantine-on-red** state transition (the *signal + gate* only; the continuous *health-watcher* is P4 — §4.5).
- **Operational:** the warmup ramp (§5.2) and the go-live runbook (§5, §6.7).
- **Supabase:** **no schema change** — `nodes.audit_status/last_audit_at/port25_open` and `verifications.verifier_ip` were all reserved in P1. Phase 3 writes real values into reserved columns.

---

## 3. Provisioning the box — a vetting checklist + requirements (build-plan §3.1)

> The build plan is explicit: *"Get a cheap VPS that **genuinely allows outbound port 25** AND **settable PTR** — vet before paying."* This section is a **vetting checklist and a requirements spec**, **not** a run script (build-plan wording). It exists because Phase 3's real failure mode is procurement, not code.

### 3.1 The narrow-market reality `LOCKED` (as the gating risk) — read this first
A host that meets **both** hard requirements — **(a) outbound TCP/25 genuinely open** and **(b) custom PTR / rDNS you can set** — is a **narrow market**. The mainstream reality:
- **Most large clouds block outbound port 25 by default** (AWS, GCP, Azure, Oracle, most of DigitalOcean's newer accounts). Some *never* unblock it; some require a support request that is frequently denied for new/low-history accounts.
- **Custom rDNS/PTR is often unavailable** on the cheapest tiers or requires the IP to be "yours" (BYOIP / dedicated IP), which changes the price bracket.
- The intersection is served mostly by **email-friendly VPS / bare-metal providers** and some second-tier clouds — a smaller, vettable set.

**Consequence for the plan (`DECISION REQUIRED`, OQ-P3-1):** treat host selection as a **pre-purchase vetting gate** with a **trial/refundable** box, and **prove both requirements on the trial box before committing** (§3.4). **Recommendation:** shortlist 2–3 known-email-friendly providers, take the cheapest that passes the §3.2 pre-purchase checks on a trial instance, and keep a documented fallback provider — because a provider that works today can block 25 tomorrow. Do **not** pay for a year up front on an unproven host.

> `GAP-16` (build-plan vs reality): the build plan says "**cheap** VPS" and "genuinely allows outbound port 25 AND settable PTR" in the same breath. In practice **cheap ∧ port-25 ∧ settable-PTR ∧ clean-IP** is an over-constrained set; something usually gives (price, or IP cleanliness, or the effort of a support ticket). Flagged so "cheap" is understood as "cheapest that passes vetting," not "the $3 box." Not resolved by design — it is a procurement trade-off (OQ-P3-1).

### 3.2 Pre-purchase vetting checklist (do this on a trial box, before committing) `LOCKED` (the gate)
Each item is **pass/fail before money is committed.** A fail on a hard item disqualifies the host.

| # | Requirement | Hard? | How it's vetted (design, not script) | Fail consequence |
|---|---|---|---|---|
| V1 | **Outbound TCP/25 genuinely open** | **HARD** | From the trial box, confirm an outbound SMTP connection to a known external MX on port 25 actually establishes (not just "the provider says it's open"). See §3.4 — this is the **port-25 egress proof**, separate from `/audit` (GAP-15). | Disqualify. Nothing else matters if 25 is blocked. |
| V2 | **Settable PTR / rDNS** | **HARD** | Confirm the provider's control panel / API lets you set a **custom PTR** on the box's IP to an arbitrary hostname (the verifier FQDN). Confirm it actually propagates. | Disqualify — a box with no controllable PTR can never pass `/audit` (§4). |
| V3 | **Clean IP (no spam history)** | **HARD (at provision)** | Check the assigned IP against major reputation/blocklists (Spamhaus etc.) and passive history **before** warmup. A pre-poisoned IP starts the phase already losing. | Reject the IP; request a different one, or a different provider. |
| V4 | **Dedicated / stable IP** | HARD | The IP must be yours for the box's life (PTR + reputation are per-IP; a shared/rotating IP makes PTR and reputation meaningless). | Disqualify shared-IP-only offerings. |
| V5 | **Static outbound IP == the audited IP** | HARD | Some hosts NAT egress through a *different* IP than the box's public/PTR'd IP. The IP that opens port-25 outbound **must be the one PTR points at** — otherwise real MX see an un-PTR'd source and `verifier_ip` is wrong. Vet that egress source IP == PTR'd IP (§3.4, §6.6). | Disqualify or reconfigure; this silently breaks reputation + `verifier_ip` accuracy. |
| V6 | Provider does not prohibit bulk SMTP / verification in ToS | HARD | Read the AUP. Some "email-friendly" hosts still forbid verification traffic. | Disqualify — ToS violation risks mid-phase shutdown. |
| V7 | Reasonable network reliability / uptime | SOFT | Provider reputation, status history. | Prefer another; not disqualifying for a first box. |
| V8 | Region sanity | SOFT (`DIAL`) | A region not itself broadly blocklisted; latency to targets acceptable. | Tune; not disqualifying. |

### 3.3 The identity/DNS requirements (what "audit-green" will require) `LOCKED`
These are the **inputs** the `/audit` gate (§4) checks. They must be provisioned **before** first audit and are the substance of `host_audit`'s three dimensions (Appendix A.1):

- **Verifier domain** — a real domain (or subdomain) we control, used as the SMTP `verifier_email`/`verifier_domain` (MAIL FROM identity). **`DECISION REQUIRED` (OQ-P3-2):** dedicated verifier domain vs a subdomain of an existing domain. **Recommendation:** a **dedicated domain (or a dedicated subdomain isolated by SPF)** so verification traffic's reputation is **isolated** from any real sending/corporate domain — a burned verifier domain must never taint a domain you send real mail from. (This mirrors the box being disposable: the verifier identity should be too.)
- **A-record: verifier domain → box IP.** `host_audit`'s **DNS** dimension checks the verifier domain's A-record resolves to the box's current IP (Appendix A.1). Must be set.
- **PTR (rDNS): box IP → verifier domain.** `host_audit`'s **PTR** dimension checks reverse DNS references the verifier domain. This is **the** make-or-break record (§4.2). Set via the provider (V2).
- **SPF on the verifier domain** — a published SPF TXT record authorizing the box IP as a sender for the verifier domain. **`GAP-15` / `GAP-17`:** SPF is a build-plan §3.1 requirement (**"verifier domain + SPF"**) but **`host_audit` does NOT check SPF** (Appendix A.1 — it checks IP/DNS/PTR only). Therefore **SPF cannot be part of the audit-green signal** and must be a **separate provisioning checklist item**, verified out-of-band (§3.4). Flagged so SPF is not assumed covered by `/audit`.
  - **`DECISION REQUIRED` (OQ-P3-3):** SPF matters for *sending* reputation; a pure RCPT-only verifier that never issues `DATA` (P2 §5.7 — the probe/verify never sends a message) arguably exercises SPF little. **Recommendation:** publish SPF anyway (it's cheap, it's in the build plan, and some MX weigh envelope-sender domain alignment even at RCPT time); treat it as **required-but-not-audit-gating**. Flagged because the build plan lists it without saying it isn't audited.

### 3.4 The port-25 egress proof + SPF check — separate from `/audit` `LOCKED` (that they're separate) — resolves GAP-15
Because `host_audit` checks only IP/DNS/PTR (Appendix A.1), **two provisioning facts are NOT covered by audit-green and must be proven independently before go-live:**

| Fact | Why audit can't prove it | How Phase 3 proves it (design) |
|---|---|---|
| **Outbound port 25 actually works** | `host_audit` never opens an outbound SMTP connection; it inspects the host's own IP/DNS/PTR. A box with 25 blocked can be **fully audit-green**. | A **one-time egress smoke test**: from the box, establish a real outbound TCP/25 session to a known external MX and confirm a banner/greeting is received (envelope only, no `DATA`, no mail sent). This is a **provisioning gate**, logged as a node capability → sets `nodes.port25_open = true` (the P1-reserved column, §4.5). |
| **SPF is published + correct** | `host_audit` doesn't check SPF (GAP-17). | An out-of-band DNS check that the verifier domain publishes an SPF TXT authorizing the box IP. A **provisioning checklist item**, not an audit dimension. |

- **`LOCKED` rule:** **a node is `:smtp`-eligible only if `audit_status == green` AND `port25_open == true`.** The audit gate (§4.4) and the egress proof are **both** required; neither alone is sufficient. This is the direct resolution of GAP-15 ("audit necessary but not sufficient").
- `nodes.port25_open` (P1 §4.5, reserved) is written by the egress smoke test, **not** by `/audit`. The two signals are stored in two columns for exactly this reason.

### 3.5 Deploy the forked rack + wrapper (unchanged from P2) `LOCKED` (Door 5)
- Deploy the **Phase-2 forked truemail-rack + wrapper container**, byte-for-byte — the `/v1/verify`, `/v1/audit`, `/v1/config`, `/v1/config/reload`, `/v1/catchall`, `/v1/stats` endpoints (P2 §4). **No code change** for Phase 3; the gem stays pinned and CI-asserted-unmodified (Door 5).
- The box is configured (env / config source, per OQ-P2-2 pull model) with the **real** `verifier_email`/`verifier_domain`, `smtp_port: 25`, and a **real** `dns:` (real resolver or system default) — replacing the mock ports. This config is what `/v1/config` (P2 §4.6) exposes and what the brain fingerprints.
- **`DECISION REQUIRED` (OQ-P3-4):** deploy topology — container on the VPS directly vs orchestrated. **Recommendation:** a single container on the VPS (the box is disposable; orchestration is P4 pool concern). Kept minimal for a first box.

### 3.6 Provisioning outcome → node registry `LOCKED` (uses P1/P2 shapes)
When §3.2–§3.5 pass, the node is registered (config-list authoritative, P1 §5.3 / OQ-5; `nodes` table mirrors, P1 §4.5) with: `base_url` (private-network address, §6), `token_ref` (node token in secrets manager, not in `nodes` clear — P1 §4.5/OQ-13), `enabled=false` initially, `status='provisioning'`, `port25_open` (from §3.4). The node stays **`provisioning` → not `:smtp`-eligible** until the first green audit (§4.4). This is the SVG ⑦ lifecycle entry point.

---

## 4. The `/audit` gate — the go/no-go for the whole phase (highest rigor)

Build plan §3.2 calls `/audit` **"the single most valuable added endpoint."** SVG §④ makes it **non-negotiable: "PTR-gated: no SMTP until `/audit` green."** This section specifies the audit **signal**, the **gate rule**, the **scheduled re-audit**, and **auto-quarantine-on-red** — as a design. The continuous health-watcher that *polls* audit + block-rate is **Phase 4**; Phase 3 designs only the audit signal and the gate that consumes it (build-plan §3.2 wording, honored).

### 4.1 What the audit actually is (anchored on `host_audit`, not invented) `LOCKED`
The `/v1/audit` endpoint (built in P2 §4.2) wraps the gem's `Truemail.host_audit` (Appendix A.1/A.6). `host_audit` returns **exactly**:
```jsonc
{ "current_host_ip": "<ip>", "warnings": { } }          // {} == green
// or:
{ "current_host_ip": "<ip>",
  "warnings": { "dns": "A-record of verifier domain not refers to current host ip address",
                "ptr": "PTR-record does not reference to current verifier domain" } }
```
- **The three dimensions `host_audit` checks (and ONLY these) `LOCKED`:**
  1. **IP** — can the host determine its own current public IP (reachability / detect IP).
  2. **DNS** — does the **verifier domain's A-record resolve to the current host IP** (forward alignment).
  3. **PTR** — does **reverse DNS for the host IP reference the verifier domain** (reverse alignment).
- **`warnings == {}` ⇒ green.** Any non-empty `warnings` ⇒ red. The wrapper already derives `audit_status: "green|red"` from this (P2 §4.2). **`LOCKED`: green is defined as `warnings == {}` — nothing else.** We do not invent audit fields (per the prompt: "Don't invent audit fields").
- **`GAP-15` restated as a hard boundary:** `host_audit` does **not** check outbound port 25 or SPF (§3.4). Therefore **audit-green is necessary but not sufficient** for `:smtp`-eligibility; the full gate is `green AND port25_open` (§4.4). This is the most important correction Phase 3 makes to a naive "audit == ready" reading.

### 4.2 Why PTR/rDNS is the make-or-break `LOCKED` (rationale)
The prompt calls PTR "the make-or-break." The reasoning, made explicit so the hard-block rule (§4.4) is understood, not just obeyed:
- Real mail servers routinely **reject or distrust SMTP connections from IPs with missing or mismatched reverse DNS** (no PTR, or PTR not aligning to the connecting identity). This is a first-line anti-spam heuristic.
- A box with a **red PTR** will therefore get **more of its SMTP attempts blocked/deferred by real MX** — which, under harden-toward-`unknown`, converts otherwise-decidable addresses into **`unknown/smtp_blocked`**. So a red-PTR box doesn't just risk reputation; **it silently degrades verdict quality into noise** while *looking* like it's working.
- And every blocked attempt from a misconfigured IP **teaches real providers to distrust the IP** — burning the one asset (a clean IP) Phase 3 exists to establish.
- **Therefore:** running `:smtp` on a red audit is **doubly destructive** (bad verdicts + burned IP) with **no upside**. Hence the gate is a **hard block, not a warning** (§4.4).

### 4.3 The audit signal — what the brain records `LOCKED` (uses reserved columns)
On every audit (initial + scheduled + on-demand), the brain reads `/v1/audit` and writes to `nodes` (P1 §4.5 reserved columns — **no schema change**):

| `nodes` column (P1-reserved) | Written from | Meaning |
|---|---|---|
| `audit_status` | `warnings == {} ? 'green' : 'red'` | The gate input. |
| `last_audit_at` | audit response `audited_at` (RFC3339 UTC) | Freshness; drives staleness re-audit (§4.5). |
| `port25_open` | the §3.4 egress smoke test (NOT the audit) | Second half of eligibility. |
| `status` | lifecycle (`provisioning\|active\|quarantined`) | SVG ⑦ state (§4.5). |

- **The raw `warnings` object is preserved** (which dimension failed — dns vs ptr) for operator diagnosis. **`DECISION REQUIRED` (OQ-P3-5):** where to store the raw warnings — a `nodes.last_audit_warnings jsonb` column vs an append-only `node_audits` history table. **Recommendation:** an **append-only `node_audits(node_id, audited_at, audit_status, warnings jsonb, current_host_ip)` history table** — audit results are **historical data** (Door-2 spirit: "what you don't log now is gone forever"), and a green→red→green flap history is exactly what P4's health-watcher will want. A single mutable column on `nodes` would overwrite that history. This is a **new table, additive**, not a change to any frozen schema — flagged as a decision because P1 didn't reserve it (it only reserved the summary columns on `nodes`). `SEAM →` P4 consumes this history; P3 just writes it.

### 4.4 The gate rule `LOCKED` — non-negotiable, the phase's spine
> **RULE (`LOCKED`, SVG §④ "non-negotiable"): A node is eligible to run `:smtp` if and only if `audit_status == green` AND `port25_open == true`. A node that is `red` (or stale, or egress-unproven) is excluded from `:smtp` candidacy — full stop.**

Enforcement is a **router candidacy filter** (brain `internal/router`, the one meaningful brain behavior Phase 3 adds):
- The router's `:smtp` candidate set = nodes where `enabled ∧ status=='active' ∧ audit_status=='green' ∧ port25_open ∧ audit-fresh` (§4.5 staleness).
- **What happens to an address when NO node is `:smtp`-eligible** (the first box is red, or the only box just got quarantined) — **`DECISION REQUIRED` (OQ-P3-6)`, and it must not silently become `invalid`:**
  - **Recommendation:** the address **falls back to the best cheap-layer verdict** — i.e., the honest Phase-1 outcome (`risky/plausible`, or `provider_no_smtp` for big providers), **never `invalid`**, and the SMTP layer is simply **not run**. This is exactly harden-toward-`unknown`/`risky`: absence of an eligible verifier is *our* limitation, not proof about the address. The verdict is logged with `verifier_ip=null` and a signal noting SMTP was skipped for lack of an eligible node.
  - **`SEAM →` P4:** with a pool, "no eligible node" triggers re-route/quarantine-recovery; in P3 (one box) it degrades gracefully to the cheap verdict. Not built here beyond the single-box degrade.
  - **New additive signal (§8):** `smtp_skipped_reason: "no_eligible_node" | "audit_red" | "provider_mx_routed" | "warmup_cap"` — an additive `signals` key so the LOG records *why* SMTP didn't run. Additive-only (Door 1); no reason/status change.
  - **`LOCKED` cache rule (extends P2 OQ-P2-6 — the "cache only settled verdicts" predicate):** a verdict that fell back to cheap **because a verifier was unavailable** (`smtp_skipped_reason ∈ {no_eligible_node, audit_red, warmup_cap}`) is **PROVISIONAL, not settled → non-cacheable.** Caching it would freeze a lower-quality answer and **suppress the SMTP verdict that becomes available once the node recovers or the cap lifts** — identical in spirit to P2's "never cache a transient `unknown`." By contrast, `provider_mx_routed` is a **settled** outcome (the provider is always `:mx`-routed; SMTP was never going to run) → `risky/provider_no_smtp` remains **cacheable** as before. The discriminator is *why* SMTP was skipped: **capacity/eligibility gap → provisional/non-cacheable; policy routing → settled/cacheable.** (Still logged either way — it's a real answer we served.)
- **`LOCKED`:** the gate is evaluated **at routing time from `nodes` state**, so a node going red between audits stops receiving `:smtp` work as soon as its state flips (§4.5), without a redeploy.

### 4.5 Scheduled re-audit + auto-quarantine-on-red — design only (health-watcher is P4) `LOCKED` (the signal+gate) / `SEAM →` P4 (the watcher)
Build plan §3.2: *"Schedule re-audit; auto-quarantine on red."* The prompt: *"design only the audit signal and the gate here"* (the watcher itself is P4). So Phase 3 specifies **the state machine and its triggers**, not a polling daemon.

- **Re-audit triggers (`DIAL` cadence, `LOCKED` that it happens):**
  - **Scheduled:** re-run `/v1/audit` on a cadence (**`DIAL`**, recommend **hourly** for a first box — PTR/DNS rarely change but silent breakage must be caught fast).
  - **Staleness:** if `last_audit_at` is older than a max-age (**`DIAL`**, recommend 2× the schedule), the node is treated as **not audit-fresh → excluded from `:smtp`** (§4.4) until re-audited — a stale green is not trusted.
  - **On-demand:** after any `/v1/config/reload` that could change verifier identity/IP (§5.3), and at go-live.
- **The lifecycle state machine (SVG ⑦) `LOCKED`:**
  ```
  provisioning ──(first audit green ∧ port25_open ∧ SPF-checked)──▶ active
      active ──(audit red │ egress lost)──▶ quarantined         [auto, on red]
      quarantined ──(re-audit clean)──▶ active                   [SVG ⑦ "re-audit clean → back to active"]
      any ──(operator disable)──▶ disabled
  ```
- **Auto-quarantine-on-red (`LOCKED` as a design; the *actor* is a P3 minimal scheduler, the *continuous watcher* is P4):** when a re-audit returns red, the brain sets `nodes.status='quarantined'` and `audit_status='red'`; the §4.4 candidacy filter then **immediately** stops routing `:smtp` to it. **`DECISION REQUIRED` (OQ-P3-7):** does auto-quarantine require **N consecutive red audits** (debounce a transient DNS blip) or fire on the first red? **Recommendation:** **quarantine on the first red** (fail-safe — a red audit is cheap to recover from and the downside of running on a genuinely-red box is IP damage), but **require N consecutive greens to auto-recover** (debounce flapping back to active). Asymmetric: quick to quarantine, slow to trust. Flagged.
- **`SEAM →` P4 (explicitly NOT built now):** the **continuous health-watcher** that also polls `/health` and **block-rate per node**, correlates signals, and manages a *pool* of quarantine/recovery (build-plan §4) is Phase 4. Phase 3 builds only: (a) the audit signal (§4.3), (b) the gate (§4.4), (c) a **minimal scheduled re-audit + the quarantine state transition** for the single box. Block-rate-driven quarantine (as opposed to audit-driven) is P4.
  - **`GAP-18`:** SVG ⑦ shows quarantine triggered by **"block/audit red"** — i.e., quarantine can be driven by **block-rate**, not only audit. But block-rate monitoring is the **P4 health-watcher**. Phase 3 therefore implements **audit-driven quarantine only**; **block-rate-driven quarantine is deferred to P4**. Flagged so the SVG's combined trigger isn't read as "P3 must watch block-rate." A first box that is getting blocked despite green audit will show up as rising `unknown/smtp_blocked` in the LOG — visible to an operator, but auto-response is P4.

### 4.6 Audit substrate note (how it was already proven, and what's new) `LOCKED`
- The `/v1/audit` **contract** was proven in P2 §4.2 against `dns_mock` PTR/A fixtures (green and red reproducible without a real host). Phase 3 adds **nothing to the contract** — it runs the same endpoint against **real** DNS/PTR.
- What is **new and unprovable-until-now:** a **real** PTR record set at a **real** provider, resolving via **real** reverse DNS. The audit logic is unchanged; only the substrate is real. This is the cleanest example of the config-flip principle: the audit code path is identical mock→real.

---

## 5. Going live selectively (build-plan §3.3)

Once a node is `active` (green audit + port25_open + SPF checked), Phase 3 goes live **selectively and gradually** — never all traffic to a fresh IP at once.

### 5.1 The `validation_type_for` routing map — honest domains `:smtp`, big providers `:mx` `LOCKED` (the principle) / `DIAL` (the list)
Build plan §3.3: *"honest corporate domains → `:smtp`; **Gmail/O365/Yahoo → `:mx`**."* SVG ⑤ ("famous providers — trust their 'no' a little, their 'yes' never"). This is enforced at **two layers that must agree** — the brain's provider routing (P1 §6.9) and the engine's own `validation_type_for` (Appendix A.1):

- **Brain-side (already built, P1 §6.9 / P2 §5.5-note):** the brain classifies `mx_provider` and routes Gmail/Yahoo/O365 to `:mx` — they **never reach the SMTP call**. Their 250s are "pure risk" (SVG ⑤), so an SMTP `valid` is never minted for them. This yields `risky/provider_no_smtp` (P1 OQ-14). **Unchanged in P3.**
- **Engine-side (`validation_type_for`, now made real):** the box's truemail config sets `validation_type_for` so that even if a big-provider address reached the engine, the engine itself does `:mx`, not `:smtp` (defense in depth). The map: `{ "gmail.com": "mx", "googlemail.com": "mx", "outlook.com": "mx", "hotmail.com": "mx", "live.com": "mx", "office365.com"/O365-tenanted → "mx", "yahoo.com": "mx", ... }`, with `default_validation_type: "smtp"` for everything else (honest corporate).
- **`GAP-19` (which layer owns the routing — a real ambiguity to flag, not silently resolve):** provider routing is now specified in **two places** — the brain (skip SMTP entirely) and the engine (`validation_type_for` does `:mx`). If they disagree (brain says `:smtp`, engine map says `:mx` for the same domain), behavior is ambiguous. **`DECISION REQUIRED` (OQ-P3-8). Recommendation:** the **brain is authoritative** for routing (it decides whether the SMTP call happens at all); the engine's `validation_type_for` is a **defense-in-depth backstop** configured to **match** the brain's provider list, kept in sync via the same source of truth. The brain never sends a `:mx`-classified provider to the box for SMTP, so the engine map is belt-and-suspenders, never the primary decision. Keeping the two lists in sync is a **`DIAL`/ops concern** (one source → both consumers). Flagged because two independent routing tables are a classic drift bug.
  - **Concrete single-source mechanism (`LOCKED` shape, so "keep in sync" isn't hand-waved):** the provider→`:mx` list lives in **one** brain-side source (the policy's `provider_routing` map — P1 §7.2, already exists). The engine's `validation_type_for` is **rendered from that same map** into the box config at provision/reload time (§5.3, via `/v1/config/reload`), never hand-authored on the box. So there is **one editable list**; the box's copy is a **generated artifact**, and `/v1/config` (§5.4) lets the brain **assert** the box's `validation_type_for` matches the expected render — a mismatch is a logged drift alarm, not silent divergence. This converts GAP-19 from "two tables that might drift" into "one source, one generated copy, one equality check."
- **O365 nuance (SVG ⑤):** the SVG distinguishes "Gmail/Yahoo → `:mx` only (250s = pure risk)" from "O365 → maybe SMTP, trust 550 low-conf." **`DECISION REQUIRED` (OQ-P3-9). Recommendation for P3:** keep **all three (Gmail/O365/Yahoo) `:mx`** for the first box — the SVG's "O365 maybe SMTP" is an optimization that needs a warmed, trusted IP and per-provider trust calibration (Phase 7). Starting O365 at `:mx` is the safe default; promoting O365 to `:smtp` is a later `DIAL` once the IP is trusted. Flagged so the SVG's O365 subtlety isn't lost — it's deliberately deferred, not overlooked.
- **`DIAL`:** the exact provider→`:mx` list is tunable and grows; the **principle** (big consumer providers never SMTP'd from our IP; their "yes" is worthless) is `LOCKED` (SVG ⑤).

### 5.2 The warmup ramp — a minimal ascending cap, NOT the P4 rate limiter `LOCKED` (that we warm up) / `DIAL` (volumes)
Build plan §3.3: *"Warm up volume gradually (don't 0→full)."* SVG ⑦: active node has a "warmup ramp." A fresh IP that suddenly emits high SMTP volume looks exactly like a spam source and gets blocked — destroying the clean IP Phase 3 just built.

- **What warmup IS in Phase 3 (`LOCKED` shape):** a **simple ascending daily cap** on real SMTP verifications routed to the new box — start low, increase over days if the box stays healthy (green audit, low block-rate). It is a **single ascending ceiling**, deliberately minimal.
- **What warmup is NOT (`SEAM →` P4):** it is **not** the P4 per-provider rate-limit + jitter machinery (build-plan §4, SVG §④ "per-provider rate+jitter"). Phase 3's warmup is one global ascending cap for one box; **per-provider pacing, jitter, and the distributed rate limiter are Phase 4.** Flagged so the two aren't conflated (the prompt: "design the ramp… flag exact volumes as dials" — and *don't* build the P4 limiter).
- **Volumes are `DIAL` (flagged as dials, per the prompt):** e.g. an illustrative ramp — **Day 1: ~50 · Day 2: ~100 · Day 3: ~250 · Day 4: ~500 · Week 2: ~1–2k/day**, gated on health at each step. **These numbers are placeholders/dials, not locked** — real warmup pace depends on the IP, provider, and observed block-rate. `DECISION REQUIRED` (OQ-P3-10): the concrete ramp schedule + the health checkpoint between steps. **Recommendation:** conservative start (~50/day), only step up when the prior day showed **low `unknown/smtp_blocked` rate** and green audit; pause/step-down on a block-rate spike.
- **Where the cap lives — and where its STATE lives (`LOCKED`):** a brain-side limit on `:smtp`-routed verifications for the node. Critically, the **warmup counter is durable state and MUST persist in Supabase** (a per-node, per-window counter row), **not** live only in brain memory — otherwise a brain restart resets the ramp and a fresh IP silently jumps back to full allowance mid-warmup (an IP-reputation hazard). This honors "the brain holds all state, in Supabase" (P1 §5.8): the *box* stays stateless, but the *warmup progress* is durable brain state, not ephemeral. **`DECISION REQUIRED` folded into OQ-P3-10:** the counter is a small additive table (e.g. `node_warmup(node_id, window_start, count, cap)`) or a reuse of `/v1/stats`-derived counts reconciled against the LOG — recommend a dedicated durable counter row (simplest, restart-safe). Either way, **persistent, not in-memory.**
- **`SEAM →` P4:** when the cap is exceeded, P3's minimal behavior is to **degrade the overflow to the cheap verdict** (`risky/plausible`, SMTP skipped, `smtp_skipped_reason:"warmup_cap"` — additive signal, §8), **never** to queue it (queue is P5) and **never** `invalid`. Overflow-handling sophistication is P4/P5.
- **`GAP-20`:** "don't 0→full" + "harden toward unknown" interact: during warmup, addresses **over the cap** get **no SMTP verdict** → they degrade to the cheap `risky/plausible`. That is honest (we didn't prove them), but it means **early-warmup verdict quality is intentionally lower** for overflow. Flagged so "the box is live" isn't read as "all traffic now gets SMTP-grade verdicts" — capacity ramps with the cap.

### 5.3 The mock→real config flip — verify it is genuinely one config value `LOCKED` (the P2 promise, now tested)
The prompt: *"The mock→real flip must be genuinely one config value (Phase 2's whole promise) — verify the Phase 2 design actually delivers that and flag it if not."* This is the acceptance heart of Phase 3.

- **What the flip actually is (audited against the P2 design):** switching a node from mock to real changes, per P2 §2 / §3.5#6:
  1. the **node descriptor** in the brain's config-list — `base_url` now points at the real box (P1 §5.3);
  2. the **box's engine config** — `smtp_port: 25` (was `smtp_mock`'s port) and `dns:` real/empty (was `dns_mock`'s port) — applied via **`POST /v1/config/reload`** (P2 §4.5, pull model) **without a redeploy**;
  3. the node's **eligibility** — `enabled=true`, and it must pass §4.4 (green audit + port25_open).
- **Is that "one config value"? — honest audit (the flag the prompt demands):**
  - **`GAP-21` (verification of the P2 promise):** "**one config value**" is a **slight overstatement** even under a correct P2 design. The *brain-code* change is **zero** (the config-flip goal genuinely holds — routing/parsing/scoring/logging are identical, verified by P2 §3.5#6). But the *operational* flip touches **a small set of config values across two locations**: the brain's node `base_url`/`enabled`, and the box's `smtp_port`/`dns` (via reload). It is **"one config change" in spirit (no code, no redeploy, reversible), but not literally one key.** **Recommendation:** treat "one config value" as the **design invariant "zero code change, config-only, reversible"** — which P2 *does* deliver — and document the flip as a **short ordered config checklist** (§5.3.1), not a single toggle. Flagged honestly rather than claiming a literal one-liner the design doesn't provide.
  - **Where it WOULD break the promise (must-not-happen):** if going live required **any brain code change** (new parsing, new scoring branch, a real-vs-mock conditional), the P2 config-flip design **failed** and it is a `GAP` to raise. Phase 3 acceptance (§11) explicitly tests **zero brain code diff** mock→real.

#### 5.3.1 The flip as an ordered checklist (config-only, reversible) `LOCKED` (ordering)
The order is safety-critical (audit **before** enabling traffic):
1. Deploy real box (§3.5); provision DNS/PTR/SPF (§3.3); prove port-25 egress → `port25_open=true` (§3.4).
2. Point the box config at real `smtp_port:25`/`dns`; `POST /v1/config/reload` → new `config_fingerprint` (P2 §4.5/§4.6). *(New fingerprint naturally invalidates any mock-era cache — P2 §4.5.)*
3. Run `/v1/audit` → require **green** (§4.4). Node `provisioning→active` only if green ∧ port25_open.
4. In the brain config-list: set the node's `base_url` to the real box, `enabled=true`.
5. Start the **warmup cap low** (§5.2).
6. Confirm real verdicts flow + log with `verifier_ip` (§5.4).
- **Reversibility (`LOCKED`):** every step is reversible by config (disable node / repoint base_url / reload mock config) with **no redeploy and no code change** — the disposability principle (SVG ⑦: kill + re-spin, no data lost). If the real box misbehaves, flipping `enabled=false` degrades to cheap verdicts (§4.4), losing no logged history.

### 5.4 Confirm real verdicts flow + `verifier_ip` tagged `LOCKED` (Door 2 activation)
Build plan §3.3: *"Confirm real verdicts flowing and logging with `verifier_ip` tagged."*
- **`verifier_ip` (P1 §3.2.2, reserved `inet`, "`null` in P1/cheap-only") is now populated** with the box's **real SMTP egress IP** for every verdict where the SMTP layer actually ran. This is the **first time `verifier_ip` is non-null** — Phase 3 activates a Door-2 field that has existed empty since P1.
  - **`LOCKED` semantics:** `verifier_ip` = the IP that **actually made the SMTP connection** (the egress IP == the PTR'd IP, per V5/§3.4), **not** the box's management/private address. If they differ (NAT egress, V5 fail), `verifier_ip` must be the **egress** IP or the field lies about reputation provenance. This is why V5 is a hard vetting item (§3.2).
  - **Where it comes from:** the box knows its own egress identity; **`DECISION REQUIRED` (OQ-P3-11):** does the brain populate `verifier_ip` from (a) the node registry's known egress IP for that `node_id`, or (b) a value the box reports (e.g., `/v1/audit`'s `current_host_ip`, or a field on `/v1/verify`)? **Recommendation:** **(a) the brain's registry** is authoritative (the brain owns state; the box stays dumb), **cross-checked** against `host_audit.current_host_ip` at audit time — if they diverge, that's a red-flag logged (it means egress≠audited IP, the V5 failure). Avoids trusting a box to self-report its own reputation-bearing identity. Flagged.
- **`meta.node_id` / `meta.engine_version` / `config_fingerprint`** (P1 §3.1.3 / §3.2) are likewise now **real** (real node, real gem version via `/version`, real engine config via `/v1/config`). All were reserved; P3 fills them.
- **Cache correctness across the flip:** because the cache key is `(email_hash, config_fingerprint)` (P1 §4.2) and the real box has a **different** `config_fingerprint` than the mock (different `smtp_port`/`dns`/verifier), **mock-era cached verdicts do not serve for real-box config** — the fingerprint change is a natural cache boundary (P2 §4.5). No manual flush; no stale mock verdict leaks into real operation.
- **Acceptance signal:** a real honest-corporate address returns a **real** SMTP verdict (`valid/accepted` via clean-250+probe-rejected, or `invalid/mailbox_not_found`, or a well-classified `unknown`), logged with non-null `verifier_ip`, non-null `node_id`, real `engine_version`, and the real `config_fingerprint`.

---

## 6. Network & security posture (senior-security — a review item)

The prompt flags this explicitly: *"The box is now internet-reachable-ish (it opens outbound 25, and the brain reaches it). Spec the network posture: box on private network / firewalled to brain only, never public. This is a security-review item."* Build plan §3.1: *"Put box on **private network**; brain-only access, token auth."*

### 6.1 The threat model in one line `LOCKED`
The box is now a **real host with a real IP, real outbound port 25, and an HTTP control surface (the wrapper)**. Two exposures appear that did not exist in P1/P2: **ingress** (someone reaching the wrapper endpoints) and **egress** (the box speaks SMTP to arbitrary internet hosts). The design goal: **the box's control plane is reachable only by the brain; the box holds nothing worth stealing; a compromised or IP-burned box is disposable (kill + re-spin, no data lost).** The last point is inherited (SVG ⑦, boxes are stateless) and is now a **security control**, not just an architecture note.

### 6.2 Ingress: brain-only, never public `LOCKED` — the core security rule
> **RULE (`LOCKED`, build-plan §3.1): the wrapper's HTTP surface (`/v1/verify`, `/v1/audit`, `/v1/config`, `/v1/config/reload`, `/v1/catchall`, `/v1/stats`) is reachable ONLY by the brain. It is NEVER exposed to the public internet.**

Defense in depth (all `LOCKED` as *requirements*; exact mechanism is a `DIAL`):
1. **Network isolation (primary):** the box sits on a **private network** with the brain; the wrapper binds to the **private interface only**, not `0.0.0.0` public. **`DECISION REQUIRED` (OQ-P3-12):** private-network mechanism — provider private VLAN vs a WireGuard/VPN mesh vs SSH tunnel. **Recommendation:** a **provider private network if the host offers one and both brain+box are same-provider/region; otherwise a WireGuard mesh** (works across providers, which matters because the box provider is chosen for port-25/PTR, not for co-location with the brain). Flagged — the answer depends on whether brain and box can share a provider private net.
2. **Firewall (host + provider):** default-deny inbound; allow the wrapper port **only from the brain's IP** (and SSH only from an admin allowlist / bastion). Public inbound to the wrapper port = **closed**.
3. **Token auth (secondary, defense-in-depth — not the primary control):** the node token (`Authorization: <node-token>`, P2 §4.0 / P1 OQ-13) is required on every endpoint. **`LOCKED` framing:** the token is the **second** layer — *network isolation is primary*; a token alone on a public endpoint is not acceptable (tokens leak). Both, always.
4. **TLS on the brain↔box hop `DECISION REQUIRED` (OQ-P3-13):** even on a private network, is the wrapper HTTP or HTTPS? **Recommendation:** **TLS (HTTPS or the WireGuard tunnel provides transport encryption)** — the node token and the (PII-adjacent) email travel this hop; even private-network traffic should be encrypted so a token/credential isn't sniffable if the private net is misconfigured or shared. If WireGuard is the isolation mechanism, its encryption satisfies this; otherwise HTTPS with an internal cert. Flagged.

### 6.3 Egress: outbound 25 is the ONLY new outbound need `LOCKED`
- The box needs **outbound TCP/25** to real mail servers (the whole point) and **outbound DNS** (for the engine's MX/PTR lookups). **Nothing else.**
- **`LOCKED` requirement:** egress firewall should **allow 25 + DNS (+ whatever the OS needs for updates/time)** and otherwise default-deny — a box that only needs to speak SMTP+DNS shouldn't be able to exfiltrate anywhere else if compromised.
- **No inbound port 25** on the box — it **originates** SMTP (verification), it does not **receive** mail. Inbound 25 closed (the box is not an MTA/mail server).

### 6.4 The box holds no secrets worth stealing `LOCKED` (disposability as security)
- **Boxes never touch the DB** (build-plan §0, P1 §4) — a compromised box **cannot read the LOG, the cache, the pepper, or any address history.** The pepper (P1 §3.3.3, the system's most fragile artifact) **never leaves the brain** and is **never** on a box. This is a **security dividend** of the stateless-box design: the blast radius of a box compromise is one box's config (verifier identity + its own node token), not the data moat.
- **What a box *does* hold:** its node token, its verifier identity (email/domain), and transient in-flight transcripts. **`DECISION REQUIRED` (OQ-P3-14):** on box compromise/IP-burn, the response is **kill + re-spin** (SVG ⑦) — but the **node token** it held must be **revoked/rotated** (it's in the brain's `ACCESS_TOKENS` allowlist). **Recommendation:** per-node unique tokens (never a shared token across boxes) so revoking one box's token doesn't disrupt others; rotation is a brain-side allowlist edit. Flagged — matters more in P4 (many boxes) but the per-node-token discipline must start now.

> **[CONSOLIDATION NOTE — 2026-07-17 · additive · two accepted operational gates on the control surface] `[Finding-2 / PN-1, accepted]` + `[PN-5, accepted]`**
> This is where the *box-facing* control surface is defined; assembled across P3–P7 the *operator-facing* control plane (node disable, global-fallback-kill, pause, shadow-mode promotion, budget change, breaker/brake confirm, privacy opt-out flip) is gated by **one Supabase service-role credential with no per-actor identity** (consistency report §0/§1 F-11), while the append-only LOG audits *verdicts*, not *control-plane changes*.
> - **`[Finding-2 / PN-1]` — accepted gate, before P3 go-live:** every manual control-plane action writes to an **append-only admin-audit log** with **per-actor identity** (who/what/when), distinct from the verdict LOG, reusing the `node_audits` / `work_item_attempts` append-only pattern. Recorded as [`DECISIONS.md`](../open-decisions.md) **DR-3**; gated in [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) §3.6; the global-fallback-kill flag (PN-16) rides this same audit trail ([`kill-switch-ladder.md`](kill-switch-ladder.md) §Global A).
> - **`[PN-5]` — accepted, node-token rotation runbook:** the OQ-P3-14 per-node-token discipline gains a lifecycle owner — rotation triggers `/config/reload`; revocation is **atomic with quarantine**; the router **excludes a revoked-token node immediately** (F-1). A runbook item, additive to the recommendation above.
> **Neither mechanism is built here — both gate later phases (P3 go-live / ongoing ops).**

### 6.5 Verifier identity isolation (reputation is a security-adjacent asset) `LOCKED`
- The **verifier domain is isolated** (§3.3, OQ-P3-2) so that burning it (getting it blocklisted through verification traffic) **cannot taint a real sending/corporate domain.** Treat the verifier domain's reputation as a **sacrificial, rotatable asset** — the domain equivalent of the disposable box.
- **`SEAM →` P4:** HELO/verifier **rotation** (SVG §④, deferred in P2 §6.7 as inert-against-mocks) becomes **real and relevant now** (a real IP talking to real providers benefits from rotation to resist fingerprinting). **But rotation machinery is P4** (per-provider pacing + rotation is the P4 reputation layer). **Phase 3 uses a single fixed verifier identity** and flags rotation as the first P4 reputation feature that a real box actually needs. `GAP-22`: this is the one SVG §④ item that was "inert against mocks" in P2 and becomes **live-relevant in P3 but is still deferred to P4** — flagged so it's a conscious deferral, not an oversight (the first box can run on a fixed identity; rotation matters once there's volume + a pool).

### 6.6 Egress-IP == audited-IP == `verifier_ip` (the three must be one) `LOCKED`
A subtle but critical security/correctness invariant, tying V5 (§3.2), `verifier_ip` (§5.4), and PTR (§4.2) together:
> **The IP that opens outbound SMTP (egress) MUST equal the IP that PTR resolves to MUST equal the `verifier_ip` logged.** If a host NATs egress through a different IP than the PTR'd/audited one, then: real MX see an un-PTR'd source (→ blocks → false `unknown`), the audit passes on the wrong IP (green but meaningless), and `verifier_ip` records an IP that didn't actually send. All three break silently together.

- This is why **V5 (egress-IP == public/PTR'd IP) is a hard vetting item** (§3.2) and why `verifier_ip` sourcing is cross-checked against `host_audit.current_host_ip` (§5.4, OQ-P3-11). Flagged as a single invariant so the three facts are never allowed to drift apart.

#### 6.6.1 The two-IP problem — audit must run against the PUBLIC egress IP `LOCKED` — GAP-24
Putting the box on a **private network for ingress** (§6.2) while it needs a **public IP for SMTP egress** (§6.3) means the box plausibly has **two addresses**: a **private** one (the brain reaches the wrapper) and a **public egress** one (talks outbound SMTP, is PTR'd, is the verifier-domain A-record target). This creates a concrete failure mode in the audit itself:
- **`host_audit` auto-detects `current_host_ip`** (Appendix A.1). If, on a multi-homed box, it detects the **private** IP, then: the DNS check compares the verifier A-record against the *private* IP (mismatch → red, OR — worse — a green against a meaningless private IP), and the PTR check runs against the wrong address. **The audit can go green on the private IP while the real public egress IP is entirely unaudited** — the most dangerous silent-pass in the phase.
- **`LOCKED` requirement (GAP-24):** the audit's `current_host_ip`, the verifier A-record target, the PTR'd IP, and the SMTP egress IP **must all be the box's PUBLIC egress IP.** The verifier domain's A-record points at the **public egress** IP (not the private ingress IP); PTR is set on the **public egress** IP; and the box must be configured so `host_audit` evaluates the **public egress** IP (e.g. verifier identity/host detection pinned to it), never the private interface.
- **Vetting consequence:** V5 (§3.2) is extended — vet not just "egress==PTR'd IP" but "**`host_audit.current_host_ip` == the public egress IP**" on a multi-homed box. The §5.4 cross-check (`verifier_ip` vs `current_host_ip`) is what catches a regression here at runtime. Flagged because the private-network security requirement (§6.2) and the public-egress SMTP requirement (§6.3) **pull in opposite directions on "what is this box's IP,"** and the audit sits exactly on that fault line.

### 6.7 Security checklist (go-live gate) `LOCKED` (the checklist)
Before `enabled=true` (§5.3.1 step 4), all must hold:
- [ ] Wrapper bound to private interface; **not** reachable from public internet (verified by an external port scan showing the wrapper port closed).
- [ ] Firewall: inbound wrapper port allowed **only** from brain IP; SSH allowlisted; all other inbound denied; inbound 25 closed.
- [ ] Egress: outbound 25 + DNS allowed; broad egress otherwise denied.
- [ ] Node token is **per-node unique**, stored in the brain's secrets manager (not in `nodes` clear — P1 OQ-13), presented over TLS/tunnel.
- [ ] Verifier domain isolated (not a real sending/corporate domain).
- [ ] Egress-IP == PTR'd-IP == will-be-`verifier_ip` (V5, §6.6) confirmed.
- [ ] Box confirmed to hold **no** DB credentials and **no** pepper (it must not — it never touches state).

---

## 7. Scoring & verdicts — unchanged from Phase 2 (what real SMTP changes, and what it doesn't) `LOCKED`

Phase 3 adds **no new scoring surface.** The entire signal→verdict map (P2 §7.2), the catch-all truth table (P2 §5.5), the 5xx classification (P2 §6.1), the `552→valid` rule (P2 §6.1.1, owner-decided), and the confidence bands (P2 §7.4) are **inherited verbatim.** This is the config-flip principle at the scoring layer: the brain scores a real transcript exactly as it scored a mock one.

**What real providers change (behavior, not code) — all already handled by the frozen map:**
- **More of the "your-fault" surface actually fires.** Real MX greylist, tarpit, rate-limit, and block in ways `smtp_mock` only approximated (P2 §3.5 honest-limit). The result is **more `unknown/smtp_blocked`, `unknown/greylisted`, `unknown/smtp_timeout`** in real traffic than in mocks. This is **expected and correct** — harden-toward-`unknown` (§0.4) absorbs it. Phase 3 does not add handling; it observes the real distribution the mock couldn't produce.
- **The catch-all `valid` path becomes honestly testable.** P2 flagged GAP-14: `smtp_mock` is accept-all-shaped, so `valid` (target-250 + probe-rejected) was only reachable with a **seeded** probe. A **real non-catch-all corporate server rejects arbitrary unknowns**, so Phase 3 is the **first time the `valid` path is exercised against a realistic peer** — with a **random** probe, as designed (P2 §5.3). `GAP-23`: this means the `valid` path's *realism* is first proven in P3, not P2 — a fact to verify at go-live (§11 acceptance), not a code change.
- **Provider "yes" is worthless, "no" is weak** (SVG ⑤) — already encoded by `:mx`-routing big providers (§5.1) and low-confidence on famous-provider 5xx (P2 §7.4). Unchanged.

**What Phase 3 must NOT do:** it must not add a `real-box` conditional to scoring, must not introduce a new `reason`/`status`, and must not change any confidence band by code (bands are recalibrated from **data** in Phase 7, not hand-tuned in P3). Any temptation to "handle real providers specially" in code is a **Door-4/config-flip violation** to flag, not implement. Real-provider trust calibration is **Phase 7** (per (reason × provider) from bounce outcomes).

---

## 8. Additive surface introduced in Phase 3 (Door 1 / Door 2 additivity ledger) `LOCKED`

Phase 3 is the **most additive-light** phase — it mostly *populates* reserved fields. This ledger exists (mirroring P2 §8) so the tiny additive surface is auditable and nothing sneaks in as a silent break.

**New `signals` keys (additive to the open `signals` bag — P1 §3.1.3, "tolerate unknown keys"):**

| New `signals` key | Type | Set when | Source § |
|---|---|---|---|
| `smtp_skipped_reason` | string enum (`no_eligible_node`\|`audit_red`\|`provider_mx_routed`\|`warmup_cap`) | the SMTP layer did **not** run and the verdict fell back to a cheap outcome | §4.4, §5.2 |

- That is the **only** new `signals` key Phase 3 needs. Everything else the box now emits (`smtp_code`, `catchall_conclusion`, `dns_divergence`, `engine_mx_hosts`, `smtp_safe_pass`, …) was **already added in P2 §8**; Phase 3 just produces them from real transcripts.

**Reserved fields now POPULATED (not added) — zero schema change:**

| Field | Reserved in | Phase 3 fills with |
|---|---|---|
| `verifications.verifier_ip` (`inet`) | P1 §3.2.2 (`null` in P1/cheap-only) | real SMTP **egress** IP (== PTR'd IP, §6.6) |
| `meta.node_id` / `meta.engine_version` / `meta.config_fingerprint` | P1 §3.1.3 | real node / real gem version / real engine config fingerprint |
| `nodes.audit_status` / `last_audit_at` / `port25_open` | P1 §4.5 (`SEAM → P3`) | real audit signal + egress-proof result |

**One `DECISION REQUIRED` new table (additive, not a frozen-schema change) — OQ-P3-5:**
- `node_audits(node_id, audited_at, audit_status, warnings jsonb, current_host_ip)` — append-only audit **history** (§4.3). New table; touches no frozen schema. Recommended because audit results are historical data (Door-2 spirit) and P4's health-watcher wants the flap history.

**Zero changes to the frozen surface (the additivity guarantee):**
- **`reason`/`status`/`recommendation` enums:** **zero new values** — real SMTP emits exactly the P2-reserved reasons.
- **`flags`:** no new flags.
- **`verifications` columns:** **zero added or altered** — P3 writes reserved columns only.
- **`/verify` response shape:** unchanged; only reserved `meta` fields get real values (+ the one additive `signals` key, tolerated by contract).

---

## 9. Contradictions & gaps found in the source material (flagged, not resolved)

Per the requirement to surface — not quietly fix — inconsistencies across the build plan, the SVG, the P1/P2 specs, and truemail reality. GAP-1..14 are inherited (P1/P2); **GAP-15..23 are new to Phase 3.**

| ID | Where | Gap | Handling in this spec |
|---|---|---|---|
| **GAP-15** (new, **the big one**) | build-plan §3.2 `/audit` gate vs `host_audit` reality | `host_audit` checks **only IP/DNS/PTR** (Appendix A.1) — it does **NOT** verify **outbound port 25 works** or **SPF is published**. A box can be **audit-green and unable to send SMTP at all**. | Egress + SPF proven **separately** (§3.4); eligibility = `green AND port25_open` (§4.4). Not resolvable by audit — a structural boundary, made explicit. |
| **GAP-16** (new) | build-plan §3.1 "cheap VPS" ∧ port-25 ∧ PTR ∧ clean-IP | Over-constrained set; "cheap ∧ all four" is a narrow/often-empty market. | Reframed as "cheapest that passes §3.2 vetting"; procurement trade-off (OQ-P3-1), not a design fix. §3.1. |
| **GAP-17** (new) | build-plan §3.1 "verifier domain + **SPF**" vs `host_audit` | SPF is required by the plan but **not checked by `host_audit`**. Risk: assuming audit-green covers SPF. | SPF is a **separate checklist item**, not an audit dimension; required-but-not-audit-gating (§3.3/§3.4, OQ-P3-3). |
| **GAP-18** (new) | SVG ⑦ "quarantine on **block/audit red**" vs build-plan §3.2 (P4 health-watcher) | SVG shows **block-rate** as a quarantine trigger, but block-rate monitoring is the **P4 watcher**. | P3 does **audit-driven** quarantine only; **block-rate-driven quarantine → P4**. §4.5. |
| **GAP-19** (new) | build-plan §3.3 provider routing vs P1 §6.9 brain routing | Provider `:mx` routing now lives in **two places** (brain skip-SMTP + engine `validation_type_for`) — they can drift/disagree. | Brain authoritative; engine map = synced defense-in-depth backstop (OQ-P3-8). §5.1. |
| **GAP-20** (new) | build-plan §3.3 warmup + §0 harden-toward-unknown | During warmup, over-cap addresses get **no SMTP verdict** → degrade to cheap `risky/plausible`. Early-warmup verdict quality is intentionally lower for overflow. | Explicit: capacity ramps with the cap; overflow degrades (never `invalid`, never queued in P3). §5.2. |
| **GAP-21** (new, **verifies the P2 promise**) | build-plan/P2 "switch = literally one config value" | The flip is **zero-code + config-only + reversible** (promise holds in spirit), but touches **a few config values across two locations**, not literally one key. | Reframed as the invariant "zero code change, config-only, reversible" + an ordered flip checklist (§5.3/§5.3.1). Honest flag, not a claim of a one-liner. |
| **GAP-22** (new) | SVG §④ "rotate HELO/verifier" — P2 §6.7 deferred (inert vs mocks) | Rotation becomes **live-relevant on a real IP** (P3) but its machinery is still **P4**. | P3 = single fixed verifier identity; rotation flagged as the first P4 reputation feature a real box needs. §6.5. |
| **GAP-23** (new) | P2 GAP-14 (`smtp_mock` accept-all) vs the `valid` path | The `valid` path's **realism** (random probe against a real non-catch-all server) is first exercisable in **P3**, not P2. | Verify at go-live with a random probe on a real corporate domain (§7, §11 acceptance). Not a code change. |
| **GAP-24** (new, from review) | §6.2 private-net ingress vs §6.3 public egress vs `host_audit` auto-detect | A box multi-homed (private ingress + public egress) can have `host_audit` detect the **wrong (private) IP** → audit green on an unaudited public egress IP. The security requirement and the SMTP requirement pull opposite directions on "what is this box's IP." | `LOCKED`: audit IP == A-record == PTR == egress == **public egress IP**; V5 extended to check `current_host_ip`==public egress; §5.4 cross-check catches runtime drift. §6.6.1. |
| GAP-1/3/4/10/14 (inherited, resolved in P2) | — | Actioned/resolved in P2. | No P3 action; listed for continuity. |
| GAP-2/8/11/12/13 (inherited) | — | GAP-13 (session reuse spike) **may re-surface in P3**: see note below. | GAP-13 note below. |

> **`GAP-13` re-surfacing check (the prompt asks explicitly):** GAP-13 (P2 §5.4 — can the wrapper hold a reusable session for the 2nd RCPT without patching the gem) is a **Phase-2 design question this doc does not re-solve.** **But it affects Phase 3's stakes:** on a **real** box, Option B (two separate SMTP sessions for target + probe) means **two connections to a real MX per catch-all check** — higher connection volume, more fingerprintable, higher reputation cost, and a real catch-all server *could* behave differently across two sessions (weaker signal). So if the P2 spike landed on Option B, **P3 pays a real reputation/accuracy cost that mocks hid.** **Handling:** flagged, **not re-solved here** (per the prompt: "note whether it affects Phase 3… but do not re-solve Phase 2"). If GAP-13 is still open at P3 go-live, the warmup ramp (§5.2) and per-connection cost should be watched more conservatively for catch-all-heavy traffic. Recorded as a cross-phase dependency, not a P3 decision.

> **No gap is silently resolved.** GAP-15 (audit ≠ port-25/SPF) is the sharpest and is made a hard part of the eligibility rule (§4.4). The others are surfaced with an explicit stance; the procurement gaps (16) and the P2 promise audit (21) are flagged honestly rather than papered over.

---

## 10. Open questions / decisions required (with recommendations)

Phase-3-specific. Numbered `OQ-P3-N` to avoid collision with P1 (OQ-1..25) and P2 (OQ-P2-1..11). **All 11 signed off by the owner (Appendix D):** recommendations accepted; **OQ-P3-1 reclassified as a pre-provisioning research task** (not answerable at design time — provider offerings change), and **OQ-P3-9 explicitly affirmed** (O365 → `:mx`, no hunch-based special-casing). The "Decision" column below is binding for build.

| OQ | Topic | Decision (signed off) | Touches |
|---|---|---|---|
| **OQ-P3-1** | Host selection given the narrow port-25∧PTR∧clean∧cheap market (GAP-16) | **RECLASSIFIED → pre-provisioning research task, not a design decision.** The real execution risk; unanswerable from the spec because provider offerings change. Before Phase 3 go-live, run a "current email-friendly VPS providers allowing port 25 + settable PTR" search; then apply §3.2 vetting on a refundable trial box; keep a documented fallback. **The spec's correctness does not make the box easy to get — this is the wall, not the design.** | §3.1 |
| **OQ-P3-2** | Verifier domain: dedicated vs subdomain of existing | **Dedicated domain / isolated subdomain** — verification reputation isolated from any real sending/corporate domain (sacrificial, like the box). | §3.3, §6.5 |
| **OQ-P3-3** | SPF required if the verifier never sends `DATA`? | **Publish it anyway** (build-plan requires it, cheap, some MX weigh envelope-sender at RCPT); **required-but-not-audit-gating** (GAP-17). | §3.3 |
| **OQ-P3-4** | Deploy topology (bare container vs orchestrated) | **Single container on the VPS**; orchestration is a P4 pool concern; box is disposable. | §3.5 |
| **OQ-P3-5** | Where raw audit `warnings` are stored | **New append-only `node_audits` history table** (audit results are historical data; P4 wants flap history) — additive, touches no frozen schema. | §4.3, §8 |
| **OQ-P3-6** | Verdict when **no** `:smtp`-eligible node exists | **Degrade to cheap-layer verdict** (`risky/plausible` / `provider_no_smtp`), SMTP skipped, `smtp_skipped_reason` signal, `verifier_ip=null` — **never `invalid`**. | §4.4 |
| **OQ-P3-7** | Auto-quarantine: first red vs N-consecutive-red | **Quarantine on first red** (fail-safe; IP damage is the downside); **require N-consecutive-green to auto-recover** (debounce flapping). Asymmetric. | §4.5 |
| **OQ-P3-8** | Which layer owns provider routing (brain vs engine `validation_type_for`) (GAP-19) | **Brain authoritative**; engine `validation_type_for` = synced defense-in-depth backstop; one source → both consumers. | §5.1 |
| **OQ-P3-9** | O365 → `:mx` or `:smtp` for the first box (SVG ⑤ nuance) | **AFFIRMED: `:mx` for all three (Gmail/O365/Yahoo).** Owner ruling: O365 historically leaks more real invalid signal, but *"historically is doing a lot of work"* and it has tightened over time. **Prove the honest-corporate path first; do NOT special-case O365 on a hunch.** Revisit promoting O365 to `:smtp` only when **P7 bounce data** says its `550`s are trustworthy — not before. | §5.1 |
| **OQ-P3-10** | Concrete warmup ramp schedule + health checkpoints | Conservative start (~50/day); step up **only** on prior-day low `smtp_blocked` rate + green audit; pause/step-down on block spike. Volumes are **dials**. | §5.2 |
| **OQ-P3-11** | `verifier_ip` sourcing (brain registry vs box self-report) | **Brain registry authoritative**, cross-checked against `host_audit.current_host_ip`; divergence = logged red flag (egress≠audited IP, the V5 failure). | §5.4, §6.6 |
| **OQ-P3-12** | Private-network mechanism (provider VLAN vs WireGuard vs tunnel) | **Provider private net if brain+box co-located; else WireGuard mesh** (box provider is chosen for port-25/PTR, not co-location). | §6.2 |
| **OQ-P3-13** | TLS on the brain↔box hop even on private net? | **Yes** — token + PII-adjacent email travel it; WireGuard encryption satisfies it, else internal-cert HTTPS. | §6.2 |
| **OQ-P3-14** | Node-token strategy on compromise/re-spin | **Per-node unique tokens** (never shared); revoke/rotate the burned box's token via brain allowlist; discipline starts now for P4. | §6.4 |

---

## 11. Phase seams (where Phase 3 deliberately stops)

| Later phase | Seam left in / honored by Phase 3 | Not built now |
|---|---|---|
| **P4** resilience & pool | Audit **signal + gate + single-box quarantine** built; `node_audits` history table feeds the watcher; `/v1/stats` shape (P2) emitted; `Strategy` interface unchanged; per-node-token discipline started; warmup = one ascending cap. | The **continuous health-watcher** (polls `/audit`+`/health`+**block-rate**), **block-rate-driven** quarantine (GAP-18), the **pool** (2nd box, `+1 config line`), sticky/least-inflight strategies, **per-provider rate+jitter**, **HELO/verifier rotation** (GAP-22), the repeatable provisioning **script**. |
| **P5** queue | Greylist → `unknown` + requeue metadata already present (P2 §6.5); real greylisting now actually occurs (real MX), marked retry-eligible but **not retried**. | The queue, backoff/retry loop, real greylist retry-then-accept round-trip, group-by-domain, bulk job handle. |
| **P6** fallback | Real catch-all / big-provider residue is cleanly `risky` (the exact residue P6 routes out); `reason=external_verified` still reserved. | Fallback API integration; routing the residue to it. |
| **P7** flywheel | Real SMTP verdicts logged with **real `verifier_ip`**, full transcripts, parsed signals — the **real** join-gold the flywheel needs; confidence bands still flagged as placeholders; per-provider trust (SVG ⑤, O365 nuance OQ-P3-9) explicitly deferred to here. | Bounce classification, per-(reason×provider) recalibration, provider auto-promotion to `:mx`/`:smtp` (incl. O365 promotion). |
| **Config-flip → N boxes** (P4 topology, SVG ⑧) | The mock→real flip proven **config-only + reversible** (§5.3) for **one** box; `nodes`/config-list already list-shaped (P1 Door 5). | "Add VPS = +1 config line" at scale, the 1-VPS-N-IPs vs N-VPS-1-IP topology choice (SVG ⑧), live migration between topologies. |

---

## 12. Phase 3 "done" — restated with acceptance criteria

**Build-plan definition:** *one audited box returns real SMTP verdicts on honest domains, and big providers are safely routed to `:mx`.*

Acceptance criteria for this spec's design (all verifiable on **one real box** — the first time a real IP is involved):

1. **Provisioning vetting (§3) passed on a real box:** outbound port 25 proven (egress smoke test → `port25_open=true`), settable PTR confirmed, clean IP confirmed at provision, verifier domain + A-record + SPF published, egress-IP == PTR'd-IP (V5). Host chosen from the narrow port-25∧PTR market via pre-purchase vetting (§3.2).
2. **`/audit` is green** (`host_audit warnings == {}`) and the **gate is enforced**: the node is `:smtp`-eligible **iff `audit_status==green AND port25_open`** (§4.4); a forced-red audit (break PTR) **removes** it from `:smtp` candidacy and it degrades to cheap verdicts — **never `invalid`** (§4.4, OQ-P3-6).
3. **Scheduled re-audit + auto-quarantine** work as a state machine (§4.5): a red re-audit flips `nodes.status→quarantined` and stops `:smtp` routing; a clean re-audit recovers it (SVG ⑦). Audit history is written to `node_audits`. **Block-rate-driven** quarantine is confirmed **absent** (P4, GAP-18).
4. **Selective routing (§5.1):** honest corporate domains → `:smtp` (real verdicts); **Gmail/O365/Yahoo → `:mx`** (never SMTP'd from our IP), enforced brain-side with the engine `validation_type_for` as a synced backstop (OQ-P3-8). No SMTP `valid` is ever minted for a `:mx`-routed provider.
5. **Warmup ramp (§5.2):** real SMTP volume to the new box follows an **ascending cap** (dials, OQ-P3-10), not 0→full; over-cap traffic degrades to cheap verdicts (GAP-20), never queued (P5) or `invalid`. Confirmed this is **not** the P4 per-provider rate limiter.
6. **The mock→real flip is config-only + reversible (§5.3):** going live required **zero brain code change** (the P2 config-flip promise verified — GAP-21); the flip is an ordered config checklist (§5.3.1), fully reversible by `enabled=false`/repoint/reload with no redeploy.
7. **Real verdicts flow + log with `verifier_ip` (§5.4):** a real honest-corporate address yields a real SMTP verdict logged with **non-null `verifier_ip`** (= egress == PTR'd IP, §6.6), real `node_id`, real `engine_version`, real `config_fingerprint`. The **`valid` path is exercised with a random probe against a real non-catch-all server** (GAP-23) — its first realistic test.
8. **Security posture (§6) holds:** wrapper is **private/brain-only** (public port scan shows it closed), firewalled (inbound only from brain; inbound 25 closed; egress 25+DNS only), per-node token over TLS/tunnel, verifier domain isolated, box holds **no** DB creds / no pepper. The §6.7 checklist passed before `enabled=true`.
9. **Additivity (§8):** the entire Phase-3 surface is additive/populate-only — **one** new `signals` key (`smtp_skipped_reason`), reserved columns filled, **one** new additive table (`node_audits`), and **zero** new `status`/`reason`/`flags`/`verifications`-column/response-shape changes. A diff against the frozen Door-1/Door-4 contracts shows no removal, rename, or type change.
10. **Door 5 intact:** the deployed rack+wrapper is the **P2 fork unchanged**; the gem is pinned and CI-asserted-unmodified; **all** new Phase-3 behavior is infrastructure (host/DNS/PTR/firewall) or a small brain-side candidacy filter — **nothing** in the gem changed.
11. **GAP-13 impact noted, not re-solved (§9):** whether the catch-all probe is same-session (Option A) or two-session (Option B) is a P2 question; its **real-box reputation cost** is flagged and watched during warmup, not re-decided here.
12. **Every OQ-P3 (§10) is signed off or explicitly deferred with an owner** — none silently assumed.

---

## Appendix A — Upstream ground truth (captured, anchors the audit + routing contracts)

> Captured from the `truemail` gem (`host_audit`) and `truemail-rack` READMEs so the Phase-3 audit/routing design is anchored in what the tools *actually* do. Reference, not our contract. Extends the P1/P2 Appendix A with the Phase-3-relevant detail.

### A.1 `Truemail.host_audit` — the audit primitive (anchor for §4)
- **Call:** `Truemail.host_audit` → `Truemail::Auditor::Result`, serialized as:
  ```jsonc
  { "current_host_ip": "127.0.0.1", "warnings": {} }            // green
  // or
  { "current_host_ip": "127.0.0.1",
    "warnings": { "dns": "A-record of verifier domain not refers to current host ip address",
                  "ptr": "PTR-record does not reference to current verifier domain" } }
  ```
- **The three (and only three) audit dimensions:**
  1. **IP** — determines the host's current public IP (`current_host_ip`); a failure to detect it is itself a warning.
  2. **DNS** — checks the **verifier domain's A-record refers to the current host IP** (forward alignment). Warning key `dns` if not.
  3. **PTR** — checks **reverse DNS (PTR) for the host IP references the verifier domain** (reverse alignment). Warning key `ptr` if not.
- **`warnings == {}` ⇒ green; any warning ⇒ red.** **`host_audit` does NOT check outbound port 25, SPF, DKIM, DMARC, or blocklist status** — this is the ground-truth basis of **GAP-15/GAP-17**. It is a **DNS/PTR alignment audit**, nothing more. We do not invent additional audit fields (§4.1).
- **Config dependency:** `host_audit` needs `verifier_domain` (and derives/detects `current_host_ip`); the verifier domain is the identity both the A-record and PTR are checked against.

### A.2 `truemail-rack` / wrapper surface relevant to P3 (from P2 §4, unchanged)
- `GET /v1/audit` (P2 §4.2) wraps `host_audit`; returns the A.1 payload + wrapper-added `audit_status: green|red` (derived from `warnings == {}`) + `audited_at`. **Phase 3 adds nothing to this contract** — it runs it against real DNS/PTR and **enforces** the gate.
- `GET /v1/config` (P2 §4.6) exposes full effective config incl. `verifier_email`, `verifier_domain`, `default_validation_type`, `validation_type_for`, `smtp_port`, `dns` — the inputs to the real `config_fingerprint` and the routing map (§5.1).
- `POST /v1/config/reload` (P2 §4.5, **pull** model — OQ-P2-2) — the mechanism of the mock→real flip (§5.3).
- **`validation_type_for`** (gem config, Appendix P1 A.1 / P2 A.1): per-domain validation-level override (e.g. `{'gmail.com' => 'mx'}`) — the engine-side of the routing map (§5.1), a **backstop** to the brain's authoritative provider routing.

### A.3 Reputation/DNS facts Phase 3 depends on (industry ground truth, not truemail-specific)
- **PTR/rDNS:** real MX widely reject or penalize SMTP from IPs with missing/mismatched reverse DNS — the basis for the hard PTR gate (§4.2).
- **Port 25 egress:** most mainstream clouds block outbound 25 by default; email-friendly VPS/bare-metal hosts are the practical source — the basis for GAP-16 / §3.1.
- **SPF:** a TXT record authorizing sending IPs for a domain; checked by receivers at/after MAIL FROM. Not checked by `host_audit` (GAP-17).
- **Warmup:** sudden high volume from a fresh IP triggers spam heuristics; gradual ramp is standard practice — the basis for §5.2.

---

## Appendix B — Phase-3 go-live proof matrix (the acceptance intent, not test code)

The minimum scenarios Phase 3 must demonstrate on **one real box**. Unlike P2 (socket-level mocks), these require the real IP/DNS/PTR — that is the phase. (Design intent; no code.)

| # | Scenario (real box) | Expected outcome | Proves |
|---|---|---|---|
| 1 | Egress smoke test: outbound 25 to a known external MX | banner received → `port25_open=true` | port-25 works (separate from audit — GAP-15) |
| 2 | `/v1/audit` with PTR/DNS/SPF correctly set | `warnings=={}` → `green`; node `provisioning→active` | audit gate green path (§4.1/§4.5) |
| 3 | Break PTR (misalign rDNS), re-audit | `audit_status=red` → node **quarantined**, removed from `:smtp` candidacy | the hard gate + auto-quarantine (§4.4/§4.5) |
| 4 | Address routed while node red / no eligible node | cheap verdict (`risky/plausible`), `smtp_skipped_reason` set, **not `invalid`**, `verifier_ip=null` | graceful degrade, never invalid (OQ-P3-6) |
| 5 | Restore PTR, re-audit clean (× N greens) | node `quarantined→active` | auto-recovery debounce (§4.5, OQ-P3-7) |
| 6 | Honest corporate real address, real 250 + **random** probe rejected | `valid/accepted`, high conf, `verifier_ip` = egress IP | the **`valid` path realistically exercised** (GAP-23); Door-2 `verifier_ip` activated |
| 7 | Real address, `550` no-such-user | `invalid/mailbox_not_found` | real address-fault (§6.1 inherited) |
| 8 | Real corporate catch-all domain, 250 + probe 250 | `risky/catch_all`, writes `catchall_domains` | real catch-all detection |
| 9 | Real MX greylists (`4xx`) | `unknown/greylisted` + `retry_eligible`, **not retried** | real greylist → unknown (P5 seam) |
| 10 | Gmail/O365/Yahoo address | `risky/provider_no_smtp`, **SMTP never called**, `smtp_skipped_reason:"provider_mx_routed"` | selective routing (§5.1); provider "yes" never trusted |
| 11 | Warmup cap exceeded | over-cap → cheap verdict, `smtp_skipped_reason:"warmup_cap"` | ramp is a cap, degrades not queues (§5.2) |
| 12 | Config flip mock→real | **zero brain code diff**; config-only, reversible | the P2 config-flip promise (GAP-21, §5.3) |
| 13 | External port scan of the box | wrapper port **closed** to public; only brain reaches it | security posture (§6.2/§6.7) |
| 14 | Egress-IP vs PTR'd-IP vs `verifier_ip` vs `host_audit.current_host_ip` | all **four** identical (= public egress IP) | the §6.6 / §6.6.1 invariant (V5 + GAP-24) |

---

## Appendix C — Review log (what the adversarial critique pass changed)

This spec was drafted, then critiqued by an adversarial **architecture + senior-devops + senior-security** pass against the frozen P1/P2 contracts and truemail/infra reality, before finalizing (mirroring P1 Appendix B and P2 Appendix C). The pass was run **inline** (a prior background reviewer stalled without producing findings; inline was faster and effective — a deliberate process choice consistent with the owner's "don't run long sessions, split the work" guidance). Findings folded in:

**Blockers (all resolved):**
- **PB1 — Provisional (capacity-degraded) verdicts must not be cached.** The draft added `smtp_skipped_reason` for "no eligible node / audit red / warmup cap" fallbacks but didn't say whether those cheap verdicts are cacheable. Caching them would **freeze a lower-quality answer and suppress the real SMTP verdict** once the node recovers / the cap lifts — the exact failure mode P2 OQ-P2-6 warned against. **Resolved:** §4.4 `LOCKED` cache rule — capacity/eligibility-gap fallbacks (`no_eligible_node`/`audit_red`/`warmup_cap`) are **provisional → non-cacheable**; only `provider_mx_routed` (a *settled* routing outcome) stays cacheable. Discriminator: *why* SMTP was skipped.

**Majors (all resolved):**
- **PM1 — GAP-19 "keep the two routing tables in sync" was hand-waved.** Asserting "one source of truth" without a mechanism is how drift bugs ship. **Resolved:** §5.1 concrete mechanism — the brain policy's `provider_routing` map (P1 §7.2) is the **one** editable source; the box's `validation_type_for` is a **generated render** pushed at reload; `/v1/config` lets the brain **assert equality** (mismatch = logged drift alarm). One source, one generated copy, one check.
- **PM2 — The two-IP / audit-detects-wrong-IP failure (GAP-24).** The private-network security requirement (§6.2) and the public-egress SMTP requirement (§6.3) mean the box is multi-homed; `host_audit` auto-detects `current_host_ip` and can pass **green on the private IP while the public egress IP is unaudited** — a silent, dangerous pass sitting exactly where two requirements conflict. **Resolved:** new §6.6.1 + GAP-24 — audit IP == A-record == PTR == egress == **public egress IP**, `LOCKED`; V5 extended; the §5.4 four-way cross-check catches runtime drift.
- **PM3 — Auto-quarantine actor vs the "health-watcher is P4" boundary.** The draft risked reading as if P3 builds a polling daemon (which the prompt reserves for P4). **Resolved:** §4.5 states P3 builds only the **signal + gate + state transition + a minimal scheduled re-audit**; the *continuous* watcher (and block-rate-driven quarantine, GAP-18) is explicitly P4. The distinction is now sharp.
- **PM4 — Warmup counter state location unspecified.** "Brain holds all state" without saying *where* invites an in-memory counter that resets on restart → a fresh IP silently jumps to full allowance mid-warmup (reputation hazard). **Resolved:** §5.2 `LOCKED` — the warmup counter is **durable state in Supabase** (per-node/per-window row), not brain memory; box stays stateless, warmup progress is durable brain state.

**Minors folded in:** `smtp_skipped_reason` enum extended to include `warmup_cap` consistently (§4.4/§5.2/§8); Appendix B row 14 extended to the **four-way** IP identity check (adding `host_audit.current_host_ip`); GAP-24 registered in the §9 table and Appendix B.

**What the review affirmed as already-right (preserved):** the eligibility rule `green AND port25_open` cleanly resolving GAP-15 (audit necessary-but-not-sufficient); harden-toward-`unknown` on all your-fault real-provider outcomes (never `invalid`); the graceful-degrade-never-`invalid` rule when no node is eligible (OQ-P3-6); zero new `reason`/`status` values (Door 4 intact); the deployed fork unchanged + gem pinned (Door 5, the least-risky-software phase); verifier-domain isolation as a sacrificial reputation asset; boxes-hold-no-secrets as a security dividend of stateless design; the config-flip audited **honestly** as "zero-code/config-only/reversible" rather than overclaimed as a literal one-liner (GAP-21); scope discipline (health-watcher/pool/queue/rotation all named as seams, not built).

**Post-review status:** all PB/PM findings (PB1, PM1–4) resolved in-line. **Eleven OQ-P3 items (§10) remain for owner sign-off** — none blocking the design, each carrying a recommendation. The two items most worth the owner's attention: **OQ-P3-1** (the narrow port-25∧PTR host market — the phase's real execution risk) and **the GAP-15/GAP-24 IP invariants** (audit-green ≠ can-send; audit must run on the public egress IP) — both flagged, neither assumed.

---

## Appendix D — Owner decision log (Phase 3 sign-off)

All 11 **OQ-P3** questions signed off by the owner. **Recommendations accepted**; **OQ-P3-1 reclassified** as a pre-provisioning research task; **OQ-P3-9, OQ-P3-5, OQ-P3-6 explicitly affirmed with reasoning**; a **standing directive** added on the GAP-13 spike. Recorded here so the reasoning survives the conversation, mirroring P1 Appendix C and P2 Appendix D.

### Reclassified — OQ-P3-1: host selection is a research task, not a design decision
- **Spec had said:** shortlist email-friendly providers, vet on a trial box (§3.1).
- **Owner ruling:** correct, but this is **the real execution risk and cannot be answered from the spec** — provider offerings change over time. It is a **"search current email-friendly VPS providers that allow port 25 + PTR" task to run before committing to Phase 3**, not a design question. **"Don't let the spec's correctness fool you into thinking the box is easy to get."** The narrow market (GAP-16) is **the wall, not the design.**
- **Applied:** §10 OQ-P3-1 reclassified; header notes it; this is the one open item requiring real-world research before go-live.

### Affirmed with reasoning — OQ-P3-9: O365 → `:mx`, no hunch-based special-casing
- **Owner ruling:** consistent with the Phase-2 note — **start O365 at `:mx` like Gmail/Yahoo.** O365 historically leaks more real invalid signal, **but "historically" is doing a lot of work and it has tightened over time.** **Prove the honest-corporate path first; revisit O365 only when Phase-7 data says its `550`s are trustworthy. Don't special-case it on a hunch.**
- **Applied:** §10 OQ-P3-9 records the reasoning; §5.1 O365 nuance deferred to P7 calibration.

### Affirmed — OQ-P3-5: approve the `node_audits` append-only history table
- **Owner ruling:** approve. **Audit flap history is exactly what Phase 4's health-watcher will want, and a mutable column would overwrite it.** Additive, correct.
- **Applied:** §4.3 / §8 — `node_audits(node_id, audited_at, audit_status, warnings jsonb, current_host_ip)`, append-only, touches no frozen schema.

### Affirmed — OQ-P3-6: no eligible node → cheap verdict, never `invalid`
- **Owner ruling:** approve. Falls back to the cheap verdict, **never `invalid`**, `verifier_ip=null`, SMTP-skipped signal set. **This is harden-toward-`unknown` applied correctly to your own capacity gap.**
- **Applied:** §4.4 + the PB1 non-cacheable rule (a capacity-gap fallback is provisional, not settled).

### Accepted as-is (batch): 
OQ-P3-2, 3, 4, 7, 8, 10, 11, 12, 13, 14 — recommendations sound, accepted without change. (OQ-P3-3 SPF-anyway, OQ-P3-7 first-red-quarantine/N-green-recover, OQ-P3-8 brain-authoritative-routing all stand.)

### Standing directive — run the GAP-13 spike BEFORE Phase 3 go-live (not during)
- **Owner ruling:** the catch-all session spike (P2 GAP-13 / OQ-P2-11) **now has real teeth.** On a real box, Option B (two sessions per catch-all check) = **two real connections = double the reputation cost per check.** So the spike is **no longer just an elegance question — on a real IP it is a reputation-cost question.** **Run it before you provision, not during go-live.** Door 5 still governs: if the gem can't expose a reusable session cleanly, accept Option B as a documented weaker/costlier signal — **never patch the gem.**
- **Applied:** §9 GAP-13 note already frames the real-box cost; this appendix elevates it to a **pre-provisioning gate**. See [[truemail-never-fork-engine-door5]] and the new [[truemail-gap13-spike-before-real-box]].

### Affirmed as correct (owner named these as the spec's strongest points, preserved verbatim):
- **GAP-15 (audit-green ≠ ready)** — `host_audit` proves nothing about outbound 25 or SPF; a box can pass audit and be unable to send a packet. *"The difference between a spec written from the README and one written from reality."* Fix (eligibility = `green AND port25_open`, port 25 proven by a separate egress smoke test) is right.
- **GAP-24 (the multi-homed trap)** — *"the sharpest catch in all three specs."* The private-network security requirement and the public-egress function requirement make the box multi-homed; `host_audit` auto-detects `current_host_ip` and can go **green on the private IP while the real egress IP is unaudited and unclean** — a silent green sitting exactly where two requirements collide. Fix (lock audit IP == A-record == PTR == egress == public egress IP, four-way runtime cross-check) closes it.
- **GAP-16 (narrow market as the real blocker)** — the honest answer to "is provisioning a formality": **no, it's the risk.** Made host selection (OQ-P3-1) the top open question.
- **PB1 (capacity-degraded fallbacks are non-cacheable)** — freezing a bad answer and suppressing the real one when the node recovers; clean discriminator: **cache settled routing outcomes, never capacity gaps.**
- **PM1 (routing-table sync mechanism)** — one editable source, one generated copy, one equality check — **not hand-waving.**

### Cross-references to standing constraints (already owner-ruled in P1/P2, honored here, not re-opened):
- **Door 5 / never-fork-the-gem** ([[truemail-never-fork-engine-door5]]) — P3 deploys the P2 fork unchanged; GAP-13's real-box cost is flagged and now gated pre-provisioning, but no gem patch is contemplated.
- **`552 mailbox_full → valid`** ([[truemail-552-mailbox-full-valid]]) — inherited from P2 §6.1.1; P3 scoring is unchanged (§7), so this ruling carries through untouched.

---

*End of Phase 3 (First real box) Design Specification — **v1.0, APPROVED**. One adversarial review pass folded in (Appendix C: PB1, PM1–4 resolved); all 11 OQ-P3 decisions signed off (Appendix D) — OQ-P3-1 reclassified as pre-provisioning research, OQ-P3-9 affirmed (`:mx` for O365, no hunch-based special-casing). Additive-only against the frozen P1/P2 contracts (one new `signals` key, one new additive `node_audits` table, reserved columns populated). The `/audit` gate (`green AND port25_open`, §4.4) is the phase's spine; the narrow port-25∧PTR host market (§3.1, OQ-P3-1) and the audit≠can-send boundary (GAP-15/GAP-24) are the real risks. **Standing directive: run the GAP-13 catch-all-session spike before provisioning — on a real IP it is a reputation-cost decision, not an elegance one.** Ready to build.*
