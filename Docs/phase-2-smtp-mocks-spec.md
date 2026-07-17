# Truemail Verification System — Phase 2 (SMTP against mocks) Design Specification

**Status:** APPROVED (v1.0) — two adversarial review passes folded in (Appendix C); all 11 OQ-P2 signed off, one override (`552`→`valid`) (Appendix D) · **Scope:** Phase 2 only · **Type:** Planning deliverable (no code, no migrations, no provisioning, no real SMTP box)
**Sources:** `truemail-build-plan.md` (§ PHASE 2), `truemail-architecture-detailed.svg` (§④ SMTP hardening, §③ limit handling, ⑤ provider trust), the **approved** `phase-1-foundations-spec.md` (frozen contracts inherited), and upstream ground truth from the `truemail` gem, `truemail-rack`, `smtp_mock`, `dns_mock`, and `truemail-rspec` READMEs (captured in Appendix A).
**Inherits (frozen, not redesigned):** the five one-way doors from Phase 1 — `/verify` response shape, LOG schema, email hashing, `status`/`reason` vocabularies, node-list/never-fork. Phase 2 is **additive-only** against all five.

---

## 0. About this document

### 0.1 Purpose
Expand **Phase 2 — SMTP against mocks** of the build plan into a complete, buildable design specification, at the same rigor as the Phase 1 spec. Phase 2's goal, stated by the build plan:

> Build and harden the **whole SMTP path against fakes**, so switching to a real server is literally **one config value.**

This document is what an engineer reads *before* wiring the mock substrate, forking the rack wrapper, or writing the SMTP-scoring code. It fixes the endpoint contracts and the SMTP-signal semantics, and it resolves the three Phase-2 seams that Phase 1 deliberately left open.

### 0.2 Scope discipline
- **In scope:** Everything under build-plan § PHASE 2 — wiring `smtp_mock` + `dns_mock`; the forked-rack wrapper endpoints (`/verify`, `/audit`, `/stats`, `/catchall/:domain`, `/config/reload`) **as contracts**; the catch-all probe; full SMTP hardening (5xx classification, greylist→`unknown`+requeue shape, `smtp_debug` transcript capture, session reuse, timeout tuning); and scoring SMTP signals → verdict under *harden-toward-`unknown`*.
- **Out of scope:** Phases 3–7. Where a Phase 2 decision touches a later phase, this doc marks the **`SEAM →`** and stops. In particular: **the queue is NOT built** (greylist requeue is designed as a shape that slots into Phase 5, §7.4); no real IP, PTR, provisioning, or warmup (Phase 3); no health-watcher/pool/rate-limit machinery (Phase 4).
- **No implementation:** no Ruby, no Go, no Dockerfiles, no SQL. Contracts (request/response shapes, failure modes), signal semantics, and interface *shapes* only.
- **Additive-only against Phase 1:** Phase 2 adds `reason` values (already **reserved** in the Phase 1 vocab, §3.4.3 of that doc), adds `signals` keys, and adds columns/tables **only** where Phase 1 explicitly reserved them. It changes **nothing** already frozen.

### 0.3 How to read the tagging (identical to Phase 1)

| Tag | Meaning |
|---|---|
| **`LOCKED`** | A one-way door (inherited or newly crossed in P2) or an already-locked build-plan decision. Additive-only afterwards. |
| **`DIAL`** | A build-time / runtime value tunable later without breaking history or consumers. |
| **`DECISION REQUIRED`** | An open choice surfaced with a recommendation but **not** assumed. Needs sign-off before build. |
| **`SEAM →`** | A Phase 2 design point that deliberately leaves room for a later phase; noted, not built. |
| **`GAP`** | A contradiction/omission found between the build plan, the SVG, the Phase 1 spec, and/or upstream truemail reality — flagged, not silently resolved. |

> **Priority rule (governs this doc):** the **catch-all probe (§5)** and the **5xx/greylist classification (§6)** are specified in full — they most directly determine verdict correctness. Endpoint contracts (§4) are specified to Phase-1 contract discipline. Everything else is specified well enough to build and no further.

### 0.4 The one principle Phase 2 must not violate
From the SVG §④ and build-plan §0, restated because every SMTP decision below descends from it:

> **Harden toward `unknown`, not toward `valid`/`invalid`.** Blocked / greylisted / timed-out / ambiguous / your-fault → **`unknown`**, never `invalid`. `invalid` is reserved for *positive proof of non-deliverability* (address-fault). `valid` (SMTP-grade) is earned **only** by a clean `250` on a **non-catch-all** mailbox. Everything else that "looks like a yes" is at best `risky`.

This is the same boundary Phase 1 locked for the `invalid` label (§7.5 of the P1 spec); Phase 2 extends it into SMTP-space without moving it.

---

## 1. Phase 2 at a glance

### 1.1 What Phase 2 delivers (from the build plan)
> **Phase 2 done when:** the entire pipeline works end-to-end on mocks, and switching to a real server is literally one config value.

Phase 2 takes the Phase-1 brain — which can prove an address **invalid** cheaply but is honest about not being able to prove **valid** without SMTP — and builds the SMTP path that produces confident `valid`, address-fault `invalid`, and (critically) a well-classified `unknown`, **entirely against fake servers**. No real mailbox is ever contacted.

### 1.2 The three inherited Phase-1 seams this doc must resolve
Phase 1 explicitly deferred these to Phase 2 (P1 spec §6.5, §3.2.5, §9). Resolving them is a hard requirement of this spec:

| Seam | From P1 | Resolved in |
|---|---|---|
| **GAP-3 / OQ-11** — fork must expose full effective config so `config_fingerprint` covers the engine, not just brain+policy. | P1 §3.2.5 | **§4.6** (`/config` contract) |
| **GAP-4** — truemail returns SMTP status as **raw strings**, not numeric codes; the brain owns parsing into `signals.smtp_code`. | P1 §3.4.3, GAP-4 | **§6.2** (transcript parsing) |
| **GAP-10 / OQ-24** — brain-DNS vs engine-DNS can disagree once SMTP exists; reconciliation rule needed. | P1 §6.5, OQ-24 | **§6.6** (DNS reconciliation) |

### 1.3 The frozen inheritance (context, not re-litigated)
Carried verbatim from the approved Phase 1 spec — listed so this doc is self-contained. **None of these may change; Phase 2 only adds.**

