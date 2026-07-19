package store

import (
	"context"
	"sort"
	"time"
)

// This file: the Phase-7 flywheel MemStore methods (§3/§5/§8) — split out of mem.go to keep the P7
// surface readable. It mirrors the pgx store's SEMANTICS (postgres_flywheel.go): the (message_id,event)
// dedup no-op, the exclusion discipline enforced at the aggregate read (§4.3/§4.4), the versioned
// calibration store with effective-dating, the recalibration CAS-on-lease-holder (§5.5/GAP-53), and the
// address-level invalid-signal rollup (§4.6). It is NOT a Postgres emulator — the real grants + the
// real CAS are proven against actual Postgres (postgres_flywheel_test.go); this fake lets the flywheel/
// orchestrator tests run without a DB.

func (m *MemStore) GetVerdictForJoin(_ context.Context, verificationID string) (VerdictJoin, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// The LOG is append-only; the latest row with this id is the verdict (ids are unique — UUIDv7).
	for i := len(m.Log) - 1; i >= 0; i-- {
		r := m.Log[i]
		if r.ID != verificationID {
			continue
		}
		vj := VerdictJoin{
			Status: r.Status, Reason: r.Reason, Confidence: r.Confidence, MXProvider: r.MXProvider,
			FromCache: r.FromCache, CreatedAt: r.CreatedAt, EmailHash: r.EmailHash, Domain: r.Domain,
		}
		// Override-derived verdicts (whitelist/blacklist) are calibration-excluded (§4.4/OQ-19).
		if r.Reason == "whitelisted" || r.Reason == "blacklisted_domain" || r.Reason == "blacklisted_mx" {
			vj.IsOverride = true
		}
		// external_verified: pull the residue_class + vendor from signals.external.* so the outcome
		// buckets SEPARATELY (§5.2/GAP-54). Mirrors postgres_flywheel.go's jsonb extraction.
		if r.Reason == "external_verified" {
			if ext, ok := r.Signals["external"].(map[string]any); ok {
				if rc, ok := ext["residue_class"].(string); ok {
					vj.ResidueClass = rc
				}
				if vn, ok := ext["vendor"].(string); ok {
					vj.Vendor = vn
				}
			}
		}
		return vj, true, nil
	}
	return VerdictJoin{}, false, nil
}

func (m *MemStore) AppendSendOutcome(_ context.Context, row SendOutcomeRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// §3.4 idempotency: (message_id, event) dedup — a replayed delivery is a NO-OP (ErrOutcomeDuplicate).
	if row.MessageID != "" && row.Event != "" {
		k := row.MessageID + "\x00" + row.Event
		if _, dup := m.outcomeDedup[k]; dup {
			return ErrOutcomeDuplicate
		}
		m.outcomeDedup[k] = struct{}{}
	}
	m.SendOutcomes = append(m.SendOutcomes, row) // APPEND ONLY
	return nil
}

func (m *MemStore) HistoricalBounceCount(_ context.Context, emailHash []byte) (int, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// §4.6: count ONLY confirmed invalid_signal outcomes with no ingest_error for this email_hash. A
	// zero-information bounce never reaches this rollup (anti-poisoning). "Independent" = distinct
	// (message_id,event) — the dedup at append already guarantees one row per distinct outcome.
	count := 0
	var latest time.Time
	for _, o := range m.SendOutcomes {
		if o.IngestError != "" || o.CalibrationClass != string(classInvalidSignalMem) {
			continue
		}
		if !bytesEqual(o.EmailHash, emailHash) {
			continue
		}
		count++
		if o.OccurredAt.After(latest) {
			latest = o.OccurredAt
		}
	}
	return count, latest, nil
}

