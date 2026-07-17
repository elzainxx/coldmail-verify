# truemail — ground truth vs. spec assumptions

**Status:** COMPLETE — source-read **and live run both done** (Phase-0 closeout, 2026-07-17). Every
PENDING-RUN row is filled from **real captured output** of the genuinely-running forked verifier-node.
**Updated 2026-07-17 (P1→P2 readiness):** the one row P0 deferred — GT-5 / GAP-14 `smtp_mock` accept-all —
is **now captured live** against the real `smtpmock` v2.5.4 server (§3.4), so Phase 2 does not build the
SMTP path on unverified engine behavior. One new non-door substrate delta flagged (blacklist→`421` not `554`).
Verification artifact.
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
| **`LIVE ✓`** | **Captured from the actually-running forked verifier-node** (`docker build` → `docker run` of `truemail-verifier-node:p0`, image 664MB), 2026-07-17. | Reality-confirmed — the strongest grade. |

**The live run is DONE (Phase-0 closeout).** The fork was built and executed:
- **`docker build` succeeded** on `ruby:3.2.0-slim` (matched to the fork's `.ruby-version`); `bundle`
  resolved and installed **truemail 3.3.1**, **net-smtp 0.5.1**, thin/eventmachine (native), 20 gems —
  `Bundle complete!`. Door 5 proven at runtime: `Truemail::VERSION = 3.3.1`.
- **The stock rack ran** and answered on `:3000`; all endpoint/JSON/host_audit shapes below are its
  **real output**, quoted verbatim.

**Environment note (resolved, recorded for reproducibility):** the host runs a Norton SSL/TLS-scanning
proxy that re-signs HTTPS with a private root CA, which broke in-container `gem`/`bundle` cert
verification. **Fixed the security-preserving way** — the proxy's root CA is exported to
`verifier-node/truemail-rack/certs/` (machine-local, gitignored) and trusted in the image via
`update-ca-certificates`; TLS verification is **never disabled**. A clean environment (CI) has no such
cert and the trust step is a no-op. See the Dockerfile + FORK.md. (This is why the *earlier* pass, on a
host with no Docker daemon and a blocked registry, correctly left these rows PENDING rather than faking
them — they are now filled for real.)

### The live-capture commands (run 2026-07-17; reproducible)

```bash
# 1. Build the stock pinned rack (regenerates the lock, resolves truemail 3.3.1).
#    Legacy builder used here (DOCKER_BUILDKIT=0) — BuildKit's network namespace interacts
#    badly with the host TLS-proxy; the legacy builder shares the daemon network.
DOCKER_BUILDKIT=0 docker build -t truemail-verifier-node:p0 verifier-node/truemail-rack

# 2. Door 5 proven at runtime:
docker run --rm truemail-verifier-node:p0 ruby -e 'require "truemail"; puts Truemail::VERSION'  # => 3.3.1

# 3. Run the stock rack + capture real shapes (from host → published :3000):
docker run -d --name vn -p 3000:3000 -e VERIFIER_EMAIL=verifier@example.test -e ACCESS_TOKENS=tok truemail-verifier-node:p0
curl -s -H "Authorization: tok" http://localhost:3000/version
curl -s -o /dev/null -w "%{http_code} %{size_download}B\n" http://localhost:3000/healthcheck
curl -s -o /dev/null -w "%{http_code}\n" -H "Authorization: tok" http://localhost:3000/validate  # 404 (GAP-1)
curl -s -H "Authorization: tok" 'http://localhost:3000/?email=test@example.com'                  # validation JSON

# 4. host_audit (gem primitive; not a stock endpoint) + as_json key check, in-container:
echo 'require "truemail";require "json";Truemail.configure{|c| c.verifier_email="v@example.test"};\
puts Truemail.host_audit.as_json; \
j=JSON.parse(Truemail.validate("test@example.com").as_json); puts j.key?("domain")' | docker exec -i vn ruby
```

### LIVE captured output (verbatim, 2026-07-17) — the evidence the rows below cite

```jsonc
// GET /version  (auth: 200)
{"core":"3.3.1","platform":"ruby 3.2.0 (2022-12-25 revision a528908271) [x86_64-linux]","version":"0.9.0"}

// GET /healthcheck (no auth)  ->  status=200  body=0 bytes
// GET /validate    (auth)     ->  status=404   (GAP-1 confirmed: no /validate)
// GET /?email=...  (no token) ->  status=401   (auth enforced)

// GET /?email=test@example.com  (auth: 200) — the lean validation body:
{"date":"2026-07-17 01:10:32 +0000","email":"test@example.com","validation_type":"smtp",
 "success":false,"errors":{"mx":"target host(s) not found"},"smtp_debug":null,
 "configuration":{ ...subset: no verifier_email/domain, timeouts, smtp_port... }}
// as_json is a STRING; keys = [configuration,date,email,errors,smtp_debug,success,validation_type]
// HAS_DOMAIN=false  HAS_MAIL_SERVERS=false   (GAP-2/9 confirmed LIVE — struct has them, JSON drops them)

// Truemail.host_audit.as_json:
{"date":"2026-07-17 01:12:28 +0000","current_host_ip":"197.51.161.198",
 "warnings":{"dns":"A-record of verifier domain not refers to current host ip address",
             "ptr":"PTR-record for current host ip address was not found"},
 "configuration":{ ...subset... }}
// host_audit keys = [configuration, current_host_ip, date, warnings]; warning keys = ["dns","ptr"]

// smtp_debug on a real SMTP attempt (gmail.com, timed out per policy) — ARRAY of per-host requests,
// each a Truemail::Validate::Smtp::Request whose @response struct is:
//   port_opened=true, connection=false, helo=nil, mailfrom=nil, rcptto=nil, errors={:connection=>"server response timeout"}
// -> confirms P2 §6.2 parse-target shape (array of per-host structs; failures in errors{stage} as raw strings).
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
| `domain` | **absent** from JSON (`HAS_DOMAIN=false` live) | P1 GAP-2: "rack JSON omits `domain`/`mail_servers`" | **GAP-2 CONFIRMED** `LIVE ✓` |
| `mail_servers` | **absent** from JSON (`HAS_MAIL_SERVERS=false` live) | P1 GAP-2 (control-plane self-resolves MX); P2 fork **adds** an `mx` block | **GAP-2 CONFIRMED** `LIVE ✓` |
| `smtp_debug` (on failure) | **array of raw-string** transcripts (`errors:{rcptto:"550 …\n"}`), no numeric code | P1 GAP-4 / P2 §6.2: raw strings, control-plane parses the code | **GAP-4 CONFIRMED** `SOURCE ✓` (README example) |
| `smtp_debug` (on clean success) | **`null`** | P2 §6.8 discriminator (`success && smtp_debug==null` = clean 250) | matches `SOURCE ✓` |
| `configuration` | **subset** (no `verifier_email/domain`, timeouts, `smtp_port`, `connection_attempts`) | P1 GAP-3 / P2 §4.6: fork adds `/v1/config` for full config | **GAP-3 CONFIRMED** `SOURCE ✓` |
| `date` | engine **local-tz string** (`"2021-02-07 10:00:56 +0200"`) | P1 GAP-8: control-plane stores UTC, keeps raw in sidecar | **GAP-8 CONFIRMED** `SOURCE ✓` |
| `validation_type` | `regex\|mx\|mx_blacklist\|smtp` (+ `whitelist\|blacklist` overrides) | P1 §3.2.2 `engine_validation_type`; GAP-7 whitelist/blacklist | matches `SOURCE ✓` |
| `success` | engine's own boolean, distinct from our `status` | P1 §3.2.2 `engine_success` kept distinct | matches `SOURCE ✓` |
| exact runtime `smtp_debug` array on a live reject | **array of per-host `Smtp::Request`; each `@response` = `{port_opened,connection,helo,mailfrom,rcptto,errors}`**; failures in `errors{stage}` as raw strings | P2 §6.2 parse procedure | **CONFIRMED** `LIVE ✓` (real gmail.com attempt) |

**No door is contradicted.** Every P1/P2 assumption about the lean body is now confirmed — the
source-read rows by reading the fork, and the runtime rows (`domain`/`mail_servers` drop, `smtp_debug`
array shape) by the **live run**.

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
- P1 §3.2.4 builds `engine_version = rack@<version>+gem@<core>` from this → **live `/version` returned
  `{"core":"3.3.1","platform":"ruby 3.2.0 …","version":"0.9.0"}`** → `engine_version = rack@0.9.0+gem@3.3.1`,
  not the specs' illustrative `rack@0.10.0+gem@3.3.0`. **Flagged below (§5).** `LIVE ✓`

### 3.3 `host_audit` shape — the P3 audit gate anchor `LIVE ✓`
- **Captured live** (`Truemail.host_audit.as_json`, in-container): keys =
  `[configuration, current_host_ip, date, warnings]`; `warnings` keys = `["dns","ptr"]`; on this
  un-provisioned host `warnings` is non-empty (dns + ptr misalignment) → **red**, exactly as expected
  for a box with no verifier DNS/PTR set. The P3 gate anchor (`current_host_ip` + `warnings`, green iff
  `warnings=={}`) is **confirmed against real output.**
- **New minor delta (GT-6, §5):** the live `host_audit.as_json` **also carries `date` and
  `configuration`** — richer than the specs' minimal `{current_host_ip, warnings}` (Appendix A.6). The
  extra keys are additive/ignorable; the gate reads only `warnings`. Non-door; flagged for record.
- **Note:** `host_audit` is a **gem primitive**, captured via a direct in-container `ruby -e` call — the
  **stock rack still exposes no `/audit` endpoint** (that wrapper is Phase 2, correctly not built). So
  the *shape* is now reality-confirmed for the P3 gate, while the *endpoint* remains a P2 activity.

### 3.4 GAP-14 — smtp_mock accept-all behavior (the `valid`-path caveat) `LIVE ✓ (captured 2026-07-17, P1→P2 readiness)`
- **What the P0 live run showed:** validating a real address drove **real SMTP egress** — the fork reached
  gmail.com's MX list and (per the 1–2s policy timeout) returned `unknown`-shaped
  (`errors:{:connection=>"server response timeout"}`), never a fabricated verdict. This confirmed the
  fork genuinely performs SMTP (not just cheap layers) and honors harden-toward-unknown on timeout.
- **GAP-14 substrate behavior — NOW CAPTURED LIVE (was deferred to P2).** The P1→P2 readiness pass ran the
  real `smtpmock` server (Go binary **v2.5.4**, the server the `smtp_mock` gem wraps) with
  `-notRegisteredEmails "nx-seeded@example.test" -blacklistedRcpttoEmails "blocked@example.test"
  -multipleRcptto`, and drove it over a raw socket. **Verbatim captured RCPT TO responses:**
  ```text
  RANDOM   924360…@example.test          -> 250 Received      # accept-all by default (GAP-14 CONFIRMED)
  SEEDED   nx-seeded@example.test        -> 550 User not found # a pre-listed addr is really rejected
  BLOCKED  blocked@example.test          -> 421 Service not available   # blacklisted rcptto (see note)
  SAME-SESSION two-RCPT (multipleRcptto):
    target 924360…@example.test          -> 250 Received      # catch-all probe: target accepted
    probe  nx-seeded@example.test        -> 550 User not found # + probe rejected in the SAME session
  ```
- **What this proves for Phase 2 (so P2 does not build on unverified engine behavior):**
  1. **Accept-all-by-default is REAL** — a random probe is always `250`, so the `valid` path (P2 §5.5 row 4)
     is unreachable without a **seeded** probe pre-listed for rejection. GAP-14 confirmed against the real server.
  2. The **seeded-probe path to `valid` works** — a pre-listed address returns a genuine `550`, so
     target-250 + seeded-probe-550 is achievable (the only `valid` path on the mock).
  3. The **same-session two-RCPT catch-all probe shape works** — `-multipleRcptto` lets one session carry
     target + probe with distinct responses. This is the substrate proof for P2 §5.4 Option A (before the
     GAP-13 spike decides whether the *wrapper* can drive it without a gem patch).
- **⚠ Phase-2 substrate delta (flagged, non-door): blacklisted rcptto returns `421`, not a `554`-class 5xx.**
  P2 spec §6.1 / Appendix B row 3 illustrate the your-fault/policy-block case with a **`554` 5xx** →
  `smtp_blocked`. The real `smtpmock` returns **`421 Service not available`** (a **4xx transient**) for a
  blacklisted RCPT. Both map to **`unknown`** (harden-toward-unknown holds either way — no door touched), but
  the *reason* differs: a 421 classifies as transient (`smtp_timeout`/`greylisted`-shaped) rather than
  `smtp_blocked`.
- **✅ RECONCILED in Phase 2 (2026-07-17) — the delta is resolved, not just flagged.** The P2 build took
  the "custom-message fixture" path: the socket proof harness (`verifier-node/truemail-rack/test/p2/`)
  crafts a **554** via `msg_rcptto_blacklisted_email` to exercise the `smtp_blocked` path, AND separately
  asserts the **stock** blacklist knob really returns **`421`** — confirming this delta empirically
  (socket proof row 3 + the "delta" row, **8/8 passed**). The brain classifier
  (`control-plane/internal/smtp/classify.go`) treats **both** 421 and 554-block as `unknown`
  (harden-toward-unknown intact): a 421 → transient (`greylisted`-shaped, retry-eligible), a 554-policy →
  `smtp_blocked`. Neither is ever `invalid`. No frozen door touched. This closes the P1→P2 handoff's
  open "§6.1 must reconcile the 421-vs-554 delta" item.

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
| **GT-4** | `host_audit` live shape | P3 builds the audit gate on `{current_host_ip, warnings}` | **CONFIRMED LIVE** — real keys `[current_host_ip, warnings(+date,configuration)]`, `warnings` keys `["dns","ptr"]` (§3.3) | P3 audit gate | **Accept** — the gate anchor matches reality; see GT-6 for the extra keys. Resolved (no longer pending). |
| **GT-5** | `smtp_mock` accept-all (P2 GAP-14) | P2 GAP-14 (seeded probe needed for `valid`) | **NOW CAPTURED LIVE (2026-07-17, §3.4):** real `smtpmock` v2.5.4 — random RCPT→`250` (accept-all confirmed), seeded→`550`, same-session two-RCPT works. Plus a **new delta:** blacklisted rcptto→`421` (4xx), not the spec's `554` (5xx) — both `unknown`, reason differs. | P2 `valid`-path + `smtp_blocked` path | **Accept** — GAP-14 accept-all confirmed against the real server; seeded-probe `valid` path proven reachable. **P2 build must reconcile the 421-vs-554 block-code delta** (§3.4) — non-door. |
| **GT-6** (new, from live run) | `host_audit.as_json` carries extra keys | Appendix A.6 shows minimal `{current_host_ip, warnings}` | live output **also** includes `date` + `configuration` | none (gate reads only `warnings`) | **Accept** — additive/ignorable; the P3 gate logic is unaffected. Non-door. Record only. |

**Deliverable #2 is now FULLY met.** Every row is either `SOURCE ✓` or `LIVE ✓`; **no row remains
PENDING-RUN.** The fork was built and executed, truemail 3.3.1 confirmed at runtime, and all
door-relevant shapes (endpoints, lean JSON with `domain`/`mail_servers` dropped, `smtp_debug` array,
`host_audit`, `/version`, auth, GAP-1) are reconciled against **real captured output**. Recorded in
[`phase-0-status.md`](phase-0-status.md).

---

## 6. Summary — is any frozen door built on a wrong assumption?

**No frozen door is contradicted by ground truth.** Every mismatch found (GT-1..GT-6) is either an
*illustrative* version string the specs never froze, a *forced dependency* resolution (net-smtp), or an
*additive extra key* — none requires rewriting a Door 1–5 contract. The specs' own GAP-1/2/3/4/8 flags
are **confirmed** — the source-read ones by reading the fork, and the runtime ones **by the live run**.
The single
honest caveat: **the live-run rows must be captured before Phase 2/3 code treats `smtp_debug`,
`host_audit`, and `smtp_mock` accept-all behavior as reality-verified.**
