package queue

import (
	"context"
	"testing"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/orchestrator"
	"github.com/mnfi/codmailling/control-plane/internal/policy"
	"github.com/mnfi/codmailling/control-plane/internal/store"
	"github.com/mnfi/codmailling/control-plane/internal/vocab"
)

// ── Fakes ────────────────────────────────────────────────────────────────────────────────────

// byAttemptComputer is the GAP-35 test harness: since the real greylist reject-then-accept
// round-trip is un-mockable (P2 GAP-12), we drive the LOOP MECHANICS by returning outcomes keyed
// on how many times a given address has been computed. This tests requeue/ceiling/settle — NOT the
// provider's actual 450-then-250 (validated only at rollout, honestly deferred).
type byAttemptComputer struct {
	calls    map[string]int
	sequence []orchestrator.QueueClass // the class to return on call 0,1,2,… for every address
	lastAddr string
}

func (c *byAttemptComputer) ComputeForQueue(_ context.Context, rawEmail string, _ *policy.Definition) (orchestrator.QueueOutcome, error) {
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	n := c.calls[rawEmail]
	c.calls[rawEmail]++
	c.lastAddr = rawEmail
	class := orchestrator.QSettled
	if n < len(c.sequence) {
		class = c.sequence[n]
	} else if len(c.sequence) > 0 {
		class = c.sequence[len(c.sequence)-1]
	}
	// Build a plausible settled verdict row for the terminal case. For a greylist class the drainer
	// requeues and ignores the row; for the exhausted case it settles this as unknown/greylisted.
	reason := vocab.ReasonAccepted
	status := vocab.StatusValid
	switch class {
	case orchestrator.QGreylisted:
		reason, status = vocab.ReasonGreylisted, vocab.StatusUnknown
	case orchestrator.QRateLimited:
		reason, status = vocab.Reason("plausible"), vocab.StatusRisky
	}
	// The real ComputeForQueue assigns the LOG row id (so SettleWorkItem stamps verification_id in
	// one tx). The fake must too, or verification_id would be empty on settle. Use a stable-ish id
	// derived from the call (the harness has no clock; a fixed valid UUIDv7 suffices per call).
	row := store.VerificationRow{
		ID: "018f0000-0000-7000-8000-0000000009" + string(rune('a'+n%20)) + "0",
		RequestID: "018f0000-0000-7000-8000-000000000fff",
		CheckedAt: time.Unix(1, 0), CreatedAt: time.Unix(1, 0),
		Status: string(status), Reason: string(reason), Confidence: 50, Recommendation: "review",
		PolicyKey: "cold-email", PolicyVersion: 1, APIVersion: "v1", ConfigFingerprint: "cfg",
	}
	return orchestrator.QueueOutcome{Class: class, LastReason: string(reason), Row: row}, nil
}

type fakePolicy struct{}

func (fakePolicy) Resolve(_ context.Context, _ string, _ int) (*policy.Definition, error) {
	return &policy.Definition{Key: "cold-email", Version: 1}, nil
}

// plainCipher is a test Decryptor: the "ciphertext" is just the plaintext bytes (real crypto is
// unit-tested in addrcipher_test.go; here we only need the plaintext back).
type plainCipher struct{}

func (plainCipher) Open(blob []byte, _ []byte) (string, error) { return string(blob), nil }

func testBackoff() Backoff {
	return Backoff{Base: 5 * time.Minute, Factor: 2, Cap: time.Hour, RateWait: 30 * time.Second}
}

// enqueue helper: put a ready item holding `addr` as plaintext (plainCipher round-trips it).
// createdAt anchors the age ceiling (§4.5); pass the test's clock so the item isn't born already
// past max_retry_age (a subtle bug if CreatedAt were left epoch).
func enqueue(t *testing.T, mem *store.MemStore, id, idem, domain, addr, jobID string, createdAt time.Time) {
	t.Helper()
	item := store.WorkItemRow{
		ID: id, IdempotencyKey: []byte(idem), Domain: domain, EmailHash: []byte("h-" + idem),
		PolicyKey: "cold-email", PolicyVersion: 1, ConfigFingerprint: "cfg", Status: "ready",
		JobID: jobID, NotBefore: createdAt, CreatedAt: createdAt, AddrCiphertext: []byte(addr),
	}
	if _, _, err := mem.EnqueueWorkItem(context.Background(), item); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
}

