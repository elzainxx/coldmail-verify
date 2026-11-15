package store

import (
	"context"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/idgen"
)

// The Phase-7 flywheel PG-parity tests. They prove against REAL Postgres 16 (fresh migrations
// 0001–0012) the two pieces the Mem fake can only APPROXIMATE: (1) the (message_id,event) UNIQUE
// dedup no-op (§3.4 idempotency, DB-enforced), and (2) CommitRecalibration's CAS-on-lease-holder
// exactly-once (§5.5/GAP-53). Gated on TM_TEST_DATABASE_URL; SKIP honestly when unset (never faked).
// The Mem CAS semantics MUST match these exactly (the P3 test-double lesson).

func mkPGVerdict(t *testing.T, pg *PG, status, reason, provider string, fromCache bool, now time.Time) string {
	t.Helper()
	id, _ := idgen.NewV7String(now)
	row := VerificationRow{
		ID: id, RequestID: id, CheckedAt: now, CreatedAt: now,
		EmailHash: []byte("hash-32-bytes-for-pg-test-xxxxxx")[:32], HashScheme: "norm/v1+hmac-sha256/v1",
		Domain: "ex.com", MXProvider: provider,
		Status: status, Reason: reason, Confidence: 50, Recommendation: "review",
		PolicyKey: "cold-email", PolicyVersion: 1, APIVersion: "v1", ConfigFingerprint: "cfg",
		FromCache: fromCache,
	}
	if err := pg.AppendVerification(context.Background(), row, nil); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}
	return id
}

