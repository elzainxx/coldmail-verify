# Phase 0 — owner sign-off sheet

**Status:** AWAITING OWNER SIGN-OFF. Nothing here is resolved by the build; each item is a
decision **only the owner makes.** Each carries a recommendation but **none is pre-decided.**
**How to use:** check the box (or write your ruling) next to each item. Items are grouped:
(A) spec-vs-reality deltas GT-1…GT-5, (B) the Door-3 hashing-touching fix that needs conscious
blessing, (C) the LICENSE choice.
**Sources:** [`truemail-ground-truth.md`](truemail-ground-truth.md) · [`phase-0-status.md`](phase-0-status.md) · [ADR-0003](adr/ADR-0003-truemail-engine-pin.md) · [ADR-0005](adr/ADR-0005-deployable-naming.md)

> **Discipline:** none of these mutate a frozen door. GT-3 and the Door-3 fix are the two that
> *touch* hashing/dependency territory and are flagged loudest. The parked sub-decisions in
> [`DECISIONS.md`](DECISIONS.md) §5 are **not** on this sheet — they remain owner/phase-time and
> untouched by this closeout.

---

## A. The five spec-vs-reality deltas (GT-1 … GT-5)

| GT | The delta | Touches a door? | Blast radius | Recommendation | Your ruling |
|---|---|---|---|---|---|
| **GT-1** | Engine version: specs' appendices cite `gem@3.3.0`; the real current release is **`3.3.1`**. | **No** — `3.3.0` was an *illustrative* README-capture string, never frozen. `engine_version` provenance is a LOG field, not a door value. | Provenance string only (`engine_version = rack@…+gem@3.3.1`). | **Accept 3.3.1** (pinned, ADR-0003). It's the current release; the spec string was illustrative. | ☐ accept ☐ other: ______ |
| **GT-2** | Rack version: specs cite `rack@0.10.0`; the real latest release tag is **`v0.9.0`** (no `0.10.0` tag exists). | **No** — same: illustrative spec string. | Provenance string only. | **Accept v0.9.0** as the fork base (FORK.md). Revisit if upstream cuts a newer tag. | ☐ accept ☐ other: ______ |
| **GT-3** | `net-smtp`: pinning gem `3.3.1` **forces** the fork's `net-smtp` from `~> 0.4.0` → **`~> 0.5.0`** (3.3.1 requires 0.5.0 on Ruby ≥3.1; upstream's 0.4.0 is a disjoint range = hard resolver conflict). | **Touches Door 5 territory** — but as a **rack-side (fork) dependency edit**, NOT a gem patch. The engine gem is byte-for-byte unmodified. | Fork build resolvability only. `net-smtp` is pure-Ruby; scoring-neutral. | **Accept the bump** — it's forced by the exact pin you asked for, is on the fork (which we *do* edit), and touches no gem code. Recorded in FORK.md #2 + ADR-0003. | ☐ accept ☐ other: ______ |
| **GT-4** | `host_audit` live output shape. | **No** (informs the P3 audit gate; the gate itself is frozen design). | P3 audit gate design assumption. | **CONFIRMED LIVE** — keys `[current_host_ip, warnings, date, configuration]`, warning keys `["dns","ptr"]`. Gate anchor matches reality. **Accept.** | ☐ accept ☐ other: ______ |
| **GT-5** | `smtp_mock` accept-all (P2 GAP-14 — `valid`-path needs a seeded probe). | **No** (a Phase-2 dev-substrate property; the `valid`-path invariant is frozen). | P2 test-substrate realism. | **Scope call:** GAP-14 is `smtp_mock` (P2); Phase-0 stock rack drove **real DNS/MX** (live SMTP egress confirmed), not the mock. **Accept** — engine-does-SMTP proven; mock fixture is a P2 verification. | ☐ accept ☐ other: ______ |
| **GT-6** | `host_audit.as_json` carries **extra** keys (`date`, `configuration`) beyond the specs' minimal `{current_host_ip, warnings}`. | **No** — additive; the P3 gate reads only `warnings`. | none. | **Accept** — additive/ignorable, gate logic unaffected. Record only. | ☐ accept ☐ other: ______ |

### A.1 GT-4 / GT-5 / GT-6 — completed from the live run (2026-07-17)

*Populated from the actual `docker build` + `docker run` of `truemail-verifier-node:p0`. Verbatim
output is in [`truemail-ground-truth.md`](truemail-ground-truth.md) §0 "LIVE captured output".*

