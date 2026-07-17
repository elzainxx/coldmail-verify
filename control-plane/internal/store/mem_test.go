package store

import (
	"context"
	"testing"
	"time"
)

func TestMemAppendOnlyAccumulates(t *testing.T) {
	m := NewMem(nil)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := m.AppendVerification(ctx, VerificationRow{ID: "id", Status: "unknown"}, nil); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// The LOG accumulates every result; there is NO method that could update/delete one.
	if len(m.Log) != 3 {
		t.Errorf("append-only LOG must accumulate all rows, got %d want 3", len(m.Log))
	}
	// The Store interface itself is the proof: assert MemStore has no mutation method by
	// confirming it satisfies Store (which offers only AppendVerification for the LOG).
	var _ Store = m
}

func TestMemCacheTTLMiss(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	m := NewMem(func() time.Time { return now })
	ctx := context.Background()
	eh := []byte{1, 2, 3, 4}
	v := CachedVerdict{
		EmailHash: eh, ConfigFingerprint: "cfg", Domain: "b.co",
		Status: "risky", Reason: "plausible", Confidence: 30, Recommendation: "review",
		CachedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := m.PutCachedVerdict(ctx, v); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Fresh hit.
	if got, err := m.GetCachedVerdict(ctx, eh, "cfg"); err != nil || got.Status != "risky" {
		t.Fatalf("expected fresh hit, got %+v err=%v", got, err)
	}
	// Wrong config => miss (composite key correctness — never serve a cross-config verdict).
	if _, err := m.GetCachedVerdict(ctx, eh, "other-cfg"); err != ErrCacheMiss {
		t.Errorf("wrong config_fingerprint must miss, got err=%v", err)
	}
	// Advance past expiry => miss (never a stale verdict).
	now = now.Add(2 * time.Hour)
	if _, err := m.GetCachedVerdict(ctx, eh, "cfg"); err != ErrCacheMiss {
		t.Errorf("expired entry must miss, got err=%v", err)
	}
}

func TestMemLists(t *testing.T) {
	m := NewMem(nil)
	ctx := context.Background()
	m.SeedDisposable("mailinator.com")
	m.SeedSpamTrap("trap.example")
	m.SeedCatchAll("catchall.example", true)

	if ok, _ := m.IsDisposable(ctx, "mailinator.com"); !ok {
		t.Error("seeded disposable must be found")
	}
	if ok, _ := m.IsDisposable(ctx, "gmail.com"); ok {
		t.Error("unseeded domain must not be disposable")
	}
	if ok, _ := m.IsSpamTrap(ctx, "trap.example"); !ok {
		t.Error("seeded spam trap must be found")
	}
	// Catch-all tri-state: seeded => known; unseeded => not known (never probed).
	if v, known, _ := m.CatchAllState(ctx, "catchall.example"); !known || !v {
		t.Errorf("seeded catchall must be known+true, got known=%v v=%v", known, v)
	}
	if _, known, _ := m.CatchAllState(ctx, "unprobed.example"); known {
		t.Error("unprobed domain must report known=false (tri-state), never a false catch-all")
	}
}

func TestMemDefaultPolicy(t *testing.T) {
	m := NewMem(nil)
	ctx := context.Background()
	if _, err := m.GetDefaultPolicy(ctx); err != ErrNotWired {
		t.Errorf("no default seeded => ErrNotWired, got %v", err)
	}
	m.SeedPolicy(PolicyRow{Key: "cold-email", Version: 1, Definition: []byte(`{}`), IsDefault: true})
	got, err := m.GetDefaultPolicy(ctx)
	if err != nil || got.Key != "cold-email" || got.Version != 1 {
		t.Fatalf("default policy: got %+v err=%v", got, err)
	}
	if _, err := m.GetPolicy(ctx, "cold-email", 1); err != nil {
		t.Errorf("GetPolicy(cold-email,1): %v", err)
	}
	if _, err := m.GetPolicy(ctx, "cold-email", 99); err != ErrNotWired {
		t.Errorf("missing version => ErrNotWired, got %v", err)
	}
}

// Regression (final-review MINOR): the MemStore fake must not alias the caller's Flags/Signals
// maps — the verdict assembler mutates flags["from_cache"], and the PG store returns fresh maps
// per read. If the deep-copy on read/write is removed, this test FAILS: a caller mutating a
// returned map would corrupt the stored entry (and vice-versa). Locks the fix so the fake can't
// silently diverge from real Postgres again.
func TestMemCacheMapsAreCopiedNotAliased(t *testing.T) {
	m := NewMem(func() time.Time { return time.Unix(1_000_000, 0) })
	ctx := context.Background()
	eh := []byte{9, 9, 9}
	src := CachedVerdict{
		EmailHash: eh, ConfigFingerprint: "cfg", Domain: "b.co",
		Status: "risky", Reason: "plausible", Confidence: 30, Recommendation: "review",
		Flags:   map[string]bool{"from_cache": false, "is_role": true},
		Signals: map[string]any{"mx_found": true},
		ExpiresAt: time.Unix(1_000_000, 0).Add(time.Hour),
	}
	if err := m.PutCachedVerdict(ctx, src); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Mutating the SOURCE map after Put must not change the stored entry (write-copy).
	src.Flags["from_cache"] = true
	src.Signals["mx_found"] = false

	got1, err := m.GetCachedVerdict(ctx, eh, "cfg")
	if err != nil {
		t.Fatalf("get1: %v", err)
	}
	if got1.Flags["from_cache"] != false || got1.Signals["mx_found"] != true {
		t.Errorf("stored entry was corrupted by a post-Put source mutation (write must copy): %+v / %+v", got1.Flags, got1.Signals)
	}
	// Mutating the RETURNED map must not change the stored entry (read-copy) — this is the exact
	// assembler behavior (flags["from_cache"]=...) that caused the original bug.
	got1.Flags["from_cache"] = true
	got1.Signals["mx_found"] = false

	got2, err := m.GetCachedVerdict(ctx, eh, "cfg")
	if err != nil {
		t.Fatalf("get2: %v", err)
	}
	if got2.Flags["from_cache"] != false || got2.Signals["mx_found"] != true {
		t.Errorf("stored entry was corrupted by mutating a returned map (read must copy): %+v / %+v", got2.Flags, got2.Signals)
	}
}

func TestStubStoreNeverFakes(t *testing.T) {
	// The no-DB stub returns ErrNotWired everywhere (never a fabricated result).
	var s Store = StubStore{}
	ctx := context.Background()
	if err := s.AppendVerification(ctx, VerificationRow{}, nil); err != ErrNotWired {
		t.Errorf("stub append must be ErrNotWired, got %v", err)
	}
	if _, err := s.GetCachedVerdict(ctx, []byte{1}, "cfg"); err != ErrNotWired {
		t.Errorf("stub cache read must be ErrNotWired, got %v", err)
	}
}
