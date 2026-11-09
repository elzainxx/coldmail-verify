# ADR-0020 — A domain with no MX but an A/AAAA record receives mail at that host (RFC 5321 §5.1): it is not `invalid`

- **Status:** ACCEPTED (2026-11-09) — ruled by the owner ("M1: IMPLICIT MX (RFC 5321 §5.1)"), closing the open decision that
  ADR-0019's audit left ("Rule on A-only domains").
- **Kind:** correctness of the module's founding rule — **`invalid` only when non-existence is provable** — applied to the one place
  it was still broken. **Door 5 is untouched**: the pinned `truemail` gem was only *read*.
- **Deciders:** owner.
- **Governs:** `control-plane/internal/layers/layers.go` (`resolveMX`, `impliedMX`) · `control-plane/internal/layers/resolver.go`
  (`NetResolver.LookupIP`) · `control-plane/internal/dnsfixture/` · `control-plane/internal/layers/dns_realdns_test.go` ·
  `control-plane/internal/orchestrator/implicit_mx_test.go` · `scripts/check-implicit-mx.sh` and its paired
  `scripts/check-implicit-mx-selftest.sh` · `.github/workflows/ci.yml` (`dns-search-domain` job, which now carries both DNS gates).

---

## Context

`resolveMX` treated *any* not-found answer to the MX query as proof that the domain cannot receive mail, and returned `mxNone` →
`mx:fail` → **`invalid / no_mx`** before it ever asked for an address. RFC 5321 §5.1 says otherwise: a domain with no MX record but
an A or AAAA record receives its mail at that host (the *implicit MX*). Reporting such an address `invalid` states, as a fact about
the user's address, something that is false. That breaks the one rule this module exists to keep.

The code had an implicit-MX fallback, and it was **dead code against real DNS for the whole life of the project**:

- Go's `net.Resolver.LookupMX` reports **NODATA as an error** with `IsNotFound` — exactly like NXDOMAIN. It never returns an empty list
  for "this name exists and has no MX". The fallback sat behind `if len(real) > 0 { … }` *after* the error return, so it ran only for an
  empty list with no error: a shape the real client does not produce.
- The test fixtures were fakes (`fixtureResolver`) and answered "no MX" with `return nil, nil` — precisely that impossible shape. So
  the fallback was exercised, every test passed, and no real domain ever reached it.
- Measured and recorded in ADR-0019: an A-only name produced `MXLayer = Fail` after a single `MX aonly.test.` query and no address query.

Go cannot expose the response code, so NXDOMAIN and NODATA cannot be told apart from the MX answer. The only way to know is to ask for the
address.

## Decision

1. **A not-found MX answer is followed by an address lookup** (`impliedMX`):
   - A and/or AAAA present → **`mxFound`**, with the domain itself as the (preference 0) mail host. The cheap layers pass and the **SMTP
     probe runs against that host**.
   - MX **and** A **and** AAAA all definitively not found → **`mxNone`** → `invalid / no_mx`. This is the only provable "no mail route".
     (It includes a domain that exists but carries only other record types, e.g. TXT: three authoritative "nothing here" answers.)
   - SERVFAIL, timeout, or any other non-not-found error at **any** step → **`mxError`** → `unknown / dns_error`. An outage of ours is
     never a fact about the address (ADR-0019, unchanged).
2. **Null MX (RFC 7505) stays `invalid`**, decided *before* any address is asked for — so a null-MX domain that also has an A record (the
   apex serving a website is common) is not rescued by it. `nullmx-with-a.test` is a control.
3. **If the probe cannot run, the verdict is `risky`, never `invalid`.** This needed no new code and is now pinned end to end: a clean
   `mx:pass` yields the cheap residue `risky / plausible`; when the SMTP stage is eligible but cannot run (red audit, no eligible node,
   warmup cap, rate limit, or not wired at all) the cheap verdict stands with `signals.smtp_skipped_reason` and is **not cached** (PB1).
   One distinction, recorded: if the probe *does* run but the node reports no host to connect to (truemail's `target host(s) not found`,
   no transcript) the existing SMTP stage says **`unknown / connection_failed`** — also never `invalid`. Both are pinned.
4. **`NetResolver.LookupIP` asks for A and AAAA as two separate lookups** and combines them itself (see *The Go error race*). Any address
   from either family → the addresses; both definitively absent → a not-found error; anything else → a non-not-found error (→ `unknown`).
5. **Real-protocol fixtures, shared.** The in-process DNS server moved from `dns_realdns_test.go` into `internal/dnsfixture` (test-only —
   `TestNotImportedByProductionCode` fails if production code imports it) so the layers *and* the orchestrator drive the **production**
   resolver over the real DNS protocol. Its zone serves real shapes — NODATA is `NOERROR` + empty answer + SOA in the authority section,
   checked on the wire by `TestZoneShapes` — including A-only, AAAA-only, dual-stack, a CNAME alias, a TXT-only name, null MX with and
   without an A record, and every partial failure of the follow-up.
6. **Paired controls, as the ruling requires.** A-only → *not* invalid; truly no records → invalid; NXDOMAIN → invalid; null MX → invalid
   (with and without an A record). Plus the controls that make them mean something (below).

