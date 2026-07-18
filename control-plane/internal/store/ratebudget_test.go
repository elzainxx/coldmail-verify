package store

import (
	"context"
	"testing"
	"time"
)

// Phase-4 store tests (§4.4/§5.2/§5.4/§5.6) against the Mem fake. The token-bucket math lives in the
// SHARED applyRefill helper used by BOTH Mem and PG, so these Mem tests exercise the real decision
// logic; the PG-gated tests (postgres_test.go) prove the same behavior survives the durable round-
// trip + the append-only grants. This mirrors the P3 discipline (Mem for logic, PG for durability).

var rt0 = time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

func rateParams(cap, refill float64) RateParams {
	return RateParams{Capacity: cap, RefillPerSec: refill}
}

// A fresh bucket starts FULL: capacity=2 → two grants, then deny (empty). No refill in between.
func TestMemReserveRateToken_BurstThenDeny(t *testing.T) {
	m := NewMem(nil)
	ctx := context.Background()
	p := rateParams(2, 0) // no refill so the burst is exactly the capacity
	for i := 0; i < 2; i++ {
		d, err := m.ReserveRateToken(ctx, "n1", "other:acme.com", p, rt0)
		if err != nil || !d.Allowed {
			t.Fatalf("grant %d must be allowed (full bucket), got allowed=%v err=%v", i, d.Allowed, err)
		}
	}
	d, err := m.ReserveRateToken(ctx, "n1", "other:acme.com", p, rt0)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if d.Allowed {
		t.Fatalf("third reserve must be denied (bucket empty, no refill), got allowed with tokens=%v", d.TokensAfter)
	}
}

// The DEFINING durability property (§4.4/PM4): a "restart" (a fresh store handle sharing nothing)
// must NOT refill the bucket to full — the allowance survives. We model the durable state by
// seeding the SAME bucket state into a new store, then asserting a still-empty bucket after zero
// elapsed time. And that real elapsed time DOES refill (restart-safe, not restart-refilled).
func TestMemRateBudget_RestartDoesNotRefill(t *testing.T) {
	m := NewMem(nil)
	ctx := context.Background()
	p := rateParams(3, 1) // 1 token/sec refill
	// Drain the bucket at rt0.
	for i := 0; i < 3; i++ {
		if d, _ := m.ReserveRateToken(ctx, "n1", "b", p, rt0); !d.Allowed {
			t.Fatalf("drain grant %d should be allowed", i)
		}
	}
	// "Restart": a new store that inherits the durable spend (SeedRateBudget mirrors reloading the
	// rate_budgets row). At the SAME instant, the bucket is still empty — a restart did not refill.
	m2 := NewMem(nil)
	m2.SeedRateBudget("n1", "b", 0, rt0)
	if d, _ := m2.ReserveRateToken(ctx, "n1", "b", p, rt0); d.Allowed {
		t.Fatalf("a restart must NOT refill the allowance — reserve at t0 should be denied, got allowed (PM4 hazard)")
	}
	// After 2 real seconds, 2 tokens have refilled (1/sec) → two grants, then deny.
	later := rt0.Add(2 * time.Second)
	if d, _ := m2.ReserveRateToken(ctx, "n1", "b", p, later); !d.Allowed {
		t.Fatalf("after 2s real elapsed, a token must have refilled")
	}
	if d, _ := m2.ReserveRateToken(ctx, "n1", "b", p, later); !d.Allowed {
		t.Fatalf("after 2s real elapsed, a 2nd token must have refilled")
	}
	if d, _ := m2.ReserveRateToken(ctx, "n1", "b", p, later); d.Allowed {
		t.Fatalf("only 2 tokens refilled in 2s — the 3rd reserve must be denied")
	}
}

