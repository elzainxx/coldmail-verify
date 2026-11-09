# ADR-0021 — The SMTP probe never connects to a non-public address: a pre-flight in the forked wrapper, a new additive reason, and an egress firewall behind them

- **Status:** ACCEPTED (2026-11-09) — ruled by the owner ("M1 SMTP probe destination (SSRF)"), closing the finding ADR-0020's audit
  raised and the runbook carried as a 🔴 item.
- **Kind:** security control + additive vocabulary. **Door 5 is untouched**: the pinned `truemail` gem was only *read*; nothing about it changes.
- **Deciders:** owner.
- **Governs:** `verifier-node/truemail-rack/app/truemail_server/wrapper/probe_guard.rb` (new) · `…/app.rb` (`verify`, `catchall`, `audit`) ·
  `…/catchall_probe.rb` · `verifier-node/egress/verifier_egress.nft` (new) · `control-plane/internal/vocab` (`non_public_mx`) ·
  `control-plane/internal/smtp/probe_refused.go` (new) · `control-plane/internal/orchestrator/smtp_stage.go` ·
  `control-plane/migrations/0015_non_public_mx_reason.sql` · `scripts/check-probe-guard.sh` and its `-selftest.sh` ·
  `verifier-node/truemail-rack/test/p2/socket_proof.rb` (rows 9–11).

---

## Context

The node opens an SMTP session to whatever address a user-supplied domain's mail host resolves to. A user can publish
`MX 10 evil.example.` with `evil.example. A 169.254.169.254` — or 127.0.0.1, 10.x.x.x, 100.64.x.x — and the verifier box connects to it. The
exposure is bounded to port 25, but that is the port internal mail relays listen on, and the SMTP transcript is persisted verbatim in the raw
sidecar (§6.4), so an internal SMTP banner would be recorded. Nothing guards it: the control plane, the fork, the compose file and the runbook were
searched, and the gem has none (`IPAddr` appears in it only in an exception rescue and the PTR audit). The finding is **not new** — an explicit MX
could always do this; ADR-0020's implicit-MX ruling added the domain's own A record as one more path.

What the gem does matters for the design. `Smtp#run` → `MxBlacklist` → `Mx` **re-runs the MX step and appends any newly resolved address** to
`result.mail_servers`, then connects to whatever is in that list. So a check made *before* the gem is called is a resolve-then-resolve gap: a DNS
answer that flips between the two (rebinding) can add a private address the check never saw. A guard that ignored this would look complete and not be.

## Decision

1. **Layer 1 — PRE-FLIGHT, before the gem's SMTP step.** `ProbeGuard.preflight` resolves the target with the **gem's own MX step**
   (`Truemail.validate(email, with: :mx)`), so the candidate addresses are exactly the ones SMTP would try (MX → CNAME → implicit A, A and AAAA). If
   **any** is non-public the probe is refused and the SMTP step never runs. *Any*, not *only*: the gem tries every address in order, so one public
   MX beside a private one still reaches the private one. (The owner's wording — "an MX resolving only to non-public addresses → risky" — describes
   the verdict for that case; the mixed case is refused too, because the alternative is to connect. A legitimate domain with a stray private
   secondary MX is therefore `risky/non_public_mx` where it used to be probed; that is the cost, and it is rare.)
2. **Layer 2 — POST-CHECK.** After the SMTP run, the addresses the gem *actually used* (`result.mail_servers`) are checked again. If any is
   non-public, the **transcript is discarded** (never returned, therefore never persisted) and the probe is reported as refused. This does not
   prevent the connection in the rebinding race — it prevents the data leak and makes the event visible. The same two layers wrap the catch-all
   probe (a second SMTP session to the same destination) and the standalone `GET /v1/catchall/:domain`.
3. **Layer 3 — the audit.** `PROBE_ALLOW_NON_PUBLIC=1` switches the guard off, **for the socket-level proof harness only** (its fakes live on loopback).
   It is exact (`"1"` and nothing else), read at call time, and it turns `GET /v1/audit` **red** (`warnings.probe_guard`), so a node running unguarded
   is never `:smtp`-eligible and receives no brain traffic. A deployment cannot quietly run without the guard.
4. **Layer 4 — the egress firewall** (`verifier-node/egress/verifier_egress.nft`, runbook Stage 1). The only layer that cannot be raced: it is applied
   by the kernel to the connection that is actually made. `scripts/check-probe-guard.sh` fails if any range the guard refuses is missing from the ruleset,
   if its IPv6 allow-list rule is lost, or if the file is not valid nftables (`nft -c`). Applied in a container's network namespace, its counters moved
   on port-25 attempts to metadata, RFC 1918 and CGNAT addresses.
