# ADR-0016 — No credential reaches a log line: the DSN is scrubbed where the error is produced, and a boot-log gate enforces it

- **Status:** ACCEPTED (2026-08-04) — cross-module alignment Pass 2, Tier 0 (escalated from fix-plan 15b)
  · **AMENDED 2026-08-07** — §2's claim that the 4-character floor was sufficient guard against
  over-collection was **wrong, and wrong in the way it itself predicted**. See "Amendment" below.
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

> **AMENDED 2026-08-07 — the paragraph above is wrong.** `5432` is four characters. It *clears* the
> floor this ADR just named it as the reason for, so the floor never guarded the case it was written
> to guard, and the "blinding operators in the other direction" outcome is the one that actually
> shipped. The guard is now **`structuralDSNKeys`**, an allowlist of keywords whose libpq semantics
> make a secret value impossible; the floor remains only as a backstop beneath it. Details in
> §Amendment.

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
    > **AMENDED 2026-08-07.** The second half of that sentence was **false as written** from
    > 2026-08-04 to 2026-08-07. The connect-stage error was *not* untouched: `5432` and the hostname
    > were both candidates, so `connection refused` and `no such host` were suppressed too. The test
    > that was supposed to assert it (`ShortValuesDoNotSuppressEverything`) could not fail. It is true
    > now, and §Amendment names the two tests that make it so.
  - **The scrub is keyed to the DSN it was handed.** A credential that reaches a log by some path
    that never saw the DSN is outside this defence; that is what the gate exists for.
- **Obligation on future changes.** A new secret-bearing env var must be added to the gate's canary
  list in the same change that introduces it, or the gate silently stops covering it.

## Amendment (2026-08-07) — the over-collection this ADR predicted was live for three days

Applying the same measurement discipline to the sender modules' equivalent leak turned this up in M1,
where a scrub already existed. It was wrong in the **opposite direction** from D12: it suppressed
messages that contained no credential at all.

`dsnCredentialCandidates` treated the value of *every* connection keyword as possibly-secret. So
`port=5432`, `dbname=verifydb`, `user=cp_runtime` and `host=db.internal` all became candidates, and
any driver message quoting one of them was replaced wholesale. Measured against the real pgx v5.10.0
messages, **5 of 6 credential-free operator errors were destroyed**:

| real message | before the amendment | on |
|---|---|---|
| `dial tcp 10.0.0.5:5432: connect: connection refused` | **SUPPRESSED** | `5432` |
| `hostname resolving error: lookup db.internal: no such host` | **SUPPRESSED** | `db.internal` |
| ``failed to connect to `user=cp_runtime database=verifydb` `` | **SUPPRESSED** | both |
| `failed to configure TLS (sslmode is invalid: require)` | **SUPPRESSED** | `require` |
| `password authentication failed for user "cp_runtime"` | **SUPPRESSED** | `cp_runtime` |
| a genuine credential in the message | suppressed (correct) | — |

The operational cost is concrete, and it inverts a claim made in §Consequences below: that section
says "the connect-stage error — the likelier failure — is untouched and keeps its host/port
diagnosis." It did not. **A Control whose database was simply DOWN reported a DSN syntax error**, and
`connection refused` — the message an operator most needs — was the one *guaranteed* to be destroyed,
because `5432` appears in every DSN and in every dial error.

**Why the existing eight tests did not catch it.** There was a counterweight test for exactly this
direction, `TestDSNRedaction_ShortValuesDoNotSuppressEverything`, and it passed. Its fixture is the
URL form `?sslmode=disable`: the only value it can extract is `disable`, which never appears in a
connect error, so the test could not fail however badly the extraction over-collected. The test's
**fixture shape dodged the bug it existed to catch** — a distinct false-green class from the others
found in this work, and the one worth carrying forward: *a control is only a control against the
shapes it actually instantiates.* The mutation table below did not catch it either, because its
"scrub unconditionally" row cites that same test as the thing that catches over-scrubbing.

**The fix.** `structuralDSNKeys` — an allowlist of keywords whose libpq semantics make a secret value
impossible (`host`, `port`, `dbname`, `user`, `sslmode`, the `pool_*` options, and so on) — skipped in
both the URL-query and the keyword/value branch. `password`, `sslpassword`, and every keyword nobody
has enumerated are deliberately absent.

