# Onboarding — the system, and how to work on it

This is the one document to read first. It explains what the system is, how it's built, the
handful of decisions that are frozen and why, how to run and test everything locally, and the
review discipline the codebase is maintained under. Read this and you can be productive without
reading the phase specs — those are the deep reference, this is the map.

---

## 1. What the system is

An **email-verification system**: given an email address, it returns an honest verdict about
whether that address can receive mail — `valid`, `invalid`, `risky`, or `unknown` — with a
confidence score and a recommended action. It is designed around one principle:

> **It reports what it can prove, and says `unknown` when it can't — it never guesses a verdict.**

Everything in the architecture follows from that. The system verifies addresses through
progressively more expensive layers (cheap syntax/DNS checks first, real SMTP probing next, a
paid third-party fallback last), learns from real send outcomes to calibrate its own confidence
over time, and degrades safely — removing capability lowers a verdict's *confidence*, never its
*honesty*.

---

## 2. The two-deployable architecture

The system is two decoupled programs. This split is deliberate and load-bearing.

```
   caller ──HTTP──▶  ┌───────────────────┐        ┌──────────────────────┐
                     │  control-plane    │──HTTP─▶ │  verifier-node       │
                     │  (Go, "the brain")│        │  (Ruby, "the box")   │
                     │                   │        │                      │
                     │  all state        │        │  stateless SMTP probe│
                     │  all decisions    │        │  a pinned, forked    │
                     │  the LOG          │        │  truemail engine     │
                     │  policy, calib.   │        │  port 25 outbound    │
                     └───────────────────┘        └──────────────────────┘
```

- **`control-plane/` — the brain (Go).** Holds *all* state (Postgres), makes *all* decisions:
  policy, the append-only verdict LOG, the cheap verification layers, node routing and rate
  limiting, the async queue, the paid-fallback logic, and the confidence-calibration flywheel.
  It has no port-25 dependency and never talks SMTP itself.

- **`verifier-node/` — the box (Ruby).** A dumb, stateless SMTP probe: a fork of the
  `truemail-rack` HTTP server wrapping a **pinned, unmodified `truemail` engine**. It does one
  thing — attempt an SMTP conversation from a reputation-bearing IP and report the raw facts.
  It renders no verdict; all interpretation is the brain's.

Why split them: the box is the part that must live on a scarce, reputation-sensitive, port-25
IP; the brain is the part that holds secrets and accumulates the data. Keeping the box dumb and
stateless means adding a second box is a config line, and a compromised box leaks nothing but
itself.

### The Go packages (control-plane/internal/)

| package | role |
|---|---|
| `vocab` | the frozen `status`/`reason`/`recommendation` enums (Door 4) |
| `hash` | the frozen email-normalization + HMAC hashing scheme (Door 3) |
| `verdict` | the `/verify` response shape (Door 1) + its assembler |
| `store` | Postgres + an in-memory fake; the append-only LOG (Door 2) |
| `policy` | the policy engine (which layers run, gates, thresholds) |
| `layers` | the cheap verification layers (syntax, MX/DNS, disposable, role, …) |
| `orchestrator` | the verify lifecycle — runs the layers/SMTP/fallback/flywheel in order |
| `router` / `node` / `ratelimit` / `health` | the node pool: routing, the HTTP client, per-provider pacing, the health-watcher control loop |
| `queue` | the async bulk queue: lease/CAS idempotency, retry/backoff, the drainer |
| `external` | the P6 third-party fallback: anti-laundering, spend breaker (ships disabled) |
| `flywheel` | the P7 loop: bounce classifier, recalibration, promotion, weaning |
| `audit` | the node audit/eligibility gate (egress-IP invariant, staleness) |
| `secrets` / `config` | secret loading (redacted) and the readable config surface |

The single entrypoint is `cmd/control-plane/main.go` — read its `buildAPI`/`buildSMTP`/
`buildQueue`/`buildExternal`/`buildFlywheel` functions to see exactly how the pieces wire
together and which switches gate each capability.

---

## 3. The five one-way doors (frozen — additive-only)

Some decisions become historical data or a contract other systems code against. Getting those
wrong later is catastrophic — a change silently corrupts every row already written, or breaks
every consumer. Those are the **five one-way doors**. They may be *extended* (add a new enum
value, a new response field) but a value is **never renamed, removed, repurposed, or
re-schemed**.