// TestPGSendOutcome_DedupNoOp proves the (message_id, event) UNIQUE partial index makes a replayed
// delivery a no-op at the DB layer (§3.4). FAIL-VERIFY: drop uq_send_outcomes_message_event in the
// migration → the second insert succeeds → ErrOutcomeDuplicate stops firing → this breaks.
func TestPGSendOutcome_DedupNoOp(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// Real PG persists across runs; a per-run-unique provider isolates THIS test's bucket count, and a
	// per-run-unique message_id avoids colliding with a prior run's dedup key.
	uniq := mustV7(t, now)
	prov := "pgdedup-" + uniq
	vid := mkPGVerdict(t, pg, "valid", "accepted", prov, false, now.Add(-time.Hour))

	o := SendOutcomeRow{
		ID: mustV7(t, now), VerificationID: vid, EmailHash: []byte("h-x"), Domain: "ex.com",
		MXProvider: prov, MessageID: "dup-" + uniq, Event: "550 5.1.1 user unknown",
		SentAt: now, OccurredAt: now, CalibrationClass: "invalid_signal", IngestedAt: now,
	}
	if err := pg.AppendSendOutcome(ctx, o); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// The SAME (message_id, event), different row id → the UNIQUE index makes it a no-op.
	o2 := o
	o2.ID = mustV7(t, now.Add(time.Second))
	if err := pg.AppendSendOutcome(ctx, o2); err != ErrOutcomeDuplicate {
		t.Fatalf("replay append err = %v, want ErrOutcomeDuplicate (§3.4 DB dedup)", err)
	}
	// This test's bucket (accepted × prov) counts the outcome exactly ONCE despite two append attempts.
	aggs, err := pg.BucketAggregatesSince(ctx, now.Add(-24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var mine int
	for _, a := range aggs {
		if a.MXProvider == prov {
			mine += a.InvalidN
		}
	}
	if mine != 1 {
		t.Fatalf("invalid-signal count for %s = %d after a replay — want exactly 1 (dedup no-op, §3.4)", prov, mine)
	}
}

// TestPGRecalibration_CASExactlyOnce proves the CommitRecalibration CAS-on-lease-holder against real
// Postgres (§5.5/GAP-53): a stale cycle whose lease-status is no longer 'leased' matches zero rows and
// discards ALL its bucket writes. FAIL-VERIFY: drop the "AND status='leased'" clause in the CAS →
// the second commit writes a second version → this breaks.
func TestPGRecalibration_CASExactlyOnce(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	now := time.Now().UTC()
	runID := mustV7(t, now)
	// Real PG persists across test runs (unlike the fresh Mem fake), so use a per-run-unique bucket key
	// (mirrors how the queue PG tests use unique work-item ids for isolation).
	prov := "pgtest-cas-" + runID
	if _, err := pg.LeaseRecalibration(ctx, RecalRun{ID: runID, LeasedBy: "A", LeasedAt: now, WindowFrom: now.Add(-time.Hour), WindowTo: now}); err != nil {
		t.Fatalf("lease: %v", err)
	}
	prop := []CalibrationRow{{ID: mustV7(t, now), Reason: "accepted", MXProvider: prov, Confidence: 80, SampleN: 600, Mode: "recommend_only", CreatedAt: now}}
	// First commit by the holder succeeds (status leased → committed).
	if err := pg.CommitRecalibration(ctx, runID, "A", prop, 1, 0, false, now); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	// Second commit on the same run (now 'committed', not 'leased') → CAS matches zero rows → discarded.
	prop2 := []CalibrationRow{{ID: mustV7(t, now.Add(time.Second)), Reason: "accepted", MXProvider: prov, Confidence: 20, SampleN: 600, Mode: "recommend_only", CreatedAt: now}}
	if err := pg.CommitRecalibration(ctx, runID, "A", prop2, 1, 0, false, now); err != ErrRecalLeaseLost {
		t.Fatalf("stale commit err = %v, want ErrRecalLeaseLost (exactly-once, §5.5/GAP-53)", err)
	}
	// Exactly ONE version was written for the bucket despite two commit attempts.
	cnt := countCalibrationVersions(t, pg, "accepted", prov)
	if cnt != 1 {
		t.Fatalf("calibration versions for (accepted×%s) = %d after a double-commit — want 1 (recalibrate-once)", prov, cnt)
	}
}

// TestPGCalibration_EffectiveReadAndApprove proves the versioned effective-dating on real PG (§5.4):
// a proposal (effective_from NULL) is NOT read by GetEffectiveCalibration; after ApproveCalibration it
// is, and a prior effective version is superseded. This is the recommend-only "not effective until
// approved" mechanism (shadow-mode §2.1) proven at the DB layer.
func TestPGCalibration_EffectiveReadAndApprove(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	now := time.Now().UTC()
	calID := mustV7(t, now)
	prov := "pgtest-eff-" + calID // per-run-unique bucket (real PG persists across runs)
	if _, err := pg.AppendCalibrationVersion(ctx, CalibrationRow{ID: calID, Reason: "plausible", MXProvider: prov, Confidence: 22, SampleN: 700, Mode: "recommend_only", CreatedAt: now}); err != nil {
		t.Fatalf("append version: %v", err)
	}
	// A proposal is NOT effective (recommend-only → zero live mutation).
	if _, known, err := pg.GetEffectiveCalibration(ctx, "plausible", prov, "", ""); err != nil || known {
		t.Fatalf("proposal read as effective (known=%v err=%v) — recommend-only must not be effective (§5.4)", known, err)
	}
	// Approve → effective.
	if err := pg.ApproveCalibration(ctx, calID, now); err != nil {
		t.Fatalf("approve: %v", err)
	}
	c, known, err := pg.GetEffectiveCalibration(ctx, "plausible", prov, "", "")
	if err != nil || !known || c.Confidence != 22 {
		t.Fatalf("approved version not effective (known=%v conf=%d err=%v) — approval must promote (§5.4)", known, c.Confidence, err)
	}
}

// The send_outcomes grant test used to live here as "attempt the DELETE; skip if it succeeds". CI
// connects as superuser, so it ALWAYS SKIPPED while ci.yml claimed append-only was proven on every
// push (D13 / fix-plan item 3). It is replaced by TestPGGrants_RuntimeCannotDeleteSendOutcomes in
// pg_grants_test.go, which SET LOCAL ROLEs to verify_app and therefore actually enforces — plus an
// anti-vacuity control proving the role switch drops privilege.

// ── helpers ── (mustV7 is defined in postgres_test.go)

func countCalibrationVersions(t *testing.T, pg *PG, reason, provider string) int {
	t.Helper()
	var n int
	if err := pg.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM verify.calibration WHERE reason=$1 AND mx_provider=$2`, reason, provider).Scan(&n); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	return n
}