- **GT-4 `host_audit` (LIVE):** `Truemail.host_audit.as_json` returned
  `{"date":…,"current_host_ip":"197.51.161.198","warnings":{"dns":"A-record…","ptr":"PTR-record…"},"configuration":{…}}`
  — the P3 gate anchor (`current_host_ip` + `warnings`, green iff `warnings=={}`) matches reality.
- **GT-5 `smtp_mock` accept-all:** deferred to P2 **correctly** — the Phase-0 stock rack drives real
  DNS/MX (live SMTP egress to gmail.com confirmed, timed out → `unknown`-shaped, never fabricated). The
  smtp_mock seeded-probe fixture is a P2 substrate verification, not a Phase-0 row.
- **GT-6 (new):** `host_audit.as_json` also carries `date` + `configuration` — additive, gate reads only
  `warnings`. Non-door.
- **Door 5 (bonus, proven live):** `Truemail::VERSION = 3.3.1` at runtime; `bundle` installed truemail
  3.3.1 + net-smtp 0.5.1. The fork genuinely runs — we are extending it, not rewriting it.

---

## B. The Door-3 local-part fix — a hashing-touching change needing conscious blessing

> **This is flagged loudest because it touches Door 3 (email hashing) — the most
> expensive-to-undo door.** It is a **tightening** (refuse-until-NFC), not a scheme change, but
> because *anything* touching hashing deserves your conscious eyes, here it is in full.

**What it is:** the code-review pass found that `hash.Normalize` guarded non-ASCII on the
**domain** but not the **local part** — so a non-ASCII local part (e.g. a decomposed diacritic)
would be hashed **without NFC**, and the composed vs decomposed byte forms of the *same* address
would produce **different `email_hash` values** — splitting one identity in the frozen store.

**The fix (already applied):** refuse non-ASCII on **both** sides (`ErrUnparseable`) until the full
`norm/v1` NFC + IDNA pipeline is wired at **P1**. So no wrong (under-normalized) hash can enter
history in the interim — the address is logged with a NULL hash (the P1 §3.2.7 un-addressable-reject
path), never mis-hashed.

- **Why it's a tightening, not a door change:** the frozen `norm/v1` scheme *already* specifies NFC.
  Phase 0 hadn't wired NFC yet, so the honest Phase-0 stance is "refuse what we can't yet normalize
  correctly" rather than "hash it wrong." When P1 wires NFC/IDNA, these inputs start hashing
  correctly — additive, no scheme change.
- **Locked by a regression test:** `TestNonASCIILocalPartRefused` (`control-plane/internal/hash/hash_test.go`)
  — proves both composed and decomposed non-ASCII local parts are refused, and documents *why*
  (they would hash differently). The original suite missed this by only testing the domain path.
- **Blast radius:** correctness of the Door-3 join key. Getting it wrong silently corrupts the key
  the whole system depends on — which is exactly why it's surfaced for conscious blessing.

**Recommendation:** **bless the fix as-is.** It's the safe direction (refuse > mis-hash), it's
locked with a test, and it changes no frozen scheme — it defers the non-ASCII path to P1 where NFC
lands. **Your ruling:** ☐ bless ☐ other: ______

---

## C. The project's own LICENSE

**What it is:** third-party components (`truemail` engine, `truemail-rack` fork — both MIT) are
respected and attributed in [`NOTICE`](../NOTICE). **The project's own license is deliberately not
asserted** — that is an owner/business decision, not something Phase 0 should invent.

- **Blast radius:** legal/distribution posture. No code impact.
- **Recommendation:** **pick one before any external distribution.** If this stays proprietary,
  add a `LICENSE` stating "All rights reserved" / proprietary terms. If it's ever opened, MIT/Apache-2.0
  are compatible with the MIT third-party components. No recommendation on *which* — that's your call.
- **Your ruling:** ☐ proprietary / all-rights-reserved ☐ MIT ☐ Apache-2.0 ☐ other: ______ ☐ defer (stays unlicensed for now)

---

## Sign-off

By checking the boxes above you accept the deltas/fix/license as ruled. Anything you mark "other"
or leave unchecked stays open and blocks nothing except its own dependent work. **None of these
gates Phase 0 completion except your awareness of them** — Phase 0 is done when this sheet is in
front of you, not when you've signed it.

**Owner:** __________________  **Date:** __________
