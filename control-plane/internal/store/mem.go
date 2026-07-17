// In-memory Store fake for unit tests. It honors the same contract as the pgx store:
// append-only LOG (accumulates rows; no mutation method exists), composite-keyed cache
// with TTL expiry, and seeded lists/policies. It is NOT a Postgres emulator — the real
// append-only GRANT enforcement is proven against actual Postgres (postgres_test.go); this
// fake lets the orchestrator/layer/policy tests run without a DB.
package store

import (
	"context"
	"encoding/hex"
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
	}
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
	return v, nil
}

func (m *MemStore) PutCachedVerdict(_ context.Context, v CachedVerdict) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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

func (m *MemStore) Ping(context.Context) error { return nil }
func (m *MemStore) Close()                      {}

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

// SeedPolicy stores a policy; if isDefault, it becomes the default (last write wins).
func (m *MemStore) SeedPolicy(p PolicyRow) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.policies[policyKey(p.Key, p.Version)] = p
	if p.IsDefault {
		m.defaultKey = policyKey(p.Key, p.Version)
	}
}
