package main

// Wiring tests for ADR-0017. The store tests prove the maintenance tick WORKS; these prove the
// control-plane actually RUNS it. That is a separate failure mode and the one that matters: removing
// the single `startRawPartitionMaintenance` call from buildAPI leaves every store test green and
// brings the cliff straight back.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/config"
	"github.com/coldmail-verify/control-plane/internal/hash"
	"github.com/coldmail-verify/control-plane/internal/secrets"
	"github.com/coldmail-verify/control-plane/internal/testpg"
)

type recordingPartitioner struct {
	mu    sync.Mutex
	calls []int
	first chan struct{}
	once  sync.Once
}

func newRecorder() *recordingPartitioner { return &recordingPartitioner{first: make(chan struct{})} }

func (r *recordingPartitioner) EnsureRawPartitions(_ context.Context, n int) (int, error) {
	r.mu.Lock()
	r.calls = append(r.calls, n)
	r.mu.Unlock()
	r.once.Do(func() { close(r.first) })
	return 0, nil
}

func (r *recordingPartitioner) snapshot() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.calls...)
}

// The loop must tick at STARTUP (not after the first interval) with N taken from config.
func TestStartRawPartitionMaintenance_TicksAtStartupWithConfiguredN(t *testing.T) {
	for _, n := range []int{5, 7} { // two values: the second is the control that N is read, not constant
		cfg := config.Defaults()
		cfg.Dials.RawPartitionMonthsAhead = n
		cfg.Dials.RawPartitionMaintainIntervalSeconds = 3600 // far longer than the test: only the startup tick can satisfy it
		rec := newRecorder()
		ctx, cancel := context.WithCancel(context.Background())

		startRawPartitionMaintenance(ctx, cfg, rec)

		select {
		case <-rec.first:
		case <-time.After(3 * time.Second):
			cancel()
			t.Fatalf("n=%d: no startup tick within 3s", n)
		}
		cancel()
		if got := rec.snapshot(); len(got) < 1 || got[0] != n {
			t.Errorf("startup tick used monthsAhead=%v, want %d (read from the config dial)", got, n)
		}
	}
}

// The real wiring, end to end: buildAPI against a real (scratch, aged) database must leave it with N
// months of headroom, and the SAME database must visibly lack that headroom beforehand — otherwise
// the assertion afterwards could pass for the wrong reason.
func TestBuildAPI_StartsPartitionMaintenance(t *testing.T) {
	dsn := testpg.Scratch(t, 0)
	ctx := context.Background()
	const n = 2

	pepper := make([]byte, 32)
	for i := range pepper {
		pepper[i] = byte(i + 1)
	}
	hasher, err := hash.NewHasher(pepper)
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	sec, err := secrets.Load(secrets.EnvSource{}, false)
	if err != nil {
		t.Fatalf("secrets: %v", err)
	}

	cfg := config.Defaults()
	cfg.DatabaseURL = dsn
	cfg.Dials.RawPartitionMonthsAhead = n

	now := time.Now().UTC()
	before, err := testpg.Parts(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if testpg.HeadroomError(before, now, n) == nil {
		t.Fatal("precondition: a freshly migrated database has only the CURRENT month and must fail the headroom guard")
	}

	appCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	_ = buildAPI(appCtx, cfg, sec, hasher)

	deadline := time.Now().Add(20 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		parts, err := testpg.Parts(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		if last = testpg.HeadroomError(parts, now, n); last == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("buildAPI did not start partition maintenance: headroom still missing after 20s: %v", last)
}
