# Phase 2 — status: SMTP path against mocks (live scorecard)

**Date:** 2026-07-17 · **Phase:** 2 (SMTP against mocks) · **Spec:** [`phase-2-smtp-mocks-spec.md`](phase-2-smtp-mocks-spec.md) (the authority)
**Status: ✅ COMPLETE** — the full SMTP path runs end-to-end against mocks; the config-flip to a real
box is config-only; the three inherited seams are resolved; no real port-25 box was ever contacted.
**What Phase 2 is:** build and harden the whole SMTP path against fakes — the forked-rack wrapper
endpoints, the catch-all probe, 5xx/greylist classification, transcript capture, DNS reconciliation,
and SMTP scoring under harden-toward-`unknown` — so switching to a real server is one config value.

**Prerequisite gate (checked before starting):** ✅ Phase 1 genuinely closed (all 7 increments),
all ground-truth rows real (no PENDING), [ADR-0006](adr/ADR-0006-door3-nonascii-localpart-refusal.md)
blessed, [`phase-1-to-2-handoff.md`](phase-1-to-2-handoff.md) exists.

---

## The run-first spike (mandated): GAP-13 → Option B

Before building the catch-all probe, the **GAP-13 spike** was run (spec §5.4/§10 OQ-P2-11, the owner's
"run early" condition). Reading the pinned `truemail 3.3.1` SMTP source proved the gem exposes **no
reusable same-session** for a 2nd RCPT (`session_data` private + one hardcoded RCPT; `Net::SMTP`
auto-closes on the block; all driving methods private). Ruling: **Option B (two sessions), gem never
patched** — [ADR-0007](adr/ADR-0007-gap13-catchall-session-two-session.md). Door 5 outranks
same-session elegance. The known weakness is one-directional and safe: it can only *miss* a `valid`
(→ `risky`), never mint a false `valid`.

---

## Build increments

| # | Increment | Status | Evidence |
|---|---|---|---|
| 1 | **GAP-13 spike + ruling** | ✅ | Gem SMTP source read in-image; [ADR-0007](adr/ADR-0007-gap13-catchall-session-two-session.md) records Option B. |
| 2 | **Fork wrapper endpoints** (`/v1/verify,/audit,/catchall/:domain,/stats,/config,/config/reload`) | ✅ | `verifier-node/truemail-rack/app/truemail_server/wrapper/*` + `config.ru` URLMap. Boots; stock paths byte-identical; `/v1` auth-gated. FORK.md updated (exhaustive). |
| 3 | **Catch-all probe** (§5, Option B) | ✅ | `wrapper/catchall_probe.rb` — two `Truemail.validate` calls; tri-state conclusion; `valid` only on target-250 + probe-rejected. |
| 4 | **Brain: transcript parse + 5xx classify + DNS reconcile** | ✅ | `control-plane/internal/smtp/{transcript,classify,score}.go` — GAP-4 code parsing, §6.1 address-fault-vs-your-fault, §6.6 engine-MX-authoritative. Unit tests on **real** transcript shapes. |
| 5 | **Brain: SMTP scoring + policy gate + config_fingerprint** | ✅ | §7.2 map + §5.5 truth table + §6.8 safe-pass + §6.5 settled-only cache; `cold-email` policy gains an additive `smtp` gate (zero new vocab); `/v1/config` fingerprint (§4.6, GAP-3) replaces `engine=unconfigured`. |
| 6 | **Orchestrator SMTP stage** (§7.1) | ✅ | `internal/orchestrator/smtp_stage.go` — runs only on the non-terminal residue; node failure → unknown (never fabricated); raw blob → sidecar (§6.4); catch-all persisted (§5.6, migration 0006). Integration tests over a fake node. |
| 7 | **Socket-level proof** (Appendix B) | ✅ | `verifier-node/truemail-rack/test/p2/` — **8/8 rows** against real `smtp_mock`/`dns_mock` (smtpmock v2.5.4). |
| 8 | **Docs + ADRs + review** | ✅ | This doc, ADR-0007, ground-truth §3.4, config-manifest/reference, P2→P3 handoff; adversarial review. |

---

## The three inherited seams — resolved

- **GAP-3 / OQ-11 (config_fingerprint):** `GET /v1/config` exposes the full effective engine config;
  the brain computes the canonical fingerprint over it (`node.EffectiveConfig.CanonicalFingerprint`),
  composed into `config.EngineConfigFingerprint` — the P1 `engine=unconfigured` placeholder is retired.
- **GAP-4 (smtp_code parsing):** the engine returns SMTP status as **raw strings** with no numeric
  code; the brain parses the leading 3-digit code from the transcript (`smtp.ParseTranscript`) →
  `signals.smtp_code`. Proven on the real gmail-timeout + smtp_mock shapes.
- **GAP-10 / OQ-24 (DNS reconciliation):** engine-MX is authoritative for the SMTP verdict, brain-MX
  advisory for routing; divergence is logged (`dns_divergence`, `brain_mx_provider`, `engine_mx_hosts`)
  — never silently reconciled (`smtp.Score` / §6.6).

---

## Socket-level proof (Appendix B matrix) — 8/8 against real mocks

Run: `verifier-node/truemail-rack/test/p2/` (`Dockerfile` + `socket_proof.rb`). Exercises the
**real** truemail SMTP/DNS code path against socket-level fakes:

