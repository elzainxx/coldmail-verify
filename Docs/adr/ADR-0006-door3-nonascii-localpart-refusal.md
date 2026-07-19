# ADR-0006 — Non-ASCII local parts are refused until NFC is wired (P1); Door 3 unchanged

- **Status:** ACCEPTED (owner sign-off 2026-07-17 — Phase-0 sign-off sheet §B) · **RESOLVED at Phase 1** (NFC/IDNA wired — see "Phase-1 resolution" below)
- **Kind:** door-referencing — a **tightening** that **affirms** Door 3. Refuses (does not mint, change, or reverse) a hash scheme. Does **not** reopen or modify the frozen `norm/v1+hmac-sha256/v1` scheme.
- **Deciders:** owner (Phase-0 closeout sign-off, "accept all + record")
- **Source finding:** Phase-0 code-review pass (inline adversarial sweep) · [`phase-0-signoff-sheet.md`](../phase-0-signoff-sheet.md) §B · [`phase-0-status.md`](../phase-0-status.md) "Code-review gate — outcome"

## Context

Door 3 (`control-plane/internal/hash/hash.go`) implements the frozen `norm/v1+hmac-sha256/v1`
scheme. The `norm/v1` pipeline *specifies* Unicode NFC on both the local and domain sides plus
IDNA ToASCII on the domain. Phase 0 is deliberately **stdlib-only** (no `x/text`, no `x/net/idna`
fetch), so the NFC/IDNA steps are marked as explicit **P1 wiring points**, and the ASCII path is
implemented in full.

The Phase-0 code-review pass found an asymmetry: `Normalize` guarded `isASCII` on the **domain**
but **not the local part**. So a non-ASCII local part (e.g. a decomposed diacritic) would be
hashed **without NFC** — and the composed (`jöhn`, one code point) vs decomposed (`jo` + combining
diaeresis) byte forms of the **same visual address** would produce **two different `email_hash`
values**, splitting one identity across the frozen, append-only store. Because `email_hash` is the
Door-3 join/dedup/cache key the whole system is built on, a silent split is a first-class Door-3
corruption — invisible until the flywheel (P7) tries to join outcomes back and finds one person in
two buckets.

This was confirmed empirically with a failing test before the fix, and is the sharpest kind of
finding: correct-looking code that silently corrupts the most expensive-to-undo door.

## Decision

**Refuse non-ASCII on BOTH the local and domain sides** (`ErrUnparseable`) until the full
`norm/v1` NFC + IDNA pipeline is wired at Phase 1. Such an address is **logged with a NULL
`email_hash`** (the P1 §3.2.7 un-addressable-reject path) — it is never mis-hashed and never
silently dropped.

- **Applied (Phase 0):** `hash.go` `Normalize` now refuses `!isASCII(local)` symmetrically with the
  existing domain guard, each marked as the P1 NFC/IDNA wiring point.
- **Locked by a regression test:** `TestNonASCIILocalPartRefused`
  (`control-plane/internal/hash/hash_test.go`) proves **both** the composed and decomposed forms
  are refused, and documents *why* (they would hash differently). The original suite missed this by
  exercising only the domain path.
- **Phase-1 hand-off:** when P1 wires NFC (`x/text/unicode/norm`) + IDNA (`x/net/idna`), these inputs
  begin hashing **correctly** (NFC-normalized) — additive, no scheme change, no historical rewrite,
  because no under-normalized hash was ever admitted in the interim.

## Why Door 3 is NOT reopened

- **The frozen scheme is unchanged.** `norm/v1+hmac-sha256/v1`, the pepper, the 32-byte `bytea`
  key, and the HMAC construction all stand verbatim. `norm/v1` *already* specifies NFC — this ADR
  changes none of that text.
- **This is a tightening in the safe direction, not a scheme change.** The honest Phase-0 stance is
  "refuse what we cannot yet normalize correctly" rather than "hash it wrong now, correct it later"
  — the latter would poison the frozen store. Refuse > mis-hash.
- **It is temporary-by-construction and additive.** The refusal is scoped to the interval before P1
  wires NFC/IDNA; once wired, the same inputs succeed. Nothing that hashed before this fix hashes
  differently after it (ASCII inputs are unaffected — the guard only fires on bytes ≥ 0x80).
- **The other four doors are untouched.**

## Consequences

