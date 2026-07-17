# ADR-0009 — GAP-24 egress-IP invariant is runtime-enforced; Phase-3 additive surface

**Status:** ACCEPTED (2026-07-17) — Phase-3 (first real box) local build.
**Door touched:** **Door 2** (LOG provenance — `verifier_ip`) in spirit, and the **Door-1/Door-4
additive-only** guarantees. No frozen door is mutated (see the reopen check).
**Sources:** [`../phase-3-first-real-box-spec.md`](../phase-3-first-real-box-spec.md) §4.4, §5.4, §6.6/§6.6.1, §8, Appendix B row 14; [`../DECISIONS.md`](../DECISIONS.md) DR-3; GAP-15, GAP-24, OQ-P3-6/8/11.

---

## Context

Phase 3 turns the already-built contracts into a live, audited box. It changes **no brain verdict
logic** and adds **no new `status`/`reason`/`recommendation` value**. But it does three things worth
a durable record because they touch door territory:

1. **It populates `verifications.verifier_ip`** (a P1-reserved Door-2 column, always NULL until now)
   with a **real** SMTP egress IP. A wrong value here silently corrupts reputation provenance and
   every future per-IP flywheel rollup — so *how* the value is sourced is a correctness commitment.

2. **GAP-24 (the multi-homed trap, §6.6.1):** the box needs a **private** IP for brain ingress and a
   **public** IP for SMTP egress. `host_audit` auto-detects `current_host_ip` and can go **green on
   the private IP while the real public egress IP is entirely unaudited** — a silent pass that would
   mint verdicts (and a `verifier_ip`) against an un-vetted, possibly-dirty IP. This is the
   "sharpest catch in all three specs" (Appendix D). The spec makes it a `LOCKED` invariant:
   **audit IP == A-record == PTR == egress == the public egress IP.**

3. **It adds a tiny additive surface:** one new `signals` key (`smtp_skipped_reason`) and two new
   tables (`node_audits` append-only history, `node_warmup` durable counter).

The question this ADR settles: is the invariant *enforced in running code* (not just prose), and is
the additive surface genuinely additive against the five frozen doors?

## Decision

1. **The GAP-24 four-way IP invariant is enforced at runtime, in two places, and test-proven
   locally against a simulated audit surface — not deferred:**
   - **The eligibility gate** (`internal/audit.Eligibility`, consumed by the router's `SMTPRoute`):
     a node is `:smtp`-eligible **only if** the latest audit's `current_host_ip` **equals** the
     node's expected public egress IP (the brain-registry `PublicEgressIP`). If they differ — or the
     expected IP is unknown — the node is **excluded** (`SkipEgressMismatch`), fail-safe. A multi-
     homed box that audited its private interface can never serve `:smtp`.
   - **The re-audit scheduler** (`internal/audit.Scheduler`): a green audit whose `current_host_ip`
     ≠ the expected egress IP is flagged `EgressMismatch` and driven through the state machine **as
     if red** — it never promotes `provisioning→active` or recovers from quarantine on a wrong-IP
     green. The mismatched audit is still recorded verbatim (history is truth; an operator sees it).
   - **`verifier_ip` sourcing (OQ-P3-11):** the logged `verifier_ip` is the **brain registry's**
     `PublicEgressIP`, never a box self-report. Because the gate already required
     `audit.current_host_ip == PublicEgressIP`, the logged IP is the IP that actually made the SMTP
     connection. This keeps the box dumb about its own reputation identity (Door-5 spirit).

2. **Only the real IP *values* are `PENDING-REAL-BOX`.** The invariant logic and its failing-case
   tests are done now (`internal/audit`, `internal/router`, `internal/orchestrator` tests, including
   a neuter/fail-verify of the GAP-24 conjunct). On the local machine `PublicEgressIP` and
   `port25_open` are empty/false, so **no node is `:smtp`-eligible** — the correct, honest state for
   a box that has no real egress-25 or PTR yet. A real host sets the value at provisioning; the code
   consuming it does not change (the mock→real flip, §5.3).

3. **The additive surface is additive-only (§8):**
   - **Door 1 / Door 4:** one new `signals` key, `smtp_skipped_reason ∈
     {no_eligible_node, audit_red, provider_mx_routed, warmup_cap}` — added to the open `signals`
     bag; **zero** new `status`/`reason`/`recommendation` values; `vocab.go` unchanged.
   - **Door 2:** `verifier_ip` and `nodes.audit_status/last_audit_at/port25_open` are **populated**,
     not added (reserved in P1). `node_audits` and `node_warmup` are **new tables** — they touch no
     frozen schema, and `node_audits` is **append-only, grant-enforced** (cp_runtime INSERT+SELECT,
     DELETE revoked — the LOG's own posture), proven live.
   - **Door 3:** unchanged (the `verification_raw` redaction of ADR-0008 carries through verbatim;
     `verifier_ip` is an IP, not an address).
   - **Door 5:** the verifier-node fork + gem are **byte-identical to Phase 2** — Phase 3 deploys the
     P2 wrapper unchanged and adds only brain-side code (a candidacy filter + a scheduler) and
     infrastructure. The `/v1/audit` and `/v1/config` endpoints Phase 3 consumes were built in P2.

4. **PB1 (capacity-gap fallbacks are non-cacheable):** when the SMTP-eligible residue can't be
   served (red / no eligible node / warmup cap), the verdict degrades to the cheap outcome
   (**never invalid**, OQ-P3-6), carries the `smtp_skipped_reason` signal, and is **not cached** —
   caching a capacity gap would freeze a lower-quality answer and suppress the SMTP verdict once the
   node recovers. `provider_mx_routed` is a *settled* routing outcome → remains cacheable.

## Consequences

- The silent-verdict-poisoning trap (GAP-24) cannot occur: a green on the wrong IP neither routes
  `:smtp` nor promotes the node. Verified by `TestScheduler_GreenOnWrongIP_DoesNotPromote_GAP24`,
  `TestSMTPRoute_EgressMismatchExcluded_GAP24`, and `TestEligibility_EgressMismatchFailsGate_GAP24`
  (the last two fail-verified by neutering the conjunct).
- On the local machine the whole SMTP path is built and tested but **inert** for real traffic (no
  eligible node, because `port25_open=false` and `PublicEgressIP` empty) — the honest state. The
  cutover to a real box is config-only (set `TM_NODE_PUBLIC_EGRESS_IP`, prove egress-25 →
  `port25_open=true`, audit green). See [`../phase-3-cutover-runbook.md`](../phase-3-cutover-runbook.md).
- `provider_mx_routed` verdicts stay cacheable; capacity-gap verdicts don't — a deliberate
  discriminator (PB1) that keeps a recovered node's better verdict from being suppressed by a stale
  cache entry.

## Does this reopen a frozen door?

**No.** Every change is additive (one `signals` key, two new tables, reserved columns populated) or
a correctness invariant *around* a frozen field (`verifier_ip` sourcing). `vocab.go` and the
hashing scheme are byte-unchanged; the gem is byte-identical (Door 5); the LOG stays append-only
(Door 2), and the new history table adopts the same append-only grant enforcement. This ADR
**enforces** the doors (harden-toward-`unknown`: an unconfirmable IP → not eligible → cheap verdict,
never a guessed `valid`/`invalid`); it does not mutate any of them.
