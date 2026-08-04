// Package ratelimit is the Phase-4 per-provider rate limiter (§4) — the reputation-protection
// machinery P3's warmup ramp explicitly was NOT (P3 §5.2). It is "one mechanism in two parts" with
// sticky routing (§3.1/§4.1): sticky routing CONCENTRATES a provider's checks on one node; this
// limiter PACES that concentration. Without stickiness a per-node cap bounds the wrong denominator
// (N nodes × K each = N×K provider-side); with it, a per-(node × provider) cap actually bounds what
// the provider sees.
//
// Two pieces of state with DELIBERATELY OPPOSITE durability (§4.3/§4.4, GAP-26):
//   - the consumed rate BUDGET (how many session-starts spent this window) is DURABLE in Supabase
//     (store.ReserveRateToken). A brain restart must NOT refill it — a freshly-restarted brain that
//     forgot its spend would let the whole pool sprint at every provider at once (the PM4 hazard).
//   - the IN-FLIGHT count (how many SMTP sessions are open right now) is EPHEMERAL, in-process.
//     On a restart the old process's TCP sessions are dead, so the correct post-restart count is
//     ZERO — a durable in-flight counter would count phantom sessions. Live gauge → ephemeral;
//     consumed allowance → durable. Confusing the two is silently catastrophic (GAP-26), so they
//     live in visibly different homes: the bucket in the store, in-flight in this struct's map.
//
// Single-brain assumption (GAP-25/OQ-P4-8): in-flight is exact for one brain. Multi-brain
// coordination is explicitly out of scope for P4 (SEAM →). Additive-only: no door, no vocab change;
// the one Door-adjacent addition (signals.smtp_skipped_reason:"rate_limited") lives in the
// orchestrator, not here.
package ratelimit

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/store"
)

// Budget is the narrow durable-store capability the limiter needs (§4.4). Satisfied by store.Store;
// kept minimal so the limiter depends only on the token-bucket read/write, and tests inject a fake.
type Budget interface {
	ReserveRateToken(ctx context.Context, nodeID, providerBucket string, p store.RateParams, now time.Time) (store.RateDecision, error)
	PeekRateBudget(ctx context.Context, nodeID, providerBucket string, p store.RateParams, now time.Time) (store.RateDecision, error)
}

// Limiter is the per-(node × provider) admission control. It holds the ephemeral in-flight counters
// (§4.3) and reads/writes the durable budget through Budget. It is safe for concurrent use.
type Limiter struct {
	budget Budget
	limits *policy.RateLimits // the policy rate_limits config (global-with-override, OQ-P4-5); nil => inert
	now    func() time.Time
	jitter func(min, max time.Duration) time.Duration // injectable for deterministic tests

	mu       sync.Mutex
	inflight map[string]int // ephemeral per-(node×provider) open-session count (§4.3, GAP-26)
}

// Config wires a Limiter. A nil Limits makes the limiter INERT (Admit always allows, no budget
// touched) — the honest state when a policy carries no rate_limits key (P1/P2/P3 policies). Now
// defaults to time.Now; Jitter defaults to a math/rand uniform in [min,max].
type Config struct {
	Budget Budget
	Limits *policy.RateLimits
	Now    func() time.Time
	Jitter func(min, max time.Duration) time.Duration
}

// New builds a Limiter. A nil Budget is tolerated only when Limits is nil (inert); otherwise the
// caller must supply a store-backed Budget (the durability §4.4 requires).
func New(c Config) *Limiter {
	now := c.Now
	if now == nil {
		now = time.Now
	}
	j := c.Jitter
	if j == nil {
		j = defaultJitter
	}
	return &Limiter{
		budget:   c.Budget,
		limits:   c.Limits,
		now:      now,
		jitter:   j,
		inflight: map[string]int{},
	}
}

// Decision is the outcome of an admission check (§4). Admitted=false means every relevant cap is
// exhausted → the caller degrades to cheap with smtp_skipped_reason="rate_limited" (§4.6). Reason
// records WHICH cap bound (for logs/metrics; never a caller-facing leak). When Admitted, the caller
// MUST call Release(node, provider) when the SMTP session closes to free the in-flight slot.
type Decision struct {
	Admitted bool
	Reason   string // "" | "inflight_cap" | "rate_budget" | "no_limit_configured_ok"
}

// inflight/rate cap reasons (internal diagnostics only — never surfaced to the caller).
const (
	reasonInflightCap = "inflight_cap"
	reasonRateBudget  = "rate_budget"
	reasonOK          = ""
)

