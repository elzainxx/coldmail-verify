// In-memory Store fake for unit tests. It honors the same contract as the pgx store:
// append-only LOG (accumulates rows; no mutation method exists), composite-keyed cache
// with TTL expiry, and seeded lists/policies. It is NOT a Postgres emulator — the real
// append-only GRANT enforcement is proven against actual Postgres (postgres_test.go); this
// fake lets the orchestrator/layer/policy tests run without a DB.
package store

import (
	"context"
	"encoding/hex"
	"fmt"
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

	// Phase 5 (§3.3/§5/§7) — the durable async queue. workItems keyed by id; idemKeys maps
	// jobID + "\x00" + hex(idempotency_key) -> id for dedup PER JOB (0018; a job-less item uses the empty
	// job id, so it dedups against other job-less items only); jobs keyed by id; attempts is the
	// append-only transcript. jobRows is verify.job_rows (0018): the input rows of each job, by job id.
	workItems map[string]WorkItemRow
	idemKeys  map[string]string
	jobs      map[string]JobRow
	jobRows   map[string][]JobRowEntry
	attempts  []WorkItemAttemptRow

	// 0019 — idempotent jobs: keyedJobs maps callerID + "\x00" + Idempotency-Key -> the job it created and the fingerprint of its request (the unique
	// index uq_jobs_caller_idempotency_key of Postgres). Node self-promotion: seq is a logical clock (one tick per registration and per audit row),
	// regSeq the tick of a node's last RegisterNode (Postgres: nodes.registered_at) and auditSeq the tick of each audit row (node_audits.created_at),
	// so "recorded AFTER the registration" is a comparison of ticks here and of the database's clock there.
	keyedJobs map[string]keyedJob
	seq       int64
	regSeq    map[string]int64
	auditSeq  map[string]int64

	// Phase 6 (§5/§6) — the append-only external-verifier call log (spend + provenance). Tests read
	// it to assert spend-once (exactly one row per settled escalation, even under a reclaim).
	ExternalCalls []ExternalCallRow

	// Phase 7 (§3/§5/§8) — the flywheel. SendOutcomes is the append-only outcome log (tests read it to
	// assert a blocked bounce never lands in a numerator, replay dedup, etc.); outcomeDedup maps
	// message_id+"\x00"+event -> struct{} for the §3.4 idempotency; Calibration is the versioned store;
	// RecalRuns is the recalibration lease/CAS log; cursors is the pull watermark per source.
	SendOutcomes []SendOutcomeRow
	outcomeDedup map[string]struct{}
	Calibration  []CalibrationRow
	RecalRuns    []RecalRun
	cursors      map[string]string
}

// callerRow mirrors one api_callers row (per-caller key + validity window).
type callerRow struct {
	callerID  string
	userID    string // api_callers.user_id (0013) — the PLATFORM user this key acts for; "" => NULL
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
		now:          now,
		cache:        map[string]CachedVerdict{},
		disposable:   map[string]struct{}{},
		spamTrap:     map[string]struct{}{},
		catchall:     map[string]bool{},
		policies:     map[string]PolicyRow{},
		health:       map[string]NodeHealth{},
		registered:   map[string]struct{}{},
		warmup:       map[string]warmupWindow{},
		rateBudget:   map[string]rateBucket{},
		callers:      map[string]callerRow{},
		workItems:    map[string]WorkItemRow{},
		idemKeys:     map[string]string{},
		jobs:         map[string]JobRow{},
		jobRows:      map[string][]JobRowEntry{},
		keyedJobs:    map[string]keyedJob{},
		regSeq:       map[string]int64{},
		auditSeq:     map[string]int64{},
		outcomeDedup: map[string]struct{}{},
		cursors:      map[string]string{},
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
	m.seq++
	m.auditSeq[row.ID] = m.seq // the row's place in time, for "recorded after the registration" (0019)
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

// ── Phase 5 (§3.3/§5/§7) — the durable async queue (Mem parity; CAS/lease faithfully reproduced) ──

// EnqueueWorkItem inserts a work-item, deduping on idempotency_key WITHIN ITS JOB (mirrors PG's
// per-job partial unique indexes and ON CONFLICT DO NOTHING, 0018): a duplicate key in the SAME job is a
// no-op returning alreadyExists=true and the id of the work item that holds the key. A different job
// never collides; a job-less item (item.JobID == "") dedups against other job-less items only.
func (m *MemStore) EnqueueWorkItem(_ context.Context, item WorkItemRow) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := item.JobID + "\x00" + hex.EncodeToString(item.IdempotencyKey)
	if existing, ok := m.idemKeys[key]; ok {
		return existing, true, nil // dedup (§5.5)
	}
	if item.Status == "" {
		item.Status = "ready"
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = item.NotBefore
	}
	m.workItems[item.ID] = item
	m.idemKeys[key] = item.ID
	return item.ID, false, nil
}

