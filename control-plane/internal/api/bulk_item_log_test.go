package api

// HTTP-level tests of the two follow-ups to the bulk row identity work (see bulk_item_identity_test.go for the fixtures):
//
//   - DOOR 2: a malformed row is LOGGED before the 202 is returned (one LOG row each, the sync path's invalid_syntax row) and its result is
//     SERVED FROM that LOG row, not made up at read time;
//   - DUPLICATES ARE VISIBLE: a row that repeats an earlier address says which row it shares (`duplicate_of`, the ordinal of the FIRST such row),
//     so the platform can charge a duplicated address once; a repeated malformed address is not a duplicate.
//
// Each has a paired control: the very same checker run over the old behaviour (no log written for a malformed row; duplicate_of not served) must
// report a violation.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/coldmail-verify/control-plane/internal/store"
)

// apiNoLogStore is the OLD behaviour of a bulk submission as a store: a malformed row was neither logged nor recorded, so it vanished from the
// results. RecordJob drops every malformed row and every LOG row before delegating.
type apiNoLogStore struct{ *store.MemStore }

func (n apiNoLogStore) RecordJob(ctx context.Context, job store.JobRow, rows []store.JobRowEntry, _ []store.VerificationRow) error {
	var kept []store.JobRowEntry
	for _, r := range rows {
		if !r.Malformed {
			kept = append(kept, r)
		}
	}
	return n.MemStore.RecordJob(ctx, job, kept, nil)
}

// malformedLoggedViolations posts `body` (a bulk request holding exactly `wantMalformed` malformed rows) and judges Door 2 over HTTP: the LOG holds
// exactly one new row per malformed row by the time the 202 is back, each the sync path's invalid_syntax row with NO email_hash and from_cache
// false; and every malformed row is in the results (they need no check) EQUAL to its LOG row — status, reason, confidence, recommendation, flags,
// signals — in ordinal order. It returns what is wrong.
func malformedLoggedViolations(t *testing.T, srv *Server, mem *store.MemStore, body string, wantMalformed int) []string {
	t.Helper()
	before := len(mem.Log)
	resp := mustAccept(t, postBulkAs(t, srv, "bulk-key", body))
	jobID, _ := resp["job_id"].(string)
	var v []string
	logged := mem.Log[before:]
	if len(logged) != wantMalformed {
		v = append(v, fmt.Sprintf("%d LOG row(s) were written by the time the 202 was returned, want exactly one per malformed row (%d)", len(logged), wantMalformed))
	}
	for i, r := range logged {
		if r.Status != "invalid" || r.Reason != "invalid_syntax" || r.Recommendation != "skip" || len(r.EmailHash) != 0 || r.HashScheme != "" || r.FromCache || r.Domain != "" {
			v = append(v, fmt.Sprintf("LOG row %d = %+v is not the sync path's invalid_syntax row (invalid/invalid_syntax/skip, no hash, no domain, from_cache false)", i, r))
		}
	}
	page := getResultsPage(t, srv, "bulk-key", jobID, "")
	if len(page.Results) != wantMalformed {
		return append(v, fmt.Sprintf("the results hold %d element(s) before any check ran, want the %d malformed rows (each has an outcome from the start)", len(page.Results), wantMalformed))
	}
	for i, el := range page.Results {
		if i >= len(logged) {
			break
		}
		l := logged[i]
		if el["status"] != l.Status || el["reason"] != l.Reason || el["recommendation"] != l.Recommendation || el["confidence"] != float64(l.Confidence) {
			v = append(v, fmt.Sprintf("element %d = %v/%v/%v/%v is not what was LOGGED (%s/%s/%s/%d): a result must be served from its LOG row",
				i, el["status"], el["reason"], el["recommendation"], el["confidence"], l.Status, l.Reason, l.Recommendation, l.Confidence))
		}
		if el["email"] != "" {
			v = append(v, fmt.Sprintf("element %d echoed an address", i))
		}
	}
	return v
}

