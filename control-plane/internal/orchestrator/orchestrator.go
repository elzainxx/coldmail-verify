// Package orchestrator runs the /v1/verify request lifecycle (P1 §5.2, SVG ①):
//
//	normalize (Door 3) → cache lookup → policy plan → cheap layers → score
//	→ verdict assembler (single source: Response + LOG row) → LOG → cache write → respond
//
// It is the seam that turns the Phase-0 honest-unknown stub into a real scored+logged verdict
// using cheap layers only — NO port 25 anywhere (Phase-1 done-when). Every result is LOGged
// before it is returned, including cache hits (from_cache=true) and un-addressable rejects
// (invalid_syntax, NULL hash — §3.2.7). The LOG write failing FAILS the request: an unlogged
// verdict violates Door 2 ("the log is the point"), so we never return one we couldn't log.
package orchestrator

import (
	"context"
	"errors"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/hash"
	"github.com/mnfi/codmailling/control-plane/internal/idgen"
	"github.com/mnfi/codmailling/control-plane/internal/layers"
	"github.com/mnfi/codmailling/control-plane/internal/policy"
	"github.com/mnfi/codmailling/control-plane/internal/store"
	"github.com/mnfi/codmailling/control-plane/internal/verdict"
	"github.com/mnfi/codmailling/control-plane/internal/vocab"
)

// Orchestrator wires the cheap-path dependencies. It holds no per-request state.
type Orchestrator struct {
	hasher   *hash.Hasher
	store    store.Store
	resolver layers.Resolver
	engine   *policy.Engine
	layerSet map[string]layers.Layer
	now      func() time.Time

	// cacheTTL is the DIAL for cache.expires_at (P1 OQ-9). Zero means "unset" → caching is
	// SKIPPED entirely (we never write a row with a guessed/absent TTL — expires_at is NOT
	// NULL, §4.2). The verdict is still logged; only the cache write is skipped.
	cacheTTL time.Duration

	// cfgFingerprint is the P1 config fingerprint (§3.2.5): brain+policy config only (the
	// engine inputs are "unconfigured" in P1 — no live engine). Precomputed at wire time.
	cfgFingerprint string
}

// Deps carries the orchestrator's dependencies.
type Deps struct {
	Hasher            *hash.Hasher
	Store             store.Store
	Resolver          layers.Resolver
	Now               func() time.Time
	CacheTTL          time.Duration
	ConfigFingerprint string
}

