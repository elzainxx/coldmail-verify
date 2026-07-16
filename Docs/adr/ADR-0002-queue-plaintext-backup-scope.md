# ADR-0002 — Queue plaintext must be backup-aware (P5 gate); Door 3 unchanged

- **Status:** ACCEPTED (owner batch sign-off 2026-07-17)
- **Kind:** door-referencing — **affirms** Door 3 and the P5 bounded exception; adds a phase-gate. Does **not** reopen or modify Door 3.
- **Deciders:** owner (cross-phase revision-pass acceptance)
- **Source finding:** cross-phase consistency report §0 finding 3 / §1 **F-7** · [`DECISIONS.md`](../DECISIONS.md) **DR-4** · **PN-3**

## Context

Two frozen promises, each correct in its own phase, leave a gap that is only visible when you hold both at once:

- **P1, Door 3 (§3.3.1, §3.2.1):** *"The raw full email is **never** stored."* Every durable store — the LOG, the cache, `send_outcomes` — holds only `email_hash` (`bytea`, HMAC-SHA256 under the global pepper).
- **P5 (§3.3, OQ-P5-9):** SMTP structurally requires the *plaintext* address at attempt time and cannot be run from a hash. So P5 opened the **single, deliberate, bounded exception**: `work_items` holds the plaintext **only for the life of the item**, purged **inside the settlement transaction** (§5.3 step 4), never by a later sweep. A crash can never strand plaintext on a settled/dead row — the item is either still live (plaintext legitimately present) or terminal (plaintext already gone, atomically).

The in-transaction purge is exactly right for the **live table**. But a **database backup / PITR snapshot** is a point-in-time copy of `work_items` that includes whatever plaintext was live at snapshot time — and **no purge transaction ever runs against a snapshot.** A greylisted item can hold plaintext for up to `max_retry_age` (~6h); a 100k bulk job holds a large plaintext working set. Any snapshot taken in that window preserves those addresses indefinitely, outside the bounded window the P5 exception was designed to guarantee.

Neither phase is wrong on its own. P1 said "no plaintext" about *its* stores. P5 bounded plaintext in *its* live table. The exception was simply designed **backup-unaware** — the seam is exactly where the two promises meet.

## Decision

**Before Phase 5, the queue's plaintext-at-rest column MUST be excluded from, or protected in, backup/PITR snapshots.** Backups/PITR are to be **treated as containing live plaintext** for their retention window.

- The **mechanism is a P5-time decision**, among (at least):
  1. **Snapshot exclusion** — exclude the plaintext column from backups (e.g. separate table/tablespace not in the snapshot set).
  2. **Separate-key at-rest encryption** — encrypt the plaintext column with a key **separate from the pepper** (so a snapshot leak ≠ a pepper leak, and vice-versa), narrowing the effective exposure to key-holders.
  3. **Short snapshot-retention** — bound how long any snapshot containing plaintext is retained, so the backup window can't exceed the intended live window by much.
- The choice, and any residual it leaves, is documented in the P5 data-handling runbook alongside the existing OQ-P5-9 exception and the P2 §6.4 verifier-identity note.

**This pass records the gate, not the mechanism.** No backup tooling or column encryption is built or chosen now — that is P5's call. Recorded in [`pre-takeoff-checklist.md`](../pre-takeoff-checklist.md) §3.7.

## Why Door 3 is NOT reopened

- **P1's "raw email never persisted" stands verbatim.** This ADR changes no P1 text and adds no plaintext store.
- **P5's bounded exception stands verbatim.** The in-transaction purge-on-settle is unchanged and remains correct for the live table.
- This ADR adds **one additive requirement**: the *reasoning* behind the bounded exception (plaintext exists only for work-not-yet-done) must extend to the backup surface, because a snapshot is a second copy the purge never reaches. It is an annotation that the exception must be backup-aware — **not** a change to the hashing scheme, the pepper, the `bytea` key, or what any table stores at steady state.
- The other four doors are untouched.

## Consequences

- **Positive.** The Door-3 exception becomes *genuinely* bounded — bounded in the live table (P5 already) **and** in backups (this gate). The one plaintext-at-rest store no longer has a silent long-lived copy.
- **Cost.** P5 must choose and implement a backup-protection mechanism before shipping the queue. Small, and native to when backups first hold real plaintext.
- **Interaction with pepper custody.** If the chosen mechanism is separate-key encryption, that key becomes a new secret with its own custody story — deliberately **separate from the pepper** so the two failure domains don't collapse into one (ties to F-9/F-19 brain-crown-jewel concentration). Flagged for the P5 runbook.

## Compliance / how it's checked

- **Pre-P5 gate (checklist §3.7):** no P5 queue ships until the backup-plaintext mechanism is chosen and its residual documented.
- **Behavioral test (senior-qa, when P5 is built):** take a backup while a greylisted `work_item` holds plaintext; restore into an isolated environment; assert the plaintext is **absent or unreadable** per the chosen mechanism. Regression = a restored snapshot yields live plaintext addresses.
