# ADR-0007 — Catch-all probe runs as TWO sessions (Option B); the gem is not patched to get same-session

- **Status:** ACCEPTED (owner sign-off carried in [`phase-2-smtp-mocks-spec.md`](../phase-2-smtp-mocks-spec.md) Appendix D — OQ-P2-4 *conditioned on this spike*; OQ-P2-11 *run early*) · **RESOLVED at Phase 2** by running the spike
- **Kind:** door-referencing — a decision that **affirms Door 5** (never fork/patch the engine gem). It chooses the wrapper-side design that keeps the gem 100% unmodified. It does **not** change any of the five frozen contracts.
- **Deciders:** owner (P2 Appendix D: "Door 5 outranks same-session elegance … never patch the gem"); spike run by the P2 build (2026-07-17)
- **Source:** [`phase-2-smtp-mocks-spec.md`](../phase-2-smtp-mocks-spec.md) §5.4 / §6.3 (GAP-13, OQ-P2-4, OQ-P2-11); handoff [`phase-1-to-2-handoff.md`](../phase-1-to-2-handoff.md) ("the one thing P2 should run first"); memory [[truemail-never-fork-engine-door5]]

## Context

The catch-all probe (P2 §5) needs a **second RCPT TO** — a random/seeded non-existent address at the
target domain — to tell a real mailbox (`250` + probe-rejected → `valid`) from an accept-all domain
(`250` + probe-`250` → `risky/catch_all`). The spec's preferred **Option A** issues that 2nd RCPT
**in the same SMTP session** as the target: one HELO / one MAIL FROM / two RCPT TO / QUIT — fewer
connections (lower reputation cost on a real box) and a stronger signal (a catch-all can behave
differently across two separate sessions).

The open question (GAP-13 / OQ-P2-11): **can the forked `truemail-rack` wrapper drive that
same-session 2nd RCPT using the pinned gem's own primitives, WITHOUT patching the gem?** Door 5 says
the `truemail` engine gem (pinned `3.3.1`) is never forked or patched — "Door 5 outranks design
elegance." The owner signed OQ-P2-4 **conditioned on this spike**: run it first; if the gem can't
expose a reusable session cleanly, fall back to Option B (two sessions), and **never** patch the gem.

## The spike (run 2026-07-17 against the pinned gem source in the built image)

Read `truemail 3.3.1`'s actual SMTP implementation
(`lib/truemail/validate/smtp.rb`, `.../smtp/request.rb`, `.../smtp/response.rb`):

- **The only public SMTP entry point is `Truemail.validate(email)`** — one address per call
  (`Truemail::Validator.new(email, …).run`). There is no batch/multi-RCPT public method.
- **`Request#session_data` is `private` and hardcoded** to `{ mailfrom: verifier_email, rcptto: email }`
  — exactly one RCPT TO, not parameterizable.
- **The `Net::SMTP` handle is never exposed.** `Session#start` calls `net_smtp.start(helo:, &block)`,
  and `Net::SMTP.start(&block)` **auto-closes** the connection when the block returns. The driving
  block (`session_actions`) is constructed internally; `Request#run`, `Session#start`, and
  `smtp_handshakes` are all `private`. No method hands back an open session to add a 2nd RCPT.
- Grep across the whole gem for `rcptto` / `multiple` / `session_data` / `.start(` / `reuse`
  confirmed: **no seam** for a second same-session recipient.

**Conclusion:** Option A is reachable only by (a) monkey-patching / editing the gem (Door 5
**forbidden**), or (b) the wrapper **reimplementing** the gem's entire SMTP exchange with its own
`Net::SMTP` client. (b) is not a "patch," but it makes the wrapper a second SMTP engine — a large
new surface that duplicates and could silently drift from the pinned engine's behavior, defeating the
*spirit* of Door 5 (one engine, pinned, authoritative) for a marginal reputation/consistency gain that
does not matter against mocks and is a Phase-3+ concern on a real IP.

## Decision

