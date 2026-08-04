// Package metrics is the Phase-4 observability derivation (§8). It computes the build-plan's four
// required metrics — block-rate, unknown-rate, latency, cache-hit — PLUS pool-health and rate-limit
// pressure, ALL from EXISTING LOG columns (Door 2, no schema change) + node statuses. It is PURE
// (no I/O): the caller reads a window of store.MetricRow + the node statuses and passes them in, so
// derivation is deterministic and unit-testable over a seeded LOG.
//
// LOCKED split (§8.1, OQ-P4-11): the health-watcher computes block-rate from the LOG for CONTROL
// (store.BlockRateByVerifierIP — authoritative; control must not depend on a TSDB being up); THIS
// package computes the same aggregates for the HUMAN dashboard/exporter. Two views of the same data.
// from_cache rows stay CALIBRATION-excluded (P1 §3.2.1) but ARE counted in the cache-hit metric
// (they're real served answers) — the split P1 locked.
package metrics

import (
	"sort"

	"github.com/coldmail-verify/control-plane/internal/store"
)

// NodeMetric is the per-node (per verifier_ip) derived view (§8.1). block-rate uses the SAME
// node-attributable filter as the control loop (from_cache=false ∧ verifier_ip set — PB1) so the
// dashboard number matches the watcher's decision input.
type NodeMetric struct {
	VerifierIP  string
	Attempts    int     // node-attributable SMTP attempts (from_cache=false, verifier_ip set)
	Blocked     int     // smtp_blocked
	BlockRate   float64 // Blocked/Attempts (0 if no attempts)
	UnknownRate float64 // unknown / total-for-this-ip (all rows, §8.1 "per node")
	Total       int     // all rows attributed to this ip (incl. non-SMTP-attempt)
}

// Snapshot is one metrics-derivation pass (§8.1). Rates are pool-wide unless noted per-node/provider.
type Snapshot struct {
	// The build-plan's four required metrics (§8.1), pool-wide.
	BlockRatePoolWide float64 // smtp_blocked / SMTP-attempts (node-attributable)
	UnknownRate       float64 // status==unknown / total
	CacheHitRate      float64 // from_cache==true / total
	LatencyP50        int     // meta.latency_ms percentiles (ms)
	LatencyP95        int
	LatencyP99        int

	Total        int // rows in the window
	SMTPAttempts int // node-attributable SMTP attempts (block-rate denominator)

	PerNode     []NodeMetric   // per verifier_ip (§8.1)
	PerProvider map[string]int // mx_provider → unknown count (reputation trend per provider)

	// Rate-limit pressure (§8.1 new): the count of rate_limited degrades in the window (capacity
	// signal → add a node, or the P5 queue). Truncated=true when the window read hit its row cap.
	RateLimitedCount int
	Truncated        bool

	blockedPoolWide int // internal accumulator for BlockRatePoolWide (single-pass); not exported
}

// PoolSummary is the pool-health metric (§8.1 new): node counts by lifecycle + the eligible-set
// size. Derived from node statuses (not the LOG). It is how an operator sees a §5.5 cascade forming
// (the pool draining toward empty).
type PoolSummary struct {
	ByStatus     map[string]int // status → count (active|quarantined|recovering|provisioning)
	EligibleSize int            // active nodes (the routing candidate floor)
	Total        int
}

// Derive computes the Snapshot from a window of LOG rows (§8.1). rows may be in any order; it does
// not mutate them. truncated is threaded through from the bounded read (§8, no silent cap).
func Derive(rows []store.MetricRow, truncated bool) Snapshot {
	s := Snapshot{PerProvider: map[string]int{}, Truncated: truncated, Total: len(rows)}
	perNode := map[string]*NodeMetric{}
	var latencies []int
	var unknownTotal, cacheHits int

	for _, r := range rows {
		if r.Status == "unknown" {
			unknownTotal++
		}
		if r.FromCache {
			cacheHits++
		}
		if r.LatencyMS > 0 {
			latencies = append(latencies, r.LatencyMS)
		}
		if sr, ok := r.Signals["smtp_skipped_reason"].(string); ok && sr == "rate_limited" {
			s.RateLimitedCount++
		}
		if r.MXProvider != "" && r.Status == "unknown" {
			s.PerProvider[r.MXProvider]++
		}
		// Per-node (verifier_ip) — node-attributable SMTP attempts for block-rate (PB1).
		if r.VerifierIP != "" {
			nm := perNode[r.VerifierIP]
			if nm == nil {
				nm = &NodeMetric{VerifierIP: r.VerifierIP}
				perNode[r.VerifierIP] = nm
			}
			nm.Total++
			if r.Status == "unknown" {
				nm.UnknownRate++ // temporarily a count; converted to a rate below
			}
			if !r.FromCache { // a real SMTP attempt on this node
				nm.Attempts++
				s.SMTPAttempts++
				if r.Reason == "smtp_blocked" {
					nm.Blocked++
					s.blockedPoolWide++
				}
			}
		}
	}

	if s.Total > 0 {
		s.UnknownRate = float64(unknownTotal) / float64(s.Total)
		s.CacheHitRate = float64(cacheHits) / float64(s.Total)
	}
	if s.SMTPAttempts > 0 {
		s.BlockRatePoolWide = float64(s.blockedPoolWide) / float64(s.SMTPAttempts)
	}
	s.LatencyP50 = percentile(latencies, 50)
	s.LatencyP95 = percentile(latencies, 95)
	s.LatencyP99 = percentile(latencies, 99)

	for _, nm := range perNode {
		if nm.Attempts > 0 {
			nm.BlockRate = float64(nm.Blocked) / float64(nm.Attempts)
		}
		if nm.Total > 0 {
			nm.UnknownRate = nm.UnknownRate / float64(nm.Total) // count → rate
		}
		s.PerNode = append(s.PerNode, *nm)
	}
	sort.Slice(s.PerNode, func(i, j int) bool { return s.PerNode[i].VerifierIP < s.PerNode[j].VerifierIP })
	return s
}

// blockedPoolWide is an unexported accumulator (kept off the public Snapshot; BlockRatePoolWide is
// the public value). Placed on the struct so Derive's single pass can accumulate it.

// SummarizePool derives the pool-health metric (§8.1) from node statuses (status → count) + the
// eligible-set size (active nodes). It is the "is the pool collapsing" at-a-glance view (§5.5).
func SummarizePool(statuses map[string]string) PoolSummary {
	ps := PoolSummary{ByStatus: map[string]int{}, Total: len(statuses)}
	for _, st := range statuses {
		ps.ByStatus[st]++
		if st == "active" {
			ps.EligibleSize++
		}
	}
	return ps
}

// percentile returns the p-th percentile of xs (ms), 0 for empty. Nearest-rank (simple, dependency-
// free — the exact interpolation is a DIAL; nearest-rank is fine for a dashboard).
func percentile(xs []int, p int) int {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]int(nil), xs...)
	sort.Ints(cp)
	if p <= 0 {
		return cp[0]
	}
	if p >= 100 {
		return cp[len(cp)-1]
	}
	// nearest-rank: ceil(p/100 * N) - 1
	rank := (p*len(cp) + 99) / 100 // ceil
	if rank < 1 {
		rank = 1
	}
	if rank > len(cp) {
		rank = len(cp)
	}
	return cp[rank-1]
}
