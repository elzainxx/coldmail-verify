package api

// The partial Tier 4 of module 1 and the bad-input fix (owner rulings OD-6/OD-8; Phase 3 close-out items A1 and A3), over HTTP, on the in-memory
// store (the same handlers and the same queue code that run in production; the Postgres side of every store call is proved in internal/store and
// internal/queue against a real database).
//
//   - GET /v1/describe: authenticated, in the shape family of modules 2 and 3, its vocabularies read from the code's own constants, every served
//     route described and nothing else, no secret in the body, its schemas equal to what the handlers really return;
//   - POST /v1/verify/bulk with an Idempotency-Key: a repeat is the original job (202, same body, header Idempotent-Replayed: true), another request
//     under the same key is a 409 with its own code, one caller's key is invisible to another, and no header is exactly today's behaviour;
//   - GET /v1/jobs/{id}: `totals`, from one read, equal to the results row by row, with the 202's counts equal to them;
//   - a job id that is not a UUID is a 400 with the envelope on every route that takes one, and the database is never asked.
//
// Every property has its paired control in the same test (the property with its cause removed must fail, or its opposite must hold).

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/config"
	"github.com/coldmail-verify/control-plane/internal/orchestrator"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/secrets"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// ── helpers ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────

func postKeyed(t *testing.T, srv *Server, bearer, idemKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/bulk", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func jsonMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, b)
	}
	return m
}

// statusByAddrComputer settles each address with a status chosen by its local part, so a job's totals have several non-zero by_status entries:
// "inv…" invalid, "risk…" risky, "unk…" unknown, anything else valid. It checks each address once and counts the checks.
type statusByAddrComputer struct {
	pgSettleComputer
	mu    sync.Mutex
	calls map[string]int
}

func (c *statusByAddrComputer) ComputeForQueue(ctx context.Context, raw string, def *policy.Definition, jobID string) (orchestrator.QueueOutcome, error) {
	out, err := c.pgSettleComputer.ComputeForQueue(ctx, raw, def, jobID)
	if err != nil {
		return out, err
	}
	c.mu.Lock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[raw]++
	c.mu.Unlock()
	switch {
	case strings.HasPrefix(raw, "inv"):
		out.Row.Status, out.Row.Reason, out.Row.Recommendation = "invalid", "mailbox_not_found", "skip"
	case strings.HasPrefix(raw, "risk"):
		out.Row.Status, out.Row.Reason, out.Row.Recommendation = "risky", "catch_all", "review"
	case strings.HasPrefix(raw, "unk"):
		out.Row.Status, out.Row.Reason, out.Row.Recommendation = "unknown", "smtp_timeout", "review"
	}
	out.Row.Domain = raw[strings.LastIndex(raw, "@")+1:]
	return out, nil
}

func (c *statusByAddrComputer) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.calls {
		n += v
	}
	return n
}

// countingJobs counts every call that reaches the store through the Server's JobReader seam: the only road from a handler to the database.
type countingJobs struct {
	store.Store
	mu    sync.Mutex
	calls int
}

func (c *countingJobs) bump() { c.mu.Lock(); c.calls++; c.mu.Unlock() }
func (c *countingJobs) n() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}
func (c *countingJobs) GetJob(ctx context.Context, j, o string) (store.JobRow, bool, error) {
	c.bump()
	return c.Store.GetJob(ctx, j, o)
}
func (c *countingJobs) JobCompletion(ctx context.Context, j, o string) (bool, int, int, error) {
	c.bump()
	return c.Store.JobCompletion(ctx, j, o)
}
func (c *countingJobs) JobStatusSnapshot(ctx context.Context, j, o string) (store.JobSnapshot, bool, error) {
	c.bump()
	return c.Store.JobStatusSnapshot(ctx, j, o)
}
func (c *countingJobs) ListJobResults(ctx context.Context, j, o, cur string, lim int) ([]store.WorkItemResult, string, error) {
	c.bump()
	return c.Store.ListJobResults(ctx, j, o, cur, lim)
}

