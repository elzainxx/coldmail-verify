package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/hash"
	"github.com/mnfi/codmailling/control-plane/internal/node"
	"github.com/mnfi/codmailling/control-plane/internal/policy"
	"github.com/mnfi/codmailling/control-plane/internal/ratelimit"
	"github.com/mnfi/codmailling/control-plane/internal/router"
	"github.com/mnfi/codmailling/control-plane/internal/store"
	"github.com/mnfi/codmailling/control-plane/internal/vocab"
)

// ── Phase 4: the per-provider rate limiter degrade + jitter + in-flight lifecycle (§4) ─────────
// These reuse the fakeNode + testEgressIP + validBody helpers from smtp_stage_test.go.

// fakeLimiter is a RateLimiter that admits/denies on command and records jitter + release calls, so
// the orchestrator wiring (jitter-before-admit, degrade-on-deny, release-on-every-path) is provable
// without a real store/ratelimit.Limiter.
type fakeLimiter struct {
	admit        bool
	admitErr     error
	jitter       time.Duration
	jitterCalls  int
	releaseCalls int
	admitCalls   int
	lastBucket   string
}

func (f *fakeLimiter) Admit(_ context.Context, _, bucket string) (ratelimit.Decision, error) {
	f.admitCalls++
	f.lastBucket = bucket
	if f.admitErr != nil {
		return ratelimit.Decision{}, f.admitErr
	}
	return ratelimit.Decision{Admitted: f.admit}, nil
}
func (f *fakeLimiter) Release(_, _ string)                { f.releaseCalls++ }
func (f *fakeLimiter) JitterDelay(_ string) time.Duration { f.jitterCalls++; return f.jitter }

// p4Setup wires an eligible node PLUS a rate limiter (and a spy sleeper). Freshness/warmup disabled.
func p4Setup(t *testing.T, lim RateLimiter, body []byte) (*Orchestrator, *store.MemStore, *policy.Definition, *int) {
	t.Helper()
	h, err := hash.NewHasher([]byte("p4-pepper"))
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	mem := store.NewMem(nil)
	mem.SeedNodeHealth(store.NodeHealth{
		NodeID: "n1", Status: "active", AuditStatus: "green", Port25Open: true,
		LastAuditAt: time.Now(), LastAuditHostIP: testEgressIP,
	})
	def, _ := policy.ColdEmailDefinition()
	nodes := []router.Node{{ID: "n1", BaseURL: "http://mock", TokenRef: "n1", Enabled: true, PublicEgressIP: testEgressIP}}
	slept := 0
	o := New(Deps{
		Hasher: h, Store: mem, Resolver: corpResolver(), CacheTTL: time.Hour,
		ConfigFingerprint: "cfg_sha256:test",
		SMTP: &SMTPDeps{
			Client:      fakeNode{body: body},
			Registry:    router.New(nodes, nil),
			TokenFor:    func(string) string { return "tok" },
			Health:      mem,
			MaxAuditAge: 0,
			Limiter:     lim,
			Sleep:       func(d time.Duration) { slept += int(d) },
		},
	})
	return o, mem, def, &slept
}

// §4.6: every eligible node at its per-provider cap → degrade to cheap with
// smtp_skipped_reason="rate_limited", PROVISIONAL/non-cacheable (PB1), NEVER invalid, NEVER queued.
func TestSMTPStage_RateLimited_DegradesNonCacheable(t *testing.T) {
	lim := &fakeLimiter{admit: false} // saturated cap
	o, mem, def, _ := p4Setup(t, lim, validBody)
	resp, err := o.Verify(context.Background(), "jane@acmecorp.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Status == vocab.StatusInvalid {
		t.Fatal("§4.6 VIOLATION: a rate-capped check must NEVER be invalid")
	}
	if resp.Status != vocab.StatusRisky || resp.Reason != vocab.ReasonPlausible {
		t.Fatalf("rate-capped → cheap residue risky/plausible, got %s/%s", resp.Status, resp.Reason)
	}
	if s, _ := resp.Signals["smtp_skipped_reason"].(string); s != "rate_limited" {
		t.Fatalf("smtp_skipped_reason must be rate_limited, got %q", s)
	}
	if mem.CacheLen() != 0 {
		t.Fatalf("PB1: a rate_limited capacity-gap fallback must NOT be cached, got %d", mem.CacheLen())
	}
	if len(mem.Log) != 1 {
		t.Fatalf("the degraded verdict must still be LOGged (Door 2), got %d", len(mem.Log))
	}
	if resp.Meta.VerifierIP != nil {
		t.Fatalf("verifier_ip must be null when SMTP didn't run (rate_limited), got %v", *resp.Meta.VerifierIP)
	}
	if len(mem.Raw) != 0 {
		t.Fatal("a rate-denied check must NOT call the node (no sidecar)")
	}
	// A denied admit must NOT hold an in-flight slot — Release is not owed (Admit returned false).
	if lim.releaseCalls != 0 {
		t.Fatalf("a denied admit owes no Release, got %d releases", lim.releaseCalls)
	}
	// Jitter is applied before the admit decision (§4.2).
	if lim.jitterCalls != 1 {
		t.Fatalf("jitter must be applied once before admit, got %d", lim.jitterCalls)
	}
}