// RecordJob creates a job, records its input rows and appends the LOG rows of its malformed rows (mirrors
// PG's single transaction, 0018, INCLUDING the constraints the database enforces: a fake laxer than the real
// store is how a defect stays invisible to the suite). ALL-OR-NOTHING, like the transaction: every check runs
// before anything is stored, so a refused batch leaves no job, no row and no LOG row.
//
//   - the job id is new (jobs_pkey) and every reject LOG id is new (verifications_pkey);
//   - (job_id, ordinal) is unique and ordinal >= 0;
//   - (job_id, item_id) is unique where item_id is non-empty, and item_id is at most MaxItemIDBytes;
//   - a malformed row carries a verification_id and no work item; any other row a work item and no verification_id;
//   - duplicate_of is below the row's ordinal and never set on a malformed row;
//   - the rows and the rejects agree (checkRejectLinks, the one rule the database cannot see).
func (m *MemStore) RecordJob(_ context.Context, job JobRow, rows []JobRowEntry, rejects []VerificationRow) error {
	if err := checkRejectLinks(rows, rejects); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.recordJobLocked(job, rows, rejects)
}

// recordJobLocked is the body of RecordJob, with m.mu held: every check runs before anything is stored, so a refusal leaves nothing behind. It is
// shared with RecordKeyedJob so the keyed path cannot be laxer than the unkeyed one.
func (m *MemStore) recordJobLocked(job JobRow, rows []JobRowEntry, rejects []VerificationRow) error {
	if _, exists := m.jobs[job.ID]; exists {
		return fmt.Errorf("store: record job: job %s already exists (jobs_pkey)", job.ID)
	}
	haveLog := make(map[string]bool, len(m.Log))
	for _, r := range m.Log {
		haveLog[r.ID] = true
	}
	for _, r := range rejects {
		if haveLog[r.ID] {
			return fmt.Errorf("store: record job: LOG row %s already exists (verifications_pkey)", r.ID)
		}
	}
	haveOrdinal := make(map[int]bool, len(rows))
	haveItem := make(map[string]bool, len(rows))
	for _, r := range rows {
		switch {
		case r.Ordinal < 0:
			return fmt.Errorf("store: record job: ordinal %d is negative (job_rows_ordinal_nonneg)", r.Ordinal)
		case haveOrdinal[r.Ordinal]:
			return fmt.Errorf("store: record job: ordinal %d recorded twice (job_rows_pkey)", r.Ordinal)
		case len(r.ItemID) > MaxItemIDBytes:
			return fmt.Errorf("store: record job: item_id of row %d is %d bytes, over %d (job_rows_item_id_shape)", r.Ordinal, len(r.ItemID), MaxItemIDBytes)
		case r.ItemID != "" && haveItem[r.ItemID]:
			return fmt.Errorf("store: record job: item_id of row %d is already used in the job (uq_job_rows_item_id)", r.Ordinal)
		case r.Malformed && (r.WorkItemID != "" || r.VerificationID == ""):
			return fmt.Errorf("store: record job: malformed row %d must carry a verification and no work item (job_rows_outcome_shape)", r.Ordinal)
		case !r.Malformed && (r.WorkItemID == "" || r.VerificationID != ""):
			return fmt.Errorf("store: record job: row %d must carry a work item and no verification (job_rows_outcome_shape)", r.Ordinal)
		case r.DuplicateOf != nil && (r.Malformed || *r.DuplicateOf < 0 || *r.DuplicateOf >= r.Ordinal):
			return fmt.Errorf("store: record job: row %d: duplicate_of must be an earlier ordinal and never on a malformed row (job_rows_duplicate_of_shape)", r.Ordinal)
		}
		haveOrdinal[r.Ordinal] = true
		if r.ItemID != "" {
			haveItem[r.ItemID] = true
		}
	}
	if job.Status == "" {
		job.Status = "submitted"
	}
	m.jobs[job.ID] = job
	m.Log = append(m.Log, rejects...)
	m.jobRows[job.ID] = append([]JobRowEntry(nil), rows...)
	return nil
}

