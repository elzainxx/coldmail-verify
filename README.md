# Truemail Verification System

An email-verification system that returns an **honest** verdict for any address — `valid`,
`invalid`, `risky`, or `unknown` — with a confidence score and a recommended action. It verifies
through progressively more thorough layers (fast syntax/DNS checks, real SMTP probing, an optional
paid third-party fallback) and learns from real send outcomes to calibrate its own confidence over
time. Its founding principle:

> **It reports what it can prove, and says `unknown` when it can't — it never guesses a verdict.**

Everything else — the architecture, the frozen contracts, the safe-degradation ladder — follows
from that.

> **New here?** Read the guide for your role:
> **[developer](Docs/ONBOARDING.md)** · **[operator](Docs/OPERATIONS.md)** ·
> **[end user](Docs/USER_GUIDE.md)**. The full doc map is [`Docs/README.md`](Docs/README.md).

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
Codmailling/
├── control-plane/   # Go — "the brain". Holds ALL state + makes ALL decisions. Port 25 CLOSED.
│                    #   front door, policy engine, cheap layers, node router, the queue,
│                    #   the append-only LOG, the calibration flywheel, secrets (incl. the pepper).
├── verifier-node/   # Ruby — "the box". A dumb, stateless SMTP probe. Port 25 OPEN outbound.
│   └── truemail-rack/   #   a FORK of truemail-rack embedding the PINNED, UNMODIFIED truemail gem.
│                        #   Holds NO state, NO DB creds, NO pepper — disposable (kill + re-spin).
├── Docs/            # The three audience guides, 7 phase specs, ADRs, DECISIONS, runbooks, config.
└── .github/         # CI: build + vet + tests (real Postgres) + gem-unmodified + docs/config gates.
```

The brain holds every secret and all accumulated data; the box does one thing — attempt an SMTP
conversation from a reputation-bearing IP and report the raw facts, rendering no verdict. Because
the box is stateless, adding a second one is a config line and a compromised box leaks nothing but
itself. Full detail: [`Docs/ONBOARDING.md`](Docs/ONBOARDING.md).

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
for f in $(ls migrations/[0-9][0-9][0-9][0-9]_*.sql | sort); do
  docker exec -i cp-pg psql -U postgres -d cptest -v ON_ERROR_STOP=1 -f - < "$f"
done

# Run the full suite against real Postgres:
TM_TEST_DATABASE_URL='postgres://postgres:dev@localhost:5432/cptest' go test -shuffle=on ./...

# Run the brain (cheap-verification-ready with a DB + a pepper):
TM_DATABASE_URL='postgres://postgres:dev@localhost:5432/cptest' TM_PEPPER='<32-bytes-of-entropy>' \
  go run ./cmd/control-plane
```

Build the box (no Ruby on the host — it's containerized), from the repo root:

```bash
cd verifier-node/truemail-rack && docker build -t verifier-node .
bash bin/assert-gem-unmodified.sh    # the Door-5 gem-unmodified assertion
```

## Deployment prerequisites

The brain runs cheap-verification-only with just a database and a pepper. Each further capability
is switched on when its prerequisite is in place — an operator action, config, or (for the vendor
and outcome-feed tiers) a pluggable adapter dropped into an existing interface seam; the brain's
decision code is never touched. Satisfy them in order (each is detailed, with verification
commands, in the [operations guide](Docs/OPERATIONS.md) and the
[go-live runbook](Docs/go-live-runbook.md)):

| Capability | Prerequisite | Switched on with |
|---|---|---|
| **Cheap verification** | a Postgres instance + an email-hash pepper | `TM_DATABASE_URL`, `TM_PEPPER` |
| **Bulk / async jobs** | an at-rest encryption key for the queue | `TM_QUEUE_ADDR_KEY` (bulk endpoints stay 503 without it — the queue never stores plaintext unencrypted) |
| **SMTP verification** | a port-25-capable host with a settable PTR on a clean IP (vet V1–V5) | the node descriptor + `TM_SMTP_MODE=enabled` |
| **Paid fallback** | a verification vendor + a signed DPA | a vendor adapter at the `external.Vendor` seam + `TM_EXTERNAL_DPA_CONFIRMED=true` + `TM_FALLBACK_MODE=enabled` + a vendor key |
| **Confidence calibration** | an authenticated (mTLS) real send-outcome feed | an mTLS client at the `OutcomeSource` seam + the trust anchor/credential; promote the mutators when ready |

These are setup steps, like provisioning a database — not caveats. Until a tier is configured, the
system simply returns its honest verdict from the layers it *can* run.

## Where to go next

| You want to… | Read |
|---|---|
| Become productive as a developer | [`Docs/ONBOARDING.md`](Docs/ONBOARDING.md) |
| Deploy and operate the system | [`Docs/OPERATIONS.md`](Docs/OPERATIONS.md) |
| Understand what a verdict means | [`Docs/USER_GUIDE.md`](Docs/USER_GUIDE.md) |
| Know the rules that never change | [`PROJECT_RULES.md`](PROJECT_RULES.md) |
| See every toggle/flag/secret/dial | [`Docs/config-reference.md`](Docs/config-reference.md) (generated) |
| See the binding decisions and ADRs | [`Docs/DECISIONS.md`](Docs/DECISIONS.md) · [`Docs/adr/`](Docs/adr/) |
| Follow the ordered go-live path | [`Docs/go-live-runbook.md`](Docs/go-live-runbook.md) |
| See the full documentation map | [`Docs/README.md`](Docs/README.md) |

## License

- **This project's own code and documentation** (`control-plane/`, the `verifier-node/` wrapper
  additions, `Docs/`) are **proprietary / all rights reserved** — see [`LICENSE`](LICENSE).
- **Third-party components** — the `truemail` engine and the forked `truemail-rack` server, both
  MIT © Vladislav Trotsenko — are respected and attributed in [`NOTICE`](NOTICE); the top-level
  license grants no rights over them and does not alter their MIT terms.
