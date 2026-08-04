package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/store"
)

var lt0 = time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

// fixedNow + a MemStore-backed Budget give a deterministic limiter (no wall-clock, no rand).
func newLimiter(t *testing.T, limits *policy.RateLimits) (*Limiter, *store.MemStore) {
	t.Helper()
	mem := store.NewMem(func() time.Time { return lt0 })
	l := New(Config{
		Budget: mem,
		Limits: limits,
		Now:    func() time.Time { return lt0 },
		Jitter: func(min, max time.Duration) time.Duration { return min }, // deterministic
	})
	return l, mem
}

func limits(inflight, starts, window int) *policy.RateLimits {
	return &policy.RateLimits{Default: policy.RateLimit{
		MaxInflight: inflight, MaxStartsPerWindow: starts, WindowS: window,
		StartDelayJitterMS: []int{50, 500},
	}}
}

// An inert limiter (nil limits) always admits and never touches state — the honest P1/P2/P3 posture.
func TestAdmit_InertWhenNoLimits(t *testing.T) {
	l := New(Config{Now: func() time.Time { return lt0 }})
	d, err := l.Admit(context.Background(), "n1", "other")
	if err != nil || !d.Admitted {
		t.Fatalf("inert limiter must admit, got admitted=%v err=%v", d.Admitted, err)
	}
	if got := l.Inflight("n1", "other"); got != 0 {
		t.Fatalf("inert limiter holds no in-flight slot, got %d", got)
	}
}

// The concurrency cap bounds simultaneous open sessions; Release frees a slot. max_inflight=2 with a
// large rate budget → 2 admits, 3rd denied on inflight, then Release lets a 4th through.
func TestAdmit_ConcurrencyCapAndRelease(t *testing.T) {
	l, _ := newLimiter(t, limits(2, 1000, 60))
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if d, _ := l.Admit(ctx, "n1", "other:acme.com"); !d.Admitted {
			t.Fatalf("admit %d must succeed under inflight cap", i)
		}
	}
	if d, _ := l.Admit(ctx, "n1", "other:acme.com"); d.Admitted || d.Reason != reasonInflightCap {
		t.Fatalf("3rd concurrent admit must be denied on inflight cap, got admitted=%v reason=%q", d.Admitted, d.Reason)
	}
	// Close one session → a slot frees → next admit succeeds.
	l.Release("n1", "other:acme.com")
	if d, _ := l.Admit(ctx, "n1", "other:acme.com"); !d.Admitted {
		t.Fatalf("after Release, an admit must succeed again")
	}
}

// The rate budget bounds session STARTS independent of concurrency: a huge inflight cap but a small
// per-window budget → only `starts` admits, then rate_budget denial. Release does NOT refund a token
// (a start is spent regardless of how the session ends — the point of pacing the START).
func TestAdmit_RateBudgetCapIndependentOfInflight(t *testing.T) {
	l, _ := newLimiter(t, limits(1000, 2, 60)) // inflight effectively unbounded; only 2 starts/window
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if d, _ := l.Admit(ctx, "n1", "other:x.com"); !d.Admitted {
			t.Fatalf("start %d must be within the rate budget", i)
		}
		l.Release("n1", "other:x.com") // free the inflight slot; the token is still spent
	}
	if d, _ := l.Admit(ctx, "n1", "other:x.com"); d.Admitted || d.Reason != reasonRateBudget {
		t.Fatalf("3rd start must be denied on the rate budget, got admitted=%v reason=%q", d.Admitted, d.Reason)
	}
}

// A rate-budget denial must NOT leak an in-flight slot (the optimistic reservation is given back).
// With inflight=1 and budget=0-available (drained), a denied Admit leaves in-flight at 0 so a later
// (post-refill) Admit can still use the single concurrency slot.
func TestAdmit_RateDenialDoesNotLeakInflight(t *testing.T) {
	l, mem := newLimiter(t, limits(1, 1, 60))
	ctx := context.Background()
	// Spend the only token.
	if d, _ := l.Admit(ctx, "n1", "other:y.com"); !d.Admitted {
		t.Fatalf("first admit must succeed")
	}
	l.Release("n1", "other:y.com")
	// Budget now empty at lt0 → this admit is denied on rate, must not hold an inflight slot.
	if d, _ := l.Admit(ctx, "n1", "other:y.com"); d.Admitted {
		t.Fatalf("expected rate denial")
	}
	if got := l.Inflight("n1", "other:y.com"); got != 0 {
		t.Fatalf("a rate denial must not leak an in-flight slot, got %d", got)
	}
	// Prove the token refilled durably after real time (via the store), then admit succeeds.
	_ = mem // budget lives in mem; refill is time-driven
	later := New(Config{Budget: mem, Limits: limits(1, 1, 60), Now: func() time.Time { return lt0.Add(90 * time.Second) }, Jitter: func(a, b time.Duration) time.Duration { return a }})
	if d, _ := later.Admit(ctx, "n1", "other:y.com"); !d.Admitted {
		t.Fatalf("after a full window elapsed, the start must be admitted again")
	}
}

