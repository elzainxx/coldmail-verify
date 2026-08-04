package queue

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/hash"
	"github.com/coldmail-verify/control-plane/internal/orchestrator"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/store"
)

// PG-gated end-to-end tests: real Enqueuer → real Postgres store → real drainer. Gated on
// TM_TEST_DATABASE_URL, SKIP-honest when unset (test-strategy §4). Prove the master invariant and
// the DR-4 at-rest guarantee against REAL Postgres, not just the Mem fake.

func testPGStore(t *testing.T) *store.PG {
	t.Helper()
	dsn := os.Getenv("TM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PENDING: set TM_TEST_DATABASE_URL to a migrated Postgres to exercise the real queue (never faked)")
	}
	pg, err := store.OpenPG(context.Background(), dsn)
	if err != nil {
		t.Fatalf("OpenPG: %v", err)
	}
	t.Cleanup(pg.Close)
	return pg
}

// settleE2EComputer settles every check valid/accepted on the first attempt (the drainer's
// Compute dependency — the real ComputeForQueue is exercised in the orchestrator/api tests; here we
// isolate the queue plumbing against real PG).
type settleE2EComputer struct{ now time.Time }

func (c settleE2EComputer) ComputeForQueue(_ context.Context, _ string, _ *policy.Definition, _ string) (orchestrator.QueueOutcome, error) {
	// The row id is assigned by the caller in real code; assign one here (valid UUID: the final
	// group is exactly 12 hex chars). randHex12 supplies those 12.
	id := "018f0000-0000-7000-8000-" + randHex12()
	row := store.VerificationRow{
		ID: id, RequestID: id, CheckedAt: c.now, CreatedAt: c.now,
		EmailHash: []byte("hashbytes"), HashScheme: hash.SchemeTag, Domain: "acmecorp.com",
		Status: "valid", Reason: "accepted", Confidence: 90, Recommendation: "send",
		PolicyKey: "cold-email", PolicyVersion: 1, APIVersion: "v1", ConfigFingerprint: "cfg",
	}
	return orchestrator.QueueOutcome{Class: orchestrator.QSettled, Row: row}, nil
}

// EscalateExhausted: no fallback wired in this e2e test → outcome unchanged (no-egress default).
func (c settleE2EComputer) EscalateExhausted(_ context.Context, _ string, _ *policy.Definition, out orchestrator.QueueOutcome, _, _ string) orchestrator.QueueOutcome {
	return out
}

var e2eCounter int

func randHex12() string {
	e2eCounter++
	// 12 hex chars, unique-ish per call within a run.
	const hexd = "0123456789abcdef"
	n := e2eCounter
	b := make([]byte, 12)
	for i := 11; i >= 0; i-- {
		b[i] = hexd[n&0xf]
		n >>= 4
	}
	return string(b)
}

