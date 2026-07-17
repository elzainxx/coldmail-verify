package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/config"
	"github.com/mnfi/codmailling/control-plane/internal/hash"
	"github.com/mnfi/codmailling/control-plane/internal/layers"
	"github.com/mnfi/codmailling/control-plane/internal/orchestrator"
	"github.com/mnfi/codmailling/control-plane/internal/policy"
	"github.com/mnfi/codmailling/control-plane/internal/secrets"
	"github.com/mnfi/codmailling/control-plane/internal/store"
	"github.com/mnfi/codmailling/control-plane/internal/verdict"
)

// The front door must (a) emit a VALID Door-1 contract response, (b) never a guessed
// valid/invalid (harden-toward-unknown), (c) enforce the auth seam, (d) be 503 when not wired.

type fakeSource map[string]string

func (f fakeSource) Get(k string) (string, bool) { v, ok := f[k]; return v, ok }

// fixtureResolver: a corp domain with real MX (so the cheap path yields risky/plausible),
// no network. mustHasher builds a real Door-3 hasher.
type fixtureResolver struct{}

func (fixtureResolver) LookupMX(_ context.Context, d string) ([]layers.MXHost, error) {
	if d == "acmecorp.com" {
		return []layers.MXHost{{Host: "mx.acmecorp.com", Pref: 10}}, nil
	}
	return nil, nil // other domains: no MX, no A → no_mx (invalid)
}
func (fixtureResolver) LookupIP(context.Context, string) ([]net.IP, error) { return nil, nil }

func mustHasher(t *testing.T) *hash.Hasher {
	t.Helper()
	h, err := hash.NewHasher([]byte("api-test-pepper"))
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	return h
}

// wiredServer builds a Server with a real MemStore-backed orchestrator (full lifecycle, no DB).
func wiredServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Defaults() // env=development => open auth when no token configured
	sec, err := secrets.Load(fakeSource{}, false)
	if err != nil {
		t.Fatalf("secrets: %v", err)
	}
	h := mustHasher(t)
	mem := store.NewMem(nil)
	def, err := policy.ColdEmailDefinition()
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	orch := orchestrator.New(orchestrator.Deps{
		Hasher: h, Store: mem, Resolver: fixtureResolver{},
		CacheTTL: time.Hour, ConfigFingerprint: "cfg_sha256:test",
	})
	return New(cfg, sec, h).WithVerifier(orch, def)
}

func TestVerifyReturnsValidContract(t *testing.T) {
	srv := wiredServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/verify", strings.NewReader(`{"email":"jane.doe@acmecorp.com"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp verdict.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Valid() {
		t.Errorf("response violates Door-1/4 contract: %+v", resp)
	}
	// Cheap-layers-only honesty: a clean corp domain is risky/plausible, NEVER valid (no SMTP).
	if resp.Status == "valid" {
		t.Errorf("P1 must not claim valid without SMTP, got %s", resp.Status)
	}
	if resp.Raw != nil {
		t.Errorf("raw must be null when no engine consulted, got %v", resp.Raw)
	}
	if resp.Email != "jane.doe@acmecorp.com" {
		t.Errorf("email must echo raw input (OQ-4), got %q", resp.Email)
	}
}

// A proven-invalid address (no MX) must return invalid and NEVER recommend send (Door 1).
func TestVerifyInvalidNeverSaysSend(t *testing.T) {
	srv := wiredServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/verify", strings.NewReader(`{"email":"a@nomx.example"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var resp verdict.Response
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Status != "invalid" || resp.Reason != "no_mx" {
		t.Fatalf("no-MX domain must be invalid/no_mx, got %s/%s", resp.Status, resp.Reason)
	}
	if resp.Recommendation == "send" {
		t.Fatal("FORBIDDEN: invalid recommended send (Door 1)")
	}
}

// Every result is logged before responding (Door 2) — including the verdict just served.
func TestVerifyLogsEveryResult(t *testing.T) {
	cfg := config.Defaults()
	sec, _ := secrets.Load(fakeSource{}, false)
	h := mustHasher(t)
	mem := store.NewMem(nil)
	def, _ := policy.ColdEmailDefinition()
	orch := orchestrator.New(orchestrator.Deps{Hasher: h, Store: mem, Resolver: fixtureResolver{}, CacheTTL: time.Hour, ConfigFingerprint: "cfg"})
	srv := New(cfg, sec, h).WithVerifier(orch, def)

	for _, email := range []string{"jane@acmecorp.com", "a@nomx.example", "not-an-email"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/verify", strings.NewReader(`{"email":"`+email+`"}`))
		srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}
	if len(mem.Log) != 3 {
		t.Errorf("every result must be logged (incl. invalid_syntax) — got %d rows, want 3", len(mem.Log))
	}
	// The un-addressable reject must be logged with a NULL hash (§3.2.7).
	var foundReject bool
	for _, row := range mem.Log {
		if row.Reason == "invalid_syntax" {
			foundReject = true
			if row.EmailHash != nil {
				t.Error("invalid_syntax row must carry a NULL email_hash (§3.2.7)")
			}
		}
	}
	if !foundReject {
		t.Error("the un-parseable input must be logged as invalid_syntax")
	}
}

// Not-wired server (no verifier) must be 503, never a fabricated verdict.
func TestVerifyNotReadyIs503(t *testing.T) {
	cfg := config.Defaults()
	sec, _ := secrets.Load(fakeSource{}, false)
	srv := New(cfg, sec, nil) // no verifier attached
	req := httptest.NewRequest(http.MethodPost, "/v1/verify", strings.NewReader(`{"email":"a@b.co"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("not-wired verify must be 503, got %d", rec.Code)
	}
}

func TestVerifyRequiresEmail(t *testing.T) {
	srv := wiredServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/verify", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing email must be 400, got %d", rec.Code)
	}
}

func TestUnknownPolicyRejected(t *testing.T) {
	srv := wiredServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/verify", strings.NewReader(`{"email":"a@acmecorp.com","policy":"nonexistent"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown policy must be 400, got %d", rec.Code)
	}
}

func TestAuthEnforcedInProduction(t *testing.T) {
	cfg := config.Defaults()
	cfg.Env = "production"
	sec, _ := secrets.Load(fakeSource{secrets.EnvFrontDoorKey: "sekret"}, false)
	h := mustHasher(t)
	mem := store.NewMem(nil)
	def, _ := policy.ColdEmailDefinition()
	orch := orchestrator.New(orchestrator.Deps{Hasher: h, Store: mem, Resolver: fixtureResolver{}, CacheTTL: time.Hour, ConfigFingerprint: "cfg"})
	srv := New(cfg, sec, h).WithVerifier(orch, def)

	// No token -> 401.
	req := httptest.NewRequest(http.MethodPost, "/v1/verify", strings.NewReader(`{"email":"a@acmecorp.com"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("prod without token must be 401, got %d", rec.Code)
	}

	// Correct token -> 200.
	req = httptest.NewRequest(http.MethodPost, "/v1/verify", strings.NewReader(`{"email":"a@acmecorp.com"}`))
	req.Header.Set("Authorization", "Bearer sekret")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("prod with correct token must be 200, got %d (body %s)", rec.Code, rec.Body.String())
	}
}

func TestReadyzReflectsPepperPresence(t *testing.T) {
	cfg := config.Defaults()
	sec, _ := secrets.Load(fakeSource{}, false)
	srv := New(cfg, sec, nil) // no pepper loaded
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no pepper => not ready (503), got %d", rec.Code)
	}
}
