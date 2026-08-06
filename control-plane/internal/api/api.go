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
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
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

// Pinger is the narrow readiness probe the front door needs: can the database be reached RIGHT NOW
// (fix-plan item 10 / contract §1.9.1, satisfied by *store.PG). One method, so readiness cannot
// accidentally acquire the ability to read or write anything.
//
// WHY IT EXISTS AT ALL. Before item 10, /readyz checked only that the pepper secret had loaded, so
// the control-plane reported `ready` while every POST /v1/verify answered 503 for a missing store —
// the contract calls that "worse than no readiness endpoint, because orchestrators act on it". The
// serving predicate and the readiness predicate had drifted apart; this closes the gap by making
// readiness ping the same dependency the serving path needs.
//
// nil => no DB configured or the open failed (main.go's buildAPI returns early on both), which is
// itself the honest NOT-ready answer. A nil Pinger reports the `db` check false rather than
// omitting it: an absent check would let a monitor read "no news is good news" off a body that is
// actually describing an unserviceable process.
type Pinger interface {
	Ping(ctx context.Context) error
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
	cfg          config.Config
	secrets      *secrets.Secrets
	hasher       *hash.Hasher  // may be nil if no pepper loaded (readiness reports it)
	verifier     Verifier      // nil until the store+orchestrator are wired (readiness reports it)
	callerAuth   CallerAuth    // nil => per-caller auth not wired (legacy static token governs)
	bulk         BulkSubmitter // nil => bulk endpoint unavailable (returns 503)
	jobs         JobReader     // nil => job endpoints unavailable
	pinger       Pinger        // nil => no DB reachable; /readyz reports db:false (item 10)
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

// WithPinger attaches the readiness DB probe (fix-plan item 10). Separate from WithVerifier even
// though main.go calls both with the same *store.PG, because the two answer different questions:
// the verifier is what /v1/verify DELEGATES to, the pinger is what /readyz MEASURES. Wiring them
// through one setter would make it impossible to test a server that can serve but whose DB has
// since gone away — which is exactly the state readiness exists to report.
func (s *Server) WithPinger(p Pinger) *Server {
	s.pinger = p
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

// Route is one served endpoint: the ServeMux pattern and the handler behind it.
//
// WHY A SLICE AND NOT SIX mux.HandleFunc CALLS (fix-plan item 14). The senders gate served-set ==
// documented-set in CI; M1 had no such gate, so a route could be added, removed, or renamed with
// nothing but a human noticing that Docs no longer matched. Go's ServeMux cannot be asked which
// patterns it holds, so the alternative was a check that GREPS this file for `mux.HandleFunc(` —
// and a source-scanning check narrower than the property it guards is a defect shape this project
// has already hit (a config-consumption gate satisfied by a TODO naming the key). Declaring the
// table makes the served set the COMPILER'S view: Routes() is what Handler() serves, by
// construction, because Handler() is a loop over it. The gate reads the same slice the server runs,
// which is the same reason the test-execution gate reads `go test -list` instead of a literal.
//
// WHAT ENFORCES THE ABOVE (routes_contract_test.go, all four run on every `go test`):
//
//   - TestRoutes_ServedSetMatchesTheDocumentedSet — Routes() vs the Docs/api-contract.md table, in
//     BOTH directions. Adding a route here without documenting it fails; documenting one that is not
//     served fails too.
//   - TestRoutes_HandlerServesExactlyTheRouteTable — every pattern dispatches, and unclaimed paths
//     get the envelope's 404.
//   - TestRoutes_WrongVerbIsTheEnvelopeNotServeMuxsPlainText — 30 wrong-verb combinations, derived
//     from this slice, all in the envelope with a correct Allow.
//   - TestRoutes_HandlerIsALoopOverRoutes — the structural half: exactly three mux.HandleFunc
//     registrations in this package. A fourth means a route bypassed this slice, which the three
//     tests above CANNOT see (verified: B83 left them green while this one went red).
type Route struct {
	// Pattern is the Go 1.22+ ServeMux pattern, METHOD first: "GET /v1/jobs/{id}".
	Pattern string
	handler http.HandlerFunc
}

// Method returns the verb half of the pattern ("GET"), Path the rest ("/v1/jobs/{id}").
func (r Route) Method() string { m, _, _ := strings.Cut(r.Pattern, " "); return m }

// Path returns the path half of the pattern, wildcards included.
func (r Route) Path() string { _, p, _ := strings.Cut(r.Pattern, " "); return p }

// Routes is the SERVED SURFACE — the single source of truth for what this module answers.
// Handler() serves exactly these and nothing else.
func (s *Server) Routes() []Route {
	return []Route{
		{"POST /v1/verify", s.handleVerify},
		// P5 bulk (§7): the async submission → handle → progress → results lifecycle. These are the
		// bulk-PII surface — auth REQUIRES a per-caller key (PN-13); the legacy static token is rejected.
		{"POST /v1/verify/bulk", s.handleBulkSubmit},
		{"GET /v1/jobs/{id}", s.handleJobStatus},
		{"GET /v1/jobs/{id}/results", s.handleJobResults},
		{"GET /healthz", s.handleHealthz},
		{"GET /readyz", s.handleReadyz},
	}
}

// Handler builds the mux. Kept explicit (stdlib) — no framework dependency.
//
// THE 405/404 FALLBACKS ARE THIS MODULE'S, NOT ServeMux's (fix-plan item 14 / module 3's audit D17,
// found here while adopting the senders' route gate). Measured on this tree before the fix:
//
//	GET    /v1/verify -> 405 text/plain "Method Not Allowed\n"   (no code, no request_id)
//	GET    /v1/nope   -> 404 text/plain "404 page not found\n"
//
// Item 18 unified 26 hand-rolled error sites into one coded envelope with a correlatable
// request_id — and could not see these two, because they never reach a handler. So a client doing
// body.error.code on any non-2xx nil-deref'd on every wrong verb and every typo'd path, and there
// was no id to grep. Registering "/" and per-path method fallbacks routes both through the same
// writeErr as everything else.
//
// The fallbacks are derived from Routes(), never hand-listed: the allowed-methods set per path comes
// from the table, so a new route gets a correct Allow header with no second list to keep in step.
// The pattern "/" is ServeMux's least-specific match, so it catches only what no real route claimed.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Which verbs each path serves, from the route table itself.
	byPath := map[string][]string{}
	for _, rt := range s.Routes() {
		mux.HandleFunc(rt.Pattern, rt.handler)
		byPath[rt.Path()] = append(byPath[rt.Path()], rt.Method())
	}

	// A real path with a verb it does not serve: 405 + Allow, in the envelope.
	//
	// Registered as a METHOD-LESS pattern per path, which ServeMux treats as less specific than any
	// method-qualified one, so this is reached only for the verbs the table does not claim. GET also
	// implies HEAD in ServeMux, and Allow reports that, matching what the mux will actually answer.
	for path, methods := range byPath {
		allow := append([]string(nil), methods...)
		for _, m := range methods {
			if m == http.MethodGet {
				allow = append(allow, http.MethodHead)
			}
		}
		sort.Strings(allow)
		allowHdr := strings.Join(allow, ", ")
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			// Allow is set BEFORE writeErr, since writeErr writes the status line.
			w.Header().Set("Allow", allowHdr)
			writeErr(w, codeMethodNotAllowed,
				"wrong verb "+r.Method+" on "+r.URL.Path+" (served: "+allowHdr+")")
		})
	}

	// Anything else: 404 in the envelope, with the SAME code and message a real-but-absent resource
	// gets. An unknown path and a job owned by someone else must be indistinguishable (D1 tenancy),
	// and that only holds if this fallback does not announce itself as a routing miss.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, codeNotFound, "no route for "+r.Method+" "+r.URL.Path)
	})
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
		writeErr(w, codeUnauthenticated, "auth rejected: no valid per-caller key or legacy token")
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
		writeErr(w, codePaused, "kill-switch Rung 4 pulled (TM_FRONTDOOR_MODE)")
		return
	}
	var req verifyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.Email == "" {
		writeErr(w, codeValidationFailed, "verify: email missing or body undecodable", fieldDetail{Field: "email", Issue: issueRequired})
		return
	}

	// The real request lifecycle runs in the orchestrator (cache → policy → cheap layers →
	// score → LOG → cache → respond). If it isn't wired (no DB/pepper), we are NOT ready —
	// return 503 rather than a fabricated verdict (Door 2: never a verdict we can't log).
	if s.verifier == nil || s.defPolicy == nil {
		writeErr(w, codeUnavailable, "orchestrator or policy not wired (no DB/pepper)")
		return
	}
	// P1 ships exactly one policy (the default). A request naming a different policy is
	// not-found for now (the seam for multiple policies exists in the store/§4.6).
	if req.Policy != "" && req.Policy != s.defPolicy.Key {
		writeErr(w, codeValidationFailed, "request named a policy this build does not serve", fieldDetail{Field: "policy", Issue: issueNotFound})
		return
	}

	resp, err := s.verifier.Verify(r.Context(), req.Email, s.defPolicy)
	if err != nil {
		if errors.Is(err, orchestrator.ErrNotReady) {
			writeErr(w, codeUnavailable, "orchestrator or policy not wired (no DB/pepper)")
			return
		}
		// A processing failure must NOT become a fabricated verdict — surface 500 honestly.
		writeErr(w, codeInternalError, "orchestrator Verify failed")
		return
	}
	if !resp.Valid() {
		// Defensive: never emit an out-of-contract response (Door 1/4). Should be impossible.
		//
		// Its own code, not internal_error (item 18): this is the one 500 that means "a Door-1 shape
		// check failed," and it must never be lost in generic internal noise. It also REUSES the
		// verdict's own request_id rather than minting a fresh one, so the error body, the LOG row
		// the orchestrator already wrote, and this log line all name the same id — the correlation
		// item 18 exists to provide. Meta.RequestID is empty only if the verdict lacked one, and
		// writeErrWithID mints a fallback in that case.
		writeErrWithID(w, resp.Meta.RequestID, codeContractViolation,
			"orchestrator returned a verdict that failed Valid() — Door-1 shape violation")
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
		writeErr(w, codeUnauthenticated, "auth rejected: no valid per-caller key or legacy token")
		return
	}
	// Rung 4 REJECT stops here; Rung 4 ACCEPT-AND-DEFER deliberately does NOT (that is the whole
	// distinction between the two sub-modes — see FrontDoorDeferring). A deferred submission is
	// accepted, sealed, and stored durably in work_items exactly as normal; the only difference is
	// that the drainer will not pick it up until the rung is released. The 202 below carries
	// "processing_paused": true so the caller is TOLD, rather than watching a job sit at zero settled
	// with no explanation.
	if s.cfg.KillSwitch.FrontDoorRejecting() {
		writeErr(w, codePaused, "kill-switch Rung 4 pulled (TM_FRONTDOOR_MODE)")
		return
	}
	if s.bulk == nil || s.defPolicy == nil {
		writeErr(w, codeUnavailable, "orchestrator or policy not wired (no DB/pepper)")
		return
	}
	// Bound the body (a 100k-address list of ~small strings; generous cap, still bounded).
	var req bulkRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&req); err != nil {
		writeErr(w, codeValidationFailed, "bulk submit: body did not decode", fieldDetail{Field: "body", Issue: issueMalformed})
		return
	}
	if len(req.Addresses) == 0 {
		writeErr(w, codeValidationFailed, "bulk submit: addresses absent or empty", fieldDetail{Field: "addresses", Issue: issueRequired})
		return
	}
	if cap := s.bulkAddressCap(); len(req.Addresses) > cap {
		// THE SECOND SHAPE (audit classification 2.7). This was the one route answering in a
		// different envelope entirely — `{error, max, got}` — so a client had to special-case it.
		// The cap and the submitted count are genuinely useful to a caller, so they are not
		// dropped: they move into the internalReason (for the operator's log line) and the caller
		// learns WHICH field was out of range through details[], which is the envelope's own
		// mechanism for exactly this. No free-text channel, no bespoke shape.
		writeErr(w, codePayloadTooLarge,
			fmt.Sprintf("bulk submit: %d addresses exceeds the cap of %d", len(req.Addresses), cap),
			fieldDetail{Field: "addresses", Issue: issueOutOfRange})
		return
	}
	if req.Policy != "" && req.Policy != s.defPolicy.Key {
		writeErr(w, codeValidationFailed, "request named a policy this build does not serve", fieldDetail{Field: "policy", Issue: issueNotFound})
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
		writeErr(w, codeInternalError, "bulk submit: store write failed")
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
		writeErr(w, codeUnauthenticated, "auth rejected: no valid per-caller key or legacy token")
		return
	}
	if s.jobs == nil {
		writeErr(w, codeUnavailable, "orchestrator or policy not wired (no DB/pepper)")
		return
	}
	jobID := r.PathValue("id")
	job, known, err := s.jobs.GetJob(r.Context(), jobID, ownerUserID)
	if err != nil {
		writeErr(w, codeInternalError, "job read: store query failed")
		return
	}
	if !known {
		// 404 FOR BOTH "no such job" AND "not your job" — the same status, the same body, byte for
		// byte (fix-plan item 1: "Return 404, not 403"). A 403 here would confirm the id exists,
		// turning a job id into something worth guessing at; identical 404s mean a cross-tenant
		// probe learns exactly nothing. This mirrors the senders' no-existence-disclosure posture.
		writeErr(w, codeNotFound, "job absent, or owned by a different caller (tenancy scoping, D1)")
		return
	}
	complete, total, settled, err := s.jobs.JobCompletion(r.Context(), jobID, ownerUserID)
	if err != nil {
		writeErr(w, codeInternalError, "job status: progress aggregation failed")
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
		writeErr(w, codeUnauthenticated, "auth rejected: no valid per-caller key or legacy token")
		return
	}
	if s.jobs == nil {
		writeErr(w, codeUnavailable, "orchestrator or policy not wired (no DB/pepper)")
		return
	}
	jobID := r.PathValue("id")
	if _, known, err := s.jobs.GetJob(r.Context(), jobID, ownerUserID); err != nil {
		writeErr(w, codeInternalError, "job read: store query failed")
		return
	} else if !known {
		// Same 404 for absent and not-yours (see handleJobStatus). ListJobResults below ALSO carries
		// the owner predicate, so a non-owner is stopped twice: once on the shape of the response,
		// once in the query. Belt and braces, on the one route whose payload is the PII.
		writeErr(w, codeNotFound, "job absent, or owned by a different caller (tenancy scoping, D1)")
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
		writeErr(w, codeInternalError, "job results: page query failed")
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

// handleHealthz is liveness: the process is up. `at` is additive (fix-plan item 10 / contract
// §1.9) and is what lets a dashboard tell "this module answered just now" from "I am looking at a
// cached or proxied reply" — the senders already carried it, this surface did not.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"at":     s.now().UTC().Format(time.RFC3339),
	})
}

