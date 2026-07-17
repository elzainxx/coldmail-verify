package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/idgen"
)

// These tests run against a REAL Postgres with the P1 migrations applied. They are gated on
// TM_TEST_DATABASE_URL and SKIP honestly when it is unset (test-strategy §4 — never fake
// green; a green run without the DSN means "not exercised", not "passed"). The migrations
// must be applied to the target DB first (see control-plane/migrations/README.md).
//
// Run locally, e.g.:
//   docker run -d --name cp-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=cptest -p 55432:5432 postgres:16-alpine
//   for f in migrations/0001*.sql migrations/0002*.sql migrations/0003*.sql migrations/0004*.sql; do \
//     psql "postgres://postgres:test@localhost:55432/cptest" -f "$f"; done
//   TM_TEST_DATABASE_URL="postgres://postgres:test@localhost:55432/cptest" go test ./internal/store/ -run PG -v

func testPG(t *testing.T) *PG {
	t.Helper()
	dsn := os.Getenv("TM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PENDING: set TM_TEST_DATABASE_URL to a migrated Postgres to exercise the real store (never faked)")
	}
	pg, err := OpenPG(context.Background(), dsn)
	if err != nil {
		t.Fatalf("OpenPG: %v", err)
	}
	t.Cleanup(pg.Close)
	return pg
}

func TestPGAppendAndCacheRoundTrip(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id, err := idgen.NewV7String(now)
	if err != nil {
		t.Fatalf("idgen: %v", err)
	}
	reqID, _ := idgen.NewV7String(now)

	row := VerificationRow{
		ID: id, RequestID: reqID, CheckedAt: now, CreatedAt: now,
		EmailHash: []byte{0xde, 0xad, 0xbe, 0xef}, HashScheme: "norm/v1+hmac-sha256/v1",
		Domain: "example.com", MXProvider: "other",
		Status: "risky", Reason: "plausible", Confidence: 30, Recommendation: "review",
		Flags: map[string]bool{"from_cache": false}, Signals: map[string]any{"syntax_valid": true},
		PolicyKey: "cold-email", PolicyVersion: 1, APIVersion: "v1",
		ConfigFingerprint: "cfg_sha256:test", FromCache: false, LatencyMS: 12,
	}
	if err := pg.AppendVerification(ctx, row, nil); err != nil {
		t.Fatalf("AppendVerification: %v", err)
	}

	// Cache round-trip on the composite key.
	cv := CachedVerdict{
		EmailHash: row.EmailHash, ConfigFingerprint: "cfg_sha256:test", Domain: "example.com",
		MXProvider: "other", HashScheme: row.HashScheme,
		Status: "risky", Reason: "plausible", Confidence: 30, Recommendation: "review",
		Flags: map[string]bool{"from_cache": false}, Signals: map[string]any{"syntax_valid": true},
		CachedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := pg.PutCachedVerdict(ctx, cv); err != nil {
		t.Fatalf("PutCachedVerdict: %v", err)
	}
	got, err := pg.GetCachedVerdict(ctx, row.EmailHash, "cfg_sha256:test")
	if err != nil {
		t.Fatalf("GetCachedVerdict: %v", err)
	}
	if got.Status != "risky" || got.Confidence != 30 || got.Domain != "example.com" {
		t.Errorf("cache round-trip mismatch: %+v", got)
	}
	// Wrong config => miss (composite-key correctness against real PG).
	if _, err := pg.GetCachedVerdict(ctx, row.EmailHash, "different-cfg"); err != ErrCacheMiss {
		t.Errorf("wrong config must miss, got %v", err)
	}
}

// A NULL-hash (invalid_syntax) row must insert (§3.2.7) — email_hash + hash_scheme + domain
// all NULL, which the DB CHECK (email_hash IS NULL)=(hash_scheme IS NULL) must accept.
func TestPGNullHashRowInserts(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id, _ := idgen.NewV7String(now)
	reqID, _ := idgen.NewV7String(now)
	row := VerificationRow{
		ID: id, RequestID: reqID, CheckedAt: now, CreatedAt: now,
		// EmailHash/HashScheme/Domain deliberately zero => NULL.
		Status: "invalid", Reason: "invalid_syntax", Confidence: 99, Recommendation: "skip",
		Flags: map[string]bool{}, Signals: map[string]any{},
		PolicyKey: "cold-email", PolicyVersion: 1, APIVersion: "v1",
		ConfigFingerprint: "cfg_sha256:test", FromCache: false,
	}
	if err := pg.AppendVerification(ctx, row, nil); err != nil {
		t.Fatalf("un-addressable reject row must insert with NULL hash (§3.2.7): %v", err)
	}
}

// P2: the SMTP path writes the raw engine blob to the verification_raw sidecar (§6.4), atomically
// with the LOG row. Proves the sidecar insert works against the real partitioned table.
func TestPGAppendWithRawSidecar(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id, _ := idgen.NewV7String(now)
	reqID, _ := idgen.NewV7String(now)
	row := VerificationRow{
		ID: id, RequestID: reqID, CheckedAt: now, CreatedAt: now,
		EmailHash: []byte{0x01, 0x02, 0x03, 0x04}, HashScheme: "norm/v1+hmac-sha256/v1",
		Domain: "corp.test", Status: "valid", Reason: "accepted", Confidence: 90, Recommendation: "send",
		Flags: map[string]bool{"from_cache": false}, Signals: map[string]any{"smtp_code": "250"},
		PolicyKey: "cold-email", PolicyVersion: 1, APIVersion: "v1", ConfigFingerprint: "cfg_sha256:test",
		NodeID: "n1", EngineVersion: "rack@0.9.0+gem@3.3.1", EngineValidationType: "smtp",
		EngineSuccessSet: true, EngineSuccess: true,
	}
	raw := &RawRow{VerificationID: id, Raw: []byte(`{"wrapper_api_version":"v1","success":true}`)}
	if err := pg.AppendVerification(ctx, row, raw); err != nil {
		t.Fatalf("SMTP verdict + raw sidecar must insert atomically (§6.4): %v", err)
	}
}

// P2 §5.6: the brain persists the catch-all conclusion (migration 0006 grants cp_runtime the
// write). A definitive true/false upserts; an inconclusive (nil) is a NO-OP that never overwrites.
func TestPGSetCatchAllTriState(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	dom := "catchall-" + must(idgen.NewV7String(time.Now().UTC())) + ".test" // unique per run
	tru := true

	// First probe → catchall true.
	if err := pg.SetCatchAll(ctx, dom, &tru, "probe", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetCatchAll(true): %v", err)
	}
	is, known, err := pg.CatchAllState(ctx, dom)
	if err != nil || !known || !is {
		t.Fatalf("after true probe want known=true is=true, got known=%v is=%v err=%v", known, is, err)
	}

	// Inconclusive (nil) must NOT overwrite the known value (§5.6).
	if err := pg.SetCatchAll(ctx, dom, nil, "probe", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetCatchAll(nil) should be a no-op, got: %v", err)
	}
	is, known, _ = pg.CatchAllState(ctx, dom)
	if !known || !is {
		t.Fatal("inconclusive must NOT overwrite a known catch-all value (§5.6)")
	}

	// A definitive false re-probe updates it.
	fal := false
	if err := pg.SetCatchAll(ctx, dom, &fal, "probe", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetCatchAll(false): %v", err)
	}
	is, known, _ = pg.CatchAllState(ctx, dom)
	if !known || is {
		t.Fatalf("after false re-probe want known=true is=false, got known=%v is=%v", known, is)
	}
}

func must(s string, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}