// An admitted check runs SMTP and MUST release the in-flight slot afterwards (§4.3), and the bucket
// is the honest-corporate "other:<domain>" key (§4.1/OQ-P4-2).
func TestSMTPStage_RateAdmitted_RunsAndReleases(t *testing.T) {
	lim := &fakeLimiter{admit: true, jitter: 100}
	o, mem, def, slept := p4Setup(t, lim, validBody)
	resp, err := o.Verify(context.Background(), "jane@acmecorp.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Status != vocab.StatusValid {
		t.Fatalf("an admitted check should run SMTP → valid, got %s/%s", resp.Status, resp.Reason)
	}
	if lim.releaseCalls != 1 {
		t.Fatalf("an admitted session must Release exactly once, got %d", lim.releaseCalls)
	}
	if lim.lastBucket != "other:acmecorp.com" {
		t.Fatalf("the rate bucket must be the honest-corporate other:<domain>, got %q", lim.lastBucket)
	}
	if *slept != 100 {
		t.Fatalf("the jitter delay must be slept before the node call, got %d", *slept)
	}
	if len(mem.Raw) != 1 {
		t.Fatalf("an admitted check runs the node (one sidecar), got %d", len(mem.Raw))
	}
}

// A limiter/store error must fail CLOSED (degrade to cheap, rate_limited) — never silently sprint
// past the cap (a reputation hazard). And it owes no Release (never admitted).
func TestSMTPStage_RateLimiterError_FailsClosed(t *testing.T) {
	lim := &fakeLimiter{admitErr: context.DeadlineExceeded}
	o, _, def, _ := p4Setup(t, lim, validBody)
	resp, err := o.Verify(context.Background(), "jane@acmecorp.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Status == vocab.StatusValid || resp.Status == vocab.StatusInvalid {
		t.Fatalf("a limiter error must fail closed to cheap, got %s/%s", resp.Status, resp.Reason)
	}
	if s, _ := resp.Signals["smtp_skipped_reason"].(string); s != "rate_limited" {
		t.Fatalf("a limiter error degrades as rate_limited, got %q", s)
	}
	if lim.releaseCalls != 0 {
		t.Fatalf("a limiter error owes no Release, got %d", lim.releaseCalls)
	}
}

// The in-flight slot is released even when the node call ERRORS (defer runs on every path, §4.3).
func TestSMTPStage_RateAdmitted_ReleasesOnNodeError(t *testing.T) {
	lim := &fakeLimiter{admit: true}
	h, _ := hash.NewHasher([]byte("p4-pepper"))
	mem := store.NewMem(nil)
	mem.SeedNodeHealth(store.NodeHealth{
		NodeID: "n1", Status: "active", AuditStatus: "green", Port25Open: true,
		LastAuditAt: time.Now(), LastAuditHostIP: testEgressIP,
	})
	def, _ := policy.ColdEmailDefinition()
	nodes := []router.Node{{ID: "n1", BaseURL: "http://mock", TokenRef: "n1", Enabled: true, PublicEgressIP: testEgressIP}}
	o := New(Deps{
		Hasher: h, Store: mem, Resolver: corpResolver(), CacheTTL: time.Hour, ConfigFingerprint: "cfg",
		SMTP: &SMTPDeps{
			Client:   fakeNode{err: &node.TransportError{Err: context.DeadlineExceeded}},
			Registry: router.New(nodes, nil), TokenFor: func(string) string { return "t" },
			Health: mem, Limiter: lim,
		},
	})
	resp, err := o.Verify(context.Background(), "jane@acmecorp.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Status != vocab.StatusUnknown {
		t.Fatalf("a node transport error degrades to unknown, got %s/%s", resp.Status, resp.Reason)
	}
	if lim.releaseCalls != 1 {
		t.Fatalf("the in-flight slot must be released even on a node error, got %d releases", lim.releaseCalls)
	}
}

// End-to-end with the REAL ratelimit.Limiter (store-backed): a cap of 1 start/window admits the
// first check (SMTP runs) then rate-limits the second (§4 full path, not a fake limiter).
func TestSMTPStage_RealLimiter_SecondCheckRateLimited(t *testing.T) {
	h, _ := hash.NewHasher([]byte("p4-pepper"))
	mem := store.NewMem(nil)
	mem.SeedNodeHealth(store.NodeHealth{
		NodeID: "n1", Status: "active", AuditStatus: "green", Port25Open: true,
		LastAuditAt: time.Now(), LastAuditHostIP: testEgressIP,
	})
	def, _ := policy.ColdEmailDefinition()
	nodes := []router.Node{{ID: "n1", BaseURL: "http://mock", TokenRef: "n1", Enabled: true, PublicEgressIP: testEgressIP}}
	// One start per 60s window, one concurrent — the second check in the same instant is rate-capped.
	limits := &policy.RateLimits{Default: policy.RateLimit{MaxInflight: 5, MaxStartsPerWindow: 1, WindowS: 60}}
	fixed := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	lim := ratelimit.New(ratelimit.Config{Budget: mem, Limits: limits, Now: func() time.Time { return fixed }})
	o := New(Deps{
		Hasher: h, Store: mem, Resolver: corpResolver(), CacheTTL: 0 /* no cache so both compute */, ConfigFingerprint: "cfg",
		SMTP: &SMTPDeps{
			Client: fakeNode{body: validBody}, Registry: router.New(nodes, nil),
			TokenFor: func(string) string { return "t" }, Health: mem, Limiter: lim,
			Sleep: func(time.Duration) {},
		},
	})
	r1, _ := o.Verify(context.Background(), "a@acmecorp.com", def)
	if r1.Status != vocab.StatusValid {
		t.Fatalf("first check (within budget) should run SMTP → valid, got %s/%s", r1.Status, r1.Reason)
	}
	r2, _ := o.Verify(context.Background(), "b@acmecorp.com", def) // same domain → same bucket, budget spent
	if s, _ := r2.Signals["smtp_skipped_reason"].(string); s != "rate_limited" {
		t.Fatalf("second check must be rate_limited (budget spent this window), got %q (status %s/%s)", s, r2.Status, r2.Reason)
	}
}