- **Positive.** No under-normalized (wrong) `email_hash` can enter the append-only store during the
  Phase-0→P1 interval. The join key stays single-valued per identity. The gap is closed *and* pinned
  by a permanent regression test, so a future refactor that "helpfully" lets non-ASCII through
  without NFC fails loudly.
- **Cost.** Non-ASCII (IDN / non-Latin local-part) addresses are un-addressable until P1 wires
  NFC/IDNA — they are logged with a NULL hash, not verified, in that window. Acceptable: P1 is the
  very next phase and wires exactly this. The alternative (mis-hashing them now) is the corruption
  Door 3 exists to prevent.
- **P1 obligation.** P1's Door-3 work MUST wire NFC + IDNA and, at that point, MAY relax these two
  guards **together** (never one before the other — an asymmetric relax reintroduces the split). The
  regression test then flips from "must refuse" to "must normalize equal" for the composed vs
  decomposed pair — the same test, asserting the stronger post-NFC invariant.

## Compliance / how it's checked

- **Now (Phase 0):** `TestNonASCIILocalPartRefused` + `TestNonASCIIDomainRefusedNotMishashed` — both
  must pass; both forms refused.
- **At P1 (senior-qa):** after NFC/IDNA is wired, add the paired invariant test — composed and
  decomposed forms of the same address MUST produce the **same** `email_hash`; and an IDN domain and
  its punycode form MUST agree. Regression = the two forms diverge (the original bug, now impossible
  to reintroduce silently).

## Phase-1 resolution (2026-07-17) — NFC/IDNA wired, interim refusal lifted

Phase 1 discharged the P1 obligation above. `control-plane/internal/hash/hash.go` now implements the
full `norm/v1` pipeline; the two interim non-ASCII guards were removed **together**:

- **Local side:** `NFC → lowercase → NFC`. The **trailing** NFC is load-bearing (not redundant):
  Unicode case mapping is not closed under NFC (UAX#15), so `ToLower` can emit a non-NFC sequence —
  without the second NFC, an address and its own case-variant twin (e.g. `J`+combining-caron vs
  precomposed `ǰ`) would hash differently. This was found by the Phase-1 code-review pass (a
  reproducible split across ~7 base-letter+mark classes in a sweep) and fixed; the trailing NFC is a
  **strict no-op on all ASCII**, so no already-valid ASCII identity shifts.
- **Domain side:** IDNA/UTS-46 `ToASCII` **first**, then strip trailing dot(s). Strip-after-ToASCII
  is required because UTS-46 maps the Unicode label separators U+3002/U+FF0E/U+FF61 (`。．｡`) to ASCII
  `.` during `ToASCII` — stripping before would split `example.com。` from `example.com`. Profile:
  `idna.New(MapForLookup(), Transitional(false), StrictDomainName(true))` — encodes real IDNs, rejects
  genuine garbage (disallowed runes, invalid labels) as `ErrUnparseable` (NULL hash, never mis-hashed).
- **`provider_canonical_hash` (OQ-2)** added: gmail dot/`+tag` folding lives ONLY here, gated on the
  literal `gmail.com`/`googlemail.com` domains (never the whole `google` class — Workspace custom
  domains may treat dots as significant). A fold that empties the local → `ok=false` (NULL), never a
  degenerate shared key. The primary `email_hash` never folds provider quirks (§3.3.5 trap avoided).

**One documented frozen edge:** the Turkish dotted/dotless-I. The composed `İ` (U+0130) and its
decomposed form (`I`+U+0307) **agree** (NFC closure holds); `İ`-derived forms lowercase to plain `i`
(Go simple case mapping drops the dot) — a stable, deterministic choice, pinned by
`TestLocalCaseFoldFrozenEdge`. Accepted as-is rather than special-cased (a `cases.Fold`-style
primitive would change other semantics, e.g. `ß→ss`).

**Compliance (now real, replacing the P1-obligation bullet above):**
- `TestNonASCIILocalPartNFCEqual` — composed == decomposed → same hash (the original bug, proven fixed).
- `TestNonASCIIDomainPunycoded` — Unicode domain and its punycode form hash identically.
- `TestTrailingDotVariantsCollapse` — FQDN + double-dot + three CJK separators all collapse.
- `TestGoldenVectorFrozen` — **absolute** golden HMAC vector pinning the frozen construction (any
  change to keying/tag/order de-joins history and fails here). The single most important guard.
- `TestLocalCaseFoldFrozenEdge`, `TestProviderCanonical*` — the İ edge and the OQ-2 canonical rules.