// walkResults reads a job's whole listing through HTTP in pages of `limit`, following next_cursor.
func walkResults(t *testing.T, srv *Server, bearer, jobID string, limit int) []map[string]any {
	t.Helper()
	var all []map[string]any
	cursor := ""
	for i := 0; i < 1000; i++ {
		path := fmt.Sprintf("/v1/jobs/%s/results?limit=%d", jobID, limit)
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rec := getAs(t, srv, bearer, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("results page: %d %s", rec.Code, rec.Body.String())
		}
		var page struct {
			Results    []map[string]any `json:"results"`
			NextCursor string           `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("results page is not JSON: %v", err)
		}
		all = append(all, page.Results...)
		if page.NextCursor == "" {
			return all
		}
		cursor = page.NextCursor
	}
	t.Fatal("the results walk did not end")
	return nil
}

// ── A3: bad input ────────────────────────────────────────────────────────────────────────────────────────────────────────────

// A job id that is not a UUID is a 400 with the standard envelope on EVERY route that takes a job id, and the store is never asked. A valid but unknown
// UUID is still the 404 not_found of tenancy, and a known one works. (Routes with a job id: GET /v1/jobs/{id} and GET /v1/jobs/{id}/results; there
// is no other, which the route table check at the end re-derives rather than assumes.)
func TestJobRoutes_ANonUUIDJobIDIs400WithTheEnvelope_AndNeverReachesTheStore(t *testing.T) {
	var counter *countingJobs
	srv, _, cipher, st := idsServer(t, func(s store.Store) store.Store { counter = &countingJobs{Store: s}; return counter })
	acc := mustAccept(t, postBulkAs(t, srv, "bulk-key", `{"addresses":["jane@acmecorp.com"]}`))
	jobID := acc["job_id"].(string)
	drainAll(t, st, cipher, &statusByAddrComputer{pgSettleComputer: pgSettleComputer{now: idsNow}})

	routes := map[string]string{"status": "/v1/jobs/%s", "results": "/v1/jobs/%s/results"}
	// Every route of the table that has an {id} wildcard is one of the two above.
	have := 0
	for _, rt := range srv.Routes() {
		if strings.Contains(rt.Path(), "{id}") {
			have++
		}
	}
	if have != len(routes) {
		t.Fatalf("the route table has %d route(s) with a job id and this test covers %d: a new route taking a job id needs its own 400 test", have, len(routes))
	}

	bad := []string{
		"job-1", "123", "not-a-uuid", "019fd000-0000-7000-8000-00000000000", // one hex digit short
		"019fd000-0000-7000-8000-0000000000001",  // one too long
		"019fd000000070008000000000000001",       // no hyphens (Postgres would take it; this service issues only the canonical form)
		"{019fd000-0000-7000-8000-000000000001}", // braces (likewise)
		"gggggggg-gggg-gggg-gggg-gggggggggggg",   // right shape, not hex
		"019fd000-0000-7000-8000-00000000000%20", // an encoded space
		"1%27%20OR%20%271%27=%271",               // SQL-shaped
	}
	for name, pat := range routes {
		for _, id := range bad {
			t.Run(name+" "+id, func(t *testing.T) {
				before := counter.n()
				rec := getAs(t, srv, "bulk-key", fmt.Sprintf(pat, id))
				env := requireEnvelope(t, rec, codeValidationFailed, http.StatusBadRequest)
				details, _ := env["details"].([]any)
				if len(details) != 1 || details[0].(map[string]any)["field"] != "id" || details[0].(map[string]any)["issue"] != issueMalformed {
					t.Errorf("details = %v, want [{field:id, issue:malformed}]", details)
				}
				if got := counter.n() - before; got != 0 {
					t.Errorf("a malformed job id reached the store %d time(s): it must be refused before any read", got)
				}
			})
		}
		// A bad id is a 400 only for an authenticated caller: a stranger learns the shape rule from nothing.
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf(pat, "job-1"), nil))
		requireEnvelope(t, rec, codeUnauthenticated, http.StatusUnauthorized)
	}

	// CONTROLS, on each route: a valid but unknown UUID is a 404 not_found (and DOES reach the store), a known id answers 200.
	for name, pat := range routes {
		t.Run("control "+name, func(t *testing.T) {
			before := counter.n()
			rec := getAs(t, srv, "bulk-key", fmt.Sprintf(pat, "019fd000-0000-7000-8000-0000000000ff"))
			requireEnvelope(t, rec, codeNotFound, http.StatusNotFound)
			if counter.n() == before {
				t.Error("control: a well-formed unknown id must be looked up (that is what makes it a 404 and not a 400)")
			}
			if rec := getAs(t, srv, "bulk-key", fmt.Sprintf(pat, jobID)); rec.Code != http.StatusOK {
				t.Errorf("control: the known job id must work, got %d %s", rec.Code, rec.Body.String())
			}
			// Another tenant's real job id is the same 404 as an unknown one.
			requireEnvelope(t, getAs(t, srv, "bulk-key-b", fmt.Sprintf(pat, jobID)), codeNotFound, http.StatusNotFound)
		})
	}
}

// The results cursor: a garbage cursor was already a 400 with the envelope (never a 500) before this change; this pins it across the shapes a caller
// can send, with the real cursors as the control.
func TestJobResults_AGarbageCursorIsA400NotA500(t *testing.T) {
	srv, _, cipher, st := idsServer(t, nil)
	acc := mustAccept(t, postBulkAs(t, srv, "bulk-key", `{"addresses":["a@acmecorp.com","b@acmecorp.com","c@acmecorp.com"]}`))
	jobID := acc["job_id"].(string)
	drainAll(t, st, cipher, &statusByAddrComputer{pgSettleComputer: pgSettleComputer{now: idsNow}})
	for _, cur := range []string{"abc", "-1", "1.5", "0x10", "%201", "99999999999999999999", "019fd000-0000-7000-8000-000000000001", "1%3B"} {
		rec := getAs(t, srv, "bulk-key", "/v1/jobs/"+jobID+"/results?cursor="+cur)
		env := requireEnvelope(t, rec, codeValidationFailed, http.StatusBadRequest)
		details, _ := env["details"].([]any)
		if len(details) != 1 || details[0].(map[string]any)["field"] != "cursor" {
			t.Errorf("cursor %q: details = %v, want the cursor field", cur, details)
		}
	}
	for _, cur := range []string{"", "0", "1"} { // CONTROL: real cursors page
		if rec := getAs(t, srv, "bulk-key", "/v1/jobs/"+jobID+"/results?limit=1&cursor="+cur); rec.Code != http.StatusOK {
			t.Errorf("control: cursor %q must page, got %d %s", cur, rec.Code, rec.Body.String())
		}
	}
}

// ── A1: totals ───────────────────────────────────────────────────────────────────────────────────────────────────────────────

const totalsBody = `{"items":[
 {"item_id":"a0","address":"valid1@acmecorp.com"},
 {"item_id":"a1","address":"inv1@acmecorp.com"},
 {"item_id":"a2","address":"Valid1@AcmeCorp.com"},
 {"item_id":"a3","address":"not an address"},
 {"item_id":"a4","address":"risk1@acmecorp.com"},
 {"item_id":"a5","address":"unk1@acmecorp.com"},
 {"item_id":"a6","address":"also not an address"},
 {"item_id":"a7","address":"inv1@acmecorp.com"}],
 "caller_ref":"platform-job-7"}`

type totalsView struct {
	Submitted, Settled, Pending, Duplicates, Malformed int
	ByStatus                                           map[string]int
}

func readTotals(t *testing.T, srv *Server, bearer, jobID string) (status map[string]any, tot totalsView) {
	t.Helper()
	rec := getAs(t, srv, bearer, "/v1/jobs/"+jobID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	status = jsonMap(t, rec.Body.Bytes())
	raw, ok := status["totals"].(map[string]any)
	if !ok {
		t.Fatalf("the status carries no totals object: %s", rec.Body.String())
	}
	num := func(k string) int { return int(raw[k].(float64)) }
	tot = totalsView{Submitted: num("submitted"), Settled: num("settled"), Pending: num("pending"), Duplicates: num("duplicates"), Malformed: num("malformed"), ByStatus: map[string]int{}}
	by, ok := raw["by_status"].(map[string]any)
	if !ok {
		t.Fatalf("totals.by_status is missing: %s", rec.Body.String())
	}
	for k, v := range by {
		tot.ByStatus[k] = int(v.(float64))
	}
	return status, tot
}

// totalsProblems states every property of a totals object: the identities, the key set of by_status (all four statuses, nothing else), and (given the
// listing) the equality with it row by row. It returns what is wrong, so the same function is the check and, pointed at a wrong count, the control.
func totalsProblems(tot totalsView, results []map[string]any, accepted map[string]any, complete bool) []string {
	var p []string
	sum := 0
	for _, n := range tot.ByStatus {
		sum += n
	}
	if tot.Submitted != tot.Settled+tot.Pending {
		p = append(p, fmt.Sprintf("submitted %d != settled %d + pending %d", tot.Submitted, tot.Settled, tot.Pending))
	}
	if sum != tot.Settled {
		p = append(p, fmt.Sprintf("sum(by_status) %d != settled %d", sum, tot.Settled))
	}
	keys := make([]string, 0, len(tot.ByStatus))
	for k := range tot.ByStatus {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "invalid,risky,unknown,valid" {
		p = append(p, fmt.Sprintf("by_status keys = %v, want exactly the four verdict statuses", keys))
	}
	counts := accepted["counts"].(map[string]any)
	if int(counts["duplicate"].(float64)) != tot.Duplicates || int(counts["malformed"].(float64)) != tot.Malformed ||
		int(counts["total"].(float64)) != tot.Submitted {
		p = append(p, fmt.Sprintf("the 202 said %v but totals say submitted %d duplicates %d malformed %d", counts, tot.Submitted, tot.Duplicates, tot.Malformed))
	}
	if len(results) != tot.Settled {
		p = append(p, fmt.Sprintf("the listing holds %d row(s), totals.settled is %d", len(results), tot.Settled))
	}
	tally := map[string]int{}
	dup := 0
	for _, r := range results {
		tally[r["status"].(string)]++
		if _, ok := r["duplicate_of"]; ok {
			dup++
		}
	}
	for _, s := range vocab.Statuses() {
		if tally[string(s)] != tot.ByStatus[string(s)] {
			p = append(p, fmt.Sprintf("the listing holds %d %s row(s), totals.by_status[%s] is %d", tally[string(s)], s, s, tot.ByStatus[string(s)]))
		}
	}
	if complete && dup != tot.Duplicates {
		p = append(p, fmt.Sprintf("the listing holds %d duplicate row(s), totals.duplicates is %d", dup, tot.Duplicates))
	}
	return p
}

func TestJobStatus_TotalsEqualTheResultsRowByRow_InFlightAndAfterCompletion(t *testing.T) {
	srv, _, cipher, st := idsServer(t, nil)
	acc := mustAccept(t, postBulkAs(t, srv, "bulk-key", totalsBody))
	jobID := acc["job_id"].(string)

	// IN FLIGHT: nothing drained yet. The two malformed rows are the only rows with an outcome; the 6 others wait. pending > 0 and every identity holds.
	status, inflight := readTotals(t, srv, "bulk-key", jobID)
	if status["complete"] != false {
		t.Fatalf("a job with unsettled work is not complete: %v", status["complete"])
	}
	if inflight.Pending <= 0 || inflight.Settled != 2 || inflight.Submitted != 8 {
		t.Fatalf("in flight: submitted %d settled %d pending %d, want 8 / 2 (the malformed rows) / 6", inflight.Submitted, inflight.Settled, inflight.Pending)
	}
	if p := totalsProblems(inflight, walkResults(t, srv, "bulk-key", jobID, 3), acc, false); len(p) != 0 {
		t.Fatalf("in flight: %v", p)
	}
	counts := status["counts"].(map[string]any)
	if int(counts["total"].(float64)) != inflight.Submitted || int(counts["settled"].(float64)) != inflight.Settled || int(counts["pending"].(float64)) != inflight.Pending {
		t.Errorf("counts %v and totals %+v must agree (they come from the same snapshot)", counts, inflight)
	}

	// COMPLETE.
	comp := &statusByAddrComputer{pgSettleComputer: pgSettleComputer{now: idsNow}}
	drainAll(t, st, cipher, comp)
	status, done := readTotals(t, srv, "bulk-key", jobID)
	if status["complete"] != true || done.Pending != 0 || done.Settled != 8 || done.Submitted != 8 {
		t.Fatalf("complete: %v %+v", status["complete"], done)
	}
	want := map[string]int{"valid": 2, "invalid": 4, "risky": 1, "unknown": 1} // rows: valid1 x2 (a0, a2); invalid: inv1 x2 (a1, a7) + the 2 malformed rows; risk1; unk1
	for k, n := range want {
		if done.ByStatus[k] != n {
			t.Errorf("by_status[%s] = %d, want %d (%v)", k, done.ByStatus[k], n, done.ByStatus)
		}
	}
	if done.Duplicates != 2 || done.Malformed != 2 {
		t.Errorf("duplicates %d malformed %d, want 2 / 2", done.Duplicates, done.Malformed)
	}
	results := walkResults(t, srv, "bulk-key", jobID, 3)
	if p := totalsProblems(done, results, acc, true); len(p) != 0 {
		t.Fatalf("complete: %v", p)
	}
	if comp.total() != 4 { // valid1, inv1, risk1, unk1: each distinct address once
		t.Errorf("checks run = %d (%v), want 4", comp.total(), comp.calls)
	}

	// PAIRED CONTROL: totalsProblems names each kind of wrong count.
	for name, doctor := range map[string]func(t *totalsView){
		"settled one too high":     func(t *totalsView) { t.Settled++; t.Pending-- },
		"pending that breaks sum":  func(t *totalsView) { t.Pending++ },
		"a by_status count high":   func(t *totalsView) { t.ByStatus["valid"]++ },
		"a by_status count moved":  func(t *totalsView) { t.ByStatus["valid"]--; t.ByStatus["unknown"]++ },
		"a by_status key missing":  func(t *totalsView) { delete(t.ByStatus, "risky") },
		"a by_status key invented": func(t *totalsView) { t.ByStatus["maybe"] = 0 },
		"duplicates not the 202's": func(t *totalsView) { t.Duplicates++ },
		"malformed not the 202's":  func(t *totalsView) { t.Malformed-- },
	} {
		bad := done
		bad.ByStatus = map[string]int{}
		for k, v := range done.ByStatus {
			bad.ByStatus[k] = v
		}
		doctor(&bad)
		if p := totalsProblems(bad, results, acc, true); len(p) == 0 {
			t.Errorf("control: %q was not reported", name)
		}
	}

	// Another tenant reads nothing of it.
	requireEnvelope(t, getAs(t, srv, "bulk-key-b", "/v1/jobs/"+jobID), codeNotFound, http.StatusNotFound)
}

// A store that hands back totals that contradict each other is answered with contract_violation (500), not served; the same snapshot made consistent is
// served. (The identities are checked in the handler because a caller bills on these numbers.)
func TestJobStatus_TotalsThatBreakTheirIdentitiesAreNotServed(t *testing.T) {
	good := store.JobTotals{Submitted: 3, Settled: 2, Pending: 1, Duplicates: 1, Malformed: 1, ByStatus: map[string]int{"valid": 1, "invalid": 1}}
	if err := checkTotals(good); err != nil {
		t.Fatalf("control: consistent totals must pass: %v", err)
	}
	for name, mutate := range map[string]func(t *store.JobTotals){
		"pending does not close the sum":       func(t *store.JobTotals) { t.Pending = 2 },
		"by_status does not add up to settled": func(t *store.JobTotals) { t.ByStatus["valid"] = 2 },
		"a status that is not one of the four": func(t *store.JobTotals) { t.ByStatus["maybe"] = 0 },
		"a negative count":                     func(t *store.JobTotals) { t.Duplicates = -1 },
		"more structural rows than rows":       func(t *store.JobTotals) { t.Duplicates, t.Malformed = 3, 3 },
	} {
		bad := good
		bad.ByStatus = map[string]int{"valid": 1, "invalid": 1}
		mutate(&bad)
		if checkTotals(bad) == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

// ── A1: idempotent create ────────────────────────────────────────────────────────────────────────────────────────────────────

const idemBody = `{"items":[{"item_id":"r0","address":"Jane@AcmeCorp.com"},{"item_id":"r1","address":"bob@acmecorp.com"},{"item_id":"r2","address":"jane@acmecorp.com"},{"item_id":"r3","address":"not-an-address"}],"caller_ref":"platform-job-9"}`

func TestBulkIdempotency_ARepeatIsTheOriginalJob_ByteForByte_AndNothingIsCreatedTwice(t *testing.T) {
	srv, _, cipher, st := idsServer(t, nil)

	first := postKeyed(t, srv, "bulk-key", "retry-me-1", idemBody)
	if first.Code != http.StatusAccepted || first.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("the first answer is a plain 202 with no Idempotent-Replayed header: %d %v", first.Code, first.Header())
	}
	replay := postKeyed(t, srv, "bulk-key", "retry-me-1", idemBody)
	if replay.Code != http.StatusAccepted {
		t.Fatalf("a repeat must answer 202, got %d %s", replay.Code, replay.Body.String())
	}
	if replay.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("a replay must say so: Idempotent-Replayed = %q", replay.Header().Get("Idempotent-Replayed"))
	}
	if !bytes.Equal(first.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatalf("a replay must be the SAME body, byte for byte:\nfirst:  %s\nreplay: %s", first.Body.String(), replay.Body.String())
	}
	if ct := replay.Header().Get("Content-Type"); ct != first.Header().Get("Content-Type") {
		t.Errorf("content types differ: %q vs %q", ct, first.Header().Get("Content-Type"))
	}
	body := jsonMap(t, first.Body.Bytes())
	jobID := body["job_id"].(string)

	// Nothing was created twice: one job's worth of checks (jane, bob) and one listing of 4 rows.
	comp := &statusByAddrComputer{pgSettleComputer: pgSettleComputer{now: idsNow}}
	drainAll(t, st, cipher, comp)
	if comp.total() != 2 {
		t.Fatalf("a replay must create no second set of work items: %d checks ran (%v), want 2", comp.total(), comp.calls)
	}
	if got := walkResults(t, srv, "bulk-key", jobID, 10); len(got) != 4 {
		t.Fatalf("the original job must list its 4 rows, got %d", len(got))
	}
	// …and the repeat is a replay even after the job has completed (the 202's body is reproduced from the job's rows).
	late := postKeyed(t, srv, "bulk-key", "retry-me-1", idemBody)
	if late.Code != http.StatusAccepted || !bytes.Equal(first.Body.Bytes(), late.Body.Bytes()) || late.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("a replay after completion must still be the first answer: %d %s", late.Code, late.Body.String())
	}

	// CONTROLS. Another key is another job. NO key is today's behaviour: every POST makes its own job, with none of the new header.
	other := postKeyed(t, srv, "bulk-key", "retry-me-2", idemBody)
	if other.Code != http.StatusAccepted || jsonMap(t, other.Body.Bytes())["job_id"] == jobID || other.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("control: another key must make another job: %d %s", other.Code, other.Body.String())
	}
	n1, n2 := postKeyed(t, srv, "bulk-key", "", idemBody), postKeyed(t, srv, "bulk-key", "", idemBody)
	id1, id2 := jsonMap(t, n1.Body.Bytes())["job_id"], jsonMap(t, n2.Body.Bytes())["job_id"]
	if n1.Code != http.StatusAccepted || n2.Code != http.StatusAccepted || id1 == id2 || id1 == jobID || n1.Header().Get("Idempotent-Replayed") != "" || n2.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("control: without the header every POST makes its own job: %v %v", n1.Body.String(), n2.Body.String())
	}
}

// Without the header the 202 is exactly what it was before: the same six keys, the same counts, no new key and no new header.
func TestBulkIdempotency_NoHeaderIsTodaysBehaviour_ByteForByte(t *testing.T) {
	srv, _, _, _ := idsServer(t, nil)
	rec := postKeyed(t, srv, "bulk-key", "", idemBody)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	got := jsonMap(t, rec.Body.Bytes())
	jobID := got["job_id"].(string)
	want := fmt.Sprintf(`{"counts":{"duplicate":1,"enqueued":2,"malformed":1,"total":4},"job_id":%q,"malformed_rows":[{"ordinal":3,"item_id":"r3"}],"malformed_rows_truncated":false,"processing_paused":false,"status":"accepted"}`+"\n", jobID)
	if rec.Body.String() != want {
		t.Fatalf("the unkeyed 202 changed:\n got: %s\nwant: %s", rec.Body.String(), want)
	}
	for h := range rec.Header() {
		if strings.EqualFold(h, "Idempotent-Replayed") {
			t.Errorf("an unkeyed answer must carry no Idempotent-Replayed header")
		}
	}
	// And the keyed FIRST answer has the same body shape (only the job id differs).
	keyed := postKeyed(t, srv, "bulk-key", "k-first", idemBody)
	kj := jsonMap(t, keyed.Body.Bytes())["job_id"].(string)
	if keyed.Body.String() != strings.ReplaceAll(want, jobID, kj) {
		t.Errorf("a keyed first answer must be the same body as an unkeyed one:\n got: %s\nwant: %s", keyed.Body.String(), strings.ReplaceAll(want, jobID, kj))
	}
}

// The same key with ANOTHER request is a 409 with its own code and the envelope; nothing is created (a drain finds only the original's work) and the key
// still names the original job.
func TestBulkIdempotency_TheSameKeyWithAnotherBodyIs409_AndCreatesNothing(t *testing.T) {
	srv, _, cipher, st := idsServer(t, nil)
	first := postKeyed(t, srv, "bulk-key", "the-key", idemBody)
	jobID := jsonMap(t, first.Body.Bytes())["job_id"].(string)
	changed := strings.Replace(idemBody, "bob@acmecorp.com", "carol@acmecorp.com", 1)
	rec := postKeyed(t, srv, "bulk-key", "the-key", changed)
	env := requireEnvelope(t, rec, codeIdempotencyKeyReused, http.StatusConflict)
	details, _ := env["details"].([]any)
	if len(details) != 1 || details[0].(map[string]any)["field"] != "Idempotency-Key" || details[0].(map[string]any)["issue"] != issueConflict {
		t.Errorf("details = %v, want [{field:Idempotency-Key, issue:conflict}]", details)
	}
	if strings.Contains(rec.Body.String(), jobID) {
		t.Error("a refusal must not disclose the other job's id")
	}
	if rec.Header().Get("Idempotent-Replayed") != "" {
		t.Error("a refusal is not a replay")
	}
	comp := &statusByAddrComputer{pgSettleComputer: pgSettleComputer{now: idsNow}}
	drainAll(t, st, cipher, comp)
	if comp.total() != 2 {
		t.Fatalf("a refused request must create nothing: %d checks ran (%v), want only the original's 2", comp.total(), comp.calls)
	}
	// CONTROL: the original body under the key is still a replay, so the 409 above was about the body.
	if again := postKeyed(t, srv, "bulk-key", "the-key", idemBody); again.Code != http.StatusAccepted || again.Header().Get("Idempotent-Replayed") != "true" ||
		jsonMap(t, again.Body.Bytes())["job_id"] != jobID {
		t.Fatalf("control: the unchanged body must still replay: %d %s", again.Code, again.Body.String())
	}
}

// One caller's key is invisible to another: the same key and the same body from two callers are two jobs, and each caller's repeat is a replay of ITS
// OWN job. (The key is scoped to the authenticated caller: the credential, never anything in the body.)
func TestBulkIdempotency_TheKeyOfOneCallerIsInvisibleToAnother(t *testing.T) {
	srv, _, _, _ := idsServer(t, nil)
	a := postKeyed(t, srv, "bulk-key", "shared", idemBody)
	b := postKeyed(t, srv, "bulk-key-b", "shared", idemBody)
	if a.Code != http.StatusAccepted || b.Code != http.StatusAccepted || b.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("the same key from another caller must create its own job: %d %d (replayed=%q)", a.Code, b.Code, b.Header().Get("Idempotent-Replayed"))
	}
	ja, jb := jsonMap(t, a.Body.Bytes())["job_id"], jsonMap(t, b.Body.Bytes())["job_id"]
	if ja == jb {
		t.Fatal("two callers with one key must have two jobs")
	}
	a2, b2 := postKeyed(t, srv, "bulk-key", "shared", idemBody), postKeyed(t, srv, "bulk-key-b", "shared", idemBody)
	if jsonMap(t, a2.Body.Bytes())["job_id"] != ja || jsonMap(t, b2.Body.Bytes())["job_id"] != jb || a2.Header().Get("Idempotent-Replayed") != "true" || b2.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("each caller's repeat must replay its own job: %s / %s", a2.Body.String(), b2.Body.String())
	}
	// And each reads only its own.
	requireEnvelope(t, getAs(t, srv, "bulk-key-b", "/v1/jobs/"+ja.(string)), codeNotFound, http.StatusNotFound)
}

// The Idempotency-Key header: 1..128 characters of [A-Za-z0-9._:-], exactly one value. Anything else is a 400 with the envelope BEFORE the body is
// read (a request with a bad key and an unreadable body is the key's 400), and creates nothing. The boundary values are the controls.
func TestBulkIdempotency_TheKeyHeaderIsValidated(t *testing.T) {
	srv, _, cipher, st := idsServer(t, nil)
	long128, long129 := strings.Repeat("k", 128), strings.Repeat("k", 129)
	bad := map[string]string{
		"a space": "a b", "a slash": "a/b", "a plus": "a+b", "an equals sign": "a=b", "a comma": "a,b", "a quote": `a"b`, "a backslash": `a\b`,
		"non-ASCII": "café", "129 characters": long129, "a semicolon": "a;b", "an at sign": "a@b", "a percent sign": "a%41",
	}
	for name, key := range bad {
		t.Run(name, func(t *testing.T) {
			rec := postKeyed(t, srv, "bulk-key", key, idemBody)
			env := requireEnvelope(t, rec, codeValidationFailed, http.StatusBadRequest)
			details, _ := env["details"].([]any)
			if len(details) != 1 || details[0].(map[string]any)["field"] != "Idempotency-Key" || details[0].(map[string]any)["issue"] != issueMalformed {
				t.Errorf("details = %v, want [{field:Idempotency-Key, issue:malformed}]", details)
			}
		})
	}
	t.Run("an empty value", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/verify/bulk", strings.NewReader(idemBody))
		req.Header.Set("Authorization", "Bearer bulk-key")
		req.Header["Idempotency-Key"] = []string{""}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		requireEnvelope(t, rec, codeValidationFailed, http.StatusBadRequest)
	})
	t.Run("the header given twice", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/verify/bulk", strings.NewReader(idemBody))
		req.Header.Set("Authorization", "Bearer bulk-key")
		req.Header.Add("Idempotency-Key", "one")
		req.Header.Add("Idempotency-Key", "two")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		requireEnvelope(t, rec, codeValidationFailed, http.StatusBadRequest)
	})
	t.Run("a bad key beats an unreadable body", func(t *testing.T) {
		env := requireEnvelope(t, postKeyed(t, srv, "bulk-key", "a b", `{not json`), codeValidationFailed, http.StatusBadRequest)
		if details, _ := env["details"].([]any); len(details) != 1 || details[0].(map[string]any)["field"] != "Idempotency-Key" {
			t.Errorf("details = %v, want the key's", details)
		}
	})
	t.Run("a bad key needs authentication first", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/verify/bulk", strings.NewReader(idemBody))
		req.Header.Set("Idempotency-Key", "a b")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		requireEnvelope(t, rec, codeUnauthenticated, http.StatusUnauthorized)
	})
	// Nothing above created a job.
	comp := &statusByAddrComputer{pgSettleComputer: pgSettleComputer{now: idsNow}}
	drainAll(t, st, cipher, comp)
	if comp.total() != 0 {
		t.Fatalf("a refused key must create nothing, %d check(s) ran", comp.total())
	}
	// CONTROLS: the boundary and the whole alphabet are accepted.
	for name, key := range map[string]string{"128 characters": long128, "one character": "k", "the whole alphabet": "AZaz09._:-", "a uuid": "019fd000-0000-7000-8000-000000000001"} {
		if rec := postKeyed(t, srv, "bulk-key", key, `{"addresses":["x@acmecorp.com"]}`); rec.Code != http.StatusAccepted {
			t.Errorf("control: %s must be accepted, got %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

// Idempotency is a property of the BULK create only: the header on the other routes changes nothing (the sync verify keeps working with or without it).
func TestBulkIdempotency_TheHeaderIsIgnoredOnEveryOtherRoute(t *testing.T) {
	srv, _, _, _ := idsServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/describe", nil)
	req.Header.Set("Authorization", "Bearer bulk-key")
	req.Header.Set("Idempotency-Key", "a b") // invalid, and irrelevant here
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("describe with a stray Idempotency-Key header: %d %s", rec.Code, rec.Body.String())
	}
}

// ── A1: describe ─────────────────────────────────────────────────────────────────────────────────────────────────────────────

func getDescribe(t *testing.T, srv *Server, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/describe", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestDescribe_RequiresAPerCallerKey(t *testing.T) {
	srv, _, _, _ := idsServer(t, nil)
	requireEnvelope(t, getDescribe(t, srv, ""), codeUnauthenticated, http.StatusUnauthorized)
	requireEnvelope(t, getDescribe(t, srv, "no-such-key"), codeUnauthenticated, http.StatusUnauthorized)
	if rec := getDescribe(t, srv, "bulk-key"); rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("control: a valid per-caller key must read describe: %d %s", rec.Code, rec.Body.String())
	}
	// The legacy static token authenticates the sync verify only; describe, like the bulk and job routes, refuses it.
	cfg := config.Defaults()
	cfg.Env = "production"
	sec, _ := secrets.Load(fakeSource{secrets.EnvFrontDoorKey: "legacy-static"}, false)
	mem := store.NewMem(nil)
	legacy := New(cfg, sec, mustHasher(t)).WithCallerAuth(mem)
	requireEnvelope(t, getDescribe(t, legacy, "legacy-static"), codeUnauthenticated, http.StatusUnauthorized)
	// A server with no caller auth wired answers 401 too (never an open describe).
	requireEnvelope(t, getDescribe(t, unwiredServer(t), "anything"), codeUnauthenticated, http.StatusUnauthorized)
}

func describeBodyOf(t *testing.T, srv *Server) map[string]any {
	t.Helper()
	rec := getDescribe(t, srv, "bulk-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("describe: %d %s", rec.Code, rec.Body.String())
	}
	return jsonMap(t, rec.Body.Bytes())
}

// The shape family of modules 2 and 3: module, api_version, capabilities, routes (id/method/path/auth/description/schema refs), schemas,
// vocabularies{name: {source, values, fields}} and error_codes[{code, http_status}] sorted by status then code.
func TestDescribe_IsInTheShapeFamilyOfModules2And3(t *testing.T) {
	srv, _, _, _ := idsServer(t, nil)
	d := describeBodyOf(t, srv)
	if d["module"] != "verify" || d["api_version"] != "v1" {
		t.Fatalf("module/api_version = %v/%v, want verify/v1", d["module"], d["api_version"])
	}
	for _, k := range []string{"capabilities", "routes", "schemas", "vocabularies", "error_codes", "idempotency"} {
		if d[k] == nil {
			t.Errorf("describe has no %q", k)
		}
	}
	caps := d["capabilities"].(map[string]any)
	for _, k := range []string{"sync_verify", "bulk_verify", "idempotent_bulk_create", "job_totals"} {
		if caps[k] != true {
			t.Errorf("capabilities.%s = %v, want true", k, caps[k])
		}
	}
	for _, k := range []string{"quote", "run", "report"} { // honest: module 1 has no such verbs, and says so
		if caps[k] != false {
			t.Errorf("capabilities.%s = %v, want false (no verb parity was built)", k, caps[k])
		}
	}
	for name, v := range d["vocabularies"].(map[string]any) {
		e := v.(map[string]any)
		vals, _ := e["values"].([]any)
		fields, _ := e["fields"].([]any)
		if e["source"] != "module_constant" || len(vals) == 0 || len(fields) == 0 {
			t.Errorf("vocabulary %s = %v, want {source: module_constant, values: [..], fields: [..]}", name, e)
		}
	}
	prev := 0
	prevCode := ""
	for _, c := range d["error_codes"].([]any) {
		m := c.(map[string]any)
		st, code := int(m["http_status"].(float64)), m["code"].(string)
		if st < prev || (st == prev && code <= prevCode) {
			t.Errorf("error_codes must be sorted by status then code: %s/%d after %s/%d", code, st, prevCode, prev)
		}
		prev, prevCode = st, code
	}
	idem := d["idempotency"].(map[string]any)
	if idem["header"] != "Idempotency-Key" || idem["replay_response_header"] != "Idempotent-Replayed" || idem["reused_with_other_request"] != codeIdempotencyKeyReused {
		t.Errorf("idempotency = %v", idem)
	}
}

// describeVsCode compares a describe body with the code's OWN vocabularies and codes and returns every difference. It is the check and, run on a
// doctored body or over a source with a value removed, the paired control.
func describeVsCode(d map[string]any) []string {
	var p []string
	strs := func(n int, at func(int) string) []string {
		o := make([]string, n)
		for i := range o {
			o[i] = at(i)
		}
		return o
	}
	st, rc, or, jb, re := vocab.Statuses(), vocab.Recommendations(), vocab.FailureOrigins(), vocab.JobStatuses(), vocab.Reasons()
	truth := map[string][]string{
		"status":              strs(len(st), func(i int) string { return string(st[i]) }),
		"reason":              strs(len(re), func(i int) string { return string(re[i]) }),
		"recommendation":      strs(len(rc), func(i int) string { return string(rc[i]) }),
		"failure_origin":      strs(len(or), func(i int) string { return string(or[i]) }),
		"smtp_skipped_reason": vocab.SkipTokens(),
		"job_status":          strs(len(jb), func(i int) string { return string(jb[i]) }),
		"submit_status":       {vocab.SubmitAccepted},
		"detail_issue":        {issueRequired, issueMalformed, issueOutOfRange, issueNotFound, issueConflict, issueDuplicate},
	}
	vocabs, _ := d["vocabularies"].(map[string]any)
	for name, want := range truth {
		e, ok := vocabs[name].(map[string]any)
		if !ok {
			p = append(p, "the vocabulary "+name+" is not described")
			continue
		}
		got := map[string]bool{}
		for _, v := range e["values"].([]any) {
			got[v.(string)] = true
		}
		for _, w := range want {
			if !got[w] {
				p = append(p, fmt.Sprintf("%s: the code has %q and describe does not", name, w))
			}
		}
		if len(got) != len(want) {
			p = append(p, fmt.Sprintf("%s: describe lists %d value(s), the code has %d", name, len(got), len(want)))
		}
	}
	for name := range vocabs {
		if _, ok := truth[name]; !ok {
			p = append(p, "describe publishes a vocabulary the code does not declare: "+name)
		}
	}
	// error_codes is the codeStatus map.
	got := map[string]int{}
	for _, c := range d["error_codes"].([]any) {
		m := c.(map[string]any)
		got[m["code"].(string)] = int(m["http_status"].(float64))
	}
	for code, status := range codeStatus {
		if g, ok := got[code]; !ok || g != status {
			p = append(p, fmt.Sprintf("error code %s: the code says %d, describe says %v", code, status, g))
		}
	}
	if len(got) != len(codeStatus) {
		p = append(p, fmt.Sprintf("describe lists %d error code(s), the code emits %d", len(got), len(codeStatus)))
	}
	return p
}

func TestDescribe_VocabulariesAndErrorCodesEqualTheCodesOwn(t *testing.T) {
	srv, _, _, _ := idsServer(t, nil)
	d := describeBodyOf(t, srv)
	if p := describeVsCode(d); len(p) != 0 {
		t.Fatalf("describe differs from the code: %v", p)
	}
	// The frozen sets are pinned beside the derivation, so a change to the frozen doors cannot ride through a derived list unnoticed.
	pin := func(name string, want ...string) {
		t.Helper()
		var got []string
		for _, v := range d["vocabularies"].(map[string]any)[name].(map[string]any)["values"].([]any) {
			got = append(got, v.(string))
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s = %v, want %v (Door 4: additive-only, and this set is frozen)", name, got, want)
		}
	}
	pin("status", "valid", "invalid", "risky", "unknown")
	pin("recommendation", "send", "review", "skip")
	pin("failure_origin", "own", "remote", "undetermined")
	pin("job_status", "submitted", "running", "complete", "complete_with_unknowns")
	pin("submit_status", "accepted")

	// PAIRED CONTROLS. (1) The comparison is red on a doctored body, for each kind of difference.
	doctor := func(f func(d map[string]any)) []string {
		clone := jsonMap(t, mustJSON(t, d))
		f(clone)
		return describeVsCode(clone)
	}
	dropValue := func(vocabName string) func(d map[string]any) {
		return func(d map[string]any) {
			e := d["vocabularies"].(map[string]any)[vocabName].(map[string]any)
			vals := e["values"].([]any)
			e["values"] = vals[:len(vals)-1]
		}
	}
	for _, name := range []string{"status", "reason", "recommendation", "failure_origin", "smtp_skipped_reason", "job_status", "submit_status", "detail_issue"} {
		if p := doctor(dropValue(name)); len(p) == 0 {
			t.Errorf("control: a value removed from %s was not reported", name)
		}
	}
	if p := doctor(func(d map[string]any) { delete(d["vocabularies"].(map[string]any), "reason") }); len(p) == 0 {
		t.Error("control: a missing vocabulary was not reported")
	}
	if p := doctor(func(d map[string]any) {
		d["vocabularies"].(map[string]any)["invented"] = map[string]any{"values": []any{}}
	}); len(p) == 0 {
		t.Error("control: an invented vocabulary was not reported")
	}
	if p := doctor(func(d map[string]any) { d["error_codes"] = d["error_codes"].([]any)[1:] }); len(p) == 0 {
		t.Error("control: a missing error code was not reported")
	}
	// (2) The control at the SOURCE: the handler's own vocabulary source with a value removed makes the real endpoint differ from the code.
	original := describedVocabularies
	t.Cleanup(func() { describedVocabularies = original })
	describedVocabularies = func() []vocabEntry {
		src := original()
		for i := range src {
			if src[i].Name == "status" {
				src[i].Values = src[i].Values[:len(src[i].Values)-1]
			}
		}
		return src
	}
	if p := describeVsCode(describeBodyOf(t, srv)); len(p) == 0 {
		t.Error("control: with a status removed from the describe source the endpoint must differ from the code, and the comparison must say so")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// routeDrift compares the three views of the route set: the served table, the meta table and the described list. Both directions, so a route cannot
// be served undescribed and a description cannot outlive its route.
func routeDrift(served []Route, metas map[string]routeMeta, described []any) []string {
	var p []string
	servedSet := map[string]bool{}
	for _, rt := range served {
		servedSet[rt.Pattern] = true
		if _, ok := metas[rt.Pattern]; !ok {
			p = append(p, "SERVED BUT NOT DESCRIBED (no routeMetas entry): "+rt.Pattern)
		}
	}
	for pat := range metas {
		if !servedSet[pat] {
			p = append(p, "DESCRIBED BUT NOT SERVED (a routeMetas entry for no route): "+pat)
		}
	}
	listed := map[string]bool{}
	for _, r := range described {
		m := r.(map[string]any)
		pat := m["method"].(string) + " " + m["path"].(string)
		listed[pat] = true
		if !servedSet[pat] {
			p = append(p, "LISTED BY describe BUT NOT SERVED: "+pat)
		}
		if m["id"] == nil || m["auth"] == nil || m["description"] == nil {
			p = append(p, "a described route lacks id/auth/description: "+pat)
		}
	}
	for pat := range servedSet {
		if !listed[pat] {
			p = append(p, "SERVED BUT NOT LISTED by describe: "+pat)
		}
	}
	return p
}

func TestDescribe_EveryServedRouteIsDescribedAndNothingElse(t *testing.T) {
	srv, _, _, _ := idsServer(t, nil)
	d := describeBodyOf(t, srv)
	described := d["routes"].([]any)
	if len(described) == 0 || len(srv.Routes()) == 0 {
		t.Fatal("an empty route set would pass vacuously")
	}
	if p := routeDrift(srv.Routes(), routeMetas, described); len(p) != 0 {
		t.Fatalf("describe and the route table have drifted: %v", p)
	}
	if len(described) != len(srv.Routes()) {
		t.Fatalf("describe lists %d route(s), the router serves %d", len(described), len(srv.Routes()))
	}
	// Every schema ref a route names exists in schemas.
	schemas := d["schemas"].(map[string]any)
	for _, r := range described {
		m := r.(map[string]any)
		for _, k := range []string{"request_schema_ref", "response_schema_ref"} {
			if ref, ok := m[k].(string); ok {
				if name := strings.TrimPrefix(ref, "#/schemas/"); schemas[name] == nil {
					t.Errorf("route %v names %s = %q, which is not in schemas", m["id"], k, ref)
				}
			}
		}
	}
	// And the router really answers each described route (not a 404): the table and the mux cannot differ (TestRoutes_HandlerIsALoopOverRoutes holds the
	// structural half; this is the behavioural one).
	h := srv.Handler()
	for _, r := range described {
		m := r.(map[string]any)
		path := strings.ReplaceAll(m["path"].(string), "{id}", "019fd000-0000-7000-8000-000000000001")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(m["method"].(string), path, nil))
		if rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), "no route for") {
			t.Errorf("describe lists %v %v but the router has no such route", m["method"], m["path"])
		}
	}

	// PAIRED CONTROLS: the drift check is red for each kind of drift.
	extra := append(append([]Route(nil), srv.Routes()...), Route{Pattern: "GET /v1/secret"})
	if len(routeDrift(extra, routeMetas, described)) == 0 {
		t.Error("control: a served route with no description was not reported")
	}
	trimmed := map[string]routeMeta{}
	for k, v := range routeMetas {
		if k != "GET /v1/describe" {
			trimmed[k] = v
		}
	}
	if len(routeDrift(srv.Routes(), trimmed, described)) == 0 {
		t.Error("control: a missing routeMetas entry was not reported")
	}
	stale := map[string]routeMeta{"GET /v1/gone": {ID: "gone"}}
	for k, v := range routeMetas {
		stale[k] = v
	}
	if len(routeDrift(srv.Routes(), stale, described)) == 0 {
		t.Error("control: a description for a route that is not served was not reported")
	}
	if len(routeDrift(srv.Routes(), routeMetas, described[1:])) == 0 {
		t.Error("control: a route missing from describe's list was not reported")
	}
	ghost := append(append([]any(nil), described...), map[string]any{"method": "GET", "path": "/v1/ghost", "id": "g", "auth": "x", "description": "d"})
	if len(routeDrift(srv.Routes(), routeMetas, ghost)) == 0 {
		t.Error("control: a listed route that is not served was not reported")
	}
}

