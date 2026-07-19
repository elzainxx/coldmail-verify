# PROJECT_RULES.md — the constitution

**Status:** LIVE — the binding rules for the Truemail verification system. Authored in **Phase 0**.
**What this is:** the one document a developer reads to understand *the rules that can never change*
and *the operational doctrine that keeps the system honest* — **without reading all seven phase specs.**
**What this is NOT:** a design spec or a decision log. It **invents nothing.** Every rule here is
*restated* from an authority that already decided it: the seven approved phase specs, the
[build plan](Docs/truemail-build-plan.md), [`Docs/DECISIONS.md`](Docs/DECISIONS.md), the
[ADRs](Docs/adr/), and the five revision-pass docs
([config-manifest](Docs/config-manifest.md) · [kill-switch-ladder](Docs/kill-switch-ladder.md) ·
[shadow-mode](Docs/shadow-mode.md) · [cross-phase-consistency-report](Docs/cross-phase-consistency-report.md) ·
[pre-takeoff-checklist](Docs/pre-takeoff-checklist.md)). Where a rule cites a source, the source wins if
they ever disagree — this file is a faithful synthesis, not a new authority.

> **The one rule that governs everything** (build plan):
> *Anything that becomes historical data, or that another system codes against, is a **one-way door** —
> get it right now. Everything else is a **dial** you can turn later.*

---

## 1. The five one-way doors (hard rules)

These are frozen. **Additive changes only, forever. A rename or removal is a break, never allowed.**
The doors are what the whole seven-phase plan spent its effort protecting; nothing below is a
suggestion.

### Door 1 — the `/verify` response shape is versioned & additive-only
- The response is `POST /v1/verify` → a JSON object with `api_version, status, reason, confidence,
  recommendation, email, domain, flags, signals, meta, raw` (P1 §3.1.3). Implemented as the frozen
  contract in `control-plane/internal/verdict`.
- **Never remove or rename a field. Never change a field's type or meaning.** `confidence` is always
  an integer 0–100 — never a float or string.
- **Only add**: new fields, new `flags`/`signals` keys, new enum values (Door 4). A breaking change is
  `/v2/`, served *alongside* `/v1/`, never in place.
- **Consumers must tolerate the unknown**: ignore unknown object keys; treat an unrecognized `status`
  as `unknown` and an unrecognized `reason` as opaque. This is *part of* the v1 contract, so additive
  changes are safe by construction.
- **Cross-field composability is locked** (P1 §3.1.3): `invalid ⇒ recommendation=skip` (an `invalid`
  that says `send` is a **forbidden** combination); `valid ⇒ {send, review}`; `risky`/`unknown ⇒
  {review, skip}`. Enforced in code by `vocab.ComposabilityOK`.

### Door 2 — the LOG is append-only, from check #1
- **Log every result** — including cheap-only verdicts and cache hits — *before* returning. "The log
  is the point" (build plan). It is `verifications`, and it is **append-only**: never updated, never
  deleted except by the retention policy on the raw-blob sidecar.
- Enforcement is not convention: `UPDATE`/`DELETE` are revoked from the runtime service role (P1
  §3.2.1); corrections are new rows. The Go `store.Store` interface deliberately offers **no**
  update/delete of a verdict — the shape refuses mutation.
- **Hardening (PN-9 / DR-3):** DDL/grant-admin is a **separate break-glass, admin-audited** credential,
  distinct from the runtime service role, so the runtime cred cannot silently rewrite history.
- Provenance is mandatory from check #1: `verifier_ip`, `engine_version`, `config_fingerprint`,
  `from_cache`, and the raw engine blob (in the `verification_raw` sidecar). The LOG must join to
  future send/bounce outcomes (P7) with **zero** change to existing columns.

### Door 3 — email hashing + normalization: one scheme, frozen
- Scheme: **`norm/v1` + `hmac-sha256/v1`**, stored as **`bytea` (32 bytes)**, keyed by a **single
  global HMAC pepper**. Implemented in `control-plane/internal/hash`.
