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
	"github.com/mnfi/codmailling/control-plane/internal/smtp"
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

	// cfgFingerprint is the config fingerprint (§3.2.5). In P1 the engine inputs are
	// "unconfigured"; in P2 the caller recomputes it over the fork's /v1/config effective config
	// (§4.6, GAP-3) and passes it here. Precomputed at wire time.
	cfgFingerprint string

	// smtp is the OPTIONAL Phase-2 SMTP stage (§7.1). nil => cheap-layers-only (Phase-1 steady
	// state / kill-switch Rung 3). When wired AND the cheap verdict is the non-terminal residue
	// AND a node is routable, the SMTP stage resolves it against the verifier-node.
	smtp *SMTPDeps
}

// Deps carries the orchestrator's dependencies.
type Deps struct {
	Hasher            *hash.Hasher
	Store             store.Store
	Resolver          layers.Resolver
	Now               func() time.Time
	CacheTTL          time.Duration
	ConfigFingerprint string
	// SMTP is the OPTIONAL Phase-2 stage. nil => cheap-layers-only (P1 steady state). Wire it
	// only when SMTP is enabled (kill-switch not SMTP-off) and a node list exists.
	SMTP *SMTPDeps
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
		smtp:           d.SMTP,
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

	requestID := mustID(o.now())

	// 3b. SMTP stage (Phase 2, §7.1). Runs ONLY when the cheap verdict is the non-terminal
	//     residue (risky/plausible, no gate stopped it) AND SMTP is wired AND a node is routable.
	//     A definitive cheap gate (invalid/no_mx, null_mx, disposable, spam_trap) or a :mx-routed
	//     provider (provider_no_smtp) is already terminal → SMTP is skipped (cheapest-sufficient).
	var rawBlob []byte
	var nodeID string
	if smtpEligible(v) {
		if res, blob, nid, ran := o.runSMTP(ctx, normalized, domain, requestID, v); ran {
			v = foldSMTP(v, res)
			rawBlob, nodeID = blob, nid
			o.persistCatchAll(ctx, domain, res)
		}
	}

	// 4. Assemble (single source: response + LOG row), 5. LOG, 6. cache write, 7. respond.
	prov := verdict.Provenance{
		RequestID: requestID, CheckedAt: start, LatencyMS: sinceMS(o.now(), start),
		PolicyKey: def.Key, PolicyVersion: def.Version, ConfigFingerprint: o.cfgFingerprint,
		FromCache: false,
		RawEmail:  rawEmail, NormalizedDomain: domain, EmailHash: emailHash,
		HashScheme: hash.SchemeTag, LocalPartLen: len(localPart), MXProvider: mxProviderOf(v.Signals),
		ProviderCanonicalHash: o.providerCanonical(normalized, v.Signals),
		NodeID:                nodeID,
	}
	resp, row := verdict.Assemble(toCore(v), prov)
	// Cache only SETTLED verdicts (§6.5/OQ-P2-6): a transient/your-fault unknown from SMTP must
	// NOT be cached (caching it would suppress the retry that resolves it). Cheap verdicts and
	// settled SMTP verdicts are cacheable; the cheap residue never reaches here as unknown.
	cacheable := smtp.CacheableReason(v.Status, v.Reason)
	if err := o.logAndCacheRaw(ctx, resp, row, emailHash, domain, prov, cacheable, rawBlob); err != nil {
		return verdict.Response{}, err
	}
	return resp, nil
}

// foldSMTP replaces the cheap residue with the scored SMTP verdict, MERGING the SMTP signals into
// the existing signals bag (additive — §8) rather than discarding the cheap evidence. The SMTP
// status/reason/confidence win (SMTP is the Type-1 core layer that resolves the residue, §7.1).
func foldSMTP(cheap policy.Verdict, res smtp.Result) policy.Verdict {
	signals := cheap.Signals
	if signals == nil {
		signals = map[string]any{}
	}
	for k, val := range res.Signals {
		signals[k] = val // additive P2 signal keys (§8)
	}
	rec := recommendationFor(res.Status, res.Reason)
	return policy.Verdict{
		Status: res.Status, Reason: res.Reason, Confidence: res.Confidence,
		Recommendation: rec, Flags: cheap.Flags, Signals: signals,
		StoppedAt: "smtp",
	}
}

// recommendationFor derives the recommendation from the SMTP status honoring the LOCKED
// composability invariants (vocab.ComposabilityOK). 552 mailbox_full is valid but rec=review
// (existence proven, deliver-now uncertain, §6.1.1). valid/accepted → send. Everything else →
// the safe legal value. Never emits the forbidden invalid+send.
func recommendationFor(status vocab.Status, reason vocab.Reason) vocab.Recommendation {
	switch status {
	case vocab.StatusValid:
		if reason == vocab.ReasonMailboxFull {
			return vocab.RecReview // "full now" caveat rides the recommendation (§6.1.1)
		}
		return vocab.RecSend
	case vocab.StatusInvalid:
		return vocab.RecSkip
	default: // risky / unknown
		return vocab.RecReview
	}
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

// logAndCacheRaw assigns the LOG id, appends the row + optional raw engine sidecar blob (Door 2),
// and — if the verdict is cacheable AND a TTL is configured AND the address is hashable — writes
// the cache. cacheable=false skips the cache write (§6.5: never cache a transient unknown). The
// raw blob (the fork's /v1/verify response incl. smtp_debug) rides the verification_raw sidecar
// (§6.4 — no new column, no append-only violation).
func (o *Orchestrator) logAndCacheRaw(ctx context.Context, resp verdict.Response, row store.VerificationRow, emailHash []byte, domain string, prov verdict.Provenance, cacheable bool, rawBlob []byte) error {
	if err := o.logWithRaw(ctx, resp, row, rawBlob); err != nil {
		return err
	}
	// Cache only when the verdict is settled AND a TTL is set (expires_at is NOT NULL, §4.2). No
	// TTL configured (OQ-9 unset) => skip caching, never write a guessed TTL. A transient unknown
	// (cacheable=false) is never cached even with a TTL (§6.5/OQ-P2-6).
	if !cacheable || o.cacheTTL <= 0 || len(emailHash) == 0 {
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

// logOnly assigns a fresh UUIDv7 id and appends the LOG row with NO raw sidecar (cheap path,
// rejects, cache re-logs). A failure here FAILS the request (Door 2: never return a verdict we
// couldn't log).
func (o *Orchestrator) logOnly(ctx context.Context, resp verdict.Response, row store.VerificationRow) error {
	return o.logWithRaw(ctx, resp, row, nil)
}

// logWithRaw assigns a fresh UUIDv7 id and appends the LOG row plus an optional raw engine blob
// (the fork's /v1/verify response) into the verification_raw sidecar (§6.4). The append is atomic
// (the store wraps both in one tx). A failure FAILS the request (Door 2).
func (o *Orchestrator) logWithRaw(ctx context.Context, resp verdict.Response, row store.VerificationRow, rawBlob []byte) error {
	id, err := idgen.NewV7String(o.now())
	if err != nil {
		return err
	}
	row.ID = id
	// Defensive: never persist/emit an out-of-contract verdict (Door 1/4).
	if !resp.Valid() {
		return errors.New("orchestrator: refusing to log an out-of-contract verdict")
	}
	var raw *store.RawRow
	if len(rawBlob) > 0 {
		raw = &store.RawRow{VerificationID: id, Raw: rawBlob}
	}
	return o.store.AppendVerification(ctx, row, raw)
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
