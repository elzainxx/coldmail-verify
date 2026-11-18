package api

// FIX-PLAN ITEM 14 — M1's OWN GAP, AND THE CONTROL FOR IT.
//
// The property item 14 states, identically in all three modules: declare the version once, derive
// what you publish from it, and REFUSE rather than emit a version you cannot vouch for. M1's
// declaration is verdict.APIVersion and its enforcement is verdict.Response.Valid(), called by
// every writer before the write. The bulk results path was the one writer that did not call it —
// and worse, verdictFromRow COERCED a missing api_version to "v1" (`firstNonEmpty(row.APIVersion,
// "v1")`), so the field always looked right and no caller-side check could ever see it missing.
//
// WHY THIS FILE EXISTS RATHER THAN AN ASSERTION BOLTED ONTO TestBulk_EndToEnd. That test drains a
// real queue through a real drainer, so every row it can produce is on-contract by construction: it
// cannot stage the input this fix is about. Reaching the refusal needs a JobReader that hands the
// handler a row the rest of the system would never write — and until this file there was NO test
// fake for JobReader anywhere in the package, which is why handleJobResults' rebuilding loop had
// never been exercised against a hostile row. That absence is the reason the gap survived.
//
// THE BAD ROW IS NOT HYPOTHETICAL. migrations/0001_verifications.sql:41 declares
// `api_version text NOT NULL` with NO CHECK constraint, and status/reason/recommendation/confidence
// are equally unconstrained at the column level (the vocabulary lives in Go, and in a CHECK only on
// some columns). So an older build, a partially-applied migration, or a future /v2 writer all
// produce rows this handler must refuse rather than serve.
//
// WHAT EACH CASE CONTROLS — the two defects are independent, and the split below is MEASURED, not
// reasoned. Each break was performed against this file and the exact red/green split recorded:
//
//   - emptyAPIVersion controls THE COERCION, and is the only case that can. Break B106 restored
//     `firstNonEmpty(row.APIVersion, "v1")` in verdictFromRow: this case went red (200 + a verdict
//     wearing api_version "v1" that no row stored — the response body is in the failure output), and
//     so did ARefusedRowFailsTheWholePage, which stages an empty-version row too. The other four
//     stayed GREEN, because for them the coercion never fires.
//   - foreignAPIVersion / offVocabularyStatus / forbiddenComposition / confidenceOutOfRange control
//     THE MISSING Valid() CALL, which the coercion cannot mask. Break B107 deleted the
//     `if !v.Valid()` branch: all five refusal cases went red, the positive control stayed green.
//
// So the fix does not pass this file in halves: B106 (coercion back, check present) and B107 (check
// gone, coercion absent) each leave it red, for different subsets and different reasons.
//
// ON THE POSITIVE DIRECTION. There is deliberately no case proving "a row's own non-"v1" version is
// echoed verbatim": Valid() requires api_version == verdict.APIVersion, so any row whose version
// differs is refused, and pass-through is observable ONLY through the two refusal cases above. That
// is the correct shape for a frozen contract — a /v2 row is not served by the /v1 surface at all.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/config"
	"github.com/coldmail-verify/control-plane/internal/secrets"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/verdict"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// stagedJobs is a JobReader that serves rows a test hands it. It is the ONLY way to put an
// off-contract stored row in front of handleJobResults (see the file header).
//
// It records the ownerUserID it was called with on BOTH methods, and the tests assert it. That is
// not incidental bookkeeping: JobReader was widened to carry the tenant precisely so "a JobReader
// implementation (including a test fake) can no longer be written that ignores tenancy without
// visibly dropping a parameter" (api.go:84). A fake that accepted the parameter and threw it away
// would satisfy the compiler while un-testing the property the widening exists for.
type stagedJobs struct {
	rows []store.WorkItemResult
	next string

	getJobOwner string
	listOwner   string
	listCalls   int
	getJobCalls int
	unknown     bool // GetJob answers "not known" — the 404 path, used for an anti-vacuity check
}

func (s *stagedJobs) GetJob(_ context.Context, jobID, ownerUserID string) (store.JobRow, bool, error) {
	s.getJobCalls++
	s.getJobOwner = ownerUserID
	if s.unknown {
		return store.JobRow{}, false, nil
	}
	return store.JobRow{ID: jobID, Status: "complete", TotalCount: len(s.rows), SettledCount: len(s.rows), OwnerUserID: ownerUserID}, true, nil
}

