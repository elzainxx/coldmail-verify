# Install and deploy — from a clean checkout to a running system

**Status:** LIVE. Commands, env var names, ports and counts were checked against the code on
2026-11-09 (15 migrations, control-plane default listen `:8080`, node `:3000`). Read
[`architecture.md`](architecture.md) first for what you are installing; operate it with
[`operator-runbook.md`](operator-runbook.md); follow the ordered, status-marked path to live in
[`go-live-checklist.md`](go-live-checklist.md). The deep per-phase design is in [`history/`](history/).

The system is two programs: the **control-plane** (Go — needs Postgres and a few secrets) and the
**verifier-node** (Ruby, containerised — needs a port-25-capable host). The control-plane runs
cheap-verification-only with just a database and a pepper; SMTP probing, the paid fallback and the
calibration feed are each switched on when their prerequisite is in place. **Turning a capability on
is always config or an operator action — never a code change or a redeploy.**

---

## 1. Prerequisites

- **Go 1.26+** (`control-plane/go.mod` says `go 1.26.0`; CI uses `setup-go '1.26'`).
- **Docker** for a throwaway Postgres 16 and for the verifier-node image. The host needs **no Ruby**;
  Ruby 3.4.12 and Bundler live inside the node image.
- **bash** for `scripts/` and `bin/`. On a TLS-scanning AV or corporate proxy host there is one more
  prerequisite that recurs on every node-image rebuild — see §2, "Prerequisite on a TLS-interception host".

## 2. Run everything locally

### Control-plane: build, migrate, test

```bash
cd control-plane
go build ./...
go vet ./...

# A throwaway Postgres for the store + append-only integration tests:
docker run -d --name cp-pg -e POSTGRES_PASSWORD=dev -e POSTGRES_DB=cptest -p 5432:5432 postgres:16-alpine
# Apply every migration in order (0001 … 0015):
for f in $(ls migrations/[0-9][0-9][0-9][0-9]_*.sql | sort); do
  docker exec -i cp-pg psql -U postgres -d cptest -v ON_ERROR_STOP=1 -f - < "$f"
done

# Run the full suite against real Postgres (the PG-tagged tests RUN, not skip):
TM_TEST_DATABASE_URL='postgres://postgres:dev@localhost:5432/cptest' go test -shuffle=on ./...

# Run the control-plane (cheap-verification-ready with a DB + a pepper):
TM_DATABASE_URL='postgres://postgres:dev@localhost:5432/cptest' TM_PEPPER='<32-bytes-of-entropy>' \
  go run ./cmd/control-plane          # listens on :8080 (TM_LISTEN_ADDR to change it)
```

Without `TM_TEST_DATABASE_URL` the Postgres integration tests **skip honestly** (they announce why)
rather than fake a pass — and CI's `check-test-execution.sh` treats an unexpected skip as a shortfall.
The in-memory store (`store.NewMem`) mirrors the Postgres CAS/lease/append semantics, so most logic is
testable with no external dependency.

> **Note on a Windows dev host:** `go test ./...` can wedge on the build-cache lock under concurrent
> invocations. The reliable path is to compile a package's test binary and run it directly:
> `go test -c -o /tmp/store.test.exe ./internal/store/ && /tmp/store.test.exe -test.v`.

> **Line endings are forced to LF by `.gitattributes`**, because `git clone -c core.autocrlf=false`
> configures only the top-level repository: a `--recurse-submodules` clone on a host with
> `core.autocrlf=true` used to check every submodule out with CRLF, which breaks the shell scripts and
> `gofmt`. You no longer need to set anything. The vendored `verifier-node/truemail-rack/**` and four
> readiness docs under `Docs/history/` are stored with CRLF and are deliberately exempt (`-text`), so
> their committed bytes never change.

### Prerequisite on a TLS-interception host — refresh the proxy CA before every rebuild

**Skip this on CI and on any host with no TLS-scanning AV/proxy.** It is a no-op there by design.

If your host runs an SSL/TLS-scanning AV or corporate proxy (Norton Web/Mail Shield on this dev host),
it re-signs HTTPS with a private root CA. The verifier-node image trusts whatever `*.crt` files sit in
`verifier-node/truemail-rack/certs/` (see the Dockerfile's OPTIONAL TLS-proxy trust block — it adds
trust for the proxy and never disables verification). Those files are **gitignored**, because they are
machine-specific — so a fresh clone has none, and the export below is the first thing to run:

