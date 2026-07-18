# ADR log

Architecture Decision Records for the Truemail verification system. An ADR is required for
**any decision that touches a one-way door** (response shape, LOG, hashing, vocabulary,
never-fork) or that **elevates/gates a cross-phase decision** an owner-OQ ruling produced
(PROJECT_RULES.md §8). Cross-phase *rulings* live in [`../DECISIONS.md`](../DECISIONS.md); the
ADRs here are the durable record for the door-touching subset.

## Format

Each ADR: `ADR-NNNN-short-slug.md`, with **Status** (PROPOSED / ACCEPTED / SUPERSEDED), the
**Door** it touches, **Context**, **Decision**, **Consequences**, and — critically — a
**"does this reopen a frozen door?"** check. ADRs are append-only in spirit: supersede, never
silently rewrite (mirror Door 2).

## Index

| ADR | Decision | Door | Reopens it? | Status |
|---|---|---|---|---|
| [ADR-0001](ADR-0001-outcome-feed-authentication.md) | OQ-P7-1 elevated to a hard pre-P7 authentication door | *new* TB-7 trust gate (P7 input) | No — new gate, five frozen doors untouched | ACCEPTED |
| [ADR-0002](ADR-0002-queue-plaintext-backup-scope.md) | Queue-plaintext backup-awareness gate at P5 | **Door 3** (hashing / no-plaintext) | **No** — affirms Door 3 + the P5 bounded exception | ACCEPTED |
| [ADR-0003](ADR-0003-truemail-engine-pin.md) | truemail engine pinned exactly at 3.3.1, never forked | **Door 5** (never-fork) | **No** — enforces Door 5 | ACCEPTED |
| [ADR-0004](ADR-0004-cheap-layers-brain-native.md) | Cheap layers run control-plane-native; the control-plane takes no truemail dependency (filename keeps "brain-native" — ADRs are immutable records) | Door 5 (engine boundary) | **No** — draws the boundary, doesn't fork | ACCEPTED |
| [ADR-0005](ADR-0005-deployable-naming.md) | Deployables named: "brain"→`control-plane`, "box"→`verifier-node` | naming (durable) | **No** — a total rename, no contract changed | ACCEPTED |
| [ADR-0006](ADR-0006-door3-nonascii-localpart-refusal.md) | Non-ASCII local parts refused until NFC/IDNA is wired (P1) | **Door 3** (hashing) | **No** — a tightening that affirms Door 3; refuses, never mis-hashes | ACCEPTED |
| [ADR-0007](ADR-0007-gap13-catchall-session-two-session.md) | Catch-all probe runs as two sessions (Option B); gem not patched for same-session (GAP-13 spike) | **Door 5** (never-fork) | **No** — enforces Door 5; the gem stays unmodified, wrapper calls `Truemail.validate` twice | ACCEPTED |
| [ADR-0008](ADR-0008-door3-raw-blob-address-redaction.md) | `verification_raw` blob redacts the plaintext address; narrows the P2 "store verbatim" text (spec-internal contradiction) | **Door 3** (hashing / no-plaintext) | **No** — enforces Door 3; conforms code to the frozen door, redacts one leak the spec missed | ACCEPTED (owner-ratified 2026-07-17, DECISIONS §6.2) |
| [ADR-0009](ADR-0009-gap24-egress-ip-invariant-runtime-enforcement.md) | GAP-24 four-way egress-IP invariant runtime-enforced (gate + scheduler); `verifier_ip` sourced from the brain registry; the P3 additive surface (one `signals` key, `node_audits`/`node_warmup` tables) | **Door 2** (LOG provenance) + Door-1/4 additivity | **No** — enforces the doors; populates reserved fields, adds append-only tables, zero vocab change | ACCEPTED |
| [ADR-0010](ADR-0010-phase-4-pool-additive-surface.md) | Phase-4 pool additive surface (one `signals` value `rate_limited`, one table `rate_budgets`, the `recovering` node-lifecycle value); block-rate quarantine (A1) defaults recommend-only; the fleet-wide cascade brake (GAP-28) + canary recovery (GAP-30) built + fail-verified | Door-1/4 additivity + **Door 2** operational state | **No** — additive `signals` value + node-lifecycle value (NOT verdict vocab), new table, unchanged Strategy interface + gem | ACCEPTED |
| [ADR-0011](ADR-0011-phase-5-queue-additive-surface.md) | Phase-5 queue additive surface (three operational tables, a bulk sibling endpoint returning the SAME shape, zero new vocab); **DR-4 mechanism = separate-key at-rest encryption** of the queue plaintext (key ≠ pepper); **PN-13 = per-caller keys + rotation**; the GAP-37 lease-TTL + CAS idempotency heart built + fail-verified on Mem AND real Postgres | Door-1/4 additivity + **Door 2** operational state + **Door 3** bounded plaintext exception | **No** — enforces the doors; encrypts + purges the one bounded exception, header-only auth (caller_id key-derived), exactly-once via CAS, unchanged gem/Strategy | ACCEPTED |

## Owner-OQ rulings that became records

The P1–P7 Appendix D sign-offs are the owner-OQ rulings; the cross-phase and door-touching ones
are captured as ADRs (above) or in the [`../DECISIONS.md`](../DECISIONS.md) §6 owner-OQ register.
Per-phase-only sign-offs remain in each spec's Appendix C/D.

## Next number

The next ADR is **ADR-0012**. Increment monotonically; never reuse a number.
