// In-memory Store fake for unit tests. It honors the same contract as the pgx store:
// append-only LOG (accumulates rows; no mutation method exists), composite-keyed cache
// with TTL expiry, and seeded lists/policies. It is NOT a Postgres emulator — the real
// append-only GRANT enforcement is proven against actual Postgres (postgres_test.go); this
// fake lets the orchestrator/layer/policy tests run without a DB.
package store

import (
	"context"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemStore is a race-safe in-memory Store.
type MemStore struct {
	mu    sync.Mutex
	now   func() time.Time
	Log   []VerificationRow // append-only; tests read this to assert "exactly one row"
	Raw   []RawRow
	cache map[string]CachedVerdict // key: hex(email_hash) + "\x00" + config_fingerprint

	disposable map[string]struct{}
	spamTrap   map[string]struct{}
	catchall   map[string]bool // present => probed; value => is_catchall
	policies   map[string]PolicyRow
	defaultKey string

	// Phase 3 (§4.3/§4.4/§5.2) — node health, append-only audit history, durable warmup counters.
	NodeAudits []NodeAuditRow          // append-only; tests read this to assert history
	health     map[string]NodeHealth   // node_id -> latest health summary (the `nodes` row mirror)
	registered map[string]struct{}     // node_ids with a `nodes` row (mirrors PG: only UpsertNodeState creates it)
	warmup     map[string]warmupWindow // node_id + "\x00" + windowStart -> counter

	// Phase 4 (§4.4) — durable per-(node × provider) token-bucket spend. Keyed node+"\x00"+bucket.
	// Uses the SHARED applyRefill helper so the fake can't diverge from PG (P3 test-double lesson).
	rateBudget map[string]rateBucket

	// Phase 5 (§7.5/PN-13) — per-caller front-door keys. Keyed hex(key_sha256) -> callerRow.
	callers map[string]callerRow
}

// callerRow mirrors one api_callers row (per-caller key + validity window).
type callerRow struct {
	callerID  string
	notBefore time.Time
	notAfter  time.Time // zero => no expiry (NULL)
}

// rateBucket mirrors one rate_budgets row (per node, per provider bucket).
type rateBucket struct {
	tokens     float64
	lastRefill time.Time
}

// warmupWindow mirrors one node_warmup row (per node, per window).
type warmupWindow struct {
	windowStart time.Time
	count       int
	cap         int
}

// NewMem builds an empty MemStore. now may be nil (defaults to time.Now).
func NewMem(now func() time.Time) *MemStore {
	if now == nil {
		now = time.Now
	}
	return &MemStore{
		now:        now,
		cache:      map[string]CachedVerdict{},
		disposable: map[string]struct{}{},
		spamTrap:   map[string]struct{}{},
		catchall:   map[string]bool{},
		policies:   map[string]PolicyRow{},
		health:     map[string]NodeHealth{},
		registered: map[string]struct{}{},
		warmup:     map[string]warmupWindow{},
		rateBudget: map[string]rateBucket{},
		callers:    map[string]callerRow{},
	}
}

func rateKey(nodeID, bucket string) string {
	return nodeID + "\x00" + bucket
}

func warmupKey(nodeID string, windowStart time.Time) string {
	return nodeID + "\x00" + windowStart.UTC().Format(time.RFC3339Nano)
}

func cacheKey(emailHash []byte, cfg string) string {
	return hex.EncodeToString(emailHash) + "\x00" + cfg
}

func policyKey(k string, v int) string {
	var b strings.Builder
	b.WriteString(k)
	b.WriteByte(0)
	b.WriteString(itoa(v))
	return b.String()
}

func itoa(v int) string {
	// tiny local itoa to avoid importing strconv in the hot map-key path
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func (m *MemStore) AppendVerification(_ context.Context, row VerificationRow, raw *RawRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Log = append(m.Log, row) // APPEND ONLY — no path updates/deletes an existing row.
	if raw != nil {
		m.Raw = append(m.Raw, *raw)
	}
	return nil
}

func (m *MemStore) GetCachedVerdict(_ context.Context, emailHash []byte, cfg string) (CachedVerdict, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.cache[cacheKey(emailHash, cfg)]
	if !ok {
		return CachedVerdict{}, ErrCacheMiss
	}
	if !v.ExpiresAt.IsZero() && !m.now().Before(v.ExpiresAt) {
		return CachedVerdict{}, ErrCacheMiss // expired => miss, never a stale verdict
	}
	// Return fresh (one-level) copies of the maps so a caller that mutates flags/signals (e.g.
	// the verdict assembler sets flags["from_cache"]) cannot corrupt the stored entry. This
	// mirrors PG's no-aliasing (it unmarshals fresh maps per read). A one-level copy suffices in
	// P1 because signal/flag values are scalars (bool/string) — no nested containers to share; a
	// P2 signal that becomes a container would need a deeper copy (see the P2 note in postgres.go).
	v.Flags = copyBoolMap(v.Flags)
	v.Signals = copyAnyMap(v.Signals)
	return v, nil
}

func copyBoolMap(m map[string]bool) map[string]bool {
	if m == nil {
		return nil
	}
	out := make(map[string]bool, len(m))
	for k, val := range m {
		out[k] = val
	}
	return out
}

func copyAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, val := range m {
		out[k] = val
	}
	return out
}

func (m *MemStore) PutCachedVerdict(_ context.Context, v CachedVerdict) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Copy maps on write too (PG serializes to JSON on write): the stored entry must not alias
	// the caller's maps, or a later caller-side mutation would silently change the cache.
	v.Flags = copyBoolMap(v.Flags)
	v.Signals = copyAnyMap(v.Signals)
	m.cache[cacheKey(v.EmailHash, v.ConfigFingerprint)] = v
	return nil
}

