package api

// HTTP-level tests for the bulk API's per-row identity and per-job dedup (owner rulings A1/A2; migration 0018; Docs/bulk-item-identity.md).
//
// The in-memory store backs most of them (fast, no database); the PG-gated one at the end runs the same flow over real Postgres in a throwaway
// database (testpg.Scratch; it SKIPS honestly without TM_TEST_DATABASE_URL). Everything here goes through Server.Handler(), so the request
// parsing, the 400/413 envelopes, the 202 body, the status counts and the results elements are what a caller sees.
//
// PAIRED CONTROLS, each making the same assertion fail on the behaviour it replaces:
//   - every rejected request has a FIXED twin (the same request without the defect) that is accepted, so a rejection is about the defect only;
//   - the pagination walker is run over a handler whose store hands back a cursor taken from the look-ahead row (M2's bug) and over an unsorted
//     listing, and must report a missing ordinal and an out-of-order one;
//   - the cross-tenant scenario is re-run over a store that dedups across jobs (the OLD scope) and must report that tenant B got no work item.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/config"
	"github.com/coldmail-verify/control-plane/internal/orchestrator"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/queue"
	"github.com/coldmail-verify/control-plane/internal/secrets"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/testpg"
	"github.com/coldmail-verify/control-plane/internal/verdict"
)

// A second tenant (the first is bulkOwner behind "bulk-key").
const bulkOwnerB = "ffffffff-ffff-4fff-8fff-ffffffffffff"

// idsNow is the clock of these tests.
var idsNow = time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

// idsServer wires a Server over a MemStore, optionally wrapped (the wrapper sits between the Enqueuer/JobReader and the Mem store), with two
// tenants' per-caller keys: "bulk-key" (bulkOwner) and "bulk-key-b" (bulkOwnerB).
func idsServer(t *testing.T, wrap func(store.Store) store.Store) (*Server, *store.MemStore, *queue.AddrCipher, store.Store) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Env = "production"
	sec, _ := secrets.Load(fakeSource{}, false)
	h := mustHasher(t)
	mem := store.NewMem(func() time.Time { return idsNow })
	var st store.Store = mem
	if wrap != nil {
		st = wrap(mem)
	}
	def, _ := policy.ColdEmailDefinition()
	cipher, err := queue.NewAddrCipher(key32(1))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	enq := &queue.Enqueuer{Store: st, Hasher: h, Cipher: cipher, ConfigFingerprint: "cfg", Now: func() time.Time { return idsNow }}
	reader, ok := st.(JobReader)
	if !ok {
		t.Fatalf("%T is not a JobReader", st)
	}
	srv := New(cfg, sec, h).WithVerifier(stubVerifier{}, def).WithCallerAuth(mem).WithBulk(enq, reader, 0)
	srv.now = func() time.Time { return idsNow }
	mem.SeedCallerWithUser(sha("bulk-key"), "caller_a", bulkOwner, idsNow.Add(-time.Hour), time.Time{})
	mem.SeedCallerWithUser(sha("bulk-key-b"), "caller_b", bulkOwnerB, idsNow.Add(-time.Hour), time.Time{})
	return srv, mem, cipher, st
}

// itemsComputer is pgSettleComputer (a verdict row with a real uuid, so it also works against Postgres) that counts how many times each address
// was checked and gives each address its own confidence, so "a duplicate row carries ITS address's verdict" is observable.
type itemsComputer struct {
	pgSettleComputer
	calls map[string]int
}

func (c *itemsComputer) ComputeForQueue(ctx context.Context, rawEmail string, def *policy.Definition, jobID string) (orchestrator.QueueOutcome, error) {
	out, err := c.pgSettleComputer.ComputeForQueue(ctx, rawEmail, def, jobID)
	if err != nil {
		return out, err
	}
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[rawEmail]++
	h := 0
	for _, b := range []byte(rawEmail) {
		h = (h*31 + int(b)) % 50
	}
	out.Row.Confidence = 40 + h
	out.Row.Domain = rawEmail[strings.LastIndex(rawEmail, "@")+1:]
	return out, nil
}

// drainAll runs a real drainer over st until the queue is empty.
func drainAll(t *testing.T, st store.Store, cipher *queue.AddrCipher, comp queue.Computer) {
	t.Helper()
	dr, err := queue.NewDrainer(queue.Deps{
		Store: st, Compute: comp, Policy: fixedPolicy{}, Cipher: cipher,
		Backoff:  queue.Backoff{Base: time.Minute, Factor: 2, Cap: time.Hour, RateWait: time.Second},
		Now:      func() time.Time { return idsNow },
		WorkerID: "ids-drainer", LeaseTTL: time.Minute, MaxSMTPDeadline: 10 * time.Second,
		MaxAttempts: 3, MaxRetryAge: time.Hour, BatchSize: 1000, ReclaimEvery: time.Hour,
	})
	if err != nil {
		t.Fatalf("drainer: %v", err)
	}
	for i := 0; i < 20; i++ {
		if !dr.TickOnce(context.Background()) {
			return
		}
	}
	t.Fatal("the drainer was still finding work after 20 ticks")
}

func postBulkAs(t *testing.T, srv *Server, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/bulk", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func mustAccept(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("202 body is not JSON: %v (%s)", err, rec.Body.String())
	}
	return out
}

type resultsPage struct {
	JobID      string           `json:"job_id"`
	Results    []map[string]any `json:"results"`
	NextCursor string           `json:"next_cursor"`
}

func getResultsPage(t *testing.T, srv *Server, bearer, jobID, query string) resultsPage {
	t.Helper()
	rec := getAs(t, srv, bearer, "/v1/jobs/"+jobID+"/results"+query)
	if rec.Code != http.StatusOK {
		t.Fatalf("results%s: %d %s", query, rec.Code, rec.Body.String())
	}
	var p resultsPage
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("results body is not JSON: %v (%s)", err, rec.Body.String())
	}
	return p
}

func ordinalOf(t *testing.T, el map[string]any) int {
	t.Helper()
	f, ok := el["ordinal"].(float64)
	if !ok {
		t.Fatalf("a result element has no numeric `ordinal`: %v", el)
	}
	return int(f)
}

// ─────────────────────────────────────────────────────────────────────────────
// the request
// ─────────────────────────────────────────────────────────────────────────────