// ── Tests ────────────────────────────────────────────────────────────────────────────────────

// TestDrainer_GreylistRetriesThenSettlesUnknownNeverInvalid is the §4.5 / GAP-35 loop-mechanics
// test: a check that greylists on every attempt is requeued up to max_attempts, then settles as
// unknown/greylisted — NEVER invalid, NEVER a fabricated valid. We advance the clock past each
// backoff so the item is re-ready, and assert the terminal verdict + status='dead' work-item.
func TestDrainer_GreylistRetriesThenSettlesUnknownNeverInvalid(t *testing.T) {
	ctx := context.Background()
	clock := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	mem := store.NewMem(func() time.Time { return clock })
	comp := &byAttemptComputer{sequence: []orchestrator.QueueClass{
		orchestrator.QGreylisted, orchestrator.QGreylisted, orchestrator.QGreylisted, orchestrator.QGreylisted,
	}}
	dr, err := NewDrainer(Deps{
		Store: mem, Compute: comp, Policy: fakePolicy{}, Cipher: plainCipher{},
		Backoff: testBackoff(), Now: func() time.Time { return clock },
		WorkerID: "w1", LeaseTTL: time.Minute, MaxSMTPDeadline: 10 * time.Second,
		MaxAttempts: 3, MaxRetryAge: 24 * time.Hour, BatchSize: 5, ReclaimEvery: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewDrainer: %v", err)
	}
	enqueue(t, mem, "018f0000-0000-7000-8000-000000000101", "g1", "ex.com", "jane@ex.com", "", clock)

	// Drain repeatedly, advancing the clock past each backoff, until the item is terminal.
	for i := 0; i < 10; i++ {
		dr.TickOnce(ctx)
		w, _, _ := mem.GetWorkItem(ctx, "018f0000-0000-7000-8000-000000000101")
		if w.Status == "dead" || w.Status == "settled" {
			break
		}
		clock = clock.Add(2 * time.Hour) // jump past any backoff so it's ready next tick
	}

	w, _, _ := mem.GetWorkItem(ctx, "018f0000-0000-7000-8000-000000000101")
	if w.Status != "dead" {
		t.Fatalf("after max_attempts the item must be dead (settled unknown), got status=%s attempt=%d", w.Status, w.Attempt)
	}
	// Exactly ONE LOG row, and it is unknown/greylisted — never invalid, never valid (§4.5).
	if len(mem.Log) != 1 {
		t.Fatalf("retry-exhausted settle must log EXACTLY one verdict, got %d", len(mem.Log))
	}
	got := mem.Log[0]
	if got.Status == "invalid" {
		t.Fatal("PRINCIPLE VIOLATED: retry-exhausted greylist settled INVALID (retrying must not manufacture certainty)")
	}
	if got.Status != "unknown" || got.Reason != "greylisted" {
		t.Fatalf("retry-exhausted must settle unknown/greylisted, got %s/%s", got.Status, got.Reason)
	}
	// max_attempts=3 means 3 attempts are MADE: attempt 0 requeues (→1), attempt 1 requeues (→2),
	// attempt 2 is the 3rd and Exhausted(2+1 >= 3) fires → it SETTLES with attempt=2 recorded (the
	// settle path doesn't bump). So the item made exactly max_attempts tries and its stored count is
	// max_attempts-1. Assert we actually retried (not a first-attempt give-up).
	if w.Attempt != 2 {
		t.Fatalf("with max_attempts=3 the item settles on the 3rd try with attempt=2 recorded, got %d", w.Attempt)
	}
}

// TestDrainer_QueuedRateLimitedWaitsNotDegrades proves the §4.6 seam: a QUEUED item that hits
// rate_limited is REQUEUED (waiting_retry, short backoff, NO attempt bump) — it WAITS for capacity,
// it does NOT settle a degraded cheap verdict. (The sync path degrades; the queue absorbs — proven
// unchanged in the orchestrator tests.) Once capacity frees (the stub returns QSettled), it settles.
func TestDrainer_QueuedRateLimitedWaitsNotDegrades(t *testing.T) {
	ctx := context.Background()
	clock := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	mem := store.NewMem(func() time.Time { return clock })
	// rate_limited on the first attempt, then capacity frees → settled.
	comp := &byAttemptComputer{sequence: []orchestrator.QueueClass{orchestrator.QRateLimited, orchestrator.QSettled}}
	dr, _ := NewDrainer(Deps{
		Store: mem, Compute: comp, Policy: fakePolicy{}, Cipher: plainCipher{},
		Backoff: testBackoff(), Now: func() time.Time { return clock },
		WorkerID: "w1", LeaseTTL: time.Minute, MaxSMTPDeadline: 10 * time.Second,
		MaxAttempts: 3, MaxRetryAge: 24 * time.Hour, BatchSize: 5, ReclaimEvery: time.Hour,
	})
	enqueue(t, mem, "018f0000-0000-7000-8000-000000000201", "r1", "ex.com", "bob@ex.com", "", clock)

	// First tick: rate_limited → waiting_retry, no attempt bump, NO LOG row (it waited).
	dr.TickOnce(ctx)
	w, _, _ := mem.GetWorkItem(ctx, "018f0000-0000-7000-8000-000000000201")
	if w.Status != "waiting_retry" {
		t.Fatalf("queued rate_limited must WAIT (waiting_retry), got %s", w.Status)
	}
	if w.Attempt != 0 {
		t.Fatalf("a capacity wait must NOT bump attempt (§4.6/OQ-P5-7), got attempt=%d", w.Attempt)
	}
	if len(mem.Log) != 0 {
		t.Fatalf("a queued rate_limited wait must NOT log a (degraded) verdict, got %d LOG rows", len(mem.Log))
	}

	// Advance past the short rate-wait backoff; next tick capacity has freed → settles.
	clock = clock.Add(time.Minute)
	dr.TickOnce(ctx)
	w, _, _ = mem.GetWorkItem(ctx, "018f0000-0000-7000-8000-000000000201")
	if w.Status != "settled" {
		t.Fatalf("once capacity frees the item must settle, got %s", w.Status)
	}
	if len(mem.Log) != 1 {
		t.Fatalf("settle must log exactly one verdict, got %d", len(mem.Log))
	}
}

// TestDrainer_SettledPurgesPlaintextAndLogsOnce proves the happy path: a check that settles on the
// first attempt logs exactly one verdict and the work-item's ciphertext is purged (via the store's
// settlement tx). This exercises the drainer→SettleWorkItem wiring end-to-end.
func TestDrainer_SettledPurgesPlaintextAndLogsOnce(t *testing.T) {
	ctx := context.Background()
	clock := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	mem := store.NewMem(func() time.Time { return clock })
	comp := &byAttemptComputer{sequence: []orchestrator.QueueClass{orchestrator.QSettled}}
	dr, _ := NewDrainer(Deps{
		Store: mem, Compute: comp, Policy: fakePolicy{}, Cipher: plainCipher{},
		Backoff: testBackoff(), Now: func() time.Time { return clock },
		WorkerID: "w1", LeaseTTL: time.Minute, MaxSMTPDeadline: 10 * time.Second,
		MaxAttempts: 3, MaxRetryAge: 24 * time.Hour, BatchSize: 5, ReclaimEvery: time.Hour,
	})
	enqueue(t, mem, "018f0000-0000-7000-8000-000000000301", "s1", "ex.com", "ok@ex.com", "", clock)
	dr.TickOnce(ctx)
	w, _, _ := mem.GetWorkItem(ctx, "018f0000-0000-7000-8000-000000000301")
	if w.Status != "settled" || w.VerificationID == "" {
		t.Fatalf("first-attempt settle must be settled with a verification_id, got %s / %q", w.Status, w.VerificationID)
	}
	if len(w.AddrCiphertext) != 0 {
		t.Fatal("plaintext (ciphertext) must be purged on settle")
	}
	if len(mem.Log) != 1 {
		t.Fatalf("exactly one LOG row, got %d", len(mem.Log))
	}
}

// TestNewDrainer_RejectsShortLease is the GAP-37 fail-fast: a lease TTL not exceeding the max SMTP
// deadline is rejected at construction (a too-short lease reintroduces the slow-drainer race the
// CAS closes). FAIL-VERIFY of the invariant: without this guard, a live attempt could lose its
// lease mid-SMTP and a reclaim would double-run it.
func TestNewDrainer_RejectsShortLease(t *testing.T) {
	_, err := NewDrainer(Deps{
		Store: store.NewMem(nil), Compute: &byAttemptComputer{}, Policy: fakePolicy{}, Cipher: plainCipher{},
		WorkerID: "w1", LeaseTTL: 10 * time.Second, MaxSMTPDeadline: 10 * time.Second, // TTL == deadline (not >)
		MaxAttempts: 3,
	})
	if err != ErrLeaseTTLTooShort {
		t.Fatalf("lease TTL <= max SMTP deadline must be rejected (GAP-37), got %v", err)
	}
	// TTL strictly greater is accepted.
	if _, err := NewDrainer(Deps{
		Store: store.NewMem(nil), Compute: &byAttemptComputer{}, Policy: fakePolicy{}, Cipher: plainCipher{},
		WorkerID: "w1", LeaseTTL: 11 * time.Second, MaxSMTPDeadline: 10 * time.Second, MaxAttempts: 3,
	}); err != nil {
		t.Fatalf("lease TTL > max SMTP deadline must be accepted, got %v", err)
	}
}

// TestDrainer_MaxRetryAgeSettlesEvenIfAttemptsRemain proves the §4.5 age ceiling: an item older
// than max_retry_age settles unknown even with attempts left (an item can't linger forever).
func TestDrainer_MaxRetryAgeSettlesEvenIfAttemptsRemain(t *testing.T) {
	ctx := context.Background()
	created := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	clock := created
	mem := store.NewMem(func() time.Time { return clock })
	comp := &byAttemptComputer{sequence: []orchestrator.QueueClass{orchestrator.QGreylisted}}
	dr, _ := NewDrainer(Deps{
		Store: mem, Compute: comp, Policy: fakePolicy{}, Cipher: plainCipher{},
		Backoff: testBackoff(), Now: func() time.Time { return clock },
		WorkerID: "w1", LeaseTTL: time.Minute, MaxSMTPDeadline: 10 * time.Second,
		MaxAttempts: 100, MaxRetryAge: time.Hour, BatchSize: 5, ReclaimEvery: time.Hour,
	})
	enqueue(t, mem, "018f0000-0000-7000-8000-000000000401", "a1", "ex.com", "old@ex.com", "", created)
	// Jump the clock well past max_retry_age, then drain once: it must settle (dead), not requeue.
	clock = created.Add(2 * time.Hour)
	dr.TickOnce(ctx)
	w, _, _ := mem.GetWorkItem(ctx, "018f0000-0000-7000-8000-000000000401")
	if w.Status != "dead" {
		t.Fatalf("an item past max_retry_age must settle (dead), got %s attempt=%d", w.Status, w.Attempt)
	}
	if mem.Log[0].Status == "invalid" {
		t.Fatal("age-exhausted greylist must NOT settle invalid")
	}
}
