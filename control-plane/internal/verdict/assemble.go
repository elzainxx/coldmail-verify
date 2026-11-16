// The verdict assembler (P1 §5.7): the SINGLE place that builds BOTH the /v1/verify
// Response (Door 1) and the LOG row (store.VerificationRow, Door 2) from one in-memory
// verdict — so the response and the LOG can never drift ("log = response + provenance").
package verdict

import (
	"time"

	"github.com/coldmail-verify/control-plane/internal/failureorigin"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/vocab"
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

	// Cause is the evidence the stage that produced an `unknown` holds about why it could not answer (see internal/failureorigin). It is
	// NEVER serialised: Assemble turns it into signals.failure_origin. Empty = the stage recorded no evidence (=> undetermined).
	Cause failureorigin.Cause
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

	// Engine provenance (P2 §3.2.2) — NULL/empty in P1 (no engine consulted on the cheap path),
	// populated when the SMTP stage consults a verifier-node. Additive: the LOG columns already
	// existed (reserved in P1), they were just always NULL.
	NodeID               string
	VerifierIP           string
	EngineVersion        string
	EngineCoreVersion    string
	EngineValidationType string
	EngineSuccessSet     bool
	EngineSuccess        bool
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
	// failure_origin (Docs/failure-origin.md): written HERE and only here, so that every `unknown` this assembler ever builds carries
	// it by construction — a new code path that mints an unknown cannot forget the key. It is an additive signals key (Door 1) and
	// changes no status, reason, confidence, recommendation, caching decision or retry.
	if origin, ok := failureorigin.Of(c.Status, c.Reason, c.Cause, signals); ok {
		signals[vocab.SignalFailureOrigin] = string(origin)
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
			VerifierIP:        strPtrOrNil(p.VerifierIP), // P2: set when a node was consulted
			NodeID:            strPtrOrNil(p.NodeID),
			EngineVersion:     strPtrOrNil(p.EngineVersion),
			ConfigFingerprint: strPtrOrNil(p.ConfigFingerprint),
			FromCache:         p.FromCache,
		},
		Raw: nil, // the response's meta.raw stays null; the raw blob rides the sidecar (§6.4)
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
		// Engine provenance (P2): populated when the SMTP stage consulted a node; empty => NULL.
		NodeID:               p.NodeID,
		VerifierIP:           p.VerifierIP,
		EngineVersion:        p.EngineVersion,
		EngineCoreVersion:    p.EngineCoreVersion,
		EngineValidationType: p.EngineValidationType,
		EngineSuccessSet:     p.EngineSuccessSet,
		EngineSuccess:        p.EngineSuccess,
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
