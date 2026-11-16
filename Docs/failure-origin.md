# Failure origin — who failed when M1 could not answer (`signals.failure_origin`)

**Status:** LIVE. Added by the P2-research close-out (gap M1-G4), additive to Door 1 (an open `signals` key; no field, status,
reason, confidence, recommendation, caching decision or retry changed). **Code:** `control-plane/internal/failureorigin`
(the mapping), `internal/verdict/assemble.go` (the one place it is written), the evidence is recorded by `internal/layers`
(DNS), `internal/smtp` (transcript), `internal/orchestrator` (our node) and `internal/queue` (the attempt cap).

> **The sentence the platform bills on.** An unknown caused by the recipient's server is charged; one caused by us, or that
> cannot be proven either way, is free.

So the error that matters is a false `remote`: it charges a customer for **our** failure. Every row below is `remote` only
where the evidence in hand proves the recipient's side caused it, and **anything M1 cannot prove is `undetermined`** — never a
guess toward `remote`. The default for a reason, a cause or a code path nobody mapped is `undetermined`.

## 1. What the key says

`signals.failure_origin` is present on **every `unknown` result** (the response and the LOG row carry the same value), with
exactly three values:

| Value | Meaning |
|---|---|
| `own` | the cause is on our side of the wire: our verifier node, our egress, our resolver, our queue, or a policy or reputation block against our IP |
| `remote` | the recipient's side provably caused it: their server answered about the recipient, or accepted the connection and then stayed silent, or their domain's DNS failed behind a working resolver of ours |
| `undetermined` | M1 cannot prove either side |

It also appears, as `own`, on a **non-`unknown`** result that is a degraded cheap-only fallback caused by us: a `risky` answer
whose `signals.smtp_skipped_reason` is `no_eligible_node`, `audit_red`, `warmup_cap`, `rate_limited` or `smtp_unwired` (section 3).
It is never present on a normal answer, and never on `provider_mx_routed` (a settled routing outcome).

## 2. How it is computed — in one place

1. The stage that sees the failure records its **cause** — the evidence it holds (a DNS error class, a node error class, a
   transcript shape). A cause is internal; it is never serialised.