// Admit tries to admit one SMTP session-start from node to the target provider (§4). It enforces
// BOTH caps as an AND (§4.2 "admitted only if both allow it"): (1) the ephemeral concurrency cap
// (max_inflight) and (2) the durable rate budget (token bucket). It increments in-flight ONLY on a
// full admit; a rate-budget denial does NOT hold an in-flight slot. When inert (no limits), it
// admits without touching state. The caller pairs a true Admit with exactly one Release.
func (l *Limiter) Admit(ctx context.Context, nodeID, provider string) (Decision, error) {
	lim, ok := l.limitFor(provider)
	if !ok {
		// No rate_limits configured for this bucket (and no default) → inert: admit, hold no slot.
		// Harden-toward-honest: an unconfigured limiter never fabricates a cap AND never blocks.
		return Decision{Admitted: true, Reason: reasonOK}, nil
	}
	bucket := ProviderBucket(provider)

	// (1) Concurrency cap first (cheap, in-process). Reserve the slot optimistically; release it if
	// the rate-budget check then denies, so a budget denial never leaks an in-flight slot.
	if lim.MaxInflight > 0 {
		l.mu.Lock()
		key := inflightKey(nodeID, bucket)
		if l.inflight[key] >= lim.MaxInflight {
			l.mu.Unlock()
			return Decision{Admitted: false, Reason: reasonInflightCap}, nil
		}
		l.inflight[key]++
		l.mu.Unlock()
	}

	// (2) Durable rate budget (token bucket, §4.4). If MaxStartsPerWindow<=0 the rate cap is unset
	// → only the concurrency cap applies (still a valid limiter). Otherwise consume one token.
	if lim.MaxStartsPerWindow > 0 && l.budget != nil {
		p := rateParams(lim)
		dec, err := l.budget.ReserveRateToken(ctx, nodeID, bucket, p, l.now())
		if err != nil {
			l.releaseInflight(nodeID, bucket, lim)
			return Decision{}, err
		}
		if !dec.Allowed {
			l.releaseInflight(nodeID, bucket, lim) // give back the optimistic slot — no session starts
			return Decision{Admitted: false, Reason: reasonRateBudget}, nil
		}
	}
	return Decision{Admitted: true, Reason: reasonOK}, nil
}

// Release frees one in-flight slot for (node, provider) after an admitted SMTP session closes
// (§4.3). It NEVER touches the durable budget — a consumed token is spent for the window regardless
// of how the session ends (that is the point of rate-limiting the START). Idempotent-safe against
// underflow (never drops below zero). A no-op when the bucket had no concurrency cap.
func (l *Limiter) Release(nodeID, provider string) {
	lim, ok := l.limitFor(provider)
	if !ok || lim.MaxInflight <= 0 {
		return
	}
	l.releaseInflight(nodeID, ProviderBucket(provider), lim)
}

func (l *Limiter) releaseInflight(nodeID, bucket string, lim policy.RateLimit) {
	if lim.MaxInflight <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := inflightKey(nodeID, bucket)
	if l.inflight[key] > 0 {
		l.inflight[key]--
	}
}

// HasHeadroom reports whether node has BOTH concurrency headroom AND rate-budget headroom for the
// provider (§3.3 eligibility "has rate-limit headroom for the target provider"). It is a PEEK — it
// consumes nothing. The router uses it to decide whether a node is a valid home for THIS check
// before selection; the actual consume happens in Admit at call time. Inert limiter → always true.
func (l *Limiter) HasHeadroom(ctx context.Context, nodeID, provider string) (bool, error) {
	lim, ok := l.limitFor(provider)
	if !ok {
		return true, nil
	}
	bucket := ProviderBucket(provider)
	if lim.MaxInflight > 0 {
		l.mu.Lock()
		atCap := l.inflight[inflightKey(nodeID, bucket)] >= lim.MaxInflight
		l.mu.Unlock()
		if atCap {
			return false, nil
		}
	}
	if lim.MaxStartsPerWindow > 0 && l.budget != nil {
		dec, err := l.budget.PeekRateBudget(ctx, nodeID, bucket, rateParams(lim), l.now())
		if err != nil {
			return false, err // a budget read error fails closed (no headroom) — never :smtp on unknown
		}
		return dec.Allowed, nil
	}
	return true, nil
}

// Inflight returns the current ephemeral open-session count for (node, provider) — for least-inflight
// routing (§3.4) and metrics (§8). The fallback strategy ranks nodes by THIS per-(node,provider)
// count (PM2 — the constraint being relieved is this provider's concurrency, not node-global).
func (l *Limiter) Inflight(nodeID, provider string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inflight[inflightKey(nodeID, ProviderBucket(provider))]
}

