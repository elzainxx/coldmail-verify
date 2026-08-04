// Package api is the control-plane's HTTP front door (P1 §5.2). Phase 0 serves:
//   - POST /v1/verify  : token-auth + stubbed verdict (honest unknown/plausible)
//   - GET  /healthz    : liveness (no deps)
//   - GET  /readyz     : readiness (reports config summary + secret presence, NOT values)
//
// No feature logic: /v1/verify returns the Door-1 shape with a Phase-0 honest verdict; it
// does NOT run cheap layers, route, or hit a verifier-node (those are P1/P2). It proves the contract
// shape, the auth seam, and the request lifecycle skeleton.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/coldmail-verify/control-plane/internal/config"
	"github.com/coldmail-verify/control-plane/internal/hash"
	"github.com/coldmail-verify/control-plane/internal/orchestrator"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/queue"
	"github.com/coldmail-verify/control-plane/internal/secrets"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/verdict"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// Verifier is the request-lifecycle dependency the front door delegates to (the orchestrator).
// An interface so tests can inject a fake without a DB.
type Verifier interface {
	Verify(ctx context.Context, rawEmail string, def *policy.Definition) (verdict.Response, error)
}

// CallerAuth is the narrow per-caller key lookup the front door needs (PN-13, satisfied by
// store.Store). Injected so api tests use a fake without a DB. nil => per-caller auth not wired
// (the legacy static-token path governs; the bulk endpoint is then unavailable).
type CallerAuth interface {
	// ownerUserID is the PLATFORM user the key acts for (api_callers.user_id, 0013 / item 1) — the
	// job-owner value, resolved from the CREDENTIAL so no request body can assert it.
	LookupCaller(ctx context.Context, keySHA256 []byte, now time.Time) (callerID, ownerUserID string, ok bool, err error)
}

// BulkSubmitter enqueues a bulk submission and returns a job handle + counts (§7.1, satisfied by
// *queue.Enqueuer). callerRef is the opaque platform correlation string (§7.6). ownerUserID is the
// authenticated key's platform user, stamped onto the job (0013). Injected so tests use a fake
// without a real queue/DB.
type BulkSubmitter interface {
	SubmitBulk(ctx context.Context, addresses []string, policyKey string, policyVersion int, callerRef, ownerUserID string) (queue.BulkResult, error)
}

// JobReader reads job progress + paginated results (§7.1, satisfied by store.Store). The results
// are byte-identical /v1/verify verdict objects (Door 1).
//
// EVERY METHOD TAKES ownerUserID (0013 / fix-plan item 1, D1; contract §1.4). Before that the three
// signatures took jobID alone, and the handlers held an authenticated caller they discarded — so any
// key could read any job's status and verdicts. Widening the INTERFACE, not just the SQL, is the
// point: a JobReader implementation (including a test fake) can no longer be written that ignores
// tenancy without visibly dropping a parameter.
type JobReader interface {
	GetJob(ctx context.Context, jobID, ownerUserID string) (store.JobRow, bool, error)
	JobCompletion(ctx context.Context, jobID, ownerUserID string) (complete bool, total, settled int, err error)
	ListJobResults(ctx context.Context, jobID, ownerUserID, cursor string, limit int) ([]store.WorkItemResult, string, error)
}

// callerIDKey is the context key under which an authenticated caller_id is stashed (never PII —
// GAP-36; the platform holds the user↔caller_id map). Distinct type so it can't collide.
type callerIDKey struct{}

// Server holds the wired dependencies. Deps are interfaces/values, so tests inject fakes.
type Server struct {
	cfg        config.Config
	secrets    *secrets.Secrets
	hasher     *hash.Hasher  // may be nil if no pepper loaded (readiness reports it)
	verifier   Verifier      // nil until the store+orchestrator are wired (readiness reports it)
	callerAuth CallerAuth    // nil => per-caller auth not wired (legacy static token governs)
	bulk         BulkSubmitter // nil => bulk endpoint unavailable (returns 503)
	jobs         JobReader     // nil => job endpoints unavailable
	bulkMaxAddrs int           // §7.5 submission cap; 0 => the built-in safe hard cap (dial UNSET)
	defPolicy    *policy.Definition
	now          func() time.Time
}

