# Config Manifest — Truemail Verification System (Phases 1–7)

**Status:** DRAFT v0.1 — cross-phase consolidation pass (planning, not code)
**Purpose:** the single exhaustive registry of every toggle, flag, threshold, secret, and dial defined across the seven approved phase specs. If a phase mentioned a knob, it appears here.
**Companion docs:** [`kill-switch-ladder.md`](kill-switch-ladder.md) · [`shadow-mode.md`](shadow-mode.md) · [`cross-phase-consistency-report.md`](cross-phase-consistency-report.md) · [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md)

---

## 0. How to read this

### 0.1 Type vocabulary
- **secret** — a credential or key. Custody, rotation, and blast-radius-on-leak matter more than a "value."
- **feature-flag** — an on/off switch or a mode/selector (includes policy-order and gate toggles that change *behavior*, not just a *number*).
- **dial** — a tunable number, threshold, window, TTL, or sample-size. Per the founding rule, dials can be turned later; the vocabularies they feed cannot.

### 0.2 The founding distinction this manifest enforces
> *"Anything that becomes historical data, or that another system codes against, is a one-way door — get it right now. Everything else is a dial you can turn later."* (build plan)

Every row is classified so the one-way doors (response shape, LOG, hashing, status/reason vocabulary, never-fork) are visibly separated from the dials. **A dial that feeds a one-way door is called out** — e.g. a confidence *number* is a dial, but the `confidence` *type/range* (int 0–100) is a frozen contract.

### 0.3 Column meanings
- **blast radius** — scope of harm if mis-set: `single box` < `pool` < `brain-wide` < `data-integrity` (system-wide, often irrecoverable). `reputation`, `cost`, `privacy`, `security` are called out where they dominate.
- **default** — the spec's value/range, or `UNSET — owner OQ` where the spec deliberately leaves it open, or `LOCKED` where the spec froze it.

### 0.4 What this manifest is NOT
It does not resolve open questions or set UNSET values — those are owner decisions, surfaced in [`pre-takeoff-checklist.md`](pre-takeoff-checklist.md). It flags divergence between phases; it does not rewrite any frozen spec.

---

## 1. PHASE 1 — Foundations

### 1.1 Secrets
| name | type | default | turning it off / losing it | blast radius | source |
|---|---|---|---|---|---|
| **email-hash pepper** (global HMAC-SHA256 key) | secret | scheme LOCKED; value UNSET (OQ-1 ELEVATED) — single long-lived global pepper | **Lose it** → every historical `email_hash` de-joins; cache/dedup/flywheel linkage uncomputable forever. **Leak it** → low-entropy local-parts brute-forceable (full-address breach of LOG + cache + send_outcomes). | data-integrity (system-wide, irrecoverable) | P1 §3.3.3, OQ-1, App C |
| **front-door bearer token(s)** (caller → brain) | secret | scheme decided (bearer, OQ-13); P1 accepts static token from config | Missing → request rejected; leaked → unauthorized verification + (by P5) bulk PII submission + (by P6) vendor-budget burn | brain-wide | P1 §3.1.1, §3.5.4, §5.2, OQ-13 |
| **node token / rack `ACCESS_TOKENS`** (brain → box) | secret | UNSET value; in secrets mgr, referenced by id, never plaintext in `nodes` | Missing → box unreachable (verdict `unknown`); leaked → box impersonation | single box | P1 §3.5.4, §4.5, OQ-13 |
| **Supabase service-role key** | secret | service-role only in P1; RLS deferred (OQ-20) | The sole credential gating ALL state + the append-only grant; compromise defeats append-only tamper-evidence | data-integrity / security | P1 §4, OQ-20 |

### 1.2 One-way-door contracts (feature-flags that are FROZEN, not tunable)
| name | type | default | turning it off | blast radius | source |
|---|---|---|---|---|---|
| **LOG-every-result** (incl. cheap layers + cache hits) | feature-flag | LOCKED on | off → data lost forever; "the log is the point" | data-integrity | P1 §3.2.1, §6.10 |
| **`confidence` type/range** | feature-flag | LOCKED int 0–100 (CHECK constraint) | change type → Door-1 (response-shape) break | data-integrity / consumers | P1 §3.1.3, §4.1 |
| **local-part lowercasing** (norm/v1) | feature-flag | LOCKED yes (OQ-3, RFC-lossy accepted) | change → re-partitions all history (frozen door 3) | data-integrity | P1 §3.3.2, OQ-3 |
| **cache key shape** `(email_hash, config_fingerprint)` | feature-flag | LOCKED composite (OQ-9) | keying on hash alone → silently-wrong cross-config answers | data-integrity | P1 §4.2, App B |
| **`config_fingerprint` input set** | feature-flag | LOCKED input list; SHA-256 canonical | wrong set → verdicts mis-bucketed; cache serves cross-config answers | data-integrity | P1 §3.2.5, OQ-11 |
| **status / reason / recommendation vocabulary** | feature-flag | LOCKED enum, additive-only | rename/remove → Door-4 break; every downstream consumer + P7 calibration breaks | data-integrity | P1 §3.4.3 |

### 1.3 Policy feature-flags (shape frozen, contents are dials)
| name | type | default | turning it off / mis-setting | blast radius | source |
|---|---|---|---|---|---|
| **active policy per request** | feature-flag | `cold-email` v1 | wrong policy → wrong gate/flag behavior | per-request | P1 §3.1.1, §7.3 |
| **`policies.is_default` single-default** | feature-flag | one system-wide default (OQ-23), partial unique index | multiple defaults → ambiguous selection | brain-wide | P1 §4.6, OQ-23 |
| **policy `order`** (which layers run, in sequence) | feature-flag | LOCKED shape / DIAL contents | reorder → changes stop-early / cheapest-sufficient behavior | brain-wide | P1 §7.2 |
| **policy definition = open object** | feature-flag | open/extensible, tolerate unknown keys (OQ-25) | closed struct → later phases can't add keys | brain-wide | P1 §7.2, OQ-25 |
| **`gates.syntax:fail`** | feature-flag | `invalid:invalid_syntax:stop` | — | per-verdict | P1 §7.2 |
| **`gates.mx:fail`** | feature-flag | `invalid:no_mx:stop` | — | per-verdict | P1 §7.2 |
| **`gates.null_mx:pass`** | feature-flag | `invalid:null_mx:stop` | — | per-verdict | P1 §7.2 |
| **`gates.spam_trap:pass`** | feature-flag | `invalid:spam_trap:stop` | — | per-verdict | P1 §7.2 |
| **`gates.mx:unknown`** | feature-flag | `unknown:dns_error:stop` | — | per-verdict | P1 §7.2 |
| **`gates.disposable:pass`** | feature-flag | `risky:disposable:stop` (OQ-7 OVERRULED from invalid) | set to `invalid` → poisons P7 calibration | data-integrity / per-verdict | P1 §7.2, §3.4.3, App C |
| **`recommendation_overrides_by_reason.disposable`** | feature-flag | `skip` | — | per-verdict | P1 §7.2 |
| **`provider_routing` map** (google/yahoo/o365 → mx) | feature-flag | `{google:mx, yahoo:mx, o365:mx}` | route big providers to `:smtp` → burns IP, worthless "yes" | pool / reputation | P1 §7.2, §6.9 |
| **cheap-layer exec: brain-native vs mocked-engine** | feature-flag | brain-native for syntax + MX/DNS/null-MX (OQ-6) | — | brain-wide | P1 §6.1, §6.5, OQ-6 |

### 1.4 Dials
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **`recommendation_thresholds.send`** | dial | 70 | too low → send on weak verdicts | per-verdict | P1 §7.2, §3.1.3 |
| **`recommendation_thresholds.review`** | dial | 30 | — | per-verdict | P1 §7.2 |
| **confidence `baseline_by_reason` table** | dial | fixed placeholder (OQ-8); invalids 90–99, plausible/provider_no_smtp ≤40, unknown low | inflate unknown → dodges first-class unknown; **recalibrated in P7** | brain-wide (calibration) | P1 §7.4, OQ-8 |
| **confidence `flag_deltas`** | dial | UNSET numeric | — | per-verdict | P1 §7.2, §7.4 |
| **layer `severity_hint`** ∈ {info,warn,critical} | dial | per-layer | — | per-verdict | P1 §6.0 |
| **cache TTL / `cache.expires_at`** | dial | UNSET — owner OQ (OQ-9); NOT NULL required | too long → stale verdicts as mailboxes change; too short → cache useless, more probes | brain-wide (correctness/cost) | P1 §4.2, OQ-9 |
| **`raw_truemail_json` retention window** | dial | ~90 days (OQ-10, DROP PARTITION) | zero → lose re-parse ability; infinite → unbounded storage | data-integrity (storage) | P1 §3.2.6, OQ-10 |
| **`verification_raw` partition cadence** | dial | monthly RANGE partition | mis-set → retro-fit is a full rewrite | data-integrity (storage) | P1 §3.2.6 |
| **disposable_domains list** (Supabase, not env) | dial (data) | source-of-truth table | empty → no disposable detection | brain-wide | P1 §4.4, §6.7 |
| **spam_trap list source** | dial (data) | UNSET — acquisition unsolved (OQ-17) | empty → no trap gating | pool / reputation | P1 §4.4, §6.8, OQ-17 |
| **typo known-good domain list** | dial (data) | built-in seed now, Supabase later (OQ-18) | empty → no typo suggestion | per-verdict | P1 §6.2, OQ-18 |
| **role-account list** | dial (data) | built-in | — | per-verdict | P1 §6.3 |
| **free-vs-corp / provider-class lists** | dial (data) | built-in | wrong → mis-routing | pool | P1 §6.4, §6.9 |

