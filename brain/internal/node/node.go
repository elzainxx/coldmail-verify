// Package node is the client to a verifier box, held to the LOCKED "pure function"
// view (Door 5, P1 §5.5): a node is `email -> raw engine JSON`. ALL interpretation
// (normalization, scoring, policy) is brain-side; the box decides nothing.
//
// Phase 0: the contract only. No live box is contacted (none exists). The parse target
// is the stock rack's lean body (Docs/truemail-ground-truth.md §2): the box returns raw
// engine facts, never a status/reason/confidence.
package node

import (
	"context"
	"errors"
)

// RawEngineResult is the untouched engine JSON the box returns. Phase 0 keeps it opaque
// (json.RawMessage-shaped as bytes); P2 parses smtp_debug etc. brain-side. Deliberately
// NOT a typed verdict — the box never renders a verdict.
type RawEngineResult []byte

// Client is the pure-function boundary. Verify sends one address to one node and returns
// raw engine facts. It never returns a status/reason — that is verdict-assembly's job.
type Client interface {
	Verify(ctx context.Context, baseURL, token, email string) (RawEngineResult, error)
}

// ErrNotImplemented marks the Phase-0 stub: the node client is a compiled contract, not a
// live caller. P2 wires the real HTTP call to the fork's /v1/verify.
var ErrNotImplemented = errors.New("node: live verify is a Phase 2 activity (stub)")

// StubClient satisfies Client without contacting a box. Returning ErrNotImplemented (not a
// fake verdict) keeps Phase 0 honest — the brain degrades to cheap/unknown, never invents SMTP.
type StubClient struct{}

func (StubClient) Verify(_ context.Context, _, _, _ string) (RawEngineResult, error) {
	return nil, ErrNotImplemented
}