func (s *stagedJobs) JobCompletion(_ context.Context, _, _ string) (bool, int, int, error) {
	return true, len(s.rows), len(s.rows), nil
}

func (s *stagedJobs) ListJobResults(_ context.Context, _, ownerUserID, _ string, _ int) ([]store.WorkItemResult, string, error) {
	s.listCalls++
	s.listOwner = ownerUserID
	return s.rows, s.next, nil
}

// stagedResultsServer wires a Server whose ONLY non-real dependency is the JobReader. Auth is the
// real per-caller lookup over a MemStore with a seeded key+user, so the request travels the same
// authenticate → tenancy → page path production does. bulk (the submitter) is nil on purpose: this
// surface never submits, and passing a live enqueuer would imply the fixture proves something about
// submission that it does not.
func stagedResultsServer(t *testing.T, jobs JobReader) *Server {
	t.Helper()
	now := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	cfg := config.Defaults()
	cfg.Env = "production" // never let a dev-mode auth bypass carry these tests
	sec, _ := secrets.Load(fakeSource{}, false)
	mem := store.NewMem(func() time.Time { return now })
	mem.SeedCallerWithUser(sha("results-key"), "caller_results", bulkOwner, now.Add(-time.Hour), time.Time{})

	srv := New(cfg, sec, mustHasher(t)).WithCallerAuth(mem).WithBulk(nil, jobs, 0)
	srv.now = func() time.Time { return now }
	return srv
}

