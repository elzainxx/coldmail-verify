package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file: the Phase-7 flywheel pgx methods (§3/§5/§8). It is the REAL implementation the mem fake
// (mem_flywheel.go) mirrors. The two load-bearing pieces proven against actual Postgres
// (postgres_flywheel_test.go): (1) the (message_id,event) UNIQUE dedup no-op (§3.4 idempotency), and
// (2) CommitRecalibration's CAS-on-lease-holder (§5.5/GAP-53 — the calibration writer's exactly-once
// guarantee, the P5 settle-once / P6 spend-once invariant transferring a third time). The exclusion
// discipline (§4.3/§4.4) is enforced in the aggregate SQL — a zero_information/from_cache/override/
// ingest_error row NEVER enters a calibration bucket, at the read.

func (p *PG) GetVerdictForJoin(ctx context.Context, verificationID string) (VerdictJoin, bool, error) {
	// Read the join projection + extract the external provenance (residue_class/vendor) from the
	// signals jsonb when the verdict is a rented (external_verified) one, so the outcome buckets
	// SEPARATELY (§5.2/GAP-54). Override-derived reasons are flagged calibration-excluded (§4.4).
	const q = `
SELECT status, reason, confidence, COALESCE(mx_provider,''), from_cache, created_at,
       email_hash, COALESCE(domain,''),
       COALESCE(signals->'external'->>'residue_class',''),
       COALESCE(signals->'external'->>'vendor','')
FROM verifications WHERE id=$1`
	var vj VerdictJoin
	err := p.pool.QueryRow(ctx, q, verificationID).Scan(
		&vj.Status, &vj.Reason, &vj.Confidence, &vj.MXProvider, &vj.FromCache, &vj.CreatedAt,
		&vj.EmailHash, &vj.Domain, &vj.ResidueClass, &vj.Vendor,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return VerdictJoin{}, false, nil
	}
	if err != nil {
		return VerdictJoin{}, false, fmt.Errorf("store: verdict-for-join: %w", err)
	}
	switch vj.Reason {
	case "whitelisted", "blacklisted_domain", "blacklisted_mx":
		vj.IsOverride = true
	}
	return vj, true, nil
}

