package metrics

import (
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/store"
)

var mt0 = time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

func row(status, reason, ip string, fromCache bool, latency int) store.MetricRow {
	return store.MetricRow{CheckedAt: mt0, Status: status, Reason: reason, VerifierIP: ip, FromCache: fromCache, LatencyMS: latency}
}

// The four build-plan metrics derive from existing LOG columns (Door 2, no schema change).
func TestDerive_FourMetrics(t *testing.T) {
	rows := []store.MetricRow{
		row("valid", "accepted", "203.0.113.10", false, 100),
		row("unknown", "smtp_blocked", "203.0.113.10", false, 200),
		row("unknown", "smtp_blocked", "203.0.113.10", false, 300),
		row("risky", "plausible", "203.0.113.10", true, 5), // cache hit — counts for cache-hit, not block-rate
	}
	s := Derive(rows, false)
	if s.Total != 4 {
		t.Fatalf("total=%d", s.Total)
	}
	// unknown-rate: 2 of 4.
	if s.UnknownRate != 0.5 {
		t.Fatalf("unknown-rate want 0.5, got %v", s.UnknownRate)
	}
	// cache-hit: 1 of 4.
	if s.CacheHitRate != 0.25 {
		t.Fatalf("cache-hit want 0.25, got %v", s.CacheHitRate)
	}
	// block-rate: 2 blocked of 3 node-attributable SMTP attempts (the cache hit is excluded, PB1).
	if s.SMTPAttempts != 3 {
		t.Fatalf("smtp attempts (excl cache hit) want 3, got %d", s.SMTPAttempts)
	}
	if s.BlockRatePoolWide < 0.66 || s.BlockRatePoolWide > 0.67 {
		t.Fatalf("block-rate want ~0.667, got %v", s.BlockRatePoolWide)
	}
	// latency percentiles present (p50 within the sample).
	if s.LatencyP50 == 0 || s.LatencyP99 < s.LatencyP50 {
		t.Fatalf("latency percentiles look wrong: p50=%d p95=%d p99=%d", s.LatencyP50, s.LatencyP95, s.LatencyP99)
	}
}

// Cache hits count for cache-hit but NOT block-rate (the P1 split: from_cache excluded from
// calibration/block-rate, counted in cache-hit).
func TestDerive_CacheHitExcludedFromBlockRate(t *testing.T) {
	rows := []store.MetricRow{
		row("unknown", "smtp_blocked", "203.0.113.10", true, 0), // cache hit "block" — must NOT count
		row("valid", "accepted", "203.0.113.10", false, 10),
	}
	s := Derive(rows, false)
	if s.SMTPAttempts != 1 || s.BlockRatePoolWide != 0 {
		t.Fatalf("a cache-hit block must not inflate block-rate: attempts=%d rate=%v", s.SMTPAttempts, s.BlockRatePoolWide)
	}
	if s.CacheHitRate != 0.5 {
		t.Fatalf("cache-hit must still count the cached row, got %v", s.CacheHitRate)
	}
}

// Per-node metrics isolate by verifier_ip (§5.6 failure isolation view).
func TestDerive_PerNodeIsolation(t *testing.T) {
	rows := []store.MetricRow{
		row("unknown", "smtp_blocked", "203.0.113.10", false, 0),
		row("unknown", "smtp_blocked", "203.0.113.10", false, 0),
		row("valid", "accepted", "203.0.113.11", false, 0),
	}
	s := Derive(rows, false)
	byIP := map[string]NodeMetric{}
	for _, n := range s.PerNode {
		byIP[n.VerifierIP] = n
	}
	if byIP["203.0.113.10"].BlockRate != 1.0 {
		t.Fatalf("dirty node block-rate want 1.0, got %v", byIP["203.0.113.10"].BlockRate)
	}
	if byIP["203.0.113.11"].BlockRate != 0 {
		t.Fatalf("clean node block-rate want 0, got %v", byIP["203.0.113.11"].BlockRate)
	}
}

// rate_limited degrades feed the rate-pressure metric (§8.1) + the rate-saturation alert (§8.2).
func TestDerive_RateLimitedPressure(t *testing.T) {
	deg := store.MetricRow{CheckedAt: mt0, Status: "risky", Reason: "plausible", Signals: map[string]any{"smtp_skipped_reason": "rate_limited"}}
	rows := []store.MetricRow{deg, deg, deg, row("valid", "accepted", "203.0.113.10", false, 0)}
	s := Derive(rows, false)
	if s.RateLimitedCount != 3 {
		t.Fatalf("rate-limited count want 3, got %d", s.RateLimitedCount)
	}
	al := DeriveAlerts(s, PoolSummary{}, AlertThresholds{RateSaturationCount: 3})
	if len(al) != 1 || al[0].Kind != "rate_saturation" {
		t.Fatalf("rate-saturation alert must fire at threshold, got %v", al)
	}
}

// Pool summary + the pool-eligible-low alert (§8.2): the eligible set draining toward empty.
func TestSummarizePool_AndEligibleLowAlert(t *testing.T) {
	statuses := map[string]string{"n1": "active", "n2": "quarantined", "n3": "recovering"}
	ps := SummarizePool(statuses)
	if ps.EligibleSize != 1 || ps.Total != 3 {
		t.Fatalf("eligible=%d total=%d", ps.EligibleSize, ps.Total)
	}
	if ps.ByStatus["recovering"] != 1 {
		t.Fatalf("recovering count want 1, got %d", ps.ByStatus["recovering"])
	}
	al := DeriveAlerts(Snapshot{}, ps, AlertThresholds{EligibleFloor: 1})
	if len(al) != 1 || al[0].Kind != "pool_eligible_low" || al[0].Severity != "high" {
		t.Fatalf("pool-eligible-low high alert must fire at floor, got %v", al)
	}
}

// No alerts when nothing crosses a threshold (and a healthy pool).
func TestDeriveAlerts_QuietWhenHealthy(t *testing.T) {
	ps := SummarizePool(map[string]string{"n1": "active", "n2": "active"})
	al := DeriveAlerts(Snapshot{RateLimitedCount: 0}, ps, AlertThresholds{EligibleFloor: 1, RateSaturationCount: 5})
	if len(al) != 0 {
		t.Fatalf("a healthy pool below thresholds must emit no metric alerts, got %v", al)
	}
}

// Truncation is threaded through (no silent cap — §8).
func TestDerive_TruncationFlag(t *testing.T) {
	s := Derive([]store.MetricRow{row("valid", "accepted", "203.0.113.10", false, 1)}, true)
	if !s.Truncated {
		t.Fatal("truncated must be threaded into the snapshot (no silent cap)")
	}
}