func (m *MemStore) IsDisposable(_ context.Context, domain string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.disposable[domain]
	return ok, nil
}

func (m *MemStore) IsSpamTrap(_ context.Context, domain string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.spamTrap[domain]
	return ok, nil
}

func (m *MemStore) CatchAllState(_ context.Context, domain string) (bool, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, known := m.catchall[domain]
	return v, known, nil
}

// SetCatchAll upserts the tri-state conclusion (§5.6). A nil isCatchall (inconclusive) is a NO-OP
// that never overwrites a known value with unknown — mirrors the PG upsert's WHERE guard.
func (m *MemStore) SetCatchAll(_ context.Context, domain string, isCatchall *bool, _ string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if isCatchall == nil {
		return nil // inconclusive → never overwrite a determined value (§5.6)
	}
	m.catchall[domain] = *isCatchall
	return nil
}

func (m *MemStore) GetDefaultPolicy(_ context.Context) (PolicyRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.defaultKey == "" {
		return PolicyRow{}, ErrNotWired
	}
	return m.policies[m.defaultKey], nil
}

func (m *MemStore) GetPolicy(_ context.Context, key string, version int) (PolicyRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.policies[policyKey(key, version)]
	if !ok {
		return PolicyRow{}, ErrNotWired
	}
	return p, nil
}

// ── Phase 3 (§4.3/§4.4/§5.2) — node health, audit history, warmup ──────────────────────

// AppendNodeAudit appends the audit-history row (append-only) and updates the node's latest health
// summary — but ONLY when a `nodes` row exists (registered via UpsertNodeState). This mirrors PG
// EXACTLY: there, AppendNodeAudit's `UPDATE nodes ... WHERE id=$1` matches 0 rows when the node was
// never registered, so the summary is NOT created and GetNodeHealth returns known=false. An audit
// that arrives before boot registration therefore updates the history (truth) but NOT the summary,
// in both stores (test-double fidelity — P3 review finding, low but real). It never touches
// status/port25_open (lifecycle/egress own those).
func (m *MemStore) AppendNodeAudit(_ context.Context, row NodeAuditRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.NodeAudits = append(m.NodeAudits, row)
	if _, ok := m.registered[row.NodeID]; !ok {
		return nil // no `nodes` row → summary UPDATE is a no-op (mirrors PG's WHERE id=$1 matching 0)
	}
	h := m.health[row.NodeID]
	h.NodeID = row.NodeID
	h.AuditStatus = row.AuditStatus
	h.LastAuditAt = row.AuditedAt
	h.LastAuditHostIP = row.CurrentHostIP
	m.health[row.NodeID] = h
	return nil
}

