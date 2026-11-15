package api

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// READINESS MUST INCLUDE THE QUEUE WHEN THE QUEUE WAS DECLARED (WithQueueStatus).
//
// THE DEFECT THIS FILE EXISTS FOR. main.go's buildQueue returns early when TM_QUEUE_ADDR_KEY is not
// set (and when the cipher or the drainer cannot be built). The bulk/job routes then answer 503
// `unavailable` to an authenticated caller, yet /readyz said `200 ready`, because it only checked the
// pepper and the DB ping. An orchestrator or the platform that trusts /readyz sent traffic to a module
// whose job routes could not work — the readiness-vs-serving drift item 10 closed for the DB, one
// dependency over (see readyz_test.go).
//
// THE SHAPE OF THE FIX, and why each test below exists:
//
//   - A server that never called WithQueueStatus must answer EXACTLY as before. Many tests, and every
//     build smoke, build servers without a queue; if declaring were the default they would all flip to
//     503. The back-compat test pins that no `queue` check and no `reasons` key appear on such a body,
//     and pins the full key set so a future field cannot be added to the old shape unnoticed.
//   - The declared-not-ready test is paired with a declared-ready control in the same file, so a
//     handler hardcoded to not-ready (or to ready) cannot satisfy both.
//   - The checks stay independent: a missing pepper is reported as pepper:false even with a healthy
//     queue, and a not-ready queue does not make the pepper or db check lie. The 8-way table pins it.

// queueReasonText is what main.go's buildQueue declares when the key is missing. It is repeated here
// as a LITERAL on purpose: this package cannot import main, and the test in cmd/control-plane
// (TestBuildQueue_*) pins that main's real text is the one operators read in the runbook. Here it is
// only a realistic payload to carry through the handler.
const queueReasonText = "TM_QUEUE_ADDR_KEY is not set: the bulk and job routes answer 503 until it is"

// oldReadyzTopLevel / oldReadyzChecks are the key sets /readyz had BEFORE the queue was added. A server
// that never declares a queue must still emit exactly these and no more.
var (
	oldReadyzTopLevel = []string{"at", "checks", "config", "pepper", "ready", "status", "supabaseKey"}
	oldReadyzChecks   = []string{"db", "pepper"}
)

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedCheckKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// reasonsOf returns body["reasons"] as []string, failing if it is present but not an array of
// strings. present=false means the key is absent (which is what a ready body must have).
func reasonsOf(t *testing.T, body map[string]any) (reasons []string, present bool) {
	t.Helper()
	raw, ok := body["reasons"]
	if !ok {
		return nil, false
	}
	arr, ok := raw.([]any)
	if !ok {
		t.Fatalf("`reasons` must be a JSON array of strings, got %T (%v)", raw, raw)
	}
	for _, v := range arr {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("`reasons` must hold strings only, got %T (%v)", v, v)
		}
		reasons = append(reasons, s)
	}
	return reasons, true
}

// TestReadyzQueue_DeclaredNotReadyIs503WithTheReason is THE defect: pepper present, DB reachable (so
// the OLD predicate says ready), queue declared not-ready. Must be 503 not_ready, with the queue check
// false and the reason named — and with pepper/db still TRUE, so the 503 provably comes from the queue
// and not from a dependency that happens to be down too.
func TestReadyzQueue_DeclaredNotReadyIs503WithTheReason(t *testing.T) {
	srv, _ := readyzServer(t, true, &fakePinger{})
	srv.WithQueueStatus(false, queueReasonText)
	rec, body := getReadyz(t, srv)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("declared-not-ready queue => 503, got %d (body %s)", rec.Code, rec.Body.String())
	}
	if body["status"] != "not_ready" {
		t.Errorf("status must be not_ready, got %v", body["status"])
	}
	if body["ready"] != false {
		t.Errorf("the pre-existing `ready` boolean must agree: want false, got %v", body["ready"])
	}
	ch := checksOf(t, body)
	q, present := ch["queue"]
	if !present {
		t.Fatalf("checks.queue must be PRESENT once declared (an absent check reads as 'nothing wrong'): %v", body)
	}
	if q {
		t.Errorf("checks.queue must be false, got true")
	}
	if !ch["pepper"] || !ch["db"] {
		t.Errorf("pepper and db must still be TRUE here — otherwise the 503 is not the queue's doing and this test is vacuous: %v", ch)
	}
	reasons, ok := reasonsOf(t, body)
	if !ok {
		t.Fatalf("a not-ready queue must add a top-level `reasons`: %s", rec.Body.String())
	}
	if len(reasons) != 1 || reasons[0] != "queue: "+queueReasonText {
		t.Errorf("reasons must be exactly [\"queue: <reason>\"], got %q", reasons)
	}
	if !strings.Contains(reasons[0], "TM_QUEUE_ADDR_KEY") {
		t.Errorf("the reason must name the missing input so the operator knows what to set: %q", reasons[0])
	}
}

