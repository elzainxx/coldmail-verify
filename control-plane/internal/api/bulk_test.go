package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/config"
	"github.com/coldmail-verify/control-plane/internal/orchestrator"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/queue"
	"github.com/coldmail-verify/control-plane/internal/secrets"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/verdict"
)

// bulkServer wires a Server with a real MemStore-backed Enqueuer + JobReader + per-caller auth,
// at a fixed clock. It uses the real queue.Enqueuer + queue.AddrCipher (so DR-4 sealing runs) and
// the real store methods (so the CAS/completion logic is exercised end-to-end through HTTP).
func bulkServer(t *testing.T, now time.Time) (*Server, *store.MemStore, *queue.AddrCipher) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Env = "production"
	sec, _ := secrets.Load(fakeSource{}, false)
	h := mustHasher(t)
	mem := store.NewMem(func() time.Time { return now })
	def, _ := policy.ColdEmailDefinition()

	cipher, err := queue.NewAddrCipher(key32(1))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	enq := &queue.Enqueuer{Store: mem, Hasher: h, Cipher: cipher, ConfigFingerprint: "cfg", Now: func() time.Time { return now }}

	srv := New(cfg, sec, h).WithVerifier(stubVerifier{}, def).WithCallerAuth(mem).WithBulk(enq, mem, 0)
	srv.now = func() time.Time { return now }
	// Seed a per-caller key (the bulk surface requires one, PN-13).
	mem.SeedCaller(sha("bulk-key"), "caller_bulk", now.Add(-time.Hour), time.Time{})
	return srv, mem, cipher
}

func key32(seed byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = seed + byte(i)
	}
	return k
}

// stubVerifier satisfies the sync Verifier (unused by the bulk tests, but WithVerifier sets defPolicy).
type stubVerifier struct{}

func (stubVerifier) Verify(context.Context, string, *policy.Definition) (verdict.Response, error) {
	return verdict.Response{}, nil
}

// TestBulkSubmit_RequiresPerCallerKey proves PN-13's F-20 gate on the bulk surface: no key → 401,
// legacy static token → 401 (rejected on bulk), valid per-caller key → 202 + a job handle.
func TestBulkSubmit_RequiresPerCallerKey(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	srv, _, _ := bulkServer(t, now)
	body := `{"addresses":["a@acmecorp.com","b@acmecorp.com"],"caller_ref":"platform-job-42"}`

	// No auth → 401.
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/verify/bulk", strings.NewReader(body)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bulk with no key must be 401, got %d", rec.Code)
	}

	// Valid per-caller key → 202 + handle.
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/bulk", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer bulk-key")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("bulk with a per-caller key must be 202, got %d (body %s)", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["job_id"] == nil || resp["job_id"] == "" {
		t.Fatal("202 response must carry a job_id handle")
	}
	if resp["status"] != "accepted" {
		t.Fatalf("status must be accepted, got %v", resp["status"])
	}
}

// TestBulkSubmit_StaticTokenRejectedOnBulk proves the legacy static token (accepted on sync) is
// REJECTED on the bulk-PII surface (F-20), even though a static token is configured.
func TestBulkSubmit_StaticTokenRejectedOnBulk(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	cfg := config.Defaults()
	cfg.Env = "production"
	sec, _ := secrets.Load(fakeSource{secrets.EnvFrontDoorKey: "legacy-static"}, false)
	h := mustHasher(t)
	mem := store.NewMem(func() time.Time { return now })
	def, _ := policy.ColdEmailDefinition()
	cipher, _ := queue.NewAddrCipher(key32(2))
	enq := &queue.Enqueuer{Store: mem, Hasher: h, Cipher: cipher, ConfigFingerprint: "cfg", Now: func() time.Time { return now }}
	srv := New(cfg, sec, h).WithVerifier(stubVerifier{}, def).WithCallerAuth(mem).WithBulk(enq, mem, 0)
	srv.now = func() time.Time { return now }

	req := httptest.NewRequest(http.MethodPost, "/v1/verify/bulk", strings.NewReader(`{"addresses":["a@acmecorp.com"]}`))
	req.Header.Set("Authorization", "Bearer legacy-static")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("F-20: legacy static token must be REJECTED on the bulk surface, got %d", rec.Code)
	}
}

