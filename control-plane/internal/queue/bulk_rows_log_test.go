package queue

// The Door-2 and duplicate_of halves of the bulk enqueuer, through the REAL enqueuer and drainer over both stores (see bulk_rows_test.go for the
// shared fixtures): every malformed row is logged before the submission returns, with the row the sync path writes; a duplicate names the first row
// of its address; a repeated malformed address is not a duplicate.

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/orchestrator"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/store"
)

func intPtr(i int) *int { return &i }

func samePtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// rowsDefinition is the real default policy (its invalid_syntax baseline is what a malformed row's confidence comes from).
func rowsDefinition(t *testing.T) *policy.Definition {
	t.Helper()
	def, err := policy.ColdEmailDefinition()
	if err != nil {
		t.Fatalf("ColdEmailDefinition: %v", err)
	}
	return def
}

// logRowCount is the number of LOG rows in a store that holds nothing else of the kind: Mem's append-only Log, or a scratch Postgres's
// verifications. Pass the BASE store, not a wrapper.
func logRowCount(t *testing.T, st store.Store) int {
	t.Helper()
	switch s := st.(type) {
	case *store.MemStore:
		return len(s.Log)
	case *store.PG:
		var n int
		if err := s.Pool().QueryRow(context.Background(), `SELECT count(*)::int FROM verify.verifications`).Scan(&n); err != nil {
			t.Fatalf("count verifications: %v", err)
		}
		return n
	}
	t.Fatalf("logRowCount: unsupported store %T (pass the base store, not a wrapper)", st)
	return 0
}

// TestSubmitBulkRows_ADuplicateNamesTheFirstRow_AndARepeatedMalformedAddressIsNotADuplicate: an address repeated three times — in different
// spellings — makes rows 2 and 5 say duplicate_of 0 (the FIRST row, not the previous duplicate), and the first row says nothing; a second address
// repeated once points at ITS first row; a malformed address repeated three times is three malformed rows with three LOG rows and NO duplicate_of
// and does not count as a duplicate; the check ran once per distinct address; and the listing is exact across page boundaries inside the runs.
func TestSubmitBulkRows_ADuplicateNamesTheFirstRow_AndARepeatedMalformedAddressIsNotADuplicate(t *testing.T) {
	bulkStores(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		enq, cipher := newBulkEnqueuer(t, st)
		rows := []BulkRow{
			{ItemID: "r0", Address: "x@acmecorp.com"},   // 0  first of x
			{ItemID: "r1", Address: "nope"},             // 1  malformed
			{ItemID: "r2", Address: "X@AcmeCorp.com"},   // 2  duplicate of 0
			{ItemID: "r3", Address: "nope"},             // 3  the SAME garbage: its own malformed row, NOT a duplicate of 1
			{ItemID: "r4", Address: "y@acmecorp.com"},   // 4  first of y
			{ItemID: "r5", Address: " x@acmecorp.com "}, // 5  duplicate of 0 (not of 2)
			{ItemID: "r6", Address: "nope"},             // 6  malformed again
			{ItemID: "r7", Address: "y@acmecorp.com"},   // 7  duplicate of 4
		}
		res, err := enq.SubmitBulkRows(ctx, rows, rowsDefinition(t), "ref", rowsOwnerA)
		if err != nil {
			t.Fatalf("SubmitBulkRows: %v", err)
		}
		if res.Total != 8 || res.Enqueued != 2 || res.Duplicate != 3 || res.Malformed != 3 {
			t.Fatalf("counts = total %d enqueued %d duplicate %d malformed %d, want 8/2/3/3: a repeated malformed address is NOT a duplicate", res.Total, res.Enqueued, res.Duplicate, res.Malformed)
		}
		if n := logRowCount(t, st); n != 3 {
			t.Fatalf("%d LOG rows right after the submission, want 3: each malformed row is its own row with its own LOG row", n)
		}
		comp := &bulkCountingComputer{settleE2EComputer: settleE2EComputer{now: rowsNow}}
		drainEverything(t, st, cipher, comp)
		if comp.calls["x@acmecorp.com"] != 1 || comp.calls["y@acmecorp.com"] != 1 || len(comp.calls) != 2 {
			t.Fatalf("checks run = %v, want exactly one check each for x and y", comp.calls)
		}

		wantDup := map[int]*int{2: intPtr(0), 5: intPtr(0), 7: intPtr(4)}
		got := allResults(t, st, res.JobID, rowsOwnerA)
		if len(got) != 8 {
			t.Fatalf("%d results, want 8", len(got))
		}
		seenLog := map[string]bool{}
		for i, r := range got {
			if !samePtr(r.DuplicateOf, wantDup[i]) {
				t.Errorf("row %d duplicate_of = %v, want %v", i, r.DuplicateOf, wantDup[i])
			}
			if r.Malformed {
				if seenLog[r.Verdict.ID] || r.Verdict.ID == "" {
					t.Errorf("malformed row %d shares or lacks its LOG row (%q): each malformed row has its own", i, r.Verdict.ID)
				}
				seenLog[r.Verdict.ID] = true
			}
		}
		if len(seenLog) != 3 {
			t.Errorf("%d distinct LOG rows across the 3 malformed rows, want 3", len(seenLog))
		}
		// Rows 0, 2 and 5 are ONE check and carry its verdict; 4 and 7 likewise.
		if got[0].Verdict.ID != got[2].Verdict.ID || got[0].Verdict.ID != got[5].Verdict.ID || got[4].Verdict.ID != got[7].Verdict.ID || got[0].Verdict.ID == got[4].Verdict.ID {
			t.Errorf("verdict ids by row: %q %q %q %q, want rows 0/2/5 to share one and 4/7 another", got[0].Verdict.ID, got[2].Verdict.ID, got[5].Verdict.ID, got[4].Verdict.ID)
		}
		// Pagination across the runs: every ordinal exactly once, in order, duplicate_of intact on every page.
		for _, limit := range []int{1, 2, 3, 5, 8, 9} {
			var ordinals []int
			cursor := ""
			for pages := 0; pages < 20; pages++ {
				page, next, err := st.ListJobResults(ctx, res.JobID, rowsOwnerA, cursor, limit)
				if err != nil {
					t.Fatalf("limit %d: %v", limit, err)
				}
				for _, r := range page {
					ordinals = append(ordinals, r.Ordinal)
					if !samePtr(r.DuplicateOf, wantDup[r.Ordinal]) {
						t.Errorf("limit %d: ordinal %d duplicate_of = %v, want %v", limit, r.Ordinal, r.DuplicateOf, wantDup[r.Ordinal])
					}
				}
				if next == "" {
					break
				}
				cursor = next
			}
			want := []int{0, 1, 2, 3, 4, 5, 6, 7}
			if !reflect.DeepEqual(ordinals, want) {
				t.Errorf("limit %d returned ordinals %v, want %v: every ordinal exactly once and in order", limit, ordinals, want)
			}
		}
	})
}