| Row | Proves |
|---|---|
| 4 | accept-all default → catch-all detected (GAP-14 confirmed) |
| 1 | seeded-probe rejected → the **only** `valid` path (target 250 + probe 550) |
| 2 | unregistered target → 550 no-user on RCPT (address-fault → invalid) |
| 5 | response_delay > timeout → timeout captured (unknown/smtp_timeout) |
| 8 | closed port → `port_opened=false` (unknown/connection_failed) |
| 7 | custom 552 body flows through RCPT (brain → valid/mailbox_full) |
| 3 | custom 554 policy-block flows through (unknown/smtp_blocked path, §6.1) |
| delta | **stock** blacklist really returns **421** (4xx) — confirms the ground-truth §3.4 421-vs-554 delta empirically; *why* row 3 needs a custom 554 fixture |

**Honest limits (not faked green):** GAP-12 — smtp_mock has no stateful greylist, so the
retry-then-accept **round-trip** is NOT socket-proven; a `450` → `unknown/greylisted` + requeue
marking is unit-proven brain-side, and true greylist validation is **rollout-only** (GAP-35). GAP-14 —
the `valid` proof uses a **seeded** probe (test artifact; faithful reject-arbitrary-unknown is P3).
Mocks prove **code-path identity**, not real-provider **behavioral identity** (spec §3.5).

---

## Additivity (§8) — proven additive against all five frozen doors

- **Door 1** — new `signals` keys only (`smtp_code, smtp_enhanced_code, greylisted, retry_after_hint_s,
  retry_eligible, smtp_safe_pass, smtp_debug_ref, dns_divergence, brain_mx_provider, engine_mx_hosts,
  catchall_conclusion`); no existing field/type/invariant changed. `invalid ⇒ skip` still enforced.
- **Door 2** — the `smtp_debug` transcript rides the existing `verification_raw` sidecar (§6.4); **no
  new `verifications` column**; still append-only, still logged-before-respond.
- **Door 3** — hashing untouched; and the sidecar blob is **redacted of the plaintext address** so no
  durable store holds the raw full email (the review CRITICAL, [ADR-0008](adr/ADR-0008-door3-raw-blob-address-redaction.md)).
- **Door 4** — **zero new enum values**; all 10 SMTP reasons were reserved in P1. The `cold-email`
  policy gains SMTP as DATA (existing `status:reason:stop|continue` grammar).
- **Door 5** — the gem is byte-identical (CI-asserted, `Truemail::VERSION=3.3.1`); only the rack
  wrapper is forked; the probe is two gem calls, no gem patch (ADR-0007).

---

## Adversarial review — one CRITICAL caught + fixed (Door 3)

A multi-dimension adversarial review ran (doors, classify, cache/dns/lifecycle, wrapper-security,
test-integrity). It surfaced one **CONFIRMED CRITICAL**, on the most-protected door:

- **Door 3 — plaintext address in `verification_raw`.** The fork's `/v1/verify` response echoes the
  checked address (top-level `email` field; and a real 550 body echoes it too), and the spec's
  §4.1/§6.4/OQ-P2-5 instruct storing that response **verbatim** as the sidecar blob — which would
  persist the raw full email, violating Door 3 ("the raw full email is NEVER persisted"). The spec's
  own sensitivity note (OQ-P2-5) had missed this (it listed only "verifier identity + probe addresses").
  **Fixed** by redacting the plaintext address at the brain's persistence boundary
  (`orchestrator/smtp_stage.go` `redactAddress`) — domain kept (Door-3-clear), transcript preserved,
  `email_hash` remains the join key. Recorded + flagged for owner spec-text ratification in
  [ADR-0008](adr/ADR-0008-door3-raw-blob-address-redaction.md). This is the exact "flag a
  spec-vs-reality mismatch that touches a frozen door" discipline (PROJECT_RULES §6) — the door won.
- **Door 3 follow-up (review HIGH):** a first redaction draft used a `len(local) >= 3` guard that would
  UNDER-redact a short local-part (`jo@x.com`) echoed bare in a body. Replaced with **word-boundary,
  no-guard** redaction (`\blocal\b`): scrubs a standalone `jo` but not the `jo` inside `json`/`rejected`.
  Both regression tests (`TestSMTPStage_RawBlobRedactsPlaintextAddress`,
  `TestSMTPStage_RawBlobRedactsShortLocalPart`) are **fail-verified** (each fires when the fix is neutered).

*(The review first ran as a 5-dimension background workflow; it began thrashing — long-session retries,
disproportionate for reversible SMTP logic — so it was stopped, its confirmed CRITICAL salvaged and
adjudicated inline, and the remaining dimensions re-run as a single bounded reviewer. That focused pass
found the Door-3 short-local HIGH (now fixed) and otherwise APPROVED — 0 further criticals; the
MEDIUM/LOW items (a harmless malformed-input branch, the heuristic 5xx regex, the P5 retry-hint DIAL)
were reviewed and judged acceptable/deferred with rationale. Depth matched reversibility, as directed.)*

## Config-flip (§3.5 #6): mock → real is config-only

The only things that change mock→real are the node descriptor's `base_url` and the engine's
`dns`/`smtp_port` (`/v1/config/reload` applies them without a redeploy). The brain code —
routing, parsing, classification, scoring, logging — is identical. Proven by the socket harness
re-pointing the engine across mock servers with zero code change, and by the brain integration
tests being substrate-agnostic (a fake node returns the same shapes a real one would).

## What Phase 2 does NOT build (correct scope — SEAM → later)

No queue / greylist-retry loop (P5 — requeue *shape* designed only). No real IP/PTR/warmup or the
`/audit` *enforcement* gate (P3 — contract built + read/logged only). No health-watcher / rate-limit /
pool / HELO rotation (P4). No fallback (P6). No flywheel (P7). The gem is never forked.