// The api version is derived from the served versioned routes; a table whose versioned routes disagree (or that has none) names no version, and the
// handler never guesses one.
func TestDescribe_APIVersionIsDerivedFromTheServedRoutes(t *testing.T) {
	srv, _, _, _ := idsServer(t, nil)
	if v := apiVersionOf(srv.Routes()); v != "v1" {
		t.Fatalf("apiVersionOf(shipped table) = %q, want v1", v)
	}
	r := func(p string) Route { return Route{Pattern: p} }
	for name, tc := range map[string]struct {
		rs   []Route
		want string
	}{
		"two versions served":       {[]Route{r("GET /v1/a"), r("GET /v2/b")}, ""},
		"only unversioned routes":   {[]Route{r("GET /healthz"), r("GET /readyz")}, ""},
		"an empty table":            {nil, ""},
		"a v2 table":                {[]Route{r("GET /v2/a"), r("POST /v2/b/{id}"), r("GET /healthz")}, "v2"},
		"a path that merely has v1": {[]Route{r("GET /verify/v1"), r("GET /v3/x")}, "v3"},
	} {
		if got := apiVersionOf(tc.rs); got != tc.want {
			t.Errorf("%s: apiVersionOf = %q, want %q", name, got, tc.want)
		}
	}
}

// No secret in the body: the server is wired with RUN-TIME-GENERATED canaries for every secret it holds (pepper, front-door token, queue key, a node
// token, a caller key) and the describe body must contain none of them, no bearer scheme and no connection string. The scanner is itself checked: it
// finds a canary planted in a body.
func TestDescribe_HoldsNoSecret(t *testing.T) {
	canary := func() string {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		return hex.EncodeToString(b)
	}
	pepper, front, queueKey, node, callerKey := canary()+canary(), canary(), canary()+canary(), canary(), canary()
	cfg := config.Defaults()
	cfg.Env = "production"
	sec, err := secrets.Load(fakeSource{
		secrets.EnvPepper: pepper, secrets.EnvFrontDoorKey: front, secrets.EnvQueueAddrKey: queueKey, secrets.EnvNodeTokens: "n1=" + node,
	}, false)
	if err != nil {
		t.Fatalf("secrets: %v", err)
	}
	mem := store.NewMem(nil)
	mem.SeedCallerWithUser(sha(callerKey), "caller_s", bulkOwner, idsNow.Add(-time.Hour), time.Time{})
	srv := New(cfg, sec, mustHasher(t)).WithCallerAuth(mem)
	srv.now = func() time.Time { return idsNow }
	rec := getDescribe(t, srv, callerKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("describe: %d %s", rec.Code, rec.Body.String())
	}
	leaks := func(body string) []string {
		var out []string
		for name, s := range map[string]string{"pepper": pepper, "front-door token": front, "queue key": queueKey, "node token": node, "caller key": callerKey} {
			if strings.Contains(body, s) {
				out = append(out, name)
			}
		}
		for _, frag := range []string{"Bearer ", "postgres://", "postgresql://", "password", "TM_PEPPER", "TM_QUEUE_ADDR_KEY"} {
			if strings.Contains(body, frag) {
				out = append(out, "the fragment "+frag)
			}
		}
		return out
	}
	if l := leaks(rec.Body.String()); len(l) != 0 {
		t.Fatalf("describe leaks: %v", l)
	}
	if l := leaks(rec.Body.String() + front); len(l) == 0 {
		t.Fatal("control: the scanner must find a planted front-door token")
	}
	if l := leaks(rec.Body.String() + " Bearer x"); len(l) == 0 {
		t.Fatal("control: the scanner must find a planted bearer scheme")
	}
}

