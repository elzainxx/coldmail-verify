// The CONTROL-PLANE — Go control plane. Holds all state; port 25 closed; no truemail
// dependency (cheap layers are control-plane-native — P1 OQ-6; the engine is verifier-node-side).
// Phase 0: stdlib only. External deps (Supabase/pg client, etc.) arrive with the
// features that need them, so the skeleton builds clean with zero third-party code.
module github.com/mnfi/codmailling/control-plane

go 1.26