```bash
powershell -ExecutionPolicy Bypass -File scripts/export-norton-ca.ps1
# -> exported LF-only PEM to ...\verifier-node\truemail-rack\certs\norton-ssl-proxy-ca.crt
```

**It recurs on every rebuild.** The proxy rotates its CA **key** while keeping an identical subject and
issuer name, so a stale PEM still *looks* right — same name, same file, wrong key. In-container
`bundle install` then dies on `Bundler::Fetcher::CertificateFailureError` against rubygems.org, and the
failure surfaces as **the RSpec gate exiting 17 with no test output at all** — no spec names, no failure
list, nothing to grep.

**On exit 17 with empty output, re-export the CA first. Do not go looking for a Ruby or spec defect.**
To confirm before rebuilding, look for a mismatch between the installed CA's Subject Key Identifier and
the presented leaf's Authority Key Identifier *despite matching names*:

```bash
openssl x509 -noout -text -in verifier-node/truemail-rack/certs/norton-ssl-proxy-ca.crt \
  | grep -A1 'Subject Key Identifier'
openssl s_client -showcerts -connect rubygems.org:443 </dev/null 2>/dev/null \
  | openssl x509 -noout -text | grep -A1 'Authority Key Identifier'
```

- **Only the RSpec gate needs this.** The other gates pass without live rubygems.org egress. RSpec is the
  exception because the image is built with `bundle config set --local without 'development test'`, so
  the test group is absent from the image and RSpec must re-resolve it at run time.
- **Never read a gate's exit code after a pipe** (`bash gate.sh | tail`) — you get the tail's status, and
  a dead run reports success. Capture `${PIPESTATUS[0]}`, or don't pipe.

### Verifier-node: build and run the box

```bash
cd verifier-node/truemail-rack
docker build -t verifier-node .
bash bin/assert-gem-unmodified.sh        # the Door-5 gem-unmodified assertion
```

`verifier-node/docker-compose.yml` runs the same image on `:3000` with placeholder `VERIFIER_EMAIL` /
`ACCESS_TOKENS` — a smoke harness, not a deployment. The wrapper's `/v1/*` endpoints are in that image.
The node's local tests run against **mock SMTP/DNS substrates** (`smtp_mock` / `dns_mock`,
`test/p2/`, built as `tmvn-p2-test:local`); these mocks and the simulated harnesses are how the system is
tested end to end without a real port-25 IP — permanent test infrastructure, not scaffolding.

### The full gate (what CI runs)

`.github/workflows/ci.yml` is authoritative: 11 jobs — the Go build/vet/test against real Postgres, the
gem-unmodified assertion, the base-image and `Gemfile.lock` gates, the real-DNS fixtures, the node's
resolver-hygiene and probe-guard gates, the node RSpec suite, the boot-log secret scan, the
docs/config-freshness gates and the code-reviewer gate. Each gate script is `scripts/check-*.sh` with a
paired `-selftest`; [`test-strategy.md`](test-strategy.md) names all of them. CI is designed to **never
fake green** — what can only be proven against real infrastructure (SMTP round-trips, vendor accuracy)
is tested at rollout and listed in [`future-work.md`](future-work.md).

---

## 3. Deploy: the capability tiers

Each tier has a prerequisite. These are ordinary setup steps, like provisioning a database: satisfy
the prerequisite, set the switches, verify. They are gated in order — a later tier is not useful
before an earlier one is live.

### 3.1 Baseline — cheap verification (a database and the secrets)

**Provide** a Postgres 16 instance and the core secrets (loaded from the environment, never committed —
see [`security-posture.md`](security-posture.md) §2):

