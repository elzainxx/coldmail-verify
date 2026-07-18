package metrics

import "fmt"

// This file: the observability ALERTS derived from the metrics + pool summary (§8.2). The
// watcher (internal/health) already emits the per-transition alerts (quarantine, block_spike incl.
// fleet-wide critical, audit_red, recovering) as it decides — those are the CONTROL-coupled alerts
// (the fleet-wide critical alert is the human backstop to the brake's deliberate non-action, §8.2).
// THIS derives the two metric-threshold alerts that fall out of the aggregate view:
//   - pool-eligible-low (§8.2 new): the eligible set dropped to/below a floor → one quarantine from
//     all-degrade; a capacity/procurement signal (add a node, §7).
//   - rate-saturation (§8.2 new): sustained rate_limited degrades → demand exceeds paced capacity →
//     add a node (§7) or (later) queue (P5, the SEAM the rate limiter's backpressure feeds).
// Kept as pure threshold logic over the derived Snapshot/PoolSummary so it's unit-testable.

// Alert mirrors health.Alert's shape (a small independent copy so the metrics package doesn't depend
// on internal/health). Kind/Severity/Detail feed whatever channel the exporter wires (OQ-P4-11).
type Alert struct {
	Kind     string // pool_eligible_low | rate_saturation
	Severity string // warn | high
	Detail   string
}

// AlertThresholds are the §8.2 DIALs. EligibleFloor: alert when EligibleSize <= this (recommend 1 —
// "one quarantine from all-degrade"). RateSaturationCount: alert when rate_limited degrades in the
// window exceed this (a sustained-saturation signal). Zero disables that alert.
type AlertThresholds struct {
	EligibleFloor       int
	RateSaturationCount int
}

// DeriveAlerts returns the metric-threshold alerts (§8.2). It is additive to the watcher's
// transition alerts — the caller emits both. Deterministic; empty when nothing crosses a threshold.
func DeriveAlerts(s Snapshot, pool PoolSummary, th AlertThresholds) []Alert {
	var out []Alert
	// pool-eligible-low: the routing candidate floor. HIGH — the pool is one quarantine from
	// all-degrade (every check would then degrade to cheap, §4.6). A procurement/capacity signal.
	if th.EligibleFloor > 0 && pool.Total > 0 && pool.EligibleSize <= th.EligibleFloor {
		out = append(out, Alert{
			Kind: "pool_eligible_low", Severity: "high",
			Detail: fmt.Sprintf("eligible set %d ≤ floor %d (of %d nodes) — one quarantine from all-degrade; add a node (§7)", pool.EligibleSize, th.EligibleFloor, pool.Total),
		})
	}
	// rate-saturation: demand exceeds paced capacity. WARN — add a node or (P5) queue the overflow.
	if th.RateSaturationCount > 0 && s.RateLimitedCount >= th.RateSaturationCount {
		out = append(out, Alert{
			Kind: "rate_saturation", Severity: "warn",
			Detail: fmt.Sprintf("%d rate_limited degrades in window ≥ threshold %d — demand exceeds paced capacity (add a node §7, or queue P5)", s.RateLimitedCount, th.RateSaturationCount),
		})
	}
	return out
}
