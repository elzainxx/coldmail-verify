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
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PG is the pgx-pool-backed Store.
type PG struct {
	pool *pgxpool.Pool
}

// OpenPG connects a pool to the given DSN and pings it. The caller owns Close().
//
// NEITHER error wraps the driver error directly (fix-plan U2 / D12). pgx v5.10.0 embeds the WHOLE
// connection string in `ParseConfigError.Error()`, and its own redaction is incomplete: `redactPW`
// takes the URL branch for any `postgres://` DSN and then `redactURL` returns `u.String()`, so
// `RawQuery` is emitted verbatim — a password carried as a QUERY PARAMETER (`?password=`,
// `?sslpassword=`, both honored by libpq/pgx) is logged in clear; and the keyword/value branch's
// `password=[^ ]*` regex cannot match the honored spaced spelling `password = SECRET`. Verified
// first-hand against the pinned version: those three shapes leak, userinfo and unspaced kv do not.
// The caller at cmd/control-plane logs this error at boot, so a leak lands in the process log — the
// most-collected, longest-lived surface there is.
//
// The defence is scrubDSNError, applied HERE rather than at the log call: a fix at the call site
// protects only that one caller, and the next `log.Printf("%v", err)` reintroduces it. The DSN is
// not needed in the message either way — the operator already knows what they configured, and the
// failure they need is the reason, not the credential.
func OpenPG(ctx context.Context, dsn string) (*PG, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: pgx pool: %w", scrubDSNError(err, dsn))
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", scrubDSNError(err, dsn))
	}
	return &PG{pool: pool}, nil
}

// scrubbedDSNError replaces a driver error whose text embeds the DSN. It keeps Unwrap so
// errors.Is/As still reach the original (a caller may legitimately test for a pgx sentinel); only
// the STRING — the thing that reaches a log — is replaced.
type scrubbedDSNError struct {
	err error
	msg string
}

func (e *scrubbedDSNError) Error() string { return e.msg }
func (e *scrubbedDSNError) Unwrap() error { return e.err }

// scrubDSNError returns err unchanged when its text cannot expose the DSN, and a scrubbed
// replacement when it can.
//
// The test is DERIVED, not a pattern list: it asks whether the error text contains any credential
// candidate actually present in the DSN we were handed. That is why it catches shapes nobody
// enumerated — a future pgx release that leaks a new field, or a DSN keyword this code has never
// heard of, is still caught, because the check is "does the message contain one of MY secrets"
// rather than "does the message match one of THEIR known-bad spellings". A blocklist of `password=`
// spellings is exactly how pgx's own redaction came to miss two.
func scrubDSNError(err error, dsn string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	// The DSN appearing whole is the common case and is enough on its own: user/host/port/database
	// are not credentials but they are infrastructure detail with no business in a log line.
	leaks := strings.Contains(msg, dsn)
	if !leaks {
		for _, cand := range dsnCredentialCandidates(dsn) {
			if strings.Contains(msg, cand) {
				leaks = true
				break
			}
		}
	}
	if !leaks {
		return err
	}
	// Deliberately NOT a partial redaction of the message. Selective rewriting is what failed
	// upstream: it leaves whatever the pattern missed. Replace the whole text and name the one fact
	// an operator needs — which env var to look at.
	return &scrubbedDSNError{
		err: err,
		msg: "connection string rejected by the driver (message suppressed: it embedded the DSN — check TM_DATABASE_URL)",
	}
}

// structuralDSNKeys are the libpq/pgx connection keywords whose VALUES cannot be a credential: they
// name where the database is and how to reach it, not who you are.
//
// THE DIRECTION OF RISK IS DELIBERATE, AND IT IS THE OPPOSITE OF A BLOCKLIST'S. A key MISSING from
// this map is over-collection: its value becomes a candidate, and the cost is one error message
// losing its detail. A key WRONGLY PRESENT is under-collection, and that costs a credential — so this
// map contains only keywords whose libpq semantics make a secret value impossible. `password` and
// `sslpassword` are deliberately absent, as is every keyword nobody has enumerated: an unknown key is
// treated as possibly-secret, which is what keeps the check DERIVED rather than a blocklist of
// known-bad spellings, and what TestDSNRedaction_UnknownKeywordShapeStillCaught pins.
//
// `sslkey` and `passfile` are PATHS here, not secrets — the file they point at holds the secret, and
// a path in a log line is infrastructure detail, not a credential.
var structuralDSNKeys = map[string]bool{
	"host": true, "hostaddr": true, "port": true, "dbname": true, "database": true,
	"user": true, "application_name": true, "fallback_application_name": true,
	"connect_timeout": true, "client_encoding": true, "options": true, "service": true,
	"sslmode": true, "sslrootcert": true, "sslcert": true, "sslcrl": true, "sslcrldir": true,
	"sslsni": true, "sslnegotiation": true, "sslkey": true, "passfile": true,
	"target_session_attrs": true, "load_balance_hosts": true, "channel_binding": true,
	"require_auth": true, "gssencmode": true, "gsslib": true, "krbsrvname": true,
	"replication": true, "tcp_user_timeout": true,
	"keepalives": true, "keepalives_idle": true, "keepalives_interval": true, "keepalives_count": true,
	"pool_max_conns": true, "pool_min_conns": true, "pool_min_idle_conns": true,
	"pool_max_conn_lifetime": true, "pool_max_conn_lifetime_jitter": true,
	"pool_max_conn_idle_time": true, "pool_health_check_period": true,
	"search_path": true,
}

// knownDSNKey is structuralDSNKeys plus the credential-bearing keywords. Used only to recognise a
// BARE token as a key awaiting its spaced '=' rather than as a value.
func knownDSNKey(s string) bool {
	return structuralDSNKeys[s] || s == "password" || s == "sslpassword"
}