func (p *PG) AppendSendOutcome(ctx context.Context, row SendOutcomeRow) error {
	if row.ID == "" {
		return fmt.Errorf("store: send-outcome requires an id")
	}
	rawJSON := row.Raw
	if len(rawJSON) == 0 {
		rawJSON = []byte("{}")
	}
	// §3.4 idempotency: ON CONFLICT on the (message_id, event) partial UNIQUE index (migration 0012)
	// DO NOTHING — a replayed delivery inserts zero rows → ErrOutcomeDuplicate (a no-op, not an error
	// to retry). The classifier annotations are set AT INSERT (not a later UPDATE), so a duplicate can
	// never reclassify an already-recorded outcome.
	const q = `
INSERT INTO send_outcomes (
  id, verification_id, email_hash, domain, mx_provider, message_id, event, sent_at, occurred_at, raw,
  calibration_class, ingest_error, is_external_verified, residue_class, vendor, from_cache, ingested_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
ON CONFLICT (message_id, event) WHERE message_id IS NOT NULL AND event IS NOT NULL DO NOTHING`
	tag, err := p.pool.Exec(ctx, q,
		row.ID, nullStr(row.VerificationID), nullBytes(row.EmailHash), nullStr(row.Domain),
		nullStr(row.MXProvider), nullStr(row.MessageID), nullStr(row.Event),
		nullTime(row.SentAt), nullTime(row.OccurredAt), rawJSON,
		nullStr(row.CalibrationClass), nullStr(row.IngestError), row.IsExternalVerified,
		nullStr(row.ResidueClass), nullStr(row.Vendor), row.FromCache, row.IngestedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("store: append send-outcome: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrOutcomeDuplicate // (message_id,event) already ingested — the §3.4 no-op
	}
	return nil
}

func (p *PG) HistoricalBounceCount(ctx context.Context, emailHash []byte) (int, time.Time, error) {
	// §4.6: count ONLY confirmed invalid_signal outcomes with no ingest_error for this email_hash. A
	// zero-information bounce never reaches this rollup (anti-poisoning). DISTINCT (message_id,event)
	// enforces "independent" (F-17) — the dedup at append already guarantees one row per outcome, so a
	// plain COUNT suffices, but COUNT(DISTINCT ...) is belt-and-suspenders against any pre-dedup rows.
	const q = `
SELECT COUNT(DISTINCT (message_id, event)), COALESCE(MAX(occurred_at), 'epoch'::timestamptz)
FROM send_outcomes
WHERE email_hash=$1 AND ingest_error IS NULL AND calibration_class='invalid_signal'`
	var count int
	var latest time.Time
	if err := p.pool.QueryRow(ctx, q, emailHash).Scan(&count, &latest); err != nil {
		return 0, time.Time{}, fmt.Errorf("store: historical-bounce count: %w", err)
	}
	if latest.Unix() <= 0 {
		latest = time.Time{} // 'epoch' sentinel => no rows → zero time
	}
	return count, latest, nil
}

func (p *PG) BucketAggregatesSince(ctx context.Context, since, now time.Time) ([]BucketAggregate, error) {
	// §5.2/§4.3/§4.4: aggregate CALIBRATABLE outcomes by (verdict.reason × mx_provider [× vendor ×
	// residue_class]) over [since, now]. The WHERE clause IS the exclusion discipline: only
	// invalid_signal/valid_signal classes, no from_cache, no ingest_error (override rows carry an
	// override reason the classifier stamped 'excluded' at ingest, so they're excluded here too).
	// External-verified outcomes group SEPARATELY via COALESCE(vendor,'')/COALESCE(residue_class,'')
	// (GAP-54). The reason dimension is the JOINED VERDICT's reason (authoritative, single source).
	const q = `
SELECT v.reason, COALESCE(o.mx_provider,''), COALESCE(o.vendor,''), COALESCE(o.residue_class,''),
       COUNT(*) AS qualifying_n,
       COUNT(*) FILTER (WHERE o.calibration_class='invalid_signal') AS invalid_n
FROM send_outcomes o
JOIN verifications v ON v.id = o.verification_id
WHERE o.occurred_at >= $1 AND o.occurred_at <= $2
  AND o.ingest_error IS NULL
  AND o.from_cache = false
  AND o.calibration_class IN ('invalid_signal','valid_signal')
GROUP BY v.reason, COALESCE(o.mx_provider,''), COALESCE(o.vendor,''), COALESCE(o.residue_class,'')
ORDER BY v.reason, 2, 3, 4`
	rows, err := p.pool.Query(ctx, q, since.UTC(), now.UTC())
	if err != nil {
		return nil, fmt.Errorf("store: bucket aggregates: %w", err)
	}
	defer rows.Close()
	var out []BucketAggregate
	for rows.Next() {
		var a BucketAggregate
		if err := rows.Scan(&a.Reason, &a.MXProvider, &a.Vendor, &a.ResidueClass, &a.QualifyingN, &a.InvalidN); err != nil {
			return nil, fmt.Errorf("store: scan bucket aggregate: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (p *PG) GetEffectiveCalibration(ctx context.Context, reason, mxProvider, vendor, residueClass string) (CalibrationRow, bool, error) {
	// The verdict-path read: the CURRENT effective row (effective_from set, effective_to NULL). The
	// partial UNIQUE index (uq_calibration_effective) guarantees at most one; ORDER/LIMIT is defensive.
	const q = `
SELECT id, reason, mx_provider, COALESCE(vendor,''), COALESCE(residue_class,''), confidence, sample_n,
       version, mode, approved, effective_from, COALESCE(effective_to, 'epoch'::timestamptz), created_at
FROM calibration
WHERE reason=$1 AND mx_provider=$2 AND COALESCE(vendor,'')=$3 AND COALESCE(residue_class,'')=$4
  AND effective_from IS NOT NULL AND effective_to IS NULL
ORDER BY version DESC LIMIT 1`
	var c CalibrationRow
	var effTo time.Time
	err := p.pool.QueryRow(ctx, q, reason, mxProvider, vendor, residueClass).Scan(
		&c.ID, &c.Reason, &c.MXProvider, &c.Vendor, &c.ResidueClass, &c.Confidence, &c.SampleN,
		&c.Version, &c.Mode, &c.Approved, &c.EffectiveFrom, &effTo, &c.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CalibrationRow{}, false, nil
	}
	if err != nil {
		return CalibrationRow{}, false, fmt.Errorf("store: effective calibration: %w", err)
	}
	return c, true, nil
}

func (p *PG) AppendCalibrationVersion(ctx context.Context, row CalibrationRow) (int, error) {
	if row.ID == "" {
		return 0, fmt.Errorf("store: calibration version requires an id")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: calibration begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	version, err := insertCalibrationVersionTx(ctx, tx, row)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: calibration commit: %w", err)
	}
	return version, nil
}

// insertCalibrationVersionTx assigns version = max(version)+1 for the bucket and inserts the row inside
// an existing tx (§5.4 versioning). A proposal has effective_from NULL (recommend-only, not effective).
func insertCalibrationVersionTx(ctx context.Context, tx pgx.Tx, row CalibrationRow) (int, error) {
	var version int
	const nextV = `
SELECT COALESCE(MAX(version),0)+1 FROM calibration
WHERE reason=$1 AND mx_provider=$2 AND COALESCE(vendor,'')=$3 AND COALESCE(residue_class,'')=$4`
	if err := tx.QueryRow(ctx, nextV, row.Reason, row.MXProvider, row.Vendor, row.ResidueClass).Scan(&version); err != nil {
		return 0, fmt.Errorf("store: calibration next version: %w", err)
	}
	const ins = `
INSERT INTO calibration (
  id, reason, mx_provider, vendor, residue_class, confidence, sample_n, version, mode, approved,
  effective_from, effective_to, created_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`
	if _, err := tx.Exec(ctx, ins,
		row.ID, row.Reason, row.MXProvider, nullStr(row.Vendor), nullStr(row.ResidueClass),
		row.Confidence, row.SampleN, version, row.Mode, row.Approved,
		nullTime(row.EffectiveFrom), nullTime(row.EffectiveTo), tsOrNow(row.CreatedAt),
	); err != nil {
		return 0, fmt.Errorf("store: insert calibration: %w", err)
	}
	return version, nil
}

func (p *PG) ApproveCalibration(ctx context.Context, calibrationID string, at time.Time) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: approve begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Close the prior effective row for the SAME bucket, then stamp this one effective — atomically
	// (§5.4). Idempotent: if the target is already effective or unknown, the updates are no-ops.
	const closePrior = `
UPDATE calibration prior SET effective_to=$2
FROM calibration target
WHERE target.id=$1 AND target.effective_from IS NULL
  AND prior.reason=target.reason AND prior.mx_provider=target.mx_provider
  AND COALESCE(prior.vendor,'')=COALESCE(target.vendor,'')
  AND COALESCE(prior.residue_class,'')=COALESCE(target.residue_class,'')
  AND prior.effective_from IS NOT NULL AND prior.effective_to IS NULL AND prior.id<>target.id`
	if _, err := tx.Exec(ctx, closePrior, calibrationID, at.UTC()); err != nil {
		return fmt.Errorf("store: approve close-prior: %w", err)
	}
	const stamp = `UPDATE calibration SET effective_from=$2, approved=true WHERE id=$1 AND effective_from IS NULL`
	if _, err := tx.Exec(ctx, stamp, calibrationID, at.UTC()); err != nil {
		return fmt.Errorf("store: approve stamp: %w", err)
	}
	return tx.Commit(ctx)
}

func (p *PG) AccuracyByProvider(ctx context.Context, since, now time.Time) ([]AccuracyRow, error) {
	// §8: per (provider [× vendor × residue_class]) over valid-labeled verdicts: coverage denominator
	// (valid_labeled), accuracy denominator (calibratable_valid), numerator (false_valid = a valid-label
	// whose outcome was invalid_signal). Grouped so external-verified populations return separately
	// (§9 weaning compares own vs vendor). A valid-label with no calibratable outcome is in the coverage
	// denominator but NOT the accuracy denominator (§8.2 — the coverage gap the metric must never hide).
	const q = `
WITH valids AS (
  SELECT v.id, COALESCE(v.mx_provider,'') AS mx_provider
  FROM verifications v
  WHERE v.status='valid' AND v.checked_at >= $1 AND v.checked_at <= $2
),
outs AS (
  SELECT o.verification_id,
         COALESCE(o.vendor,'') AS vendor, COALESCE(o.residue_class,'') AS residue_class,
         bool_or(o.ingest_error IS NULL AND o.from_cache=false
                 AND o.calibration_class IN ('invalid_signal','valid_signal')) AS calibratable,
         bool_or(o.calibration_class='invalid_signal' AND o.ingest_error IS NULL) AS false_valid
  FROM send_outcomes o
  WHERE o.occurred_at >= $1 AND o.occurred_at <= $2
  GROUP BY o.verification_id, COALESCE(o.vendor,''), COALESCE(o.residue_class,'')
)
SELECT vd.mx_provider, COALESCE(o.vendor,''), COALESCE(o.residue_class,''),
       COUNT(*) AS valid_labeled,
       COUNT(*) FILTER (WHERE o.calibratable) AS calibratable_valid,
       COUNT(*) FILTER (WHERE o.false_valid) AS false_valid
FROM valids vd
LEFT JOIN outs o ON o.verification_id = vd.id
GROUP BY vd.mx_provider, COALESCE(o.vendor,''), COALESCE(o.residue_class,'')
ORDER BY vd.mx_provider, 2`
	rows, err := p.pool.Query(ctx, q, since.UTC(), now.UTC())
	if err != nil {
		return nil, fmt.Errorf("store: accuracy by provider: %w", err)
	}
	defer rows.Close()
	var out []AccuracyRow
	for rows.Next() {
		var a AccuracyRow
		if err := rows.Scan(&a.MXProvider, &a.Vendor, &a.ResidueClass, &a.ValidLabeled, &a.CalibratableValid, &a.FalseValid); err != nil {
			return nil, fmt.Errorf("store: scan accuracy: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (p *PG) LeaseRecalibration(ctx context.Context, run RecalRun) (string, error) {
	if run.ID == "" {
		return "", fmt.Errorf("store: recalibration lease requires an id")
	}
	const q = `
INSERT INTO recalibration_runs (id, status, leased_by, leased_at, window_from, window_to, buckets_touched, buckets_at_max, brake_held)
VALUES ($1,'leased',$2,$3,$4,$5,0,0,false)`
	if _, err := p.pool.Exec(ctx, q, run.ID, run.LeasedBy, run.LeasedAt.UTC(), run.WindowFrom.UTC(), run.WindowTo.UTC()); err != nil {
		return "", fmt.Errorf("store: lease recalibration: %w", err)
	}
	return run.ID, nil
}

func (p *PG) CommitRecalibration(ctx context.Context, runID, leasedBy string, versions []CalibrationRow, bucketsTouched, bucketsAtMax int, brakeHeld bool, now time.Time) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: recal commit begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// CAS-on-lease-holder FIRST (§5.5/GAP-42-analogue): flip 'leased'→'committed' ONLY if this worker
	// still holds the lease. A reclaimed lease matches ZERO rows → ErrRecalLeaseLost with NOTHING
	// written (the calibration inserts below never run). The CAS, not the lease, is the exactly-once
	// guarantee — settle-once (P5) → spend-once (P6) → recalibrate-once (P7), the invariant's 3rd use.
	const cas = `
UPDATE recalibration_runs SET status='committed', buckets_touched=$3, buckets_at_max=$4, brake_held=$5, committed_at=$6
WHERE id=$1 AND leased_by=$2 AND status='leased'`
	tag, err := tx.Exec(ctx, cas, runID, leasedBy, bucketsTouched, bucketsAtMax, brakeHeld, now.UTC())
	if err != nil {
		return fmt.Errorf("store: recal CAS: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRecalLeaseLost // lease reclaimed → discard ALL bucket writes (exactly-once)
	}
	// Only now (under the held lease) stage the proposed calibration versions.
	for _, v := range versions {
		if _, err := insertCalibrationVersionTx(ctx, tx, v); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (p *PG) RecentRecalRuns(ctx context.Context, limit int) ([]RecalRun, error) {
	if limit <= 0 {
		limit = 20
	}
	const q = `
SELECT id, status, leased_by, leased_at, window_from, window_to, buckets_touched, buckets_at_max, brake_held
FROM recalibration_runs WHERE status='committed'
ORDER BY leased_at DESC LIMIT $1`
	rows, err := p.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("store: recent recal runs: %w", err)
	}
	defer rows.Close()
	var out []RecalRun
	for rows.Next() {
		var r RecalRun
		if err := rows.Scan(&r.ID, &r.Status, &r.LeasedBy, &r.LeasedAt, &r.WindowFrom, &r.WindowTo, &r.BucketsTouched, &r.BucketsAtMax, &r.BrakeHeld); err != nil {
			return nil, fmt.Errorf("store: scan recal run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *PG) GetOutcomeCursor(ctx context.Context, sourceKey string) (string, bool, error) {
	var cursor string
	err := p.pool.QueryRow(ctx, `SELECT cursor FROM outcome_ingest_cursor WHERE source_key=$1`, sourceKey).Scan(&cursor)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: get outcome cursor: %w", err)
	}
	return cursor, true, nil
}

func (p *PG) SetOutcomeCursor(ctx context.Context, sourceKey, cursor string, at time.Time) error {
	const q = `
INSERT INTO outcome_ingest_cursor (source_key, cursor, updated_at) VALUES ($1,$2,$3)
ON CONFLICT (source_key) DO UPDATE SET cursor=EXCLUDED.cursor, updated_at=EXCLUDED.updated_at`
	if _, err := p.pool.Exec(ctx, q, sourceKey, cursor, at.UTC()); err != nil {
		return fmt.Errorf("store: set outcome cursor: %w", err)
	}
	return nil
}

// ── small helpers ──

// nullTime maps a zero time to SQL NULL (no zero-time sentinels — mirrors nullStr/nullBytes).
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

// tsOrNow returns the given time (UTC) or now() when zero — for created_at defaults.
func tsOrNow(t time.Time) any {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t.UTC()
}