// Peek reports headroom WITHOUT consuming — the §3.3 eligibility check must not spend budget.
func TestMemPeekRateBudget_DoesNotConsume(t *testing.T) {
	m := NewMem(nil)
	ctx := context.Background()
	p := rateParams(1, 0)
	// Peek many times: never consumes, so a subsequent reserve still succeeds.
	for i := 0; i < 5; i++ {
		if d, _ := m.PeekRateBudget(ctx, "n1", "b", p, rt0); !d.Allowed {
			t.Fatalf("peek %d must report headroom (full bucket, peek never consumes)", i)
		}
	}
	if d, _ := m.ReserveRateToken(ctx, "n1", "b", p, rt0); !d.Allowed {
		t.Fatalf("reserve after peeks must still succeed — peek must not have spent the token")
	}
	if d, _ := m.PeekRateBudget(ctx, "n1", "b", p, rt0); d.Allowed {
		t.Fatalf("after the single token was reserved, peek must report NO headroom")
	}
}

// Capacity<=0 means no allowance (§4.6 — a provider with a zero cap never admits).
func TestMemReserveRateToken_ZeroCapacityDenies(t *testing.T) {
	m := NewMem(nil)
	if d, _ := m.ReserveRateToken(context.Background(), "n1", "b", rateParams(0, 5), rt0); d.Allowed {
		t.Fatalf("capacity<=0 must deny (no allowance), got allowed")
	}
}

