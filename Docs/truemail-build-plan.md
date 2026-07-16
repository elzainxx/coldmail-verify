# Truemail Verification System — Build Plan

A phased, decision-locked plan to build the architecture from our brainstorming.
Order is deliberate: **the cheapest, least-reversible things first.**

---

## 0. Decisions already locked (don't re-litigate)

| Decision | Choice |
|---|---|
| Engine | truemail **Ruby gem**, pinned — never forked |
| What you fork | **truemail-rack** only (the HTTP server) |
| Control-plane language | **Go** |
| Database | **Supabase** (managed Postgres) |
| Front door | **one authenticated, versioned API** (no UI, no CSV in v1) |
| Box model | **dumb, stateless, disposable**; brain holds all state |
| Coordination | **brain-only**; boxes never talk to each other |
| Layer execution | **policy-driven & configurable**; default = cheapest-sufficient |
| Limit handling | blocked/limited/broken → **`unknown`, never invalid** |
| Cost control | dropped for v1 — **but health-based auto-quarantine kept** (IP protection) |
| Topology | brain on good VPS (port 25 closed) + cheap boxes (port 25 open) |
| Scaling | **1..N by config**, same code |

---

## The one rule that governs everything

> Anything that becomes **historical data**, or that **another system codes against**, is a
> one-way door — get it right now. Everything else is a dial you can turn later.

**The five one-way doors:**
1. `/verify` response shape → **versioned, additive-only**
2. The **LOG** → log everything, from the very first check
3. **Email hashing** → decide the scheme once, never change it
4. **status / reason vocabulary** → additive-only (new values fine, renames never)
5. **Never fork the engine**; brain talks to a **node LIST** (even of length 1)

---

## PHASE 1 — Foundations (no port 25, no real IP needed)

Goal: everything that doesn't need SMTP, built and tested on any machine.

### 1.1 Lock the contracts (before any feature code)
- [ ] Write the **`/verify` response schema** (status, reason, confidence, recommendation, flags, signals, meta, raw) — version it `/v1/`
- [ ] Write the **LOG record schema** (= response + verdict + provenance: verifier_ip, engine_version, config_fingerprint, from_cache, raw_truemail_json)
- [ ] Decide the **email-hashing scheme** + normalization rules (dots, plus-aliases) — write it down, freeze it
- [ ] Define the **status + reason vocabularies** as a fixed enum list

### 1.2 Supabase setup
- [ ] Create project; tables: `verifications` (the log), `cache`, `catchall_domains`, `disposable_domains`, `nodes`, `policies`
- [ ] `send_outcomes` + join table: create the empty schema now (fill later)
- [ ] Note retention plan for the log (biggest data grower)

### 1.3 Fork & run truemail-rack unchanged
- [ ] Fork `truemail-rack`, pin `truemail` gem as dependency
- [ ] Get the stock container running locally, hit `/validate` — confirm baseline works

### 1.4 Brain skeleton (Go)
- [ ] API front door with token auth, `/v1/verify` endpoint (stub)
- [ ] Node registry read from **config list** (one mock entry)
- [ ] Router with **pluggable strategy** interface (start: random) — never hardcode a single endpoint
- [ ] Supabase client wired

### 1.5 Cheap layers live (these need nothing else)
- [ ] syntax / regex · typo · role · free-vs-corp
- [ ] MX / DNS · null-MX (RFC 7505)
- [ ] disposable check (list in Supabase, not env) · spam-trap list
- [ ] provider classification (gmail/o365/yahoo/self/other)
- [ ] **LOG every result** — even the cheap ones. The log is the point.

### 1.6 Policy engine (v1 = one policy)
- [ ] Signals-plus-policy model: layers emit `{signal, result, severity}`; policy decides gate vs flag vs stop-early
- [ ] Ship **one policy** (cold-email, cheapest-sufficient), structured so more slot in

**Phase 1 done when:** you can POST an address and get a scored, logged verdict using
cheap layers only — no port 25 anywhere.

---

## PHASE 2 — SMTP against mocks (still no real IP)

Goal: build and harden the whole SMTP path against fakes, so the real box is a config flip.

- [ ] Wire truemail's `smtp_mock` + `dns_mock` (its own test tooling)
- [ ] Build the **wrapper** on the forked rack: `/verify`, `/audit`, `/stats`, `/catchall/:domain`, `/config/reload`
- [ ] **Catch-all probe** (2nd RCPT, random fake address) — the biggest gap
- [ ] **SMTP hardening** against mocks:
  - [ ] classify 5xx (address-fault vs your-fault)
  - [ ] greylist 450 → `unknown` (retry logic stubbed until queue)
  - [ ] capture full `smtp_debug` transcript into the log
  - [ ] session-reuse design (many RCPT / one connection)
  - [ ] timeout tuning → timeout = `unknown`
- [ ] **Scoring** maps signals → verdict per the harden-toward-`unknown` principle
- [ ] Prove: mocked "real" address → valid; mocked fake → invalid; mocked block → `unknown`