| Env var | Required | Notes |
|---|---|---|
| `TM_DATABASE_URL` | yes | empty ⇒ `/v1/verify` answers 503 (not ready); health is still served |
| `TM_PEPPER` | yes | the email-hash pepper — the most fragile secret: generate once with strong entropy (`openssl rand -hex 32`), back it up geo-separately, **never rotate casually** (losing it de-joins all history) |
| `TM_FRONTDOOR_TOKEN` | for the sync endpoint | legacy bearer for `POST /v1/verify`; the bulk endpoints need a per-caller key (rows in `api_callers`) |
| `TM_ENV` | recommended | `production` makes the core secrets mandatory (the process refuses to start without them); `development` is the only mode where an unconfigured front door is open |
| `TM_SUPABASE_KEY` | **when `TM_ENV=production`** | presence-checked at boot and shown by `/readyz` as `supabaseKey`; **nothing reads its value** — Postgres access uses `TM_DATABASE_URL`. Set any non-empty value on plain Postgres (retiring it is [`future-work.md`](future-work.md) FW-5) |
| `TM_QUEUE_ADDR_KEY` | for bulk | 32 bytes, hex or base64 (`openssl rand -hex 32`); **a different secret from the pepper**. Without it the queue refuses to start and the bulk endpoints stay 503. `TM_QUEUE_ADDR_KEY_PREV` holds the previous key during a rotation overlap |
| `TM_LISTEN_ADDR` | no | default `:8080` |
| `TM_CACHE_TTL_SECONDS` | no | unset ⇒ no verdict caching (never a guessed expiry) |

A template with every secret and non-secret knob is `control-plane/config/secrets.example.env`; the full
list of every dial and flag is [`config-reference.md`](config-reference.md) (generated).

Apply the migrations **in order, `0001` … `0015`** (the loop in §2), then start the control-plane.
**Verify:**

```bash
curl -s localhost:8080/readyz     # {"status":"ready","checks":{"db":true,"pepper":true},…}  — 503 if EITHER is down
curl -s localhost:8080/healthz    # liveness: 200 while the process is up, even with every dependency down
curl -s -H "authorization: Bearer $TM_FRONTDOOR_TOKEN" \
     -d '{"email":"someone@example.com"}' localhost:8080/v1/verify
```

At this tier the system returns cheap-layer verdicts (syntax, domain/MX, disposable, role, …) —
provable negatives at full confidence, everything needing SMTP as `risky`/`unknown`. That is a correct,
shippable state, not a degraded one. Migration `0014` must be applied before any real verdict is written:
it, with the maintenance loop, keeps `verification_raw` partitions ahead of time ([`operator-runbook.md`](operator-runbook.md) §8).

The control-plane is **not packaged** (no Dockerfile or compose file — [`future-work.md`](future-work.md)
FW-4): today a deployment is `go build ./cmd/control-plane`, an environment, and the migration loop.

### 3.2 SMTP verification — the real box

**Provide** a host that genuinely allows outbound port 25, with a settable PTR record, on a clean,
dedicated IP. The market for port-25 + settable-PTR + clean-IP hosts is narrow — that is the wall, not
the design. The ordered path with a status per step is [`go-live-checklist.md`](go-live-checklist.md)
Stage 1; the commands are here.

**a. Vet the host on a refundable trial, before paying.** Each is pass/fail; a hard fail disqualifies
the host. Record the output.

| V | Requirement | Proof (run from the trial box unless noted) |
|---|---|---|
| **V1** | Outbound TCP/25 genuinely open | `nc -vz <a-real-external-mx> 25` connects **and** `swaks --server <mx>:25 --quit-after CONNECT` sees a `220` banner — not the provider's claim |
| **V2** | Settable custom PTR/rDNS | set PTR for the box IP → the verifier FQDN; `dig -x <box-ip> +short` returns it |
| **V3** | Clean IP | check `<box-ip>` against Spamhaus/Barracuda/passive history **before** warmup |
| **V4** | Dedicated / stable IP | the provider confirms the IP is yours for the box's life |
| **V5** | Egress == PTR'd == audited IP | `curl -s ifconfig.me` == the PTR'd IP == the verifier A-record target == `host_audit.current_host_ip` — **all four identical** |

Also decide the verifier domain (dedicated/isolated), the private-network mechanism between brain and
box, and keep a documented fallback provider.

**b. DNS.** A-record `<verifier-domain>` → the box's **public egress** IP (not a private ingress IP):
`dig +short <verifier-domain>` == the egress IP. PTR: egress IP → `<verifier-domain>`:
`dig -x <egress-ip> +short`. SPF (required, **not** checked by the audit): a TXT authorizing the egress
IP — `dig TXT <verifier-domain>`.