// handleReadyz reports readiness WITHOUT leaking secret values (presence only) and shows
// the kill-switch rung — so an operator can see the system is at cheap-layers-only.
//
// FIX-PLAN ITEM 10 (contract §1.9.1-2), two changes, both required together:
//
//  1. IT NOW PINGS THE DATABASE. Before this, `ready` was `secrets.HasPepper()` alone, so a
//     control-plane with a pepper and no reachable store answered `200 ready:true` while every
//     POST /v1/verify answered 503. The readiness predicate and the serving predicate disagreed,
//     and readiness is the one an orchestrator acts on. Both dependencies are now required, because
//     either one missing means the same thing to a caller: no verdict can be produced. The ping is
//     LIVE per request (not a cached boot-time result) — a DB that has gone away since startup is
//     the case a cached answer would miss, and it is the common one.
//
//  2. IT CARRIES THE CONTRACT'S `status`/`checks{}`/`at` ENVELOPE. The three modules previously
//     shared not one readiness field name (this one returned {ready, config, pepper, supabaseKey},
//     the senders {status, db, singleton_lock, at}), so one dashboard needed three parsers. The
//     uniform part is the envelope; the per-module dependency NAMES live inside checks{}, which is
//     what makes one shape possible without pretending the modules have the same dependencies.
//
// EVERY PRE-EXISTING FIELD IS STILL HERE, unchanged, at the top level. That is PROJECT_RULES'
// additive-only discipline applied to an operator surface: `config` in particular is grepped by a
// documented runbook step (OPERATIONS.md's `grep -o 'frontdoor=[^ "]*'`) and by
// TestFrontDoorDefer_SummaryNamesTheRung, so removing or nesting it would break a procedure an
// operator runs during an incident. `ready` stays as the boolean it always was, and `status` is its
// word form — a monitor reading either gets the same answer, which is the property the test pins.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	pepperOK := s.secrets != nil && s.secrets.HasPepper()

	// A bounded ping: readiness must answer even when the DB is wedged rather than merely down.
	// Without the timeout an unresponsive-but-connected Postgres would hang /readyz itself, and a
	// readiness endpoint that hangs reads as "unknown" to some orchestrators and "healthy" to
	// others. 3s matches the senders' handleReadyz so the three modules degrade alike.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	dbOK := s.pinger != nil && s.pinger.Ping(ctx) == nil

	ready := pepperOK && dbOK
	code := http.StatusOK
	if !ready {
		// No pepper => cannot hash; no DB => cannot log a verdict, and Door 2 forbids a verdict we
		// cannot log. Either way this process cannot do its job, so it must not be sent traffic.
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{
		// The contract envelope (item 10).
		"status": readyWord(ready),
		"checks": map[string]bool{
			"pepper": pepperOK,
			"db":     dbOK,
		},
		"at": s.now().UTC().Format(time.RFC3339),
		// Pre-existing fields, retained verbatim — see the additive-only note above.
		"ready":       ready,
		"config":      s.cfg.Summary(),
		"pepper":      boolPresent(pepperOK),
		"supabaseKey": boolPresent(s.secrets != nil && s.secrets.HasSupabaseKey()),
	})
}

// readyWord is the contract's §1.9 status word. Deliberately not named statusWord (the senders'
// name) — this file already has boolPresent returning "set"/"unset" for the same kind of job, and
// two similarly-named string mappers in one package invite the wrong one being called.
func readyWord(ok bool) string {
	if ok {
		return "ready"
	}
	return "not_ready"
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
//
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
