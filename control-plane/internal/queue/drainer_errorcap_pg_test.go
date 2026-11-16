package queue

import (
	"context"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/hash"
	"github.com/coldmail-verify/control-plane/internal/store"
)

// PG-gated (TM_TEST_DATABASE_URL; SKIP-honest when unset, like every test that needs the real database): the row an error-capped item
// settles with is accepted by REAL Postgres — every CHECK (valid_reason carries unknown_error, hash_scheme_linkage, the uuid columns) and
// the jsonb signals — and reads back with the signals the platform will see. The MemStore cannot prove any of that: it stores whatever it
// is given. The drainer test (drainer_errorcap_test.go) proves WHEN the row is built and settled; this proves the row is a row Postgres
// takes.
func TestPGE2E_ErrorCappedVerdictIsAcceptedByPostgresAndReadsBack(t *testing.T) {
	pg := testPGStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	h, err := hash.NewHasher([]byte("errorcap-pg-pepper"))
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	norm, domain, err := h.Normalize("capped-" + now.Format("150405.000000000") + "@acmecorp.com")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	it := store.WorkItemRow{
		ID: "018f0000-0000-7000-8000-" + randHex12(), Domain: domain, EmailHash: h.EmailHash(norm),
		PolicyKey: "cold-email", PolicyVersion: 1, ConfigFingerprint: "cfg-errorcap-pg", Status: "in_flight", Attempt: 4,
	}
	out, err := ErrorCappedOutcome(it, now, "compute_error")
	if err != nil {
		t.Fatalf("ErrorCappedOutcome: %v", err)
	}
	if err := pg.AppendVerification(ctx, out.Row, nil); err != nil {
		t.Fatalf("Postgres refused the error-capped verdict (a CHECK, a uuid or the signals jsonb): %v", err)
	}
	t.Cleanup(func() {
		// Owner-role cleanup (verify_app can't DELETE), as the other PG tests do.
		_, _ = pg.Pool().Exec(context.Background(), `DELETE FROM verify.verifications WHERE id=$1`, out.Row.ID)
	})

	var status, reason, recommendation, origin, capped string
	var confidence int
	var fromCache bool
	if err := pg.Pool().QueryRow(ctx,
		`SELECT status, reason, recommendation, confidence, from_cache,
		        COALESCE(signals->>'failure_origin',''), COALESCE(signals->>'error_capped','')
		   FROM verify.verifications WHERE id=$1`, out.Row.ID).Scan(&status, &reason, &recommendation, &confidence, &fromCache, &origin, &capped); err != nil {
		t.Fatalf("read the verdict back: %v", err)
	}
	if status != "unknown" || reason != "unknown_error" || recommendation != "review" || confidence != 10 || fromCache {
		t.Errorf("stored verdict = %s/%s/%s/%d from_cache=%v, want unknown/unknown_error/review/10 false", status, reason, recommendation, confidence, fromCache)
	}
	if origin != "own" {
		t.Errorf("signals.failure_origin = %q, want own", origin)
	}
	if capped != "true" {
		t.Errorf("signals.error_capped = %q, want the JSON boolean true", capped)
	}
}