// New wires the server. A nil hasher/verifier is allowed for a build smoke (no pepper / no
// DB); /readyz reports "not ready" and /v1/verify returns 503 rather than pretending.
func New(cfg config.Config, sec *secrets.Secrets, hasher *hash.Hasher) *Server {
	return &Server{cfg: cfg, secrets: sec, hasher: hasher, now: time.Now}
}

// WithVerifier attaches the orchestrator + the default policy (the real /v1/verify path).
func (s *Server) WithVerifier(v Verifier, def *policy.Definition) *Server {
	s.verifier = v
	s.defPolicy = def
	return s
}

// WithCallerAuth attaches the per-caller key lookup (PN-13). Once wired, a valid per-caller key
// authenticates any endpoint; the bulk surface REQUIRES one (the legacy static token is rejected
// there). The sync /v1/verify additionally accepts the legacy static token during transition.
func (s *Server) WithCallerAuth(c CallerAuth) *Server {
	s.callerAuth = c
	return s
}

// WithBulk attaches the P5 bulk submitter + job reader (§7). The bulk routes are served only when
// both are wired AND per-caller auth is present (PN-13 gates the bulk-PII surface). A nil pair
// leaves the bulk endpoints returning 503 (honest not-ready), never a fabricated handle. maxAddrs
// is the §7.5 submission-backpressure cap; <=0 => use the built-in safe hard cap (the dial is
// owner-OQ/UNSET, so an operator hasn't tuned it — we still bound the surface, never unbounded).
func (s *Server) WithBulk(b BulkSubmitter, j JobReader, maxAddrs int) *Server {
	s.bulk = b
	s.jobs = j
	if maxAddrs > 0 {
		s.bulkMaxAddrs = maxAddrs
	}
	return s
}

// Handler builds the mux. Kept explicit (stdlib) — no framework dependency.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/verify", s.handleVerify)
	// P5 bulk (§7): the async submission → handle → progress → results lifecycle. These are the
	// bulk-PII surface — auth REQUIRES a per-caller key (PN-13); the legacy static token is rejected.
	mux.HandleFunc("POST /v1/verify/bulk", s.handleBulkSubmit)
	mux.HandleFunc("GET /v1/jobs/{id}", s.handleJobStatus)
	mux.HandleFunc("GET /v1/jobs/{id}/results", s.handleJobResults)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	return mux
}

