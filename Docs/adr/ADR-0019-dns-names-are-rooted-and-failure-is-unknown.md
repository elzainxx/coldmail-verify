# ADR-0019 — Every DNS name is looked up fully qualified, "does not exist" is a narrow door, and a DNS failure of ours is `unknown`, never `invalid`

- **Status:** ACCEPTED (2026-11-08) — ruled by the owner (the "same bug class in module 1" brief, chunk C2), after module 3's
  first GitHub Actions run exposed the identical defect in its From-gate (M3-CI-OD-1).
- **Kind:** correctness of the harden-toward-unknown discipline (the founding Door 4 invariant), applied to DNS. **Door 5 is
  untouched**: the pinned `truemail` gem was only *read*; nothing about it changed.
- **Deciders:** owner.
- **Governs:** `control-plane/internal/layers/resolver.go` (`fqdn`, `asDNSNotFound`, `NetResolver`, `NewNetResolverAt`) ·
  `control-plane/internal/layers/dns_realdns_test.go` · `scripts/check-dns-search-domain.sh` and its paired
  `scripts/check-dns-search-domain-selftest.sh` · `.github/workflows/ci.yml` (`dns-search-domain` job).

---

## Context

The control plane does its own MX resolution on the cheap path (OQ-6). `resolveMX` maps the DNS result to one of five outcomes,
and the policy turns them into verdicts: `mx:fail → invalid:no_mx:stop`, `mx:unknown → unknown:dns_error:stop`. Which side a
failure lands on is therefore the difference between "we could not find out" and "this address is invalid".

Go's resolver, given a name **without a trailing dot**, walks the search list from `/etc/resolv.conf` — the name as written, then
`name.<search-domain>` for each — and unless `Resolver.StrictErrors` is set it returns the error of the **last** name tried. So on
a host with a search domain (ordinary on cloud VMs and in Kubernetes), a SERVFAIL for `example.com` was overwritten by the
NXDOMAIN of `example.com.<search-domain>`. `asDNSNotFound` read "this domain does not exist", `resolveMX` returned `mxNone`, the
MX layer returned `Fail/Critical`, and the verdict was `invalid / no_mx`: **a DNS outage of ours reported as a fact about the
user's address**, which this module exists never to do. The same walk can also return *another name's records* (a NXDOMAIN for
the real name followed by a wildcard under a search domain answers for a domain the user never typed).

A development host has no search domain, so no local run could see any of it. Module 3's From-gate had the same bug and it was
found only because a GitHub runner has one (M3-CI-OD-1). `asDNSNotFound` also returned `IsNotFound` alone, so an error that
was both not-found and temporary counted as proof of non-existence.

## Decision

1. **Every name that reaches the DNS client is rooted** (`fqdn`: a trailing dot), inside `NetResolver`, for both the MX and the
   A/AAAA lookup. A rooted name is looked up exactly as written; the search list and `ndots` are never consulted.
2. **"Does not exist" is the narrow door.** `asDNSNotFound` is true only for a `*net.DNSError` that is `IsNotFound` and **neither**
   `IsTimeout` **nor** `IsTemporary`. SERVFAIL, timeout, a refused or unreachable server, a deadline, a malformed reply and any
   non-DNS error are not non-existence, and stay `mxError → unknown`.
3. **A permanent CI fixture** (`dns-search-domain` job): the layers' DNS tests run inside a digest-pinned container whose
   `resolv.conf` carries `search search-domain.test`. The DNS server is **in the test binary** (an authoritative server on loopback
   built on `golang.org/x/net/dns/dnsmessage`, already a dependency), so nothing needs starting, nothing skips, and the same tests
   run in `go test ./...`. Each case asserts the **verdict** (SERVFAIL → `Unknown`, never `Fail`) *and* the **wire** (the only
   name ever asked is the rooted one — a suffixed query is the defect). `TestSearchDomainCanary` makes the fixture prove itself: where
   a search list exists it asserts that Go's *unrooted* lookup really does collapse SERVFAIL into not-found, and it fails if the
   fixture claims a search domain it does not have.
4. **The fixture is proven to red without the fix, on every run.** The paired control `check-dns-search-domain-selftest.sh` builds a
   MUTANT with the one rooting line disabled (the mutation is verified to have landed), runs the same container, and requires the
   SERVFAIL case to fail with the collapse signature (`MXLayer result="fail"`) while the canary passes.
5. `NewNetResolverAt(addr)` exists so tests can point the **production** resolver at a controllable server. It replaces the server
   only; no production path uses it.

