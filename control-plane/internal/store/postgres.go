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
	"time"

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

// SetCatchAll upserts a domain's catch-all conclusion (P2 §5.6 — brain-side; migration 0006
// grants cp_runtime INSERT+UPDATE, never DELETE). A nil isCatchall (inconclusive) is a NO-OP:
// it must never overwrite a previously-determined value with unknown (§5.6). checked_at is now();
// method ∈ probe|seed|manual. On conflict it updates is_catchall/detected_at/method/expires_at.
func (p *PG) SetCatchAll(ctx context.Context, domain string, isCatchall *bool, method string, expiresAt time.Time) error {
	if isCatchall == nil {
		return nil // inconclusive → never overwrite a known value with NULL (§5.6)
	}
	const q = `
INSERT INTO catchall_domains (domain, is_catchall, detected_at, method, expires_at)
VALUES ($1, $2, now(), $3, $4)
ON CONFLICT (domain) DO UPDATE SET
  is_catchall = EXCLUDED.is_catchall,
  detected_at = EXCLUDED.detected_at,
  method      = EXCLUDED.method,
  expires_at  = EXCLUDED.expires_at`
	var exp any
	if !expiresAt.IsZero() {
		exp = expiresAt.UTC()
	}
	if _, err := p.pool.Exec(ctx, q, domain, *isCatchall, nullStr(method), exp); err != nil {
		return fmt.Errorf("store: set catchall: %w", err)
	}
	return nil
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

// ── Phase 3 (§4.3/§4.4/§5.2) — node health, append-only audit history, durable warmup ──────

// AppendNodeAudit INSERTs the append-only audit-history row AND upserts the node's summary health
// columns (audit_status/last_audit_at/current_host_ip) in ONE transaction so the latest-audit
// summary on `nodes` and the `node_audits` history can never diverge (P3 §4.3). It does NOT touch
// status/port25_open — those move via UpsertNodeState (lifecycle) / the egress smoke test (§3.4).
// current_host_ip is written as inet; "" => NULL (§6.6.1 anchor, cross-checked at routing time).
func (p *PG) AppendNodeAudit(ctx context.Context, row NodeAuditRow) error {
	warningsJSON, err := json.Marshal(orEmptySignals(row.Warnings))
	if err != nil {
		return fmt.Errorf("store: marshal audit warnings: %w", err)
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin node audit: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after a successful commit is a no-op

	if _, err := tx.Exec(ctx,
		`INSERT INTO node_audits (id, node_id, audited_at, audit_status, warnings, current_host_ip)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		row.ID, row.NodeID, row.AuditedAt.UTC(), row.AuditStatus, warningsJSON, nullStr(row.CurrentHostIP),
	); err != nil {
		return fmt.Errorf("store: insert node audit: %w", err)
	}
	// Update the summary columns on `nodes` (row must already exist via UpsertNodeState at boot).
	// This never creates the row — an audit for an unregistered node is a no-op on the summary
	// (the history row is still recorded). The gate reads GetNodeHealth, backed by these columns.
	if _, err := tx.Exec(ctx,
		`UPDATE nodes SET audit_status=$2, last_audit_at=$3 WHERE id=$1`,
		row.NodeID, row.AuditStatus, row.AuditedAt.UTC(),
	); err != nil {
		return fmt.Errorf("store: update node audit summary: %w", err)
	}
	return tx.Commit(ctx)
}

// GetNodeHealth reads the gate-relevant state. It joins the `nodes` summary with the latest
// node_audits row for the current_host_ip (the §6.6.1 GAP-24 anchor lives only in the history).
func (p *PG) GetNodeHealth(ctx context.Context, nodeID string) (NodeHealth, bool, error) {
	const q = `
SELECT n.status, COALESCE(n.audit_status,''), n.last_audit_at, COALESCE(n.port25_open,false),
       COALESCE(host(a.current_host_ip),'')
FROM nodes n
LEFT JOIN LATERAL (
  SELECT current_host_ip FROM node_audits WHERE node_id = n.id ORDER BY audited_at DESC LIMIT 1
) a ON true
WHERE n.id=$1`
	var h NodeHealth
	var lastAuditAt *time.Time
	err := p.pool.QueryRow(ctx, q, nodeID).Scan(&h.Status, &h.AuditStatus, &lastAuditAt, &h.Port25Open, &h.LastAuditHostIP)
	if errors.Is(err, pgx.ErrNoRows) {
		return NodeHealth{}, false, nil // never mirrored/audited → router treats as not eligible
	}
	if err != nil {
		return NodeHealth{}, false, fmt.Errorf("store: node health read: %w", err)
	}
	h.NodeID = nodeID
	if lastAuditAt != nil {
		h.LastAuditAt = *lastAuditAt
	}
	return h, true, nil
}

// UpsertNodeState mirrors the config descriptor + writes lifecycle status/port25_open (§3.6/§4.5).
// It preserves audit_status/last_audit_at (a separate write path). Never deletes (migration 0007).
func (p *PG) UpsertNodeState(ctx context.Context, nodeID, baseURL, status string, port25Open bool) error {
	const q = `
INSERT INTO nodes (id, base_url, status, port25_open, enabled)
VALUES ($1,$2,$3,$4,true)
ON CONFLICT (id) DO UPDATE SET
  base_url = EXCLUDED.base_url,
  status = EXCLUDED.status,
  port25_open = EXCLUDED.port25_open`
	if _, err := p.pool.Exec(ctx, q, nodeID, baseURL, status, port25Open); err != nil {
		return fmt.Errorf("store: upsert node state: %w", err)
	}
	return nil
}

// ReserveWarmupSlot atomically reserves one :smtp slot against the window cap (§5.2). The
// increment is a single atomic upsert: it creates the window row at count=1 if absent, else
// increments count ONLY while count < cap (the WHERE guard). RowsAffected==0 on the UPDATE path
// means the window is at cap → Allowed=false (caller degrades to cheap, warmup_cap). Durable in
// Postgres so a brain restart can't reset a fresh IP's ramp mid-warmup (§5.2 LOCKED / PM4).
func (p *PG) ReserveWarmupSlot(ctx context.Context, nodeID string, windowStart time.Time, cap int) (WarmupDecision, error) {
	ws := windowStart.UTC()
	if cap <= 0 {
		// A cap of 0 means "no :smtp allowance" — deny without an INSERT (the INSERT path below has
		// no WHERE guard, so a fresh count=1 would otherwise slip past a 0 cap). Never create a row.
		return WarmupDecision{Allowed: false, Count: 0, Cap: cap, WindowStart: ws}, nil
	}
	// Single statement: INSERT ... ON CONFLICT DO UPDATE with a guard that only increments below
	// cap, and RETURNING the resulting count so we know whether the reservation was granted.
	const q = `
INSERT INTO node_warmup (node_id, window_start, count, cap, updated_at)
VALUES ($1, $2, 1, $3, now())
ON CONFLICT (node_id, window_start) DO UPDATE SET
  count = node_warmup.count + 1,
  cap = EXCLUDED.cap,
  updated_at = now()
WHERE node_warmup.count < EXCLUDED.cap
RETURNING count, cap`
	var count, gotCap int
	err := p.pool.QueryRow(ctx, q, nodeID, ws, cap).Scan(&count, &gotCap)
	if errors.Is(err, pgx.ErrNoRows) {
		// The WHERE guard blocked the UPDATE (window at cap). Read back the current count for the
		// denied decision (cap<=0 was already handled above, so a row exists here).
		var curCount, curCap int
		rerr := p.pool.QueryRow(ctx,
			`SELECT count, cap FROM node_warmup WHERE node_id=$1 AND window_start=$2`, nodeID, ws).
			Scan(&curCount, &curCap)
		if rerr != nil {
			return WarmupDecision{}, fmt.Errorf("store: warmup read-after-deny: %w", rerr)
		}
		return WarmupDecision{Allowed: false, Count: curCount, Cap: curCap, WindowStart: ws}, nil
	}
	if err != nil {
		return WarmupDecision{}, fmt.Errorf("store: reserve warmup: %w", err)
	}
	return WarmupDecision{Allowed: true, Count: count, Cap: gotCap, WindowStart: ws}, nil
}

// ── Phase 4 (§4.4/§5.2/§5.4/§5.6) — durable rate budget, block-rate, lifecycle ──────────

// reserveRate is the shared body of ReserveRateToken/PeekRateBudget: it SELECT … FOR UPDATEs the
// bucket row inside a tx, computes the refill via the SHARED applyRefill helper (identical math to
// the Mem fake — the P3 test-double lesson), optionally consumes one token, and upserts the new
// state. FOR UPDATE serializes concurrent admits on the same bucket (a single brain, but multiple
// request goroutines) so two requests can't both spend the last token. A never-seen bucket is
// treated as FULL at capacity (mirrors Mem) — a fresh node has full burst headroom.
//
// Cold-start race (Phase-4 round-2 review): SELECT ... FOR UPDATE cannot lock a NONEXISTENT row (no
// gap locks under READ COMMITTED), so two concurrent first-admits to a never-seen bucket would both
// see ErrNoRows and race on a guardless INSERT → unique_violation → a spurious rate_limited degrade.
// Fixed by pre-creating the row IDEMPOTENTLY (INSERT ... ON CONFLICT DO NOTHING at full capacity)
// BEFORE the SELECT FOR UPDATE — after that the row always exists, so FOR UPDATE always locks and the
// bare-INSERT branch is gone. Mirrors ReserveWarmupSlot's ON CONFLICT discipline.
func (p *PG) reserveRate(ctx context.Context, nodeID, providerBucket string, pp RateParams, now time.Time, consume bool) (RateDecision, error) {
	if pp.Capacity <= 0 {
		return RateDecision{Allowed: false, TokensAfter: 0, Capacity: pp.Capacity}, nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return RateDecision{}, fmt.Errorf("store: begin rate: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after a successful commit is a no-op

	// Idempotent pre-create: a never-seen bucket starts FULL at capacity (mirrors Mem). ON CONFLICT
	// DO NOTHING makes concurrent first-touches safe — the loser is a no-op, not a unique_violation.
	if _, err := tx.Exec(ctx,
		`INSERT INTO rate_budgets (node_id, provider_bucket, tokens, capacity, refill_per_sec, last_refill_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,now())
		 ON CONFLICT (node_id, provider_bucket) DO NOTHING`,
		nodeID, providerBucket, pp.Capacity, pp.Capacity, pp.RefillPerSec, now.UTC(),
	); err != nil {
		return RateDecision{}, fmt.Errorf("store: rate pre-create: %w", err)
	}

	// The row now exists → FOR UPDATE always locks it (serializes concurrent admits on this bucket).
	var stored float64
	var lastRefill time.Time
	if err := tx.QueryRow(ctx,
		`SELECT tokens, last_refill_at FROM rate_budgets WHERE node_id=$1 AND provider_bucket=$2 FOR UPDATE`,
		nodeID, providerBucket).Scan(&stored, &lastRefill); err != nil {
		return RateDecision{}, fmt.Errorf("store: rate select: %w", err)
	}

	tokens, canGrant := applyRefill(stored, lastRefill, pp, now)
	allowed := canGrant
	if consume && canGrant {
		tokens -= 1.0
	} else if consume {
		allowed = false // no whole token → deny (still persist the read-side refill below)
	}

	// Persist the refill + any decrement. capacity/refill are refreshed from the caller's params (a
	// policy edit takes effect next call, mirroring ReserveWarmupSlot's per-call cap).
	if _, err := tx.Exec(ctx,
		`UPDATE rate_budgets SET tokens=$3, capacity=$4, refill_per_sec=$5, last_refill_at=$6, updated_at=now()
		 WHERE node_id=$1 AND provider_bucket=$2`,
		nodeID, providerBucket, tokens, pp.Capacity, pp.RefillPerSec, now.UTC(),
	); err != nil {
		return RateDecision{}, fmt.Errorf("store: rate update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RateDecision{}, fmt.Errorf("store: rate commit: %w", err)
	}
	return RateDecision{Allowed: allowed, TokensAfter: tokens, Capacity: pp.Capacity}, nil
}

// ReserveRateToken consumes one token from the durable bucket (§4.4). Restart-safe: the refill is
// derived from real elapsed time since last_refill_at (a brain restart never resets to full — the
// PM4 reputation hazard the durable state prevents).
func (p *PG) ReserveRateToken(ctx context.Context, nodeID, providerBucket string, pp RateParams, now time.Time) (RateDecision, error) {
	return p.reserveRate(ctx, nodeID, providerBucket, pp, now, true)
}

// PeekRateBudget refills-and-reads WITHOUT consuming (§3.3 headroom check).
func (p *PG) PeekRateBudget(ctx context.Context, nodeID, providerBucket string, pp RateParams, now time.Time) (RateDecision, error) {
	return p.reserveRate(ctx, nodeID, providerBucket, pp, now, false)
}

// BlockRateByVerifierIP aggregates per-node block-rate from the LOG over [since, now] (§5.2/§5.6).
// It counts ONLY from_cache=false rows with a non-NULL verifier_ip — cache hits ran no SMTP and
// cheap-degrades have verifier_ip NULL (§4.6), so neither can inflate/deflate a node's rate (PB1).
// smtp_blocked is the primary numerator; smtp_timeout/connection_failed the secondary; greylisted
// is NOT counted (OQ-P4-6). One row per verifier_ip with ≥1 attempt in the window.
func (p *PG) BlockRateByVerifierIP(ctx context.Context, since time.Time) ([]NodeBlockStats, error) {
	const q = `
SELECT host(verifier_ip) AS ip,
       count(*)                                                        AS attempts,
       count(*) FILTER (WHERE reason = 'smtp_blocked')                 AS blocked,
       count(*) FILTER (WHERE reason IN ('smtp_timeout','connection_failed')) AS timeouts
FROM verifications
WHERE from_cache = false
  AND verifier_ip IS NOT NULL
  AND checked_at >= $1
GROUP BY verifier_ip
ORDER BY ip`
	rows, err := p.pool.Query(ctx, q, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("store: block-rate query: %w", err)
	}
	defer rows.Close()
	var out []NodeBlockStats
	for rows.Next() {
		var s NodeBlockStats
		if err := rows.Scan(&s.VerifierIP, &s.Attempts, &s.Blocked, &s.Timeouts); err != nil {
			return nil, fmt.Errorf("store: block-rate scan: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: block-rate rows: %w", err)
	}
	return out, nil
}

// MetricRowsSince reads a bounded window of LOG rows for the observability metrics (§8), newest
// first, capped at limit+1 so we can detect truncation. Derived from existing verifications columns
// (Door 2, no schema change). limit<=0 uses 5000. It is a read-only projection; control decisions
// never depend on it (OQ-P4-11 — block-rate uses BlockRateByVerifierIP).
func (p *PG) MetricRowsSince(ctx context.Context, since time.Time, limit int) ([]MetricRow, bool, error) {
	if limit <= 0 {
		limit = 5000
	}
	const q = `
SELECT checked_at, status, reason, COALESCE(mx_provider,''), COALESCE(host(verifier_ip),''),
       from_cache, COALESCE(latency_ms,0), signals
FROM verifications
WHERE checked_at >= $1
ORDER BY checked_at DESC
LIMIT $2`
	rows, err := p.pool.Query(ctx, q, since.UTC(), limit+1) // +1 to detect truncation
	if err != nil {
		return nil, false, fmt.Errorf("store: metric rows query: %w", err)
	}
	defer rows.Close()
	var out []MetricRow
	for rows.Next() {
		var r MetricRow
		var signalsJSON []byte
		if err := rows.Scan(&r.CheckedAt, &r.Status, &r.Reason, &r.MXProvider, &r.VerifierIP, &r.FromCache, &r.LatencyMS, &signalsJSON); err != nil {
			return nil, false, fmt.Errorf("store: metric row scan: %w", err)
		}
		if len(signalsJSON) > 0 {
			_ = json.Unmarshal(signalsJSON, &r.Signals) // best-effort; a bad blob just drops the signal metric
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: metric rows: %w", err)
	}
	truncated := len(out) > limit
	if truncated {
		out = out[:limit]
	}
	return out, truncated, nil
}

// SetNodeStatus writes ONLY the lifecycle status column (§5.4) — the health-watcher's single-writer
// transition. It never touches port25_open/audit columns. WHERE id=$1 matching 0 rows (unregistered
// node) is a no-op (mirrors Mem + AppendNodeAudit's summary posture). Never deletes.
func (p *PG) SetNodeStatus(ctx context.Context, nodeID, status string) error {
	if _, err := p.pool.Exec(ctx, `UPDATE nodes SET status=$2 WHERE id=$1`, nodeID, status); err != nil {
		return fmt.Errorf("store: set node status: %w", err)
	}
	return nil
}

// ResetWarmup resets the node's warmup counter for the window to count=0 at the given cap (§5.4/PM3
// canary recovery). Upserts the window row (idempotent).
func (p *PG) ResetWarmup(ctx context.Context, nodeID string, windowStart time.Time, cap int) error {
	const q = `
INSERT INTO node_warmup (node_id, window_start, count, cap, updated_at)
VALUES ($1, $2, 0, $3, now())
ON CONFLICT (node_id, window_start) DO UPDATE SET count = 0, cap = EXCLUDED.cap, updated_at = now()`
	if _, err := p.pool.Exec(ctx, q, nodeID, windowStart.UTC(), cap); err != nil {
		return fmt.Errorf("store: reset warmup: %w", err)
	}
	return nil
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