func (m *MemStore) BucketAggregatesSince(_ context.Context, since, now time.Time) ([]BucketAggregate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// §5.2/§4.3/§4.4: aggregate CALIBRATABLE outcomes by bucket over [since, now]. Exclusion discipline
	// enforced HERE (the read): zero_information / excluded / ingest_error / from_cache rows never enter
	// a bucket. External-verified rows bucket SEPARATELY (Vendor/ResidueClass set) — GAP-54. The bucket
	// reason is the JOINED VERDICT's reason (authoritative, single source — PG uses a JOIN, mem looks it
	// up from the LOG); the outcome's mx_provider was backfilled from the same verdict at ingest.
	type key struct{ reason, prov, vendor, residue string }
	agg := map[key]*BucketAggregate{}
	for _, o := range m.SendOutcomes {
		if !inWindow(o.OccurredAt, since, now) {
			continue
		}
		if !calibratable(o) {
			continue
		}
		k := key{reason: m.verdictReason(o.VerificationID), prov: o.MXProvider, vendor: o.Vendor, residue: o.ResidueClass}
		a := agg[k]
		if a == nil {
			a = &BucketAggregate{Reason: k.reason, MXProvider: k.prov, Vendor: k.vendor, ResidueClass: k.residue}
			agg[k] = a
		}
		a.QualifyingN++
		if o.CalibrationClass == string(classInvalidSignalMem) {
			a.InvalidN++
		}
	}
	out := make([]BucketAggregate, 0, len(agg))
	for _, a := range agg {
		out = append(out, *a)
	}
	// Deterministic order (tests + reproducibility).
	sort.Slice(out, func(i, j int) bool {
		if out[i].Reason != out[j].Reason {
			return out[i].Reason < out[j].Reason
		}
		if out[i].MXProvider != out[j].MXProvider {
			return out[i].MXProvider < out[j].MXProvider
		}
		if out[i].Vendor != out[j].Vendor {
			return out[i].Vendor < out[j].Vendor
		}
		return out[i].ResidueClass < out[j].ResidueClass
	})
	return out, nil
}

func (m *MemStore) GetEffectiveCalibration(_ context.Context, reason, mxProvider, vendor, residueClass string) (CalibrationRow, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// The verdict-path read: the CURRENT effective row (EffectiveFrom set, EffectiveTo zero). Latest wins.
	var best CalibrationRow
	found := false
	for _, c := range m.Calibration {
		if c.Reason != reason || c.MXProvider != mxProvider || c.Vendor != vendor || c.ResidueClass != residueClass {
			continue
		}
		if c.EffectiveFrom.IsZero() || !c.EffectiveTo.IsZero() {
			continue // a proposal, or a superseded version — not effective (§5.4)
		}
		if !found || c.Version > best.Version {
			best, found = c, true
		}
	}
	return best, found, nil
}

func (m *MemStore) AppendCalibrationVersion(_ context.Context, row CalibrationRow) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Version = max(version)+1 for the bucket (§5.4 versioning).
	v := 0
	for _, c := range m.Calibration {
		if c.Reason == row.Reason && c.MXProvider == row.MXProvider && c.Vendor == row.Vendor && c.ResidueClass == row.ResidueClass {
			if c.Version > v {
				v = c.Version
			}
		}
	}
	row.Version = v + 1
	m.Calibration = append(m.Calibration, row)
	return row.Version, nil
}

func (m *MemStore) ApproveCalibration(_ context.Context, calibrationID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Find the target; stamp its EffectiveFrom, and close the prior effective row for the SAME bucket.
	var target *CalibrationRow
	for i := range m.Calibration {
		if m.Calibration[i].ID == calibrationID {
			target = &m.Calibration[i]
			break
		}
	}
	if target == nil {
		return nil // idempotent: unknown id is a no-op (mirrors PG's 0-row update)
	}
	if !target.EffectiveFrom.IsZero() {
		return nil // already effective — idempotent
	}
	for i := range m.Calibration {
		c := &m.Calibration[i]
		if c.ID == calibrationID {
			continue
		}
		if c.Reason == target.Reason && c.MXProvider == target.MXProvider && c.Vendor == target.Vendor &&
			c.ResidueClass == target.ResidueClass && !c.EffectiveFrom.IsZero() && c.EffectiveTo.IsZero() {
			c.EffectiveTo = at // supersede the prior effective version (§5.4)
		}
	}
	target.EffectiveFrom = at
	target.Approved = true
	return nil
}