**The direction of risk is deliberate, and it is the opposite of a blocklist's.** A key *missing* from
the map over-collects: its value becomes a candidate and one message loses its detail. A key *wrongly
present* under-collects, and that costs a credential. So the map contains only keywords whose
semantics make a secret impossible, unknown keys stay possibly-secret, and §2's derived-not-enumerated
property is preserved rather than traded away — which is what `UnknownKeywordShapeStillCaught` pins.
`sslkey` and `passfile` are in the map because they are **paths**; the file they name holds the
secret, and a path in a log line is infrastructure detail.

## Compliance / how it's checked

`internal/store/dsn_redaction_test.go` — **ten** tests (eight below, plus the two the 2026-08-07
amendment added), each paired with a control:

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
  **This test passed over the over-suppression defect for its whole life** (see §Amendment): its
  fixture is the URL form `?sslmode=disable`, whose only extractable value never appears in a connect
  error. Kept, because the floor is still a real backstop and the test still pins it — but it is no
  longer the evidence for "structural values do not suppress". The two tests below are.
- `TestDSNRedaction_StructuralValuesNeverSuppress` *(added 2026-08-07)* — 11 cases over both DSN
  forms, each asserting a credential-free message survives **unchanged**. Three are URL-query cases
  carrying verbatim measured pgx messages, and they exist because mutation testing showed the first
  eight left the query-branch skip **unproven**: `sslmode=require` yields a value that is not in the
  message, and `connect_timeout=10` yields one under the 4-char floor. That is the same fixture-shape
  weakness described above, caught this time by breaking the branch and watching nothing go red. A
  case only controls that branch if its structural value is ≥4 chars, appears in the message, **and**
  the message does not also echo the whole DSN — otherwise the containment check suppresses it and
  masks the branch entirely.
- `TestDSNRedaction_SpacedCredentialStillCaught` *(added 2026-08-07)* — 8 cases, the counterweight in
  the other direction, so "stop suppressing structural values" cannot be satisfied by collecting
  **nothing**. Without it the amendment's fix and a deleted scrub are indistinguishable.
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
| scrub unconditionally | caught — `CleanErrorsKeepTheirDetail` + `ShortValuesDoNotSuppressEverything` — **but see the amendment row below: this row overstated what the second test could detect** |
| delete the URL-query candidate branch | caught — `CandidateCheckStandsAlone/url-query` |
| delete the bare-token candidate branch | caught — `CandidateCheckStandsAlone/kv-spaced` + `UnwrapSurvivesScrubbing` |
| delete the whole-DSN containment check | caught — `WholeDSNCheckStandsAlone` |
| **gate:** restore the D12 defect (raw error to the log) | caught by `check-no-secret-leak.sh` — the password appears in the real boot log |
| **gate:** add `log.Printf("debug: pepper=%s")` to `main.go` | caught by the gate; **passes every unit test**, which is the case a unit test structurally cannot cover |
| **gate:** remove the malformed `pool_max_conns` from the fixture | caught by the parse-stage premise check (it was **not** caught before §4 was added) |
| **gate:** truncate the log before the scan | caught by the line-count floor (likewise not caught before §4) |

**The amendment is break-verified separately, 2026-08-07**, seven breaks in a throwaway copy, all run
in Docker (the Windows host does not reliably execute freshly-built Go test binaries, so a host-run
result is not admissible evidence here). Baseline GREEN and restored GREEN on either side:

| break | result |
|---|---|
| revert to the original over-collecting extraction (the measured defect) | **RED** — `StructuralValuesNeverSuppress`, 3 subtests |
| `structuralDSNKeys` emptied | **RED** — same, 3 subtests |
| `knownDSNKey` no longer recognises the `password` KEY | **RED** — `…/the literal word password in a SERVER error` |
| the URL-query structural skip removed | **RED** — the 3 URL-query subtests, **and only those**, which is what makes them that branch's sole control |
| spaced `password = SECRET` tracking removed (a credential escapes) | **RED** — `…/port, kv SPACED` |
| candidates collect **nothing** (the fix satisfied by doing nothing) | **RED** — `SpacedCredentialStillCaught` + `UnwrapSurvivesScrubbing` |
| the whole-DSN containment check removed | **RED** — `WholeDSNCheckStandsAlone` |

The fourth row is the one worth keeping: on the first attempt it came back **GREEN**, which is how the
missing URL-query fixtures were found. A break that does not go red means the branch it breaks is
unproven, not that the code is right.