// HasHeadroom is a PEEK: it reports headroom without consuming, so repeated checks don't drain the
// budget or hold slots.
func TestHasHeadroom_PeekDoesNotConsume(t *testing.T) {
	l, _ := newLimiter(t, limits(1, 1, 60))
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		ok, err := l.HasHeadroom(ctx, "n1", "other:z.com")
		if err != nil || !ok {
			t.Fatalf("peek %d must report headroom (full), got ok=%v err=%v", i, ok, err)
		}
	}
	// Still admits after the peeks (nothing consumed).
	if d, _ := l.Admit(ctx, "n1", "other:z.com"); !d.Admitted {
		t.Fatalf("admit after peeks must succeed")
	}
	// Now the single token is spent + the single inflight slot held → no headroom.
	ok, _ := l.HasHeadroom(ctx, "n1", "other:z.com")
	if ok {
		t.Fatalf("after the only slot+token are used, HasHeadroom must be false")
	}
}

// Per-(node × provider) isolation: node n1 hitting its cap does NOT consume n2's budget/slots (§7.2
// — per-node budgets are independent, the basis for the least-inflight fallback at N>1).
func TestAdmit_PerNodeIsolation(t *testing.T) {
	l, _ := newLimiter(t, limits(1, 1, 60))
	ctx := context.Background()
	if d, _ := l.Admit(ctx, "n1", "other:acme.com"); !d.Admitted {
		t.Fatalf("n1 first admit")
	}
	if d, _ := l.Admit(ctx, "n1", "other:acme.com"); d.Admitted {
		t.Fatalf("n1 second admit must be capped")
	}
	// n2 is untouched — full headroom.
	if d, _ := l.Admit(ctx, "n2", "other:acme.com"); !d.Admitted {
		t.Fatalf("n2 must have its OWN independent budget/slot, got denied")
	}
}

// Different providers on the same node are independent (a node busy with provider A still has room
// for provider B — PM2, the reason least-inflight ranks per-(node,provider)).
func TestAdmit_PerProviderIsolationSameNode(t *testing.T) {
	l, _ := newLimiter(t, limits(1, 1, 60))
	ctx := context.Background()
	if d, _ := l.Admit(ctx, "n1", "other:a.com"); !d.Admitted {
		t.Fatalf("provider a first admit")
	}
	if d, _ := l.Admit(ctx, "n1", "other:a.com"); d.Admitted {
		t.Fatalf("provider a is capped")
	}
	if d, _ := l.Admit(ctx, "n1", "other:b.com"); !d.Admitted {
		t.Fatalf("provider b on the same node must be independent, got denied")
	}
}

func TestJitterDelay_WithinSpread(t *testing.T) {
	// Real (random) jitter must stay within [min, max].
	l := New(Config{Limits: limits(2, 30, 60), Now: func() time.Time { return lt0 }})
	for i := 0; i < 50; i++ {
		d := l.JitterDelay("other:acme.com")
		if d < 50*time.Millisecond || d >= 500*time.Millisecond {
			t.Fatalf("jitter %v out of [50ms,500ms)", d)
		}
	}
	// Inert (no limits) → no jitter.
	if got := New(Config{}).JitterDelay("other"); got != 0 {
		t.Fatalf("no-limits jitter must be 0, got %v", got)
	}
}

func TestProviderBucketAndBucketForDomain(t *testing.T) {
	cases := []struct{ provider, domain, want string }{
		{"google", "gmail.com", "google"},
		{"o365", "acme.com", "o365"},
		{"yahoo", "yahoo.com", "yahoo"},
		{"other", "acme.com", "other:acme.com"},
		{"self", "corp.example", "other:corp.example"},
		{"", "x.com", "other:x.com"},
	}
	for _, c := range cases {
		if got := BucketForDomain(c.provider, c.domain); got != c.want {
			t.Fatalf("BucketForDomain(%q,%q)=%q want %q", c.provider, c.domain, got, c.want)
		}
	}
	// ProviderBucket collapses a bare class; an already-composed "other:x" passes through.
	if got := ProviderBucket("other:acme.com"); got != "other:acme.com" {
		t.Fatalf("ProviderBucket passthrough: %q", got)
	}
	if got := ProviderBucket("google"); got != "google" {
		t.Fatalf("ProviderBucket(google)=%q", got)
	}
}

// compile-time: the store satisfies Budget (the durable §4.4 contract).
var _ Budget = (*store.MemStore)(nil)