**Phase 2 done when:** the entire pipeline works end-to-end on mocks, and switching to a
real server is literally one config value.

---

## PHASE 3 — First real box (port 25 enters)

Goal: one clean, audited SMTP box in the pool.

### 3.1 Provision (the real gating work)
- [ ] Get a cheap VPS that **genuinely allows outbound port 25** AND **settable PTR** — vet before paying
- [ ] Clean IP (no spam history), verifier domain + SPF, PTR matching verifier domain
- [ ] Deploy the forked rack + wrapper container
- [ ] Put box on **private network**; brain-only access, token auth

### 3.2 The audit gate
- [ ] Expose `/audit` (wraps `host_audit`) — the single most valuable added endpoint
- [ ] **Rule:** node stays ineligible for SMTP until `/audit` is green
- [ ] Schedule re-audit; auto-quarantine on red

### 3.3 Go live selectively
- [ ] `validation_type_for`: honest corporate domains → `:smtp`; **Gmail/O365/Yahoo → `:mx`**
- [ ] Warm up volume gradually (don't 0→full)
- [ ] Flip mock SMTP → real box in config
- [ ] Confirm real verdicts flowing and logging with `verifier_ip` tagged

**Phase 3 done when:** one audited box returns real SMTP verdicts on honest domains, and
big providers are safely routed to `:mx`.

---

## PHASE 4 — Resilience & pool

Goal: survive blocks, scale by config.

- [ ] **Health watcher**: poll `/audit` + `/health` + block-rate per node → auto-quarantine
- [ ] **Router strategies**: add sticky-by-domain (default) + least-inflight fallback
- [ ] **Per-provider rate limit + jitter** (reputation protection)
- [ ] **Provisioning script**: spin → install → PTR → audit → register → warm up (repeatable)
- [ ] Add node #2 = **+1 config line**; prove 1→N is config-only
- [ ] **Observability**: metrics (block-rate, unknown-rate, latency, cache-hit) + alerts (quarantine, block spike, audit red)

**Phase 4 done when:** a blocked box auto-quarantines, traffic re-routes, and adding a box
is a config edit.

---

## PHASE 5 — The queue (when volume forces it)

Goal: unblock the things synchronous v1 can't do.

- [ ] Async job queue behind the router (same registry, same strategies)
- [ ] **Greylist retry / backoff** (needs the queue — real 450 handling)
- [ ] **Bulk** `/verify/bulk` can return a **job handle** (seam already designed in)
- [ ] Group work **by domain, not upload order**

**Trigger to start Phase 5:** greylist retries and per-domain pacing start mattering, or
throughput at 1M+ strains synchronous calls.

---

## PHASE 6 — Third-party fallback (rent the moat)

Goal: answer the residue you structurally can't (Gmail, catch-all) — now, before you have history.

- [ ] Integrate a fallback API (MillionVerifier or similar) as a **layer**
- [ ] Route only the **unanswerable residue** to it (not whole lists) — keeps cost low
- [ ] Log fallback answers alongside your own (tagged as external)

**Why here:** makes you *usable* on consumer/catch-all lists immediately, at low cost,
while your own data grows.

---

## PHASE 7 — The flywheel (needs your sender)

Goal: grow your own data moat; wean off the fallback.

- [ ] Sender infrastructure comes online (separate project)
- [ ] **Close the loop**: every send → bounce → joined to the verdict that allowed it
- [ ] **Classify bounces** (mailbox_not_found = invalid; mailbox_full = valid; blocked = no info)
- [ ] Recalibrate `confidence` per (reason × provider) bucket from real outcomes
- [ ] **#4 answered**: auto-promote a provider to `:mx`-only when its valid-label bounce rate crosses threshold
- [ ] **#5 answered**: measure "of valids, % that bounced" ≤ target → you now know your real accuracy
- [ ] Drop the fallback per-provider as your own history beats it

**Phase 7 is the endgame:** this is what turns a truemail wrapper into something that can
actually rival the incumbents — on the providers your data covers.

---

## Milestone summary

| Phase | Delivers | Needs |
|---|---|---|
| 1 | Scored + logged verdicts, cheap layers | nothing |
| 2 | Full SMTP path on mocks | nothing |
| 3 | Real SMTP, one audited box | port-25 VPS + PTR |
| 4 | Auto-heal + scale by config | 2nd box |
| 5 | Queue, greylist retry, bulk | volume pressure |
| 6 | Coverage on hard residue | fallback API key |
| 7 | Own data moat, real accuracy | your sender |

---

## What to protect at all costs

1. **The log** — unlogged data is gone forever. Log from check #1.
2. **The response shape** — versioned, additive-only.
3. **Email hashing** — one scheme, frozen.
4. **The node-list discipline** — never hardcode one endpoint.
5. **`unknown` as first-class** — never guess to avoid it; that's the whole game.