### 1.5 Structural feature-flags (topology/plumbing)
| name | type | default | turning it off | blast radius | source |
|---|---|---|---|---|---|
| **node registry source of truth** | feature-flag | config-list authoritative; `nodes` table = mirror (OQ-5) | — | brain-wide | P1 §5.3, §4.5, OQ-5 |
| **router strategy** | feature-flag | `single`/`random` at N=1 (via Strategy interface) | hardcode endpoint → Door-5 break | pool | P1 §3.5.1, §5.4 |
| **`nodes.enabled`** | feature-flag | operator on/off | off → node inert | single box | P1 §4.5 |
| **API versioning transport** | feature-flag | URI path `/v1/` (OQ-12) | — | consumers | P1 §3.1.1, OQ-12 |
| **`request_id` origin** | feature-flag | server-generated, client override allowed (OQ-21) | — | per-request | P1 §3.2.7, OQ-21 |
| **Supabase access model** | feature-flag | service-role only in P1, RLS deferred (OQ-20) | — | data-integrity | P1 §4, OQ-20 |
| **`/verify` echo raw vs normalized email** | feature-flag | echo raw (OQ-4) | — | per-request (UX) | P1 §3.3.7, OQ-4 |
| **`provider_canonical_hash` populate** | feature-flag | define + populate from row #1 (OQ-2) | — | data-integrity | P1 §3.3.5, OQ-2 |
| **`TM_DATABASE_URL`** (Postgres/Supabase DSN) | feature-flag | empty ⇒ `/v1/verify` not-ready (503); health still served | missing ⇒ no verify; wrong ⇒ no state | brain-wide | P1 §5.6 |

---

## 2. PHASE 2 — SMTP against mocks

### 2.1 Engine knobs (in the config fingerprint — verdict-affecting)
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **`smtp_safe_check`** (engine) | feature-flag | OFF (OQ-P2-7) | on → ambiguous passes count as success; verdict-affecting | per-verdict / calibration | P2 §6.8, OQ-P2-7, App A.1 |
| **`smtp_fail_fast`** (engine) | feature-flag | false (gem default) | scoring-neutral in P2; pool pacing use deferred P4 | single box | P2 §6.7, App A.1 |
| **`connection_timeout`** (engine) | dial | 2s (gem default) | too low → false `smtp_timeout`/unknown; too high → hung sessions | single box | P2 §6.5, App A.1 |
| **`response_timeout`** (engine) | dial | 2s (gem default) | same as above | single box | P2 §6.5, App A.1 |
| **`connection_attempts`** (engine) | dial | 2 (gem default) | — | single box | P2 §6.5, App A.1 |
| **`smtp_port`** (engine) | feature-flag/dial | 25 (mock port in P2) | wrong → no SMTP | single box | P2 §6.7, App A.1 |
| **`dns` resolver** (engine) | feature-flag | `[]` / `['ip:port']` (dns_mock in P2) | — | single box | P2 §3.2, App A.1 |
| **`not_rfc_mx_lookup_flow`** (engine) | feature-flag | false | changes MX/null-MX lookup breadth | single box | P2 App A.1 |

### 2.2 SMTP classification & scoring (data-integrity heavy)
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **`smtp_error_body_pattern`** (5xx address-fault discriminator) | dial | `/(?=.*550)(?=.*(user\|account\|customer\|mailbox)).*/i` | wrong → your-fault 5xx mislabeled invalid (**poisons calibration**) | data-integrity / per-verdict | P2 §6.1, App A.1 |
| **5xx code/body address-fault set** | dial | starts from engine pattern; ambiguity → unknown LOCKED | over-broad → false invalids | data-integrity | P2 §6.1 |
| **`552` mailbox-full → valid** | feature-flag | `valid`/high-confidence (owner OVERRIDE from risky) | mislabel invalid → poisons P7; contradicts existence-proof ruling | per-verdict / calibration | P2 §6.1.1, App D |
| **SMTP confidence bands** | dial | clean-250+probe-rejected 85–95; safe-check ≤70; address-fault invalid 90–99; mailbox_full 85–95; catch_all/plausible 25–50; unknown low | — | brain-wide (calibration, P7-recalibrated) | P2 §7.4 |
| **safe-check confidence cap** | dial | ≤70 (below clean-250) | = clean-250 → indistinguishable ambiguous pass | per-verdict | P2 §6.8, §7.4 |
| **`smtp:*` policy gate entries** (valid/invalid/catch_all/mailbox_full/unknown) | feature-flag | per §7.3 map | — | per-verdict | P2 §7.3 |
| **SMTP enabled in default policy (P2)** | feature-flag | enabled, routed to mock node (OQ-P2-8) | — | brain-wide | P2 §7.3, OQ-P2-8 |

### 2.3 Catch-all probe
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **catch-all probe on/off** (`probe_catchall`) | feature-flag | true for `:smtp` (§5) | off → can't mint `valid` on possibly-catch-all domain | per-verdict | P2 §4.1, §5.5 |
| **probe random local-part construction** | dial | 16+ random hex/base32, optional `nx-` prefix, lowercase; CSPRNG prod, seeded in tests (OQ-P2-3) | non-random → collision with real mailbox; low entropy → false catch-all | per-verdict | P2 §5.3, OQ-P2-3 |
| **catch-all probe location** (Option A same-session vs B two-session) | feature-flag | **Option B (two-session) — RESOLVED by the GAP-13 spike; gem exposes no reusable session, never patch it ([ADR-0007](adr/ADR-0007-gap13-catchall-session-two-session.md))** | weaker signal, 2× connections/reputation cost (safe: can only miss a `valid`→`risky`, never mint a false `valid`) | pool / reputation | P2 §5.4, OQ-P2-4/11, ADR-0007 |
| **number of probe RCPTs** | dial | 1 in P2; N-probe deferred P4 (OQ-P2-10) | — | per-verdict | P2 §5, OQ-P2-10 |
| **re-probe cadence / `catchall_domains.expires_at`** | dial | UNSET — SEAM (§5.6) | — | brain-wide | P2 §5.6 |

### 2.4 Plumbing & caching discipline
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **brain client-side deadline on `/v1/verify`** (`TM_SMTP_CLIENT_DEADLINE_MS`) | dial | UNSET numeric ⇒ client default 15s (P5 lease TTL must *exceed* this — latent numeric coupling) | too low → premature unknown; too high → brain hangs on wedged box | brain-wide | P2 §6.5 |
| **SMTP enabled at the kill-switch** (`TM_SMTP_MODE=enabled` + a node list) | feature-flag | SMTP wired only when Rung 1 not pulled AND ≥1 routable node; else cheap-layers-only (Rung 3) | wrongly on with no node → still degrades to cheap (never invents SMTP) | brain-wide | P2 §7.1, kill-switch Rung 1/3 |
| **engine `config_fingerprint` source** (`GET /v1/config`) | feature-flag | brain fetches full effective config; on fetch-fail → refuse a confident fingerprint, stay cheap-only | fetch-fail silently trusting a stale hash → mis-bucketed cache | brain-wide (correctness) | P2 §4.6, GAP-3 |
| **cache-only-settled-verdicts predicate** | feature-flag | LOCKED: never cache transient/your-fault unknown (OQ-P2-6) | caching transients → suppresses the retry that resolves them | brain-wide (correctness) | P2 §6.5, OQ-P2-6 |
| **greylist retry hint** `retry_after_hint_s` | dial | parsed from 4xx else policy default (SEAM to P5) | — | per-verdict | P2 §6.5 |
| **wrapper endpoint namespacing** | feature-flag | `/v1/` on the box (OQ-P2-1) | — | brain↔box contract | P2 §4.0, OQ-P2-1 |
| **`/config/reload` push vs pull** | feature-flag | PULL (OQ-P2-2) | push → secrets on the wire, split source of truth | single box / security | P2 §4.5, OQ-P2-2 |
| **`/v1/stats` counter set** | dial | shape fixed, counters DIAL | — | observability | P2 §4.4 |
| **store raw transcript verbatim** | feature-flag | verbatim (OQ-P2-5); sidecar holds verifier identity + probe addrs | scrubbing → loses join gold | data-integrity / privacy | P2 §6.4, OQ-P2-5 |

---

## 3. PHASE 3 — First real box

### 3.1 SMTP-eligibility & audit gate (LOCKED safety invariant)
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **`:smtp`-eligibility rule** `green AND port25_open` | feature-flag | LOCKED both required | either alone → run SMTP on a dead/red box (bad verdicts + burned IP) | single box / reputation | P3 §3.4, §4.4, GAP-15 |
| **`nodes.port25_open`** (egress smoke-test result) | feature-flag | set by egress test, not by audit | false → not SMTP-eligible | single box | P3 §3.4, §4.3 |
| **`nodes.audit_status`** green/red | feature-flag | from `warnings=={}` | red → excluded from `:smtp` candidacy | single box | P3 §4.3, §4.4 |
| **scheduled re-audit cadence** | dial | hourly for first box | too slow → silent breakage undetected; too fast → load | single box | P3 §4.5 |
| **audit staleness max-age** | dial | 2× the schedule | stale-green treated as ineligible | single box | P3 §4.5 |
| **auto-quarantine trigger** (first-red vs N-red) | feature-flag/dial | quarantine on FIRST red (OQ-P3-7) | debounce → risk running on genuinely-red box | single box / reputation | P3 §4.5, OQ-P3-7 |
| **auto-recovery threshold** (N consecutive greens) | dial | N-consecutive-green (asymmetric, OQ-P3-7) | too few → flapping | single box | P3 §4.5, OQ-P3-7 |