// TestBulkItems_RequestShapesAndItemIDRules: exactly one of `addresses` / `items`; the item_id rules; each refusal is a 400 validation_failed whose
// details[] names the offending fields (and ordinals) with the fixed issue slugs; each has a FIXED twin that is accepted, so the refusal is about
// the defect and nothing else. The old `addresses` shape keeps working.
func TestBulkItems_RequestShapesAndItemIDRules(t *testing.T) {
	srv, _, _, _ := idsServer(t, nil)
	long := func(n int) string { return strings.Repeat("x", n) }
	type detail struct{ field, issue string }

	cases := []struct {
		name  string
		body  string
		want  []detail // EXACTLY these details, in this order
		fixed string   // the same request without the defect: accepted
	}{
		{"both shapes given",
			`{"addresses":["a@acmecorp.com"],"items":[{"item_id":"r0","address":"a@acmecorp.com"}]}`,
			[]detail{{"addresses", "conflict"}, {"items", "conflict"}},
			`{"items":[{"item_id":"r0","address":"a@acmecorp.com"}]}`},
		{"neither shape given", `{}`,
			[]detail{{"addresses", "required"}, {"items", "required"}},
			`{"addresses":["a@acmecorp.com"]}`},
		{"neither given, only a caller_ref", `{"caller_ref":"x"}`,
			[]detail{{"addresses", "required"}, {"items", "required"}},
			`{"caller_ref":"x","addresses":["a@acmecorp.com"]}`},
		{"an empty addresses list", `{"addresses":[]}`,
			[]detail{{"addresses", "required"}},
			`{"addresses":["a@acmecorp.com"]}`},
		{"an empty items list", `{"items":[]}`,
			[]detail{{"items", "required"}},
			`{"items":[{"item_id":"r0","address":"a@acmecorp.com"}]}`},
		{"a missing item_id", `{"items":[{"item_id":"r0","address":"a@acmecorp.com"},{"address":"b@acmecorp.com"}]}`,
			[]detail{{"items[1].item_id", "required"}},
			`{"items":[{"item_id":"r0","address":"a@acmecorp.com"},{"item_id":"r1","address":"b@acmecorp.com"}]}`},
		{"a blank item_id", `{"items":[{"item_id":"","address":"a@acmecorp.com"}]}`,
			[]detail{{"items[0].item_id", "required"}},
			`{"items":[{"item_id":"r0","address":"a@acmecorp.com"}]}`},
		{"a whitespace-only item_id", `{"items":[{"item_id":"  \t ","address":"a@acmecorp.com"}]}`,
			[]detail{{"items[0].item_id", "required"}},
			`{"items":[{"item_id":"r0","address":"a@acmecorp.com"}]}`},
		{"an item_id of 201 bytes", `{"items":[{"item_id":"` + long(201) + `","address":"a@acmecorp.com"}]}`,
			[]detail{{"items[0].item_id", "out_of_range"}},
			`{"items":[{"item_id":"` + long(200) + `","address":"a@acmecorp.com"}]}`},
		{"an item_id of 101 two-byte runes (202 bytes)", `{"items":[{"item_id":"` + strings.Repeat("é", 101) + `","address":"a@acmecorp.com"}]}`,
			[]detail{{"items[0].item_id", "out_of_range"}},
			`{"items":[{"item_id":"` + strings.Repeat("é", 100) + `","address":"a@acmecorp.com"}]}`},
		{"a newline in the item_id", `{"items":[{"item_id":"r\n0","address":"a@acmecorp.com"}]}`,
			[]detail{{"items[0].item_id", "malformed"}},
			`{"items":[{"item_id":"r 0","address":"a@acmecorp.com"}]}`},
		{"a tab in the item_id", `{"items":[{"item_id":"r\t0","address":"a@acmecorp.com"}]}`,
			[]detail{{"items[0].item_id", "malformed"}},
			`{"items":[{"item_id":"r-0","address":"a@acmecorp.com"}]}`},
		{"a NUL in the item_id", `{"items":[{"item_id":"r\u00000","address":"a@acmecorp.com"}]}`,
			[]detail{{"items[0].item_id", "malformed"}},
			`{"items":[{"item_id":"r0","address":"a@acmecorp.com"}]}`},
		{"a DEL in the item_id", `{"items":[{"item_id":"r\u007f0","address":"a@acmecorp.com"}]}`,
			[]detail{{"items[0].item_id", "malformed"}},
			`{"items":[{"item_id":"r0","address":"a@acmecorp.com"}]}`},
		{"a duplicate item_id names EVERY offending ordinal, earlier ones too",
			`{"items":[{"item_id":"r0","address":"a@acmecorp.com"},{"item_id":"r1","address":"b@acmecorp.com"},{"item_id":"r0","address":"c@acmecorp.com"},{"item_id":"r9","address":"d@acmecorp.com"},{"item_id":"r1","address":"e@acmecorp.com"}]}`,
			[]detail{{"items[0].item_id", "duplicate"}, {"items[1].item_id", "duplicate"}, {"items[2].item_id", "duplicate"}, {"items[4].item_id", "duplicate"}},
			`{"items":[{"item_id":"r0","address":"a@acmecorp.com"},{"item_id":"r1","address":"b@acmecorp.com"},{"item_id":"r2","address":"c@acmecorp.com"},{"item_id":"r9","address":"d@acmecorp.com"},{"item_id":"r3","address":"e@acmecorp.com"}]}`},
		{"two problems at once are both named, lowest ordinal first",
			`{"items":[{"item_id":"a","address":"a@acmecorp.com"},{"item_id":"","address":"b@acmecorp.com"},{"item_id":"a","address":"c@acmecorp.com"}]}`,
			[]detail{{"items[0].item_id", "duplicate"}, {"items[1].item_id", "required"}, {"items[2].item_id", "duplicate"}},
			`{"items":[{"item_id":"a","address":"a@acmecorp.com"},{"item_id":"b","address":"b@acmecorp.com"},{"item_id":"c","address":"c@acmecorp.com"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postBulkAs(t, srv, "bulk-key", tc.body)
			body := requireEnvelope(t, rec, codeValidationFailed, http.StatusBadRequest)
			raw, _ := body["details"].([]any)
			if len(raw) != len(tc.want) {
				t.Fatalf("details = %v, want exactly %v", raw, tc.want)
			}
			for i, w := range tc.want {
				m, _ := raw[i].(map[string]any)
				if m["field"] != w.field || m["issue"] != w.issue {
					t.Errorf("details[%d] = %v, want field %q issue %q", i, m, w.field, w.issue)
				}
				requireDetail(t, body, w.field, w.issue) // and the issue is from the fixed slug set
			}
			// The envelope never echoes the input (two-vocabulary rule): an item_id the caller sent must not come back in the error.
			if strings.Contains(rec.Body.String(), "acmecorp.com") {
				t.Errorf("the error echoed an address: %s", rec.Body.String())
			}
			// CONTROL: the same request without the defect is accepted.
			mustAccept(t, postBulkAs(t, srv, "bulk-key", tc.fixed))
		})
	}

	t.Run("case and surrounding spaces make distinct item_ids", func(t *testing.T) {
		mustAccept(t, postBulkAs(t, srv, "bulk-key", `{"items":[{"item_id":"A","address":"a@acmecorp.com"},{"item_id":"a","address":"b@acmecorp.com"},{"item_id":" a","address":"c@acmecorp.com"}]}`))
	})
	t.Run("the same ADDRESS under different item_ids is fine (duplicate rows are the point)", func(t *testing.T) {
		resp := mustAccept(t, postBulkAs(t, srv, "bulk-key", `{"items":[{"item_id":"x","address":"a@acmecorp.com"},{"item_id":"y","address":"a@acmecorp.com"}]}`))
		counts := resp["counts"].(map[string]any)
		if counts["total"] != float64(2) || counts["enqueued"] != float64(1) || counts["duplicate"] != float64(1) {
			t.Errorf("counts = %v, want total 2 enqueued 1 duplicate 1", counts)
		}
	})
	t.Run("a bad ADDRESS is a malformed row, never a 400", func(t *testing.T) {
		resp := mustAccept(t, postBulkAs(t, srv, "bulk-key", `{"items":[{"item_id":"x","address":"nope"},{"item_id":"y","address":""},{"item_id":"z"}]}`))
		if resp["counts"].(map[string]any)["malformed"] != float64(3) {
			t.Errorf("counts = %v, want 3 malformed rows", resp["counts"])
		}
	})
	t.Run("the cap applies to items and names the field", func(t *testing.T) {
		capped, _, _, _ := idsServer(t, nil)
		capped.bulkMaxAddrs = 2
		rec := postBulkAs(t, capped, "bulk-key", `{"items":[{"item_id":"a","address":"a@acmecorp.com"},{"item_id":"b","address":"b@acmecorp.com"},{"item_id":"c","address":"c@acmecorp.com"}]}`)
		requireDetail(t, requireEnvelope(t, rec, codePayloadTooLarge, http.StatusRequestEntityTooLarge), "items", issueOutOfRange)
		// CONTROL: two rows at cap 2 are accepted.
		mustAccept(t, postBulkAs(t, capped, "bulk-key", `{"items":[{"item_id":"a","address":"a@acmecorp.com"},{"item_id":"b","address":"b@acmecorp.com"}]}`))
	})
	t.Run("at most 100 offenders are named", func(t *testing.T) {
		var sb strings.Builder
		sb.WriteString(`{"items":[`)
		for i := 0; i < 250; i++ {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(`{"item_id":"same","address":"a@acmecorp.com"}`)
		}
		sb.WriteString(`]}`)
		body := requireEnvelope(t, postBulkAs(t, srv, "bulk-key", sb.String()), codeValidationFailed, http.StatusBadRequest)
		raw, _ := body["details"].([]any)
		if len(raw) != maxValidationDetails {
			t.Errorf("%d details for 250 duplicate rows, want %d (bounded)", len(raw), maxValidationDetails)
		}
		if first, _ := raw[0].(map[string]any); first["field"] != "items[0].item_id" {
			t.Errorf("the first offender named is %v, want items[0] (lowest ordinal first)", first)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// the whole flow: create response, status counts, results
// ─────────────────────────────────────────────────────────────────────────────

var door1Keys = []string{"api_version", "status", "reason", "confidence", "recommendation", "email", "domain", "flags", "signals", "meta", "raw"}

// TestBulkItems_EndToEndOverHTTP is the exit test of A1 and of dedup-within-a-job through the handler: submit labelled rows with duplicates and
// malformed addresses; read the 202 body (counts, malformed_rows); drain; status reaches pending 0; the results hold one element per input row in
// ordinal order with the row's item_id, the verdict of the SHARED check on duplicate rows, and invalid/invalid_syntax on the malformed ones; and
// no element ever carries an address.
func TestBulkItems_EndToEndOverHTTP(t *testing.T) {
	srv, mem, cipher, st := idsServer(t, nil)
	resp := mustAccept(t, postBulkAs(t, srv, "bulk-key", `{"caller_ref":"platform-job-7","items":[
		{"item_id":"row-0","address":"Jane@AcmeCorp.com"},
		{"item_id":"row-1","address":"bob@acmecorp.com"},
		{"item_id":"row-2","address":"jane@acmecorp.com"},
		{"item_id":"row-3","address":"not-an-address"},
		{"item_id":"row-4","address":"jane@acmecorp.com"},
		{"item_id":"row-5","address":"carol@acmecorp.com"}]}`))
	jobID, _ := resp["job_id"].(string)
	counts, _ := resp["counts"].(map[string]any)
	if resp["status"] != "accepted" || counts["total"] != float64(6) || counts["enqueued"] != float64(3) || counts["duplicate"] != float64(2) || counts["malformed"] != float64(1) {
		t.Fatalf("202 body = %v, want accepted with counts total 6 enqueued 3 duplicate 2 malformed 1", resp)
	}
	if bad, _ := resp["malformed_rows"].([]any); len(bad) != 1 || !reflect.DeepEqual(bad[0], map[string]any{"ordinal": float64(3), "item_id": "row-3"}) || resp["malformed_rows_truncated"] != false {
		t.Fatalf("malformed_rows = %v truncated=%v, want [{ordinal 3, item_id row-3}] and false", resp["malformed_rows"], resp["malformed_rows_truncated"])
	}
	// Door 2: by the time the 202 came back the malformed row was LOGGED — exactly one LOG row, the sync path's invalid_syntax row with a NULL hash.
	if len(mem.Log) != 1 || mem.Log[0].Reason != "invalid_syntax" || mem.Log[0].Status != "invalid" || len(mem.Log[0].EmailHash) != 0 || mem.Log[0].FromCache {
		t.Fatalf("LOG after the 202 = %+v, want exactly one invalid/invalid_syntax row with no email_hash and from_cache false", mem.Log)
	}
	logged := mem.Log[0]

	// Before any check ran: pending is everything except the malformed row, which already has an outcome.
	before := getAs(t, srv, "bulk-key", "/v1/jobs/"+jobID)
	var beforeStatus struct {
		Complete bool           `json:"complete"`
		Counts   map[string]int `json:"counts"`
	}
	if err := json.Unmarshal(before.Body.Bytes(), &beforeStatus); err != nil || beforeStatus.Complete || beforeStatus.Counts["total"] != 6 || beforeStatus.Counts["settled"] != 1 || beforeStatus.Counts["pending"] != 5 {
		t.Fatalf("status before draining = %s (err %v), want not complete, total 6 settled 1 pending 5", before.Body.String(), err)
	}

	comp := &itemsComputer{pgSettleComputer: pgSettleComputer{now: idsNow}}
	drainAll(t, st, cipher, comp)
	if comp.calls["jane@acmecorp.com"] != 1 || comp.calls["bob@acmecorp.com"] != 1 || comp.calls["carol@acmecorp.com"] != 1 || len(comp.calls) != 3 {
		t.Fatalf("checks run = %v, want exactly one check each for jane, bob and carol (three rows share jane's)", comp.calls)
	}

	after := getAs(t, srv, "bulk-key", "/v1/jobs/"+jobID)
	var afterStatus struct {
		Complete bool           `json:"complete"`
		Status   string         `json:"status"`
		CallerRf string         `json:"caller_ref"`
		Counts   map[string]int `json:"counts"`
	}
	if err := json.Unmarshal(after.Body.Bytes(), &afterStatus); err != nil || !afterStatus.Complete || afterStatus.Status != "complete" || afterStatus.CallerRf != "platform-job-7" ||
		afterStatus.Counts["total"] != 6 || afterStatus.Counts["settled"] != 6 || afterStatus.Counts["pending"] != 0 {
		t.Fatalf("status after draining = %s (err %v), want complete with total 6 settled 6 pending 0", after.Body.String(), err)
	}

	page := getResultsPage(t, srv, "bulk-key", jobID, "")
	if page.NextCursor != "" || len(page.Results) != 6 {
		t.Fatalf("results: %d element(s) next_cursor=%q, want 6 in one page", len(page.Results), page.NextCursor)
	}
	for i, el := range page.Results {
		if ordinalOf(t, el) != i {
			t.Errorf("element %d has ordinal %v: results must be in ordinal order", i, el["ordinal"])
		}
		if el["item_id"] != fmt.Sprintf("row-%d", i) {
			t.Errorf("element %d has item_id %v, want row-%d", i, el["item_id"], i)
		}
		for _, k := range door1Keys {
			if _, has := el[k]; !has {
				t.Errorf("element %d lost the Door-1 key %q: the bulk result stays the byte-identical verdict object plus two additive keys", i, k)
			}
		}
		if el["email"] != "" {
			t.Errorf("element %d echoed an address %q: a bulk result never does (Door 3)", i, el["email"])
		}
		// An old client parses the element as a plain verdict and it is valid: the new keys are ignorable.
		raw, _ := json.Marshal(el)
		var v verdict.Response
		if err := json.Unmarshal(raw, &v); err != nil || !v.Valid() {
			t.Errorf("element %d does not parse as a valid verdict.Response (err %v): %s", i, err, raw)
		}
	}
	// The malformed row: the settled invalid / invalid_syntax / skip verdict, with its own ordinal and item_id.
	bad := page.Results[3]
	if bad["status"] != "invalid" || bad["reason"] != "invalid_syntax" || bad["recommendation"] != "skip" {
		t.Errorf("the malformed row (3) = %v/%v/%v, want invalid/invalid_syntax/skip", bad["status"], bad["reason"], bad["recommendation"])
	}
	if sig, _ := bad["signals"].(map[string]any); sig["syntax_valid"] != false {
		t.Errorf("the malformed row's signals = %v, want syntax_valid=false (as the sync path says)", bad["signals"])
	}
	// SERVED FROM THE LOG: the element equals the LOG row written at submit, field for field (confidence included: it is the logged number).
	if bad["confidence"] != float64(logged.Confidence) || bad["reason"] != logged.Reason || bad["status"] != logged.Status || bad["recommendation"] != logged.Recommendation ||
		!reflect.DeepEqual(bad["flags"], map[string]any{"from_cache": logged.Flags["from_cache"]}) {
		t.Errorf("the malformed element %v does not equal the LOG row it must be served from (%+v)", bad, logged)
	}
	// duplicate_of: rows 2 and 4 repeat row 0's address (the FIRST row, not each other); no other row says anything — including the malformed row.
	for i, el := range page.Results {
		got, has := el["duplicate_of"]
		if i == 2 || i == 4 {
			if !has || got != float64(0) {
				t.Errorf("element %d duplicate_of = %v (present=%v), want 0", i, got, has)
			}
		} else if has {
			t.Errorf("element %d carries duplicate_of %v, want none", i, got)
		}
	}
	// Rows 0, 2 and 4 are one check: the verdict part of each element is IDENTICAL; only ordinal, item_id and duplicate_of differ.
	strip := func(el map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range el {
			if k != "ordinal" && k != "item_id" && k != "duplicate_of" {
				out[k] = v
			}
		}
		return out
	}
	if !reflect.DeepEqual(strip(page.Results[0]), strip(page.Results[2])) || !reflect.DeepEqual(strip(page.Results[0]), strip(page.Results[4])) {
		t.Errorf("rows 0, 2 and 4 share one check and must carry the same verdict:\n%v\n%v\n%v", strip(page.Results[0]), strip(page.Results[2]), strip(page.Results[4]))
	}
	// CONTROL: rows of different addresses do not (confidence is derived from the address).
	if reflect.DeepEqual(strip(page.Results[0]), strip(page.Results[1])) {
		t.Error("CONTROL: jane's and bob's verdicts are identical, so 'duplicates carry the same verdict' proves nothing in this fixture")
	}
}

// TestBulkItems_AddressesShapeHasOrdinalsButNoItemID: the original shape answers as before, and its rows are numbered too; `item_id` is ABSENT
// (not null, not "") from elements that had none.
func TestBulkItems_AddressesShapeHasOrdinalsButNoItemID(t *testing.T) {
	srv, _, cipher, st := idsServer(t, nil)
	resp := mustAccept(t, postBulkAs(t, srv, "bulk-key", `{"addresses":["a@acmecorp.com","oops","b@acmecorp.com"]}`))
	jobID, _ := resp["job_id"].(string)
	if bad, _ := resp["malformed_rows"].([]any); len(bad) != 1 || !reflect.DeepEqual(bad[0], map[string]any{"ordinal": float64(1)}) {
		t.Fatalf("malformed_rows = %v, want [{ordinal 1}] with NO item_id key", resp["malformed_rows"])
	}
	drainAll(t, st, cipher, &itemsComputer{pgSettleComputer: pgSettleComputer{now: idsNow}})
	page := getResultsPage(t, srv, "bulk-key", jobID, "")
	if len(page.Results) != 3 {
		t.Fatalf("%d results, want 3", len(page.Results))
	}
	for i, el := range page.Results {
		if ordinalOf(t, el) != i {
			t.Errorf("element %d ordinal = %v", i, el["ordinal"])
		}
		if _, has := el["item_id"]; has {
			t.Errorf("element %d carries item_id %v although the row had none", i, el["item_id"])
		}
	}
	if page.Results[0]["ordinal"].(float64) != 0 {
		t.Error("ordinal 0 must be present and 0 (a real position), not omitted")
	}
}

// TestBulkItems_MalformedRowsAreCappedInTheCreateResponseButCountedExactly: over the 1000-entry cap the list is cut and malformed_rows_truncated is
// true; the count is exact; exactly at the cap it is not truncated. malformed_rows is always an array, never null.
func TestBulkItems_MalformedRowsAreCappedInTheCreateResponseButCountedExactly(t *testing.T) {
	srv, _, _, _ := idsServer(t, nil)
	build := func(n int) string {
		var sb strings.Builder
		sb.WriteString(`{"items":[`)
		for i := 0; i < n; i++ {
			if i > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, `{"item_id":"bad-%d","address":"x"}`, i)
		}
		sb.WriteString(`]}`)
		return sb.String()
	}
	for _, c := range []struct {
		n         int
		listed    int
		truncated bool
	}{{999, 999, false}, {1000, 1000, false}, {1001, 1000, true}, {1500, 1000, true}} {
		resp := mustAccept(t, postBulkAs(t, srv, "bulk-key", build(c.n)))
		list, _ := resp["malformed_rows"].([]any)
		if len(list) != c.listed || resp["malformed_rows_truncated"] != c.truncated || resp["counts"].(map[string]any)["malformed"] != float64(c.n) {
			t.Errorf("%d malformed rows: listed %d truncated=%v count=%v, want listed %d truncated=%v count %d",
				c.n, len(list), resp["malformed_rows_truncated"], resp["counts"].(map[string]any)["malformed"], c.listed, c.truncated, c.n)
		}
	}
	resp := mustAccept(t, postBulkAs(t, srv, "bulk-key", `{"addresses":["a@acmecorp.com"]}`))
	if list, ok := resp["malformed_rows"].([]any); !ok || len(list) != 0 || resp["malformed_rows_truncated"] != false {
		t.Errorf("with nothing malformed malformed_rows must be [] (not null or absent) and truncated false, got %v / %v", resp["malformed_rows"], resp["malformed_rows_truncated"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// pagination
// ─────────────────────────────────────────────────────────────────────────────

// walkHTTP pages through the results route with `limit` and returns the ordinals of each page, in order.
func walkHTTP(t *testing.T, srv *Server, bearer, jobID string, limit int) [][]int {
	t.Helper()
	var pages [][]int
	cursor := ""
	for i := 0; i < 1000; i++ {
		q := fmt.Sprintf("?limit=%d", limit)
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		p := getResultsPage(t, srv, bearer, jobID, q)
		page := make([]int, len(p.Results))
		for j, el := range p.Results {
			page[j] = ordinalOf(t, el)
		}
		pages = append(pages, page)
		if p.NextCursor == "" {
			return pages
		}
		cursor = p.NextCursor
	}
	t.Fatalf("the cursor never ran out after 1000 pages at limit %d", limit)
	return nil
}

// exactOnce judges a walk: every wanted ordinal exactly once (set equality with multiplicity), nothing else, strictly increasing.
func exactOnce(pages [][]int, want []int) []string {
	var v []string
	seen := map[int]int{}
	prev := -1
	for p, page := range pages {
		for j, o := range page {
			seen[o]++
			if o <= prev {
				v = append(v, fmt.Sprintf("out of order: ordinal %d came after %d (page %d, position %d)", o, prev, p, j))
			}
			prev = o
		}
	}
	wanted := map[int]bool{}
	for _, o := range want {
		wanted[o] = true
		if seen[o] != 1 {
			v = append(v, fmt.Sprintf("ordinal %d returned %d times, want exactly once", o, seen[o]))
		}
	}
	for o := range seen {
		if !wanted[o] {
			v = append(v, fmt.Sprintf("ordinal %d returned but is not a row of the job", o))
		}
	}
	sort.Strings(v)
	return v
}

// lookAheadJobs is M2's bug (M2-G5) as a JobReader: it asks the real store for limit+1 rows, returns limit of them, and hands back the EXTRA row's
// ordinal as the next cursor, so the next page starts after it and that row is never served.
type lookAheadJobs struct{ JobReader }

func (l lookAheadJobs) ListJobResults(ctx context.Context, jobID, owner, cursor string, limit int) ([]store.WorkItemResult, string, error) {
	rows, _, err := l.JobReader.ListJobResults(ctx, jobID, owner, cursor, limit+1)
	if err != nil || len(rows) <= limit {
		return rows, "", err
	}
	return rows[:limit], store.FormatResultsCursor(rows[limit].Ordinal), nil
}

// unsortedJobs serves every page in reverse order.
type unsortedJobs struct{ JobReader }

func (u unsortedJobs) ListJobResults(ctx context.Context, jobID, owner, cursor string, limit int) ([]store.WorkItemResult, string, error) {
	rows, next, err := u.JobReader.ListJobResults(ctx, jobID, owner, cursor, limit)
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, next, err
}

// TestBulkItems_PaginationFindsEveryOrdinalExactlyOnce: several pages (limit < total) over a job whose duplicate runs straddle page boundaries,
// every ordinal exactly once with exact set equality, through the HTTP route. The PAIRED CONTROLS run the same walker over a look-ahead cursor and
// an unsorted listing and must report a missing ordinal and an out-of-order one.
func TestBulkItems_PaginationFindsEveryOrdinalExactlyOnce(t *testing.T) {
	srv, _, cipher, st := idsServer(t, nil)
	// 12 rows: runs of the same address at 0-2, 4-5 and 8-10, malformed rows at 3 and 11.
	addrs := []string{"a", "a", "a", "!", "b", "b", "c", "d", "e", "e", "e", "!"}
	var sb strings.Builder
	sb.WriteString(`{"items":[`)
	for i, a := range addrs {
		if i > 0 {
			sb.WriteString(",")
		}
		addr := a + "@acmecorp.com"
		if a == "!" {
			addr = "malformed"
		}
		fmt.Fprintf(&sb, `{"item_id":"r%d","address":%q}`, i, addr)
	}
	sb.WriteString(`]}`)
	jobID, _ := mustAccept(t, postBulkAs(t, srv, "bulk-key", sb.String()))["job_id"].(string)
	drainAll(t, st, cipher, &itemsComputer{pgSettleComputer: pgSettleComputer{now: idsNow}})
	want := make([]int, len(addrs))
	for i := range want {
		want[i] = i
	}

	for _, limit := range []int{1, 2, 3, 5, 11, 12, 13, 1000} {
		pages := walkHTTP(t, srv, "bulk-key", jobID, limit)
		if v := exactOnce(pages, want); len(v) != 0 {
			t.Errorf("limit %d: %s", limit, strings.Join(v, "; "))
		}
		if wantPages := (len(want) + limit - 1) / limit; len(pages) != wantPages {
			t.Errorf("limit %d: %d page(s), want %d", limit, len(pages), wantPages)
		}
	}
	// limit=2 puts a boundary between rows 1|2 (one check) and limit=5 between 4|5 (one check): the case the owner asked for is exercised.
	if p := walkHTTP(t, srv, "bulk-key", jobID, 2); len(p) < 2 || p[0][len(p[0])-1] != 1 || p[1][0] != 2 {
		t.Fatalf("fixture: with limit 2 the first boundary must fall between rows 1 and 2, which share a check; pages = %v", p)
	}

	// CONTROLS.
	broken := *srv
	broken.jobs = lookAheadJobs{srv.jobs}
	if v := exactOnce(walkHTTP(t, &broken, "bulk-key", jobID, 3), want); len(v) == 0 || !strings.Contains(strings.Join(v, "\n"), "returned 0 times") {
		t.Errorf("CONTROL: a cursor taken from the look-ahead row must be reported as a skipped ordinal, got %q", v)
	}
	unsorted := *srv
	unsorted.jobs = unsortedJobs{srv.jobs}
	if v := exactOnce(walkHTTP(t, &unsorted, "bulk-key", jobID, 3), want); len(v) == 0 || !strings.Contains(strings.Join(v, "\n"), "out of order") {
		t.Errorf("CONTROL: an unsorted listing must be reported as out of order, got %q", v)
	}
}

// TestBulkItems_AResultsCursorMustBeOneThisServiceIssued: the cursor is opaque but validated — garbage and the pre-0018 work-item uuid are a 400
// naming the field, never a 500 or a silently different page; a stranger still gets the tenancy 404 first.
func TestBulkItems_AResultsCursorMustBeOneThisServiceIssued(t *testing.T) {
	srv, _, cipher, st := idsServer(t, nil)
	jobID, _ := mustAccept(t, postBulkAs(t, srv, "bulk-key", `{"addresses":["a@acmecorp.com","b@acmecorp.com","c@acmecorp.com"]}`))["job_id"].(string)
	drainAll(t, st, cipher, &itemsComputer{pgSettleComputer: pgSettleComputer{now: idsNow}})

	for _, bad := range []string{"abc", "-1", "018f0000-0000-7000-8000-0000000000a1", "1.5", "99999999999"} {
		rec := getAs(t, srv, "bulk-key", "/v1/jobs/"+jobID+"/results?cursor="+bad)
		requireDetail(t, requireEnvelope(t, rec, codeValidationFailed, http.StatusBadRequest), "cursor", issueMalformed)
	}
	// CONTROL: a cursor the service issued is accepted and continues exactly after that row.
	first := getResultsPage(t, srv, "bulk-key", jobID, "?limit=1")
	if first.NextCursor == "" || len(first.Results) != 1 {
		t.Fatalf("fixture: page 1 = %d element(s) next=%q", len(first.Results), first.NextCursor)
	}
	second := getResultsPage(t, srv, "bulk-key", jobID, "?limit=1&cursor="+first.NextCursor)
	if len(second.Results) != 1 || ordinalOf(t, second.Results[0]) != 1 {
		t.Errorf("the page after cursor %q must start at ordinal 1, got %v", first.NextCursor, second.Results)
	}
	// A stranger with a garbage cursor still learns nothing: 404 before the cursor is judged.
	rec := getAs(t, srv, "bulk-key-b", "/v1/jobs/"+jobID+"/results?cursor=abc")
	requireEnvelope(t, rec, codeNotFound, http.StatusNotFound)
}

// ─────────────────────────────────────────────────────────────────────────────
// A2 across tenants, over HTTP
// ─────────────────────────────────────────────────────────────────────────────

// apiGlobalScopeStore is the OLD dedup scope: once ANY job holds an idempotency key, every later enqueue of it, in any job, is a "duplicate".
type apiGlobalScopeStore struct {
	*store.MemStore
	holder map[string]string
}

func (g *apiGlobalScopeStore) EnqueueWorkItem(ctx context.Context, item store.WorkItemRow) (string, bool, error) {
	k := string(item.IdempotencyKey)
	if id, ok := g.holder[k]; ok {
		return id, true, nil
	}
	id, existed, err := g.MemStore.EnqueueWorkItem(ctx, item)
	if err == nil && !existed {
		g.holder[k] = id
	}
	return id, existed, err
}

// crossTenantHTTPViolations: tenant A submits and fully settles a job; tenant B then submits the same addresses. B must get its own job with a
// result for every row, and A must not read it. Returns what is wrong, so it can be pointed at the old scope as the control.
func crossTenantHTTPViolations(t *testing.T, srv *Server, st store.Store, cipher *queue.AddrCipher) []string {
	t.Helper()
	comp := &itemsComputer{pgSettleComputer: pgSettleComputer{now: idsNow}}
	aRec := postBulkAs(t, srv, "bulk-key", `{"items":[{"item_id":"a0","address":"jane@acmecorp.com"},{"item_id":"a1","address":"bob@acmecorp.com"}]}`)
	aJob, _ := mustAccept(t, aRec)["job_id"].(string)
	drainAll(t, st, cipher, comp)
	if rec := getAs(t, srv, "bulk-key", "/v1/jobs/"+aJob); !strings.Contains(rec.Body.String(), `"complete":true`) {
		t.Fatalf("fixture: tenant A's job must be complete first: %s", rec.Body.String())
	}

	var v []string
	bRec := postBulkAs(t, srv, "bulk-key-b", `{"items":[{"item_id":"b0","address":"jane@acmecorp.com"},{"item_id":"b1","address":"bob@acmecorp.com"},{"item_id":"b2","address":"dave@acmecorp.com"}]}`)
	if bRec.Code != http.StatusAccepted {
		return append(v, fmt.Sprintf("tenant B's submission of addresses tenant A already verified yielded no work item: HTTP %d %s", bRec.Code, bRec.Body.String()))
	}
	var b map[string]any
	_ = json.Unmarshal(bRec.Body.Bytes(), &b)
	counts, _ := b["counts"].(map[string]any)
	if counts["total"] != float64(3) || counts["enqueued"] != float64(3) || counts["duplicate"] != float64(0) {
		v = append(v, fmt.Sprintf("tenant B's counts = %v, want total 3 enqueued 3 duplicate 0: another job's rows are never duplicates", counts))
	}
	bJob, _ := b["job_id"].(string)
	drainAll(t, st, cipher, comp)
	if comp.calls["jane@acmecorp.com"] != 2 || comp.calls["bob@acmecorp.com"] != 2 || comp.calls["dave@acmecorp.com"] != 1 {
		v = append(v, fmt.Sprintf("checks run = %v, want jane and bob checked once per JOB", comp.calls))
	}
	page := getResultsPage(t, srv, "bulk-key-b", bJob, "")
	if len(page.Results) != 3 {
		return append(v, fmt.Sprintf("tenant B's job has %d result(s), want 3", len(page.Results)))
	}
	for i, el := range page.Results {
		if ordinalOf(t, el) != i || el["item_id"] != fmt.Sprintf("b%d", i) || el["status"] == nil {
			v = append(v, fmt.Sprintf("tenant B's element %d = %v", i, el))
		}
	}
	if rec := getAs(t, srv, "bulk-key", "/v1/jobs/"+bJob+"/results"); rec.Code != http.StatusNotFound {
		v = append(v, fmt.Sprintf("tenant A reading tenant B's job got %d, want 404", rec.Code))
	}
	return v
}

// TestBulkItems_AnotherTenantGetsResultsForAddressesVerifiedYesterday: the A2 exit through the handler. Control: the same scenario over the old
// global scope must report that tenant B got no work item (the submission fails instead of coming back short).
func TestBulkItems_AnotherTenantGetsResultsForAddressesVerifiedYesterday(t *testing.T) {
	srv, _, cipher, st := idsServer(t, nil)
	if v := crossTenantHTTPViolations(t, srv, st, cipher); len(v) != 0 {
		t.Fatalf("A2 (no dedup across jobs or tenants) is violated:\n  %s", strings.Join(v, "\n  "))
	}

	srvOld, _, cipherOld, stOld := idsServer(t, func(s store.Store) store.Store {
		return &apiGlobalScopeStore{MemStore: s.(*store.MemStore), holder: map[string]string{}}
	})
	v := crossTenantHTTPViolations(t, srvOld, stOld, cipherOld)
	if len(v) == 0 || !strings.Contains(strings.Join(v, "\n"), "yielded no work item") {
		t.Fatalf("CONTROL: over the old global scope the scenario must report that tenant B got no work item, got %q", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// the results element, with a staged store
// ─────────────────────────────────────────────────────────────────────────────

// TestJobResults_ElementsCarryOrdinalItemIDAndDuplicateOf_AndEveryRowIsServedFromALogRow stages a page (the contract tests' stagedJobs) so the
// projection is checked without a queue: `ordinal` always, `item_id` only when set, `duplicate_of` only on a row that repeats an earlier one (and
// as 0 when it repeats row 0), and a malformed row served from ITS OWN LOG ROW — its confidence here is the staged row's 97, not any policy
// constant, which is what "served from the log" means. A malformed row whose LOG row is missing is NOT papered over with a made-up verdict: it
// fails the page, like any off-contract row.
func TestJobResults_ElementsCarryOrdinalItemIDAndDuplicateOf_AndEveryRowIsServedFromALogRow(t *testing.T) {
	good := onContractRow("wi-1")
	good.Ordinal, good.ItemID = 0, "first"
	dup := onContractRow("wi-1")
	dup.Ordinal, dup.DuplicateOf = 5, intp(0) // a duplicate of ROW 0 must say `0`, not vanish; no item_id
	logRow := store.VerificationRow{
		ID: "log-7", APIVersion: verdict.APIVersion, Status: "invalid", Reason: "invalid_syntax", Confidence: 97, Recommendation: "skip",
		PolicyKey: "cold-email", PolicyVersion: 1, Flags: map[string]bool{"from_cache": false}, Signals: map[string]any{"syntax_valid": false},
	}
	bad := store.WorkItemResult{Ordinal: 7, ItemID: "broken-input", Malformed: true, Verdict: logRow}
	jobs := &stagedJobs{rows: []store.WorkItemResult{good, dup, bad}}
	srv := stagedResultsServer(t, jobs)

	page := getResultsPage(t, srv, "results-key", stagedJobID, "")
	if len(page.Results) != 3 {
		t.Fatalf("%d elements, want 3", len(page.Results))
	}
	if page.Results[0]["ordinal"] != float64(0) || page.Results[0]["item_id"] != "first" {
		t.Errorf("element 0 = ordinal %v item_id %v, want 0 / first", page.Results[0]["ordinal"], page.Results[0]["item_id"])
	}
	if _, has := page.Results[0]["duplicate_of"]; has {
		t.Errorf("element 0 (a first row) carries duplicate_of %v", page.Results[0]["duplicate_of"])
	}
	if _, has := page.Results[1]["item_id"]; has || page.Results[1]["ordinal"] != float64(5) {
		t.Errorf("element 1 = %v, want ordinal 5 and no item_id key", page.Results[1])
	}
	if got, has := page.Results[1]["duplicate_of"]; !has || got != float64(0) {
		t.Errorf("element 1 duplicate_of = %v (present=%v), want the number 0: the first row's ordinal, present even when it is 0", got, has)
	}
	m := page.Results[2]
	if m["status"] != "invalid" || m["reason"] != "invalid_syntax" || m["recommendation"] != "skip" || m["ordinal"] != float64(7) || m["item_id"] != "broken-input" ||
		m["email"] != "" || m["domain"] != "" || m["confidence"] != float64(97) {
		t.Errorf("malformed element = %v, want its LOG row's invalid/invalid_syntax/skip/97, ordinal 7, item_id broken-input, no email or domain", m)
	}
	if _, has := m["duplicate_of"]; has {
		t.Errorf("a malformed element carries duplicate_of %v: a malformed row is never a duplicate", m["duplicate_of"])
	}

	// CONTROL 1: a malformed row with NO LOG row behind it is not served as a made-up verdict: the page fails (contract_violation), like any
	// stored row that cannot be served on contract.
	noLog := store.WorkItemResult{Ordinal: 3, ItemID: "no-log", Malformed: true}
	if rec := getResults(t, stagedResultsServer(t, &stagedJobs{rows: []store.WorkItemResult{noLog}}), stagedJobID); rec.Code != http.StatusInternalServerError {
		t.Errorf("CONTROL: a malformed row with no LOG row must fail the page, not be synthesized; got %d %s", rec.Code, rec.Body.String())
	}
	// CONTROL 2: an off-contract stored row on the same page still fails the WHOLE page, malformed row or not.
	offContract := withVerdict("wi-9", func(v *store.VerificationRow) { v.APIVersion = "" })
	srvBad := stagedResultsServer(t, &stagedJobs{rows: []store.WorkItemResult{bad, offContract}})
	if rec := getResults(t, srvBad, stagedJobID); rec.Code != http.StatusInternalServerError {
		t.Errorf("CONTROL: an off-contract row beside a malformed one must still fail the page, got %d", rec.Code)
	}
}

func intp(i int) *int { return &i }

// ─────────────────────────────────────────────────────────────────────────────
// the same flow over real Postgres
// ─────────────────────────────────────────────────────────────────────────────

// TestPGBulkItems_EndToEndOverHTTPOnAScratchDatabase runs the flow over a throwaway Postgres: labelled rows with duplicates and a malformed
// address through the real enqueuer, drainer and store; the status reaches pending 0; the results are one per row in ordinal order with item_id and
// the shared verdict; every page boundary of several limits finds each ordinal exactly once; another tenant submitting the same addresses later gets
// its own results; the old global scope (re-created as an index) makes that second submission fail. It SKIPS without TM_TEST_DATABASE_URL.
func TestPGBulkItems_EndToEndOverHTTPOnAScratchDatabase(t *testing.T) {
	dsn := testpg.Scratch(t, 0)
	pg, err := store.OpenPG(context.Background(), dsn)
	if err != nil {
		t.Fatalf("OpenPG(scratch): %v", err)
	}
	t.Cleanup(pg.Close)
	now := time.Now().UTC()
	tag := now.Format("150405.000000000")
	const userA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const userB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	keyA, keyB := "ids-a-"+tag, "ids-b-"+tag
	seedTenantKey(t, pg, keyA, "caller_ids_a_"+tag, userA)
	seedTenantKey(t, pg, keyB, "caller_ids_b_"+tag, userB)
	srv, cipher := tenancyServer(t, pg, now)

	body := `{"items":[
		{"item_id":"p0","address":"jane@acmecorp.com"},
		{"item_id":"p1","address":"bob@acmecorp.com"},
		{"item_id":"p2","address":"Jane@acmecorp.com"},
		{"item_id":"p3","address":"not-an-address"},
		{"item_id":"p4","address":"jane@acmecorp.com"},
		{"item_id":"p5","address":"carol@acmecorp.com"},
		{"item_id":"p6","address":"carol@acmecorp.com"}]}`
	resp := mustAccept(t, postBulkAs(t, srv, keyA, body))
	aJob, _ := resp["job_id"].(string)
	counts, _ := resp["counts"].(map[string]any)
	if counts["total"] != float64(7) || counts["enqueued"] != float64(3) || counts["duplicate"] != float64(3) || counts["malformed"] != float64(1) {
		t.Fatalf("202 counts = %v, want total 7 enqueued 3 duplicate 3 malformed 1", counts)
	}
	// Door 2 over real Postgres: the malformed row's LOG row was committed with the job, before the 202 — one row in the whole (scratch) LOG, the sync
	// path's invalid_syntax row with NULL email_hash and hash_scheme.
	var logged int
	if err := pg.Pool().QueryRow(context.Background(), `SELECT count(*)::int FROM verify.verifications WHERE status='invalid' AND reason='invalid_syntax' AND email_hash IS NULL AND hash_scheme IS NULL AND NOT from_cache`).Scan(&logged); err != nil || logged != 1 {
		t.Fatalf("invalid_syntax LOG rows right after the 202 = %d (err %v), want exactly 1", logged, err)
	}
	if err := pg.Pool().QueryRow(context.Background(), `SELECT count(*)::int FROM verify.verifications`).Scan(&logged); err != nil || logged != 1 {
		t.Fatalf("LOG rows right after the 202 = %d (err %v), want exactly 1 (the addressable rows are logged when they settle)", logged, err)
	}
	compA := &itemsComputer{pgSettleComputer: pgSettleComputer{now: now}}
	drainPG(t, pg, cipher, compA, now)
	if compA.calls["jane@acmecorp.com"] != 1 || compA.calls["bob@acmecorp.com"] != 1 || compA.calls["carol@acmecorp.com"] != 1 || len(compA.calls) != 3 {
		t.Fatalf("checks run = %v, want one check each for jane, bob and carol", compA.calls)
	}

	var status struct {
		Complete bool           `json:"complete"`
		Counts   map[string]int `json:"counts"`
	}
	rec := getAs(t, srv, keyA, "/v1/jobs/"+aJob)
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil || !status.Complete || status.Counts["total"] != 7 || status.Counts["settled"] != 7 || status.Counts["pending"] != 0 {
		t.Fatalf("status = %s (err %v), want complete 7/7 pending 0", rec.Body.String(), err)
	}
	page := getResultsPage(t, srv, keyA, aJob, "")
	if len(page.Results) != 7 {
		t.Fatalf("%d results, want 7", len(page.Results))
	}
	for i, el := range page.Results {
		if ordinalOf(t, el) != i || el["item_id"] != fmt.Sprintf("p%d", i) {
			t.Errorf("element %d = ordinal %v item_id %v", i, el["ordinal"], el["item_id"])
		}
	}
	if page.Results[3]["reason"] != "invalid_syntax" || page.Results[3]["status"] != "invalid" {
		t.Errorf("the malformed row = %v/%v, want invalid/invalid_syntax", page.Results[3]["status"], page.Results[3]["reason"])
	}
	if page.Results[0]["confidence"] != page.Results[2]["confidence"] || page.Results[0]["confidence"] != page.Results[4]["confidence"] || page.Results[5]["confidence"] != page.Results[6]["confidence"] {
		t.Error("rows that share a check must carry its verdict")
	}
	// duplicate_of over Postgres: rows 2 and 4 repeat row 0 (the FIRST row, not each other), row 6 repeats row 5; nothing else says so, the malformed row included.
	if v := duplicateVisibilityViolations(t, srv, keyA, aJob, 7, map[int]int{2: 0, 4: 0, 6: 5}); len(v) != 0 {
		t.Errorf("duplicate_of over Postgres: %s", strings.Join(v, "; "))
	}
	want := []int{0, 1, 2, 3, 4, 5, 6}
	for _, limit := range []int{1, 2, 3, 6, 7, 100} {
		if v := exactOnce(walkHTTP(t, srv, keyA, aJob, limit), want); len(v) != 0 {
			t.Errorf("limit %d over Postgres: %s", limit, strings.Join(v, "; "))
		}
	}

	// Tenant B, a day later, submits the same addresses: its own work items, checks and results.
	bResp := mustAccept(t, postBulkAs(t, srv, keyB, `{"items":[{"item_id":"b0","address":"jane@acmecorp.com"},{"item_id":"b1","address":"bob@acmecorp.com"}]}`))
	bJob, _ := bResp["job_id"].(string)
	if c := bResp["counts"].(map[string]any); c["enqueued"] != float64(2) || c["duplicate"] != float64(0) {
		t.Fatalf("tenant B's counts = %v, want enqueued 2 duplicate 0", c)
	}
	drainPG(t, pg, cipher, compA, now)
	if compA.calls["jane@acmecorp.com"] != 2 || compA.calls["bob@acmecorp.com"] != 2 {
		t.Errorf("checks run = %v, want jane and bob checked once per job (2)", compA.calls)
	}
	if got := getResultsPage(t, srv, keyB, bJob, ""); len(got.Results) != 2 || got.Results[0]["item_id"] != "b0" {
		t.Errorf("tenant B's results = %v, want its own 2 rows", got.Results)
	}
	if rec := getAs(t, srv, keyA, "/v1/jobs/"+bJob+"/results"); rec.Code != http.StatusNotFound {
		t.Errorf("tenant A reading tenant B's job got %d, want 404", rec.Code)
	}
	// The paired control for "tenant B gets results" over Postgres (the old global unique index re-created next to the new ones, after which the
	// second tenant's enqueue is refused) is TestEnqueueScope_ControlTheOldGlobalScopeFailsTheSameScenario in internal/store; over the handler it is
	// TestBulkItems_AnotherTenantGetsResultsForAddressesVerifiedYesterday.
}

// drainPG runs a real drainer over the scratch database's queue.
func drainPG(t *testing.T, pg *store.PG, cipher *queue.AddrCipher, comp queue.Computer, now time.Time) {
	t.Helper()
	dr, err := queue.NewDrainer(queue.Deps{
		Store: pg, Compute: comp, Policy: fixedPolicy{}, Cipher: cipher,
		Backoff:  queue.Backoff{Base: time.Minute, Factor: 2, Cap: time.Hour, RateWait: time.Second},
		Now:      func() time.Time { return now },
		WorkerID: "ids-pg-drainer", LeaseTTL: time.Minute, MaxSMTPDeadline: 10 * time.Second,
		MaxAttempts: 3, MaxRetryAge: time.Hour, BatchSize: 1000, ReclaimEvery: time.Hour,
	})
	if err != nil {
		t.Fatalf("drainer: %v", err)
	}
	for i := 0; i < 20; i++ {
		if !dr.TickOnce(context.Background()) {
			return
		}
	}
	t.Fatal("the drainer was still finding work after 20 ticks")
}
