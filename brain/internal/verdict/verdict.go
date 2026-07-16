// Package verdict holds the Door 1 response contract shape and the single assembler
// that builds BOTH the /v1/verify response and the LOG row from one in-memory verdict
// (P1 §5.7) — so response and LOG can never drift.
//
// Phase 0: the versioned, additive-only response STRUCT (Door 1) + JSON tags. No scoring
// (that maps signals->verdict and is later-phase); the assembler is a stub that produces
// an honest Phase-0 verdict (unknown/plausible with no engine consulted).
package verdict

import "github.com/mnfi/codmailling/brain/internal/vocab"

// APIVersion is the frozen contract version. A breaking change means /v2, never in-place.
const APIVersion = "v1"

// Response is the Door 1 v1 contract (P1 §3.1.3). Additive-only: never remove/rename a
// field or change a type. `flags`/`signals` are open bags; consumers must tolerate unknown keys.
type Response struct {
	APIVersion     string                 `json:"api_version"`
	Status         vocab.Status           `json:"status"`
	Reason         vocab.Reason           `json:"reason"`
	Confidence     int                    `json:"confidence"` // int 0-100, LOCKED type/range
	Recommendation vocab.Recommendation   `json:"recommendation"`
	Email          string                 `json:"email"`  // echoes RAW input (OQ-4)
	Domain         string                 `json:"domain"` // normalized; "" if not extractable
	Flags          map[string]bool        `json:"flags"`
	Signals        map[string]any         `json:"signals"`
	Meta           Meta                   `json:"meta"`
	Raw            map[string]any         `json:"raw"` // null when no engine consulted (P0: always null)
}

// Meta is the provenance echoed to the caller (subset of the LOG's superset — P1 §3.1.3).
type Meta struct {
	RequestID         string  `json:"request_id"`
	CheckedAt         string  `json:"checked_at"` // RFC3339 UTC
	LatencyMS         int     `json:"latency_ms"`
	Policy            string  `json:"policy"`
	PolicyVersion     int     `json:"policy_version"`
	VerifierIP        *string `json:"verifier_ip"`         // null in P0/cheap-only
	NodeID            *string `json:"node_id"`             // null when no node consulted
	EngineVersion     *string `json:"engine_version"`      // null when no engine consulted
	ConfigFingerprint *string `json:"config_fingerprint"`
	FromCache         bool    `json:"from_cache"`
}

// NewCheapUnknown returns the honest Phase-0 verdict: no engine was consulted, no SMTP
// ran, so the truthful answer is unknown/plausible at low confidence — NEVER a guessed
// valid/invalid. This is the harden-toward-unknown default the whole system rests on.
// It also proves the composability invariant holds for the shape we ship.
func NewCheapUnknown(rawEmail, domain, requestID, checkedAt string) Response {
	return Response{
		APIVersion:     APIVersion,
		Status:         vocab.StatusUnknown,
		Reason:         vocab.ReasonPlausible,
		Confidence:     0,
		Recommendation: vocab.RecReview,
		Email:          rawEmail,
		Domain:         domain,
		Flags:          map[string]bool{"from_cache": false},
		Signals:        map[string]any{},
		Meta: Meta{
			RequestID: requestID, CheckedAt: checkedAt,
			Policy: "cold-email", PolicyVersion: 1, FromCache: false,
		},
		Raw: nil,
	}
}

// Valid checks the response against the frozen vocabulary + composability invariants
// (Door 1/4). The assembler and tests use it to guarantee nothing illegal is ever emitted.
func (r Response) Valid() bool {
	return r.APIVersion == APIVersion &&
		vocab.ValidStatus(r.Status) &&
		vocab.ValidReason(r.Reason) &&
		vocab.ValidRecommendation(r.Recommendation) &&
		r.Confidence >= 0 && r.Confidence <= 100 &&
		vocab.ComposabilityOK(r.Status, r.Recommendation)
}
