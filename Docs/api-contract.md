# API contract — the served surface of the control plane

**Status:** LIVE. This file is the DOCUMENTED SET, and it is **verified against the code**, not
maintained by hope. Four tests in `control-plane/internal/api/routes_contract_test.go` run on every
`go test` and each was verified by breaking it:

| Test | Holds | Break-verified by |
|---|---|---|
| `TestRoutes_ServedSetMatchesTheDocumentedSet` | the table below == `(*Server).Routes()`, both directions | B80 (served, undocumented), B81 (documented, not served), B84 (this file deleted → FAIL, not skip) |
| `TestRoutes_HandlerServesExactlyTheRouteTable` | every pattern dispatches; unclaimed paths get the envelope's 404 | B86 (404 fallback removed) |
| `TestRoutes_WrongVerbIsTheEnvelopeNotServeMuxsPlainText` | 30 wrong-verb combinations, all envelope, correct `Allow` | B85 (405 body regressed to plain text), B87 (`Allow` drifted) |
| `TestRoutes_HandlerIsALoopOverRoutes` | exactly three `mux.HandleFunc` registrations — no route bypasses `Routes()` | B83 (route registered outside the table) |

**Why the fourth test is a source scan.** A route registered directly on the mux is invisible from
outside the process unless you already know its path, so no probe finds it. B83 demonstrated this
rather than assuming it: with an extra `mux.HandleFunc("GET /v1/quickdebug", …)` in `Handler()`, the
doc-comparison test **passed** and only the scan went red. The behavioural tests cover what is
observable; the scan covers the structural property that makes their coverage total.

**Why this file exists (fix-plan item 14).** Modules 2 and 3 have had a `contract_drift` CI gate
since their Phase 0: it probes the running Control and asserts served-set == documented-set. M1 had
the other half of the mechanism — an in-code `Valid()` assertion on the response shape before every
sync write — and no route gate at all. Each module held half. This is M1's half, adapted rather than
copied, because the senders' gate probes a live compose stack over the network and **M1 has no
compose stack** (that is fix-plan item 15). Asserting the same property in-process gets it today
instead of after packaging, and it is strictly stronger in one respect: it runs on every `go test`,
including on a laptop with no Docker.

**The route table is derived, not restated.** `(*Server).Routes()` is a declared slice and
`Handler()` is a loop over it, so the served set is the compiler's view of itself. A gate that
grepped `mux.HandleFunc(` out of the source would be narrower than the property it guards — the same
shape as a config gate satisfied by a TODO naming the key, which this project has already found.

---

## Routes

Every route this module serves. `{id}` is a ServeMux wildcard.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `POST` | `/v1/verify` | per-caller key **or** legacy static token | Verify one address synchronously; returns the Door-1 v1 response (P1 §3.1.3) |
| `POST` | `/v1/verify/bulk` | per-caller key **required** (PN-13) | Submit a bulk job as `addresses` **or** `items` (each row with a caller `item_id`); returns a job handle (§7). Row identity and dedup scope: [`bulk-item-identity.md`](bulk-item-identity.md) |
| `GET` | `/v1/jobs/{id}` | per-caller key **required** (PN-13) | Job progress counts **over input rows** (`pending` reaches 0 when complete), scoped to the owning caller (D1 tenancy) |
| `GET` | `/v1/jobs/{id}/results` | per-caller key **required** (PN-13) | One result per input row that has an outcome, **ordered by `ordinal`**, cursor-paginated (cursor = the last row returned); each is the `/v1/verify` object plus `ordinal` and `item_id` |
| `GET` | `/healthz` | none | Liveness: the process is up. Additive `at` timestamp (item 10) |
| `GET` | `/readyz` | none | Readiness with secret PRESENCE only, never values. 503 `not_ready` unless `pepper`, `db` and (once declared) `queue` are all true; a not-ready queue adds `reasons` (below) |

