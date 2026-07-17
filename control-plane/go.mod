// The CONTROL-PLANE — Go control plane. Holds all state; port 25 closed; no truemail
// dependency (cheap layers are control-plane-native — P1 OQ-6; the engine is verifier-node-side).
// External deps arrive with the features that need them (PROJECT_RULES §6): Phase 0 was
// stdlib-only; Phase 1 adds golang.org/x/text (Unicode NFC) + golang.org/x/net (IDNA/UTS-46)
// for the frozen Door-3 norm/v1 pipeline (§3.3.2, ADR-0006). A Postgres driver lands with the
// store. Still NO truemail dependency in the control-plane.
module github.com/mnfi/codmailling/control-plane

go 1.26

require (
	golang.org/x/net v0.57.0
	golang.org/x/text v0.40.0
)