1. **`/verify` response shape** (`internal/verdict`) — versioned, additive-only. Consumers
   depend on it; a field never changes meaning.
2. **The LOG is append-only** (`internal/store`, migrations `0005_grants.sql`) — a verdict is
   never updated or deleted; corrections are new rows. Enforced physically by **revoked
   UPDATE/DELETE grants** on the runtime DB role, not just by convention.
3. **Email hashing** (`internal/hash`) — one frozen scheme (`norm/v1 + hmac-sha256/v1`). The
   raw email is **never persisted**; the normalized address is HMAC'd under a secret pepper. A
   scheme change de-joins every historical row. (One bounded, encrypted, transactionally-purged
   exception exists for in-flight queue items.)
4. **The `status`/`reason`/`recommendation` vocabulary** (`internal/vocab`) — additive-only.
   Downstream consumers branch on these strings and the calibration flywheel buckets on
   `reason`; a rename corrupts history. `vocab.go` has been byte-identical since Phase 0.
5. **Never fork the engine** — the `truemail` gem is a **pinned (3.3.1), unmodified**
   dependency; only the `truemail-rack` *server* is forked. A CI check asserts the gem is
   byte-identical. All added behavior uses the gem's public API, never a patch.

If a task seems to require touching a door, that's a signal to stop and reconsider the design —
the doors are the constraint the whole system is built to respect.

---

## 4. The nine irreversible surfaces (and how to prove they hold)

Beyond the doors, nine specific guards enforce correctness properties that are expensive or
impossible to undo if they regress. Each has a **fail-verify test**: a named test that goes red
the moment the guard is neutered. The way to trust these — and the way to check them after any
risky change — is to neuter the guard, watch the test fail, then restore and watch it pass.

| # | Surface | Guard (file) | The test that breaks when neutered |
|---|---|---|---|
| 1 | Door-3 hashing NFC/case-fold closure | `internal/hash/hash.go` | `TestLocalCaseFoldFrozenEdge`, `TestGoldenVectorFrozen` |
| 2 | Door-2 append-only LOG | `internal/store/mem.go` + `0005_grants.sql` | `TestMemAppendOnlyAccumulates` |
| 3 | GAP-24 four-way egress-IP invariant | `internal/audit/audit.go` | `TestEligibility_EgressMismatchFailsGate_GAP24` |
| 4 | P4 fleet cascade brake | `internal/health/decide.go` | `TestEvaluate_FleetWideSpike_DoesNotEmptyPool_GAP28` |
| 5 | GAP-37 CAS exactly-once settlement | `internal/store/mem.go` + `postgres.go` | `TestSettle_ExactlyOnceUnderSlowDrainerReclaim` |
| 6 | P6 anti-laundering (rented ≠ SMTP-grade valid) | `internal/external/external.go` | `TestResolve_VendorValid_IsNeverValid` |
| 7 | P6 spend-once | `internal/store/mem.go` + `postgres.go` | `TestSpendOnce_UnderSlowDrainerReclaim` |
| 8 | P7 classifier zero-information exclusion | `internal/flywheel/classifier.go` | `TestClassify_BlockedNeverInvalidNumerator` |
| 9 | P7 recalibration CAS | `internal/store/mem_flywheel.go` + `postgres_flywheel.go` | `TestRecalibrate_CASExactlyOnce` |

Three of these (settle-once → spend-once → recalibrate-once) are the **same CAS-on-lease-holder
discipline** applied at three layers: the transition (append the LOG row, record the vendor
charge, apply the calibration) commits **only if the worker still holds the lease** — a
reclaimed or slow attempt matches zero rows and discards, never double-writes. If you touch the
queue, the fallback spend path, or the calibration writer, this is the invariant to preserve.

**How to fail-verify (the discipline):** copy the guard file, neuter the guard in place, run the
named test and confirm it **fails**, then restore *from the copy* (never `git checkout` — that
can mask an uncommitted state), and confirm it passes again. A guard whose test still passes when
neutered is not actually load-bearing — that's a finding.

---

## 5. Running everything locally

