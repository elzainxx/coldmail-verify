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