- **Door 1** — `/verify` response shape is versioned & additive-only. P2 adds `signals` keys (`smtp_code`, `greylisted`, `smtp_debug_ref`, …) and populates reserved fields; it does **not** alter existing fields, types, or the cross-field invariants (P1 §3.1).
- **Door 2** — the LOG (`verifications`) is append-only; the raw engine blob lives in the `verification_raw` sidecar. P2's `smtp_debug` transcript is captured **into that existing sidecar**, not a new mutable column (§6.4).
- **Door 3** — `norm/v1 + hmac-sha256/v1` hashing, `bytea` key, global pepper (the system's most fragile artifact). P2 changes nothing here.
- **Door 4** — `status`/`reason`/`recommendation` enums are additive-only. Every SMTP `reason` P2 emits (`accepted`, `mailbox_not_found`, `mailbox_full`, `catch_all`, `accept_all`, `greylisted`, `smtp_blocked`, `smtp_timeout`, `smtp_error`, `connection_failed`) was **reserved in P1 §3.4.3** — P2 does not add a single new enum value beyond those already reserved (if it needs one, that is flagged as a new one-way-door crossing, §8).
- **Door 5** — the **truemail gem is never forked**; only **truemail-rack** is forked. Every new endpoint and behavior in §4–§6 lives in the rack wrapper or the brain — **never** in the gem. This is the single most load-bearing constraint on the entire endpoint design (§4.0).
- Limit handling: blocked/limited/broken/timeout → **`unknown`, never `invalid`**.
- Boxes are dumb, stateless, disposable; brain holds all state; boxes never talk to each other.

### 1.4 What "against mocks" actually means (two distinct substrates — see GAP-11)
The build plan says "wire truemail's `smtp_mock` + `dns_mock` (its own test tooling)." Upstream reality (Appendix A.3–A.5) is more precise, and the distinction is **load-bearing** for what Phase 2 can prove:

- **`smtp_mock` + `dns_mock`** are *separate mocktools gems* — real UDP (DNS) and TCP (SMTP) fake servers that truemail is pointed at via its own `dns:` and `smtp_port:` config. These exercise the **real SMTP/DNS code paths** end-to-end against a fake peer. **This is the Phase-2 substrate** — it proves the wire behavior, transcript capture, timeouts, and catch-all probe for real.
- **`truemail-rspec`** (`create_validator`) *stubs the `Validator` result object* — no socket is opened. It proves **brain/wrapper logic** given a known engine verdict, without a live protocol exchange.

Both are used, for different proofs (§3.4). Conflating them (as "its own test tooling" does) would let a whole class of wire-level bugs pass untested — hence **GAP-11**.

---

## 2. System context for Phase 2

Phase 1 built the brain with the box path present in *design* (router + node registry + node client) but never contacting a real engine. Phase 2 lights up that path against fakes.

```
                    ┌─────────────────────────────────────────────────────┐
   caller ──POST──▶ │  BRAIN (Go, port 25 CLOSED)  — UNCHANGED from P1     │
   /v1/verify       │  front door → cache → policy → cheap layers →        │
                    │  provider routing → router(node-list) → [SMTP call]  │
                    │  → SCORING (now SMTP-aware, §7) → LOG → cache → resp  │
                    └───────┬──────────────────────────────────┬──────────┘
                            │ reads/writes                      │ HTTP  Authorization: <node-token>
                            ▼                                   ▼  GET /  ·  GET /audit  ·  GET /config …
                    ┌───────────────┐              ┌────────────────────────────────────────┐
                    │  Supabase     │              │  NODE = forked truemail-rack + wrapper    │
                    │  (all state)  │              │  ┌──────────────────────────────────┐    │
                    │  + catchall_  │              │  │ truemail GEM (pinned, UNFORKED)   │    │
                    │    domains    │              │  │  regex→mx→mx_blacklist→SMTP       │    │
                    └───────────────┘              │  └──────────────────────────────────┘    │
                                                   │        │ dns:            │ smtp_port:       │
                                                   │        ▼                 ▼                  │
                                                   │  ┌───────────┐    ┌──────────────┐          │
                                                   │  │ dns_mock  │    │  smtp_mock   │  ← P2     │
                                                   │  │ (UDP)     │    │  (TCP)       │  substrate│
                                                   │  └───────────┘    └──────────────┘          │
                                                   └────────────────────────────────────────────┘
```

**Phase 2 truth (the config-flip goal):** the *only* things that change between "mock" and "real box" are the node descriptor's `base_url` (points at a mock-backed rack vs a real one) and the engine's `dns:` / `smtp_port:` pointing at `dns_mock`/`smtp_mock` vs the real resolver/port 25. **Everything the brain does — routing, parsing, classification, scoring, logging — is identical against mock and real.** That identity is the deliverable; §3.5 states how it is proven.

**What is genuinely new in Phase 2, by module:**
- **Node/rack:** the fork gains the wrapper endpoints (§4). The gem is untouched.
- **Brain `internal/node`:** the node client, dormant in P1, now parses live `smtp_debug` transcripts (§6.2) and calls `/audit`, `/config`, `/catchall`.
- **Brain `internal/verdict` (scoring):** gains the SMTP signal→verdict mapping (§7) — the largest new brain surface.
- **Supabase:** no schema change beyond what P1 reserved. `catchall_domains` (created empty in P1) is now **written** by the probe (§5.6).

---

## 3. The mock substrate (`smtp_mock` + `dns_mock`)

This section specifies *how the fakes are wired* and *what each is responsible for proving*. It is design, not code — but it names the exact substrate options that make each hardening path (§5–§6) testable, so the spec and the eventual test matrix cannot drift.

### 3.1 Why a wire-level mock at all (not just stubbed results) `LOCKED` (intent)
The whole point of Phase 2 is that **the SMTP code path is real** and only the *peer* is fake. A stubbed `Validator` result (truemail-rspec) proves the brain's scoring given a verdict, but it never opens a socket — so it cannot prove transcript capture, timeout behavior, session reuse, or the two-RCPT catch-all probe. Those are exactly the risky parts. Therefore the primary substrate is the **socket-level** `smtp_mock`/`dns_mock`. `truemail-rspec` is a secondary substrate for brain-logic unit tests (§3.4).

### 3.2 `dns_mock` — the DNS substrate
- **What it is (Appendix A.4):** a lightweight UDP DNS mock server; can mock `A`, `AAAA`, `CNAME`, `MX`, `NS`, `PTR`, `SOA`, `SRV`, `TXT`; auto-punycodes hostnames; dynamic or fixed port.
- **How truemail consumes it:** truemail's `config.dns = ['127.0.0.1:<dns_mock_port>']` points the engine's resolver at the mock (Appendix A.1, `dns` option). **`LOCKED`** — this is the only wiring point; no gem change.
- **What it must mock for Phase 2 (`DIAL` on exact fixtures, `LOCKED` on the capability list):**
  - Domains with **valid MX** → to reach the SMTP layer.
  - **Null-MX** (`.:0` per Appendix A.4) → to prove null-MX detection still gates *before* SMTP (P1 §6.6 path unchanged).
  - **No MX / NXDOMAIN** → to prove `no_mx`/`invalid_domain` gate before any SMTP.
  - **`PTR` records** → needed by the `/audit` path (§4.2), which checks PTR/DNS alignment.
  - **Slow responses** are a `smtp_mock` concern, not DNS (dns_mock has no delay knob); DNS-timeout→`unknown` is proven by pointing at a dead port (§6.6, GAP-10 test).
- **`GAP-10` reconciliation substrate note:** because the brain resolves DNS itself (cheap path) *and* the engine resolves via `dns_mock`, the test harness can deliberately feed the **brain's** resolver and **dns_mock** *divergent* MX to prove the reconciliation rule (§6.6) actually fires and logs. This is the only way GAP-10 gets tested before a real box exists.

### 3.3 `smtp_mock` — the SMTP substrate
- **What it is (Appendix A.3):** a Ruby wrapper over the Go `smtpmock` server — a real TCP SMTP server whose behavior is fully configurable per session. truemail is pointed at it via `config.smtp_port = <smtp_mock_port>` (and `dns_mock` MX resolving to `127.0.0.1`).
- **The substrate options that make Phase-2 hardening testable** (these map 1:1 to the hardening paths — this mapping is the spec's guarantee that every §6 claim is provable):

| `smtp_mock` option (Appendix A.3) | Proves (spec section) |
|---|---|
| `not_registered_emails: [...]` | `550`-class no-such-user → `mailbox_not_found` / `invalid` (§6.1) |
| `blacklisted_rcptto_emails: [...]` | policy/your-fault reject → `smtp_blocked` / **`unknown`** not invalid (§6.1) |
| `multiple_rcptto: true` | **the catch-all probe**: two RCPT TO in one session (§5) — *the single most important substrate switch* |
| `response_delay_rcptto: N`, `response_delay_*` | timeout tuning → `smtp_timeout` / **`unknown`** (§6.5) |
| `msg_rcptto_*` custom messages | craft exact 5xx/4xx bodies to test 5xx classification + `smtp_error_body_pattern` (§6.1–6.2) |
| `session_timeout`, `fail_fast` | session-reuse and fail-fast-on-bulk behavior (§6.3, §6.7) |
| default `250` accept-all | catch-all domain (accepts every RCPT) → `catch_all`/`accept_all` → **`risky`** (§5) |

- **Greylisting substrate note (`GAP-12`):** `smtp_mock` has **no first-class "greylist / 450 then accept on retry" mode**. A `4xx` transient can be simulated via a custom `msg_rcptto_*` returning a `450`, but the *stateful* "reject once, accept later" greylist dance cannot be faithfully reproduced by the stock mock. **Handling:** Phase 2 tests greylist **detection + classification → `unknown` + requeue-decision** (the brain-side behavior), and explicitly **defers the retry-then-accept round-trip** to Phase 5 (when the queue exists) or to a real greylisting box. Flagged so "greylist handling is done" is not overclaimed (§6.5, acceptance §11).
- **Accept-all-by-default substrate note (`GAP-14` — affects the `valid` path):** `smtp_mock` **250-accepts every RCPT except addresses it is explicitly told to reject** (`not_registered_emails` / `blacklisted_rcptto_emails`); it has **no "reject all unknown recipients" (allowlist) mode**. So by construction it behaves like a **catch-all** server. This has a sharp consequence for the **only path to `valid`** (§5.5 row 4 = target 250 + *random* probe **rejected**): a **random** probe address is, by definition, not pre-listed, so against stock `smtp_mock` it is **always accepted** → every domain looks catch-all → **`valid` is unreachable** unless the test **fixes the probe address** (OQ-P2-3 seeded/fixed `probe_local_part`) and pre-lists *that* value in `not_registered_emails`. **Handling:** Phase 2 proves the `valid` path with a **seeded probe** so the mock can reject exactly it; but this is acknowledged as a **test artifact** — `smtp_mock` cannot faithfully model a *real* non-catch-all server that rejects *arbitrary* unknowns. Faithful non-catch-all behavior is a real-box property (P3). Flagged so the `valid`-path proof is not mistaken for proof against a realistic non-catch-all peer. Ties to OQ-P2-3, GAP-13, and the §3.5 honest-limit note.

### 3.4 Two substrates, two proof scopes (resolves GAP-11) `LOCKED` (intent)

| Substrate | Opens a socket? | Proves | Used for |
|---|---|---|---|
| `smtp_mock` + `dns_mock` | **Yes** (real TCP/UDP) | wire behavior: transcript capture, catch-all 2nd RCPT, timeouts, 5xx bodies, session reuse | the **end-to-end** Phase-2 proof (§3.5) |
| `truemail-rspec` `create_validator` / `create_auditor` | No (stubs the result) | brain scoring & wrapper logic given a known engine result/audit | fast unit tests of §7 scoring + §4 wrapper handlers |

Both are legitimate; the spec requires the **end-to-end proof to use the socket-level substrate** so nothing wire-level is asserted only against a stub.

### 3.5 The end-to-end proof obligation (build-plan "Prove:" line) `LOCKED`
Build plan: *"Prove: mocked 'real' address → valid; mocked fake → invalid; mocked block → `unknown`."* This spec expands that into the minimum proof matrix Phase 2 must satisfy against the **socket-level substrate** (full matrix in §10):

1. `smtp_mock` accepts (clean `250`, non-catch-all) → **`valid` / `accepted`**, high confidence.
2. `smtp_mock` `not_registered_emails` (`550` no-such-user) → **`invalid` / `mailbox_not_found`**.
3. `smtp_mock` `blacklisted_rcptto`/blocked (`5xx` your-fault) → **`unknown` / `smtp_blocked`** (NOT invalid).
4. `smtp_mock` accept-all (250 to a random fake local-part) → **`risky` / `catch_all`** (NOT valid).
5. `smtp_mock` `response_delay > timeout` → **`unknown` / `smtp_timeout`**.
6. **Config-flip proof:** the same brain code, re-pointed from mock `base_url`/ports to a (would-be) real box, requires **zero code change** — only config. Demonstrated by a config-only diff.

> **Honest limit of the mock proof (`LOCKED` framing).** Mocks prove **code-path identity** — that the same brain/wrapper code drives mock and real without modification, and that every branch (§Appendix B matrix) is exercised. They do **not** prove **behavioral identity** with real providers: real Gmail/corporate servers greylist, tarpit, lie, and rate-limit in ways `smtp_mock` only approximates. Phase 2's claim is precisely "the pipeline is *built and correct against defined inputs*, and the switch is config-only" — **not** "real-world accuracy is proven." Real-world behavior is what Phase 3 (one real box) and Phase 7 (bounce calibration) measure. This distinction is stated so "done on mocks" is never mistaken for "done."

---

## 4. The forked truemail-rack wrapper — endpoint contracts

### 4.0 Wrapper design law (Door 5) `LOCKED`
Every endpoint below lives in the **forked truemail-rack wrapper**, never in the gem. The wrapper is a thin HTTP+serialization layer around the pinned gem:
- It **calls** `Truemail.validate` / `Truemail.host_audit` (gem public API) and **serializes** the result — it never patches validation logic.
- Behavior the gem doesn't offer (catch-all 2nd RCPT, full-config exposure, richer serialization) is added **in the wrapper**, using the gem's own primitives (e.g., re-invoking validation for a probe address) — not by editing the gem.
- **Enforcement:** the pinned gem version is asserted unmodified in CI (P1 §3.5.2). If a wrapper endpoint *cannot* be built without gem changes, that is a **`GAP` / one-way-door escalation**, not a silent fork — flagged, not worked around.

**Node auth (inherited, `LOCKED`):** all endpoints require `Authorization: <node-token>` (raw token matched against the rack's `ACCESS_TOKENS`, per Appendix A.2 — **not** `Bearer`). This is the *box* token (brain→box), distinct from the front-door caller token (P1 §3.5.4). The brain is the **only** client of these endpoints.

**Contract discipline (inherited from P1 Door 1):** each endpoint is specified as **purpose · request · response shape · failure modes**. Wrapper responses are **additive-only** in the same way the `/verify` contract is — new fields may appear, existing ones never change type/meaning. The wrapper carries its own `wrapper_api_version` so the brain can assert what it parsed against.

> **`DECISION REQUIRED` (OQ-P2-1) — wrapper endpoint namespacing.** The build plan lists bare paths (`/verify`, `/audit`, …). The stock rack already serves `GET /` and `GET /version`, `GET /healthcheck` (Appendix A.2). **Recommendation:** namespace all wrapper additions under `/v1/` on the box too (`/v1/verify`, `/v1/audit`, …) to mirror the brain's versioning discipline and avoid colliding with the stock root, keeping stock endpoints intact for baseline health. The brain↔box contract is then independently versionable from the caller↔brain contract.

> **`GAP-1` (inherited, now actioned).** Build-plan §1.3 said "hit `/validate`"; stock rack has **no `/validate`** — validation is `GET /?email=` (Appendix A.2). Phase 2 introduces the fork's own **`/v1/verify`** (POST, richer than stock root) as the brain's real target; the stock `GET /` remains for baseline. The node client targets the fork's `/v1/verify` from Phase 2 on.

---

### 4.1 `POST /v1/verify` — validate one address (the workhorse)

- **Purpose:** run the gem's validation for one address at the wrapper-configured validation level, and return the **full engine result** (not the lean stock subset), including the parsed-ready `smtp_debug` transcript and the catch-all probe outcome (§5).
- **Why POST (vs stock `GET /?email=`):** the probe + richer response + avoiding the address in query logs (it's PII-adjacent even pre-hash) justify a POST body. Stock `GET /` is preserved but not used by the brain.

**Request:**
```jsonc
POST /v1/verify
Authorization: <node-token>
{
  "email": "<string, required>",
  "validation_type": "regex|mx|mx_blacklist|smtp",   // optional; else wrapper/engine default
  "probe_catchall": true,                              // optional, default true for :smtp (see §5)
  "request_id": "<uuid, optional>"                     // brain-supplied, echoed for trace correlation
}
```

**Response (200) — additive superset of the stock rack JSON (Appendix A.2):**
```jsonc
{
  "wrapper_api_version": "v1",
  "email": "admin@bestweb.com.ua",
  "validation_type": "smtp",
  "success": true,                    // truemail's own top-level boolean (engine_success in the LOG)
  "errors": null,                     // truemail's {layer => message} or null
  "smtp_debug": [ ... ],              // FULL transcript array (see §6.2 shape) or null on clean success
  "catchall": {                       // ← WRAPPER-ADDED (§5). Absent if probe not run.
    "probed": true,
    "probe_local_part": "a1b2c3d4e5f6...@",   // the random fake used
    "accepted": false,                // did the domain 250 the fake address?
    "conclusion": "not_catchall|catchall|inconclusive"
  },
  "mx": {                             // ← WRAPPER-ADDED: closes GAP-2 (stock JSON omits mail_servers/domain)
    "domain": "bestweb.com.ua",
    "mail_servers": ["1.2.3.4", "1.2.3.5"]
  },
  "configuration": { ... },           // stock subset — NOT authoritative for fingerprint; see /config §4.6
  "date": "2021-02-07 10:00:56 +0200" // engine local-tz string (GAP-8: brain stores UTC, keeps this in raw)
}
```

- **`LOCKED` additive fields:** `catchall` and `mx` are wrapper additions that **close GAP-2** (stock JSON drops `domain`/`mail_servers`) and surface the probe. They are additive to the stock shape; nothing stock is removed.
- **`raw` mapping (Door 2):** the brain stores this object as the `verification_raw` blob (P1 §3.2.6) — **verbatim EXCEPT the plaintext checked-address identity, which is redacted first (Door 3).** The `smtp_debug` transcript therefore lands in the LOG's existing sidecar with **no new column** (§6.4). **[AMENDED 2026-07-17 · owner-ratified · [ADR-0008](adr/ADR-0008-door3-raw-blob-address-redaction.md)]** — the draft said "entire object" verbatim, but the response echoes the target's plaintext address (the `email` field + 5xx-body echoes) and Door 3 forbids persisting the raw full email in a durable store; the brain scrubs the address (email_hash remains the join key; domain stays Door-3-clear) before persisting. Everything else stays verbatim.

**Failure modes:**

| Condition | Wrapper returns | Brain maps to |
|---|---|---|
| Missing/invalid node-token | `401` | node error → retry/route elsewhere; verdict `unknown` if unroutable |
| Malformed request body | `400` | brain bug; surfaced, not a verdict |
| Gem raises mid-validation | `200` with `success:false` + `errors` + `smtp_debug` (truemail's own error surfacing) | classified per §6 (likely `smtp_error`/`unknown`) |
| Wrapper/box internal error | `500` | node unhealthy → `SEAM →` P4 quarantine; this verdict → `unknown`/`connection_failed` |
| Timeout (no response in budget) | (no HTTP response) | brain's own client timeout → `smtp_timeout`/`unknown` (§6.5) |

> **Note — the wrapper never renders a verdict.** It returns *engine facts* (`success`, `errors`, `smtp_debug`, `catchall`, `mx`). `status`/`reason`/`confidence` are **brain-only** (Door 5: all interpretation is brain-side). This keeps the box a pure function `email → raw engine facts`.

---

### 4.2 `GET /v1/audit` — host audit (the SMTP eligibility gate) `SEAM →` P3

- **Purpose:** expose the gem's `host_audit` (Appendix A.1 / A.6) so the brain can tell whether *this box* is fit to run SMTP (IP reachable, verifier-domain DNS aligned, PTR present & correct). The build plan calls `/audit` "the single most valuable added endpoint."
- **Phase 2 role:** the **contract is built and testable now** (against `dns_mock` PTR/A fixtures), but the **enforcement rule** ("no SMTP until audit green") is a **Phase 3** activity (real IP/PTR). Phase 2 defines the shape and lets the brain *read and log* it; it does not gate mock traffic on it.

**Request:** `GET /v1/audit` · `Authorization: <node-token>` · optional `?refresh=true` to force re-run.

**Response (200):**
```jsonc
{
  "wrapper_api_version": "v1",
  "current_host_ip": "127.0.0.1",
  "warnings": {                       // empty object = green. Mirrors gem's Auditor::Result.warnings
    "dns": "A-record of verifier domain not refers to current host ip address",
    "ptr": "PTR-record does not reference to current verifier domain"
  },
  "audit_status": "green|red",        // ← WRAPPER-ADDED convenience: green iff warnings == {}
  "audited_at": "<RFC3339 UTC>"       // ← WRAPPER-ADDED
}
```

- **`audit_status`** is a wrapper convenience derived from `warnings == {}`; the brain may also compute it. **`SEAM →`** its consequence (quarantine/ineligibility) is Phase 3/4; here it is recorded to `nodes.audit_status` / `nodes.last_audit_at` (columns P1 reserved, P1 §4.5).
- **Substrate:** `dns_mock` supplies PTR + A fixtures so both green and red audits are reproducible without a real host.

**Failure modes:** `401` (bad token); `500` (audit couldn't run → treat node as `red`/ineligible, `SEAM →` P3 rule).

---

### 4.3 `GET /v1/catchall/:domain` — catch-all status for a domain

- **Purpose:** let the brain ask a box to determine (or re-determine) whether a domain is accept-all, by running the catch-all probe (§5) against a **random non-existent** local-part at that domain. The box **returns the conclusion only**; the **brain** writes `catchall_domains` (P1 §4.3). Boxes are stateless and never touch the DB (P1 §4 / build-plan §0) — this endpoint reports, it does not persist.
- **Relationship to `/v1/verify`:** `/verify` runs the probe *inline* for the address being checked; `/v1/catchall/:domain` runs it *standalone* (domain-level, no specific mailbox) for pre-seeding / re-probing. Same probe logic (§5), different entry point.

**Request:** `GET /v1/catchall/example.com` · `Authorization: <node-token>` · optional `?probe_local_part=` (else wrapper generates a random one).

**Response (200):**
```jsonc
{
  "wrapper_api_version": "v1",
  "domain": "example.com",
  "is_catchall": true,                // tri-state at the brain: true|false|null(inconclusive)→see §5.4
  "conclusion": "catchall|not_catchall|inconclusive",
  "probe_local_part": "a1b2c3...@",
  "smtp_debug": [ ... ],              // transcript of the probe, for the LOG/audit trail
  "probed_at": "<RFC3339 UTC>"
}
```

- **`LOCKED` semantics:** `is_catchall` here is boolean-or-inconclusive; the brain maps `inconclusive → NULL` into `catchall_domains.is_catchall` (tri-state, P1 §4.3). The response never collapses "not yet probed" and "probed, not catch-all."
- **Failure modes:** `401`; domain unresolvable → `conclusion: "inconclusive"` + `200` (not an error — inability to probe is `unknown`-shaped, not a failure); `500` → inconclusive.

---

### 4.4 `GET /v1/stats` — node self-report `SEAM →` P4

- **Purpose:** a lightweight per-node counters/health snapshot the brain can poll. Build-plan lists `/stats`; its consumers (health-watcher, block-rate alerts, observability dashboards) are **Phase 4**.
- **Phase 2 role:** define a **minimal, additive** shape and have the box emit it; the brain may log it. **Do not** build the health-watcher, quarantine logic, or metrics pipeline (all `SEAM →` P4).

**Request:** `GET /v1/stats` · `Authorization: <node-token>`.

**Response (200):**
```jsonc
{
  "wrapper_api_version": "v1",
  "uptime_s": 12345,
  "engine": { "core": "3.3.0", "version": "0.10.0", "platform": "ruby 3.3.0" }, // = /version
  "counters": {                        // since-start counters; exact set is DIAL
    "verify_total": 0, "smtp_sessions": 0, "catchall_probes": 0,
    "timeouts": 0, "5xx_total": 0, "4xx_total": 0
  },
  "reported_at": "<RFC3339 UTC>"
}
```

- **`DIAL`:** the counter set is tunable; only the *shape* (an additive `counters` object) is fixed now so P4 dashboards attach without a contract break.
- **`SEAM →`** block-rate/unknown-rate/latency aggregation and alerting are Phase 4 (build-plan §4 observability). Stats here is a raw feed, not an alerting system.

---

### 4.5 `POST /v1/config/reload` — hot-reload box config

- **Purpose:** let the brain trigger the box to reload its effective engine configuration (validation levels, timeouts, verifier identity, `validation_type_for`, `dns`/`smtp_port` — including flipping mock→real) **without a redeploy**. This is what makes "switch to a real server is one config value" operationally true at the box.
- **Phase 2 role:** **fully in scope** — the config-flip proof (§3.5 #6) depends on it.

**Request:**
```jsonc
POST /v1/config/reload
Authorization: <node-token>
{ "reason": "<string, optional audit trail>" }   // reload from the box's config source of truth
```
- **`DECISION REQUIRED` (OQ-P2-2) — push vs pull config.** Does `/config/reload` (a) re-read the box's own env/config file (**pull**), or (b) accept a new config *in the request body* (**push**)? **Recommendation: pull** — the box re-reads its configured source; the brain never ships secrets (verifier_email, tokens) in a request body, and the box stays the source of its own runtime truth. Push would put verifier identity/secrets on the wire and split the config source of truth. Flagged, not assumed.

**Response (200):**
```jsonc
{
  "wrapper_api_version": "v1",
  "reloaded": true,
  "config_fingerprint": "cfg_sha256:...",   // NEW effective fingerprint post-reload (see §4.6)
  "reloaded_at": "<RFC3339 UTC>"
}
```
- **Ties to cache invalidation (Door 2/§4.2 P1):** a reload that changes verdict-affecting config yields a **new `config_fingerprint`**, which — because the cache key is `(email_hash, config_fingerprint)` (P1 §4.2) — **naturally invalidates** the cache for the old config. No explicit cache flush needed; the fingerprint change does it. This is called out so the operational model is understood.

**Failure modes:** `401`; invalid/unloadable config → `409` with `reloaded:false` + `error` (box keeps running old config — a reload must never brick the box); `500`.

---

### 4.6 `GET /v1/config` — full effective config (closes GAP-3 / OQ-11) `LOCKED` (that it exists)

- **Purpose:** the resolution of the Phase-1 seam. The brain's `config_fingerprint` (P1 §3.2.5) must cover the **engine's actual verdict-affecting config**, but the stock validation response embeds only a **subset** of `configuration` (Appendix A.2 — no `verifier_email`, `verifier_domain`, timeouts, `smtp_port`, `connection_attempts`). So the fingerprint **cannot** be computed from the `/verify` response alone. This endpoint exposes the **full effective config** so the brain can fingerprint it.

**Request:** `GET /v1/config` · `Authorization: <node-token>`.

**Response (200) — the full verdict-affecting config set (P1 §3.2.5 input list):**
```jsonc
{
  "wrapper_api_version": "v1",
  "effective_config": {
    "verifier_email": "verifier@example.com",
    "verifier_domain": "example.com",
    "default_validation_type": "smtp",
    "validation_type_for": { "gmail.com": "mx" },
    "smtp_safe_check": false,
    "smtp_fail_fast": false,
    "not_rfc_mx_lookup_flow": false,
    "email_pattern": "default gem value",
    "smtp_error_body_pattern": "default gem value",
    "connection_timeout": 2, "response_timeout": 2, "connection_attempts": 2,
    "smtp_port": 25,
    "blacklisted_mx_ip_addresses": [], "whitelisted_domains": [], "blacklisted_domains": [],
    "whitelist_validation": false, "dns": []
  },
  "config_fingerprint": "cfg_sha256:...",   // box MAY compute it too; brain still recomputes canonically
  "engine": { "core": "3.3.0", "version": "0.10.0" },
  "reported_at": "<RFC3339 UTC>"
}
```

- **`LOCKED`:** the *existence* of a full-effective-config endpoint is now a locked requirement (it is what makes `config_fingerprint` honest). The exact field list tracks P1 §3.2.5's input set; adding a future engine option = additive.
- **Fingerprint authority (`LOCKED`):** the **brain** computes the canonical fingerprint (SHA-256 over sorted-key serialization, P1 §3.2.5) from `effective_config`. The box's own `config_fingerprint` is advisory/cross-check only — the brain never trusts a box-computed hash as authoritative (single canonical algorithm, brain-side).
- **`GAP-3` resolved:** with `/v1/config`, the P1 note "in P1 the fingerprint marks engine inputs as `unconfigured`" is retired — from Phase 2, the fingerprint covers the real engine config.
- **Failure modes:** `401`; `500` (can't read config → brain must **refuse to record a confident fingerprint**; verdicts proceed but are flagged fingerprint-degraded rather than silently mis-bucketed).

---

## 5. The catch-all probe (the biggest gap) — full specification

Build plan and SVG both single this out: *"catch-all probe (2nd RCPT, random fake address) — the biggest gap."* It is the difference between a truthful `valid` and a worthless one. Specified here in full.

### 5.1 The problem it solves `LOCKED` (principle)
A **catch-all** (accept-all) domain returns `250 OK` to `RCPT TO` for **every** address, whether or not the mailbox exists. So a bare `250` on the *target* address proves nothing on such a domain. Without a probe, a catch-all domain would mint false `valid` verdicts for non-existent mailboxes — poisoning both the caller's send list and (later) the Phase-7 calibration set. The probe detects this by asking the server about an address we **know** cannot exist.

### 5.2 The probe procedure `LOCKED` (semantics) / `DIAL` (parameters)
Within the **same SMTP validation flow** for the target address, additionally test a **random, almost-certainly-nonexistent** local-part at the **same domain**:

1. Target RCPT: `RCPT TO:<target@domain>` → record response A.
2. Probe RCPT: `RCPT TO:<random_fake@domain>` → record response B, where `random_fake` is a high-entropy local-part (§5.3).
3. **Interpret (the core truth table, §5.5):**
   - target `250` **and** probe `250` → **domain is catch-all** → target verdict `risky`/`catch_all` (existence unprovable), **never `valid`**.
   - target `250` **and** probe `5xx` (rejected) → domain distinguishes real from fake → target `250` is **meaningful** → eligible for `valid`/`accepted`.
   - target `5xx` (no-such-user) → target `invalid`/`mailbox_not_found` regardless of probe (address-fault is decisive).
   - anything transient/ambiguous on **either** RCPT → `unknown`-shaped (§5.5 handles precedence).

### 5.3 The random fake local-part `LOCKED` (that it's random & non-deliverable) / `DIAL` (construction)
- **Requirement:** high enough entropy that a collision with a real mailbox is negligible, and shaped so it can't accidentally be a role/alias. **Recommendation:** 16+ random hex/base32 chars, optionally a fixed unmistakable prefix (e.g. `nx-<random>`), all-lowercase to survive `norm/v1`.
- **`LOCKED`:** the probe address is **never** logged as a verification of a real address, is **never** cached as a verdict, and its `email_hash` is **not** written to `verifications` — it is an instrument, not a subject. Its transcript is captured only inside the target row's `smtp_debug`/`catchall` provenance.
- **`DECISION REQUIRED` (OQ-P2-3) — randomness source determinism.** For reproducible tests the fake must be generatable deterministically in the test harness but random in prod. **Recommendation:** brain/wrapper uses a CSPRNG in prod; tests inject a seeded generator or a fixed `probe_local_part`. Flagged because "random" and "reproducible test" must both hold.

### 5.4 Where the probe runs `DECISION REQUIRED` (OQ-P2-4)
The probe requires a **2nd RCPT in the same session** (§6.3 session reuse). The gem's `Truemail.validate` validates **one** address and does not natively issue a second probe RCPT. Two options:

| Option | How | Trade-off |
|---|---|---|
| **A — wrapper-orchestrated (recommended)** | The fork wrapper, after the gem's SMTP step, issues the probe RCPT on a wrapper-managed session (or calls `Truemail.validate` a second time for the fake and correlates) and adds the `catchall` block (§4.1). | Keeps the gem unforked (Door 5); wrapper owns the extra RCPT. Two validations if not sharing a socket — mitigated by §6.3. |
| **B — brain-orchestrated** | Brain calls `/v1/verify` for target, then `/v1/catchall/:domain` for the probe, and combines. | Simplest wrapper; but **two sessions**, losing same-session guarantee — weaker signal (a catch-all could behave differently across sessions), more connections (reputation cost on a real box). |

- **Recommendation: Option A** — the same-session 2nd RCPT is the whole point ("2nd RCPT" in the build plan), and it minimizes connections (reputation-friendly, §6.3). Requires the wrapper to manage a raw SMTP session for the probe **using the gem's primitives / a thin SMTP client in the wrapper**, without patching the gem.
- **`GAP-13`:** if Option A cannot be implemented purely in the wrapper without gem changes (the gem may not expose a reusable open session), that is a Door-5 tension → escalate as a flagged decision (fork-wrapper-only vs. accept two sessions), **never** a silent gem patch. Recorded, not resolved here.

### 5.5 The catch-all decision truth table `LOCKED`
Precedence matters: address-fault and transient outcomes **outrank** the catch-all conclusion. Evaluated top-down; first match wins:

| # | Target RCPT | Probe RCPT | `catchall.conclusion` | Verdict (status/reason) | Notes |
|---|---|---|---|---|---|
| 1 | 5xx address-fault (`550` no-user) | (any / not run) | n/a | **invalid / mailbox_not_found** | Address-fault is decisive; catch-all irrelevant. |
| 2 | 4xx transient / timeout | (any / not run) | inconclusive **for the target**; but if probe=250, record domain `catchall` (see note) | **unknown / greylisted\|smtp_timeout** | Target verdict can't be trusted (§6.5). The *target* is `unknown` regardless of the probe — a transient on the target outranks a catch-all conclusion for **this address**, but a probe-250 still informs the **domain**. |
| 3 | 250 | 250 | **catchall** | **risky / catch_all** | Existence unprovable. **Never valid.** Brain writes `catchall_domains`. |
| 4 | 250 | 5xx (rejected) | **not_catchall** | **valid / accepted** (SMTP-grade) | Server distinguishes real vs fake → target 250 is meaningful. |
| 5 | 250 | 4xx / timeout / error | **inconclusive** | **risky / catch_all** (conservative) | Probe unreliable → cannot rule out catch-all → do not mint `valid`. Harden toward risky. |
| 6 | 250 | probe skipped (`probe_catchall:false`) | not_probed | **risky / plausible** (SMTP-unconfirmed-catchall) | If we didn't probe, we can't claim `valid` on a domain that *might* be catch-all. |
| 7 | 250 | session died before probe RCPT (§6.3) | **inconclusive** | **risky / plausible** | Same-session probe couldn't complete; treat like row 6 (unprobed) — never `valid`. `signals.catchall_conclusion=inconclusive`. |

- **`LOCKED` invariant:** **a `valid`/`accepted` verdict requires row #4 — target 250 AND probe explicitly rejected.** Every other "target accepted" path is at best `risky`. This is the SVG rule "confidence earned only by clean 250 on non-catch-all" made executable.
- **Row 2 precedence clarification (`LOCKED`):** a transient/timeout on the **target** always makes the **target address** `unknown` — the probe never *upgrades* an `unknown` target to a verdict. But a **probe = 250** observed in that same session is still valid **domain-level** evidence of catch-all and **may** be recorded to `catchall_domains` (it doesn't depend on the target mailbox existing). This separates *address verdict* (unknown) from *domain knowledge* (possibly catch-all) — they are not the same fact.
- **Row 5 rationale:** an inconclusive probe is treated as *possible catch-all*, not *not catch-all* — the conservative direction, consistent with harden-toward-`unknown`/`risky`.
- **Famous-provider interaction (SVG ⑤):** Gmail/Yahoo/O365 are routed to `:mx` (P1 §6.9) and **never reach SMTP**, so the probe doesn't run for them; their "250s" are pure risk by policy, not by probe. The probe governs **SMTP-eligible** (honest corporate) domains.
- **`catch_all` vs `accept_all` (two reserved reasons, disambiguated `LOCKED`):** the reserved P1 vocab (§3.4.3) has both. This spec uses **`catch_all`** for a *domain-level* accept-all detected by the §5.5 probe (target 250 + probe 250) — the P2 workhorse. **`accept_all`** is reserved for the *provider-level* "250s everything" case (SVG ⑤); since such providers are `:mx`-routed and never reach the SMTP probe in P2, **`accept_all` is not emitted in Phase 2** — it stays reserved for when a provider is deliberately SMTP-probed (a later-phase / policy `DIAL`). Flagged so the unused reserved reason is intentional, not an oversight.

### 5.6 Persisting the catch-all result `LOCKED` (that it persists) / `SEAM →` re-probe cadence
- A **catchall** or **not_catchall** conclusion is written **by the brain** (never the box — §4.3) to `catchall_domains` (P1 §4.3): `domain`, `is_catchall` (tri-state; inconclusive → leave `NULL`/unchanged), `detected_at`, `method='probe'`, `expires_at`.
- **`inconclusive` never overwrites a known value** with `NULL` — it is a no-op on a previously-determined domain (avoids a transient blip erasing good knowledge).
- The response `flags.is_catchall` continues to read per P1 §4.3 (the flag collapses `NULL→false`; **catch-all rate analytics read the table, not the flag** — P1 rule preserved).
- **`SEAM →`** re-probe cadence / `expires_at` refresh policy is a `DIAL`; automated re-probing at scale relates to P4 pacing — not built now.

> **[CONSOLIDATION NOTE — 2026-07-17 · additive · assigns ownership to a partial seam] `[PN-15 / SEAM FLAG-C, accepted]`**
> SEAM ledger (consistency report §3, FLAG-C): `catchall_domains.expires_at` exists, and this seam says re-probe cadence "relates to P4 pacing," but **no phase specifies the loop that reads `expires_at` and re-probes** — the seam is partial. **Accepted:** assign re-probe ownership to a concrete home (**P4 health-watcher** or **P5 queue**) **or** mark it `FUTURE / Phase-8+`. **Which home is the owner's call — flagged, not decided** ([`DECISIONS.md`](DECISIONS.md) §5). Companion FLAG-A (`signals.platform` owner) noted at P1 §3.1.3. Changes no frozen behavior; the tri-state persistence rule above stands.

### 5.7 What the probe does NOT do (scope fence)
- It does **not** attempt `DATA` / send a message — RCPT only (never delivers mail; a mock or real box sees only envelope commands).
- It does **not** run for `:mx`-routed providers (§5.5 note).
- It does **not** by itself prove a specific mailbox exists on a catch-all domain — that is structurally impossible via SMTP and is the residue Phase 6 (fallback) / Phase 7 (send history) address. `SEAM →` noted, not solved.

---

## 6. SMTP hardening (full)

All of it, per build-plan § PHASE 2 and SVG §④. Every sub-section descends from §0.4: *harden toward `unknown`.*

### 6.1 5xx classification: address-fault vs your-fault `LOCKED` (the mapping principle) — highest rigor
The single most correctness-critical classification in Phase 2. A `5xx` can mean **the address is bad** (address-fault → `invalid`) or **we/our IP/policy were rejected** (your-fault → **`unknown`**, never invalid). Conflating them corrupts the `invalid` label and Phase-7 calibration.

**The discriminator (grounded in truemail reality, Appendix A.1):** truemail ships `smtp_error_body_pattern` defaulting to:
```
/(?=.*550)(?=.*(user|account|customer|mailbox)).*/i
```
i.e. a `550` whose body mentions user/account/customer/mailbox is read as **no-such-user (address-fault)**. This is the engine's own address-fault heuristic and the anchor for our classification.

**Classification table `LOCKED` (principle) / `DIAL` (exact code/body lists):**

| SMTP response class | Example body | Classification | Verdict (status/reason) |
|---|---|---|---|
| `550` + user/mailbox-not-found body | `550 5.1.1 User unknown` | **address-fault** | **invalid / mailbox_not_found** |
| `551`/`553` recipient-rejected (address form) | `553 mailbox name not allowed` | address-fault | invalid / mailbox_not_found |
| `552` mailbox full / over quota | `552 mailbox full` | **existence proven, deliver-now uncertain** | **valid / mailbox_full** (§6.1.1, owner-decided) |
| `554`/`550` policy / spam / blocked | `554 5.7.1 blocked`, `550 policy` | **your-fault** | **unknown / smtp_blocked** |
| `550` **without** user/mailbox body | `550 rejected` (ambiguous) | **ambiguous → your-fault side** | **unknown / smtp_error** (conservative) |
| `5xx` unrecognized | any other 5xx | ambiguous | **unknown / smtp_error** |
| `4xx` transient | `450`, `421` | transient | **unknown / greylisted\|smtp_timeout** (§6.5) |
| clean `250` (+ probe rejected) | `250 OK` | success | **valid / accepted** (via §5.5 row 4) |

- **`LOCKED` rule:** **only** a `5xx` that positively indicates *the recipient does not exist* (address-fault) maps to `invalid`. Every ambiguous or policy/blocked `5xx` maps to **`unknown`** — the harden-toward-unknown direction. This preserves the P1 invariant that `invalid` = positive proof of non-deliverability (P1 §7.5).
- **`DIAL`:** the exact set of codes/body regexes that count as address-fault is tunable (starts from truemail's `smtp_error_body_pattern`; the brain may extend it). The *direction of ambiguity* (ambiguous → unknown) is `LOCKED`.
- **Famous-provider weighting (SVG ⑤, `SEAM →` P7):** even a `550 no-user` from Gmail/Yahoo is a **weak** invalid signal (they lie), so those providers are `:mx`-routed and don't reach here; where a big-provider 550 *is* seen, confidence is low. Full per-provider trust calibration is Phase 7; here it is a fixed low-confidence note (§7.4).

#### 6.1.1 `552` mailbox full — a POSITIVE existence signal `LOCKED` (owner-decided lean)
A `552`/`452` "mailbox full / over quota" means **the mailbox exists** but can't accept *right now*. **DECIDED (owner override of the draft's `risky` lean):** `mailbox_full` maps to **`valid` / `mailbox_full` at high confidence** — **never `invalid`**, and **not** merely `risky`.
- **Rationale (owner ruling):** a `552` is **positive proof the mailbox exists** — *a non-existent mailbox cannot be "full."* After a clean `250`, it is the **strongest existence signal SMTP produces**. Leaning it `risky` undersold it and was **mildly inconsistent with its own Phase-7 destination** (build-plan §7: "mailbox_full = valid"). P2 now leans where it knows it's going.
- **Status/confidence:** `status = valid`, `confidence` in the **high band** (comparable to address-fault `invalid` — both are positive proofs, §7.4), because *existence* is proven.
- **Recommendation stays honest:** existence ≠ deliverable-right-now, so `recommendation = review` (per the P1 composability invariant `valid ⇒ {send, review}`, P1 §3.1.3) — the mailbox is real, but a sender may want to wait/retry. The **verdict** tells the truth (it exists); the **recommendation** carries the "full now" caveat. This keeps the honest split the whole system relies on: proof-of-existence is a `status` fact, deliverability-timing is a `recommendation` hint.
- **Not a door crossing:** `mailbox_full` was reserved in P1 §3.4.3 as `risky/valid` (both statuses pre-approved) — leaning `valid` is inside the frozen envelope, fully additive. `DIAL` remains only the exact confidence number; the **`valid` lean is now `LOCKED` by owner decision**.

### 6.2 `smtp_debug` transcript capture + parsing (resolves GAP-4) `LOCKED`
Build plan: *"capture full `smtp_debug` transcript into the log."* SVG: *"exact code+body → log / join gold."*

**Ground truth (Appendix A.1) — the transcript shape truemail actually emits.** `smtp_debug` is `null` on clean success, else an **array** of per-host request objects. Each carries a `response` struct:
```jsonc
// one element of smtp_debug[]
{
  "mail_host": "127.0.1.1",
  "port_opened": true,
  "connection": true,
  "helo": true,
  "mailfrom": { "status": "250", "string": "250 OK\n" },   // Net::SMTP::Response-shaped, or false
  "rcptto": false,                                          // false on failure
  "errors": { "rcptto": "550 User not found\n" }            // {stage => raw SMTP string}
}
```
Key realities that drive parsing:
- **GAP-4 confirmed:** the code lives in **raw strings** (`"550 User not found\n"`), often only inside `errors{}`; `rcptto` is a bare `false` on failure, not a code. There is **no structured numeric code** from the engine.
- On a `mailfrom`/`rcptto` *success*, the value is a `{status, string}` object; on failure it's `false` and the detail is in `errors{stage}`.
- `connection`, `helo`, `port_opened` booleans localize *where* a session failed (e.g. `port_opened=false` → couldn't connect → `connection_failed`).

**The brain's parsing job (`LOCKED` that the brain owns it; `DIAL` on regex specifics):**
1. Walk `smtp_debug[]`; for the relevant host, read `errors{rcptto|mailfrom|connection}` and the success `{status,string}`.
2. **Extract the numeric code** with a regex (`^\s*(\d{3})`) from the raw string → `signals.smtp_code` (string token, e.g. `"550"`; **kept as string** per P1 Door-1 `smtp_code` type).
3. **Extract enhanced status** (`5.1.1`) if present → optional `signals.smtp_enhanced_code` (**new additive signal key**, §8).
4. Classify per §6.1 using code + body + `smtp_error_body_pattern`.
5. Locate failure stage from booleans (`port_opened/connection/helo/mailfrom/rcptto`) → drives `connection_failed` vs `smtp_error` vs `mailbox_not_found`.

- **`smtp_code` is parsed, not invented (Door 1):** `signals.smtp_code` was reserved in P1 §3.1.3 as "raw SMTP status token when SMTP ran"; P2 populates it from step 2. No contract change.

### 6.3 Session reuse (many RCPT / one connection) `LOCKED` (design intent) / `SEAM →` P4 scale
Build plan: *"session-reuse design (many RCPT / one connection)."* Two pressures make this matter: the **catch-all probe needs a 2nd RCPT in the same session** (§5.4 Option A), and on a real box **fewer connections = lower reputation cost + harder to fingerprint** (SVG §④).

- **Phase 2 (mocks):** the design is proven against `smtp_mock` with **`multiple_rcptto: true`** (Appendix A.3) — the switch that lets one session accept target + probe RCPTs. This is exactly why that option is in the substrate matrix (§3.3).
- **`LOCKED` shape:** the SMTP interaction is modeled as *one session → HELO/MAIL FROM once → N RCPT TO (target + probe[s]) → QUIT*, not one-session-per-RCPT. The wrapper (§5.4 Option A) owns the session.
- **`SEAM →` P4:** batching *many target addresses for the same domain* into one session (true bulk session reuse, grouped by domain) is a Phase-4/5 optimization (build-plan §5 "group by domain"). Phase 2 proves the **2-RCPT (target+probe)** case; it does **not** build domain-batching.
- **`GAP-13` (again):** whether the wrapper can hold an open session for the 2nd RCPT without patching the gem is the same open tension as §5.4; recorded there.

### 6.4 Transcript into the LOG — no schema change `LOCKED`
- The full `smtp_debug` array is part of the `/v1/verify` response object (§4.1), which the brain stores **whole** as the `verification_raw` sidecar blob (P1 §3.2.6). **The transcript therefore lands in the LOG with zero new columns and zero append-only violation** — it rides the existing sidecar (retention = `DROP PARTITION`).
- **Promoted, queryable facts** go to existing `signals` (jsonb): `smtp_code`, `greylisted`, plus new additive keys (§8). Nothing about the transcript requires altering `verifications`.
- **DECIDED (OQ-P2-5) — store the transcript verbatim EXCEPT the redacted address identity; note the sidecar is now slightly more sensitive.** Store the transcript in `verification_raw` (it is the "join gold"; do **not** scrub codes/bodies/mx/catchall) — **but redact the plaintext checked-address identity first (Door 3).** **[AMENDED 2026-07-17 · owner-ratified · [ADR-0008](adr/ADR-0008-door3-raw-blob-address-redaction.md)]** — this note's original text ("store verbatim") missed that the wrapper response echoes the **target's own plaintext address** (top-level `email` field, and 5xx bodies echo it) — it enumerated only "verifier identity + probe addresses." Door 3 ("the raw full email is NEVER persisted") forbids writing that plaintext address into a durable store, so the brain scrubs the address at the persistence boundary (case-insensitively; `email_hash` stays the join key; `domain` stays Door-3-clear). This IS effectively a Door-3 conformance (the door governs a spec instruction that contradicted it), applied in the safe direction — see ADR-0008. **Access-control follow-up (owner-noted):** from Phase 2, `verification_raw` also contains **verifier identity + probe (nx-) addresses**, so it is marginally more sensitive than "just untouched engine output." This warrants **one line in the Phase-1 access-control runbook** (the same runbook that governs the pepper and the raw blob, P1 §3.3.3). `SEAM →` the runbook itself is a P1 operational artifact; this is a pointer to update it, not a new control.

### 6.5 Timeouts + greylist → `unknown` (+ requeue shape) `LOCKED` (verdict) / `SEAM →` P5 (queue)
- **Timeout tuning (`DIAL` values, `LOCKED` verdict):** truemail exposes `connection_timeout`, `response_timeout`, `connection_attempts` (Appendix A.1). Tuning these is a `DIAL`; the **verdict for a timeout is `LOCKED` = `unknown` / `smtp_timeout`**. The brain also enforces its **own** client-side deadline on `/v1/verify` (a hung box must not hang the brain) → also `smtp_timeout`/`unknown`.
  - **Substrate:** proven with `smtp_mock` `response_delay_rcptto > response_timeout` (§3.3) → forces a real timeout against a fake peer.
- **Greylist (`450`) → `LOCKED` = `unknown` / `greylisted`.** Never `invalid` (SVG §④, P1 vocab). A `4xx` on RCPT is transient; the address is *unproven*, not *bad*.
- **The requeue shape (`SEAM →` P5, designed not built):** a greylisted result carries the metadata a future queue needs to retry, **without** Phase 2 building any queue:
  ```jsonc
  // signals on a greylisted verdict — additive keys, all SEAM → P5 consumers
  "greylisted": true,
  "retry_after_hint_s": 300,        // parsed from 4xx body if present, else policy default (DIAL)
  "retry_eligible": true,           // brain marks it retryable; nothing retries it in P2
  "smtp_code": "450"
  ```
  - **`LOCKED` now:** the verdict (`unknown/greylisted`) and the *presence* of retry metadata in `signals` (so a P5 queue can act without a schema/contract change).
  - **`SEAM →` P5 (NOT built):** the actual backoff/requeue/retry loop, "group by domain," and the retry-then-accept round-trip (which the mock can't fully simulate anyway — **GAP-12**). Phase 2 stubs retry: it *classifies and marks*, it does not *retry*.
  - **DECIDED (OQ-P2-6) — the clean rule: cache only *settled* verdicts; never cache your-fault or transient `unknown`s.** A greylist/timeout/blocked `unknown` is *expected to change on retry* — caching it would **suppress the very retry that resolves it**, freezing a transient non-answer into a served answer. Generalized (owner-approved) beyond just greylist: **`smtp_blocked`, `smtp_timeout`, `greylisted`, `connection_failed`, and any your-fault/transient `unknown` are non-cacheable**; only settled verdicts (`valid`, `invalid`, `risky/catch_all`, address-fault) enter the cache. `smtp_error` (ambiguous) is treated as transient → non-cacheable. This is a single, testable predicate on the verdict, not a per-reason exception list to maintain.

### 6.6 Brain-DNS vs engine-DNS reconciliation (resolves GAP-10 / OQ-24) `LOCKED`
Phase 1 flagged that once SMTP exists there are **two independent resolvers**: the brain's (cheap path: MX presence, provider classification, null-MX — P1 §6.5) and the engine's inside the box (its own MX lookup before RCPT, via `dns_mock` in P2). They can disagree.

**The reconciliation rule (`LOCKED`, per P1 OQ-24 recommendation, now adopted):**
- **The box/engine's MX view is authoritative for the SMTP verdict** — it is the resolver that actually drove the RCPT attempt, so the SMTP result is only interpretable against the MX the box used.
- **The brain's MX view is advisory — used for routing/provider classification** (deciding `:mx` vs `:smtp`, provider trust) *before* the call.
- **Disagreement is logged, not silently reconciled.** When brain-MX and engine-MX differ (different hosts, or one finds MX and the other doesn't), the brain records it for calibration:
  ```jsonc
  // additive signals — §8
  "dns_divergence": true,
  "brain_mx_provider": "google",
  "engine_mx_hosts": ["1.2.3.4"]        // from the /v1/verify `mx` block (§4.1, closes GAP-2)
  ```
- **Verdict on divergence:** if the brain routed to `:smtp` but the **engine found no MX**, the engine wins → `no_mx`-shaped, but because the disagreement signals a resolver/timing issue rather than clean proof, the conservative map is **`unknown` / `smtp_error`** (not `invalid`) unless the engine's own result is an unambiguous address-fault. `DIAL` on the exact precedence; `LOCKED` that divergence is captured and never silently favors the routing (advisory) view.
- **Substrate:** proven by feeding brain-resolver and `dns_mock` **divergent MX** (§3.2) — the only pre-real-box way to exercise this. This is why GAP-10 could not be closed in P1 (no box) and is closed now.

### 6.7 Fail-fast on bulk / per-provider pacing `SEAM →` P4
- truemail's `smtp_fail_fast` (Appendix A.1) ends an SMTP session after the first failure on the first MX — useful for bulk to cut total time. **Phase 2:** the `config_fingerprint` already includes `smtp_fail_fast` (P1 §3.2.5), and `/v1/config` exposes it; its **effect** on scoring is neutral (a fail-fast failure is classified the same as a non-fail-fast one). **`SEAM →` P4:** per-provider rate + jitter and bulk pacing (SVG §④, build-plan §4) are **not** built now — noted so the config knob is understood but its pool-level use is deferred.
- **HELO / verifier-identity rotation (SVG §④ "rotate HELO/verifier") — `SEAM →` P4.** Rotating the HELO name and MAIL FROM verifier identity is a **reputation/anti-fingerprinting** feature that only matters on a **real IP** talking to real providers. Against mocks it is inert (the fake peer doesn't judge reputation). Phase 2 uses a **single fixed** verifier identity (`verifier_email`/`verifier_domain` from `/v1/config`) and does not build rotation. Flagged explicitly because it appears in the SVG SMTP-hardening panel — it is deliberately deferred, not overlooked. (Verifier identity is nonetheless captured in the transcript/provenance — OQ-P2-5.)

### 6.8 `smtp_safe_check` — the ambiguous-pass semantics (required carry-through) `LOCKED`
The task requires: *"an ambiguous-pass is lower-confidence, logged distinctly from a clean 250."* Ground truth (Appendix A.1) makes this concrete:

- With **`smtp_safe_check = true`**, truemail parses SMTP error bodies and can return **`success = true` even when the session had errors** (it treats non-address-fault errors leniently). Critically, in that case **`smtp_debug` is populated** (the transcript shows the errors), whereas a **clean 250 returns `success = true` with `smtp_debug = null`**.
- **The `LOCKED` discriminator:** `success == true && smtp_debug == null` → **clean pass** (eligible for full `valid` confidence via §5.5 row 4). `success == true && smtp_debug != null` → **ambiguous/safe pass** → **lower confidence, logged distinctly.**
- **How it's logged distinctly (additive, no Door change):**
  ```jsonc
  "smtp_safe_pass": true,        // additive signal (§8): success came via safe_check, not a clean 250
  "smtp_debug_ref": true         // transcript present in verification_raw despite success
  ```
  and a **confidence penalty** in scoring (§7.4): a safe-check pass is capped below a clean-250 pass. It may still be `valid` (if the probe rejected, §5.5), but at reduced confidence — or `risky` if policy is strict. `DIAL` on the exact cap; `LOCKED` that the two are distinguishable in the LOG and never assigned identical confidence.
- **`DECISION REQUIRED` (OQ-P2-7) — is `smtp_safe_check` on or off by default in P2?** **Recommendation: OFF by default** (clean, unambiguous verdicts; ambiguity → `unknown`), with `smtp_safe_check=true` available as a policy/config dial for domains where lenient passing is wanted. Off-by-default keeps the harden-toward-`unknown` posture strongest. Flagged (it is verdict-affecting and part of `config_fingerprint`).

---

## 7. Scoring: SMTP signals → verdict

Phase 1 built the signals-plus-policy engine (P1 §7) and its cheap-layer mappings; the SMTP layer was reserved. Phase 2 adds the **SMTP-signal → status/reason/confidence** mapping — the largest new brain surface — **inside the existing policy/scoring model, additively.** No grammar change (P1 OQ-25 open-object discipline holds).

### 7.1 SMTP as a Type-1 core layer in the existing model `LOCKED`
- SMTP is a **Type-1 (core) layer** (SVG §②): it can gate the verdict. It emits `LayerOutput`s (P1 §6.0) into the same signals bag; the **policy** decides gate/flag/stop — SMTP does not decide the verdict itself (Door 5 + P1 §7.1 preserved).
- It runs **only if** the cheap layers didn't already stop-early (syntax/mx/null-mx/disposable/spam-trap gates) **and** the provider isn't `:mx`-routed (P1 §6.9). This is the SVG lifecycle: cheap layers → provider routing → SMTP on box.

### 7.2 The SMTP signal → verdict map `LOCKED` (principle) / `DIAL` (thresholds)
Consolidated from §5.5 (catch-all) and §6.1 (5xx). This is the **complete** SMTP decision surface. Evaluated after the transcript is parsed (§6.2); first match wins:

| SMTP outcome | status | reason | confidence band (§7.4) |
|---|---|---|---|
| Clean 250, probe rejected (§5.5 #4), `smtp_debug=null` | **valid** | accepted | high |
| Clean 250, probe rejected, but via `smtp_safe_check` (`smtp_debug≠null`, §6.8) | **valid** | accepted | **reduced** (safe-pass cap) |
| 250 target + 250 probe (catch-all, §5.5 #3) | **risky** | catch_all | low |
| 250 target, probe inconclusive/skipped (§5.5 #5/#6) | **risky** | catch_all \| plausible | low |
| 550 no-such-user (address-fault, §6.1) | **invalid** | mailbox_not_found | high |
| 552 mailbox full (§6.1.1) — **positive existence proof** | **valid** | mailbox_full | high (rec=review) |
| 554/550 policy/blocked (your-fault, §6.1) | **unknown** | smtp_blocked | n/a (uncertainty) |
| 450 / 4xx transient greylist (§6.5) | **unknown** | greylisted | n/a |
| timeout (engine or brain deadline, §6.5) | **unknown** | smtp_timeout | n/a |
| couldn't open session / `port_opened=false` (§6.2) | **unknown** | connection_failed | n/a |
| ambiguous/unrecognized 5xx (§6.1) | **unknown** | smtp_error | n/a |
| DNS divergence, engine found no MX (§6.6) | **unknown** | smtp_error | n/a |

- **`LOCKED` invariants (all inherited, restated in SMTP terms):**
  - `valid` **only** via the clean-250 + probe-rejected path (§5.5 row 4). Nothing else mints `valid`.
  - `invalid` **only** via positive address-fault proof (550 no-user class). Every ambiguity → `unknown`.
  - Every transient/your-fault/timeout/blocked → `unknown`, **never** `invalid`.
- **`DIAL`:** confidence band values and the recommendation thresholds (P1 §7.2 `recommendation_thresholds`) — not the status/reason mapping.

### 7.3 Policy integration — additive, no grammar change `LOCKED`
- The `cold-email` policy (P1 §7.3) gains SMTP in its `order` and `gates` **as data**, using the existing action grammar `"<status>:<reason>:<stop|continue>"` (P1 §7.2):
  ```jsonc
  // additive entries to the existing cold-email policy definition (JSONB) — shape unchanged
  "order": [ ...cheap layers..., "provider_classify", "smtp" ],
  "gates": {
    "smtp:valid":      "valid:accepted:stop",
    "smtp:invalid":    "invalid:mailbox_not_found:stop",
    "smtp:catch_all":  "risky:catch_all:stop",
    "smtp:mailbox_full":"valid:mailbox_full:stop",   // 552 = positive existence proof (§6.1.1, owner-decided); rec=review carries the "full now" caveat
    "smtp:unknown":    "unknown:smtp_error:stop"     // reason refined by §7.2 sub-map
  }
  ```
- **`LOCKED`:** because the policy is an open JSONB object (P1 OQ-25) and every `reason` used was pre-reserved (P1 §3.4.3), **adding SMTP is additive — no schema, no grammar, no contract change.** This is the Phase-1 forward-expressiveness check (P1 §7.2) paying off exactly as designed.
- **`DECISION REQUIRED` (OQ-P2-8) — does the default policy enable SMTP, or ship it off until Phase 3?** Since Phase 2 has no real box, SMTP runs against mocks only. **Recommendation:** the `cold-email` policy gains an `smtp` entry **enabled**, but the *node it routes to* is the mock in P2; Phase 3 flips the node to real. This keeps the policy stable across P2→P3 (only the node changes — the config-flip goal). Flagged so "SMTP on by default" is a conscious choice.

### 7.4 Confidence for SMTP verdicts `DIAL` / `SEAM →` P7
Extends P1 §7.4's fixed baseline-by-reason table (still not a learned model — no outcome data until P7):
- **Clean 250 + probe-rejected** → high (e.g. 85–95): the strongest signal Phase 2 can produce, but **not** 100 — SMTP `250` is evidence, not proof of a human mailbox.
- **Safe-check pass** (§6.8) → capped below clean-250 (e.g. ≤ 70): ambiguity is explicit.
- **address-fault `invalid`** → high (e.g. 90–99): positive proof.
- **`552` `mailbox_full` → `valid`, high (e.g. 85–95):** positive *existence* proof (§6.1.1, owner-decided); `recommendation=review` carries the "full now" caveat, so high confidence in the verdict does not mean "send immediately."
- **`catch_all`/`plausible` `risky`** → low–medium (e.g. 25–50).
- **all `unknown`** → low, expressing uncertainty (never inflated to dodge `unknown`).
- **Famous-provider 5xx** (rare, since `:mx`-routed) → low-confidence invalid signal (SVG ⑤).
- **`SEAM →` P7:** every band here is recalibrated per (reason × provider) from real bounce outcomes; these are placeholders (P1 §7.4 continuity).

### 7.5 The frozen boundary, restated for SMTP `LOCKED`
The P1 boundary — *`invalid` = positive proof of non-deliverability; everything limited/blocked/ambiguous = `unknown`; deliverable-but-undesirable = `risky`* — is **unchanged**. Phase 2 simply populates it with SMTP evidence. A blocked/greylisted/timed-out SMTP result is `unknown` for the identical reason a rate-limited cheap layer was in P1: absence of proof is not proof of absence.

---

## 8. New additive `signals` keys introduced in Phase 2 (Door 1 additivity ledger) `LOCKED`

Every key below is an **addition** to the open `signals` bag (P1 §3.1.3), permitted by Door 1's additive-only rule and the "tolerate unknown keys" contract (P1 §3.1.2 rule 5). **No existing field, flag, enum value, or type changes.** This ledger exists so the additive surface is auditable and nothing sneaks in as a silent break.

| New `signals` key | Type | Set when | Source § |
|---|---|---|---|
| `smtp_code` | string | SMTP ran (populated, was reserved in P1) | §6.2 |
| `smtp_enhanced_code` | string | enhanced status (`5.1.1`) present in body | §6.2 |
| `greylisted` | bool | 4xx transient seen (populated, was reserved in P1) | §6.5 |
| `retry_after_hint_s` | integer | greylist retry hint (queue seam) | §6.5 |
| `retry_eligible` | bool | brain marks transient as retryable (P5 seam) | §6.5 |
| `smtp_safe_pass` | bool | success via `smtp_safe_check`, not clean 250 | §6.8 |
| `smtp_debug_ref` | bool | transcript present in `verification_raw` | §6.4/§6.8 |
| `dns_divergence` | bool | brain-MX vs engine-MX disagreed | §6.6 |
| `brain_mx_provider` | string | brain's provider classification (advisory) | §6.6 |
| `engine_mx_hosts` | array<string> | MX the box actually used (authoritative) — closes GAP-2 | §4.1/§6.6 |
| `catchall_conclusion` | string | `catchall\|not_catchall\|inconclusive\|not_probed` | §5.5 |

- **`reason` enum:** **zero new values** — all SMTP reasons (`accepted`, `mailbox_not_found`, `mailbox_full`, `catch_all`, `accept_all`, `greylisted`, `smtp_blocked`, `smtp_timeout`, `smtp_error`, `connection_failed`) were reserved in P1 §3.4.3. If Phase 2 ever needs one beyond these, that is a **new one-way-door crossing** and is flagged in §9, not slipped in.
- **`flags`:** no new flags required in P2 (`is_catchall` already exists, P1 §3.1.3; now populated by the probe). If catch-all detection wants a distinct "detected vs seeded" flag, that is an additive `flags` key — deferred, not added speculatively.
- **LOG impact:** all of the above ride the existing `signals` jsonb column and `verification_raw` sidecar (§6.4) — **no `verifications` column added or altered.**

---

## 9. Contradictions & gaps found in the source material (flagged, not resolved)

Per the requirement to surface — not quietly fix — inconsistencies between the build plan, the SVG, the Phase 1 spec, and upstream truemail reality. GAP-1..10 are inherited from Phase 1 (GAP-1/3/4/10 are *actioned* here as noted); GAP-11..13 are new to Phase 2.

| ID | Where | Gap | Handling in this spec |
|---|---|---|---|
| **GAP-1** (inherited, actioned) | build-plan §1.3 "hit `/validate`" | Stock rack has no `/validate`; it's `GET /?email=`. | Fork adds `POST /v1/verify`; node client targets it from P2. §4.0/§4.1. |
| **GAP-3** (inherited, **resolved**) | `config_fingerprint` needs full box config | Stock validation response embeds only a config **subset**. | **Resolved:** `GET /v1/config` exposes full effective config; brain fingerprints it. §4.6. |
| **GAP-4** (inherited, **resolved**) | `smtp_code` | truemail returns SMTP status as **raw strings**, no numeric code. | **Resolved:** brain parses code from `smtp_debug` strings → `signals.smtp_code`. §6.2. |
| **GAP-10** (inherited, **resolved**) | brain-DNS vs engine-DNS | Two resolvers can disagree once SMTP exists. | **Resolved:** engine-MX authoritative for verdict, brain-MX advisory for routing, divergence logged. §6.6. |
| **GAP-2** (inherited, mitigated) | rack JSON omits `domain`/`mail_servers` | Provider classification/verdict can't use engine's over-the-wire MX. | Wrapper `/v1/verify` **adds** an `mx` block (§4.1); brain still self-resolves for routing. §4.1/§6.6. |
| **GAP-8** (inherited) | truemail local-tz timestamps | Engine emits `"2021-02-07 10:00:56 +0200"`. | Brain stores UTC; raw tz string preserved in `verification_raw`. §4.1. |
| **GAP-11** (new) | build-plan "smtp_mock + dns_mock (its own test tooling)" | Imprecise: `smtp_mock`/`dns_mock` are **separate mocktools gems** (socket-level), distinct from `truemail-rspec` (result-stub). Conflation risks leaving wire behavior untested. | Two substrates, two proof scopes; end-to-end proof uses the socket-level mocks. §1.4/§3.4. |
| **GAP-12** (new) | build-plan "greylist 450 → unknown (retry logic stubbed until queue)" | `smtp_mock` has **no stateful greylist (reject-then-accept) mode** — the full greylist round-trip can't be faithfully mocked. | P2 proves **detection + classification + requeue-marking**; defers retry-then-accept to P5/real box. Explicitly not overclaimed. §3.3/§6.5. |
| **GAP-13** (new) | build-plan "session-reuse (many RCPT/one connection)" + Door 5 | The 2nd-RCPT catch-all probe (§5.4 Option A) needs a reusable open SMTP session; the pinned gem may not expose one without a patch — tension with never-fork-engine. | Flagged as a Door-5 escalation point: wrapper-side session vs accept-two-sessions (Option B); **never** a silent gem patch. §5.4/§6.3. |
| **GAP-14** (new) | `smtp_mock` behavior vs the `valid` path | `smtp_mock` 250-accepts every RCPT except pre-listed ones (no "reject unknown recipients" mode) → it is inherently catch-all-shaped, so a **random** probe is always accepted and the `valid` path (§5.5#4) is unreachable without a **seeded** probe pre-listed for rejection. | P2 proves `valid` with a seeded probe (OQ-P2-3); acknowledged as a **test artifact** — faithful reject-arbitrary-unknown behavior is a real-box (P3) property. §3.3/§3.5 honest-limit. |

> **No gap is silently resolved.** Where this spec *acts* on an inherited gap (GAP-1/3/4/10), that is the Phase-1-designated resolution phase arriving — not a quiet change. The three new gaps (11–13) are surfaced with an explicit handling stance, and GAP-13 is left as a genuine open escalation because resolving it may require an implementation spike the spec cannot pre-decide.

---

## 10. Open questions / decisions required (with recommendations)

Phase-2-specific. Numbered `OQ-P2-N` to avoid collision with Phase 1's OQ-1..25. **All 11 signed off by the owner (see Appendix D):** 10 recommendations accepted; **OQ-P2-4 conditioned on the GAP-13 spike** (Door 5 outranks same-session elegance); the **552 lean was overridden separately** (§6.1.1). The "Decision" column is now binding for build.

| OQ | Topic | Decision (signed off) | Touches |
|---|---|---|---|
| **OQ-P2-1** | Wrapper endpoint namespacing (bare vs `/v1/`) | **Accepted:** namespace under `/v1/` on the box; keep stock `GET /`,`/version`,`/healthcheck`. | §4.0 |
| **OQ-P2-2** | `/config/reload` push vs pull | **Accepted — PULL.** Box re-reads its own config source; verifier identity/secrets are **never** shipped in a request body. | §4.5 |
| **OQ-P2-3** | Probe randomness source (random vs reproducible) | **Accepted:** CSPRNG in prod; seeded/fixed `probe_local_part` in tests (also required by GAP-14 to reach the `valid` path on mocks). | §5.3 |
| **OQ-P2-4** | Where the catch-all 2nd RCPT runs | **Accepted, CONDITIONED on the GAP-13 spike (OQ-P2-11).** Prefer Option A (same-session), **but if the pinned gem can't expose a reusable session without a patch, fall back to Option B (two sessions, documented weaker signal) — Door 5 outranks same-session elegance. Never patch the gem.** | §5.4 |
| **OQ-P2-5** | Store raw transcript verbatim (contains probe fake + verifier identity)? | **Accepted, AMENDED (ADR-0008, owner-ratified 2026-07-17):** store verbatim (join gold) **EXCEPT the plaintext checked-address identity, which is redacted first (Door 3 — the response also echoes the *target's* address, which the original note missed).** **Note:** sidecar still holds verifier identity + probe (nx-) addresses → one-line update to the P1 access-control runbook (§6.4). | §6.4, [ADR-0008](adr/ADR-0008-door3-raw-blob-address-redaction.md) |
| **OQ-P2-6** | Cache greylisted/transient `unknown`? | **Accepted + GENERALIZED:** never cache your-fault/transient `unknown`s (`smtp_blocked`/`smtp_timeout`/`greylisted`/`connection_failed`/`smtp_error`); cache **only settled verdicts**. | §6.5 |
| **OQ-P2-7** | `smtp_safe_check` default on/off | **Accepted — OFF by default** (strongest harden-toward-unknown); available as a policy/config dial. | §6.8 |
| **OQ-P2-8** | Default policy: SMTP enabled in P2 or off until P3? | **Accepted:** enable `smtp` in the policy now, routed to the **mock** node; P3 flips node to real (policy stable across the flip). | §7.3 |
| **OQ-P2-9** | `/audit` enforcement in P2 | **Accepted:** contract + read/log only; the "no SMTP until green" **gate** stays Phase 3 (needs real IP/PTR). | §4.2 |
| **OQ-P2-10** | Multiple probe RCPTs for higher catch-all confidence? | **Accepted:** one probe RCPT in P2; N-probe is a P4 `DIAL` if false-catch-all rates warrant. | §5 |
| **OQ-P2-11** | GAP-13: wrapper session reuse vs gem constraint | **Accepted — RUN THE SPIKE EARLY, before committing to Option A.** It is the one thing that could force an architecture choice. If the gem can't expose a session cleanly → Option B, never a gem patch. | §5.4/§6.3 |

---

## 11. Phase seams (where Phase 2 deliberately stops)

| Later phase | Seam left in / honored by Phase 2 | Not built now |
|---|---|---|
| **P3** first real box | `/v1/audit` contract built + read/logged; `nodes.audit_status`/`last_audit_at`/`port25_open` populated; `/config/reload` proven → node flip is config-only; policy stable across flip (OQ-P2-8). | The audit **gate** (no-SMTP-until-green enforcement), real IP/PTR/SPF, warmup ramp, flipping mock→real box. |
| **P4** resilience & pool | `/v1/stats` shape emitted; `Strategy` interface unchanged (P1 §5.4); `smtp_fail_fast` config-exposed; session-reuse (2-RCPT) proven. | Health-watcher, auto-quarantine, sticky/least-inflight strategies, **per-provider rate+jitter**, domain-batched session reuse, metrics/alerting pipeline. |
| **P5** queue | Greylist → `unknown` + **requeue metadata** (`retry_eligible`/`retry_after_hint_s`) present in `signals`; transient `unknown` not cached (OQ-P2-6). | The **queue itself**, backoff/retry loop, retry-then-accept round-trip (GAP-12), group-by-domain, `/v1/verify/bulk` job handle. |
| **P6** fallback | `catch_all`/`accept_all` residue is cleanly labeled `risky` (the exact residue P6 routes out); `reason=external_verified` still reserved (P1). | Fallback API integration; routing the catch-all/Gmail residue to it. |
| **P7** flywheel | Every SMTP verdict logs full transcript (`verification_raw`) + parsed `signals` (`smtp_code`, catch-all, safe-pass, dns-divergence) — the join-gold; `mailbox_full`→`valid` and address-fault→`invalid` already match the P7 bounce-classification rules ("mailbox_full = valid"); confidence bands flagged as placeholders. | Bounce classification, per-(reason×provider) recalibration of the §7.4 bands, provider auto-promotion to `:mx`. |

---

## 12. Phase 2 "done" — restated with acceptance criteria

**Build-plan definition:** *the entire pipeline works end-to-end on mocks, and switching to a real server is literally one config value.*

Acceptance criteria for this spec's design (all verifiable against the **socket-level** `smtp_mock`/`dns_mock` substrate — no real IP):

1. **Endpoint contracts** (§4) exist on the forked rack — `/v1/verify`, `/v1/audit`, `/v1/stats`, `/v1/catchall/:domain`, `/v1/config/reload`, `/v1/config` — each with the specified request/response/failure shapes, node-token auth, and **zero gem modification** (Door 5, CI-asserted).
2. **The core "Prove:" matrix (§3.5) passes** against socket-level mocks: real→`valid/accepted` (via a **seeded** probe — GAP-14); fake(550 no-user)→`invalid/mailbox_not_found`; blocked(5xx your-fault)→`unknown/smtp_blocked`; accept-all→`risky/catch_all`; delay>timeout→`unknown/smtp_timeout`.
3. **Catch-all probe** (§5) runs as a same-session 2nd RCPT (OQ-P2-4/A), and the §5.5 truth table holds — in particular **`valid` is minted only on target-250 + probe-rejected**; catch-all domains write `catchall_domains` (tri-state, inconclusive never overwrites).
4. **5xx classification** (§6.1) distinguishes address-fault (`invalid`) from your-fault/ambiguous (`unknown`) using code + `smtp_error_body_pattern`; `552` mailbox-full is **`valid`** (positive existence proof, rec=review), never `invalid`.
5. **Transcript capture** (§6.2/§6.4): full `smtp_debug` lands in `verification_raw` with **no new `verifications` column**; `signals.smtp_code` is parsed from the raw string (GAP-4 closed).
6. **Greylist/timeout** → `unknown` (§6.5) with requeue metadata present in `signals`; **no queue built**; transient `unknown` not cached; GAP-12 (no stateful greylist mock) explicitly acknowledged, retry-then-accept deferred.
7. **DNS reconciliation** (§6.6, GAP-10 closed): engine-MX authoritative for the SMTP verdict, brain-MX advisory for routing, divergence logged in `signals`; proven with divergent brain/dns_mock MX fixtures.
8. **`config_fingerprint`** (§4.6, GAP-3 closed) now covers real engine config via `/v1/config`; a `/config/reload` that changes verdict-affecting config yields a new fingerprint that naturally invalidates cache (P1 §4.2).
9. **`smtp_safe_check` semantics** (§6.8): a safe-check pass is logged distinctly (`smtp_safe_pass`) and scored below a clean 250; the two are never assigned identical confidence.
10. **Additivity proof** (§8): the entire Phase-2 surface is additive — new `signals` keys + populated reserved `reason`s only; a diff against the frozen Door-1/Door-4 contracts shows **no removal, rename, or type change**.
11. **Config-flip proof** (§3.5 #6): a config-only diff re-points the brain from mock to a would-be real box with **zero code change**.
12. **Every OQ-P2** (§10) is signed off or explicitly deferred with an owner — none silently assumed; GAP-13 (§5.4) carries an explicit escalation path, not a silent gem patch.

---

## Appendix A — Upstream ground truth (captured, anchors the SMTP contracts)

> Captured from the `truemail` gem, `truemail-rack`, `smtp_mock`, `dns_mock`, and `truemail-rspec` READMEs so the SMTP-path design is anchored in what the tools *actually* do — not assumptions. This is reference, not our contract. (A.1/A.2 extend the Phase-1 Appendix A with the SMTP-relevant detail.)

### A.1 truemail gem — SMTP-relevant config & behavior
- **SMTP is validation level 4:** `[whitelist/blacklist] → regex → mx → mx_blacklist → smtp`. SMTP runs only if prior levels pass.
- **`connection_timeout` (def 2s), `response_timeout` (def 2s), `connection_attempts` (def 2)** — timeout tuning knobs (§6.5). If a domain has one MX, `connection_attempts` is used as attempts.
- **`smtp_fail_fast`** (def false): ends the SMTP session after the first failure on the first MX (any fail case) — cuts total time, for bulk (§6.7).
- **`smtp_safe_check`** (def false): parses SMTP error **bodies**; can return `success=true` with a **populated `smtp_debug`** on non-address-fault errors. Clean 250 → `success=true`, `smtp_debug=null`. This null-vs-populated split is the §6.8 discriminator.
- **`smtp_error_body_pattern`** (def `/(?=.*550)(?=.*(user|account|customer|mailbox)).*/i`): the engine's own **address-fault** discriminator — anchor for §6.1 5xx classification.
- **`smtp_port`** (def 25): pointed at `smtp_mock`'s port for P2. **`dns`** (def `[]`, else `['ip:port', ...]`): pointed at `dns_mock` for P2.
- **`not_rfc_mx_lookup_flow`** (def false): changes MX/Null-MX lookup breadth — **not** the RFC-7505 null-MX detection (P1 §6.6 note preserved).

### A.2 `smtp_debug` transcript shape (the parse target, §6.2)
`smtp_debug` = `null` on clean success, else an array of per-host objects:
```jsonc
{ "mail_host":"127.0.1.1", "port_opened":true, "connection":true, "helo":true,
  "mailfrom": {"status":"250","string":"250 OK\n"} | false,
  "rcptto": false,                                   // {status,string} on success, false on failure
  "errors": { "rcptto":"550 User not found\n" } }    // {stage => raw SMTP string}; stage ∈ connection|mailfrom|rcptto
```
- Failure detail lives in `errors{stage}` as **raw strings** (GAP-4); success in a `{status,string}` object.
- Stage booleans (`port_opened/connection/helo/mailfrom/rcptto`) localize where a session failed (§6.2 step 5).
- `smtp_fail_fast=true` example shows iteration stops after the first host's failure (`port_opened=false`, empty `errors{}`) → `connection_failed`-shaped.

### A.3 `smtp_mock` (mocktools) — the SMTP substrate
- Separate gem; Ruby wrapper over Go `smtpmock`; a **real TCP SMTP server**. Dynamic/fixed port. truemail points at it via `smtp_port`.
- Behavior knobs used by this spec: `not_registered_emails` (550 no-user), `blacklisted_rcptto_emails` (policy reject), **`multiple_rcptto: true`** (2nd RCPT → catch-all probe), `response_delay_rcptto`/`response_delay_*` (timeouts), `session_timeout`, `fail_fast`, and `msg_rcptto_*`/`msg_*` custom response bodies (craft exact 5xx/4xx). Default behavior 250-accepts (models catch-all).
- **No stateful greylist mode** (GAP-12): reject-then-accept can't be faithfully simulated.

### A.4 `dns_mock` (mocktools) — the DNS substrate
- Separate gem; lightweight **UDP DNS mock**. Mocks `A/AAAA/CNAME/MX/NS/PTR/SOA/SRV/TXT`; auto-punycodes; dynamic/fixed port. truemail points at it via `dns`.
- Null-MX via `.:0`; PTR records supported (needed for `/audit`, §4.2). No response-delay knob (DNS-timeout tested via dead port, §3.2).

### A.5 `truemail-rspec` — the result-stub substrate (secondary)
- `create_validator(...)` returns a **stubbed `Truemail::Validator`** (no socket): params `validation_type`, `email`, `mail_servers`, `success:`, `rcptto_error:`, `configuration:`. `create_auditor(...)` stubs `host_audit`. Used for fast brain-logic unit tests (§3.4), **not** the end-to-end wire proof.

### A.6 `host_audit` output (the `/audit` payload, §4.2)
```jsonc
// Truemail.host_audit → Auditor::Result
{ "current_host_ip":"127.0.0.1",
  "warnings": {} }                                   // green
// or warnings present:
{ "current_host_ip":"127.0.0.1",
  "warnings": { "dns":"A-record of verifier domain not refers to current host ip address",
                "ptr":"PTR-record does not reference to current verifier domain" } }
```
- Audit dimensions: **IP** (host reachable / detect IP), **DNS** (verifier domain A-record → host IP), **PTR** (reverse DNS → verifier domain). `warnings == {}` ⇒ green.

---

## Appendix B — Phase-2 proof matrix (the test intent, not test code)

The minimum socket-level scenarios Phase 2 must demonstrate. Substrate column names the exact `smtp_mock`/`dns_mock` fixture. (Design intent for the eventual test suite — no code here.)

| # | Substrate fixture | Expected verdict | Proves |
|---|---|---|---|
| 1 | dns_mock MX→127.0.0.1; smtp_mock 250 for target; **seeded** probe (OQ-P2-3) pre-listed in `not_registered_emails:[<seeded_probe>]` so exactly it is rejected | `valid/accepted`, high conf | clean-250 + probe-rejected = the only `valid` path (§5.5#4). **Seeded probe required (GAP-14)** — a random probe would be 250'd by the accept-all mock. |
| 2 | smtp_mock `not_registered_emails:[<target>]` | `invalid/mailbox_not_found` | address-fault 550 = invalid (§6.1) |
| 3 | smtp_mock `blacklisted_rcptto_emails:[<target>]` / `msg_rcptto_*`=`554 blocked` | `unknown/smtp_blocked` | your-fault 5xx ≠ invalid (§6.1) |
| 4 | smtp_mock default 250 to **all** (incl. random probe) | `risky/catch_all` | catch-all detection (§5.5#3); writes `catchall_domains` |
| 5 | smtp_mock `response_delay_rcptto` > `response_timeout` | `unknown/smtp_timeout` | timeout = unknown (§6.5) |
| 6 | smtp_mock `msg_rcptto_*`=`450 greylisted` | `unknown/greylisted` + `retry_eligible` | greylist = unknown + requeue-marked, not retried (§6.5) |
| 7 | smtp_mock `msg_rcptto_*`=`552 mailbox full` | `valid/mailbox_full`, high conf, rec=review | 552 = positive existence proof (§6.1.1); full mailbox exists, never invalid |
| 8 | smtp_mock port closed / `port_opened=false` | `unknown/connection_failed` | can't-connect = unknown (§6.2) |
| 9 | brain-resolver MX ≠ dns_mock MX | verdict per engine + `dns_divergence:true` logged | GAP-10 reconciliation (§6.6) |
| 10 | `smtp_safe_check=true`, ambiguous error (success + populated `smtp_debug`) | `valid` reduced-conf + `smtp_safe_pass:true` | safe-pass logged distinctly, scored lower (§6.8) |
| 11 | dns_mock null-MX (`.:0`) | `invalid/null_mx` **before** SMTP | cheap gate precedes SMTP (P1 §6.6 unchanged) |
| 12 | dns_mock PTR misaligned | `/v1/audit` → `red` (read/logged only) | audit contract works; gate deferred to P3 (§4.2) |
| 13 | Re-point node `base_url`+ports mock→(would-be)real; config-only diff | identical brain behavior | the config-flip goal (§3.5#6) |

---

## Appendix C — Review log (what the adversarial critique pass changed)

This spec was drafted, then critiqued by **two** adversarial architecture-review passes against the frozen Phase-1 contracts and upstream truemail reality, before finalizing (as required by the task, mirroring the Phase-1 Appendix B). The first pass produced blockers PB1–PB3 and majors PM1–PM7; a second verification pass confirmed those were resolved in the body and added majors PM8–PM10. All findings folded in:

**Blockers (all resolved):**
- **PB1 — Box statelessness violation.** `/v1/catchall/:domain` (§4.3) and §5.6 read as if the *box* writes `catchall_domains`. Boxes are stateless and never touch the DB (build-plan §0, P1 §4). **Resolved:** box returns the conclusion only; the **brain** persists (§4.3, §5.6).
- **PB2 — `valid` mintable on a catch-all.** An early draft of the §5.5 table risked `valid` when the probe was inconclusive. **Resolved:** the `LOCKED` invariant — `valid` requires **target-250 AND probe-explicitly-rejected** (row #4); every other accepted path is `risky` (rows #3/#5/#6).
- **PB3 — Reserved reason `accept_all` silently unused.** §5.5/§7.2 emitted only `catch_all`, leaving a reserved P1 reason with no producer and no explanation. **Resolved:** `catch_all` (domain-level, probe-detected) vs `accept_all` (provider-level, `:mx`-routed → not emitted in P2) disambiguated and flagged intentional (§5.5).

**Majors (all resolved):**
- **PM1 — GAP-4 under-specified.** "Parse the code" glossed the reality that the code is a raw string often only inside `errors{}`, with `rcptto=false` on failure. **Resolved:** concrete parse procedure against the real `smtp_debug` shape (§6.2, Appendix A.2).
- **PM2 — Greylist over-claim.** Draft implied greylist handling was "done." `smtp_mock` has no stateful reject-then-accept mode. **Resolved:** GAP-12; P2 proves detect+classify+mark, defers retry-then-accept to P5/real box; acceptance §11 says so.
- **PM3 — Door-5 tension on session reuse.** The same-session 2nd RCPT may need a reusable open session the pinned gem doesn't expose → risk of a silent gem patch. **Resolved:** GAP-13 + OQ-P2-11 as an explicit escalation (wrapper-session vs two-session Option B), never a gem patch (§5.4/§6.3).
- **PM4 — `smtp_safe_check` distinctness mechanism absent.** Task requires ambiguous-pass logged distinctly; draft asserted it without a mechanism. **Resolved:** the `success && smtp_debug==null` vs `success && smtp_debug!=null` discriminator + `smtp_safe_pass` signal + confidence cap (§6.8).
- **PM5 — Additivity not audited.** No single place proved Phase 2 is additive-only against the five doors. **Resolved:** §8 additivity ledger (every new `signals` key; zero new `reason`s; zero `verifications` columns) + acceptance §11.10.
- **PM6 — HELO/verifier rotation dropped.** SVG §④ lists it; draft omitted it. **Resolved:** named as a P4 seam, inert against mocks, single fixed identity in P2 (§6.7).
- **PM7 — Mock-proof overclaim.** "Config flip = done" conflated code-path identity with real-world behavioral identity. **Resolved:** honest-limit note (§3.5) — mocks prove code-path identity, not provider behavior; that's P3/P7.

**Minors folded in:** `/config/reload` push-vs-pull secrets risk → pull recommended (OQ-P2-2/§4.5); transient-`unknown` caching would suppress the resolving retry → don't cache (OQ-P2-6/§6.5); `552` mailbox-full never `invalid`, matching the P7 bounce rule (§6.1.1) — *the risky-vs-valid lean was subsequently overridden to `valid` by the owner, Appendix D*; inconclusive probe never overwrites a known `catchall_domains` value (§5.6); wrapper never renders a verdict — returns engine facts only (§4.1, Door 5).

**What the review affirmed as already-right (preserved):** additive-only discipline against all five doors; every SMTP `reason` pre-reserved in P1 (no new enum values); transcript into the existing `verification_raw` sidecar (no new column, no append-only violation); engine-MX-authoritative / brain-MX-advisory DNS reconciliation (P1 OQ-24 honored); `config_fingerprint` completion via `/v1/config` (GAP-3 closed as P1 designated); harden-toward-`unknown` applied uniformly; correct scope discipline (queue/P3/P4 seams named, not built); two-substrate proof separation (GAP-11).

**Second (verification) review pass — 3 additional findings folded in.** A second adversarial pass re-checked PB1–PM7 as *actually resolved in the body* (confirmed) and hunted for what the first pass missed. Three new majors surfaced:
- **PM8 — Substrate can't reach `valid` with a random probe (`GAP-14`).** `smtp_mock` accept-all-by-default means a *random* catch-all probe is always 250'd, so the only `valid` path (§5.5#4) is untestable without a **seeded** probe pre-listed for rejection. **Resolved:** GAP-14 added (§3.3), the `valid`-path proof (Appendix B row 1) now names the seeded probe, and it is labeled a **test artifact** (faithful reject-arbitrary-unknown is a P3 real-box property) — so the mock `valid` proof is not mistaken for realism. Ties to OQ-P2-3.
- **PM9 — Catch-all truth-table gaps (§5.5).** No row for "target 250 but session died before the probe ran," and row 2 conflated *address verdict* with *domain knowledge*. **Resolved:** added row 7 (session-died-before-probe → `risky/plausible`, never `valid`); clarified row-2 precedence — a transient target is always `unknown`, but a probe-250 in the same session may still inform domain-level `catchall_domains`.
- **PM10 — GAP-14 not registered in §9 / proof matrix.** **Resolved:** GAP-14 added to the §9 table and the Appendix B substrate fixtures updated.

**Post-review status:** all PB/PM findings (PB1–3, PM1–10) resolved in-line across two adversarial passes. Eleven **OQ-P2** items (§10) remain for owner sign-off — none blocking the design, each carrying a recommendation. **OQ-P2-11 (GAP-13)** is the one genuine pre-build spike (can the wrapper reuse a session without patching the gem); **GAP-14** is the one substrate-fidelity limit that the `valid`-path proof must not overclaim. Both are flagged, not assumed.

---

## Appendix D — Owner decision log (Phase 2 sign-off)

All 11 **OQ-P2** questions signed off. **10 recommendations accepted** (one — OQ-P2-4 — conditioned on the GAP-13 spike); **1 spec recommendation overridden** (the `552` lean). Recorded here so the reasoning survives the conversation, mirroring the Phase-1 Appendix C.

### Override — `552 mailbox full`: `valid`/high-confidence, not `risky` (touches verdict correctness)
- **Spec had recommended:** `risky` in P2, valid-leaning later.
- **Owner ruling:** rejected the `risky` lean. A `552` is **positive proof the mailbox exists** — *a non-existent mailbox cannot be full* — making it the **strongest existence signal after a clean 250**. Leaning `risky` undersold it and was **inconsistent with its own Phase-7 destination** ("mailbox_full = valid"). Lean `valid` now.
- **Applied:** §6.1.1 rewritten (`valid`, high confidence, `recommendation=review` carries the "full-now" caveat — existence is a `status` fact, deliverability-timing a `recommendation` hint); propagated to §7.2 map, §7.3 policy gate, §7.4 confidence band, and Appendix B row 7. Not a door crossing (`mailbox_full` was reserved `risky/valid` in P1 — both statuses pre-approved).

### Accepted with reinforcement — OQ-P2-4 / OQ-P2-11 (GAP-13): Door 5 outranks same-session elegance
- **Owner ruling:** this is **the real risk in Phase 2**. **Run the spike before committing to Option A.** If the pinned gem cannot expose a reusable session without a patch, **accept Option B (two sessions) as a documented weaker signal — do not fork/patch the gem to get same-session.** "Just patch the gem a little" is explicitly disallowed. Door 5 (never fork the engine) is the tie-breaker.
- **Applied:** OQ-P2-4 marked *conditioned on the spike*; OQ-P2-11 marked *run early*; §5.4/§6.3 already frame Option B as the fallback and forbid the gem patch.

### Accepted + generalized — OQ-P2-6: never cache transient/your-fault `unknown`s
- **Owner ruling:** correct, and generalize it into a clean rule — **cache only settled verdicts; never cache your-fault or transient `unknown`s**, because caching a transient suppresses the retry that resolves it.
- **Applied:** §6.5 states the single predicate (settled-only); the OQ table records the generalization across `smtp_blocked`/`smtp_timeout`/`greylisted`/`connection_failed`/`smtp_error`.

### Accepted with a note — OQ-P2-5: transcript sensitivity feeds back to the P1 privacy posture
- **Owner ruling:** store verbatim (it's join gold), but note the `verification_raw` sidecar now contains **verifier identity + probe addresses** — slightly more sensitive than "just engine output." Worth **one line in the access-control runbook**.
- **Applied:** §6.4 carries the follow-up pointer to the P1 §3.3.3 runbook.
- **[AMENDED 2026-07-17 · owner-ratified · ADR-0008]:** the Phase-2 build + ratification review found this note under-scoped the sidecar's contents — it listed "verifier identity + probe addresses" but the wrapper response ALSO echoes the **target's own plaintext address** (the `email` field and 5xx-body echoes). Door 3 forbids persisting the raw full email, so "store verbatim" is narrowed to "store verbatim **except the redacted address identity**." The door governed the spec instruction (a spec-internal contradiction, not a door re-decision). See [ADR-0008](adr/ADR-0008-door3-raw-blob-address-redaction.md).

### Accepted as-is (batch): 
OQ-P2-1, 2, 3, 7, 8, 9, 10 — recommendations sound, accepted without change (OQ-P2-2 pull-not-push and OQ-P2-7 safe-check-off explicitly affirmed).

### Affirmed as correct (owner named these as the spec's strongest points, preserved verbatim):
- **The catch-all `valid` invariant** (§5.5 row 4 only — target 250 **AND** probe explicitly rejected) = the SVG rule made executable. *The single most important line in the spec.*
- **5xx anchored on truemail's actual `smtp_error_body_pattern`** rather than an invented classifier (§6.1) — lean on the engine's own heuristic, extend as a dial.
- **Row-2 address-verdict ≠ domain-knowledge separation** (§5.5) — a transient target stays `unknown`, but a probe-250 still informs domain catch-all.
- **GAP-14** (§3.3) — `smtp_mock` is accept-all-shaped, so the `valid` path is only testable with a seeded probe; the sharpest catch in either spec.

---

*End of Phase 2 (SMTP against mocks) Design Specification — **v1.0, APPROVED**. Two adversarial review passes folded in (Appendix C); all 11 OQ-P2 decisions signed off (Appendix D, one override: `552`→`valid`); OQ-P2-11/GAP-13 flagged as the run-early pre-build spike. Additive-only against the frozen Phase-1 contracts. Ready to build — with the GAP-13 spike gating the catch-all session design.*






