# Bulk jobs: row identity, duplicates, result order, and the dedup scope

**Status:** LIVE (2026-11-17). Implements the owner's rulings **A1** (per-item identity on bulk) and **A2** (dedup scope), and the two
follow-up rulings that went with them (a malformed row is logged; a duplicate is visible), which close gaps **M1-G1** and **M1-G2** in the
platform's research record (`coldmail-platform/Docs/history/p2-research/m1-verify-outcomes.txt`). Migration:
[`0018_per_job_dedup.sql`](../control-plane/migrations/0018_per_job_dedup.sql). Code: `control-plane/internal/api/bulk_rows.go`
(request and result shape), `internal/queue/enqueue.go` (the enqueuer), `internal/store` (`RecordJob`, `job_rows`, the listing, the counts).

Everything here is **additive** on the `/v1` surface ([`PROJECT_RULES.md`](../PROJECT_RULES.md) Door 1): the original request
shape, every existing response key and every existing result field are unchanged. Two things changed *meaning* and are called
out in §7 so nobody finds them by accident; both are now owner rulings.

## 1. What a caller can now rely on

| Before | Now |
|---|---|
| A bulk result carried no row identity; results came back in the order of a random work-item id (M1-G1). | Every result carries its row's **`ordinal`** (0-based index in the list you submitted) and, if you gave one, its **`item_id`**. Results come back **in ordinal order**. |
| A malformed address was counted and then vanished. | It keeps its ordinal, is listed in the create response, **is logged** (its own `verifications` row, exactly as `/v1/verify` logs it) **and is a result** served from that row: `invalid` / `invalid_syntax` / `skip`. |
| An address any tenant had ever enqueued produced **no work item and no result** in a later job (M1-G2). | **Nothing is deduplicated across jobs or tenants.** A new job yields a result for every row. |
| Duplicates inside a job were counted, never answered, and could not be told apart; `pending` never reached 0. | Identical addresses **inside one job** are checked once and **every row gets the result**; a repeated row says which row it repeats (**`duplicate_of`**), so the platform can charge the address once; `pending` reaches 0. |

## 2. The request

`POST /v1/verify/bulk` takes **exactly one** of two shapes (plus the unchanged `policy` and `caller_ref`):

```json
{"addresses": ["jane@acmecorp.com", "bob@acmecorp.com"], "caller_ref": "platform-job-42"}
```

```json
{"items": [
   {"item_id": "row-0", "address": "jane@acmecorp.com"},
   {"item_id": "row-1", "address": "bob@acmecorp.com"},
   {"item_id": "row-2", "address": "Jane@AcmeCorp.com"},
   {"item_id": "row-3", "address": "not-an-address"}
 ],
 "caller_ref": "platform-job-42"}
```

The `addresses` shape works exactly as before; its rows simply have no `item_id`. The submission cap
(`TM_QUEUE_BULK_MAX_ADDRESSES`, default 100 000) counts rows of either shape.

**`item_id` rules.** Your own opaque label for a row. In the `items` shape it is **mandatory** on every row (ruling). M1 stores it,
echoes it and never interprets it.

| Rule | Refusal |
|---|---|
| present and not blank (a missing key, `""` and whitespace-only all count as blank) | `items[N].item_id` `required` |
| at most **200 bytes** (bytes, not characters) | `items[N].item_id` `out_of_range` |
| no control characters (Unicode category Cc: NUL, newline, tab, DEL, the C1 range) | `items[N].item_id` `malformed` |
| **unique within the request**, compared exactly (case-sensitive, no trimming) | `items[N].item_id` `duplicate`, naming **every** row that shares the value |

Every refusal is `400 validation_failed`, with `details[]` naming the fields by their position, lowest ordinal first, **at most
100 entries**:

```json
{"error": {"code": "validation_failed", "message": "One or more fields are invalid.",
           "details": [{"field": "items[0].item_id", "issue": "duplicate"},
                       {"field": "items[2].item_id", "issue": "duplicate"}],
           "request_id": "…"}}
```

Giving both `addresses` and `items` is `400` with `{"field":"addresses","issue":"conflict"}` and `{"field":"items","issue":"conflict"}`;
giving neither (or an empty list) is `400` with `required` on the field(s). `conflict` and `duplicate` are the two issue slugs this
feature adds to the fixed vocabulary (`required`, `malformed`, `out_of_range`, `not_found` as before).

**A bad address is never a bad request.** A row whose address does not normalise (norm/v1: not exactly one `@`, a domain IDNA
cannot encode, blank) is a **malformed row**: the job proceeds, the row keeps its ordinal (the caller's own index, malformed rows
counted), and it is reported and logged as below.