// TestBulk_EndToEnd_HandleThenDrainThenResults exercises the full §7 lifecycle through HTTP: submit
// → 202 handle → drain (a real drainer settles the items) → GET status shows complete → GET results
// returns byte-identical /v1/verify verdict objects that carry NO plaintext email (Door 3 — purged
// on settle) but DO carry the opaque caller_ref echo on status.
func TestBulk_EndToEnd_HandleThenDrainThenResults(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	srv, mem, cipher := bulkServer(t, now)

	// Submit.
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/bulk",
		strings.NewReader(`{"addresses":["a@acmecorp.com","b@acmecorp.com"],"caller_ref":"platform-job-42"}`))
	req.Header.Set("Authorization", "Bearer bulk-key")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}
	var submit map[string]any
	json.Unmarshal(rec.Body.Bytes(), &submit)
	jobID := submit["job_id"].(string)

	// Drain: a settle-on-first-attempt computer + a real drainer settle both items.
	comp := &settleComputer{}
	dr, err := queue.NewDrainer(queue.Deps{
		Store: mem, Compute: comp, Policy: fixedPolicy{}, Cipher: cipher,
		Backoff:  queue.Backoff{Base: time.Minute, Factor: 2, Cap: time.Hour, RateWait: time.Second},
		Now:      func() time.Time { return now },
		WorkerID: "w1", LeaseTTL: time.Minute, MaxSMTPDeadline: 10 * time.Second,
		MaxAttempts: 3, MaxRetryAge: time.Hour, BatchSize: 10, ReclaimEvery: time.Hour,
	})
	if err != nil {
		t.Fatalf("drainer: %v", err)
	}
	dr.TickOnce(context.Background())

	// Status → complete (authoritative), caller_ref echoed opaque.
	statusReq := httptest.NewRequest(http.MethodGet, "/v1/jobs/"+jobID, nil)
	statusReq.Header.Set("Authorization", "Bearer bulk-key")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, statusReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	var status map[string]any
	json.Unmarshal(rec.Body.Bytes(), &status)
	if status["complete"] != true {
		t.Fatalf("all items settled → complete must be true, got %v (body %s)", status["complete"], rec.Body.String())
	}
	if status["caller_ref"] != "platform-job-42" {
		t.Fatalf("caller_ref must be echoed opaque, got %v", status["caller_ref"])
	}

	// Results → byte-identical verdict objects; email empty (plaintext purged, Door 3).
	resReq := httptest.NewRequest(http.MethodGet, "/v1/jobs/"+jobID+"/results", nil)
	resReq.Header.Set("Authorization", "Bearer bulk-key")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, resReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("results: %d %s", rec.Code, rec.Body.String())
	}
	var results struct {
		Results []map[string]any `json:"results"`
	}
	json.Unmarshal(rec.Body.Bytes(), &results)
	if len(results.Results) != 2 {
		t.Fatalf("expected 2 settled results, got %d", len(results.Results))
	}
	for _, r := range results.Results {
		// Door-1 shape present.
		if r["status"] == nil || r["reason"] == nil || r["recommendation"] == nil {
			t.Fatalf("result missing Door-1 fields: %v", r)
		}
		// Door 3: the raw address must NOT be echoed in a bulk result (purged on settle).
		if r["email"] != "" {
			t.Fatalf("Door 3 VIOLATED: bulk result echoed a plaintext email %q", r["email"])
		}
	}
}

// settleComputer settles every check on the first attempt (valid/accepted).
type settleComputer struct{}

func (settleComputer) ComputeForQueue(_ context.Context, _ string, _ *policy.Definition, _ string) (orchestrator.QueueOutcome, error) {
	row := store.VerificationRow{
		ID: "018f0000-0000-7000-8000-00000000f" + randHex(), RequestID: "018f0000-0000-7000-8000-000000000fff",
		CheckedAt: time.Unix(1, 0), CreatedAt: time.Unix(1, 0),
		Status: "valid", Reason: "accepted", Confidence: 90, Recommendation: "send",
		Domain: "acmecorp.com", PolicyKey: "cold-email", PolicyVersion: 1, APIVersion: "v1", ConfigFingerprint: "cfg",
	}
	return orchestrator.QueueOutcome{Class: orchestrator.QSettled, Row: row}, nil
}

// EscalateExhausted: no fallback wired in this bulk-endpoint test → outcome unchanged.
func (settleComputer) EscalateExhausted(_ context.Context, _ string, _ *policy.Definition, out orchestrator.QueueOutcome, _, _ string) orchestrator.QueueOutcome {
	return out
}

type fixedPolicy struct{}

func (fixedPolicy) Resolve(_ context.Context, _ string, _ int) (*policy.Definition, error) {
	return &policy.Definition{Key: "cold-email", Version: 1}, nil
}

var hexCounter int

func randHex() string {
	hexCounter++
	return string(rune('0' + hexCounter%10))
}