**Bulk row identity and dedup scope (owner rulings A1/A2, migration 0018).** The bulk request takes exactly one of `addresses`
(unchanged) or `items` (`[{"item_id": "...", "address": "..."}]`; `item_id` mandatory, non-blank, at most 200 bytes, no control
characters, unique in the request). Every result element keeps all its Door-1 keys and gains `ordinal` (always), `item_id` (when the
row had one) and, on a row that repeats an earlier address, `duplicate_of` (the ordinal of the first such row, so a duplicated
address can be charged once); `email` stays blank. A malformed address keeps its ordinal, is listed in the 202's `malformed_rows`
(at most 1000, with `malformed_rows_truncated`), is **logged before the 202 is returned** (its own `verifications` row, exactly as
`/v1/verify` logs an un-addressable input) and is a result served from that row (`invalid` / `invalid_syntax`); a repeated malformed
address is several malformed rows, never a duplicate. Identical addresses **within one job** are checked once and every row gets the
result; **nothing is deduplicated across jobs or tenants**. Read results after `complete` is `true`. The whole contract, with
examples, is [`bulk-item-identity.md`](bulk-item-identity.md); the pagination property is held by a walk over several page sizes
that must return every ordinal exactly once, with paired controls (a look-ahead cursor, an unsorted listing).

**`/readyz` body.** `status` (`ready`/`not_ready`), `ready` (bool, the same answer), `checks{}` (booleans: `pepper`,
`db`, and `queue` on a process whose boot reached the queue wiring), `at`, and the older `config`, `pepper`,
`supabaseKey`, all unchanged. When the queue is declared not ready (no `TM_QUEUE_ADDR_KEY`, a key that does not
build the cipher, or a drainer that cannot be built) the body also carries an additive top-level
`"reasons": ["queue: <why>"]`, a JSON array of strings that is **omitted entirely** when there is nothing to say.
The exact reason texts are in [`operator-runbook.md`](operator-runbook.md) §6.1.

**The `/v1` prefix is LOCKED** (build-plan §1.1, URI-path versioning). A breaking change means
`/v2`, never an in-place edit — which is the same rule `verdict.APIVersion` enforces on the body.

### Auth: the `Bearer` scheme is MANDATORY (fix-plan item 15b)

Every authenticated route takes `Authorization: Bearer <token>`. **A scheme-less bare token is
rejected**, on both credential kinds (per-caller key and the legacy static token) and both surfaces
(sync and bulk). The scheme name is matched **case-insensitively**, so `bearer`/`BEARER` are accepted:
RFC 9110 §11.1 makes the scheme case-insensitive, both senders already do this
(`platformapi/auth.go`, `adminapi/auth.go` — `strings.EqualFold`), and M1 alone used to reject a
conforming request. Closing the laxness and fixing that strictness were the same edit; a
case-sensitive fix would have swapped one cross-module divergence for another.

The ruling predates the fix twice: **OQ-13**, resolved "Bearer for `/v1/verify`"
(`history/phase-1-foundations-spec.md:774`), and `canonical-module-contract.md` §1 clause 1, which names M1's
laxness explicitly. No ADR is owed.

**Why it survived a full audit — the comment, not the code.** The old `trimBearer` fell through to
`return h` for any header lacking the prefix, justified by "the front door is bearer; the verifier-node
token is raw — kept distinct per P1 §3.5.4". Both halves are true and they describe **opposite
directions**: the raw node token is sent *outbound* by `internal/node/*_client.go` and validated by the
Ruby node (`wrapper/app.rb:176`, stock `AccessToken`). It never arrives at this package. `trimBearer`
had exactly one caller — the inbound front door — so the clause mentioning raw tokens did no work at
the only site that ran. A comment explaining why a laxness is acceptable is the most durable kind of
laxness.

A scheme-less credential now lands on the same path as no credential at all: `presented == ""` skips
the per-caller branch and fails the static branch's non-empty guard, so it is `unauthenticated` (401)
rather than being compared against a secret, and the envelope never echoes what was presented.

