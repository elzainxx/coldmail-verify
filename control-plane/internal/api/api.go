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
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/config"
	"github.com/mnfi/codmailling/control-plane/internal/hash"
	"github.com/mnfi/codmailling/control-plane/internal/orchestrator"
	"github.com/mnfi/codmailling/control-plane/internal/policy"
	"github.com/mnfi/codmailling/control-plane/internal/secrets"
	"github.com/mnfi/codmailling/control-plane/internal/verdict"
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
	LookupCaller(ctx context.Context, keySHA256 []byte, now time.Time) (callerID string, ok bool, err error)
}

// callerIDKey is the context key under which an authenticated caller_id is stashed (never PII —
// GAP-36; the platform holds the user↔caller_id map). Distinct type so it can't collide.
type callerIDKey struct{}

// Server holds the wired dependencies. Deps are interfaces/values, so tests inject fakes.
type Server struct {
	cfg        config.Config
	secrets    *secrets.Secrets
	hasher     *hash.Hasher // may be nil if no pepper loaded (readiness reports it)
	verifier   Verifier     // nil until the store+orchestrator are wired (readiness reports it)
	callerAuth CallerAuth   // nil => per-caller auth not wired (legacy static token governs)
	defPolicy  *policy.Definition
	now        func() time.Time
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

// Handler builds the mux. Kept explicit (stdlib) — no framework dependency at Phase 0.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/verify", s.handleVerify)
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
	callerID, ok := s.authenticate(r, false)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if callerID != "" {
		r = r.WithContext(context.WithValue(r.Context(), callerIDKey{}, callerID))
	}
	// Front-door mode (kill-switch Rung 4): reject if paused-reject.
	if s.cfg.KillSwitch.FrontDoor == config.FrontDoorReject {
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
// It returns (callerID, ok). callerID is "" for the legacy/dev paths (no per-caller principal).
func (s *Server) authenticate(r *http.Request, requirePerCaller bool) (callerID string, ok bool) {
	presented := trimBearer(r.Header.Get("Authorization"))

	// (1) Per-caller key (PN-13). A presented bearer that matches an active key wins on any endpoint.
	if s.callerAuth != nil && presented != "" {
		sum := sha256.Sum256([]byte(presented))
		if id, hit, err := s.callerAuth.LookupCaller(r.Context(), sum[:], s.now()); err == nil && hit {
			return id, true
		}
		// A miss falls through: it MIGHT still be the legacy static token on a sync call.
	}

	// (2) Legacy static token — sync endpoints only (never the bulk-PII surface, F-20).
	if !requirePerCaller {
		if want, configured := s.frontDoorToken(); configured {
			// Constant-time compare so a wrong token gives no timing oracle.
			if presented != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1 {
				return "", true // legacy caller — no per-caller principal
			}
			return "", false // a static token IS configured but didn't match → reject
		}
	}

	// (3) No auth configured at all → open only in development (build smoke), never production, and
	// never on the per-caller-required (bulk) surface.
	if !requirePerCaller && s.callerAuth == nil {
		if _, configured := s.frontDoorToken(); !configured {
			return "", s.cfg.Env == "development"
		}
	}
	return "", false
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
