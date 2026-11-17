# Truemail Verification System

Source visible for transparency only; all rights reserved (see LICENSE and NOTICE).

An email-verification system that returns an **honest** verdict for any address — `valid`,
`invalid`, `risky`, or `unknown` — with a confidence score and a recommended action. It verifies
through progressively more thorough layers (fast syntax/DNS checks, real SMTP probing, an optional
paid third-party fallback) and learns from real send outcomes to calibrate its own confidence over
time. Its founding principle:

> **It reports what it can prove, and says `unknown` when it can't — it never guesses a verdict.**

Everything else — the architecture, the frozen contracts, the safe-degradation ladder — follows
from that.

> **New here?** Read the doc for your role:
> **[developer](Docs/architecture.md)** (then [install & deploy](Docs/install-and-deploy.md)) ·
> **[operator](Docs/operator-runbook.md)** · **[caller / end user](Docs/api-contract.md)** (Appendix A explains a verdict).
> The full doc map is [`Docs/README.md`](Docs/README.md).

---

## What it does

- **Verifies an address through layered checks**, stopping at the cheapest layer that can answer:
  syntax and domain/MX validity, disposable/role/typo detection, real SMTP mailbox probing from a
  reputation-managed IP, and an optional paid vendor fallback for the residue nothing else can
  resolve.
- **Returns a stable, versioned verdict**: a frozen `status`/`reason`/`recommendation` vocabulary
  and a 0–100 confidence, safe to build automation on.
- **Keeps an append-only record** of every verdict — the log is the point.
- **Runs a resilient node pool**: sticky routing, per-provider rate limiting, health-watching with
  automatic quarantine and slow, canary-gated recovery, and a fleet-wide brake that refuses to turn
  an automated heal into a self-inflicted outage.
- **Processes bulk jobs asynchronously** through a durable, exactly-once queue with greylist-aware
  retry.
- **Closes the loop**: once connected to a real send-outcome feed, it joins outcomes back to the
  verdicts that authorized them, classifies what each proves, and calibrates confidence, promotes
  reliable providers, and weans off the paid fallback — all under human-gated controls by default,
  starting from a cautious baseline and sharpening only where real volume has been measured.
- **Degrades safely**: every kill-switch rung returns a real, correctly-labelled verdict from fewer
  layers. Removing capability lowers a verdict's *confidence*, never its *honesty*; nothing ever
  fabricates a `valid`/`invalid`.

## Architecture in brief

Two decoupled deployables — the boundary is a security and correctness boundary, not packaging:

```
coldmail-verify/
├── control-plane/   # Go — "the brain". Holds ALL state + makes ALL decisions. Port 25 CLOSED.
│                    #   front door, policy engine, cheap layers, node router, the queue,
│                    #   the append-only LOG, the calibration flywheel, secrets (incl. the pepper).
├── verifier-node/   # Ruby — "the box". A dumb, stateless SMTP probe. Port 25 OPEN outbound.
│   └── truemail-rack/   #   a FORK of truemail-rack embedding the PINNED, UNMODIFIED truemail gem.
│                        #   Holds NO state, NO DB creds, NO pepper — disposable (kill + re-spin).
├── Docs/            # architecture, install-and-deploy, operator-runbook, config, API contract, security,
│                    #   go-live checklist, future-work, open-decisions, 21 ADRs; history/ holds the phase specs.
└── .github/         # CI: 11 jobs — build + vet + tests (real Postgres), gem-unmodified, base-image / lockfile /
                     #   DNS / resolver / probe-guard gates, secret-leak scan, docs/config gates, code review.
```

The brain holds every secret and all accumulated data; the box does one thing — attempt an SMTP
conversation from a reputation-bearing IP and report the raw facts, rendering no verdict. Because
the box is stateless, adding a second one is a config line and a compromised box leaks nothing but
itself. Full detail: [`Docs/architecture.md`](Docs/architecture.md).

### The five frozen contracts ("one-way doors")

Decisions that become historical data or a consumer contract are frozen — extensible, never
mutated: **(1)** the `/verify` response shape, **(2)** the append-only LOG (enforced by revoked
DB grants), **(3)** the email-hashing scheme (raw email never persisted), **(4)** the
`status`/`reason`/`recommendation` vocabulary, and **(5)** the pinned, never-forked `truemail`
engine (only the rack *server* is forked; CI asserts the gem is byte-identical). See
[`PROJECT_RULES.md`](PROJECT_RULES.md).

## Quick start

Bring up the brain with a database — it verifies through the cheap layers immediately:

```bash
cd control-plane
go build ./... && go vet ./...

# A Postgres for state + the append-only integration tests:
docker run -d --name cp-pg -e POSTGRES_PASSWORD=dev -e POSTGRES_DB=cptest -p 5432:5432 postgres:16-alpine
# Apply every migration, once and in order, with the runner (records them in verify.schema_migrations; a second run is a no-op):
TM_MIGRATE_DATABASE_URL='postgres://postgres:dev@localhost:5432/cptest' go run ./cmd/migrate up

# Run the full suite against real Postgres:
TM_TEST_DATABASE_URL='postgres://postgres:dev@localhost:5432/cptest' go test -shuffle=on ./...

# Run the brain (a DB + a pepper serve /v1/verify; /readyz also needs the queue key, else 503 not_ready; listens on :8080):
TM_DATABASE_URL='postgres://postgres:dev@localhost:5432/cptest' TM_PEPPER='<32-bytes-of-entropy>' \
  TM_QUEUE_ADDR_KEY="$(openssl rand -hex 32)" \
  go run ./cmd/control-plane
```