2. `failureorigin.OriginFor(reason, cause)` is a **pure function** over the table in section 3. One reason-level rule precedes the
   table: `unknown/smtp_blocked` is **always** `own` (a policy or reputation block is a statement about our IP; it matches M1's own bounce classification). A pair that is not in
   the table is `undetermined`.
3. `verdict.Assemble` — the single place a verdict becomes a response and a LOG row — calls `failureorigin.Of` and writes the key.
   Because it is written there and only there, **every unknown this assembler builds carries it by construction**: a new code
   path that mints an unknown cannot forget the key, and a stage cannot assert its own origin through the signals bag (a forged
   or stale value is overwritten).

The evidence flows: DNS — `layers.causeRecorder` remembers the last resolver error, `layers.dnsCause` classifies it,
`MXLayer` puts it on its `unknown` output, `policy.Evaluate` carries it to the verdict; SMTP — `smtp.CauseOf(Parsed)` reads the
same fields `Classify` reads; our node — `orchestrator.smtpFromNodeError`; the queue — `queue.ErrorCappedOutcome`.
**Nothing in this path changes a status, reason, confidence, recommendation, cacheability or the queue class** — those tests
(`TestFailureOrigin_SMTPStage_ThroughTheRealLayers` asserts the pre-existing queue class per row) pin it.

## 3. The mapping: one row per code path that yields `unknown`

The table is checked against the code by `TestDoc_MappingTableMatchesTheCode` (`internal/failureorigin`): every row of
`failureorigin.Rows()` must appear here and every row here must give exactly this origin from `OriginFor` — the doc cannot drift from
the function. File:line references are as of this writing.

<!-- origin-table:begin -->
| reason | condition | cause | origin | branch (file:line) | test |
|---|---|---|---|---|---|
| `dns_error` | no resolver is wired into the evidence (our own configuration) | `dns_no_resolver` | `own` | `layers/layers.go:126-128`; gate `policy/coldemail.go:49` | `TestDNSFailureCause_NoResolverWiredIsOwn` |
| `dns_error` | our resolver did not answer inside our budget: a socket read timeout, or our own context deadline or cancel — on the MX query or on the A/AAAA follow-up | `dns_resolver_timeout` | `own` | `layers/layers.go:157` (MX), `:194` (follow-up); classified `layers/dns_cause.go:75,85` | `TestDNSFailureCause_RealDNS_WhoFailed` (`blackhole.test`, `a-timeout.test`); `TestFailureOrigin_DNSError_ThroughTheRealLayers` |
| `dns_error` | a socket-level failure talking to the resolver we configured (refused, unreachable, write error): a DNS client error that names the server and is not a reply | `dns_resolver_unreachable` | `own` | `layers/layers.go:157,194`; classified `layers/dns_cause.go:79-83` | `TestDNSFailureCause_RealDNS_AnUnreachableResolverIsOwn`; `TestFailureOrigin_DNSError_ThroughTheRealLayers` |
| `dns_error` | our working resolver REPLIED SERVFAIL ("server misbehaving", temporary) for the MX query or the A/AAAA follow-up: it reports that the domain's name servers gave it no answer | `dns_servfail` | `remote` | `layers/layers.go:157,194`; classified `layers/dns_cause.go:77-78` | `TestDNSFailureCause_RealDNS_WhoFailed` (`servfail.test`, `mxservfail.test`, `mxservfail-noaddr.test`, `aaaa-servfail.test`, `a-servfail.test`); `TestFailureOrigin_DNSError_ThroughTheRealLayers` |
| `dns_error` | any other DNS error: a non-SERVFAIL bad rcode (REFUSED, FORMERR, NOTIMP), a malformed reply, a lame REFERRAL from a server that is not recursive, a cgo-resolver error with no server, an error that is not a DNS error | `dns_unclassified` | `undetermined` | `layers/layers.go:157,194`; classified `layers/dns_cause.go:88` | `TestDNSCause_ClassifiesTheErrorShapesGoProduces` (unit level: the DNS fixture cannot produce these shapes) |
| `smtp_timeout` | no HTTP response from our verifier node (unreachable, refused, the brain-side deadline): `node.TransportError` | `node_transport` | `own` | `orchestrator/smtp_stage.go:297` | `TestFailureOrigin_SMTPStage_ThroughTheRealLayers` (two rows) |
| `smtp_error` | our node answered HTTP 400 or 401 (a protocol or credential bug on our side) | `node_http_400_401` | `own` | `orchestrator/smtp_stage.go:303` | `TestFailureOrigin_SMTPStage_ThroughTheRealLayers` |
| `connection_failed` | our node answered any other non-2xx status | `node_http_status` | `own` | `orchestrator/smtp_stage.go:305` | `TestFailureOrigin_SMTPStage_ThroughTheRealLayers` (500, 503) |
| `smtp_error` | our client failed outside a transport or status error (marshal, request build, body read) | `node_client_error` | `own` | `orchestrator/smtp_stage.go:307` | `TestFailureOrigin_SMTPStage_ThroughTheRealLayers` |
| `smtp_error` | our node answered 2xx with a body that is not the wrapper's JSON | `node_bad_response` | `own` | `orchestrator/smtp_stage.go:210` | `TestFailureOrigin_SMTPStage_ThroughTheRealLayers` |
| `connection_failed` | no session in the transcript: no transcript at all (the node's own MX lookup found nothing), or `port_opened=false` on every host — our egress (port 25 blocked) and the recipient refusing look identical | `smtp_no_session` | `undetermined` | `smtp/classify.go:42`; `smtp/cause.go:24` | `TestScore_FailureOrigin_PerTranscriptShape`; `TestFailureOrigin_SMTPStage_ThroughTheRealLayers` |
| `smtp_timeout` | a timeout in a session whose TCP port the transcript shows OPEN (`port_opened`, or the RCPT stage reached): the recipient's server accepted the connection and did not answer | `smtp_silent_after_connect` | `remote` | `smtp/classify.go:46`; `smtp/cause.go:27-31` | `TestScore_FailureOrigin_PerTranscriptShape`; `TestFailureOrigin_SMTPStage_ThroughTheRealLayers` |
| `smtp_timeout` | a timeout with no evidence the TCP session ever opened (a MAIL FROM-stage error with `port_opened=false`; the engine does not report this shape, the branch is defensive) | `smtp_no_session` | `undetermined` | `smtp/classify.go:46`; `smtp/cause.go:27-31` | `TestCauseOf_EveryClassifiedUnknownHasATableRow` (unit: no real transcript has this shape) |
| `connection_failed` | a reply code at the GREETING (connection stage, port open): it answers the connecting client — our IP's reputation, postscreen — not the mailbox | `smtp_pre_recipient_reply` | `undetermined` | `smtp/classify.go:50`; `smtp/cause.go:32-36` | `TestScore_FailureOrigin_PerTranscriptShape`; `TestFailureOrigin_SMTPStage_ThroughTheRealLayers` |
| `connection_failed` | a connection-stage failure with the port open and no reply code and no timeout (a reset, a TLS error): the raw string cannot be attributed | `smtp_session_lost` | `undetermined` | `smtp/classify.go:50`; `smtp/cause.go:37` | `TestScore_FailureOrigin_PerTranscriptShape` |
| `smtp_error` | a decisive stage with no numeric reply code and no timeout (a dropped connection): the raw string cannot be attributed | `smtp_session_lost` | `undetermined` | `smtp/classify.go:64`; `smtp/cause.go:37` | `TestScore_FailureOrigin_PerTranscriptShape`; `TestFailureOrigin_SMTPStage_ThroughTheRealLayers` |
| `smtp_error` | an ambiguous 550 at RCPT (not no-such-user, not policy language) with a transcript | `smtp_reply` | `remote` | `smtp/classify.go:86`; `smtp/cause.go:32-34` | `TestScore_FailureOrigin_PerTranscriptShape`; `TestFailureOrigin_SMTPStage_ThroughTheRealLayers` |
| `smtp_error` | any other 5xx reply at RCPT (503, 5xx the classifier does not know) | `smtp_reply` | `remote` | `smtp/classify.go:115,118`; `smtp/cause.go:32-34` | `TestScore_FailureOrigin_PerTranscriptShape` |
| `smtp_error` | an ambiguous 550 (or other 5xx) at MAIL FROM, before the recipient was named: about our sender, not the mailbox | `smtp_pre_recipient_reply` | `undetermined` | `smtp/classify.go:86,115`; `smtp/cause.go:32-36` | `TestScore_FailureOrigin_PerTranscriptShape`; `TestFailureOrigin_SMTPStage_ThroughTheRealLayers` |
| `smtp_blocked` | a 550 whose body carries policy or reputation language (blocked, blacklist, spam, rbl, policy, refused, denied…) | `smtp_reply` | `own` | `smtp/classify.go:84`; reason rule `failureorigin/failureorigin.go` (`OriginFor`) | `TestOriginFor_OwnerRulings`; `TestFailureOrigin_SMTPStage_ThroughTheRealLayers` |
| `smtp_blocked` | a 554 at RCPT with no no-such-user body | `smtp_reply` | `own` | `smtp/classify.go:99` | `TestScore_FailureOrigin_PerTranscriptShape`; `TestFailureOrigin_SMTPStage_ThroughTheRealLayers` |
| `greylisted` | a 4xx at RCPT (450, 421, any 4xx but 452): the recipient's server's own deferral | `smtp_reply` | `remote` | `smtp/classify.go:106,111`; `smtp/cause.go:32-34` | `TestScore_FailureOrigin_PerTranscriptShape`; `TestFailureOrigin_SMTPStage_ThroughTheRealLayers` |
| `greylisted` | a 4xx at MAIL FROM: a deferral aimed at the connecting client | `smtp_pre_recipient_reply` | `undetermined` | `smtp/classify.go:111`; `smtp/cause.go:32-36` | `TestScore_FailureOrigin_PerTranscriptShape`; `TestFailureOrigin_SMTPStage_ThroughTheRealLayers` |
| `unknown_error` | an item's policy-resolve, decrypt or compute error repeated `TM_QUEUE_ERROR_MAX_ATTEMPTS` times (the queue settles it instead of requeueing forever) | `error_capped` | `own` | `queue/drainer.go:365` (`errorRetry`), `:383` (`settleErrorCapped`), `:400` (`ErrorCappedOutcome`) | `TestDrainer_ErrorCap_SettlesExactlyAtTheCap`; `TestErrorCappedOutcome_IsALegalVerdictBuiltFromTheItemAlone` |
| `smtp_error` | the scorer's defensive fall-through after its status switch (unreachable: `Classify` returns only valid, invalid or unknown) | (none) | `undetermined` | `smtp/score.go:97` | none possible: unreachable by construction; the key is still guaranteed by `TestAssemble_EveryUnknownReasonCarriesAFailureOrigin` |
| `external_verified` | the policy DATA names `external:unknown`, but the policy engine never runs the external layer and `external.Resolve` keeps the residue on a vendor unknown, so no `unknown/external_verified` is ever produced | (none) | `undetermined` | `policy/coldemail.go:61` (data only) | `TestAssemble_EveryUnknownReasonCarriesAFailureOrigin` (sweeps every reason, this one included) |
<!-- origin-table:end -->

**Retry-exhausted items.** An item the queue settles `dead` after its greylist / transient retries are spent
(`queue/drainer.go:295`, `:312`) keeps the verdict of its last attempt, so it carries that attempt's origin (a persistent 4xx at RCPT
is `remote`). Intermediate attempts never settle and never log, so the only greylisted rows a bulk job can contain are exactly
these.

**Results that are not unknown but carry `own`** (the cheap-only answer is a degraded fallback caused by us). Verdict, confidence,
recommendation, caching and queue class are unchanged:

| `smtp_skipped_reason` | when | branch | test |
|---|---|---|---|
| `no_eligible_node`, `audit_red` | no node is eligible for SMTP | `orchestrator/smtp_stage.go:117,139` | `TestFailureOrigin_DegradedCheapOnlyAnswersAreOwn_AndSettledRoutingIsNot` (red node) |
| `warmup_cap` | the node is at its warm-up cap | `orchestrator/smtp_stage.go:143,146` | same test (cap of 1) |
| `rate_limited` | every eligible node is at its per-provider cap | `orchestrator/smtp_stage.go:167,174` | `TestAssemble_NonUnknownResultsCarryTheKeyOnlyForOwnDegradation` (token level) |
| `smtp_unwired` | the SMTP stage is not wired in this process | `orchestrator/smtp_stage.go:101`, `orchestrator/orchestrator.go:343` | `TestSMTPUnwired_TheTokenIsPresent_TheVerdictIsUnchanged_AndNothingIsCached` |
| `provider_mx_routed` | **not a failure**: a settled routing outcome, no key | `orchestrator/orchestrator.go` (`reasonProviderMX`) | same degraded-answer test (the Gmail control) |

## 4. Cases that cannot be classified honestly

Every branch left `undetermined`, and why. Nothing here was forced into `own` or `remote`: each is a place where the information M1
holds does not say whose side failed.

1. **`connection_failed` with no session (no transcript, or `port_opened=false` everywhere).** A port 25 blocked at OUR egress and a
   recipient refusing connections are the same transcript: `port_opened=false` and an empty `errors{}`. The node's audit says port
   25 was open recently, but that is true of every node that reaches this stage, so it cannot discriminate. A node-side MX lookup
   that found nothing (`errors.mx: "target host(s) not found"`) is likewise the node's resolver or the domain's.
2. **A reply at the greeting or at MAIL FROM (`connection_failed`, `smtp_error`, `greylisted`).** It answers the connecting client
   — our IP's reputation, our sender identity — before the mailbox is named. DNSBL refusals and postscreen deferrals are sent
   exactly here. It is not proof the recipient is at fault.
3. **A session that was open and ended with no reply code and no timeout** (a reset, an end-of-file, a TLS error). The raw string
   is Ruby's exception message; it cannot be attributed to the recipient's server or to our node's SMTP client.
4. **A timeout with no proof the TCP session opened** (defensive: Truemail does not report a MAIL FROM-stage error with
   `port_opened=false`).
5. **Unclassified DNS errors** (REFUSED and other bad rcodes — which can be OUR resolver refusing us —, malformed replies, a lame
   referral from a resolver that is not recursive — our configuration —, a cgo resolver error, a non-DNS error).
6. **The scorer's unreachable fall-through** and **`unknown/external_verified`** (never emitted): `undetermined` by default, so the
   key is still present if either ever appears.

Judgment calls on the rulings (they are the owner's to confirm; each is one row of the table in `failureorigin.go`):

- **`dns_servfail` → `remote` rests on a premise M1 cannot verify.** A stub resolver sees only its own resolver's SERVFAIL. A
  working recursive resolver says SERVFAIL when the domain's name servers fail (unreachable, lame delegation, DNSSEC failure) — and
  also when it has trouble of its own. The ruling says `remote`; the premise is "our resolver was healthy". If that premise is not
  acceptable the honest value is `undetermined` for every DNS failure (then no DNS unknown is ever billable).
- **A resolver timeout → `own`.** It can also be slow name servers behind a healthy resolver, which would be `remote`; `own` is the
  free direction and was ruled.
- **A timeout after the TCP session was proven open → `remote`.** Silence after we know the session is up is the recipient's. It
  assumes our node's process was healthy and that the recipient is not deliberately tarpitting us (a reputation effect).
- **`greylisted` at RCPT is `remote` for a sync check and for an exhausted bulk item alike.** The ruling names "retries exhausted";
  a single sync 4xx is the same fact (the recipient's server deferred). Note that M1 classifies every 4xx but 452 as `greylisted`
  (`smtp/classify.go:101-111`), including a deferral whose body is reputation language; the same words at 5xx are `smtp_blocked` and
  `own`. Applying that body test to 4xx would make those `own`; it is not done, because the ruling is explicit.

## 5. What a consumer does with it

- Treat an unrecognised value as `undetermined`. Ignore the key on a result you do not understand (Door 1: the bag is open).
- The key never replaces `status` and `reason`: those are the answer; this says who failed to give it.
- A bulk result that is `unknown_error` with `error_capped: true` was settled because the item could not be computed on
  `TM_QUEUE_ERROR_MAX_ATTEMPTS` attempts; it is terminal, appears in the job's results and is `own`. It is not cached.
- A `risky/plausible` carrying `smtp_skipped_reason` (other than `provider_mx_routed`) is not what a working M1 would have said,
  and says `own`; re-verify later.

## 6. The tests that hold this, and why they cannot pass vacuously

| Test | What it proves | Why it cannot pass over nothing |
|---|---|---|
| `TestAssemble_EveryUnknownReasonCarriesAFailureOrigin` | every reason in the frozen vocabulary, assembled as an unknown, carries a valid key in the response and the LOG row, and never `remote` with no cause | iterates `vocab.Reasons()` and asserts it has more than 20 entries; the control is the next row |
| `TestAssemble_NonUnknownResultsCarryTheKeyOnlyForOwnDegradation` | the key is absent on every normal answer and present (as `own`) only for the five own-failure skip tokens | sweeps every status and reason; asserts the verdict fields are unchanged |
| `TestOriginFor_RemoteOnlyWhereTheTableProvesIt` | over every reason × every cause (and "no cause"), `remote` comes only from a table row that says so; `smtp_blocked` is always `own` | fails if the table has no remote rows |
| `TestDoc_MappingTableMatchesTheCode` | this table equals `failureorigin.Rows()` | fails if the markers or the rows are missing |
| `TestFailureOrigin_SMTPStage_ThroughTheRealLayers` | the whole `Verify` path with real DNS, policy, transcript parser, classifier, scorer, assembler and LOG: origin, unchanged verdict, unchanged queue class, never cached; paired control (a clean 250) per row | ends with a coverage check: every origin and every SMTP unknown reason must have been observed |
| `TestFailureOrigin_DNSError_ThroughTheRealLayers`, `TestDNSFailureCause_RealDNS_*` | the production `NetResolver` against the in-process DNS server (`internal/dnsfixture`): SERVFAIL is `remote`, a silent server and a dead resolver are `own`; the same address reads `remote` behind SERVFAIL and `own` behind a dead resolver | controls resolve normally and carry no cause |
| `TestEvaluate_DNSUnknownCarriesTheLayersCause_AndNothingElseDoes` | the hand-off layer → policy: the `unknown/dns_error` verdict carries the mx layer's cause, nothing else does, and the verdict is unchanged | controls: three non-unknown verdicts carry no cause |
| `TestScore_FailureOrigin_*`, `TestCauseOf_*` | the real transcript parser, classifier and scorer, per transcript shape; every classified unknown has a table row | shape sweep asserts at least 10 shapes classified unknown |
| `TestDrainer_ErrorCap_*` | a real `Drainer` on the real `MemStore`: still retried one below the cap, settled exactly at it (the faulty component called exactly cap times), never again | per stage and per cap; a recovered item (control) is not capped |

Ruby-level socket proof (`verifier-node/truemail-rack/test/p2`, `smtp_mock`) is not reachable from the Go tests; the transcript
shapes the node can report are exercised through the wrapper JSON the Go `fakeNode` fixture returns (the fixture every SMTP-stage
test here uses), which is the lowest real level that exists for the brain.