// getResults performs the real HTTP round trip for the results page.
func getResults(t *testing.T, srv *Server, jobID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/jobs/"+jobID+"/results", nil)
	req.Header.Set("Authorization", "Bearer results-key")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// onContractRow is a stored row that IS servable: the version this build declares, an in-vocabulary
// status/reason/recommendation, a composable status→recommendation pair, confidence in range.
//
// It reads verdict.APIVersion rather than the literal "v1" on purpose. If the module ever moves to
// /v2, a literal here would turn the POSITIVE control red for a reason that has nothing to do with
// this fix, and whoever hit it would be tempted to edit the fixture rather than notice that the
// refusal cases below are the ones that must be re-thought.
func onContractRow(id string) store.WorkItemResult {
	return store.WorkItemResult{
		WorkItemID: id,
		EmailHash:  []byte("hash-" + id),
		Verdict: store.VerificationRow{
			ID:             "ver-" + id,
			APIVersion:     verdict.APIVersion,
			Status:         string(vocab.StatusRisky),
			Reason:         string(vocab.ReasonPlausible),
			Confidence:     55,
			Recommendation: string(vocab.RecReview),
			Domain:         "acmecorp.com",
			PolicyKey:      "cold_email",
			PolicyVersion:  1,
		},
	}
}

// withVerdict returns onContractRow with one field bent off-contract.
func withVerdict(id string, mutate func(*store.VerificationRow)) store.WorkItemResult {
	r := onContractRow(id)
	mutate(&r.Verdict)
	return r
}

// TestJobResults_ServesAnOnContractRow is the POSITIVE control. Without it every refusal case below
// is satisfiable by a handler that refuses everything — the vacuity shape this repo keeps finding.
func TestJobResults_ServesAnOnContractRow(t *testing.T) {
	jobs := &stagedJobs{rows: []store.WorkItemResult{onContractRow("wi-1"), onContractRow("wi-2")}, next: "wi-2"}
	srv := stagedResultsServer(t, jobs)

	rec := getResults(t, srv, stagedJobID)
	if rec.Code != http.StatusOK {
		t.Fatalf("an on-contract page must serve 200; got %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		JobID      string           `json:"job_id"`
		Results    []map[string]any `json:"results"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if len(body.Results) != 2 {
		t.Fatalf("both staged rows must be served, got %d (body %s)", len(body.Results), rec.Body.String())
	}
	if body.JobID != stagedJobID || body.NextCursor != "wi-2" {
		t.Fatalf("envelope must echo job_id and the store's cursor, got job_id=%q next_cursor=%q", body.JobID, body.NextCursor)
	}
	for _, r := range body.Results {
		if r["api_version"] != verdict.APIVersion {
			t.Fatalf("a served result must carry the row's api_version (%q), got %v", verdict.APIVersion, r["api_version"])
		}
		if r["status"] != string(vocab.StatusRisky) || r["recommendation"] != string(vocab.RecReview) {
			t.Fatalf("the settled verdict must be served verbatim, got %v", r)
		}
		// Door 3 (§5.3/DR-4): the plaintext was purged on settle, so a bulk result never echoes it.
		// Asserted here too because this is the one test that stages the row DIRECTLY — if
		// verdictFromRow ever started echoing a field the drainer's own fixture leaves empty, the
		// end-to-end test could not tell.
		if r["email"] != "" {
			t.Fatalf("Door 3 VIOLATED: a bulk result echoed a plaintext address %q", r["email"])
		}
	}

	// ANTI-VACUITY: the page really came from the fake, and the fake really received the tenant.
	if jobs.listCalls != 1 || jobs.getJobCalls != 1 {
		t.Fatalf("handler must read the job then its page exactly once each; got GetJob=%d ListJobResults=%d", jobs.getJobCalls, jobs.listCalls)
	}
	if jobs.listOwner != bulkOwner || jobs.getJobOwner != bulkOwner {
		t.Fatalf("both reads must be scoped to the AUTHENTICATED key's user (%s); got GetJob=%q ListJobResults=%q",
			bulkOwner, jobs.getJobOwner, jobs.listOwner)
	}
}

// TestJobResults_RefusesAStoredRowThatFailsValid is the control for the fix. Each case is a row the
// column-level schema permits and the contract does not.
func TestJobResults_RefusesAStoredRowThatFailsValid(t *testing.T) {
	// A version this build is not. Derived from the declaration so it cannot accidentally BECOME the
	// declared version at a /v2 bump — the case would then silently start asserting the opposite.
	foreignVersion := verdict.APIVersion + "-alt"

	cases := []struct {
		name string
		row  store.WorkItemResult
		why  string
	}{
		{
			"emptyAPIVersion",
			withVerdict("wi-1", func(v *store.VerificationRow) { v.APIVersion = "" }),
			"api_version is `text NOT NULL` with no CHECK, so '' is storable; it must be refused, " +
				"NOT coerced to the declared version (that coercion is the defect item 14 removes)",
		},
		{
			"foreignAPIVersion",
			withVerdict("wi-1", func(v *store.VerificationRow) { v.APIVersion = foreignVersion }),
			"a row written by a different contract version cannot be served as a " + verdict.APIVersion + " verdict",
		},
		{
			"offVocabularyStatus",
			withVerdict("wi-1", func(v *store.VerificationRow) { v.Status = "probably_fine" }),
			"the four statuses are frozen (Door 4); a fifth one reaching a caller breaks every consumer's switch",
		},
		{
			"forbiddenComposition",
			withVerdict("wi-1", func(v *store.VerificationRow) {
				v.Status, v.Recommendation = string(vocab.StatusInvalid), string(vocab.RecSend)
			}),
			"invalid+send is the LOCKED composability violation — both values are individually legal, " +
				"which is why only ComposabilityOK catches it",
		},
		{
			"confidenceOutOfRange",
			withVerdict("wi-1", func(v *store.VerificationRow) { v.Confidence = 101 }),
			"confidence is a LOCKED 0-100 int; the column is a bare integer",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jobs := &stagedJobs{rows: []store.WorkItemResult{tc.row}}
			srv := stagedResultsServer(t, jobs)

			rec := getResults(t, srv, stagedJobID)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("%s: %s — expected 500, got %d %s", tc.name, tc.why, rec.Code, rec.Body.String())
			}
			var env struct {
				Error struct {
					Code      string `json:"code"`
					Message   string `json:"message"`
					RequestID string `json:"request_id"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("%s: the refusal must be the coded envelope, not plain text: %v (body %s)", tc.name, err, rec.Body.String())
			}
			// contract_violation, not internal_error: the two share HTTP 500 on purpose (errors.go's
			// codeStatus), and the CODE is what tells an operator "a STORED row is off-contract"
			// rather than "something blew up". Asserting the code, not just the status, is what makes
			// this test able to notice the refusal being downgraded to generic noise.
			if env.Error.Code != codeContractViolation {
				t.Fatalf("%s: expected code %q, got %q (body %s)", tc.name, codeContractViolation, env.Error.Code, rec.Body.String())
			}
			if env.Error.RequestID == "" {
				t.Fatalf("%s: every error carries a request_id (§1.3); got none", tc.name)
			}

			// Two-vocabulary rule (Rule 5 / §1.3): the envelope must not leak the row that offended.
			// The internal reason names the condition and goes to the LOG only.
			body := rec.Body.String()
			for _, leak := range []string{"probably_fine", foreignVersion, "acmecorp.com", "cold_email", "101"} {
				if strings.Contains(body, leak) {
					t.Fatalf("%s: the error envelope leaked internal detail %q: %s", tc.name, leak, body)
				}
			}
		})
	}
}

