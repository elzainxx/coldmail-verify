# Truemail Verification System — Phase 4 (Resilience & pool) Design Specification

**Status:** APPROVED (v1.0) — adversarial architecture + backend-architect + senior-devops review pass folded in (Appendix C: PB1, PM1–PM4 resolved inline); **all 14 OQ-P4 signed off** (Appendix D), the four flagged items (OQ-P4-9/7/12/3) affirmed with reasoning · **Scope:** Phase 4 only · **Type:** Planning deliverable (no code, no provisioning, no second box bought)
**Sources:** `truemail-build-plan.md` (§ PHASE 4), `truemail-architecture-detailed.svg` (§④ SMTP hardening / per-provider rate+jitter / HELO rotation, §⑤ provider trust, §⑦ node lifecycle / quarantine-recovery, §⑧ topology 1→N), and the **approved** `phase-1-foundations-spec.md`, `phase-2-smtp-mocks-spec.md`, `phase-3-first-real-box-spec.md` (frozen contracts inherited), plus upstream ground truth from the `truemail` gem / `truemail-rack` (Appendix A).
**Inherits (frozen, not redesigned):** the five one-way doors — `/verify` response shape, LOG schema, email hashing, `status`/`reason`/`recommendation` vocabularies, node-list/never-fork. Phase 4 is **additive-only** against all five, and adds **one** new `signals` enum value (`smtp_skipped_reason:"rate_limited"`) beyond what P1/P2/P3 already reserved. Phase 4 is where the **node-list discipline (Door 5) and the config-flip (P2/P3) are cashed in at N>1** — the brain becomes a *pool operator*.

---

## 0. About this document

### 0.1 Purpose
Expand **Phase 4 — Resilience & pool** of the build plan into a complete, buildable design specification, at the same rigor as the Phase 1–3 specs. Phase 4's goal, stated by the build plan:

> **Survive blocks, scale by config.** Phase 4 done when: **a blocked box auto-quarantines, traffic re-routes, and adding a box is a config edit.**

