# Operations — deploying and running the system

This is the operator's guide: how to deploy the system, satisfy each deployment prerequisite,
run it safely, pull the kill-switches when you need to, promote self-tuning mechanisms from
recommend-only to automatic, and read what the system is telling you. For the internal
architecture see [`ONBOARDING.md`](ONBOARDING.md); for the exact ordered bring-up sequence see
[`go-live-runbook.md`](go-live-runbook.md), which this guide summarizes and complements.

---

## 1. What you deploy

Two programs (see [`ONBOARDING.md`](ONBOARDING.md) §2):

- the **control-plane** (Go) — holds all state, needs a Postgres and a set of secrets;
- the **verifier-node** (Ruby, containerized) — the SMTP probe, needs a port-25-capable IP.

The control-plane runs cheap-verification-only out of the box with just a database and a pepper.
SMTP probing, the paid fallback, and the calibration feed are each switched on when their
prerequisite is in place. **Turning capability on is always config or an operator action — never
a code change or redeploy** (this is the payoff of the two-deployable split).

---

## 2. Deployment prerequisites — and how to satisfy each

Each capability tier has a prerequisite. These are ordinary setup steps, like provisioning a
database: satisfy the prerequisite, set the switches, verify. They are gated in order — you can't
usefully do a later tier before an earlier one is live.

### 2.1 Baseline — cheap verification (database + secrets)

**Provide:** a Postgres 16 instance and the core secrets.

| Secret | Env var | Notes |
|---|---|---|
| Database DSN | `TM_DATABASE_URL` | empty ⇒ `/v1/verify` reports not-ready (503); health still served |
| Email-hash pepper | `TM_PEPPER` | the most fragile secret — generate once with strong entropy, back up geo-separately, **never rotate casually** (losing it de-joins all history) |
| Front-door bearer | `TM_FRONTDOOR_TOKEN` | authenticates callers on `/v1/verify` |

Apply the migrations in order (`migrations/0001_*.sql` … `0012_*.sql`), then start the
control-plane. **Verify:**

```bash
curl -s localhost:8080/readyz          # {"status":"ready","checks":{"db":true,"pepper":true},...} — 503 if EITHER is down
curl -s localhost:8080/healthz         # liveness: 200 while the process is up, even with every dependency down
curl -s -H "authorization: Bearer $TM_FRONTDOOR_TOKEN" \
     -d '{"email":"someone@example.com"}' localhost:8080/v1/verify
```

At this tier the system returns cheap-layer verdicts (syntax, domain/MX, disposable, role, …) —
provable negatives at full confidence, everything needing SMTP as `risky`/`unknown`. This is a
correct, shippable state, not a degraded one.

### 2.2 SMTP verification (a port-25 host)

**Provide:** a host that genuinely allows outbound port 25, with a settable PTR record, on a
clean IP. This is the real procurement work — the market for port-25 + settable-PTR + clean-IP
hosts is narrow. Vet a candidate host on a refundable trial against **V1–V5** before you pay:

| Check | What to confirm | Proof |
|---|---|---|
| **V1** | Outbound TCP/25 genuinely open | `swaks --server <a-real-mx>:25 --quit-after CONNECT` sees a `220` banner |
| **V2** | Settable custom PTR/rDNS | set PTR → verifier FQDN; `dig -x <box-ip> +short` returns it |
| **V3** | Clean IP (no spam history) | check `<box-ip>` against reputation lists *before* warmup |
| **V4** | Dedicated / stable IP | provider confirms the IP is yours for the box's life |
| **V5** | Egress == PTR == audited IP | `curl ifconfig.me` == the PTR'd IP == the node's configured egress IP — **all identical** |

**Then set** the node descriptor and switches:

```
TM_NODE_ID / TM_NODE_BASE_URL / TM_NODE_PUBLIC_EGRESS_IP   # the box (egress IP MUST equal V5)
TM_NODE_ENABLED=true
TM_SMTP_MODE=enabled
TM_WARMUP_DAILY_CAP=50        # start low; step up on clean prior-day audits
```

**Verify:** the node promotes to `active` **only** if its audit is green, the audit's
self-detected IP equals the configured egress IP (the four-way IP invariant — a green audit on
the wrong IP will not promote, by design), **and** the egress smoke-test has set `port25_open`
(see the runbook for setting it). Confirm real verdicts flow with a non-null `verifier_ip` equal
to the egress IP.

