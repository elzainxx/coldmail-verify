# Phase 3 — real-box cutover runbook (the PENDING-REAL-BOX checklist + exact steps)

**Date:** 2026-07-17 · **Status:** the local build is COMPLETE and inert-for-real-traffic; this
runbook is the mechanical path from "one config flip away" to a live audited box.
**Authorities:** [`phase-3-first-real-box-spec.md`](phase-3-first-real-box-spec.md) §3 (provisioning
/ vetting), §3.4 (egress proof), §4.4 (the gate), §5.3 (the flip), §6 (security posture), §6.6.1
(GAP-24); [ADR-0009](../adr/ADR-0009-gap24-egress-ip-invariant-runtime-enforcement.md); [`phase-3-readiness-assessment.md`](phase-3-readiness-assessment.md) (the three cutover gates).

> **The design is done; the box is not acquired.** Getting a host that passes V1–V5 is the wall
> (Gate 1). This runbook assumes you have a trial box and walks the exact vetting + flip + proof.
> **Never fake a real-IP check green** — each PENDING item below has the command that proves it.

---

## 0. Before you touch the box — the three cutover gates

| Gate | State | Action before go-live |
|---|---|---|
| **1. Host** (port25 ∧ settable PTR ∧ clean IP ∧ dedicated ∧ egress==PTR'd IP) | 🔴 owner procurement | Vet V1–V5 on a **refundable trial** box before paying (§1 below). This is the real work. |
| **2. GAP-13 reputation cost** | 🟡 known input | Run the catch-all-session spike **before provisioning** if not already (ADR-0007 stands: Option B, gem never patched). Size the warmup cap against **connections** (~2× verifications for catch-all-heavy traffic). |
| **3. Admin-audit log (DR-3)** | 🔴 required before go-live | Build the append-only admin-audit write-path (per-actor identity) — buildable now, in parallel; **not** part of this local build. Gates the go-live, not the box vetting. |

---

## 1. Pre-purchase vetting (§3.2, V1–V5) — do this on a TRIAL box, before paying

Each is pass/fail. A HARD fail disqualifies the host. Record the command output.

| V | Requirement | The proof command (run FROM the trial box unless noted) | PENDING until |
|---|---|---|---|
| **V1** | Outbound TCP/25 genuinely open (GAP-15) | `nc -vz <a-real-external-mx> 25` establishes a connection AND a `220` banner is seen (e.g. `swaks --server <mx>:25 --quit-after CONNECT`). **Not** the provider's claim. | a banner is received |
| **V2** | Settable custom PTR/rDNS | In the provider panel/API, set PTR for the box IP → the verifier FQDN; confirm `dig -x <box-ip> +short` returns it (may take propagation time). | PTR resolves to the FQDN |
| **V3** | Clean IP (no spam history) | Check `<box-ip>` against Spamhaus/Barracuda/passive history **before** warmup. | IP is not pre-poisoned |
| **V4** | Dedicated / stable IP | Confirm the IP is yours for the box's life (not shared/rotating). | provider confirms |
| **V5** | Egress IP == PTR'd IP == audited IP (GAP-24) | `curl -s ifconfig.me` (egress) == the PTR'd IP == the verifier A-record target == `host_audit.current_host_ip`. **All four identical.** | all four match the public egress IP |

Also decide: verifier domain (dedicated/isolated, OQ-P3-2), private-network mechanism (OQ-P3-12),
per-node token (OQ-P3-14). Keep a documented fallback provider (§3.1).

---

## 2. Provision DNS/PTR/SPF (§3.3)

- **A-record:** `<verifier-domain>` → the box's **public egress** IP (NOT a private ingress IP —
  GAP-24). Prove: `dig +short <verifier-domain>` == public egress IP.
- **PTR:** box public egress IP → `<verifier-domain>` (V2). Prove: `dig -x <public-egress-ip> +short`.
- **SPF (GAP-17 — required, NOT audit-gating):** publish a TXT authorizing the egress IP. Prove:
  `dig TXT <verifier-domain>` shows the SPF. `host_audit` does **not** check this — it's a separate
  checklist item.

---

## 3. Deploy the box (§3.5, Door 5 — the P2 fork unchanged)

- Deploy the **Phase-2 forked truemail-rack + wrapper container byte-for-byte** (gem pinned 3.3.1,
  CI-asserted unmodified). No code change for Phase 3.
- Configure the box's ENV with the **real** `verifier_email`/`verifier_domain`, `smtp_port: 25`,
  and a real `dns:` (real resolver or system default) — replacing the mock ports.
- **Security posture (§6.7 checklist — all before `enabled=true`):** wrapper bound to the private
  interface (public port scan shows it CLOSED); firewall inbound wrapper only from the brain, inbound
  25 closed, egress 25+DNS only; per-node token over TLS/tunnel; box holds **no** DB creds / no pepper.

---

## 4. The flip (§5.3.1) — config-only, reversible, ordered (audit BEFORE traffic)

The order is safety-critical. **Zero brain code changes** (GAP-21).