Phase 3 delivered *one* audited box returning real verdicts, and deliberately deferred the machinery that makes a *fleet* safe: the continuous health-watcher, block-rate-driven quarantine, per-provider rate limiting, the repeatable provisioning script, and the second node. Phase 4 builds exactly those. Because Phase 4 is about **distributed-system behavior** — many nodes, health, routing, rate-limiting, failure isolation — the two capabilities carrying the most weight are **backend-architect** (router strategies, rate limiter, state placement, the control loop) and **senior-devops** (the provisioning script, observability, the pool's operational posture).

### 0.2 Scope discipline
- **In scope:** Everything under build-plan § PHASE 4 — (a) the **health-watcher** (continuously polls `/v1/audit` + `/health` + per-node block-rate → drives auto-quarantine, building on P3's audit signal + the `node_audits` history + the P3 gate); (b) **router strategies** — **sticky-by-domain (default)** + **least-inflight (fallback)**, both new implementations behind the P1 `Strategy` interface; (c) **per-provider rate limit + jitter** (the reputation-protection machinery P3's warmup explicitly was *not*); (d) the **repeatable provisioning script** (turns P3's vetting checklist into an ordered, idempotent script design — spin → install → PTR → audit → register → warm up); (e) **node #2 = +1 config line** (prove 1→N is config-only); (f) **observability** — metrics (block-rate, unknown-rate, latency, cache-hit) + alerts (quarantine, block spike, audit red).
- **Out of scope:** Phases 5–7. Where a Phase 4 decision touches a later phase, this doc marks the **`SEAM →`** and stops. In particular: **no queue / greylist-retry / backoff loop** (Phase 5 — a rate-capped or greylisted check *degrades* or *marks-retry-eligible*, it is **never** queued here, §4.6); **no third-party fallback** (Phase 6); **no bounce calibration / provider auto-promotion** (Phase 7); **no multi-brain rate-limiter state** (a single brain is assumed, GAP-25); **no topology migration** (1-VPS-N-IPs ↔ N-VPS-1-IP, SVG ⑧ — flagged as a decision, not migrated).
- **No implementation:** no Go, no Ruby, no Terraform/Ansible/cloud-init, no shell. The provisioning "script" is an **ordered stage design + contract**, not runnable code (mirrors P3's "checklist, not a run script" discipline, one level more concrete). Interface *shapes*, state placement, thresholds-as-dials, decision rules, and control-loop semantics only.
- **Additive-only against Phases 1–3:** Phase 4 adds **two** new `Strategy` implementations behind the **unchanged** P1 §5.4 interface; **populates/consumes** the P3 `node_audits` history and `nodes.status` lifecycle; adds **at most one** new `signals` enum value (`smtp_skipped_reason:"rate_limited"`, additive to the P3 enum); and adds **new operational tables** (rate-limit budgets, in-flight is ephemeral) only where they touch **no frozen schema**. It changes **nothing** already frozen.

### 0.3 How to read the tagging (identical to Phases 1–3)

| Tag | Meaning |
|---|---|
| **`LOCKED`** | A one-way door (inherited) or an already-locked build-plan decision. Additive-only afterwards. |
| **`DIAL`** | A build-time / runtime value tunable later without breaking history or consumers (thresholds, windows, cadences, caps, jitter spread). |
| **`DECISION REQUIRED`** | An open choice surfaced with a recommendation but **not** assumed. Needs sign-off before build. |
| **`SEAM →`** | A Phase 4 design point that deliberately leaves room for a later phase; noted, not built. |
| **`GAP`** | A contradiction/omission found across the build plan, the SVG, the P1–P3 specs, and/or upstream truemail reality — flagged, not silently resolved. |

> **Priority rule (governs this doc):** **sticky-by-domain routing (§3)** and the **block-rate quarantine state machine (§5)** are specified in full — they are the two highest-rigor items (the prompt names both). The **per-provider rate limiter (§4)** is specified to backend-architect rigor because sticky routing is meaningless without it (and vice-versa). The **provisioning script (§6)** and **observability (§8)** are specified to devops rigor. **Node #2 (§7)** is specified as a *proof obligation*, not new machinery. Everything else is specified well enough to build and no further.

### 0.4 The two principles Phase 4 must not violate
Both inherited, restated because every Phase-4 decision descends from them:

1. **Harden toward `unknown`, never `invalid`** (P1 §7.5, P2 §0.4, P3 §0.4). A check that cannot run because a node is quarantined, rate-capped, or the pool is saturated is a **your-fault `unknown`/degraded-to-cheap** outcome — **never** an address `invalid`. Phase 4 adds *new reasons a check can't run* (rate cap, no eligible node in a churning pool) — every one degrades honestly, never fabricates a verdict.
2. **One dirty box must not poison the fleet** (build-plan §0 "health-based auto-quarantine kept — IP protection"; SVG ⑦). This is the entire point of a pool. Phase 4's failure-isolation guarantee (§5.6) rests on P3's **`verifier_ip` tagging** making per-node block-rate *measurable*, and on quarantine *removing exactly one node* from candidacy without disturbing the rest — subject to the redistribution caveat (GAP-28).

---
## 1. Phase 4 at a glance

### 1.1 What Phase 4 delivers (from the build plan)
> **Phase 4 done when:** a blocked box auto-quarantines, traffic re-routes, and adding a box is a config edit.

Three provable outcomes, in the build plan's own words:
1. **Auto-heal:** a box that starts getting blocked (or goes audit-red) is **automatically quarantined** by the health-watcher — no operator in the loop (§5).
2. **Re-route:** traffic that would have gone to the quarantined box **flows to the rest of the pool** with no dropped verdicts and no `invalid` (§3, §5.6).
3. **Scale by config:** adding node #2 (and #3, #N) is **a config edit** — no code change (§7). This is the Door-5 node-list discipline finally exercised at N>1.

Underneath those three, Phase 4 also builds the **reputation-protection layer** P3 explicitly deferred: **per-provider concurrency caps + jitter** (§4), so a warmed pool paces itself against each provider instead of hammering it.

### 1.2 What Phase 4 consumes from Phases 1–3 (the seams now cashed in)
Phase 4 builds new control-plane behavior, but it **activates** a large amount that earlier phases reserved specifically for it:

| Consumed from | What it is | Phase 4 action |
|---|---|---|
| P1 §5.4 | `Strategy.Pick(ctx, plan, candidates []Node) (Node, error)` interface (shipped at N=1 with `single`/`random`) | Add **`sticky-by-domain`** (default) + **`least-inflight`** (fallback) as new implementations — **no interface change** (§3). |
| P1 §5.3 / OQ-5 | Node registry = **config-list authoritative**, `nodes` table = mirror/health-history home | Node #2 is **+1 config-list entry** (§7); the `nodes` table becomes the health-watcher's working state (§5). |
| P1 §7.2 | Policy `provider_routing` map (the single source for `:mx`/`:smtp` per provider) | Extends with an additive `rate_limits` key supplying **per-provider caps/jitter as parameters** (§4.2) — the Strategy consumes them; grammar unchanged (P1 OQ-25 open-object). |
| P3 §4.3 / §8 | `node_audits(node_id, audited_at, audit_status, warnings, current_host_ip)` append-only history | The health-watcher **reads** the flap history to debounce quarantine/recovery (§5.4); it does not rebuild it. |
| P3 §4.4 | The eligibility gate: `:smtp`-eligible iff `audit_status==green AND port25_open` (a router candidacy filter) | Phase 4 **extends the same candidacy filter** with `status=='active'` (not quarantined) + `not rate-capped` + `warmup-fresh` (§3.3) — same filter, more predicates. |
| P3 §4.5 / GAP-18 | Audit-driven quarantine (first-red) + the SVG ⑦ lifecycle state machine; **block-rate-driven quarantine deferred to P4** | Build the **continuous watcher** and the **block-rate trigger** — P3's explicitly-deferred core (§5). |
| P3 §5.2 | Warmup = durable ascending cap in Supabase (per-node/per-window counter), restart-safe | The **per-provider rate limiter** reuses this **exact durability discipline** (§4.4) — restart-safe budget state, mirror the warmup-counter reasoning. |
| P2 §4.4 | `GET /v1/stats` (uptime, engine, counters: verify_total, smtp_sessions, timeouts, 5xx/4xx) — "consumers are P4" | The health-watcher **polls it** as one of three health inputs (§5.2). |
| P2 §4.2 / P3 §4 | `GET /v1/audit` + `audit_status` | The health-watcher **polls it** on a cadence (§5.2), replacing P3's minimal scheduler. |
| P3 §3 (V1–V8 vetting) | The pre-purchase vetting checklist + the port-25 egress smoke test | Encoded into the **repeatable provisioning script** as ordered, idempotent, fail-closed stages (§6). |
| P3 §6.4 / GAP-22 | Per-node unique tokens; HELO/verifier rotation flagged as "the first P4 reputation feature a real box needs" | Rotation is **designed here** as part of the reputation layer (§4.5); per-node tokens are now the pool norm (§6). |

### 1.3 The frozen inheritance (context, not re-litigated)
Carried verbatim from the approved P1/P2/P3 specs — **none may change; Phase 4 only activates, consumes, or adds behind the frozen interfaces:**

- **Door 1** — `/verify` response shape versioned & additive-only. P4 adds **one** `signals.smtp_skipped_reason` enum value (`rate_limited`) — additive to the open string enum P3 introduced (`no_eligible_node|audit_red|provider_mx_routed|warmup_cap`), and adds **no** top-level field. `meta.node_id`/`verifier_ip` (real since P3) now vary **across a pool** — same fields, more distinct values.
- **Door 2** — the LOG is append-only. P4 writes the same rows; the only new *analytic* use is aggregating existing columns (`verifier_ip`, `status`, `reason`, `mx_provider`, `from_cache`) into per-node/per-provider metrics (§8). **No schema change to `verifications`.**
- **Door 3** — `norm/v1 + hmac-sha256/v1`, `bytea`, global pepper. **Sticky-by-domain routes on `domain` (already in clear, P1 §3.2.2), never on `email_hash`** — no new hashing, no pepper exposure to the router (§3.2). P4 changes nothing here.
- **Door 4** — `status`/`reason`/`recommendation` enums additive-only. **P4 emits zero new `status`/`reason`/`recommendation` values** — a rate-capped or pool-saturated check degrades to the *existing* cheap verdicts (`risky/plausible`, `provider_no_smtp`) exactly as P3's `no_eligible_node`/`warmup_cap` degrade (§4.6).
- **Door 5** — the **truemail gem is never forked**; only **truemail-rack** is. **Phase 4 is the phase Door 5 was written for:** the brain talks to a *list* of nodes, routes through the *Strategy interface*, and adds node #2 as *config*. If adding a node requires **any** code change, Door 5's node-list discipline failed — that is a `GAP` to raise, not to patch around (§7 tests this explicitly). The gem stays pinned + CI-asserted-unmodified; the deployed rack+wrapper is the P2/P3 fork unchanged.
- Boxes are dumb, stateless, disposable; **the brain holds all pool state in Supabase**; boxes never talk to each other; boxes never touch the DB. In P4 this is load-bearing: the health-watcher, rate-limiter budgets, and in-flight accounting are **all brain-side** — a box contributes only its `/stats` + `/audit` self-report and its verdict stream (§5, §4.4). A compromised/burned box is quarantined + re-spun (P3 §6), now automatically.

### 1.4 The make-or-break, named up front
Three facts dominate everything below:

- **Sticky-by-domain is what makes per-provider rate discipline real (§3.1, the highest-rigor item).** If a provider's checks scatter across all N nodes, each node independently opens sessions to that provider, and you **multiply the provider-perceived load by N** — defeating any per-provider cap and burning every IP in parallel. Sticky routing **concentrates a domain's (hence largely a provider's) checks on one node**, so a per-node/per-provider cap actually bounds what the provider sees. Rate-limiting without sticky routing is theatre; sticky routing without rate-limiting wastes the concentration. They are **one mechanism in two parts** (§3.1, §4.1).
- **Auto-quarantine on block-rate is a control loop, and control loops oscillate (§5, the other highest-rigor item).** P3 did audit-driven quarantine (a clean binary signal). Block-rate is a **noisy, lagging, statistical** signal: a small-sample node looks 100%-blocked after two unlucky checks; quarantining it redistributes its load onto peers, which then cross *their* thresholds — a **cascading false-quarantine** that can empty the pool (GAP-27/28). The state machine must be **hysteretic, sample-size-gated, and redistribution-aware**, or "auto-heal" becomes "auto-outage."
- **The rate-limiter's state must survive a brain restart (§4.4).** A per-provider budget that lives only in brain memory resets to full on every deploy/crash — so a freshly-restarted brain lets the whole pool sprint at every provider at once, exactly when (post-deploy) things are most fragile. This is the **identical hazard** P3 flagged for the warmup counter (P3 §5.2, PM4), and it takes the **identical fix**: durable budget state in Supabase. Getting this wrong silently converts the reputation-protection layer into a reputation *hazard* on every restart.

---

## 2. System context for Phase 4

Phase 3 lit up **one** real box behind the brain, gated by `/audit`, warming up under a single ascending cap. Phase 4 turns that into a **managed pool of N boxes** with a **continuous control loop** (the health-watcher) closing the feedback between "what providers are doing to our IPs" and "where we route + how fast." **The brain's request-path code is nearly unchanged** — new *Strategy* implementations and new candidacy predicates, not new stages. The genuinely new component is a **background control loop**, not a new request stage.

```
                    ┌────────────────────────────────────────────────────────────────┐
   caller ──POST──▶ │  BRAIN (Go, port 25 CLOSED)                                      │
   /v1/verify       │  front door → cache → policy → cheap layers → provider routing → │
                    │     ROUTER ── Strategy.Pick(candidates) ──┐  (sticky-by-domain    │
                    │       candidates = eligibility filter §3.3 │   default; least-     │
                    │       (active ∧ green ∧ port25 ∧ !capped)  │   inflight fallback)  │
                    │     → [rate-limiter admit? §4] → SMTP call → SCORING → LOG → resp │
                    │                                                                    │
                    │  ┌──────────────────────────────────────────────────────────┐    │
                    │  │  HEALTH-WATCHER (background loop, NEW) §5                   │    │
                    │  │   every T: poll /v1/audit + /health + read block-rate      │    │
                    │  │   from LOG (per verifier_ip) → update nodes.status         │    │
                    │  │   (active⇄quarantined) with hysteresis + sample gate       │    │
                    │  └──────────────────────────────────────────────────────────┘    │
                    └───────┬───────────────────────────────────────────┬──────────────┘
      reads/writes ALL      │                                            │ HTTPS (private net; per-node token)
      pool state            ▼                                            ▼
              ┌─────────────────────────────┐        ┌──────────────────────────────────────────────┐
              │  Supabase (all state)        │        │  POOL — N × (forked rack + wrapper), each      │
              │   nodes.* (status lifecycle) │        │  a P3-shaped box, UNCHANGED code               │
              │   node_audits (flap history) │        │   ┌────────┐  ┌────────┐        ┌────────┐     │
              │   rate_budgets (§4.4, NEW)    │◀──────▶│   │ node-1 │  │ node-2 │  ...   │ node-N │     │
              │   node_warmup (P3 §5.2)       │        │   │ (real  │  │ (+1 cfg│        │        │     │
              │   verifications (LOG)         │        │   │  IP,PTR)│ │  line) │        │        │     │
              └─────────────────────────────┘        │   └───┬────┘  └───┬────┘        └───┬────┘     │
                                                      └───────┼───────────┼─────────────────┼──────────┘
                                                              ▼           ▼                 ▼  OUTBOUND 25
                                                     honest corporate MX (Gmail/O365/Yahoo → :mx, never SMTP'd)
```

**Phase 4 truth (the config-flip cashed in at N):** the *only* thing that changes to go from N=1 to N=2 is **one more entry in the node config-list** (P1 §5.3), plus that node passing the same P3 provisioning + audit gate (now via the §6 script). **Every line of the request-path brain code is identical to Phase 3.** The new code is the **background health-watcher** and the **two Strategy implementations + rate-limiter admission check** — all of which sit behind interfaces/seams reserved since Phase 1. If going to N=2 needs a request-path code change, the node-list promise was false (a `GAP`, §7).

**What is genuinely new in Phase 4, by layer:**
- **Brain `internal/router` (Strategy impls):** `sticky-by-domain` + `least-inflight`, behind the unchanged P1 interface (§3). Plus the extended eligibility filter (§3.3).
- **Brain — the rate limiter (`internal/ratelimit`, new):** per-(node × provider) concurrency admission + jitter, with **durable budget state** in Supabase (§4).
- **Brain — the health-watcher (`internal/health`, new):** the background control loop polling audit + `/health` + block-rate, driving `nodes.status` (§5). This is the largest new surface and the one with control-theory risk.
- **Brain — in-flight accounting:** ephemeral per-(node,provider) counters for `least-inflight` + concurrency caps (§4.3) — the one piece of **intentionally non-durable** state, with a stated rationale (§4.3, GAP-26).
- **Operational — the provisioning script (§6)** and **observability (metrics + alerts, §8)** — the devops surface.
- **Supabase:** `rate_budgets` (new, §4.4) and the activation of `nodes.status` transitions + `node_audits` reads. **No change to any frozen schema** (`verifications`, `cache`, hashing).

## 3. Router strategies — sticky-by-domain (default) + least-inflight (fallback) — HIGHEST RIGOR

The prompt names this the load-bearing item. Both strategies are **new implementations behind the unchanged P1 §5.4 `Strategy` interface** (`Pick(ctx, plan, candidates []Node) (Node, error)`). No interface change — that is the Door-5 payoff (P1 §5.4 `SEAM →` said "sticky-by-domain + least-inflight are Phase 4; they are new `Strategy` implementations, no interface change" — this section cashes that in).

### 3.1 Why sticky-by-domain is the load-bearing strategy `LOCKED` (the rationale — the phase's spine)
The prompt: *"if a provider's checks scatter across all nodes, every node independently hits that provider and you multiply block risk by node count. Sticky routing is what makes per-provider rate discipline actually work."* Made fully explicit, because the whole reputation layer rests on it:

- **The failure mode without stickiness (N nodes, random/least-inflight routing):** checks for `acme.com` (and, more importantly, for a *provider* like a shared corporate MX or a warmed non-`:mx` provider) land on **all N nodes**. Each node independently opens SMTP sessions to that provider's MX. A per-node "≤ K concurrent to provider P" cap then permits **N×K** concurrent sessions provider-side — the cap is per-node, but the provider sees the **sum**. So the provider-perceived load, connection rate, and block risk **scale with pool size**, and adding nodes to "spread load" actually **multiplies** the reputation damage. Every IP gets trained-to-distrust in parallel.
- **What sticky-by-domain does:** it makes **one node the home for a given domain** (hence, in aggregate, largely the home for a given provider's honest-corporate domains — see the provider-vs-domain nuance in GAP-29). Now the provider mostly sees traffic from **one IP**, a per-node/per-provider cap **actually bounds** what the provider sees, warmup per-IP is meaningful, and a block affects **one measurable IP** (feeding clean per-node block-rate, §5.6) rather than smearing across the fleet.
- **The synthesis (`LOCKED`):** **sticky routing concentrates; the rate limiter paces the concentration.** Neither works alone: rate-limiting without stickiness caps the wrong denominator (per-node, not per-provider-seen); stickiness without rate-limiting concentrates load but doesn't bound its rate. Phase 4 builds **both, as one mechanism** (§3 + §4). This is the single most important design statement in the phase.

### 3.2 Sticky-by-domain — the mechanism `LOCKED` (stickiness key + affinity) / `DIAL` (mapping algorithm)
- **Sticky key = the normalized `domain`** (P1 `norm/v1`, already in clear on every request — Door 3 note: the router **never** touches `email_hash`/local-part, so no pepper/PII enters routing). **`LOCKED`.** Stickiness is by domain, not by full address — all of `acme.com` concentrates on one node, which is the point (per-provider concentration).
- **Scope: the router (and hence stickiness) only ever governs `:smtp`-eligible checks (`LOCKED`, review finding PM4).** Big-provider addresses (Gmail/O365/Yahoo) are `:mx`-routed and **never reach a node** (P1 §6.9, P3 §5.1) — they resolve to `risky/provider_no_smtp` with `smtp_skipped_reason:"provider_mx_routed"` before routing. Cheap-layer-decided verdicts (syntax/MX/disposable gates, P1 §6) also never reach the router. So sticky-by-domain applies to the **honest-corporate `:smtp` residue only** — which is exactly the traffic that touches real IPs and needs per-provider concentration. Stated so "route every domain to a node" isn't misread — most domains never get routed at all.
- **Affinity mapping (`DECISION REQUIRED`, OQ-P4-1):** how a domain maps to a node in the *current eligible set*. Two candidate mechanisms:

  | Mechanism | How | Trade-off |
  |---|---|---|
  | **A — Rendezvous (HRW) hashing (recommended)** | node = `argmax_over_eligible(hash(domain, node_id))`. | **Minimal reshuffle on membership change**: when a node quarantines, only *its* domains move; every other domain stays put. Naturally handles N→N±1 (the whole point of the phase). No stored table. Deterministic, stateless, brain-restart-safe. |
  | **B — Consistent-hash ring** | domains placed on a hash ring of nodes; walk clockwise. | Same "minimal movement" property; slightly more machinery (virtual nodes for balance). HRW is simpler for small N and gives better balance without vnode tuning. |
  | **C — Stored domain→node table** | persist assignments in Supabase. | Explicit + inspectable, but adds durable state that must be reconciled on every membership change (a mini-scheduler); reintroduces the churn HRW avoids. Rejected as over-engineered for the phase. |

  **Recommendation: A (rendezvous/HRW)** — it delivers sticky affinity *and* minimal-disruption re-routing (§5.6) from a pure function of `(domain, eligible-node-set)`, with **no stored routing state** to keep consistent. When the eligible set changes (quarantine/recovery/add-node), only the affected domains rehash; this is exactly the "re-route on quarantine, absorb a new node" behavior the phase must prove. `DIAL`: the hash function + any weighting (e.g., weight by a node's warmup headroom).
- **`LOCKED` invariant:** sticky affinity is computed **over the current *eligible* candidate set** (§3.3), never over all-configured-nodes. A quarantined/capped node is not a valid home; its domains rehash to eligible peers automatically. This is what couples routing to the health-watcher without the router calling the watcher — both read `nodes.status`.

### 3.3 The eligibility filter — extends the P3 candidacy filter `LOCKED`
The router's candidate set is the P3 filter (P3 §4.4) with Phase-4 predicates **added** (same filter, more conjuncts — not a new mechanism):

> **A node is a routing candidate for an SMTP check iff:**
> `enabled ∧ status=='active' ∧ audit_status=='green' ∧ port25_open ∧ audit-fresh` **(all inherited, P3 §4.4/§4.5)**
> `∧ not quarantined` (the health-watcher's live signal — §5; `status=='active'` already encodes this, stated explicitly)
> `∧ has warmup headroom` (P3 §5.2 durable cap not yet hit for this window)
> `∧ has rate-limit headroom for the target provider` (§4 — else this node isn't a valid home for *this* check right now).

- **Two-stage pick (`LOCKED` ordering):** (1) compute the eligible set via the filter; (2) apply the **sticky-by-domain** strategy to pick the home node; (3) **if the sticky home is ineligible or has no rate headroom**, fall through to the **least-inflight** fallback over the remaining eligible set (§3.4). Sticky first, least-inflight only as the fallback — exactly the build-plan wording ("sticky-by-domain (default) + least-inflight fallback").
- **Empty eligible set** = the P3 `no_eligible_node` degrade (cheap verdict, never `invalid`, non-cacheable — P3 §4.4/PB1). Phase 4 adds nothing to that rule; it just makes it more likely-to-be-temporary (a churning pool) and therefore reinforces **non-cacheable** (§4.6).

### 3.4 Least-inflight — the fallback strategy `LOCKED` (role) / `DIAL` (tie-break)
- **When it runs:** only when sticky-by-domain cannot place the check — the domain's HRW home is **ineligible** (quarantined/capped/no warmup headroom) **or** at its per-provider concurrency cap (§4.3). Least-inflight then picks, **among the remaining eligible nodes with rate headroom**, the node with the **fewest in-flight SMTP sessions** (best real-time load signal; §4.3 defines the in-flight counter).
- **Which in-flight count (`LOCKED`, review finding PM2):** the fallback ranks nodes by their **per-(node, target-provider) in-flight count**, not node-global in-flight — because the constraint being relieved is *this provider's* concurrency (§4.3), and a node busy with provider A still has headroom for provider B. Ranking on node-global in-flight would misroute around the wrong bottleneck. (Node-global in-flight is still useful as a **tie-break** and as an overload signal for metrics, §8 — but the primary fallback key is per-provider.)
- **Why least-inflight (not round-robin/random) for the fallback:** during a quarantine event, load is *already* unbalanced (a node just left); least-inflight actively drains toward the least-loaded survivor instead of blindly spreading, which dampens the "redistribute onto an already-stressed peer" cascade (§5, GAP-28). `DIAL`: tie-break among equal-inflight nodes (recommend: HRW again, so ties are still stable-ish per domain).
- **`LOCKED` boundary:** the fallback **never** places a check on an *ineligible* node to "spread load" — eligibility (§3.3) is absolute; least-inflight only ever chooses *within* the eligible set. A saturated eligible set → degrade (§4.6), never overflow onto a quarantined/capped node.
- **`SEAM →` P5:** when *every* eligible node is at its per-provider cap (not quarantined, just paced), P4 **degrades to the cheap verdict** with `smtp_skipped_reason:"rate_limited"` (§4.6). **Queuing that check for later** (when capacity frees) is the Phase-5 queue — the retry-eligible shape is left in `signals` (P2 §6.5) but nothing retries it here.

### 3.5 What the router still does NOT do (scope fence)
- **No cross-node coordination** — nodes never talk to each other (build-plan §0); all routing is brain-side from `nodes.status` + in-flight counters. The router reads shared state; it does not orchestrate boxes.
- **No domain-batching / session-reuse across many target addresses** — the 2-RCPT (target+probe) session reuse is P2 §6.3; batching *many targets for one domain* into one session ("group by domain") is **P5** (build-plan §5). Sticky-by-domain *sets up* that future batching (same domain → same node → same warm connection) but does not build it. `SEAM →` P5.
- **No queue** — a check that can't be placed degrades now (§4.6); it is never parked.

### 3.6 Cache correctness across a pool — the key is deliberately node-agnostic `LOCKED` (review finding PM1)
A subtlety the pool surfaces: the cache key is `(email_hash, config_fingerprint)` (P1 §4.2), and `config_fingerprint` covers engine+layer+policy config but **not** which node ran the check (P1 §3.2.5 — verifier identity is intentionally excluded because it's per-node, not verdict-affecting *in principle*). So a verdict computed on **node #1** can be served from cache to a request that would have routed to **node #2**. **This is correct and intended, and Phase 4 must not "fix" it:**
- A settled verdict (`valid/accepted`, `invalid/mailbox_not_found`, `risky/catch_all`) is a fact **about the address**, not about which IP proved it — a mailbox that exists exists regardless of which node's SMTP session observed the 250. Caching it node-agnostically is right; keying the cache per-node would **shatter the cache by pool size** (N× misses, N× provider load — the exact multiplication §3.1 fights) for no correctness gain.
- The verdicts that **are** node/transient-sensitive are exactly the ones **already non-cacheable** (P2 OQ-P2-6 + P3 PB1 + §4.6): `smtp_blocked`, `smtp_timeout`, `greylisted`, `connection_failed`, and every capacity/eligibility-gap degrade (`no_eligible_node`/`warmup_cap`/`rate_limited`). Those never enter the cache, so a node-specific transient can never be served for a different node.
- **`LOCKED`:** the cache stays node-agnostic; the settled-vs-provisional predicate (which the pool does **not** change) is what keeps it correct. Flagged because "different node served node #1's answer" *looks* like a bug at N>1 and is not — the discriminator is settled-vs-transient, not which-node.

## 4. Per-provider rate limiting + jitter (the reputation-protection machinery)

Build-plan §4: *"Per-provider rate limit + jitter (reputation protection)."* SVG §④: *"per-provider rate+jitter."* This is exactly what P3's warmup ramp was **explicitly not** (P3 §5.2: "warmup is one global ascending cap for one box; per-provider pacing, jitter, and the distributed rate limiter are Phase 4"). Warmup bounds *how much a fresh IP sends total*; the rate limiter bounds *how fast the pool hits each provider*. Both coexist per node (§4.6).

### 4.1 What the limiter protects, and why per-*provider* `LOCKED` (the dimension)
- **The reputation asset is per-IP-per-provider trust.** A provider (Gmail's MX, a corporate Proofpoint tenant, etc.) forms an opinion of *your IP* based on *your connection/attempt rate to it*. Too-fast → greylist → tarpit → block. So the meaningful rate dimension is **(node/IP × provider)** — how fast **this IP** approaches **that provider**.
- **Why sticky routing is a precondition (restated from §3.1):** the limiter is a **per-(node × provider)** control. It only bounds provider-perceived load if a provider's checks are *concentrated* on few nodes (sticky). Without stickiness, N nodes each independently under-cap still sum to N× provider-side. **The limiter and sticky routing are the same mechanism (§3.1).**
- **Provider identity `LOCKED`:** the limiter keys on the brain's `mx_provider` classification (P1 §6.9: `google|o365|yahoo|self|other`) **plus** a coarse **MX-host/domain bucket** for `other` (honest corporate), so distinct corporate providers aren't all lumped into one `other` budget. **`DECISION REQUIRED` (OQ-P4-2):** granularity of the `other` bucket — per-MX-host vs per-registrable-domain vs a single `other` pool. **Recommendation:** bucket `other` by **registrable domain of the MX host** (e.g. `pphosted.com`, `google.com`) so shared mail platforms are paced as one provider, while a single `other` pool (too coarse — one slow corp throttles all) and per-exact-MX-host (too fine — misses shared platforms) are both rejected. Flagged. (Gmail/O365/Yahoo are `:mx`-routed and **never SMTP'd** — P3 §5.1 — so the limiter's live work is on honest-corporate `:smtp` traffic; the `:mx` providers need no SMTP rate budget.)

### 4.2 The limiter shape — concurrency cap + rate + jitter `LOCKED` (the three controls) / `DIAL` (all values)
Per **(node × provider-bucket)**, three controls, all dials, sourced from the policy's additive `rate_limits` key (P1 §7.2 open-object; §4.7):

| Control | What it bounds | Default posture (`DIAL`) |
|---|---|---|
| **Concurrency cap** `max_inflight` | simultaneous open SMTP sessions from this node to this provider | small (e.g. 1–3); the primary reputation guard — providers notice parallelism fastest |
| **Rate cap** `max_starts_per_window` | new sessions started per rolling window | conservative (e.g. N/min); the "how fast" guard |
| **Jitter** `start_delay_jitter` | randomized delay before each new session start | a spread (e.g. 50–500ms) so starts don't arrive as a periodic, fingerprintable burst |

- **Jitter rationale (`LOCKED` that we jitter):** a rate cap alone still produces **evenly-spaced, machine-regular** connections — a fingerprint. Jitter randomizes inter-connection timing so the pattern looks less automated (SVG §④). It is cheap reputation insurance; the *spread* is a `DIAL`.
- **Interaction with warmup (`LOCKED`):** a node's effective allowance to a provider = **min(warmup headroom this window, rate-limit budget this window)**. Warmup is the *node-global* ceiling (P3 §5.2, durable); the rate limit is the *per-provider* ceiling. A check is admitted only if **both** allow it. Early in warmup, warmup usually binds; once warmed, the per-provider limiter binds. Neither replaces the other.

### 4.3 In-flight accounting — the one intentionally-ephemeral piece of state `LOCKED` (rationale) — GAP-26
`max_inflight` and `least-inflight` (§3.4) both need a **live count of currently-open SMTP sessions per (node, provider)**. This is the **one** piece of pool state that is deliberately **not** durable:

- **Why ephemeral is correct here:** an in-flight count is only meaningful *while sessions are actually open*. It is a real-time gauge of *this brain process's* current work, not historical fact. On a brain restart, any sessions the old process held are **dead** (their TCP connections died with the process) — so the correct post-restart in-flight count **is** zero. A durable counter would be **wrong after a crash** (it would count phantom sessions that no longer exist), forcing a reconciliation/decay mechanism that a fresh in-memory counter gets right for free.
- **`GAP-26` (the honest asymmetry vs §4.4):** this is the **opposite** decision from the rate *budget* (§4.4, which MUST be durable) — and the distinction is load-bearing, so it's flagged, not glossed: **in-flight = live gauge → ephemeral (restart ⇒ correct zero); rate/warmup budget = consumed-allowance-this-window → durable (restart must NOT refill it).** Confusing the two is the trap: making in-flight durable breaks after a crash; making the budget ephemeral refills the reputation allowance on every restart (§4.4). The rule: **durable = "how much have we already spent this window" (a fact); ephemeral = "how many are open right now" (a live measurement).**
- **`SEAM →` multi-brain (GAP-25):** with one brain, in-memory in-flight is exact. With multiple brain replicas, in-flight would need sharing (or partitioning domains across brains). **Phase 4 assumes a single brain** (P1 §5.8 runs a single brain); multi-brain rate-limiter coordination is explicitly out of scope and flagged (GAP-25, OQ-P4-8).

### 4.4 Rate-budget state MUST be durable in Supabase `LOCKED` — mirrors the P3 warmup-counter discipline
The **consumed-allowance** side of the limiter — how many session-starts this (node × provider) has spent in the current window — **must persist in Supabase**, exactly as the warmup counter does (P3 §5.2, review finding PM4).

- **The hazard if it's in-memory (`LOCKED` reasoning):** on every brain restart/deploy/crash, an in-memory budget resets to **full**. So a freshly-restarted brain would immediately permit the **whole pool to sprint at every provider at once** — a burst of the exact shape providers punish — and precisely at the moment (just after a deploy) when the system is least observed. A rate limiter that forgets its spend on restart is a **reputation hazard masquerading as a reputation control**. This is the same failure P3 caught for warmup; it takes the same fix.
- **The durable shape (`LOCKED` that it's durable; `DIAL` the table):** a small per-(node, provider-bucket, window) counter, e.g. `rate_budgets(node_id, provider_bucket, window_start, starts_count, window_len_s)`, incremented as sessions start; the admission check reads it. **`DECISION REQUIRED` (OQ-P4-3):** exact windowing — fixed tumbling window (simplest, cheap, slight boundary burstiness) vs sliding-log (smoother, more writes) vs token-bucket-with-durable-refill-timestamp (smoothest, one row per key). **Recommendation:** **token-bucket with a durable `last_refill_at` + `tokens` per key** — one row per (node, provider), restart-safe (tokens are reconstructed from `last_refill_at` on read, so no refill-on-restart), smooth, and cheap. Fixed windows are the acceptable simpler fallback if token-bucket bookkeeping is deemed heavy. Flagged.
- **Write-cost note (`DIAL`, acknowledged):** durable budget = a Supabase write per session-start (or a batched decrement). At Phase-4 volumes (a small pool, honest-corporate `:smtp` traffic only — big providers are `:mx`) this is modest; if it becomes hot, a **short-TTL write-behind** (in-memory admit + periodic durable flush) is the tuning path — but the **source of truth stays durable** so a restart never over-grants. `SEAM →` that optimization is a `DIAL`, not built now.

### 4.5 HELO / verifier-identity rotation — the reputation layer P3 deferred here `LOCKED` (that it belongs here) / `DIAL` (cadence) — closes GAP-22
P3 §6.5/GAP-22 flagged HELO/verifier rotation as *"the first P4 reputation feature a real box actually needs"* — inert against mocks (P2 §6.7), live-relevant on a real IP, deferred to P4. Phase 4 designs it:

- **What rotates:** the **HELO/EHLO hostname** and the **MAIL FROM verifier identity** (`verifier_email`/`verifier_domain`) a node presents. Rotating them resists trivial fingerprinting (a provider correlating every probe to one static HELO). **`LOCKED` constraint (from P3 §6.6 / GAP-24):** rotation must **preserve the audit invariant** — HELO/PTR/verifier-domain-A-record/egress-IP must stay mutually consistent, so rotation cannot break `/audit` green. This means rotation is **within a pre-audited set of identities per node**, not arbitrary strings.
- **`DECISION REQUIRED` (OQ-P4-4):** rotation granularity — per-node fixed identity (rotate only across nodes), vs a small **pool of pre-provisioned verifier identities per node** rotated over time. **Recommendation:** **per-node single identity in early P4** (each node already has a distinct verifier domain/IP from P3, so the *pool* already presents multiple identities to a provider only if sticky routing didn't concentrate — i.e. rotation across nodes is partly automatic), and treat **multi-identity-per-node rotation as a `DIAL` to enable once volume warrants** and each rotated identity has passed `/audit` (the invariant above). Aggressive per-check rotation is rejected — it multiplies the DNS/PTR/SPF provisioning surface (each identity needs its own audited alignment) for marginal gain at Phase-4 scale. Flagged.
- **`SEAM →`:** identity rotation *tuned from observed per-provider block-rate* (rotate faster where blocks rise) is a Phase-7-flavored feedback; P4 ships fixed/dial cadence, not adaptive. Noted.

### 4.6 When the limiter says no — degrade, never queue, never `invalid` `LOCKED` — one new signal value
When a check cannot be admitted because **every eligible node is at its per-provider cap** (not quarantined — just paced), Phase 4 does exactly what P3 does for `warmup_cap`:

- **The verdict degrades to the best cheap-layer outcome** (`risky/plausible`, or `provider_no_smtp` if applicable) with the SMTP layer **not run** — **never `invalid`** (harden-toward-`unknown`, §0.4). `verifier_ip=null` on that row (no node ran SMTP).
- **New additive `signals` value (`LOCKED`, the phase's only Door-adjacent addition):** `smtp_skipped_reason: "rate_limited"` — added to the P3 enum (`no_eligible_node|audit_red|provider_mx_routed|warmup_cap`). Additive-only to an open string enum (Door 1/§8); **no** new `reason`/`status` value.
- **Cacheability (`LOCKED`, extends P3 PB1 + P2 OQ-P2-6):** a `rate_limited` degrade is a **capacity/eligibility-gap fallback → provisional → NON-cacheable**, identical in class to `no_eligible_node`/`warmup_cap`. Caching it would freeze a lower-quality answer and suppress the SMTP verdict available once the cap frees. (Contrast `provider_mx_routed`, still settled/cacheable.) The P3 discriminator holds unchanged: **capacity gap → provisional/non-cacheable; policy routing → settled/cacheable.**
- **`SEAM →` P5 (the queue):** the *right* long-term answer to "capped now" is **retry when capacity frees** — that is the Phase-5 queue. P4 leaves the retry-eligible shape in `signals` (P2 §6.5 `retry_eligible`/`retry_after_hint_s`) so a P5 queue can act, but **P4 does not queue or retry** — it degrades synchronously. This is the cleanest seam in the phase: the rate limiter *creates* the backpressure that the queue will later *absorb*.

### 4.7 Policy integration — additive, no grammar change `LOCKED`
- The per-provider caps/jitter live under a **new `rate_limits` key** on the policy definition (P1 §7.2 open JSONB object, OQ-25 "tolerate unknown keys"):
  ```jsonc
  // additive key on the existing policy definition — shape/grammar unchanged
  "rate_limits": {
    "default":       { "max_inflight": 2, "max_starts_per_window": 30, "window_s": 60, "start_delay_jitter_ms": [50, 500] },
    "by_provider":   { "other:pphosted.com": { "max_inflight": 1, "max_starts_per_window": 10, "window_s": 60 } }
  }
  ```
- **`LOCKED`:** because the policy is an open object and every value here is a *parameter the Strategy/limiter reads* (not new grammar), this is **additive — no schema, no contract change** (the P1 §7.2 forward-expressiveness check named "P4 per-provider rate/jitter + sticky routing → a new `routing`/`rate_limits` key" exactly). `provider_routing` (the `:mx`/`:smtp` map, P1/P3) is untouched; `rate_limits` sits beside it.
- **`DECISION REQUIRED` (OQ-P4-5):** are rate limits **policy-scoped** (per cold-email policy) or **global pool config**? **Recommendation:** **global pool config** (reputation is a property of the IP↔provider relationship, not of which caller policy triggered the check) with **optional per-policy override** for a policy that wants to be gentler. A purely per-policy limit would let two policies independently exhaust the same IP↔provider budget — the wrong denominator again. Flagged; leans global-with-override.

## 5. The health-watcher + block-rate quarantine state machine — HIGHEST RIGOR

Build-plan §4: *"Health watcher: poll `/audit` + `/health` + block-rate per node → auto-quarantine."* This is the continuous control loop P3 explicitly deferred (P3 §4.5 `SEAM →` P4, GAP-18: "block-rate-driven quarantine is deferred to P4"). P3 built the *audit-driven* quarantine (a clean binary signal) and the lifecycle state machine skeleton (SVG ⑦); P4 builds the **continuous watcher** and adds the **block-rate trigger** — a noisy statistical signal that demands control-loop discipline the audit signal did not.

### 5.1 The watcher is a background control loop, not a request stage `LOCKED`
- A **single background loop** in the brain (`internal/health`), running every **T** (`DIAL`, recommend 30–60s), independent of the request path. It reads three signals per node, evaluates the state machine (§5.4), and writes `nodes.status`. The request path (§3) only ever *reads* `nodes.status` — the watcher is the *only* writer of the audit/block-driven lifecycle transitions. This single-writer discipline avoids two code paths racing on a node's status.
- **`LOCKED` separation of concern:** the watcher **decides** health; the router **reacts** to it (via the eligibility filter, §3.3). They communicate only through `nodes.status` in Supabase — no direct call. This is what keeps "auto-quarantine" and "re-route" decoupled and independently testable.

### 5.2 The three health inputs `LOCKED` (the set) / `DIAL` (thresholds)
Per node, each cycle:

| Input | Source | What it catches | Freshness |
|---|---|---|---|
| **Audit status** | `GET /v1/audit` (P3 §4, cadence now owned by the watcher) | PTR/DNS/A-record misalignment — the P3 hard gate | polled every audit-cadence (`DIAL`, ≤ hourly) |
| **Liveness/health** | `GET /health` (P2 stock `/healthcheck` + `/v1/stats` uptime/counters, P2 §4.4) | box down / unreachable / wedged (5xx, timeouts on the control endpoints) | every T |
| **Block-rate** | **the LOG** — aggregate recent `verifications` for this node's `verifier_ip` (P3-tagged): fraction of SMTP attempts resulting in `unknown/smtp_blocked` (and optionally `greylisted`/`smtp_timeout`) | the IP getting distrusted by real providers *despite* green audit — the P3-invisible failure (P3 GAP-18) | rolling window (`DIAL`, e.g. last 15–30 min or last K attempts) |

- **`LOCKED` — block-rate is measured from the LOG, keyed on `verifier_ip`.** This is precisely why P3 made `verifier_ip` real and enforced egress==PTR==verifier_ip==audit-IP (P3 §6.6/GAP-24): **without accurate per-node `verifier_ip`, per-node block-rate is unmeasurable and failure isolation (§5.6) is impossible.** The watcher does *not* ask the box "are you blocked" (a box can't know it's being quietly distrusted); it **infers** block-rate from the verdict stream the box produced. `SEAM →` `/v1/stats` counters (`5xx_total`/`4xx_total`, P2 §4.4) are a **secondary cross-check**, not the primary signal — the LOG is authoritative (the box's own counters could be wedged).
- **`LOCKED` — the block-rate query counts ONLY fresh, node-attributable SMTP attempts (review finding PB1).** The block-rate numerator/denominator is computed over `verifications WHERE from_cache=false AND verifier_ip = <node IP> AND <SMTP actually ran>` — i.e. it **excludes `from_cache=true` rows** (a cache hit ran no SMTP on any node — counting it would attribute a served verdict to a node that didn't produce it) and **excludes cheap-degraded rows** (`verifier_ip=null`, where SMTP was skipped for `no_eligible_node`/`warmup_cap`/`rate_limited`/`provider_mx_routed`, §4.6). Only rows where **this node actually opened an SMTP session** count toward its block-rate. Missing this filter would let cache hits and degrades silently inflate/deflate a node's rate and quarantine it for verdicts it never generated — the same `from_cache`-excluded-from-calibration discipline P1 §3.2.1 locked, applied to the control loop.
- **`DECISION REQUIRED` (OQ-P4-6):** which reasons count as "blocked" for the block-rate numerator. **Recommendation:** **`smtp_blocked` is the primary** signal (your-fault reject = reputation problem); **`smtp_timeout`/`connection_failed` are a secondary, separately-thresholded** signal (could be network, not reputation); **`greylisted` is NOT counted as a block** (it's expected transient, and counting it would quarantine nodes for doing their job). Flagged — the numerator definition directly drives false-quarantine rate.

### 5.3 Why block-rate is dangerous, and the three guards it needs `LOCKED`
Audit is binary and truthful; block-rate is **noisy, lagging, and statistical**. Treating it like the audit signal is how "auto-heal" becomes "auto-outage." Three guards, all `LOCKED`:

1. **Sample-size gate (`LOCKED`, guards small-N noise — GAP-27):** block-*rate* is meaningless below a minimum attempt count in the window. Two unlucky checks ≠ a 100%-blocked node. **The block-rate trigger does not fire until ≥ `min_samples` SMTP attempts** in the window (`DIAL`, e.g. ≥ 20). Below that, only audit/liveness can quarantine. Without this gate, a barely-used node (or one just added) false-quarantines on the first bad luck.
2. **Hysteresis (`LOCKED`, guards flapping):** the quarantine threshold and the recovery threshold are **different and asymmetric** (§5.4) — quarantine on a *high* block-rate over a *short* confirmation, recover only after a *sustained clean* period. A single threshold with no gap makes a node oscillate active⇄quarantined around the boundary.
3. **Redistribution-awareness (`LOCKED`, guards the cascade — GAP-28):** quarantining a node **moves its load onto peers** (sticky domains rehash, §3.2). Those peers' block-rate and in-flight then rise — possibly across *their* thresholds — triggering more quarantines: a **cascading pool collapse** from a shared/systemic cause (e.g., a blocklist that hit the whole subnet, or a provider-wide policy change that isn't any single IP's fault). The watcher must not treat a **fleet-wide** block spike as N independent per-node faults (§5.5).

### 5.4 The lifecycle state machine `LOCKED` (states + transitions) / `DIAL` (thresholds)
Extends the P3 SVG-⑦ machine (P3 §4.5) — same states, **block-rate trigger and hysteretic recovery added**:

```
 provisioning ──(first audit green ∧ port25_open ∧ SPF-checked; §6)──▶ active
     active ──(audit red)──────────────────────────────▶ quarantined     [P3, first-red, fail-safe]
     active ──(liveness fail × M consecutive)───────────▶ quarantined     [box down/wedged]
     active ──(block-rate ≥ QUAR_THRESH  ∧  samples ≥ min_samples
                        ∧ NOT a fleet-wide spike §5.5)──▶ quarantined     [NEW: block-rate trigger, guarded]
     quarantined ──(audit green ∧ liveness ok
                        ∧ block-rate ≤ RECOVER_THRESH sustained ≥ RECOVER_WINDOW)──▶ active   [hysteretic recovery]
     any ──(operator disable)───────────────────────────▶ disabled
     quarantined ──(operator: kill + re-spin, §6 / P3 §6)▶ (node replaced; new node re-enters at provisioning)
```

- **Asymmetric thresholds (`LOCKED` that they're asymmetric; `DIAL` values):** `QUAR_THRESH` (quarantine) is crossed on a *short confirmation* (e.g. block-rate ≥ 30% over ≥ min_samples, confirmed on **2 consecutive cycles** to reject a single-cycle blip); `RECOVER_THRESH` is *lower* (e.g. ≤ 5%) and must hold for a **sustained** `RECOVER_WINDOW` (e.g. 30–60 min of clean traffic). **Quick to quarantine, slow to trust** — the same asymmetry P3 locked for audit recovery (P3 OQ-P3-7), now applied to block-rate.
- **Recovery needs live traffic — the drain/probe problem (`DECISION REQUIRED`, OQ-P4-7):** a quarantined node gets **no traffic**, so its block-rate can't improve (no samples) — it could be stuck quarantined forever on the block-rate condition. Recovery must be driven primarily by **audit-green + liveness** (which don't need verification traffic), with block-rate recovery evaluated via a **small metered "canary" trickle** of real checks routed to the recovering node once audit/liveness clear. **Recommendation:** recovery = `audit green ∧ liveness ok` moves the node to a **`recovering` sub-state** that receives a **capped canary trickle** (a few checks/window, well under warmup start); if the canary's block-rate stays ≤ `RECOVER_THRESH` over `RECOVER_WINDOW`, promote to `active` (and re-enter warmup from a low cap, since the IP may have cooled); if the canary re-blocks, back to `quarantined`.
  - **`LOCKED` (review finding PM3) — recovery RESETS the durable warmup counter to a low start.** A node that was quarantined for blocks has, by definition, a **damaged/cooling IP reputation**; on promotion it must **not** resume at its pre-quarantine (possibly full) warmup allowance. The durable warmup counter (P3 §5.2) is **reset to a low cap** on recovery, so the recovered node re-warms gradually exactly like a fresh IP. Without this, a block-quarantined node that briefly recovers would immediately be permitted full volume into the same providers that just blocked it — re-burning it. Recovery is "start over gently," not "resume where you left off." Flagged — without a canary path, block-rate quarantine is a **one-way door to permanent quarantine**, which is a real design bug. (`recovering` is an internal sub-state of `quarantined` for candidacy purposes — it is NOT a general `:smtp` candidate; only the metered canary reaches it.)
- **`GAP-30`:** the P3 spec's recovery ("re-audit clean → active", P3 §4.5) implicitly assumed the *audit* signal drives recovery. With block-rate quarantine added, that assumption breaks (a node can be audit-green but block-quarantined). The `recovering`-canary sub-state (OQ-P4-7) is the resolution; flagged because it's a genuine gap between P3's clean-binary recovery and P4's statistical signal.

### 5.5 Fleet-wide spike detection — don't quarantine the whole pool `LOCKED` (that it's guarded) / `DIAL` (detection)
The redistribution cascade (§5.3 guard 3) and shared-cause blocks need an explicit brake:

- **`LOCKED` rule:** before quarantining node X on block-rate, the watcher checks whether the block spike is **node-specific** or **fleet-wide**. If **most/all active nodes** are simultaneously over threshold, the cause is almost certainly **systemic** (subnet blocklisting, a provider-wide policy change, an upstream DNS/network fault, or our own bad deploy) — **not** N independent dirty IPs. Quarantining them all empties the pool and turns a degraded state into a total outage, with **no healthy node to redistribute to**.
- **Response to a fleet-wide spike (`DECISION REQUIRED`, OQ-P4-9):** **Recommendation:** **do NOT auto-quarantine on a fleet-wide block spike; instead raise a high-priority alert (§8) and hold current membership** (optionally globally throttle via the rate limiter, §4). Auto-quarantine is the right response to **one anomalous node relative to its peers**; a fleet-wide event is an operator/systemic matter. So the block-rate trigger is effectively **"quarantine a node whose block-rate is an outlier *above the pool median*, given enough samples, and only while the pool as a whole is healthy."** Flagged — this is the single most important guard against the auto-heal→auto-outage failure.
- **`DIAL`:** the "fleet-wide" definition (e.g. > 50% of active nodes over threshold within the same window) and whether to auto-throttle vs merely alert.

> **[CONSOLIDATION NOTE — 2026-07-17 · additive · applies to both brakes] `[PN-8 / F-12, accepted]`**
> Cross-phase review: this fleet-brake and P7's cascade-brake (§6.4) both **alert-and-hold for a human**, but no phase specifies a **brake-hold timeout / default-on-timeout behavior** or an **authenticated confirm** — an alerting-pipeline outage silently neutralizes both valves. Accepted additions: specify default-on-timeout behavior (stay-held vs. auto-release — *value flagged, not decided*, [`DECISIONS.md`](DECISIONS.md) §5); make the human confirm an **authenticated, admin-audited** action (rides DR-3); and monitor alert-pipeline health. Additive to the existing `LOCKED` brake; changes no rule. Companion note at P7 §6.4. **Not built here.**

### 5.6 Failure isolation — the whole point of a pool `LOCKED`
The prompt: *"one dirty box doesn't poison the fleet — verify `verifier_ip` tagging makes per-node block-rate measurable, and that quarantine isolates correctly."*

- **Measurability (`LOCKED`, inherited chain):** per-node block-rate is computable **only because** every SMTP verdict logs an accurate `verifier_ip` (P3 §5.4) that provably equals the node's real egress/PTR IP (P3 §6.6/GAP-24). The watcher's block-rate query is `GROUP BY verifier_ip` over recent `verifications`. **This is the concrete payoff of P3's hardest invariant** — if `verifier_ip` were wrong or shared, one dirty box's blocks would smear across the fleet's stats and isolation would be impossible. Stated so the P3→P4 dependency is explicit.
- **Isolation (`LOCKED`):** quarantine removes **exactly one node** from candidacy (§3.3); sticky domains for that node rehash to eligible peers (§3.2, minimal reshuffle); every other node is undisturbed. The dirty IP stops receiving SMTP work immediately (next watcher cycle), so it stops accumulating blocks and starts cooling.
- **The redistribution caveat (`GAP-28`, honest):** isolation is clean **only if the cause is node-local**. If node X was blocked because of a **shared** cause (same subnet, same provider policy), rehashing X's domains onto peers just **moves the problem** — the peers will block too. This is exactly why §5.5's fleet-wide brake exists. Isolation is a guarantee for **independent per-node faults** (a single burned IP), not for **correlated fleet faults** — and the spec says so rather than overclaiming "one box never poisons the fleet" unconditionally.

### 5.7 What the watcher does NOT do (scope fence)
- **No auto-provisioning of replacements** — on kill+re-spin, the watcher marks the node quarantined/disabled; **spinning a replacement is the provisioning script (§6) invoked by an operator/automation**, not the watcher. (Auto-scaling the pool from load is beyond Phase 4 — `SEAM →` later.)
- **No cost/budget quarantine** — cost budgeting was dropped for v1 (build-plan §0); only **health-based** quarantine exists (IP protection). Unchanged.
- **No retry of degraded checks** — a check that hit `no_eligible_node` because the pool is churning degrades now (§4.6); the watcher heals the *pool*, it does not re-drive *past checks* (that's the P5 queue).

## 6. The repeatable provisioning script (devops) — design, not code

Build-plan §4: *"Provisioning script: spin → install → PTR → audit → register → warm up (repeatable)."* Phase 3 specified provisioning as a **vetting checklist + requirements** ("a design/checklist — not a run script yet", P3 §3). Phase 4 turns that checklist into a **repeatable, idempotent, fail-closed ordered-stage design** — one level more concrete than P3, but still **design, not runnable code** (§0.2). It exists because "adding a box is a config edit" (§7) is only true if standing the box up is a **push-button, repeatable** operation, not a bespoke manual ritual.

### 6.1 Design principles for the script `LOCKED`
- **Idempotent:** re-running a stage that already succeeded is a no-op (re-run safe after a partial failure). Provisioning a real box touches DNS/PTR/firewall — a half-applied run must be resumable, not a landmine.
- **Fail-closed at every gate:** a stage that cannot verify its own success **stops the pipeline and leaves the node `provisioning`/`disabled`** — it never advances a half-ready box toward `active`. The P3 gates (audit-green, port-25 egress proof) are **hard stops** in the script, not warnings.
- **The box stays disposable:** the script provisions a **cattle, not a pet** — everything it does is reproducible from config + the node's identity, so kill+re-spin (P3 §6, SVG ⑦) re-runs the same script. Nothing hand-tuned survives only on the box.
- **Never writes secrets to the box in clear beyond what P3 allows:** per-node token from the secrets manager (P3 §6.4); no DB creds, no pepper (P3 §6.4 — the box holds no state).

### 6.2 The ordered stages (each a gate) `LOCKED` (order + gates) / `DIAL` (mechanism)
Maps the build-plan's *spin → install → PTR → audit → register → warm up* to gated stages. Each stage's *mechanism* (which cloud API, which config tool) is a `DIAL`; the *order and the pass/fail gate* are `LOCKED` (the order is safety-critical — audit before enable, exactly as P3 §5.3.1):

| # | Stage | Does | Gate (fail-closed) | Anchored in |
|---|---|---|---|---|
| 1 | **Spin** | provision the VPS on a **pre-vetted** provider (P3 §3.2 V1–V8 already passed for this provider) with a dedicated, clean IP | IP reputation pre-check (P3 V3) — a pre-poisoned IP is rejected here, before anything else | P3 §3.2 |
| 2 | **Install** | deploy the **P2/P3 forked rack + wrapper container, byte-identical** (Door 5 — gem pinned, CI-asserted); apply firewall (ingress brain-only, egress 25+DNS only) | container health + firewall verified (external scan shows wrapper port closed to public — P3 §6.7) | P3 §3.5/§6 |
| 3 | **PTR + DNS + SPF** | set PTR (rDNS) on the **public egress IP** → verifier FQDN; A-record verifier domain → **public egress IP**; publish SPF | all three set **and propagated**; **the GAP-24 invariant checked**: PTR-IP == A-record-IP == public-egress-IP | P3 §3.3/§6.6.1 |
| 4 | **Egress proof** | one-time outbound TCP/25 smoke test to a known MX (envelope only, no DATA) | banner received → `port25_open=true`; **fail ⇒ stop** (audit-green alone is insufficient — P3 GAP-15) | P3 §3.4 |
| 5 | **Audit** | call `/v1/audit`; require green (`warnings=={}`) **on the public egress IP** | green **and** `host_audit.current_host_ip == public egress IP` (GAP-24 runtime check); red ⇒ stop, node stays `provisioning` | P3 §4 |
| 6 | **Register** | add the node to the **config-list** (authoritative, P1 §5.3) → seeds the `nodes` row; `enabled=false`, `status='provisioning'`, per-node unique token in secrets mgr | node appears in registry, health-watcher sees it, still ineligible (§3.3) | P1 §5.3, §7 |
| 7 | **Warm up** | set `enabled=true`; node enters `active` under a **low warmup cap** (P3 §5.2 durable counter, from a low start) + per-provider limiter (§4) | first real verdicts flow, logged with correct `verifier_ip`; block-rate watched (§5) | P3 §5.2, §4, §5 |

- **`LOCKED`:** stages 3→4→5 are the P3 eligibility trilogy (DNS/PTR identity → egress works → audit green) in the safety-critical order; the script **cannot reach stage 7 (enable)** without all of 1–6 passing. This is P3 §5.3.1's ordered checklist made into a pipeline with hard gates.
- **`DECISION REQUIRED` (OQ-P4-10):** how much of the script is **automated vs operator-gated**. Some stages resist full automation on the narrow port-25∧PTR market (P3 GAP-16, OQ-P3-1) — PTR-setting and port-25 unblocking may require **provider-specific manual/ticketed steps** that can't be scripted uniformly. **Recommendation:** script stages **2, 4, 5, 6, 7 fully** (idempotent, uniform); treat **stage 1 (spin) and stage 3 (PTR)** as **"script-with-manual-fallback"** — the script attempts the provider's API, but where a provider only offers PTR via a support ticket, the script **pauses at a gate** for the operator to complete it, then resumes and verifies. Flagged because pretending the whole thing is one-click ignores the P3 host-market reality (the same "the narrow market is the wall, not the design" ruling from P3 OQ-P3-1). **The script's honesty about its own manual gates is a feature, not a limitation.**

### 6.3 Relationship to node #2 and "config edit" `LOCKED`
- The provisioning script produces a **provisioned, audited, registered node**; the *routing* activation of that node is the **+1 config-list line** (§7). The script and the config edit are **two distinct acts**: the script makes a box *fit to serve*; the config line makes the brain *route to it*. Both must be true; neither alone suffices (a registered-but-not-enabled node is inert; an enabled-but-unprovisioned node fails the eligibility filter and never gets traffic — fail-safe).
- **`SEAM →` P4-later / P5:** invoking the script **automatically** (auto-scale the pool on load, auto-re-spin a killed node) is beyond this phase — Phase 4 makes the script **repeatable when invoked**, not **self-invoking**. Flagged.

---

## 7. Node #2 = +1 config line — the 1→N proof obligation

Build-plan §4: *"Add node #2 = +1 config line; prove 1→N is config-only."* This is not new machinery — it is the **acceptance test for Door 5** (node-list discipline). Everything needed already exists; §7 states what "config-only" must provably mean.

### 7.1 What "+1 config line" must literally be `LOCKED` (the invariant) / honest bound (GAP-31)
- **`LOCKED` invariant:** adding node #2 to the *routing pool* is **adding one entry to the node config-list** (P1 §5.3: `{ id, base_url, token_ref, enabled, capabilities }`) — **zero brain code change, zero redeploy of the brain, zero schema change.** The router (Strategy over the candidate list, §3), the health-watcher (iterates `nodes`, §5), the rate limiter (per-node budgets keyed by `node_id`, §4), and metrics (per-`verifier_ip`, §8) were **all built list-shaped/per-node from Phase 1** precisely so #2 needs no code. If any of them assumed N=1, that assumption is a **Door-5 violation to flag** (§7.3).
- **`GAP-31` (honest bound, mirrors P3 GAP-21):** "+1 config line" is true **for the routing activation** — but only *after* node #2 has been **provisioned + audited + registered** by the §6 script (which is emphatically **not** one line — it's a real box on the narrow port-25∧PTR market, P3 GAP-16). So the honest claim is: **"once you have a fit box, wiring it into the pool is one config line + config-only"** — the *code* is genuinely N-agnostic; the *box* is the same procurement wall P3 named. Flagged so "+1 config line" isn't misread as "adding a node is trivial" — the **brain** treats N as a config value; the **world** still makes you earn each clean IP. Same shape as P3's honest audit of "one config value" (GAP-21).

### 7.2 What adding node #2 exercises that N=1 never did `LOCKED`
Node #2 is the first time these are *actually* tested (they were built blind at N=1):

- **Sticky-by-domain actually distributes** — with two eligible nodes, HRW (§3.2) splits domains across them; a domain consistently lands on the same node (stickiness) and the split is roughly balanced (§3.2 balance property). At N=1 the strategy was a trivial pass-through; N=2 is its first real exercise.
- **Quarantine actually re-routes** — quarantining node #1 with node #2 present proves traffic **flows to #2** (build-plan outcome 2), not degrades to cheap. At N=1, quarantine could *only* degrade (no peer). **This is the phase's headline demo (§9 acceptance).**
- **Per-node rate budgets are independent** — node #1 hitting its per-provider cap does **not** consume node #2's budget (§4.4 keys on `node_id`); the fallback (§3.4) can place the check on #2. At N=1 there was no "other node" to fall back to.
- **Per-node block-rate is isolated** — a dirty node #1 shows a high block-rate on *its* `verifier_ip` without moving node #2's stats (§5.6). Failure isolation is untestable at N=1.

### 7.3 The 1→N proof `LOCKED` (acceptance)
The proof that 1→N is config-only (§9 acceptance criterion) is a **config-only diff**: adding node #2 is demonstrably a config-list edit (+ the §6 provisioning of the box) with **no diff in brain source**. If standing up #2 required touching router/watcher/limiter/store code, Phase 4 has **failed its central promise** and it is raised as a `GAP`, not worked around. (This mirrors P3 §5.3's "zero brain code diff mock→real" proof — now "zero brain code diff 1→N".)

## 8. Observability — metrics + alerts (devops)

Build-plan §4: *"Observability: metrics (block-rate, unknown-rate, latency, cache-hit) + alerts (quarantine, block spike, audit red)."* P1 §5.2 named the observability seam ("metrics for block-rate/unknown-rate/cache-hit is Phase 4"); P4 builds it. Observability here is **not decoration** — the block-rate metric **is** the health-watcher's input (§5.2), so metrics and control are the same data viewed two ways.

### 8.1 The metrics `LOCKED` (the required set) / `DIAL` (backend, exact buckets)
All derivable from **existing** LOG columns (Door 2 — no schema change) plus watcher/limiter internal counters. Emitted per-node (by `verifier_ip`/`node_id`) and per-provider (by `mx_provider`) and pool-wide:

| Metric | Definition (from existing data) | Primary use |
|---|---|---|
| **block-rate** | `unknown/smtp_blocked` ÷ SMTP attempts, per `verifier_ip`, rolling window | **the health-watcher input (§5.2)** + quarantine alerting |
| **unknown-rate** | all `status=='unknown'` ÷ total, per node + per provider | reputation/health trend; a rising unknown-rate is early warning before hard blocks |
| **latency** | `meta.latency_ms` distribution (p50/p95/p99), per node + pool | slow-node detection; SMTP timeouts vs cheap-path latency |
| **cache-hit** | `from_cache==true` ÷ total (P1 §3.2.2) | cost/efficiency; a cache-hit collapse signals config churn (fingerprint changes invalidating cache, P2 §4.5) |
| **pool health** (new, `DIAL`) | count of nodes by `status` (active/quarantined/recovering/disabled); eligible-set size | at-a-glance "is the pool healthy / is it collapsing (§5.5)" |
| **rate-limit pressure** (new, `DIAL`) | `smtp_skipped_reason=="rate_limited"` rate; per-provider budget utilization (§4.4) | is the pool too small for demand → capacity signal (`SEAM →` P5 queue trigger) |

- **`LOCKED`:** the build-plan's named four (block-rate, unknown-rate, latency, cache-hit) are required; the two "new" ones fall out of Phase-4 state and are strongly recommended (pool-health is how an operator sees a §5.5 cascade forming). **`from_cache==true` rows remain calibration-excluded** (P1 §3.2.1) but **are** counted in cache-hit metrics (they're real served answers) — the split P1 already locked.
- **`DECISION REQUIRED` (OQ-P4-11):** metrics backend — query-the-LOG (Supabase aggregate queries on a cadence) vs a real metrics/TSDB pipeline (Prometheus/etc.) fed by the brain. **Recommendation:** **both, layered** — the health-watcher already computes block-rate from the LOG (§5.2, authoritative, drives control), and a **lightweight metrics exporter** publishes the same aggregates to whatever dashboard/alerting backend for humans. The LOG is the source of truth (control decisions must not depend on a separate TSDB being up); the TSDB/dashboard is the human view. Flagged; leans layered.

### 8.2 The alerts `LOCKED` (the required set) / `DIAL` (thresholds, channel)
| Alert | Fires when | Severity | Why |
|---|---|---|---|
| **quarantine** | any node transitions active→quarantined (§5.4) | warn | an IP just left the pool — operator should know why (audit vs block vs liveness) |
| **block spike** | block-rate crosses a spike threshold, **especially fleet-wide (§5.5)** | **high (fleet-wide = critical)** | fleet-wide = the auto-heal→auto-outage danger (§5.5); the watcher deliberately does **not** auto-quarantine on it, so the **alert is the response** (OQ-P4-9) |
| **audit red** | a node's `/v1/audit` goes red (§5.2) | warn/high | the P3 hard gate tripped; PTR/DNS broke on a live IP |
| **pool-eligible-low** (new) | eligible-set size drops below a floor (e.g. ≤ 1, or below demand) | high | the pool is one quarantine from all-degrade; capacity/procurement signal |
| **rate-saturation** (new) | sustained `rate_limited` degrades (§4.6) | warn | demand exceeds paced capacity → add a node (§7) or (later) queue (P5) |

- **`LOCKED` linkage:** **block-spike + fleet-wide detection is the human backstop to §5.5's deliberate non-action.** The watcher's refusal to auto-quarantine a fleet-wide event (OQ-P4-9) is only safe if a human is **loudly alerted** — the alert and the control decision are designed together. This is the one alert that must never be merely a dashboard number.
- **`DIAL`:** all thresholds, alert channel, dedup/debounce. **`SEAM →`:** alert-driven auto-remediation (auto-re-spin, auto-throttle) beyond the health-watcher's existing transitions is later; P4 alerts a human for anything the watcher doesn't already handle autonomously.

### 8.3 What observability does NOT do here (scope fence)
- **No distributed tracing / APM build-out** — structured logs + `request_id` correlation (P1 §5.2) already exist; a full tracing stack is a `DIAL`/later, not required for the phase's "done."
- **No SLO/error-budget machinery** — the metrics enable it; formalizing SLOs is beyond "survive blocks, scale by config."

---

## 9. Additive surface introduced in Phase 4 (Door 1 / Door 2 additivity ledger) `LOCKED`

Phase 4 adds **behavior and operational state**, not verdict-contract surface. This ledger (mirroring P2 §8 / P3 §8) makes the tiny additive surface auditable so nothing sneaks in as a silent break.

**New `signals` values (additive to the open `signals` bag + the P3 open enum):**

| Additive item | Type | Set when | Source § |
|---|---|---|---|
| `smtp_skipped_reason: "rate_limited"` | new **enum value** on the existing P3 `smtp_skipped_reason` string key | every eligible node was at its per-provider cap; SMTP skipped, verdict degraded to cheap | §4.6 |

- That is the **only** verdict-contract-adjacent addition, and it is an **additive enum value on an already-open string key** (P3 §8) — **not** a new key, **not** a new `reason`/`status`/`recommendation`. `smtp_skipped_reason` was designed as an open string enum in P3 exactly so P4 could add to it.

**New operational tables (additive, touch NO frozen schema):**

| Table | Shape (sketch) | Purpose | § |
|---|---|---|---|
| `rate_budgets` | `(node_id, provider_bucket, window_start/last_refill_at, starts/tokens, window_len_s)` | durable per-(node×provider) rate-limit spend — restart-safe (§4.4) | §4.4 |

- `node_warmup` (P3 §5.2) and `node_audits` (P3 §4.3) already exist — P4 **reads/consumes** them, adds neither. The only new table is `rate_budgets`. It touches no frozen schema (`verifications`/`cache`/hashing untouched).

**Reserved/existing surfaces now activated at N>1 (not added):**

| Surface | Reserved/built in | Phase 4 use |
|---|---|---|
| `Strategy.Pick(...)` interface | P1 §5.4 (shipped at N=1) | two new implementations (sticky, least-inflight) — **no interface change** |
| node config-list (list-shaped) | P1 §5.3 | node #2 = +1 entry (§7) |
| `nodes.status` lifecycle | P1 §4.5 / P3 §4.5 | health-watcher is now its writer (§5) |
| `node_audits` history | P3 §4.3 | flap-history read for hysteresis (§5.4) |
| `verifications.verifier_ip` | P1 §3.2.2 / P3 §5.4 | the `GROUP BY` key for per-node block-rate (§5.6, §8) |
| policy `provider_routing` / open object | P1 §7.2 | additive `rate_limits` key (§4.7) |

**Zero changes to the frozen surface (the additivity guarantee):**
- **`status`/`reason`/`recommendation` enums:** **zero new values.** A rate-capped/pool-saturated check degrades to existing cheap verdicts.
- **`flags`:** no new flags. **`verifications` columns:** **zero added or altered.** **`/verify` response shape:** unchanged (one additive `smtp_skipped_reason` enum value, tolerated by the open-enum contract).
- **Email hashing / cache key:** untouched. Sticky routing keys on `domain` (in clear), never on `email_hash`/pepper.

## 10. Contradictions & gaps found in the source material (flagged, not resolved)

Per the requirement to surface — not quietly fix — inconsistencies across the build plan, the SVG, the P1–P3 specs, and truemail reality. GAP-1..24 are inherited (P1/P2/P3); **GAP-25..31 are new to Phase 4.**

| ID | Where | Gap | Handling in this spec |
|---|---|---|---|
| **GAP-25** (new) | build-plan §0 "1..N by config, same code" vs a single-brain rate limiter (§4.3/§4.4) | The rate limiter's in-flight (§4.3) and budget (§4.4) state assume **one brain**. If the *brain* is ever scaled to N replicas, in-flight is per-process and budgets would double-count/race. The build plan scales *boxes* 1→N but is silent on scaling *brains*. | **Phase 4 assumes a single brain** (P1 §5.8 runs one brain). Multi-brain rate-limiter coordination (shared/partitioned state) is explicitly **out of scope** and flagged (OQ-P4-8). Not resolved — a genuine future architecture decision. |
| **GAP-26** (new) | §4.3 in-flight (ephemeral) vs §4.4 budget (durable) | Two pieces of limiter state with **opposite** durability requirements — easy to get backwards, and getting it backwards is silently catastrophic (durable in-flight = phantom sessions after crash; ephemeral budget = reputation refill on restart). | Made explicit: **live gauge → ephemeral (restart⇒correct zero); consumed allowance → durable (restart must not refill)**. §4.3/§4.4. A design clarification, not a contradiction to resolve — flagged so it's never conflated. |
| **GAP-27** (new, **a control-loop trap**) | build-plan §4 "block-rate → auto-quarantine" | Block-*rate* on a **small sample** is noise; a lightly-used or freshly-added node false-quarantines on 1–2 unlucky checks. The build plan states the trigger without the statistics. | **Sample-size gate** (§5.3 guard 1): the block-rate trigger doesn't fire below `min_samples`; below that only audit/liveness quarantine. Not resolvable by the naive rule — the guard is the resolution. |
| **GAP-28** (new, **the big one — auto-heal→auto-outage**) | build-plan §4 auto-quarantine + §3 sticky redistribution | Quarantining a node **redistributes its load onto peers** (sticky rehash), raising *their* block-rate/in-flight → **cascading false-quarantine that empties the pool** on a shared/systemic cause. "Auto-heal" naively implemented is "auto-outage." | **Fleet-wide spike brake** (§5.5): do **not** auto-quarantine when most nodes are simultaneously over threshold (systemic, not N dirty IPs); alert instead (OQ-P4-9). Block-rate quarantine is for a node that's an **outlier above the pool**, only while the pool is healthy. The single most important guard in the phase. |
| **GAP-29** (new) | build-plan §4 "sticky-by-**domain**" vs §4 "per-**provider** rate limit" | Stickiness is keyed on **domain**; rate limiting is per-**provider**. Two different domains on the **same provider** (e.g. two corporates behind the same Proofpoint/Google-Workspace tenant) may hash to **different nodes**, so a provider's load still splits across nodes despite per-domain stickiness — partially defeating the per-provider cap (§3.1's premise). | Flagged honestly: domain-stickiness **approximates** provider-concentration but doesn't guarantee it for **shared mail platforms**. Mitigations (not fully resolved): the `other`-bucket granularity (§4.1/OQ-P4-2) paces shared platforms as one provider *per node*, and the per-provider cap still bounds each node — but the N-node multiplication (§3.1) is only *reduced*, not eliminated, for shared-platform providers. Truly provider-sticky routing (hash on provider, not domain) is a **`SEAM →`/OQ-P4-12** trade-off (it would concentrate a whole provider on one node — a single-point-of-failure and a hot node). Not silently resolved. |
| **GAP-30** (new) | P3 §4.5 recovery ("re-audit clean → active") vs P4 block-rate quarantine | P3's recovery assumed the **audit** signal drives it. A block-quarantined node can be **audit-green** yet blocked, and gets **no traffic while quarantined**, so its block-rate can never improve → **permanent quarantine** on the block-rate condition. | The **`recovering`-canary sub-state** (§5.4/OQ-P4-7): audit-green+liveness moves a node to `recovering`, which receives a **metered canary trickle**; clean canary over `RECOVER_WINDOW` → active. Without it, block-rate quarantine is a one-way door. Flagged as a genuine gap between P3's binary recovery and P4's statistical signal. |
| **GAP-31** (new, mirrors P3 GAP-21) | build-plan §4 "add node #2 = **+1 config line**" | "+1 config line" is true for **routing activation** but a node must first be **provisioned+audited+registered** (§6) — a real box on the narrow port-25∧PTR market (P3 GAP-16). "+1 line" hides the procurement wall. | Reframed (§7.1): **the brain is genuinely N-agnostic (code = one config line); the world still makes you earn each clean IP** (the §6 script + P3's host market). Honest bound, same shape as P3's audit of "one config value" (GAP-21). Not overclaimed. |
| GAP-13 (inherited) | P2 §5.4 / P3 note — catch-all session reuse | Re-surfaces at pool scale: sticky-by-domain sets up domain-batched session reuse (P5), and Option B's two-connection cost (P3 memory) is now **per node** across the pool. | **Not re-solved here** (P2/P3 question). Noted: sticky routing makes the future domain-batched session-reuse (P5) natural; the GAP-13 spike remains a pre-provisioning gate (P3 Appendix D standing directive). Cross-phase dependency, not a P4 decision. |
| GAP-15/16/24 (inherited, P3) | audit≠can-send; narrow host market; multi-homed audit IP | Still govern **every** node the §6 script provisions. | Honored: §6 stages 3–5 enforce the GAP-24 invariant + GAP-15 egress proof per node; GAP-16 host market is why §6.2/OQ-P4-10 keeps manual gates. Inherited, not re-opened. |

> **No gap is silently resolved.** GAP-28 (cascade) and GAP-27 (small-sample noise) are the sharpest — both are consequences of the build plan stating "block-rate → auto-quarantine" without the control-theory, and both are handled by explicit guards (§5.3/§5.5) rather than trusting the naive rule. GAP-29 (domain-sticky ≠ provider-sticky) is flagged as a **real, only-partially-mitigated** limitation, not papered over.

---

## 11. Open questions / decisions required (with recommendations)

Phase-4-specific. Numbered `OQ-P4-N` to avoid collision with P1 (OQ-1..25), P2 (OQ-P2-1..11), P3 (OQ-P3-1..14). All carry a recommendation; **none is assumed** — each needs owner sign-off before build.

| OQ | Topic | Recommendation | Touches |
|---|---|---|---|
| **OQ-P4-1** | Sticky affinity mapping (HRW vs consistent-hash ring vs stored table) | **Rendezvous/HRW hashing** — sticky affinity + minimal reshuffle on membership change, no stored routing state, restart-safe. | §3.2 |
| **OQ-P4-2** | Per-provider bucket granularity for `other` (honest corporate) | Bucket `other` by **registrable domain of the MX host** (paces shared platforms as one provider); reject single-`other`-pool (too coarse) and per-exact-MX-host (misses shared platforms). | §4.1 |
| **OQ-P4-3** | Rate-budget windowing (tumbling vs sliding-log vs token-bucket) | **Token-bucket with durable `last_refill_at`+`tokens`** — one row/key, restart-safe (no refill-on-restart), smooth. Fixed window = acceptable simpler fallback. | §4.4 |
| **OQ-P4-4** | HELO/verifier rotation granularity | **Per-node single audited identity** in early P4 (rotation across nodes is partly automatic); multi-identity-per-node rotation a `DIAL` once volume warrants + each identity `/audit`-green. Reject per-check rotation (multiplies DNS/PTR surface). | §4.5 |
| **OQ-P4-5** | Rate limits: policy-scoped vs global pool config | **Global pool config** (reputation is per IP↔provider, not per caller policy) **with optional per-policy override**. Purely per-policy = wrong denominator. | §4.7 |
| **OQ-P4-6** | Which reasons count as "blocked" in the block-rate numerator | **`smtp_blocked` primary**; `smtp_timeout`/`connection_failed` secondary + separately thresholded (may be network); **`greylisted` NOT counted** (expected transient). | §5.2 |
| **OQ-P4-7** | Recovery from block-rate quarantine (the drain/canary problem) | **`recovering` canary sub-state**: audit-green+liveness → metered canary trickle → clean over `RECOVER_WINDOW` → active (re-enter warmup low); re-block → quarantined. Without it, permanent quarantine (GAP-30). | §5.4 |
| **OQ-P4-8** | Single-brain assumption for rate-limiter state (GAP-25) | **Assume a single brain in P4**; multi-brain coordination out of scope, flagged. Revisit only if brain HA is required (then partition domains across brains, or shared budget store). | §4.3/§4.4 |
| **OQ-P4-9** | Response to a **fleet-wide** block spike (GAP-28) | **Do NOT auto-quarantine; raise critical alert + hold membership** (optionally global throttle). Auto-quarantine only for a node that's an outlier above a healthy pool. The phase's most important guard. | §5.5 |
| **OQ-P4-10** | How much of the provisioning script is automated vs operator-gated | Script stages 2,4,5,6,7 fully; **stage 1 (spin) + stage 3 (PTR) = script-with-manual-fallback** (pause-at-gate for provider ticket steps). Honesty about manual gates is a feature (P3 host-market reality). | §6.2 |
| **OQ-P4-11** | Metrics backend (query-the-LOG vs TSDB pipeline) | **Both, layered**: LOG is control's source of truth (§5.2); a lightweight exporter feeds a TSDB/dashboard for humans. Control must not depend on the TSDB being up. | §8.1 |
| **OQ-P4-12** | Provider-sticky vs domain-sticky routing (GAP-29) | **Keep domain-sticky in P4** (avoids concentrating a whole provider → hot node / single point of failure); accept that shared-platform providers are only *partially* concentrated; revisit provider-sticky only if shared-platform block-multiplication proves material in metrics. | §3.2/GAP-29 |
| **OQ-P4-13** | Watcher cadence T + block-rate window length | T = 30–60s; block-rate window = last 15–30 min **or** last K attempts, whichever gives ≥ `min_samples` soonest. Both `DIAL`; start conservative (longer window = less twitchy). | §5.1/§5.2 |
| **OQ-P4-14** | Does the health-watcher run in-process or as a separate brain sidecar? | **In-process background loop** in the (single) brain for P4 — simplest, shares the DB client + config, single-writer of `nodes.status`. A separate watcher process is a `SEAM →` only if brain HA arrives (ties to OQ-P4-8). | §5.1 |

---

## 12. Phase seams (where Phase 4 deliberately stops)

| Later phase | Seam left in / honored by Phase 4 | Not built now |
|---|---|---|
| **P5** queue | The rate limiter **creates the backpressure** (`rate_limited` degrade, §4.6) the queue will absorb; retry-eligible shape already in `signals` (P2 §6.5); sticky-by-domain **sets up** domain-batched session reuse (same domain → same node → warm connection). | The **queue itself**, backoff/retry loop, retry-then-accept round-trip, **domain-batched** many-target session reuse ("group by domain"), `/v1/verify/bulk` job handle, retrying `rate_limited`/`no_eligible_node` degrades when capacity frees. |
| **P6** fallback | Pool-saturation / structural-residue (`rate_limited`, `catch_all`, `provider_no_smtp`) is cleanly labeled `risky`/degraded — the exact residue P6 routes out; `reason=external_verified` still reserved. | Fallback API integration; routing the residue to it (a rate-saturated pool could route overflow to fallback — a P6 policy call, not built). |
| **P7** flywheel | Per-node/per-provider block-rate, unknown-rate, and full transcripts logged with real `verifier_ip` — the **operational** data the flywheel joins to bounce outcomes; per-provider trust (SVG ⑤, O365 nuance, P3 OQ-P3-9) still deferred; adaptive rotation/rate-tuning from block-rate flagged as P7-flavored (§4.5). | Bounce classification, per-(reason×provider) recalibration, provider auto-promotion, **block-rate-driven adaptive rate/rotation tuning**. |
| **Multi-brain HA** (cross-phase) | Rate-limiter state (§4.4 durable budgets) is in Supabase (shareable); in-flight (§4.3) is per-process (GAP-25). Single-brain assumed (OQ-P4-8/14). | Multi-brain rate-budget coordination, in-flight sharing/partitioning, watcher-as-separate-process, leader election for the single-writer `nodes.status` discipline. |
| **Auto-scale / self-healing infra** | The provisioning script is **repeatable when invoked** (§6); the watcher marks nodes for kill+re-spin (§5.7). | The script **self-invoking** (auto-scale pool on load, auto-re-spin a killed node), alert-driven auto-remediation beyond the watcher's existing transitions (§8.2). |
| **Topology choice** (SVG ⑧) | Nodes are list-shaped and per-`node_id`/`verifier_ip` addressable, so either topology fits the model. | The **1-VPS-N-IPs vs N-VPS-1-IP** decision (SVG ⑧) and any live migration between topologies — flagged (OQ-adjacent), not chosen or built here. |

---

## 13. Phase 4 "done" — restated with acceptance criteria

**Build-plan definition:** *a blocked box auto-quarantines, traffic re-routes, and adding a box is a config edit.*

Acceptance criteria for this spec's design (all verifiable on a **2-node pool** — the first time N>1 exists):

1. **Auto-quarantine on health (§5):** a node that goes **audit-red** is quarantined on first red (P3, inherited); a node whose **block-rate** crosses `QUAR_THRESH` with **≥ `min_samples`** and while the **pool is healthy** (not a fleet-wide spike, §5.5) is auto-quarantined by the continuous watcher — **no operator in the loop.** A **liveness** failure (box down/wedged) quarantines. All three triggers demonstrated.
2. **Traffic re-routes (§3, §5.6):** with node #1 quarantined and node #2 active, checks for node #1's sticky domains **rehash to node #2** and get **real SMTP verdicts** — not degraded to cheap. Zero dropped verdicts; **zero `invalid`** produced by the quarantine. (At N=1 this could only degrade; N=2 proves genuine re-route.)
3. **Adding a box is a config edit (§7):** node #2 enters the routing pool via **+1 config-list line + zero brain code diff** (§7.3), after the §6 provisioning script stands it up (spin→install→PTR→audit→register→warm up), each stage fail-closed. The "1→N is config-only" claim is proven by a config-only diff.
4. **Sticky-by-domain works (§3):** a given domain consistently routes to the same eligible node (stickiness); domains distribute across the 2 nodes (balance); on membership change, **only the affected node's domains rehash** (HRW minimal reshuffle, OQ-P4-1). Sticky-first, least-inflight-fallback ordering holds (§3.3).
5. **Per-provider rate limiting + jitter (§4):** per-(node×provider) concurrency cap + rate cap + jitter enforced from the policy `rate_limits` key; **budget state is durable in Supabase** (survives a brain restart without refilling — the P3 warmup-counter discipline, §4.4); in-flight is ephemeral and correct-at-zero post-restart (§4.3). A saturated cap **degrades to cheap** with `smtp_skipped_reason:"rate_limited"` — **never queues, never `invalid`** (§4.6).
6. **Failure isolation (§5.6):** a dirty node #1 shows a high block-rate on **its** `verifier_ip` without moving node #2's stats; quarantine removes exactly node #1. The measurement works **because** `verifier_ip` is accurate per-node (P3 §6.6 payoff). The redistribution caveat (GAP-28) is respected — a fleet-wide spike alerts, doesn't cascade-quarantine.
7. **Observability (§8):** block-rate, unknown-rate, latency, cache-hit metrics emitted per-node/per-provider/pool-wide from existing LOG columns (no schema change); the four build-plan alerts (quarantine, block spike incl. fleet-wide, audit red) fire; block-rate metric and health-watcher input are the same data (§5.2/§8.1).
8. **The cascade guard holds (§5.5, GAP-28):** a simulated fleet-wide block spike **does not empty the pool** — it raises a critical alert and holds membership (OQ-P4-9), rather than auto-quarantining every node.
9. **Recovery is not a one-way door (§5.4, GAP-30):** a block-quarantined node that goes audit-green recovers via the metered **canary** path, not stuck-forever. Hysteresis holds (quick to quarantine, slow to trust).
10. **Additivity (§9):** the entire Phase-4 surface is additive — **one** new `signals` enum value (`rate_limited`), **one** new operational table (`rate_budgets`), two new `Strategy` implementations behind the **unchanged** interface, and **zero** new `status`/`reason`/`recommendation`/`flags`/`verifications`-column/response-shape changes. A diff against the frozen Door-1/Door-4 contracts shows no removal, rename, or type change.
11. **Door 5 intact (§7):** the deployed rack+wrapper is the P2/P3 fork unchanged; the gem is pinned + CI-asserted-unmodified; **all** new Phase-4 behavior is brain-side (strategies, limiter, watcher) or operational (script, metrics) — **nothing** in the gem or the box's contract changed. Going 1→N required **zero request-path code change**.
12. **Every OQ-P4 (§11) is signed off or explicitly deferred with an owner** — none silently assumed. GAP-25 (multi-brain), GAP-28 (cascade), GAP-29 (domain≠provider sticky) carry explicit owner-visible stances.

## Appendix A — Upstream ground truth (captured, anchors the pool/rate/health design)

> Captured from the `truemail` gem / `truemail-rack` and the SVG so the Phase-4 design is anchored in what the tools actually do and what earlier phases actually built. Reference, not our contract. Extends the P1–P3 Appendix A with the Phase-4-relevant detail.

### A.1 What Phase 4 relies on being already true (from P1–P3, not new)
- **`Strategy.Pick(ctx, plan, candidates []Node) (Node, error)`** (P1 §5.4) — the router interface, shipped at N=1 with a trivial `single`/`random` strategy, explicitly reserving sticky/least-inflight for P4. **No interface change in P4.**
- **Node config-list** `{ id, base_url, token_ref, enabled, capabilities }` (P1 §5.3) — authoritative for routing; `nodes` table mirrors + holds health/audit history (OQ-5).
- **`nodes.status` ∈ {provisioning, active, quarantined, disabled}** + `audit_status`, `last_audit_at`, `port25_open` (P1 §4.5, populated P3) — the lifecycle the watcher drives.
- **`node_audits(node_id, audited_at, audit_status, warnings jsonb, current_host_ip)`** append-only (P3 §4.3) — flap history for hysteresis.
- **`verifications.verifier_ip` (inet)** = the node's real egress/PTR IP, provably egress==PTR==audit-IP (P3 §5.4/§6.6/GAP-24) — the per-node block-rate `GROUP BY` key.
- **`node_warmup(node_id, window_start, count, cap)`** durable, restart-safe (P3 §5.2) — the durability discipline the rate limiter mirrors.
- **`GET /v1/audit`** → `{current_host_ip, warnings, audit_status}` (P2 §4.2 / P3 §4), **`GET /v1/stats`** → uptime/engine/counters (P2 §4.4), **`GET /healthcheck`** (stock, P1 A.2) — the watcher's poll targets.
- **`smtp_skipped_reason`** open string enum (P3 §8): `no_eligible_node|audit_red|provider_mx_routed|warmup_cap` — P4 adds `rate_limited`.
- **`smtp_fail_fast`** (gem config, P2 A.1) — already config-exposed; relevant to per-provider pacing but its scoring effect is neutral (P2 §6.7).
- **`provider_routing` map** (P1 §7.2, P3 §5.1) — Gmail/O365/Yahoo → `:mx` (never SMTP'd), so the rate limiter's live work is honest-corporate `:smtp` only.

### A.2 SVG panels that govern Phase 4
- **§④ SMTP hardening:** "per-provider rate+jitter" (§4), "rotate HELO/verifier" (§4.5, closes P3 GAP-22).
- **§⑤ provider trust:** "trust their 'no' a little, their 'yes' never" — big providers `:mx`-routed, so no SMTP rate budget needed for them (§4.1).
- **§⑦ node lifecycle:** provision → active (warmup) → **quarantine (block/audit red)** → re-audit clean → active. P4 adds the block-rate trigger + hysteretic/canary recovery to this exact machine (§5.4).
- **§⑧ topology:** 1-VPS-N-IPs vs N-VPS-1-IP; "add VPS = +1 config line" — the topology choice is flagged (§12), not made; "+1 config line" is §7 (honest-bounded by GAP-31).

### A.3 Industry ground truth Phase 4 depends on (not truemail-specific)
- **Providers rate/reputation-judge per IP:** connection concurrency + rate to a given provider from a given IP drives greylisting/tarpitting/blocking — the basis for per-(node×provider) limiting (§4.1).
- **Machine-regular connection timing is a fingerprint:** evenly-spaced connections look automated; jitter is standard reputation practice (§4.2).
- **Statistical process control:** a rate over a small sample is dominated by noise; control loops on lagging noisy signals need hysteresis + sample gates or they oscillate — the basis for §5.3's guards.
- **Rendezvous (HRW) hashing:** maps keys to the current member set with minimal reassignment on membership change — the basis for sticky-by-domain re-routing (§3.2).

---

## Appendix B — Phase-4 proof matrix (the acceptance intent, not test code)

The minimum scenarios Phase 4 must demonstrate on a **2-node pool** (some are simulatable against P2/P3 mock-backed nodes; block-rate/quarantine behaviors need a controllable verdict stream). Design intent; no code.

| # | Scenario | Expected outcome | Proves |
|---|---|---|---|
| 1 | Add node #2 to the config-list; provision via §6 script | node #2 reaches `active`; **zero brain code diff** | 1→N config-only (§7, Door 5) |
| 2 | Steady traffic across many domains, 2 active nodes | each domain sticks to one node; ~balanced split | sticky-by-domain + HRW balance (§3.2) |
| 3 | Quarantine node #1 (force audit-red), node #2 active | node #1's domains rehash to node #2; real verdicts continue; no `invalid` | re-route on quarantine (§3.2/§5.6) — the headline demo |
| 4 | Node #1 verdict stream shows high `smtp_blocked` rate, ≥ min_samples, pool otherwise healthy | node #1 auto-quarantined by the watcher | block-rate quarantine, guarded (§5.3/§5.4) |
| 5 | Node #1 shows 100% blocked but only 2 samples | **NOT** quarantined (sample gate) | GAP-27 guard (§5.3) |
| 6 | **All** active nodes cross block threshold at once | **NO** auto-quarantine; **critical fleet-wide alert**; membership held | GAP-28 cascade brake (§5.5, OQ-P4-9) |
| 7 | Block-quarantined node #1 goes audit-green | enters `recovering`; metered canary trickle; clean → active (warmup low) | recovery not one-way (§5.4/GAP-30) |
| 8 | Node #1 at per-provider concurrency cap; check for its sticky domain arrives | falls back to least-inflight → node #2 (if it has headroom) | rate cap + least-inflight fallback (§3.4/§4) |
| 9 | **Every** eligible node at per-provider cap for provider P | check degrades to cheap, `smtp_skipped_reason:"rate_limited"`, non-cacheable, not `invalid`, not queued | §4.6 degrade rule (Door 4 intact) |
| 10 | Restart the brain mid-window with budgets partially spent | rate budgets resume from **durable** spend (no refill); in-flight resets to **zero** (correct) | §4.4 durable / §4.3 ephemeral (GAP-26) |
| 11 | Jitter enabled | session starts to a provider are randomly spaced, not periodic | §4.2 anti-fingerprint |
| 12 | Dirty node #1 vs clean node #2 block-rate | node #1's `verifier_ip` shows high block-rate; node #2 unaffected | failure isolation via per-node `verifier_ip` (§5.6) |
| 13 | Metrics + alerts | block-rate/unknown-rate/latency/cache-hit emitted; quarantine/block-spike/audit-red alerts fire | observability (§8) |
| 14 | Node #1 `/health` unreachable/wedged × M cycles | node #1 quarantined on liveness | liveness trigger (§5.4) |

---

## Appendix C — Review log (what the adversarial critique pass changed)

This spec was drafted, then critiqued by an adversarial **architecture + backend-architect + senior-devops** pass against the frozen P1–P3 contracts and distributed-systems reality, before finalizing (mirroring P1 Appendix B, P2 Appendix C, P3 Appendix C). The pass was run **inline** (consistent with the owner's "don't run long sessions, split the work" guidance and the P3 process note — inline was faster and effective). Findings folded in:

**Blocker (resolved):**
- **PB1 — Block-rate query would count cache hits and cheap-degrades.** The health-watcher reads block-rate from the LOG (§5.2), but the LOG includes `from_cache=true` rows (ran no SMTP on any node) and cheap-degraded rows (`verifier_ip=null`, SMTP skipped). Counting those would attribute verdicts to a node that never produced them — quarantining a node for cache hits or degrades it didn't cause. **Resolved:** §5.2 `LOCKED` — the block-rate query counts **only** `from_cache=false ∧ verifier_ip=<node> ∧ SMTP-actually-ran` rows; cache hits and degrades are excluded (the same `from_cache`-excluded discipline P1 §3.2.1 locked, applied to the control loop).

**Majors (all resolved):**
- **PM1 — Node-agnostic cache looks like a pool bug but is correct.** The cache key `(email_hash, config_fingerprint)` excludes verifier identity (P1 §3.2.5), so node #1's verdict serves a node #2-routed request. Left unstated, a future maintainer would "fix" it by keying per-node — shattering the cache N× and multiplying provider load (the exact harm §3.1 fights). **Resolved:** new §3.6 `LOCKED` — the cache stays node-agnostic because a settled verdict is a fact about the *address*, not the IP; the node/transient-sensitive verdicts are exactly the ones already non-cacheable (P2 OQ-P2-6/P3 PB1/§4.6). The settled-vs-provisional predicate keeps it correct at any N.
- **PM2 — Least-inflight denominator ambiguous.** "Fewest in-flight sessions" didn't say per-node-global vs per-(node,provider); ranking on the wrong denominator misroutes around the wrong bottleneck (a node busy with provider A still has room for provider B). **Resolved:** §3.4 `LOCKED` — the fallback ranks on **per-(node, target-provider)** in-flight (the constraint being relieved); node-global is a tie-break/metric only.
- **PM3 — Recovery could resume full warmup allowance on a cooled IP.** GAP-30's canary recovery said "re-enter warmup from a low cap" but didn't pin that the **durable** warmup counter (P3 §5.2) resets — a briefly-recovered block-quarantined node could jump straight back to full volume into the providers that just blocked it, re-burning it. **Resolved:** §5.4 `LOCKED` — recovery **resets the durable warmup counter to a low start**; a recovered node re-warms gradually like a fresh IP ("start over gently," not "resume where you left off").
- **PM4 — "Sticky-by-domain" over-scoped.** The draft read as if every domain routes to a node; but `:mx` providers (Gmail/O365/Yahoo) and cheap-decided verdicts never reach the router at all. **Resolved:** §3.2 `LOCKED` — stickiness governs **only `:smtp`-eligible (honest-corporate) checks**; the big providers resolve to `provider_no_smtp` before routing. Sharpens what "route every domain" means (most domains never get routed).

**Minors folded in:** the in-flight (ephemeral) vs budget (durable) asymmetry given its own gap-flag (GAP-26) so it's never conflated; the fleet-wide brake (§5.5) explicitly paired with its critical alert (§8.2) so the watcher's deliberate *non-action* on a systemic spike is always human-visible; `smtp_skipped_reason:"rate_limited"` confirmed additive to the P3 open enum (no new key, §9).

**What the review affirmed as already-right (preserved):** sticky-by-domain + per-provider rate limiting as **one mechanism in two parts** (§3.1/§4.1) — the phase's spine; the durable-budget/ephemeral-in-flight split mirroring P3's warmup-counter discipline (§4.4, PM4-P3); the fleet-wide cascade brake as the auto-heal→auto-outage guard (§5.5/GAP-28); the sample-size gate on a noisy statistical signal (§5.3/GAP-27); the canary recovery path so block-rate quarantine isn't a one-way door (§5.4/GAP-30); degrade-never-queue-never-`invalid` on rate saturation (§4.6, harden-toward-`unknown`); node #2 = +1 config line as the Door-5 proof, honestly bounded by the procurement wall (§7/GAP-31); zero new `status`/`reason`/`recommendation` values (Door 4 intact); the deployed fork unchanged + gem pinned (Door 5); scope discipline (queue/fallback/flywheel/multi-brain/topology all named as seams, not built).

**Post-review status:** all findings (PB1, PM1–PM4) resolved in-line. **Fourteen OQ-P4 items (§11) remain for owner sign-off** — none blocking the design, each carrying a recommendation. The items most worth the owner's attention: **OQ-P4-9/GAP-28** (fleet-wide cascade brake — the phase's most important safety guard), **OQ-P4-7/GAP-30** (recovery not a one-way door), **OQ-P4-12/GAP-29** (domain-sticky only partially concentrates shared-platform providers), and **OQ-P4-3/§4.4** (durable rate-budget state).

---

## Appendix D — Owner decision log (Phase 4 sign-off)

All 14 **OQ-P4** questions signed off by the owner. **All recommendations accepted**; the four flagged items (OQ-P4-9, 7, 12, 3) **affirmed with reasoning**; a **carry-forward directive** added on the GAP-24 dependency. Recorded here so the reasoning survives the conversation, mirroring P1 Appendix C, P2/P3 Appendix D.

### Affirmed with reasoning — OQ-P4-9 / GAP-28: the fleet-wide brake is the load-bearing guard
- **Owner ruling:** approve alert-don't-quarantine on a fleet-wide spike. *"This is the finding that justifies the whole spec"* — auto-heal becomes auto-outage: quarantining a blocked node rehashes its load onto peers, whose block-rate/in-flight then rise, cascading until the pool empties, on a **shared cause that was never any single IP's fault** (subnet blocklist, provider policy change). Without the brake, a self-healing pool **reliably self-destructs the first time a whole subnet gets listed.**
- **Added directive (owner):** **when in doubt whether a spike is node-local or fleet-wide, err toward ALERT.** A missed auto-quarantine costs some bad verdicts; a wrongful cascade costs the whole pool. The asymmetry favors caution.
- **Applied:** §5.5 (the brake), §8.2 (the paired critical alert — the watcher's deliberate non-action is only safe if a human is loudly alerted). **Treated as the single most important line in the phase.**

### Affirmed — OQ-P4-7 / GAP-30: canary recovery is non-negotiable
- **Owner ruling:** approve the `recovering` sub-state + metered canary trickle. *"It's the only thing that makes block-quarantine reversible."* A quarantined node gets no traffic → no samples → can't prove it recovered — a **one-way door to permanent quarantine** without the canary path. Owner named this *"the second-order thinking that separates a real distributed-systems spec from a plausible-looking one."*
- **Applied:** §5.4 (`recovering` sub-state, audit-green+liveness → metered canary → clean over `RECOVER_WINDOW` → active; re-block → quarantined; recovery resets the durable warmup counter, PM3).

### Affirmed — OQ-P4-12 / GAP-29: keep domain-sticky (don't chase provider concentration into a hot node)
- **Owner ruling:** approve leaving it domain-sticky. Two corporates on the same shared platform (both on `pphosted.com`) can hash to different nodes, only **partially** concentrating that provider — the spec is **honest that this is reduced, not eliminated.** Provider-sticky would fix it but **creates hot nodes** (a whole provider on one box = single point of failure). For this fleet size, domain-sticky is the right trade. **Revisit only if real data shows a shared platform is the block source.**
- **Applied:** §3.2/GAP-29/OQ-P4-12 (domain-sticky retained; provider-sticky flagged as a `SEAM →` trade-off, revisit-on-data).

### Affirmed — OQ-P4-3 / §4.4: durable rate-budget state in Supabase
- **Owner ruling:** approve durable-in-Supabase, **same reasoning as Phase 3's warmup counter** — a rate limiter that **refills its allowance on every brain restart is a reputation hazard** (a freshly-restarted brain would let the whole pool sprint at every provider at once, exactly post-deploy when it's least observed).
- **Applied:** §4.4 (durable budget, token-bucket with durable `last_refill_at`, restart-safe); §4.3 (in-flight stays ephemeral — the deliberate opposite decision, GAP-26).

### Accepted as-is (batch):
OQ-P4-1 (HRW sticky affinity), 2 (`other`-bucket by registrable MX domain), 4 (per-node single audited identity; multi-identity rotation a dial), 5 (rate limits global-with-override), 6 (block numerator = `smtp_blocked` primary, greylist not counted), 8 (single-brain assumption; multi-brain out of scope), 10 (script automates 2/4/5/6/7, manual-fallback on spin+PTR), 11 (metrics both-layered, LOG authoritative for control), 13 (watcher cadence/window dials), 14 (in-process watcher loop) — recommendations sound, accepted without change.

### Carry-forward directive — GAP-24 is now the load-bearing dependency of the whole pool
- **Owner ruling:** Phase 4 makes P3's hardest invariant (**GAP-24: `verifier_ip` == egress == PTR == audit-IP**) load-bearing for the entire pool — per-node block-rate is `GROUP BY verifier_ip` (§5.6), so **if that invariant is even slightly wrong, one dirty box smears across the fleet's stats and failure isolation silently fails.** The GAP-24 four-way cross-check is **not just a Phase-3 nicety — it's what makes Phase 4's failure-isolation guarantee real. Don't let it rot.**
- **Applied:** §5.6 already states this dependency explicitly; recorded here as a standing directive so the P3→P4 chain is never weakened. See [[truemail-audit-green-not-ready]].

### Affirmed as correct (owner named these as the spec's strongest points, preserved verbatim):
- **GAP-28 (auto-heal→auto-outage)** — *"the catch that justifies the whole spec… subtle enough that most designs ship without it."* The cascade + the fleet-wide brake. Load-bearing.
- **GAP-30 corollary (block-rate quarantine is a one-way door)** — the canary sub-state; *"second-order thinking that separates a real distributed-systems spec from a plausible-looking one."*
- **PB1 (block-rate excludes `from_cache` + cheap-degrades)** — otherwise you quarantine a node for verdicts it never produced; reuses the P1 `from_cache`-excluded discipline on the control loop. *"Correct and consistent."*
- **PM1 (node-agnostic cache is correct, not a bug)** — a mailbox exists regardless of which IP saw the 250; per-node caching would shatter the cache by pool size (the exact multiplication the phase fights); discriminator = settled-vs-transient, not which-node.
- **PM3 (recovery resets the warmup counter)** — a block-quarantined node has a cooling IP; re-warm from low, don't re-burn it.

### Cross-references to standing constraints (owner-ruled earlier, honored here, not re-opened):
- **Door 5 / never-fork-the-gem** ([[truemail-never-fork-engine-door5]]) — P4 is the phase Door 5 was written for; node #2 = +1 config line, gem unchanged. No patch contemplated.
- **GAP-13 spike before provisioning** ([[truemail-gap13-spike-before-real-box]]) — remains a pre-provisioning gate for every node the §6 script stands up; sticky-by-domain sets up the P5 domain-batched session reuse that makes it matter more.
- **`552 mailbox_full → valid`** ([[truemail-552-mailbox-full-valid]]) — P4 scoring is unchanged; this carries through untouched.

**Net (owner):** *"approve Phase 4. It's proportional, it caught the cascade failure that would have sunk a naive pool, and it made its recovery path reversible. Treat the fleet-wide brake as the single most important line in the phase."*

---

*End of Phase 4 (Resilience & pool) Design Specification — **v1.0, APPROVED**. One adversarial review pass folded in (Appendix C: PB1, PM1–PM4 resolved); all 14 OQ-P4 decisions signed off (Appendix D) — the four flagged items (fleet-wide brake, canary recovery, domain-sticky, durable budget) affirmed with reasoning. Additive-only against the frozen P1–P3 contracts (one new `signals` enum value `rate_limited`; one new operational table `rate_budgets`; two new `Strategy` implementations behind the unchanged interface). Sticky-by-domain (§3) + the block-rate quarantine state machine (§5) are the phase's spine; **the fleet-wide cascade brake (GAP-28/§5.5) is the single most important line in the phase** — auto-heal must not become auto-outage. **Standing carry-forward directive: GAP-24 (`verifier_ip` == egress == PTR == audit-IP) is the load-bearing dependency of the pool's whole failure-isolation guarantee — per-node block-rate is `GROUP BY verifier_ip`; don't let it rot.** Node #2 = +1 config line proves Door 5 at N>1. Ready to build.*








