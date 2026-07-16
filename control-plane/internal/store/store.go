// Package store is the Supabase/Postgres boundary. Phase 0 defines the interface and
// the APPEND-ONLY intent (Door 2) — no driver, no DDL, no live DB.
//
// LOCKED intents encoded here (enforced for real at P1 with the DB + revoked grants):
//   - the LOG (verifications) is INSERT/SELECT only; UPDATE/DELETE are revoked from the
//     service role (P1 §3.2.1). This interface therefore offers AppendVerification and
//     reads, but NO update/delete of a verdict — the shape refuses mutation by construction.
//   - a separate grant-admin credential holds DDL (PN-9 / DR-3); the runtime store does not.
package store

import (
	"context"
	"errors"
)

// VerificationRow is the append-only LOG row (P1 §3.2.2), reduced to the Phase-0 contract
// surface. The full column set is specified in P1 §3.2.2; this is the shape the assembler
// writes. email_hash is bytea (nil for un-addressable rejects — P1 §3.2.7).
type VerificationRow struct {
	EmailHash  []byte // nil for invalid_syntax / unparseable (logged with NULL — P1 §3.2.7)
	HashScheme string // "" iff EmailHash is nil
	Domain     string // "" -> stored as NULL (no empty-string sentinels — P1 §3.2.7)
	Status     string
	Reason     string
	Confidence int
	// ... full provenance (verifier_ip, engine_version, config_fingerprint, from_cache, ...)
	// is added at P1 alongside the real schema. Phase 0 keeps the append-only SHAPE.
}

// ErrNotWired marks the Phase-0 stub (no live DB).
var ErrNotWired = errors.New("store: no database wired (Phase 0 stub)")

// Store is the control-plane's only path to state. Note the deliberate ABSENCE of an
// UpdateVerification / DeleteVerification method — Door 2 append-only is enforced by the
// interface shape, not just by DB grants.
type Store interface {
	AppendVerification(ctx context.Context, row VerificationRow) error // INSERT only
	// GetCachedVerdict, PutCachedVerdict, etc. arrive with the cache feature (P1).
	Ping(ctx context.Context) error
}

// StubStore satisfies Store without a DB, for building/testing the skeleton.
type StubStore struct{}

func (StubStore) AppendVerification(context.Context, VerificationRow) error { return ErrNotWired }
func (StubStore) Ping(context.Context) error                               { return ErrNotWired }
