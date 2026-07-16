// Package layers holds the cheap-layer contract (P1 §6.0). Phase 0 defines the LOCKED
// LayerOutput shape only — no layer implementations (syntax/mx/disposable/... are P1).
//
// A layer reports a finding + a hint; the POLICY decides gate/flag/stop (P1 §7.1). A layer
// NEVER decides the final verdict, and an `unknown` from a layer propagates toward status
// unknown, never invalid (Door 4 invariant).
package layers

// Result is a layer's finding (P1 §6.0).
type Result string

const (
	Pass    Result = "pass"
	Fail    Result = "fail"
	Unknown Result = "unknown"
	Flag    Result = "flag"
	Skip    Result = "skip"
)

// Severity is the layer's hint (the policy, not the layer, acts on it).
type Severity string

const (
	Info     Severity = "info"
	Warn     Severity = "warn"
	Critical Severity = "critical"
)

// LayerOutput is the LOCKED shape emitted into the signals bag (P1 §6.0).
type LayerOutput struct {
	LayerID      string
	SignalKey    string
	Result       Result
	SeverityHint Severity
	Value        any
	Detail       string
}

// Layer is the common interface each cheap layer implements at P1. Declared here so the
// runner/verdict wiring has a stable type; no implementations at Phase 0.
type Layer interface {
	// Run is implemented per-layer at P1 (syntax, mx, null_mx, disposable, spam_trap,
	// provider_classify, role, typo, free_corp). Phase 0: contract only.
	ID() string
}
