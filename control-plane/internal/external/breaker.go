package external

import "time"

// This file: the per-vendor SPEND budget + circuit breaker (§5, HIGHEST RIGOR). This is MONEY, not
// IP reputation — explicitly a DISTINCT control from the P4 per-provider rate limiter (§5.1/GAP-44):
// the P4 limiter never governs vendor calls; this breaker never governs SMTP. Two backpressure
// mechanisms, two resource domains, never conflated.
//
// The breaker is a BRAKE, not a cascade (§5.5, inherited fleet-brake posture): when it is open the
// external layer is SKIPPED and the residue verdict settles as-is (keep_residue) — it never
// fabricates a verdict, never disables the policy, never fails a job. A tripped breaker is an
// operational signal ("residue volume exceeds budget — raise the cap or improve internal coverage"),
// surfaced to the operator; recovery is automatic (window roll / half-open probe).
//
// Budget state is DURABLE (the same transactional store as the queue — single source of truth, per
// the P5 GAP-34 discipline): the spend counter is derived from the append-only external_calls LOG
// (§6), so a brain restart cannot reset the day's spend to zero and sprint at the vendor. This file
// holds the PURE decision logic over injected spend/error/time facts; the store supplies the facts.

// Budget is the per-vendor spend ceiling (§5.2) — all DIALs (existence locked, values owner-OQ).
// A zero cap means UNSET → that ceiling is not enforced (the owner has not chosen a value; §5.2
// "start conservative and raise from observed volume"). Enforcement is fail-SAFE toward NOT
// spending: see Decide — an unset cap does not authorize unbounded spend on its own, it just doesn't
// add a ceiling; the layer only runs at all when deliberately enabled (Fallback==enabled + DPA).
type Budget struct {
	DailyCap    int // §5.2 hard ceiling on cost-units per rolling day; 0 => unset (not enforced)
	MonthlyCap  int // §5.2 per rolling month; 0 => unset
	PerJobCap   int // §5.2 per bulk job — one pathological list can't drain the global budget; 0 => unset
	RateCeiling int // §5.2 max vendor CALLS per minute (a burst brake, distinct from the cost caps); 0 => unset
}

// BreakerParams are the circuit-breaker DIALs (§5.3 thresholds). ErrorRateThreshold is the sustained
// failure fraction that trips condition 2; MinSamples gates it (don't trip on 1-2 unlucky calls,
// mirrors the P4 block-rate sample gate GAP-27); Cooldown is how long the breaker stays open before
// a half-open probe. A zero ErrorRateThreshold/MinSamples disables the error-rate condition (only
// budget-exhaustion and explicit vendor-down then trip it).
type BreakerParams struct {
	ErrorRateThreshold float64       // §5.3 cond.2: sustained error fraction → open; 0 => disabled
	MinSamples         int           // §5.3/GAP-27: min calls in window before error-rate can trip
	Cooldown           time.Duration // §5.3: open duration before a half-open probe
}

// SpendFacts is the durable spend/error state the store supplies for a breaker decision — the facts
// the pure Decide logic reasons over. All are windowed reads off the append-only external_calls LOG
// (§6), so they survive a brain restart (§5.2 durable, the PM4-analogue hazard for MONEY).
type SpendFacts struct {
	SpentDay        int // cost-units spent in the rolling day
	SpentMonth      int // cost-units spent in the rolling month
	SpentJob        int // cost-units spent by THIS bulk job (0 for a singular/sync check)
	Calls           int // vendor calls in the error-rate window (denominator)
	Errors          int // failed/timed-out vendor calls in the window (numerator)
	CallsLastMinute int // vendor calls in the last rolling minute — the rate_ceiling brake (§5.2)
	// OpenedAt is when the breaker last tripped on error-rate/vendor-down (zero => not open). The
	// store persists it (or derives it) so the open state survives a restart; a half-open probe is
	// allowed once now >= OpenedAt + Cooldown.
	OpenedAt time.Time
}

// BreakerState is the standard three-state breaker (§5.3).
type BreakerState int

const (
	Closed   BreakerState = iota // serving — vendor calls allowed
	Open                         // tripped — skip the vendor, keep_residue (§5.3 LOCKED behavior)
	HalfOpen                     // cooldown elapsed — allow ONE probe call; success => Closed
)

