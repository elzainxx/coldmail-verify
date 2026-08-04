package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/config"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/queue"
	"github.com/coldmail-verify/control-plane/internal/secrets"
	"github.com/coldmail-verify/control-plane/internal/store"
)

// =============================================================================
// Kill-switch Rung 4 — the front door half (fix-plan item 4 / D2)
// =============================================================================
//
// THE DEFECT. TM_FRONTDOOR_MODE=accept_and_defer validated at load and had NO consumer: both readers
// of KillSwitch.FrontDoor compared only against FrontDoorReject. So the middle rung of the pause
// control was indistinguishable from `serving` — an operator who pulled it during a data-integrity
// incident kept producing verdicts, with no error and no warning. "A silently inert safety control
// is worse than an absent one" (fix plan item 4), because an absent one at least fails loudly.
//
// WHAT THE RUNG PROMISES, split by surface (kill-switch-ladder.md Rung 4, PROJECT_RULES.md §4·PAUSED):
//
//	/v1/verify (SYNC)  → the explicit unavailable envelope. There is no deferred form of a
//	                     synchronous answer: no handle to return, no queue row to defer into. Serving
//	                     a verdict would defeat the rung; returning 200 without one would be a
//	                     fabricated verdict (Doors 1 and 2). So it refuses, exactly as under reject.
//	/v1/verify/bulk    → 202 + a job handle, work stored durably, `processing_paused: true`. THIS is
//	                     what distinguishes defer from reject, and why the two sub-modes cannot share
//	                     one predicate at this call site.
//	GET /v1/jobs/{id}  → the handle keeps working and reports processing_paused, so a caller learns
//	                     WHY progress is zero instead of inferring a stall.
//
// The drainer half — that nothing is actually processed — lives in
// queue/drainer_pause_test.go. Both halves are required: accepting work that then quietly drains
// would be the rung failing in the dangerous direction, and refusing work would be `reject` wearing
// the wrong name.
//
// ANTI-VACUITY. Every refusal is paired with the SAME request at `serving`, which must succeed, and
// every acceptance is paired with the same request at `reject`, which must be refused. A 503 proves
// nothing on its own: an unwired server, a bad fixture, or a missing key all produce one.
//
// FAIL-VERIFY:
//   - Revert handleVerify's gate to `== config.FrontDoorReject` → the sync-refusal case fails at
//     defer (the D2 defect, restored).
//   - Change handleBulkSubmit's gate to VerdictProductionPaused → the accept case fails (503 where
//     the rung promises a handle) — defer would have collapsed into reject.
//   - Hardcode `"processing_paused": false` → the disclosure cases fail. Hardcode `true` → the
//     serving controls fail. Only the live derivation satisfies both.

// deferServer is bulkServer at an explicit front-door mode: same wiring, one config value changed,
// so a difference in outcome is attributable to the rung and nothing else.
func deferServer(t *testing.T, now time.Time, mode config.FrontDoorMode) (*Server, *store.MemStore, *queue.AddrCipher) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Env = "production"
	cfg.KillSwitch.FrontDoor = mode
	// The mode must be one Validate() accepts — otherwise this test would be asserting behaviour for
	// a config the process would refuse to start with.
	if err := cfg.Validate(); err != nil {
		t.Fatalf("front-door mode %q must be a VALID config (it is in the accepted set): %v", mode, err)
	}
	sec, _ := secrets.Load(fakeSource{}, false)
	h := mustHasher(t)
	mem := store.NewMem(func() time.Time { return now })
	def, _ := policy.ColdEmailDefinition()
	cipher, err := queue.NewAddrCipher(key32(3))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	enq := &queue.Enqueuer{Store: mem, Hasher: h, Cipher: cipher, ConfigFingerprint: "cfg", Now: func() time.Time { return now }}
	srv := New(cfg, sec, h).WithVerifier(stubVerifier{}, def).WithCallerAuth(mem).WithBulk(enq, mem, 0)
	srv.now = func() time.Time { return now }
	mem.SeedCallerWithUser(sha("defer-key"), "caller_defer", bulkOwner, now.Add(-time.Hour), time.Time{})
	return srv, mem, cipher
}

func do(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer defer-key")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	return rec
}