**Do not put personal data in `item_id`** (ruling: it is echoed in clear). It is stored next to the job and returned to you; M1's
hashing door (no address is ever stored or returned after settlement) applies to the address, not to a label you chose. Use your own
row id.

## 3. The 202 response

```json
{
  "job_id": "019a…",
  "status": "accepted",
  "counts": {"total": 4, "enqueued": 2, "duplicate": 1, "malformed": 1},
  "malformed_rows": [{"ordinal": 3, "item_id": "row-3"}],
  "malformed_rows_truncated": false,
  "processing_paused": false
}
```

* `counts` are over **input rows** and add up: **`total = enqueued + duplicate + malformed`**.
  `enqueued` is the number of work items created (the first row of each distinct address in the job), `duplicate` is the number of
  rows that shared an **earlier row's** work item **in this job** (the same figure as the number of results carrying `duplicate_of`),
  `malformed` is the number of rows that did not normalise. **A malformed address that appears several times is several malformed
  rows, never a duplicate.**
* `malformed_rows` lists up to **1000** malformed rows, by `ordinal` (and `item_id` when the row had one; the key is absent for the
  `addresses` shape). The list is always present (`[]` when there are none). `malformed_rows_truncated` is `true` when more than 1000
  rows were malformed; **`counts.malformed` is always the exact number**, and every malformed row is in the results regardless.
* The 202 is the only place `duplicate` and the malformed list appear (the status route does not repeat them): keep this response.
* **Door 2: by the time the 202 is returned, every malformed row's verdict is already in the LOG** (§5). The job, its rows and those
  LOG rows are committed in one transaction, before any work item is enqueued and before anything is returned.

## 4. Status: `GET /v1/jobs/{id}`

```json
{"job_id": "019a…", "status": "complete", "complete": true, "caller_ref": "platform-job-42",
 "counts": {"total": 4, "settled": 4, "pending": 0}, "processing_paused": false}
```

`counts` are over **rows**: `settled` is the number of rows that have an outcome (their work item is settled or dead, **or** they
are malformed), `pending = total - settled`. Rows that share a work item settle together, so **`pending` reaches 0 exactly when the
job is complete**, duplicates included. `complete` is still decided from the work items (never from these counts).

## 5. Results: `GET /v1/jobs/{id}/results?cursor=&limit=`

One element per input row that has an outcome, **ordered by `ordinal`**. Each element is the `/v1/verify` verdict object, key for
key (Door 1), plus additive keys:

```json
{
  "api_version": "v1", "status": "valid", "reason": "accepted", "confidence": 90, "recommendation": "send",
  "email": "", "domain": "acmecorp.com", "flags": {"from_cache": false}, "signals": {"smtp_code": "250"},
  "meta": {"request_id": "", "checked_at": "", "latency_ms": 0, "policy": "cold-email", "policy_version": 1,
           "verifier_ip": null, "node_id": null, "engine_version": null, "config_fingerprint": null, "from_cache": false},
  "raw": null,
  "item_id": "row-2",
  "ordinal": 2,
  "duplicate_of": 0
}
```

* **`ordinal`** is always present (0 is a real position). **`item_id`** is present only when the row had one. **`duplicate_of`** is
  present only on a row that repeats an earlier row's address: it is the **ordinal of the FIRST row of the job with that address**
  (never of an intermediate duplicate; `0` is a real value), and it is absent on a first row and on every malformed row. An old
  client that parses the element as a plain verdict keeps working: unknown keys are ignored.
* **Duplicates are visible so they can be charged once.** Rows 0, 2 and 4 of a job with one address repeated are **one check**: the
  verdict part of the three elements is identical, and rows 2 and 4 say `duplicate_of: 0`. The platform charges the address once
  (the first row) and nothing for the rows that name it.
* **`email` is blank on every element, always.** No address is ever returned (Door 3); you map a result to your input by
  `ordinal` / `item_id`.
