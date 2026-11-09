package external

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// This file: the CONTROLLABLE MOCK vendor (GAP-47 / §14). The vendor's REAL accuracy on the exact
// residue we send it (catch-all, big-provider) is untestable pre-integration — like P2's un-mockable
// greylist (GAP-12/GAP-35). So we do NOT fake accuracy; we test the MECHANICS (routing, gates,
// anti-laundering, spend-once, breaker, normalization, idempotency) against a fake that returns
// valid/invalid/unknown/error/timeout/rate-limited ON COMMAND. Anything needing a real vendor stays
// PENDING-REAL-VENDOR (history/phase-6-status.md), never faked green.
//
// The mock also COUNTS calls and records the idempotency keys it saw — that count is what the
// spend-once fail-verify test asserts (a P5-reclaim double-attempt must call the vendor at most once
// for a given key, or the CAS didn't hold — GAP-42).

// MockOutcome is a programmed answer for the mock (§8 mock contract). It maps directly onto what a
// real adapter would return AFTER normalization, plus the two failure modes (error, timeout).
type MockOutcome struct {
	Result        vocab.Status // normalized result the mock returns (valid|invalid|risky|unknown)
	StatusRaw     string       // vendor's raw status string (for provenance assertions)
	ConfidenceRaw int          // vendor's own confidence (0 => none)
	CostUnits     int          // cost of this call (default 1 if zero — every real call costs)
	Err           error        // non-nil => Verify returns this error (vendor-down/timeout/rate-limited)
}

// Sentinel errors the mock can return on command (the failure modes the kickoff enumerates). They
// let a test assert the breaker trips / the stage degrades to keep_residue on each.
var (
	ErrVendorDown    = errors.New("external: mock vendor down")
	ErrVendorTimeout = errors.New("external: mock vendor timeout")
	ErrRateLimited   = errors.New("external: mock vendor rate-limited")
)

// MockVendor is a race-safe controllable fake Vendor. Program it with Default (the outcome for any
// address) and/or ByAddress (per-address overrides). It records CallCount + the idempotency keys it
// was called with, so the spend-once test can assert "called at most once per key under a reclaim".
type MockVendor struct {
	mu        sync.Mutex
	name      string
	Default   MockOutcome
	ByAddress map[string]MockOutcome

	CallCount  int              // total Verify calls (spend-once assertion)
	KeysSeen   map[string]int   // idempotencyKey -> times called (dedup/idempotency assertion)
	AddrsSeen  []string         // plaintext addresses passed (privacy assertion: address-only egress)
}

// NewMockVendor builds a mock returning `def` for every address unless overridden per-address.
func NewMockVendor(name string, def MockOutcome) *MockVendor {
	if name == "" {
		name = "mock"
	}
	return &MockVendor{name: name, Default: def, ByAddress: map[string]MockOutcome{}, KeysSeen: map[string]int{}}
}

func (m *MockVendor) Name() string { return m.name }

// Set programs a per-address outcome (overrides Default for that exact plaintext address).
func (m *MockVendor) Set(addr string, out MockOutcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ByAddress[addr] = out
}

// Verify returns the programmed outcome, recording the call for spend/idempotency assertions. On a
// programmed Err it returns that error (the caller degrades to keep_residue + trips the breaker) and
// does NOT bill (a failed call produces no verdict; the mock still counts it so the error-rate
// window is measurable). A successful call returns a VendorResult with a synthetic RawJSON blob.
func (m *MockVendor) Verify(_ context.Context, plaintextAddr, idempotencyKey string) (VendorResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CallCount++
	m.KeysSeen[idempotencyKey]++
	m.AddrsSeen = append(m.AddrsSeen, plaintextAddr)

	out := m.Default
	if o, ok := m.ByAddress[plaintextAddr]; ok {
		out = o
	}
	if out.Err != nil {
		return VendorResult{}, out.Err
	}
	cost := out.CostUnits
	if cost == 0 {
		cost = 1 // every real vendor call costs at least one unit (spend accounting)
	}
	raw, _ := json.Marshal(map[string]any{
		"status": out.StatusRaw, "confidence": out.ConfidenceRaw, "vendor": m.name,
	})
	return VendorResult{
		Result:        out.Result,
		StatusRaw:     out.StatusRaw,
		ConfidenceRaw: out.ConfidenceRaw,
		RequestID:     "mock-" + idempotencyKey, // deterministic (no wall-clock/rand) — mirrors a vendor idempotency key
		CostUnits:     cost,
		LatencyMS:     0,
		RawJSON:       raw,
	}, nil
}

// Calls returns the total Verify call count (spend-once assertion helper).
func (m *MockVendor) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.CallCount
}