// DequeueReady leases up to `limit` ready items in group-by-domain order (§6.2). The MemStore's
// single-lock discipline gives SKIP-LOCKED-equivalent exclusivity (no concurrent dequeue races).
func (m *MemStore) DequeueReady(_ context.Context, workerID string, limit int, now time.Time) ([]WorkItemRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = 10
	}
	var ready []WorkItemRow
	for _, w := range m.workItems {
		if (w.Status == "ready" || w.Status == "waiting_retry") && !now.Before(w.NotBefore) {
			ready = append(ready, w)
		}
	}
	// ORDER BY domain, not_before (mirrors PG) — domain-spread + oldest-due within a domain.
	sort.Slice(ready, func(i, j int) bool {
		if ready[i].Domain != ready[j].Domain {
			return ready[i].Domain < ready[j].Domain
		}
		if !ready[i].NotBefore.Equal(ready[j].NotBefore) {
			return ready[i].NotBefore.Before(ready[j].NotBefore)
		}
		return ready[i].ID < ready[j].ID // stable tiebreak
	})
	var out []WorkItemRow
	for i := 0; i < len(ready) && len(out) < limit; i++ {
		w := ready[i]
		w.Status = "in_flight"
		w.LockedBy = workerID
		w.LockedAt = now.UTC()
		m.workItems[w.ID] = w
		out = append(out, w)
	}
	return out, nil
}

