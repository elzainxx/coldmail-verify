# The consolidated PENDING ledger — every real-world-gated item across the arc

> **OWNER: MODULE 1 (`coldmail-verify`, the Truemail control-plane / verifier-node pair).** "The arc" below means module 1's own phases 0–7 — nothing here concerns module 2 (`coldmail-send-managed`) or module 3 (`coldmail-send-byo`), which keep their own ledgers under their own `Docs/`. Module 1's *rulings of record* are in [`open-decisions.md`](open-decisions.md); this file is only the register of work that is built and fail-verified but honestly blocked on a real-world input (added 2026-08-05).

**Date:** 2026-07-19 · **Status:** the single register of everything that is BUILT + fail-verified in logic but honestly deferred until a real-world input exists. **Every row was flagged PENDING at build time and NONE was ever faked green** (each phase's status doc carries the same "never faked green — the honest ledger" framing; grep-confirmed across `Docs/`, §5 below).

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
> forward-only with **no** down-migration mechanism (measured: 0 `.down.sql`, no version-tracking
> table), and the LOG tables are append-only with `cp_runtime` holding no `UPDATE`/`DELETE` on them by
> construction. So for the LOG, **restore IS the only recovery path there is** — B8 is not one recovery
> option among several, it is the whole set. Detail and the go-live checklist:
> [`history/go-live-rollback-posture.md`](history/go-live-rollback-posture.md).

**The one wiring gap (flagged loudly, not silently deferred):** `port25_open` can currently only be *set false* (boot registration); there is **no operator/env/admin command to flip it true** in a running system (no `TM_NODE_PORT25_OPEN`). Interim is a `cp_admin` break-glass `UPDATE nodes SET port25_open=true WHERE id=…`; its permanent home is the **DR-3 admin-audit-log egress-test-writer** (phase-3-cutover-runbook §4 step 1). Recorded in [`go-live-checklist.md`](go-live-checklist.md) Stage 1 step 1 + Stage 0 (DR-3) — the highest-value parallel build.

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
| H1 | **Live-HTTP endpoint parity (§5/§6/§10)** | every endpoint's behavior is proven in-process — Go `httptest` handler tests (verdict matrix, contract, auth) + the fork's RSpec request specs (71 examples, 0 failures) exercise the same handlers | the dev host cannot run/kill a live server reliably (Windows-specific; the Go binary and Ruby container both boot but don't serve dependably) | on the first Linux host, run history/TEST-PLAN.md **§5.4** (start the server on a real port; re-run the §5.1 matrix, §5.3 auth, §6 rows, and the §10 fork endpoints over a real socket) and confirm they **match** the in-process results | the first Linux/production host |
| H2 | **TLS termination at the app (if used)** | TLS is never disabled anywhere (no `InsecureSkipVerify`, vuln-scan confirmed); not exercised locally | no live server ran locally; TLS may also terminate at a proxy (deployment choice) | if TLS terminates at the app, repeat §5.4 over `https://` and confirm the certificate is served + verification succeeds | the production TLS decision |

**Expectation, stated plainly:** this is almost certainly a Windows-only limitation and will **likely just work** on Linux — the handler code is identical and already green in-process. But it is **untested until cutover, so it is verified there, not assumed.** Recorded in [`go-live-checklist.md`](go-live-checklist.md) Stage 1 (a code/config step, not a real-world-input gate).

---

## 4. Cross-arc: the un-mockable chain (why none of the above could be faked)

Each phase has exactly one behavior that **cannot** be proven locally, and they are the same shape — a real counterparty's reputation/behavior:

1. **P2/P3 greylist round-trip (GAP-35)** — a real MX's stateful 450-then-250. `smtp_mock` can't reproduce reputation.
2. **P5 greylist reject-then-accept** — the same, under the durable retry loop.
3. **P6 vendor accuracy (GAP-46/47)** — "like P2's un-mockable greylist"; the vendor is weakest exactly on the residue we route.
4. **P7 real accuracy + live authentication (GAP-48)** — the sender must exist and emit authenticated outcomes.

None carries a runnable local command; each is honestly rollout-only. This is by design — the machine is verified; the accuracy is earned.

---

## 5. "Never faked green" — grep-confirmed

Every `PENDING-REAL-*` marker sits inside an "honest ledger / never faked green" framing. Occurrence counts across `Docs/` this closeout, **excluding these three new consolidation docs** (counting them would be circular):
- **`PENDING-REAL-BOX`** — 25 occurrences across 11 files; anchor: `history/phase-3-status.md:7` ("every real-IP-dependent check is honestly marked PENDING-REAL-BOX with its verification command — never faked green").
- **`PENDING-REAL-VENDOR`** — 12 occurrences across 8 files; anchor: `history/phase-6-status.md:103` ("never faked green — the honest ledger").
- **`PENDING-REAL-SENDER`** — 16 occurrences across 8 files; anchor: `history/phase-7-status.md:70` ("PENDING-REAL-SENDER / rollout (never faked green — the honest ledger)").

**Confirmed: no PENDING item was ever asserted as passing.** Each is built, wired, logic-tested, and marked deferred with (where a command exists) the exact proof to run at rollout — or, where the behavior is un-mockable, an honest "rollout observation only."

---

## 6. Chunk-4 disposition (2026-08-07) — all 20 rows are GENUINELY UNCLOSABLE here, and that is a claim about the rows, not about effort

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
the three things a test harness structurally cannot manufacture. §4's chain is the same observation
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
   `go-live-checklist.md` Stage 1 step 1 and Stage 0 — **it is not one of the twenty**, and it is named
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
waiting on a decision, a judgement, or a piece of unwritten code inside this repo.**

---

*Companion docs: the ordered go-live path is [`go-live-checklist.md`](go-live-checklist.md); the arc's honest close is [`history/arc-closing.md`](history/arc-closing.md).*
