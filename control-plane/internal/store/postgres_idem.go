package store

// postgres_idem.go — three things that are new with migration 0019 and that Postgres does in ONE statement or ONE transaction each:
//
//   - RecordKeyedJob: an idempotent bulk submission (the job, its rows, the LOG rows of its malformed rows and its work items, under a unique
//     (caller, Idempotency-Key)), written all at once or not at all;
//   - JobStatusSnapshot: everything GET /v1/jobs/{id} reports, from one statement;
//   - workItemStatements: the chunked multi-row INSERT of a keyed job's work items.
//
// An UNKEYED submission does not come through here at all: it is still RecordJob followed by one EnqueueWorkItem per item, byte for byte as before.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-verify/control-plane/internal/vocab"
)

const (
	// workItemsChunk is how many work items one INSERT carries: workItemParams parameters per row, so 13,000 per statement, far under Postgres's
	// 65,535-parameter limit. 100,000 items are 100 statements.
	workItemsChunk = 1000
	workItemParams = 13
)

// workItemStatements builds the multi-row INSERTs of a keyed job's work items, workItemsChunk rows per statement, with the SAME columns and the same
// defaults as EnqueueWorkItem (status 'ready', created_at falling back to not_before). Pure: no connection. There is no ON CONFLICT: the keys were
// de-duplicated inside the job by the enqueuer, so a conflict here is a defect and must fail the whole transaction, not be swallowed.
func workItemStatements(items []WorkItemRow) []pgStatement {
	spans := chunkSpans(len(items), workItemsChunk)
	out := make([]pgStatement, 0, len(spans))
	for _, sp := range spans {
		var sb strings.Builder
		sb.WriteString(`INSERT INTO verify.work_items (id, idempotency_key, job_id, domain, email_hash, policy_key, policy_version, config_fingerprint, status, attempt, not_before, addr_ciphertext, created_at) VALUES `)
		args := make([]any, 0, workItemParams*(sp[1]-sp[0]))
		for i, it := range items[sp[0]:sp[1]] {
			if i > 0 {
				sb.WriteByte(',')
			}
			base := 1 + workItemParams*i
			sb.WriteByte('(')
			for j := 0; j < workItemParams; j++ {
				if j > 0 {
					sb.WriteByte(',')
				}
				fmt.Fprintf(&sb, "$%d", base+j)
			}
			sb.WriteByte(')')
			status := it.Status
			if status == "" {
				status = "ready"
			}
			created := it.CreatedAt
			if created.IsZero() {
				created = it.NotBefore
			}
			args = append(args, it.ID, it.IdempotencyKey, nullStr(it.JobID), it.Domain, it.EmailHash, it.PolicyKey, it.PolicyVersion,
				it.ConfigFingerprint, status, it.Attempt, it.NotBefore.UTC(), nullBytes(it.AddrCiphertext), created.UTC())
		}
		out = append(out, pgStatement{sql: sb.String(), args: args})
	}
	return out
}

// insertKeyedJobSQL claims the key and creates the job in one statement. ON CONFLICT names the partial unique index's columns and predicate so
// that ONLY a taken (caller, key) is swallowed (a primary-key collision on id is still an error). When another transaction holds the key
// uncommitted, Postgres makes this statement WAIT for that transaction: if it commits the statement does nothing (no row returned), if it rolls
// back the statement inserts. That wait is the concurrency control: there is no application lock.
const insertKeyedJobSQL = `
INSERT INTO verify.jobs (id, policy_key, policy_version, status, total_count, settled_count, caller_ref, owner_user_id,
                         idem_caller_id, idempotency_key, request_fingerprint)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
ON CONFLICT (idem_caller_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
RETURNING id::text`