One interpretation, recorded: the ruling reads "only a genuine NXDOMAIN". Go reports NXDOMAIN and NODATA identically and does not
expose the rcode, so this is implemented as **genuine not-found for the exact (rooted) name**. Splitting the two would need a raw DNS
client in place of `net.Resolver`; it is not needed to stop a failure becoming a fact, and it is not done.

## Audit — every DNS lookup in module 1, and in the sender modules

| where | lookups | disposition |
|---|---|---|
| `control-plane/internal/layers/resolver.go` | `LookupMX`, `LookupIPAddr` — **the only explicit DNS in the Go control plane** | **fixed**: rooted, strict. `mx`, `null_mx` and `provider_classify` all read the one memoized `Evidence.mx()`, so all three are covered. |
| control plane, implicit | `net/http` to the verifier nodes and any vendor endpoint; the Postgres host | **unchanged, noted**: resolved by the Go transport from operator-configured addresses; no verdict is derived from the *absence* of a name. |
| Ruby verifier node (`truemail` 3.3.1) | `Truemail::Dns::Worker < Resolv::DNS`, names passed **unrooted** | **reported, not changed — see below** (Door 5). |
| M2 (`coldmail-send-managed`) | **none explicit.** The worker dials the user's SMTP host by name (`net.connect({host})`, system resolver). | any connect/DNS error becomes `connect_refused` → `classTimeout` (requeue, backoff, **no penalty**); never a bounce or an account fault. |
| M3 (`coldmail-send-byo`) | the From-gate — fixed under M3-CI-OD-1 — and the same worker dial as M2 | see its own ledger. |

### How the Ruby verifier node resolves (measured against a fixture DNS server, Ruby 3.2.11)

- truemail builds a fresh `Truemail::Dns::Worker` per call. With `dns: []` (default) it uses the system configuration, **search list
  included**. With `dns: [servers]` it uses those servers but `Resolv` then **derives a search list from the container's hostname**
  (measured: hostname `box.search-domain.test` → search domain `search-domain.test`).
- `getresources` returns **`[]` for a genuine NXDOMAIN, for SERVFAIL and for a timeout alike**, with no exception. truemail therefore
  cannot tell "no MX" from "DNS failed" and reports `target host(s) not found` for both. This is inside the gem and Door 5 forbids
  touching it.
- Unlike Go, Ruby **does not** fall through to a search-suffixed name on SERVFAIL, but **does** on NXDOMAIN: with a wildcard MX
  under the search domain, an unrooted lookup of a nonexistent name returned `wrong-host.test` after asking `…wild-search.test` on the
  wire, while the rooted lookup returned nothing and asked once.
- **Why the verdict is nonetheless safe.** `invalid` can only be minted from an SMTP transcript code. A node response with no
  transcript (`!PortOpened && !Reached`) classifies as `unknown / connection_failed`, so truemail's own DNS failure can never
  become an `invalid`. And the node only runs after the Go cheap path has found the real name's MX, which makes a node-side
  NXDOMAIN for the same name unlikely.
- **The residual exposure** is therefore narrow: the node resolving a *different domain's* MX after an inconsistent NXDOMAIN, on a
  box whose resolver has a search domain with a wildcard. **Mitigation is deployment hygiene, not code:** run the node with no
  search domain and without a dotted container hostname. A fork-owned startup guard (warn or refuse to start when either is true) is
  an option and is **not** implemented here; it is a change to the node's boot behaviour and needs the owner's call.

## Not changed, found while auditing — needs a ruling

> **RESOLVED 2026-11-09 by [ADR-0020](ADR-0020-implicit-mx-rfc5321.md)** (ruled: implicit MX per RFC 5321 §5.1; null MX and
> no-A/AAAA stay `invalid`; an outage is `unknown`; a probe that cannot run leaves `risky`). What follows is the finding as it was
> recorded, kept for the record.

**An address at a domain with an A record and no MX is reported `invalid / no_mx`.** `resolveMX` treats *any* not-found from
`LookupMX` as proof of no mail route and returns before the A/AAAA fallback that its own comments describe (RFC 5321 §5.1, implicit
MX). Go reports NODATA on an MX query as an error with `IsNotFound`, so that fallback is dead code against real DNS. Measured
through the production layers: an A-only name yields `MXLayer` = `Fail` with a single query, `MX aonly.test.`, and no A lookup.
This is independent of the search list and was true before this change. It changes which real domains are called invalid, so it is
**not** folded into a DNS-hygiene fix. If the intent is RFC 5321 §5.1, the repair is small (on a not-found MX, ask for A/AAAA and
only a not-found *there* is `mxNone`) and the existing fixtures and the layer's detail text already assume it. Owner ruling needed.
Null-MX detection was probed in the same way and works against real DNS (`nullmx.test` → `mx=fail`, `null_mx=pass`).