// Block-rate excludes cache hits AND cheap-degrades (PB1) — the control-loop must not quarantine a
// node for verdicts it never produced. Seed a mix and assert only real SMTP attempts count.
func TestMemBlockRate_ExcludesCacheAndDegrades_PB1(t *testing.T) {
	m := NewMem(nil)
	ctx := context.Background()
	ip := "203.0.113.10"
	mk := func(reason, verifierIP string, fromCache bool) VerificationRow {
		return VerificationRow{
			ID: "x", CheckedAt: rt0, CreatedAt: rt0, Status: "unknown", Reason: reason,
			VerifierIP: verifierIP, FromCache: fromCache,
		}
	}
	// 2 real blocks + 1 real ok on node ip.
	_ = m.AppendVerification(ctx, mk("smtp_blocked", ip, false), nil)
	_ = m.AppendVerification(ctx, mk("smtp_blocked", ip, false), nil)
	_ = m.AppendVerification(ctx, mk("accepted", ip, false), nil)
	// A CACHE HIT for the same ip — must NOT count (ran no SMTP).
	_ = m.AppendVerification(ctx, mk("smtp_blocked", ip, true), nil)
	// A CHEAP-DEGRADE (verifier_ip="" — no node ran SMTP) — must NOT count.
	_ = m.AppendVerification(ctx, mk("smtp_blocked", "", false), nil)

	stats, err := m.BlockRateByVerifierIP(ctx, rt0.Add(-time.Hour))
	if err != nil {
		t.Fatalf("block-rate: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("exactly one verifier_ip should have attempts, got %d (%v)", len(stats), stats)
	}
	s := stats[0]
	if s.VerifierIP != ip {
		t.Fatalf("verifier_ip mismatch: %q", s.VerifierIP)
	}
	if s.Attempts != 3 {
		t.Fatalf("attempts must exclude the cache hit + the degrade → 3 real attempts, got %d", s.Attempts)
	}
	if s.Blocked != 2 {
		t.Fatalf("blocked must be the 2 real smtp_blocked (not the cached one), got %d", s.Blocked)
	}
}

// greylisted is NOT counted as a block (OQ-P4-6 — expected transient; counting it would quarantine
// a node for doing its job). Timeouts count only as the SECONDARY signal, not Blocked.
func TestMemBlockRate_GreylistNotCounted_TimeoutSecondary(t *testing.T) {
	m := NewMem(nil)
	ctx := context.Background()
	ip := "203.0.113.11"
	mk := func(reason string) VerificationRow {
		return VerificationRow{ID: "x", CheckedAt: rt0, CreatedAt: rt0, Status: "unknown", Reason: reason, VerifierIP: ip}
	}
	_ = m.AppendVerification(ctx, mk("greylisted"), nil)
	_ = m.AppendVerification(ctx, mk("smtp_timeout"), nil)
	_ = m.AppendVerification(ctx, mk("connection_failed"), nil)
	_ = m.AppendVerification(ctx, mk("smtp_blocked"), nil)
	stats, _ := m.BlockRateByVerifierIP(ctx, rt0.Add(-time.Hour))
	s := stats[0]
	if s.Attempts != 4 {
		t.Fatalf("all 4 real attempts count in the denominator, got %d", s.Attempts)
	}
	if s.Blocked != 1 {
		t.Fatalf("only smtp_blocked is a block (greylist NOT counted), got %d", s.Blocked)
	}
	if s.Timeouts != 2 {
		t.Fatalf("timeout+connection_failed are the secondary signal → 2, got %d", s.Timeouts)
	}
}

// A window boundary excludes older rows (the watcher looks at a rolling window, §5.2).
func TestMemBlockRate_WindowBoundary(t *testing.T) {
	m := NewMem(nil)
	ctx := context.Background()
	ip := "203.0.113.12"
	old := VerificationRow{ID: "o", CheckedAt: rt0.Add(-2 * time.Hour), CreatedAt: rt0, Status: "unknown", Reason: "smtp_blocked", VerifierIP: ip}
	fresh := VerificationRow{ID: "f", CheckedAt: rt0, CreatedAt: rt0, Status: "unknown", Reason: "smtp_blocked", VerifierIP: ip}
	_ = m.AppendVerification(ctx, old, nil)
	_ = m.AppendVerification(ctx, fresh, nil)
	stats, _ := m.BlockRateByVerifierIP(ctx, rt0.Add(-time.Hour)) // window = last hour → excludes `old`
	if len(stats) != 1 || stats[0].Attempts != 1 {
		t.Fatalf("only the in-window attempt should count, got %v", stats)
	}
}

// SetNodeStatus writes only the lifecycle; an unregistered node is a no-op (PG parity), and the
// 'recovering' value round-trips (0008 CHECK widening).
func TestMemSetNodeStatus_RegisteredAndRecovering(t *testing.T) {
	m := NewMem(nil)
	ctx := context.Background()
	// Unregistered → no-op, still not known.
	if err := m.SetNodeStatus(ctx, "ghost", "recovering"); err != nil {
		t.Fatalf("SetNodeStatus on unregistered: %v", err)
	}
	if _, known, _ := m.GetNodeHealth(ctx, "ghost"); known {
		t.Fatalf("SetNodeStatus must not create a nodes row (PG parity)")
	}
	// Registered → status moves to recovering without clobbering port25/audit.
	_ = m.UpsertNodeState(ctx, "n1", "http://box1", "quarantined", true)
	if err := m.SetNodeStatus(ctx, "n1", "recovering"); err != nil {
		t.Fatalf("SetNodeStatus: %v", err)
	}
	h, known, _ := m.GetNodeHealth(ctx, "n1")
	if !known || h.Status != "recovering" {
		t.Fatalf("status should be recovering, got known=%v status=%q", known, h.Status)
	}
	if !h.Port25Open {
		t.Fatalf("SetNodeStatus must NOT touch port25_open (separate write path)")
	}
}

// ResetWarmup restarts the ramp at count=0 (§5.4/PM3): a recovered node re-warms from a low start
// even if its window was previously at cap.
func TestMemResetWarmup_ReopensRamp(t *testing.T) {
	m := NewMem(nil)
	ctx := context.Background()
	m.SeedWarmup("n1", rt0, 50, 50) // window at cap → next reserve would be denied
	if d, _ := m.ReserveWarmupSlot(ctx, "n1", rt0, 50); d.Allowed {
		t.Fatalf("precondition: a full window must deny")
	}
	if err := m.ResetWarmup(ctx, "n1", rt0, 10); err != nil {
		t.Fatalf("ResetWarmup: %v", err)
	}
	d, _ := m.ReserveWarmupSlot(ctx, "n1", rt0, 10)
	if !d.Allowed || d.Count != 1 {
		t.Fatalf("after reset the ramp reopens at a low cap: want allowed count=1, got allowed=%v count=%d", d.Allowed, d.Count)
	}
}