// TestSubmitBulkRows_AMalformedRowIsLoggedLikeTheSyncPathLogsIt compares, field by field, the LOG row a bulk submission writes for an
// un-addressable input with the one the SYNC path (a real orchestrator.Verify) writes for the same input: Door 2 asks for the same thing in
// both places, and the sync path is the reference ("same provenance fields the sync path writes", from_cache false, NULL hash). The control
// gives the policy a different invalid_syntax baseline and shows the comparison follows it on both sides (so the confidence is not a constant).
func TestSubmitBulkRows_AMalformedRowIsLoggedLikeTheSyncPathLogsIt(t *testing.T) {
	ctx := context.Background()
	compare := func(def *policy.Definition) []string {
		bulkMem := store.NewMem(func() time.Time { return rowsNow })
		enq, _ := newBulkEnqueuer(t, bulkMem)
		if _, err := enq.SubmitBulkRows(ctx, []BulkRow{{ItemID: "x", Address: "not-an-address"}}, def, "", rowsOwnerA); err != nil {
			t.Fatalf("SubmitBulkRows: %v", err)
		}
		syncMem := store.NewMem(func() time.Time { return rowsNow })
		o := orchestrator.New(orchestrator.Deps{Hasher: enq.Hasher, Store: syncMem, ConfigFingerprint: enq.ConfigFingerprint, Now: func() time.Time { return rowsNow }})
		if _, err := o.Verify(ctx, "not-an-address", def); err != nil {
			t.Fatalf("sync Verify: %v", err)
		}
		if len(bulkMem.Log) != 1 || len(syncMem.Log) != 1 {
			t.Fatalf("LOG rows: bulk %d, sync %d, want 1 each", len(bulkMem.Log), len(syncMem.Log))
		}
		a, b := bulkMem.Log[0], syncMem.Log[0]
		var diff []string
		check := func(name string, x, y any) {
			if !reflect.DeepEqual(x, y) {
				diff = append(diff, fmt.Sprintf("%s: bulk %v, sync %v", name, x, y))
			}
		}
		check("status", a.Status, b.Status)
		check("reason", a.Reason, b.Reason)
		check("confidence", a.Confidence, b.Confidence)
		check("recommendation", a.Recommendation, b.Recommendation)
		check("flags", a.Flags, b.Flags)
		check("signals", a.Signals, b.Signals)
		check("email_hash length", len(a.EmailHash), len(b.EmailHash))
		check("hash_scheme", a.HashScheme, b.HashScheme)
		check("domain", a.Domain, b.Domain)
		check("mx_provider", a.MXProvider, b.MXProvider)
		check("policy", [2]any{a.PolicyKey, a.PolicyVersion}, [2]any{b.PolicyKey, b.PolicyVersion})
		check("api_version", a.APIVersion, b.APIVersion)
		check("config_fingerprint", a.ConfigFingerprint, b.ConfigFingerprint)
		check("from_cache", a.FromCache, b.FromCache)
		check("checked_at", a.CheckedAt, b.CheckedAt)
		check("local_part_len", a.LocalPartLen, b.LocalPartLen)
		check("latency_ms", a.LatencyMS, b.LatencyMS)
		check("node, ip, engine", [5]any{a.NodeID, a.VerifierIP, a.EngineVersion, a.EngineCoreVersion, a.EngineValidationType}, [5]any{b.NodeID, b.VerifierIP, b.EngineVersion, b.EngineCoreVersion, b.EngineValidationType})
		if a.ID == "" || a.RequestID == "" {
			diff = append(diff, "the bulk LOG row has no id / request_id")
		}
		return diff
	}
	def := rowsDefinition(t)
	if diff := compare(def); len(diff) != 0 {
		t.Fatalf("a malformed row's LOG row differs from the sync path's for the same input:\n  %s", strings.Join(diff, "\n  "))
	}
	// CONTROL: with another baseline for invalid_syntax both rows follow it, and the default's confidence is not that number.
	custom := *def
	custom.Confidence.BaselineByReason = map[string]int{"invalid_syntax": 42}
	if diff := compare(&custom); len(diff) != 0 {
		t.Fatalf("with a custom invalid_syntax baseline the two paths must still agree: %v", diff)
	}
	if RejectConfidence(&custom) != 42 || RejectConfidence(def) == 42 {
		t.Errorf("CONTROL: RejectConfidence must follow the policy baseline (custom=%d default=%d)", RejectConfidence(&custom), RejectConfidence(def))
	}
}
