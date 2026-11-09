package smtp

import (
	"sort"

	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// confNonPublicMX is deliberately low: nothing was learned about the MAILBOX. The domain publishes a mail host that is not on the
// public internet, so there was no probe — "can't tell", not "bad" (and certainly not invalid).
const confNonPublicMX = 20

// ProbeRefusedResult maps the node's destination-guard refusal (ADR-0021) to the verdict: risky / non_public_mx. It is a FACT about
// the domain's DNS, not about the address, so it is never invalid; and because it is derived from DNS state that the domain owner can
// change, it is NOT cacheable (a stray private A record must not be remembered for a TTL).
func ProbeRefusedResult(p *ProbeRefusal) Result {
	seen := map[string]struct{}{}
	for _, t := range p.Targets {
		seen[t.Range] = struct{}{}
	}
	ranges := make([]string, 0, len(seen))
	for r := range seen {
		ranges = append(ranges, r)
	}
	sort.Strings(ranges)
	signals := map[string]any{
		"probe_refused":        true,
		"probe_refused_ranges": ranges,
		"probe_refused_count":  p.Count,
	}
	return Result{Status: vocab.StatusRisky, Reason: vocab.ReasonNonPublicMX, Confidence: confNonPublicMX, Signals: signals, Cacheable: false}
}
