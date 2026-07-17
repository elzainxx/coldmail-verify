// pgx-backed Store — the real Postgres/Supabase client (P1 §5.6). It writes ONLY via
// INSERT on verifications (Door 2); the interface has no update/delete, and the DB grants
// (migrations/0005) revoke UPDATE/DELETE from the runtime role as the physical backstop.
//
// P2 fidelity note (flagged by the P1→P2 readiness review): this store round-trips `Signals`
// through JSON, so a numeric value written as a Go `int` reads back as `float64`, and a nil
// flags/signals map is stored + returned as non-nil `{}` (via orEmpty*). In P1 this is inert
// (signals are bool/string only, and the live path always sets from_cache so maps are non-nil).
// When P2 adds NUMERIC signals (`smtp_code` is a string, but `retry_after_hint_s` is an int,
// §8 of the P2 spec), a MemStore-backed test could pass with an `int` that real PG returns as
// `float64` — masking a type-assertion bug. P2 must either round-trip the fake through JSON too,
// or not assert on `Signals` value *types* in mem-fake tests. Recorded so it isn't discovered late.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PG is the pgx-pool-backed Store.
type PG struct {
	pool *pgxpool.Pool
}

// OpenPG connects a pool to the given DSN and pings it. The caller owns Close().
func OpenPG(ctx context.Context, dsn string) (*PG, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: pgx pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &PG{pool: pool}, nil
}

func (p *PG) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }
func (p *PG) Close()                         { p.pool.Close() }

// nullBytes / nullStr map Go zero values to SQL NULL (no empty-string sentinels — §3.2.7).
func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func nullInt(i int) any {
	if i == 0 {
		return nil
	}
	return i
}

func (p *PG) AppendVerification(ctx context.Context, row VerificationRow, raw *RawRow) error {
	flagsJSON, err := json.Marshal(orEmptyFlags(row.Flags))
	if err != nil {
		return fmt.Errorf("store: marshal flags: %w", err)
	}
	signalsJSON, err := json.Marshal(orEmptySignals(row.Signals))
	if err != nil {
		return fmt.Errorf("store: marshal signals: %w", err)
	}

	// A raw sidecar row (when an engine was consulted) must be atomic with the LOG row, so
	// a crash never leaves a blob without its verdict or vice-versa. Wrap both in one tx.
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after a successful commit is a no-op

	var engineSuccess any
	if row.EngineSuccessSet {
		engineSuccess = row.EngineSuccess
	}

	const q = `
INSERT INTO verifications (
  id, request_id, checked_at, created_at,
  email_hash, hash_scheme, domain, provider_canonical_hash, local_part_len,
  status, reason, confidence, recommendation, flags, signals, mx_provider,
  policy_key, policy_version, api_version,
  verifier_ip, node_id, engine_version, engine_core_version, config_fingerprint,
  engine_validation_type, engine_success, from_cache, latency_ms
) VALUES (
  $1,$2,$3,$4,
  $5,$6,$7,$8,$9,
  $10,$11,$12,$13,$14,$15,$16,
  $17,$18,$19,
  $20,$21,$22,$23,$24,
  $25,$26,$27,$28
)`
	if _, err := tx.Exec(ctx, q,
		row.ID, row.RequestID, row.CheckedAt.UTC(), row.CreatedAt.UTC(),
		nullBytes(row.EmailHash), nullStr(row.HashScheme), nullStr(row.Domain), nullBytes(row.ProviderCanonicalHash), nullInt(row.LocalPartLen),
		row.Status, row.Reason, row.Confidence, row.Recommendation, flagsJSON, signalsJSON, nullStr(row.MXProvider),
		row.PolicyKey, row.PolicyVersion, row.APIVersion,
		nullStr(row.VerifierIP), nullStr(row.NodeID), nullStr(row.EngineVersion), nullStr(row.EngineCoreVersion), row.ConfigFingerprint,
		nullStr(row.EngineValidationType), engineSuccess, row.FromCache, nullInt(row.LatencyMS),
	); err != nil {
		return fmt.Errorf("store: insert verification: %w", err)
	}

	if raw != nil {
		if _, err := tx.Exec(ctx,
			`INSERT INTO verification_raw (verification_id, raw, created_at) VALUES ($1,$2,$3)`,
			raw.VerificationID, raw.Raw, row.CreatedAt.UTC(),
		); err != nil {
			return fmt.Errorf("store: insert raw: %w", err)
		}
	}
	return tx.Commit(ctx)
}

func (p *PG) GetCachedVerdict(ctx context.Context, emailHash []byte, cfg string) (CachedVerdict, error) {
	const q = `
SELECT email_hash, config_fingerprint, domain, mx_provider, hash_scheme,
       status, reason, confidence, recommendation, flags, signals, cached_at, expires_at
FROM cache WHERE email_hash=$1 AND config_fingerprint=$2 AND expires_at > now()`
	var v CachedVerdict
	var mxProvider *string
	var flagsJSON, signalsJSON []byte
	err := p.pool.QueryRow(ctx, q, emailHash, cfg).Scan(
		&v.EmailHash, &v.ConfigFingerprint, &v.Domain, &mxProvider, &v.HashScheme,
		&v.Status, &v.Reason, &v.Confidence, &v.Recommendation, &flagsJSON, &signalsJSON, &v.CachedAt, &v.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CachedVerdict{}, ErrCacheMiss // absent OR expired => honest miss, never stale
	}
	if err != nil {
		return CachedVerdict{}, fmt.Errorf("store: cache read: %w", err)
	}
	if mxProvider != nil {
		v.MXProvider = *mxProvider
	}
	if err := json.Unmarshal(flagsJSON, &v.Flags); err != nil {
		return CachedVerdict{}, fmt.Errorf("store: cache flags: %w", err)
	}
	if err := json.Unmarshal(signalsJSON, &v.Signals); err != nil {
		return CachedVerdict{}, fmt.Errorf("store: cache signals: %w", err)
	}
	return v, nil
}