1. **Egress-25 smoke test** (§3.4) → set the node's `port25_open=true`. Command: V1 above.
   > **⚠ WIRING GAP (flagged, review 2026-07-18 — not faked, but not yet operator-executable):**
   > `port25_open` is only ever *set false* by boot registration (`UpsertNodeState(..., false)`); the
   > re-audit scheduler reads it back and rewrites it unchanged. There is currently **no
   > operator/env/admin command** to flip it to `true` in a running system (no `TM_NODE_PORT25_OPEN`
   > env, no admin endpoint, no egress-test writer). So as of this build the node **cannot be made
   > `:smtp`-eligible without an out-of-band DB write** — which is honest (the gate fails closed;
   > nothing is faked green) but means this cutover step is **not mechanically executable yet.**
   > Flipping `port25_open` is a **manual control-plane mutation** and therefore belongs behind the
   > **DR-3 admin-audit-log** (per-actor identity), which is the accepted **pre-P3-go-live gate** and is
   > **buildable now in parallel with Phase 4**. The egress-test-writer + its admin path are the
   > concrete first slice of that gate. Until then, the interim is a direct DB write
   > (`UPDATE nodes SET port25_open=true WHERE id=...`) — the migration grants both `cp_runtime` and
   > `cp_admin` UPDATE on `nodes`, so do it as `cp_admin` (break-glass, to be admin-audited under DR-3),
   > recorded here as the interim manual step until the audited write-path exists.
   Until `port25_open` is true, the node is **not** `:smtp`-eligible.
2. **Point the box at real config** (`smtp_port:25`, real `dns`) and `POST /v1/config/reload` →
   a new `config_fingerprint` (naturally invalidates any mock-era cache).
3. **Set the brain node descriptor** (env / config-list):
   - `TM_NODE_BASE_URL=<private-network address of the box>`
   - `TM_NODE_PUBLIC_EGRESS_IP=<the public egress IP>` — the GAP-24 anchor. **Must** equal V5.
   - `TM_NODE_ENABLED=true`
   - `TM_SMTP_MODE=enabled` (kill-switch Rung 0/1 — SMTP allowed)
4. **First audit → require green.** The re-audit ticker audits at boot; or hit `GET /v1/audit`.
   The node promotes `provisioning→active` **only** if green ∧ `port25_open` ∧ (GAP-24) the audit's
   `current_host_ip` == `TM_NODE_PUBLIC_EGRESS_IP`. A green on the wrong IP will **not** promote
   (ADR-0009) — check the logs for the `egress-IP != audited current_host_ip (GAP-24)` warning.
5. **Start the warmup cap low** — `TM_WARMUP_DAILY_CAP=50` (day 1). Step up only on prior-day low
   `smtp_blocked` + green audit (OQ-P3-10). Remember: the cap counts **verifications**; a
   catch-all-eligible check is ~2 connections (GAP-13) — size against connections.
6. **Confirm real verdicts flow + `verifier_ip` tagged.** A real honest-corporate address yields a
   real SMTP verdict logged with non-null `verifier_ip` (== the egress IP), real `node_id`, real
   `engine_version`, real `config_fingerprint`.

**Reversibility:** `TM_NODE_ENABLED=false` (or `TM_SMTP_MODE=disabled`) degrades to cheap verdicts
(never invalid) with **no redeploy, no code change, no lost history**. Re-point `base_url` to a mock
and reload to go back to the P2 substrate.

---

## 5. The go-live acceptance proofs (§12, on the real box)

Confirm each — these are the real-IP proofs the local build could only simulate:

- [ ] Egress smoke test → `port25_open=true` (V1).
- [ ] `/v1/audit` green (`warnings=={}`); node `provisioning→active`.
- [ ] Break PTR → re-audit red → node auto-**quarantined**, removed from `:smtp` candidacy; an
      address degrades to cheap (`risky/plausible`), `smtp_skipped_reason=audit_red`, **not invalid**.
- [ ] Restore PTR → N consecutive greens → node recovers to `active` (OQ-P3-7).
- [ ] Honest-corporate real address + **random** probe rejected → `valid/accepted`, `verifier_ip`
      = egress IP (GAP-23: the `valid` path's first realistic test).
- [ ] Gmail/O365/Yahoo address → `risky/provider_no_smtp`, SMTP never called,
      `smtp_skipped_reason=provider_mx_routed` (settled → cacheable).
- [ ] Warmup cap exceeded → over-cap degrades to cheap, `smtp_skipped_reason=warmup_cap`, non-cached.
- [ ] External port scan → the wrapper port is **closed** to the public internet.
- [ ] Four-way IP identity: `curl ifconfig.me` == `dig +short <verifier-fqdn>` == `dig -x` PTR ==
      `host_audit.current_host_ip` == `TM_NODE_PUBLIC_EGRESS_IP` (§6.6.1 / GAP-24).
- [ ] Provider-routing drift alarm is silent at boot (the box's `validation_type_for` matches the
      policy render — check the `provider-routing backstop in sync` log line).

---

## 6. What does NOT change at cutover (the config-flip promise, verified)

- **No brain code diff** — routing, parsing, classification, scoring, logging, the gate, the
  scheduler are byte-identical mock→real. If the cutover *required* a code change, GAP-21 is
  violated — raise it, don't patch around it.
- **No verifier-node change** — the P2 fork + gem are deployed byte-identical (Door 5).
- **No schema change** — Phase 3's tables (`node_audits`, `node_warmup`) and reserved columns were
  applied by migration 0007 during this build; the flip writes real values into them.