Held by `control-plane/internal/api/bearer_scheme_test.go` (3 tests, 9 cases), break-verified with two
splits that are **not** what they read like: **B110** (fallback restored) reddens only the two *bare*
cases — `Basic …` and `Bearer…`-no-space stay green, because with the fallback they are rejected for
the wrong reason (compared whole, matching no secret); **B111** (case-sensitive `CutPrefix`, the naive
fix) reddens *only* the case test while every refusal case stays green; **B112** (always return `""`)
reddens both authenticating tests; **B113** (static compare neutered) reddens only the positive
control, which is what makes it non-redundant with the case test.

### `api_version` is DERIVED and never invented (fix-plan item 14, M1's own half)

Item 14's property, identical in all three modules: **declare the version once, derive what you
publish from it, and refuse rather than emit a version you cannot vouch for.** M1 declares it once as
`verdict.APIVersion` and enforces it with `verdict.Response.Valid()`, which every writer calls before
the write. Both senders derive theirs from the served route table instead; the property is the same,
the mechanism differs per module.

**The gap this closed, found by adopting the senders' gate rather than by reading the code.** §7.4
promises a bulk result is byte-identical to a sync verdict. The sync path honoured that —
`handleVerify` refuses a response failing `Valid()` (`api.go:328`, `contract_violation`). The bulk
results path did **not check at all**, and `verdictFromRow` coerced a missing `api_version` to `"v1"`:

```go
APIVersion: firstNonEmpty(row.APIVersion, "v1"),   // REMOVED
```

So the two halves of one promise had different enforcement, and the weaker half was the one serving
stored, PII-adjacent rows. The coercion was worse than a missing check because it was
**self-concealing**: it guaranteed the field always looked right, so no caller-side check could ever
observe it missing. `firstNonEmpty` was deleted along with it — a helper whose only purpose is to
invent a default invites re-introducing the defect at the next field that looks empty.

**The bad row is reachable, not hypothetical.** `migrations/0001_verifications.sql:41` declares
`api_version text NOT NULL` with **no CHECK**, and status/reason/recommendation/confidence are
likewise unconstrained at the column level. An older build, a partially-applied migration, or a
future `/v2` writer each produce a row this surface must refuse.

**What a caller sees.** `GET /v1/jobs/{id}/results` answers `contract_violation` (500) if **any** row
on the page fails `Valid()`, and ships no partial page. Failing the page is deliberate: filtering the
bad rows out would look friendlier and be silent data loss — a page that quietly omits rows is
indistinguishable from one whose omitted rows were never submitted, and `next_cursor` still advances,
so the caller has no field to detect it by. Tenancy still short-circuits first: a job that is absent
or another caller's is `not_found` before the page is read, so the refusal code cannot become a
cross-tenant oracle.

Held by `control-plane/internal/api/job_results_contract_test.go` (8 tests), each break-verified:
B106 restored the coercion (only the empty-version cases went red, proving the other four control the
missing `Valid()` call independently), B107 deleted the `Valid()` branch (all five refusal cases red,
positive control green), and B109 replaced the page refusal with a per-row skip plus an
all-rows-bad check — which left every single-row case green and only
`TestJobResults_ARefusedRowFailsTheWholePage` red, which is that test's whole reason for existing.

### Not served, deliberately

- **No `/metrics`.** `internal/metrics` computes block/unknown/cache rates and latency percentiles
  with no HTTP exposure. That is fix-plan item 15b, open, and listed here so its absence reads as a
  known gap rather than an oversight.
- **No platform-facing admin surface.** M1's Tier 4 platform routes were declined; see
  `../../coldmail-platform/alignment-fix-plan.md`.

---

## Errors

Every error body from a handler in this package is the item-18 envelope, written by the one
serializer in `internal/api/errors.go`:

```json
{"error":{"code":"validation_failed","message":"One or more fields are invalid.",
          "details":[{"field":"email","issue":"required"}],
          "request_id":"01997f4e-...-7000-8000-..."}}
```

`code` is machine-readable and additive-only; `message` is a curated constant, never interpolated
from input; `request_id` is a UUIDv7 valid in the LOG's `uuid` column, so the body, the LOG row, and
the one log line an operator greps all name the same id.

| Code | Status | Means |
|---|---|---|
| `unauthenticated` | 401 | Missing or invalid credentials |
| `validation_failed` | 400 | A field is missing, malformed, or out of range. `details[].issue` is one of `required`, `malformed`, `out_of_range`, `not_found`, and (bulk, 0018) `conflict` (both `addresses` and `items` given) and `duplicate` (a repeated `item_id`; the field is `items[N].item_id`) |
| `not_found` | 404 | No such resource — **or** it belongs to another caller (tenancy scoping, D1: the two are indistinguishable on purpose) |
| `method_not_allowed` | 405 | A real path, a verb it does not serve |
| `payload_too_large` | 413 | Over the §7.5 submission cap |
| `unavailable` | 503 | This deployment is not wired (no pepper, no reachable DB) — page someone |
| `paused` | 503 | An operator pulled kill-switch Rung 4 — retry later, page nobody |
| `internal_error` | 500 | An unexpected failure |
| `contract_violation` | 500 | A Door-1 shape check failed; never lost in generic 500 noise |

`unauthenticated`, `validation_failed`, `not_found`, `paused`, and `method_not_allowed` are spelled
BYTE-IDENTICALLY to the senders' codes so a platform client's error handling is one switch across
three modules; that is asserted by
`TestErrorEnvelope_CodesSharedWithTheSendersMatchByteForByte`, not assumed.

### 405 and 404 come from this module, not from ServeMux

**Found while adopting the senders' gate (item 14), and it is the same defect module 3's audit D17
found in module 3.** Before this change, every wrong verb and every unknown path on M1 was answered
by `net/http.ServeMux` itself:

```
GET     /v1/verify   -> 405 text/plain  "Method Not Allowed\n"   (no code, no request_id)
DELETE  /healthz     -> 405 text/plain  "Method Not Allowed\n"
GET     /v1/nope     -> 404 text/plain  "404 page not found\n"
```

So a client doing `body.error.code` on any non-2xx got a nil deref on those paths, and there was no
`request_id` to correlate — the exact operational gap item 18 was built to close, surviving in the
one place item 18 could not see, because it never reached a handler. Both are now the envelope:
`method_not_allowed` with a correct `Allow` header, and `not_found` for an unknown path.

`OPTIONS` is served on no route: Go's ServeMux does not auto-answer it, and this module has no CORS
surface. An `OPTIONS` request therefore gets `method_not_allowed`, which is what the wrong-verb tests
probe with.

---

## Appendix A — Reading a verdict (what each field means, in plain language)

*Folded in from the former end-user guide (kept whole in [`history/USER_GUIDE.md`](history/USER_GUIDE.md)) and corrected
against the code on 2026-11-13. This appendix has no route rows; the route table above is the only thing
`TestRoutes_ServedSetMatchesTheDocumentedSet` reads.*

Each result has four parts: a **status** (the headline answer), a **reason** (the single most important cause), a
**confidence** (0–100, how strongly the evidence supports the status) and a **recommendation** (what to do next). The
vocabulary is frozen and additive-only: a value means the same thing today as on the first day the system ran, so you can
build rules on it. An unrecognised `status` should be treated as `unknown` and an unrecognised `reason` as opaque.

### A.1 The four statuses

| Status | What it means | In practice |
|---|---|---|
| **valid** | Positive evidence the mailbox can receive mail. | Safe to send. |
| **invalid** | Positive evidence it cannot. | Don't send. |
| **risky** | Real enough to accept mail, but sending carries elevated risk (a shared inbox, a throwaway address, a domain that accepts everything, a provider that will not say). | Send with care, or review. |
| **unknown** | We could not determine the answer. | Honest, not broken — see A.3. |