// TripReason records WHY the breaker is open (observability §9; also the alert text). "" => closed.
type TripReason string

const (
	TripNone         TripReason = ""
	TripBudgetDay    TripReason = "budget_daily_cap"
	TripBudgetMonth  TripReason = "budget_monthly_cap"
	TripBudgetJob    TripReason = "budget_per_job_cap"
	TripErrorRate    TripReason = "vendor_error_rate"
	TripVendorDown   TripReason = "vendor_down"
	TripRateCeiling  TripReason = "rate_ceiling"
)

// Decision is the breaker's answer to "may I call the vendor now?" (§5.3). Allow=false => the layer
// is skipped and the residue settles as-is (keep_residue) — NEVER a fabricated verdict.
type Decision struct {
	Allow  bool
	State  BreakerState
	Reason TripReason // why not allowed (or why half-open); TripNone when cleanly closed
}

// Decide is the PURE breaker + budget decision (§5.3). Given the DIALs and the durable facts at a
// point in time, it returns whether a vendor call is allowed. Order matters — budget ceilings are
// checked first (a hard "no more money this window" is unconditional and needs no cooldown), then the
// error-rate/vendor-down cooldown state.
//
// Budget-open behavior is LOCKED (§5.3): breaker open => skip => keep_residue. This function only
// DECIDES; the caller (the orchestrator's external stage) is what actually degrades to keep_residue.
// It never itself produces a verdict.
func Decide(b Budget, p BreakerParams, f SpendFacts, now time.Time) Decision {
	// Condition 1 — budget exhaustion (§5.3.1). Any window cap reached => open until the window rolls.
	// A cap of 0 is UNSET (not enforced); a reached cap (spent >= cap) trips. per_job first so a
	// pathological single list is bounded before it can push the global day/month over (§5.2/§4.4).
	if b.PerJobCap > 0 && f.SpentJob >= b.PerJobCap {
		return Decision{Allow: false, State: Open, Reason: TripBudgetJob}
	}
	if b.DailyCap > 0 && f.SpentDay >= b.DailyCap {
		return Decision{Allow: false, State: Open, Reason: TripBudgetDay}
	}
	if b.MonthlyCap > 0 && f.SpentMonth >= b.MonthlyCap {
		return Decision{Allow: false, State: Open, Reason: TripBudgetMonth}
	}
	// Rate ceiling (§5.2): a burst brake on vendor CALLS per minute — distinct from the cumulative
	// cost caps (it bounds the RATE so a spike can't cost-spike or trip the vendor's own limiter).
	// 0 => unset (not enforced). This degrades to keep_residue like any other open state (never a
	// fabricated verdict); it self-clears as the minute window rolls.
	if b.RateCeiling > 0 && f.CallsLastMinute >= b.RateCeiling {
		return Decision{Allow: false, State: Open, Reason: TripRateCeiling}
	}

	// Conditions 2/3 — error-rate / vendor-down cooldown (§5.3.2/§5.3.3). If the breaker was tripped
	// (OpenedAt set), it stays Open until the cooldown elapses, then HalfOpen (allow ONE probe).
	if !f.OpenedAt.IsZero() {
		if now.Before(f.OpenedAt.Add(p.Cooldown)) {
			return Decision{Allow: false, State: Open, Reason: TripErrorRate}
		}
		// Cooldown elapsed → half-open: allow a single probe. Success (recorded as a clean call)
		// clears OpenedAt on the next facts read → Closed; a failure re-opens (the store re-stamps).
		return Decision{Allow: true, State: HalfOpen, Reason: TripErrorRate}
	}

	// Not currently tripped: evaluate the error-rate condition fresh (§5.3.2). Gated by MinSamples
	// (GAP-27 — never trip on a tiny denominator). This SIGNALS a trip; the store stamps OpenedAt so
	// the trip persists across the cooldown (and restarts). Until stamped, we still Allow (the caller
	// records this call's outcome, which is what moves the durable error count).
	if p.ErrorRateThreshold > 0 && p.MinSamples > 0 && f.Calls >= p.MinSamples {
		if float64(f.Errors)/float64(f.Calls) >= p.ErrorRateThreshold {
			return Decision{Allow: false, State: Open, Reason: TripErrorRate}
		}
	}
	return Decision{Allow: true, State: Closed, Reason: TripNone}
}
