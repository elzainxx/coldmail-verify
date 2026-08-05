# ADR-0015 — Rung 4 is enforced: `accept_and_defer` stops the drainer, refuses the sync surface, and discloses the pause additively

- **Status:** ACCEPTED (2026-08-04) — cross-module alignment Pass 2, Tier 0 item 4.
- **Kind:** door-referencing — a **tightening** that makes an existing operational control real. Additive on Door 1 (one new boolean field on two bulk envelopes), zero change to Door 4 vocabulary, zero change to the persisted job-status set. No frozen door is mutated (see the reopen check).
- **Deciders:** owner (alignment-plan ruling, Tier 0 = "wrong today, independent of any alignment goal").
- **Source finding:** cross-module audit finding **D2** → [`alignment-fix-plan.md`](../../../coldmail-platform/alignment-fix-plan.md) item 4; canonical contract §1.11.1, §1.11.5.
- **Governs:** [`../kill-switch-ladder.md`](../kill-switch-ladder.md) Rung 4 · [`../OPERATIONS.md`](../OPERATIONS.md) §3 · `PROJECT_RULES.md` §4 (PAUSED).

---

## Context

`FrontDoorMode` has three values. `serving` and `reject` were consumed. **`accept_and_defer` was
not.** It passed `Validate()` and both readers of `KillSwitch.FrontDoor` (`api.go` sync verify and
bulk submit) compared only against `FrontDoorReject` — so `TM_FRONTDOOR_MODE=accept_and_defer`
loaded cleanly, printed itself in the boot line and `/readyz`, and then behaved **exactly like
`serving`**. Full verdict production, no error, no warning.

That is worse than the value not existing. The ladder documents Rung 4 as the response to a
*data-integrity emergency* — suspected calibration poisoning, a pepper-custody incident, an
append-only-LOG tamper suspicion. An operator reaching for the middle rung in that situation
believed they had stopped producing verdicts while the system kept writing them into the frozen
append-only LOG. The echo in `/readyz` made it worse, not better: it was evidence of the config
being *read*, which reads as evidence of it being *obeyed*.

The fix plan allowed either direction — implement the behaviour (MEDIUM) or delete the value from
the accepted set so it fails at load (LOW). Two things decided it for implementation:

1. **The rung already has a written definition**, and it is unambiguous for the async path:
   "the front door accepts submissions and returns a job handle, but the drainer is stopped — work
   sits durably in `work_items`, nothing is processed. Restart resumes with zero loss."
2. **The soft rung is the one an operator actually wants.** The common Rung-4 trigger is "the queue
   and DB are healthy but processing must halt." Deleting the value would leave only `reject`, which
   makes every paused minute a minute of the platform's own submissions being refused, pushing the
   retry problem upstream into a caller that has no durable queue of its own.

The definition was silent on three points that had to be decided rather than inferred. This ADR is
the record of those three.

## Decision

**Implement the rung, driven by one dial, with the ambiguities resolved explicitly.**

### 1. One dial moves both halves

`TM_FRONTDOOR_MODE` governs the front door **and** the drainer. Three predicates on `KillSwitch`
(`config.go`) name the states, and the wiring in `main.go` hands the drainer
`Paused: func() bool { return cfg.KillSwitch.VerdictProductionPaused() }`:

| predicate | true at | used by |
|---|---|---|
| `FrontDoorRejecting()` | `reject` | bulk submit — the only surface where the sub-modes differ |
| `FrontDoorDeferring()` | `accept_and_defer` | the design record; `Summary()` |
| `VerdictProductionPaused()` | either | sync verify, the drainer gate, the disclosure flag |

The rung cannot be half-pulled. The earlier shape — separate "stop the drainer" and "front-door
mode" levers, as OPERATIONS.md described it — is exactly how one half stays running.

### 2. The SYNC endpoint refuses at BOTH sub-modes

`POST /v1/verify` returns `503` with `error.code = "paused"` under `accept_and_defer`, identical to
`reject`. There is no deferred form of a synchronous answer: no handle to return, no queue row to
defer into. The two alternatives each break a door:

