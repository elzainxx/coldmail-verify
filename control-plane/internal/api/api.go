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

// Server holds the wired dependencies. Deps are interfaces/values, so tests inject fakes.
type Server struct {
	cfg       config.Config
	secrets   *secrets.Secrets
	hasher    *hash.Hasher // may be nil if no pepper loaded (readiness reports it)
	verifier  Verifier     // nil until the store+orchestrator are wired (readiness reports it)
	defPolicy *policy.Definition
	now       func() time.Time
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
	if !s.authOK(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
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

// authOK checks the front-door bearer. Phase 0 accepts a single configured token; per-caller
// keys + rotation are a P5 gate (PN-13). If no token is configured (build smoke), auth is open
// ONLY in development env — never in production.
func (s *Server) authOK(r *http.Request) bool {
	want, ok := s.frontDoorToken()
	if !ok {
		return s.cfg.Env == "development"
	}
	got := r.Header.Get("Authorization")
	got = trimBearer(got)
	return got != "" && got == want
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