// RecordKeyedJob: see Store.RecordKeyedJob.
func (p *PG) RecordKeyedJob(ctx context.Context, rec KeyedJobRecord, malformedListCap int) (KeyedJobOutcome, error) {
	if err := checkKeyedRecord(rec); err != nil {
		return KeyedJobOutcome{}, err
	}
	logStmts, err := verificationStatements(rec.Rejects)
	if err != nil {
		return KeyedJobOutcome{}, err
	}
	rowStmts := jobRowsStatements(rec.Job.ID, rec.Rows)
	itemStmts := workItemStatements(rec.Items)

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return KeyedJobOutcome{}, fmt.Errorf("store: record keyed job begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // after a commit this is a no-op; after a replay or a conflict it discards the nothing that was written

	args := append(jobInsertArgs(rec.Job), rec.Claim.CallerID, rec.Claim.Key, rec.Claim.Fingerprint)
	var inserted string
	err = tx.QueryRow(ctx, insertKeyedJobSQL, args...).Scan(&inserted)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The key is taken by a job that is COMMITTED (a pending holder made the statement wait, above). Select it in the same transaction.
		var existingID string
		var fp []byte
		if qerr := tx.QueryRow(ctx,
			`SELECT id::text, request_fingerprint FROM verify.jobs WHERE idem_caller_id = $1 AND idempotency_key = $2`,
			rec.Claim.CallerID, rec.Claim.Key).Scan(&existingID, &fp); qerr != nil {
			return KeyedJobOutcome{}, fmt.Errorf("store: record keyed job: the key is taken but its job cannot be read: %w", qerr)
		}
		if !bytes.Equal(fp, rec.Claim.Fingerprint) {
			return KeyedJobOutcome{Conflict: true}, nil
		}
		sub, serr := jobSubmissionOf(ctx, tx, existingID, malformedListCap)
		if serr != nil {
			return KeyedJobOutcome{}, serr
		}
		return KeyedJobOutcome{JobID: existingID, Replayed: true, Submission: sub}, nil
	case err != nil:
		return KeyedJobOutcome{}, fmt.Errorf("store: record keyed job: %w", err)
	}

	// The key is ours. Everything else of the submission follows in the SAME transaction: the LOG rows before the job rows that point at them,
	// the work items last. Nothing is visible to the drainer or to a reader (or to a repeat of the key) until the commit.
	for _, s := range logStmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			return KeyedJobOutcome{}, fmt.Errorf("store: record keyed job: log rows: %w", err)
		}
	}
	for _, s := range rowStmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			return KeyedJobOutcome{}, fmt.Errorf("store: record keyed job: rows: %w", err)
		}
	}
	for _, s := range itemStmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			return KeyedJobOutcome{}, fmt.Errorf("store: record keyed job: work items: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return KeyedJobOutcome{}, fmt.Errorf("store: record keyed job commit: %w", err)
	}
	return KeyedJobOutcome{JobID: inserted}, nil
}

