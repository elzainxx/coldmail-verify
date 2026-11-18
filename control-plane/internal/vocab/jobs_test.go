package vocab

import "testing"

// The ordered lists that describe publishes (Statuses, Recommendations, FailureOrigins) are second spellings of the validity sets the guards use
// (statuses, recommendations, failureOrigins). They must be the SAME sets, in both directions, or a value added to one is silently missing from the other:
// describe would then publish a vocabulary the guards reject, or a guard would accept a value describe never mentions.
func TestOrderedListsAreExactlyTheValiditySets(t *testing.T) {
	sameSet := func(name string, listed []string, valid func(string) bool, size int) []string {
		var p []string
		seen := map[string]bool{}
		for _, v := range listed {
			if seen[v] {
				p = append(p, name+": "+v+" is listed twice")
			}
			seen[v] = true
			if !valid(v) {
				p = append(p, name+": the list holds "+v+", which the guard rejects")
			}
		}
		if len(seen) != size {
			p = append(p, name+": the list has a different number of values than the validity set")
		}
		return p
	}
	strs := func(n int, at func(int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = at(i)
		}
		return out
	}
	st, rc, or := Statuses(), Recommendations(), FailureOrigins()
	checks := map[string][]string{
		"statuses": sameSet("Statuses", strs(len(st), func(i int) string { return string(st[i]) }), func(s string) bool { return ValidStatus(Status(s)) }, len(statuses)),
		"recommendations": sameSet("Recommendations", strs(len(rc), func(i int) string { return string(rc[i]) }),
			func(s string) bool { return ValidRecommendation(Recommendation(s)) }, len(recommendations)),
		"failure origins": sameSet("FailureOrigins", strs(len(or), func(i int) string { return string(or[i]) }),
			func(s string) bool { return ValidFailureOrigin(FailureOrigin(s)) }, len(failureOrigins)),
	}
	for name, p := range checks {
		if len(p) != 0 {
			t.Errorf("%s: %v", name, p)
		}
	}

	// PAIRED CONTROL: the comparison is red when a list drops a value or a guard set gains one.
	short := []string{"valid", "invalid", "risky"} // `unknown` dropped
	if p := sameSet("Statuses", short, func(s string) bool { return ValidStatus(Status(s)) }, len(statuses)); len(p) == 0 {
		t.Error("control: a list missing a value of the validity set was not reported")
	}
	extra := []string{"valid", "invalid", "risky", "unknown", "maybe"}
	if p := sameSet("Statuses", extra, func(s string) bool { return ValidStatus(Status(s)) }, len(statuses)); len(p) == 0 {
		t.Error("control: a list holding a value the guard rejects was not reported")
	}
}

// The job statuses are the database CHECK's four words (migration 0009, valid_job_status; the CHECK is compared with this list on Postgres by
// TestVocabMirror_JobStatuses in internal/store). Frozen: a rename or removal would break every caller that branches on them.
func TestJobStatusesAreTheFourFrozenWords(t *testing.T) {
	got := map[string]bool{}
	for _, s := range JobStatuses() {
		got[string(s)] = true
	}
	for _, want := range []string{"submitted", "running", "complete", "complete_with_unknowns"} {
		if !got[want] {
			t.Errorf("job status %q is missing", want)
		}
	}
	if len(got) != 4 || len(JobStatuses()) != 4 {
		t.Errorf("JobStatuses = %v, want exactly the four frozen words", JobStatuses())
	}
	if SubmitAccepted != "accepted" {
		t.Errorf("SubmitAccepted = %q", SubmitAccepted)
	}
}