// SettleWorkItem mirrors PG's GAP-37/33/38 CAS settlement: the transition applies ONLY when the
// item is still in_flight AND held by workerID; otherwise ErrLeaseLost (no LOG row appended). On
// success: append the ONE LOG row, stamp verification_id, NULL the ciphertext, bump the job
// counter, append the transcript — all atomically under the single lock.
func (m *MemStore) SettleWorkItem(_ context.Context, itemID, workerID, terminal string, verdict VerificationRow, raw *RawRow, attempt WorkItemAttemptRow, extCall *ExternalCallRow) error {
	if terminal != "settled" && terminal != "dead" {
		return fmt.Errorf("store: settle: invalid terminal %q", terminal)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.workItems[itemID]
	if !ok || w.LockedBy != workerID || w.Status != "in_flight" {
		return ErrLeaseLost // reclaimed or already transitioned → discard (GAP-37/42); NO LOG/spend row
	}
	// (1) append the one LOG row (Door 2).
	m.Log = append(m.Log, verdict)
	if raw != nil {
		m.Raw = append(m.Raw, *raw)
	}
	// (2) CAS transition + purge ciphertext + clear lease.
	w.Status = terminal
	w.VerificationID = verdict.ID
	w.SettledAt = verdict.CheckedAt
	w.AddrCiphertext = nil // purge-on-settle (§5.3 step 4)
	w.LockedBy = ""
	w.LockedAt = time.Time{}
	m.workItems[itemID] = w
	// (3) bump the job counter (cheap progress; completion is authoritative — GAP-38).
	if w.JobID != "" {
		if j, ok := m.jobs[w.JobID]; ok {
			j.SettledCount++
			m.jobs[w.JobID] = j
		}
	}
	// (4) append the transcript (OQ-P5-8) — only if the caller recorded one.
	if attempt.ID != "" {
		m.attempts = append(m.attempts, attempt)
	}
	// (5) SPEND-ONCE (P6 §5.4/GAP-42): the vendor spend record commits with the settle. A reclaimed
	// attempt hit ErrLeaseLost above and never reaches here → the charge is recorded exactly once.
	if extCall != nil {
		m.ExternalCalls = append(m.ExternalCalls, *extCall)
	}
	return nil
}

// AppendExternalCall records a SYNC-path external call (§6.1) — a plain append (no CAS).
func (m *MemStore) AppendExternalCall(_ context.Context, call ExternalCallRow) error {
	if call.ID == "" {
		return fmt.Errorf("store: external call requires an id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ExternalCalls = append(m.ExternalCalls, call)
	return nil
}

// ExternalSpendSince mirrors PG: spend (ok calls only) over day/month/job + call/error counts over
// the error-rate window, from the append-only external-call log.
func (m *MemStore) ExternalSpendSince(_ context.Context, vendor, jobID string, dayStart, monthStart, errWindowStart, minuteStart, now time.Time) (ExternalSpend, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s ExternalSpend
	for _, c := range m.ExternalCalls {
		if c.Vendor != vendor {
			continue
		}
		if c.Outcome == "ok" {
			if !c.CalledAt.Before(dayStart) {
				s.SpentDay += c.CostUnits
			}
			if !c.CalledAt.Before(monthStart) {
				s.SpentMonth += c.CostUnits
			}
			if jobID != "" && c.JobID == jobID {
				s.SpentJob += c.CostUnits
			}
		}
		if !c.CalledAt.Before(errWindowStart) {
			s.Calls++
			if c.Outcome == "error" {
				s.Errors++
			}
		}
		if !c.CalledAt.Before(minuteStart) {
			s.CallsLastMinute++ // the rate_ceiling brake (§5.2) — all calls in the last minute
		}
	}
	return s, nil
}

// RequeueWorkItem mirrors PG's GAP-37 CAS requeue: transition ONLY when in_flight AND held by
// workerID, else ErrLeaseLost. bumpAttempt=false for a capacity wait (§4.6/OQ-P5-7).
func (m *MemStore) RequeueWorkItem(_ context.Context, itemID, workerID string, notBefore time.Time, reason string, bumpAttempt bool, attempt WorkItemAttemptRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.workItems[itemID]
	if !ok || w.LockedBy != workerID || w.Status != "in_flight" {
		return ErrLeaseLost
	}
	w.Status = "waiting_retry"
	if bumpAttempt {
		w.Attempt++
	}
	w.NotBefore = notBefore
	w.LastReason = reason
	w.LockedBy = ""
	w.LockedAt = time.Time{}
	m.workItems[itemID] = w
	if attempt.ID != "" {
		m.attempts = append(m.attempts, attempt)
	}
	return nil
}

// ReclaimExpiredLeases resets in_flight items whose lease predates now-leaseTTL (mirrors PG).
func (m *MemStore) ReclaimExpiredLeases(_ context.Context, leaseTTL time.Duration, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := now.Add(-leaseTTL)
	n := 0
	for id, w := range m.workItems {
		if w.Status == "in_flight" && w.LockedAt.Before(cutoff) {
			if !now.Before(w.NotBefore) {
				w.Status = "ready"
			} else {
				w.Status = "waiting_retry"
			}
			w.LockedBy = ""
			w.LockedAt = time.Time{}
			m.workItems[id] = w
			n++
		}
	}
	return n, nil
}

// GetWorkItem reads one item by id.
func (m *MemStore) GetWorkItem(_ context.Context, itemID string) (WorkItemRow, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.workItems[itemID]
	return w, ok, nil
}

// CreateJob inserts a jobs row.
func (m *MemStore) CreateJob(_ context.Context, job JobRow) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if job.Status == "" {
		job.Status = "submitted"
	}
	m.jobs[job.ID] = job
	return job.ID, nil
}

// GetJob reads one job by id, SCOPED TO ITS OWNER (0013 / fix-plan item 1, D1) — mirroring PG's
// `WHERE id=$1 AND owner_user_id=$2` EXACTLY, including the two NULL cases, because a fake that is
// laxer than the real store is how D1 stayed invisible to this suite in the first place (§8.3):
// with an in-memory store there is no SQL for a missing predicate to be absent from, so the mirror
// has to carry the property by hand.
//
//   - ownerUserID == "" matches NOTHING (PG: `owner_user_id = NULL` is NULL, not true).
//   - a job with OwnerUserID == "" (a NULL owner) is matched by NOBODY, including another "" caller
//     — NULL = NULL is not true either. Hence the two separate guards below rather than one ==.
//
// known=false covers both "no such id" and "not yours"; the handler maps both to the same 404.
func (m *MemStore) GetJob(_ context.Context, jobID, ownerUserID string) (JobRow, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[jobID]
	if !ok {
		return JobRow{}, false, nil
	}
	if ownerUserID == "" || j.OwnerUserID == "" || j.OwnerUserID != ownerUserID {
		return JobRow{}, false, nil // not yours (or ownerless) — indistinguishable from absent
	}
	return j, true, nil
}

// jobOwnedBy reports whether jobID exists AND is owned by ownerUserID, with PG's NULL semantics
// (see GetJob). Callers must already hold m.mu. Extracted so the three read paths cannot drift
// apart — the failure mode item 1 exists to fix was three read paths each deciding for itself.
func (m *MemStore) jobOwnedBy(jobID, ownerUserID string) (JobRow, bool) {
	j, ok := m.jobs[jobID]
	if !ok || ownerUserID == "" || j.OwnerUserID == "" || j.OwnerUserID != ownerUserID {
		return JobRow{}, false
	}
	return j, true
}

// JobCompletion judges completion AUTHORITATIVELY (mirrors PG NOT EXISTS — GAP-38): iterate items,
// not the counter. Returns total/settled from the job row for cheap display only.
// SCOPED TO ITS OWNER (0013 / item 1): a job owned by another caller returns ErrNotWired, exactly
// as a nonexistent one does — mirroring PG, where the owner predicate sits on the counts query so
// the non-owner never reaches the completion computation.
func (m *MemStore) JobCompletion(_ context.Context, jobID, ownerUserID string) (bool, int, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobOwnedBy(jobID, ownerUserID)
	if !ok {
		return false, 0, 0, ErrNotWired
	}
	complete := true
	for _, w := range m.workItems {
		if w.JobID == jobID && w.Status != "settled" && w.Status != "dead" {
			complete = false
			break
		}
	}
	// settled counts ROWS (mirrors PG, 0018): input rows whose work item is terminal, plus the malformed
	// rows (they have no work item to wait for). Computed on read, not from j.SettledCount.
	settled := 0
	for _, r := range m.jobRows[jobID] {
		if r.Malformed {
			settled++
			continue
		}
		if w, ok := m.workItems[r.WorkItemID]; ok && (w.Status == "settled" || w.Status == "dead") {
			settled++
		}
	}
	return complete, j.TotalCount, settled, nil
}

// ListJobResults returns the input rows of a job that have an outcome, joined to their verdicts,
// ORDERED BY ORDINAL and cursor-paginated by ordinal (mirrors PG, 0018). A row has an outcome when its
// work item is settled|dead or when it is malformed; identical addresses in one job share a work item,
// so their rows share a verdict and keep their own ordinal and item_id.
// SCOPED TO ITS OWNER (0013 / item 1) — the path that leaked the actual verdicts under D1. A
// non-owner gets ZERO ROWS and an empty cursor, mirroring PG's inner join back to jobs: the owner
// check happens BEFORE pagination, so no cursor/limit combination walks another tenant's results.
//
// The cursor is the ordinal of the LAST ROW RETURNED. The page is built by scanning rows in ordinal order
// and stopping at limit; "is there a next page" is learnt by finding one more qualifying row, whose
// ordinal is NEVER used as the cursor (that is the defect M2's campaign report had, M2-G5).
func (m *MemStore) ListJobResults(_ context.Context, jobID, ownerUserID, cursor string, limit int) ([]WorkItemResult, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = 100
	}
	after, cerr := ParseResultsCursor(cursor)
	if cerr != nil {
		return nil, "", cerr
	}
	if _, ok := m.jobOwnedBy(jobID, ownerUserID); !ok {
		return nil, "", nil // not yours (or absent) — an empty page, never a partial one
	}
	logByID := map[string]VerificationRow{}
	for _, r := range m.Log {
		logByID[r.ID] = r
	}
	rows := append([]JobRowEntry(nil), m.jobRows[jobID]...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Ordinal < rows[j].Ordinal })

	var out []WorkItemResult
	for _, r := range rows {
		if r.Ordinal <= after {
			continue
		}
		res := WorkItemResult{Ordinal: r.Ordinal, ItemID: r.ItemID, Malformed: r.Malformed, DuplicateOf: r.DuplicateOf}
		if r.Malformed {
			// A malformed row is answered by its OWN LOG row, written at submit (Door 2): served like every other result.
			res.Verdict = logByID[r.VerificationID]
		} else {
			w, ok := m.workItems[r.WorkItemID]
			if !ok || (w.Status != "settled" && w.Status != "dead") || w.VerificationID == "" {
				continue // no outcome yet
			}
			res.WorkItemID, res.EmailHash, res.Verdict = w.ID, w.EmailHash, logByID[w.VerificationID]
		}
		if len(out) == limit {
			// One more qualifying row exists, so another page does too. The cursor is the last row
			// RETURNED (out's tail), never this look-ahead row.
			return out, FormatResultsCursor(out[len(out)-1].Ordinal), nil
		}
		out = append(out, res)
	}
	return out, "", nil
}

