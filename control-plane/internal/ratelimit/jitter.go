package ratelimit

import (
	"math/rand"
	"time"
)

// defaultJitter returns a uniform random delay in [min, max) (§4.2 anti-fingerprint spread). It uses
// math/rand (non-crypto — this is timing jitter, not a secret) via the package-global source, which
// Go 1.20+ auto-seeds, so starts don't arrive as a periodic, fingerprintable burst. Tests inject a
// deterministic Jitter instead (Config.Jitter) so behavior is reproducible; ONLY the production path
// is random. min>=max returns min (no spread).
func defaultJitter(min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	return min + time.Duration(rand.Int63n(int64(max-min)))
}