// TestBulkItems_AMalformedRowIsLoggedBeforeThe202AndServedFromTheLog is the Door-2 exit over HTTP, and its control: the same checker over a store
// that behaves as before (nothing logged for a malformed row) must report both the missing LOG rows and the missing results.
func TestBulkItems_AMalformedRowIsLoggedBeforeThe202AndServedFromTheLog(t *testing.T) {
	// Three malformed rows, two of them the SAME garbage, and an addressable row between them (which adds no LOG row at submit).
	const body = `{"items":[
		{"item_id":"g1","address":"garbage"},
		{"item_id":"ok","address":"fine@acmecorp.com"},
		{"item_id":"g2","address":"garbage"},
		{"item_id":"g3","address":"also bad"}]}`

	srv, mem, _, _ := idsServer(t, nil)
	if v := malformedLoggedViolations(t, srv, mem, body, 3); len(v) != 0 {
		t.Fatalf("Door 2 (a malformed row's verdict is logged before it is returned, and served from the log) is violated:\n  %s", strings.Join(v, "\n  "))
	}

	oldSrv, oldMem, _, _ := idsServer(t, func(s store.Store) store.Store { return apiNoLogStore{s.(*store.MemStore)} })
	v := malformedLoggedViolations(t, oldSrv, oldMem, body, 3)
	joined := strings.Join(v, "\n")
	if len(v) < 2 || !strings.Contains(joined, "0 LOG row(s) were written") || !strings.Contains(joined, "the results hold 0 element(s)") {
		t.Fatalf("CONTROL: with no log written for malformed rows the checker must report the missing LOG rows and results, got %q", v)
	}
}

// noDupJobs is the OLD behaviour of the results route: duplicate_of is not served.
type noDupJobs struct{ JobReader }

func (n noDupJobs) ListJobResults(ctx context.Context, jobID, owner, cursor string, limit int) ([]store.WorkItemResult, string, error) {
	rows, next, err := n.JobReader.ListJobResults(ctx, jobID, owner, cursor, limit)
	for i := range rows {
		rows[i].DuplicateOf = nil
	}
	return rows, next, err
}