type verifyRequest struct {
	Email  string `json:"email"`
	Policy string `json:"policy,omitempty"`
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	// Sync /v1/verify: a per-caller key OR (during transition) the legacy static token authenticates
	// (requirePerCaller=false). The bulk-PII surface (requirePerCaller=true) is handled separately.
	// The owner id is not used here: /v1/verify is synchronous and stores no job, so there is
	// nothing to own. It is discarded EXPLICITLY rather than by an unnamed `_` so that the contrast
	// with the bulk path below is visible — that path's `_ = callerID` was precisely D1.
	callerID, _, ok := s.authenticate(r, false)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if callerID != "" {
		r = r.WithContext(context.WithValue(r.Context(), callerIDKey{}, callerID))
	}
	// Front-door mode (kill-switch Rung 4): BOTH sub-modes stop the sync surface, because there is no
	// deferred form of a synchronous answer. /v1/verify has no job handle to return and no queue row
	// to defer into — its response IS the verdict. At a rung whose definition is "verdict production
	// disabled", the only two things this handler could do are produce a verdict anyway (defeating
	// the rung) or return 200 with something that is not a verdict (a fabricated answer, forbidden
	// by Doors 1 and 2). So it returns the explicit unavailable envelope, and the bulk surface —
	// which DOES have somewhere to defer to — keeps accepting. That asymmetry is the rung working as
	// specified, not an inconsistency: "either a job handle + paused job status (accept-and-defer),
	// or an explicit unavailable response (reject)" (kill-switch-ladder.md Rung 4).
	//
	// Before fix-plan item 4 this compared only against FrontDoorReject, so accept_and_defer served
	// verdicts at full capability (D2).
	if s.cfg.KillSwitch.VerdictProductionPaused() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "service paused"})
		return
	}
	var req verifyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email is required"})
		return
	}

	// The real request lifecycle runs in the orchestrator (cache → policy → cheap layers →
	// score → LOG → cache → respond). If it isn't wired (no DB/pepper), we are NOT ready —
	// return 503 rather than a fabricated verdict (Door 2: never a verdict we can't log).
	if s.verifier == nil || s.defPolicy == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "not ready"})
		return
	}
	// P1 ships exactly one policy (the default). A request naming a different policy is
	// not-found for now (the seam for multiple policies exists in the store/§4.6).
	if req.Policy != "" && req.Policy != s.defPolicy.Key {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown policy"})
		return
	}

	resp, err := s.verifier.Verify(r.Context(), req.Email, s.defPolicy)
	if err != nil {
		if errors.Is(err, orchestrator.ErrNotReady) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "not ready"})
			return
		}
		// A processing failure must NOT become a fabricated verdict — surface 500 honestly.
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "verification failed"})
		return
	}
	if !resp.Valid() {
		// Defensive: never emit an out-of-contract response (Door 1/4). Should be impossible.
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal contract violation"})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// bulkRequest is the /v1/verify/bulk body. addresses is the work list; policy names the governing
// policy (default when empty); caller_ref is an OPAQUE correlation string the brain echoes but
// never interprets (§7.6/GAP-36 — NOT a user id; the platform maps its users↔caller_ref). There is
// deliberately NO CSV, filename, credits, or per-row field here (the two-job boundary).
type bulkRequest struct {
	Addresses []string `json:"addresses"`
	Policy    string   `json:"policy,omitempty"`
	CallerRef string   `json:"caller_ref,omitempty"`
}

// maxBulkAddresses is the built-in safe hard cap on one submission (§7.5 backpressure) used when the
// TM_QUEUE_BULK_MAX_ADDRESSES dial is UNSET (owner-OQ). It bounds the surface so an unbounded upload
// can't exhaust the brain/DB even before an operator tunes the dial; a Set dial overrides it via
// s.bulkMaxAddrs. Over-limit → 413 with a clear message, never a silent truncation.
const maxBulkAddresses = 100_000

// bulkAddressCap returns the effective per-submission cap: the configured dial when Set, else the
// built-in hard cap. Never unbounded (the dial being UNSET does not mean "no limit").
func (s *Server) bulkAddressCap() int {
	if s.bulkMaxAddrs > 0 {
		return s.bulkMaxAddrs
	}
	return maxBulkAddresses
}

// handleBulkSubmit is POST /v1/verify/bulk (§7.1): authenticate with a PER-CALLER key (PN-13 —
// this is the bulk-PII surface; the legacy static token is rejected here), enqueue the work, and
// return 202 + a job handle IMMEDIATELY (no inline verdicts, no SMTP — the drainer runs it later
// through the identical P4 gates). The verdict shape is unchanged (Door 1); this is a NEW sibling
// endpoint, not a change to /v1/verify.
func (s *Server) handleBulkSubmit(w http.ResponseWriter, r *http.Request) {
	// PN-13: the bulk surface REQUIRES a per-caller key (requirePerCaller=true).
	callerID, ownerUserID, ok := s.authenticate(r, true)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	// Rung 4 REJECT stops here; Rung 4 ACCEPT-AND-DEFER deliberately does NOT (that is the whole
	// distinction between the two sub-modes — see FrontDoorDeferring). A deferred submission is
	// accepted, sealed, and stored durably in work_items exactly as normal; the only difference is
	// that the drainer will not pick it up until the rung is released. The 202 below carries
	// "processing_paused": true so the caller is TOLD, rather than watching a job sit at zero settled
	// with no explanation.
	if s.cfg.KillSwitch.FrontDoorRejecting() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "service paused"})
		return
	}
	if s.bulk == nil || s.defPolicy == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "not ready"})
		return
	}
	// Bound the body (a 100k-address list of ~small strings; generous cap, still bounded).
	var req bulkRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if len(req.Addresses) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "addresses is required and must be non-empty"})
		return
	}
	if cap := s.bulkAddressCap(); len(req.Addresses) > cap {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"error": "too many addresses", "max": cap, "got": len(req.Addresses),
		})
		return
	}
	if req.Policy != "" && req.Policy != s.defPolicy.Key {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown policy"})
		return
	}
	_ = callerID // authenticated principal; rate-limiting/attribution by caller_id is a later dial

	// OWNERSHIP IS STAMPED HERE (0013 / fix-plan item 1, D1). ownerUserID comes from
	// authenticate() → LookupCaller → api_callers.user_id — the CREDENTIAL. req.CallerRef, one
	// argument to its left, comes from the BODY and is untrusted; the two must never be confused
	// (see SubmitBulk's own note). This line is what makes the three read predicates meaningful:
	// without a stamped owner every job would be ownerless and no caller could read anything.
	//
	// Note what is NOT here: any read of an owner/user/tenant field off req. There is no such field
	// on bulkRequest, and adding one would be the regression — decodeStrict-style unknown-field
	// rejection is not in play on this endpoint, so a `user_id` in the body is silently ignored
	// today. That is the correct behaviour for a credential-based module, and the cross-tenant test
	// asserts it stays that way.
	res, err := s.bulk.SubmitBulk(r.Context(), req.Addresses, s.defPolicy.Key, s.defPolicy.Version, req.CallerRef, ownerUserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "bulk submit failed"})
		return
	}
	// 202 Accepted + the handle. counts.total is the accepted (normalizable) set; malformed inputs
	// are reported, not silently dropped (§7.6 boundary — the platform decides how to present them).
	//
	// processing_paused is the Rung-4 accept-and-defer disclosure (fix-plan item 4): the work is
	// stored, and the drainer is stopped, so this job will report zero progress until the rung is
	// released. It is ADDITIVE (a new key on a P5 envelope; Door 1 governs the verdict object, which
	// is untouched) and DERIVED from live config on every response — never stored on the job row.
	// Deriving it is the load-bearing choice twice over: a stored flag would still read "paused"
	// after the rung was released (a job would lie about the present), and a hardcoded false would be
	// the same class of inert control this item exists to remove.
	writeJSON(w, http.StatusAccepted, map[string]any{
		"job_id": res.JobID,
		"status": "accepted",
		"counts": map[string]int{
			"total": res.Total, "enqueued": res.Enqueued, "duplicate": res.Duplicate, "malformed": res.Malformed,
		},
		"processing_paused": s.cfg.KillSwitch.VerdictProductionPaused(),
	})
}