// TestReadyzQueue_DeclaredReadyIs200AndHasNoReasons is the CONTROL for the test above: same pepper,
// same pinger, same server construction — only the queue declaration differs. Without it, a handler
// hardcoded to `not_ready` once a queue is declared would pass every assertion in the file.
func TestReadyzQueue_DeclaredReadyIs200AndHasNoReasons(t *testing.T) {
	srv, _ := readyzServer(t, true, &fakePinger{})
	srv.WithQueueStatus(true, "")
	rec, body := getReadyz(t, srv)

	if rec.Code != http.StatusOK || body["status"] != "ready" || body["ready"] != true {
		t.Errorf("declared-ready queue + pepper + db => ready/200, got %d %v ready=%v (body %s)", rec.Code, body["status"], body["ready"], rec.Body.String())
	}
	ch := checksOf(t, body)
	if q, present := ch["queue"]; !present || !q {
		t.Errorf("checks.queue must be present and TRUE once declared ready, got present=%v value=%v", present, q)
	}
	if _, present := body["reasons"]; present {
		t.Errorf("`reasons` must be OMITTED from a ready body (not an empty array): %s", rec.Body.String())
	}
}

// TestReadyzQueue_NeverDeclaredLeavesTheBodyAsItWas is the back-compat control. Adding the feature
// must not be able to flip an old server: no `queue` check, no `reasons`, and the key sets identical
// to the pre-queue shape — checked on a READY body (200) and on a NOT-ready one (db down), because a
// `reasons` that appeared only on failure would slip past a ready-only assertion.
func TestReadyzQueue_NeverDeclaredLeavesTheBodyAsItWas(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pingErr  error
		wantCode int
	}{
		{"ready", nil, http.StatusOK},
		{"db down", errors.New("down"), http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := readyzServer(t, true, &fakePinger{err: tc.pingErr}) // never calls WithQueueStatus
			rec, body := getReadyz(t, srv)

			if rec.Code != tc.wantCode {
				t.Fatalf("a never-declared server must behave as before: want %d, got %d (body %s)", tc.wantCode, rec.Code, rec.Body.String())
			}
			if _, present := body["queue"]; present {
				t.Errorf("no top-level `queue` key may appear: %s", rec.Body.String())
			}
			if _, present := body["reasons"]; present {
				t.Errorf("no `reasons` key may appear on a never-declared server: %s", rec.Body.String())
			}
			if got := sortedKeys(body); !equalStrings(got, oldReadyzTopLevel) {
				t.Errorf("top-level keys must be exactly the pre-queue set %v, got %v", oldReadyzTopLevel, got)
			}
			if got := sortedCheckKeys(checksOf(t, body)); !equalStrings(got, oldReadyzChecks) {
				t.Errorf("checks keys must be exactly the pre-queue set %v, got %v", oldReadyzChecks, got)
			}
		})
	}
}

// TestReadyzQueue_PepperMissingStillReportsNotReadyForPepper: the checks are independent. A healthy
// queue must not mask a missing pepper, and the pepper check must say so by itself.
func TestReadyzQueue_PepperMissingStillReportsNotReadyForPepper(t *testing.T) {
	srv, _ := readyzServer(t, false, &fakePinger{}) // no pepper, db fine
	srv.WithQueueStatus(true, "")                   // queue fine
	rec, body := getReadyz(t, srv)

	if rec.Code != http.StatusServiceUnavailable || body["status"] != "not_ready" || body["ready"] != false {
		t.Errorf("no pepper => not ready even with a ready queue, got %d %v ready=%v", rec.Code, body["status"], body["ready"])
	}
	ch := checksOf(t, body)
	if ch["pepper"] {
		t.Errorf("checks.pepper must be false, got true")
	}
	if !ch["queue"] || !ch["db"] {
		t.Errorf("queue and db must stay TRUE (independent checks), got %v", ch)
	}
	if _, present := body["reasons"]; present {
		t.Errorf("`reasons` is about the queue; a ready queue must not produce one even when something else is down: %s", rec.Body.String())
	}
}

