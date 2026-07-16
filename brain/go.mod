// The BRAIN — Go control plane. Holds all state; port 25 closed; no truemail
// dependency (cheap layers are brain-native — P1 OQ-6; the engine is box-side).
// Phase 0: stdlib only. External deps (Supabase/pg client, etc.) arrive with the
// features that need them, so the skeleton builds clean with zero third-party code.
module github.com/mnfi/codmailling/brain

go 1.26
