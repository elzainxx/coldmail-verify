package orchestrator

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/hash"
	"github.com/mnfi/codmailling/control-plane/internal/layers"
	"github.com/mnfi/codmailling/control-plane/internal/policy"
	"github.com/mnfi/codmailling/control-plane/internal/store"
	"github.com/mnfi/codmailling/control-plane/internal/vocab"
)

type fixtureResolver struct {
	mx  map[string][]layers.MXHost
	err map[string]error
}

func (f fixtureResolver) LookupMX(_ context.Context, d string) ([]layers.MXHost, error) {
	if e, ok := f.err[d]; ok {
		return nil, e
	}
	return f.mx[d], nil
}
func (f fixtureResolver) LookupIP(context.Context, string) ([]net.IP, error) { return nil, nil }

func setup(t *testing.T, r layers.Resolver, ttl time.Duration) (*Orchestrator, *store.MemStore, *policy.Definition) {
	t.Helper()
	h, err := hash.NewHasher([]byte("orch-test-pepper"))
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	mem := store.NewMem(nil)
	def, err := policy.ColdEmailDefinition()
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	o := New(Deps{Hasher: h, Store: mem, Resolver: r, CacheTTL: ttl, ConfigFingerprint: "cfg_sha256:test"})
	return o, mem, def
}

func corpResolver() fixtureResolver {
	return fixtureResolver{mx: map[string][]layers.MXHost{"acmecorp.com": {{Host: "mx.acmecorp.com", Pref: 10}}}}
}

// The full lifecycle: a clean corp address → risky/plausible, logged exactly once, cached.
func TestVerifyLogsAndCaches(t *testing.T) {
	o, mem, def := setup(t, corpResolver(), time.Hour)
	resp, err := o.Verify(context.Background(), "jane.doe@acmecorp.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Status != vocab.StatusRisky || resp.Reason != vocab.ReasonPlausible {
		t.Errorf("clean corp => risky/plausible, got %s/%s", resp.Status, resp.Reason)
	}
	if resp.Status == vocab.StatusValid {
		t.Fatal("P1 must never mint valid")
	}
	if len(mem.Log) != 1 {
		t.Fatalf("exactly one LOG row per verify, got %d", len(mem.Log))
	}
	if mem.Log[0].FromCache {
		t.Error("first (computed) verdict must be from_cache=false")
	}
	if len(mem.Log[0].EmailHash) != 32 {
		t.Error("computed row must carry the 32-byte email_hash")
	}
}

// A second identical request hits the cache and RE-LOGS with from_cache=true (§4.2/OQ-9).
func TestVerifyCacheHitReLogs(t *testing.T) {
	o, mem, def := setup(t, corpResolver(), time.Hour)
	ctx := context.Background()
	if _, err := o.Verify(ctx, "jane.doe@acmecorp.com", def); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if _, err := o.Verify(ctx, "jane.doe@acmecorp.com", def); err != nil {
		t.Fatalf("second verify: %v", err)
	}
	if len(mem.Log) != 2 {
		t.Fatalf("a cache hit is still logged — want 2 rows, got %d", len(mem.Log))
	}
	if mem.Log[0].FromCache {
		t.Error("row 1 (computed) must be from_cache=false")
	}
	if !mem.Log[1].FromCache {
		t.Error("row 2 (cache hit) must be from_cache=true (§4.2)")
	}
}

