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
| `POST` | `/v1/verify/bulk` | per-caller key **required** (PN-13) | Submit a bulk job; returns a job handle (§7) |
| `GET` | `/v1/jobs/{id}` | per-caller key **required** (PN-13) | Job progress counts, scoped to the owning caller (D1 tenancy) |
| `GET` | `/v1/jobs/{id}/results` | per-caller key **required** (PN-13) | Settled verdicts, cursor-paginated; each is the `/v1/verify` object |
| `GET` | `/healthz` | none | Liveness: the process is up. Additive `at` timestamp (item 10) |
| `GET` | `/readyz` | none | Readiness with secret PRESENCE only, never values |

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
| `validation_failed` | 400 | A field is missing, malformed, or out of range |
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