func (m *MemStore) AccuracyByProvider(_ context.Context, since, now time.Time) ([]AccuracyRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// §8: for VALID-labeled verdicts, coverage (ValidLabeled), the accuracy denominator
	// (CalibratableValid), and the numerator (FalseValid = invalid-signal). Grouped by provider, with
	// external-verified populations returned separately (Vendor set) for §9 weaning comparison.
	type key struct{ prov, vendor, residue string }
	// Index outcomes by verification_id so we can find "did this valid-label get a calibratable outcome".
	byVerif := map[string][]SendOutcomeRow{}
	for _, o := range m.SendOutcomes {
		if o.VerificationID == "" || o.IngestError != "" {
			continue
		}
		if !inWindow(o.OccurredAt, since, now) {
			continue
		}
		byVerif[o.VerificationID] = append(byVerif[o.VerificationID], o)
	}
	agg := map[key]*AccuracyRow{}
	for _, r := range m.Log {
		if r.Status != "valid" {
			continue // §8.1: accuracy is measured on valid-labels (the falsifiable claims, §8.4)
		}
		outs := byVerif[r.ID]
		var vendor, residue string
		if len(outs) > 0 {
			vendor, residue = outs[0].Vendor, outs[0].ResidueClass
		}
		k := key{prov: r.MXProvider, vendor: vendor, residue: residue}
		a := agg[k]
		if a == nil {
			a = &AccuracyRow{MXProvider: k.prov, Vendor: k.vendor, ResidueClass: k.residue}
			agg[k] = a
		}
		a.ValidLabeled++
		calibratableSeen, falseValid := false, false
		for _, o := range outs {
			if calibratable(o) {
				calibratableSeen = true
			}
			if o.CalibrationClass == string(classInvalidSignalMem) {
				falseValid = true
			}
		}
		if calibratableSeen {
			a.CalibratableValid++
		}
		if falseValid {
			a.FalseValid++
		}
	}
	out := make([]AccuracyRow, 0, len(agg))
	for _, a := range agg {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MXProvider != out[j].MXProvider {
			return out[i].MXProvider < out[j].MXProvider
		}
		return out[i].Vendor < out[j].Vendor
	})
	return out, nil
}

func (m *MemStore) LeaseRecalibration(_ context.Context, run RecalRun) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run.Status = "leased"
	m.RecalRuns = append(m.RecalRuns, run)
	return run.ID, nil
}

func (m *MemStore) CommitRecalibration(_ context.Context, runID, leasedBy string, versions []CalibrationRow, bucketsTouched, bucketsAtMax int, brakeHeld bool, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// CAS-on-lease-holder (§5.5/GAP-53): flip 'leased'→'committed' ONLY if this worker still holds the
	// lease. A reclaimed lease (status != 'leased' OR leased_by != leasedBy) matches nothing → the whole
	// commit is discarded (ErrRecalLeaseLost), writing NO calibration rows. This is the exactly-once
	// invariant: the CAS, not the lease, is the guarantee (settle-once → spend-once → recalibrate-once).
	var run *RecalRun
	for i := range m.RecalRuns {
		if m.RecalRuns[i].ID == runID {
			run = &m.RecalRuns[i]
			break
		}
	}
	if run == nil || run.Status != "leased" || run.LeasedBy != leasedBy {
		return ErrRecalLeaseLost
	}
	// Stage the proposed versions (append, version-assigned per bucket) UNDER the same guard.
	for _, v := range versions {
		ver := 0
		for _, c := range m.Calibration {
			if c.Reason == v.Reason && c.MXProvider == v.MXProvider && c.Vendor == v.Vendor && c.ResidueClass == v.ResidueClass {
				if c.Version > ver {
					ver = c.Version
				}
			}
		}
		v.Version = ver + 1
		m.Calibration = append(m.Calibration, v)
	}
	run.Status = "committed"
	run.BucketsTouched = bucketsTouched
	run.BucketsAtMax = bucketsAtMax
	run.BrakeHeld = brakeHeld
	return nil
}