// duplicateVisibilityViolations judges the duplicate_of contract over HTTP for one job: element i must carry duplicate_of == want[i] when i is a
// key of want, and NONE otherwise (first rows and malformed rows). Read across pages of size 2 so a page boundary falls inside a duplicate run.
func duplicateVisibilityViolations(t *testing.T, srv *Server, bearer, jobID string, rows int, want map[int]int) []string {
	t.Helper()
	var v []string
	seen := 0
	cursor := ""
	for pages := 0; pages < 50; pages++ {
		q := "?limit=2"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		p := getResultsPage(t, srv, bearer, jobID, q)
		for _, el := range p.Results {
			i := ordinalOf(t, el)
			seen++
			got, has := el["duplicate_of"]
			if w, isDup := want[i]; isDup {
				if !has || got != float64(w) {
					v = append(v, fmt.Sprintf("row %d: duplicate_of = %v (present=%v), want %d", i, got, has, w))
				}
			} else if has {
				v = append(v, fmt.Sprintf("row %d: carries duplicate_of %v, want none", i, got))
			}
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if seen != rows {
		v = append(v, fmt.Sprintf("read %d element(s), want %d", seen, rows))
	}
	return v
}

// TestBulkItems_ADuplicateSaysWhichRowItSharesAndARepeatedMalformedAddressIsNotADuplicate: an address repeated three times — rows 1 and 2 carry
// duplicate_of 0 (the FIRST row; the first row carries none); a second address repeated once points at its own first row; a malformed address
// repeated is NOT a duplicate (each is its own row with its own LOG row). The 202 counts the duplicates. The control reads the same job through
// the old behaviour (duplicate_of not served) and the checker must report every duplicate as missing.
func TestBulkItems_ADuplicateSaysWhichRowItSharesAndARepeatedMalformedAddressIsNotADuplicate(t *testing.T) {
	srv, mem, cipher, st := idsServer(t, nil)
	resp := mustAccept(t, postBulkAs(t, srv, "bulk-key", `{"items":[
		{"item_id":"a0","address":"x@acmecorp.com"},
		{"item_id":"a1","address":"X@AcmeCorp.com"},
		{"item_id":"a2","address":" x@acmecorp.com"},
		{"item_id":"a3","address":"junk"},
		{"item_id":"a4","address":"junk"},
		{"item_id":"a5","address":"y@acmecorp.com"},
		{"item_id":"a6","address":"y@acmecorp.com"}]}`))
	jobID, _ := resp["job_id"].(string)
	counts, _ := resp["counts"].(map[string]any)
	if counts["total"] != float64(7) || counts["enqueued"] != float64(2) || counts["duplicate"] != float64(3) || counts["malformed"] != float64(2) {
		t.Fatalf("202 counts = %v, want total 7 enqueued 2 duplicate 3 malformed 2: the repeated malformed address is NOT a duplicate", counts)
	}
	if len(mem.Log) != 2 {
		t.Fatalf("%d LOG rows after the 202, want 2 (each malformed row its own)", len(mem.Log))
	}
	comp := &itemsComputer{pgSettleComputer: pgSettleComputer{now: idsNow}}
	drainAll(t, st, cipher, comp)
	if comp.calls["x@acmecorp.com"] != 1 || comp.calls["y@acmecorp.com"] != 1 {
		t.Fatalf("checks run = %v, want one per distinct address", comp.calls)
	}

	want := map[int]int{1: 0, 2: 0, 6: 5} // rows 1 and 2 repeat row 0 (not each other); row 6 repeats row 5
	if v := duplicateVisibilityViolations(t, srv, "bulk-key", jobID, 7, want); len(v) != 0 {
		t.Fatalf("duplicate visibility is violated:\n  %s", strings.Join(v, "\n  "))
	}
	// The rows that share a check also share its verdict (the platform can charge it once, and sees which rows to skip).
	page := getResultsPage(t, srv, "bulk-key", jobID, "")
	for _, pair := range [][2]int{{0, 1}, {0, 2}, {5, 6}} {
		a, b := page.Results[pair[0]], page.Results[pair[1]]
		if a["confidence"] != b["confidence"] || a["status"] != b["status"] || a["reason"] != b["reason"] {
			t.Errorf("rows %d and %d are one check and must carry one verdict, got %v/%v and %v/%v", pair[0], pair[1], a["status"], a["confidence"], b["status"], b["confidence"])
		}
	}

	// CONTROL: the old route (duplicate_of not served) is reported, row by row.
	old := *srv
	old.jobs = noDupJobs{srv.jobs}
	v := duplicateVisibilityViolations(t, &old, "bulk-key", jobID, 7, want)
	if len(v) != 3 || !strings.Contains(strings.Join(v, "\n"), "row 1: duplicate_of = <nil> (present=false), want 0") {
		t.Errorf("CONTROL: with duplicate_of not served the checker must report the 3 duplicate rows, got %q", v)
	}
	// And a checker that was wrong about which rows are duplicates is reported too: claiming the malformed repeat (row 4) is a duplicate of row 3.
	wrong := map[int]int{1: 0, 2: 0, 4: 3, 6: 5}
	if v := duplicateVisibilityViolations(t, srv, "bulk-key", jobID, 7, wrong); len(v) == 0 {
		t.Error("CONTROL: a claim that a repeated malformed address is a duplicate must be reported as wrong")
	}
	if rec := getAs(t, srv, "bulk-key", "/v1/jobs/"+jobID+"/results"); rec.Code != http.StatusOK {
		t.Fatalf("results: %d", rec.Code)
	}
}
