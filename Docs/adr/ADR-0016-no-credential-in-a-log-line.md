# ADR-0016 — No credential reaches a log line: the DSN is scrubbed where the error is produced, and a boot-log gate enforces it

- **Status:** ACCEPTED (2026-08-04) — cross-module alignment Pass 2, Tier 0 (escalated from fix-plan 15b).
- **Kind:** door-referencing — a **tightening** of Door-3 secret custody. No contract, vocabulary,
  schema, or response shape changes. Nothing observable to a caller changes at all.
- **Deciders:** owner (alignment-plan ruling; U2 escalated to Tier 0 as "wrong today").
- **Source findings:** cross-module audit **U2** / **D12** → [`alignment-fix-plan.md`](../../../coldmail-platform/alignment-fix-plan.md)
  15b rows "verify whether pgx v5 can embed DSN fragments in the error logged at `main.go:113`" and
  "adopt a `no_secret_leak`-style CI gate" (14.4).
- **Governs:** `control-plane/internal/store/postgres.go` · `scripts/check-no-secret-leak.sh` ·
  `PROJECT_RULES.md` §1 Door 3 (custody) · every future `log.Printf` on the boot path.

---

## Context

The audit recorded D12 as an **unverified risk**: `cmd/control-plane/main.go` logs the error from
`store.OpenPG` verbatim, and it was unknown whether pgx could carry DSN material inside it. It also
noted that M1 had **no CI leak gate that would catch it either way**.

It was verified first-hand against the pinned **pgx v5.10.0**, by reading the driver source and by
measuring. Both halves of the finding are real, and the measurement is more specific than the
finding was:

| DSN shape | at this pin |
|---|---|
| `postgres://u@h/db?password=SECRET` | **LEAKS** |
| `postgres://u@h/db?sslpassword=SECRET` | **LEAKS** |
| `host=h password = SECRET dbname=db` (spaces are honored) | **LEAKS** |
| `host=h password=SECRET dbname=db` | redacted by pgx |
| `postgres://u:SECRET@h/db` | redacted by pgx |

The mechanism, in `pgconn/errors.go`: `ParseConfigError.Error()` embeds the **whole** connection
string, and the redaction meant to protect it is incomplete. `redactPW` takes the URL branch for any
`postgres://` DSN, and `redactURL` then returns `u.String()` — which re-emits `RawQuery` **verbatim**,
so a password carried as a query parameter is printed in clear. Separately, the keyword/value
branch's `password=[^ ]*` regex cannot match the spaced spelling `password = SECRET`, which libpq and
pgx both honor.

There is an offsetting fact worth recording because it bounds the severity honestly: the **parse**
stage is the leaky one, and the **connect** stage — the far likelier real-world failure, e.g. a
database that is simply unreachable — produces a clean message that structurally cannot carry a
password. So this was a live leak on a narrower path than "any DB failure at boot", not a
theoretical one and not an every-restart one.

Two decisions follow: where the defence goes, and what proves it stays.

## Decision

### 1. Scrub where the error is PRODUCED, not at the log call

`OpenPG` passes both of its error returns through `scrubDSNError(err, dsn)`. The alternative — fixing
the `log.Printf` in `main.go` — protects exactly one caller, and the next `log.Printf("%v", err)`
anywhere reintroduces the leak. Scrubbing at production means no caller can hold an unsafe value.

`scrubDSNError` replaces the **message** and keeps `Unwrap()`, so `errors.Is`/`errors.As` still reach
the driver's error: a caller legitimately testing for a pgx sentinel must not be broken by a change
about logging. The replacement text names the one thing an operator needs — *which env var to look
at* — and deliberately does **not** partially redact. Selective rewriting is precisely what failed
upstream: it leaves whatever the pattern missed.

### 2. The test is DERIVED from the DSN, not a list of known-bad spellings

`scrubDSNError` fires when the error text contains the whole DSN **or** any credential candidate
extracted from the DSN it was handed. The question is *"does this message contain one of MY
secrets"*, never *"does it match one of THEIR spellings"*. A blocklist of `password=` spellings is
exactly how pgx's own redaction came to miss two of them, and a blocklist cannot cover a future pgx
release that leaks a field nobody has enumerated.

Extraction over-collects on purpose (userinfo password, URL query values decoded, keyword/value
values quoted or spaced, bare tokens): a false positive costs one error message its detail; a false
negative costs a credential. The one guard against over-collection is a **4-character floor** —
without it, values like `db` and `5432` would match incidentally and suppress every database error,
blinding operators in the other direction.

### 3. A CI gate scans the real boot log, because a unit test cannot cover the surface

`scripts/check-no-secret-leak.sh` boots the **real** binary with eight distinct canaries — one per
secret-bearing env var — captures the process log, and fails if any canary appears. It is a separate
CI job so a red reads unambiguously as "a credential reached a log line" instead of being buried in a
380-test run.

Why a gate *and* unit tests: the unit tests guard the one call site they know about. The gate guards
the **surface** — a future `log.Printf` of a pepper, a node token, or any error value that embeds a
credential fails here even though no existing test mentions it. That distinction was measured, not
asserted: a mutation adding `log.Printf("debug: pepper=%s", p)` to `main.go` passes every unit test
in the repo and is caught by the gate.

The DSN canary is deliberately carried as a **query parameter**, the shape pgx fails to redact, so the
gate exercises the worst case rather than the one the driver already handles.

### 4. The gate asserts its own premises