**Adopt Option B: the catch-all probe runs as TWO gem calls (two sessions).** The wrapper calls
`Truemail.validate(target)` and `Truemail.validate(probe)` (the probe = a random/seeded, almost-
certainly-nonexistent local-part at the same domain), then combines the two engine results into the
`catchall` block of `POST /v1/verify` (P2 §4.1) per the §5.5 truth table. **The gem is not touched.**

- **Same-session (Option A) is explicitly NOT built** in Phase 2. The reputation/consistency edge it
  buys is a real-box (P3+) concern, inert against mocks, and not worth a gem patch or a shadow SMTP
  client. If a future phase wants it on a real box, that is a new, separately-decided escalation —
  and still never a gem patch (it would be a wrapper-owned `Net::SMTP` client, weighed on its own).
- **The weaker-signal cost is documented and carried into scoring:** two separate sessions mean a
  catch-all domain *could* answer differently across them. P2 §5.5 already handles the fallout — an
  inconclusive/uncooperative probe never mints `valid` (rows #5/#6/#7 → `risky`), so Option B's
  weaker signal can only ever cost us a *missed* `valid` (→ `risky`), **never a false `valid`**. The
  harden-toward-`unknown`/`risky` direction absorbs the weakness safely.
- **`catchall_domains` writes are unaffected:** the brain still persists the tri-state conclusion
  (P2 §5.6); Option B changes only how the two RCPT observations are gathered, not what is stored.

## Why Door 5 is NOT reopened (it is enforced)

- **The engine gem stays byte-identical to the pin.** The CI `bin/assert-gem-unmodified.sh` check
  (Door 5) continues to pass — this decision is what keeps it passing. Everything new lives in the
  **fork wrapper** (allowed) and the **brain** (allowed).
- **No shadow SMTP engine.** We deliberately reject reimplementing the SMTP exchange in the wrapper;
  the wrapper calls the gem's public `Truemail.validate` twice and serializes facts. The wrapper
  "renders no verdict" (P2 §4.1) — it returns engine facts for both RCPTs; the brain interprets.
- **The other four doors are untouched.** No response-shape, LOG, hashing, or vocab change. The
  `catchall` block is an additive field on the wrapper response (P2 §4.1), and the probe's own
  address is never logged/cached/hashed as a subject (P2 §5.3).

## Consequences

- **Positive.** The gem stays pinned and unpatched (Door 5 intact, CI-asserted). The probe is simple,
  testable, and honest. The known weakness is one-directional and safe (can only under-claim, never
  over-claim `valid`). Two RCPTs is the substrate-confirmed shape — the live `smtpmock` v2.5.4 run
  ([`truemail-ground-truth.md`](../truemail-ground-truth.md) §3.4) showed a seeded probe genuinely
  returns `550`, so target-`250` + seeded-probe-`550` = `valid` is reachable with two calls.
- **Cost.** Two SMTP connections per probed address instead of one → more connections (a reputation
  cost **only on a real box**, P3+, and a pacing concern for P4 — noted as a SEAM, not solved here)
  and a theoretically weaker cross-session signal (bounded safe, above).
- **Reversibility.** This is a wrapper-side design choice, not a frozen contract. If P3+ evidence
  shows two-session probing materially hurts on a real IP, a wrapper-owned same-session client can be
  revisited **as its own decision** — still never a gem patch. The verdict contract does not change
  either way (the `catchall` block and `signals.catchall_conclusion` are identical for A or B).

## Compliance / how it's checked

- **Door 5:** `bin/assert-gem-unmodified.sh` (CI) — the gem is unmodified; this ADR is why.
- **Behavior:** the catch-all substrate tests (P2 Appendix B rows 1 & 4) exercise two-session probing
  against real `smtp_mock` — a **seeded** probe rejection yields `valid/accepted` (row 1), and a
  default accept-all (probe also `250`) yields `risky/catch_all` (row 4). The `valid` invariant
  (target-`250` **AND** probe-rejected only) is locked by those tests.
- **The forbidden path stays forbidden:** no wrapper code opens its own `Net::SMTP` session and no gem
  file is edited — asserted by FORK.md's exhaustive modification list staying wrapper-only + the
  gem-unmodified CI check.