> **P3→P4 subsumption note (Phase-8 sweep):** the P3 *spec* dials above (re-audit cadence, auto-recovery
> N-green threshold) are recorded here as the P3 design. At build time the **P4 health-watcher subsumed the
> P3 re-audit ticker**, so their live env vars are the P4 watcher's (`TM_WATCHER_CADENCE_SECONDS`,
> `TM_CANARY_RECOVER_GREENS` — §4.6), not the P3-named `TM_AUDIT_INTERVAL_SECONDS` / `TM_AUDIT_RECOVER_GREENS`,
> which were removed as settable-but-ignored (see the §3.5 note). The audit staleness max-age remains its own
> consumed dial (`TM_AUDIT_MAX_AGE_SECONDS`).

### 3.2 Warmup & routing
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **warmup ascending daily cap schedule** | dial | UNSET (OQ-P3-10) illustrative D1~50/D2~100/D3~250/D4~500/wk2~1–2k | 0→full → fresh IP looks like spam, blocked | single box / reputation | P3 §5.2, OQ-P3-10, GAP-20 |
| **warmup counter durability** | feature-flag | durable in Supabase (per-node/per-window) | in-memory → restart resets ramp, IP jumps to full mid-warmup | single box / reputation | P3 §5.2 (PM4) |
| **warmup health-checkpoint gate** | dial | step up only on prior-day low smtp_blocked + green audit | — | single box | P3 §5.2, OQ-P3-10 |
| **`validation_type_for` map** (engine backstop) | feature-flag | gmail/googlemail/outlook/hotmail/live/o365/yahoo → mx; default smtp | drift from brain routing → ambiguous (GAP-19) | pool / reputation | P3 §5.1, OQ-P3-8 |
| **routing authority** (brain vs engine) | feature-flag | brain authoritative; engine map = generated backstop (OQ-P3-8) | two hand-authored tables → drift bug | pool | P3 §5.1, OQ-P3-8, GAP-19 |
| **O365 routing** | feature-flag | `:mx` (affirmed, OQ-P3-9); promote only on P7 data | `:smtp` on a hunch → untrustworthy 550s | pool / calibration | P3 §5.1, OQ-P3-9 |
| **no-eligible-node degrade** | feature-flag | degrade to cheap verdict, never invalid (OQ-P3-6) | fabricate invalid → harden-toward-unknown violated | per-verdict | P3 §4.4, OQ-P3-6 |
| **provisional-fallback non-cacheable rule** | feature-flag | capacity-gap degrades non-cacheable; `provider_mx_routed` cacheable (PB1) | caching capacity gaps → freezes low-quality, suppresses recovery verdict | brain-wide | P3 §4.4 (PB1) |
| **`smtp_skipped_reason`** signal enum | feature-flag | `no_eligible_node\|audit_red\|provider_mx_routed\|warmup_cap` (open string enum) | — | data-integrity (log) | P3 §4.4, §8 |

### 3.3 Reputation identity & network security
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **egress-IP == PTR-IP == verifier_ip == audit-IP invariant** | feature-flag | LOCKED all-one (public egress IP) | drift → blocks, meaningless audit, wrong verifier_ip (GAP-24) | reputation / data-integrity | P3 §6.6, §6.6.1, GAP-24 |
| **`verifier_ip` sourcing** | feature-flag | brain registry authoritative, cross-checked vs host_audit.current_host_ip (OQ-P3-11) | box self-report → trusts box for its own reputation identity | data-integrity / reputation | P3 §5.4, OQ-P3-11 |
| **verifier domain isolation** | feature-flag | dedicated/isolated subdomain (OQ-P3-2) | shared with real sending domain → burns real domain reputation | reputation | P3 §3.3, §6.5, OQ-P3-2 |
| **SPF published** (required, not audit-gating) | feature-flag | publish anyway (OQ-P3-3) | not audited by host_audit (GAP-17) | reputation | P3 §3.3, §3.4, OQ-P3-3 |
| **private-network mechanism** | feature-flag | provider VLAN if co-located else WireGuard (OQ-P3-12) | public wrapper → ingress attack surface | security / single box | P3 §6.2, OQ-P3-12 |
| **TLS on brain↔box hop** | feature-flag | yes / WireGuard-encrypted (OQ-P3-13) | plaintext → token/PII sniffable | security | P3 §6.2, OQ-P3-13 |
| **firewall: inbound wrapper only from brain; inbound 25 closed; egress 25+DNS only** | feature-flag | LOCKED default-deny | open → exfil / attack surface | security / single box | P3 §6.2–6.3, §6.7 |
| **node-token strategy** | feature-flag | per-node unique, revoke/rotate on compromise (OQ-P3-14) | shared token → one burn disrupts all | pool / security | P3 §6.4, OQ-P3-14 |

### 3.4 Provisioning & storage
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **host-selection (port-25 ∧ PTR ∧ clean ∧ cheap)** | feature-flag | RECLASSIFIED to pre-provisioning research task (OQ-P3-1) | wrong host → phase can't proceed | provisioning (the wall) | P3 §3.1, OQ-P3-1, GAP-16 |
| **deploy topology** | feature-flag | single container on VPS (OQ-P3-4) | — | single box | P3 §3.5, OQ-P3-4 |
| **`node_audits` history table** (raw warnings) | feature-flag | new append-only table (OQ-P3-5) | mutable column → overwrites flap history P4 needs | data-integrity | P3 §4.3, §8, OQ-P3-5 |
| **`node_warmup` counter table** (per-node/per-window) | feature-flag | new durable table (migration 0007) | in-memory → ramp resets on restart | single box / reputation | P3 §5.2, §8 |