*Reversible:* `TM_NODE_ENABLED=false` or `TM_SMTP_MODE=disabled` returns to cheap verdicts with
no redeploy and no lost history.

### 2.3 Paid third-party fallback (a vendor + a signed DPA)

**Provide:** a chosen verification vendor and a **signed Data Processing Agreement**. The
fallback layer ships **disabled**; it egresses addresses to a third party, so enabling it before
the DPA is signed would be unlawful sub-processing. Enabling takes **three independent affirmative
switches** — accidental enablement is designed to be hard:

```
TM_EXTERNAL_DPA_CONFIRMED=true    # your affirmation the §7.4 DPA checklist passed
TM_FALLBACK_MODE=enabled          # the fallback rung
TM_VENDOR_KEYS=<vendor>=<key>     # the vendor's API key
```

Any one missing ⇒ the layer stays off. Set conservative spend ceilings from observed volume
before raising them:

```
TM_EXTERNAL_DAILY_CAP / _MONTHLY_CAP / _PER_JOB_CAP / _RATE_CEILING
```

**Verify:** with the switches set, the layer routes only the *residue* (addresses the earlier
tiers structurally couldn't answer) to the vendor, and a vendor "valid" is recorded as `risky`
at a capped confidence — never laundered into an SMTP-grade `valid`.

*Reversible:* `TM_FALLBACK_MODE=off` is the global panic button — no address egresses, all
policies, one action.

### 2.4 Confidence calibration (a real send-outcome feed)

**Provide:** an authenticated feed of real send outcomes (bounces/deliveries) from your sending
system, over a mutually-authenticated (mTLS) channel. Provision the trust anchor and the
control-plane's client credential (loaded like the node tokens — never committed).

The feed lets the system learn from reality: it classifies each outcome for what it *proves*
about the address, recalibrates per-bucket confidence, and — once you promote the mechanisms
(§4) — auto-promotes reliable providers and weans off the paid fallback where its own accuracy
wins.

**Verify:** until the feed is authenticated the ingestor pulls **nothing** (fail-closed, never
unauthenticated data). The three calibration mutators (recalibration, promotion, weaning) stay
**recommend-only** until you promote them; a fresh calibration store falls back to baseline
confidence, so day-one behavior is unchanged until real volume accrues.

---

## 3. The kill-switch ladder — safe degradation

The system degrades in **rungs**. Every rung returns a real, correctly-labelled verdict from
fewer layers — degradation lowers a verdict's *confidence*, never its *honesty*. Nothing on this
ladder ever returns a fabricated `valid`/`invalid`. Pull a rung by config; no redeploy.

| Rung | State | Pull it when | How |
|---|---|---|---|
| **0** | Full — cheap + SMTP + queue + fallback + (promoted) self-tuning | steady state | — |
| **1** | SMTP-off | IP reputation emergency, a compromised/blocked box | `TM_SMTP_MODE=disabled`, or `TM_NODE_ENABLED=false` per node, or route a provider to `:mx` |
| **2** | Fallback-off | vendor bill runaway, vendor outage, a privacy/DPA event | `TM_FALLBACK_MODE=off` (global panic button) |
| **3** | Cheap-layers-only | total SMTP loss *and* fallback off; or a cost freeze | Rung 1 + Rung 2 together (this is the baseline steady state) |
| **4** | Paused | data-integrity emergency, DB outage, maintenance | `TM_FRONTDOOR_MODE=accept_and_defer` (bulk still accepted, drainer stopped) or `=reject` (nothing accepted) |

At **Rung 1**, addresses that *would* have earned `valid` via SMTP settle as `risky` instead —
**never `invalid`** for lack of a probe. At **Rung 2**, residue stays exactly as the queue left
it (`unknown`/`risky`) — never worse. At **Rung 4 accept-and-defer**, the front door returns a
job handle and work sits durably in the queue; restarting resumes with zero loss.

**Pulling Rung 4 in practice.** `TM_FRONTDOOR_MODE` is one dial that moves both halves — the
front door and the drainer — so you cannot pause one and leave the other running:

```bash
# Soft pause: keep taking bulk work, process none of it.
TM_FRONTDOOR_MODE=accept_and_defer   # then restart (config is read at boot; no reload/SIGHUP)
# Hard pause: take nothing at all.
TM_FRONTDOOR_MODE=reject
# Release:
TM_FRONTDOOR_MODE=serving
```

What you will see, and what to tell a caller:

- `POST /v1/verify` returns **503 with `error.code = "paused"` at BOTH sub-modes.** A synchronous
  call has no deferred form — no handle to return, nowhere to defer to — and answering it would
  either defeat the pause or fabricate a verdict. **Accept-and-defer only helps bulk callers.**
  The two sub-modes are indistinguishable to the caller apart from the per-request `request_id`.

  **`paused` is a DIFFERENT code from `unavailable`, over the same 503** (fix-plan item 18). If you
  are triaging and see `unavailable`, the pause is **not** what you are looking at — that code means
  the process is not wired (no pepper or no reachable DB) and someone should be paged. `paused` means
  an operator pulled this rung deliberately; a caller should retry later, and nobody needs waking.
  When a caller reports a failure, **ask for the `request_id`** and grep it in the control-plane log:
  every error logs one line as `api error: code=… status=… request_id=… reason="…"`, where `reason`
  is the internal detail that is deliberately NOT in the response body.
- `POST /v1/verify/bulk` at `accept_and_defer` returns **202 with a `job_id` and
  `"processing_paused": true`**; the addresses are stored encrypted in `work_items` as `ready`.
  `GET /v1/jobs/{id}` keeps working and repeats `processing_paused: true` with `settled: 0`, so a
  caller polling the handle learns *why* progress is zero instead of diagnosing a stall.
- The drainer logs `PAUSED (kill-switch Rung 4 accept-and-defer)` once on entry and
  `RESUMED (Rung 4 released) — draining the work that accumulated while paused` on release. Only
  transitions are logged, so the incident's real signal is not buried.
- **Confirm the rung is actually pulled** before you rely on it — `/readyz` and the boot line show
  `frontdoor=paused-accept-and-defer(Rung4)` or `paused-reject(Rung4)`:

```bash
curl -s localhost:3000/readyz | grep -o 'frontdoor=[^ "]*'
```

While paused the drainer also skips the lease-reclaim sweep. That is deliberate: it takes no new
lease, so the only `in_flight` rows are ones a pre-pause attempt owns, and re-readying work nothing
may process would be churn. The sweep catches them on the first tick after release.

Backlog while paused is bounded only by `work_items` growth and the §7.5 per-submission cap — a
long soft pause under load accumulates rows. Watch the `ready` count and drop to `reject` if the
queue is growing faster than you intend to drain it. Full per-surface matrix and the reasoning:
[`kill-switch-ladder.md`](kill-switch-ladder.md) Rung 4 and [`adr/ADR-0015-rung4-pause-enforcement.md`](adr/ADR-0015-rung4-pause-enforcement.md).

Two **brakes** are the ladder's safety interlock — they refuse to let an *automated* action
cascade into a self-inflicted outage:

- **Fleet brake:** if a majority of active nodes trip at once, the cause is systemic → the system
  **alerts and holds** instead of auto-quarantining the whole pool (which would empty it).
- **Cascade brake:** if a systemic confidence swing appears across many calibration buckets, the
  system **holds for human confirmation** instead of auto-applying a mass recalibration.

Both convert a would-be automatic global change into a human decision. There is also an absolute
**active-floor backstop**: statistical quarantine may never drop the pool below the serving floor
— the last healthy node is never auto-quarantined.

See [`kill-switch-ladder.md`](kill-switch-ladder.md) for the full ladder with every scope.

---

## 4. Shadow-mode — promoting a mechanism to automatic

Four mechanisms can change what callers see — verdict confidence, node routing, provider
eligibility, spend — without a human in the loop. On a fresh system **all four default to
recommend-only**: they compute and *recommend*, but do not mutate live state until you promote
them. This is the starting posture; promotion is deliberate and per-mechanism.

| Mechanism | Mutates | Default | Promote when |
|---|---|---|---|
| **A1** block-rate quarantine | node routing | recommend-only | you trust the statistical trigger; its blast radius (one node, reversible) is the smallest — the most defensible early promotion |
| **A6** confidence recalibration | confidence numbers | recommend-only | a recalibration version has proven sane over a real window and you approve it |
| **A8** provider promotion | routing/eligibility | recommend-only | a provider's measured accuracy over a real sample supports dropping SMTP for it |
| **A9** fallback weaning | spend/eligibility | recommend-only | your own measured accuracy beats the vendor's by a safety margin on that bucket |

The bring-up sequence for each: run it in **shadow** (compute + log, no proposals) to confirm its
own outputs look sane, then **recommend-only** (it emits approvable proposals), then **auto** (it
acts within its guards). Promotion to `auto` is an admin-audited operator action, per mechanism,
per scope — never a global flip. The audit-red node quarantine, canary recovery, rate-limiter
pacing, the historical-bounce gate, and the spend circuit-breaker stay automatic by default —
they act on deterministic signals and degrade only in the honest direction.

Recommend-only is a *mitigation*, not a substitute for authenticating the outcome feed — keep the
calibration mutators recommend-only until the feed is authenticated **and** the mechanism's shadow
outputs have proven sane. See [`shadow-mode.md`](shadow-mode.md).

---

## 5. Health, quarantine, and recovery

The control-plane runs a continuous **health-watcher** — the single writer of node status. It:

- **audits** each node on a cadence (`TM_WATCHER_CADENCE_SECONDS`), recording green/red;
- **polls liveness**, quarantining a node after `TM_LIVENESS_MISSES` consecutive failures;
- **watches block-rate** — if a node's SMTP blocks exceed `TM_BLOCK_QUAR_THRESHOLD_PCT` over a
  window (with at least `TM_BLOCK_MIN_SAMPLES` attempts), it's quarantine-eligible.

Quarantine is **quick, recovery is slow** (asymmetric, on purpose — reputation is easy to lose,
hard to rebuild):

- A quarantined node recovers only via a **canary**: a small metered trickle of real traffic
  (`TM_CANARY_WARMUP_CAP`), and only after `TM_CANARY_RECOVER_GREENS` consecutive clean cycles
  with at least `TM_CANARY_MIN_SAMPLES` real attempts. A node never re-enters the pool at full
  volume on zero evidence.
- On recovery its warmup counter resets low — a recovered node re-warms, it doesn't resume at
  full send rate.

Startup validates the control-loop dials: an out-of-range value that would silently disable a
brake (e.g. a fleet-brake fraction ≥ 100%) is a **startup error**, never a silent no-op.

---

## 6. What the alerts mean

| Alert | Meaning | Action |
|---|---|---|
| **audit-red** | a node's audit went red (DNS/PTR/config problem) | the node auto-quarantines; investigate the host |
| **block-spike** (single node) | one node's block-rate crossed the threshold | it quarantines (if A1 promoted) or recommends; check that IP's reputation |
| **fleet-wide** | a majority of nodes tripped at once — systemic | **the fleet brake has held the pool** — this needs a human; do not drain, find the common cause (a routing change, a shared upstream) |
| **recovering** | a quarantined node is on its canary trickle | informational; watch for clean cycles |
| **rate-saturation** | demand is exceeding paced capacity | add a node, or route more through the queue |
| **pool-eligible-low** | the eligible node set is near the floor | you are one quarantine from cheap-only; add capacity |

The **fleet-wide** alert is the one that must never be a mere dashboard number — it is the signal
that the brake did its job and a human decision is required.

---

## 7. Operating principles

- **Every rung is reversible.** Quarantine → canary recovery; breaker → window-roll; pause →
  resume with the durable queue. You can always step back up.
- **Degrade toward `unknown`, never fabricate.** Every safe state the system falls into returns
  humbler verdicts, never wrong ones.
- **Enabling capability is deliberate.** SMTP, the vendor, and auto-calibration each take an
  explicit operator action with its prerequisite in place — nothing dangerous turns itself on.
- **The manual controls should be audited.** Node disables, the fallback-kill, pauses, and
  mechanism promotions are operator actions that change what callers see; record who did what,
  when (an append-only admin-audit log, distinct from the verdict LOG, is the intended home).

For the full ordered bring-up with every owner gate and verification command, work through
[`go-live-runbook.md`](go-live-runbook.md).
