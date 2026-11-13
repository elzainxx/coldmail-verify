# Go-live checklist — one ordered path from local build to live, with a real status on every line

**Status date:** 2026-11-09 · **Evidence base:** the tree at `4b73326`, CI run
[42633652063](https://github.com/elzainxx/coldmail-verify/actions/runs/42633652063) (all 11 jobs green), and the
local gate runs of 2026-11-09. The seven phases are built and fail-verified locally; this is the sequence that turns
that machine on, safely, in order. Every step is config or an operator action — **zero control-plane code changes**
(the Door-5 payoff) — except the three 🔧 build tasks below, which are code and are said to be.

**Legend — every item carries exactly one:**

| Mark | Meaning |
|---|---|
| ✅ | **Done.** The evidence is cited: an ADR, a commit, a CI run, a gate, or a named test. |
| ⏳ | **Owner-only, real-world step.** It needs a real host, a counterparty, a signature, or elapsed time; nothing in this repo can do it. Each is collected in the owner's action list. |
| 🔧 | **Not built yet — a build task inside this repo.** Not an owner action and not a decision; tracked in [`future-work.md`](future-work.md) §4. |
| ➖ | **Not applicable** (with the reason). |

There are no unchecked-by-default boxes: if a line is not ✅, it says what is missing and who can supply it.

**Reversibility, stated precisely.** "Each stage is independently reversible" is true of **flags and config**: every
stage below turns off by unsetting a variable, with no redeploy and no lost history. It is **not** true of the schema —
migrations are forward-only, with no down-migrations and no version-tracking table, and `git revert` of a migration
does not undo it. Read [`operator-runbook.md`](operator-runbook.md) §9 before a second environment exists.

**The order is safety-critical and the stages are gated:** Stage 2 (the vendor) needs Stage 1 (a real box) earning
verdicts, and Stage 3 (the outcome feed) needs a system that has been sending long enough to have outcomes — and a
sender that does not yet exist. Companion detail: the commands are in [`install-and-deploy.md`](install-and-deploy.md)
§3; everything deferred is in [`future-work.md`](future-work.md).

**Counts, checked today (the tally at the end is computed from the tables):** 8 ✅ · 30 ⏳ · 6 🔧 · 0 ➖ — 44 items.

---

## Stage 0 — before anything ships anywhere non-local

| ID | Item | Status | Evidence / what is missing |
|---|---|---|---|
| **G0.1** | The project's own LICENSE | ✅ | Proprietary / all-rights-reserved: [`LICENSE`](../LICENSE) (commit `bcb88b1`), [`NOTICE`](../NOTICE) respects the MIT third-party parts; [`open-decisions.md`](open-decisions.md) §6.1. |
| **G0.2** | **DR-3 admin-audit log** — an append-only, per-actor audit of every manual control-plane action | 🔧 | Ruling stands ([`open-decisions.md`](open-decisions.md) DR-3); **not built**: no migration 0001–0015 creates such a table and no Go code writes one. [`future-work.md`](future-work.md) FW-1. It is the permanent home for G1.2. |
| **G0.3** | Apply migrations `0001` … `0015` to the production database, and confirm `verification_raw` partition headroom | ⏳ | Needs the real database. In CI every push applies all 15 to a fresh Postgres (job `control-plane`, step "apply migrations"). `0014` + the maintenance loop ([ADR-0017](adr/ADR-0017-raw-partition-maintenance.md)) must be in place before any real verdict is written; confirm with `SELECT relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = 'verification_raw'::regclass ORDER BY 1;` (current month plus `TM_RAW_PARTITION_MONTHS_AHEAD` ahead), and watch for `raw-partitions: maintenance FAILED`. See [`operator-runbook.md`](operator-runbook.md) §8. |
| **G0.4** | Re-run `govulncheck ./...` in `control-plane/` immediately before the pilot | ⏳ | Clean on 2026-11-07 (go1.26.10, `x/net` v0.62.0, `x/text` v0.43.0; it was *not* clean before: 17 live-call-path advisories). The advisory database moves, so this is a re-run, not a one-time tick — use a `golang:1.26` image at or above the patched floor. Any live call path blocks. |
| **G0.5** | Ruby end-of-life (3.2 → supported) | ✅ | Closed 2026-11-09: node on `ruby:3.4.12-slim-bookworm@sha256:c81ac5a2…`, truemail 3.3.1 unchanged ([ADR-0018](adr/ADR-0018-verifier-node-base-image.md) addendum 2; FORK.md row 18). `check-base-image-pinned.sh` and `check-gemfile-lock.sh` pass on 3.4.12 (run locally 2026-11-09); commit `4b73326`; CI run 42633652063. |
| **G0.6** | Verifier-node resolver hygiene is a control, not a sentence | ✅ | A `search`/`domain` line, `ndots` > 1 or a dotted hostname turns `GET /v1/audit` red, so the node does no SMTP ([ADR-0019](adr/ADR-0019-dns-names-are-rooted-and-failure-is-unknown.md) addendum; FORK.md row 16). Gate `check-resolver-hygiene.sh` + selftest; commit `f092e02`; CI run 42614985930. What the operator must do on the real box is G1.4. |
| **G0.7** | A-only domains (an A/AAAA record, no MX) are not `invalid` | ✅ | Ruled and fixed ([ADR-0020](adr/ADR-0020-implicit-mx-rfc5321.md)): the domain's own address is its mail host; if the probe cannot run the verdict is `risky`; null MX and "no MX and no A/AAAA" stay `invalid`; our outage is `unknown`. Gate `check-implicit-mx.sh` + selftest (two mutants); commit `1b3d083`; CI run 42610984679. Cost to know: a not-found MX now costs three DNS queries (MX, A, AAAA). |
| **G0.8** | The SMTP probe never connects to a non-public address (the code and the shipped ruleset) | ✅ | [ADR-0021](adr/ADR-0021-smtp-probe-destination-guard.md): pre-flight guard + post-check; the brain returns `risky` / `non_public_mx` (migration `0015`), never `invalid`, never cached. Gate `check-probe-guard.sh` + selftest (independent oracle, four mutants); commit `4b73326`; CI run 42633652063. **Installing the firewall on the real box is G1.5.** |
| **G0.9** | Migration version tracking | 🔧 | Not built: no `schema_migrations`, 0 down-migrations of 15. Cheap before a second environment exists, archaeology afterwards. [`future-work.md`](future-work.md) FW-3. |
| **G0.10** | The control-plane is packaged (a Dockerfile / compose file) | 🔧 | Not built: `control-plane/` has no Dockerfile or compose file; deployment is `go build` + env + the migration loop. Not a blocker for a single host. [`future-work.md`](future-work.md) FW-4. |
| **G0.11** | CI is green on the commit being shipped | ✅ | All 11 jobs green on `4b73326`: run [42633652063](https://github.com/elzainxx/coldmail-verify/actions/runs/42633652063). Re-check the run for whatever commit is actually deployed. |
| **G0.12** | No credential reaches a log line; the engine is unmodified; the gates and their self-tests are wired | ✅ | `check-no-secret-leak.sh` ([ADR-0016](adr/ADR-0016-no-credential-in-a-log-line.md)), `bin/assert-gem-unmodified.sh` (Door 5: "truemail pinned EXACTLY to 3.3.1"), `check-test-enumeration.sh` + selftest ("all 20 check script(s) wired … each named in `test-strategy.md`") — all passing 2026-11-09. |

## Stage 1 — the real box (SMTP verification goes live)

**Gate (the wall):** a host that passes V1–V5 on a refundable trial *before* you pay. The narrow
port-25 ∧ settable-PTR ∧ clean-IP market is the wall, not the design. Commands: [`install-and-deploy.md`](install-and-deploy.md) §3.2.

| ID | Item | Status | Evidence / what is missing |
|---|---|---|---|
| **G1.1** | Host vetting **V1–V5**: outbound :25 open, settable PTR, clean IP, dedicated IP, egress == PTR == audited IP | ⏳ | Needs a real provider and a real IP. Logic is built and fail-verified (`TestEligibility_EgressMismatchFailsGate_GAP24`); the proofs are `nc`/`swaks`/`dig`/`curl` commands run *from the trial box*. [`future-work.md`](future-work.md) B1–B4. |
| **G1.2** | An operator command that sets `port25_open=true` | 🔧 | Not built: `port25_open` is only ever written false; there is no `TM_NODE_PORT25_OPEN`, no admin endpoint, no egress-test writer. Interim: as `cp_admin`, `UPDATE nodes SET port25_open=true WHERE id='…'`. [`future-work.md`](future-work.md) FW-2, riding G0.2. |
| **G1.3** | Egress-25 smoke test from the real box, then set `port25_open=true` | ⏳ | Needs the real box. Until it is true the node is not `:smtp`-eligible (the gate fails closed). |
| **G1.4** | Give the box a **short hostname** (`hostnamectl set-hostname verifier`, no dot) and a nameservers-only `resolv.conf` | ⏳ | A real-host step. The control is built (G0.6); a box that skips it audits red with the reason in `warnings.resolver` and does no SMTP. |
| **G1.5** | **Install the egress firewall** (`nft -f verifier-node/egress/verifier_egress.nft`), mirror it in the provider's egress security group, restrict the instance metadata service (IMDSv2, hop limit 1) | ⏳ | A real-host step, **before the first real probe**. The ruleset is shipped and gated (G0.8). Acceptance: from the box `nc -w2 169.254.169.254 25` and `nc -w2 10.0.0.1 25` fail at once and the `nft` counter rises. |
| **G1.6** | DNS for the verifier domain: A-record → the public egress IP, PTR → the FQDN, SPF TXT authorizing the egress IP | ⏳ | Needs the real domain and IP. The audit checks PTR but **not** SPF (a separate item). [`future-work.md`](future-work.md) B2, B5. |
| **G1.7** | Point the box at real config (`SMTP_PORT=25`, real `DNS`) and `POST /v1/config/reload` | ⏳ | Needs the real box. The reload mechanism is built and specced (`/v1/config/reload`; a new `config_fingerprint` invalidates any mock-era cache). |
| **G1.8** | Set the brain's node descriptor: `TM_NODE_ID`, `TM_NODE_BASE_URL`, `TM_NODE_PUBLIC_EGRESS_IP` (== V5), `TM_NODE_ENABLED=true`, `TM_SMTP_MODE=enabled`, and the node token in `TM_NODE_TOKENS` | ⏳ | Needs the real egress IP and a generated per-node token. Node #2 is a numbered `TM_NODE_2_*` block — config only (`TestLoad_NodeTwoIsConfigOnly_GAP31`). |
| **G1.9** | First audit green → the node promotes `provisioning → active` | ⏳ | Needs the real box. Promotes **only** if audit green ∧ `port25_open` ∧ `current_host_ip == TM_NODE_PUBLIC_EGRESS_IP`; a green on the wrong IP does not promote ([ADR-0009](adr/ADR-0009-gap24-egress-ip-invariant-runtime-enforcement.md)). |
| **G1.10** | Start the warmup cap low: `TM_WARMUP_DAILY_CAP=50`, step up on clean prior-day audits | ⏳ | Real-world, over days. The cap counts *verifications*; a catch-all check is about two connections. |
| **G1.11** | **Live-HTTP verification** (§5.4 matrix, auth and node endpoints over a real socket) and the **TLS termination decision** | ⏳ | First Linux/production host. In-process equivalents pass in CI (Go `httptest`; the fork's RSpec, 180/0 per FORK.md; the node socket proof 11/11) and CI already boots the real binary on a Linux runner (`check-no-secret-leak.sh`); the live matrix and TLS are not exercised until a real host. Whether TLS terminates at the app or a proxy is a deployment decision with no recorded recommendation. [`install-and-deploy.md`](install-and-deploy.md) §4; [`future-work.md`](future-work.md) H1, H2. |
| **G1.12** | Go-live acceptance proofs on the real IP (break PTR → quarantine → recover; honest-corporate `valid`/`accepted`; Gmail/O365/Yahoo `risky`/`provider_no_smtp` with SMTP never called; warmup-cap degrade; external port scan; four-way IP identity; provider-routing drift alarm silent) | ⏳ | Real IP required; each proof is listed in [`install-and-deploy.md`](install-and-deploy.md) §3.2e. [`future-work.md`](future-work.md) B3. |
| **G1.13** | DR-4 backup/PITR **restore-into-isolation drill** | ⏳ | Needs go-live infrastructure and a real snapshot. For the LOG, restore is the *only* recovery path ([`operator-runbook.md`](operator-runbook.md) §9). The at-rest ciphertext and in-transaction purge are built and proven locally (`TestAddrCipher_CiphertextContainsNoPlaintext`). [`future-work.md`](future-work.md) B8. |
| **G1.14** | **Pepper backup and recovery drill** — redundant, geo-separate, access-audited backups; recovery rehearsed | ⏳ | Needs the real secret store. Losing the pepper de-joins all history; the custody rule is written ([`security-posture.md`](security-posture.md) §2). |
| **G1.15** | Control-plane host hardening: core-dump suppression, access control, the brain-compromise threat model (PN-12) | ⏳ | A deployment concern; no host exists yet. The pepper is held in memory, so this is part of its custody. |
| **G1.16** | Greylist reject-then-accept round-trip observed (GAP-35) | ⏳ | Un-mockable: needs a real greylisting provider at rollout. The retry mechanics are proven on a by-attempt-count stub. [`future-work.md`](future-work.md) B6. |
| **G1.17** | Two-box reputation isolation in the wild | ⏳ | Needs two real IPs (one dirty, one clean). Per-`verifier_ip` isolation is proven on seeded LOG rows. [`future-work.md`](future-work.md) B7. |

## Stage 2 — the paid vendor fallback (enable the P6 layer)

**Gate (legal — the DPA):** the `external` layer ships **disabled**; enabling it before the §7.4 checklist passes
(DPA signed, retention understood, TLS confirmed, key custody established) is unlawful sub-processing. Setting the flag
is the operator's affirmation that it passed.

| ID | Item | Status | Evidence / what is missing |
|---|---|---|---|
| **G2.1** | Select a verification vendor (one at launch) | ⏳ | A commercial decision. [`future-work.md`](future-work.md) V2. |
| **G2.2** | Signed DPA and the §7.4 due-diligence checklist | ⏳ | A legal act only the owner can perform. [`future-work.md`](future-work.md) V3. |
| **G2.3** | A real `external.Vendor` adapter wired into `buildExternal` | 🔧 | Not built — and not buildable until G2.1 names a vendor; `buildExternal` returns nil, fail-closed. [`future-work.md`](future-work.md) V2. The mechanics are proven against a controllable mock (`TestResolve_VendorValid_IsNeverValid`, `TestSpendOnce_UnderSlowDrainerReclaim`). |
| **G2.4** | Conservative budget ceilings: `TM_EXTERNAL_DAILY_CAP`, `_MONTHLY_CAP`, `_PER_JOB_CAP`, `_RATE_CEILING` | ⏳ | Needs observed residue-tail volume. The policy is ruled ("start low, raise from data, never loosen blind"); only the numbers wait. [`future-work.md`](future-work.md) V4. |
| **G2.5** | Flip the three switches: `TM_EXTERNAL_DPA_CONFIRMED=true` + `TM_FALLBACK_MODE=enabled` + a `TM_VENDOR_KEYS` entry | ⏳ | After G2.1–G2.4. Any one missing ⇒ off; `TM_FALLBACK_MODE=off` overrides all. The guard is built and tested. |
| **G2.6** | OQ-P6-6 disclosure wording (disclose + offer opt-out) | ⏳ | The ruling is made ([`open-decisions.md`](open-decisions.md) §6); the platform's terms/consent copy is a product decision. The opt-out is mechanical (a policy without the `external` block never egresses). |
| **G2.7** | F-4 integration assertion: the platform opt-out flag maps 1:1 to a policy without `external` | ⏳ | Verified at integration with the platform; not testable inside this repo. |
| **G2.8** | Standing vendor controls: cumulative-egressed metric and a periodic vendor-deletion request (PN-7) | ⏳ | Operational, on a cadence, once a vendor exists. `external_calls` is queryable by `email_hash` for deletion (PN-10). [`future-work.md`](future-work.md) V5. |
| **G2.9** | Measure vendor accuracy on the residue (GAP-46/47) | ⏳ | Needs a real vendor *and* the outcome feed; never claimed until measured. [`future-work.md`](future-work.md) V1. |

## Stage 3 — the outcome feed (turn on the flywheel)

**Gate (cross-project):** the flywheel's live value needs the sender ("Feature 2", GAP-48), which does not exist yet.
The authentication *mechanism* is decided — mutually-authenticated pull (mTLS), [ADR-0013](adr/ADR-0013-outcome-feed-auth-mechanism-and-tb7-threat-model.md)
— and the design gate is discharged.

| ID | Item | Status | Evidence / what is missing |
|---|---|---|---|
| **G3.1** | The sender exists and emits the locked `send_outcome` shape on an authenticated, append-only, cursor-paginated feed | ⏳ | Another project; the supply is its commitment. [`future-work.md`](future-work.md) S3. |
| **G3.2** | Provision the mTLS trust anchor and the control-plane's client credential | ⏳ | A new secret class, loaded like the node tokens, never committed; arrives with the sender. [`future-work.md`](future-work.md) S2. |
| **G3.3** | A real `MTLSClient` wired into `PullSource` | 🔧 | Not built; needs the sender's half of the contract (G3.1). `PullSource` is the fail-closed skeleton: `Authenticated()` is false with no client, so nothing is pulled. [`future-work.md`](future-work.md) S2. |
| **G3.4** | The live-path opt-ins (`TM_FLYWHEEL_GATE_ENABLED`, `TM_FLYWHEEL_CALIBRATION_ENABLED`) stay OFF and the three mutators stay `recommend-only` until an owner opts in | ✅ | Built default: `config.ShadowModes` all `recommend-only`; `TestFreshSystem_FlywheelWiredButInert` proves a wired-but-fresh flywheel mutates nothing ([ADR-0014](adr/ADR-0014-phase-7-flywheel-additive-surface.md)). |
| **G3.5** | OQ-P7-8 disclosure ("your sends train our accuracy") | ⏳ | Ruled: disclose-don't-hide ([`open-decisions.md`](open-decisions.md) §6); the platform's wording is a product decision. |
| **G3.6** | Real accuracy numbers; any promotion/weaning actually firing | ⏳ | Needs real send→bounce outcomes and volume over time — "the moat is years of running it." [`future-work.md`](future-work.md) S1, S4, S5. |

## Decisions that are not gates (so they cannot be mistaken for ones)

The four sub-decisions moved out of the decision log — FLAG-A, FLAG-C, PN-8, PN-9 — do **not** block go-live: the built
behaviour of each is safe and documented ([`future-work.md`](future-work.md) §5). They are listed for the owner there.

---

## Tally

| Mark | Count | IDs |
|---|---|---|
| ✅ done | 8 | G0.1, G0.5, G0.6, G0.7, G0.8, G0.11, G0.12, G3.4 |
| ⏳ owner-only | 30 | G0.3, G0.4, G1.1, G1.3 – G1.17, G2.1, G2.2, G2.4 – G2.9, G3.1, G3.2, G3.5, G3.6 |
| 🔧 not built | 6 | G0.2, G0.9, G0.10, G1.2, G2.3, G3.3 |
| ➖ not applicable | 0 | — (nothing in this path was found inapplicable; the three-way mix above is the honest state) |

The ⏳ items are copied into the owner's action list with the reason only the owner can do each. The 🔧 items are
the in-repo build tasks of [`future-work.md`](future-work.md) §4 (FW-1 → G0.2, FW-2 → G1.2, FW-3 → G0.9, FW-4 →
G0.10; FW-5 is a cleanup with no go-live line) plus the two seams that wait on an outside party (the vendor adapter G2.3 and the mTLS client G3.3).

*Companion docs: everything deferred is in [`future-work.md`](future-work.md); the arc's honest close is in
[`history/arc-closing.md`](history/arc-closing.md); the rolled-up status log this file replaces is in git history
(`git log -p -- Docs/go-live-checklist.md`, formerly `go-live-runbook.md`).*