// handleJobStatus is GET /v1/jobs/{id} (§7.1): coarse brain-side progress (counts + status),
// NOT a live per-row feed (that's the platform's job, §7.6). Completion is judged AUTHORITATIVELY
// (NOT EXISTS a non-terminal item, GAP-38), never the denormalized counter.
func (s *Server) handleJobStatus(w http.ResponseWriter, r *http.Request) {
	// 0013 / item 1: the authenticated key's platform user is now USED, not discarded. Every read
	// below is scoped to it.
	_, ownerUserID, ok := s.authenticate(r, true)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if s.jobs == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "not ready"})
		return
	}
	jobID := r.PathValue("id")
	job, known, err := s.jobs.GetJob(r.Context(), jobID, ownerUserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "job read failed"})
		return
	}
	if !known {
		// 404 FOR BOTH "no such job" AND "not your job" — the same status, the same body, byte for
		// byte (fix-plan item 1: "Return 404, not 403"). A 403 here would confirm the id exists,
		// turning a job id into something worth guessing at; identical 404s mean a cross-tenant
		// probe learns exactly nothing. This mirrors the senders' no-existence-disclosure posture.
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job not found"})
		return
	}
	complete, total, settled, err := s.jobs.JobCompletion(r.Context(), jobID, ownerUserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "job status failed"})
		return
	}
	status := job.Status
	if complete && status != "complete" && status != "complete_with_unknowns" {
		status = "complete" // authoritative completion overrides a stale stored 'running'
	}
	// Rung 4 promises "a job handle + 'processing paused' job status" (kill-switch-ladder.md). It is
	// reported as an ADDITIVE BOOLEAN, deliberately NOT as a new `status` value: the job-status
	// vocabulary is a closed set with a DB CHECK constraint (0009: submitted|running|complete|
	// complete_with_unknowns), and minting a fifth value would change a persisted vocabulary — a
	// one-way door — to express a transient operational state. A pause is not a state the job is IN;
	// it is a statement about the system the job is waiting on, which is why the same key with the
	// same derivation also appears on the 202 submit envelope.
	writeJSON(w, http.StatusOK, map[string]any{
		"job_id":     jobID,
		"status":     status,
		"complete":   complete,
		"caller_ref": job.CallerRef, // echoed opaque (§7.6)
		"counts":     map[string]int{"total": total, "settled": settled, "pending": total - settled},
		// Same live derivation as the submit envelope: never stored, so releasing the rung
		// immediately stops every job from claiming to be paused.
		"processing_paused": s.cfg.KillSwitch.VerdictProductionPaused(),
	})
}