// Every schema describe publishes is checked against a REAL response of the real handler: each key the response has is in the schema's properties, and
// each key the schema requires is in the response. A handler that gains a key without the schema (or loses one the schema requires) is red.
func TestDescribe_SchemasMatchRealResponses(t *testing.T) {
	srv, _, cipher, st := idsServer(t, nil)
	schemas := describeBodyOf(t, srv)["schemas"].(map[string]any)

	check := func(name string, body []byte) []string {
		var p []string
		sch, ok := schemas[name].(map[string]any)
		if !ok {
			return []string{"no schema " + name}
		}
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			return []string{name + ": the response is not a JSON object: " + err.Error()}
		}
		props, _ := sch["properties"].(map[string]any)
		for k := range obj {
			if _, ok := props[k]; !ok {
				p = append(p, fmt.Sprintf("%s: the response has the key %q and the schema does not", name, k))
			}
		}
		req, _ := sch["required"].([]any)
		for _, r := range req {
			if _, ok := obj[r.(string)]; !ok {
				p = append(p, fmt.Sprintf("%s: the schema requires %q and the response lacks it", name, r))
			}
		}
		// One level down for the nested objects the contract spells out (counts, totals, error).
		for k, v := range obj {
			sub, isObj := v.(map[string]any)
			subSch, _ := props[k].(map[string]any)
			subProps, _ := subSch["properties"].(map[string]any)
			if !isObj || subProps == nil {
				continue
			}
			for sk := range sub {
				if _, ok := subProps[sk]; !ok {
					p = append(p, fmt.Sprintf("%s.%s: the response has the key %q and the schema does not", name, k, sk))
				}
			}
			subReq, _ := subSch["required"].([]any)
			for _, r := range subReq {
				if _, ok := sub[r.(string)]; !ok {
					p = append(p, fmt.Sprintf("%s.%s: the schema requires %q and the response lacks it", name, k, r))
				}
			}
		}
		return p
	}

	acc := postBulkAs(t, srv, "bulk-key", totalsBody)
	jobID := jsonMap(t, acc.Body.Bytes())["job_id"].(string)
	drainAll(t, st, cipher, &statusByAddrComputer{pgSettleComputer: pgSettleComputer{now: idsNow}})
	statusRec := getAs(t, srv, "bulk-key", "/v1/jobs/"+jobID)
	resRec := getAs(t, srv, "bulk-key", "/v1/jobs/"+jobID+"/results")
	errRec := getAs(t, srv, "bulk-key", "/v1/jobs/not-a-uuid")
	hz := httptest.NewRecorder()
	srv.Handler().ServeHTTP(hz, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	rz := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rz, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	real := map[string][]byte{
		"bulk_accepted_response": acc.Body.Bytes(), "job_status_response": statusRec.Body.Bytes(), "job_results_response": resRec.Body.Bytes(),
		"error_response": errRec.Body.Bytes(), "healthz_response": hz.Body.Bytes(), "readyz_response": rz.Body.Bytes(),
	}
	for name, body := range real {
		if p := check(name, body); len(p) != 0 {
			t.Errorf("%v", p)
		}
	}
	// A results element and a verdict: the first element of the real listing against the element schema, a real sync verdict against the verdict one.
	var page struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(resRec.Body.Bytes(), &page); err != nil || len(page.Results) == 0 {
		t.Fatalf("results: %v / %d element(s)", err, len(page.Results))
	}
	elementSchema := schemas["job_results_response"].(map[string]any)["properties"].(map[string]any)["results"].(map[string]any)["items"].(map[string]any)
	schemas["__element"] = elementSchema
	if p := check("__element", page.Results[0]); len(p) != 0 {
		t.Errorf("a result element differs from its schema: %v", p)
	}
	for _, name := range []string{"verify_request", "bulk_request"} {
		if schemas[name] == nil {
			t.Errorf("no request schema %s", name)
		}
	}
	vsrv := wiredServer(t)
	vrec := httptest.NewRecorder()
	vreq := httptest.NewRequest(http.MethodPost, "/v1/verify", strings.NewReader(`{"email":"jane@acmecorp.com"}`))
	vsrv.Handler().ServeHTTP(vrec, vreq)
	if vrec.Code != http.StatusOK {
		t.Fatalf("sync verify: %d %s", vrec.Code, vrec.Body.String())
	}
	if p := check("verdict", vrec.Body.Bytes()); len(p) != 0 {
		t.Errorf("a sync verdict differs from the verdict schema: %v", p)
	}

	// PAIRED CONTROLS: the comparison is red when the response gains a key the schema lacks, and when it loses a required one.
	extra := jsonMap(t, statusRec.Body.Bytes())
	extra["a_new_key"] = 1
	if p := check("job_status_response", mustJSON(t, extra)); len(p) == 0 {
		t.Error("control: a response key missing from the schema was not reported")
	}
	lost := jsonMap(t, statusRec.Body.Bytes())
	delete(lost, "totals")
	if p := check("job_status_response", mustJSON(t, lost)); len(p) == 0 {
		t.Error("control: a required key missing from the response was not reported")
	}
	nested := jsonMap(t, statusRec.Body.Bytes())
	nested["totals"].(map[string]any)["surprise"] = 1
	if p := check("job_status_response", mustJSON(t, nested)); len(p) == 0 {
		t.Error("control: a nested response key missing from the schema was not reported")
	}
}