- The **raw full email is NEVER persisted.** Only `email_hash` (the address's identity across history)
  and `domain` (in clear) are stored. Local-part lowercased (OQ-3, RFC-lossy accepted); dots/`+tags`
  are **not** folded into the primary hash (provider quirks are a separate optional column).
- **The pepper is the single most fragile artifact in the system** (OQ-1 ELEVATED). Lose it → all
  history de-joins forever; leak it → low-entropy addresses become brute-forceable. It gets its **own
  tested backup/custody runbook** (not a secrets-manager line): generated once, stored outside the DB,
  redundant + geo-separate + access-audited backups, recovery **drilled before P1 ships**, rotation
  additive-only. The control-plane holds it in memory → host-hardening + core-dump suppression are part of
  custody (F-9/F-19). See [`control-plane/internal/secrets`](control-plane/internal/secrets/secrets.go) and
  [pre-takeoff-checklist §1.1](Docs/pre-takeoff-checklist.md).
- **The one bounded exception** (P5, DR-4/ADR-0002): the queue (`work_items`) holds *plaintext for
  work-not-yet-done only*, purged transactionally on settle, and its backup copy must be
  excluded/protected. Every *other* durable store holds only `email_hash`.

### Door 4 — the `status` / `reason` / `recommendation` vocabularies are additive-only
- **New values are fine forever; renames and removals are never allowed.** Consumers branch on these
  strings and P7 calibration buckets on `reason`.
- `status ∈ {valid, invalid, risky, unknown}`; `recommendation ∈ {send, review, skip}`; `reason` is the
  frozen enum in P1 §3.4.3. All later-phase values (`historical_bounce`, `external_verified`, the SMTP
  reasons, …) are **reserved now** so no future addition is breaking.
- The enums live in **one** place: [`control-plane/internal/vocab`](control-plane/internal/vocab/vocab.go). Never
  duplicated per service. Storage-layer CHECK constraints mirror it (adding a value = an additive
  migration).

### Door 5 — never fork the engine; talk to a node LIST
- **The `truemail` gem (the engine) is a pinned dependency at `3.3.1`, NEVER forked or patched**
  ([ADR-0003](Docs/adr/ADR-0003-truemail-engine-pin.md)). Only **`truemail-rack` (the HTTP server)** is
  forked (`verifier-node/truemail-rack/`, provenance in [FORK.md](verifier-node/truemail-rack/FORK.md)). CI asserts the
  engine is unmodified ([`bin/assert-gem-unmodified.sh`](verifier-node/truemail-rack/bin/assert-gem-unmodified.sh)).
- The control-plane talks to a **LIST of nodes** (even length 0/1) through a pluggable **Strategy interface** —
  there is **no** code path referencing a single hardcoded endpoint (`control-plane/internal/router`). Adding
  node #2 is a **config-list edit, never a code change** (GAP-31).
- Any behavioral gap is closed **control-plane-side** or in the **rack wrapper** (Phase 2+), **never** by
  editing the gem. "Door 5 outranks design elegance" — if a nicety needs a gem patch, drop the nicety
  (e.g. the GAP-13 catch-all-session decision).

---

## 2. `unknown` is first-class — never guess to avoid it

This is the founding discipline, not a Door but the reason the Doors matter.

- **Blocked / limited / greylisted / timed-out / broken / ambiguous → `unknown`.** Never `invalid`.
- **`invalid` means exactly one thing: positive proof of non-deliverability** (bad syntax, no MX,
  null-MX, a confirmed no-such-user 550, a gated list hit). Absence of proof is **not** proof of
  absence.
- **`valid` (SMTP-grade) is earned, never assumed** — only a clean `250` on a probe-confirmed
  non-catch-all mailbox. A rented (vendor) "valid" is capped to `risky` (P6 anti-laundering).
- **A low-value-but-deliverable signal is not `invalid`** — a disposable address is a real mailbox, so
  it is `risky` + `is_disposable`, and the policy *skips* it; it is never labeled `invalid` (OQ-7).
  Conflating "won't deliver" with "don't want" poisons P7 calibration.
- The flywheel never pressures `unknown → valid` to look more accurate (P7 §8.4). First-class `unknown`
  survives all seven phases intact.

The control-plane embodies this: with no engine consulted, `/v1/verify` returns
`unknown`/`plausible`, never a guessed verdict (`verdict.Assemble` emits `status=unknown` /
`reason=plausible` on the no-engine path).

---

## 3. Two vocabularies — never leak mechanics or cost structure

There are **two** distinct vocabularies, and they must not bleed into each other:

| | **Internal `reason`** (the mechanic) | **User-facing `status` + `recommendation`** (the answer) |
|---|---|---|
| Audience | us — calibration, debugging, the flywheel | the caller / platform |
| Examples | `smtp_blocked`, `catch_all`, `rate_limited`, `provider_no_smtp`, `external_verified` | `valid/invalid/risky/unknown` + `send/review/skip` |
| Leaks what? | *our* infrastructure state, provider behavior, cost structure | nothing about how we got the answer |

- **Never leak mechanics or cost structure to the caller.** The caller learns *what the address is*
  (`status`) and *what to do* (`recommendation`), not *which node was blocked*, *whether we paid a
  vendor*, or *why SMTP was skipped* beyond what the frozen `signals`/`reason` contract already exposes.
- The `external_verified` reason marks provenance internally so P7 can measure the rented population —
  it is **not** an invitation to surface "we rented this answer" to the user; that disclosure is a
  **platform** decision (OQ-P6-6), surfaced not shipped silently.
- Cost structure (vendor spend, per-provider economics) is **never** in a caller-facing field.

---

## 4. Shadow-mode default for anything that mutates routing/confidence from data

Anything that autonomously changes **routing, confidence, provider eligibility, or spend** from data
carries a `mode ∈ {shadow, recommend-only, auto}` and **defaults to `recommend-only`** — compute +
alert, never auto-change, until a human deliberately promotes it (DR-2, [shadow-mode.md](Docs/shadow-mode.md)).

- **The four day-one mutators default to `recommend-only`:** A1 (P4 block-rate quarantine, statistical
  trigger), A6 (P7 confidence recalibration — the sharpest), A8 (P7 provider auto-promotion), A9 (P7
  fallback weaning). Wired as real config in [`control-plane/internal/config`](control-plane/internal/config/config.go)
  (`ShadowModes`, defaulted recommend-only, tested).
- Promotion to `auto` is a per-mechanism, per-scope, **admin-audited** owner action — never a global
  flip. The two systemic brakes (A5 fleet, A7 cascade) already alert-and-hold; they are the pattern
  being generalized, and they must **not** auto-cascade on a fleet-wide signal (err toward alert —
  [[truemail-fleet-brake-no-cascade]], GAP-28).
- **Shadow-mode mitigates but does not authenticate the outcome feed.** The P7 feed must be an
  authenticated channel with the sender as an identified principal (DR-1 / [ADR-0001](Docs/adr/ADR-0001-outcome-feed-authentication.md)) —
  a hard pre-P7 door, not a dial. Recommend-only + authentication are **both** required.
- **Fresh-system invariant:** on a new system every mutator sits at its default mode and performs
  **zero live mutation** (behavior identical to end-of-P6). This is a regression gate for the first
  phase to ship each mutator (DR-2, pre-takeoff-checklist §3.5).

---

## 5. The kill-switch ladder — operational doctrine

The documented degradation hierarchy (full → SMTP-off → fallback-off → cheap-only → paused). **Every
rung returns a real, correctly-labelled verdict — degradation lowers *confidence*, never *honesty*.**
No rung ever fabricates `valid`/`invalid`. Every rung is reachable by **config/flag, not code** (Door
5). Full detail: [kill-switch-ladder.md](Docs/kill-switch-ladder.md). Wired as real config in
`control-plane/internal/config` (`KillSwitch`).

| Rung | Disables | Still returns | Scope |
|---|---|---|---|
| **0 · FULL** | nothing | full verdict space | steady state (post-P6 + promotions) |
| **1 · SMTP-OFF** | SMTP probing | cheap + fallback; would-be `valid` → `risky/provider_no_smtp`, **never `invalid`** | per-node → per-provider → global |
| **2 · FALLBACK-OFF** | the paid vendor | everything but vendor answers; residue → `keep_residue` (`unknown`/`risky`) | per-policy → per-vendor → global |
| **3 · CHEAP-ONLY** | SMTP **and** fallback | provable invalids at full confidence; unprovable → `risky`/`unknown`, **never guessed `valid`** | **= Phase-1 steady state** |
| **4 · PAUSED** | verdict production | job handle + "paused", or explicit unavailable | accept-and-defer / reject |

- **Two global master switches** sit outside the graceful rungs: **global-fallback-kill** (Global A —
  the panic button, "no PII egress in one action"; PN-16, admin-audited, rides DR-3) and **shadow-mode**
  (Global B — turns off the system's ability to *change itself*).
- **Rung 3 (cheap-layers-only) is the only rung reachable without SMTP** and is the Phase-0/Phase-1
  default. `config.Defaults()` starts here (SMTP=disabled, fallback=off) and
  `KillSwitch.CheapLayersOnly()` proves it — a Phase-0 done-criterion.
- **Authz/audit on who pulls a rung** is a gate before P3 go-live: every manual control-plane action
  writes to an append-only admin-audit log with per-actor identity (DR-3, pre-takeoff-checklist §3.6).
  Not built in Phase 0; recorded as a gate.

---

## 6. Coding standards

- **Two deployables, never merged.** Go control-plane (`control-plane/`), Ruby forked rack (`verifier-node/`). The boundary is a
  security + correctness boundary (README.md). Verifier-nodes are dumb/stateless/disposable; the control-plane holds all
  state; nodes never talk to each other and never touch the DB.
- **LOCKED → code; DIAL → config.** Everything the specs marked LOCKED becomes a rule or a type
  (`vocab`, `hash`, the append-only `store` shape); everything marked DIAL becomes a readable config
  entry ([`config`](control-plane/internal/config/config.go)). **Nothing the config manifest calls a flag is
  hardcoded.**
- **Interfaces at the seams.** The router `Strategy`, the node `Client` (pure-function `email → raw
  JSON`), and the `store.Store` are interfaces so later phases add implementations without touching
  callers (Door 5's payoff).
- **Secrets are never committed, never logged.** Loaded from secure config at startup, held in memory,
  redacted in any `String()`. `.gitignore` excludes `*.env` (except `*.example.env`), `secrets/`, keys,
  and the pepper. See [`control-plane/config/secrets.example.env`](control-plane/config/secrets.example.env).
- **Harden toward `unknown` in code, not just prose.** A path that cannot prove a verdict returns
  `unknown`/degrades to cheap — it never invents `valid`/`invalid` (see the node stub returning
  `ErrNotImplemented`, never a fake verdict).
- **Match the surrounding code.** Go is stdlib-first at Phase 0 (external deps arrive with the features
  that need them). Ruby fork stays stock (no gem patch, minimal rack-side edits, all listed in FORK.md).
- **Flag, don't fix, a spec-vs-reality mismatch.** A mismatch that touches a frozen door is an owner
  decision — surface it (as in [truemail-ground-truth.md](Docs/truemail-ground-truth.md)), never
  silently "correct" the door.

---

## 7. The review gate — every change runs code-reviewer

- **Every change runs the code-reviewer gate before it is considered done** (CI:
  [`.github/workflows/ci.yml`](.github/workflows/ci.yml)). This applies to *doc/spec changes too* — the
  planning passes are gated the same way as code.
- The gate checks: correctness, the additive-only doors, no hardcoded manifest flag, no secret in the
  diff, and no future-phase mechanism built ahead of its phase.
- CI also runs: `go build` + `go test` + `go vet` (control-plane), the **gem-unmodified assertion** (Door 5),
  and the **docs-updated staleness check** (§8).

---

## 8. A change isn't done until its docs are updated

- **Documentation staleness is a failing check, not a nag.** A behavior change (code under `control-plane/` or
  `verifier-node/`) that does not also touch a doc/ADR fails the docs-updated CI check
  ([`scripts/check-docs-updated.sh`](scripts/check-docs-updated.sh)).
- **One-way-door decisions and owner-OQ rulings become ADRs** ([`Docs/adr/`](Docs/adr/)). Cross-phase
  rulings live in [`Docs/DECISIONS.md`](Docs/DECISIONS.md); parked sub-decisions stay parked in §5 there
  until an owner resolves them (this Phase 0 did **not** resolve them).
- **The config reference is generated from the manifest and checked in**
  ([`Docs/config-reference.md`](Docs/config-reference.md)) so "what's toggleable" is always current;
  regenerate it when the manifest changes.
- **Living docs mirror the phases** ([`Docs/README.md`](Docs/README.md) is the index). A new phase's
  spec, ground-truth deltas, and test additions land together.

---

## 9. What a new developer should now understand

Reading this file alone, you know: the **five doors** you can never break; that **`unknown` is
first-class** and `invalid` means exactly one thing; that there are **two vocabularies** and you never
leak mechanics/cost to the caller; that **data-driven mutators default to recommend-only**; the
**kill-switch ladder** and that every rung stays honest; and that **every change is gated by
code-reviewer + a docs-updated check**. For the *why* behind any single rule, follow its citation to the
owning spec — this constitution is the map, the specs are the territory.
