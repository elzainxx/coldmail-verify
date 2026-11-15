# Full Test Plan — Email Verification System

Work top to bottom. Each tier assumes the previous one passed.

> **Exact commands live in your repo** — see [`ONBOARDING.md`](../install-and-deploy.md) (§5 "Running
> everything locally") and [`go-live-runbook.md`](../go-live-checklist.md). This document specifies
> *what* to test, *how to verify*, and *what a pass looks like*.

> **In-process substitution for the HTTP tiers.** On a dev host where a long-lived HTTP server
> can't be run/killed reliably (observed on Windows), the endpoint tiers (§5, §6, §10) are proven
> by the projects' own in-process handler tests — `httptest` for the Go API, RSpec request specs
> for the fork — which exercise the identical handler code. That is the right call for local
> verification, but it does **not** cover port binding, real socket serialization, production
> middleware ordering, or TLS. Those are verified against a **live server on the first
> Linux/production host** — see §5.4 below and [`go-live-runbook.md`](../go-live-checklist.md).

---

## 0. Prerequisites

### Tools
| Tool | Why |
|---|---|
| Go (version per `go.mod`) | build + test the control plane |
| Docker | Postgres 16, the verifier-node container |
| Ruby | only if running the fork outside Docker |
| psql (or any PG client) | direct row inspection — several tests require it |
| curl / httpie / Postman | API smoke tests |
| git | fresh-clone test |

### Environment variables
| Var | Set for testing? | Note |
|---|---|---|
| `TM_PEPPER` | **yes** | Door 3. App must refuse to start without it (in production) |
| Postgres/Supabase DSN | **yes** | point at the throwaway container |
| Front-door auth token | **yes** | sync path |
| Per-caller API key | **yes** | required for bulk (P5/PN-13) |
| `TM_QUEUE_ADDR_KEY` | **yes** | AES-256-GCM; queue subsystem only activates when set |
| `TM_FALLBACK_MODE` | **no — leave unset** | testing it disabled *is* a test |
| `TM_EXTERNAL_DPA_CONFIRMED` | **no** | same |
| `TM_VENDOR_KEYS` | **no** | same |
| mTLS certs (outcome feed) | **no** | not testable without a real source |

### Ground rules
- **Never test migrations against a database that was migrated incrementally.**
  Always start from empty and apply `0001` → latest.
- **Never use `git checkout` to restore a neutered file.** Copy to a backup
  first, restore from the backup. `git checkout` can wipe unrelated work.
- **A test that passes is worth less than a test that provably fails when its
  guard is removed.** Tier 4 is the point of this document.

---

## 1. Build & static checks

| # | Step | Pass |
|---|---|---|
| 1.1 | `go version` matches `go.mod` | exact match |
| 1.2 | `docker ps` responds | daemon up |
| 1.3 | `go build ./...` | clean, no output |
| 1.4 | `go vet ./...` | clean |
| 1.5 | Start Postgres 16 throwaway container | healthy, DSN noted |
| 1.6 | Start app in production (`TM_ENV=production`) with `TM_PEPPER` **unset** | **refuses to start** (`secrets: TM_PEPPER is required`) — this is a pass |
| 1.7 | Start app with pepper set | starts; `/healthz` 200 with `status`+`at`; `/readyz` carries `{"status":…,"checks":{"db":…,"pepper":…,"queue":…},"at":…}` and is **`ready`/200 only when ALL checks are true** — with the DB down (or `TM_DATABASE_URL` unset) it is `not_ready`/503 even though the pepper is set, and with `TM_QUEUE_ADDR_KEY` unset it is `not_ready`/503 with `"reasons":["queue: TM_QUEUE_ADDR_KEY is not set: …"]` |

---

## 2. Schema & migrations

| # | Step | Pass |
|---|---|---|
| 2.1 | Apply migrations `0001` → latest on an **empty** DB | all apply, no error |
| 2.2 | List tables | all expected tables present (verifications, verification_raw, cache, catchall_domains, disposable_domains, nodes, policies, node_audits, node_warmup, rate_budgets, work_items, jobs, api_callers, external_calls, send_outcomes + join, calibration) |
| 2.3 | Check `send_outcomes` + join table | exist and are **empty** — created in P1, filled later |
| 2.4 | Check join anchor | references `verifications.id` (verdict-level), **not** address-level fan-out |
| 2.5 | Check every hash column type | `bytea` everywhere |
| 2.6 | Check `mx_provider` | a typed column, not buried in JSON |
| 2.7 | Vocabulary CHECK constraints | match the enum source of truth exactly; inserting a bogus status/reason is **rejected** |

---

## 3. Automated test suites

| # | Step | Pass |
|---|---|---|
| 3.1 | `go test ./...` | all packages green |
| 3.2 | `go test -shuffle=on ./...` | still green — order-dependence is a real bug class |
| 3.3 | PG-gated integration suite against live Postgres | all green |
| 3.4 | Full gate: build + test + vet + Door-5 gem-unmodified + append-only grants + config-freshness + docs-updated | every check green |

> **3.3 is not optional.** The in-memory fake has hidden real bugs in multiple
> phases (a UUID cast failure, a first-page pagination failure, store-parity
> gaps). Anything that only runs against the fake is under-tested.

---

## 4. Fresh-clone test

Catches load-bearing untracked files and tracked secrets.

| # | Step | Pass |
|---|---|---|
| 4.1 | `git clone` into a **clean** directory | succeeds |
| 4.2 | `go build ./...` there | clean — no missing package |
| 4.3 | Apply migrations from empty | clean |
| 4.4 | Run the full gate there | green |
| 4.5 | `git grep` for keys/secrets/credentials | **zero** tracked |
| 4.6 | Confirm `.gitignore` excludes credential files at any depth but does **not** exclude source packages | both true |

---

## 5. API smoke tests

Start the control plane. POST to `/v1/verify` with a valid token.

### 5.1 Verdict matrix

| Input | Expected status / reason | Confidence | Rec | Proves |
|---|---|---|---|---|
| `not-an-email` | `invalid` / `invalid_syntax` | high | skip | syntax layer terminal |
| `a@<nonexistent-domain>` | `invalid` / `no_mx` | high | skip | real DNS NXDOMAIN |
| address on a null-MX domain | `invalid` / `null_mx` | high | skip | RFC 7505 handled |
| `test@gmail.com` | `risky` / `provider_no_smtp` | low–mid | review | routed `:mx`; **never claims valid** |
| `test@outlook.com` | `risky` / `provider_no_smtp` | low–mid | review | O365 also `:mx` |
| `x@mailinator.com` | `risky` / `disposable` | low–mid | skip | disposable is **risky, never invalid** |
| `info@<corp domain>` | role flag set | reduced | review | role flags, never gates alone |
| `user@gmial.com` | typo suggestion present | — | — | typo layer |
| Any of the above, repeated | same verdict, `from_cache=true` | same | same | cache path |

### 5.2 Contract invariants

| # | Check | Pass |
|---|---|---|
| 5.2.1 | Response is versioned (`/v1/`) | version present |
| 5.2.2 | Across all inputs above, an `invalid` **never** carries `send` | structurally impossible |
| 5.2.3 | No response contains raw SMTP codes, layer names, node IPs, or cost hints | two-vocabularies rule holds |
| 5.2.4 | Every response carries `meta` (engine version, config fingerprint, latency) | present |

### 5.3 Auth

| # | Check | Pass |
|---|---|---|
| 5.3.1 | No token | rejected |
| 5.3.2 | Bad token | rejected |
| 5.3.3 | Valid token, sync path | accepted |
| 5.3.4 | Bulk **without** a per-caller key | rejected |
| 5.3.5 | Bulk **with** a per-caller key | accepted |
| 5.3.6 | `caller_id` (key-derived) is distinct from any opaque `caller_ref` | never conflated |

### 5.4 Live-HTTP verification (first Linux/production host only)

The §5/§6/§10 endpoint behavior above is proven locally via in-process handler tests (see the
header note). On the **first Linux/production host** — where a real server runs — repeat the
endpoint checks against a **live** control-plane and confirm they match the in-process results.
This closes what in-process tests cannot: port binding, real socket serialization, production
middleware ordering, and TLS. **Expectation:** this is almost certainly a Windows-only limitation
and will just work — but it is untested until here, so it is **verified at cutover, not assumed**.

```bash
# 1. Start the control-plane bound to a real port (with a DB + pepper + queue key + front-door token):
TM_ENV=production \
  TM_DATABASE_URL='postgres://…' TM_PEPPER='<32 bytes>' TM_FRONTDOOR_TOKEN='<token>' \
  TM_QUEUE_ADDR_KEY='<32 bytes, hex>' TM_LISTEN_ADDR=':8080' ./control-plane &

# 2. Health/readiness bind (proves the socket is really listening):
curl -fsS http://127.0.0.1:8080/healthz          # 200 — liveness; stays 200 with every dependency down
curl -fsS http://127.0.0.1:8080/readyz           # 200 only with db:true AND pepper:true AND queue:true

# 2b. The readiness check is a LIVE ping, not a boot-time snapshot — prove it by stopping the DB
#     container and re-probing without restarting the app. `-f` is wrong here (it exits non-zero on
#     the 503 we are asserting), so capture the code with -w:
docker stop <pg-container>
curl -sS -w ' <- %{http_code}\n' http://127.0.0.1:8080/readyz   # not_ready, db:false, 503
docker start <pg-container>
curl -sS -w ' <- %{http_code}\n' http://127.0.0.1:8080/readyz   # recovers to ready/200, no restart

# 3. Re-run the §5.1 verdict matrix against the live server, e.g.:
curl -fsS -H "authorization: Bearer $TM_FRONTDOOR_TOKEN" \
     -d '{"email":"not-an-email"}' http://127.0.0.1:8080/v1/verify   # invalid/invalid_syntax
# …repeat for each §5.1 row; confirm status/reason/recommendation match the in-process results.

# 4. §5.3 auth against the live server: no token → 401, bad token → 401, valid → 200; bulk
#    without a per-caller key → 401. (Use POST — /v1/verify is POST-only; a GET returns 405,
#    not 401. Do NOT use `-f` here: it makes curl exit non-zero on the very 401 we're asserting;
#    capture the status with -w instead.)
curl -sS -o /dev/null -w '%{http_code}\n' -X POST \
     -d '{"email":"a@b.com"}' http://127.0.0.1:8080/v1/verify                        # 401 (no token)
curl -sS -o /dev/null -w '%{http_code}\n' -X POST -H "authorization: Bearer wrong" \
     -d '{"email":"a@b.com"}' http://127.0.0.1:8080/v1/verify                        # 401 (bad token)

# 5. §6 row inspection: after the live calls, confirm the LOG rows, NULL-hash syntax reject,
#    from_cache on re-request, and redacted transcript are present (psql, as in §6).

# 6. §10 fork endpoints against the live node container (real socket, not RSpec):
docker run -d --name vn -e VERIFIER_EMAIL=… -e ACCESS_TOKENS=<tok> -p 3000:3000 verifier-node
curl -fsS http://127.0.0.1:3000/healthcheck                                          # stock, 200
curl -fsS -H "authorization: Bearer <tok>" http://127.0.0.1:3000/v1/audit            # host_audit shape
# …/v1/config, /v1/stats, /v1/catchall/:domain per §10.4–10.6.

# 7. If the deployment terminates TLS at the app (not a proxy), repeat step 2–4 over https://
#    and confirm the certificate is served and verification succeeds.
```

**Pass:** the live results match the in-process results for every §5.1 row, auth behaves per §5.3,
the §6 rows are correct, and the §10 fork endpoints respond per contract — over a real socket
(and TLS, if terminated at the app).

---

## 6. Database verification (after the smoke tests)

Inspect rows directly with psql.

| # | Check | Pass |
|---|---|---|
| 6.1 | Every request produced a LOG row | 1:1 |
| 6.2 | The syntax-reject row | hash is **NULL** |
| 6.3 | The cached re-request row | `from_cache = true` |
| 6.4 | `verification_raw` transcript | address is **redacted** — no plaintext email in the blob, including short local parts like `jo@x.com` |
| 6.5 | Any `550` body stored | address redacted there too |
| 6.6 | As the app role: `UPDATE verifications ...` | **denied** |
| 6.7 | As the app role: `DELETE FROM verifications ...` | **denied** |
| 6.8a | As the app role, on `node_audits` (append-only history, mirrors the LOG): `UPDATE` **and** `DELETE` | **both denied** |
| 6.8b | As the app role, on `node_warmup` (a **durable counter** — it must increment): `UPDATE` | **allowed** (the counter increments); only `DELETE` is denied — do **not** treat the allowed `UPDATE` as a defect |
| 6.9 | Same hash for the same address twice | identical `bytea` |
| 6.10 | Composed vs decomposed Unicode form of one address | **same hash** (the NFC fix) |
| 6.11 | Rejected/transient results | **not** cached |

---

## 7. The nine irreversible surfaces — fail-verification

**The most valuable testing in this document.**

Method for each: `cp file file.bak` → neuter the guard → run the named test →
**it must fail** → `cp file.bak file` → re-run → **green**.

| # | Surface | Neuter | Must fail |
|---|---|---|---|
| 7.1 | **Door 3 — hashing** | break normalization (case-fold or NFC) | golden-vector test |
| 7.2 | **Door 2 — append-only** | grant UPDATE/DELETE to the app role | immutability test |
| 7.3 | **GAP-24 — four-way IP invariant** | remove the egress-vs-audit-IP cross-check | simulated multi-homed box (egress ≠ audit IP) is wrongly accepted |
| 7.4 | **P4 fleet brake** | see 7.4.a–d below | each named test |
| 7.5 | **GAP-37 — CAS settlement** | remove the compare-and-set on lease holder | exactly-once test (two LOG rows appear) |
| 7.6 | **P6 — anti-laundering** | remove the confidence cap / mapping | `TestResolve_VendorValid_IsNeverValid` |
| 7.7 | **P6 — spend-once** | neuter the in-CAS external-call insert | double-charge appears under simulated reclaim |
| 7.8 | **P7 — classifier exclusion** | remove the zero-information exclusion | a `blocked` bounce enters a calibration numerator |
| 7.9 | **P7 — cascade brake** | see 7.9.a–d below | each named test |

### 7.4 / 7.9 — the brakes (deepest scrutiny)

This component has failed **three times** across two phases. Test all four
properties on **both** the P4 (pool) and P7 (calibration) side:

| Sub | Property | Pass |
|---|---|---|
| a | **Simultaneous** spike | brake fires → alert, **no** mass auto-quarantine / auto-apply |
| b | **Staggered / gradual** drift (one at a time, across cycles) | brake **still** fires — a single-cycle snapshot must not miss it |
| c | **Config range** — set the fleet-wide percentage out of range (e.g. ≥ 100) | **startup error**, never a silent disable |
| d | **Per-run at-max persistence** | the at-max state survives across runs; a restart doesn't reset the brake |
| e | **Canary recovery** | a quarantined node/account can return via metered trickle — quarantine is not one-way |

---

## 8. System-state tests

### 8.1 Fresh-system invariant (clean install, no data)

| # | Check | Pass |
|---|---|---|
| 8.1.1 | A1 block-rate quarantine mode | `recommend-only` |
| 8.1.2 | A6 recalibration mode | `recommend-only` |
| 8.1.3 | A8 provider promotion mode | `recommend-only` |
| 8.1.4 | A9 fallback weaning mode | `recommend-only` |
| 8.1.5 | A2 audit-red quarantine | `auto` (deterministic — correct) |
| 8.1.6 | External vendor layer | **disabled** |
| 8.1.7 | Confidence buckets applied / providers promoted / buckets weaned | **zero** each |
| 8.1.8 | Verdicts vs flywheel-off | **byte-identical** |
| 8.1.9 | Effective kill-switch rung | cheap-layers-only |

> A regression is **any** mechanism silently mutating live state at mode-default.

### 8.2 Kill-switch ladder

Pull each rung. The answer must get **humbler, never wrong**.

| Rung | Pull | Expected |
|---|---|---|
| 1 — SMTP off | disable node / route provider `:mx` / disable globally | would-be `valid` → `risky/provider_no_smtp`; **never `invalid`** |
| 2 — Fallback off | per-policy, per-vendor, or global kill | residue stays `unknown`/`risky`; **never fabricated** |
| 3 — Cheap-only | rungs 1 + 2 together | provable invalids keep full confidence; unprobed → `risky`/`unknown`; **never `valid`** |
| 4 — Paused | stop the drainer / reject at front door | job handle + paused status, **or** explicit unavailable; **never a fabricated verdict** |
| Global A | fallback-kill switch | **all** vendor egress stops in one action, regardless of per-policy config or breaker state |
| Global B | shadow-mode | mutators stop mutating; answering continues |

### 8.3 Vendor-layer enablement resistance

| # | Set | Pass |
|---|---|---|
| 8.3.1 | `TM_FALLBACK_MODE=enabled` only | **fails closed** — missing DPA confirmation + keys |
| 8.3.2 | Mode + DPA confirmed, no keys | fails closed |
| 8.3.3 | Any test/dev path | cannot flip it implicitly |

---

## 9. Queue tests (`TM_QUEUE_ADDR_KEY` set)

| # | Check | Pass |
|---|---|---|
| 9.1 | Submit bulk | returns a **job handle**, not inline results |
| 9.2 | Job progress | counts only — no per-row live feed from the brain |
| 9.3 | Job completion | judged by authoritative `NOT EXISTS(... NOT IN (settled,dead))`, **not** a counter |
| 9.4 | Plaintext at rest | queue row holds **ciphertext**, never plaintext |
| 9.5 | After settle | plaintext column is **NULL** — purged inside the settlement transaction |
| 9.6 | Kill the app mid-job, restart | resumes from `pending`; `done` items are not re-run or re-charged |
| 9.7 | Simulated slow-drainer reclaim | exactly **one** settlement; the losing attempt discards its result |
| 9.8 | Greylist `450` (by-attempt stub) | → requeue → after max attempts settles `unknown/greylisted`; **never `invalid`** |
| 9.9 | Retry routing | same-node retry affinity (greylisting is keyed on sending IP) |
| 9.10 | `rate_limited` — queued item | **waits and retries** |
| 9.11 | `rate_limited` — sync request | **degrades cheap** (unchanged behavior) |
| 9.12 | A queued check | passes **every** gate a sync check does — no bypass |
| 9.13 | Transient unknowns (greylist / your-fault / capacity) | **not cached** |
| 9.14 | Lease TTL vs max SMTP deadline | TTL is longer; app **fails fast** at startup if misconfigured |

---

## 10. Verifier-node / fork tests

| # | Check | Pass |
|---|---|---|
| 10.1 | Build and run the forked rack container | starts |
| 10.2 | Gem-unmodified assertion | passes — engine byte-identical to the pinned version |
| 10.3 | Stock endpoints | byte-identical to upstream |
| 10.4 | `/v1/config` | returns full effective config (feeds `config_fingerprint`) |
| 10.5 | `/audit` | returns the real `host_audit` shape |
| 10.6 | `/stats`, `/config/reload`, `/catchall/:domain` | respond per contract |
| 10.7 | Fork's RSpec suite | green |
| 10.8 | Against `smtp_mock` + `dns_mock` | socket-level tests pass |
| 10.9 | Catch-all `valid` path | **only** provable with a *seeded* probe rejection — the mock accepts everything by default |
| 10.10 | Catch-all probe implementation | two separate `validate` calls (Option B), gem **not** patched |

---

## 11. Routing, eligibility & audit gate (simulated nodes)

| # | Check | Pass |
|---|---|---|
| 11.1 | Node with red audit | **hard-blocked** from `:smtp` — not a soft warning |
| 11.2 | Node audit-green but port-25 blocked | **not eligible** (audit ≠ egress proof) |
| 11.3 | Node whose egress IP ≠ audit IP | rejected (GAP-24) |
| 11.4 | Stale audit (past freshness window) | not eligible |
| 11.5 | **No** eligible node | cheap verdict, `verifier_ip = null`, **never `invalid`** |
| 11.6 | Real `:smtp` verdict | logged with `verifier_ip` tagged |
| 11.7 | Sticky-by-domain routing | one provider's checks concentrate on one node (anti-multiplication) |
| 11.8 | Node removed from the pool | minimal reshuffle — only that node's domains move (HRW) |
| 11.9 | Least-inflight fallback | engages when the sticky node is saturated/quarantined |
| 11.10 | Add node #2 | **config only** — no code change, Strategy interface untouched |
| 11.11 | Warmup cap exceeded | degrades cheap, **non-cacheable**; never queues, never `invalid` |
| 11.12 | Rate-limiter state after restart | **does not refill** — durable, no reputation hazard |
| 11.13 | In-flight counters after crash | correct-at-zero (deliberately ephemeral) |
| 11.14 | Block-rate calculation | **excludes** `from_cache` rows and cheap-degrades |
| 11.15 | Routing-map drift (policy vs engine backstop) | **logged alarm**, not silent auto-fix |

---

## 12. Vendor layer (disabled, mock vendor)

| # | Check | Pass |
|---|---|---|
| 12.1 | Residue classes that escalate | exactly the closed set: retry-exhausted `unknown/greylisted`, `catch_all`, `provider_no_smtp`, gave-up-capacity |
| 12.2 | Hard-invalids, SMTP-valids, cache hits, disposables, whole lists | **never** escalate |
| 12.3 | Mock vendor returns "valid" | → `risky/external_verified`, capped confidence — **never `valid`** |
| 12.4 | Vendor "invalid" on retry-exhausted residue | accepted as `invalid` |
| 12.5 | Vendor "invalid" on catch-all / `:mx` residue | **downgraded to `risky`** |
| 12.6 | Cache re-read of an escalated verdict | **does not re-bill** (escalation decided once, at mint time) |
| 12.7 | Budget cap reached | breaker trips → `keep_residue` + alert |
| 12.8 | Per-job spend cap | enforced on **every** path reaching the vendor, including inline escalation of queued bulk items |
| 12.9 | Vendor down / error / timeout | `keep_residue`; answer stays `unknown`/`risky` |
| 12.10 | Global fallback-kill | stops all egress in one action |
| 12.11 | Vendor payload | contains an **address only** — never user, credit, job, or file identity |

---

## 13. Flywheel (simulated outcome source)

| # | Check | Pass |
|---|---|---|
| 13.1 | `mailbox_not_found` bounce | → invalid-signal (gold) |
| 13.2 | `mailbox_full` bounce | → **valid-signal** (a non-existent mailbox can't be full) |
| 13.3 | `blocked` / `policy` / `reputation` / `spam_reject` / greylist | → **zero-information, excluded** — never enters a numerator |
| 13.4 | Bounce text containing a code-like substring (e.g. quoted original) | classified by **structural DSN parse**, not substring match |
| 13.5 | Delivered, no engagement | weak evidence |
| 13.6 | Delivered on a catch-all domain | weight ≈ 0 |
| 13.7 | `from_cache` rows | excluded from calibration |
| 13.8 | `external_verified` (rented) verdicts | bucketed **separately** from SMTP-earned |
| 13.9 | Sample count below gate | recalibration **blocked** |
| 13.10 | Large proposed swing | bounded step applied |
| 13.11 | Confidence direction | down-fast, up-slow (asymmetric hysteresis) |
| 13.12 | Replayed outcome | **does not double-count** (idempotent ingestion) |
| 13.13 | Historical-bounce gate | fires only on confirmed `mailbox_not_found`, needs ≥N independent outcomes |
| 13.14 | Historical-bounce TTL expiry | degrades to a confidence penalty — a re-created mailbox recovers |
| 13.15 | Accuracy reporting | **refuses** to publish a global number without its coverage |
| 13.16 | Empty system | falls back to baseline confidence; promotion/weaning start at zero |
| 13.17 | `unknown` verdicts | never retroactively pressured toward `valid` to improve metrics |

---

## 14. Security

| # | Check | Pass |
|---|---|---|
| 14.1 | `vulnerability-scanner` over the repo | no findings |
| 14.2 | Tracked secrets | none |
| 14.3 | Pepper | loaded from secure config, never logged, never in a response |
| 14.4 | AES key | genuinely separate from the pepper |
| 14.5 | Queue ciphertext | AAD-bound to the row id; a swapped row fails to decrypt |
| 14.6 | Key missing/wrong | **fails closed** — no plaintext fallback |
| 14.7 | Control plane / node | not publicly reachable; token required |
| 14.8 | Error responses | leak no internal mechanics, IPs, or SMTP codes |
| 14.9 | Logs | contain no plaintext addresses outside the queue's bounded window |

---

## 15. Not testable locally

These need real-world inputs. The machinery around each is already
fail-verified against simulated inputs — what's untested is the real *values*.

| Item | Needs | Verification command |
|---|---|---|
| Real SMTP verdicts | host with outbound port 25 + PTR | in the go-live runbook |
| PTR / rDNS propagation | a real IP | runbook |
| Greylist reject-then-accept round-trip | a real greylisting provider (structurally un-mockable) | runbook |
| Vendor accuracy on residue | vendor account + DPA | runbook |
| Calibration accuracy | a real outcome feed | runbook |
| mTLS outcome-feed handshake | a real outcome source | runbook |

---

## 16. Sign-off checklist

- [ ] Build + vet clean
- [ ] Migrations apply from empty
- [ ] `go test ./...` green, and green with `-shuffle=on`
- [ ] PG-gated integration suite green
- [ ] Full gate green (incl. Door-5, append-only grants, config-freshness, docs-updated)
- [ ] Fresh clone builds, migrates, and passes the gate
- [ ] API verdict matrix matches §5.1
- [ ] LOG rows correct; UPDATE/DELETE denied; transcript redacted
- [ ] **All nine irreversible surfaces fail-verified** (§7)
- [ ] Both brakes pass all five sub-properties (§7.4 / §7.9)
- [ ] Fresh-system invariant holds (§8.1)
- [ ] Kill-switch ladder degrades honestly at every rung (§8.2)
- [ ] Vendor layer resists accidental enablement (§8.3)
- [ ] Queue: exactly-once, purge-on-settle, resume-after-crash (§9)
- [ ] Fork runs; gem byte-identical; catch-all is Option B (§10)
- [ ] Audit gate hard-blocks; GAP-24 enforced; node #2 is config-only (§11)
- [ ] Vendor residue set closed; anti-laundering holds; spend capped on all paths (§12)
- [ ] Classifier exclusions hold; no accuracy claim without coverage (§13)
- [ ] Security sweep clean (§14)

---

## The one rule worth repeating

Run §7 yourself at least once rather than trusting a report. The gap between
*"the tests pass"* and *"the tests would catch it if the guard disappeared"* is
where every serious defect in this project was found — the hashing split, the
IP invariant, the brake (three times), the CAS, anti-laundering, and the
classifier poisoning bug. All of them passed normal tests.
