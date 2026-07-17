// Package store is the Supabase/Postgres boundary. It defines the APPEND-ONLY intent
// (Door 2) as an interface shape, plus a pgx-backed implementation (postgres.go) and an
// in-memory fake (mem.go) for tests.
//
// LOCKED intents encoded here (enforced for real by the DB migrations + revoked grants):
//   - the LOG (verifications) is INSERT/SELECT only; UPDATE/DELETE are revoked from the
//     runtime service role (P1 §3.2.1). This interface offers AppendVerification + reads,
//     but NO update/delete of a verdict — the shape refuses mutation by construction.
//   - a separate grant-admin credential holds DDL (PN-9 / DR-3); the runtime store does not.
//   - the cache is keyed on the COMPOSITE (email_hash, config_fingerprint) (P1 §4.2, OQ-9),
//     never email_hash alone — a per-config verdict must not be served across configs.
package store

import (
	"context"
	"errors"
	"time"
)

// VerificationRow is the append-only LOG row (P1 §3.2.2), full provenance surface. This is
// what the verdict assembler writes for EVERY result (cheap verdicts + cache hits included).
// Pointer/typed-nil fields map to SQL NULL (invalid_syntax rows carry NULL email_hash — §3.2.7;
// no empty-string sentinels — a "" domain is written NULL).
type VerificationRow struct {
	ID         string    // UUIDv7 (idgen) — frozen join anchor (§4.7)
	RequestID  string    // correlates to meta.request_id; NOT unique (§3.2.7)
	CheckedAt  time.Time // verdict time, stored UTC
	CreatedAt  time.Time // insert time (== CheckedAt normally)

	// Door 3 identity. EmailHash nil => NULL (+ HashScheme "" and Domain "" also NULL).
	EmailHash             []byte
	HashScheme            string
	Domain                string
	ProviderCanonicalHash []byte // OQ-2; nil => NULL (provider unknown)
	LocalPartLen          int    // 0 => NULL-ish; stored as-is (shape signal, DIAL)

	// Door 4 verdict (validated against vocab before this struct is ever built).
	Status         string
	Reason         string
	Confidence     int
	Recommendation string

	Flags   map[string]bool // -> jsonb
	Signals map[string]any  // -> jsonb

	MXProvider string // promoted typed column; "" => NULL

	PolicyKey     string
	PolicyVersion int
	APIVersion    string

	// Engine provenance — all NULL in P1 (no engine consulted on the cheap path).
	VerifierIP           string // "" => NULL
	NodeID               string // "" => NULL; FK-less
	EngineVersion        string // "" => NULL
	EngineCoreVersion    string // "" => NULL
	ConfigFingerprint    string // NOT NULL (§3.2.2)
	EngineValidationType string // "" => NULL
	EngineSuccessSet     bool   // whether EngineSuccess is meaningful (else NULL)
	EngineSuccess        bool

	FromCache bool
	LatencyMS int
}

// RawRow is the 1:1 sidecar (verification_raw) holding the untouched engine blob. Written
// only when an engine was consulted; nil in P1 (cheap path never has a blob).
type RawRow struct {
	VerificationID string
	Raw            []byte // raw engine JSON; the ONLY retention-managed payload (§3.2.6)
}

// CachedVerdict is a cache row (P1 §4.2). The key is the COMPOSITE (EmailHash, ConfigFingerprint).
type CachedVerdict struct {
	EmailHash         []byte
	ConfigFingerprint string
	Domain            string
	MXProvider        string
	HashScheme        string
	Status            string
	Reason            string
	Confidence        int
	Recommendation    string
	Flags             map[string]bool
	Signals           map[string]any
	CachedAt          time.Time
	ExpiresAt         time.Time
}

// PolicyRow is a stored policy definition (P1 §4.6). Definition is the open jsonb object.
type PolicyRow struct {
	Key        string
	Version    int
	Definition []byte // raw jsonb (open object — OQ-25)
	IsDefault  bool
}

// ErrNotWired marks the no-DB stub. ErrCacheMiss is a normal (non-error) cache absence.
var (
	ErrNotWired  = errors.New("store: no database wired")
	ErrCacheMiss = errors.New("store: cache miss")
)

// Store is the control-plane's only path to state. Note the deliberate ABSENCE of an
// UpdateVerification / DeleteVerification method — Door 2 append-only is enforced by the
// interface shape, not just by DB grants. A correction is a new AppendVerification row.
type Store interface {
	// AppendVerification INSERTs one LOG row (+ optional raw sidecar). INSERT only.
	AppendVerification(ctx context.Context, row VerificationRow, raw *RawRow) error

	// Cache: composite-keyed read/write (§4.2). GetCachedVerdict returns ErrCacheMiss when
	// absent or expired (never a stale verdict). PutCachedVerdict upserts.
	GetCachedVerdict(ctx context.Context, emailHash []byte, configFingerprint string) (CachedVerdict, error)
	PutCachedVerdict(ctx context.Context, v CachedVerdict) error

	// List membership reads (brain-owned lists, §4.4). Domains are norm/v1 (lowercased).
	IsDisposable(ctx context.Context, domain string) (bool, error)
	IsSpamTrap(ctx context.Context, domain string) (bool, error)
	// CatchAllState returns (isCatchall, known). known=false => never probed (§4.3 tri-state).
	CatchAllState(ctx context.Context, domain string) (isCatchall bool, known bool, err error)

	// GetDefaultPolicy / GetPolicy load policy definitions (§4.6).
	GetDefaultPolicy(ctx context.Context) (PolicyRow, error)
	GetPolicy(ctx context.Context, key string, version int) (PolicyRow, error)

	Ping(ctx context.Context) error
	Close()
}

// StubStore satisfies Store without a DB, for building/smoke-testing the skeleton. Every
// method returns ErrNotWired (never a fabricated result) so a control-plane with no DB is
// honestly not-ready rather than silently returning fake state.
type StubStore struct{}

func (StubStore) AppendVerification(context.Context, VerificationRow, *RawRow) error { return ErrNotWired }
func (StubStore) GetCachedVerdict(context.Context, []byte, string) (CachedVerdict, error) {
	return CachedVerdict{}, ErrNotWired
}
func (StubStore) PutCachedVerdict(context.Context, CachedVerdict) error       { return ErrNotWired }
func (StubStore) IsDisposable(context.Context, string) (bool, error)          { return false, ErrNotWired }
func (StubStore) IsSpamTrap(context.Context, string) (bool, error)            { return false, ErrNotWired }
func (StubStore) CatchAllState(context.Context, string) (bool, bool, error)   { return false, false, ErrNotWired }
func (StubStore) GetDefaultPolicy(context.Context) (PolicyRow, error)         { return PolicyRow{}, ErrNotWired }
func (StubStore) GetPolicy(context.Context, string, int) (PolicyRow, error)   { return PolicyRow{}, ErrNotWired }
func (StubStore) Ping(context.Context) error                                 { return ErrNotWired }
func (StubStore) Close()                                                      {}
