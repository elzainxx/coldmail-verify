# ADR-0008 — The verification_raw blob redacts the plaintext address (Door 3); the P2 "store verbatim" text is narrowed, not the door

- **Status:** ACCEPTED · **OWNER-RATIFIED 2026-07-17** (P2→P3 readiness pass — the owner explicitly authorized ratifying this ADR and applying the spec-text amendment; recorded in [`open-decisions.md`](../open-decisions.md) §6.2). The P2-spec *instruction* (§4.1/§6.4/OQ-P2-5 "store the entire object verbatim") is narrowed to "verbatim except the redacted address identity" because that instruction, taken literally, would violate the frozen **Door 3**. The door wins; the spec is additive-only and cannot override a frozen contract. The spec text is now amended to agree with the code.
- **Kind:** door-referencing — a **tightening that AFFIRMS Door 3** (the raw full email is NEVER persisted). It changes no hashing, no response shape, no vocab. It redacts one leak the P2 spec's own sensitivity analysis missed.
- **Deciders:** Phase-2 adversarial review (CRITICAL finding, dimension = doors) + build; **owner ratified 2026-07-17**.
- **Source:** the Phase-2 review's confirmed CRITICAL finding; [`PROJECT_RULES.md`](../../PROJECT_RULES.md) Door 3; [`history/phase-2-smtp-mocks-spec.md`](../history/phase-2-smtp-mocks-spec.md) §4.1, §6.4, OQ-P2-5.

## Context

Door 3 (frozen, PROJECT_RULES.md): **"The raw full email is NEVER persisted. Only `email_hash` (the
address's identity across history) and `domain` (in clear) are stored."** The one bounded exception
is the P5 queue (`work_items`, plaintext for work-not-yet-done, purged on settle — ADR-0002). **Every
other durable store holds only `email_hash`.**