> **AMENDED 2026-08-05 (fix-plan item 18).** This section read `503 {"error":"service paused"}`,
> **byte-identical** to `reject`. The body is now the contract's coded envelope
> (`{"error":{code,message,request_id}}`, §1.3), which carries a **per-request `request_id`** — so two
> refusals can no longer be byte-identical, and the word is wrong rather than the claim. The property
> this ADR asserts is unchanged and still enforced: a caller **cannot tell which sub-mode is pulled**.
> `TestFrontDoorDefer_SyncVerifyRefusesLikeReject` now compares the two envelopes with `request_id`
> blanked, and additionally requires both ids to be present and to DIFFER — so the loosening cannot
> be satisfied by a build that emits no id, or one constant id, for every request. Amended in place
> per the ADR-0008 precedent (ADRs are immutable in *number and identity*; a claim the code has since
> outgrown is corrected with a dated note rather than left standing as a false statement).

- serve the verdict → produce a verdict while verdict production is disabled (defeats the rung);
- return `200` without one → a fabricated verdict (Doors 1 and 4 forbid it outright).

So **accept-and-defer is a *bulk* capability.** A caller cannot tell which sub-mode is pulled from
the sync surface, which is correct — the rung's sub-mode is operational detail, not a client concern.

### 3. The pause is checked BEFORE the lease

The gate sits at the top of `tick()`, ahead of both `ReclaimExpiredLeases` and `DequeueReady`. A
gate placed after the dequeue would lease a batch and then abandon it: rows left `in_flight` behind
a lease that a paused drainer never settles and a paused reclaim never sweeps. That is a pause that
**damages** the queue instead of freezing it — the opposite of "resumes with zero loss."

Freezing the reclaim sweep along with the rest is safe *because* of that ordering: a paused drainer
takes no new lease, so the only `in_flight` rows during a pause belong to a pre-pause attempt, and
re-readying work that nothing may process is churn. The sweep catches them on the first tick after
release.

### 4. The pause is disclosed as an ADDITIVE BOOLEAN, derived per response

The 202 submit envelope and the `GET /v1/jobs/{id}` envelope each gained
`"processing_paused": <bool>`. Two sub-decisions:

- **Not a fifth `status` value.** The job-status vocabulary is persisted under a DB CHECK
  (`0009_work_items_jobs.sql`: `submitted|running|complete|complete_with_unknowns`). Minting a
  permanent vocabulary value to express a transient operational state would spend a one-way door on
  a flag that can flip back in a minute.
- **Derived on every response, never stored.** A column written at submit time would still read
  "paused" long after the rung was released — and a hardcoded literal would be the same inert-control
  bug this item exists to remove, in the other direction.

### 5. `nil` Paused means never paused

`NewDrainer` defaults `Deps.Paused` to `func() bool { return false }`. Rungs 0–3 are the
overwhelmingly common case and every pre-existing caller passes nothing. A nil defaulting the other
way, or panicking, would silently stop the queue.

### 6. Operator visibility names the rung, not the mode string

`Summary()` — which feeds both the boot log and `/readyz` — emits
`frontdoor=paused-accept-and-defer(Rung4)` / `paused-reject(Rung4)`. D2's real damage was that
everything looked normal while the code ignored the config, so echoing the raw mode string proved
nothing. The drainer additionally logs the PAUSED and RESUMED **transitions** (not every idle poll),
and `main.go` logs at boot if the process starts already paused.

## Why no frozen door is reopened

- **Door 1 (response shape) — additive only.** One new boolean on two *bulk* envelopes (the P5
  sibling surface, itself additive under ADR-0011). No field is removed, renamed, retyped, or
  repurposed; the `/v1/verify` verdict object is untouched; `/v1` is unchanged. A client that ignores
  the field behaves exactly as before.
- **Door 2 (append-only LOG) — strengthened.** The change only ever causes **fewer** LOG rows to be
  written (zero, while paused). No row is rewritten, no grant changes, no schema change, no
  migration.
- **Door 3 (hashing / no plaintext) — untouched, and the bounded exception is respected.** Deferred
  work keeps its `addr_ciphertext` sealed exactly as ADR-0011 specified; the pause changes *when*
  it is purged (at settlement, after release) and never *whether*. Nothing is decrypted while paused
  — the whole point is that nothing runs.
- **Door 4 (vocabulary) — zero new values.** No status, reason, or recommendation value is added;
  §4.2 above is precisely the decision *not* to add one.
