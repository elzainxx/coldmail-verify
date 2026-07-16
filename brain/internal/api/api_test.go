package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mnfi/codmailling/brain/internal/config"
	"github.com/mnfi/codmailling/brain/internal/secrets"
	"github.com/mnfi/codmailling/brain/internal/verdict"
)

// The Phase-0 front door must (a) emit a VALID Door-1 contract response, (b) never a
// guessed valid/invalid (harden-toward-unknown), (c) enforce the auth seam.

func devServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Defaults() // env=development => open auth when no token configured
	sec, err := secrets.Load(fakeSource{}, false)
	if err != nil {
		t.Fatalf("secrets: %v", err)
	}
	return New(cfg, sec, nil)
}

type fakeSource map[string]string

func (f fakeSource) Get(k string) (string, bool) { v, ok := f[k]; return v, ok }

func TestVerifyReturnsValidContract(t *testing.T) {
	srv := devServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/verify", strings.NewReader(`{"email":"a@b.co"}`))
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
	// Phase-0 honesty: no engine consulted => unknown, never a guessed valid/invalid.
	if resp.Status != "unknown" {
		t.Errorf("P0 must return unknown (no engine consulted), got %q", resp.Status)
	}
	if resp.Raw != nil {
		t.Errorf("raw must be null when no engine consulted, got %v", resp.Raw)
	}
}

func TestVerifyRequiresEmail(t *testing.T) {
	srv := devServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/verify", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing email must be 400, got %d", rec.Code)
	}
}

func TestAuthEnforcedInProduction(t *testing.T) {
	cfg := config.Defaults()
	cfg.Env = "production"
	sec, _ := secrets.Load(fakeSource{secrets.EnvFrontDoorKey: "sekret"}, false)
	srv := New(cfg, sec, nil)

	// No token -> 401.
	req := httptest.NewRequest(http.MethodPost, "/v1/verify", strings.NewReader(`{"email":"a@b.co"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("prod without token must be 401, got %d", rec.Code)
	}

	// Correct token -> 200.
	req = httptest.NewRequest(http.MethodPost, "/v1/verify", strings.NewReader(`{"email":"a@b.co"}`))
	req.Header.Set("Authorization", "Bearer sekret")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("prod with correct token must be 200, got %d", rec.Code)
	}
}

func TestReadyzReflectsPepperPresence(t *testing.T) {
	srv := devServer(t) // no pepper loaded
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no pepper => not ready (503), got %d", rec.Code)
	}
}
