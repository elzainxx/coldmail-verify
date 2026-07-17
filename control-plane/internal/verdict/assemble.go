// The verdict assembler (P1 §5.7): the SINGLE place that builds BOTH the /v1/verify
// Response (Door 1) and the LOG row (store.VerificationRow, Door 2) from one in-memory
// verdict — so the response and the LOG can never drift ("log = response + provenance").
package verdict

import (
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/store"
	"github.com/mnfi/codmailling/control-plane/internal/vocab"
)

// Core is the policy's verdict output plus the address identity, decoupled from the policy
// package (which imports layers) to keep verdict dependency-light. The orchestrator fills it.
type Core struct {
	Status         vocab.Status
	Reason         vocab.Reason
	Confidence     int
	Recommendation vocab.Recommendation
	Flags          map[string]bool
	Signals        map[string]any
}

// Provenance is everything the assembler needs beyond the verdict to build the response meta
// and the LOG row's provenance columns (§3.2). In P1 the engine fields are empty (no engine
// on the cheap path) and verifier_ip/node_id are nil — cheap-layers-only, no port 25.
type Provenance struct {
	RequestID         string
	CheckedAt         time.Time
	LatencyMS         int
	PolicyKey         string
	PolicyVersion     int
	ConfigFingerprint string
	FromCache         bool

	// Door-3 identity (from hash.Normalize/HashRaw). EmailHash nil => un-addressable
	// reject → logged with NULL hash (§3.2.7). RawEmail is echoed to the caller (OQ-4).
	RawEmail              string
	NormalizedDomain      string
	EmailHash             []byte
	ProviderCanonicalHash []byte
	HashScheme            string
	LocalPartLen          int
	MXProvider            string
}

// Assemble builds the Door-1 Response and the append-only LOG row from ONE core verdict +
// provenance. Both draw from the same fields, so they cannot diverge. The Response is
// validated by the caller via Response.Valid() before it is ever emitted.
func Assemble(c Core, p Provenance) (Response, store.VerificationRow) {
	flags := c.Flags
	if flags == nil {
		flags = map[string]bool{}
	}
	// from_cache is provenance, orthogonal to the verdict — mirrored into flags + meta (§3.1.3).
	flags["from_cache"] = p.FromCache
	signals := c.Signals
	if signals == nil {
		signals = map[string]any{}
	}

	resp := Response{
		APIVersion:     APIVersion,
		Status:         c.Status,
		Reason:         c.Reason,
		Confidence:     c.Confidence,
		Recommendation: c.Recommendation,
		Email:          p.RawEmail,          // echo RAW input (OQ-4)
		Domain:         p.NormalizedDomain,  // normalized; "" if not extractable
		Flags:          flags,
		Signals:        signals,
		Meta: Meta{
			RequestID:         p.RequestID,
			CheckedAt:         p.CheckedAt.UTC().Format(time.RFC3339),
			LatencyMS:         p.LatencyMS,
			Policy:            p.PolicyKey,
			PolicyVersion:     p.PolicyVersion,
			VerifierIP:        nil, // P1: no SMTP, no node consulted
			NodeID:            nil,
			EngineVersion:     nil, // no engine on the cheap path
			ConfigFingerprint: strPtrOrNil(p.ConfigFingerprint),
			FromCache:         p.FromCache,
		},
		Raw: nil, // null when no engine consulted (P1 always)
	}

	row := store.VerificationRow{
		ID:                    "", // set by the orchestrator (idgen) at write time
		RequestID:             p.RequestID,
		CheckedAt:             p.CheckedAt,
		CreatedAt:             p.CheckedAt,
		EmailHash:             p.EmailHash,
		HashScheme:            hashSchemeFor(p.EmailHash, p.HashScheme),
		Domain:                p.NormalizedDomain,
		ProviderCanonicalHash: p.ProviderCanonicalHash,
		LocalPartLen:          p.LocalPartLen,
		Status:                string(c.Status),
		Reason:                string(c.Reason),
		Confidence:            c.Confidence,
		Recommendation:        string(c.Recommendation),
		Flags:                 flags,
		Signals:               signals,
		MXProvider:            p.MXProvider,
		PolicyKey:             p.PolicyKey,
		PolicyVersion:         p.PolicyVersion,
		APIVersion:            APIVersion,
		ConfigFingerprint:     p.ConfigFingerprint,
		FromCache:             p.FromCache,
		LatencyMS:             p.LatencyMS,
		// Engine provenance intentionally empty in P1 (=> SQL NULL).
	}
	return resp, row
}

// hashSchemeFor keeps the §3.2.2 CHECK invariant (email_hash NULL iff hash_scheme NULL): if
// there is no hash, force the scheme to "" so the store writes NULL for both.
func hashSchemeFor(emailHash []byte, scheme string) string {
	if len(emailHash) == 0 {
		return ""
	}
	return scheme
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