5. **What is refused.** IPv4: every IANA special-purpose block that is not globally routable (0/8, 10/8, 100.64/10 CGNAT, 127/8, 169.254/16
   link-local and cloud metadata, 172.16/12, 192.0.0/24, TEST-NET-1/2/3, 192.88.99/24, 192.168/16, 198.18/15), plus multicast and 240/4 (incl. broadcast).
   IPv6 is an **allow-list**: only global unicast `2000::/3` is public, minus 2001::/23 (IETF/Teredo), 2001:db8::/32, 2002::/16 (6to4) and 3fff::/20;
   everything else (`::1`, `::`, ULA, link-local, multicast, NAT64 `64:ff9b::/96`, …) is non-public by construction. An IPv4-mapped address
   (`::ffff:a.b.c.d`) is judged as the IPv4 address it will connect to. Anything that does not parse is refused.
6. **The wrapper reports a fact; the brain decides.** The refusal is `probe_refused: {reason, count, targets[≤8: address, range]}` with no transcript.
   The brain maps it to **`risky` / `non_public_mx`** — a new **additive** reason (Door 4: added to `vocab`, and to the `valid_reason` CHECK on both
   `verifications` and `cache` by migration 0015, `NOT VALID` + `VALIDATE` so the append-only log's scan does not hold the lock). It is **never
   `invalid`** (a fact about the domain's DNS, not the address), recommendation `review`, confidence 20, and **not cacheable** — it derives from DNS
   state the domain's owner can change, so a stray private A record is not remembered for a TTL. Signals: `probe_refused`, `probe_refused_ranges`,
   `probe_refused_count`.

## What is proved, and how

- **Every range, with its neighbours.** RSpec: ~60 refused addresses (each block's first/last and inside) and ~25 public controls that sit on the
  *other* side of every IPv4 edge (`172.15.255.255`, `172.32.0.0`, `100.63.255.255`, `100.128.0.0`, `169.253.255.255`, …).
- **Against an oracle that is not the guard's own table.** `check-probe-guard.sh` has Python generate the address set from its **own** transcription
  of the IANA registries (every block's edges and midpoint, ~50k seeded random IPv4/IPv6, IPv4-mapped forms), the **real** guard module decide each
  under the node image's own Ruby, and Python's `ipaddress.is_global` judge each decision. Soundness: the guard never allows a non-global address (0
  of 50,939); it may over-refuse (unassigned IPv6, multicast, 6to4 — by design). Four mutants — CGNAT, link-local, 172.16/12 removed from the guard;
  CGNAT removed from the firewall file — each go red, **at the removed block's edges** and nowhere else. (The first version generated edges from the
  guard's own table, so deleting a range deleted its test points and only random sampling could notice; the mutant's violation count — 2 where 4+ were
  expected — exposed it, and generation moved to the oracle.)
- **Against real DNS and the real gem, observing connections.** The socket-proof harness (`test/p2`, 11/11): `dns_mock` maps `corp.test`'s MX to
  127.0.0.1 and a **counting listener** is the destination. Guard on → refused, **0** connections; the catch-all probe likewise 0; **control** (override on,
  same destination) → the gem connects, **2** connections — so zero is an observation, not an inference.
- **The wire contract is shared.** `spec/fixtures/probe_refused.json` is asserted byte-for-byte by the Ruby request spec and decoded by the Go tests —
  the two languages cannot drift apart on the field names.
- **End to end in the brain.** `Verify` with the golden body → `risky/non_public_mx`, node called once, never invalid, not cached, logged once, the stored
  node body redacted (Door 3); control: an accepting node on the same domain is `valid/accepted`; control: the unmapped body would have been
  `unknown/connection_failed`.
- **The migration, against real Postgres.** `TestVocabMirrorsTheDatabaseCheckConstraints` reads both `valid_reason` CHECKs back from the catalog and
  requires them to equal `vocab.Reasons()` — the mirror the vocab's own comment always claimed and nothing verified. Control: through 0014 the database
  rejects exactly `non_public_mx`.

## Consequences and what is NOT closed

- **Rebinding can still cause the *connection* in the race between layers 1 and the gem's re-resolve**; layer 2 contains the leak, layer 4 prevents the
  connection. Layer 4 is therefore a Stage 1 deployment requirement, not an optional hardening (runbook).
- Cloud metadata is reachable on ports other than 25; the firewall file scopes itself to the SMTP probe. Restrict the metadata service independently
  (IMDSv2, hop limit 1). Said in the file and the runbook.
- Two DNS lookups per request instead of one for an SMTP-typed verify (the pre-flight's MX step, then the gem's). The node's resolver caches; acceptable.
- A refused request still consumes a warm-up slot and a rate-limit token on the node it was sent to (the brain only learns of the refusal from the node).
- `non_public_mx` is a brain-visible reason: consumers that branch on `reason` (calibration buckets, the flywheel) see a new value. Additive-only, so no
  existing consumer breaks.