// TestFrontDoorDefer_SyncVerifyRefusesLikeReject: the sync surface must refuse at BOTH pause
// sub-modes, and the two refusals must be identical — an operator who pulled the softer rung has
// still disabled verdict production, and a caller must not be able to tell which rung is pulled
// (that is operational detail, not a client concern).
func TestFrontDoorDefer_SyncVerifyRefusesLikeReject(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	const body = `{"email":"jane.doe@acmecorp.com"}`

	// The D2 case: accept_and_defer used to serve verdicts here.
	srvDefer, _, _ := deferServer(t, now, config.FrontDoorDefer)
	recDefer := do(t, srvDefer, http.MethodPost, "/v1/verify", body)
	if recDefer.Code != http.StatusServiceUnavailable {
		t.Fatalf("D2: /v1/verify at accept_and_defer returned %d, want 503 — the rung disables verdict production and there is no deferred form of a synchronous answer (body %s)",
			recDefer.Code, recDefer.Body.String())
	}

	srvReject, _, _ := deferServer(t, now, config.FrontDoorReject)
	recReject := do(t, srvReject, http.MethodPost, "/v1/verify", body)
	if recReject.Code != recDefer.Code || recReject.Body.String() != recDefer.Body.String() {
		t.Fatalf("the two pause sub-modes must be indistinguishable on the SYNC surface: defer=%d %s vs reject=%d %s",
			recDefer.Code, recDefer.Body.String(), recReject.Code, recReject.Body.String())
	}

	// ANTI-VACUITY: the identical request at `serving` must NOT be 503, or the two assertions above
	// would hold for a server that is simply broken.
	srvServing, _, _ := deferServer(t, now, config.FrontDoorServing)
	recServing := do(t, srvServing, http.MethodPost, "/v1/verify", body)
	if recServing.Code == http.StatusServiceUnavailable {
		t.Fatalf("control failed: /v1/verify at serving is ALSO 503 (%s) — the refusals above prove nothing about the rung",
			recServing.Body.String())
	}
}

// TestFrontDoorDefer_BulkStillAcceptsAndDisclosesPause is the sub-mode's whole reason to exist: work
// is accepted and stored, and the caller is TOLD it will not be processed yet.
func TestFrontDoorDefer_BulkStillAcceptsAndDisclosesPause(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	const body = `{"addresses":["a@acmecorp.com","b@acmecorp.com"],"caller_ref":"platform-job-99"}`

	srv, mem, _ := deferServer(t, now, config.FrontDoorDefer)
	rec := do(t, srv, http.MethodPost, "/v1/verify/bulk", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("accept_and_defer must ACCEPT a bulk submission (that is the difference from reject), got %d %s",
			rec.Code, rec.Body.String())
	}
	var submit map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &submit); err != nil {
		t.Fatalf("decode 202: %v", err)
	}
	jobID, _ := submit["job_id"].(string)
	if jobID == "" {
		t.Fatal("accept_and_defer must return a JOB HANDLE — the rung's promise is 'a job handle + processing paused'")
	}
	if submit["processing_paused"] != true {
		t.Fatalf("the 202 must disclose processing_paused=true at Rung 4, got %v — otherwise the caller watches a job sit at zero settled with no explanation",
			submit["processing_paused"])
	}
	// The work must be DURABLE, not merely acknowledged: "work sits durably in work_items".
	items := mem.WorkItemsForJob(jobID)
	if len(items) != 2 {
		t.Fatalf("deferred submission stored %d work item(s), want 2 — accept-and-defer stores the work, it does not accept-and-drop", len(items))
	}
	for _, it := range items {
		if it.Status != "ready" {
			t.Fatalf("deferred item status=%q, want ready (queued and waiting, not held or dead)", it.Status)
		}
		if len(it.AddrCiphertext) == 0 {
			t.Fatal("deferred item has no DR-4 ciphertext — there would be nothing to check when the rung is released")
		}
	}

	// The handle keeps working, and it too discloses the pause.
	st := do(t, srv, http.MethodGet, "/v1/jobs/"+jobID, "")
	if st.Code != http.StatusOK {
		t.Fatalf("the job handle must remain readable at Rung 4, got %d %s", st.Code, st.Body.String())
	}
	var status map[string]any
	json.Unmarshal(st.Body.Bytes(), &status)
	if status["processing_paused"] != true {
		t.Fatalf("job status must report processing_paused=true at Rung 4, got %v", status["processing_paused"])
	}
	if status["complete"] != false {
		t.Fatalf("a deferred job cannot be complete, got complete=%v", status["complete"])
	}
	if counts, ok := status["counts"].(map[string]any); !ok || counts["settled"] != float64(0) {
		t.Fatalf("a deferred job must report zero settled, got %v", status["counts"])
	}

	// ANTI-VACUITY #1: at reject, the SAME submission is refused. Without this, "202 at defer" would
	// not show that the gate distinguishes the sub-modes at all.
	srvReject, _, _ := deferServer(t, now, config.FrontDoorReject)
	if rr := do(t, srvReject, http.MethodPost, "/v1/verify/bulk", body); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("reject must REFUSE a bulk submission, got %d %s — reject and defer would be the same rung",
			rr.Code, rr.Body.String())
	}

	// ANTI-VACUITY #2: at serving, the same submission is accepted with processing_paused=FALSE. This
	// is what makes the flag a derived fact rather than a constant.
	srvServing, _, _ := deferServer(t, now, config.FrontDoorServing)
	rs := do(t, srvServing, http.MethodPost, "/v1/verify/bulk", body)
	if rs.Code != http.StatusAccepted {
		t.Fatalf("serving must accept a bulk submission, got %d %s", rs.Code, rs.Body.String())
	}
	var servingSubmit map[string]any
	json.Unmarshal(rs.Body.Bytes(), &servingSubmit)
	if servingSubmit["processing_paused"] != false {
		t.Fatalf("at serving, processing_paused must be FALSE, got %v — a hardcoded true would be the same inert-control bug in the other direction",
			servingSubmit["processing_paused"])
	}
	servingJob, _ := servingSubmit["job_id"].(string)
	ss := do(t, srvServing, http.MethodGet, "/v1/jobs/"+servingJob, "")
	var servingStatus map[string]any
	json.Unmarshal(ss.Body.Bytes(), &servingStatus)
	if servingStatus["processing_paused"] != false {
		t.Fatalf("at serving, job status processing_paused must be FALSE, got %v", servingStatus["processing_paused"])
	}
}