`invalid` is reserved for actual evidence of non-deliverability (bad syntax, no mail host at all, a null MX, a confirmed
no-such-mailbox). **An address we merely could not check is never `invalid`** — blocked, rate-limited, greylisted, timed-out
and "our own DNS failed" all come back `unknown` or `risky`. That protects you from silently discarding good contacts.

### A.2 The reason — the common ones

- **invalid_syntax** — not a well-formed address.
- **invalid_domain / no_mx** — the domain has no mail host: no MX *and* no A/AAAA record (a domain with only an A/AAAA
  record is treated as receiving mail at that host, RFC 5321 §5.1, and is **not** `invalid`), or it publishes a null MX
  (**null_mx**).
- **dns_error** — our DNS lookup failed; the answer is `unknown`, never `invalid`.
- **mailbox_not_found** — the mail service said this mailbox does not exist.
- **mailbox_full** — the mailbox exists but is over quota (a real mailbox, so a `valid` result).
- **role_account** — a shared inbox (`info@`, `sales@`) rather than a person.
- **disposable** — a throwaway address: real today, likely gone tomorrow, so **risky** and never `invalid`.
- **catch_all / accept_all** — the domain accepts mail to every address, so this particular mailbox cannot be confirmed.
- **provider_no_smtp** — a large mailbox provider that does not reveal whether a mailbox exists (A.4); we did not probe it.
- **greylisted · smtp_blocked · smtp_timeout · smtp_error · connection_failed** — the probe could not get an answer
  (`unknown`/`risky`); retry later. Every one of them says **who** failed in `signals.failure_origin` (A.7).
- **unknown_error** — we could not compute this address at all (our node, key or policy failed on it repeatedly), so
  the item was settled as `unknown` rather than left pending forever. Always `signals.failure_origin = "own"` and
  `signals.error_capped = true` (A.7).
- **non_public_mx** — the domain's mail host resolves to a non-public address (loopback, a private range, cloud
  metadata, …). We refuse to connect to such an address, so the result is `risky`, never `invalid`, and is not cached.
- **external_verified · historical_bounce** — provenance values for a rented third-party answer (capped at `risky`) and
  for an address with confirmed earlier bounces. They tell you where an answer came from, not an extra mailbox fact.
- **plausible** — the address looks deliverable and nothing is wrong, but there is no positive proof either way.

### A.3 What `unknown` means — and why it is a good answer

`unknown` means we could not obtain reliable evidence and are saying so instead of guessing. A fake `valid` costs a bounce
(and enough bounces damage your ability to reach anyone); a fake `invalid` costs a real customer you never contact again;
an honest `unknown` costs a moment's judgment and is recoverable — send cautiously, verify again later, or hold. A verifier
that never returns `unknown` is not more capable; it is guessing where this one is honest. Treat `unknown` as "proceed with
caution."

### A.4 Confidence, and the addresses nobody can determine

**Confidence is how strongly the evidence supports the status — not a probability of delivery** (80 does not mean "80%
deliverable"; delivery also depends on the recipient's spam filter, whether they muted you, whether their mailbox fills up
tomorrow). A `risky` at 60 and a `valid` at 60 are not equally good: the status is the headline, confidence refines it.

Some addresses are inherently undeterminable, because of how email works: the largest consumer providers deliberately do
not reveal whether a mailbox exists (we return `risky`, honestly), and catch-all domains accept everything, so "accepted"
says nothing about one mailbox (we return `risky / catch_all`). A system that returns a confident `valid` here is
overstating what it knows.

### A.5 The recommendation

| Recommendation | Meaning |
|---|---|
| **send** | The evidence supports delivery. |
| **review** | Worth a human look, or send with care. |
| **skip** | Don't send — proven undeliverable, or the risk outweighs the benefit. |

