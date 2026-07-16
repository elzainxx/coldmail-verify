# Config reference (GENERATED — do not edit by hand)

**Generated from** [`config-manifest.md`](config-manifest.md) **by** `scripts/gen-config-reference.sh`.
Regenerate on any manifest change; CI verifies this file is up to date
(PROJECT_RULES.md §8 — docs staleness is a failing check).

> This is the always-current index of **what is toggleable**. The manifest remains the
> authoritative source (with blast-radius, mis-setting effects, and full rationale); this
> reference is the derived, at-a-glance list. Type vocabulary: **secret** (custody matters),
> **feature-flag** (on/off or mode/selector), **dial** (a tunable number/window/TTL).

## Counts

| bucket | count |
|---|---|
| secrets | 8 |
| feature-flags | 137 |
| dials | 79 |
| other/uncategorized | 2 |
| **total knobs** | **226** |

> One-way doors are **not** dials: the frozen contracts (response shape, LOG, hashing,
> vocabulary, never-fork) live in code (`control-plane/internal/{vocab,hash,verdict,store,router}`),
> not in this toggle list. A dial that *feeds* a door (e.g. a `confidence` number vs. the
> `confidence` int-0–100 *type*) is a dial; the type it feeds is frozen.

## Secrets (8)

| knob | default / state | source |
|---|---|---|
| email-hash pepper (global HMAC-SHA256 key) | scheme LOCKED; value UNSET (OQ-1 ELEVATED) — single long-lived global pepper | P1 §3.3.3, OQ-1, App C |
| front-door bearer token(s) (caller → brain) | scheme decided (bearer, OQ-13); P1 accepts static token from config | P1 §3.1.1, §3.5.4, §5.2, OQ-13 |
| node token / rack `ACCESS_TOKENS` (brain → box) | UNSET value; in secrets mgr, referenced by id, never plaintext in `nodes` | P1 §3.5.4, §4.5, OQ-13 |
| Supabase service-role key | service-role only in P1; RLS deferred (OQ-20) | P1 §4, OQ-20 |
| vendor API key (per adapter) | UNSET — held brain-side in secrets, rotated (§7.4) | P6 §7.2, §7.4, §8.3 |
| node token (brain→box) | no rotation *cadence*; revoke-vs-drain ordering at queue scale unspecified (security F-1) | no rotation *cadence*; revoke-vs-drain ordering at queue scale unspecified (security F-1) |
| vendor API key (P6) | no rotation story; leak = budget-DoS + billed enumeration oracle (security F-6) | no rotation story; leak = budget-DoS + billed enumeration oracle (security F-6) |
| front-door bearer token | no per-caller scoping/rotation brain-side; guards bulk-PII endpoint by P5 (security F-20) | no per-caller scoping/rotation brain-side; guards bulk-PII endpoint by P5 (security F-20) |

## Feature-flags (137)