## Addendum 2026-11-09 — the node's resolver rule is now a control, not a sentence

The residual exposure above was left as a deployment rule ("no search domain, no dotted container hostname") with a startup guard
offered but not built. The owner ruled that **a documented deployment rule is not a control**, and the rule is now enforced by the
mechanism that already decides whether a node may do SMTP: the host audit.

- **Where.** The fork's `GET /v1/audit` (`TruemailServer::Wrapper::ResolverHygiene`, FORK.md row 16) adds one warning, `resolver`, to the
  gem's own `ptr`/`dns` warnings when the node's resolver is not clean. The brain needed **no change**: green has always been
  "warnings == {}", the brain re-derives it (never trusting the box's `audit_status`), the first red quarantines, and a node that is not
  active and green is never `:smtp`-eligible. Pinned on the exact wrapper JSON by `internal/audit/resolver_warning_test.go`, and end to
  end — real scheduler, real store, `Verify` — by `internal/orchestrator/resolver_hygiene_test.go` (the node is never called; the verdict is
  the cheap `risky/plausible`, never `invalid`; the control, a clean audit, does run the probe).
- **What turns the audit red.** A `search` line; a `domain` line; `options ndots:N` with N > 1 (all in `/etc/resolv.conf`, as ruled) —
  **and a hostname containing a dot, which the ruling did not list and which is added on purpose.** Read in the image's Ruby 3.2.11,
  `Resolv::DNS::Config` derives a search domain from the hostname's domain part whenever no explicit one is in effect: on every box
  whose resolv.conf is clean, and on *every* box where truemail is given `dns:` servers (the worker then hands Resolv a Hash and
  resolv.conf is never read). A PTR-compliant mail box very often has an FQDN hostname, so a resolv.conf-only check would pass exactly
  the boxes most likely to be affected. ADR-0019's own rule already said "without a dotted container hostname"; the control covers the
  whole rule. A resolv.conf that exists but cannot be read is red as well (the gate does not guess); a missing one is not (no
  configuration, nothing to flag).
- **Deliberately over-approximate.** A `search` line is flagged even when `dns:` servers are set (and resolv.conf is therefore unused), and
  `ndots` > 1 is flagged although it only bites together with a search domain: the gate refuses on *not provably clean*, never on
  *provably harmful*.
- **Proof, against the real thing.** `scripts/check-resolver-hygiene.sh` runs the real fork module under the node image's own digest-pinned
  Ruby, in containers whose `/etc/resolv.conf` and hostname are the fixture's. It asserts the expected verdict for 8 cases **and** that the
  check is *sound against an oracle*: wherever Ruby's own `Resolv::DNS` (asked, in both truemail modes, which names it would try for a
  relative name) would use a search domain, the check is red. Its self-test builds three mutants — search/domain rule off, hostname rule
  off, ndots rule off — and requires each to go red, for that rule only (the search and hostname mutants are also caught by the oracle).
  The RSpec examples inject their inputs, because a GitHub runner's container has a `search` line and a spec that read the machine would
  be red there and green on a developer's PC.
- **What an operator will see.** A box whose hostname is an FQDN (`verifier.example.com`) now audits red. Give it a short hostname; the
  PTR and the A record carry the FQDN, the hostname does not need to. Runbook Stage 0/1.
- **Door 5 untouched:** the gem is not patched; the wrapper only adds a fact to a map it already returns.

## Consequences

- A SERVFAIL, a timeout and an unreachable resolver reach the policy as `mx:unknown → unknown:dns_error`, on every host.
- A genuine NXDOMAIN is still `invalid / no_mx` — the strictness did not weaken the real invalid (an explicit control case).
- The cache-poisoning variant is closed too: a not-found-and-temporary error can no longer count as non-existence.
- Controls, each shown to fail when its property breaks: rooting disabled → the SERVFAIL case, `TestNetResolver_RootsEveryName` and
  `TestFQDN` fail while the canary passes; the old `return dnsErr.IsNotFound` restored → `TestAsDNSNotFound_IsTheNarrowDoor` fails
  on its two OLD-BUG rows.
- `check-test-enumeration.sh` requires the gate in `ci.yml` and in `Docs/test-strategy.md` §5 (item 11); both are done.