// JitterDelay returns a randomized pre-start delay for the target provider (§4.2 anti-fingerprint):
// a rate cap alone still produces evenly-spaced, machine-regular connections — jitter breaks the
// periodic pattern. The spread is the bucket's start_delay_jitter_ms [min,max]; zero/absent => no
// delay. The caller sleeps this before opening the SMTP session.
func (l *Limiter) JitterDelay(provider string) time.Duration {
	lim, ok := l.limitFor(provider)
	if !ok || len(lim.StartDelayJitterMS) != 2 {
		return 0
	}
	lo := time.Duration(lim.StartDelayJitterMS[0]) * time.Millisecond
	hi := time.Duration(lim.StartDelayJitterMS[1]) * time.Millisecond
	if hi <= lo {
		return lo
	}
	return l.jitter(lo, hi)
}

// limitFor resolves the effective RateLimit for a provider: the ByProvider override for its bucket,
// else Default (global-with-override, OQ-P4-5). ok=false means no limits configured at all (inert).
func (l *Limiter) limitFor(provider string) (policy.RateLimit, bool) {
	if l.limits == nil {
		return policy.RateLimit{}, false
	}
	bucket := ProviderBucket(provider)
	if l.limits.ByProvider != nil {
		if rl, ok := l.limits.ByProvider[bucket]; ok {
			return rl, true
		}
		// A by_provider entry may also be keyed on the bare class (e.g. "other") — try that too.
		if rl, ok := l.limits.ByProvider[provider]; ok {
			return rl, true
		}
	}
	// Default applies when set (a non-zero window/cap distinguishes it from the zero value).
	if l.limits.Default.MaxInflight > 0 || l.limits.Default.MaxStartsPerWindow > 0 {
		return l.limits.Default, true
	}
	return policy.RateLimit{}, false
}

// rateParams converts a policy RateLimit's window controls into the store token-bucket DIALs
// (§4.4/OQ-P4-3): capacity = max_starts_per_window (the burst), refill = capacity/window_s (steady
// rate that refills the window's allowance over window_s). window_s<=0 falls back to 60s.
func rateParams(lim policy.RateLimit) store.RateParams {
	win := lim.WindowS
	if win <= 0 {
		win = 60
	}
	cap := float64(lim.MaxStartsPerWindow)
	return store.RateParams{Capacity: cap, RefillPerSec: cap / float64(win)}
}

func inflightKey(nodeID, bucket string) string { return nodeID + "\x00" + bucket }

// ProviderBucket maps the brain's mx_provider class to the rate-limit BUCKET key (OQ-P4-2). The big
// consumer providers (google/o365/yahoo/self) bucket by their class. The honest-corporate "other"
// bucket is refined by the domain so distinct corporate providers aren't lumped into one budget:
// "other:<domain>". (§4.1 note: google/o365/yahoo are :mx-routed and never SMTP'd, so the live
// buckets are the "other:*" honest-corporate traffic; those classes appear here only for
// completeness / a mis-routed check.) The precise "registrable domain of the MX host" refinement is
// a DIAL (GAP-29: domain-sticky only PARTIALLY concentrates shared platforms) — this uses the
// request domain, which the orchestrator already has; a future MX-host-registrable-domain refinement
// is additive and flagged in the spec.
func ProviderBucket(provider string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	switch p {
	case "google", "o365", "yahoo", "self":
		return p
	case "", "other":
		return "other" // no finer key available at this layer
	default:
		// The orchestrator passes "other:<domain>" for honest-corporate traffic (see BucketForDomain).
		return p
	}
}

// BucketForDomain builds the honest-corporate bucket key from the provider class + the request
// domain (OQ-P4-2): a big-provider class buckets by class; "other"/self buckets by "other:<domain>"
// so two corporates are paced independently. The orchestrator calls this to pass a concrete bucket
// into Admit/Release/Inflight via the provider argument.
func BucketForDomain(provider, domain string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	switch p {
	case "google", "o365", "yahoo":
		return p
	default: // "self", "other", "" → pace per honest-corporate domain
		d := strings.ToLower(strings.TrimSpace(domain))
		if d == "" {
			return "other"
		}
		return "other:" + d
	}
}

// Buckets returns the current in-flight bucket keys with a non-zero count (for metrics/diagnostics),
// sorted for determinism.
func (l *Limiter) Buckets() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.inflight))
	for k, v := range l.inflight {
		if v > 0 {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