// ── the listing is stable once the job is complete ──────────────────────────────────────────────────────────────────────────

// Docs/api-contract.md: the listing of a job's results is stable from the moment `complete` is true: two full walks, whatever the page size, return the
// SAME rows in the SAME order with the same verdicts. While the job runs it is NOT (a row can settle behind a cursor already issued): the control walks a
// job in flight, lets the drainer finish, walks again, and the two differ.
func TestJobResults_TheListingIsStableOnceTheJobIsComplete(t *testing.T) {
	srv, _, cipher, st := idsServer(t, nil)
	acc := mustAccept(t, postBulkAs(t, srv, "bulk-key", totalsBody))
	jobID := acc["job_id"].(string)

	inflight := walkResults(t, srv, "bulk-key", jobID, 3) // only the malformed rows exist yet
	drainAll(t, st, cipher, &statusByAddrComputer{pgSettleComputer: pgSettleComputer{now: idsNow}})
	if status, _ := readTotals(t, srv, "bulk-key", jobID); status["complete"] != true {
		t.Fatalf("the job must be complete now: %v", status["complete"])
	}
	first := walkResults(t, srv, "bulk-key", jobID, 3)
	second := walkResults(t, srv, "bulk-key", jobID, 3)
	third := walkResults(t, srv, "bulk-key", jobID, 1000)
	asJSON := func(rows []map[string]any) string { return string(mustJSON(t, rows)) }
	if len(first) != 8 {
		t.Fatalf("a complete listing holds every row: %d, want 8", len(first))
	}
	if asJSON(first) != asJSON(second) {
		t.Fatalf("two full walks after completion must be identical:\n%s\n%s", asJSON(first), asJSON(second))
	}
	if asJSON(first) != asJSON(third) {
		t.Fatalf("the listing must not depend on the page size:\n%s\n%s", asJSON(first), asJSON(third))
	}
	for i, r := range first {
		if int(r["ordinal"].(float64)) != i {
			t.Errorf("row %d has ordinal %v: a complete listing is every ordinal once, in order", i, r["ordinal"])
		}
	}
	// CONTROL: before completion the listing was different (smaller): "stable" is a property of COMPLETE, not of every moment.
	if asJSON(inflight) == asJSON(first) || len(inflight) >= len(first) {
		t.Fatalf("control: the in-flight listing (%d rows) must differ from the complete one (%d rows)", len(inflight), len(first))
	}
}

