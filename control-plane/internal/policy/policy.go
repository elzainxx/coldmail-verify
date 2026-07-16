// Package policy is the signals-plus-policy engine boundary (P1 §7.1). Phase 0 defines
// the shape only: a policy is table-driven DATA (jsonb in the policies store), evaluated
// by the engine — "more policies slot in" without new code (P1 §1.6).
//
// The action grammar "<status>:<reason>:<stop|continue>" (P1 §7.2) is the LOCKED shape;
// the specific gate mappings are DIALs (data). Phase 0 does NOT evaluate anything — no
// layers run yet. This is the extension point later phases add keys to (rate_limits P4,
// external P6, historical_bounce gate P7), never new grammar.
package policy

// Definition is the open, extensible policy object (P1 OQ-25: tolerate unknown keys — a
// closed struct would block later-phase keys). Phase 0 keeps it as raw config the engine
// will parse; the shape is locked, contents are dials.
type Definition struct {
	Key     string
	Version int
	// Order, Gates, Flags, ProviderRouting, Confidence, RecommendationThresholds, and the
	// later-phase keys (rate_limits, external, ...) are carried as an open map so unknown
	// keys are tolerated — parsed by the engine at P1, not modeled as a closed struct here.
	Raw map[string]any
}

// Engine evaluates layer signals against a policy to a verdict. Phase 0 is a placeholder:
// no layers, no evaluation. The interface exists so the router/verdict wiring compiles and
// so P1 implements Evaluate without changing callers.
type Engine interface {
	// Evaluate is intentionally unimplemented at Phase 0 (returns the honest cheap-unknown
	// at the call site instead). Signature lands at P1 with the real signal bag + verdict.
}