| knob | default / state | source |
|---|---|---|
| LOG-every-result (incl. cheap layers + cache hits) | LOCKED on | P1 §3.2.1, §6.10 |
| `confidence` type/range | LOCKED int 0–100 (CHECK constraint) | P1 §3.1.3, §4.1 |
| local-part lowercasing (norm/v1) | LOCKED yes (OQ-3, RFC-lossy accepted) | P1 §3.3.2, OQ-3 |
| cache key shape `(email_hash, config_fingerprint)` | LOCKED composite (OQ-9) | P1 §4.2, App B |
| `config_fingerprint` input set | LOCKED input list; SHA-256 canonical | P1 §3.2.5, OQ-11 |
| status / reason / recommendation vocabulary | LOCKED enum, additive-only | P1 §3.4.3 |
| active policy per request | `cold-email` v1 | P1 §3.1.1, §7.3 |
| `policies.is_default` single-default | one system-wide default (OQ-23), partial unique index | P1 §4.6, OQ-23 |
| policy `order` (which layers run, in sequence) | LOCKED shape / DIAL contents | P1 §7.2 |
| policy definition = open object | open/extensible, tolerate unknown keys (OQ-25) | P1 §7.2, OQ-25 |
| `gates.syntax:fail` | `invalid:invalid_syntax:stop` | P1 §7.2 |
| `gates.mx:fail` | `invalid:no_mx:stop` | P1 §7.2 |
| `gates.null_mx:pass` | `invalid:null_mx:stop` | P1 §7.2 |
| `gates.spam_trap:pass` | `invalid:spam_trap:stop` | P1 §7.2 |
| `gates.mx:unknown` | `unknown:dns_error:stop` | P1 §7.2 |
| `gates.disposable:pass` | `risky:disposable:stop` (OQ-7 OVERRULED from invalid) | P1 §7.2, §3.4.3, App C |
| `recommendation_overrides_by_reason.disposable` | `skip` | P1 §7.2 |
| `provider_routing` map (google/yahoo/o365 → mx) | `{google:mx, yahoo:mx, o365:mx}` | P1 §7.2, §6.9 |
| cheap-layer exec: brain-native vs mocked-engine | brain-native for syntax + MX/DNS/null-MX (OQ-6) | P1 §6.1, §6.5, OQ-6 |
| node registry source of truth | config-list authoritative; `nodes` table = mirror (OQ-5) | P1 §5.3, §4.5, OQ-5 |
| router strategy | `single`/`random` at N=1 (via Strategy interface) | P1 §3.5.1, §5.4 |
| `nodes.enabled` | operator on/off | P1 §4.5 |
| API versioning transport | URI path `/v1/` (OQ-12) | P1 §3.1.1, OQ-12 |
| `request_id` origin | server-generated, client override allowed (OQ-21) | P1 §3.2.7, OQ-21 |
| Supabase access model | service-role only in P1, RLS deferred (OQ-20) | P1 §4, OQ-20 |
| `/verify` echo raw vs normalized email | echo raw (OQ-4) | P1 §3.3.7, OQ-4 |
| `provider_canonical_hash` populate | define + populate from row #1 (OQ-2) | P1 §3.3.5, OQ-2 |
| `smtp_safe_check` (engine) | OFF (OQ-P2-7) | P2 §6.8, OQ-P2-7, App A.1 |
| `smtp_fail_fast` (engine) | false (gem default) | P2 §6.7, App A.1 |
| `smtp_port` (engine) | 25 (mock port in P2) | P2 §6.7, App A.1 |
| `dns` resolver (engine) | `[]` / `['ip:port']` (dns_mock in P2) | P2 §3.2, App A.1 |
| `not_rfc_mx_lookup_flow` (engine) | false | P2 App A.1 |
| `552` mailbox-full → valid | `valid`/high-confidence (owner OVERRIDE from risky) | P2 §6.1.1, App D |
| `smtp:*` policy gate entries (valid/invalid/catch_all/mailbox_full/unknown) | per §7.3 map | P2 §7.3 |
| SMTP enabled in default policy (P2) | enabled, routed to mock node (OQ-P2-8) | P2 §7.3, OQ-P2-8 |
| catch-all probe on/off (`probe_catchall`) | true for `:smtp` (§5) | P2 §4.1, §5.5 |
| catch-all probe location (Option A same-session vs B two-session) | Option A, CONDITIONED on GAP-13 spike (OQ-P2-4) | P2 §5.4, OQ-P2-4/11 |
| cache-only-settled-verdicts predicate | LOCKED: never cache transient/your-fault unknown (OQ-P2-6) | P2 §6.5, OQ-P2-6 |
| wrapper endpoint namespacing | `/v1/` on the box (OQ-P2-1) | P2 §4.0, OQ-P2-1 |
| `/config/reload` push vs pull | PULL (OQ-P2-2) | P2 §4.5, OQ-P2-2 |
| store raw transcript verbatim | verbatim (OQ-P2-5); sidecar holds verifier identity + probe addrs | P2 §6.4, OQ-P2-5 |
| `:smtp`-eligibility rule `green AND port25_open` | LOCKED both required | P3 §3.4, §4.4, GAP-15 |
| `nodes.port25_open` (egress smoke-test result) | set by egress test, not by audit | P3 §3.4, §4.3 |
| `nodes.audit_status` green/red | from `warnings=={}` | P3 §4.3, §4.4 |
| auto-quarantine trigger (first-red vs N-red) | quarantine on FIRST red (OQ-P3-7) | P3 §4.5, OQ-P3-7 |
| warmup counter durability | durable in Supabase (per-node/per-window) | P3 §5.2 (PM4) |
| `validation_type_for` map (engine backstop) | gmail/googlemail/outlook/hotmail/live/o365/yahoo → mx; default smtp | P3 §5.1, OQ-P3-8 |
| routing authority (brain vs engine) | brain authoritative; engine map = generated backstop (OQ-P3-8) | P3 §5.1, OQ-P3-8, GAP-19 |
| O365 routing | `:mx` (affirmed, OQ-P3-9); promote only on P7 data | P3 §5.1, OQ-P3-9 |
| no-eligible-node degrade | degrade to cheap verdict, never invalid (OQ-P3-6) | P3 §4.4, OQ-P3-6 |
| provisional-fallback non-cacheable rule | capacity-gap degrades non-cacheable; `provider_mx_routed` cacheable (PB1) | P3 §4.4 (PB1) |
| `smtp_skipped_reason` signal enum | `no_eligible_node\ | P3 §4.4, §8 |
| egress-IP == PTR-IP == verifier_ip == audit-IP invariant | LOCKED all-one (public egress IP) | P3 §6.6, §6.6.1, GAP-24 |
| `verifier_ip` sourcing | brain registry authoritative, cross-checked vs host_audit.current_host_ip (OQ-P3-11) | P3 §5.4, OQ-P3-11 |
| verifier domain isolation | dedicated/isolated subdomain (OQ-P3-2) | P3 §3.3, §6.5, OQ-P3-2 |
| SPF published (required, not audit-gating) | publish anyway (OQ-P3-3) | P3 §3.3, §3.4, OQ-P3-3 |
| private-network mechanism | provider VLAN if co-located else WireGuard (OQ-P3-12) | P3 §6.2, OQ-P3-12 |
| TLS on brain↔box hop | yes / WireGuard-encrypted (OQ-P3-13) | P3 §6.2, OQ-P3-13 |
| firewall: inbound wrapper only from brain; inbound 25 closed; egress 25+DNS only | LOCKED default-deny | P3 §6.2–6.3, §6.7 |
| node-token strategy | per-node unique, revoke/rotate on compromise (OQ-P3-14) | P3 §6.4, OQ-P3-14 |
| host-selection (port-25 ∧ PTR ∧ clean ∧ cheap) | RECLASSIFIED to pre-provisioning research task (OQ-P3-1) | P3 §3.1, OQ-P3-1, GAP-16 |
| deploy topology | single container on VPS (OQ-P3-4) | P3 §3.5, OQ-P3-4 |
| `node_audits` history table (raw warnings) | new append-only table (OQ-P3-5) | P3 §4.3, §8, OQ-P3-5 |
| router strategy set (sticky-by-domain default + least-inflight fallback) | sticky-first, least-inflight fallback (behind unchanged P1 interface) | P4 §3, §3.3 |
| sticky affinity mapping (HRW vs ring vs stored table) | rendezvous/HRW hashing (OQ-P4-1) | P4 §3.2, OQ-P4-1 |
| eligibility filter predicates (active∧green∧port25∧fresh∧warmup-headroom∧rate-headroom) | LOCKED conjunction | P4 §3.3 |
| least-inflight denominator | per-(node, target-provider) in-flight (PM2); node-global = tie-break | P4 §3.4 (PM2) |
| domain-sticky vs provider-sticky | keep domain-sticky (OQ-P4-12); shared platforms only partially concentrated (GAP-29) | P4 §3.2, OQ-P4-12, GAP-29 |
| `other`-bucket granularity (provider bucketing) | registrable domain of MX host (OQ-P4-2) | P4 §4.1, OQ-P4-2 |
| rate-budget durability | durable in Supabase (OQ-P4-3) | P4 §4.4, OQ-P4-3 |
| rate-budget windowing algorithm | token-bucket w/ durable `last_refill_at`+`tokens` (OQ-P4-3); tumbling = fallback | P4 §4.4, OQ-P4-3 |
| in-flight counter durability | ephemeral (correct-zero post-crash) — GAP-26 | P4 §4.3, GAP-26 |
| HELO/verifier rotation granularity | per-node single audited identity; multi-identity a DIAL (OQ-P4-4) | P4 §4.5, OQ-P4-4, GAP-22 |
| rate limits scope (policy vs global) | global pool config + optional per-policy override (OQ-P4-5) | P4 §4.7, OQ-P4-5 |
| `smtp_skipped_reason:"rate_limited"` (new enum value) | additive to P3 enum | P4 §4.6, §9 |
| rate-saturation degrade | degrade to cheap, non-cacheable, never queue/invalid (P4); queue absorbs in P5 | P4 §4.6 |
| block-rate numerator reasons | `smtp_blocked` primary; timeout/connection_failed secondary separately-thresholded; `greylisted` NOT counted (OQ-P4-6) | P4 §5.2, OQ-P4-6 |
| block-rate query filter (`from_cache=false ∧ verifier_ip=node ∧ SMTP ran`) | LOCKED (PB1) | P4 §5.2 (PB1) |
| hysteresis (quick-quarantine / slow-recover) | LOCKED asymmetric | P4 §5.3, §5.4 |
| recovery resets durable warmup counter to low | LOCKED reset (PM3) | P4 §5.4 (PM3) |
| fleet-wide spike brake (alert, don't cascade-quarantine) | LOCKED: do NOT auto-quarantine fleet-wide; alert + hold (OQ-P4-9) | P4 §5.5, OQ-P4-9, GAP-28 |
| single-brain assumption (rate-limiter state) | single brain (OQ-P4-8); multi-brain out of scope | P4 §4.3/4.4, OQ-P4-8, GAP-25 |
| health-watcher process placement | in-process background loop, single writer of nodes.status (OQ-P4-14) | P4 §5.1, OQ-P4-14 |
| node #2 = +1 config line | config-only, zero code diff (Door 5 proof) | P4 §7, GAP-31 |
| provisioning-script stage automation | stages 2/4/5/6/7 auto; spin+PTR script-with-manual-fallback (OQ-P4-10) | P4 §6.2, OQ-P4-10 |
| metrics backend | both layered (LOG authoritative for control + TSDB for humans) (OQ-P4-11) | P4 §8.1, OQ-P4-11 |
| `rate_budgets` table | new operational table | P4 §4.4, §9 |
| queue substrate | Supabase `work_items` + `FOR UPDATE SKIP LOCKED` (OQ-P5-1) | P5 §3.1, OQ-P5-1, GAP-34 |
| compare-and-set settlement (`WHERE locked_by=me AND status=in_flight`) | LOCKED (GAP-37) — the CAS not the lease is the guarantee | P5 §5.2, §5.3, GAP-37 |
| idempotency key derivation | `hash(email_hash ‖ policy_key ‖ policy_version ‖ config_fingerprint)` | P5 §5.1 |
| `work_item_attempts` transcript table | yes, append-only sidecar (OQ-P5-8) | P5 §5.3, OQ-P5-8 |
| job completion oracle | authoritative `NOT EXISTS(... NOT IN settled,dead)`; `settled_count` = display only (GAP-38) | P5 §7.3, GAP-38 |
| plaintext-at-rest in `work_items` | held for item life, purged transactionally on settle (OQ-P5-9) — the one bounded Door-3 exception | P5 §3.3, §5.3, OQ-P5-9, GAP-32b |
| master invariant: queue never bypasses a gate | LOCKED | P5 §8, §8.1 |
| retryable transients | `greylisted` primary; timeout/connection_failed secondary; `smtp_blocked` NOT retried (OQ-P5-12) | P5 §4.5, OQ-P5-12 |
| `rate_limited` counts toward `max_attempts`? | NO — capacity wait bounded by max_retry_age only (OQ-P5-7) | P5 §4.6, OQ-P5-7 |
| sync greylist/rate_limited auto-queue? | NO by default (OQ-P5-5) | P5 §3.4, §4.6, OQ-P5-5 |
| retry node affinity (same-IP) | prefer same node for retry; least-inflight only if home gone (OQ-P5-6) | P5 §4.4, OQ-P5-6, GAP-32 |
| group-by-domain ordering | domain-spread across, paced within (vs upload order) | P5 §6.2 |
| domain-batched multi-RCPT session reuse | NO in P5 (OQ-P5-4); contingent on GAP-13 spike | P5 §6.4, OQ-P5-4 |
| result delivery | poll-only, cursor-paginated (OQ-P5-11); webhook = SEAM | P5 §7.1, OQ-P5-11 |
| retry-provenance signals (`signals.attempt`/`retry_settled`) | yes, additive (OQ-P5-10) | P5 §10, OQ-P5-10 |
| `complete_with_unknowns` job status | informational job-status string (not verdict vocab) | P5 §7.3 |
| `caller_ref` (opaque platform correlation) | echoed, never interpreted (two-job boundary) | P5 §7.2, §7.6, GAP-36 |
| `external` layer enabled (per policy) | enabled in cold-email (OQ-P6-2); a policy without the block = **the privacy opt-out lever** | P6 §3.2, §7.5, OQ-P6-6 |
| `external` layer position in `order` | LAST (most expensive, runs only on residue) | P6 §3.1 |
| `vendor` selector (adapter) | `millionverifier` (OQ-P6-8, one at launch); swappable | P6 §3.2, §8, OQ-P6-8 |
| one vendor vs two (A/B) | one at launch (OQ-P6-8); multi-vendor = SEAM P7 | P6 §8.4, OQ-P6-8 |
| external `valid` → `risky` (never SMTP-grade) | LOCKED risky (OQ-P6-1) | P6 §3.4, OQ-P6-1 |
| vendor `invalid` acceptance (asymmetric) | accept as invalid only on retry-exhausted/gave-up-capacity; downgrade to risky on catch-all/`:mx` (OQ-P6-3) | P6 §3.4, §8.2, OQ-P6-3 |
| `route_when` residue classes | all four at launch (catch_all, provider_no_smtp, retry_exhausted_greylisted, gave_up_rate_limited) (OQ-P6-2); may not add a 5th | P6 §3.2, §4.1, OQ-P6-2 |
| `on_vendor_unknown` / `on_budget_exhausted` / `on_vendor_error` | `keep_residue` (never fabricate, never worse than P5) | P6 §3.2, §5.3 |
| escalation decided once at verdict-mint (not on cache read) | LOCKED (GAP-43) | P6 §4.3, GAP-43 |
| cache the `external_verified` verdict | yes (settled), same key; excluded from calibration when from_cache (OQ-P6-9) | P6 §4.3, OQ-P6-9 |
| circuit-breaker trip conditions (budget / error-rate / vendor-down) | three conditions, per-vendor state (§5.3) | P6 §5.3 |
| breaker = brake not cascade | LOCKED alert-and-degrade, never auto-disable policy/fail jobs (§5.5) | P6 §5.5 |
| spend idempotency (CAS on lease-holder) | LOCKED — vendor call + cost-decrement + settle under same CAS (GAP-42) | P6 §5.4, GAP-42 |
| vendor idempotency key | use where supported (`email_hash ‖ vendor`) (OQ-P6-4) | P6 §5.4, §8.3, OQ-P6-4 |
| plaintext egress to vendor | address-only, residue-only, DPA-gated, TLS; reuses P5 transient plaintext (§7.2/7.3) | P6 §7.2, §7.3, GAP-40 |
| vendor DPA / due-diligence gate | layer ships DISABLED until checklist passes (§7.4) | P6 §7.4 |
| platform disclosure posture | disclose + opt-out (options 1+2) (OQ-P6-6); per-policy opt-out mechanical | P6 §7.5, OQ-P6-6, GAP-45 |
| sync-residue: inline vs async-requeue | inline with tight timeout, keep_residue on timeout (OQ-P6-7); async = opt-in SEAM | P6 §2.2, OQ-P6-7 |
| outcome transport | sender-owned append feed P7 ingests (OQ-P7-1) | P7 §3.2, OQ-P7-1, GAP-48 |
| ingestion idempotency key | `(message_id, event)` dedup, no-op append-guard | P7 §3.4 |
| integrity reject-and-flag (unknown verification_id / sent_at<created_at / malformed) | reject + `ingest_error`, exclude from calibration (GAP-51) | P7 §3.3, §3.4, GAP-51 |
| bounce classifier three-class map | LOCKED: mailbox_not_found→invalid-signal; mailbox_full→valid-signal; blocked/policy/transient→zero-information | P7 §4.2, §4.3, GAP-49 |
| calibration exclusions (from_cache, override, external-separate, disposable-risky, zero-information) | LOCKED, consumed from P1/P6 | P7 §4.4, §5.3, GAP-54 |
| historical-bounce gate (`historical_bounce:pass → invalid:historical_bounce:stop`) | reserved reason, TTL-windowed (OQ-P7-2 DECIDED) | P7 §4.6, OQ-P7-2 |
| calibration bucket dimension | `(reason × mx_provider)`; external parallel-separate by `(vendor, residue_class, provider)` | P7 §5.2, GAP-54 |
| calibration store (separate, versioned, effective-dated) | new table, never mutates verifications (Door 2) | P7 §5.4 |
| recalibration CAS-on-lease-holder | LOCKED (GAP-53) — the calibration writer is a lease-and-settle path | P7 §5.5, GAP-53 |
| cold-start fallback to P1 baseline | uncalibrated buckets use P1 baseline (§5.6) | P7 §5.6, §14.2, GAP-52 |
| hysteresis (down-fast / up-slow) | LOCKED asymmetric — doubt applied fast, trust earned slow | P7 §6.3 |
| cascade brake (system-wide swing → alert, not auto-apply) | LOCKED: hold + human confirm; revert via versioned store | P7 §6.4, GAP-50 |
| weaning rule (drop fallback per residue×provider) | when own measured accuracy beats vendor's, both sample-gated; reversible | P7 §9.2 |
| `unknown` not pressured to `valid` | LOCKED — first-class unknown survives flywheel | P7 §8.4 |
| platform "your sends train our accuracy" disclosure | surface it (OQ-P7-8), disclose-don't-hide | P7 §14, OQ-P7-8 |

## Dials (79)

| knob | default / state | source |
|---|---|---|
| `recommendation_thresholds.send` | 70 | P1 §7.2, §3.1.3 |
| `recommendation_thresholds.review` | 30 | P1 §7.2 |
| confidence `baseline_by_reason` table | fixed placeholder (OQ-8); invalids 90–99, plausible/provider_no_smtp ≤40, unknown low | P1 §7.4, OQ-8 |
| confidence `flag_deltas` | UNSET numeric | P1 §7.2, §7.4 |
| layer `severity_hint` ∈ {info,warn,critical} | per-layer | P1 §6.0 |
| cache TTL / `cache.expires_at` | UNSET — owner OQ (OQ-9); NOT NULL required | P1 §4.2, OQ-9 |
| `raw_truemail_json` retention window | ~90 days (OQ-10, DROP PARTITION) | P1 §3.2.6, OQ-10 |
| `verification_raw` partition cadence | monthly RANGE partition | P1 §3.2.6 |
| disposable_domains list (Supabase, not env) | source-of-truth table | P1 §4.4, §6.7 |
| spam_trap list source | UNSET — acquisition unsolved (OQ-17) | P1 §4.4, §6.8, OQ-17 |
| typo known-good domain list | built-in seed now, Supabase later (OQ-18) | P1 §6.2, OQ-18 |
| role-account list | built-in | P1 §6.3 |
| free-vs-corp / provider-class lists | built-in | P1 §6.4, §6.9 |
| `connection_timeout` (engine) | 2s (gem default) | P2 §6.5, App A.1 |
| `response_timeout` (engine) | 2s (gem default) | P2 §6.5, App A.1 |
| `connection_attempts` (engine) | 2 (gem default) | P2 §6.5, App A.1 |
| `smtp_error_body_pattern` (5xx address-fault discriminator) | `/(?=.*550)(?=.*(user\ | P2 §6.1, App A.1 |
| 5xx code/body address-fault set | starts from engine pattern; ambiguity → unknown LOCKED | P2 §6.1 |
| SMTP confidence bands | clean-250+probe-rejected 85–95; safe-check ≤70; address-fault invalid 90–99; mailbox_full 85–95; catch_all/plausible 25–50; unknown low | P2 §7.4 |
| safe-check confidence cap | ≤70 (below clean-250) | P2 §6.8, §7.4 |
| probe random local-part construction | 16+ random hex/base32, optional `nx-` prefix, lowercase; CSPRNG prod, seeded in tests (OQ-P2-3) | P2 §5.3, OQ-P2-3 |
| number of probe RCPTs | 1 in P2; N-probe deferred P4 (OQ-P2-10) | P2 §5, OQ-P2-10 |
| re-probe cadence / `catchall_domains.expires_at` | UNSET — SEAM (§5.6) | P2 §5.6 |
| brain client-side deadline on `/v1/verify` | UNSET numeric (P5 lease TTL must *exceed* this — latent numeric coupling) | P2 §6.5 |
| greylist retry hint `retry_after_hint_s` | parsed from 4xx else policy default (SEAM to P5) | P2 §6.5 |
| `/v1/stats` counter set | shape fixed, counters DIAL | P2 §4.4 |
| scheduled re-audit cadence | hourly for first box | P3 §4.5 |
| audit staleness max-age | 2× the schedule | P3 §4.5 |
| auto-recovery threshold (N consecutive greens) | N-consecutive-green (asymmetric, OQ-P3-7) | P3 §4.5, OQ-P3-7 |
| warmup ascending daily cap schedule | UNSET (OQ-P3-10) illustrative D1~50/D2~100/D3~250/D4~500/wk2~1–2k | P3 §5.2, OQ-P3-10, GAP-20 |
| warmup health-checkpoint gate | step up only on prior-day low smtp_blocked + green audit | P3 §5.2, OQ-P3-10 |
| `max_inflight` (per node×provider concurrency cap) | small, 1–3 | P4 §4.2 |
| `max_starts_per_window` (rate cap) | ~N/min conservative | P4 §4.2 |
| `window_s` (rate window) | 60 | P4 §4.2, §4.7 |
| `start_delay_jitter_ms` | 50–500ms spread | P4 §4.2, §4.7 |
| health-watcher cadence `T` | 30–60s (OQ-P4-13) | P4 §5.1, OQ-P4-13 |
| block-rate window length | last 15–30 min or last K attempts (OQ-P4-13) | P4 §5.2, OQ-P4-13 |
| `min_samples` (block-rate sample gate) | ≥20 (GAP-27) | P4 §5.3, OQ-P4-13, GAP-27 |
| `QUAR_THRESH` (block-rate quarantine threshold) | ~30% over ≥min_samples, confirmed 2 consecutive cycles | P4 §5.4 |
| `RECOVER_THRESH` (recovery block-rate) | ~5% (asymmetric, lower than QUAR) | P4 §5.4 |
| `RECOVER_WINDOW` (sustained-clean duration) | 30–60 min | P4 §5.4 |
| `recovering` canary trickle size | few checks/window, well under warmup start (OQ-P4-7) | P4 §5.4, OQ-P4-7, GAP-30 |
| liveness-fail quarantine (M consecutive) | M consecutive `/health` fails | P4 §5.4 |
| fleet-wide detection threshold | >50% active nodes over threshold same window | P4 §5.5 |
| alert thresholds/channels (quarantine, block-spike, audit-red, pool-eligible-low, rate-saturation) | set per §8.2 | P4 §8.2 |
| lease TTL | LOCKED: must exceed brain's max SMTP client deadline (GAP-37) | P5 §5.2, GAP-37 |
| in-flight dedup collapse | collapse-on-enqueue when cheap; CAS backstop | P5 §5.5 |
| backoff base step | 5 min (hint-seeded floor from `retry_after_hint_s`) | P5 §4.2, OQ-P5-2 |
| backoff factor | ×2 exponential | P5 §4.2, OQ-P5-2 |
| backoff ceiling | 60 min | P5 §4.2, OQ-P5-2 |
| backoff jitter | ±10–20% | P5 §4.2, OQ-P5-2 |
| `max_attempts` | 3–5 | P5 §4.5, OQ-P5-2 |
| `max_retry_age` | ~6h | P5 §4.5, OQ-P5-2 |
| `rate_limited` retry backoff (queued) | short (seconds–minute), distinct from greylist | P5 §4.6 |
| cross-job fairness | domain-spread + per-job in-flight ceiling; WFQ deferred (OQ-P5-3) | P5 §6.5, OQ-P5-3 |
| per-job in-flight ceiling (% of capacity) | UNSET numeric (OQ-P5-3) | P5 §6.5 |
| bulk max addresses-per-submission | UNSET (backpressure) | P5 §7.5 |
| max in-flight-jobs | UNSET | P5 §7.5 |
| `jobs` retention window | days (results-fetch window), then work_items purged | P5 §7.2 |
| `confidence_cap` (external valid) | 60 (anti-laundering) | P6 §3.2, §3.4, OQ-P6-1 |
| vendor normalization table (raw status → system vocab) | per §8.2; unrecognized → `unknown` (never guess) | P6 §8.2 |
| `daily_cap` (external spend) | UNSET — start conservative (OQ-P6-5) | P6 §5.2, OQ-P6-5 |
| `monthly_cap` | UNSET conservative | P6 §5.2, OQ-P6-5 |
| `per_job_cap` | UNSET — low enough one list can't drain global (OQ-P6-5) | P6 §5.2, §4.4, OQ-P6-5 |
| `rate_ceiling` (vendor calls/min) | UNSET | P6 §5.2, OQ-P6-5 |
| vendor error-rate trip threshold + cooldown | UNSET | P6 §5.3 |
| DSN-code → event mapping | provider-specific, tunable; three-class taxonomy LOCKED | P7 §4.3 |
| evidence-strength weights | ordering LOCKED (mailbox_full ≥ discriminating-delivery > catch-all-delivery ≈ 0); weights DIAL | P7 §4.5 |
| historical-bounce TTL | 6–12 months on confirmed mailbox_not_found only, then degrades to penalty (OQ-P7-2) | P7 §4.6, OQ-P7-2, App C PB2 |
| `confidence` recalibration (numbers only) | replaces P1 baseline per bucket; int 0–100 unchanged | P7 §5.1, §5.2 |
| `min_bucket_samples` (sample-size gate) | start 500 (build-plan ~200–500) (OQ-P7-3) | P7 §6.2, OQ-P7-3 |
| `max_step` (bounded step per cycle) | ≤10–15 points/cycle | P7 §6.3 |
| cascade-brake "system-wide swing" definition | e.g. >N% high-volume buckets moving >max_step | P7 §6.4 |
| recalibration cadence | daily/weekly rollup, not per-outcome (OQ-P7-7) | P7 §5, OQ-P7-7 |
| `promote_threshold` (provider auto-promote to `:mx`-only, #4) | ≤~1–2% mailbox_not_found on valid-labels (OQ-P7-4) | P7 §7.2, OQ-P7-4 |
| `min_promote_samples` | UNSET (sample-gated) (OQ-P7-4) | P7 §7.2, OQ-P7-4 |
| `demote_threshold` (hysteretic) | higher than promote (quick-demote/slow-promote) (OQ-P7-4) | P7 §7.2, OQ-P7-4 |
| accuracy metric target (#5) | UNSET — owner-set per provider, publish with coverage (OQ-P7-5) | P7 §8.1, §8.2, OQ-P7-5 |
| weaning safety margin | margin not parity (OQ-P7-6) | P7 §9.2, OQ-P7-6 |

## Other / uncategorized (2)

| knob | default / state | source |
|---|---|---|
| email-hash pepper | brain holds it in memory to compute HMACs → a brain core-dump is an unaddressed leak vector; read-surface grows silently as P5/P7 add `email_hash`-derived stores | brain holds it in memory to compute HMACs → a brain core-dump is an unaddressed leak vector; read-surface grows silently as P5/P7 add `email_hash`-derived stores |
| Supabase service-role key | the sole credential gating all state + the append-only grant (security F-11/F-13) | the sole credential gating all state + the append-only grant (security F-11/F-13) |

---

*End generated reference. Source of truth: [`config-manifest.md`](config-manifest.md).*
