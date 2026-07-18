package queue

import "time"

// Backoff computes retry due-times (§4.2). The SHAPE is LOCKED (hint-seeded, exponential, capped,
// jittered); the NUMBERS are DIALs. It is pure + deterministic given an injected jitter source, so
// the scheduler is unit-testable without real time (the greylist round-trip itself is un-mockable —
// GAP-35 — so we test the loop MECHANICS here, not the reject-then-accept).
type Backoff struct {
	Base       time.Duration // attempt-0 delay when the provider gave no hint (DIAL, e.g. 5m)
	Factor     float64       // exponential factor (DIAL, e.g. 2.0)
	Cap        time.Duration // ceiling on a single step (DIAL, e.g. 60m)
	JitterFrac float64       // ± fraction spread (DIAL, e.g. 0.15 = ±15%) to break thundering-herd
	// RateWait is the SHORT backoff for a capacity wait (rate_limited / no-eligible-node, §4.6):
	// capacity frees on the order of the rate window, not provider-decision minutes.
	RateWait time.Duration // DIAL, e.g. 30s

	// jitter returns a value in [-1,1]; injected so tests are deterministic (Math.random is
	// unavailable in the harness and non-deterministic anyway). nil => no jitter (tests/degenerate).
	jitter func() float64
}

// NextGreylist returns the due delay for a greylist/transient retry at the given attempt (0-based).
// It seeds attempt 0 from the provider's hint (retry_after_hint_s, the FLOOR) when present, else
// Base; grows exponentially, caps, and applies ±JitterFrac jitter. Retrying before the provider's
// window elapses just earns another 450 (§4.1), so the hint is a floor, never shortened by jitter.
func (b Backoff) NextGreylist(attempt, hintSeconds int) time.Duration {
	step := b.Base
	if attempt > 0 {
		f := 1.0
		for i := 0; i < attempt; i++ {
			f *= b.Factor
		}
		step = time.Duration(float64(b.Base) * f)
	}
	if b.Cap > 0 && step > b.Cap {
		step = b.Cap
	}
	// Honor the provider hint as a FLOOR on attempt 0 (§4.2): come back no sooner than it asked.
	hint := time.Duration(hintSeconds) * time.Second
	jittered := b.applyJitter(step)
	if attempt == 0 && hint > jittered {
		return hint
	}
	return jittered
}

// NextRateWait returns the SHORT capacity-wait delay (§4.6) with jitter — used for rate_limited and
// no-eligible-node requeues (a capacity/health wait, not a provider-transient).
func (b Backoff) NextRateWait() time.Duration {
	return b.applyJitter(b.RateWait)
}

// applyJitter spreads d by ±JitterFrac using the injected source, so a whole domain-batch that
// greylisted together doesn't retry in a synchronized burst (§4.2 — the same anti-fingerprint
// reasoning as P4 §4.2 jitter, now in the retry time domain). Never returns negative.
func (b Backoff) applyJitter(d time.Duration) time.Duration {
	if b.JitterFrac <= 0 || b.jitter == nil {
		return d
	}
	delta := float64(d) * b.JitterFrac * b.jitter() // jitter() ∈ [-1,1]
	out := time.Duration(float64(d) + delta)
	if out < 0 {
		return 0
	}
	return out
}

// Exhausted reports whether an item has run out of retry budget (§4.5): either it hit the attempt
// ceiling OR it has been retrying longer than maxAge (whichever trips first). age is now-createdAt.
// maxAttempts<=0 disables the count ceiling; maxAge<=0 disables the age ceiling (at least one
// should be set, enforced by config validation, so an item can never linger forever).
func Exhausted(attempt, maxAttempts int, age, maxAge time.Duration) bool {
	if maxAttempts > 0 && attempt >= maxAttempts {
		return true
	}
	if maxAge > 0 && age >= maxAge {
		return true
	}
	return false
}