// isUUID accepts the canonical 8-4-4-4-12 form in either case of hex and nothing else (Postgres compares uuids, not strings, so an upper-case id finds
// the job there; the in-memory store keys by string, which is why the HTTP controls above use the issued lower-case id).
func TestIsUUID(t *testing.T) {
	for _, ok := range []string{
		"019fd000-0000-7000-8000-000000000001", "019FD000-0000-7000-8000-000000000001", "00000000-0000-0000-0000-000000000000",
		"ffffffff-ffff-ffff-ffff-ffffffffffff", "AbCdEf01-2345-6789-aBcD-eF0123456789",
	} {
		if !isUUID(ok) {
			t.Errorf("%q is a UUID", ok)
		}
	}
	for _, bad := range []string{
		"", "job-1", "019fd000000070008000000000000001", "{019fd000-0000-7000-8000-000000000001}", "019fd000-0000-7000-8000-00000000000",
		"019fd000-0000-7000-8000-0000000000010", "019fd000_0000-7000-8000-000000000001", "019fd000-0000-7000-8000-00000000000g",
		" 019fd000-0000-7000-8000-000000000001", "019fd000-0000-7000-8000-000000000001\n", "019fd0000-000-7000-8000-000000000001",
	} {
		if isUUID(bad) {
			t.Errorf("%q is not a UUID", bad)
		}
	}
}
