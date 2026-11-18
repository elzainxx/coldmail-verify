package api

// job_input.go — the small input rules and response builders that belong to the three additive changes of the partial Tier 4 and the bad-input
// fix (owner rulings OD-6/OD-8, close-out items A1 and A3):
//
//   - isUUID: a job id in a path that is not a UUID is a 400 before the database is touched (A3);
//   - idempotencyKeyOf: the optional Idempotency-Key header of POST /v1/verify/bulk (A1);
//   - checkTotals / totalsJSON: the `totals` object of GET /v1/jobs/{id} (A1).

import (
	"fmt"
	"net/http"

	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// isUUID reports whether s is a UUID in its canonical 8-4-4-4-12 form (hex digits of either case). Job ids are issued by this service as lower-case
// UUIDv7s (internal/idgen), and the jobs.id column is a Postgres uuid, which REFUSES anything it cannot parse; checking the shape here is what keeps
// that refusal from becoming a 500. Postgres also accepts a few other spellings (braces, no hyphens); this service issues only the canonical one
// and answers only for it, so the others are a 400 like any other non-UUID.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}

const (
	// idempotencyKeyHeader is the request header that makes POST /v1/verify/bulk idempotent (0019).
	idempotencyKeyHeader = "Idempotency-Key"
	// idempotentReplayedHeader is the response header set to "true" on an answer that is a replay of an earlier one (0019). Absent otherwise.
	idempotentReplayedHeader = "Idempotent-Replayed"
	// maxIdempotencyKeyLen is the longest accepted key; the jobs table's CHECK (0019, jobs_idempotency_shape) says the same.
	maxIdempotencyKeyLen = 128
)

// validIdempotencyKey reports whether k is between 1 and maxIdempotencyKeyLen bytes long, every byte one of the letters, digits, dot, underscore, colon or hyphen.
func validIdempotencyKey(k string) bool {
	if k == "" || len(k) > maxIdempotencyKeyLen {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-') {
			return false
		}
	}
	return true
}

// idempotencyKeyOf reads the Idempotency-Key header. present is false when the header is absent (today's behaviour, unchanged). ok is false when it
// is present and not exactly one valid value: an empty value, a value outside [A-Za-z0-9._:-], one over 128 characters, or the header given more
// than once. The value is returned as the client sent it (net/http has already trimmed surrounding spaces).
func idempotencyKeyOf(r *http.Request) (key string, present, ok bool) {
	vals := r.Header.Values(idempotencyKeyHeader)
	switch len(vals) {
	case 0:
		return "", false, true
	case 1:
		if !validIdempotencyKey(vals[0]) {
			return "", true, false
		}
		return vals[0], true, true
	default:
		return "", true, false
	}
}

// totalsJSON renders a job's totals. by_status always carries all four verdict statuses (zero included), keyed by the vocabulary the code owns, so
// a caller never has to guess whether a missing key means zero.
func totalsJSON(t store.JobTotals) map[string]any {
	by := make(map[string]int, 4)
	for _, st := range vocab.Statuses() {
		by[string(st)] = t.ByStatus[string(st)]
	}
	return map[string]any{
		"submitted":  t.Submitted,
		"settled":    t.Settled,
		"pending":    t.Pending,
		"duplicates": t.Duplicates,
		"malformed":  t.Malformed,
		"by_status":  by,
	}
}

// checkTotals verifies the identities the contract promises before the totals are served: submitted = settled + pending, settled = the sum of
// by_status, and every number is a count. A store that breaks one is answered with contract_violation rather than served: a total a caller bills
// on must not be one this service knows to be wrong.
func checkTotals(t store.JobTotals) error {
	sum := 0
	for k, n := range t.ByStatus {
		if !vocab.ValidStatus(vocab.Status(k)) {
			return fmt.Errorf("by_status carries %q, which is not a verdict status", k)
		}
		if n < 0 {
			return fmt.Errorf("by_status[%s] is negative", k)
		}
		sum += n
	}
	switch {
	case t.Submitted < 0 || t.Settled < 0 || t.Pending < 0 || t.Duplicates < 0 || t.Malformed < 0:
		return fmt.Errorf("a total is negative")
	case t.Submitted != t.Settled+t.Pending:
		return fmt.Errorf("submitted %d != settled %d + pending %d", t.Submitted, t.Settled, t.Pending)
	case sum != t.Settled:
		return fmt.Errorf("sum(by_status) %d != settled %d", sum, t.Settled)
	case t.Duplicates+t.Malformed > t.Submitted:
		return fmt.Errorf("duplicates %d + malformed %d exceed submitted %d", t.Duplicates, t.Malformed, t.Submitted)
	}
	return nil
}