// LookupCaller resolves a caller from sha256(bearer key) honoring the validity window (PN-13,
// §7.5) — mirrors PG EXACTLY: a match requires now ∈ [notBefore, notAfter) (notAfter zero => no
// expiry), so a rotated-out key yields ok=false. A miss is ok=false, never an error (→ 401).
// It also returns the platform user the key acts for (api_callers.user_id, 0013) — "" when the
// seeded row has none, mirroring a NULL user_id.
func (m *MemStore) LookupCaller(_ context.Context, keySHA256 []byte, now time.Time) (string, string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.callers[hex.EncodeToString(keySHA256)]
	if !ok {
		return "", "", false, nil
	}
	if now.Before(c.notBefore) {
		return "", "", false, nil // not yet valid
	}
	if !c.notAfter.IsZero() && !now.Before(c.notAfter) {
		return "", "", false, nil // expired (rotated out): now >= notAfter
	}
	return c.callerID, c.userID, true, nil
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
// rows for one callerID with different windows. The key carries NO platform user — use
// SeedCallerWithUser to seed one and exercise the 0013 owner path.
func (m *MemStore) SeedCaller(keySHA256 []byte, callerID string, notBefore, notAfter time.Time) {
	m.SeedCallerWithUser(keySHA256, callerID, "", notBefore, notAfter)
}

// SeedCallerWithUser is SeedCaller plus the platform user the key acts for (api_callers.user_id,
// 0013 / item 1) — the value that becomes jobs.owner_user_id at submit. Seed two keys with
// DIFFERENT userIDs to drive the cross-tenant paths; userID "" models a NULL user_id (a pre-0013
// key: it authenticates and can submit, but its jobs are ownerless and therefore read-closed).
func (m *MemStore) SeedCallerWithUser(keySHA256 []byte, callerID, userID string, notBefore, notAfter time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callers[hex.EncodeToString(keySHA256)] = callerRow{callerID: callerID, userID: userID, notBefore: notBefore, notAfter: notAfter}
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

// WorkItemsForJob returns every work item belonging to jobID (test-support). It exists so a test can
// assert that submitted work was STORED, not merely acknowledged — the property kill-switch Rung 4
// accept-and-defer rests on ("work sits durably in work_items"). Deliberately NOT part of the Store
// interface: production reads job progress through JobCompletion/ListJobResults, which are
// owner-scoped (0013); this raw, unscoped view is a fake-only affordance.
func (m *MemStore) WorkItemsForJob(jobID string) []WorkItemRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []WorkItemRow
	for _, w := range m.workItems {
		if w.JobID == jobID {
			out = append(out, w)
		}
	}
	return out
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