// dsnCredentialCandidates extracts every substring of the DSN that could be a secret, in every
// spelling pgx/libpq honors: URL userinfo password, URL query values, and keyword/value values
// (quoted, unquoted, spaced or not).
//
// THE SPACED SPELLING IS WHY THIS TRACKS STATE, AND WHY structuralDSNKeys EXISTS. libpq honors
// `password = SECRET`, which splits into three whitespace fields, so the value arrives detached from
// its key. The first version handled that by collecting EVERY bare token and every `key=value` value
// regardless of the key — and MEASURED at the pin on 2026-08-07, that suppressed FIVE of six
// credential-free messages an operator actually needs:
//
//	dsn                                        message                                    suppressed by
//	host=h port=5432 dbname=verifydb …         dial tcp 10.0.0.5:5432: connection refused  "5432"
//	host=db.internal port=5432 dbname=…        lookup db.internal: no such host            "db.internal"
//	host=h … user=verify_app password = …      password authentication failed for "…"      "verify_app"
//	host=h … sslmode=require                   failed to configure TLS (sslmode invalid)   "require"
//	host=h port=5432 dbname=verifydb user=…    failed to connect to `user=… database=…`     "verifydb"
//
// Every one of those is replaced by "connection string rejected by the driver", so an operator whose
// database is simply DOWN is told their connection string is malformed. Blinding every operator to
// every database error is not a conservative failure — it is a different outage.
// TestDSNRedaction_ShortValuesDoNotSuppressEverything did not catch it because its fixture is the URL
// form `?sslmode=disable`, whose one extractable value never appears in a connect error; the KV form
// a real deployment uses is what breaks. TestDSNRedaction_StructuralValuesNeverSuppress is the
// generalisation, with both spellings, so a fixture that happens to dodge the bug cannot pass for the
// property again.
func dsnCredentialCandidates(dsn string) []string {
	var out []string
	add := func(s string) {
		// Under 4 chars a "secret" is more likely to appear incidentally in a driver message
		// ("db", "u") than to be a real credential, and matching it would suppress every error. This
		// is a backstop BENEATH structuralDSNKeys, not the primary defence — 5432 is four characters,
		// which is exactly why the length floor alone was not enough.
		if len(s) >= 4 {
			out = append(out, s)
		}
	}
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" {
		if pw, ok := u.User.Password(); ok {
			add(pw)
		}
		for k, vs := range u.Query() {
			if structuralDSNKeys[k] {
				continue
			}
			for _, v := range vs {
				add(v)
			}
		}
	}
	// Keyword/value form, and the query-ish tail of a URL that failed to parse.
	var pendingKey string
	for _, field := range strings.FieldsFunc(dsn, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '&' || r == '?'
	}) {
		if eq := strings.IndexByte(field, '='); eq >= 0 {
			key := strings.TrimSpace(field[:eq])
			val := strings.Trim(strings.TrimSpace(field[eq+1:]), `'"`)
			if key == "" {
				// A lone "=" or "=VALUE": the key was the previous field (`password = SECRET`).
				key = pendingKey
			}
			if val == "" {
				// `password =` — the value is the next field.
				pendingKey = key
				continue
			}
			pendingKey = ""
			if !structuralDSNKeys[key] {
				add(val)
			}
			continue
		}
		token := strings.Trim(field, `'"`)
		if pendingKey != "" {
			// The detached value of a spaced pair.
			if !structuralDSNKeys[pendingKey] {
				add(token)
			}
			pendingKey = ""
			continue
		}
		if knownDSNKey(token) {
			// A key awaiting its spaced '='. Not a value, so not a candidate — and collecting it would
			// make the literal word "password" a candidate, which suppresses every
			// `password authentication failed for user "u"` the server ever returns.
			pendingKey = token
			continue
		}
		// An unrecognised bare token: possibly the value of a spaced pair for a keyword this code has
		// never heard of. Over-collect, per the risk direction documented on structuralDSNKeys.
		add(token)
	}
	return out
}

func (p *PG) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }
func (p *PG) Close()                         { p.pool.Close() }