**Prerequisites:** Go (1.26+), Docker (for a throwaway Postgres), and Ruby/Bundler are handled
inside the verifier-node's Docker image (the host needs no Ruby). **On a host running TLS-scanning AV
or a corporate proxy, there is one more prerequisite and it recurs on every image rebuild — see
"Prerequisite on a TLS-interception host" below before you build the verifier-node.**

### Control-plane: build, migrate, test

```bash
cd control-plane
go build ./...
go vet ./...

# A throwaway Postgres for the store + append-only integration tests:
docker run -d --name cp-pg -e POSTGRES_PASSWORD=dev -e POSTGRES_DB=cptest -p 5432:5432 postgres:16-alpine
# Apply every migration in order:
for f in $(ls migrations/[0-9][0-9][0-9][0-9]_*.sql | sort); do
  docker exec -i cp-pg psql -U postgres -d cptest -v ON_ERROR_STOP=1 -f - < "$f"
done

# Run the full suite against real Postgres (the PG-tagged tests RUN, not skip):
TM_TEST_DATABASE_URL='postgres://postgres:dev@localhost:5432/cptest' go test -shuffle=on ./...
```

Without `TM_TEST_DATABASE_URL` the Postgres integration tests **skip honestly** (they announce
why) rather than fake a pass. The in-memory store (`store.NewMem`) faithfully mirrors the
Postgres CAS/lease/append semantics, so most logic is testable with zero external dependencies.

> **Note on this Windows dev host:** `go test ./...` can wedge on the build-cache lock under
> concurrent invocations. The reliable path is to compile a package's test binary and run it
> directly: `go test -c -o /tmp/store.test.exe ./internal/store/ && /tmp/store.test.exe -test.v`.

> **Line endings are forced to LF by `.gitattributes`** (added 2026-11-07), because `git clone -c core.autocrlf=false`
> configures only the top-level repository: a `--recurse-submodules` clone of the parent repo on a host with
> `core.autocrlf=true` used to check every submodule out with CRLF, which breaks the shell scripts and `gofmt`. You
> no longer need to set anything. The vendored `verifier-node/truemail-rack/**` and four readiness docs are stored
> with CRLF and are deliberately exempt (`-text`), so their committed bytes never change.

### Prerequisite on a TLS-interception host — refresh the proxy CA before every rebuild

**Skip this on CI and on any host with no TLS-scanning AV/proxy.** It is a no-op there by design.

If your host runs an SSL/TLS-scanning AV or corporate proxy (Norton Web/Mail Shield on this dev
host), it re-signs HTTPS with a private root CA. The verifier-node image trusts whatever `*.crt`
files sit in `verifier-node/truemail-rack/certs/` (see the Dockerfile's OPTIONAL TLS-proxy trust
block — it adds trust for the proxy and never disables verification). Those files are **gitignored**,
because they are machine-specific — so a fresh clone has none, and the export below is the first
thing to run:

```bash
powershell -ExecutionPolicy Bypass -File scripts/export-norton-ca.ps1
# -> exported LF-only PEM to ...\verifier-node\truemail-rack\certs\norton-ssl-proxy-ca.crt
```

**Why this is written down rather than left to be rediscovered: it recurs on every rebuild.** The
proxy rotates its CA **key** while keeping an identical subject and issuer name, so a stale PEM
still *looks* right — same name, same file, wrong key. In-container `bundle install` then dies on
`Bundler::Fetcher::CertificateFailureError` against rubygems.org, and the failure surfaces as **the
RSpec gate exiting 17 with no test output at all** — no spec names, no failure list, nothing to grep.
Nothing in the tree points at the fix (the export script has no caller), which is exactly how the
same hour gets spent twice.

**On exit 17 with empty output, re-export the CA first. Do not go looking for a Ruby or spec defect.**
The tell, if you want to confirm before rebuilding, is a mismatch between the installed CA's Subject
Key Identifier and the presented leaf's Authority Key Identifier *despite matching names*:

```bash
openssl x509 -noout -text -in verifier-node/truemail-rack/certs/norton-ssl-proxy-ca.crt \
  | grep -A1 'Subject Key Identifier'
openssl s_client -showcerts -connect rubygems.org:443 </dev/null 2>/dev/null \
  | openssl x509 -noout -text | grep -A1 'Authority Key Identifier'
```

Two further notes that cost real time here:

- **Only the RSpec gate needs this.** The other gates pass fine without live rubygems.org egress.
  RSpec is the exception because the image is built with `bundle config set --local without
  'development test'`, so the test group is absent from the image and RSpec must re-resolve it at
  run time.
- **Never read a gate's exit code after a pipe** (`bash gate.sh | tail`) — you get the tail's status,
  and a dead run reports success. Capture `${PIPESTATUS[0]}`, or don't pipe.

### Verifier-node: build and run the box

```bash
cd verifier-node/truemail-rack
docker build -t verifier-node .
# The Door-5 gem-unmodified assertion:
bash bin/assert-gem-unmodified.sh
```

The node runs against **mock SMTP/DNS substrates** (`smtp_mock`/`dns_mock`) for local testing —
these mocks and the simulated harnesses are how the system is tested end-to-end without a real
port-25 IP; they are permanent test infrastructure, not scaffolding.

### The full gate (what CI runs)

`.github/workflows/ci.yml` is the authoritative gate: `go build` + `go vet` + `go test` against
a real Postgres, the gem-unmodified assertion, the fork's RSpec suite, the docs-freshness checks,
and the config-reference freshness check. (Running it on a TLS-interception host: the RSpec job is the
one that needs the proxy CA refreshed first — **exit 17 with no output is that, not a spec failure.**
See the prerequisite above.) It is designed to **never fake green** — properties
that can only be proven against real infrastructure (SMTP round-trips, vendor accuracy) are
tested at rollout, never asserted in CI.

---

## 6. Where everything lives

| You want… | Look in |
|---|---|
| The rules that govern all work | [`../PROJECT_RULES.md`](../PROJECT_RULES.md) |
| Every decision that was made (and by whom) | [`DECISIONS.md`](DECISIONS.md) |
| Architecture decision records | [`adr/`](adr/) (start at [`adr/README.md`](adr/README.md)) |
| The deep design of each capability | `phase-N-*-spec.md` (1=foundations … 7=flywheel) |
| Every config knob, its default and blast radius | [`config-manifest.md`](config-manifest.md) → [`config-reference.md`](config-reference.md) (generated) |
| How to deploy and operate | [`OPERATIONS.md`](OPERATIONS.md) |
| What verdicts mean (end-user language) | [`USER_GUIDE.md`](USER_GUIDE.md) |
| The safe degradation ladder | [`kill-switch-ladder.md`](kill-switch-ladder.md) |
| Which mechanisms self-modify, and their default mode | [`shadow-mode.md`](shadow-mode.md) |
| The ordered go-live path + owner setup items | [`go-live-runbook.md`](go-live-runbook.md) |

The phase specs are historical *and* current: each one froze contracts against the engine's
*actual* behavior, and the "status" docs record what shipped. When code and a phase spec
disagree, the code and `DECISIONS.md` win — but they rarely disagree, because the specs were
written to be built.

---

## 7. The review discipline

Changes are held to a **depth proportional to blast radius**:

- **Irreversible surfaces (the doors + the nine guards): full-depth review, always.** Any change
  near hashing, the append-only LOG, the vocabulary, the CAS/lease invariants, the anti-laundering
  rule, the cascade brakes, or the classifier exclusion gets a complete, adversarial review and a
  fail-verify pass. These are the parts where a subtle wrong is expensive forever.
- **Everything else: bounded review** proportional to what it touches.

**Use the synchronous code reviewer.** Review runs one reviewer at a time, in the foreground. The
multi-agent parallel-review workflow is **unreliable in this environment** — concurrent Go
compilation contends for the build-cache lock and wedges (it hung repeatedly during development).
The synchronous reviewer is the supported path; a single reviewer reading the diff, plus a
fail-verify pass on any touched guard, is the discipline.

The non-negotiables, restated for a new contributor:

- **Additive-only** at the doors — extend, never mutate.
- **No dial is hardcoded** — anything the config manifest calls a dial is a readable config
  entry, and every settable knob is either genuinely consumed or explicitly documented as
  reserved (no settable-but-ignored knobs).
- **Every removal states its reason;** when unsure whether something is dead, keep it and flag it.
- **Harden toward `unknown`** — when in doubt, the safe direction is always the humbler verdict,
  never a fabricated `valid`/`invalid`.
