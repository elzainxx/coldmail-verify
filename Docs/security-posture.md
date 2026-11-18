# Security posture — what protects what, and what is honestly not protected yet

**Status:** LIVE. Every control below was checked against the code, a migration, an ADR or a gate on
2026-11-13; the evidence is named so it can be re-checked. Where a control is *not built*, this file
says so (§9) rather than implying it. The threat model for the one cross-project trust boundary (the
outcome feed) is [ADR-0013](adr/ADR-0013-outcome-feed-auth-mechanism-and-tb7-threat-model.md); the
rules that bind all work are [`../PROJECT_RULES.md`](../PROJECT_RULES.md).

## 1. What is being protected, and from whom

| Asset | Why it matters | Main threats |
|---|---|---|
| **The email-hash pepper** | Lose it and all history de-joins forever; leak it and low-entropy addresses become brute-forceable. The single most fragile secret. | host compromise of the control-plane; a careless backup |
| **Addresses under verification** | Personal data. Door 3: the raw address is never persisted; hashes only. | a leaking log line, backup, or raw blob; a vendor sub-processor |
| **The LOG** (`verifications`, `verification_raw`, `send_outcomes`, …) | Calibration ground truth; the audit trail of every verdict. | silent rewrite of history |
| **Verdict integrity** | A wrong `valid`/`invalid` is a business harm to the caller. | forged outcome feed; vendor over-claiming; our own DNS failure read as a fact |
| **The verifier IP's reputation** | One scarce, port-25 IP; once poisoned it is expensive to replace. | abuse of the probe; an open resolver path; a steered destination |
| **The network the node sits on** | The node makes outbound connections to *user-supplied* names. | SSRF to loopback / RFC 1918 / cloud metadata |

Trust boundaries: caller → control-plane (HTTP, bearer); control-plane → node (HTTP, per-node token,
private network); node → the internet (SMTP/DNS only); sender → control-plane (the outcome feed, mTLS
pull, TB-7); control-plane → vendor (HTTPS, off by default); control-plane → Postgres.

## 2. Secrets and their custody

Loaded from the environment by `internal/secrets` (never committed — `.gitignore` excludes `*.env`
except `*.example.env`; see `control-plane/config/secrets.example.env`), held in memory, and redacted
by `Secrets.String()`. **No secret may reach a log line** ([ADR-0016](adr/ADR-0016-no-credential-in-a-log-line.md)):
`scripts/check-no-secret-leak.sh` boots the real binary with a canary in every secret variable and scans its log.

| Secret | Env var | If lost | If leaked |
|---|---|---|---|
| Email-hash pepper | `TM_PEPPER` | all history de-joins; irrecoverable | addresses brute-forceable |
| Queue at-rest key (AES-256-GCM, ≠ pepper) | `TM_QUEUE_ADDR_KEY` (+ `TM_QUEUE_ADDR_KEY_PREV` during rotation) | in-flight, not-yet-settled work becomes undecryptable; re-enqueue | the queue's transient plaintext is readable |
| Front-door legacy bearer | `TM_FRONTDOOR_TOKEN` | sync callers on the legacy path 401 | anyone can call sync `/v1/verify` |
| Per-caller API keys | stored **hashed** in `api_callers` (`key_sha256`) | issue a new key | rotate: windows overlap, then expire |
| Node tokens (per node) | `TM_NODE_TOKENS` (`nodeid=token,…`); the node's `ACCESS_TOKENS` | the brain cannot call that node | that node's wrapper is callable |
| Vendor key (ZeroBounce) | the environment variable named by `TM_EXTERNAL_ZEROBOUNCE_KEY_ENV` (default `ZEROBOUNCE_API_KEY`); `TM_VENDOR_KEYS` is no longer read for the vendor | the fallback stays off | spend and egress at the vendor |
| Database DSN | `TM_DATABASE_URL` | the process is not ready | full data access |
| Migrating identity's DSN | `TM_MIGRATE_DATABASE_URL` (migration runner only; the control plane never reads it) | migrations cannot be applied; the running service is unaffected | a superuser-grade credential (DDL, ownership, the whole database): custody like the break-glass role, never the runtime `verify_app`. The runner never prints it (driver errors that embed it are suppressed; `TestRun_NeverPrintsTheCredential`) |
| Presence-checked secret | `TM_SUPABASE_KEY` | the process refuses to start when `TM_ENV=production` | nothing — it has no consumer (a known oddity, [`future-work.md`](future-work.md) FW-5) |
| Future: mTLS trust anchor + client credential | not yet wired | feed stays fail-closed | the TB-7 threat model applies |