// TestJobResults_ARefusedRowFailsTheWholePage pins the CHOICE the fix made, which is not implied by
// any case above: a bad row fails the PAGE rather than being filtered out of it.
//
// Filtering would look strictly friendlier — the caller gets its good rows — and would be a silent
// data-loss bug: a page that quietly omits rows is indistinguishable from a page whose omitted rows
// were never submitted, and the caller has no field to detect it by (next_cursor still advances).
// The refusal is loud, coded, and correlatable instead.
//
// THIS TEST IS THE ONLY ONE THAT CAN SEE THAT, break-verified rather than assumed. The cases above
// each stage a ONE-ROW page, so any implementation that refuses when NOTHING survived satisfies all
// of them. Measured (break B109): replacing the refusal with `continue` plus a trailing
// `if len(results) == 0 && len(rows) > 0 { refuse }` left every case above GREEN and only this test
// red. A plain `continue` with no trailing check (break B108) is caught more widely — but that is the
// cruder mistake, and it is the subtle one that needs a control.
func TestJobResults_ARefusedRowFailsTheWholePage(t *testing.T) {
	good := onContractRow("wi-1")
	bad := withVerdict("wi-2", func(v *store.VerificationRow) { v.APIVersion = "" })
	jobs := &stagedJobs{rows: []store.WorkItemResult{good, bad}, next: "wi-2"}
	srv := stagedResultsServer(t, jobs)

	rec := getResults(t, srv, stagedJobID)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("one off-contract row must fail the whole page, not be filtered from it; got %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "\"results\"") || strings.Contains(body, "wi-1") || strings.Contains(body, "next_cursor") {
		t.Fatalf("the refusal must not ship a partial page (no results[], no cursor to continue from): %s", body)
	}
}

// TestJobResults_TenancyStillShortCircuitsBeforeThePage guards the ORDER of the two checks, which
// the fix could plausibly have disturbed by moving work earlier. A job that is absent or another
// tenant's must 404 from GetJob and never reach ListJobResults — otherwise a cross-tenant caller
// could learn, from a contract_violation vs a not_found, that someone else's job holds a malformed
// row. That is a real (if narrow) oracle, and it exists only because the refusal is a DIFFERENT
// code from the 404.
func TestJobResults_TenancyStillShortCircuitsBeforeThePage(t *testing.T) {
	jobs := &stagedJobs{
		unknown: true,
		rows:    []store.WorkItemResult{withVerdict("wi-1", func(v *store.VerificationRow) { v.APIVersion = "" })},
	}
	srv := stagedResultsServer(t, jobs)

	rec := getResults(t, srv, otherStagedJobID)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("an absent-or-not-yours job must 404 before the page is read; got %d %s", rec.Code, rec.Body.String())
	}
	if jobs.listCalls != 0 {
		t.Fatalf("ListJobResults must not run for a job the caller cannot see; it ran %d time(s)", jobs.listCalls)
	}
}

// JobStatusSnapshot is on JobReader since 0019 (the status route is built from one snapshot). The results-route tests do not use it, so this fake serves a
// complete job whose totals are the staged rows; it records nothing and asserts nothing, and the route that uses it is covered with the real store.
func (s *stagedJobs) JobStatusSnapshot(_ context.Context, jobID, ownerUserID string) (store.JobSnapshot, bool, error) {
	if s.unknown {
		return store.JobSnapshot{}, false, nil
	}
	n := len(s.rows)
	return store.JobSnapshot{
		Job:      store.JobRow{ID: jobID, Status: "complete", TotalCount: n, SettledCount: n, OwnerUserID: ownerUserID},
		Complete: true,
		Totals:   store.JobTotals{Submitted: n, Settled: n, ByStatus: map[string]int{}},
	}, true, nil
}

// The job ids of the staged-results tests. They are UUIDs because a job id that is not one is a 400 before any reader is called (0019, A3); the old
// fixtures used "job-1" and "someone-elses-job", which the route now refuses as malformed ids. Only the id's shape changed, not what any test asserts.
const (
	stagedJobID      = "019fd000-0000-7000-8000-000000000001"
	otherStagedJobID = "019fd000-0000-7000-8000-000000000002"
)