Phase 2 added the SMTP path. The fork wrapper's `POST /v1/verify` response (§4.1) echoes the checked
address in a top-level **`email` field** (`"email":"admin@bestweb.com.ua"` in the spec's own example),
and a real SMTP server frequently echoes the address **inside a rejection body**
(`550 <jane@acme.com> User unknown`). The P2 spec then instructs (§4.1 / §6.4 / OQ-P2-5): the brain
stores this **entire object verbatim** as the `verification_raw` sidecar blob — a **durable,
retention-managed (~90-day) store.**

Storing it verbatim therefore writes the **plaintext full email** into a durable store — a direct
Door-3 violation. Crucially, the P2 spec's *own* sensitivity note (OQ-P2-5) enumerates only what it
believed the sidecar would newly contain — *"verifier identity + probe addresses"* — and **never
noticed the response echoes the target's own plaintext address.** This is a spec-internal
contradiction (an additive-only doc instructing something the frozen door forbids), not a Door-3
re-decision. Per PROJECT_RULES §6 ("flag, don't fix, a spec-vs-reality mismatch that touches a frozen
door") and the primacy of the doors, the door governs.

## Decision

**Redact the plaintext checked address from the raw engine blob at the brain's persistence boundary,
BEFORE it is written to `verification_raw`.** (`control-plane/internal/orchestrator/smtp_stage.go`,
`redactAddress`.)

- The brain owns Door 3 (the box is dumb/stateless). Redaction happens brain-side, on every path that
  produces a blob (success **and** the node-error paths).
- It replaces every occurrence of the **full address** (longest match — the echoed `email` field and
  a `<local@domain>` body echo) and the **bare local-part at word boundaries** (a body that prints
  only the local part, e.g. "User jo does not exist") with a fixed `[redacted-local]` token. The
  **domain is left intact** — it is Door-3-clear (stored plaintext in `verifications.domain` by
  design), and analytics/joins rely on it.
- **No length guard (Door-3-safe):** a draft used a `len(local) >= 3` guard, which would UNDER-redact
  a short local-part (`jo@x.com`) echoed bare in a body — a Door-3 leak (caught by the review's HIGH
  finding). Removed. Word boundaries (`\blocal\b`) are what make an unguarded replace safe: a
  standalone `jo` is scrubbed, but the `jo` inside `json`/`rejected`/hex is NOT — so we never
  under-redact the identity and never shred the transcript. (Door 3 forbids persisting the raw
  address at ANY length; over-redaction would be acceptable, but word boundaries avoid even that.)
- **Edge non-word chars (found in ratification, 2026-07-17):** the `\b` anchor only works when the
  local-part STARTS and ENDS with a word char — a leading/trailing non-word char (`.foo`, `foo.`)
  breaks the anchor at that end and would leak a bare body echo. So when the local-part's first or
  last byte is non-word (`edgeIsNonWord`), the bare-local pass falls back to an UNGUARDED
  case-insensitive replace (over-redaction is Door-3-safe). Internal special chars (`foo.bar`,
  `foo+tag`, `foo-bar`) are fine with `\b` — they are literal via `QuoteMeta`, anchored only at the
  ends — **verified empirically**, not assumed. Both fallback paths (full-address and bare-local) are
  case-insensitive too, so no path can leak a case variant.
- **Case-insensitive (found during ratification, 2026-07-17):** `normalized` is lowercased (Door 3),
  but a real SMTP server may echo the address in a DIFFERENT case in a bounce body
  (`550 <SecretLocal@Acme.com> rejected` — RFC 5321 permits it). A case-sensitive replace would leak
  that variant. Both passes use `(?i)`. Locked by `TestSMTPStage_RawBlobRedactsCaseVariantEcho`
  (fail-verified: it failed against the case-sensitive draft, passes after `(?i)`).
- **Global over the whole blob (not field-aware):** redaction is a substitution over the entire blob
  string, so the address is scrubbed wherever it appears — the top-level `email` field, an
  `smtp_debug[].errors.rcptto` body, or a nested `catchall.smtp_debug[]` sub-transcript. Locked by
  `TestSMTPStage_RawBlobRedactsNestedCatchallField`. (The synthetic `nx-` probe address is an
  instrument, not PII — §5.3 — and is intentionally NOT redacted.)
- **Nothing joinable is lost:** the LOG row already carries `email_hash` (the frozen join key). The
  rest of the transcript — `smtp_debug` codes/bodies, `mx`, `catchall`, `configuration` — is preserved
  **verbatim**, so the spec's "join gold" intent (§6.4) is honored *except* for the one frozen-door
  leak. The redaction is a pure byte-substitution on the JSON (no parse of untrusted structure).
- **Locked by a regression test:** `TestSMTPStage_RawBlobRedactsPlaintextAddress` asserts the
  persisted blob contains neither the local-part in the `email` field nor the address echoed in a 550
  body, that the domain remains, and that the transcript (the `550` code) survives.

## What this narrows (and what it does NOT change)

- **Narrowed:** the P2-spec *instruction* "store the entire object verbatim" now reads "store the
  transcript verbatim **with the plaintext address identity redacted** (email_hash is the identity)."
  This is the minimal change that reconciles §4.1/§6.4 with Door 3.
- **Unchanged (all frozen):** the hashing scheme (Door 3 core), the `/verify` response shape (Door 1 —
  the caller-facing response still echoes the raw `email`, OQ-4; only the *persisted sidecar* is
  redacted), the LOG append-only property (Door 2), the vocabulary (Door 4), never-fork (Door 5).
- **Not a scheme change, not a new store, not a caller-visible change.** A verdict is byte-identical to
  the caller; only the durable blob is scrubbed.

## Why Door 3 is NOT reopened (it is enforced)

- The change makes the code CONFORM to the frozen door — the door text ("raw full email is NEVER
  persisted") is applied literally where the spec instruction would have broken it.
- Redaction is in the safe direction (refuse to persist plaintext), mirroring [ADR-0006](ADR-0006-door3-nonascii-localpart-refusal.md)'s
  posture: when a Door-3 leak is found, harden now, never ship the violation.
- `email_hash` remains the sole durable identity; `domain` remains the sole durable plaintext — exactly
  the Door-3 contract.

## Consequences

- **Positive.** No durable store holds the plaintext full email (Door 3 intact). The transcript stays
  useful for calibration (codes/bodies/mx/catchall preserved). The leak is closed *and* regression-locked.
- **Cost.** A vanishingly small chance that a local-part collides with a standalone word elsewhere in
  the blob and is over-redacted — acceptable (it only over-redacts, never under-redacts). Word
  boundaries (not a length guard) avoid shredding substrings; there is no length guard, so short
  local-parts are still scrubbed. The redaction is O(blob size), negligible.
- **Owner ratification — DONE (2026-07-17).** The P2 spec text (§4.1/§6.4/OQ-P2-5 + Appendix D) is
  amended to "verbatim except the redacted address identity," so spec and code now agree (a spec still
  instructing a door violation is a latent trap). Recorded in [`open-decisions.md`](../open-decisions.md) §6.2.

## Compliance / how it's checked

- `TestSMTPStage_RawBlobRedactsPlaintextAddress` (regression, fail-verified) — the persisted blob must
  not contain the plaintext local-part (in the `email` field OR a 550 body echo); domain + transcript
  code must remain.
- `TestSMTPStage_RawBlobRedactsShortLocalPart` (regression, fail-verified) — a 1-2 char local-part
  echoed bare in a body is scrubbed at word boundaries, WITHOUT shredding substrings (`json`/`rejected`).
- `TestSMTPStage_RawBlobRedactsCaseVariantEcho` (regression, fail-verified) — a mixed-case address echo
  is scrubbed (case-insensitive redaction).
- `TestSMTPStage_RawBlobRedactsNestedCatchallField` (regression) — the address is scrubbed even inside a
  nested `catchall.smtp_debug` field (global blob substitution); the `nx-` probe instrument is preserved.
- `TestSMTPStage_RawBlobRedactsSpecialCharLocalParts` (regression) — `foo.bar`/`foo+tag`/`foo-bar`
  (internal) and `.foo`/`foo.` (edge non-word) local-parts are all scrubbed from a bare body echo.
- Applies to all SMTP-stage return paths (success + node-error), since `redactAddress` runs immediately
  after the node call, before any blob is returned for persistence.
