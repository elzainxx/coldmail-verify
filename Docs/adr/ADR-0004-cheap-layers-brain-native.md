# ADR-0004 — Cheap layers run control-plane-native; the control-plane takes no truemail dependency

**Status:** ACCEPTED — 2026-07-17 (Phase 0; restates the P1 OQ-6 sign-off as an ADR)
**Door:** touches Door 5 (engine boundary) — does NOT reopen it
**Deciders:** owner (P1 Appendix C batch-accept of OQ-6); recorded as an ADR in Phase 0
**Companions:** [ADR-0003](ADR-0003-truemail-engine-pin.md) · [ADR-0005](ADR-0005-deployable-naming.md) · [`../../PROJECT_RULES.md`](../../PROJECT_RULES.md) · P1 §6 / §9 OQ-6

> **Terminology note (ADR-0005):** the deployables were renamed after this ADR was written —
> the design docs' **"brain" → `control-plane`** and **"box" → `verifier-node`**. This ADR's
> content uses the new names; the **filename keeps `brain-native`** because ADRs are immutable
> records (renaming the file would break durable references — the ADR *number* is the identity).
> "control-plane-native" here == the P1 design term "brain-native".

---

## Context

The build plan lists "cheap layers" (syntax/regex, typo, role, free-vs-corp, MX/DNS, null-MX,
disposable, spam-trap, provider classification). Some overlap with things the `truemail` gem *can* do
(regex, MX). P1 OQ-6 asked, as a build-time choice: do the cheap layers **call a mocked/real engine**,
or run **control-plane-native** (Go)?

This is recorded as an ADR (not just a spec line) because it is **structurally hard to reverse**: it
determines whether the Go control-plane takes a dependency on the Ruby engine at all, and therefore
whether the two-deployable boundary (README.md) is clean or blurred.

## Decision

**Cheap layers run control-plane-native in Go.** Specifically (P1 OQ-6, batch-accepted):
- syntax/regex, MX/DNS resolution, and null-MX (RFC 7505) detection are **control-plane-native** — the
  control-plane resolves DNS itself for the cheap path (it needs the MX hosts anyway for provider
  classification, and the rack JSON omits `mail_servers` — GAP-2).
- the **engine is used only for the SMTP path** (Phase 2+), which lives verifier-node-side.

**Consequence, made explicit:** the **Go control-plane takes NO `truemail` dependency.** The gem lives
*only* in `verifier-node/truemail-rack/` (Ruby). This is why `control-plane/go.mod` is stdlib-only at
Phase 0 and declares no engine import.

## Why this is the right cut

- **Keeps the two-deployable boundary clean** (README.md): the control-plane is Go + its own logic; the
  engine is a verifier-node-side Ruby dependency. No cross-language coupling for the cheap path.
- **Avoids an engine round-trip for the cheapest, highest-volume checks** — a syntax reject shouldn't
  need a network hop to a Ruby verifier-node.
- **The control-plane needs its own MX/DNS view regardless** for provider routing and null-MX; running
  the cheap path control-plane-native means one resolver for that purpose, with the engine's resolver
  authoritative only for the *SMTP verdict* (the GAP-10/OQ-24 reconciliation, P2 §6.6).

## Relationship to Door 5 (does not reopen it)

Door 5 says *never fork the engine; talk to a node list*. Running cheap layers control-plane-native is
**not** forking or reimplementing the engine — it is choosing which checks are the control-plane's own
responsibility (they always were: disposable/spam-trap/typo/provider-class/null-MX are **not** truemail
features — GAP-5). The engine still does exactly what it does (SMTP validation), unmodified and
verifier-node-side. So this ADR **touches** the engine boundary (it draws where the control-plane stops
and the verifier-node starts) without reopening Door 5.

## Consequences

- **Positive:** clean language/deployable boundary; no truemail dependency in the control-plane; the
  cheap path has no network hop; Phase-0 control-plane builds stdlib-only.
- **Cost:** two DNS resolvers exist once SMTP arrives (control-plane's for routing, engine's for the SMTP
  verdict) — a known, named reconciliation seam (P2 §6.6, engine-MX authoritative for the verdict,
  control-plane-MX advisory for routing, divergence logged). Accepted, not a surprise.
- **Enforcement:** `control-plane/go.mod` has no truemail dependency; ADR-0003's Door-5 check keeps the
  engine pinned verifier-node-side.