The runner applies all 18 (`0001`–`0018`) exactly once each and records them; running it again changes nothing, and it refuses a changed file, a database ahead of the checkout or a hole in the history (see `control-plane/migrations/README.md` "The runner and the tracker"; a plain `psql` loop is valid only on an empty database). A production start (`TM_ENV=production`) also needs
`TM_SUPABASE_KEY` (and `TM_FRONTDOOR_TOKEN` to call the sync endpoint) — see [`Docs/install-and-deploy.md`](Docs/install-and-deploy.md) §3.1.

Build the box (no Ruby on the host — it's containerized), from the repo root:

```bash
cd verifier-node/truemail-rack && docker build -t verifier-node .
bash bin/assert-gem-unmodified.sh    # the Door-5 gem-unmodified assertion
```

## Deployment prerequisites

The brain runs cheap-verification-only with just a database and a pepper (it reports `/readyz` ready only once the queue key below is also set). Each further capability
is switched on when its prerequisite is in place — an operator action, config, or (for the outcome-feed
tier; the vendor tier's ZeroBounce adapter already ships, disabled) a pluggable adapter dropped into an existing interface seam; the brain's
decision code is never touched. Satisfy them in order (each is detailed, with verification
commands, in [install & deploy](Docs/install-and-deploy.md) §3, and tracked item by item — done, owner-only,
or not built yet — in the [go-live checklist](Docs/go-live-checklist.md)):

| Capability | Prerequisite | Switched on with |
|---|---|---|
| **Cheap verification** | a Postgres instance + an email-hash pepper | `TM_DATABASE_URL`, `TM_PEPPER` |
| **Bulk / async jobs** | an at-rest encryption key for the queue | `TM_QUEUE_ADDR_KEY` (bulk endpoints stay 503 without it — the queue never stores plaintext unencrypted — and `/readyz` reports `not_ready` with the reason, so a module whose job routes cannot work is not sent traffic) |
| **SMTP verification** | a port-25-capable host with a settable PTR on a clean IP (vet V1–V5) | the node descriptor + `TM_SMTP_MODE=enabled` |
| **Paid fallback** | a ZeroBounce account + a signed DPA (the ZeroBounce adapter at the `external.Vendor` seam ships **disabled**) | `TM_EXTERNAL_DPA_CONFIRMED=true` + `TM_FALLBACK_MODE=enabled` + the vendor key in the environment variable named by `TM_EXTERNAL_ZEROBOUNCE_KEY_ENV` (default `ZEROBOUNCE_API_KEY`) + a daily spend cap above 0 (`TM_EXTERNAL_DAILY_SPEND_CAP`) |
| **Confidence calibration** | an authenticated (mTLS) real send-outcome feed | an mTLS client at the `OutcomeSource` seam + the trust anchor/credential; promote the mutators when ready |

These are setup steps, like provisioning a database — not caveats. Until a tier is configured, the
system simply returns its honest verdict from the layers it *can* run.

**Why `/readyz` is all-or-nothing on `TM_QUEUE_ADDR_KEY`.** `POST /v1/verify` serves with just a database and a pepper, yet `/readyz` answers `503 not_ready` until the queue key is set too, and there is no setting that says "no bulk wanted". That is deliberate: the platform always submits bulk jobs, so a deployment without the queue key can never serve it; a verify-only mode is not offered. Readiness is what an orchestrator reads to decide whether to send traffic, so a module whose job routes can only answer 503 must not report `ready` (the body's `reasons` names the missing key; [`Docs/operator-runbook.md`](Docs/operator-runbook.md) §6.1). Behaviour is unchanged by this note.

## Where to go next

| You want to… | Read |
|---|---|
| Understand how it is built | [`Docs/architecture.md`](Docs/architecture.md) |
| Install, run, test and deploy it | [`Docs/install-and-deploy.md`](Docs/install-and-deploy.md) |
| Operate it (kill-switches, alerts, rollback) | [`Docs/operator-runbook.md`](Docs/operator-runbook.md) |
| Call the API / understand what a verdict means | [`Docs/api-contract.md`](Docs/api-contract.md) (Appendix A) |
| See what protects what | [`Docs/security-posture.md`](Docs/security-posture.md) |
| Know the rules that never change | [`PROJECT_RULES.md`](PROJECT_RULES.md) |
| See every toggle/flag/secret/dial | [`Docs/config-reference.md`](Docs/config-reference.md) (generated) |
| See the binding decisions and ADRs | [`Docs/open-decisions.md`](Docs/open-decisions.md) · [`Docs/adr/`](Docs/adr/) |
| Follow the ordered go-live path | [`Docs/go-live-checklist.md`](Docs/go-live-checklist.md) |
| See what is not done yet | [`Docs/future-work.md`](Docs/future-work.md) |
| See the full documentation map | [`Docs/README.md`](Docs/README.md) |

## License

- **This project's own code and documentation** (`control-plane/`, the `verifier-node/` wrapper
  additions, `Docs/`) are **all rights reserved**; the source is visible for transparency only — see [`LICENSE`](LICENSE).
- **Third-party components** — the `truemail` engine and the forked `truemail-rack` server, both
  MIT © Vladislav Trotsenko — are respected and attributed in [`NOTICE`](NOTICE); the top-level
  license grants no rights over them and does not alter their MIT terms.