func (p *PG) PutCachedVerdict(ctx context.Context, v CachedVerdict) error {
	flagsJSON, err := json.Marshal(orEmptyFlags(v.Flags))
	if err != nil {
		return fmt.Errorf("store: marshal flags: %w", err)
	}
	signalsJSON, err := json.Marshal(orEmptySignals(v.Signals))
	if err != nil {
		return fmt.Errorf("store: marshal signals: %w", err)
	}
	const q = `
INSERT INTO cache (
  email_hash, config_fingerprint, domain, mx_provider, hash_scheme,
  status, reason, confidence, recommendation, flags, signals, cached_at, expires_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
ON CONFLICT (email_hash, config_fingerprint) DO UPDATE SET
  domain=EXCLUDED.domain, mx_provider=EXCLUDED.mx_provider, hash_scheme=EXCLUDED.hash_scheme,
  status=EXCLUDED.status, reason=EXCLUDED.reason, confidence=EXCLUDED.confidence,
  recommendation=EXCLUDED.recommendation, flags=EXCLUDED.flags, signals=EXCLUDED.signals,
  cached_at=EXCLUDED.cached_at, expires_at=EXCLUDED.expires_at`
	if _, err := p.pool.Exec(ctx, q,
		v.EmailHash, v.ConfigFingerprint, v.Domain, nullStr(v.MXProvider), v.HashScheme,
		v.Status, v.Reason, v.Confidence, v.Recommendation, flagsJSON, signalsJSON, v.CachedAt.UTC(), v.ExpiresAt.UTC(),
	); err != nil {
		return fmt.Errorf("store: cache write: %w", err)
	}
	return nil
}

func (p *PG) existsDomain(ctx context.Context, table, domain string) (bool, error) {
	var one int
	// table is a fixed internal literal (never user input) — no injection surface.
	err := p.pool.QueryRow(ctx, "SELECT 1 FROM "+table+" WHERE domain=$1", domain).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: %s lookup: %w", table, err)
	}
	return true, nil
}

func (p *PG) IsDisposable(ctx context.Context, domain string) (bool, error) {
	return p.existsDomain(ctx, "disposable_domains", domain)
}
func (p *PG) IsSpamTrap(ctx context.Context, domain string) (bool, error) {
	return p.existsDomain(ctx, "spam_trap_domains", domain)
}

func (p *PG) CatchAllState(ctx context.Context, domain string) (bool, bool, error) {
	var isCatchall *bool
	err := p.pool.QueryRow(ctx, "SELECT is_catchall FROM catchall_domains WHERE domain=$1", domain).Scan(&isCatchall)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil // never probed (§4.3 tri-state)
	}
	if err != nil {
		return false, false, fmt.Errorf("store: catchall lookup: %w", err)
	}
	if isCatchall == nil {
		return false, false, nil // row exists but is_catchall NULL => still unknown
	}
	return *isCatchall, true, nil
}

func (p *PG) GetDefaultPolicy(ctx context.Context) (PolicyRow, error) {
	const q = `SELECT key, version, definition, is_default FROM policies WHERE is_default = true`
	return p.scanPolicy(p.pool.QueryRow(ctx, q))
}

func (p *PG) GetPolicy(ctx context.Context, key string, version int) (PolicyRow, error) {
	const q = `SELECT key, version, definition, is_default FROM policies WHERE key=$1 AND version=$2`
	return p.scanPolicy(p.pool.QueryRow(ctx, q, key, version))
}

// EnsurePolicy inserts a policy row if that (key, version) is not already present, and sets
// it as the default when asDefault is true and no default yet exists. Idempotent: re-running
// is a no-op. Policies are immutable (§4.6) — this never overwrites an existing definition.
// It is how the code-owned default policy (the cold-email constant) reaches the store.
func (p *PG) EnsurePolicy(ctx context.Context, key string, version int, definition []byte, asDefault bool) error {
	// Insert the policy row if absent (immutable — ON CONFLICT DO NOTHING).
	if _, err := p.pool.Exec(ctx,
		`INSERT INTO policies (key, version, definition, is_default) VALUES ($1,$2,$3,false)
		 ON CONFLICT (key, version) DO NOTHING`,
		key, version, definition,
	); err != nil {
		return fmt.Errorf("store: ensure policy: %w", err)
	}
	if !asDefault {
		return nil
	}
	// Set as default only if there is no default yet (the partial-unique index guarantees at
	// most one; this avoids fighting an operator-chosen default).
	if _, err := p.pool.Exec(ctx,
		`UPDATE policies SET is_default=true
		 WHERE key=$1 AND version=$2
		   AND NOT EXISTS (SELECT 1 FROM policies WHERE is_default=true)`,
		key, version,
	); err != nil {
		return fmt.Errorf("store: set default policy: %w", err)
	}
	return nil
}

func (p *PG) scanPolicy(row pgx.Row) (PolicyRow, error) {
	var pr PolicyRow
	err := row.Scan(&pr.Key, &pr.Version, &pr.Definition, &pr.IsDefault)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyRow{}, ErrNotWired
	}
	if err != nil {
		return PolicyRow{}, fmt.Errorf("store: policy read: %w", err)
	}
	return pr, nil
}

func orEmptyFlags(f map[string]bool) map[string]bool {
	if f == nil {
		return map[string]bool{}
	}
	return f
}
func orEmptySignals(s map[string]any) map[string]any {
	if s == nil {
		return map[string]any{}
	}
	return s
}

// compile-time assertion that PG satisfies Store.
var _ Store = (*PG)(nil)
