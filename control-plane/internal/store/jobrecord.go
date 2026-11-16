package store

// jobrecord.go — the parts of RecordJob that do not depend on Postgres: the consistency rule between a job's rows and the LOG rows of its
// malformed rows (shared by the Mem fake and the pgx store, so neither can be laxer than the other), and the chunk arithmetic that keeps a
// 100,000-row submission to a bounded number of statements.

import "fmt"

// checkRejectLinks judges what the database cannot see (it holds no foreign key to verifications, by design): the rows and the LOG rows of
// a RecordJob call must agree. Every row that names a verification_id must name one of the rejects, no two rows may name the same reject,
// and every reject must be named by a row — a LOG row nobody points at would be an orphan, and a row pointing at nothing would be served
// as a verdict that was never logged (the Door-2 breach this exists to prevent). Each reject must carry an id.
//
// It deliberately does NOT judge the shape rules the database CHECKs (a malformed row has a verification_id and no work item, a
// duplicate_of below the ordinal, ...): those are enforced by Postgres, mirrored by the Mem fake, and tested against both.
func checkRejectLinks(rows []JobRowEntry, rejects []VerificationRow) error {
	byID := make(map[string]bool, len(rejects))
	for _, r := range rejects {
		if r.ID == "" {
			return fmt.Errorf("store: record job: a reject LOG row has no id")
		}
		// byID's VALUE means "already referenced by a row", so presence (not the value) is what says "seen before": testing the value
		// here let a reject written twice through, because at this point nothing has been referenced yet and both copies read false.
		if _, seen := byID[r.ID]; seen {
			return fmt.Errorf("store: record job: reject LOG row %s appears twice", r.ID)
		}
		byID[r.ID] = false // false = not yet referenced by a row
	}
	for _, row := range rows {
		if row.VerificationID == "" {
			continue
		}
		referenced, known := byID[row.VerificationID]
		switch {
		case !known:
			return fmt.Errorf("store: record job: row %d names verification %s, which is not among the LOG rows being written", row.Ordinal, row.VerificationID)
		case referenced:
			return fmt.Errorf("store: record job: row %d names verification %s, which another row already uses", row.Ordinal, row.VerificationID)
		}
		byID[row.VerificationID] = true
	}
	for id, referenced := range byID {
		if !referenced {
			return fmt.Errorf("store: record job: LOG row %s is written for no row of the job", id)
		}
	}
	return nil
}

// chunkSpans splits n items into consecutive [start, end) spans of at most size items. n <= 0 yields none. It is the one place the
// chunk count of a bulk write is decided, so a test can state it: 100,000 rows are 100 spans of 1000, or 200 of 500.
func chunkSpans(n, size int) [][2]int {
	if n <= 0 || size <= 0 {
		return nil
	}
	spans := make([][2]int, 0, (n+size-1)/size)
	for start := 0; start < n; start += size {
		end := start + size
		if end > n {
			end = n
		}
		spans = append(spans, [2]int{start, end})
	}
	return spans
}