// New wires an orchestrator. Resolver defaults to the real net resolver; Now to time.Now.
func New(d Deps) *Orchestrator {
	res := d.Resolver
	if res == nil {
		res = layers.NewNetResolver()
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Orchestrator{
		hasher:         d.Hasher,
		store:          d.Store,
		resolver:       res,
		engine:         policy.NewEngine(),
		layerSet:       policy.LayerSet(),
		now:            now,
		cacheTTL:       d.CacheTTL,
		cfgFingerprint: d.ConfigFingerprint,
	}
}

// ErrNotReady means a core dependency (hasher or store) is missing — the control-plane
// cannot hash or log, so it must not pretend to verify. The API maps this to 503.
var ErrNotReady = errors.New("orchestrator: not ready (missing hasher or store)")

// Verify runs the full lifecycle for one address under the given policy definition, and
// returns the Door-1 response. The verdict is LOGged (append-only) before returning.
func (o *Orchestrator) Verify(ctx context.Context, rawEmail string, def *policy.Definition) (verdict.Response, error) {
	if o.hasher == nil || o.store == nil {
		return verdict.Response{}, ErrNotReady
	}
	start := o.now()

	// 1. Normalize + hash (Door 3). An unparseable address is an un-addressable reject:
	//    logged with a NULL hash (§3.2.7), status invalid/invalid_syntax — never dropped.
	normalized, domain, err := o.hasher.Normalize(rawEmail)
	if errors.Is(err, hash.ErrUnparseable) {
		return o.finishReject(ctx, rawEmail, start, def)
	}
	if err != nil {
		return verdict.Response{}, err
	}
	emailHash := o.hasher.EmailHash(normalized)
	localPart := normalized[:indexAt(normalized)]

	// 2. Cache lookup on the COMPOSITE key (email_hash, config_fingerprint) — §4.2. A hit is
	//    still LOGged with from_cache=true (§4.2/OQ-9). A miss/expiry falls through to compute.
	if cv, cerr := o.store.GetCachedVerdict(ctx, emailHash, o.cfgFingerprint); cerr == nil {
		return o.finishFromCache(ctx, rawEmail, domain, emailHash, localPart, cv, start, def)
	} else if !errors.Is(cerr, store.ErrCacheMiss) {
		return verdict.Response{}, cerr
	}

	// 3. Evaluate the cheap layers under the policy (cheapest-sufficient stop-early).
	ev := layers.NewEvidence(rawEmail, normalized, localPart, domain, o.resolver, listAdapter{o.store})
	v, err := o.engine.Evaluate(ctx, def, ev, o.layerSet)
	if err != nil {
		return verdict.Response{}, err
	}

	// 4. Assemble (single source: response + LOG row), 5. LOG, 6. cache write, 7. respond.
	prov := verdict.Provenance{
		RequestID: mustID(o.now()), CheckedAt: start, LatencyMS: sinceMS(o.now(), start),
		PolicyKey: def.Key, PolicyVersion: def.Version, ConfigFingerprint: o.cfgFingerprint,
		FromCache: false,
		RawEmail:  rawEmail, NormalizedDomain: domain, EmailHash: emailHash,
		HashScheme: hash.SchemeTag, LocalPartLen: len(localPart), MXProvider: mxProviderOf(v.Signals),
		ProviderCanonicalHash: o.providerCanonical(normalized, v.Signals),
	}
	resp, row := verdict.Assemble(toCore(v), prov)
	if err := o.logAndCache(ctx, resp, row, emailHash, domain, prov, false); err != nil {
		return verdict.Response{}, err
	}
	return resp, nil
}

// finishReject handles an un-addressable input: invalid/invalid_syntax, NULL hash, logged.
func (o *Orchestrator) finishReject(ctx context.Context, rawEmail string, start time.Time, def *policy.Definition) (verdict.Response, error) {
	core := verdict.Core{
		Status: vocab.StatusInvalid, Reason: vocab.ReasonInvalidSyntax,
		Confidence: confidenceForReject(def), Recommendation: vocab.RecSkip,
		Flags: map[string]bool{}, Signals: map[string]any{"syntax_valid": false},
	}
	prov := verdict.Provenance{
		RequestID: mustID(o.now()), CheckedAt: start, LatencyMS: sinceMS(o.now(), start),
		PolicyKey: def.Key, PolicyVersion: def.Version, ConfigFingerprint: o.cfgFingerprint,
		RawEmail: rawEmail, NormalizedDomain: "", EmailHash: nil, // NULL hash (§3.2.7)
	}
	resp, row := verdict.Assemble(core, prov)
	// An un-hashable reject is NOT cached (its key would be NULL) — logged only.
	if err := o.logOnly(ctx, resp, row); err != nil {
		return verdict.Response{}, err
	}
	return resp, nil
}

// finishFromCache re-logs a cache hit (from_cache=true) and returns the cached verdict.
func (o *Orchestrator) finishFromCache(ctx context.Context, rawEmail, domain string, emailHash []byte, localPart string, cv store.CachedVerdict, start time.Time, def *policy.Definition) (verdict.Response, error) {
	core := verdict.Core{
		Status: vocab.Status(cv.Status), Reason: vocab.Reason(cv.Reason),
		Confidence: cv.Confidence, Recommendation: vocab.Recommendation(cv.Recommendation),
		Flags: cv.Flags, Signals: cv.Signals,
	}
	prov := verdict.Provenance{
		RequestID: mustID(o.now()), CheckedAt: start, LatencyMS: sinceMS(o.now(), start),
		PolicyKey: def.Key, PolicyVersion: def.Version, ConfigFingerprint: o.cfgFingerprint,
		FromCache: true,
		RawEmail:  rawEmail, NormalizedDomain: domain, EmailHash: emailHash,
		HashScheme: hash.SchemeTag, LocalPartLen: len(localPart), MXProvider: cv.MXProvider,
	}
	resp, row := verdict.Assemble(core, prov)
	// A cache hit is logged (from_cache=true) but NOT re-written to the cache.
	if err := o.logOnly(ctx, resp, row); err != nil {
		return verdict.Response{}, err
	}
	return resp, nil
}

// logAndCache assigns the LOG id, appends the row (Door 2), and — if a TTL is configured and
// the address is hashable — writes the cache. writeCache=false skips the cache write.
func (o *Orchestrator) logAndCache(ctx context.Context, resp verdict.Response, row store.VerificationRow, emailHash []byte, domain string, prov verdict.Provenance, _ bool) error {
	if err := o.logOnly(ctx, resp, row); err != nil {
		return err
	}
	// Cache only settled cheap verdicts, and only when a TTL is set (expires_at is NOT NULL,
	// §4.2). No TTL configured (OQ-9 unset) => skip caching, never write a guessed TTL.
	if o.cacheTTL <= 0 || len(emailHash) == 0 {
		return nil
	}
	now := o.now()
	cv := store.CachedVerdict{
		EmailHash: emailHash, ConfigFingerprint: o.cfgFingerprint, Domain: domain,
		MXProvider: prov.MXProvider, HashScheme: hash.SchemeTag,
		Status: string(resp.Status), Reason: string(resp.Reason), Confidence: resp.Confidence,
		Recommendation: string(resp.Recommendation), Flags: resp.Flags, Signals: resp.Signals,
		CachedAt: now, ExpiresAt: now.Add(o.cacheTTL),
	}
	return o.store.PutCachedVerdict(ctx, cv)
}

// logOnly assigns a fresh UUIDv7 id and appends the LOG row. A failure here FAILS the request
// (Door 2: never return a verdict we couldn't log).
func (o *Orchestrator) logOnly(ctx context.Context, resp verdict.Response, row store.VerificationRow) error {
	id, err := idgen.NewV7String(o.now())
	if err != nil {
		return err
	}
	row.ID = id
	// Defensive: never persist/emit an out-of-contract verdict (Door 1/4).
	if !resp.Valid() {
		return errors.New("orchestrator: refusing to log an out-of-contract verdict")
	}
	return o.store.AppendVerification(ctx, row, nil) // no raw sidecar on the cheap path
}

func (o *Orchestrator) providerCanonical(normalized string, signals map[string]any) []byte {
	prov := mxProviderOf(signals)
	if prov == "" {
		return nil
	}
	h, ok := o.hasher.ProviderCanonicalHash(normalized, prov)
	if !ok {
		return nil
	}
	return h
}

// listAdapter adapts the store to the layers.ListSource capability interface.
type listAdapter struct{ s store.Store }

func (a listAdapter) IsDisposable(ctx context.Context, d string) (bool, error) {
	return a.s.IsDisposable(ctx, d)
}
func (a listAdapter) IsSpamTrap(ctx context.Context, d string) (bool, error) {
	return a.s.IsSpamTrap(ctx, d)
}

// ── small helpers ────────────────────────────────────────────────────────────────────

func toCore(v policy.Verdict) verdict.Core {
	return verdict.Core{
		Status: v.Status, Reason: v.Reason, Confidence: v.Confidence,
		Recommendation: v.Recommendation, Flags: v.Flags, Signals: v.Signals,
	}
}

func mxProviderOf(signals map[string]any) string {
	if s, ok := signals["mx_provider"].(string); ok {
		return s
	}
	return ""
}

func confidenceForReject(def *policy.Definition) int {
	if v, ok := def.Confidence.BaselineByReason["invalid_syntax"]; ok {
		return v
	}
	return 99
}

func indexAt(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '@' {
			return i
		}
	}
	return 0
}

// mustID returns a bare UUIDv7 string for request_id. It must be a plain UUID (no prefix):
// the LOG's request_id column is Postgres `uuid` (§3.2.2). A crypto/rand failure is
// vanishingly unlikely; if it happens we still return a valid all-zero-ish UUID rather than
// a non-UUID sentinel that the DB would reject.
func mustID(t time.Time) string {
	id, err := idgen.NewV7String(t)
	if err != nil {
		return "00000000-0000-7000-8000-000000000000"
	}
	return id
}

func sinceMS(now, start time.Time) int {
	ms := int(now.Sub(start) / time.Millisecond)
	if ms < 0 {
		return 0
	}
	return ms
}
