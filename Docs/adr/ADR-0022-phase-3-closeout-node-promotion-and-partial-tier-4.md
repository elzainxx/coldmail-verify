# ADR-0022 — Phase 3 close-out: a node promotes itself from a green audit, a partial Tier 4 (describe, an idempotent create, job totals), and one syntax rule

- **Status:** ACCEPTED (2026-11-18) — ruled by the owner at the close of the platform's Phase 3 (`coldmail-platform/Docs/phase-3-closeout-spec.md`,
  rulings OD-4, OD-6/OD-8 and OD-9, items A1, A2, A3 and A5).
- **Kind:** additive API and behaviour changes. **No door is reopened**: Door 1 gains response keys and a route only; Door 2 (the LOG) is only appended to;
  Door 3 stores a keyed digest, never an address; Door 4 gains no value of `status` / `reason` / `recommendation`; Door 5 is untouched.
- **Deciders:** owner.
- **Governs:** `control-plane/migrations/0019_idempotent_jobs_and_node_promotion.sql` · `internal/api` (`describe.go`, `job_input.go`, `api.go`, `errors.go`) ·
  `internal/queue/enqueue.go` · `internal/store` (`RecordKeyedJob`, `JobStatusSnapshot`, `RegisterNode`, `SettleProvisioningNode`) ·
  `internal/health` (`Evaluate`, the watcher) · `internal/hash/split.go` and `testdata/norm-vectors.json` (format `/2`).

---

## Context

The platform's Phase 3 drove module 1 as a real caller and found four things the module's own CI could not: (1) the install docs promised that the
first green audit promotes a node `provisioning → active`, but **no running code did it** and every boot reset the node to `provisioning` with
`port25_open=false`, so a person ran SQL after every restart (OD-4); (2) module 1 was the only module that could not describe its vocabularies, a
retried `POST /v1/verify/bulk` created a **second job**, and a job's duplicate and malformed counts existed only in the 202 (OD-6/OD-8); (3) a job id
that is not a UUID reached Postgres and came back a `500`; (4) the platform's own address check and module 1's syntax layer were two rules (OD-9).

## Decision

1. **Node self-promotion (OD-4).** A node is promoted to `active` **only** by a green audit row recorded **after this boot's registration**, on the node's
   configured egress IP (GAP-24), from a node that answers its liveness probe, in **one statement** that takes `port25_open` from that audit and records the
   audit row's id (`nodes.promoted_by_audit_id`). Every boot re-registers (`RegisterNode`: `provisioning`, `port25_open=false`, `registered_at` = the database
   clock); a stale `active` is never trusted. A red audit leaves a **provisioning** node provisioning (it is not quarantined: it was never serving). The
   first audit is the watcher's immediate first cycle; the existing `TM_WATCHER_CADENCE_SECONDS` dial is its cadence (no new dial).
2. **A partial Tier 4 (OD-6/OD-8)**, three additive changes and **no** `quote` / `run` / `report`: `GET /v1/describe` in the shape family of modules 2 and 3
   (vocabularies built from the code's own constants, routes derived from the served table); an **idempotent bulk create** on an optional `Idempotency-Key`
   header, scoped to the authenticated caller, unique per `(caller, key)` in the database (a partial unique index), a repeat returning the original job with
   `Idempotent-Replayed: true`, another request under the key a `409 idempotency_key_reused`; and **`totals`** on the job status, from one consistent read, with
   the listing declared stable once `complete` is true.
3. **A non-UUID job id is `400 validation_failed`** on both routes that take one, before the database is touched (A3).
4. **One syntax rule (OD-9).** The shared vector file is format `/2`: every non-malformed vector carries `syntax_ok` (the real syntax layer over the parts the
   orchestrator's own split returns, `hash.SplitNormalized`), with boundary vectors on both sides. M1-10 (what such an address is charged) is unchanged.

## Consequences

- **A green audit no longer implies a separate egress-25 proof.** The audit checks the public-IP lookup, the A record, the PTR and the resolver, not outbound
  port 25, and `port25_open` now follows the audit. The operator's V1 check, the warm-up cap and the block-rate watcher are the backstops, and a dedicated
  egress test stays `future-work.md` FW-2. The owner ruled that `port25_open` is "taken from the audit"; this is the consequence, stated rather than hidden.
- A keyed job is written in **one** transaction (job, rows, LOG rows and work items), so a repeat never meets a half-made job; an unkeyed job is still two
  steps, byte for byte as before. A replay prepares and discards a full set of sealed rows (cost: one preparation), never writes them.
- The canonical request that identifies a retry is the **normalised** one (case and spacing of an address, an omitted default policy do not matter; an address,
  a label, the order, the `caller_ref` or the owner do). A malformed row is identified by position and label only. Keys never expire (they live with the job row).
- `GET /v1/jobs/{id}` reads job, completion and totals in one statement, which is O(rows of the job) because `by_status` needs each settled row's verdict status.
- A restart leaves the node `provisioning` for the length of its first audit; SMTP work in that window is requeued as the capacity wait `no_eligible_node`.

## Reopens it?

No. Every change is additive (a route, response keys, an error code, nullable columns, a header); the frozen vocabularies and the verdict object are untouched.