### 3.5 Concrete config keys (this build — the env vars the P3 code reads)
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **`TM_NODE_PUBLIC_EGRESS_IP`** (node's public egress IP) | feature-flag | empty ⇒ GAP-24 conjunct fails safe (no `:smtp`) | wrong IP → egress-mismatch excludes node, or (if unset) never eligible | single box / reputation | P3 §5.4, §6.6.1, GAP-24 |
| **`TM_AUDIT_MAX_AGE_SECONDS`** (staleness window) | dial | 7200 (2× the P4 watcher cadence) | too small → green flaps ineligible; too large → stale green trusted | single box | P3 §4.5 |
| **`TM_WARMUP_DAILY_CAP`** (per-node/per-day `:smtp` cap) | dial | 50 (conservative day-1; 0 ⇒ warmup off) | 0→full → fresh IP looks like spam, blocked | single box / reputation | P3 §5.2, OQ-P3-10, GAP-20 |

> **Superseded P3 audit dials (Phase-8 dial sweep — removed, NOT settable):** the P3 spec listed a
> **re-audit cadence** (`TM_AUDIT_INTERVAL_SECONDS`) and an **N-green recover count**
> (`TM_AUDIT_RECOVER_GREENS`) as their own env vars. The **P4 health-watcher subsumed the P3 re-audit
> ticker** (`main.go` `startHealthWatcher`): the live re-audit cadence is now **`TM_WATCHER_CADENCE_SECONDS`**
> (§4.6) and the recover-green count is **`TM_CANARY_RECOVER_GREENS`** (§4.6). The two `TM_AUDIT_*` vars had
> become **settable-but-ignored aliases** (read + validated at load, but their values reached no consumer) —
> the same footgun class as the removed `TM_EXTERNAL_CONFIDENCE_CAP` (§6.1). They were **removed** in the
> Phase-8 sweep so an operator cannot set a knob that silently does nothing; the removal is locked by
> `TestLoad_RemovedAuditAliasesAreNotSettableFootguns`. Only the **consumed** staleness window
> (`TM_AUDIT_MAX_AGE_SECONDS`, the router freshness filter) remains a P3 audit dial.

---

## 4. PHASE 4 — Resilience & pool

### 4.1 Router & stickiness
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **router strategy set** (sticky-by-domain default + least-inflight fallback) | feature-flag | sticky-first, least-inflight fallback (behind unchanged P1 interface) | no stickiness → provider load × N nodes, defeats caps | pool / reputation | P4 §3, §3.3 |
| **sticky affinity mapping** (HRW vs ring vs stored table) | feature-flag | rendezvous/HRW hashing (OQ-P4-1) | stored table → churn on membership change | pool | P4 §3.2, OQ-P4-1 |
| **eligibility filter predicates** (active∧green∧port25∧fresh∧warmup-headroom∧rate-headroom) | feature-flag | LOCKED conjunction | missing conjunct → routes to unfit node | pool / reputation | P4 §3.3 |
| **least-inflight denominator** | feature-flag | per-(node, target-provider) in-flight (PM2); node-global = tie-break | wrong denominator → misroutes around wrong bottleneck | pool | P4 §3.4 (PM2) |
| **domain-sticky vs provider-sticky** | feature-flag | keep domain-sticky (OQ-P4-12); shared platforms only partially concentrated (GAP-29) | provider-sticky → hot node / single point of failure | pool / reputation | P4 §3.2, OQ-P4-12, GAP-29 |

### 4.2 Rate limiting & pacing (reputation dials)
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **`max_inflight`** (per node×provider concurrency cap) | dial | small, 1–3 | 0 → no SMTP; high → parallelism providers punish fastest | pool / reputation | P4 §4.2 |
| **`max_starts_per_window`** (rate cap) | dial | ~N/min conservative | high → too-fast, greylist/block | pool / reputation | P4 §4.2 |
| **`window_s`** (rate window) | dial | 60 | — | pool | P4 §4.2, §4.7 |
| **`start_delay_jitter_ms`** | dial | 50–500ms spread | zero → machine-regular timing (fingerprint) | reputation | P4 §4.2, §4.7 |
| **`other`-bucket granularity** (provider bucketing) | feature-flag/dial | registrable domain of MX host (OQ-P4-2) | single pool → one slow corp throttles all; per-exact-host → misses shared platforms | pool / reputation | P4 §4.1, OQ-P4-2 |
| **rate-budget durability** | feature-flag | durable in Supabase (OQ-P4-3) | in-memory → restart refills, whole pool sprints post-deploy | pool / reputation | P4 §4.4, OQ-P4-3 |
| **rate-budget windowing algorithm** | feature-flag | token-bucket w/ durable `last_refill_at`+`tokens` (OQ-P4-3); tumbling = fallback | — | pool | P4 §4.4, OQ-P4-3 |
| **in-flight counter durability** | feature-flag | ephemeral (correct-zero post-crash) — GAP-26 | durable → phantom sessions after crash | pool | P4 §4.3, GAP-26 |
| **HELO/verifier rotation granularity** | feature-flag/dial | per-node single audited identity; multi-identity a DIAL (OQ-P4-4) | per-check rotation → multiplies DNS/PTR/SPF surface | reputation | P4 §4.5, OQ-P4-4, GAP-22 |
| **rate limits scope** (policy vs global) | feature-flag | global pool config + optional per-policy override (OQ-P4-5) | per-policy only → two policies exhaust same IP↔provider budget | pool / reputation | P4 §4.7, OQ-P4-5 |
| **`smtp_skipped_reason:"rate_limited"`** (new enum value) | feature-flag | additive to P3 enum | — | data-integrity (log) | P4 §4.6, §9 |
| **rate-saturation degrade** | feature-flag | degrade to cheap, non-cacheable, never queue/invalid (P4); queue absorbs in P5 | queue in P4 → out of scope | per-verdict | P4 §4.6 |

### 4.3 Health watcher & auto-quarantine (control-loop guards)
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **health-watcher cadence `T`** | dial | 30–60s (OQ-P4-13) | too fast → twitchy; too slow → late quarantine | pool | P4 §5.1, OQ-P4-13 |
| **block-rate window length** | dial | last 15–30 min or last K attempts (OQ-P4-13) | too short → noise; too long → slow response | pool | P4 §5.2, OQ-P4-13 |
| **block-rate numerator reasons** | feature-flag | `smtp_blocked` primary; timeout/connection_failed secondary separately-thresholded; `greylisted` NOT counted (OQ-P4-6) | counting greylist → quarantine nodes for doing their job | pool | P4 §5.2, OQ-P4-6 |
| **block-rate query filter** (`from_cache=false ∧ verifier_ip=node ∧ SMTP ran`) | feature-flag | LOCKED (PB1) | including cache hits/degrades → quarantine node for verdicts it didn't produce | pool | P4 §5.2 (PB1) |
| **`min_samples` (block-rate sample gate)** | dial | ≥20 (GAP-27) | zero → false-quarantine on 1–2 unlucky checks | pool | P4 §5.3, OQ-P4-13, GAP-27 |
| **`QUAR_THRESH`** (block-rate quarantine threshold) | dial | ~30% over ≥min_samples, confirmed 2 consecutive cycles | too low → over-quarantine | pool | P4 §5.4 |
| **`RECOVER_THRESH`** (recovery block-rate) | dial | ~5% (asymmetric, lower than QUAR) | too high → premature recovery | pool | P4 §5.4 |
| **`RECOVER_WINDOW`** (sustained-clean duration) | dial | 30–60 min | too short → flapping | pool | P4 §5.4 |
| **hysteresis (quick-quarantine / slow-recover)** | feature-flag | LOCKED asymmetric | symmetric → oscillation | pool | P4 §5.3, §5.4 |
| **`recovering` canary trickle size** | dial | few checks/window, well under warmup start (OQ-P4-7) | none → block-quarantine is a one-way door (GAP-30) | pool | P4 §5.4, OQ-P4-7, GAP-30 |
| **recovery resets durable warmup counter to low** | feature-flag | LOCKED reset (PM3) | not resetting → recovered node re-burns at full volume | single box / reputation | P4 §5.4 (PM3) |
| **liveness-fail quarantine** (M consecutive) | dial | M consecutive `/health` fails | — | single box | P4 §5.4 |

### 4.4 Fleet-wide brake (cascade protection — see kill-switch-ladder)
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **fleet-wide spike brake** (alert, don't cascade-quarantine) | feature-flag | LOCKED: do NOT auto-quarantine fleet-wide; alert + hold (OQ-P4-9) | auto-quarantine on systemic cause → cascade empties pool (auto-heal→auto-outage) | pool / brain-wide | P4 §5.5, OQ-P4-9, GAP-28 |
| **fleet-wide detection threshold** | dial | ≥50% active nodes ELEVATED (not just tripped) same window | — | pool | P4 §5.5 |
| **elevated-fraction (staggered-spike guard)** | dial | 0.6 × QuarThreshold ("climbing") counts toward the fleet signal | too high → a staggered subnet spike drains the pool one node at a time before the brake sees it (the Phase-4 review finding) | pool / brain-wide | P4 §5.5, GAP-28 |
| **cross-cycle cascade memory** | feature-flag | LOCKED: a recent block-quarantine + a fresh trip = cascade → brake (spans cycles) | single-cycle snapshot misses the staggered drain | pool | P4 §5.5, GAP-28 |
| **active-floor backstop** | feature-flag | LOCKED: block-rate quarantine NEVER drops the active pool below the serving floor | without it, block-rate could self-empty the pool even past the brake | pool / brain-wide | P4 §5.5 |

### 4.5 Topology & observability
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **single-brain assumption (rate-limiter state)** | feature-flag | single brain (OQ-P4-8); multi-brain out of scope | multi-brain → in-flight per-process, budgets race (GAP-25) | pool | P4 §4.3/4.4, OQ-P4-8, GAP-25 |
| **health-watcher process placement** | feature-flag | in-process background loop, single writer of nodes.status (OQ-P4-14) | — | pool | P4 §5.1, OQ-P4-14 |
| **node #2 = +1 config line** | feature-flag | config-only, zero code diff (Door 5 proof) | any code change → Door-5 violation (GAP-31) | pool | P4 §7, GAP-31 |
| **provisioning-script stage automation** | feature-flag/dial | stages 2/4/5/6/7 auto; spin+PTR script-with-manual-fallback (OQ-P4-10) | — | provisioning | P4 §6.2, OQ-P4-10 |
| **metrics backend** | feature-flag | both layered (LOG authoritative for control + TSDB for humans) (OQ-P4-11) | control depends on TSDB → control fails if TSDB down | observability | P4 §8.1, OQ-P4-11 |
| **alert thresholds/channels** (quarantine, block-spike, audit-red, pool-eligible-low, rate-saturation) | dial | set per §8.2 | fleet-wide block-spike must never be a mere dashboard number | observability / pool | P4 §8.2 |
| **`rate_budgets` table** | feature-flag | new operational table | — | pool | P4 §4.4, §9 |

### 4.6 Concrete config keys (this build — the env vars the P4 code reads)
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **`TM_NODE_<i>_ID` / `_BASE_URL` / `_ENABLED` / `_PUBLIC_EGRESS_IP`** (nodes #2..#N, numbered) | feature-flag | absent ⇒ N=1 (only node #1's bare `TM_NODE_*`) | the GAP-31 proof: adding node #2 is a config-only numbered block, zero brain code | pool | P4 §7, GAP-31 |
| **`TM_WATCHER_CADENCE_SECONDS`** (control-loop `T`) | dial | 45 (0 ⇒ one pass at boot, no loop) | too fast → twitchy; too slow → late quarantine | pool | P4 §5.1, OQ-P4-13 |
| **`TM_BLOCK_RATE_WINDOW_SECONDS`** (rolling block-rate window) | dial | 1800 (30 min) | too short → noise; too long → slow | pool | P4 §5.2, OQ-P4-13 |
| **`TM_BLOCK_MIN_SAMPLES`** (sample gate) | dial | 20 (zero-disallowed) | zero → false-quarantine on 1–2 unlucky checks | pool | P4 §5.3, GAP-27 |
| **`TM_BLOCK_QUAR_THRESHOLD_PCT`** (quarantine threshold %) | dial | 30 | too low → over-quarantine | pool | P4 §5.4 |
| **`TM_BLOCK_RECOVER_THRESHOLD_PCT`** (recovery threshold %) | dial | 5 (asymmetric, lower) | too high → premature recovery | pool | P4 §5.4 |
| **`TM_CANARY_RECOVER_GREENS`** (consecutive clean canary cycles) | dial | 3 | too low → flapping recovery | pool | P4 §5.4 |
| **`TM_CANARY_MIN_SAMPLES`** (min real canary attempts before promotion) | dial | 10 | zero → promote a recovering node on ZERO evidence (re-warm a still-blocklisted IP); the Phase-4 review finding | pool / reputation | P4 §5.4, GAP-30 |
| **`TM_CANARY_SAMPLE_N`** (canary domain-sample rate) | dial | 5 (≈1/N of distinct domains reach a recovering node even in a HEALTHY pool) | too high → canary starves (zero samples → stuck-forever, the Finding-B regression); too low → canary over-fed (still bounded by the low canary cap) | pool / reputation | P4 §5.4, GAP-30 |
| **`TM_LIVENESS_MISSES`** (M consecutive `/health` fails) | dial | 3 | too low → quarantine on a transient blip | single box | P4 §5.4 |
| **`TM_FLEET_WIDE_PCT`** (cascade-brake trigger, % of active nodes) | dial | 50 (VALIDATED 1..99 — a value ≥100 is REJECTED at startup, as it would make the fraction-brake mathematically unable to fire; finding G) | too high → brake never fires (auto-heal→auto-outage) — now a startup error; too low → over-holds | pool / brain-wide | P4 §5.5, GAP-28 |
| **`TM_CANARY_WARMUP_CAP`** (low re-warm cap on recovery) | dial | 20 (VALIDATED ≥ `TM_CANARY_MIN_SAMPLES` — a smaller cap makes the canary unable to accrue enough samples to promote → stuck-forever; finding C) | too high → recovered node re-burns at full volume; below min-samples → stuck-forever (rejected at startup) | single box / reputation | P4 §5.4 (PM3), GAP-30 |
| **`TM_POOL_ELIGIBLE_FLOOR`** (pool-eligible-low alert floor) | dial | 1 | — (CONSUMED: also the block-rate active-floor backstop `health.MinActiveFloor`, the "pool cannot self-empty" guarantee, main.go) | observability / pool | P4 §8.2, §5.5 |
| **`TM_RATE_SATURATION_COUNT`** (rate-saturation alert threshold) | dial | 50 (**RESERVED — read + validated, consumed once `internal/metrics` is wired**) | — | observability | P4 §8.2 |

> **`TM_RATE_SATURATION_COUNT` — reserved, not a footgun (Phase-8 dial sweep).** It is read + validated at
> load, and its sole consumer is the fully-built, unit-tested `internal/metrics` alert deriver
> (`DeriveAlerts` → `AlertThresholds.RateSaturationCount`). That observability package is **not yet wired into
> `cmd/`** (metrics backend is an integration-time seam — the LOG is authoritative for control, the TSDB/alert
> layer is layered on at deployment, P4 §8.1 OQ-P4-11). So the §8.2 rate-saturation *alert* does not fire until
> metrics is wired — the dial is **documented-reserved**, distinct from the removed `TM_AUDIT_*` aliases: its
> consumer exists and is correct, only the wiring is deferred. It is retained (not removed) because wiring the
> metrics package activates it with no further config change. (`TM_POOL_ELIGIBLE_FLOOR` shares that unwired
> alert path but is **rescued** by a second, live consumer — the block-rate active-floor backstop — so it is
> read-and-used today.)

> **Shadow-mode note (DR-2):** the block-rate quarantine (A1) reads `ShadowModes.A1BlockRateQuarantine`
> (default `recommend_only`). At the default the watcher performs **zero live block-rate mutation** —
> it recommends + alerts; audit-red (A2) and the canary (A3) stay auto. Promotion to `auto` is an
> owner action, admin-audited under DR-3.

---

## 5. PHASE 5 — The queue

### 5.1 Substrate & exactly-once (LOCKED — GAP-37/38)
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **queue substrate** | feature-flag | Supabase `work_items` + `FOR UPDATE SKIP LOCKED` (OQ-P5-1) | external broker → 2nd source of truth, reintroduces double-run (GAP-34) | data-integrity | P5 §3.1, OQ-P5-1, GAP-34 |
| **lease TTL** | dial | LOCKED: must exceed brain's max SMTP client deadline (GAP-37) | too short → reclaims a live slow attempt → double-run + state race | data-integrity | P5 §5.2, GAP-37 |
| **compare-and-set settlement** (`WHERE locked_by=me AND status=in_flight`) | feature-flag | LOCKED (GAP-37) — the CAS not the lease is the guarantee | naive lease → double-settle, verdict corruption under load | data-integrity | P5 §5.2, §5.3, GAP-37 |
| **idempotency key derivation** | feature-flag | `hash(email_hash ‖ policy_key ‖ policy_version ‖ config_fingerprint)` | wrong → double-log/double-run | data-integrity | P5 §5.1 |
| **in-flight dedup collapse** | dial | collapse-on-enqueue when cheap; CAS backstop | — | data-integrity | P5 §5.5 |
| **`work_item_attempts` transcript table** | feature-flag | yes, append-only sidecar (OQ-P5-8) | — | data-integrity (P7 join gold) | P5 §5.3, OQ-P5-8 |
| **job completion oracle** | feature-flag | authoritative `NOT EXISTS(... NOT IN settled,dead)`; `settled_count` = display only (GAP-38) | trusting counter → drifted count leaves job "running" forever | per-job | P5 §7.3, GAP-38 |
| **plaintext-at-rest in `work_items`** | feature-flag | held for item life, purged transactionally on settle (OQ-P5-9) — the one bounded Door-3 exception | later-sweep purge → crash strands plaintext | privacy / data-integrity | P5 §3.3, §5.3, OQ-P5-9, GAP-32b |
| **master invariant: queue never bypasses a gate** | feature-flag | LOCKED | bypass → bulk job sprints whole pool at every provider | pool / reputation | P5 §8, §8.1 |

### 5.2 Retry & backoff dials
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **backoff base step** | dial | 5 min (hint-seeded floor from `retry_after_hint_s`) | too fast → re-greylist; too slow → long tail | per-check | P5 §4.2, OQ-P5-2 |
| **backoff factor** | dial | ×2 exponential | — | per-check | P5 §4.2, OQ-P5-2 |
| **backoff ceiling** | dial | 60 min | — | per-check | P5 §4.2, OQ-P5-2 |
| **backoff jitter** | dial | ±10–20% | zero → thundering-herd retry wave | reputation | P5 §4.2, OQ-P5-2 |
| **`max_attempts`** | dial | 3–5 | zero → never retries; high → long-lived items | per-check | P5 §4.5, OQ-P5-2 |
| **`max_retry_age`** | dial | ~6h | too long → items (and their plaintext, §5.1) linger for days | per-check / job / privacy | P5 §4.5, OQ-P5-2 |
| **retryable transients** | feature-flag | `greylisted` primary; timeout/connection_failed secondary; `smtp_blocked` NOT retried (OQ-P5-12) | retrying smtp_blocked → confirms block, wastes budget | per-check | P5 §4.5, OQ-P5-12 |
| **`rate_limited` retry backoff (queued)** | dial | short (seconds–minute), distinct from greylist | queued degrade instead → wastes queue's purpose | per-check | P5 §4.6 |
| **`rate_limited` counts toward `max_attempts`?** | feature-flag | NO — capacity wait bounded by max_retry_age only (OQ-P5-7) | counting → busy pool burns greylisted item's retry budget | per-check | P5 §4.6, OQ-P5-7 |
| **sync greylist/rate_limited auto-queue?** | feature-flag | NO by default (OQ-P5-5) | auto-queue → blurs two-path model | per-request | P5 §3.4, §4.6, OQ-P5-5 |
| **retry node affinity (same-IP)** | feature-flag | prefer same node for retry; least-inflight only if home gone (OQ-P5-6) | different IP → provider re-greylists, restarts clock (GAP-32) | per-check / reputation | P5 §4.4, OQ-P5-6, GAP-32 |

### 5.3 Fairness, bulk & boundaries
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **group-by-domain ordering** | feature-flag | domain-spread across, paced within (vs upload order) | upload order → bursts one provider / wastes warm session | reputation | P5 §6.2 |
| **cross-job fairness** | dial | domain-spread + per-job in-flight ceiling; WFQ deferred (OQ-P5-3) | no ceiling → big job starves small | per-job | P5 §6.5, OQ-P5-3 |
| **per-job in-flight ceiling** (% of capacity) | dial | UNSET numeric (OQ-P5-3) | — | per-job | P5 §6.5 |
| **domain-batched multi-RCPT session reuse** | feature-flag | NO in P5 (OQ-P5-4); contingent on GAP-13 spike | building it → reopens never-fork-gem tension | per-check / Door 5 | P5 §6.4, OQ-P5-4 |
| **bulk max addresses-per-submission** | dial | UNSET (backpressure) | unbounded → brain/Supabase overload | brain-wide | P5 §7.5 |
| **max in-flight-jobs** | dial | UNSET | — | brain-wide | P5 §7.5 |
| **result delivery** | feature-flag | poll-only, cursor-paginated (OQ-P5-11); webhook = SEAM | — | consumers | P5 §7.1, OQ-P5-11 |
| **retry-provenance signals** (`signals.attempt`/`retry_settled`) | feature-flag | yes, additive (OQ-P5-10) | — | data-integrity (P7) | P5 §10, OQ-P5-10 |
| **`complete_with_unknowns`** job status | feature-flag | informational job-status string (not verdict vocab) | — | per-job | P5 §7.3 |
| **`jobs` retention window** | dial | days (results-fetch window), then work_items purged | — | data (operational) | P5 §7.2 |
| **`caller_ref`** (opaque platform correlation) | feature-flag | echoed, never interpreted (two-job boundary) | interpreting → platform-UI leak (GAP-36) | brain↔platform boundary | P5 §7.2, §7.6, GAP-36 |

### 5.4 Concrete config keys (this build — the env vars the P5 code reads)
These promote the §5.2/§5.3 dials from `buildQueue` code-defaults to readable config (PROJECT_RULES §6 — anything the manifest calls a dial is a config entry, never hardcoded). Backoff/attempt/age carry the §4.2/§4.5 recommended defaults; the §7.5 submission-backpressure dials are owner-OQ (**UNSET** — they read UNSET, never a guessed number; a safe built-in hard cap still bounds the surface).

| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **`TM_QUEUE_ADDR_KEY`** (+`_PREV`) (DR-4 at-rest key) | secret | UNSET ⇒ queue subsystem refuses to start (bulk 503); key ≠ pepper, 32 bytes | absent ⇒ no bulk; wrong ⇒ cipher init fails, bulk disabled | privacy / security | P5 §3.3, ADR-0011, DR-4 |
| **`TM_QUEUE_BACKOFF_BASE_SECONDS`** (attempt-0 greylist floor) | dial | 300 (5 min) | too fast → re-greylist; too slow → long tail | per-check / reputation | P5 §4.2, OQ-P5-2 |
| **`TM_QUEUE_BACKOFF_FACTOR`** (exponential factor) | dial | 2 | — | per-check | P5 §4.2, OQ-P5-2 |
| **`TM_QUEUE_BACKOFF_CAP_SECONDS`** (per-step ceiling) | dial | 3600 (60 min) | — | per-check | P5 §4.2, OQ-P5-2 |
| **`TM_QUEUE_BACKOFF_JITTER_PCT`** (± jitter %) | dial | 15 (0 legal for tests) | zero → thundering-herd retry wave | reputation | P5 §4.2, OQ-P5-2 |
| **`TM_QUEUE_RATE_WAIT_SECONDS`** (capacity-wait backoff) | dial | 30 | too long → slow drain when capacity frees | per-check | P5 §4.6 |
| **`TM_QUEUE_MAX_ATTEMPTS`** (retry ceiling) | dial | 4 (spec 3–5) | zero → never retries (rejected: needs a ceiling); high → long-lived items | per-check | P5 §4.5, OQ-P5-2 |
| **`TM_QUEUE_MAX_RETRY_AGE_SECONDS`** (time ceiling) | dial | 21600 (6h) | too long → items (and their plaintext, §5.1) linger | per-check / privacy | P5 §4.5, OQ-P5-2 |
| **`TM_QUEUE_LEASE_MARGIN_SECONDS`** (lease TTL = maxSMTPdeadline + this) | dial | 120 (2 min); VALIDATED > 0 (GAP-37: TTL must EXCEED the deadline, else a slow attempt loses its lease and is double-run) | ≤0 rejected at startup; too small → live attempt reclaimed under load | data-integrity | P5 §5.2, GAP-37, X-7 |
| **`TM_QUEUE_DRAIN_BATCH_SIZE`** (items per drain tick) | dial | 10 | too high → burst; too low → slow drain | pool | P5 §6 |
| **`TM_QUEUE_DRAIN_IDLE_POLL_SECONDS`** (idle sleep) | dial | 2 | too high → latency; too low → busy-poll | brain-wide | P5 §3 |
| **`TM_QUEUE_BULK_MAX_ADDRESSES`** (submission cap, §7.5) | dial | **UNSET — owner OQ** (built-in hard cap 100k applies until set) | unbounded upload → brain/DB overload (bounded by the hard cap even when UNSET) | brain-wide | P5 §7.5 |
| **`TM_FRONTDOOR_MODE`** (kill-switch Rung 4 — the pause) | feature-flag | `serving`; `accept_and_defer` = bulk still accepted, drainer STOPPED; `reject` = nothing accepted. Both pause sub-modes also 503 the sync `/v1/verify` (no deferred form of a synchronous answer) and disclose `processing_paused` on the bulk envelopes ([ADR-0015](adr/ADR-0015-rung4-pause-enforcement.md)) | left at a pause sub-mode → backlog grows in `work_items` with nothing draining; read at boot only, so a stale value survives until restart | brain-wide (availability) | kill-switch Rung 4, ADR-0015, audit D2 |

> **Not-yet-wired backpressure dials (§7.5 max-in-flight-jobs, §6.5 per-job-in-flight-% / WFQ):** these
> stay in the §8.2 "no default anywhere" UNSET inventory and are **deliberately NOT settable config yet** —
> there is no enforcement path (per-job concurrency / weighted-fair-queuing is a deferred SEAM, OQ-P5-3),
> and a settable-but-ignored env var is a silent footgun (P5→P6 review finding). They are wired by the
> phase that actually enforces them, not before. Only the *consumed* bulk-max dial above is promoted.

---

## 6. PHASE 6 — Third-party fallback

### 6.1 Secret & layer wiring
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **vendor API key** (per adapter) | secret | UNSET — held brain-side in secrets, rotated (§7.4) | missing → layer disabled; leaked → vendor account abuse + budget burn + enumeration oracle | brain-wide / cost | P6 §7.2, §7.4, §8.3 |
| **`external` layer enabled** (per policy) | feature-flag | enabled in cold-email (OQ-P6-2); a policy without the block = **the privacy opt-out lever** | disabled → residue settles internally (more unknown/risky) | per-policy / privacy | P6 §3.2, §7.5, OQ-P6-6 |
| **`external` layer position in `order`** | feature-flag | LAST (most expensive, runs only on residue) | earlier → escalates non-residue, blows budget | cost | P6 §3.1 |
| **`vendor`** selector (adapter) | feature-flag | `millionverifier` (OQ-P6-8, one at launch); swappable | — | brain-wide | P6 §3.2, §8, OQ-P6-8 |
| **one vendor vs two (A/B)** | feature-flag | one at launch (OQ-P6-8); multi-vendor = SEAM P7 | — | brain-wide | P6 §8.4, OQ-P6-8 |

### 6.2 Anti-laundering & verdict mapping (data-integrity — protects P7 calibration)
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **`confidence_cap`** (external valid) | dial | 60 (anti-laundering) — a **per-policy** key in the policy JSON (`external.confidence_cap`), NOT an env var; the runtime reads it from the policy (external.Resolve). Different policies may cap differently. | uncapped/SMTP-grade → poisons P7 calibration with vendor errors (GAP-41) | data-integrity / calibration | P6 §3.2, §3.4, OQ-P6-1 |
| **external `valid` → `risky` (never SMTP-grade)** | feature-flag | LOCKED risky (OQ-P6-1) | minting `valid` → launders vendor uncertainty, breaks weaning | data-integrity | P6 §3.4, OQ-P6-1 |
| **vendor `invalid` acceptance (asymmetric)** | feature-flag | accept as invalid only on retry-exhausted/gave-up-capacity; downgrade to risky on catch-all/`:mx` (OQ-P6-3) | accepting on catch-all → a guess irreversibly discards a real address | data-integrity / suppression | P6 §3.4, §8.2, OQ-P6-3 |
| **`route_when` residue classes** | feature-flag/dial | all four at launch (catch_all, provider_no_smtp, retry_exhausted_greylisted, gave_up_rate_limited) (OQ-P6-2); may not add a 5th | add non-residue → cost blowout | cost | P6 §3.2, §4.1, OQ-P6-2 |
| **vendor normalization table** (raw status → system vocab) | dial | per §8.2; unrecognized → `unknown` (never guess) | mapping new status to valid → unsafe direction | per-verdict | P6 §8.2 |
| **`on_vendor_unknown` / `on_budget_exhausted` / `on_vendor_error`** | feature-flag | `keep_residue` (never fabricate, never worse than P5) | anything else → fabricated verdict | per-verdict | P6 §3.2, §5.3 |
| **escalation decided once at verdict-mint (not on cache read)** | feature-flag | LOCKED (GAP-43) | re-escalate on cache read → re-bill re-uploaded lists | cost / data-integrity | P6 §4.3, GAP-43 |
| **cache the `external_verified` verdict** | feature-flag | yes (settled), same key; excluded from calibration when from_cache (OQ-P6-9) | — | brain-wide | P6 §4.3, OQ-P6-9 |

### 6.3 Cost control & circuit breaker
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **`daily_cap`** (external spend) | dial | UNSET — start conservative (OQ-P6-5) | exhausted → breaker open, keep_residue; too high → runaway bill | cost | P6 §5.2, OQ-P6-5 |
| **`monthly_cap`** | dial | UNSET conservative | — | cost | P6 §5.2, OQ-P6-5 |
| **`per_job_cap`** | dial | UNSET — low enough one list can't drain global (OQ-P6-5) | none → pathological list drains budget | cost | P6 §5.2, §4.4, OQ-P6-5 |
| **`rate_ceiling`** (vendor calls/min) | dial | UNSET | none → cost spike / trips vendor rate limit | cost | P6 §5.2, OQ-P6-5 |
| **circuit-breaker trip conditions** (budget / error-rate / vendor-down) | feature-flag/dial | three conditions, per-vendor state (§5.3) | no breaker → pay for degraded vendor / cascade | cost / brain-wide | P6 §5.3 |
| **vendor error-rate trip threshold + cooldown** | dial | UNSET | — | cost | P6 §5.3 |
| **breaker = brake not cascade** | feature-flag | LOCKED alert-and-degrade, never auto-disable policy/fail jobs (§5.5) | auto-disable → self-inflicted outage | brain-wide | P6 §5.5 |
| **spend idempotency (CAS on lease-holder)** | feature-flag | LOCKED — vendor call + cost-decrement + settle under same CAS (GAP-42) | naive → double-charge under P5 reclaim | cost / data-integrity | P6 §5.4, GAP-42 |
| **vendor idempotency key** | feature-flag/dial | use where supported (`email_hash ‖ vendor`) (OQ-P6-4) | absent → one extra billed lookup under reclaim | cost | P6 §5.4, §8.3, OQ-P6-4 |

### 6.4 Privacy & egress (see cross-phase report §security)
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **plaintext egress to vendor** | feature-flag | address-only, residue-only, DPA-gated, TLS; reuses P5 transient plaintext (§7.2/7.3) | send more than address → over-exposure (GAP-40/45) | privacy / security | P6 §7.2, §7.3, GAP-40 |
| **vendor DPA / due-diligence gate** | feature-flag | layer ships DISABLED until checklist passes (§7.4) | enabling pre-DPA → unlawful sub-processing | privacy / legal | P6 §7.4 |
| **platform disclosure posture** | feature-flag | disclose + opt-out (options 1+2) (OQ-P6-6); per-policy opt-out mechanical | silent egress → breaks platform privacy promise (GAP-45) | cross-project / legal | P6 §7.5, OQ-P6-6, GAP-45 |
| **sync-residue: inline vs async-requeue** | feature-flag/dial | inline with tight timeout, keep_residue on timeout (OQ-P6-7); async = opt-in SEAM | — | per-request | P6 §2.2, OQ-P6-7 |

### 6.5 Concrete config keys (this build — the env vars the P6 code reads)
These are the actual environment variables `internal/config` reads for the external-fallback layer.
The layer is built DISABLED; egress requires **three** affirmative switches (`TM_FALLBACK_MODE=enabled`
AND `TM_EXTERNAL_DPA_CONFIRMED=true` AND a `TM_VENDOR_KEYS` entry), and the global-fallback-kill
(`TM_FALLBACK_MODE=off`) overrides all of them. `TM_FALLBACK_MODE` is the pre-existing kill-switch
Rung-2 dial (see §1 kill-switch); `TM_VENDOR_KEYS` is the pre-existing vendor-key secret slot.

| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **`TM_EXTERNAL_DPA_CONFIRMED`** (§7.4 legal enablement gate) | feature-flag | **false** — layer stays disabled; only `"true"` enables (after the DPA checklist passes) | true pre-DPA → unlawful sub-processing; the AND-gate + global-kill still guard | privacy / legal | P6 §7.4 |
| **`TM_EXTERNAL_DAILY_CAP`** (cost-units/day) | dial | **UNSET — owner OQ** (0/unset ⇒ not enforced) | unbounded day spend when unset + enabled; too low → early keep_residue | cost | P6 §5.2, OQ-P6-5 |
| **`TM_EXTERNAL_MONTHLY_CAP`** (cost-units/month) | dial | **UNSET — owner OQ** | — | cost | P6 §5.2, OQ-P6-5 |
| **`TM_EXTERNAL_PER_JOB_CAP`** (cost-units per bulk job) | dial | **UNSET — owner OQ** | none → a 90%-catch-all list drains the global budget (§4.4) | cost | P6 §5.2, §4.4, OQ-P6-5 |
| **`TM_EXTERNAL_RATE_CEILING`** (vendor calls/min) | dial | **UNSET — owner OQ** | none → cost spike / trips the vendor's own rate limit | cost | P6 §5.2, OQ-P6-5 |
| **`TM_EXTERNAL_ERROR_RATE_PCT`** (breaker cond-2 trip %) | dial | 0 (error-rate condition off; budget ceilings still apply) | too low → trips on noise; disabled → pay for a degraded vendor until a budget cap | cost | P6 §5.3 |
| **`TM_EXTERNAL_ERROR_MIN_SAMPLES`** (error-rate sample gate) | dial | 20 | zero → trips on 1–2 unlucky calls (GAP-27) | cost | P6 §5.3, GAP-27 |
| **`TM_EXTERNAL_BREAKER_COOLDOWN_SECONDS`** (open→half-open) | dial | 300 (5 min) | too short → flapping; too long → slow recovery | cost | P6 §5.3 |

---

## 7. PHASE 7 — The flywheel

> **All three P7 automated actions (recalibration apply, provider auto-promotion, fallback weaning) default to recommend-only.** See [`shadow-mode.md`](shadow-mode.md). The dials below set *what* they compute; shadow-mode governs *whether they act*.

### 7.1 Ingestion & join integrity (LOCKED — data-integrity heart)
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **outcome transport** | feature-flag | sender-owned append feed P7 ingests (OQ-P7-1) | shared table → two-writer hazard; **unauthenticated → poisoning surface (security F-15)** | data-integrity / security | P7 §3.2, OQ-P7-1, GAP-48 |
| **ingestion idempotency key** | feature-flag | `(message_id, event)` dedup, no-op append-guard | absent → double-count bounce into bucket | data-integrity (calibration) | P7 §3.4 |
| **integrity reject-and-flag** (unknown verification_id / sent_at<created_at / malformed) | feature-flag | reject + `ingest_error`, exclude from calibration (GAP-51) | guessing → poisoned join | data-integrity | P7 §3.3, §3.4, GAP-51 |

### 7.2 Bounce classifier & gates (LOCKED taxonomy — GAP-49)
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **bounce classifier three-class map** | feature-flag | LOCKED: mailbox_not_found→invalid-signal; mailbox_full→valid-signal; blocked/policy/transient→zero-information | naive "bounce=invalid" → permanently poisons calibration (GAP-49, stuck-bucket) | data-integrity (calibration) | P7 §4.2, §4.3, GAP-49 |
| **DSN-code → event mapping** | dial | provider-specific, tunable; three-class taxonomy LOCKED | mis-map → wrong calibration class | data-integrity | P7 §4.3 |
| **evidence-strength weights** | dial | ordering LOCKED (mailbox_full ≥ discriminating-delivery > catch-all-delivery ≈ 0); weights DIAL | delivered=valid naive → over-trusts silent-drop channels | calibration | P7 §4.5 |
| **calibration exclusions** (from_cache, override, external-separate, disposable-risky, zero-information) | feature-flag | LOCKED, consumed from P1/P6 | blending → trains on cache staleness / vendor errors / sender faults | data-integrity | P7 §4.4, §5.3, GAP-54 |
| **historical-bounce gate** (`historical_bounce:pass → invalid:historical_bounce:stop`) | feature-flag | reserved reason, TTL-windowed (OQ-P7-2 DECIDED) | fires on zero-info bounce → permanently mislabels real address | per-verdict | P7 §4.6, OQ-P7-2 |
| **historical-bounce TTL** | dial | 6–12 months on confirmed mailbox_not_found only, then degrades to penalty (OQ-P7-2) | no TTL → one bounce permanently blacklists a reclaimed mailbox (PB2) | per-verdict / reversibility | P7 §4.6, OQ-P7-2, App C PB2 |

### 7.3 Recalibration store & feedback-loop safety (LOCKED — GAP-50/53)
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **calibration bucket dimension** | feature-flag | `(reason × mx_provider)`; external parallel-separate by `(vendor, residue_class, provider)` | merging external → can't wean (GAP-54) | calibration | P7 §5.2, GAP-54 |
| **calibration store** (separate, versioned, effective-dated) | feature-flag | new table, never mutates verifications (Door 2) | writing to LOG → append-only violation; not versioned → no rollback | data-integrity | P7 §5.4 |
| **`confidence` recalibration** (numbers only) | dial | replaces P1 baseline per bucket; int 0–100 unchanged | mint new reason/status → Door-4 break | brain-wide (calibration) | P7 §5.1, §5.2 |
| **recalibration CAS-on-lease-holder** | feature-flag | LOCKED (GAP-53) — the calibration writer is a lease-and-settle path | slow cycle double-applies bucket update | data-integrity | P7 §5.5, GAP-53 |
| **cold-start fallback to P1 baseline** | feature-flag | uncalibrated buckets use P1 baseline (§5.6) | claiming moat at ship time → overclaim (GAP-52) | brain-wide | P7 §5.6, §14.2, GAP-52 |
| **`min_bucket_samples`** (sample-size gate) | dial | start 500 (build-plan ~200–500) (OQ-P7-3) | zero → small-N noise moves buckets (GAP-27 analogue) | calibration | P7 §6.2, OQ-P7-3 |
| **`max_step`** (bounded step per cycle) | dial | ≤10–15 points/cycle | large → one bad window captures a bucket | calibration | P7 §6.3 |
| **hysteresis (down-fast / up-slow)** | feature-flag | LOCKED asymmetric — doubt applied fast, trust earned slow | symmetric → runaway/oscillation (GAP-50) | calibration | P7 §6.3 |
| **cascade brake** (system-wide swing → alert, not auto-apply) | feature-flag | LOCKED: hold + human confirm; revert via versioned store | auto-apply systemic swing → mass mis-calibration | brain-wide (calibration) | P7 §6.4, GAP-50 |
| **cascade-brake "system-wide swing" definition** | dial | e.g. >N% high-volume buckets moving >max_step | — | calibration | P7 §6.4 |
| **recalibration cadence** | dial | daily/weekly rollup, not per-outcome (OQ-P7-7) | per-outcome → twitch, meaningless per-cycle bound | calibration | P7 §5, OQ-P7-7 |

### 7.4 Promotion, accuracy & weaning (all recommend-only by default)
| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **`promote_threshold`** (provider auto-promote to `:mx`-only, #4) | dial | ≤~1–2% mailbox_not_found on valid-labels (OQ-P7-4) | too high → promote untrustworthy provider | pool / reputation | P7 §7.2, OQ-P7-4 |
| **`min_promote_samples`** | dial | UNSET (sample-gated) (OQ-P7-4) | too low → promote on untested provider | pool | P7 §7.2, OQ-P7-4 |
| **`demote_threshold`** (hysteretic) | dial | higher than promote (quick-demote/slow-promote) (OQ-P7-4) | — | pool | P7 §7.2, OQ-P7-4 |
| **accuracy metric target (#5)** | dial | UNSET — owner-set per provider, publish with coverage (OQ-P7-5) | bare global number → vanity overclaim | reporting / trust | P7 §8.1, §8.2, OQ-P7-5 |
| **weaning rule** (drop fallback per residue×provider) | feature-flag | when own measured accuracy beats vendor's, both sample-gated; reversible | wean on noise/parity → drop coverage prematurely | cost / coverage | P7 §9.2 |
| **weaning safety margin** | dial | margin not parity (OQ-P7-6) | parity → wean on noise | cost / coverage | P7 §9.2, OQ-P7-6 |
| **`unknown` not pressured to `valid`** | feature-flag | LOCKED — first-class unknown survives flywheel | eroding → relitigates founding discipline | brain-wide | P7 §8.4 |
| **platform "your sends train our accuracy" disclosure** | feature-flag | surface it (OQ-P7-8), disclose-don't-hide | silent → cross-project privacy contradiction | cross-project | P7 §14, OQ-P7-8 |

### 7.5 Concrete P7 env vars (as-built, local flywheel — [`internal/config`](../control-plane/internal/config/config.go) + [`cmd/control-plane`](../control-plane/cmd/control-plane/main.go))

> As-built rows for the P7 build. The two LIVE-PATH touches (the §4.6 gate + the §5.4 calibrated read) default **OFF** — a fresh P7 system behaves identically to end-of-P6 (the DR-2 fresh-system invariant); enabling each is an explicit `=true`. The recalibration guards carry the spec's conservative defaults; the brake fraction/window/up-step are **range-validated at startup** (out-of-range → startup error, never a silent disable — the P4 lesson-b, [`config.Validate`](../control-plane/internal/config/config.go)). Live ingestion is **PENDING-REAL-SENDER** (ADR-0013 — the mTLS feed needs the sender; local build uses `SimulatedSource`).

| name | type | default | mis-setting effect | blast radius | source |
|---|---|---|---|---|---|
| **TM_FLYWHEEL_GATE_ENABLED** | feature-flag | `false` (OFF — fresh-system invariant) | enabling before an owner opts in → verdicts short-circuit on historical bounces prematurely | per-verdict | P7 §4.6, DR-2 |
| **TM_FLYWHEEL_CALIBRATION_ENABLED** | feature-flag | `false` (OFF — P1 baseline stands) | enabling → verdicts read APPROVED calibration instead of the baseline (still inert until a version is approved) | brain-wide (confidence) | P7 §5.4, DR-2 |
| **TM_HISTORICAL_BOUNCE_TTL_DAYS** | dial | 270 (9mo, within OQ-P7-2's 6–12mo) | 0 → gate inert; too long → a reclaimed mailbox stays blacklisted longer | per-verdict / reversibility | P7 §4.6, OQ-P7-2 |
| **TM_HISTORICAL_BOUNCE_MIN_N** | dial | 2 (F-17/PN-11 — one forged outcome can't blacklist) | 1 → a single forged/mislabeled bounce suppresses a real address for the TTL | per-verdict / security | P7 §4.6, F-17, PN-11 |
| **TM_HISTORICAL_BOUNCE_PENALTY** | dial | 30 (post-TTL confidence penalty, recoverable) | too high → a re-created mailbox over-penalized | per-verdict | P7 §4.6 |
| **TM_RECAL_MIN_BUCKET_SAMPLES** | dial | 500 (OQ-P7-3 build-plan upper) | too low → recalibrate on small-N noise (GAP-27 analogue) | calibration | P7 §6.2, OQ-P7-3 |
| **TM_RECAL_MAX_STEP** | dial | 10 (points/cycle) | too high → one bad window captures a bucket | calibration | P7 §6.3 |
| **TM_RECAL_UP_STEP_PCT** | dial | 50 (up moves at half rate — down-fast/up-slow) | >100 → inverts the hysteresis asymmetry (rejected at startup) | calibration | P7 §6.3 |
| **TM_RECAL_BRAKE_WINDOW_CYCLES** | dial | 3 (>1 catches STAGGERED drift — P4 lesson-a) | 0 → disables the brake (rejected at startup) | brain-wide (calibration) | P7 §6.4 |
| **TM_RECAL_BRAKE_BUCKET_PCT** | dial | 50 (must be 1..99) | outside 1..99 → brake can never fire, silently disabled (rejected at startup — P4 lesson-b) | brain-wide (calibration) | P7 §6.4 |
| **TM_RECAL_BRAKE_MIN_BUCKETS** | dial | 20 (denominator floor) | 0 → brake trips on one noisy bucket | calibration | P7 §6.4 |
| **TM_RECAL_CADENCE_SECONDS** | dial | 86400 (daily rollup, OQ-P7-7) | per-outcome → twitch, meaningless per-cycle bound | calibration | P7 §5, OQ-P7-7 |

---

## 8. CROSS-PHASE FLAGS

These are surfaced for owner attention — **flagged, not resolved.** None rewrites a frozen spec.

### 8.1 Same knob / same instinct, two phases — divergent names or defaults (contradiction candidates)
| # | The two (or three) knobs | Why flagged | Cites |
|---|---|---|---|
| X-1 | **Warmup cap** (P3, one global ascending per-node cap) vs **per-provider rate limiter** (P4, per node×provider). | Both are "how fast a node may send," different scope/name/default. They *coexist* (P4 effective allowance = `min(warmup headroom, rate budget)`) but a reader can conflate them. | P3 §5.2 vs P4 §4.1–4.2 |
| X-2 | **Auto-quarantine trigger**: P3 audit-driven **first-red** vs P4 block-rate-driven **sample-gated hysteretic**. | Same "quarantine" verb, two trigger definitions on one state machine (SVG-⑦, GAP-18). Must be reconciled as one machine with a trigger *set*. | P3 §4.5 vs P4 §5.4 |
| X-3 | **"Slow-to-trust / quick-to-distrust" hysteresis** implemented three times: P3 N-green recovery, P4 RECOVER_THRESH+WINDOW+canary, P7 confidence up-slow. | One principle, three independent dial sets, no shared config. | P3 §4.5 · P4 §5.4 · P7 §6.3 |
| X-4 | **Brake pattern** implemented twice: P4 fleet-wide block-spike (alert, don't cascade-quarantine) and P7 systemic calibration swing (alert, don't auto-apply). | Same "err toward alert" brake with different trigger definitions and no shared config primitive. **This is the pattern shadow-mode generalizes.** | P4 §5.5 · P7 §6.4 |
| X-5 | **Sample-size gate**: P4 `min_samples` (≥20, block-rate) vs P7 `min_bucket_samples` (~500, calibration). | Same instinct, wildly different defaults (20 vs 500) for different denominators — intentional but confusable. | P4 §5.3 · P7 §6.2 |
| X-6 | **Lease+CAS exactly-once**: P5 settle-once, P6 spend-once, P7 calibration-write-once. | One invariant (lease TTL > max op deadline + CAS on holder), three sites, no shared config primitive. The load-bearing cross-phase invariant. | P5 GAP-37 · P6 GAP-42 · P7 GAP-53 |
| X-7 | **Latent numeric coupling**: P5 lease TTL is LOCKED to "exceed the brain's max SMTP client deadline" — a value P2 §6.5 introduces but never numbers. | A LOCKED dependency anchored to an UNSET value. The SMTP deadline must be set *before* the lease TTL can be. | P2 §6.5 → P5 §5.2 |
| X-8 | **`smtp_skipped_reason` enum** grows across P3 (4 values) + P4 (`rate_limited`). | Additive-by-design (open string enum), but the complete value set is defined across two phases. | P3 §8 · P4 §9 |
| X-9 | **`retry_after_hint_s`** introduced P2 (parsed 4xx hint, "policy default" unspecified) but its default number lives in P5 (backoff floor 5 min). | Knob and its default live in different phases. | P2 §6.5 · P5 §4.2 |

### 8.2 Dials with no default anywhere (UNSET — owner decisions, see pre-takeoff-checklist)
- **Secrets (values):** email-hash pepper (P1 OQ-1), front-door token(s) (P1 OQ-13), node tokens (P1), Supabase service-role key, vendor API key (P6 §7.4).
- **P1:** cache TTL (OQ-9), confidence `flag_deltas`, spam_trap list source (OQ-17).
- **P2:** brain client-side SMTP deadline (§6.5 — and X-7 above), catch-all re-probe cadence (§5.6).
- **P3:** warmup ramp schedule (OQ-P3-10, illustrative only), re-audit cadence / staleness / N-green counts (§4.5, recommendations only).
- **P4:** `max_inflight`, `max_starts_per_window`, jitter spread, `QUAR_THRESH`, `RECOVER_THRESH`, `RECOVER_WINDOW`, `min_samples`, fleet-wide %, watcher `T`, block-rate window, canary trickle size, liveness `M` (all §4.2/§5.2–5.5, illustrative).
- **P5:** per-job in-flight ceiling %, bulk max-addresses-per-submission, max in-flight-jobs, `jobs` retention (§6.5/§7.5/§7.2).
- **P6:** `daily_cap`, `monthly_cap`, `per_job_cap`, `rate_ceiling`, vendor error-rate trip threshold + cooldown (§5.2–5.3, "start conservative").
- **P7:** `min_promote_samples`, `demote_threshold`, accuracy target (#5), weaning safety margin, cascade-brake swing threshold, `max_step` exact value, recalibration cadence (§6–9).

### 8.3 Secret custody roll-up (detail in cross-phase-consistency-report §Security)
| secret | custody rigor | gap |
|---|---|---|
| **email-hash pepper** | HIGHEST — own tested backup/custody runbook, geo-separate access-audited backups, recovery tested pre-P1, rotation additive-only "assume never rotated" (P1 §3.3.3, App C) | brain holds it in memory to compute HMACs → a brain core-dump is an unaddressed leak vector; read-surface grows silently as P5/P7 add `email_hash`-derived stores |
| **node token** (brain→box) | HIGH — secrets mgr by-id, never plaintext in `nodes`, per-node unique, revoke-on-burn (P1 §4.5, P3 §6.4, P4 §6) | no rotation *cadence*; revoke-vs-drain ordering at queue scale unspecified (security F-1) |
| **vendor API key** (P6) | HIGH — brain-side secrets, per-adapter, layer disabled until DPA passes (P6 §7.2/§7.4) | no rotation story; leak = budget-DoS + billed enumeration oracle (security F-6) |
| **front-door bearer token** | PARTIAL — scheme decided (bearer), storage "in config/secrets" only (P1 §5.2, OQ-13) | no per-caller scoping/rotation brain-side; guards bulk-PII endpoint by P5 (security F-20) |
| **Supabase service-role key** | PARTIAL by exclusion — box holds none; brain's own storage asserted but not runbook-level (P1 OQ-20) | the sole credential gating all state + the append-only grant (security F-11/F-13) |

---

*End of config manifest. Companion analysis of what these knobs imply for safety and rollout is in the four companion docs.*