## The Go error race (found while implementing; this is why decision 4 exists)

The obvious follow-up is `LookupIPAddr(domain)`. It is wrong. Go's combined lookup sends A and AAAA concurrently and, for a **rooted**
name, keeps the **last error to arrive** (`goLookupIPCNAMEOrder`: `else if lastErr == nil || fqdn == name+"."  { lastErr = result.error }`).
With A = NODATA and AAAA = SERVFAIL it returns "not found" or "server misbehaving" depending on which goroutine finished last. "Not found"
is the one answer this module turns into `invalid`, so a *partial* outage of ours would sometimes have been stated as a fact about the
user's domain — ADR-0019's defect, reintroduced by the fix for ADR-0020's. Asked separately (`ip4`, `ip6`), each error is unambiguous.
`TestNetResolver_LookupIP_CombinesTheTwoFamiliesExplicitly` pins all 13 combinations (run 20× each, the two lookups being concurrent), and
the real-DNS matrix repeats the partial-failure shapes 40× so a regression to a combined lookup cannot hide behind scheduling luck.

## How the verifier node behaves (read, not changed)

truemail 3.3.1's MX validator, in its default RFC flow, tries `hosts_from_mx_records? → hosts_from_cname_records? → host_from_a_record?`,
so **the node already implements the implicit MX**; our fork leaves `not_rfc_mx_lookup_flow` at its default (`false`). The brain was the only
party still stopping at "no MX". If that flag were ever set `true` the node would stop doing it, and an A-only domain would come back as a
transcript-less response → `unknown / connection_failed` — still never `invalid`.

## Consequences

- **Cost.** A domain whose MX answer is not-found now costs three queries (`MX`, `A`, `AAAA`) instead of one, because Go cannot tell NXDOMAIN
  from NODATA. `A` and `AAAA` run in parallel, so latency grows by one round trip, not two. Typo'd domains (frequent in lists) pay it. A
  raw DNS client that sees the rcode would avoid it and is **not** adopted: it replaces `net.Resolver` for a saving, and the failure it would
  remove is a cost, not a wrong verdict.
- **No historical verdict changes** — nothing is deployed (per the ruling).
- **ADR-0019's gate keeps its teeth, by a different route.** With the follow-up in place, Go prefers the *original* name's error for address
  lookups, which masks the search-list collapse for a plain SERVFAIL (the MX collapses to not-found, then the address lookup returns the real
  SERVFAIL). The rooting-disabled mutant therefore no longer turns the plain `servfail.test` row into `Fail`. The collapse still reaches the
  verdict where the name has **no address records**, so a zone entry `mxservfail-noaddr.test.` (MX SERVFAIL, A/AAAA NODATA) was added and is
  now the verdict-level signature that `check-dns-search-domain.sh --mutant` requires; the plain SERVFAIL row must still go red by its wire
  assertion (a search-suffixed query). Found because the mutant was re-run after the change instead of assumed.
- **Controls, each shown to fail when its property breaks** (`scripts/check-implicit-mx-selftest.sh`, run on every CI build):
  - *follow-up removed* (the pre-ADR-0020 behaviour): the A-only rows fail with `MXLayer result="fail" outcome=2`, and 4 of the 5 end-to-end
    tests go red (A-only reaches no probe; outages become `invalid`); the no-records / NXDOMAIN / null-MX rows keep their verdicts.
  - *any AAAA failure counted as "absent"* (the race's failure mode): the `A = NODATA, AAAA = SERVFAIL` row fails with `result="fail"` where
    `unknown` is required, `TestNetResolver_LookupIP_…` fails, and the end-to-end outage test goes red.
  - `TestMXLayer_RealDNS_ImplicitMX_LegacyLogicFailsTheFixture` runs a frozen copy of the old `resolveMX` against the same real-DNS fixture and
    requires it to call every A-only shape `invalid` *and* to never ask for an address — in `go test ./...`, so the fixture's ability to see
    the defect is itself continuously proven.
- `fixtureResolver` (the fake) now carries a warning that its `return nil, nil` is a shape real Go never produces.

## Found while auditing — NOT changed, needs a decision

**Nothing restricts where the SMTP probe connects.** The node resolves the mail host's address and connects to whatever it is — loopback,
RFC 1918, link-local, a cloud metadata address — on port 25. I searched `control-plane/`, the node's `app/` and `config/`, the compose file
and the runbook for any destination filter and found none; the pinned gem has none either (`IPAddr` appears only in an exception rescue and
in the PTR audit). This is **not new**: a user can already publish `MX 10 evil.example.` with `evil.example. A 10.0.0.5`. The implicit-MX
ruling adds one more way to the same place (the domain's own A record) and no new capability, so it is not folded into this change. The
exposure is bounded to port 25, but the SMTP transcript is persisted verbatim in the raw sidecar (§6.4), so an internal SMTP banner would be
recorded. Because Door 5 forbids touching the gem, the control belongs at the network edge (an egress policy on the verifier box denying
non-public destinations on 25) or in a fork-owned pre-flight; either is a deployment/design decision. Recorded as a 🔴 item in
`go-live-runbook.md` Stage 0.
