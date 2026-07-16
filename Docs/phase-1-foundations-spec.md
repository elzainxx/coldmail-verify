# Truemail Verification System — Phase 1 (Foundations) Design Specification

**Status:** APPROVED (v1.0) — all 25 open questions signed off 2026-07-15 · **Scope:** Phase 1 only · **Type:** Planning deliverable (no code, no migrations, no provisioning)
**Sources:** `truemail-build-plan.md`, `truemail-architecture-detailed.svg`, and the upstream `truemail` gem + `truemail-rack` server READMEs (captured in Appendix A).
**Review:** Critiqued by an adversarial database-architect pass and an architecture pass (Appendix B), then signed off by the owner with two overrules of the spec's recommendations — **OQ-7 flipped** (disposable → `risky`, not `invalid`) and **OQ-1 elevated** (pepper = the system's single most fragile artifact). Full decision log in Appendix C.

---

## 0. About this document

### 0.1 Purpose
Expand **Phase 1 — Foundations** of the build plan into a complete, buildable design specification. This document is what an engineer reads *before* writing the first line of Go, forking the rack, or creating a Supabase project. It fixes the contracts that cannot change later and describes — as design, not code — everything Phase 1 must contain.

### 0.2 Scope discipline
- **In scope:** Everything under build-plan §§1.1–1.6 (contracts, Supabase data model on paper, brain skeleton shape, cheap layers, policy engine + one default policy) — plus the five one-way doors at maximum rigor.
- **Out of scope:** Phases 2–7. Where a Phase 1 decision touches a later phase, this doc marks the **`SEAM →`** and moves on. It does not design the later phase.
- **No implementation:** no SQL/DDL, no Go source, no Dockerfiles, no provisioning. Field/type/purpose tables and interface *shapes* only.
- **Confirmed boundary — dedup is out of scope, by design.** The brain computes a stable `email_hash` (Door 3) that *enables* dedup, but **it does not deduplicate an input list** — list-level dedup is a platform/caller concern, not a verification-engine concern. This is a deliberate boundary, not an omission: the brain answers "what is this address?" one address at a time; deciding which addresses to ask about (and collapsing duplicates) belongs to whoever feeds the API.

### 0.3 How to read the tagging
Every non-trivial decision in this doc carries one tag:

| Tag | Meaning |
|---|---|
| **`LOCKED`** | A one-way door or an already-locked build-plan decision. Get it right now; additive-only afterwards. |
| **`DIAL`** | A build-time / runtime value you can turn later without breaking history or consumers. |
| **`DECISION REQUIRED`** | An open choice this doc surfaces with a recommendation but does **not** assume. Needs your sign-off before build. |
| **`SEAM →`** | A Phase 1 design point that deliberately leaves room for a later phase; noted, not built. |
| **`GAP`** | A contradiction or omission found *between the source documents (or vs. upstream truemail)* — flagged, not silently resolved. |

> **Priority rule (governs the whole doc):** the five one-way doors (§3) are specified in full. Everything else (§§4–7) is specified well enough to build and no further, because it is a dial.

### 0.4 Relationship to the source documents
This spec treats the build plan and the SVG as authoritative for *intent*, and the upstream truemail/truemail-rack READMEs as authoritative for *what the engine actually emits*. Where intent and engine reality diverge, §8 lists the gap; §9 lists the decision it forces.

---

## 1. Phase 1 at a glance

### 1.1 What Phase 1 delivers (from the build plan)
> Phase 1 done when: you can POST an address and get a scored, logged verdict using cheap layers only — **no port 25 anywhere.**

Phase 1 is deliberately the cheapest, least-reversible work first: it locks the contracts, stands up the data model, and builds the stateless brain skeleton with the no-SMTP layer stack. It is strong at proving an address **invalid** cheaply and honest about not being able to prove **valid** without SMTP (that arrives in Phase 2+).

### 1.2 The one-way-door priority map
The build plan names five one-way doors. This table is the reader's index into §3.

| # | One-way door | Locked in | Why it can't change |
|---|---|---|---|
| 1 | `/verify` response shape | §3.1 | External systems code against it. Versioned, additive-only. |
| 2 | The LOG record | §3.2 | Becomes historical data. Unlogged provenance is gone forever. |
| 3 | Email hashing + normalization | §3.3 | Changing it re-partitions every historical row; joins to future bounce data break. |
| 4 | `status` / `reason` vocabularies | §3.4 | Consumers branch on these values; renames silently corrupt downstream logic. |
| 5 | Node-list discipline + never-fork-engine | §3.5 | Hardcoding one endpoint or forking the engine is structurally expensive to undo. |

### 1.3 Inherited locked decisions (context, not re-litigated)
Carried verbatim from build-plan §0 — listed so this spec is self-contained:

- Engine = truemail **Ruby gem**, pinned, never forked. We fork **truemail-rack** only.
- Control-plane ("brain") language = **Go**. Database = **Supabase** (managed Postgres).
- Front door = **one authenticated, versioned API** (no UI, no CSV in v1).
- Boxes = **dumb, stateless, disposable**; the brain holds all state; boxes never talk to each other.
- Layer execution = **policy-driven & configurable**; default = cheapest-sufficient.
- Limit handling = blocked/limited/broken → **`unknown`, never `invalid`**.
- Cost budgeting dropped for v1; **health-based auto-quarantine kept** (IP protection).
- Scaling = **1..N by config**, same code.

---

## 2. System context for Phase 1

```
                    ┌───────────────────────────────────────────────┐
   caller ──POST──▶ │  BRAIN (Go, good VPS, port 25 CLOSED)          │
   /v1/verify       │                                               │
   (bearer token)   │  front door → cache → policy engine           │
                    │       → cheap layers (no port 25)             │
                    │       → [provider routing] → router (node-list)│
                    │       → scoring → LOG → cache write → respond  │
                    └───────┬───────────────────────────────┬───────┘
                            │ writes/reads                   │ HTTP (Phase 2+ path;
                            ▼                                ▼  stubbed/mocked in P1)
                    ┌───────────────┐              ┌──────────────────────┐
                    │  Supabase     │              │  node(s) = forked      │
                    │  (Postgres)   │              │  truemail-rack + gem   │
                    │  all state    │              │  "N nodes in a list"   │
                    └───────────────┘              └──────────────────────┘
```

**Phase 1 truth:** the box path exists in the *design* (router + node registry + node client) but no real SMTP box is contacted. Whether the cheap layers call a *mocked* engine or run *brain-native* is an explicit build-time open question (§9, OQ-6). Everything else in the diagram is live in Phase 1.

---

## 3. THE FIVE ONE-WAY DOORS (maximum rigor)

### 3.1 Door 1 — the `/verify` response contract (v1)

#### 3.1.1 Endpoint & versioning
- **Route:** `POST /v1/verify` **`LOCKED`** (URI-path versioning per build-plan §1.1 `/v1/`).
  - `DECISION REQUIRED` (OQ-12): URI-path vs header versioning. **Recommendation:** URI path (`/v1/…`) — simplest to reason about, visible in logs, matches the build plan.
- **Request body (v1):** `{ "email": "<string>", "policy": "<string, optional>" }`.
  - `email` **`LOCKED`** required. `policy` **`DIAL`** optional; defaults to the one shipped policy (§7.3).
  - `SEAM →` bulk (`/v1/verify/bulk` returning a job handle) is Phase 5; the singular endpoint is designed so a bulk sibling slots beside it. Not built now.
- **Auth:** bearer token on our front door (§3.5.4). Distinct secret from the per-node box token.

#### 3.1.2 The additive-only contract (the actual door)
These rules are the thing that is locked, more than any single field:

1. **Never remove a field.** Once shipped in v1, a field exists for the life of v1.
2. **Never rename a field.** A rename is a remove+add = a break.
3. **Never change a field's type or meaning.** `confidence` is always an integer 0–100; it never becomes a float or a string.
4. **Only add.** New fields, new `flags` keys, new `signals` keys, new `status`/`reason` enum values may be added within v1. (Enum additivity is Door 4, §3.4.)
5. **Consumers must tolerate the unknown.** The contract *requires* clients to ignore unknown object keys and to treat an unrecognized `status` as `unknown` and an unrecognized `reason` as opaque. This is published as part of v1 so additive changes are safe by construction. **`LOCKED`**
6. **A breaking change means `/v2/`**, served alongside `/v1/`, never in place.

#### 3.1.3 Field specification (v1)

Top-level object:

| Field | Type | Null? | Locked/Dial | Purpose |
|---|---|---|---|---|
| `api_version` | string | no | LOCKED | Always `"v1"`. Lets consumers assert the contract they parsed against. |
| `status` | enum string | no | LOCKED (§3.4) | The verdict: `valid \| invalid \| risky \| unknown`. |
| `reason` | enum string | no | LOCKED (§3.4) | Single dominant cause of the verdict. |
| `confidence` | integer 0–100 | no | LOCKED (range/type) | Calibrated trust in the verdict. Computation is a `DIAL` (§7.4). |
| `recommendation` | enum string | no | LOCKED (§3.4) | Action hint: `send \| review \| skip`. |
| `email` | string | no | LOCKED | Echo of the caller's input (see OQ-4 re: raw vs normalized). |
| `domain` | string | no | LOCKED | Normalized domain (punycode/lowercased). Present even on syntax fail if extractable, else `""`. |
| `flags` | object<string,bool> | no | LOCKED (as object) | Boolean facts about the address. Keys additive-only (§3.4.5). |
| `signals` | object<string,any> | no | LOCKED (as object) | Open, extensible evidence bag. Keys additive-only. |
| `meta` | object | no | LOCKED (as object) | Provenance echoed to the caller (subset of the LOG's provenance). |
| `raw` | object \| null | yes | LOCKED (as field) | Untouched engine output, nested. `null` when no engine was consulted (e.g., cheap-layer stop, or P1 mock). See GAP-2/GAP-9. |

**Cross-field composability invariants (LOCKED).** The four verdict fields do not vary independently; these rules are part of the contract so consumers can rely on them and so no future value combination silently contradicts:

| Invariant | Rule |
|---|---|
| status → recommendation | `invalid` ⇒ `recommendation ∈ {skip}`; `valid` ⇒ `{send, review}`; `risky` ⇒ `{review, skip}`; `unknown` ⇒ `{review, skip}`. `invalid`+`send` is a **forbidden** combination. |
| status → confidence direction | `invalid`/`valid` may carry high confidence (positive proof). `unknown` confidence expresses *uncertainty* and is bounded low; it is never inflated to dodge `unknown`. |
| reason ⊂ status | Each `reason` maps to exactly one primary `status` per the §3.4.3 table (a few are policy-dependent and annotated there). A `reason` never appears under a `status` its table row disallows. |
| from_cache orthogonal | `flags.from_cache`/`meta.from_cache` may be `true` under any status; it describes provenance, not the verdict. |

The recommendation-threshold *values* (which confidence cut turns `review` into `send`) are a `DIAL` (§7.2); the *directional invariants above are `LOCKED`*.

`flags` object (v1 initial keys — all booleans, additive-only):

| Key | Meaning |
|---|---|
| `is_role` | Local-part is a role account (`info@`, `sales@`, …). |
| `is_disposable` | Domain is a known disposable/DEA provider. Drives `recommendation=skip` under the cold-email policy; the address stays `risky` (deliverable), **never** `invalid` (OQ-7). |
| `is_free_provider` | Domain is a known free consumer mailbox provider. |
| `is_catchall` | Domain is known/detected accept-all. (`SEAM →` populated by the Phase 2 catch-all probe; in P1 sourced only from the `catchall_domains` table if present, else `false`.) |
| `has_typo` | A likely typo with a suggested correction exists (see `signals.suggestion`). |
| `is_spam_trap` | Address/domain matches a known spam-trap/honeypot list. |
| `is_whitelisted` | Matched a configured whitelist (engine or policy). |
| `is_blacklisted` | Matched a configured blacklist (engine or policy). |
| `from_cache` | This response was served from cache, not freshly computed. |

`signals` object (v1 initial keys — open bag, additive-only; any key may be absent):

| Key | Type | Meaning |
|---|---|---|
| `syntax_valid` | bool | Passed regex/format. |
| `mx_found` | bool | At least one usable mail server resolved. |
| `mx_provider` | string | Classified provider: `google \| o365 \| yahoo \| self \| other`. See GAP-2 (where MX host data comes from). |
| `null_mx` | bool | RFC 7505 null-MX present (domain explicitly accepts no mail). |
| `suggestion` | string | Suggested corrected address when `has_typo`. |
| `smtp_code` | string | Raw SMTP status token when SMTP ran (`SEAM →` P2). Parsed from the engine transcript; see GAP-4. |
| `greylisted` | bool | Transient 4xx greylist seen (`SEAM →` P2). |
| `platform` | string | Platform-signal provider (Gravatar/GitHub, etc.) (`SEAM →` later Type-2 layers). |

> **[CONSOLIDATION NOTE — 2026-07-17 · additive · assigns ownership to a dangling seam] `[PN-15 / SEAM FLAG-A, accepted]`**
> SEAM ledger (consistency report §3, FLAG-A): the reserved `signals.platform` key is **never populated by any phase 1–7** — the one reserved response-key with zero downstream owner. Harmless (the open `signals` bag tolerates absence), but it should not silently dangle. **Accepted:** name its owning phase **or** mark it explicitly `FUTURE / Phase-8+`. **Which resolution is a product-scope call — flagged, not decided** ([`DECISIONS.md`](DECISIONS.md) §5). The companion FLAG-C (catch-all re-probe cadence ownership) is noted at P2 §5.6. No key is added or removed; the reserved key stays reserved.

`meta` object (provenance echoed to caller — the LOG in §3.2 holds the authoritative superset):

| Key | Type | Purpose |
|---|---|---|
| `request_id` | string (uuid) | Correlates the response to its LOG row and logs. |
| `checked_at` | string (RFC 3339 UTC) | When the verdict was produced. |
| `latency_ms` | integer | Server-measured processing time. |
| `policy` | string | Policy key that decided this verdict. |
| `policy_version` | integer | Version of that policy. |
| `verifier_ip` | string \| null | IP of the node that ran SMTP; `null` in P1 / cheap-only. |
| `node_id` | string \| null | Node that served the engine call; `null` when no node consulted. |
| `engine_version` | string \| null | Composite engine identity (§3.2.4). `null` when no engine consulted. |
| `config_fingerprint` | string \| null | Fingerprint of the engine+layer+policy config (§3.2.5). |
| `from_cache` | bool | Mirrors `flags.from_cache`; present in meta for consumers that read provenance only. |

#### 3.1.4 Canonical example (illustrative — not a fixed golden value)

```jsonc
// POST /v1/verify   { "email": "Jane.Doe@Gmail.com" }
{
  "api_version": "v1",
  "status": "risky",
  "reason": "provider_no_smtp",
  "confidence": 35,
  "recommendation": "review",
  "email": "Jane.Doe@Gmail.com",
  "domain": "gmail.com",
  "flags": {
    "is_role": false, "is_disposable": false, "is_free_provider": true,
    "is_catchall": false, "has_typo": false, "is_spam_trap": false,
    "is_whitelisted": false, "is_blacklisted": false, "from_cache": false
  },
  "signals": { "syntax_valid": true, "mx_found": true, "mx_provider": "google", "null_mx": false },
  "meta": {
    "request_id": "5f0b…", "checked_at": "2026-07-15T09:00:00Z", "latency_ms": 42,
    "policy": "cold-email", "policy_version": 1,
    "verifier_ip": null, "node_id": null,
    "engine_version": null, "config_fingerprint": "cfg_sha256:9ab3…", "from_cache": false
  },
  "raw": null
}
```
> Note the P1-honest shape: a Gmail address routes to `:mx` (no SMTP), so the best truthful verdict is `risky/provider_no_smtp` at low confidence — not `valid`. This is the "harden toward unknown" principle visible in the contract.

---

### 3.2 Door 2 — the LOG record schema + provenance

The LOG (`verifications` table) is the point of the whole system. **Rule: log every result — including cheap-only verdicts and cache hits — before returning.** It is **append-only**: never updated, never deleted except by the retention policy (§3.2.6). It must be **joinable to future send/bounce outcomes** (Phase 7) without any schema change to existing columns.

#### 3.2.1 Design principles (locked)
1. **Log = response + verdict + provenance.** Everything in the `/verify` response, plus how it was produced.
2. **Provenance is mandatory from check #1:** `verifier_ip`, `engine_version`, `config_fingerprint`, `from_cache`, and the raw engine blob (in the `verification_raw` sidecar, §3.2.6).
3. **No PII in clear beyond `domain`.** The local-part is only ever stored hashed (§3.3). The raw full email is **never** persisted. `DECISION REQUIRED` (OQ-4) only concerns the *response echo*, not the LOG.
4. **`from_cache = true` rows are excluded from calibration** (per SVG) but are still logged (they are real answers we served).
5. **Immutable, and enforced as such.** Corrections are new rows, never mutations. Append-only is not just a convention: the intended enforcement is to **revoke `UPDATE`/`DELETE`** on `verifications` from the brain's service role (only `INSERT`/`SELECT`). This is *why* the mutable, retention-aged raw blob is moved out to the `verification_raw` sidecar (§3.2.6) — so nothing on `verifications` is ever updated in place.

> **[CONSOLIDATION NOTE — 2026-07-17 · additive · hardens Door-2 *enforcement*, does not change the Door-2 contract] `[PN-9 / F-13, accepted]`**
> Cross-phase review (F-13): grant-based immutability is only as strong as who holds the grant-admin credential. Whoever holds the Supabase **owner** cred can re-grant `UPDATE`/`DELETE` and rewrite history — including calibration ground-truth — with no tamper-evidence. **Accepted additive hardening:** hold DDL/grant-admin under a **separate credential with break-glass, admin-audited access** (rides the DR-3 admin-audit gate), distinct from the brain's runtime service role. **Flagged, not decided** ([`DECISIONS.md`](DECISIONS.md) §5): whether to add a periodic **hash-chain checkpoint of `verifications` to external WORM** for true tamper-evidence, or accept grant-based immutability with a documented residual (also [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md) §1.2). This **annotates enforcement**; the append-only log contract, the revoked-grant design, and the sidecar split are all unchanged. **Not built here.**

#### 3.2.2 Field specification (`verifications`)

| Field | Postgres type | Null? | Locked/Dial | Purpose |
|---|---|---|---|---|
| `id` | uuid (pk), **time-ordered (v7/ULID)** | no | LOCKED | Row identity; the authoritative join anchor for send-outcomes (§4.7). Time-ordered so high-rate append inserts stay locality-friendly (avoids random-UUIDv4 index fragmentation) — decided now to avoid a mixed-scheme index later. |
| `request_id` | uuid | no | LOCKED | Correlates to the returned `meta.request_id`. Not unique (replays/retries may repeat it on an append-only log — see §3.2.7). |
| `checked_at` | timestamptz | no | LOCKED | Verdict time, stored **UTC** (see GAP-8). |
| `email_hash` | **bytea (32-byte HMAC)** | **yes** | LOCKED (type+nullability) | Normalized-email HMAC (§3.3). The address's identity across history. **Nullable by design:** `invalid_syntax`/no-parse rows carry `NULL` (they are logged but not hashable — see §3.2.7). Stored as raw `bytea`, not hex text, so it is half the size and index cost on the system's most-indexed column, and byte-identical across the tables that must join. |
| `hash_scheme` | text | yes | LOCKED | Scheme tag, e.g. `norm/v1+hmac-sha256/v1`. Additive hedge (§3.3.6). `NULL` iff `email_hash` is `NULL`. |
| `domain` | text (guaranteed `norm/v1`, lowercase ASCII/punycode) | **yes** | LOCKED (type+nullability) | Normalized domain in clear — needed for per-domain analytics, cache, catch-all. **Plain `text`, not `citext`:** the value is already deterministically lowercased + IDNA-ToASCII by `norm/v1` (§3.3.2), so `citext` case-folding is redundant, slower, and locale-risky. `NULL` (not `''`) when no domain is extractable. A `CHECK (domain = lower(domain))` is the intended guard. |
| `mx_provider` | text | yes | LOCKED (promoted column) | Classified provider (`google\|o365\|yahoo\|self\|other`). **Promoted out of `signals` jsonb to a typed column** because it is the permanent grouping dimension of the Phase-7 flywheel ("recalibrate per (reason × provider)", "auto-promote a provider to `:mx`") and is computed for nearly every row from check #1. `NULL` when not yet classified. The `signals.mx_provider` copy is retained for response fidelity. |
| `local_part_len` | smallint | yes | DIAL | Optional non-reversible shape signal for analytics. |
| `status` | text (enum) | no | LOCKED | §3.4 vocabulary. |
| `reason` | text (enum) | no | LOCKED | §3.4 vocabulary. |
| `confidence` | smallint | no | LOCKED | 0–100. |
| `recommendation` | text (enum) | no | LOCKED | `send/review/skip`. |
| `flags` | jsonb | no | LOCKED | Full `flags` object as returned. |
| `signals` | jsonb | no | LOCKED | Full `signals` object as returned. |
| `policy_key` | text | no | LOCKED | Which policy decided. |
| `policy_version` | integer | no | LOCKED | Version of that policy. |
| `api_version` | text | no | LOCKED | Contract version that produced the row (`v1`). |
| `verifier_ip` | inet | yes | LOCKED | Node IP that ran SMTP; `null` in P1/cheap-only. |
| `node_id` | text | yes | LOCKED | Node that served the engine; `null` if none. |
| `engine_version` | text | yes | LOCKED | Composite engine identity (§3.2.4). |
| `engine_core_version` | text | yes | LOCKED | Gem core version, split out for bucketing (§3.2.4). |
| `config_fingerprint` | text | **no** | LOCKED | §3.2.5. **NOT NULL:** the P1 fingerprint always covers brain+policy config (§3.2.5 shows it is always computable), and it is the calibration/invalidation bucketing key + part of the cache key (§4.2). A nullable value would silently defeat both. |
| `from_cache` | boolean | no | LOCKED | Served-from-cache marker; calibration excludes `true`. |
| `engine_validation_type` | text | yes | LOCKED | truemail's own `validation_type` (`regex/mx/mx_blacklist/smtp/whitelist/blacklist`) — see GAP-7. |
| `engine_success` | boolean | yes | LOCKED | truemail's own top-level `success` boolean, preserved distinct from our `status`. |
| `latency_ms` | integer | yes | DIAL | Processing time. |
| `created_at` | timestamptz | no | LOCKED | Insert time (== `checked_at` normally; kept separately for audit). |

> **`raw_truemail_json` is NOT a column on `verifications`.** To keep the log truly append-only (see §3.2.1 and §3.2.6), the untouched engine blob lives in a **1:1 sidecar table `verification_raw(verification_id → raw jsonb)`**, so retention never UPDATEs a `verifications` row. See §3.2.6.

`SEAM →` **Phase 7 join:** send-outcome linkage is achieved via `email_hash` (address identity) and/or a dedicated join table (§4.7). No column here changes when that arrives — that is the point of locking `id` + `email_hash` now.

#### 3.2.3 What is deliberately NOT in the LOG
- Raw full email address (PII) — only `email_hash` + `domain`.
- Mutable verdict state — corrections are new rows.
- Any field that would require a rename later — when unsure, it goes into `signals`/`flags` jsonb rather than a new typed column, keeping the typed surface small and stable. **Exception (deliberate):** a value that is a *permanent analytic grouping dimension* (currently only `mx_provider`) is promoted to a typed column despite the "when unsure, jsonb" rule — because it is not unsure, it is load-bearing for the flywheel.

#### 3.2.7 Nullable-identity + immutability rules (LOCKED — these are one-way decisions)
Because `email_hash` and `domain` are `LOCKED` fields, their **nullability is itself a one-way door** and is fixed here:
- **`invalid_syntax` / unparseable rows are logged with `email_hash = NULL`, `hash_scheme = NULL`, and `domain = NULL`.** This resolves the otherwise-contradiction between "hash the normalized email" (§3.3.2 refuses to hash an unparseable address) and "log every result including the cheapest reject" (§6.10, acceptance-criterion #2). Analytics must therefore treat `email_hash IS NULL` as "un-addressable reject," never as an error.
- **No empty-string sentinels.** The absence of a value is always `NULL`, never `''` — so `GROUP BY domain` never mixes a sentinel with real data.
- **`request_id` is not unique.** On an append-only log, a client retry (OQ-21 allows client-supplied ids) may repeat a `request_id`; duplicate rows are acceptable and expected. Idempotency, if ever needed, is a caller concern, not a log constraint.

#### 3.2.4 `engine_version` — precise definition **`LOCKED`**
Sourced from the node's `/version` endpoint (Appendix A.2): `{ "core": "<gem>", "version": "<rack>", "platform": "<ruby>" }`.
- `engine_version` = `rack@<version>+gem@<core>` (e.g., `rack@0.10.0+gem@3.3.0`).
- `engine_core_version` = `<core>` alone, for coarse bucketing.
- `platform` is captured inside `raw`/provenance if needed, not promoted to a typed column.

#### 3.2.5 `config_fingerprint` — precise definition **`LOCKED`** (inputs) / `DECISION REQUIRED` (OQ-11, sourcing)
A stable hash identifying *which engine+config+policy produced this verdict*, so verdicts can be bucketed and selectively invalidated when config changes.

- **Algorithm:** `SHA-256` over a canonical (sorted-key, UTF-8) serialization of the fingerprint input set → stored as `cfg_sha256:<hex-prefix>`. **`LOCKED`**
- **Input set (verdict-affecting only):**
  - Engine: `default_validation_type`, `validation_type_for`, `smtp_safe_check`, `smtp_fail_fast`, `not_rfc_mx_lookup_flow`, `email_pattern`, `smtp_error_body_pattern`, `connection_timeout`, `response_timeout`, `connection_attempts`, `smtp_port`, `blacklisted_mx_ip_addresses`, whitelist/blacklist sets, `dns`.
  - Brain: the layer-config version and the enabled-layer set.
  - Policy: `policy_key` + `policy_version`.
- **`GAP-3` / OQ-11:** the truemail-rack validation response embeds only a *subset* of `configuration` (it omits `verifier_email`, `verifier_domain`, timeouts, `smtp_port`, `connection_attempts`). Therefore the fingerprint **cannot be computed from the response alone.** The node must expose its full effective config. **Recommendation:** the fork adds a `/config` (or extends `/version`) endpoint returning full effective config; the brain fingerprints that. `SEAM →` the fork endpoint is built in Phase 2; in Phase 1 (no live engine) the fingerprint covers brain+policy config and marks engine inputs as `unconfigured`.

#### 3.2.6 Retention & growth (build-plan §1.2 "note retention plan")
The biggest data grower is the raw engine blob, and retention must **not** violate append-only. The design separates the two:
- **`verifications` (structured columns):** retained **indefinitely** — small, and the calibration/accuracy dataset. Never updated (append-only enforced, §3.2.1).
- **`verification_raw(verification_id, raw jsonb)` — a 1:1 sidecar** holding the untouched engine JSON. It is the biggest grower and the *only* retention-managed store.
  - **Retention mechanism = drop, not update.** `verification_raw` is intended to be **RANGE-partitioned by time (monthly)** so aging out old blobs is an instant `DROP PARTITION`, not a mass in-place null (which would generate large WAL + dead tuples on a TOASTed column and would break the append-only guarantee). Declaring the partitioning from row #1 is nearly free; retrofitting a multi-billion-row table is a full rewrite. **`DIAL`** = the retention window (Recommendation **90 days**, OQ-10); **`LOCKED`** = the "separate sidecar, drop-don't-update" shape.
  - Because the structured `signals`/`flags`/`mx_provider`/`engine_*` columns already capture what we analyze, dropping an old blob loses only *re-parse* ability, not analytics.
- **TOAST/bloat note (`DIAL`, acknowledged now):** `flags`, `signals`, and the sidecar `raw` are jsonb and will TOAST; large-scale bloat/vacuum behavior is a known future tuning concern, explicitly deferred but flagged so it isn't a surprise.
- **Partitioning of `verifications` itself** (by `checked_at`) is a `DIAL` deferred to when volume warrants; unlike the raw blob it is never retention-dropped, so the dependency is weaker — but the same "declare early if you'll ever want it" logic applies (OQ-10).

---

### 3.3 Door 3 — email hashing + normalization scheme

This is the subtlest door: over-normalize and you **collide distinct real addresses** (wrong cache hits → wrong verdicts, forever); under-normalize and you miss dedup. The scheme is chosen once and frozen. This section proposes a concrete, fully-specified scheme and flags the two sub-decisions that need your sign-off.

#### 3.3.1 What gets stored (locked shape, per SVG)
- `email_hash` — hash of the **normalized full email**. The address's identity across all history.
- `domain` — stored **in clear** (needed for analytics/cache/catch-all). Only the **local-part is secret.**
- The raw full email is **never** stored.

#### 3.3.2 Normalization pipeline `norm/v1` **`LOCKED`** (deterministic, ordered)
1. Trim leading/trailing Unicode whitespace.
2. Require exactly one `@` (split on the **last** `@`); if not → `invalid_syntax`, address is **not** hashed/stored beyond `domain=""`.
3. **Domain side:** lowercase; strip a single trailing dot; apply IDNA/UTS-46 **ToASCII (punycode)**; Unicode **NFC**.
4. **Local side:** Unicode **NFC**; **lowercase** (see OQ-3 caveat); **do not** strip dots; **do not** fold `+tag` (these are provider-specific — handled separately in §3.3.5).
5. Recompose `normalized_email = local + "@" + domain`.

#### 3.3.3 Hashing `hmac-sha256/v1` **`LOCKED`** algorithm / `DECISION REQUIRED` (OQ-1) key strategy
- **Algorithm:** `email_hash = HMAC_SHA256(pepper, normalized_email)`. **Stored as raw `bytea` (32 bytes)**, not 64-char hex text — half the storage and btree-index size on the most-indexed column, and byte-identical across `verifications`, `cache`, and `send_outcomes` so the Phase-7 join is same-type. (Hex is a display concern only.)
- **Why HMAC, not plain SHA-256:** the email space is enumerable/low-entropy; a plain digest is reversible by brute force / rainbow tables. An HMAC with a secret **pepper** makes offline reversal infeasible without the key while staying **deterministic** (required — the hash is a dedup/cache/join key, so per-record salts and bcrypt/argon2 are disqualified).
- **DECIDED (OQ-1) — the pepper is the single most fragile artifact in the entire system.** More permanent than any schema decision: it is chosen once and can *never* change in place. The failure modes are asymmetric and both catastrophic:
  - **Lose it →** every historical `email_hash` de-joins. The dedup key, the cache key, and the entire Phase-7 flywheel linkage (`email_hash` rollups across `verifications` ↔ `send_outcomes`) become uncomputable forever. No recovery — the raw addresses were never stored.
  - **Leak it →** the local-parts become brute-forceable. The email space is low-entropy/enumerable, so an attacker with the pepper can recover addresses from hashes. The pepper is the *only* thing standing between "domain-in-clear" (acceptable) and "full-address-recoverable" (a breach).
  - **Custody: this gets its own backup/custody runbook — not a line in a secrets manager.** Minimum bar: generated once with strong entropy; stored outside the database; **redundant, geographically separate, access-audited backups**; documented recovery procedure tested before Phase 1 ships; access restricted and logged. Treat its loss/leak as a top-tier incident class in its own right.
  - **Rotation, if ever forced, is additive only** — a new `hash_scheme` tag + a dual-write window (§3.3.6), never a re-pepper in place. Assume in practice it is never rotated.
  - Trade-off accepted explicitly: a global pepper means two identical normalized emails share a hash (good for dedup/joins) but the DB alone can't reverse an address (good for privacy) — *provided the pepper stays secret*, which is why custody dominates everything else here.

#### 3.3.4 Domain retained in clear — confirmation
`domain` (plain `text`, `norm/v1`-normalized — see §3.2.2 for why not `citext`) is stored plaintext by design; it is not PII-sensitive the way the mailbox is, and every per-domain feature (cache, catch-all, provider stats, disposable) needs it. `LOCKED`.

#### 3.3.5 Provider-specific canonicalization — kept SEPARATE (the trap avoided) `DECISION REQUIRED` (OQ-2)
Gmail ignores dots and `+tags`; most providers do **not**. Baking dot-stripping/plus-folding into the frozen `email_hash` would collide distinct addresses on providers that treat them as distinct.
- **Recommendation:** do **not** fold provider quirks into the primary `email_hash`. If cross-alias dedup is wanted, compute a **separate** `provider_canonical_hash` (provider-aware: e.g., for gmail strip dots + drop `+tag`) as an **additional** column, never the primary identity.
- **OQ-2:** include `provider_canonical_hash` in Phase 1, or defer? **Recommendation:** since it is additive and populating it later still requires the raw address (which we never keep), **define AND populate the column from row #1** — a nullable `provider_canonical_hash bytea` on `verifications` (and mirrored on `cache`/`send_outcomes`), computed whenever the provider is known, `NULL` otherwise. The primary `email_hash` is unaffected either way. *(Resolves the prior inconsistency where this column was recommended in text but missing from the §3.2.2 field table — it is now explicitly part of the schema, added alongside `mx_provider`.)*

#### 3.3.6 Scheme versioning hedge **`LOCKED`**
Store `hash_scheme` (e.g., `norm/v1+hmac-sha256/v1`) on every LOG/cache row. This does **not** license changing the scheme — it is provenance so that *if* a future migration ever adds a second scheme **additively** (new column, dual-write), historical rows remain unambiguously attributable. The v1 scheme itself is frozen.

#### 3.3.7 Frozen caveats / open sign-offs
- **OQ-3 (local-part case):** RFC 5321 says the local-part *may* be case-sensitive; in practice essentially no provider treats it so. **Recommendation:** lowercase the local-part (fewer false distinct-address splits). Flagged because it is technically a semantic choice baked into a one-way door.
- **OQ-4 (response echo):** does the `/verify` `email` field echo the **raw input** or the **normalized** address? **Recommendation:** echo the raw input for caller UX; normalization/hashing remain internal. This does not affect the LOG (which never stores raw).

---

### 3.4 Door 4 — `status` / `reason` / `recommendation` vocabularies

**Rule: additive-only. New values are fine forever; renames and removals are never allowed.** Consumers branch on these strings.

#### 3.4.1 `status` — 4 values **`LOCKED`**

| Value | Meaning | Guarantee |
|---|---|---|
| `valid` | Mailbox is believed to exist and accept mail. | In P1, only reachable for a narrow set; SMTP-grade confidence is Phase 2+. |
| `invalid` | Proven not to exist / cannot receive. | Only from *positive proof* of non-deliverability (bad syntax, no MX, null-MX, confirmed no-such-user, gated list hit per policy). |
| `risky` | Deliverable-but-dangerous or low-trust. | catch-all, role (policy-dependent), accept-all provider 250, provider-routed-to-`:mx`. |
| `unknown` | Could not determine. | **First-class.** blocked / greylisted / timeout / limited / engine error. Never guessed away to avoid it. |

> **Invariant (from build-plan §0 & SVG):** blocked/limited/broken → `unknown`, **never** `invalid`. This is enforced in the policy engine's signal→status mapping (§7.5), not left to individual layers.

#### 3.4.2 `recommendation` — 3 values **`LOCKED`**
`send` · `review` · `skip`. (Per SVG.) The policy maps (status × confidence) → recommendation; the mapping thresholds are a `DIAL`.

#### 3.4.3 `reason` — frozen enum, additive-only **`LOCKED`**
One **single dominant** `reason` per verdict (OQ-15: single vs array — **Recommendation:** single primary reason; full multi-signal detail lives in `signals`). Grouped by originating layer. "P2+" rows are **reserved now** so the enum never needs a breaking addition later; they simply won't occur in Phase 1.

| `reason` | Typical `status` | Phase | Meaning |
|---|---|---|---|
| `invalid_syntax` | invalid | P1 | Failed format/regex. |
| `invalid_domain` | invalid | P1 | Domain malformed/non-resolvable as a domain. |
| `no_mx` | invalid | P1 | No usable mail server (no MX/A fallback). |
| `null_mx` | invalid | P1 | RFC 7505 null-MX — domain explicitly accepts no mail. |
| `dns_error` | unknown | P1 | DNS lookup failed/timed out (transient → not invalid). |
| `disposable` | **risky** | P1 | Known disposable/DEA domain. **A disposable mailbox is real and deliverable — just low-value; it is `risky`, never `invalid`** (OQ-7 decided). Conflating "won't deliver" with "don't want" would poison Phase-7 bounce calibration. Policy skips it via the `is_disposable` flag, not by faking a bad verdict. |
| `blacklisted_domain` | invalid | P1 | Configured domain blacklist hit. |
| `blacklisted_mx` | invalid | P1 | MX IP on configured blacklist. |
| `spam_trap` | invalid *or* risky | P1 | Known trap/honeypot (OQ-17 sourcing). |
| `role_account` | risky *or* flag-only | P1 | Role address; usually a flag, a `reason` only if policy gates it. |
| `whitelisted` | valid | P1 | Configured whitelist forced a pass (not a real check — see GAP-7). |
| `provider_no_smtp` | risky | P1 | Provider routed to `:mx` (Gmail/Yahoo/O365); cannot SMTP-confirm. |
| `plausible` | risky/unknown | P1 | Passed all cheap layers but unproven without SMTP (the honest P1 "maybe"). |
| `accepted` | valid | P2 | Clean 250 on a non-catch-all mailbox. |
| `mailbox_not_found` | invalid | P2 | SMTP 550 no-such-user (address fault). |
| `mailbox_full` | risky/valid | P2 | Mailbox exists but full. |
| `catch_all` | risky | P2 | Domain accepts all recipients; existence unprovable. |
| `accept_all` | risky | P2 | Provider 250s everything (worthless yes). |
| `greylisted` | unknown | P2 | Transient 4xx; needs retry (queue = P5). |
| `smtp_blocked` | unknown | P2 | Our IP/policy blocked (your-fault, not address-fault). |
| `smtp_timeout` | unknown | P2 | SMTP timed out. |
| `smtp_error` | unknown | P2 | Other SMTP failure not classifiable as address-fault. |
| `connection_failed` | unknown | P2 | Could not open session. |
| `external_verified` | (varies) | P6 | Verdict came from the third-party fallback (tagged external). |
| `historical_bounce` | invalid | P7 | Prior real bounce for this address. |
| `unknown_error` | unknown | P1 | Catch-all internal error; verdict withheld safely. |

**DECIDED (OQ-7):** in the cold-email default policy `disposable` is a **flag → `risky`**, *not* a gate → `invalid`. Rationale (owner overrule of the draft's recommendation): a disposable address is a real, deliverable mailbox — labeling it `invalid` conflates "won't deliver" with "don't want," and because this feeds the frozen `reason` vocabulary and the Phase-7 calibration set, a disposable that *would* have delivered but was logged `invalid` teaches the model the wrong thing. Keeping it `risky` + `is_disposable` keeps the verdict honest and the training data clean; the policy still *skips* it (recommendation `skip`) so cold-email behavior is unchanged operationally. (OQ-14: provider-routed-to-`:mx` in P1 → `risky` — decided. OQ-19: `whitelisted`/`blacklisted` engine outcomes map per §7.5 — decided.)

#### 3.4.4 Governance of the vocabularies **`LOCKED`**
- The enum lists live in **one** authoritative place (this doc + a single source-of-truth constant in the brain) — never duplicated per service.
- Adding a value = additive change, allowed any time, requires only that consumers already honor the "tolerate unknown" rule (§3.1.2 rule 5).
- Removing/renaming a value = forbidden; if a value becomes obsolete it is simply no longer emitted.

#### 3.4.5 `flags` keys are also additive-only **`LOCKED`**
The `flags` object's key set (listed in §3.1.3) follows the same additive-only discipline as the enums: new boolean keys may be added; existing keys are never removed, renamed, or repurposed.

---

### 3.5 Door 5 — node-list discipline + never-fork-engine

Two structural commitments that are cheap now and expensive to retrofit.

#### 3.5.1 Node-list discipline **`LOCKED`**
- The brain talks to a **list of nodes**, even when the list has length 1. There is **no** code path that references a single hardcoded endpoint.
- Routing goes through a **pluggable strategy interface** (§5.4) from day one; Phase 1 ships the trivial `random`/`single` strategy, but through the interface.
- The node registry is a **list-shaped config** from the first commit (§5.3).

#### 3.5.2 Never-fork-engine **`LOCKED`**
- The **truemail gem** is a **pinned dependency**, never patched. We fork **truemail-rack** (the HTTP server) only.
- **All** added behavior lives either in the rack wrapper (added endpoints, extended serialization) or in the **brain** (normalization, scoring, policy, provider classification). The gem's validation behavior is never altered.
- Consequence for design: the brain must treat the box as a **pure function** `email → raw engine JSON`. Because policy/scoring/normalization are entirely brain-side, the engine stays stock and **swappable** (e.g., a future swap to `truemail-go` changes only the node client's parsing, not the contracts). `SEAM →` engine-swap is not a Phase 1 activity but the boundary is drawn here.
- **Enforcement mechanisms (design intent):** pinned gem version in the fork's lockfile; a CI check asserting the vendored gem is unmodified; a written rule that any behavioral gap is closed by a brain-side transform, not a gem patch.

#### 3.5.3 Where the seam to the node actually is (Phase 1 reality)
In Phase 1 there is no live SMTP node. The design still includes the **node client** and **router** so that turning on a real box in Phase 3 is "a config value," but:
- `GAP-1`: build-plan §1.3 says "hit `/validate`". The stock truemail-rack has **no `/validate`** — it exposes `GET /?email=<addr>` (root), `/version`, `/healthcheck` (Appendix A.2). The node client must target `GET /` (or the fork's future `/verify`), not `/validate`. Flagged; §9 OQ notes the naming.
- The node client contract for Phase 1: `GET /?email=<urlencoded>` with header `Authorization: <node-token>`, returns the JSON in Appendix A.2.

#### 3.5.4 Auth seams (two distinct secrets) `DECISION REQUIRED` (OQ-13)
- **Front-door token** (caller → brain): **Recommendation** bearer token(s) on `/v1/verify`.
- **Node token** (brain → box): the rack's `ACCESS_TOKENS` presented via the `Authorization` header (raw token, not `Bearer …` — see Appendix A.2). These are different secrets with different rotation lifecycles; the design keeps them separate from day one.

---

## 4. Supabase data model (schema on paper)

Postgres via Supabase. All tables are **brain-owned**; boxes are stateless and never read/write the DB. Every table lists field/type/null/purpose and a locked-vs-dial tag. **No DDL is produced here** — this is the paper design.

> `DECISION REQUIRED` (OQ-20): access model. **Recommendation:** Phase 1 uses the **service-role** key from the brain only; Row-Level Security is not relied upon in P1 because there are no end-user JWT identities (the only client is the brain). Note this explicitly so it isn't mistaken for "RLS off forever." `SEAM →` if the API ever exposes tenant identities, RLS is designed then.

### 4.1 `verifications` (THE LOG) — fully specified in §3.2
The append-only log. See §3.2.2 for the complete field table.

**Indexing intent** (`DIAL` on exact set; some are load-bearing enough to name now): btree on `email_hash` (address history / rollup), on `domain` (per-domain analytics), on `checked_at` (time queries + eventual partitioning), and on **`mx_provider`** (the flywheel's grouping dimension — now a typed column, §3.2.2). Flag-based analytics (e.g. `is_disposable` rate) need either the promoted-column treatment or a GIN/expression index on `flags` — deferred as a `DIAL` but noted so it isn't discovered late.

**Storage-layer guards for the frozen vocabularies** (`LOCKED` intent): the enum-bearing `text` columns (`status`, `reason`, `recommendation`) and `confidence` carry **CHECK constraints** — `CHECK (confidence BETWEEN 0 AND 100)` and allowed-value checks on the enums. Text-over-native-PG-enum is the deliberate additivity choice (adding a `reason` value must not require an `ALTER TYPE`), but the CHECK guards stop a typo'd value from silently corrupting Door 4 at the storage layer. Adding an enum value = updating the CHECK (an additive, non-breaking migration).

### 4.2 `cache`
Serves the "cache lookup → return verdict (no probe)" step; primary cost control.

> **Correctness fix (was a latent bug).** A verdict is a function of **(address × policy × config)**, not the address alone. Keying the cache on `email_hash` only means a request under policy/config B could be served a verdict computed under policy/config A — a **silently wrong answer**. The cache key is therefore the **composite `(email_hash, config_fingerprint)`**, and `config_fingerprint` already folds in `policy_key`+`policy_version` (§3.2.5). This makes the cache correct-by-construction and makes config/policy changes naturally cache-invalidating (a new fingerprint = a cache miss = a fresh verdict).

| Field | Type | Null? | Locked/Dial | Purpose |
|---|---|---|---|---|
| `email_hash` | **bytea** (pk part) | no | LOCKED | Cache key part = the frozen hash (§3.3). Never `NULL` here — un-hashable rejects are not cached. |
| `config_fingerprint` | text (pk part) | **no** | LOCKED | Cache key part. Ties the cached verdict to the exact engine+layer+policy that produced it; always computable in P1 (§3.2.5). |
| `domain` | text (`norm/v1`) | no | LOCKED | For domain-scoped invalidation/analytics. Plain `text`, not `citext` (§3.2.2 rationale). |
| `mx_provider` | text | yes | DIAL | Mirror of the promoted column for provider-scoped cache analytics. |
| `status` | text (enum) | no | LOCKED | Cached verdict. |
| `reason` | text (enum) | no | LOCKED | Cached reason. |
| `confidence` | smallint | no | LOCKED | Cached confidence. |
| `recommendation` | text (enum) | no | LOCKED | Cached recommendation. |
| `flags` | jsonb | no | LOCKED | Enough to reconstruct the response. |
| `signals` | jsonb | no | LOCKED | Enough to reconstruct the response. |
| `hash_scheme` | text | no | LOCKED | Which scheme produced the key. |
| `cached_at` | timestamptz | no | LOCKED | For TTL math. |
| `expires_at` | timestamptz | **no** | DIAL (value) / LOCKED (non-null) | TTL (OQ-9). **NOT NULL** — a cache whose premise is that verdicts go stale must never hold a "never expires" row; the window is a dial, its presence is not. Intended index for TTL sweeps. |

**DECIDED (OQ-9):** **key = composite `(email_hash, config_fingerprint)`** (per-email × per-config; per-domain state lives in `catchall_domains`); `expires_at` always set (TTL *value* remains a `DIAL` to tune, since verdicts go stale as mailboxes change); and **a cache hit is still logged** to `verifications` with `from_cache = true`. Only the TTL *value* is a runtime dial; the key shape and re-log rule are locked.

### 4.3 `catchall_domains`
Per-domain accept-all state. Mostly `SEAM →` Phase 2 (the catch-all probe populates it); the table exists in Phase 1 and, if pre-seeded, feeds `flags.is_catchall`.

| Field | Type | Null? | Locked/Dial | Purpose |
|---|---|---|---|---|
| `domain` | text (`norm/v1`) (pk) | no | LOCKED | The domain. Plain `text`, `norm/v1`-normalized, same type as `verifications.domain` so joins stay same-type. |
| `is_catchall` | boolean | yes | LOCKED | Tri-state via null = unknown / not yet probed. **Sound in the table**; the footgun is only the `null → false` collapse into the response `flags.is_catchall` (see note below). |
| `detected_at` | timestamptz | yes | DIAL | When last determined. |
| `method` | text | yes | DIAL | How determined (`probe`, `seed`, `manual`). |
| `expires_at` | timestamptz | yes | DIAL | Re-probe cadence. |

> **Catch-all analytics must read this table, not the flag.** The response `flags.is_catchall` is a plain boolean (`null`/unknown collapses to `false`), so it cannot distinguish "probed, not catch-all" from "never probed." Any catch-all rate analysis reads `catchall_domains.is_catchall` (tri-state), never the flattened flag.

### 4.4 `disposable_domains`
Build-plan §1.5: "list in Supabase, not env." The source of truth for `is_disposable`.

| Field | Type | Null? | Locked/Dial | Purpose |
|---|---|---|---|---|
| `domain` | text (`norm/v1`) (pk) | no | LOCKED | Disposable/DEA domain. Same `text`/`norm/v1` type as everywhere else. |
| `source` | text | yes | DIAL | Where the entry came from (list name/version). |
| `added_at` | timestamptz | no | DIAL | Provenance/audit. |

`SEAM →` a `spam_trap` list is analogous; OQ-17 flags that trap lists are hard to source. **Recommendation:** model spam-traps as a sibling table `spam_trap_domains` (or a `type` column on a shared `list_entries` table) — decide at build time; the layer (§6.8) reads whichever shape is chosen.

### 4.5 `nodes`
Registry/persistence for boxes. **`GAP-6`:** build-plan §1.4 says the node registry is read from a **config list**, while §1.2 says create a **`nodes` table**. These are two candidate sources of truth.
`DECISION REQUIRED` (OQ-5). **Recommendation:** in Phase 1 the **config-list is the source of truth for routing** (satisfies node-list discipline with one mock entry); the `nodes` table exists as a **mirror + home for audit/health history** that becomes authoritative in Phase 4. Documented so the duplication is intentional, not accidental.

| Field | Type | Null? | Locked/Dial | Purpose |
|---|---|---|---|---|
| `id` | text (pk) | no | LOCKED | Stable node id (also written to the LOG's `node_id`). |
| `label` | text | yes | DIAL | Human name. |
| `base_url` | text | no | DIAL | Where the rack is reached. |
| `enabled` | boolean | no | DIAL | Operator on/off. |
| `status` | text | no | DIAL | `provisioning \| active \| quarantined` (lifecycle per SVG ⑦). |
| `port25_open` | boolean | yes | DIAL | Capability flag; `false`/`null` for the P1 mock. |
| `last_audit_at` | timestamptz | yes | SEAM → | `/audit` gate is Phase 3. |
| `audit_status` | text | yes | SEAM → | green/red; Phase 3. |
| `created_at` | timestamptz | no | DIAL | Provenance. |

Intended constraints/notes: `UNIQUE(base_url)` (two rows must not point at the same box); `verifications.node_id` is deliberately **FK-less text** (an append-only log row must survive a node being deleted from the registry — the id is a historical fact, not a live reference), so a logged `node_id` may reference a since-removed node. One authoritative sync direction: **config-list seeds the `nodes` table at boot** (OQ-5), never the reverse.

> The node's **auth token is not stored here in clear.** `DECISION REQUIRED` embedded in OQ-13: node tokens live in the secrets manager / config, referenced by id — not persisted plaintext in `nodes`.

### 4.6 `policies`
Stores policy definitions (§7). One row in Phase 1.

| Field | Type | Null? | Locked/Dial | Purpose |
|---|---|---|---|---|
| `key` | text (pk part) | no | LOCKED | Policy identifier, e.g. `cold-email`. |
| `version` | integer (pk part) | no | LOCKED | Immutable version; new version = new row. |
| `definition` | jsonb | no | LOCKED (shape §7.2) | Ordered layers, gate-set, stop-early, flags, weights, provider routing. |
| `is_default` | boolean | no | DIAL | Marks the **single system default** policy. |
| `created_at` | timestamptz | no | DIAL | Provenance. |

> Policies are **versioned and immutable** (like the contract): editing a policy = inserting a new `version`, so every LOG row's `(policy_key, policy_version)` always resolves to the exact rules that produced it.
>
> **Single-default constraint (LOCKED semantics for v1 — OQ-23 signed off).** `is_default` means *the one system-wide default policy+version* used when a request names no policy, guarded by a **partial unique index `UNIQUE (is_default) WHERE is_default`** (exactly one default row across the whole table).
>
> **`SEAM →` tiers (do not let this harden).** A single system-wide default is correct for v1, but tiers (e.g. Normal/Premium) are expected to map to *different* policies later. When that arrives, "the default" becomes tier-scoped (default *per tier*), not global. This is a known seam, not a permanent truth — flagged here so the v1 single-default assumption is revisited rather than silently inherited when tiers land. The `policies` table shape (`key`,`version`,`definition`) already accommodates multiple policies; only the *default-selection* rule changes.

### 4.7 `send_outcomes` + join table — empty schema now (`SEAM →` Phase 7)
Created empty in Phase 1 so the shape exists; **filled only when the sender comes online.** No Phase 1 code writes here.

> **The join must be verdict-level, not address-level.** `email_hash` identifies an *address*, but the append-only log has **many rows per address over time** (different dates, policies, configs). The build plan's Phase-7 requirement is to join a bounce to *"the verdict that allowed the send"* — a specific row, not the whole address history. Therefore the **authoritative linkage is `verifications.id`** (via the join table below or a direct FK); `email_hash` on an outcome is only an **address-rollup convenience**, never the primary join. Getting this wrong would fan a single bounce out across every historical verdict for the address and make per-verdict accuracy uncomputable.

`send_outcomes` (sketch):

| Field | Type | Purpose |
|---|---|---|
| `id` | uuid (pk, time-ordered) | Outcome identity. |
| `verification_id` | uuid (FK → `verifications.id`) | **The authoritative link to the exact verdict that permitted the send.** Nullable only if a send ever occurs with no prior verdict on record. |
| `email_hash` | bytea | Address-rollup convenience only (same `bytea` type as `verifications`). Not the primary join. |
| `provider_canonical_hash` | bytea | For alias-aware rollups (mirrors §3.3.5). |
| `hash_scheme` | text | Disambiguates `email_hash` if the scheme is ever additively migrated (§3.3.6) — "gone forever if not captured from row #1." |
| `domain` | text (`norm/v1`) | Needed for the flywheel's per-domain/per-provider rollups without forcing every rollup through the address join. |
| `mx_provider` | text | Provider dimension for "auto-promote a provider to `:mx`" (P7) — captured at outcome time. |
| `message_id` | text | Sender's message id. |
| `event` | text | Bounce/complaint/delivered class (classified in P7). |
| `sent_at` | timestamptz | When the message was sent — required for temporal attribution (which verdict was current at send time). |
| `occurred_at` | timestamptz | When the outcome (bounce/etc.) happened. |
| `raw` | jsonb | Untouched provider event. |

> **[CONSOLIDATION NOTE — 2026-07-17 · additive · flags an open enforcement site, decides nothing] `[PN-14 companion, accepted]`**
> Schema review (consistency report §5, note 2): P7 §3.4 locks the ingestion dedup **key** = `(message_id, event)`, but no phase pins **where the guard lives** — a DB `UNIQUE` constraint on `send_outcomes(message_id, event)`, an app-level append-guard, or a staging table. Since this table is still *empty on paper*, this is the natural place to record the question. **Flagged, not decided** ([`DECISIONS.md`](DECISIONS.md) §5) — choosing an enforcement site is a new schema commitment, an owner call. The calibration-store column sketch and the `residue_class` typed-vs-jsonb question are noted at P7 §5.4. No column here changes.

`verification_outcomes` join table (sketch): `verification_id → send_outcome_id` (many-to-many, same-type uuid FKs both sides), so a verdict can be linked to one or more later outcomes without altering `verifications`.
> This is the flywheel seam (build-plan Phase 7 / SVG "join table needs sender"). Locking `verifications.id` (Door 2) as the join anchor and `email_hash` (Door 3) as the rollup key **now** — and giving `send_outcomes` its own `domain`/`mx_provider`/`hash_scheme`/`sent_at` — is exactly what makes the flywheel computable **later** with zero migration of existing `verifications` columns. (The zero-change guarantee is about `verifications`; the outcomes-side shape is designed correctly here so *it* never needs a breaking change either.)

---

## 5. Brain skeleton shape (Go) — design, not code

Described as modules, responsibilities, and interface *shapes*. No Go source. Emphasis: statelessness (all state in Supabase), interface-first boundaries (router, layers), and the pure-function view of nodes.

### 5.1 Module map
```
cmd/brain                 process entry: load config, wire deps, serve
internal/api              HTTP front door: /v1/verify, auth, request/response codec
internal/policy           policy engine: load policy, plan, evaluate signals → verdict
internal/layers           cheap layers, each implementing the Layer interface
internal/router           NodeRegistry + Strategy interface (node-list discipline)
internal/node             node client: email → raw engine JSON (pure-function view)
internal/store            Supabase/Postgres client: verifications (+verification_raw sidecar), cache, lists, policies, nodes
internal/verdict          scoring + response/LOG assembly (single source for both)
internal/hash             frozen normalization + HMAC (Door 3), scheme-tagged
internal/vocab            the frozen enums (Door 4) — one authoritative constant set
```

### 5.2 API front door (`internal/api`)
- Serves `POST /v1/verify`; token-auth middleware (§3.5.4); decodes `{email, policy?}`.
- Delegates to an orchestrator that runs the request lifecycle (SVG ①): cache → policy plan → cheap layers → provider routing → (router/node — stubbed P1) → scoring → **LOG** → cache write → respond.
- Emits structured logs + a `request_id` per call. Observability specifics are a `DIAL`; the seam (metrics for block-rate/unknown-rate/cache-hit) is Phase 4.
- **Auth stub note:** Phase 1 may accept a static bearer token from config; richer auth is a `DIAL`.

### 5.3 Node registry (`internal/router`) — config-list **`LOCKED`**
- Reads a **list** of node descriptors from config (one mock entry in Phase 1): `{ id, base_url, token_ref, enabled, capabilities }`.
- Exposes the list to the router. Never exposes "the endpoint." (Door 5.)
- OQ-5 governs config-list vs `nodes`-table authority.

### 5.4 Router with pluggable strategy (`internal/router`) — interface-first **`LOCKED`**
- Interface shape (conceptual): `Strategy.Pick(ctx, plan, candidates []Node) (Node, error)`.
- Phase 1 strategy: `single`/`random` over the (length-1) candidate list.
- `SEAM →` sticky-by-domain (default) + least-inflight fallback are Phase 4; they are new `Strategy` implementations, no interface change.

### 5.5 Node client (`internal/node`) — pure-function view **`LOCKED`**
- Contract: `Verify(ctx, node, email) (RawEngineResult, error)`.
- Phase 1: **not called against a live box.** Whether it hits a *mocked* engine or is bypassed entirely is OQ-6. The parsing target is the Appendix A.2 JSON (GAP-1: `GET /?email=`, not `/validate`; GAP-2: no `domain`/`mail_servers` over the wire).
- Keeps *all* interpretation out of the box — the client returns raw JSON; the brain interprets.

### 5.6 Supabase client (`internal/store`)
- Write: `verifications` (every result) + `verification_raw` sidecar (the engine blob). Read/write: `cache`. Read: `policies`, `disposable_domains`, `spam_trap` list, `catchall_domains`, `nodes` (mirror).
- All writes to `verifications` are **inserts only** — the service role's grants are intended to *lack* `UPDATE`/`DELETE` on it (§3.2.1). Retention operates on `verification_raw` partitions (`DROP PARTITION`), never on `verifications` rows (§3.2.6).

### 5.7 Layer runner + verdict assembler (`internal/layers`, `internal/verdict`)
- Each layer implements a common interface (§6.1) and emits a `LayerOutput` into a signals bag.
- The verdict assembler is the **single** place that builds both the `/verify` response and the LOG record from the same in-memory verdict — guaranteeing the LOG is "response + provenance" and they never drift.

### 5.8 Statelessness & scaling
- The brain holds **no** per-request state beyond the request; all durable state is in Supabase. This satisfies "1..N by config, same code" for the brain itself (though Phase 1 runs a single brain).

---

## 6. Cheap layers (no port 25)

Each layer is specified as: **purpose · input · output · signal(s) emitted · gate/flag nature.** Per the task, this doc does **not** decide mock-vs-live execution — that is a build-time call flagged as **OQ-6** and noted per layer where relevant. Layers map to the SVG's Type-1 (core, gates the verdict) and Type-2 (additional, adjusts/flags) split.

### 6.0 Layer output shape (build-plan §1.6) **`LOCKED`**
`LayerOutput { layer_id, signal_key, result ∈ {pass,fail,unknown,flag,skip}, severity_hint ∈ {info,warn,critical}, value (any), detail (string) }`.
- The layer reports a finding + a *hint*; the **policy** decides whether that finding gates, flags, or stops early (§7). Layers never decide the final verdict.
- `unknown` from a layer (e.g., DNS timeout) propagates toward a `unknown` status, never `invalid` (Door 4 invariant).

### 6.1 Syntax / regex — Type-1 (core, gate)
- **Purpose:** reject malformed addresses; produce the normalized form.
- **Input:** raw email. **Output:** pass/fail + normalized email + extracted domain.
- **Signals:** `syntax_valid`. **Nature:** gate → `invalid_syntax` on fail (stop-early).
- **Mock/live (OQ-6):** brain-native regex, or the engine's `:regex`. **Overlap note:** truemail also does regex; running both duplicates. Recommendation leans brain-native for the cheap path (no engine round-trip), flagged.

### 6.2 Typo / suggestion — Type-2 (flag)
- **Purpose:** detect likely misspelled domains, suggest a correction.
- **Input:** domain. **Output:** suggestion or none.
- **Signals:** `has_typo` (flag), `suggestion`. **Nature:** flag only (never gates).
- **Open:** OQ-18 — source of the known-good domain list (built-in vs Supabase-backed).

### 6.3 Role account — Type-2 (flag; gate only if policy says so)
- **Purpose:** identify role addresses (`info@`, `admin@`, `sales@`, …).
- **Input:** local-part. **Output:** is_role. **Signals:** `is_role`. **Nature:** flag by default; the cold-email policy may treat it as a soft signal (OQ-7 relatives). List-driven (built-in role list; `DIAL`).

### 6.4 Free-vs-corp — Type-2 (flag)
- **Purpose:** classify free consumer mailbox domains vs corporate.
- **Input:** domain. **Output:** is_free. **Signals:** `is_free_provider`, feeds `mx_provider`. **Nature:** flag. List-driven.

### 6.5 MX / DNS — Type-1 (core, gate)
- **Purpose:** confirm a usable mail server exists (RFC MX lookup: MX → CNAME → A per truemail).
- **Input:** domain. **Output:** has_mx + mail-server hosts + provider classification input.
- **Signals:** `mx_found`, `mx_provider`. **Nature:** gate → `no_mx` (invalid) on none; DNS failure → `dns_error` (**unknown**, not invalid).
- **`GAP-2`:** the truemail-rack JSON does **not** return `mail_servers`/`domain`, so provider classification cannot rely on the engine's over-the-wire output. **Recommendation:** the brain does its **own** MX/DNS resolution for the cheap path (it needs the hosts anyway for provider classification and null-MX). OQ-6/OQ-11 related. Flagged.
- **`GAP-10` (divergent-DNS risk — architecture seam to P2).** Once SMTP arrives (P2), there are **two independent DNS resolvers**: the brain's (cheap path, provider classification, null-MX) and the engine's inside the box (which does its own MX lookup before RCPT). They can disagree (different resolvers, caches, timing) → the brain may classify `mx_found` while the box finds none, or route on a provider the box's MX contradicts. **Recommendation / seam:** in P1 this is latent (no box). Flag it now so P2 decides the reconciliation rule — e.g., the box's MX view is authoritative for the SMTP verdict and the brain's is advisory for routing, with any disagreement captured in `signals` for calibration. **OQ-24** records this; not resolved here (it's a P2 decision), but the seam is named so P2 can attach cleanly.

### 6.6 Null-MX (RFC 7505) — Type-1 (core, gate)
- **Purpose:** detect an explicit null-MX (a single `.` / empty target, priority 0) meaning "this domain sends/accepts no mail."
- **Input:** domain MX records. **Output:** null_mx bool. **Signals:** `null_mx`. **Nature:** gate → `null_mx` (invalid).
- **`GAP`/clarification:** truemail's `not_rfc_mx_lookup_flow` config is **not** the same as RFC 7505 null-MX detection; it changes lookup breadth. Detecting the null-MX sentinel is **brain-native** here. Marked "add" in the SVG. Flagged so it isn't conflated with an engine flag.

### 6.7 Disposable / DEA — Type-2 (flag → `risky`; policy skips, never gates to invalid)
- **Purpose:** identify disposable/DEA addresses so policy can *skip* them — without lying about deliverability.
- **Input:** domain. **Output:** is_disposable. **Signals:** `is_disposable`. **Source:** `disposable_domains` table (§4.4), not env.
- **Nature (OQ-7 DECIDED):** **flag → `risky`** with `reason=disposable` and `recommendation=skip`. **Not** a gate to `invalid` — a disposable mailbox is real and deliverable, so calling it `invalid` would corrupt the frozen `reason` vocabulary's meaning and poison Phase-7 bounce calibration (§3.4.3). The cold-email policy achieves "don't send to disposables" through the skip recommendation, keeping the *verdict* honest and the *training data* clean. This is a deliberate departure from a hard gate precisely because it touches a one-way door.

### 6.8 Spam-trap / honeypot — Type-1 (gate)
- **Purpose:** avoid known traps (sending to these damages sender reputation).
- **Input:** email/domain. **Output:** is_spam_trap. **Signals:** `is_spam_trap`. **Nature:** gate. **Source:** a trap list table (§4.4 note).
- **Open:** OQ-17 — trap lists are genuinely hard to source; the layer is specified but its data source is flagged as an unresolved acquisition problem.

### 6.9 Provider classification — Type-2 (drives routing)
- **Purpose:** classify the mailbox provider from MX/domain → `google | o365 | yahoo | self | other`.
- **Input:** domain + MX hosts. **Output:** provider class. **Signals:** `mx_provider`. **Nature:** not a gate; **feeds provider routing** (Gmail/O365/Yahoo → `:mx`).
- **`SEAM →` Phase 3:** the *effect* of routing (skip SMTP for big providers) matters once SMTP exists. In Phase 1 the classification is computed and logged, and yields `provider_no_smtp`/`risky` for those providers (OQ-14).

### 6.10 Cross-cutting: **LOG every result** **`LOCKED`**
Even a syntax-only rejection is logged (build-plan §1.5: "The log is the point"). The verdict assembler (§5.7) writes the LOG before responding, cheap verdicts included.

---

## 7. Policy engine + the one default policy

### 7.1 Model: signals-plus-policy **`LOCKED`** (build-plan §1.6)
- Layers emit `LayerOutput`s (§6.0) into a signals bag.
- The **policy** owns the decisions: which layers run, in what order, which results **gate** (stop with a verdict), which merely **flag** (adjust confidence), and where to **stop early** (cheapest-sufficient).
- The engine is **table-driven**: a policy is data (JSONB in `policies`), not code — so "more policies slot in" without new code (build-plan §1.6).

### 7.2 Policy definition shape (`policies.definition` JSONB) — shape **`LOCKED`**, contents **`DIAL`**
```jsonc
{
  "order": ["syntax", "mx", "null_mx", "disposable", "spam_trap",
            "provider_classify", "role", "typo", "free_corp"],
  "gates": {                       // layer result → action
    "syntax:fail":     "invalid:invalid_syntax:stop",
    "mx:fail":         "invalid:no_mx:stop",
    "null_mx:pass":    "invalid:null_mx:stop",
    "spam_trap:pass":  "invalid:spam_trap:stop",
    "mx:unknown":      "unknown:dns_error:stop",
    "disposable:pass": "risky:disposable:stop"          // OQ-7 DECIDED: risky, NOT invalid — a disposable mailbox is real/deliverable
  },
  "flags": ["role", "typo", "free_corp", "disposable"], // adjust confidence / set is_* flag, never fake an invalid
  "recommendation_overrides_by_reason": { "disposable": "skip" }, // policy skips disposables without lying about deliverability
  "provider_routing": { "google": "mx", "yahoo": "mx", "o365": "mx" },
  "confidence": { "baseline_by_reason": { "...": 0 }, "flag_deltas": { "...": 0 } },
  "recommendation_thresholds": { "send": 70, "review": 30 }  // DIAL
}
```
> The action grammar `"<status>:<reason>:<stop|continue>"` is the locked shape; the specific mappings are dials a policy author sets. Note the `disposable` outcome is `risky` (not `invalid`) and is steered to `skip` via `recommendation_overrides_by_reason` — the cold-email policy still refuses to send to disposables, but the *verdict* stays truthful so Phase-7 calibration isn't poisoned (OQ-7). The `skip` result on a `risky` status is consistent with the §3.1.3 composability invariant (`risky ⇒ {review, skip}`).

**Forward-expressiveness check (does this shape survive Phases 4–7 additively?).** The definition is an **open JSONB object**, so new *keys* can be added without a shape change — which is what keeps it additive. The later-phase needs map to new keys, not new grammar:
- P4 per-provider rate/jitter + sticky routing → a new `routing`/`rate_limits` key (the `Strategy` picks nodes; the policy only supplies parameters).
- P6 external fallback → the external verifier is just another entry in `order` + a `gates`/`flags` rule; `reason=external_verified` is already reserved (§3.4.3).
- P7 historical-bounce gate → a new layer id in `order` + a `gates` rule (`historical_bounce:pass → invalid:historical_bounce:stop`, already reserved).
- **Locked now:** `order`, `gates` (with the action grammar), `flags`, `provider_routing`, `confidence`, `recommendation_thresholds` as *open keys*. **`SEAM →`** the P4–P7 keys are **not** designed here; they are noted as additive extension points so the JSONB shape itself never needs to break. `DECISION REQUIRED` **OQ-25**: confirm the policy definition is treated as an open/extensible object (unknown keys tolerated by the evaluator) rather than a closed struct — the same "tolerate unknown" discipline as the response contract.

### 7.3 The one shipped policy: `cold-email` (cheapest-sufficient) — v1
- **Intent:** for cold outreach, cheaply eliminate the clearly-bad and clearly-risky; do not pretend to prove "valid" without SMTP.
- **Order & gates:** as in §7.2 — syntax → MX → null-MX → spam-trap gate hard to `invalid`; **disposable stops to `risky` + `skip`** (not `invalid` — OQ-7); provider classification routes big providers to `:mx`; role/typo/free are confidence flags.
- **Cheapest-sufficient behavior:** stop at the **first** outcome that fires (a hard `invalid` gate, or the `risky:disposable` stop); if nothing fires and the provider isn't SMTP-routed, the honest P1 outcome is `risky`/`plausible` (or `provider_no_smtp` for big providers) — **not** `valid`, because SMTP (Phase 2) is required for a confident yes.
- **Decisions inside this policy (all signed off):** OQ-7 (disposable → `risky`+`skip`, **overruled** from the draft's `invalid` gate), OQ-14 (provider-routed → `risky`), OQ-19 (whitelist/blacklist mapping per §7.5).

### 7.4 Confidence in Phase 1 — `DIAL` / `DECISION REQUIRED` (OQ-8)
- **Type/range:** integer 0–100 (Door 1). **Computation:** `DIAL`.
- **P1 recommendation:** a **fixed baseline-by-reason table** (not a learned model — there's no outcome data yet), nudged by flag deltas. Reflecting P1's honest ceiling:
  - High confidence is reserved for **invalids** with positive proof (`invalid_syntax`, `no_mx`, `null_mx`, list hits) → e.g. 90–99.
  - "Passed cheap layers but unproven" (`plausible`, `provider_no_smtp`) is **capped low** (e.g. ≤ 40) — because without SMTP we have not proven the mailbox exists.
  - `unknown` → confidence expresses uncertainty (low), never inflated to avoid `unknown`.
- `SEAM →` Phase 7 recalibrates confidence per (reason × provider) from real bounce outcomes; the fixed table is explicitly a placeholder for that.

### 7.5 Signal → status mapping invariants **`LOCKED`**
- blocked/limited/broken/timeout/greylist → **`unknown`** (never `invalid`).
- Only **positive proof of non-deliverability** → `invalid`. **A low-value-but-deliverable signal is not proof of non-deliverability** — hence disposable → `risky`, never `invalid` (OQ-7). This is the boundary that keeps the `invalid` label meaning exactly one thing for Phase-7 calibration.
- accept-all / catch-all / provider-`:mx` / disposable / role(if flagged risky) → `risky`.
- **`GAP-7` / OQ-19 (DECIDED):** engine `validation_type ∈ {whitelist, blacklist}` are **configuration overrides, not verifications.** Map `whitelist → valid` + set `flags.is_whitelisted`, and `blacklist → invalid`/`blacklisted_domain` + set `flags.is_blacklisted`, with confidence marked as override-derived so **calibration excludes them** (same principle as OQ-7: don't teach the model from a verdict that wasn't a real check).

---

## 8. Contradictions & gaps found in the source material (flagged, not resolved)

Per the requirement to surface — not quietly fix — inconsistencies between the build plan, the SVG, and upstream truemail reality:

| ID | Where | Gap | Handling in this spec |
|---|---|---|---|
| **GAP-1** | build-plan §1.3 "hit `/validate`" | Stock truemail-rack has **no `/validate`**; it exposes `GET /?email=`, `/version`, `/healthcheck` (Appendix A.2). | Node client targets `GET /` in P1; a fork-added `/verify` is a Phase 2 seam. Naming flagged (OQ-relations §3.5.3). |
| **GAP-2** | SVG `signals {mx_provider,…}`, LOG `domain` | The rack JSON **omits `domain` and `mail_servers`** (the gem struct has them; the `ValidatorJson` serializer drops them). | Brain does its own MX/DNS for the cheap path (§6.5). Flagged; ties to OQ-6/OQ-11. |
| **GAP-3** | provenance `config_fingerprint` | The rack response's `configuration` block is a **subset** (no verifier email/domain, timeouts, port, attempts) → fingerprint can't be computed from the response alone. | Fork exposes full effective config (Phase 2 seam); P1 fingerprints brain+policy only. §3.2.5, OQ-11. |
| **GAP-4** | SVG/LOG `smtp_code` | truemail returns SMTP status only as **raw strings** (`"550 5.7.1 No such user!\n"`), no structured numeric code. | Numeric-code extraction is our parsing job (Phase 2); `signals.smtp_code` holds the parsed token. Flagged. |
| **GAP-5** | build-plan §1.5 cheap layers | disposable/spam-trap/typo/role/provider-class/null-MX are **not** truemail features. | Specified as **brain-owned** layers (§6). Not a contradiction, but an ownership fact worth stating. |
| **GAP-6** | build-plan §1.2 vs §1.4 | Node registry: **`nodes` table** (§1.2) vs **config-list** (§1.4). | Dual source made intentional: config-list authoritative in P1, table = mirror/history. OQ-5. |
| **GAP-7** | truemail `validation_type` | Includes `whitelist`/`blacklist` — **non-verification** outcomes with `success` true/false. | Explicit mapping DECIDED (§7.5, OQ-19): `whitelist→valid`+flag, `blacklist→invalid`+flag, both calibration-excluded. |
| **GAP-8** | truemail timestamps | Engine emits local-tz strings (`"2021-02-07 10:00:56 +0200"`). | LOG stores **UTC** (`timestamptz`); raw string preserved inside the `verification_raw` blob. §3.2.2. |
| **GAP-9** | SVG `raw` "untouched truemail output" | Ambiguous *which* output: the lean rack JSON (available) or the rich gem `Result` (not available over HTTP). | `raw` = the rack JSON we actually receive. Documented so expectations are correct. §3.1.3. |
| **GAP-10** | brain-own-DNS (§6.5) vs engine DNS | From P2, brain and box resolve DNS independently and can disagree, risking divergent routing vs SMTP verdicts. | Named as a P2 reconciliation seam; brain-MX advisory / box-MX authoritative recommended. §6.5, OQ-24. |

---

## 9. Open questions / decisions — ALL SIGNED OFF (2026-07-15)

All 25 items are decided. The owner **accepted 23 recommendations as a batch** and **overruled 2** (OQ-7 flipped; OQ-1 elevated in weight). The "Decision" column below is now binding for build; the full rationale for the two overrules and the two confirm-with-note items is in **Appendix C**. Nothing here is still open.

| OQ | Topic | Decision (signed off) | Touches |
|---|---|---|---|
| **OQ-1** | Email-hash pepper strategy & custody (Door 3) | **ELEVATED:** single long-lived global HMAC-SHA-256 pepper — treated as *the single most fragile artifact in the system*, with its own tested backup/custody runbook (§3.3.3), not just a secrets-manager entry. Rotation additive-only. | Door 3 freeze |
| **OQ-2** | Provider-aware `provider_canonical_hash` | Define column now, populate later; never fold quirks into primary hash. | Door 3 shape |
| **OQ-3** | Lowercase the local-part? | **Yes — confirmed.** Acknowledged as a formally RFC-lossy choice baked into a frozen hash; safe in practice (no real provider treats the local-part case-sensitively). | Door 3 freeze |
| **OQ-4** | `/verify` echoes raw vs normalized email | Echo raw input; LOG never stores raw. | Door 1 minor |
| **OQ-5** | Node registry source of truth in P1 | config-list authoritative; `nodes` table = mirror. | §5.3 |
| **OQ-6** | Cheap layers: brain-native vs mocked-engine (the build-time flag) | Brain-native for syntax + MX/DNS/null-MX cheap path; keep engine for the SMTP path (P2). | build-time |
| **OQ-7** | Disposable = gate→invalid or flag→risky (cold policy) | **OVERRULED → `risky` + `is_disposable` flag, policy skips (recommendation `skip`).** A disposable mailbox is real/deliverable; `invalid` would conflate "won't deliver" with "don't want" and poison Phase-7 calibration. Touches Door 4 (reason vocab). | policy v1 · **Door 4** |
| **OQ-8** | Confidence computation in P1 | Fixed baseline-by-reason table; invalids high, unproven capped low. | policy v1 |
| **OQ-9** | Cache key / TTL value / re-log on hit | Composite `(email_hash, config_fingerprint)` key (correctness fix); `expires_at` always set, TTL *value* TBD; always log hits with `from_cache=true`. | §4.2 |
| **OQ-10** | Raw-blob retention window + partition-from-row-#1 | Age out `verification_raw` blob at ~90d via `DROP PARTITION`; structured columns indefinite; declare partitioning now. | §3.2.6 |
| **OQ-11** | `config_fingerprint` sourcing (needs full box config) | Fork exposes `/config`; P1 fingerprints brain+policy. | Door 2 completeness |
| **OQ-12** | API versioning transport | URI path `/v1/`. | Door 1 |
| **OQ-13** | Auth: front-door token scheme + node-token custody | Bearer for `/v1/verify`; node tokens in secrets mgr, not in `nodes` clear. | §3.5.4 |
| **OQ-14** | Provider-routed-to-`:mx` status in P1 | `risky` + `provider_no_smtp`. | policy v1 |
| **OQ-15** | `reason` single vs array | Single dominant reason; detail in `signals`. | Door 4 |
| **OQ-16** | Null-MX detection ownership | Brain-native RFC 7505 detection (not the engine flag). | §6.6 |
| **OQ-17** | Spam-trap list sourcing | Model the table now; acknowledge acquisition is unsolved. | §6.8 data |
| **OQ-18** | Typo known-good domain list source | Built-in seed now; Supabase-backed later. | §6.2 data |
| **OQ-19** | whitelist/blacklist engine outcome mapping | `whitelist→valid`+flag, `blacklist→invalid`+flag, calibration-excluded. | Door 4/§7.5 |
| **OQ-20** | Supabase access model (RLS vs service-role) | Service-role only in P1 (single client = brain); RLS designed if tenants appear. | §4 |
| **OQ-21** | `request_id` origin | Server-generated; allow client override for idempotency/tracing. | §3.1 minor |
| **OQ-22** | LOG timestamp normalization | Store UTC; keep raw engine tz string in `raw`. | Door 2 |
| **OQ-23** | `policies.is_default` scope | **One system-wide default — confirmed for v1** (`UNIQUE (is_default) WHERE is_default`). **Seam note:** tiers (Normal/Premium) will later map to policies; do not let the single-default assumption harden when tiers arrive (§4.6, §10). | §4.6 |
| **OQ-24** | Brain-vs-engine DNS reconciliation (GAP-10) | Defer to P2; box-MX authoritative for SMTP verdict, brain-MX advisory for routing, disagreement logged. | P2 seam |
| **OQ-25** | Policy definition = open object vs closed struct | Open/extensible object; evaluator tolerates unknown keys (same discipline as the response contract). | §7.2 |

---

## 10. Phase seams (where Phase 1 deliberately stops)

| Later phase | Seam left in Phase 1 | Not built now |
|---|---|---|
| **P2** SMTP on mocks | Node client + router exist; `raw`/`smtp_debug` fields, SMTP `reason`s reserved; fork `/verify`+`/config`+`/audit` endpoints anticipated. | Catch-all probe, 5xx/greylist classification, SMTP hardening. |
| **P3** first real box | `nodes` table has `port25_open`, `last_audit_at`, `audit_status`; provider routing computed. | `/audit` gate, PTR/warmup, flipping mock→real. |
| **P4** resilience & pool | `Strategy` interface + `nodes` status lifecycle; observability seam noted. | Health watcher, sticky/least-inflight strategies, per-provider rate/jitter. |
| **P5** queue | Singular `/v1/verify` shaped so `/v1/verify/bulk` (job handle) slots beside it. | Async queue, greylist retry/backoff, group-by-domain. |
| **P6** third-party fallback | `reason=external_verified` reserved; layer model accommodates an external layer. | Fallback API integration, residue routing. |
| **P7** flywheel | `send_outcomes` (+`verification_id` FK, `domain`, `mx_provider`, `hash_scheme`, `sent_at`) + join table (empty); `verifications.id` = authoritative join anchor, `email_hash` = rollup key; promoted `mx_provider` column; `from_cache` + override-derived (whitelist/blacklist) calibration-exclusion; **disposable kept `risky` not `invalid` so bounce data stays clean** (OQ-7). | Bounce classification, per-(reason×provider) recalibration, provider auto-promotion. |
| **Tiers** (billing, cross-phase) | `policies` holds multiple versioned policies; request carries optional `policy`. **`SEAM →`** default-selection is system-wide in v1 (OQ-23); becomes tier-scoped (Normal/Premium → policy) when tiers arrive — do not harden the single-default assumption. | Tier→policy mapping, per-tier default resolution. |

---

## 11. Phase 1 "done" — restated with acceptance criteria

**Build-plan definition:** *you can POST an address and get a scored, logged verdict using cheap layers only — no port 25 anywhere.*

Acceptance criteria for this spec's design (all verifiable without SMTP):

1. `POST /v1/verify` accepts `{email}` and returns the full v1 response shape (§3.1.3), auth-gated.
2. Every request writes exactly one append-only `verifications` row with complete provenance (§3.2) — including cheap-only verdicts and cache hits (`from_cache=true`), and including un-hashable `invalid_syntax` rows (`email_hash=NULL`, §3.2.7). The engine blob, when present, is written to the `verification_raw` sidecar.
3. Email hashing/normalization is applied per the frozen `norm/v1 + hmac-sha256/v1` scheme (§3.3), with `hash_scheme` recorded.
4. `status`/`reason`/`recommendation` come only from the frozen vocabularies (§3.4); no value outside the enums is ever emitted.
5. Cheap layers (§6) run under the `cold-email` policy (§7.3) and produce honest P1 verdicts (invalids provable; "valid" not claimed without SMTP).
6. Routing goes through the node-list + `Strategy` interface even at N=1 (§3.5/§5.4); no hardcoded endpoint exists.
7. The engine remains stock (pinned gem; fork = rack only); all interpretation is brain-side (§3.5.2).
8. All Supabase tables in §4 exist on paper with the specified shapes, including the empty Phase 7 seam tables.
9. Every open question in §9 is either signed off or explicitly deferred with an owner — none silently assumed.

---

## Appendix A — Upstream truemail reference (captured ground truth)

> Captured from the `truemail` gem and `truemail-rack` READMEs to anchor the one-way doors in what the engine *actually* emits. This is reference, not our contract.

### A.1 truemail gem — config options & result shape
- **Config options** (`Truemail.configure`): `verifier_email` (required), `verifier_domain`, `email_pattern`, `smtp_error_body_pattern`, `connection_timeout` (def 2), `response_timeout` (def 2), `connection_attempts` (def 2), `default_validation_type` (def `:smtp`), `validation_type_for` (per-domain), `whitelisted_emails`, `blacklisted_emails`, `whitelisted_domains`, `blacklisted_domains`, `whitelist_validation`, `blacklisted_mx_ip_addresses`, `dns`, `not_rfc_mx_lookup_flow`, `smtp_port` (def 25), `smtp_fail_fast`, `smtp_safe_check`, `logger`.
- **Validation types (levels):** `:regex` → `:mx` (DNS; RFC MX→CNAME→A) → `:mx_blacklist` → `:smtp`. `validation_type_for` overrides per domain (e.g., `{'gmail.com' => :mx}`). Also two override outcomes: `:whitelist`, `:blacklist`.
- **Validator result (`.as_json` / `ValidatorJson`):** `{ date, email, validation_type, success, errors, smtp_debug, configuration }`. **Note:** the JSON serializer emits **no `domain` and no `mail_servers`** (those exist on the in-memory `Result` struct only). `errors` is `{layer => message}` e.g. `{ "smtp": "smtp error" }`, `{ "mx_blacklist": "blacklisted mx server ip address" }`. `smtp_debug` is `null` on success, else an array of `{ mail_host, port_opened, connection, errors:{ rcptto|mailfrom|connection: "<raw SMTP string>" } }`.
- **Host audit (`host_audit`, Phase 3 seam):** `{ current_host_ip, warnings:{ dns, ptr, … } }`.

### A.2 truemail-rack server — HTTP surface (what our node client actually talks to)
- **Endpoints:** `GET /?email=<addr>` (validation — the root, **not** `/validate`), `GET /version`, `GET /healthcheck`. Auth via header `Authorization: <token>` (raw token, matched against `ACCESS_TOKENS`).
- **Validation response (200):**
  ```json
  { "date":"2021-02-07 10:00:56 +0200", "email":"admin@bestweb.com.ua",
    "validation_type":"smtp", "success":true, "errors":null, "smtp_debug":null,
    "configuration":{ "validation_type_by_domain":null,"whitelisted_emails":null,
      "blacklisted_emails":null,"whitelisted_domains":null,"blacklisted_domains":null,
      "whitelist_validation":false,"blacklisted_mx_ip_addresses":null,"dns":null,
      "email_pattern":"default gem value","not_rfc_mx_lookup_flow":false,
      "smtp_error_body_pattern":"default gem value","smtp_fail_fast":false,"smtp_safe_check":false } }
  ```
  (Failure adds `errors:{"smtp":"smtp error"}` and a populated `smtp_debug` array.)
- **`/version`:** `{ "core":"3.3.0", "platform":"ruby 3.3.0 …", "version":"0.10.0" }` → source for `engine_version`/`engine_core_version`.
- **Config via env:** `VERIFIER_EMAIL`(req), `ACCESS_TOKENS`(req, comma-sep), `VERIFIER_DOMAIN`, `EMAIL_PATTERN`, `SMTP_ERROR_BODY_PATTERN`, `CONNECTION_TIMEOUT`, `RESPONSE_TIMEOUT`, `CONNECTION_ATTEMPTS`, `DEFAULT_VALIDATION_TYPE`, `VALIDATION_TYPE_FOR`, whitelists/blacklists, `WHITELIST_VALIDATION`, `BLACKLISTED_MX_IP_ADDRESSES`, `DNS`, `NOT_RFC_MX_LOOKUP_FLOW`, `SMTP_PORT`, `SMTP_FAIL_FAST`, `SMTP_SAFE_CHECK`, `LOG_STDOUT`.
- **Confirms GAP-1/2/3:** no `/validate`; no `domain`/`mail_servers` in JSON; `configuration` is a subset of full config.

---

## Appendix B — Review log (what the critique passes changed)

This spec was drafted, then critiqued by an adversarial **database-architect** pass and an **architecture** pass before finalizing (as required by the task). Findings folded in:

**Blockers (all resolved):**
- **B1 — Cache correctness.** Cache keyed on `email_hash` alone would serve a verdict computed under a different policy/config → wrong answer. **Resolved:** composite key `(email_hash, config_fingerprint)`, `config_fingerprint` NOT NULL (§4.2).
- **B2 — Unlocked join-key type.** `email_hash` physical type was left `text|bytea` across the three joining tables — a Door-3 key that wasn't actually locked. **Resolved:** pinned `bytea` (32-byte HMAC) everywhere (§3.2.2, §3.3.3, §4.2, §4.7).
- **B3 — Null-hash contradiction.** `email_hash NOT NULL` vs "log every syntax-fail" (unparseable, unhashable). **Resolved:** `email_hash`/`domain`/`hash_scheme` nullable; `invalid_syntax` rows carry `NULL` (§3.2.7).

**Majors (all resolved):**
- **M1 — Phase-7 join ambiguity** ("`email_hash` AND/OR join table"). `email_hash` fans out to all historical verdicts per address. **Resolved:** `verifications.id` is the authoritative join; `email_hash` demoted to rollup key; `send_outcomes` gains `verification_id` FK + `sent_at` (§4.7).
- **M2 — `send_outcomes` missing `domain`/`hash_scheme`.** Needed for flywheel rollups and hash disambiguation; "gone forever if not captured." **Resolved:** added `domain`, `mx_provider`, `hash_scheme`, `provider_canonical_hash` (§4.7).
- **M3 — `mx_provider` buried in jsonb** despite being the permanent flywheel grouping dimension. **Resolved:** promoted to a typed column from row #1 (§3.2.2, §4.1 index).
- **M4 — Append-only vs retention conflict** (nulling `raw` in place is an UPDATE). **Resolved:** `raw` moved to a 1:1 `verification_raw` sidecar, partitioned, retention = `DROP PARTITION`; append-only enforced by revoked UPDATE/DELETE grants (§3.2.1, §3.2.6).
- **M5 — `domain` as `citext`** redundant/locale-risky given `norm/v1` already lowercases+punycodes. **Resolved:** plain `text` + `CHECK (domain = lower(domain))`, consistent across all tables (§3.2.2, §4.2–4.4).
- **M6 — Random UUIDv4 PK** on the highest-write table fragments the index. **Resolved:** time-ordered UUIDv7/ULID from row #1 (§3.2.2).
- **M7 — `policies.is_default` multi-default risk.** **Resolved:** partial unique index; scope ambiguity raised as OQ-23 (§4.6).
- **M8 — `config_fingerprint` nullable** defeats its own bucketing/cache role. **Resolved:** NOT NULL on `verifications` (§3.2.2).
- **M9 (architecture) — status × recommendation composability** unconstrained (`invalid`+`send` possible). **Resolved:** locked cross-field invariant table (§3.1.3).
- **M10 (architecture) — policy grammar forward-expressiveness** for P4–P7 unproven. **Resolved:** open-object discipline + additive extension-point map; OQ-25 (§7.2).
- **M11 (architecture) — divergent brain-vs-engine DNS** risk from P2. **Resolved:** named as GAP-10 / OQ-24 P2 reconciliation seam (§6.5).

**Minors folded in:** enum/`confidence` CHECK constraints (§4.1); `cache.expires_at` NOT NULL (§4.2); `nodes` `UNIQUE(base_url)` + FK-less `node_id` rationale (§4.5); `provider_canonical_hash` inconsistency resolved (§3.3.5); catch-all-analytics-read-the-table note (§4.3); no empty-string sentinels / `request_id` non-unique (§3.2.7); TOAST/bloat acknowledged (§3.2.6).

**What the reviews affirmed as already-right (preserved):** `timestamptz`-UTC + raw-tz-string preservation; deterministic HMAC-with-global-pepper as the joinable identity (rejecting per-row salt/argon2); composite `(key, version)` immutable policies with `policy_version` stamped on every row; engine-native `engine_success`/`engine_validation_type` kept distinct from our `status`/`reason`; the `hash_scheme` additive hedge + tolerate-unknown enum discipline; the response↔LOG single-assembler guarantee; correct scope discipline (stops at Phase 1, seams named not built).

*(Post-review status: all 25 OQs were subsequently signed off by the owner — see §9 and Appendix C. OQ-24 (P2 DNS reconciliation) is "decided to defer to Phase 2," which is itself the decision.)*

---

## Appendix C — Owner decision log (2026-07-15 sign-off)

All 25 open questions were signed off by the owner. **23 recommendations accepted as a batch; 2 overruled; 2 accepted with a standing caveat.** Recorded here so the reasoning behind the two overrules survives — they are the decisions where the spec's own recommendation was rejected on merit.

### Overrule 1 — OQ-1: the pepper is the system's most fragile artifact (weight elevated)
- **Spec had said:** correct scheme (global HMAC pepper), but framed as "a one-way secret" — a line in a secrets manager.
- **Owner ruling:** the recommendation is *right but under-weighted*. This pepper is **more permanent than any schema decision**: lose it and all history de-joins (dedup/cache/flywheel linkage gone, unrecoverable — raw addresses were never stored); leak it and the low-entropy local-parts become brute-forceable (the only thing separating "domain-in-clear, acceptable" from "full-address-recoverable, a breach").
- **Applied:** §3.3.3 rewritten to name it the single most fragile artifact, mandate its **own tested backup/custody runbook** (redundant, geographically separate, access-audited backups; recovery tested before Phase 1 ships) rather than a secrets-manager entry, and classify its loss/leak as a top-tier incident. §9 OQ-1 marked ELEVATED.

### Overrule 2 — OQ-7: disposable is `risky`, not `invalid` (touches a one-way door)
- **Spec had recommended:** gate disposable → `invalid` for cold-email ("cold outreach to disposables is worthless").
- **Owner ruling:** rejected. A disposable address is a **real, deliverable mailbox** — just low-value. Labeling it `invalid` **conflates "won't deliver" with "don't want,"** and because `reason` is a frozen one-way vocabulary feeding Phase-7 calibration, a disposable that *would* have delivered but is logged `invalid` **teaches the bounce model the wrong thing.** Correct answer: `risky` + `is_disposable` flag, and let the *policy* skip it (`recommendation=skip`). Keeps the verdict honest and the training data clean; operational behavior (don't send) is identical.
- **Applied:** §3.4.3 (`disposable` → `risky`), §6.7 (Type-2 flag, not Type-1 gate), §7.2 (policy grammar: `risky:disposable:stop` + `recommendation_overrides_by_reason`), §7.3, §7.5 (invalid-means-proof-of-non-deliverability boundary), §3.1.3 flag note, §10 (P7 seam), §9 OQ-7 marked OVERRULED.

### Confirm-with-caveat 1 — OQ-3: lowercase the local-part
- **Confirmed.** Formally RFC-technically-lossy (RFC 5321 permits case-sensitive local-parts) but safe in practice; the choice is knowingly baked into the frozen hash. §3.3.3/§9 annotated to say so explicitly.

### Confirm-with-caveat 2 — OQ-23: one system-wide default policy
- **Confirmed for v1**, with a standing caveat: tiers (Normal/Premium) will map to policies later, so the single-default assumption **must not harden** — default-selection becomes tier-scoped when tiers arrive. Recorded as a seam in §4.6 and §10 (Tiers row), not just a table cell.

### Batch-accepted (23): 
OQ-2, 4, 5, 6, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 24, 25 — all accepted with the recommendations already in §9 (OQ-24 = "defer reconciliation to Phase 2," which is the decision). No wording change needed beyond marking them signed off.

### Boundary confirmed (not a change): 
The spec correctly does **not** assign list-deduplication to the brain — dedup is a platform/caller concern. Confirmed as a deliberate boundary (§0.2), not a gap.

---

*End of Phase 1 (Foundations) Design Specification — **v1.0, APPROVED**. All 25 open questions signed off (Appendix C); two owner overrules (OQ-7, OQ-1) applied throughout. Ready to build.*