// TestReadyzQueue_ChecksAreIndependent walks all eight combinations of pepper x db x queue. ready must
// be exactly the AND of the three, each check must report only its own dependency, and `reasons` must
// appear exactly when the queue is not ready — never for the other two.
func TestReadyzQueue_ChecksAreIndependent(t *testing.T) {
	for _, pepper := range []bool{true, false} {
		for _, dbUp := range []bool{true, false} {
			for _, queueUp := range []bool{true, false} {
				var pingErr error
				if !dbUp {
					pingErr = errors.New("down")
				}
				name := strings.Join([]string{boolWord("pepper", pepper), boolWord("db", dbUp), boolWord("queue", queueUp)}, "/")
				t.Run(name, func(t *testing.T) {
					srv, _ := readyzServer(t, pepper, &fakePinger{err: pingErr})
					reason := ""
					if !queueUp {
						reason = queueReasonText
					}
					srv.WithQueueStatus(queueUp, reason)
					rec, body := getReadyz(t, srv)
					ch := checksOf(t, body)

					if ch["pepper"] != pepper || ch["db"] != dbUp || ch["queue"] != queueUp {
						t.Errorf("each check must track its own dependency: want pepper=%v db=%v queue=%v, got %v", pepper, dbUp, queueUp, ch)
					}
					wantReady := pepper && dbUp && queueUp
					wantCode := http.StatusServiceUnavailable
					if wantReady {
						wantCode = http.StatusOK
					}
					if body["ready"] != wantReady || rec.Code != wantCode || (body["status"] == "ready") != wantReady {
						t.Errorf("ready must be the AND of the three: want ready=%v/%d, got ready=%v status=%v code=%d", wantReady, wantCode, body["ready"], body["status"], rec.Code)
					}
					reasons, present := reasonsOf(t, body)
					if present != !queueUp {
						t.Errorf("`reasons` must be present iff the queue is not ready: queueUp=%v present=%v (%s)", queueUp, present, rec.Body.String())
					}
					if present && (len(reasons) != 1 || !strings.HasPrefix(reasons[0], "queue: ")) {
						t.Errorf("reasons must hold exactly one `queue: ...` entry, got %q", reasons)
					}
				})
			}
		}
	}
}

func boolWord(name string, up bool) string {
	if up {
		return name + "_up"
	}
	return name + "_down"
}

// TestReadyzQueue_EmptyReasonIsNeverABareLabel: a caller that declares not-ready and forgets the
// reason must still tell the operator SOMETHING — `queue: ` followed by nothing is worse than no
// field, because it looks like a message and carries none.
func TestReadyzQueue_EmptyReasonIsNeverABareLabel(t *testing.T) {
	srv, _ := readyzServer(t, true, &fakePinger{})
	srv.WithQueueStatus(false, "")
	_, body := getReadyz(t, srv)

	reasons, present := reasonsOf(t, body)
	if !present || len(reasons) != 1 {
		t.Fatalf("a not-ready queue must always carry exactly one reason, got %v (present=%v)", reasons, present)
	}
	if got := strings.TrimSpace(strings.TrimPrefix(reasons[0], "queue:")); got == "" {
		t.Errorf("reason must not be a bare `queue:` label, got %q", reasons[0])
	}
}

// TestReadyzQueue_LastDeclarationWins: the declaration is a plain setter, not a latch. A server that
// declares not-ready and is then (re)declared ready must read ready, so a wiring path that corrects
// itself is not stuck at its first answer.
func TestReadyzQueue_LastDeclarationWins(t *testing.T) {
	srv, _ := readyzServer(t, true, &fakePinger{})
	srv.WithQueueStatus(false, queueReasonText)
	if rec, _ := getReadyz(t, srv); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("precondition: declared not-ready must be 503, got %d", rec.Code)
	}
	srv.WithQueueStatus(true, "")
	rec, body := getReadyz(t, srv)
	if rec.Code != http.StatusOK || body["ready"] != true {
		t.Errorf("re-declared ready must be 200/ready, got %d ready=%v", rec.Code, body["ready"])
	}
	if _, present := body["reasons"]; present {
		t.Errorf("the earlier reason must not linger after re-declaring ready: %s", rec.Body.String())
	}
}