- **Door 5 (never-fork) — untouched.** The gem is not involved; a paused drainer simply does not
  call it.

## Consequences

- **Positive.** The middle rung of the pause control does what its own documentation says. An
  operator pulling it during a data-integrity incident gets a genuinely halted system that keeps
  accepting durable work, and the caller polling a handle is *told* why progress is zero rather than
  diagnosing a stall. The structural test (below) makes the class of bug — an accepted config value
  with no behaviour — fail on the day a fourth mode is added.
- **Cost / limits.**
  - **Config is read at boot; there is no reload.** Pulling or releasing Rung 4 requires a restart.
    That is pre-existing (no SIGHUP path exists in M1) and is now stated in OPERATIONS.md rather
    than left for an operator to discover mid-incident. A hot-reloadable kill switch is future work.
  - **A long soft pause accumulates rows.** Backlog is bounded only by `work_items` growth and the
    §7.5 per-submission cap. OPERATIONS.md says to watch the `ready` count and drop to `reject` if
    the queue grows faster than it will be drained.
  - **`accept_and_defer` does nothing for sync callers**, by decision §2. An operator who needs the
    sync surface to keep answering must not be at Rung 4 at all.
- **Obligation on future front-door modes.** Any new `FrontDoorMode` value must be given a distinct
  behaviour triple in the same change that adds it to the accepted set — enforced by
  `TestFrontDoorModes_EveryAcceptedValueHasBehaviour`, which fails if two accepted modes are
  behaviourally identical.

## Compliance / how it's checked

Eight tests across three packages, each paired with an anti-vacuity control, because "nothing
happened" is the *expected* observation at a pause and is also what a broken fixture produces —
which is precisely how D2 hid.

- `internal/queue/drainer_pause_test.go`
  - `TestDrainer_PausedProcessesNothingThenResumesWithZeroLoss` — P1 nothing runs (zero compute
    calls, zero LOG rows), P2 nothing is leased (`ready`, empty `locked_by`), P3 nothing is lost
    (attempt, `not_before`, ciphertext all intact), P4 release drains the accumulated item. Three
    ticks while paused, so a first-tick-only latch would not pass.
  - `TestDrainer_PauseDoesNotReclaimLeases` — a stale lease staged through the real `DequeueReady`
    survives the pause and is reclaimed after release.
  - `TestDrainer_NilPausedNeverPauses` — the §5 default.
- `internal/api/frontdoor_defer_test.go`
  - `TestFrontDoorDefer_SyncVerifyRefusesLikeReject` — 503 at defer, byte-identical to reject, and
    NOT 503 at serving.
  - `TestFrontDoorDefer_BulkStillAcceptsAndDisclosesPause` — 202 + handle + `processing_paused:true`,
    two durable `ready` rows with ciphertext, status readable and disclosing, `settled: 0`; plus the
    same submission refused at `reject` and accepted with `processing_paused:false` at `serving`.
  - `TestFrontDoorDefer_JobStatusVocabularyUnchanged` — the returned `status` stays inside the 0009
    CHECK set (decision §4.1).
  - `TestFrontDoorDefer_SummaryNamesTheRung` — `Summary()` and `/readyz` name the pulled rung; the
    serving control asserts no "Rung4" claim.
- `internal/config/config_test.go`
  - `TestFrontDoorModes_EveryAcceptedValueHasBehaviour` — every accepted mode yields a **distinct**
    `(rejecting, deferring, paused)` triple, so an accepted-but-inert value fails; a misspelled
    `"accept-and-defer"` is rejected at load and does not read as paused.

**Fail-verified 2026-08-04** — seven mutations, each caught by the named test, each reverted with no
residue: (1) sync gate back to `== FrontDoorReject` → the D2 defect, caught; (2) bulk gate widened to
`VerdictProductionPaused` → defer collapses into reject, caught; (3) `processing_paused` hardcoded
`false` → disclosure fails; (4) hardcoded `true` → the serving control fails; (5) the `tick()` gate
deleted → the paused drainer settles the item, caught; (6) the gate moved after `DequeueReady` → the
item comes back `in_flight`, caught (this is the stranding bug §3 exists to prevent); (7) nil
`Paused` defaulting to `true` → three unrelated drainer tests fail, catching a default that would
silently stop the queue.
