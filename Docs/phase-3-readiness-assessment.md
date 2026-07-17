# Phase 3 readiness assessment — the honest one

**Date:** 2026-07-17 · **By:** P2→P3 readiness pass · **Verdict up front: Phase 3 is NOT ready to start —
and the blocker is not the design, it is a host that does not exist yet.**
**Authorities:** [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) §3.1/§3.2/§3.6, [`phase-3-first-real-box-spec.md`](phase-3-first-real-box-spec.md) §3 (host market) / §4 (audit gate) / §5.2 (warmup), [`DECISIONS.md`](DECISIONS.md) DR-3, [`truemail-ground-truth.md`](truemail-ground-truth.md) §3.3 (host_audit), [ADR-0007](adr/ADR-0007-gap13-catchall-session-two-session.md).

Phase 3 changes **no brain logic** — P2 proved the SMTP path is config-flip-ready. Phase 3 is
**provisioning + an audit gate + a selective go-live against a real IP.** Its readiness is therefore
gated almost entirely by things *outside the code*. Three gates decide it.

---

## Gate 1 — Host-selection research (port 25 ∧ settable PTR ∧ clean IP) — 🔴 **NOT READY · needs owner action · THE WALL**

**Status: the single largest execution risk of Phase 3, and it is unmet.** This is a procurement/research
gate, not a design gate (OQ-P3-1 was explicitly reclassified to "pre-provisioning research"). The design
is done; **the host is not acquired, not vetted, and may not exist at an acceptable price.**

The requirements are conjunctive and each is a real filter (P3 §3.2, V1–V5):
- **V1 — outbound TCP/25 genuinely open** (HARD). Most mainstream clouds (AWS, GCP, Azure, Oracle, newer
  DigitalOcean) block egress 25 by default; some never unblock, some deny the request for low-history
  accounts. **Must be proven from a trial box by actually opening a connection to a real external MX on
  25 — not by the provider's claim.** This is the **port-25 egress proof, separate from `/audit`**
  (GAP-15): a box can be **audit-green and still unable to send a single SMTP packet.**
- **V2 — settable custom PTR/rDNS** (HARD). Often unavailable on the cheapest tiers or requires a
  dedicated/BYOIP address (a different price bracket).
- **V3 — clean IP** (HARD at provision). Check Spamhaus et al. + passive history *before* warmup; a
  pre-poisoned IP starts the phase already losing.
- **V4 — dedicated/stable IP** and **V5 — egress IP == the PTR'd IP** (HARD). A NAT'd egress through a
  different IP silently breaks both reputation and `verifier_ip` accuracy.

**What "ready" requires (owner action):** shortlist 2–3 email-friendly providers; take a **trial/refundable**
box; **prove V1–V5 on the trial before committing** (V1/V2/V3/V5 are behaviorally checkable, V6–V8 ToS/
uptime/region are procurement judgment); keep a documented fallback provider (a host that works today can
block 25 tomorrow); **do not pay a year up front on an unproven host.** Also decide the verifier domain
(OQ-P3-2: dedicated domain/subdomain, SPF-isolated) and the private-network mechanism (OQ-P3-12).

**Do not treat getting the box as a formality.** Until a host passes V1–V5 on a trial instance, Phase 3
cannot begin its go-live — this is the blocker, full stop.

---

## Gate 2 — GAP-13 reputation cost (now known) — 🟡 **READY as an input · factor into warmup sizing**

**Status: resolved and quantified — not a blocker, but a number P3's warmup design must carry.** The
GAP-13 spike ran in P2 ([ADR-0007](adr/ADR-0007-gap13-catchall-session-two-session.md)): the pinned gem
exposes no reusable same-session, so the catch-all probe is **Option B — two SMTP sessions (two
connections) per catch-all check**, gem never patched.

**The reputation-cost consequence for Phase 3 (this is the factoring the owner asked for):**
- On a **real IP**, a catch-all-eligible verification costs **~2 outbound connections, not 1** (target
  RCPT + probe RCPT, in separate sessions). Cheap-gated and `:mx`-routed addresses cost **0** (no SMTP);
  a plain SMTP-eligible address with the catch-all probe on costs **2**.
- **Warmup sizing (P3 §5.2, the minimal ascending cap) must budget the connection count, not the
  verification count** — a daily cap of *N verifications* can be up to *~2N connections* to real MX. Size
  the ramp against connections so the warmup doesn't silently send ~2× the intended volume and trip a
  provider's rate/reputation heuristics early.
- **Reversibility note:** Option B is a wrapper-side choice, revisitable on real-IP evidence (still never
  a gem patch). If 2× connections proves reputation-costly on the real box, a wrapper-owned same-session
  client is a *separate* future decision — but the verdict contract is identical either way.
- **Safe direction preserved:** Option B can only *miss* a `valid` (→ `risky`), never mint a false
  `valid` (§5.5) — so the reputation cost buys correctness that hardens toward unknown.

**No owner action needed to *start* P3 on this** — it's a known input. The owner action is only to **set
the warmup ramp numbers with the 2×-connection factor in mind** when go-live is designed.

---

## Gate 3 — Admin identity + control-plane audit log — 🔴 **NOT READY · needs owner action · gates P3 GO-LIVE**

**Status: an accepted phase-gate (DR-3 / Finding 2 / PN-1), required before P3 go-live, not yet built.**
Today the entire control surface is gated by **one Supabase service-role credential with no per-actor
identity and no audit of which human changed what** — while the celebrated append-only LOG audits
*verdicts*, not *control-plane mutations*.

Before P3 go-live, **every manual control-plane action** — node disable, global-fallback-kill, pause,
shadow-mode promotion, budget change, breaker/brake confirm, privacy opt-out flip — **must write to an
append-only admin-audit log with per-actor identity** (who/what/when), distinct from the verdict LOG,
reusing the `node_audits`/`work_item_attempts` append-only pattern. Several of these are security-critical
(opt-out flip = privacy event; brake-confirm = the safety valve). It is deliberately **not built** (it
gates P3, not P1/P2).

**What "ready" requires (owner action):** decide the admin-identity model (who authenticates, how) and
build the admin-audit table + write-path as the **first P3 work item, before any real-box go-live**. This
is buildable now against the existing pattern — it is not blocked by the host — so it can proceed in
parallel with Gate 1's procurement.

---

## Summary

| Gate | State | Blocks | Owner action |
|---|---|---|---|
| **1. Host (port25 ∧ PTR ∧ clean IP)** | 🔴 NOT READY | **all of P3** | Research + trial-box-vet V1–V5 before paying; pick verifier domain + private-net mechanism. **The wall.** |
| **2. GAP-13 reputation cost** | 🟡 READY (input) | nothing to *start*; shapes warmup | Set warmup ramp against **connections (~2× verifications)**, not verification count. |
| **3. Admin-audit log** | 🔴 NOT READY | P3 **go-live** | Decide admin-identity model + build the append-only admin-audit write-path (buildable now, parallel to Gate 1). |

**Bottom line:** Phase 2 is genuinely complete and committed green; the brain is config-flip-ready. But
**Phase 3 cannot start until a real host passes V1–V5 on a trial box (Gate 1) — that host does not exist
yet, and acquiring it is the real work.** Gate 3 (admin-audit log) can be built in parallel now; Gate 2 is
a known number to fold into the warmup ramp. Two of the three gates need owner action; one is design-ready.

*Also still open (unchanged, non-blocking): LICENSE (DECISIONS §6.1 — pick before external distribution).*