// handleJobResults is GET /v1/jobs/{id}/results?cursor=…&limit=… (§7.1): the settled verdicts,
// cursor-paginated, each the BYTE-IDENTICAL /v1/verify verdict object (Door 1 — a consumer that
// parses a sync verdict parses a bulk result unchanged, §7.4).
func (s *Server) handleJobResults(w http.ResponseWriter, r *http.Request) {
	// 0013 / item 1: this is the route that returned another tenant's verdicts under D1.
	_, ownerUserID, ok := s.authenticate(r, true)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if s.jobs == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "not ready"})
		return
	}
	jobID := r.PathValue("id")
	if _, known, err := s.jobs.GetJob(r.Context(), jobID, ownerUserID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "job read failed"})
		return
	} else if !known {
		// Same 404 for absent and not-yours (see handleJobStatus). ListJobResults below ALSO carries
		// the owner predicate, so a non-owner is stopped twice: once on the shape of the response,
		// once in the query. Belt and braces, on the one route whose payload is the PII.
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job not found"})
		return
	}
	cursor := r.URL.Query().Get("cursor")
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	rows, next, err := s.jobs.ListJobResults(r.Context(), jobID, ownerUserID, cursor, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "job results failed"})
		return
	}
	// Rebuild each result as the Door-1 verdict object. The stored VerificationRow → verdict.Response
	// mapping is the same shape /v1/verify returns (status/reason/confidence/recommendation/flags/
	// signals). We do NOT expose work-item internals (lease, attempt, plaintext) — §7.4/§7.6.
	results := make([]verdict.Response, 0, len(rows))
	for _, row := range rows {
		results = append(results, verdictFromRow(row.Verdict))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"job_id":      jobID,
		"results":     results,
		"next_cursor": next,
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz reports readiness WITHOUT leaking secret values (presence only) and shows
// the kill-switch rung — so an operator can see the system is at cheap-layers-only.
func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	ready := s.secrets != nil && s.secrets.HasPepper()
	code := http.StatusOK
	if !ready {
		code = http.StatusServiceUnavailable // no pepper => cannot hash => not ready for real work
	}
	writeJSON(w, code, map[string]any{
		"ready":       ready,
		"config":      s.cfg.Summary(),
		"pepper":      boolPresent(s.secrets != nil && s.secrets.HasPepper()),
		"supabaseKey": boolPresent(s.secrets != nil && s.secrets.HasSupabaseKey()),
	})
}