**c. Deploy the box** — the forked image, byte for byte (truemail 3.3.1, CI-asserted unmodified). Node
environment: `VERIFIER_EMAIL` and `ACCESS_TOKENS` (required; the token is the one the brain sends as its
raw `Authorization` value, from `TM_NODE_TOKENS`), `VERIFIER_DOMAIN`, `SMTP_PORT` (25), and a real `DNS`
or the system default — replacing the mock ports.

- **Security posture, all before `enabled=true`:** the wrapper is bound to the private interface (a public port
  scan shows it closed); firewall: inbound wrapper port only from the brain, inbound 25 closed, egress 25 + DNS
  only; a per-node token over TLS or a tunnel; the box holds **no** database credential and no pepper.
- **Give the box a short hostname and a nameservers-only `resolv.conf`** (ADR-0019 addendum).
  `hostnamectl set-hostname verifier` (no dot — the PTR and A record carry the FQDN, the hostname need not);
  no `search` or `domain` line, no `options ndots:2+`. Otherwise `GET /v1/audit` is red
  (`warnings.resolver`) and the node does no SMTP.
- **Install the egress firewall before the first real probe** ([ADR-0021](adr/ADR-0021-smtp-probe-destination-guard.md);
  defence in depth — the layer DNS rebinding cannot race): `nft -f verifier-node/egress/verifier_egress.nft`
  (persist via your distro's `nftables.service`), then `nft list chain inet verifier_egress output` and confirm the
  counters exist and are 0. It rejects `tcp/25` to every non-public destination (IPv4 deny-list; IPv6 allow-list
  `2000::/3`). **Mirror it in the provider's egress security group**, and restrict the instance metadata service
  independently (IMDSv2, hop limit 1) — metadata is reachable on ports other than 25. Acceptance: from the box,
  `nc -w2 169.254.169.254 25` and `nc -w2 10.0.0.1 25` fail at once and the counter rises.

**d. The ordered, config-only flip — audit BEFORE traffic, the order is safety-critical.** Zero control-plane
code changes.

1. **Egress-25 smoke test, then set the node's `port25_open=true`.** ⚠ There is no operator/env/admin command
   for this yet ([`future-work.md`](future-work.md) FW-2). Interim, as `cp_admin`:
   `UPDATE nodes SET port25_open=true WHERE id='…'`. Until it is true the node is not `:smtp`-eligible.
2. **Point the box at real config** (`SMTP_PORT=25`, real `DNS`) and `POST /v1/config/reload` → a new
   `config_fingerprint` (which naturally invalidates a mock-era cache).
3. **Set the brain's node descriptor:** `TM_NODE_ID`, `TM_NODE_BASE_URL=<private-network address>`,
   `TM_NODE_PUBLIC_EGRESS_IP=<public egress IP>` (**must equal V5**), `TM_NODE_ENABLED=true`,
   `TM_SMTP_MODE=enabled`, and the node's token in `TM_NODE_TOKENS` (`<id>=<token>`). Node #2 is a numbered
   block, `TM_NODE_2_ID`, `TM_NODE_2_BASE_URL`, `TM_NODE_2_ENABLED`, `TM_NODE_2_PUBLIC_EGRESS_IP` — no code change.
4. **First audit → require green.** The node promotes `provisioning → active` **only** if the audit is green ∧
   `port25_open` ∧ the audit's `current_host_ip == TM_NODE_PUBLIC_EGRESS_IP`. A green on the wrong IP will not
   promote (ADR-0009) — look for the `egress-IP != audited current_host_ip (GAP-24)` warning.
5. **Start the warmup cap low:** `TM_WARMUP_DAILY_CAP=50` on day 1; step up only on prior-day low `smtp_blocked`
   and a green audit. The cap counts *verifications*; a catch-all check is about two connections — size against
   connections.
6. **Confirm real verdicts flow** with a non-null `verifier_ip` equal to the egress IP, and a real `node_id`,
   `engine_version` and `config_fingerprint`.

**Reversible:** `TM_NODE_ENABLED=false` or `TM_SMTP_MODE=disabled` returns to cheap verdicts with no redeploy and
no lost history.

**e. Go-live acceptance — the real-IP proofs the local build could only simulate.** Egress smoke → `port25_open`;
`/v1/audit` green and the node `provisioning → active`; break PTR → auto-quarantine and degrade to cheap (never
`invalid`) → restore PTR → N greens → recovers; an honest corporate address with a random probe rejected →
`valid` / `accepted` with `verifier_ip` = the egress IP; a Gmail/O365/Yahoo address → `risky` / `provider_no_smtp`
with SMTP never called; warmup cap exceeded → cheap + `smtp_skipped_reason=warmup_cap`; an external port scan finds
the wrapper port closed; the four-way IP identity holds; the provider-routing drift alarm is silent at boot (the
`provider-routing backstop in sync` log line).

**f. What does not change at cutover.** No control-plane diff (routing, parsing, scoring, logging, the gate and the
scheduler are identical mock → real — if cutover seems to need a code change, raise it, do not patch around it);
no node change; no schema change.

### 3.3 Paid third-party fallback (a vendor and a signed DPA)

**Provide** a chosen vendor and a **signed Data Processing Agreement**. The layer ships **disabled**; it egresses
addresses to a third party, so enabling it before the DPA is signed would be unlawful sub-processing. Three
independent affirmative switches, any one missing ⇒ off:

```
TM_EXTERNAL_DPA_CONFIRMED=true    # your affirmation that the DPA checklist passed (only the exact string "true")
TM_FALLBACK_MODE=enabled          # the fallback rung
TM_VENDOR_KEYS=<vendor>=<key>     # the vendor's API key
```

A real `external.Vendor` adapter must also be written and wired into `buildExternal` — **none ships** (the layer is
fail-closed without one; [`future-work.md`](future-work.md) V2). Set conservative ceilings from observed volume
before raising them: `TM_EXTERNAL_DAILY_CAP`, `_MONTHLY_CAP`, `_PER_JOB_CAP`, `_RATE_CEILING`. **Verify:** only the
*residue* is routed to the vendor, and a vendor `valid` is recorded as `risky` at a capped confidence — never an
SMTP-grade `valid`. *Reversible:* `TM_FALLBACK_MODE=off` is the global panic button.

### 3.4 Confidence calibration (a real send-outcome feed)

**Provide** an authenticated feed of real send outcomes over mutually-authenticated TLS, and provision the trust
anchor and the control-plane's client credential (loaded like the node tokens — never committed). A real
`MTLSClient` must be wired into `PullSource`; **it does not exist yet** (the sender does not exist —
[`future-work.md`](future-work.md) S2/S3). Until the feed is authenticated the ingestor pulls **nothing**
(fail-closed). The two live-path touches (`TM_FLYWHEEL_GATE_ENABLED`, `TM_FLYWHEEL_CALIBRATION_ENABLED`) stay
**OFF**, and the three calibration mutators stay **recommend-only**, until an owner deliberately opts in.

---

## 4. Live-HTTP verification on the first Linux host

Every endpoint is proven in-process locally (Go `httptest`, the fork's RSpec request specs — identical handler
code), because a long-lived server could not be run and killed reliably on the Windows dev host. That does not
exercise port binding, real socket serialisation, production middleware ordering or TLS. **On the first Linux
host, run this against a live server and confirm the results match the in-process ones**
(`future-work.md` H1/H2; go-live G1.11). Almost certainly it just works — but it is verified there, not assumed.

```bash
# 1. Start the control-plane on a real port:
TM_ENV=production TM_DATABASE_URL='postgres://…' TM_PEPPER='<32 bytes>' TM_FRONTDOOR_TOKEN='<token>' \
  TM_SUPABASE_KEY='<any non-empty value>' TM_LISTEN_ADDR=':8080' ./control-plane &

# 2. The socket really listens, and readiness is a LIVE ping (not a boot-time snapshot):
curl -fsS http://127.0.0.1:8080/healthz                            # 200 even with every dependency down
curl -fsS http://127.0.0.1:8080/readyz                             # 200 only with db:true AND pepper:true
docker stop <pg-container>
curl -sS -w ' <- %{http_code}\n' http://127.0.0.1:8080/readyz      # not_ready, db:false, 503   (no -f: we assert the 503)
docker start <pg-container>
curl -sS -w ' <- %{http_code}\n' http://127.0.0.1:8080/readyz      # recovers to ready/200 with no restart

# 3. A verdict over the socket, then the auth matrix (capture the status with -w; /v1/verify is POST-only):
curl -fsS -H "authorization: Bearer $TM_FRONTDOOR_TOKEN" -d '{"email":"not-an-email"}' http://127.0.0.1:8080/v1/verify
curl -sS -o /dev/null -w '%{http_code}\n' -X POST -d '{"email":"a@b.com"}' http://127.0.0.1:8080/v1/verify                       # 401
curl -sS -o /dev/null -w '%{http_code}\n' -X POST -H "authorization: Bearer wrong" -d '{"email":"a@b.com"}' http://127.0.0.1:8080/v1/verify   # 401
# bulk without a per-caller key → 401; with one → 202.

# 4. The node's endpoints over a real socket (the node compares the RAW token, not "Bearer <token>"):
docker run -d --name vn -e VERIFIER_EMAIL=… -e ACCESS_TOKENS=<tok> -p 3000:3000 verifier-node
curl -fsS http://127.0.0.1:3000/healthcheck
curl -fsS -H "authorization: <tok>" http://127.0.0.1:3000/v1/audit        # host_audit shape; also /v1/config, /v1/stats, /v1/catchall/:domain

# 5. If TLS terminates at the app (not a proxy), repeat 2–3 over https:// and confirm the certificate is served
#    and verified. Which of the two a deployment uses is its own decision (go-live G1.11).
```

**Pass:** the live results match the in-process results for the verdict matrix, auth behaves as in
[`api-contract.md`](api-contract.md), the LOG rows are correct (`from_cache` on a repeat, a redacted transcript),
and the node endpoints respond per contract — over a real socket, and TLS if terminated at the app.

---

## 5. Where everything lives

| You want… | Look in |
|---|---|
| The rules that govern all work | [`../PROJECT_RULES.md`](../PROJECT_RULES.md) |
| How it is built | [`architecture.md`](architecture.md) |
| What protects what | [`security-posture.md`](security-posture.md) |
| Every decision and ruling | [`open-decisions.md`](open-decisions.md) · [`adr/`](adr/) (start at [`adr/README.md`](adr/README.md)) |
| Every config knob, default and blast radius | [`config-manifest.md`](config-manifest.md) → [`config-reference.md`](config-reference.md) (generated) |
| The HTTP contract and what each verdict means | [`api-contract.md`](api-contract.md) (incl. the verdict guide appendix) |
| How to operate it, pull a kill-switch, read an alert | [`operator-runbook.md`](operator-runbook.md) |
| The ordered path to live, with a status per step | [`go-live-checklist.md`](go-live-checklist.md) |
| What is not done yet | [`future-work.md`](future-work.md) |
| How it is tested, and every gate | [`test-strategy.md`](test-strategy.md) |
| The deep design of each phase, and provenance | [`history/`](history/) |

When code and a phase spec disagree, the code and `open-decisions.md` win — but they rarely disagree, because
the specs were written to be built.

## 6. Working on the code — the review discipline

Changes are held to a **depth proportional to blast radius**:

- **Irreversible surfaces (the five doors + the nine guards, [`architecture.md`](architecture.md) §7–8): full-depth
  review, always.** Any change near hashing, the append-only LOG, the vocabulary, the CAS/lease invariants, the
  anti-laundering rule, the cascade brakes or the classifier exclusion gets a complete, adversarial review and a
  fail-verify pass (neuter the guard, watch the named test go red, restore from a copy, watch it go green).
- **Everything else: bounded review**, proportional to what it touches.

**Use the synchronous code reviewer** — one reviewer at a time, in the foreground. Parallel review is unreliable
here: concurrent Go compilation contends for the build-cache lock and wedges.

The non-negotiables, restated for a new contributor:

- **Additive-only** at the doors — extend, never mutate.
- **No dial is hardcoded** — anything the config manifest calls a dial is a readable config entry, and every settable
  knob is either genuinely consumed or explicitly documented as reserved (no settable-but-ignored knobs).
- **Every removal states its reason;** when unsure whether something is dead, keep it and flag it.
- **Harden toward `unknown`** — when in doubt the safe direction is the humbler verdict, never a fabricated
  `valid`/`invalid`.
- **A change is not done until its docs are updated** — `check-docs-updated.sh` fails a behaviour change with no
  doc or ADR touch ([`../PROJECT_RULES.md`](../PROJECT_RULES.md) §8).