// TestPGE2E_SubmitDrainSettleResults_MasterInvariant proves the full §7 lifecycle against real
// Postgres: SubmitBulk enqueues durable work-items holding ENCRYPTED plaintext; the drainer leases
// them, settles via the GAP-37 CAS; job completion is authoritative (GAP-38); and the plaintext
// column is CIPHERTEXT at rest, purged to NULL on settle (DR-4/OQ-P5-9). This is the definitive
// integration proof that the queue changes WHEN a check runs, never WHETHER (§8) — the drainer's
// only path to settlement is the store CAS.
func TestPGE2E_SubmitDrainSettleResults_MasterInvariant(t *testing.T) {
	pg := testPGStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	h, err := hash.NewHasher([]byte("e2e-test-pepper"))
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	cipher, err := NewAddrCipher(key32(3))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	enq := &Enqueuer{Store: pg, Hasher: h, Cipher: cipher, ConfigFingerprint: "cfg", Now: func() time.Time { return now }}

	// Unique addresses per run so a shared DB doesn't collide on idempotency_key.
	tag := now.Format("150405.000000000")
	addrs := []string{"alice-" + tag + "@acmecorp.com", "bob-" + tag + "@acmecorp.com"}
	res, err := enq.SubmitBulk(ctx, addrs, "cold-email", 1, "platform-ref-e2e")
	if err != nil {
		t.Fatalf("SubmitBulk: %v", err)
	}
	if res.Total != 2 || res.Enqueued != 2 {
		t.Fatalf("expected 2 enqueued, got total=%d enqueued=%d malformed=%d", res.Total, res.Enqueued, res.Malformed)
	}
	jobID := res.JobID
	t.Cleanup(func() {
		// Owner-role cleanup (cp_runtime can't DELETE).
		_, _ = pg.Pool().Exec(context.Background(), `DELETE FROM work_item_attempts WHERE work_item_id IN (SELECT id FROM work_items WHERE job_id=$1)`, jobID)
		_, _ = pg.Pool().Exec(context.Background(), `DELETE FROM verifications WHERE id IN (SELECT verification_id FROM work_items WHERE job_id=$1 AND verification_id IS NOT NULL)`, jobID)
		_, _ = pg.Pool().Exec(context.Background(), `DELETE FROM work_items WHERE job_id=$1`, jobID)
		_, _ = pg.Pool().Exec(context.Background(), `DELETE FROM jobs WHERE id=$1`, jobID)
	})

	// DR-4 at-rest proof: BEFORE draining, the plaintext column is CIPHERTEXT (not the raw address).
	var addrCT []byte
	if err := pg.Pool().QueryRow(ctx,
		`SELECT addr_ciphertext FROM work_items WHERE job_id=$1 LIMIT 1`, jobID).Scan(&addrCT); err != nil {
		t.Fatalf("read addr_ciphertext: %v", err)
	}
	if len(addrCT) == 0 {
		t.Fatal("a live work-item must hold the (encrypted) plaintext")
	}
	for _, a := range addrs {
		if strings.Contains(string(addrCT), a) || strings.Contains(string(addrCT), "alice-"+tag) {
			t.Fatal("DR-4 VIOLATED: raw address found in the at-rest column (not encrypted)")
		}
	}

	// Not complete yet (both items ready).
	if complete, _, _, _ := pg.JobCompletion(ctx, jobID); complete {
		t.Fatal("job must not be complete before draining")
	}

	// Drain: a real drainer against real PG settles both items via the CAS.
	dr, err := NewDrainer(Deps{
		Store: pg, Compute: settleE2EComputer{now: now}, Policy: fakePolicy{}, Cipher: cipher,
		Backoff:  Backoff{Base: time.Minute, Factor: 2, Cap: time.Hour, RateWait: time.Second},
		Now:      func() time.Time { return now },
		WorkerID: "e2e-worker", LeaseTTL: time.Minute, MaxSMTPDeadline: 10 * time.Second,
		MaxAttempts: 3, MaxRetryAge: time.Hour, BatchSize: 10, ReclaimEvery: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewDrainer: %v", err)
	}
	dr.TickOnce(ctx)

	// Authoritative completion (GAP-38): all items settled → complete.
	complete, total, settled, err := pg.JobCompletion(ctx, jobID)
	if err != nil {
		t.Fatalf("JobCompletion: %v", err)
	}
	if !complete || total != 2 || settled != 2 {
		t.Fatalf("after draining: complete=%v total=%d settled=%d (want true/2/2)", complete, total, settled)
	}

	// Results: 2 settled verdicts joined from the LOG.
	results, _, err := pg.ListJobResults(ctx, jobID, "", 10)
	if err != nil {
		t.Fatalf("ListJobResults: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for _, r := range results {
		if r.Verdict.Status != "valid" || r.Verdict.Reason != "accepted" {
			t.Fatalf("result verdict mismatch: %s/%s", r.Verdict.Status, r.Verdict.Reason)
		}
	}

	// DR-4 purge proof: AFTER settle, the plaintext column is NULL on every item.
	var remaining int
	if err := pg.Pool().QueryRow(ctx,
		`SELECT count(*) FROM work_items WHERE job_id=$1 AND addr_ciphertext IS NOT NULL`, jobID).Scan(&remaining); err != nil {
		t.Fatalf("count non-null ciphertext: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("OQ-P5-9 VIOLATED: %d settled item(s) still hold plaintext (not purged in the settlement tx)", remaining)
	}
}