// authenticate is the front-door auth (PN-13). It resolves the presented bearer to a caller in
// this precedence:
//  1. PER-CALLER KEY: if caller-auth is wired, sha256(key) → LookupCaller (constant-time compare +
//     validity window). A hit authenticates ANY endpoint and yields the caller_id.
//  2. LEGACY STATIC TOKEN: accepted ONLY when requirePerCaller is false (the sync /v1/verify during
//     transition, PN-13 backward-compat). The bulk-PII surface (requirePerCaller=true) REJECTS it —
//     that is the F-20 gate: the bulk surface demands a per-caller key from day one.
//  3. DEV OPEN: only when NO auth is configured at all AND env=development (build smoke) — never in
//     production, never when a static token or caller-auth is configured.
//
// It returns (callerID, ownerUserID, ok). callerID is "" for the legacy/dev paths (no per-caller
// principal).
//
// ownerUserID (0013 / fix-plan item 1, D1) is the PLATFORM user the authenticated key acts for —
// the value stamped onto a new job and predicated on by every job read. It comes ONLY from
// api_callers.user_id via LookupCaller, i.e. from the credential. There is deliberately no path by
// which a request body, query string, or header other than the bearer can influence it: that is
// contract §1.4 clause 6's credential-based model, the stronger of the two coherent options, and
// the one M1 must use because M1 authenticates the CALLER (unlike the senders, where the platform
// token authorises the platform and the caller supplies user_id).
//
// It is "" on the legacy-static-token and dev-open paths, which have no platform user. Those
// callers can still submit; their jobs get a NULL owner and are then readable by nobody over the
// API (see GetJob). That is the fail-safe direction — "" must never be read as "match anything".
func (s *Server) authenticate(r *http.Request, requirePerCaller bool) (callerID, ownerUserID string, ok bool) {
	presented := trimBearer(r.Header.Get("Authorization"))

	// (1) Per-caller key (PN-13). A presented bearer that matches an active key wins on any endpoint.
	if s.callerAuth != nil && presented != "" {
		sum := sha256.Sum256([]byte(presented))
		if id, owner, hit, err := s.callerAuth.LookupCaller(r.Context(), sum[:], s.now()); err == nil && hit {
			return id, owner, true
		}
		// A miss falls through: it MIGHT still be the legacy static token on a sync call.
	}

	// (2) Legacy static token — sync endpoints only (never the bulk-PII surface, F-20).
	if !requirePerCaller {
		if want, configured := s.frontDoorToken(); configured {
			// Constant-time compare so a wrong token gives no timing oracle.
			if presented != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1 {
				return "", "", true // legacy caller — no per-caller principal, no platform user
			}
			return "", "", false // a static token IS configured but didn't match → reject
		}
	}

	// (3) No auth configured at all → open only in development (build smoke), never production, and
	// never on the per-caller-required (bulk) surface.
	if !requirePerCaller && s.callerAuth == nil {
		if _, configured := s.frontDoorToken(); !configured {
			return "", "", s.cfg.Env == "development"
		}
	}
	return "", "", false
}

// verdictFromRow rebuilds the Door-1 verdict object from a stored LOG row for a bulk result (§7.4).
// It is the SAME shape /v1/verify returns. TWO deliberate differences, both Door-honoring:
//   - Email is EMPTY: the sync path echoes the RAW input (OQ-4), but a bulk result is served AFTER
//     the plaintext was purged on settle (§5.3/DR-4 — Door 3). We cannot (and must not) echo it.
//     The caller correlates via domain + the envelope's email_hash (a hash, not PII), or its own
//     submission bookkeeping — the full input↔result correlation is a platform seam (§7.6).
//   - Meta is minimal (policy/version + from_cache): the per-request provenance (request_id,
//     latency, node) belongs to each attempt, not the job-level result envelope.
// Flags/Signals/Status/Reason/Confidence/Recommendation are the settled verdict verbatim.
func verdictFromRow(row store.VerificationRow) verdict.Response {
	flags := row.Flags
	if flags == nil {
		flags = map[string]bool{}
	}
	signals := row.Signals
	if signals == nil {
		signals = map[string]any{}
	}
	policy := row.PolicyKey
	return verdict.Response{
		APIVersion:     firstNonEmpty(row.APIVersion, "v1"),
		Status:         vocab.Status(row.Status),
		Reason:         vocab.Reason(row.Reason),
		Confidence:     row.Confidence,
		Recommendation: vocab.Recommendation(row.Recommendation),
		Email:          "", // purged on settle (Door 3) — a bulk result never echoes the raw address
		Domain:         row.Domain,
		Flags:          flags,
		Signals:        signals,
		Meta:           verdict.Meta{Policy: policy, PolicyVersion: row.PolicyVersion, FromCache: row.FromCache},
		Raw:            nil,
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func boolPresent(b bool) string {
	if b {
		return "set"
	}
	return "unset"
}
