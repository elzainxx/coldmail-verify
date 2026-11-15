# Future work — everything not done yet, and why

> **OWNER: MODULE 1 (`coldmail-verify`, the Truemail control-plane / verifier-node pair).** "The arc" below means module 1's own phases 0–7 — nothing here concerns module 2 (`coldmail-send-managed`) or module 3 (`coldmail-send-byo`), which keep their own `Docs/future-work.md`. Module 1's *rulings of record* are in [`open-decisions.md`](open-decisions.md) (zero open items); this file is the register of everything still to do: §1–3.5 work that is built and fail-verified but honestly blocked on a real-world input; §4 work inside this repo that is simply not built yet; §5 the four sub-decisions that still need an owner; §6 what has closed since the register was first written.

**Verified:** 2026-11-13 against the code, migrations 0001–0015 and CI run [43445703791](https://github.com/elzainxx/coldmail-verify/actions/runs/43445703791) (all 11 jobs green on `de8b2a5`). **Status of §1–3.5:** the single register of everything that is BUILT + fail-verified in logic but honestly deferred until a real-world input exists. **Every row was flagged PENDING at build time and NONE was ever faked green** (each phase's status doc carries the same "never faked green — the honest ledger" framing; grep-confirmed across `Docs/`, §8 below).

**How to read a row:** *Item* = the deferred thing · *Built + fail-verified now* = the logic that exists and is tested locally · *Why deferred* = the real-world dependency · *Verification command / proof* = the exact thing to run when the dependency lands · *Unblocked by* = the real-world event that makes the proof runnable.

**The through-line:** exactly one behavior per phase is genuinely un-mockable pre-integration, and they chain — the greylist round-trip (P2/P3/P5, GAP-35) → vendor accuracy (P6, GAP-46/47) → real send-outcome accuracy + live authentication (P7, GAP-48). Each resolves only at real-box / real-vendor / real-sender rollout. This ledger keeps them in one place so the machine's honesty survives the arc.

---

## 1. PENDING-REAL-BOX (Phase 3, carried through P4/P5) — the SMTP verifier-node

The pool/queue/audit LOGIC is built and tested against simulated nodes + a controllable LOG; only real-IP *reputation behavior* and the *proof values* are deferred. Source: [`history/phase-3-status.md`](history/phase-3-status.md), [`history/phase-3-cutover-runbook.md`](history/phase-3-cutover-runbook.md), [`history/phase-4-status.md`](history/phase-4-status.md), [`history/phase-5-status.md`](history/phase-5-status.md).

| # | Item | Built + fail-verified now | Why deferred | Verification command / proof | Unblocked by |
|---|---|---|---|---|---|
| B1 | **Outbound port-25 works (GAP-15)** | `port25_open` eligibility input + the egress-smoke-test capability shape; `false` in the local build → no node eligible | a host that genuinely allows outbound :25 is a narrow market | on the trial box: `nc -vz <external-mx> 25` (or `swaks --server <mx>:25 --quit-after CONNECT`) establishes + sees a `220` banner → then set the node's `port25_open=true` | a real host with :25 open |
| B2 | **Real PTR / rDNS** | the `/audit` gate reads `warnings` (green iff `{}`); proven vs `dns_mock` + the sim surface | needs a provider that lets you set PTR | `dig -x <public-egress-ip> +short` == verifier FQDN; then `GET /v1/audit` → `warnings:{}` | a host with settable PTR |
| B3 | **Egress IP == audited IP == PTR (GAP-24, four-way)** | the four-way cross-check (eligibility gate + re-audit scheduler), fail-verified (re-verified this closeout) | requires the real box's real egress identity | all four identical: `curl -s ifconfig.me` == `dig +short <verifier-fqdn>` == `dig -x <ip>` PTR == `host_audit.current_host_ip` == `TM_NODE_PUBLIC_EGRESS_IP` | the real box |
| B4 | **Clean IP (no spam history)** | procurement gate — the audit does NOT check blocklists (GAP-17 sibling) | out-of-band reputation check | Spamhaus/Barracuda/passive check on `<box-ip>` **before** warmup | procurement |
| B5 | **SPF published (GAP-17)** | out-of-band checklist item (`host_audit` does NOT check it) | DNS the operator controls | `dig TXT <verifier-domain>` shows an SPF authorizing the egress IP | DNS config |
| B6 | **Real greylist reject-then-accept round-trip (GAP-35)** | the retry loop mechanics (requeue, ceiling→settle, hint-seeded backoff, idempotency) on a by-attempt-count stub (P5); `greylisted→unknown`+retry-eligible (P2/P3) | the `smtp_mock` cannot do a stateful 450-then-250; **un-mockable** | **rollout observation only** — a REAL greylisting provider; no runnable local command (honest — the single un-mockable P5 behavior) | a real greylisting MX at rollout |
| B7 | **2-box reputation isolation in the wild** | per-`verifier_ip` isolation proven on seeded LOG rows; node #2 = +1 config line proven (`TestLoad_NodeTwoIsConfigOnly_GAP31`) | needs two real IPs | one dirty real IP vs one clean real IP (P4 acceptance criterion 6) | two real boxes |
| B8 | **DR-4 backup/PITR restore drill** | the at-rest ciphertext + transactional purge, proven locally (dump-the-row → ciphertext only) | a real snapshot restore-into-isolation | a real backup/PITR restore-into-isolation drill at go-live (ADR-0002 compliance test) | go-live infra |

> **B8 got more load-bearing on 2026-08-07, so it should stop being the easy deferral.** The schema is
> forward-only with **no** down-migration mechanism (measured 2026-08-07, re-measured 2026-11-13 over 15 migrations: 0 `.down.sql`, no version-tracking
> table — [FW-3](#4-not-built-yet-inside-this-repo)), and the LOG tables are append-only with `cp_runtime` holding no `UPDATE`/`DELETE` on them by
> construction. So for the LOG, **restore IS the only recovery path there is** — B8 is not one recovery
> option among several, it is the whole set. Detail and the go-live checklist:
> [`operator-runbook.md`](operator-runbook.md) §9 (the original 2026-08-07 write-up is kept in
> [`history/go-live-rollback-posture.md`](history/go-live-rollback-posture.md)).

**The one wiring gap (flagged loudly, not silently deferred):** `port25_open` can currently only be *set false* (boot registration); there is **no operator/env/admin command to flip it true** in a running system (no `TM_NODE_PORT25_OPEN`). Interim is a `cp_admin` break-glass `UPDATE nodes SET port25_open=true WHERE id=…`; its permanent home is the **DR-3 admin-audit-log egress-test-writer** (build tasks FW-2 and FW-1 in §4). Recorded in [`go-live-checklist.md`](go-live-checklist.md) G0.2 and G1.2 — the highest-value parallel build.

---

## 2. PENDING-REAL-VENDOR (Phase 6) — the third-party fallback

The external layer is BUILT, DISABLED, and fail-verified against a controllable **mock vendor**; the two irreversible surfaces (anti-laundering, spend-once) are re-fail-verified this closeout. Only real-vendor *accuracy* + the *legal/operational enablement* are deferred. Source: [`history/phase-6-status.md`](history/phase-6-status.md).

| # | Item | Built + fail-verified now | Why deferred | Verification command / proof | Unblocked by |
|---|---|---|---|---|---|
| V1 | **Vendor accuracy on the residue (GAP-46/47)** | the residue-only routing, anti-laundering (`Resolve`), spend-once (in-CAS), breaker — all fail-verified vs the mock | catch-all/big-provider is exactly where vendors are weakest; **un-mockable pre-integration** (like GAP-35) | live integration + P7 measurement: `SELECT residue_class, vendor, vendor_status_raw, <bounced?> FROM external_calls JOIN outcomes …` — measured, never claimed | a real vendor + the P7 outcome feed |
| V2 | **Real vendor HTTP adapter** | the swappable `external.Vendor` interface + the mock prove the mechanics; `buildExternal` returns nil (fail-closed) | no real vendor enabled this phase (by design) | implement a real `external.Vendor` adapter; wire it in `buildExternal`; one adapter + the §7.4 checklist from live | a vendor selection |
| V3 | **The §7.4 DPA / due-diligence gate** | the three-switch enablement guard is built + tested (any missing ⇒ off; global-kill overrides) | owner/legal action; **enabling pre-DPA = unlawful sub-processing** | DPA signed + retention/deletion recorded + TLS + key custody → set `TM_EXTERNAL_DPA_CONFIRMED=true` + `TM_FALLBACK_MODE=enabled` + a `TM_VENDOR_KEYS` entry | a signed DPA (owner/legal) |
| V4 | **Budget ceiling values (OQ-P6-5)** | the budget/breaker consume the dials; UNSET = not-enforced (never unbounded — the layer only egresses when enabled) | owner-OQ: "start conservative, raise from observed residue-tail volume, never loosen blind" | set `TM_EXTERNAL_DAILY_CAP` / `_MONTHLY_CAP` / `_PER_JOB_CAP` / `_RATE_CEILING` from observed §9 volume | observed volume |
| V5 | **PN-7/F-5 standing controls** | `external_calls` is queryable by `email_hash` for SAR/deletion (PN-10) | cumulative-egressed metric + periodic vendor-deletion are operational, not built here | operational: exercise the DPA deletion right on a cadence (the exposure grows unbounded) | ongoing operations |

**Claim discipline (GAP-47):** coverage is bounded and honest, **not** accurate. We can prove we only spend on the tail and only rent on the residue; we cannot prove the vendor is *right* until P7 measures it against real bounces. "We have fallback coverage now" must never harden into "we're accurate on Gmail now."

---

## 3. PENDING-REAL-SENDER (Phase 7) — the outcome feed / the flywheel

The whole flywheel is BUILT and fail-verified against a `SimulatedSource`; the two irreversible surfaces (classifier exclusion, recalibration CAS) are re-fail-verified this closeout. Only real *accuracy*, live *authentication*, and any mutator *actually firing* are deferred — the sender ("Feature 2") does not exist yet (GAP-48). Source: [`history/phase-7-status.md`](history/phase-7-status.md), [ADR-0013](adr/ADR-0013-outcome-feed-auth-mechanism-and-tb7-threat-model.md).

| # | Item | Built + fail-verified now | Why deferred | Verification command / proof | Unblocked by |
|---|---|---|---|---|---|
| S1 | **Real accuracy numbers (§8)** | the classifier, recalibration + 4 guards, promotion, accuracy-with-coverage, weaning — all tested vs a simulated corpus | measurable only against real send→bounce outcomes; a simulated corpus tests the MACHINE, never the ACCURACY | live integration + `AccuracyByProvider` over real outcomes, reported WITH coverage (never a bare global number) | the real sender emitting outcomes |
| S2 | **A real mTLS transport client** | the swappable `OutcomeSource` interface + `SimulatedSource` prove the mechanics; `PullSource` is the fail-closed skeleton (`Authenticated()`=false with no client) | the sender must implement its half of the mTLS feed (cross-project, GAP-48) | wire a real `MTLSClient` into `PullSource`; `Authenticated()` flips true only then (fail-closed until) | the sender project |
| S3 | **The agreed `send_outcome` contract** | P7 §3.2 LOCKS the shape P7 consumes; the ingestion path is built against it | the SUPPLY is the sender's commitment | the sender emits the locked shape into its authenticated feed | cross-project agreement |
| S4 | **DSN-fidelity-bounded classification quality** | the classifier is correct by construction (structural DSN parse, fail-verified); `send_outcomes.raw` retained for re-audit | real-world accuracy depends on the sender's raw DSNs — un-mockable pre-integration | measured against real DSNs; `NormalizeEvent` re-audited against retained raw events | real DSNs |
| S5 | **Any promotion/weaning actually firing** | `EvaluatePromotion`/`EvaluateWeaning` are sample-gated, reversible, recommend-only; on a fresh system both return "hold" | volume-gated per provider (§7.4/§9.4); day one = ZERO promoted, ZERO weaned | over real accumulated accuracy — the moat is years of running it, not a ship-time feature | volume over time |

---

## 3.5 PENDING-LIVE-HTTP — the endpoint tiers run in-process locally, verified against a live server at cutover

This is **not** an un-mockable-behavior gap like §1–§3 (those wait on a real counterparty's reputation). It is a **dev-host limitation**: on the Windows dev host a long-lived HTTP server could not be run/killed reliably, so the endpoint tiers of the full test plan (history/TEST-PLAN.md §5/§6/§10) were proven by the projects' own **in-process handler tests** — `httptest` for the Go API, RSpec request specs for the fork — which exercise the *identical* handler code. That is the correct local substitute, but by construction it does not cover port binding, real socket serialization, production middleware ordering, or TLS. Those are verified against a **live server on the first Linux/production host**.

| # | Item | Built + verified now | Why deferred | Verification command / proof | Unblocked by |
|---|---|---|---|---|---|
| H1 | **Live-HTTP endpoint parity (§5/§6/§10)** | every endpoint's behavior is proven in-process — Go `httptest` handler tests (verdict matrix, contract, auth) + the fork's RSpec request specs (71 examples, 0 failures) exercise the same handlers | the dev host cannot run/kill a live server reliably (Windows-specific; the Go binary and Ruby container both boot but don't serve dependably) | on the first Linux host, run history/TEST-PLAN.md **§5.4** (summarised in [`install-and-deploy.md`](install-and-deploy.md) §4) (start the server on a real port; re-run the §5.1 matrix, §5.3 auth, §6 rows, and the §10 fork endpoints over a real socket) and confirm they **match** the in-process results | the first Linux/production host |
| H2 | **TLS termination at the app (if used)** | TLS is never disabled anywhere (no `InsecureSkipVerify`, vuln-scan confirmed); not exercised locally | no live server ran locally; TLS may also terminate at a proxy (deployment choice) | if TLS terminates at the app, repeat §5.4 over `https://` and confirm the certificate is served + verification succeeds | the production TLS decision |

**Expectation, stated plainly:** this is almost certainly a Windows-only limitation and will **likely just work** on Linux — the handler code is identical and already green in-process. But it is **untested until cutover, so it is verified there, not assumed.** Recorded in [`go-live-checklist.md`](go-live-checklist.md) Stage 1 (a code/config step, not a real-world-input gate).

---

## 4. Not built yet, inside this repo

Everything in §1–3.5 waits on something *outside* this repo. These five do not: nothing external is missing, the work is simply unwritten. Each was checked absent on 2026-11-13 (the command is in the last column). They are build tasks, not decisions; none changes a door.

| ID | Task | What it blocks / why it matters | Fix shape | Verified absent by |
|---|---|---|---|---|
| **FW-1** | **DR-3 admin-audit log** — an append-only table with per-actor identity (who / what / when) that every manual control-plane action writes: node disable, global-fallback-kill, pause, shadow-mode promotion, budget change, breaker/brake confirm, privacy opt-out flip. Distinct from the verdict LOG. | Gates **go-live** ([`open-decisions.md`](open-decisions.md) §2, DR-3). Until it exists, "promote to `auto` is an admin-audited action" and "the fallback-kill is audited" are not true in code. | Reuse the `node_audits` / `work_item_attempts` append-only pattern (migrations 0007, 0009): table + `cp_runtime` INSERT/SELECT only + a typed writer every mutator calls. FW-2 is its first slice. | `grep -rniE "admin_audit\|AdminAudit" control-plane` → no hits; no migration 0001–0015 creates such a table. |
| **FW-2** | **An operator command that sets `port25_open=true`.** `port25_open` is only ever *written false* (boot registration); there is no `TM_NODE_PORT25_OPEN`, no admin endpoint and no egress-test writer. | Without it a node can never become `:smtp`-eligible without an out-of-band DB write (the gate fails closed — nothing is faked). | The audited egress-test writer under FW-1 (smoke-test port 25 from the node, record the result, set the flag). Interim: as `cp_admin`, `UPDATE nodes SET port25_open=true WHERE id='…'`. | `grep -rn PORT25_OPEN control-plane` → no hits (no env var); `port25_open` is only read back unchanged by the re-audit scheduler (`internal/audit/scheduler.go`). |
| **FW-3** | **Migration version tracking.** Migrations are applied with a shell loop; nothing records which a database has run (no `schema_migrations`), and there are no down-migrations (0 of 15). | Cheap before a second environment exists, archaeology afterwards ([`operator-runbook.md`](operator-runbook.md) §9). | A tracking table plus an apply script that skips what is recorded, or an off-the-shelf tool; keep forward-only. | `ls control-plane/migrations` (no `.down.sql`); `grep -rn schema_migrations control-plane` finds only the README's recommendation. |
| **FW-4** | **Package the control-plane.** There is no Dockerfile or compose file for it (`control-plane/` holds `cmd config go.mod go.sum internal migrations test`); the verifier-node has both. | Deployment is `go build` + env + the migration loop today. Not a go-live blocker for a single host, but nothing builds a control-plane image. | A multi-stage Dockerfile pinned by digest; extend `check-base-image-pinned.sh` to cover it so it cannot be cache-only. | `ls control-plane`. |
| **FW-5** | **`TM_SUPABASE_KEY` has no consumer.** `secrets.Load` requires it when `TM_ENV=production` and `/readyz` reports it as `supabaseKey`, but nothing reads its value — the database connection uses `TM_DATABASE_URL`. | An operator on plain Postgres must set a meaningless secret to start in production. | Retire it (keep accepting it — additive only) or give it a real consumer. [`install-and-deploy.md`](install-and-deploy.md) §3.1 tells the operator what to do meanwhile. | `grep -rn "SupabaseKey()\|supabaseKey" control-plane --include=*.go` → only the presence check. |

The two other in-repo seams are in §2 and §3 because they wait on an external party: the real vendor adapter (**V2**, needs a selected vendor) and the real mTLS client (**S2**, needs the sender).

---

## 5. PARKED — sub-decisions that still need an owner

Moved here from [`open-decisions.md`](open-decisions.md) §5 on 2026-11-09. Each needs an owner call, and **no recommendation is recorded anywhere** — so none is ruled here. For each, the built behaviour is safe, which is why none blocks go-live.

| ID | The question | What is built today | Source |
|---|---|---|---|
| **FW-6** (PN-15 / FLAG-A) | `signals.platform` is a reserved response key that no phase populates. Assign it to a phase, or declare it Phase-8+? | The key stays reserved and absent; the open `signals` bag tolerates absence (Door 1). | `history/cross-phase-consistency-report.md` FLAG-A; P1 §3.1.3 note. |
| **FW-7** (PN-15 / FLAG-C) | Who owns the loop that re-probes a catch-all verdict when `catchall_domains.expires_at` passes — the P4 health-watcher, the P5 queue, or Phase-8+? | `expires_at` is written (`store/postgres.go`); nothing re-probes on it (`orchestrator/smtp_stage.go`, the FLAG-C comment). | `history/cross-phase-consistency-report.md` FLAG-C; P2 §5.6 note. |
| **FW-8** (PN-8 / F-12) | When a brake (fleet block-spike, or the recalibration cascade) is holding for a human, should it ever time out — and if so, stay held or auto-release? Should the human confirm be an authenticated, audited action (depends on FW-1)? | **Stay-held:** both brakes alert and hold; there is no timeout and nothing auto-releases (`flywheel/guards.go`, `health/decide.go`). | P4 §5.5 and P7 §6.4 notes. |
| **FW-9** (PN-9 / F-13) | Add an external tamper-evidence checkpoint (periodic hash-chain of `verifications` to write-once storage), or accept grant-based immutability with a documented residual? | Grant-based immutability: the runtime role cannot update or delete the LOG; the residual — a `cp_admin` holder can — is written down in [`security-posture.md`](security-posture.md) §3. | P1 §3.2.1 note; checklist §1.2 in `history/pre-takeoff-checklist.md`. |

The disclosure decisions OQ-P6-6 and OQ-P7-8 are *ruled* (disclose + opt-out; disclose-don't-hide) — what is left is the platform's own wording, tracked in [`go-live-checklist.md`](go-live-checklist.md) G2.6 and G3.5, not here.

---

## 6. Closed since this register was written (2026-07-19 → 2026-11-09)

Rows that used to be open here or in the go-live path, with the evidence that closed them. CI: every job green on `4b73326`, run [42633652063](https://github.com/elzainxx/coldmail-verify/actions/runs/42633652063).

| Was open | Closed by | Evidence |
|---|---|---|
| The project's own LICENSE (deferred since Phase 0) | Proprietary / all-rights-reserved, 2026-07-19 | [`LICENSE`](../LICENSE), [`NOTICE`](../NOTICE); `open-decisions.md` §6.1. |
| **Ruby 3.2 end-of-life** (2026-04-01) | Node moved to Ruby 3.4.12, truemail 3.3.1 unchanged | [ADR-0018](adr/ADR-0018-verifier-node-base-image.md) addendum 2; FORK.md row 18; `check-base-image-pinned.sh` and `check-gemfile-lock.sh` pass on 3.4.12; RSpec 180/0 and the socket-level proof 11/11 per FORK.md. |
| **The SMTP probe's destination was unfiltered** (SSRF) | Pre-flight guard + post-check + egress firewall file; brain returns `risky` / `non_public_mx` | [ADR-0021](adr/ADR-0021-smtp-probe-destination-guard.md); migration 0015; `check-probe-guard.sh` + selftest. Installing the firewall on the real box is the real-world step G1.5. |
| A-only domains reported `invalid / no_mx` | Implicit MX (RFC 5321 §5.1) | [ADR-0020](adr/ADR-0020-implicit-mx-rfc5321.md); `check-implicit-mx.sh` + selftest. |
| A dirty resolver on the node let it resolve another domain's MX | Resolver hygiene turns the host audit red | [ADR-0019](adr/ADR-0019-dns-names-are-rooted-and-failure-is-unknown.md) addendum; `check-resolver-hygiene.sh` + selftest. Giving the real box a short hostname is the real-world step G1.4. |
| A DNS failure of ours was reported as `invalid` | Rooted names; failure is `unknown` | ADR-0019; `check-dns-search-domain.sh` + selftest. |
| `verification_raw` ran out of partitions on 2026-10-01 | Maintenance function + loop | [ADR-0017](adr/ADR-0017-raw-partition-maintenance.md); migration 0014. |
| The node image could not be built from scratch; transitive gems floated | Digest-pinned base, `--no-cache` CI builds, committed `Gemfile.lock` | ADR-0018 and its first addendum; `check-base-image-pinned.sh`, `check-gemfile-lock.sh`. |
| Kill-switch Rung 4 was validated but never enforced | `TM_FRONTDOOR_MODE` moves both halves | [ADR-0015](adr/ADR-0015-rung4-pause-enforcement.md). |
| A credential could reach a log line | Redaction + a gate that boots the real binary | [ADR-0016](adr/ADR-0016-no-credential-in-a-log-line.md); `check-no-secret-leak.sh`. |
| The parked sub-decisions in `open-decisions.md` | Five closed with evidence, four moved to §5 above | `open-decisions.md` §5. |

---

## 7. Cross-arc: the un-mockable chain (why none of the above could be faked)

Each phase has exactly one behavior that **cannot** be proven locally, and they are the same shape — a real counterparty's reputation/behavior:

1. **P2/P3 greylist round-trip (GAP-35)** — a real MX's stateful 450-then-250. `smtp_mock` can't reproduce reputation.
2. **P5 greylist reject-then-accept** — the same, under the durable retry loop.
3. **P6 vendor accuracy (GAP-46/47)** — "like P2's un-mockable greylist"; the vendor is weakest exactly on the residue we route.
4. **P7 real accuracy + live authentication (GAP-48)** — the sender must exist and emit authenticated outcomes.

None carries a runnable local command; each is honestly rollout-only. This is by design — the machine is verified; the accuracy is earned.

---

## 8. "Never faked green" — grep-confirmed

Every `PENDING-REAL-*` marker sits inside an "honest ledger / never faked green" framing. Occurrence counts across `Docs/`, re-measured 2026-11-13, **excluding this file, `go-live-checklist.md` and `history/arc-closing.md`** (counting them would be circular):
- **`PENDING-REAL-BOX`** — 26 occurrences across 11 files; anchor: `history/phase-3-status.md:7` ("every real-IP-dependent check is honestly marked PENDING-REAL-BOX with its verification command — never faked green").
- **`PENDING-REAL-VENDOR`** — 13 occurrences across 8 files; anchor: `history/phase-6-status.md:103` ("never faked green — the honest ledger").
- **`PENDING-REAL-SENDER`** — 16 occurrences across 8 files; anchor: `history/phase-7-status.md:70` ("PENDING-REAL-SENDER / rollout (never faked green — the honest ledger)").

**Confirmed: no PENDING item was ever asserted as passing.** Each is built, wired, logic-tested, and marked deferred with (where a command exists) the exact proof to run at rollout — or, where the behavior is un-mockable, an honest "rollout observation only."

---

## 9. Chunk-4 disposition (2026-08-07) — all 20 rows are GENUINELY UNCLOSABLE here, and that is a claim about the rows, not about effort

The Chunk-4 close required emptying every open-decision ledger in all four repos — rule it, close it,
or move it to future-work — and listing anything genuinely unclosable. **This ledger is the honest
"unclosable" list. All twenty rows stay open, and none is a deferral of a decision.**

**The test applied to every row, one at a time.** A row may stay only if its blocker is a *real-world
input that does not exist yet* — not a design question, not unfinished work, not a judgement call
someone has been avoiding. A row that could be closed by *deciding* something must be decided now; a
row that could be closed by *building* something belongs in a future-work register with a fix shape.
**Every one of these twenty fails to be closable for the first reason.** Grouped by what is actually
missing:

| What is missing | Rows | Count |
|---|---|---|
| **A physical machine with a specific network identity** — outbound :25, settable PTR, a known-clean IP, a second IP | B1, B2, B3, B4, B5, B7 | 6 |
| **A counterparty whose behaviour cannot be simulated** — a real greylisting MX, a real vendor's accuracy on the residue, real DSN fidelity | B6, V1, S4 | 3 |
| **A commercial or legal act** — a signed DPA, a vendor selection | V2, V3 | 2 |
| **Another system that does not exist yet** — the sender emitting authenticated outcomes, and the contract it commits to | S2, S3 | 2 |
| **Accumulated time or observed volume** — you cannot fast-forward a reputation or a calibration corpus | S1, S5, V4, V5 | 4 |
| **Infrastructure that only exists at go-live** — a real snapshot to restore; a Linux host to bind a real port | B8, H1, H2 | 3 |

**Twenty rows, twenty blockers, zero of them a decision.** That distribution is itself the finding: the
un-mockable surface of this system is almost entirely *reputation, counterparties, and elapsed time* —
the three things a test harness structurally cannot manufacture. §7's chain is the same observation
stated per phase.

**What was checked, so this is a verification and not a re-assertion.** Each row was read for the three
disqualifying shapes rather than taken on trust:

1. **Is it actually a disguised open decision?** Two came close and both were checked and cleared.
   `V4` (budget ceiling *values*) reads like an unmade decision, but the mechanism consumes the dials
   and UNSET means not-enforced — the *policy* is ruled ("start conservative, raise from observed
   volume, never loosen blind"); only the numbers wait on volume that does not exist. `V5` is
   explicitly labelled operational-not-built, which is a correct disposition, not a pending one.
2. **Is it a build task hiding in a deferral?** One is, and it is already flagged loudly rather than
   quietly parked: the `port25_open` write path (§1's note) has **no** operator/env/admin command to
   flip it true, with a `cp_admin` break-glass `UPDATE` as the interim and the DR-3 admin-audit
   egress-test-writer as its permanent home. That is real work with a known shape, recorded in
   `go-live-checklist.md` G0.2 and G1.2 (build tasks FW-1 and FW-2 in §4) — **it is not one of the twenty**, and it is named
   here again so the distinction stays visible: the *reputation behaviour* is unclosable, the *plumbing
   to record it* is merely unbuilt.
3. **Does the row still describe the tree?** `B8` did not, and was corrected on 2026-08-07 — see the
   blockquote under §1. It read as one recovery option among several; measurement (0 `.down.sql`, no
   version-tracking table, `cp_runtime` holding no `UPDATE`/`DELETE` on the LOG) showed restore is the
   *only* recovery path the LOG has. **A row that understates its own load-bearing weight is the same
   failure class as a stale assertion:** it stays technically true while quietly ceasing to mean what a
   reader takes it to mean.

**What this section does not claim.** It does not claim the twenty are un-closable *forever* — each has
a named unblocking event and (where one can exist) an exact command to run when it lands. It does not
claim they are low-risk: `V1`, `S1`, and `S4` are precisely where this system's *accuracy* will first be
measured against reality, and §2's claim discipline exists because "we have coverage" must never harden
into "we are accurate." And it does not claim the *count* is stable — a new phase can add a
twenty-first. What it claims is narrower and checkable: **as of 2026-08-07 nothing in this ledger is
waiting on a decision, a judgement, or a piece of unwritten code inside this repo.** Re-checked 2026-11-13: still true of these twenty rows. The unwritten in-repo code that does exist is §4 (FW-1 … FW-5) and the decisions that remain are §5 (FW-6 … FW-9) — deliberately outside the twenty.

---

*Companion docs: the ordered go-live path is [`go-live-checklist.md`](go-live-checklist.md); the arc's honest close is [`history/arc-closing.md`](history/arc-closing.md).*