The pepper has its own custody rule (generated once with strong entropy, stored outside the DB,
redundant geo-separate access-audited backups, rotation additive-only); the control-plane holds it in
memory, so host hardening and core-dump suppression are part of custody. The queue key is separate
*on purpose*: losing it is recoverable, losing the pepper is not.

## 3. Data protection

- **Door 3 — hashing.** One frozen scheme (`norm/v1` + `hmac-sha256/v1`, `bytea` key, single global
  pepper) in `internal/hash`. Non-ASCII local parts are *refused* rather than mis-hashed until NFC/IDNA
  is wired ([ADR-0006](adr/ADR-0006-door3-nonascii-localpart-refusal.md)). Fail-verify tests:
  `TestGoldenVectorFrozen`, `TestLocalCaseFoldFrozenEdge`.
- **No plaintext in the raw sidecar.** `verification_raw` redacts the address from every engine blob,
  case-insensitively, including 5xx bodies and nested fields ([ADR-0008](adr/ADR-0008-door3-raw-blob-address-redaction.md)).
- **The one bounded plaintext exception — the queue.** `work_items.addr_ciphertext` is AES-256-GCM under a
  key distinct from the pepper, and purged in the same transaction that settles the item
  ([ADR-0011](adr/ADR-0011-phase-5-queue-additive-surface.md), [ADR-0002](adr/ADR-0002-queue-plaintext-backup-scope.md)).
  `TestAddrCipher_CiphertextContainsNoPlaintext` fails if a ciphertext column ever contains the address.
  A backup therefore holds ciphertext only; the restore-into-isolation drill that proves it is
  real-world-gated ([`future-work.md`](future-work.md) B8).
- **The LOG is append-only by grant.** `verify_app` has INSERT+SELECT only on `verifications`,
  `verification_raw`, `node_audits`, `work_item_attempts`, `external_calls` and `send_outcomes`
  (migrations 0005, 0007, 0009, 0011, 0012); the Go `store.Store` interface offers no update or delete of
  a verdict. **Residual, stated plainly:** this is immutability by *grant*, not by write-once media. Whoever
  holds `verify_admin` (the break-glass table-privilege role, no login) or `verify_owner` (owns the objects: DDL and re-granting) can rewrite history. An
  external tamper-evidence checkpoint is an open owner decision ([`future-work.md`](future-work.md) FW-9);
  the audit of who used `verify_admin` depends on the unbuilt admin-audit log (FW-1).
- **Third-party egress.** The vendor layer is *off*. It needs three independent affirmative switches
  (`TM_EXTERNAL_DPA_CONFIRMED=true`, `TM_FALLBACK_MODE=enabled`, the vendor key present in the environment
  variable named by `TM_EXTERNAL_ZEROBOUNCE_KEY_ENV`); any one missing ⇒ no address leaves, and even with all three on
  nothing is sent until the hard daily spend cap `TM_EXTERNAL_DAILY_SPEND_CAP` is set above its default `0`. A
  ZeroBounce adapter now ships behind this guard, disabled ([`vendor-zerobounce.md`](vendor-zerobounce.md)). `TM_FALLBACK_MODE=off` is the global panic button. `external_calls`
  stores only `email_hash` and is queryable by hash for subject-access and deletion requests
  ([ADR-0012](adr/ADR-0012-phase-6-external-fallback-additive-surface.md)).

## 4. Authentication and authorization

- **Callers** (`internal/api.authenticate`): a presented bearer is looked up as `sha256(key)` in
  `api_callers` (constant-time compare, validity window `[not_before, not_after)` so rotation has no
  downtime). The legacy static `TM_FRONTDOOR_TOKEN` is accepted **only** on sync `POST /v1/verify`; the
  bulk surface (`/v1/verify/bulk`, jobs) **requires** a per-caller key. With no auth configured the
  endpoint is open **only when `TM_ENV=development`**, never in production. A scheme-less credential is
  treated as no credential. Missing or wrong credentials are `401` on every authenticated route.
