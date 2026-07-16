# truemail — ground truth vs. spec assumptions

**Status:** PARTIAL — source-read complete; **live run PENDING** (see §0). Planning/verification artifact.
**Purpose (Phase 0 deliverable #2):** reconcile **what the pinned engine actually is/emits** against
**what the seven specs assumed**, for every field / endpoint / behavior a one-way door depends on.
**Rule (non-negotiable):** any mismatch is a **flagged item for owner sign-off — never a silent fix.**
A frozen door built on a wrong assumption is the exact failure the seven-phase planning prevented.
**Anchors:** pinned engine `truemail 3.3.1` ([ADR-0003](adr/ADR-0003-truemail-engine-pin.md)); fork `truemail-rack v0.9.0` ([FORK.md](../verifier-node/truemail-rack/FORK.md)).

---

## 0. Provenance & method — read this first (why some rows are PENDING)

There are **two** grades of evidence in this document, and they are **never** conflated:

| Grade | Meaning | Trust for a one-way door |
|---|---|---|
| **`SOURCE ✓`** | Read directly from the **pinned fork's own source** (`verifier-node/truemail-rack/app/…`), the **gem's `v3.3.1` gemspec/version**, or **upstream's own documented output** (rack README). | Authoritative for **endpoints, JSON keys/shape, config surface**. |
| **`PENDING-RUN`** | Requires **executing** the stock rack against `smtp_mock` + `dns_mock` to observe **runtime values / behavior**. | **Not yet verified.** Do not freeze a door on a `PENDING-RUN` row until the live capture confirms it. |

**Why the live run is pending in this environment (flagged, not worked around):**
- **Docker daemon not running** — `docker` CLI is present but the Desktop Linux engine pipe is
  absent (`open //./pipe/dockerDesktopLinuxEngine: … cannot find`). The verifier-node builds/runs only via Docker.
- **No host Ruby** — `ruby`/`bundle` are not installed on the host.
- **rubygems.org HTTPS blocked here** — `curl` to the registry fails with a Windows schannel
  revocation error (`CRYPT_E_NO_REVOCATION_CHECK`), so gems can't resolve on the host either.

None of these are project faults; they are this sandbox's limits. The capture is **ready and
reproducible** — the exact command is below. **The `PENDING-RUN` rows must be filled by running it
on a machine with a working Docker daemon (or Ruby + registry access) before the affected doors are
treated as reality-confirmed.**

### The exact live-capture command (run this to complete the doc)

```bash
# 1. Build + run the stock pinned rack (regenerates the lock, resolves truemail 3.3.1):
docker compose -f verifier-node/docker-compose.yml build
docker compose -f verifier-node/docker-compose.yml up -d

# 2. Confirm the pin actually resolved to 3.3.1 (Door 5, stronger than the host check):
docker compose -f verifier-node/docker-compose.yml exec verifier-node bundle list truemail   # => truemail (3.3.1)

# 3. Capture the REAL emitted shapes (fill the PENDING-RUN rows from these):
TOKEN=phase0-stock-smoke-token
curl -s -H "Authorization: $TOKEN" 'http://localhost:3000/version'                 # /version shape
curl -s -i http://localhost:3000/healthcheck                                        # 200, empty body
curl -s -H "Authorization: $TOKEN" 'http://localhost:3000/?email=test@example.com'  # validation JSON shape
curl -s -i 'http://localhost:3000/validate'                                         # PROVE /validate 404s (GAP-1)

# 4. For smtp_debug/host_audit/GAP-14 accept-all behavior: point the gem's dns:/smtp_port:
#    at dns_mock/smtp_mock fixtures (Phase-2 substrate) and re-capture. host_audit is a GEM
#    primitive NOT exposed by the stock rack (no /audit) — it requires the Phase-2 wrapper or a
#    direct `ruby -e 'require "truemail"; …Truemail.host_audit…'` inside the container.
```

---

## 1. Endpoints — what the stock pinned rack actually serves `SOURCE ✓`

Read from `verifier-node/truemail-rack/app/truemail_server/router.rb` (the `Resolver`) — **authoritative**:

| Endpoint | Exists in stock fork? | Spec assumption | Verdict |
|---|---|---|---|
| `GET /?email=<addr>` (root = validation) | **yes** (`Resolver`: `resource == '/'`) | P1 build-plan §1.3 said *"hit `/validate`"* | **GAP-1 CONFIRMED** — see §3.1 |
| `GET /version` | **yes** | assumed (Appendix A.2) | matches |
| `GET /healthcheck` | **yes** (returns `200`, **empty body**) | assumed | matches — note empty body |
| anything else | `Error::Show` (404-class) | — | — |
| `GET /validate` | **NO** | build-plan §1.3 wording | **does not exist** (GAP-1) |
| `GET /audit` (`host_audit`) | **NO** (not in stock rack) | P2 §4.2 adds it as a **fork/wrapper** endpoint | **not stock** — Phase-2 addition; see §4 |
| `/verify`,`/catchall`,`/config`,`/stats` | **NO** | P2 wrapper endpoints | **not stock** — Phase 2, correctly absent in P0 |

**Consequence:** the specs already know this — P1 flags GAP-1 and P2 introduces the fork's
`/v1/verify` + `/v1/audit` etc. **Phase 0 confirms the starting reality: the stock fork has no
`/validate` and no `/audit`.** No door is frozen on `/validate`. ✓ consistent, no new mismatch.

---

## 2. The validation JSON body — real shape vs. what the specs bank on

Source: `services/email_validator.rb` = **`Truemail.validate(email).as_json`** (`SOURCE ✓`), i.e.
the gem's lean `ValidatorJson`. Confirmed against upstream README's own documented 200 response.

| Field a door depends on | Real stock output | Spec assumption | Verdict |
|---|---|---|---|
| top-level keys | `date, email, validation_type, success, errors, smtp_debug, configuration` | P1 §3.1.3 knows the lean set; control-plane builds its own response | matches `SOURCE ✓` |
| `domain` | **absent** from JSON | P1 GAP-2: "rack JSON omits `domain`/`mail_servers`" | **GAP-2 CONFIRMED** `SOURCE ✓` |
| `mail_servers` | **absent** from JSON | P1 GAP-2 (control-plane self-resolves MX); P2 fork **adds** an `mx` block | **GAP-2 CONFIRMED** `SOURCE ✓` |
| `smtp_debug` (on failure) | **array of raw-string** transcripts (`errors:{rcptto:"550 …\n"}`), no numeric code | P1 GAP-4 / P2 §6.2: raw strings, control-plane parses the code | **GAP-4 CONFIRMED** `SOURCE ✓` (README example) |
| `smtp_debug` (on clean success) | **`null`** | P2 §6.8 discriminator (`success && smtp_debug==null` = clean 250) | matches `SOURCE ✓` |
| `configuration` | **subset** (no `verifier_email/domain`, timeouts, `smtp_port`, `connection_attempts`) | P1 GAP-3 / P2 §4.6: fork adds `/v1/config` for full config | **GAP-3 CONFIRMED** `SOURCE ✓` |
| `date` | engine **local-tz string** (`"2021-02-07 10:00:56 +0200"`) | P1 GAP-8: control-plane stores UTC, keeps raw in sidecar | **GAP-8 CONFIRMED** `SOURCE ✓` |
| `validation_type` | `regex\|mx\|mx_blacklist\|smtp` (+ `whitelist\|blacklist` overrides) | P1 §3.2.2 `engine_validation_type`; GAP-7 whitelist/blacklist | matches `SOURCE ✓` |
| `success` | engine's own boolean, distinct from our `status` | P1 §3.2.2 `engine_success` kept distinct | matches `SOURCE ✓` |
| exact runtime `smtp_debug` array on a live reject | — | P2 §6.2 parse procedure | **PENDING-RUN** (needs smtp_mock reject fixture) |

**No door is contradicted.** Every P1/P2 assumption about the lean body is confirmed from source;
the two things still needing a live run are the *runtime* `smtp_debug` array contents and timing.

---

## 3. Specific one-way-door reconciliations

### 3.1 GAP-1 — `/validate` (build-plan) vs `/` (reality) `SOURCE ✓ CONFIRMED`
Stock rack validation is the **root** `/`, not `/validate` (router.rb). The specs already resolved
this (node client targets `/` in P1; the fork adds `/v1/verify` in P2). **Not a frozen-door break** —
the door is the *response contract*, which is versioned and control-plane-owned. **No sign-off needed beyond
noting the build-plan's `/validate` wording was never literal.** ✓

### 3.2 `/version` shape `SOURCE ✓`
`services/environment.rb` emits **`{core, platform, version}`** where `core = Truemail::VERSION`
(**`"3.3.1"`**), `platform = ruby -v` string, `version = TruemailServer::VERSION` (the **rack's** version).
- P1 §3.2.4 builds `engine_version = rack@<version>+gem@<core>` from this → with the real pin that is
  **`rack@0.9.0+gem@3.3.1`**, not the specs' illustrative `rack@0.10.0+gem@3.3.0`. **Flagged below (§5).** `SOURCE ✓`

### 3.3 `host_audit` shape — the P3 audit gate anchor `SOURCE ✓ (shape)` / `PENDING-RUN (live)`
- **Shape** (specs' Appendix A.6, and the gem's public `Truemail.host_audit`): `{current_host_ip, warnings:{}}`
  where `warnings == {}` ⇒ green; keys are `dns`/`ptr`. The P3 gate (`green AND port25_open`) is built on this.
- **Reality check:** the **stock rack does not expose `host_audit`** (no `/audit` endpoint — §1). It is a
  **gem primitive**; the Phase-2 fork wraps it. So its *live output* **cannot be captured from the stock
  rack** — it needs the Phase-2 wrapper or a direct in-container gem call. **PENDING-RUN**, and correctly
  so: Phase 0 does not build `/audit`. **No door frozen on unconfirmed `host_audit` runtime output.**

### 3.4 GAP-14 — smtp_mock accept-all behavior (the `valid`-path caveat) `PENDING-RUN`
P2 GAP-14: `smtp_mock` 250-accepts every RCPT except pre-listed ones → it is catch-all-shaped, so the
`valid` path is only testable with a **seeded** probe. This is **`smtp_mock` runtime behavior** (a dev
dependency), **not** observable from fork source. **PENDING-RUN** — must be confirmed when the live
substrate runs. Flagged so "we proved the valid path" is never claimed from source alone.

---

## 4. What Phase 0 deliberately did NOT capture (correct scope)

- **`host_audit`/`/audit` live output** — Phase-2 wrapper endpoint; not stock (§3.3).
- **Catch-all 2nd-RCPT probe, 5xx classification, greylist retry** — Phase 2/5 behaviors; the stock
  rack does one `Truemail.validate` per request with no wrapper logic.
- **Real SMTP against a real IP** — Phase 3.

These are **not** gaps in ground-truth; they are out-of-phase. Capturing them now would mean building
Phase-2 code, which Phase 0 must not do.

---

## 5. Mismatches flagged for owner sign-off (never silently fixed)

Per the rule. None of these mutates a frozen door; each is surfaced for an explicit decision.

| # | Mismatch | Spec said | Reality (pinned) | Impact | Recommended disposition |
|---|---|---|---|---|---|
| **GT-1** | engine version | appendices cite `gem@3.3.0` | **`3.3.1`** (latest tag; `version.rb`) | `engine_version` provenance string | **Pin 3.3.1** (ADR-0003). Specs' `3.3.0` was illustrative — accept 3.3.1, no door change. |
| **GT-2** | rack version | appendices cite `rack@0.10.0` | **`v0.9.0`** (no `0.10.0` tag exists) | `engine_version = rack@0.9.0+gem@3.3.1` | Accept real version in provenance. Illustrative spec string; no door change. |
| **GT-3** | `net-smtp` pin | (not in specs) | 3.3.1 forces `~> 0.5.0`; upstream rack pinned `~> 0.4.0` (conflict) | fork build resolvability | **Bump fork's net-smtp to 0.5.0** (FORK.md #2). Rack-side edit, not a gem patch. Owner: confirm acceptable. |
| **GT-4** | `host_audit` live shape | P3 builds the audit gate on `{current_host_ip, warnings}` | shape matches specs, but **not verifiable until the Phase-2 wrapper runs it live** | P3 audit gate | Do **not** freeze the P3 gate's runtime assumptions until the PENDING-RUN capture (§3.3) confirms them on a real node. |
| **GT-5** | `smtp_mock` accept-all | P2 GAP-14 (seeded probe needed for `valid`) | **PENDING-RUN** — unverified from source | P2 `valid`-path proof | Confirm at live capture; until then P2's `valid`-path proof is "designed," not "observed." |

**All PENDING-RUN rows and GT-4/GT-5 mean deliverable #2 is only *partially* met in this
environment** — the reconciliation is complete for everything source-readable (endpoints, JSON shape,
config surface, versions), and **explicitly pending** for everything that needs a live run. This is
recorded honestly in the Phase-0 status ([`phase-0-status.md`](phase-0-status.md)), not papered over.

---

## 6. Summary — is any frozen door built on a wrong assumption?

**No frozen door is contradicted by ground truth.** Every mismatch found (GT-1..GT-5) is either an
*illustrative* version string the specs never froze, a *forced dependency* resolution (net-smtp), or a
*runtime behavior still pending a live run* — none requires rewriting a Door 1–5 contract. The specs'
own GAP-1/2/3/4/8 flags are **confirmed from source**, exactly as the specs predicted. The single
honest caveat: **the live-run rows must be captured before Phase 2/3 code treats `smtp_debug`,
`host_audit`, and `smtp_mock` accept-all behavior as reality-verified.**