// An un-parseable input is logged as invalid_syntax with a NULL hash (§3.2.7), never dropped.
func TestVerifyUnparseableLogsNullHash(t *testing.T) {
	o, mem, def := setup(t, corpResolver(), time.Hour)
	resp, err := o.Verify(context.Background(), "not-an-email", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Status != vocab.StatusInvalid || resp.Reason != vocab.ReasonInvalidSyntax {
		t.Errorf("unparseable => invalid/invalid_syntax, got %s/%s", resp.Status, resp.Reason)
	}
	if resp.Recommendation == vocab.RecSend {
		t.Fatal("invalid must never say send (Door 1)")
	}
	if len(mem.Log) != 1 || mem.Log[0].EmailHash != nil {
		t.Errorf("reject must be logged once with NULL hash, got %d rows hash=%v", len(mem.Log), mem.Log[0].EmailHash)
	}
	// Un-hashable rejects are NOT cached (no key).
	if mem.CacheLen() != 0 {
		t.Error("un-addressable reject must not be cached")
	}
}

// No TTL configured (OQ-9 unset) => verdict still logged, but NOT cached (expires_at is NOT
// NULL, so we never write a guessed TTL).
func TestVerifyNoTTLSkipsCache(t *testing.T) {
	o, mem, def := setup(t, corpResolver(), 0)
	ctx := context.Background()
	if _, err := o.Verify(ctx, "jane.doe@acmecorp.com", def); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(mem.Log) != 1 {
		t.Errorf("verdict must still be logged without a TTL, got %d rows", len(mem.Log))
	}
	if mem.CacheLen() != 0 {
		t.Error("with no TTL configured, nothing may be cached (never a guessed expires_at)")
	}
}

// The LOG write failing must FAIL the request (Door 2: never return an unlogged verdict).
func TestVerifyLogFailureFailsRequest(t *testing.T) {
	h, _ := hash.NewHasher([]byte("p"))
	def, _ := policy.ColdEmailDefinition()
	o := New(Deps{Hasher: h, Store: failingStore{}, Resolver: corpResolver(), CacheTTL: time.Hour, ConfigFingerprint: "cfg"})
	if _, err := o.Verify(context.Background(), "jane@acmecorp.com", def); err == nil {
		t.Fatal("a failed LOG write must fail the request — an unlogged verdict violates Door 2")
	}
}

// Regression: the LOG row's id AND request_id must be valid UUIDs (both are Postgres `uuid`
// columns, §3.2.2). A "req-"-prefixed or otherwise non-UUID value inserts fine into the
// MemStore fake but is REJECTED by real Postgres (SQLSTATE 22P02) — this pins the format so
// that class of bug is caught without a DB. (It escaped once; never again.)
func TestVerifyIDsAreValidUUIDs(t *testing.T) {
	o, mem, def := setup(t, corpResolver(), time.Hour)
	if _, err := o.Verify(context.Background(), "jane.doe@acmecorp.com", def); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	row := mem.Log[0]
	if !isUUID(row.ID) {
		t.Errorf("LOG id must be a valid UUID (Postgres uuid column), got %q", row.ID)
	}
	if !isUUID(row.RequestID) {
		t.Errorf("request_id must be a valid UUID (no 'req-' prefix — it is a Postgres uuid column), got %q", row.RequestID)
	}
}

// isUUID checks the canonical 8-4-4-4-12 hex form (enough to catch a prefix/format bug).
func isUUID(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	for i, r := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// Not-ready (no store) => ErrNotReady, never a verdict.
func TestVerifyNotReady(t *testing.T) {
	h, _ := hash.NewHasher([]byte("p"))
	def, _ := policy.ColdEmailDefinition()
	o := New(Deps{Hasher: h, Store: nil, Resolver: corpResolver()})
	if _, err := o.Verify(context.Background(), "a@b.co", def); !errors.Is(err, ErrNotReady) {
		t.Errorf("nil store must yield ErrNotReady, got %v", err)
	}
}

// failingStore appends error — everything else behaves like an empty store.
type failingStore struct{ store.StubStore }

func (failingStore) AppendVerification(context.Context, store.VerificationRow, *store.RawRow) error {
	return errors.New("db down")
}
func (failingStore) GetCachedVerdict(context.Context, []byte, string) (store.CachedVerdict, error) {
	return store.CachedVerdict{}, store.ErrCacheMiss
}
func (failingStore) IsDisposable(context.Context, string) (bool, error) { return false, nil }
func (failingStore) IsSpamTrap(context.Context, string) (bool, error)   { return false, nil }
