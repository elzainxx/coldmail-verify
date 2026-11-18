package vocab

// This file: the vocabularies of the bulk-job surface that other systems code against, declared once beside the frozen verdict enums so that
// GET /v1/describe can publish them FROM THESE CONSTANTS and a test can compare them with the database CHECK that mirrors them. Nothing here is a
// Door-4 enum (status / reason / recommendation); the same rule of the door applies anyway: a value is never renamed, removed or repurposed.

// JobStatus is the `status` word of a bulk job on GET /v1/jobs/{id}. The database CHECK (migration 0009, valid_job_status) names exactly these
// four. The code writes only JobRunning (internal/queue/enqueue.go) and the status route DERIVES JobComplete from the work items
// (internal/api handleJobStatus), so a caller branches on the boolean `complete`, never on this word.
type JobStatus string

const (
	JobSubmitted            JobStatus = "submitted"
	JobRunning              JobStatus = "running"
	JobComplete             JobStatus = "complete"
	JobCompleteWithUnknowns JobStatus = "complete_with_unknowns"
)

// JobStatuses returns every legal job status in declaration order.
func JobStatuses() []JobStatus {
	return []JobStatus{JobSubmitted, JobRunning, JobComplete, JobCompleteWithUnknowns}
}

// SubmitAccepted is the `status` of the 202 answer to POST /v1/verify/bulk (the only value it takes).
const SubmitAccepted = "accepted"

// Statuses returns every legal verdict status in the order the vocabulary declares them (valid, invalid, risky, unknown).
func Statuses() []Status {
	return []Status{StatusValid, StatusInvalid, StatusRisky, StatusUnknown}
}

// Recommendations returns every legal recommendation in declaration order (send, review, skip).
func Recommendations() []Recommendation {
	return []Recommendation{RecSend, RecReview, RecSkip}
}

// FailureOrigins returns the three legal signals.failure_origin values in declaration order (own, remote, undetermined).
func FailureOrigins() []FailureOrigin {
	return []FailureOrigin{OriginOwn, OriginRemote, OriginUndetermined}
}