// GetNodeHealth returns known iff a `nodes` row exists (registered) — mirroring PG, where known is
// the `nodes` row's existence (pgx.ErrNoRows => known=false), NOT whether an audit ever arrived.
func (m *MemStore) GetNodeHealth(_ context.Context, nodeID string) (NodeHealth, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.registered[nodeID]; !ok {
		return NodeHealth{}, false, nil // no `nodes` row → not known (router treats as not eligible)
	}
	return m.health[nodeID], true, nil
}

// UpsertNodeState mirrors the descriptor + writes the lifecycle status/port25_open (§3.6/§4.5). It
// CREATES the `nodes` row (registered), preserves the audit columns already present (audit is a
// separate write path), and never deletes.
func (m *MemStore) UpsertNodeState(_ context.Context, nodeID, _ /*baseURL*/, status string, port25Open bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registered[nodeID] = struct{}{}
	h := m.health[nodeID]
	h.NodeID = nodeID
	h.Status = status
	h.Port25Open = port25Open
	m.health[nodeID] = h
	return nil
}

// ReserveWarmupSlot atomically reserves one :smtp slot against the window cap (§5.2). Denied when
// the window is at cap (Allowed=false → caller degrades to cheap, warmup_cap). Durable-equivalent:
// the counter survives as long as the (test) store lives, mirroring PG's per-window row.
func (m *MemStore) ReserveWarmupSlot(_ context.Context, nodeID string, windowStart time.Time, cap int) (WarmupDecision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cap <= 0 {
		// No allowance — deny without creating a row (mirrors the PG cap<=0 guard).
		return WarmupDecision{Allowed: false, Count: 0, Cap: cap, WindowStart: windowStart.UTC()}, nil
	}
	key := warmupKey(nodeID, windowStart)
	w, ok := m.warmup[key]
	if !ok {
		// Fresh window: PG's INSERT lands count=1,cap=<caller cap> (guaranteed under cap since cap>=1).
		w = warmupWindow{windowStart: windowStart.UTC(), count: 1, cap: cap}
		m.warmup[key] = w
		return WarmupDecision{Allowed: true, Count: 1, Cap: cap, WindowStart: w.windowStart}, nil
	}
	// Existing window. PG evaluates the WHERE guard against the STORED cap semantics: the UPDATE
	// applies `count=count+1, cap=EXCLUDED.cap` ONLY when the guard (stored count < caller cap)
	// passes. On DENY (stored count >= caller cap) the whole SET — including cap — does NOT apply,
	// and the read-after-deny returns the STORED row UNCHANGED. So we must NOT mutate the stored cap
	// on the deny path; we return the stored count+cap (the P3 review finding — Mem previously
	// lowered + returned the caller cap, diverging from PG).
	if w.count >= cap {
		return WarmupDecision{Allowed: false, Count: w.count, Cap: w.cap, WindowStart: w.windowStart}, nil
	}
	// Grant: mirror PG's `count=count+1, cap=EXCLUDED.cap` — the cap DOES update on the allow path.
	w.count++
	w.cap = cap
	m.warmup[key] = w
	return WarmupDecision{Allowed: true, Count: w.count, Cap: w.cap, WindowStart: w.windowStart}, nil
}

// ── Phase 4 (§4.4/§5.2/§5.4/§5.6) — durable rate budget, block-rate, lifecycle ─────────

// ReserveRateToken consumes one token from the durable (node × provider) bucket (§4.4). It refills
// via the SHARED applyRefill helper (identical math to PG) from real elapsed time since lastRefill,
// so a fresh MemStore models a cold bucket (starts at capacity — a never-seen node has full burst)
// and an existing bucket refills only by elapsed wall-clock (never on restart — restart-safe).
func (m *MemStore) ReserveRateToken(_ context.Context, nodeID, providerBucket string, p RateParams, now time.Time) (RateDecision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.Capacity <= 0 {
		return RateDecision{Allowed: false, TokensAfter: 0, Capacity: p.Capacity}, nil
	}
	b, ok := m.rateBudget[rateKey(nodeID, providerBucket)]
	if !ok {
		// A never-seen bucket starts FULL (capacity tokens at `now`) — mirrors PG's INSERT of a fresh
		// row at capacity. The first reservation then decrements from full.
		b = rateBucket{tokens: p.Capacity, lastRefill: now}
	}
	tokens, canGrant := applyRefill(b.tokens, b.lastRefill, p, now)
	if !canGrant {
		// Persist the refill (advance the anchor) but do NOT decrement — mirrors PG's read-side refill.
		m.rateBudget[rateKey(nodeID, providerBucket)] = rateBucket{tokens: tokens, lastRefill: now}
		return RateDecision{Allowed: false, TokensAfter: tokens, Capacity: p.Capacity}, nil
	}
	tokens -= 1.0
	m.rateBudget[rateKey(nodeID, providerBucket)] = rateBucket{tokens: tokens, lastRefill: now}
	return RateDecision{Allowed: true, TokensAfter: tokens, Capacity: p.Capacity}, nil
}