// pgQueryer is what jobSubmissionOf needs: a transaction or the pool.
type pgQueryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// jobSubmissionOf reproduces what the 202 of a job said, from its rows (verify.job_rows is append-only: they cannot have changed since).
func jobSubmissionOf(ctx context.Context, q pgQueryer, jobID string, malformedListCap int) (JobSubmission, error) {
	var sub JobSubmission
	if err := q.QueryRow(ctx, `
SELECT count(*)::int, (count(*) FILTER (WHERE malformed))::int, (count(*) FILTER (WHERE duplicate_of_ordinal IS NOT NULL))::int
FROM verify.job_rows WHERE job_id = $1`, jobID).Scan(&sub.Total, &sub.Malformed, &sub.Duplicate); err != nil {
		return JobSubmission{}, fmt.Errorf("store: job submission counts: %w", err)
	}
	sub.Enqueued = sub.Total - sub.Duplicate - sub.Malformed
	sub.MalformedRows = []MalformedRowRef{}
	if malformedListCap > 0 && sub.Malformed > 0 {
		rows, err := q.Query(ctx, `
SELECT ordinal, COALESCE(item_id, '') FROM verify.job_rows WHERE job_id = $1 AND malformed ORDER BY ordinal LIMIT $2`, jobID, malformedListCap)
		if err != nil {
			return JobSubmission{}, fmt.Errorf("store: job submission malformed rows: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var m MalformedRowRef
			if err := rows.Scan(&m.Ordinal, &m.ItemID); err != nil {
				return JobSubmission{}, fmt.Errorf("store: job submission malformed rows scan: %w", err)
			}
			sub.MalformedRows = append(sub.MalformedRows, m)
		}
		if err := rows.Err(); err != nil {
			return JobSubmission{}, fmt.Errorf("store: job submission malformed rows: %w", err)
		}
	}
	sub.MalformedRowsTruncated = sub.Malformed > malformedListCap
	return sub, nil
}

// statusSnapshotSQL reads a job's row, its completion and its totals in ONE statement, so ONE snapshot: Postgres evaluates every subquery of a
// statement against the snapshot taken when the statement began, so a work item the drainer settles meanwhile is either counted everywhere or
// nowhere, and Submitted = Settled + Pending and Settled = sum(by status) hold by construction. The CTE r (the job's rows with their verdict
// status) is referenced more than once, so Postgres materialises it: the rows are read once.
//
// A row is SETTLED exactly when the results listing would serve it (job_rows rows that are malformed, or whose work item is settled|dead and carries
// its verdict): that is what makes the totals equal the listing's rows row by row. The owner predicate is `owner_user_id = $2` on the job, as in
// every other read; a job that is not the caller's yields no row at all.
const statusSnapshotSQL = `
WITH j AS (
  SELECT id::text AS id, policy_key, policy_version, status, total_count, settled_count, submitted_at,
         COALESCE(completed_at, 'epoch'::timestamptz) AS completed_at, COALESCE(caller_ref, '') AS caller_ref,
         COALESCE(owner_user_id::text, '') AS owner
  FROM verify.jobs WHERE id = $1 AND owner_user_id = $2
), r AS (
  SELECT x.malformed, x.duplicate_of_ordinal,
         (x.malformed OR COALESCE(w.status IN ('settled','dead') AND w.verification_id IS NOT NULL, false)) AS settled,
         v.status AS vstatus
  FROM verify.job_rows x
  LEFT JOIN verify.work_items w ON w.id = x.work_item_id
  LEFT JOIN verify.verifications v ON v.id = COALESCE(w.verification_id, x.verification_id)
  WHERE x.job_id = $1 AND EXISTS (SELECT 1 FROM j)
)
SELECT j.id, j.policy_key, j.policy_version, j.status, j.total_count, j.settled_count, j.submitted_at, j.completed_at, j.caller_ref, j.owner,
       NOT EXISTS (SELECT 1 FROM verify.work_items WHERE job_id = $1 AND status NOT IN ('settled','dead')) AS complete,
       (SELECT count(*) FROM r)::int,
       (SELECT count(*) FROM r WHERE settled)::int,
       (SELECT count(*) FROM r WHERE duplicate_of_ordinal IS NOT NULL)::int,
       (SELECT count(*) FROM r WHERE malformed)::int,
       (SELECT count(*) FROM r WHERE settled AND vstatus = $3)::int,
       (SELECT count(*) FROM r WHERE settled AND vstatus = $4)::int,
       (SELECT count(*) FROM r WHERE settled AND vstatus = $5)::int,
       (SELECT count(*) FROM r WHERE settled AND vstatus = $6)::int
FROM j`

// statusOrder is the order of the four verdict statuses in the statement above ($3..$6).
var statusOrder = [4]vocab.Status{vocab.StatusValid, vocab.StatusInvalid, vocab.StatusRisky, vocab.StatusUnknown}

// JobStatusSnapshot: see Store.JobStatusSnapshot.
func (p *PG) JobStatusSnapshot(ctx context.Context, jobID, ownerUserID string) (JobSnapshot, bool, error) {
	var s JobSnapshot
	var completed time.Time
	var byStatus [4]int
	err := p.pool.QueryRow(ctx, statusSnapshotSQL, jobID, nullStr(ownerUserID),
		string(statusOrder[0]), string(statusOrder[1]), string(statusOrder[2]), string(statusOrder[3])).Scan(
		&s.Job.ID, &s.Job.PolicyKey, &s.Job.PolicyVersion, &s.Job.Status, &s.Job.TotalCount, &s.Job.SettledCount, &s.Job.SubmittedAt,
		&completed, &s.Job.CallerRef, &s.Job.OwnerUserID,
		&s.Complete,
		&s.Totals.Submitted, &s.Totals.Settled, &s.Totals.Duplicates, &s.Totals.Malformed,
		&byStatus[0], &byStatus[1], &byStatus[2], &byStatus[3])
	if errors.Is(err, pgx.ErrNoRows) {
		return JobSnapshot{}, false, nil // no such job, or not yours: indistinguishable by design
	}
	if err != nil {
		return JobSnapshot{}, false, fmt.Errorf("store: job status snapshot: %w", err)
	}
	if completed.Unix() > 0 {
		s.Job.CompletedAt = completed
	}
	s.Totals.Pending = s.Totals.Submitted - s.Totals.Settled
	s.Totals.ByStatus = make(map[string]int, len(statusOrder))
	for i, st := range statusOrder {
		s.Totals.ByStatus[string(st)] = byStatus[i]
	}
	return s, true, nil
}

// checkKeyedRecord judges what the database cannot see, shared by the in-memory store and Postgres so neither is laxer than the other: the claim is
// well-formed, and the rows, the LOG rows and the work items agree (every non-malformed row names one of the work items, every work item is named,
// belongs to this job and carries the idempotency key the rows imply is unique).
func checkKeyedRecord(rec KeyedJobRecord) error {
	c := rec.Claim
	if c.Key == "" || len(c.Key) > 128 {
		return fmt.Errorf("store: record keyed job: the idempotency key must be 1..128 characters, got %d", len(c.Key))
	}
	if len(c.Fingerprint) != 32 {
		return fmt.Errorf("store: record keyed job: the request fingerprint must be 32 bytes, got %d", len(c.Fingerprint))
	}
	if err := checkRejectLinks(rec.Rows, rec.Rejects); err != nil {
		return err
	}
	byID := make(map[string]bool, len(rec.Items))
	for _, it := range rec.Items {
		if it.ID == "" {
			return fmt.Errorf("store: record keyed job: a work item has no id")
		}
		if byID[it.ID] {
			return fmt.Errorf("store: record keyed job: work item %s appears twice", it.ID)
		}
		if it.JobID != rec.Job.ID {
			return fmt.Errorf("store: record keyed job: work item %s belongs to job %q, not %q", it.ID, it.JobID, rec.Job.ID)
		}
		byID[it.ID] = false // false = not yet named by a row
	}
	for _, r := range rec.Rows {
		if r.Malformed || r.WorkItemID == "" {
			continue
		}
		if _, ok := byID[r.WorkItemID]; !ok {
			return fmt.Errorf("store: record keyed job: row %d names work item %s, which is not among the work items being written", r.Ordinal, r.WorkItemID)
		}
		byID[r.WorkItemID] = true
	}
	for id, named := range byID {
		if !named {
			return fmt.Errorf("store: record keyed job: work item %s is written for no row of the job", id)
		}
	}
	return nil
}