* **A malformed row is answered by its own LOG row.** At submit, one `verifications` row is appended **per malformed row**, written
  exactly as the sync path writes an un-addressable input (§3.2.7): `status: "invalid"`, `reason: "invalid_syntax"`,
  `recommendation: "skip"`, `confidence` from the policy's `invalid_syntax` baseline (99 by default), `flags: {"from_cache": false}`,
  `signals: {"syntax_valid": false}`, **no `email_hash` and no `hash_scheme`**, no domain. The result element is that row, served
  from the LOG like every other result; it appears in the listing from the moment of submission (it needs no check). Each malformed
  row has its **own** LOG row, even when the same garbage is submitted twice. The platform's charge for it is the platform's rule
  (this module's `invalid_syntax` is positive proof of non-deliverability, not a rented answer).
* **The cursor** is `next_cursor`, a string, opaque to you: pass it back unchanged. It is the position of the **last row returned**,
  and an exhausted list returns `""`. Walk a listing by repeating the request with `next_cursor` until it is `""`. A cursor this
  service did not issue (garbage, or a work-item id from before 0018) is `400 validation_failed` on field `cursor`, `malformed`.
  `limit` is 1..1000, default 100 (anything else falls back to 100).
* **Read the results after `complete` is `true`.** A row appears once its work item is settled. If you page a job that is still
  running, a row that settles later than the position your cursor already passed is not revisited, so the walk can miss it. A
  complete job's listing is immutable; walk it then.

## 6. The dedup scope

* **Within one job:** rows whose normalised address, policy and config are identical share **one work item and one check**; the
  first row creates it, the others point at it and name the first row (`duplicate_of`). Case, surrounding whitespace and the other
  norm/v1 equivalences all collapse (`Jane@AcmeCorp.com`, `jane@acmecorp.com` and ` jane@acmecorp.com ` are one address).
* **Across jobs and across tenants: none.** The idempotency key is unique **per job** (migration 0018). Tenant B submitting an
  address tenant A verified yesterday gets its own work item, its own check and its own result. What can short-circuit the *check*
  is the cache (TTL dial; default unset), exactly as for the sync path: a cache answer is a real answer marked
  **`flags.from_cache: true`** (use the flag, not `meta.from_cache`, which is always `false` in a bulk result), and the platform
  charges it as a cache answer.
* **A job-less queued check** (no `job_id`; nothing in the public API creates one today) keeps its old semantics: deduplicated by
  its key alone, against other job-less checks only.
* **Retrying a POST** creates a new job. `POST /v1/verify/bulk` has no idempotency key (`future-work.md` FW-10); with the old global
  rule a retried POST came back as a job of "duplicates", now it comes back as a second full job.

## 7. Two changes of meaning (owner rulings)

1. **`counts.total` is now every input row, malformed rows included** (it used to be the rows that normalised). A caller that
   needs the old figure computes `total - malformed`.
2. **`counts.settled` includes the malformed rows** (they have an outcome from the start). Otherwise `total` would count rows
   that can never settle and `pending` could not reach 0.

Jobs submitted **before** migration 0018 keep their old `total` (rows that normalised, duplicates included, which can exceed the
work items that exist), so their `pending` can still stay above 0, exactly as before. Their results are served: the migration
back-filled one row per work item (ordinals `0..n-1` in work-item id order, no `item_id`, no `duplicate_of`: the old code never kept
which rows collapsed, so none can be invented).

## 8. What is stored

`verify.job_rows` (migration 0018), one row per input row: `(job_id, ordinal)` primary key; `item_id` (optional, unique per job);
`work_item_id` (the work item that answers the row; rows of one job may share it); `duplicate_of_ordinal` (the ordinal of the first
row with the same address, set on the rows that share its work item); `malformed` with `verification_id` (the row's own LOG row). It
holds **no address and no hash**. CHECKs, all NULL-safe: `ordinal >= 0`; a malformed row has a `verification_id` and no
`work_item_id`, and every other row has a `work_item_id` and no `verification_id`; `duplicate_of_ordinal`, when set, is below the
row's ordinal and the row is not malformed; `item_id` is 1..200 bytes. `verify_app` holds `SELECT, INSERT` only (rows are never
edited; the LOG rows it appends are the same `INSERT` it already holds on `verifications`). `work_items.idempotency_key` is unique per
job (`uq_work_items_job_idem`, partial on `job_id IS NOT NULL`) and, for a job-less check, on its own (`uq_work_items_unjobbed_idem`,
partial on `job_id IS NULL`); the old global constraint `work_items_idempotency_key_key` is gone. See
[`../control-plane/migrations/README.md`](../control-plane/migrations/README.md).

The enqueuer writes **the job, all its rows and the LOG rows of its malformed rows in ONE transaction** (`Store.RecordJob`), **then**
the work items, so a work item cannot settle before the rows that point at it exist, the status counts are exact at every instant,
and a refused batch leaves nothing behind (no job, no row, no LOG row). The writes are chunked multi-row `INSERT`s: 1000 job rows
(6 001 parameters) or 500 LOG rows (14 000 parameters) per statement, so the worst case, 100 000 malformed rows, is 100 + 200
statements, each far under Postgres's 65 535-parameter limit.

## 9. Pagination sweep (A7): does M1 have M2's skipped-row bug?

M2's campaign report built `next_cursor` from the (page size + 1)th row, so each page boundary skipped one item (M2-G5). Checked
in M1 on 2026-11-17: **no.** Both implementations of the results listing (the Postgres store and the in-memory store) fetched or
looked ahead by one row only to learn whether another page exists, then took the cursor from the **last row returned**. The other
`limit + 1` read in the module (`MetricRowsSince`) is a truncation flag with no cursor, and the flywheel's pull cursor is the
index after the last row returned. What M1 does share with M2 is the design hazard in §5: paging a job that is still running can
miss a row that settles behind the cursor; the contract is "read after `complete`".

The listing was rewritten for ordinals in this change, so the property is now pinned: a walk over several page sizes, including page
boundaries **inside** a run of duplicate rows, must return every ordinal **exactly once, in order** (exact set equality), with each
row's `duplicate_of` intact on whichever page it lands. It has paired controls: the same walker over a cursor taken from the look-ahead
row, over an unsorted listing, and over a cursor that repeats a row must each report a violation.

## 10. How it is proven

All run on the CI runner against a real Postgres as well as the in-memory store:

| Property | Tests |
|---|---|
| per-job scope; tenant B gets results; the old global scope fails the same scenario | `TestEnqueueScope_*` (store), `TestSubmitBulkRows_AnotherTenantGetsResultsForAddressesVerifiedYesterday` and `…ControlTheOldGlobalScopeFailsTheSameScenario` (queue), `TestBulkItems_AnotherTenantGetsResultsForAddressesVerifiedYesterday` (api) |
| a duplicate is one check and N results with their own ordinal / item_id | `TestSubmitBulkRows_EveryRowKeepsItsOrdinalAndLabel_AndADuplicateIsCheckedOnce`, `TestBulkItems_EndToEndOverHTTP`, `TestPGBulkItems_EndToEndOverHTTPOnAScratchDatabase` |
| a duplicate names the FIRST row; a repeated malformed address is not a duplicate; the control reads the old route | `TestSubmitBulkRows_ADuplicateNamesTheFirstRow_AndARepeatedMalformedAddressIsNotADuplicate` (queue), `TestBulkItems_ADuplicateSaysWhichRowItSharesAndARepeatedMalformedAddressIsNotADuplicate` (api), `TestListJobResults_OneResultPerRowInOrdinalOrderWithItsIdentity` (store) |
| exactly one LOG row per malformed row, committed with the job; the served result equals the logged verdict; the control is the old no-log behaviour | `TestRecordJob_AMalformedRowHasExactlyOneLogRowAndIsServedFromIt` and `…ControlTheOldNoLogBehaviourFailsTheSameScenario` (store, both stores), `TestBulkItems_AMalformedRowIsLoggedBeforeThe202AndServedFromTheLog` (api, with its control), `TestJobResults_ElementsCarryOrdinalItemIDAndDuplicateOf_AndEveryRowIsServedFromALogRow` (api: no synthesis, a missing LOG row fails the page) |
| the LOG row is the sync path's, field for field | `TestSubmitBulkRows_AMalformedRowIsLoggedLikeTheSyncPathLogsIt` (queue: a real `orchestrator.Verify` beside the bulk row, with a policy-baseline control) |
| a refused batch leaves no job, row or LOG row; the database refuses what the fake refuses | `TestRecordJob_BothStoresRefuseWhatTheConstraintsRefuse`, `TestCheckRejectLinks` |
| the 100k worst case stays chunked, under the parameter limit, every row once | `TestRecordJobStatements_The100kWorstCaseStaysChunked` (with a control: one giant statement is reported) |
| `pending` reaches 0 with duplicates; the old per-work-item counter does not | `TestJobCompletion_PendingReachesZeroWithDuplicates` |
| every ordinal exactly once across pages, with controls | `TestListJobResults_EveryOrdinalExactlyOnceAcrossPages` (store), `TestBulkItems_PaginationFindsEveryOrdinalExactlyOnce` (HTTP) |
| request shapes, item_id rules, the 100-entry cap, the malformed-rows cap | `TestBulkItems_RequestShapesAndItemIDRules`, `TestBulkItems_MalformedRowsAreCappedInTheCreateResponseButCountedExactly` |
| the table's constraints, grants, ownership, backfill, idempotency | `TestPGGrants_JobRowsAreAppendOnlyForTheRuntime`, `TestMigrate_0018*`, `TestRuntimeAsVerifyApp` (the whole `RecordJob` as the real `verify_app` login) |

## 11. Rulings recorded (formerly open points)

* **A malformed row is logged** (Door 2): one `verifications` row per malformed row, the same row the sync path writes, committed
  with the job before the 202. The cost is up to 100 000 inserts on the 202 path in the worst case, in 200 chunked statements inside
  the one transaction; accepted.
* **`counts.total` includes malformed rows, `counts.settled` includes them too, and `item_id` is mandatory in the `items` shape and
  echoed in clear** (§7, §2): accepted as written.
* **Duplicates are visible** (`duplicate_of`), because the platform charges a duplicated address once.