"No canary found" is the *expected* observation, and it is also what a gate that scanned nothing
produces — the same vacuity class as the always-skipping grant tests (fix-plan item 3). So before
trusting an absence the gate requires: the boot lines to be present, the store open to have failed
**at the parse stage specifically** (the only stage whose message carries the DSN), and the scanned
line count to clear a floor. The parse-stage requirement is not decoration: a fixture that lost its
malformed `pool_max_conns` still reaches a connect error and still logs "store unavailable", and an
earlier draft of this gate passed under exactly that mutation.

## Why no frozen door is reopened

- **Door 1 (response shape)** — untouched. No endpoint, field, or status code changes; the scrub
  affects a log line and an error string, never a response body.
- **Door 2 (append-only LOG)** — untouched. No row is written, rewritten, or skipped; no grant or
  schema change.
- **Door 3 (hashing / no plaintext)** — **strengthened.** Door 3 makes the pepper's custody a
  first-class concern ("leak it → low-entropy addresses become brute-forceable"). A credential in a
  process log is a custody failure on the most-collected, longest-lived surface in the system. The
  hashing scheme itself is untouched.
- **Door 4 (vocabulary)** — untouched. No status, reason, or recommendation value is involved.
- **Door 5 (never-fork)** — untouched, and specifically **not** the fix. The defect is upstream in
  pgx, and the fix is a local wrapper, not a patched dependency. pgx is not a frozen-pin dependency
  the way the truemail gem is, but the same discipline applies: wrap, don't fork.

## Consequences

- **Positive.** A configured-but-unparseable DSN no longer prints its password at boot. The rule
  ("no credential reaches a log line") now has an automated enforcer instead of resting on review
  attention, and it covers credentials nobody has thought to test.
- **Cost / limits — stated plainly.**
  - **The gate covers the BOOT path only.** Config load, secrets load, the store open, API wiring,
    listen, shutdown. It does **not** cover per-request logging, because M1 has no compose stack to
    drive live traffic against (fix-plan item 15 defers that). A request-path leak scan is future
    work and is recorded as such — this is a real gap, not full coverage.
  - **It cannot see a request-time credential**, e.g. a bearer token a caller sends. Only
    boot-configured secrets are canaried.
  - **One error message loses its detail** when a DSN credential appears in it. That is the intended
    trade; the connect-stage error — the likelier failure — is untouched and keeps its host/port
    diagnosis, which is asserted by a test so an over-eager scrub cannot creep in.
  - **The scrub is keyed to the DSN it was handed.** A credential that reaches a log by some path
    that never saw the DSN is outside this defence; that is what the gate exists for.
- **Obligation on future changes.** A new secret-bearing env var must be added to the gate's canary
  list in the same change that introduces it, or the gate silently stops covering it.

## Compliance / how it's checked

`internal/store/dsn_redaction_test.go` — eight tests, each paired with a control:

- `TestDSNRedaction_NoCredentialSurvivesOpenPG` — the property, over all five DSN shapes.
- `TestDSNRedaction_PgxStillLeaksAtThisPin` — the **anti-vacuity control**, and the important one: it
  asserts the raw driver error *still* leaks, so the suite is measuring a defence rather than a pgx
  version that never leaked. A future pgx bump that fixes redaction upstream fails this test, which
  is the signal to re-read the upstream code and re-decide, not to delete the assertion.
- `TestDSNRedaction_CleanErrorsKeepTheirDetail` — a `127.0.0.1:1` connect error must NOT be scrubbed
  and must keep its host, so an unconditional scrub cannot pass.
- `TestDSNRedaction_UnknownKeywordShapeStillCaught` — the derived-not-enumerated property (§2).
- `TestDSNRedaction_UnwrapSurvivesScrubbing` — `errors.Is` still reaches the original; `nil` stays nil.
- `TestDSNRedaction_ShortValuesDoNotSuppressEverything` — the 4-char floor, from the other side.
- `TestDSNRedaction_CandidateCheckStandsAlone` / `..._WholeDSNCheckStandsAlone` — one per internal
  check. These exist **because** mutation testing showed that deleting either check alone left every
  `OpenPG`-level test green: pgx echoes the DSN entire, so the whole-DSN check satisfies every
  end-to-end leak assertion by itself, and the candidate machinery — the part that covers a driver
  naming one credential without quoting the connection string — was unproven until it was tested
  directly. A test that cannot fail is not evidence.

**Fail-verified 2026-08-04**, each mutation reverted with no residue (md5 confirmed):

| mutation | result |
|---|---|
| drop `scrubDSNError` from either `OpenPG` return | caught — 4 tests |
| scrub unconditionally | caught — `CleanErrorsKeepTheirDetail` + `ShortValuesDoNotSuppressEverything` |
| delete the URL-query candidate branch | caught — `CandidateCheckStandsAlone/url-query` |
| delete the bare-token candidate branch | caught — `CandidateCheckStandsAlone/kv-spaced` + `UnwrapSurvivesScrubbing` |
| delete the whole-DSN containment check | caught — `WholeDSNCheckStandsAlone` |
| **gate:** restore the D12 defect (raw error to the log) | caught by `check-no-secret-leak.sh` — the password appears in the real boot log |
| **gate:** add `log.Printf("debug: pepper=%s")` to `main.go` | caught by the gate; **passes every unit test**, which is the case a unit test structurally cannot cover |
| **gate:** remove the malformed `pool_max_conns` from the fixture | caught by the parse-stage premise check (it was **not** caught before §4 was added) |
| **gate:** truncate the log before the scan | caught by the line-count floor (likewise not caught before §4) |