It follows the status honestly and is checked in code (`vocab.ComposabilityOK`): `invalid ⇒ skip`; `valid ⇒ send` or
`review`; `risky` and `unknown ⇒ review` or `skip`. You are free to apply your own thresholds.

### A.6 Two worked examples

> **status:** risky · **reason:** catch_all · **confidence:** 45 · **recommendation:** review
> *The domain accepts all mail, so this exact mailbox cannot be confirmed. Probably deliverable, no proof of the specific
> recipient — review before sending, especially for a cold, reputation-sensitive campaign.*

> **status:** valid · **reason:** mailbox_full · **confidence:** 88 · **recommendation:** send
> *The mailbox exists — it is over quota, which only a real mailbox can be. Safe; it may just be temporarily full.*

### A.7 When an answer could not be produced: who failed, and why SMTP did not run

Three additive keys in `signals` (Door 1: the bag is open, every consumer ignores keys it does not know). None of them
changes a status, reason, confidence, recommendation, caching decision or retry; they only say more about a result.

**`signals.failure_origin`** — present on **every `unknown`** result, in the response and in the log row. Exactly three values:

| Value | Means | Typical cause |
|---|---|---|
| `own` | the cause is on our side of the wire | our verifier node unreachable or erroring, our resolver unreachable or timing out, a policy or reputation block against our IP, an item we could not compute |
| `remote` | the recipient's side provably caused it | their server answered with an SMTP reply code (a 4xx greylist, an ambiguous 550), or accepted the connection and then did not answer; their domain's DNS failed behind a working resolver of ours (SERVFAIL) |
| `undetermined` | M1 cannot prove either side | the connection never opened (our egress blocked and their refusal look identical), a session that dropped with no reply, an unclassifiable DNS error |

The default for anything not proven is `undetermined`, never `remote`. A consumer must treat an unrecognised value as
`undetermined`. The complete mapping — every code path that yields an `unknown`, the evidence it rests on, the file and line of
the branch and the test that covers it — and the list of cases that cannot be classified honestly is
[`failure-origin.md`](failure-origin.md).

The key also appears, as `own`, on a non-`unknown` result that is a **degraded cheap-only fallback caused by us**: a `risky`
answer whose `signals.smtp_skipped_reason` is one of `no_eligible_node`, `audit_red`, `warmup_cap`, `rate_limited` or
`smtp_unwired`. It is never present on a normal answer.

**`signals.smtp_skipped_reason`** — present when the SMTP probe did not run for a `risky/plausible` residue. An open, additive
string enum; the tokens today:

| Token | Meaning | Cached? |
|---|---|---|
| `no_eligible_node` | no verifier node is eligible for SMTP (health or routing gap) | no |
| `audit_red` | the node pool is red | no |
| `warmup_cap` | the node is at its warm-up cap | no |
| `rate_limited` | every eligible node is at its per-provider cap | no |
| `smtp_unwired` | the SMTP stage is not wired in this process at all (SMTP disabled, or no node or config could be fetched at boot). Before this token existed such a result was indistinguishable from a genuine cheap-layers answer | no |
| `provider_mx_routed` | a large provider that is deliberately never probed (`risky/provider_no_smtp`): a settled answer, not a failure | yes |

Every token except `provider_mx_routed` marks a result that is **not what a working M1 would have answered**; none of them is
cached, so the address is probed afresh once the cause clears.

**`signals.error_capped`** — `true` only on a result the queue settled because the item could not be computed (a policy,
decryption or compute error) on `TM_QUEUE_ERROR_MAX_ATTEMPTS` attempts (default 5). Such a result is
`unknown` / `unknown_error`, confidence 10, recommendation `review`, `failure_origin = "own"`, and is not cached. It is a
terminal state: the item appears in the job's results and the job can complete, where before such an item was retried forever
and the job never did.

The principle behind all of it: **we tell you what we can prove, and tell you honestly when we cannot prove anything.**