// TestFrontDoorDefer_JobStatusVocabularyUnchanged pins the CHOICE OF MECHANISM, not just the effect:
// the pause is disclosed as an additive boolean and NOT as a new `status` value. The job-status
// vocabulary is persisted under a DB CHECK constraint (0009: submitted|running|complete|
// complete_with_unknowns); minting a fifth value to express a transient operational state would put
// a one-way door in the service of a config flag that can flip back in a minute.
func TestFrontDoorDefer_JobStatusVocabularyUnchanged(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	srv, _, _ := deferServer(t, now, config.FrontDoorDefer)
	rec := do(t, srv, http.MethodPost, "/v1/verify/bulk", `{"addresses":["a@acmecorp.com"]}`)
	var submit map[string]any
	json.Unmarshal(rec.Body.Bytes(), &submit)
	jobID, _ := submit["job_id"].(string)

	st := do(t, srv, http.MethodGet, "/v1/jobs/"+jobID, "")
	var status map[string]any
	json.Unmarshal(st.Body.Bytes(), &status)

	allowed := map[string]bool{"submitted": true, "running": true, "complete": true, "complete_with_unknowns": true}
	got, _ := status["status"].(string)
	if !allowed[got] {
		t.Fatalf("job status %q is outside the persisted vocabulary (0009 CHECK) — the Rung-4 pause must be an ADDITIVE field, never a new status value", got)
	}
	// And the submit envelope's status field is likewise untouched.
	if submit["status"] != "accepted" {
		t.Fatalf("the 202 envelope status must stay %q, got %v", "accepted", submit["status"])
	}
}

// TestFrontDoorDefer_SummaryNamesTheRung: the operator-visible surfaces must AGREE with the config
// they were handed. D2's real damage was that everything looked normal — the boot log and /readyz
// echoed the mode string while the code ignored it, so the echo was evidence of nothing.
func TestFrontDoorDefer_SummaryNamesTheRung(t *testing.T) {
	cfg := config.Defaults()
	cfg.KillSwitch.FrontDoor = config.FrontDoorDefer
	if s := cfg.Summary(); !strings.Contains(s, "paused-accept-and-defer(Rung4)") {
		t.Fatalf("Summary must name the pulled rung, got %q", s)
	}
	cfg.KillSwitch.FrontDoor = config.FrontDoorReject
	if s := cfg.Summary(); !strings.Contains(s, "paused-reject(Rung4)") {
		t.Fatalf("Summary must name the pulled rung, got %q", s)
	}
	// Control: at serving the summary must NOT claim a pause.
	cfg.KillSwitch.FrontDoor = config.FrontDoorServing
	if s := cfg.Summary(); strings.Contains(s, "Rung4") {
		t.Fatalf("Summary claims Rung 4 while serving: %q", s)
	}

	// And /readyz carries the same summary, so the pause is visible to a monitor, not only in a log
	// line that scrolled past hours ago.
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	srv, _, _ := deferServer(t, now, config.FrontDoorDefer)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if !strings.Contains(rec.Body.String(), "paused-accept-and-defer(Rung4)") {
		t.Fatalf("/readyz must show the pulled rung, got %s", rec.Body.String())
	}
}