// Pool exposes the underlying pgx pool for INTEGRATION TESTS + break-glass admin tooling ONLY
// (e.g. an external-package E2E test that seeds/inspects/cleans rows the append-only interface
// deliberately can't). It is NOT part of the Store interface and MUST NOT be used on the request
// path — the append-only Door-2 discipline lives in the interface shape + the DB grants, both of
// which still apply to whatever this pool runs (verify_app cannot UPDATE/DELETE the LOG regardless).
func (p *PG) Pool() *pgxpool.Pool { return p.pool }

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
	// A raw sidecar row (when an engine was consulted) must be atomic with the LOG row, so a crash
	// never leaves a blob without its verdict or vice-versa. Wrap both in one tx. The INSERT body
	// is shared with the P5 queue settlement (insertVerificationTx) so the LOG-row shape can't drift.
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after a successful commit is a no-op
	if err := insertVerificationTx(ctx, tx, row, raw); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *PG) GetCachedVerdict(ctx context.Context, emailHash []byte, cfg string) (CachedVerdict, error) {
	const q = `
SELECT email_hash, config_fingerprint, domain, mx_provider, hash_scheme,
       status, reason, confidence, recommendation, flags, signals, cached_at, expires_at
FROM verify.cache WHERE email_hash=$1 AND config_fingerprint=$2 AND expires_at > now()`
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
INSERT INTO verify.cache (
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
	err := p.pool.QueryRow(ctx, "SELECT 1 FROM verify."+table+" WHERE domain=$1", domain).Scan(&one)
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
	err := p.pool.QueryRow(ctx, "SELECT is_catchall FROM verify.catchall_domains WHERE domain=$1", domain).Scan(&isCatchall)
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
// grants verify_app INSERT+UPDATE, never DELETE). A nil isCatchall (inconclusive) is a NO-OP:
// it must never overwrite a previously-determined value with unknown (§5.6). checked_at is now();
// method ∈ probe|seed|manual. On conflict it updates is_catchall/detected_at/method/expires_at.
func (p *PG) SetCatchAll(ctx context.Context, domain string, isCatchall *bool, method string, expiresAt time.Time) error {
	if isCatchall == nil {
		return nil // inconclusive → never overwrite a known value with NULL (§5.6)
	}
	const q = `
INSERT INTO verify.catchall_domains (domain, is_catchall, detected_at, method, expires_at)
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
	const q = `SELECT key, version, definition, is_default FROM verify.policies WHERE is_default = true`
	return p.scanPolicy(p.pool.QueryRow(ctx, q))
}

func (p *PG) GetPolicy(ctx context.Context, key string, version int) (PolicyRow, error) {
	const q = `SELECT key, version, definition, is_default FROM verify.policies WHERE key=$1 AND version=$2`
	return p.scanPolicy(p.pool.QueryRow(ctx, q, key, version))
}

// EnsurePolicy inserts a policy row if that (key, version) is not already present, and sets
// it as the default when asDefault is true and no default yet exists. Idempotent: re-running
// is a no-op. Policies are immutable (§4.6) — this never overwrites an existing definition.
// It is how the code-owned default policy (the cold-email constant) reaches the store.
func (p *PG) EnsurePolicy(ctx context.Context, key string, version int, definition []byte, asDefault bool) error {
	// Insert the policy row if absent (immutable — ON CONFLICT DO NOTHING).
	if _, err := p.pool.Exec(ctx,
		`INSERT INTO verify.policies (key, version, definition, is_default) VALUES ($1,$2,$3,false)
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
		`UPDATE verify.policies SET is_default=true
		 WHERE key=$1 AND version=$2
		   AND NOT EXISTS (SELECT 1 FROM verify.policies WHERE is_default=true)`,
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
		`INSERT INTO verify.node_audits (id, node_id, audited_at, audit_status, warnings, current_host_ip)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		row.ID, row.NodeID, row.AuditedAt.UTC(), row.AuditStatus, warningsJSON, nullStr(row.CurrentHostIP),
	); err != nil {
		return fmt.Errorf("store: insert node audit: %w", err)
	}
	// Update the summary columns on `nodes` (row must already exist via UpsertNodeState at boot).
	// This never creates the row — an audit for an unregistered node is a no-op on the summary
	// (the history row is still recorded). The gate reads GetNodeHealth, backed by these columns.
	if _, err := tx.Exec(ctx,
		`UPDATE verify.nodes SET audit_status=$2, last_audit_at=$3 WHERE id=$1`,
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
FROM verify.nodes n
LEFT JOIN LATERAL (
  SELECT current_host_ip FROM verify.node_audits WHERE node_id = n.id ORDER BY audited_at DESC LIMIT 1
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
INSERT INTO verify.nodes (id, base_url, status, port25_open, enabled)
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
INSERT INTO verify.node_warmup (node_id, window_start, count, cap, updated_at)
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
			`SELECT count, cap FROM verify.node_warmup WHERE node_id=$1 AND window_start=$2`, nodeID, ws).
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
		`INSERT INTO verify.rate_budgets (node_id, provider_bucket, tokens, capacity, refill_per_sec, last_refill_at, updated_at)
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
		`SELECT tokens, last_refill_at FROM verify.rate_budgets WHERE node_id=$1 AND provider_bucket=$2 FOR UPDATE`,
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
		`UPDATE verify.rate_budgets SET tokens=$3, capacity=$4, refill_per_sec=$5, last_refill_at=$6, updated_at=now()
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
FROM verify.verifications
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
FROM verify.verifications
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
	if _, err := p.pool.Exec(ctx, `UPDATE verify.nodes SET status=$2 WHERE id=$1`, nodeID, status); err != nil {
		return fmt.Errorf("store: set node status: %w", err)
	}
	return nil
}

// ResetWarmup resets the node's warmup counter for the window to count=0 at the given cap (§5.4/PM3
// canary recovery). Upserts the window row (idempotent).
func (p *PG) ResetWarmup(ctx context.Context, nodeID string, windowStart time.Time, cap int) error {
	const q = `
INSERT INTO verify.node_warmup (node_id, window_start, count, cap, updated_at)
VALUES ($1, $2, 0, $3, now())
ON CONFLICT (node_id, window_start) DO UPDATE SET count = 0, cap = EXCLUDED.cap, updated_at = now()`
	if _, err := p.pool.Exec(ctx, q, nodeID, windowStart.UTC(), cap); err != nil {
		return fmt.Errorf("store: reset warmup: %w", err)
	}
	return nil
}

// ── Phase 5 (§3.3/§5/§7) — the durable async queue ──────────────────────────────────────────

// EnqueueWorkItem inserts a work-item, deduping on idempotency_key (§5.5) WITHIN ITS JOB (0018, owner
// ruling A2). ON CONFLICT DO NOTHING makes a duplicate (same address×policy×config in the SAME job) a
// no-op returning alreadyExists=true and the id of the work item that already holds the key. The
// conflict arbiter is the PER-JOB partial unique index (job_id, idempotency_key) WHERE job_id IS NOT
// NULL; a different job, of any owner, never conflicts, so the same address always gets its own work
// item and its own result there. An item with no job (a singular queued check) is arbitrated by
// idempotency_key alone among job-less items, which is the only scope it ever had.
//
// The ON CONFLICT clause names the arbiter explicitly (it is not a bare DO NOTHING) on purpose: a bare
// DO NOTHING would also swallow a primary-key collision on id and report it as a harmless duplicate.
// The caller sealed AddrCiphertext (DR-4) — this store persists ciphertext, never plaintext.
func (p *PG) EnqueueWorkItem(ctx context.Context, item WorkItemRow) (string, bool, error) {
	const insert = `
INSERT INTO verify.work_items (
  id, idempotency_key, job_id, domain, email_hash, policy_key, policy_version,
  config_fingerprint, status, attempt, not_before, addr_ciphertext, created_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`
	q := insert + `
ON CONFLICT (job_id, idempotency_key) WHERE job_id IS NOT NULL DO NOTHING
RETURNING id`
	if item.JobID == "" {
		q = insert + `
ON CONFLICT (idempotency_key) WHERE job_id IS NULL DO NOTHING
RETURNING id`
	}
	status := item.Status
	if status == "" {
		status = "ready"
	}
	created := item.CreatedAt
	if created.IsZero() {
		created = item.NotBefore
	}
	var returnedID string
	err := p.pool.QueryRow(ctx, q,
		item.ID, item.IdempotencyKey, nullStr(item.JobID), item.Domain, item.EmailHash,
		item.PolicyKey, item.PolicyVersion, item.ConfigFingerprint, status, item.Attempt,
		item.NotBefore.UTC(), nullBytes(item.AddrCiphertext), created.UTC(),
	).Scan(&returnedID)
	if errors.Is(err, pgx.ErrNoRows) {
		// idempotency_key already present in this job → dedup (§5.5). Report WHICH work item holds it (the
		// Mem fake does), so a caller can point a duplicate row at the work item that answers it.
		var existing string
		var qerr error
		if item.JobID == "" {
			qerr = p.pool.QueryRow(ctx,
				`SELECT id::text FROM verify.work_items WHERE job_id IS NULL AND idempotency_key = $1`,
				item.IdempotencyKey).Scan(&existing)
		} else {
			qerr = p.pool.QueryRow(ctx,
				`SELECT id::text FROM verify.work_items WHERE job_id = $1 AND idempotency_key = $2`,
				item.JobID, item.IdempotencyKey).Scan(&existing)
		}
		if qerr != nil && !errors.Is(qerr, pgx.ErrNoRows) {
			return "", true, fmt.Errorf("store: enqueue work item: look up the existing holder of the key: %w", qerr)
		}
		return existing, true, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: enqueue work item: %w", err)
	}
	return returnedID, false, nil
}

// RecordJob (the job, its input rows and the LOG rows of its malformed rows, in one transaction) is in
// postgres_jobs.go, with the statement builders it is made of.

// DequeueReady leases up to `limit` ready items with FOR UPDATE SKIP LOCKED in group-by-domain
// order (§6.2). The inner SELECT locks the chosen rows; the outer UPDATE stamps the lease. ORDER
// BY domain, not_before spreads across domains (a domain's items cluster, so distinct domains
// interleave) while draining the oldest-due first within a domain.
func (p *PG) DequeueReady(ctx context.Context, workerID string, limit int, now time.Time) ([]WorkItemRow, error) {
	if limit <= 0 {
		limit = 10
	}
	const q = `
UPDATE verify.work_items SET status='in_flight', locked_by=$2, locked_at=$3
WHERE id IN (
  SELECT id FROM verify.work_items
  WHERE status IN ('ready','waiting_retry') AND not_before <= $3
  ORDER BY domain, not_before
  FOR UPDATE SKIP LOCKED
  LIMIT $1
)
RETURNING id, idempotency_key, COALESCE(job_id::text,''), domain, email_hash, policy_key,
          policy_version, config_fingerprint, status, attempt, not_before, addr_ciphertext,
          created_at`
	rows, err := p.pool.Query(ctx, q, limit, workerID, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("store: dequeue: %w", err)
	}
	defer rows.Close()
	var out []WorkItemRow
	for rows.Next() {
		var w WorkItemRow
		var addrCT []byte
		if err := rows.Scan(
			&w.ID, &w.IdempotencyKey, &w.JobID, &w.Domain, &w.EmailHash, &w.PolicyKey,
			&w.PolicyVersion, &w.ConfigFingerprint, &w.Status, &w.Attempt, &w.NotBefore, &addrCT,
			&w.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("store: dequeue scan: %w", err)
		}
		w.AddrCiphertext = addrCT
		w.LockedBy = workerID
		w.LockedAt = now.UTC()
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: dequeue rows: %w", err)
	}
	return out, nil
}

// SettleWorkItem is the GAP-37/33/38/42 conditional-CAS settlement (§5.3 + P6 §5.4) in ONE
// transaction. See the interface doc. The CAS (WHERE id=? AND locked_by=? AND status='in_flight') is
// the exactly-once guarantee: zero rows → ROLLBACK (no LOG row, no spend row — GAP-33/42) →
// ErrLeaseLost. terminal ∈ {settled, dead}. extCall != nil => a paid vendor call was made for this
// settle; its spend record is inserted IN THIS TX (spend-once — a reclaimed attempt's charge rolls
// back with its discarded settlement).
func (p *PG) SettleWorkItem(ctx context.Context, itemID, workerID, terminal string, verdict VerificationRow, raw *RawRow, attempt WorkItemAttemptRow, extCall *ExternalCallRow) error {
	if terminal != "settled" && terminal != "dead" {
		return fmt.Errorf("store: settle: invalid terminal %q", terminal)
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: settle begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	// (2 first — do the CAS BEFORE the LOG insert so a lost lease costs no wasted INSERT work and,
	// critically, so a zero-row CAS returns ErrLeaseLost with NOTHING written. verification_id is
	// set to verdict.ID here; the LOG row with that id is inserted in step (1) below within the
	// same tx, so the FK-less pointer and the row commit atomically — GAP-33.)
	const casUpdate = `
UPDATE verify.work_items SET
  status=$5, verification_id=$2, settled_at=$3, addr_ciphertext=NULL, locked_by=NULL, locked_at=NULL
WHERE id=$1 AND locked_by=$4 AND status='in_flight'`
	tag, err := tx.Exec(ctx, casUpdate, itemID, verdict.ID, verdict.CheckedAt.UTC(), workerID, terminal)
	if err != nil {
		return fmt.Errorf("store: settle CAS: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost // lease reclaimed or item already transitioned → discard (GAP-37/42)
	}

	// (1) INSERT the one verifications LOG row (Door 2 append) + optional raw sidecar.
	if err := insertVerificationTx(ctx, tx, verdict, raw); err != nil {
		return err
	}

	// (3) If job-owned, bump the denormalized progress counter (cheap display; completion is
	// authoritative via JobCompletion — GAP-38). Guarded so a NULL job_id is a no-op.
	if _, err := tx.Exec(ctx,
		`UPDATE verify.jobs SET settled_count = settled_count + 1
		 WHERE id = (SELECT job_id FROM verify.work_items WHERE id=$1 AND job_id IS NOT NULL)`,
		itemID); err != nil {
		return fmt.Errorf("store: settle job count: %w", err)
	}

	// (4) INSERT the per-attempt transcript (append-only, OQ-P5-8).
	if err := insertAttemptTx(ctx, tx, attempt); err != nil {
		return err
	}

	// (5) SPEND-ONCE (P6 §5.4/GAP-42): if a paid vendor call produced this verdict, its spend record
	// commits UNDER THE SAME CAS. A reclaimed attempt never reaches here (the CAS returned zero rows
	// above); so the charge is recorded exactly once, atomically with the settle.
	if extCall != nil {
		if err := insertExternalCallTx(ctx, tx, *extCall); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// RequeueWorkItem is the GAP-37 conditional-CAS requeue (§4.2). Zero rows → ErrLeaseLost.
func (p *PG) RequeueWorkItem(ctx context.Context, itemID, workerID string, notBefore time.Time, reason string, bumpAttempt bool, attempt WorkItemAttemptRow) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: requeue begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// attempt=attempt+CASE: a capacity wait (rate_limited/no-eligible) must NOT burn the greylist
	// retry budget (§4.6/OQ-P5-7) → bumpAttempt=false; a provider-transient bumps.
	inc := 0
	if bumpAttempt {
		inc = 1
	}
	const casUpdate = `
UPDATE verify.work_items SET
  status='waiting_retry', attempt=attempt+$5, not_before=$2, last_reason=$3,
  locked_by=NULL, locked_at=NULL
WHERE id=$1 AND locked_by=$4 AND status='in_flight'`
	tag, err := tx.Exec(ctx, casUpdate, itemID, notBefore.UTC(), nullStr(reason), workerID, inc)
	if err != nil {
		return fmt.Errorf("store: requeue CAS: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	if err := insertAttemptTx(ctx, tx, attempt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReclaimExpiredLeases resets in_flight items whose lease predates now-leaseTTL (§5.2). An item
// past its lease goes back to 'ready' when it's due, else 'waiting_retry' (its not_before is still
// future). The lease is cleared so DequeueReady can pick it up again.
func (p *PG) ReclaimExpiredLeases(ctx context.Context, leaseTTL time.Duration, now time.Time) (int, error) {
	cutoff := now.Add(-leaseTTL).UTC()
	const q = `
UPDATE verify.work_items SET
  status = CASE WHEN not_before <= $2 THEN 'ready' ELSE 'waiting_retry' END,
  locked_by=NULL, locked_at=NULL
WHERE status='in_flight' AND locked_at < $1`
	tag, err := p.pool.Exec(ctx, q, cutoff, now.UTC())
	if err != nil {
		return 0, fmt.Errorf("store: reclaim leases: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// GetWorkItem reads one work-item by id.
func (p *PG) GetWorkItem(ctx context.Context, itemID string) (WorkItemRow, bool, error) {
	const q = `
SELECT id, idempotency_key, COALESCE(job_id::text,''), domain, email_hash, policy_key,
       policy_version, config_fingerprint, status, attempt, not_before,
       COALESCE(locked_by,''), COALESCE(last_reason,''), COALESCE(verification_id::text,''),
       addr_ciphertext, created_at
FROM verify.work_items WHERE id=$1`
	var w WorkItemRow
	var addrCT []byte
	err := p.pool.QueryRow(ctx, q, itemID).Scan(
		&w.ID, &w.IdempotencyKey, &w.JobID, &w.Domain, &w.EmailHash, &w.PolicyKey, &w.PolicyVersion,
		&w.ConfigFingerprint, &w.Status, &w.Attempt, &w.NotBefore, &w.LockedBy, &w.LastReason,
		&w.VerificationID, &addrCT, &w.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkItemRow{}, false, nil
	}
	if err != nil {
		return WorkItemRow{}, false, fmt.Errorf("store: get work item: %w", err)
	}
	w.AddrCiphertext = addrCT
	return w, true, nil
}

// CreateJob inserts a jobs row (§7.2).
func (p *PG) CreateJob(ctx context.Context, job JobRow) (string, error) {
	if _, err := p.pool.Exec(ctx, insertJobSQL, jobInsertArgs(job)...); err != nil {
		return "", fmt.Errorf("store: create job: %w", err)
	}
	return job.ID, nil
}

// insertJobSQL / jobInsertArgs are the one INSERT of a jobs row, shared by CreateJob and RecordJob so the two cannot drift.
//
// owner_user_id (0013 / item 1): the platform user the submitting key acts for. nullStr maps "" to SQL NULL rather than inventing a
// sentinel owner — a job whose submitter had no recorded platform user is honestly ownerless, and the read predicates make an ownerless
// row match no caller (NULL = $2 is NULL, not true), so the fail-safe direction is read-CLOSED.
const insertJobSQL = `
INSERT INTO verify.jobs (id, policy_key, policy_version, status, total_count, settled_count, caller_ref, owner_user_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`

func jobInsertArgs(job JobRow) []any {
	status := job.Status
	if status == "" {
		status = "submitted"
	}
	return []any{
		job.ID, job.PolicyKey, job.PolicyVersion, status, job.TotalCount, job.SettledCount,
		nullStr(job.CallerRef), nullStr(job.OwnerUserID),
	}
}

// GetJob reads one job by id, SCOPED TO ITS OWNER (0013 / fix-plan item 1, D1).
//
// THE PREDICATE IS `owner_user_id = $2`, NOT a post-read comparison in Go. That matters: a filter
// applied after the row is in memory is one `if` away from being forgotten (and one refactor away
// from being logged), whereas a row that never leaves Postgres cannot leak. It also means the
// index (idx_jobs_owner) does the work.
//
// A cross-tenant read is ErrNoRows → known=false → the handler's 404 — byte-identical to a
// nonexistent id. Never 403: a 403 confirms the id exists, which is the existence disclosure the
// senders' posture forbids and which would make job ids worth harvesting.
//
// $2 = "" (no owner) matches NOTHING, because `owner_user_id = ''::uuid` would be a cast error and
// nullStr sends NULL instead — and `owner_user_id = NULL` is NULL, not true, under SQL's
// three-valued logic. So an ownerless caller reads no job, and a pre-0013 NULL-owner row is
// unreadable over the API by anyone. Both are the fail-safe direction, and both are deliberate.
func (p *PG) GetJob(ctx context.Context, jobID, ownerUserID string) (JobRow, bool, error) {
	const q = `
SELECT id, policy_key, policy_version, status, total_count, settled_count, submitted_at,
       COALESCE(completed_at, 'epoch'::timestamptz), COALESCE(caller_ref,''),
       COALESCE(owner_user_id::text,'')
FROM verify.jobs WHERE id=$1 AND owner_user_id=$2`
	var j JobRow
	var completed time.Time
	err := p.pool.QueryRow(ctx, q, jobID, nullStr(ownerUserID)).Scan(
		&j.ID, &j.PolicyKey, &j.PolicyVersion, &j.Status, &j.TotalCount, &j.SettledCount,
		&j.SubmittedAt, &completed, &j.CallerRef, &j.OwnerUserID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return JobRow{}, false, nil
	}
	if err != nil {
		return JobRow{}, false, fmt.Errorf("store: get job: %w", err)
	}
	if completed.Unix() > 0 {
		j.CompletedAt = completed
	}
	return j, true, nil
}

// JobCompletion judges completion AUTHORITATIVELY (§7.3/GAP-38): NOT EXISTS a non-terminal item.
// SCOPED TO ITS OWNER (0013 / item 1).
//
// The owner predicate goes on the COUNTS query — the first of the two — so a cross-tenant call
// exits at ErrNoRows → ErrNotWired without ever running the second. That ordering is deliberate:
// the NOT EXISTS query is keyed on work_items.job_id, and its boolean answer ("is this job
// finished?") is itself a small cross-tenant signal. Gate first, then compute.
//
// The second query needs no predicate of its own precisely because it is unreachable for a
// non-owner. It is left keyed on job_id alone rather than joined back to jobs so the GAP-38
// invariant stays legible as exactly what the spec words say — completion is NOT EXISTS a
// non-terminal item, never settled==total.
func (p *PG) JobCompletion(ctx context.Context, jobID, ownerUserID string) (bool, int, int, error) {
	var total int
	if err := p.pool.QueryRow(ctx,
		`SELECT total_count FROM verify.jobs WHERE id=$1 AND owner_user_id=$2`,
		jobID, nullStr(ownerUserID)).Scan(&total); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Covers BOTH "no such job" and "not yours" — indistinguishable by design (the handler
			// maps this to the same 404 a nonexistent id gets).
			return false, 0, 0, ErrNotWired
		}
		return false, 0, 0, fmt.Errorf("store: job completion counts: %w", err)
	}
	// settled counts ROWS (0018), not work items: every input row whose work item is terminal, plus the
	// malformed rows (their verdict exists from submission, and they have no work item to wait for). A
	// duplicate row shares its first row's work item, so it settles with it, and total - settled reaches 0
	// exactly when the job is complete. Computed from job_rows on read, not from jobs.settled_count: a
	// counter bumped per work item cannot say how many ROWS a work item answers, and one that the enqueue
	// order could outrun is how a pending count gets stuck above zero.
	var settled int
	if err := p.pool.QueryRow(ctx, `
SELECT count(*)::int
FROM verify.job_rows r
LEFT JOIN verify.work_items w ON w.id = r.work_item_id
WHERE r.job_id = $1 AND (r.malformed OR w.status IN ('settled','dead'))`,
		jobID).Scan(&settled); err != nil {
		return false, 0, 0, fmt.Errorf("store: job completion settled rows: %w", err)
	}
	var complete bool
	if err := p.pool.QueryRow(ctx,
		`SELECT NOT EXISTS(SELECT 1 FROM verify.work_items WHERE job_id=$1 AND status NOT IN ('settled','dead'))`,
		jobID).Scan(&complete); err != nil {
		return false, 0, 0, fmt.Errorf("store: job completion authoritative: %w", err)
	}
	return complete, total, settled, nil
}

// ListJobResults returns settled work-items joined to their verdicts, cursor-paginated by id (§7.1).
// SCOPED TO ITS OWNER (0013 / fix-plan item 1, D1).
//
// THIS IS THE PATH D1 ACTUALLY LEAKED THROUGH. GetJob exposed counts and a status; this one hands
// back the full settled verdicts — status, reason, confidence, recommendation, flags, signals,
// domain — for every address in the job. That is the PII, and it was readable by any authenticated
// caller who knew a job_id.
//
// The predicate is an INNER JOIN back to jobs, not a WHERE on work_items: work_items has no owner
// column (nor should it — ownership is a property of the job, stored once). A non-owner therefore
// matches ZERO ROWS and gets an empty page, never a partial one; there is no arrangement of
// cursor/limit that walks another tenant's results, because the owner condition is applied before
// pagination, not after.
//
// Note the handler ALSO calls GetJob first and 404s on a non-owner, so this is the second of two
// independent barriers. That redundancy is intentional: the handler's check protects the HTTP
// response shape, this one protects the QUERY, and either alone would be one edit away from being
// the only thing holding.
//
// SINCE 0018 A RESULT IS ONE PER INPUT ROW, ORDERED BY ORDINAL, AND THE CURSOR IS THE ORDINAL. The page is
// read from verify.job_rows (the caller's own rows, malformed ones included) and LEFT-joined to the work
// item and its verdict: identical addresses in one job share a work item, so their rows share a verdict
// and keep their own ordinal and item_id. A row appears once it has an outcome — its work item is
// settled|dead, or it is malformed. The page asks for limit+1 rows ONLY to learn whether another page
// exists: it is then cut back to limit and the cursor is the ordinal of the LAST ROW RETURNED. A cursor
// taken from the look-ahead row would skip that row on the next page (the defect M2's campaign report
// had, M2-G5); the pagination tests walk every page and demand every ordinal exactly once, and a paired
// control that does take the look-ahead row's ordinal must fail them.
func (p *PG) ListJobResults(ctx context.Context, jobID, ownerUserID, cursor string, limit int) ([]WorkItemResult, string, error) {
	if limit <= 0 {
		limit = 100
	}
	after, cerr := ParseResultsCursor(cursor) // -1 on the first page; ErrBadCursor for anything else
	if cerr != nil {
		return nil, "", cerr
	}
	// The verdict of a row is its work item's (shared by every row of one address) or, for a MALFORMED row, its own
	// invalid_syntax LOG row (r.verification_id, written at submit): every result is served from a LOG row, none is made up.
	const q = `
SELECT r.ordinal, COALESCE(r.item_id,''), r.malformed, r.duplicate_of_ordinal, COALESCE(w.id::text,''), w.email_hash,
       COALESCE(v.id::text,''), COALESCE(v.status,''), COALESCE(v.reason,''), COALESCE(v.confidence::int,0),
       COALESCE(v.recommendation,''), v.flags, v.signals,
       COALESCE(v.mx_provider,''), COALESCE(v.domain,''), COALESCE(v.policy_key,''),
       COALESCE(v.policy_version,0), COALESCE(v.api_version,'')
FROM verify.job_rows r
JOIN verify.jobs j ON j.id = r.job_id AND j.owner_user_id = $2
LEFT JOIN verify.work_items w ON w.id = r.work_item_id
LEFT JOIN verify.verifications v ON v.id = COALESCE(w.verification_id, r.verification_id)
WHERE r.job_id = $1
  AND r.ordinal > $3
  AND (r.malformed OR (w.status IN ('settled','dead') AND w.verification_id IS NOT NULL))
ORDER BY r.ordinal
LIMIT $4`
	rows, err := p.pool.Query(ctx, q, jobID, nullStr(ownerUserID), after, limit+1) // +1 to learn whether another page exists
	if err != nil {
		return nil, "", fmt.Errorf("store: list job results: %w", err)
	}
	defer rows.Close()
	var out []WorkItemResult
	for rows.Next() {
		var r WorkItemResult
		var v VerificationRow
		var flagsJSON, signalsJSON []byte
		if err := rows.Scan(
			&r.Ordinal, &r.ItemID, &r.Malformed, &r.DuplicateOf, &r.WorkItemID, &r.EmailHash,
			&v.ID, &v.Status, &v.Reason, &v.Confidence, &v.Recommendation, &flagsJSON, &signalsJSON,
			&v.MXProvider, &v.Domain, &v.PolicyKey, &v.PolicyVersion, &v.APIVersion,
		); err != nil {
			return nil, "", fmt.Errorf("store: list job results scan: %w", err)
		}
		_ = json.Unmarshal(flagsJSON, &v.Flags)
		_ = json.Unmarshal(signalsJSON, &v.Signals)
		r.Verdict = v
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("store: list job results rows: %w", err)
	}
	nextCursor := ""
	if len(out) > limit {
		// The extra row only proved a next page exists. The cursor is the LAST ROW RETURNED, never the look-ahead.
		out = out[:limit]
		nextCursor = FormatResultsCursor(out[len(out)-1].Ordinal)
	}
	return out, nextCursor, nil
}

// insertVerificationTx inserts one LOG row (+ optional raw sidecar) inside an existing tx — the
// shared body of AppendVerification and the queue settlement (§5.3). Door 2 append; never UPDATE.
func insertVerificationTx(ctx context.Context, tx pgx.Tx, row VerificationRow, raw *RawRow) error {
	args, err := verificationArgs(row)
	if err != nil {
		return err
	}
	const q = `
INSERT INTO verify.verifications (` + verificationColumns + `
) VALUES (
  $1,$2,$3,$4, $5,$6,$7,$8,$9, $10,$11,$12,$13,$14,$15,$16, $17,$18,$19,
  $20,$21,$22,$23,$24, $25,$26,$27,$28
)`
	if _, err := tx.Exec(ctx, q, args...); err != nil {
		return fmt.Errorf("store: settle insert verification: %w", err)
	}
	if raw != nil {
		if _, err := tx.Exec(ctx,
			`INSERT INTO verify.verification_raw (verification_id, raw, created_at) VALUES ($1,$2,$3)`,
			raw.VerificationID, raw.Raw, row.CreatedAt.UTC(),
		); err != nil {
			return fmt.Errorf("store: settle insert raw: %w", err)
		}
	}
	return nil
}

// AppendExternalCall records a SYNC-path external call (§6.1) — a plain append (no CAS: the sync
// path settles by logging, not by a lease transition). INSERT-only, append-only. For a QUEUED
// escalation the spend record rides SettleWorkItem's CAS instead (spend-once) — never both.
func (p *PG) AppendExternalCall(ctx context.Context, call ExternalCallRow) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: external call begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := insertExternalCallTx(ctx, tx, call); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// insertExternalCallTx appends one external-call spend/provenance row inside an existing tx (§6.1).
// Append-only (mirrors work_item_attempts). Stores email_hash (Door 3), never plaintext (PN-10 —
// queryable by email_hash for SAR/deletion). Empty FK-less pointers (verification_id/work_item_id/
// job_id) map to SQL NULL.
func insertExternalCallTx(ctx context.Context, tx pgx.Tx, c ExternalCallRow) error {
	if c.ID == "" {
		return fmt.Errorf("store: external call requires an id")
	}
	const q = `
INSERT INTO verify.external_calls (
  id, verification_id, work_item_id, job_id, vendor, residue_class, email_hash,
  vendor_status_raw, vendor_confidence_raw, request_id, cost_units, outcome, latency_ms, called_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`
	if _, err := tx.Exec(ctx, q,
		c.ID, nullStr(c.VerificationID), nullStr(c.WorkItemID), nullStr(c.JobID),
		c.Vendor, nullStr(c.ResidueClass), nullBytes(c.EmailHash),
		nullStr(c.VendorStatusRaw), nullInt(c.VendorConfidenceRaw), nullStr(c.RequestID),
		c.CostUnits, c.Outcome, nullInt(c.LatencyMS), c.CalledAt.UTC(),
	); err != nil {
		return fmt.Errorf("store: insert external call: %w", err)
	}
	return nil
}

// ExternalSpendSince derives the breaker's durable spend/error facts (§5.2/§5.3) from the append-only
// external_calls table. Only "ok" calls count toward spend (an errored call produced no verdict and
// costs 0); ALL calls count toward the error-rate denominator. Windowed so a restart can't reset the
// day (the MONEY analogue of the P4 restart-safe budget). jobID scopes SpentJob ("" => 0).
func (p *PG) ExternalSpendSince(ctx context.Context, vendor, jobID string, dayStart, monthStart, errWindowStart, minuteStart, now time.Time) (ExternalSpend, error) {
	var s ExternalSpend
	// Spend (ok calls only) over day / month, and this job.
	const spendQ = `
SELECT
  COALESCE(SUM(cost_units) FILTER (WHERE outcome='ok' AND called_at >= $2), 0),
  COALESCE(SUM(cost_units) FILTER (WHERE outcome='ok' AND called_at >= $3), 0),
  COALESCE(SUM(cost_units) FILTER (WHERE outcome='ok' AND $4 <> '' AND job_id::text = $4), 0)
FROM verify.external_calls
WHERE vendor = $1`
	if err := p.pool.QueryRow(ctx, spendQ, vendor, dayStart.UTC(), monthStart.UTC(), jobID).
		Scan(&s.SpentDay, &s.SpentMonth, &s.SpentJob); err != nil {
		return ExternalSpend{}, fmt.Errorf("store: external spend: %w", err)
	}
	// Error-rate window: all calls (denominator) + errored calls (numerator). Last-minute window: all
	// calls (the rate_ceiling brake, §5.2 — a burst count, distinct from the cumulative cost caps).
	const errQ = `
SELECT
  COUNT(*) FILTER (WHERE called_at >= $2),
  COUNT(*) FILTER (WHERE outcome='error' AND called_at >= $2),
  COUNT(*) FILTER (WHERE called_at >= $3)
FROM verify.external_calls
WHERE vendor = $1 AND called_at >= LEAST($2, $3)`
	if err := p.pool.QueryRow(ctx, errQ, vendor, errWindowStart.UTC(), minuteStart.UTC()).
		Scan(&s.Calls, &s.Errors, &s.CallsLastMinute); err != nil {
		return ExternalSpend{}, fmt.Errorf("store: external error-rate: %w", err)
	}
	return s, nil
}

// insertAttemptTx appends one per-attempt transcript row inside an existing tx (OQ-P5-8).
func insertAttemptTx(ctx context.Context, tx pgx.Tx, a WorkItemAttemptRow) error {
	if a.ID == "" {
		return nil // no transcript to record (caller opted out) — the settle/requeue still holds
	}
	signalsJSON, err := json.Marshal(orEmptySignals(a.Signals))
	if err != nil {
		return fmt.Errorf("store: marshal attempt signals: %w", err)
	}
	const q = `
INSERT INTO verify.work_item_attempts (id, work_item_id, attempt, attempted_at, node_id, verifier_ip, reason, signals, latency_ms)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`
	if _, err := tx.Exec(ctx, q,
		a.ID, a.WorkItemID, a.Attempt, a.AttemptedAt.UTC(), nullStr(a.NodeID), nullStr(a.VerifierIP),
		nullStr(a.Reason), signalsJSON, nullInt(a.LatencyMS),
	); err != nil {
		return fmt.Errorf("store: insert attempt: %w", err)
	}
	return nil
}

// LookupCaller resolves an authenticated caller from sha256(bearer key) (PN-13, §7.5). The
// validity window is enforced IN the query (now ∈ [not_before, not_after)), so a rotated-out key
// yields ErrNoRows → ok=false. During a rotation overlap two rows share a caller_id with
// overlapping windows; the lookup is by key_sha256 (the PK), so each key resolves independently —
// LIMIT 1 is defensive (key_sha256 is unique). A miss is ok=false, never an error (→ 401).
//
// It also returns user_id — the PLATFORM user the key acts for (0013 / item 1). This is the single
// origin of jobs.owner_user_id, and it is read HERE, off the credential, so that no code path can
// take an owner from the request body. Rotation is unaffected: both keys in an overlap window
// belong to the same caller and carry the same user_id, so a rotation cannot silently move a
// caller's jobs to a different owner. "" (NULL user_id) means a key issued before ownership was
// recorded — it authenticates and may submit, but its jobs get a NULL owner and are read-closed.
func (p *PG) LookupCaller(ctx context.Context, keySHA256 []byte, now time.Time) (string, string, bool, error) {
	const q = `
SELECT caller_id, COALESCE(user_id::text,'') FROM verify.api_callers
WHERE key_sha256 = $1
  AND not_before <= $2
  AND (not_after IS NULL OR not_after > $2)
LIMIT 1`
	var callerID, ownerUserID string
	err := p.pool.QueryRow(ctx, q, keySHA256, now.UTC()).Scan(&callerID, &ownerUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil // unknown OR expired key → not authenticated (caller maps to 401)
	}
	if err != nil {
		return "", "", false, fmt.Errorf("store: lookup caller: %w", err)
	}
	return callerID, ownerUserID, true, nil
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
