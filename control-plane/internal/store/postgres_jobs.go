package store

// postgres_jobs.go — RecordJob on Postgres (migration 0018): the job, its input rows (verify.job_rows) and the LOG rows of its malformed
// rows (verify.verifications) in ONE transaction, built as a bounded number of multi-row INSERTs. The statement builders are pure
// functions (no connection) so that "a 100,000-row submission stays chunked" is a unit test of the code that runs, not a claim.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// jobRowsChunk is how many job_rows one INSERT carries: jobRowParams parameters per row plus the shared job id, so 6,001 parameters
	// per statement, far under Postgres's 65,535-parameter limit. 100,000 rows are 100 statements.
	jobRowsChunk = 1000
	jobRowParams = 6

	// verificationsChunk is how many LOG rows one INSERT carries: verificationParams parameters per row, so 14,000 per statement.
	// 100,000 malformed rows (the worst case: a submission of nothing but garbage) are 200 statements.
	verificationsChunk  = 500
	verificationParams  = 28
	maxStatementParams  = 65535
	verificationColumns = `
  id, request_id, checked_at, created_at,
  email_hash, hash_scheme, domain, provider_canonical_hash, local_part_len,
  status, reason, confidence, recommendation, flags, signals, mx_provider,
  policy_key, policy_version, api_version,
  verifier_ip, node_id, engine_version, engine_core_version, config_fingerprint,
  engine_validation_type, engine_success, from_cache, latency_ms`
)

// pgStatement is one parameterised statement of a bulk write.
type pgStatement struct {
	sql  string
	args []any
}

// verificationArgs is the ONE conversion of a VerificationRow to the 28 parameters of an INSERT into verify.verifications, in the order
// of verificationColumns. The single-row insert (the settlement CAS, the sync path) and the bulk insert (RecordJob) both use it, so a
// malformed row's LOG row is written with exactly the columns, NULL mapping and JSON encoding every other LOG row gets.
func verificationArgs(row VerificationRow) ([]any, error) {
	flagsJSON, err := json.Marshal(orEmptyFlags(row.Flags))
	if err != nil {
		return nil, fmt.Errorf("store: marshal flags: %w", err)
	}
	signalsJSON, err := json.Marshal(orEmptySignals(row.Signals))
	if err != nil {
		return nil, fmt.Errorf("store: marshal signals: %w", err)
	}
	var engineSuccess any
	if row.EngineSuccessSet {
		engineSuccess = row.EngineSuccess
	}
	return []any{
		row.ID, row.RequestID, row.CheckedAt.UTC(), row.CreatedAt.UTC(),
		nullBytes(row.EmailHash), nullStr(row.HashScheme), nullStr(row.Domain), nullBytes(row.ProviderCanonicalHash), nullInt(row.LocalPartLen),
		row.Status, row.Reason, row.Confidence, row.Recommendation, flagsJSON, signalsJSON, nullStr(row.MXProvider),
		row.PolicyKey, row.PolicyVersion, row.APIVersion,
		nullStr(row.VerifierIP), nullStr(row.NodeID), nullStr(row.EngineVersion), nullStr(row.EngineCoreVersion), row.ConfigFingerprint,
		nullStr(row.EngineValidationType), engineSuccess, row.FromCache, nullInt(row.LatencyMS),
	}, nil
}

// nullIntPtr maps a nil *int to SQL NULL and a set one to its value (ordinal 0 is a real value, not NULL).
func nullIntPtr(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// jobRowsStatements builds the multi-row INSERTs of a job's rows, jobRowsChunk rows per statement. Pure: no connection.
func jobRowsStatements(jobID string, rows []JobRowEntry) []pgStatement {
	spans := chunkSpans(len(rows), jobRowsChunk)
	out := make([]pgStatement, 0, len(spans))
	for _, sp := range spans {
		var sb strings.Builder
		sb.WriteString(`INSERT INTO verify.job_rows (job_id, ordinal, item_id, work_item_id, malformed, verification_id, duplicate_of_ordinal) VALUES `)
		args := make([]any, 0, 1+jobRowParams*(sp[1]-sp[0]))
		args = append(args, jobID) // $1, shared by every row of the statement
		for i, r := range rows[sp[0]:sp[1]] {
			if i > 0 {
				sb.WriteByte(',')
			}
			base := 2 + jobRowParams*i
			fmt.Fprintf(&sb, "($1,$%d,$%d,$%d,$%d,$%d,$%d)", base, base+1, base+2, base+3, base+4, base+5)
			args = append(args, r.Ordinal, nullStr(r.ItemID), nullStr(r.WorkItemID), r.Malformed, nullStr(r.VerificationID), nullIntPtr(r.DuplicateOf))
		}
		out = append(out, pgStatement{sql: sb.String(), args: args})
	}
	return out
}

// verificationStatements builds the multi-row INSERTs of LOG rows, verificationsChunk rows per statement. Pure: no connection.
func verificationStatements(rows []VerificationRow) ([]pgStatement, error) {
	spans := chunkSpans(len(rows), verificationsChunk)
	out := make([]pgStatement, 0, len(spans))
	for _, sp := range spans {
		var sb strings.Builder
		sb.WriteString(`INSERT INTO verify.verifications (` + verificationColumns + "\n) VALUES ")
		args := make([]any, 0, verificationParams*(sp[1]-sp[0]))
		for i, r := range rows[sp[0]:sp[1]] {
			a, err := verificationArgs(r)
			if err != nil {
				return nil, err
			}
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteByte('(')
			for j := 0; j < verificationParams; j++ {
				if j > 0 {
					sb.WriteByte(',')
				}
				fmt.Fprintf(&sb, "$%d", len(args)+j+1)
			}
			sb.WriteByte(')')
			args = append(args, a...)
		}
		out = append(out, pgStatement{sql: sb.String(), args: args})
	}
	return out, nil
}

// RecordJob creates the job, appends the LOG rows of its malformed rows and records its input rows in ONE transaction: all of it commits,
// or none of it does (a refused batch leaves no job, no row and no LOG row behind). The LOG rows go in BEFORE the job rows that point
// at them, so no row ever names a verdict that is not yet logged; and nothing is returned to the caller until the commit, which is Door
// 2's "log before returning". See Store.RecordJob for the contract.
func (p *PG) RecordJob(ctx context.Context, job JobRow, rows []JobRowEntry, rejects []VerificationRow) error {
	if err := checkRejectLinks(rows, rejects); err != nil {
		return err
	}
	logStmts, err := verificationStatements(rejects)
	if err != nil {
		return err
	}
	rowStmts := jobRowsStatements(job.ID, rows)

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: record job begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op
	if _, err := tx.Exec(ctx, insertJobSQL, jobInsertArgs(job)...); err != nil {
		return fmt.Errorf("store: record job: %w", err)
	}
	for _, s := range logStmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			return fmt.Errorf("store: record job: log rows: %w", err)
		}
	}
	for _, s := range rowStmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			return fmt.Errorf("store: record job: rows: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: record job commit: %w", err)
	}
	return nil
}