- **Job ownership comes only from the credential.** `owner_user_id` is read from `api_callers.user_id`
  for the presented key; no body, query string or other header can influence it. A job created on the
  legacy path has no owner and is readable by nobody over the API — the fail-safe direction.
- **Control-plane → node:** a per-node token, sent as the raw `Authorization` value, resolved from
  `TM_NODE_TOKENS` by node id and never stored in clear in the `nodes` table. The wrapper is bound to
  the private network; inbound 25 is closed.
- **The outcome feed** is an *authenticated* channel with the sender as an identified principal:
  mutually-authenticated pull (mTLS), built behind the `OutcomeSource` interface. `PullSource` is
  **fail-closed** — with no authenticated client it pulls nothing, never unauthenticated data. The
  recommend-only default of the calibration mutators is a mitigation, not a substitute
  ([ADR-0001](adr/ADR-0001-outcome-feed-authentication.md), ADR-0013).
- **Operator actions** (pull a kill-switch rung, promote a mutator) are *not yet*
  per-actor audited: that is DR-3 ([`future-work.md`](future-work.md) FW-1). Until it is built they are
  config/env changes and `verify_admin` writes, attributable only by the host's own access logs.

## 5. The verifier node

The node probes whatever mail host a user-supplied domain publishes, so its controls are layered:

1. **The host audit** (`GET /v1/audit`; the brain's eligibility gate). A node is `:smtp`-eligible only
   when its status is `active`, the audit is green (`warnings` empty), `port25_open`, and the **four-way
   egress-IP invariant** holds: the egress IP == the PTR'd IP == the audited `current_host_ip` ==
   `TM_NODE_PUBLIC_EGRESS_IP`. A green audit on the wrong IP does not promote
   ([ADR-0009](adr/ADR-0009-gap24-egress-ip-invariant-runtime-enforcement.md),
   `TestEligibility_EgressMismatchFailsGate_GAP24`).
2. **Resolver hygiene is a control.** A `search` or `domain` line in `resolv.conf`, `options ndots:N` with
   N > 1, or a hostname containing a dot adds a `resolver` warning, so the audit is red, the node is
   quarantined and does no SMTP ([ADR-0019](adr/ADR-0019-dns-names-are-rooted-and-failure-is-unknown.md)
   addendum; gate `check-resolver-hygiene.sh`, Ruby's own resolver as the oracle, one mutant per rule).
   **Operationally: give the box a short hostname** and a nameservers-only `resolv.conf`.
3. **The probe destination guard** ([ADR-0021](adr/ADR-0021-smtp-probe-destination-guard.md)). Before the
   gem's SMTP step the wrapper resolves the target and refuses if **any** address is non-public
   (loopback, RFC 1918, link-local/cloud-metadata, CGNAT, TEST-NETs, multicast, reserved; IPv6 is an
   allow-list, `2000::/3` only). A post-check discards a transcript whose connection address was not public. The
   brain turns a refusal into `risky` / `non_public_mx` (migration 0015) — never `invalid`, never cached.
   `PROBE_ALLOW_NON_PUBLIC=1` exists for the proof harness only and turns the audit red. Gate:
   `check-probe-guard.sh` (about 51 000 generated addresses judged by an independent oracle; four mutants).
4. **The egress firewall** (`verifier-node/egress/verifier_egress.nft`) — the layer a DNS-rebinding race
   cannot beat, because the gem re-resolves inside its own SMTP step. It rejects `tcp/25` to every
   non-public destination. **It must be installed on the real box before the first real probe**, mirrored in
   the provider's egress security group, and the instance metadata service restricted separately
   (IMDSv2, hop limit 1 — metadata is reachable on ports other than 25). This is a real-world step
   ([`go-live-checklist.md`](go-live-checklist.md) G1.5).
5. **The engine is never modified** (Door 5): `bin/assert-gem-unmodified.sh` fails CI if `truemail` differs
   from 3.3.1; every wrapper behaviour uses the gem's public API.
6. **The node is disposable.** No database credential, no pepper, no state; if it is compromised, kill and re-spin.

## 6. Failing safe

- **`unknown`, never `invalid`, for our own failures.** A DNS failure of ours is `unknown`
  ([ADR-0019](adr/ADR-0019-dns-names-are-rooted-and-failure-is-unknown.md)); a probe that cannot run leaves
  `risky`; nothing on the degradation ladder fabricates a verdict.
- **Anti-laundering.** A vendor's `valid` is recorded as `risky` / `external_verified` at a capped
  confidence, never an SMTP-grade `valid` (`TestResolve_VendorValid_IsNeverValid`).
- **Brakes that refuse to cascade.** If a majority of nodes trip at once the fleet brake alerts and holds
  instead of emptying the pool (`TestEvaluate_FleetWideSpike_DoesNotEmptyPool_GAP28`); the recalibration
  cascade brake holds for a human; the last healthy node is never auto-quarantined.
- **Misconfiguration is a startup error.** An out-of-range brake dial that would silently disable a safety
  is rejected at boot, not ignored.

## 7. Supply chain and build

- The verifier-node base image is `ruby:3.4.12-slim-bookworm` **pinned by digest**, CI builds with
  `--pull --no-cache`, and the CI Postgres service image is digest-pinned too
  (`check-base-image-pinned.sh` fails any workflow image without a digest).
- Gems come from a committed `Gemfile.lock` installed frozen; the lock's `truemail` and `net-smtp` must equal
  the Gemfile's exact pins (`check-gemfile-lock.sh`).
- `govulncheck ./...` was clean on 2026-11-07 (go1.26.10, `x/net` v0.62.0, `x/text` v0.43.0). The advisory
  database moves, so it is a **re-run immediately before the pilot**, not a one-time tick (go-live G0.4).
- No code path disables TLS verification (`InsecureSkipVerify` appears nowhere in the Go or Ruby code).
  On a host behind a TLS-intercepting proxy the image trusts the proxy's CA *in addition to* the system
  store, never instead of it ([`install-and-deploy.md`](install-and-deploy.md) §2).

## 8. How each control is verified

| Control | Gate / test |
|---|---|
| No credential in a log line | `check-no-secret-leak.sh` (CI job `no-secret-leak`) |
| Gem unmodified, exact pin | `bin/assert-gem-unmodified.sh` (CI job `door5-gem-unmodified`) |
| Base image digest-pinned, no cached build | `check-base-image-pinned.sh` + selftest |
| Gems frozen from the lock | `check-gemfile-lock.sh` + selftest |
| DNS failure of ours is `unknown`; A-only is not `invalid` | `check-dns-search-domain.sh`, `check-implicit-mx.sh` + selftests |
| Dirty resolver turns the audit red | `check-resolver-hygiene.sh` + selftest |
| Probe never reaches a non-public address | `check-probe-guard.sh` + selftest |
| Append-only grants actually enforced | Postgres-backed grant tests, counted by `check-test-execution.sh` (a skip is a shortfall) |
| The API matches its documented routes | `TestRoutes_*` against [`api-contract.md`](api-contract.md) |
| Every gate is wired and named | `check-test-enumeration.sh` + selftest ([`test-strategy.md`](test-strategy.md)) |

## 9. Not built, or not yet protected — the honest list

| Gap | Effect today | Tracked |
|---|---|---|
| **No admin-audit log** (DR-3) | Manual control-plane actions have no per-actor record | [`future-work.md`](future-work.md) FW-1; go-live G0.2 |
| **`port25_open` follows the audit, not a separate egress proof** | Since `0019` a green audit (public-IP lookup, A, PTR, resolver) sets `port25_open`; the audit does not probe port 25, so a host that blocks it is caught by the operator's V1 check, the warm-up cap and the block-rate watcher, not by the gate | FW-2 (a dedicated egress-test writer) |
| **A `verify_admin` holder can rewrite the LOG** | Immutability is by grant, not write-once media | FW-9 (owner decision) |
| **Brakes have no timeout and no authenticated confirm** | They stay held until a human acts | FW-8 (owner decision) |
| **The egress firewall and short hostname are not installed anywhere yet** | They are shipped and gated, but the real box does not exist | go-live G1.4, G1.5 |
| **TLS termination is undecided** | Whether the app or a proxy terminates TLS is a deployment choice; neither has been exercised on a live host | go-live G1.11 |
| **Pepper backup/recovery drill** | The custody rule is written; the drill needs the real environment | go-live G1.14 |
| **Host hardening of the control-plane** (brain-compromise threat model, PN-12) | Deployment concern, no host exists yet | go-live G1.15 |