func (m *MemStore) RecentRecalRuns(_ context.Context, limit int) ([]RecalRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Committed runs, most recent (by leased_at) first, capped at limit — the cascade brake's window.
	committed := make([]RecalRun, 0, len(m.RecalRuns))
	for _, r := range m.RecalRuns {
		if r.Status == "committed" {
			committed = append(committed, r)
		}
	}
	sort.Slice(committed, func(i, j int) bool { return committed[i].LeasedAt.After(committed[j].LeasedAt) })
	if limit > 0 && len(committed) > limit {
		committed = committed[:limit]
	}
	return committed, nil
}

func (m *MemStore) GetOutcomeCursor(_ context.Context, sourceKey string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.cursors[sourceKey]
	return c, ok, nil
}

func (m *MemStore) SetOutcomeCursor(_ context.Context, sourceKey, cursor string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cursors[sourceKey] = cursor
	return nil
}

// SeedSendOutcome appends a pre-classified outcome directly (test-support: model a send_outcomes row
// without going through the ingestion path — e.g. to seed the §4.6 historical-bounce rollup). It still
// honors the (message_id,event) dedup so a duplicated seed is a no-op.
func (m *MemStore) SeedSendOutcome(row SendOutcomeRow) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if row.MessageID != "" && row.Event != "" {
		k := row.MessageID + "\x00" + row.Event
		if _, dup := m.outcomeDedup[k]; dup {
			return
		}
		m.outcomeDedup[k] = struct{}{}
	}
	m.SendOutcomes = append(m.SendOutcomes, row)
}

// ── local helpers (mem-side; kept out of the flywheel package so the store never imports it) ──

// classInvalidSignalMem mirrors flywheel.ClassInvalidSignal's string WITHOUT importing the flywheel
// package (store must not depend on flywheel — layering). The value is the migration-0012 CHECK token.
type calibClass string

const classInvalidSignalMem calibClass = "invalid_signal"

// calibratable mirrors flywheel.Classification.Calibrates() + the §4.4 exclusions, at the store layer:
// a row calibrates iff it has a real signal (invalid_signal, or valid_signal that isn't the near-zero
// catch-all case), is not from_cache, and carries no ingest_error. The classifier stamps
// CalibrationClass='excluded' for from_cache/override rows, and 'zero_information' for sender-fault
// bounces — both are non-calibratable here, so the exclusion discipline holds at the read (§4.3/§4.4).
func calibratable(o SendOutcomeRow) bool {
	if o.IngestError != "" || o.FromCache {
		return false
	}
	switch o.CalibrationClass {
	case "invalid_signal":
		return true
	case "valid_signal":
		return true // strength-zero (delivered-on-catch-all) is stamped 'excluded' by the classifier, not here
	default:
		return false // zero_information / excluded / "" — never calibrates
	}
}

// verdictReason returns the joined verdict's reason from the append-only LOG — the calibration bucket's
// authoritative reason dimension (§5.2). Mirrors the PG JOIN send_outcomes→verifications. Caller holds
// the lock. "" verification_id (a send with no prior verdict, GAP-51) never reaches here — such rows
// carry an ingest_error and are non-calibratable.
func (m *MemStore) verdictReason(verificationID string) string {
	for i := len(m.Log) - 1; i >= 0; i-- {
		if m.Log[i].ID == verificationID {
			return m.Log[i].Reason
		}
	}
	return ""
}

func inWindow(t, since, now time.Time) bool {
	if t.Before(since) {
		return false
	}
	if !now.IsZero() && t.After(now) {
		return false
	}
	return true
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