// PeekRateBudget refills-and-reads WITHOUT consuming (§3.3 headroom check). It advances the refill
// anchor (idempotent) but never decrements the spend.
func (m *MemStore) PeekRateBudget(_ context.Context, nodeID, providerBucket string, p RateParams, now time.Time) (RateDecision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.Capacity <= 0 {
		return RateDecision{Allowed: false, TokensAfter: 0, Capacity: p.Capacity}, nil
	}
	b, ok := m.rateBudget[rateKey(nodeID, providerBucket)]
	if !ok {
		b = rateBucket{tokens: p.Capacity, lastRefill: now}
	}
	tokens, canGrant := applyRefill(b.tokens, b.lastRefill, p, now)
	m.rateBudget[rateKey(nodeID, providerBucket)] = rateBucket{tokens: tokens, lastRefill: now}
	return RateDecision{Allowed: canGrant, TokensAfter: tokens, Capacity: p.Capacity}, nil
}

// BlockRateByVerifierIP aggregates per-verifier_ip block-rate from the in-memory LOG over [since,∞)
// (§5.2/§5.6). It applies the EXACT filter PG does (from_cache=false ∧ verifier_ip!="" ∧ SMTP
// actually ran), so cache hits and cheap-degrades never count (PB1). "SMTP ran" is inferred from a
// non-empty VerifierIP (§4.6: a cheap-degrade logs verifier_ip=""/NULL). Deterministic order.
func (m *MemStore) BlockRateByVerifierIP(_ context.Context, since time.Time) ([]NodeBlockStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	agg := map[string]*NodeBlockStats{}
	for _, row := range m.Log {
		if row.FromCache || row.VerifierIP == "" { // exclude cache hits + cheap-degrades (PB1)
			continue
		}
		if row.CheckedAt.Before(since) {
			continue
		}
		s, ok := agg[row.VerifierIP]
		if !ok {
			s = &NodeBlockStats{VerifierIP: row.VerifierIP}
			agg[row.VerifierIP] = s
		}
		s.Attempts++
		switch row.Reason {
		case "smtp_blocked":
			s.Blocked++
		case "smtp_timeout", "connection_failed":
			s.Timeouts++
		}
	}
	out := make([]NodeBlockStats, 0, len(agg))
	for _, s := range agg {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VerifierIP < out[j].VerifierIP })
	return out, nil
}

// MetricRowsSince returns LOG rows with CheckedAt >= since, newest first, capped at limit (§8). It
// mirrors PG's bounded windowed read (ORDER BY checked_at DESC LIMIT). limit<=0 uses 5000.
func (m *MemStore) MetricRowsSince(_ context.Context, since time.Time, limit int) ([]MetricRow, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = 5000
	}
	// Collect in-window rows newest-first.
	var all []MetricRow
	for i := len(m.Log) - 1; i >= 0; i-- {
		row := m.Log[i]
		if row.CheckedAt.Before(since) {
			continue
		}
		all = append(all, MetricRow{
			CheckedAt: row.CheckedAt, Status: row.Status, Reason: row.Reason, MXProvider: row.MXProvider,
			VerifierIP: row.VerifierIP, FromCache: row.FromCache, LatencyMS: row.LatencyMS, Signals: row.Signals,
		})
	}
	truncated := len(all) > limit
	if truncated {
		all = all[:limit]
	}
	return all, truncated, nil
}

// SetNodeStatus writes ONLY the lifecycle status (§5.4). No `nodes` row → no-op (mirrors PG's
// UPDATE ... WHERE id=$1 matching 0). It never touches port25_open/audit columns.
func (m *MemStore) SetNodeStatus(_ context.Context, nodeID, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.registered[nodeID]; !ok {
		return nil // no row → no-op (PG parity)
	}
	h := m.health[nodeID]
	h.NodeID = nodeID
	h.Status = status
	m.health[nodeID] = h
	return nil
}

// ResetWarmup resets the node's warmup counter for the window to count=0 at the given cap (§5.4/PM3
// canary recovery — re-warm gently). Upserts the window row.
func (m *MemStore) ResetWarmup(_ context.Context, nodeID string, windowStart time.Time, cap int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.warmup[warmupKey(nodeID, windowStart)] = warmupWindow{windowStart: windowStart.UTC(), count: 0, cap: cap}
	return nil
}

// LookupCaller resolves a caller from sha256(bearer key) honoring the validity window (PN-13,
// §7.5) — mirrors PG EXACTLY: a match requires now ∈ [notBefore, notAfter) (notAfter zero => no
// expiry), so a rotated-out key yields ok=false. A miss is ok=false, never an error (→ 401).
func (m *MemStore) LookupCaller(_ context.Context, keySHA256 []byte, now time.Time) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.callers[hex.EncodeToString(keySHA256)]
	if !ok {
		return "", false, nil
	}
	if now.Before(c.notBefore) {
		return "", false, nil // not yet valid
	}
	if !c.notAfter.IsZero() && !now.Before(c.notAfter) {
		return "", false, nil // expired (rotated out): now >= notAfter
	}
	return c.callerID, true, nil
}

func (m *MemStore) Ping(context.Context) error { return nil }
func (m *MemStore) Close()                     {}

// ── Test seeding helpers (not part of the Store interface) ──────────────────────────

func (m *MemStore) SeedDisposable(domains ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range domains {
		m.disposable[d] = struct{}{}
	}
}

func (m *MemStore) SeedSpamTrap(domains ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range domains {
		m.spamTrap[d] = struct{}{}
	}
}

func (m *MemStore) SeedCatchAll(domain string, isCatchall bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.catchall[domain] = isCatchall
}

// SeedNodeHealth sets a node's full health summary directly (test-support: construct the
// simulated audit surface — green, red, port25-blocked, egress-mismatch — without a real box).
func (m *MemStore) SeedNodeHealth(h NodeHealth) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registered[h.NodeID] = struct{}{} // seeding health implies the `nodes` row exists (GetNodeHealth => known)
	m.health[h.NodeID] = h
}

// SeedWarmup sets a node's window counter directly (test-support: drive the cap-exceeded path).
func (m *MemStore) SeedWarmup(nodeID string, windowStart time.Time, count, cap int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.warmup[warmupKey(nodeID, windowStart)] = warmupWindow{windowStart: windowStart.UTC(), count: count, cap: cap}
}

// SeedRateBudget sets a node×provider bucket's durable state directly (test-support: model the
// rate_budgets row a brain reloads on restart — the §4.4 restart-safe property).
func (m *MemStore) SeedRateBudget(nodeID, providerBucket string, tokens float64, lastRefill time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rateBudget[rateKey(nodeID, providerBucket)] = rateBucket{tokens: tokens, lastRefill: lastRefill}
}

// SeedCaller registers a per-caller key (test-support: model an api_callers row). keySHA256 is
// sha256(raw key); notAfter zero => no expiry. Drive the rotation/expiry paths by seeding two
// rows for one callerID with different windows.
func (m *MemStore) SeedCaller(keySHA256 []byte, callerID string, notBefore, notAfter time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callers[hex.EncodeToString(keySHA256)] = callerRow{callerID: callerID, notBefore: notBefore, notAfter: notAfter}
}

// SeedPolicy stores a policy; if isDefault, it becomes the default (last write wins).
func (m *MemStore) SeedPolicy(p PolicyRow) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.policies[policyKey(p.Key, p.Version)] = p
	if p.IsDefault {
		m.defaultKey = policyKey(p.Key, p.Version)
	}
}

// CacheLen returns the number of cache entries (test-support: assert what was/wasn't cached).
func (m *MemStore) CacheLen() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.cache)
}

// cacheContents returns a snapshot of the cache (internal test-support).
func (m *MemStore) cacheContents() map[string]CachedVerdict {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]CachedVerdict, len(m.cache))
	for k, v := range m.cache {
		out[k] = v
	}
	return out
}
